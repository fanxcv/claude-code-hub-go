package convert

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf8"
)

// 本文件钉住「占位 thinking signature」这一新增能力：思考来自 chat/responses 线上游（没有
// Anthropic 签名）而客户端说 Anthropic 协议时，补一个合法 wire 形状的占位签名。
//
// 三条必须成立的性质（任何一条破了都会把「客户端显示思考」变成「客户端报错」或「下游解出垃圾」）：
//  1. 形状合法：base64 → 0x12 开头的 protobuf → [2][1] → channel_id [2][1][1] / model_text [2][1][6]；
//  2. 只补不覆盖：上游带的真实签名一律原样透传，开关也改不了它；
//  3. 关掉开关即回退：无签名的思考块照旧丢弃并记 Loss（新增能力不得成为隐式默认行为）。

// TestPlaceholderThinkingSignatureWireShapeIsLegalClaudeSignature 用**独立的** protobuf 读取器
// 校占位签名，不复用生产代码：形状错了要能被这条钉住，而不是被同一份实现自证。
func TestPlaceholderThinkingSignatureWireShapeIsLegalClaudeSignature(t *testing.T) {
	signature := PlaceholderThinkingSignature()
	if signature == "" {
		t.Fatal("占位签名不得为空")
	}
	raw, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		t.Fatalf("占位签名必须是合法 base64: %v", err)
	}
	if len(raw) == 0 || raw[0] != 0x12 {
		t.Fatalf("首字节须为 0x12（顶层 field 2, wire type 2），收到 0x%02x", raw[0])
	}
	// 单层 E 形：0x12 的高 6 位恰为 4，其 base64 首字符必然是 'E'（实签名同样是这个形状）。
	if signature[0] != 'E' {
		t.Fatalf("单层签名的 base64 应以 'E' 开头，收到 %q", signature[0])
	}

	container, ok := protoBytesField(t, raw, 2)
	if !ok {
		t.Fatal("顶层 field 2 缺失：解析端取不到 container")
	}
	channelBlock, ok := protoBytesField(t, container, 1)
	if !ok {
		t.Fatal("container field 1 缺失：解析端取不到 channel block")
	}
	channelID, ok := protoVarintField(t, channelBlock, 1)
	if !ok {
		t.Fatal("channel block field 1（channel_id）缺失：实签名里它是必填项")
	}
	if channelID != 11 {
		t.Fatalf("channel_id 期望 11，收到 %d", channelID)
	}
	modelText, ok := protoBytesField(t, channelBlock, 6)
	if !ok {
		t.Fatal("channel block field 6（model_text）缺失")
	}
	if string(modelText) != "placeholder" {
		t.Fatalf("model_text 期望显式占位值 placeholder，收到 %q", string(modelText))
	}
	if !utf8.Valid(modelText) {
		t.Fatal("model_text 必须是合法 UTF-8：解析端会按字符串读它")
	}
}

// TestPlaceholderThinkingSignatureIsStableAndRecognizable 钉住「同一进程内同值」与「只认全等」：
// 回程剥离按字符串相等判定，值一变就漏剥（占位签名被回传上游即 400）。
func TestPlaceholderThinkingSignatureIsStableAndRecognizable(t *testing.T) {
	first := PlaceholderThinkingSignature()
	if second := PlaceholderThinkingSignature(); second != first {
		t.Fatalf("占位签名必须每次同值，收到 %q 与 %q", first, second)
	}
	if !IsPlaceholderThinkingSignature(first) {
		t.Fatal("自家占位签名应被识别")
	}
	if IsPlaceholderThinkingSignature("") {
		t.Fatal("空签名不是占位签名（它表示「没有签名」，另一条分支）")
	}
	if IsPlaceholderThinkingSignature(first + "=") {
		t.Fatal("识别必须是全等：近似匹配会误剥真实签名")
	}
}

// TestAnthropicResponseInjectsPlaceholderSignatureForChatUpstream 走完整转换链
// （chat 上游响应 → Anthropic 客户端），断言思考块**带着合法占位签名**送达客户端。
func TestAnthropicResponseInjectsPlaceholderSignatureForChatUpstream(t *testing.T) {
	ctx := ConvertCtx{
		ClientFormat:                 FormatClaude,
		TargetProto:                  ProtocolOpenAIChat,
		Model:                        "deepseek-v4-flash",
		PlaceholderThinkingSignature: true,
	}
	source := mustParsePayload(t, `{"id":"c1","object":"chat.completion","model":"deepseek-v4-flash",`+
		`"choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"先想一下","content":"答案"},`+
		`"finish_reason":"stop"}]}`)

	decoded, ok := DecodeResponse(ProtocolOpenAIChat, source, ctx)
	if !ok {
		t.Fatal("chat 线无响应解码器")
	}
	encoded, ok := EncodeResponse(ProtocolAnthropicMessages, decoded.Value, ctx)
	if !ok {
		t.Fatal("anthropic 线无响应编码器")
	}

	body := mustParsePayload(t, encoded.Body.MarshalCompact())
	content := body.ArrayField("content")
	if len(content) == 0 {
		t.Fatalf("出站正文没有 content：%s", encoded.Body.MarshalCompact())
	}
	blockType, _ := content[0].StringField("type")
	if blockType != "thinking" {
		t.Fatalf("首个块应为 thinking，收到 %q", blockType)
	}
	if thinking, _ := content[0].StringField("thinking"); thinking != "先想一下" {
		t.Fatalf("思考正文应原样保留，收到 %q", thinking)
	}
	signature, _ := content[0].StringField("signature")
	if signature != PlaceholderThinkingSignature() {
		t.Fatalf("思考块应带占位签名，收到 %q", signature)
	}

	// 报表里必须是 rewritten（块留下了、签名是造的），不是「无签名丢弃」。
	assertLossAction(t, encoded.Loss, LossThinkingSignature, LossRewritten, "placeholder_signature_injected")
}

