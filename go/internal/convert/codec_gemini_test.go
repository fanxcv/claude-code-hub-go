package convert

import "testing"

// TestDecodeGeminiResponseUsage 钉住 Gemini 用量口径。
//
// 逐条对齐 Node（src/app/v1/_lib/proxy/response-handler.ts:6045-6068）：
// input = max(promptTokenCount - cachedContentTokenCount, 0)、output = candidatesTokenCount、
// cache_read = cachedContentTokenCount。第一个用例专门覆盖「不减去 cached 就重复计费」。
func TestDecodeGeminiResponseUsage(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		input     *float64
		output    *float64
		cacheRead *float64
	}{
		{
			name:      "prompt 含 cached：input 必须减去它",
			body:      `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"cachedContentTokenCount":40}}`,
			input:     numberPtr(60),
			output:    numberPtr(20),
			cacheRead: numberPtr(40),
		},
		{
			name:   "无 cached：input 就是 prompt",
			body:   `{"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3}}`,
			input:  numberPtr(7),
			output: numberPtr(3),
		},
		{
			name:      "cached 大于 prompt：下界截到 0，不写负数",
			body:      `{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"cachedContentTokenCount":9}}`,
			input:     numberPtr(0),
			output:    numberPtr(1),
			cacheRead: numberPtr(9),
		},
		{
			name: "没有 usageMetadata：用量留空，不写 0",
			body: `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			value, err := ParseJSON([]byte(testCase.body))
			if err != nil {
				t.Fatalf("解析正文失败: %v", err)
			}
			decoded, ok := DecodeResponse(ProtocolGemini, value, ConvertCtx{})
			if !ok {
				t.Fatal("DecodeResponse 应认识 Gemini 观测标签（它供非流式记账取用量）")
			}
			usage := decoded.Value.Usage
			if testCase.input == nil && testCase.output == nil && testCase.cacheRead == nil {
				if !usage.IsEmpty() {
					t.Fatalf("无用量对象时用量应为空，实为 %+v", usage)
				}
				return
			}
			assertToken(t, "input", testCase.input, usage.InputTokens)
			assertToken(t, "output", testCase.output, usage.OutputTokens)
			assertToken(t, "cache_read", testCase.cacheRead, usage.CacheReadTokens)
		})
	}
}

// TestDecodeGeminiResponseModel 钉住上游实际模型名的取值顺序：
// modelVersion > model_version > 上下文模型（Node actual-response-model.ts:145-146）。
func TestDecodeGeminiResponseModel(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "modelVersion 优先", body: `{"modelVersion":"gemini-2.0-flash-001","model_version":"older"}`, want: "gemini-2.0-flash-001"},
		{name: "退回 model_version", body: `{"model_version":"gemini-1.5-pro-002"}`, want: "gemini-1.5-pro-002"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			value, err := ParseJSON([]byte(testCase.body))
			if err != nil {
				t.Fatalf("解析正文失败: %v", err)
			}
			decoded, _ := DecodeResponse(ProtocolGemini, value, ConvertCtx{Model: "fallback-model"})
			if decoded.Value.Model != testCase.want {
				t.Fatalf("模型 = %q，期望 %q", decoded.Value.Model, testCase.want)
			}
		})
	}

	value, _ := ParseJSON([]byte(`{"candidates":[]}`))
	decoded, _ := DecodeResponse(ProtocolGemini, value, ConvertCtx{Model: "client-model"})
	if decoded.Value.Model != "client-model" {
		t.Fatalf("正文无模型字段时应退回上下文模型，实为 %q", decoded.Value.Model)
	}
}

// TestGeminiIsNotInConversionMatrix 钉住金标语料的选择语义没被「支持 gemini」误伤：
// 它只有只读解码，**不注册 codec**，故跨线组合恒不可转。
func TestGeminiIsNotInConversionMatrix(t *testing.T) {
	if HasCodec(ProtocolGemini) {
		t.Fatal("ProtocolGemini 不得注册 codec：那会让 CanConvert 把跨线组合判为可转，违反 selection.json")
	}
	if _, ok := ProtocolOfProviderType(ProviderGemini); ok {
		t.Fatal("ProtocolOfProviderType(gemini) 必须仍为 false（Node 侧为 null，语料已钉）")
	}
	if _, ok := ProtocolOfClientFormat(FormatGemini); ok {
		t.Fatal("ProtocolOfClientFormat(gemini) 必须仍为 false（Node 侧为 null，语料已钉）")
	}
	if compat := ResolveProtocolCompat(FormatGemini, ProviderClaude, true); compat != CompatIncompatible {
		t.Fatalf("gemini 客户端 + claude 供应商应 incompatible，实为 %s", compat)
	}
	if compat := ResolveProtocolCompat(FormatGemini, ProviderGemini, true); compat != CompatNative {
		t.Fatalf("gemini 两端应 native，实为 %s", compat)
	}
	if plan := PlanConversion(FormatGemini, ProviderGemini, true); plan != nil {
		t.Fatal("gemini 两端不应产生转换计划")
	}
}

func assertToken(t *testing.T, name string, want *float64, got *float64) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("%s 应为空，实为 %v", name, *got)
		}
		return
	}
	if got == nil {
		t.Fatalf("%s 应为 %v，实为空", name, *want)
	}
	if *got != *want {
		t.Fatalf("%s = %v，期望 %v", name, *got, *want)
	}
}

func numberPtr(value float64) *float64 { return &value }
