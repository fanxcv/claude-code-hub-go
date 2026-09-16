package convert

import "testing"

// 本文件钉住「请求侧高层字段跨线无承载位」的两条处置：
//   - 状态型字段（previous_response_id / conversation / store:true）：只**判定**冲突，由
//     forward.BuildPlan 据此 fail-closed（见 forward/plan_test.go 的对应用例）；
//   - 约束型字段（prompt_cache_key / response_format / text / store:false）：记损，不阻断。
//
// 为什么要钉：这些字段此前落进 passthrough 逃生舱后**没有任何消费者**（request 侧 passthrough
// 只在目标线取用，而跨线时目标线与来源线不同），于是 Node 与本仓都表现为静默丢弃——客户端
// 以为命中了前缀缓存、以为拿到的是 json_schema 约束下的输出，实际都不是，且报告里查不到痕迹。

func TestStatefulConversionConflict(t *testing.T) {
	cases := []struct {
		name string
		wire WireProtocol
		body string
		want string
	}{
		{"previous_response_id", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","previous_response_id":"resp_123"}`, "previous_response_id"},
		{"conversation 字符串", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","conversation":"conv_1"}`, "conversation"},
		{"conversation 对象", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","conversation":{"id":"conv_1"}}`, "conversation"},
		{"store:true", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","store":true}`, "store"},
		{"store:false 不算冲突", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","store":false}`, ""},
		{"无状态字段", ProtocolOpenAIResponses, `{"model":"gpt-5","input":"hi"}`, ""},
		{"空串视为未给出", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","previous_response_id":""}`, ""},
		{"null 视为未给出", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","conversation":null}`, ""},
		// 其它方言里出现同名字段只是杂项键：不能因一个杂项键把请求打回（错误体形状也不对）。
		{"chat 线上的同名字段不判冲突", ProtocolOpenAIChat,
			`{"model":"gpt-5","messages":[],"previous_response_id":"resp_123"}`, ""},
		{"anthropic 线上的同名字段不判冲突", ProtocolAnthropicMessages,
			`{"model":"claude","messages":[],"conversation":"conv_1"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StatefulConversionConflict(tc.wire, mustParsePayload(t, tc.body))
			if got != tc.want {
				t.Fatalf("冲突字段应为 %q，收到 %q", tc.want, got)
			}
		})
	}
}

// TestForeignDroppableFieldsRecordedAsLoss 走真实编解码链，断言跨线丢弃的约束型字段都进了损失台账。
func TestForeignDroppableFieldsRecordedAsLoss(t *testing.T) {
	cases := []struct {
		name     string
		source   WireProtocol
		target   WireProtocol
		body     string
		wantLoss []string
	}{
		{
			name:   "responses→chat：缓存键、结构化输出、落库开关",
			source: ProtocolOpenAIResponses,
			target: ProtocolOpenAIChat,
			body: `{"model":"gpt-5","input":"hi","prompt_cache_key":"k1",` +
				`"text":{"format":{"type":"json_schema","name":"r","schema":{"type":"object"}}},` +
				`"store":false}`,
			wantLoss: []string{
				LossPromptCacheKey + "/dropped/prompt_cache_key",
				LossTextControls + "/dropped/text",
				LossStoreFlag + "/dropped/store",
			},
		},
		{
			name:     "chat→responses：response_format",
			source:   ProtocolOpenAIChat,
			target:   ProtocolOpenAIResponses,
			body:     `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`,
			wantLoss: []string{LossResponseFormat + "/dropped/response_format"},
		},
		{
			name:     "chat→anthropic：response_format 同样记损",
			source:   ProtocolOpenAIChat,
			target:   ProtocolAnthropicMessages,
			body:     `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`,
			wantLoss: []string{LossResponseFormat + "/dropped/response_format"},
		},
		{
			name:     "无这些字段时不记损（不多不少）",
			source:   ProtocolOpenAIChat,
			target:   ProtocolAnthropicMessages,
			body:     `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
			wantLoss: []string{},
		},
		{
			name:   "null / 空值不算给出",
			source: ProtocolOpenAIResponses,
			target: ProtocolOpenAIChat,
			body:   `{"model":"gpt-5","input":"hi","prompt_cache_key":null,"text":{},"store":null}`,
			// text 是空对象，isMeaningful 判为无内容；另外两个是 null。
			wantLoss: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := mustParsePayload(t, tc.body)
			ctx := ConvertCtx{
				ClientFormat:   clientFormatOfProtocol(tc.source),
				TargetProto:    tc.target,
				Model:          "m",
				ToWireToolName: NormalizeToolName,
			}
			decoded, ok := DecodeRequest(tc.source, source, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无解码器", tc.source)
			}
			encoded, ok := EncodeRequest(tc.target, decoded.Value, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无编码器", tc.target)
			}

			got := []string{}
			for _, entry := range encoded.Loss.Entries {
				if !entry.HasDetail {
					continue
				}
				got = append(got, entry.Capability+"/"+string(entry.Action)+"/"+entry.Detail)
			}
			for _, want := range tc.wantLoss {
				if !containsString(got, want) {
					t.Fatalf("缺少损失条目 %s；实际：%v", want, got)
				}
			}
			// 「不多」也钉住：无这些字段时不得凭空补条目（多余条目会让台账失去可读性）。
			// 只查本文件新增的四类：同一份报告里还有其它来源的损失（如 max_tokens.defaulted）。
			if len(tc.wantLoss) == 0 {
				for _, entry := range encoded.Loss.Entries {
					for _, class := range foreignDroppableClasses {
						if entry.Capability == class {
							t.Fatalf("不该有 %s 类损失，实际：%v", class, got)
						}
					}
				}
			}
		})
	}
}

// foreignDroppableClasses 是本文件钉住的四类损失；「不多不少」断言按它过滤，避免误伤其它来源的损失。
var foreignDroppableClasses = []string{
	LossPromptCacheKey, LossResponseFormat, LossTextControls, LossStoreFlag,
}

func containsString(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
