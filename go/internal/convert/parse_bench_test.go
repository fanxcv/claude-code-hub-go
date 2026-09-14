package convert

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 本文件量转换层 JSON 解析器的每请求成本（分配与耗时），用真实形状的请求体。
//
// 为什么需要它：生产剖析（60s 真流量）里 `convert.(*jsonParser).parseLiteral` 占**全服务累计
// 分配的 31.9%**、strings.Builder 两处合计 25.6% —— 这两项都在解析器里，且都不需要改
// 外部行为就能去掉。基准是「改前/改后」的唯一判据。
//
// 请求体形状照真实客户端：大量短字符串（角色、内容片段），少量嵌套对象，若干布尔与 null。

// benchBody 造一个约 size 字节的 chat 形状请求体（合法 JSON：元素之间用逗号连接）。
func benchBody(size int) []byte {
	var builder strings.Builder
	builder.WriteString(`{"model":"gpt-5.6","stream":true,"messages":[`)
	first := true
	for builder.Len() < size {
		if !first {
			builder.WriteString(",")
		}
		first = false
		builder.WriteString(`{"role":"assistant","content":"`)
		builder.WriteString(strings.Repeat("x", 96))
		builder.WriteString(`","tool_calls":null,"partial":false,"index":`)
		builder.WriteString(fmt.Sprint(builder.Len()))
		builder.WriteString(`}`)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}

func BenchmarkParseJSONRequest(b *testing.B) {
	for _, size := range []int{8 << 10, 64 << 10, 256 << 10} {
		body := benchBody(size)
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for index := 0; index < b.N; index++ {
				if _, err := ParseJSON(body); err != nil {
					b.Fatalf("解析失败: %v", err)
				}
			}
		})
	}
}

// BenchmarkParseJSONStringHeavy 单独压「大量短字符串、无转义」这一最常见形状。
func BenchmarkParseJSONStringHeavy(b *testing.B) {
	var builder strings.Builder
	builder.WriteString(`{"items":[`)
	for builder.Len() < 64<<10 {
		builder.WriteString(`"`)
		builder.WriteString(strings.Repeat("y", 40))
		builder.WriteString(`",`)
	}
	builder.WriteString(`null]}`)
	body := []byte(builder.String())
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for index := 0; index < b.N; index++ {
		if _, err := ParseJSON(body); err != nil {
			b.Fatalf("解析失败: %v", err)
		}
	}
}

// TestParseJSONMatchesEncodingJSON 是**等价性钉子**：优化不得改变解析结果。
// 用标准库作为参照语义（只比对我们关心的形状：字符串、数字、布尔、null、嵌套）。
func TestParseJSONMatchesEncodingJSON(t *testing.T) {
	cases := []string{
		`{"a":"plain","b":true,"c":false,"d":null,"e":1.5,"f":-2,"g":[1,"two",null]}`,
		`{"escaped":"line\nbreak\ttab\"quote\\slash\/solid\u00e9"}`,
		`{"empty":"","nested":{"deep":{"value":"x"}}}`,
		`{}`,
		`[]`,
		`{"unicode":"中文与 emoji 🚀","num":1e10}`,
		`{"tail":null,"after":"还在"}`,
	}
	for _, input := range cases {
		parsed, err := ParseJSON([]byte(input))
		if err != nil {
			t.Fatalf("ParseJSON(%s) 失败: %v", input, err)
		}
		var want any
		if err := json.Unmarshal([]byte(input), &want); err != nil {
			t.Fatalf("标准库解析参照失败: %v", err)
		}
		got := valueToAny(parsed)
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if string(wantJSON) != string(gotJSON) {
			t.Fatalf("解析结果与标准库不一致：\n输入 %s\n得到 %s\n期望 %s", input, gotJSON, wantJSON)
		}
	}
}

// valueToAny 把内部 Value 转成原生结构，供与标准库对拍；只覆盖等价性需要判定的形状。
func valueToAny(value *Value) any {
	switch value.Kind() {
	case "null":
		return nil
	case "bool":
		result, _ := value.Bool()
		return result
	case "number":
		literal, _ := value.NumberLiteral()
		var number any
		if err := json.Unmarshal([]byte(literal), &number); err != nil {
			return literal
		}
		return number
	case "string":
		result, _ := value.String()
		return result
	case "array":
		items := value.Items()
		result := make([]any, 0, len(items))
		for _, item := range items {
			result = append(result, valueToAny(item))
		}
		return result
	case "object":
		members := value.Members()
		result := make(map[string]any, len(members))
		for _, member := range members {
			result[member.Key] = valueToAny(member.Value)
		}
		return result
	default:
		return nil
	}
}
