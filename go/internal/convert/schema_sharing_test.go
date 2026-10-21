package convert

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// 本文件是「货架上的 JSON schema 必须零拷贝搬运」的可执行基线。
//
// 为什么需要这条基线：response_format / text.format 的 schema 动辄 50 KiB，若用 Clone()
// 搬进枢纽就是凭空多一整份（实测 55 KiB schema 每次编码多分配 ~90 KiB）。而 Value 的做法
// 从来不是靠 Clone()——它靠「同一个 *Value 指针在源树、枢纽、出站体之间传递」。
//
// 为什么指针共享在这里是安全的（尽管 Value 并非 immutable）：Set / Delete / Append 都是
// 原地改并返回同一接收者（value.go），故共享的合法前提是「枢纽持有的子树只读」。这条约定
// 早已在生产上承载 Tool.Parameters、Block.Value 与 passthrough 的同线字段，并由
// TestEncodeRequestHasNoHiddenState 钉住「编码器不改写入参」。本文件只是把它量出来。

// schemaWithProperties 造一份约 50 KiB 的 JSON schema（200 个属性）。
func schemaWithProperties(t *testing.T) *Value {
	t.Helper()
	var builder strings.Builder
	builder.WriteString(`{"type":"object","properties":{`)
	for index := 0; index < 200; index++ {
		if index > 0 {
			builder.WriteString(",")
		}
		fmt.Fprintf(&builder, `"field_%03d":{"type":"string","description":"%s"}`,
			index, strings.Repeat("x", 230))
	}
	builder.WriteString(`},"required":["field_000"],"additionalProperties":false}`)
	schema := mustParsePayload(t, builder.String())
	if size := len(schema.MarshalCompact()); size < 50<<10 {
		t.Fatalf("语料须 ≥50 KiB，实得 %d B", size)
	}
	return schema
}

// chatBodyWithTool 把 schema 装进 chat 线 tools[].function.parameters。
func chatBodyWithTool(t *testing.T, schema *Value) *Value {
	t.Helper()
	body := NewObject().
		Set("model", NewString("gpt-4o")).
		Set("messages", NewArray(NewObject().
			Set("role", NewString("user")).
			Set("content", NewString("hi")))).
		Set("tools", NewArray(NewObject().
			Set("type", NewString("function")).
			Set("function", NewObject().
				Set("name", NewString("big")).
				Set("parameters", schema))))
	return mustParsePayload(t, body.MarshalCompact())
}

// TestResponseFormatNoDeepCopy 钉住 response_format 的 schema 进枢纽不做深拷贝。
//
// 现状（#5 未落地）：chat 线的 response_format 落在 Passthrough[chatWire] 里，与源树共享
// 同一个 *Value——本用例把这条零拷贝现状量成基线。#5 移植时必须沿用同一做法（枢纽字段直接
// 持有源树指针），而不是 ResponseFormat{Schema: format.Clone()}。
func TestResponseFormatNoDeepCopy(t *testing.T) {
	schema := schemaWithProperties(t)
	body := NewObject().
		Set("model", NewString("gpt-4o")).
		Set("messages", NewArray(NewObject().
			Set("role", NewString("user")).
			Set("content", NewString("hi")))).
		Set("response_format", NewObject().
			Set("type", NewString("json_schema")).
			Set("json_schema", NewObject().
				Set("name", NewString("big")).
				Set("strict", NewBool(true)).
				Set("schema", schema)))
	source := mustParsePayload(t, body.MarshalCompact())

	decoded, ok := DecodeRequest(ProtocolOpenAIChat, source, ConvertCtx{Model: "gpt-4o"})
	if !ok {
		t.Fatal("chat 解码失败")
	}
	carried, present := decoded.Value.Passthrough[chatWire].Get("response_format")
	if !present {
		t.Fatal("response_format 未进枢纽")
	}
	sourceFormat, _ := source.Get("response_format")
	if carried != sourceFormat {
		t.Fatal("response_format 进枢纽时被复制 —— 应持有源树的同一 *Value")
	}

	// schema 位于 response_format.json_schema.schema，最深一层同样不得被复制。
	carriedSchema := fieldOrNil(fieldOrNil(carried, "json_schema"), "schema")
	sourceSchema := fieldOrNil(fieldOrNil(sourceFormat, "json_schema"), "schema")
	if carriedSchema != sourceSchema {
		t.Fatal("response_format 的 schema 进枢纽时被复制")
	}
	if carriedSchema.MarshalCompact() != sourceSchema.MarshalCompact() {
		t.Fatal("schema 搬运后序列化与源树不一致")
	}
	if carried.MarshalCompact() != sourceFormat.MarshalCompact() {
		t.Fatal("response_format 搬运后序列化与源树不一致")
	}
}