// TestAnthropicResponseKeepsUpstreamSignatureUntouched 钉住「只补不覆盖」：
// 上游带的真实签名在开关开与关两种状态下都必须逐字不变。
func TestAnthropicResponseKeepsUpstreamSignatureUntouched(t *testing.T) {
	const upstreamSignature = "Eupstream-real-signature"
	for _, placeholder := range []bool{true, false} {
		ctx := ConvertCtx{
			ClientFormat:                 FormatClaude,
			TargetProto:                  ProtocolOpenAIResponses,
			Model:                        "claude-sonnet-4-5",
			PlaceholderThinkingSignature: placeholder,
		}
		response := &Response{
			ID:    "msg_1",
			Model: "claude-sonnet-4-5",
			Blocks: []Block{
				{Kind: BlockThinking, Text: "上游的真实思考", Signature: upstreamSignature},
			},
		}
		encoded, ok := EncodeResponse(ProtocolAnthropicMessages, response, ctx)
		if !ok {
			t.Fatal("anthropic 线无响应编码器")
		}
		body := mustParsePayload(t, encoded.Body.MarshalCompact())
		content := body.ArrayField("content")
		if len(content) != 1 {
			t.Fatalf("真实签名的思考块必须原样保留，收到 %d 个块", len(content))
		}
		got, _ := content[0].StringField("signature")
		if got != upstreamSignature {
			t.Fatalf("上游签名被改动：期望 %q，收到 %q", upstreamSignature, got)
		}
		if encoded.Loss.Entries != nil {
			t.Fatalf("真实签名路径不应产生任何损失声明，收到 %v", encoded.Loss.Entries)
		}
	}
}

// TestAnthropicResponseDropsUnsignedThinkingWhenSwitchOff 钉住回退：开关关闭时无签名的思考块
// 照旧丢弃并记 Loss（新增能力不得悄悄变成默认行为）。
func TestAnthropicResponseDropsUnsignedThinkingWhenSwitchOff(t *testing.T) {
	ctx := ConvertCtx{
		ClientFormat:                 FormatClaude,
		TargetProto:                  ProtocolOpenAIChat,
		Model:                        "deepseek-v4-flash",
		PlaceholderThinkingSignature: false,
	}
	source := mustParsePayload(t, `{"id":"c1","object":"chat.completion","choices":[{"index":0,`+
		`"message":{"role":"assistant","reasoning_content":"先想一下","content":"答案"},"finish_reason":"stop"}]}`)
	decoded, ok := DecodeResponse(ProtocolOpenAIChat, source, ctx)
	if !ok {
		t.Fatal("chat 线无响应解码器")
	}
	encoded, ok := EncodeResponse(ProtocolAnthropicMessages, decoded.Value, ctx)
	if !ok {
		t.Fatal("anthropic 线无响应编码器")
	}
	body := mustParsePayload(t, encoded.Body.MarshalCompact())
	for _, block := range body.ArrayField("content") {
		if blockType, _ := block.StringField("type"); blockType == "thinking" {
			t.Fatal("开关关闭时不应产出 thinking 块")
		}
	}
	assertLossAction(t, encoded.Loss, LossThinkingSignature, LossDropped, "missing_signature")
}

