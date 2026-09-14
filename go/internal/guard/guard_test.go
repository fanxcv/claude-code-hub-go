package guard

import (
	"strings"
	"testing"
)

// 顺序即契约：与 src/app/v1/_lib/proxy/guard-pipeline.ts 逐字对齐，重排即缺陷。
func TestPresetSequences(t *testing.T) {
	cases := []struct {
		name   string
		actual []StepKey
		want   []StepKey
	}{
		{
			name:   "CHAT_PIPELINE",
			actual: ChatPipeline.Steps,
			want: []StepKey{
				"auth", "sensitive", "client", "model", "version", "probe", "session",
				"warmup", "requestFilter", "replayAttach", "rateLimit", "provider",
				"providerRequestFilter", "messageContext",
			},
		},
		{
			name:   "RAW_PASSTHROUGH_PIPELINE",
			actual: RawPassthroughPipeline.Steps,
			want:   []StepKey{"auth", "client", "model", "version", "probe", "provider"},
		},
		{
			name:   "RAW_SAFE_SESSION_PIPELINE",
			actual: RawSafeSessionPipeline.Steps,
			want: []StepKey{
				"auth", "client", "model", "version", "probe", "session", "provider", "messageContext",
			},
		},
		{
			name:   "COUNT_TOKENS_PIPELINE",
			actual: CountTokensPipeline.Steps,
			want: []StepKey{
				"auth", "client", "model", "version", "probe", "session", "provider", "messageContext",
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if len(testCase.actual) != len(testCase.want) {
				t.Fatalf("%s 步数应为 %d，收到 %d", testCase.name, len(testCase.want), len(testCase.actual))
			}
			for index, want := range testCase.want {
				if testCase.actual[index] != want {
					t.Fatalf("%s 第 %d 步应为 %q，收到 %q", testCase.name, index+1, want, testCase.actual[index])
				}
			}
		})
	}
}

func TestChatPipelineLength(t *testing.T) {
	if len(ChatPipeline.Steps) != 14 {
		t.Fatalf("CHAT_PIPELINE 应为十四步，收到 %d", len(ChatPipeline.Steps))
	}
	if ChatPipeline.Name != "CHAT_PIPELINE" {
		t.Errorf("预设名应为 CHAT_PIPELINE，收到 %q", ChatPipeline.Name)
	}
}

func TestCountTokensPipelineReusesSafeSession(t *testing.T) {
	if len(CountTokensPipeline.Steps) != len(RawSafeSessionPipeline.Steps) {
		t.Fatalf("COUNT_TOKENS_PIPELINE 应复用安全会话链")
	}
	if got := FromRequestType(RequestTypeCountTokens); len(got.Steps) != 8 {
		t.Fatalf("count_tokens 应走八步安全会话链，收到 %d 步", len(got.Steps))
	}
	if got := FromRequestType(RequestTypeChat); len(got.Steps) != 14 {
		t.Fatalf("普通对话应走十四步完整链，收到 %d 步", len(got.Steps))
	}
}

func TestAllStepKeysUniqueAndCoverPresets(t *testing.T) {
	seen := map[StepKey]bool{}
	for _, key := range AllStepKeys() {
		if seen[key] {
			t.Fatalf("步骤键 %q 重复", key)
		}
		seen[key] = true
	}
	for _, pipeline := range []Pipeline{
		ChatPipeline, RawPassthroughPipeline, RawSafeSessionPipeline, CountTokensPipeline,
	} {
		for _, key := range pipeline.Steps {
			if !seen[key] {
				t.Fatalf("预设 %s 用到未登记的步骤键 %q", pipeline.Name, key)
			}
		}
	}
}

// 键名是跨语言契约：Go 侧一旦改成 camelCase 之外的写法，Node 侧的链条目就无法对齐。
func TestStepKeySpelling(t *testing.T) {
	for _, key := range AllStepKeys() {
		text := string(key)
		if text == "" {
			t.Fatal("步骤键不得为空")
		}
		if first := text[0]; first < 'a' || first > 'z' {
			t.Fatalf("步骤键必须小驼峰（首字符小写），收到 %q", text)
		}
		if strings.Contains(text, "_") || strings.Contains(text, "-") {
			t.Fatalf("步骤键不得含下划线或连字符，收到 %q", text)
		}
	}
}