// TestHubSchemaCarriedWithoutDeepCopy 钉住同一条约定在跨线编码上的表现：schema 从枢纽搬去
// 目标线时挂的是同一个 *Value，不是一份深拷贝。这条是 #5 真正会踩的那一脚——chat 的
// response_format 要转成 responses 的 text.format，schema 正是在编码器里被搬运。
func TestHubSchemaCarriedWithoutDeepCopy(t *testing.T) {
	schema := schemaWithProperties(t)
	source := chatBodyWithTool(t, schema)
	sourceSchema := fieldOrNil(fieldOrNil(source.ArrayField("tools")[0], "function"), "parameters")

	ctx := ConvertCtx{Model: "gpt-4o"}
	decoded, ok := DecodeRequest(ProtocolOpenAIChat, source, ctx)
	if !ok || len(decoded.Value.Tools) != 1 {
		t.Fatalf("chat 解码失败: ok=%v tools=%d", ok, len(decoded.Value.Tools))
	}
	if decoded.Value.Tools[0].Parameters != sourceSchema {
		t.Fatal("进枢纽时 schema 被复制 —— 应直接持有源树的同一 *Value")
	}

	encoded, ok := EncodeRequest(ProtocolOpenAIResponses, decoded.Value, ctx)
	if !ok {
		t.Fatal("responses 编码失败")
	}
	emitted := fieldOrNil(encoded.Body.ArrayField("tools")[0], "parameters")
	if emitted != sourceSchema {
		t.Fatal("编码时 schema 被复制 —— 应把同一 *Value 挂进出站体")
	}
	if emitted.MarshalCompact() != sourceSchema.MarshalCompact() {
		t.Fatal("搬运后 schema 序列化与源树不一致")
	}
}

// TestHubSchemaCarriedWithoutDeepCopyAllocs 用分配量把同一件事钉死：编码一次不得分配出与
// schema 同量级的字节。深拷贝一份 50 KiB schema 会直接顶破阈值（实测 ~90 KiB vs ~2 KiB）。
func TestHubSchemaCarriedWithoutDeepCopyAllocs(t *testing.T) {
	schema := schemaWithProperties(t)
	source := chatBodyWithTool(t, schema)
	ctx := ConvertCtx{Model: "gpt-4o"}
	decoded, ok := DecodeRequest(ProtocolOpenAIChat, source, ctx)
	if !ok {
		t.Fatal("chat 解码失败")
	}
	schemaBytes := uint64(len(schema.MarshalCompact()))

	const rounds = 20
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for index := 0; index < rounds; index++ {
		if _, ok := EncodeRequest(ProtocolOpenAIResponses, decoded.Value, ctx); !ok {
			t.Fatal("responses 编码失败")
		}
	}
	runtime.ReadMemStats(&after)

	perEncode := (after.TotalAlloc - before.TotalAlloc) / rounds
	if perEncode >= schemaBytes {
		t.Fatalf("每次编码分配 %d B，达到 schema 本身 %d B —— schema 被深拷贝了一份",
			perEncode, schemaBytes)
	}
	t.Logf("每次编码分配 %d B（schema %d B，比值 %.3f）",
		perEncode, schemaBytes, float64(perEncode)/float64(schemaBytes))
}