// TestAnthropicStreamEmitsSignatureDeltaBeforeBlockStop 钉住流式形态：占位签名走
// signature_delta（官方形态），且必须落在 thinking_delta 之后、content_block_stop 之前。
func TestAnthropicStreamEmitsSignatureDeltaBeforeBlockStop(t *testing.T) {
	ctx := ConvertCtx{
		ClientFormat:                 FormatClaude,
		TargetProto:                  ProtocolOpenAIChat,
		Model:                        "deepseek-v4-flash",
		Stream:                       true,
		PlaceholderThinkingSignature: true,
	}
	pipe, ok := NewStreamPipe(ProtocolOpenAIChat, ProtocolAnthropicMessages, ctx)
	if !ok {
		t.Fatal("建管道失败：openai-chat -> anthropic-messages")
	}
	frames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"先想"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"一下"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"答案"},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	var out strings.Builder
	for _, frame := range frames {
		for _, chunk := range pipe.Push([]byte(frame)) {
			out.WriteString(string(chunk))
		}
	}
	for _, chunk := range pipe.Flush() {
		out.WriteString(string(chunk))
	}
	text := out.String()

	startIndex := strings.Index(text, `"type":"thinking"`)
	thinkingDeltaIndex := strings.Index(text, `"type":"thinking_delta"`)
	signatureDeltaIndex := strings.Index(text, `"type":"signature_delta"`)
	stopIndex := strings.LastIndex(text, `"type":"content_block_stop"`)
	if startIndex < 0 || thinkingDeltaIndex < 0 || signatureDeltaIndex < 0 || stopIndex < 0 {
		t.Fatalf("流式事件不全（start=%d thinking=%d signature=%d stop=%d）：%q",
			startIndex, thinkingDeltaIndex, signatureDeltaIndex, stopIndex, text)
	}
	if !(startIndex < thinkingDeltaIndex && thinkingDeltaIndex < signatureDeltaIndex && signatureDeltaIndex < stopIndex) {
		t.Fatalf("事件顺序应为 start → thinking_delta → signature_delta → stop，收到：%q", text)
	}
	if !strings.Contains(text, `"signature":"`+PlaceholderThinkingSignature()+`"`) {
		t.Fatalf("signature_delta 未携带占位签名：%q", text)
	}
	// 块起始里不得带签名：官方流式的签名走增量事件，塞进 start 是形状错误。
	startFrame := text[startIndex:thinkingDeltaIndex]
	if strings.Contains(startFrame, `"signature"`) {
		t.Fatalf("content_block_start 不应携带 signature：%q", startFrame)
	}
}

// assertLossAction 断言报表里存在指定能力 + 动作（含 detail）的一条损失声明。
func assertLossAction(t *testing.T, report LossReport, capability string, action LossAction, detail string) {
	t.Helper()
	for _, entry := range report.Entries {
		if entry.Capability == capability && entry.Action == action && entry.Detail == detail {
			return
		}
	}
	t.Fatalf("报表里没有 %s/%s/%s，收到 %+v", capability, action, detail, report.Entries)
}

// protoBytesField 是测试用的最小 protobuf 读取器：取指定字段号的 length-delimited 值。
func protoBytesField(t *testing.T, message []byte, field int) ([]byte, bool) {
	t.Helper()
	for offset := 0; offset < len(message); {
		key, size := protoVarint(message[offset:])
		if size == 0 {
			return nil, false
		}
		offset += size
		if int(key>>3) != field {
			skipped, ok := protoSkip(message[offset:], key&7)
			if !ok {
				return nil, false
			}
			offset += skipped
			continue
		}
		if key&7 != 2 {
			t.Fatalf("字段 %d 的 wire type 应为 2（length-delimited），收到 %d", field, key&7)
		}
		length, lengthSize := protoVarint(message[offset:])
		if lengthSize == 0 {
			return nil, false
		}
		offset += lengthSize
		if offset+int(length) > len(message) {
			return nil, false
		}
		return message[offset : offset+int(length)], true
	}
	return nil, false
}

// protoVarintField 取指定字段号的 varint 值。
func protoVarintField(t *testing.T, message []byte, field int) (uint64, bool) {
	t.Helper()
	for offset := 0; offset < len(message); {
		key, size := protoVarint(message[offset:])
		if size == 0 {
			return 0, false
		}
		offset += size
		if int(key>>3) == field {
			if key&7 != 0 {
				t.Fatalf("字段 %d 的 wire type 应为 0（varint），收到 %d", field, key&7)
			}
			value, valueSize := protoVarint(message[offset:])
			if valueSize == 0 {
				return 0, false
			}
			return value, true
		}
		skipped, ok := protoSkip(message[offset:], key&7)
		if !ok {
			return 0, false
		}
		offset += skipped
	}
	return 0, false
}

// protoVarint 读一个 varint，返回取值与占用字节数（0 表示不完整）。
func protoVarint(raw []byte) (uint64, int) {
	var value uint64
	for index := 0; index < len(raw) && index < 10; index++ {
		value |= uint64(raw[index]&0x7F) << (7 * index)
		if raw[index]&0x80 == 0 {
			return value, index + 1
		}
	}
	return 0, 0
}

// protoSkip 跳过一个非目标字段的值，返回跳过的字节数。
func protoSkip(raw []byte, wireType uint64) (int, bool) {
	switch wireType {
	case 0:
		_, size := protoVarint(raw)
		return size, size > 0
	case 2:
		length, size := protoVarint(raw)
		if size == 0 || size+int(length) > len(raw) {
			return 0, false
		}
		return size + int(length), true
	default:
		return 0, false
	}
}
