package gate

import (
	"testing"
	"time"
)

// 本文件钉住分级速率闸的纯逻辑：语义 payload 计量口径与三档离散检查点状态机。
// 门控集成层（Run 里的提交/判慢）在 rate_ladder_test.go。

// testStages 把出厂三档缩到毫秒级：否则每个用例都要真等 10 秒。
// 倍数语义与出厂一致（首档 2 倍，其后 1 倍）。
func testStages() []ladderStage {
	return []ladderStage{
		{20 * time.Millisecond, PrecommitFastMultiplier},
		{60 * time.Millisecond, 1},
		{200 * time.Millisecond, 1},
	}
}

// TestSemanticPayloadBytesMetering 钉住计量口径：什么算产出、什么不算。
func TestSemanticPayloadBytesMetering(t *testing.T) {
	cases := []struct {
		name   string
		family Family
		event  string
		data   string
		want   int
	}{
		{
			name:   "anthropic text delta 计入",
			family: FamilyAnthropic,
			event:  "content_block_delta",
			data:   `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`,
			want:   5,
		},
		{
			name:   "anthropic thinking delta 计入",
			family: FamilyAnthropic,
			event:  "content_block_delta",
			data:   `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"think!!"}}`,
			want:   7,
		},
		{
			name:   "anthropic tool args delta 计入",
			family: FamilyAnthropic,
			event:  "content_block_delta",
			data:   `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
			want:   7,
		},
		{
			name:   "openai chat content 计入",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{"choices":[{"delta":{"content":"hello"}}]}`,
			want:   5,
		},
		{
			name:   "openai chat tool arguments 计入",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"a\":1}"}}]}}]}`,
			want:   7,
		},
		{
			name:   "openai chat reasoning 计入",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{"choices":[{"delta":{"reasoning_content":"abcd"}}]}`,
			want:   4,
		},
		{
			name:   "openai responses delta 计入",
			family: FamilyOpenAIResponses,
			event:  "response.output_text.delta",
			data:   `{"type":"response.output_text.delta","delta":"abc"}`,
			want:   3,
		},
		{
			name:   "UTF-8 按字节计",
			family: FamilyAnthropic,
			event:  "content_block_delta",
			data:   `{"type":"content_block_delta","delta":{"type":"text_delta","text":"中文"}}`,
			want:   6,
		},
		{
			name:   "心跳帧不计入",
			family: FamilyAnthropic,
			event:  "",
			data:   `{"type":"ping"}`,
			want:   0,
		},
		{
			name:   "头帧（只有 role）不计入",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{"choices":[{"delta":{"role":"assistant"}}]}`,
			want:   0,
		},
		{
			name:   "终止哨兵不计入",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `[DONE]`,
			want:   0,
		},
		{
			name:   "usage 帧不计入",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22}}`,
			want:   0,
		},
		{
			name:   "anthropic message_start 的 usage 外壳不计入",
			family: FamilyAnthropic,
			event:  "message_start",
			data:   `{"type":"message_start","message":{"usage":{"input_tokens":42}}}`,
			want:   0,
		},
		{
			name:   "SSE 前缀与 JSON 外壳不计入（只算 delta 文本）",
			family: FamilyAnthropic,
			event:  "content_block_delta",
			data:   `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			want:   2,
		},
		{
			name:   "非法 JSON 不计入",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{not json`,
			want:   0,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := SemanticPayloadBytes(testCase.family, testCase.event, testCase.data); got != testCase.want {
				t.Fatalf("SemanticPayloadBytes = %d, want %d", got, testCase.want)
			}
		})
	}
}

// TestSemanticPayloadBytesContainerContentIsMetered 钉住容器型内容帧仍能启动时钟。
//
// web_search_tool_result 的 content 是对象集合，标量计量拿不到字节；若无整帧兑底，
// 「只发这类帧的流」会量出 0 ⇒ 时钟永不启动 ⇒ 速率闸永不裁决（静默失效）。
func TestSemanticPayloadBytesContainerContentIsMetered(t *testing.T) {
	data := `{"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","content":[{"type":"web_search_result","url":"https://example.com"}]}}`
	if got := Classify(FamilyAnthropic, "content_block_start", data); got != VerdictContent {
		t.Fatalf("该帧应被判为内容，得 %s", got)
	}
	if got := SemanticPayloadBytes(FamilyAnthropic, "content_block_start", data); got <= 0 {
		t.Fatalf("容器型内容帧必须量出非零字节，得 %d", got)
	}
}

// TestLadderDoesNotStartWithoutSemanticContent 钉住时钟起点：只有中性帧不启动时钟。
func TestLadderDoesNotStartWithoutSemanticContent(t *testing.T) {
	ladder := newLadderWithStages(1000, testStages())
	if ladder.Started() {
		t.Fatal("新构造的速率闸不应已启动")
	}
	// 中性帧（心跳）不产出语义 payload，也就不该启动时钟。
	ladder.Observe(SemanticPayloadBytes(FamilyAnthropic, "", `{"type":"ping"}`), time.Now())
	if ladder.Started() {
		t.Fatal("只有中性帧时时钟不得启动（否则「先发心跳再正常吐字」会被算成低速）")
	}
	ladder.Observe(1, time.Now())
	if !ladder.Started() {
		t.Fatal("首个非零语义 payload 应启动时钟")
	}
}

// TestLadderDiscreteCheckpoints 钉住三档的判定顺序与阈值都取自「实际流逝」的分母。
func TestLadderDiscreteCheckpoints(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	stages := testStages()

	t.Run("首档按 2θ 放行", func(t *testing.T) {
		// θ=1000 B/s，首档 20ms：需 > 1000×2×0.02 = 40 字节。
		ladder := newLadderWithStages(1000, stages)
		ladder.Observe(41, base)
		if got := ladder.Evaluate(base.Add(20 * time.Millisecond)); got != LadderCommit {
			t.Fatalf("首档达标应放行，得 %s", got)
		}
	})

	t.Run("首档恰好等于阈值时不放行（用严格大于）", func(t *testing.T) {
		ladder := newLadderWithStages(1000, stages)
		ladder.Observe(40, base)
		if got := ladder.Evaluate(base.Add(20 * time.Millisecond)); got != LadderContinue {
			t.Fatalf("恰好等于阈值应保守判继续，得 %s", got)
		}
	})

	t.Run("首档未达标进次档，次档按 θ 放行", func(t *testing.T) {
		// 次档 60ms：需 > 1000×0.06 = 60 字节。首档 41 字节会达标，故先给 10 字节。
		ladder := newLadderWithStages(1000, stages)
		ladder.Observe(10, base)
		if got := ladder.Evaluate(base.Add(20 * time.Millisecond)); got != LadderContinue {
			t.Fatalf("首档 10 字节（<40）应继续，得 %s", got)
		}
		if ladder.Stage() != 1 {
			t.Fatalf("首档未过后档位应为 1，得 %d", ladder.Stage())
		}
		ladder.Observe(55, base.Add(30*time.Millisecond))
		if got := ladder.Evaluate(base.Add(60 * time.Millisecond)); got != LadderCommit {
			t.Fatalf("次档累计 65 字节（>60）应放行，得 %s", got)
		}
	})

	t.Run("三档皆不过判慢", func(t *testing.T) {
		ladder := newLadderWithStages(1000, stages)
		ladder.Observe(5, base)
		if got := ladder.Evaluate(base.Add(20 * time.Millisecond)); got != LadderContinue {
			t.Fatalf("首档应继续，得 %s", got)
		}
		ladder.Observe(5, base.Add(30*time.Millisecond))
		if got := ladder.Evaluate(base.Add(60 * time.Millisecond)); got != LadderContinue {
			t.Fatalf("次档应继续，得 %s", got)
		}
		// 末档 200ms：需 > 1000×0.2 = 200 字节，实际只有 10。
		if got := ladder.Evaluate(base.Add(200 * time.Millisecond)); got != LadderSlow {
			t.Fatalf("三档皆不过应判慢，得 %s", got)
		}
	})

	t.Run("未到检查点不裁决", func(t *testing.T) {
		ladder := newLadderWithStages(1000, stages)
		ladder.Observe(10000, base)
		if got := ladder.Evaluate(base.Add(19 * time.Millisecond)); got != LadderContinue {
			t.Fatalf("未到检查点不得提前放行（否则 3s 档会成死代码），得 %s", got)
		}
	})

	t.Run("晚判用实际流逝做分母（偏保守）", func(t *testing.T) {
		// 该情形只可能出现在读侧被更大的超时压住时。名义 20ms 的档被拖到 40ms 才判，
		// 分母取 40ms ⇒ 需 > 1000×2×0.04 = 80 字节；只有 41 字节则不达标。
		ladder := newLadderWithStages(1000, stages)
		ladder.Observe(41, base)
		if got := ladder.Evaluate(base.Add(40 * time.Millisecond)); got != LadderContinue {
			t.Fatalf("晚判应偏保守地不提交，得 %s", got)
		}
	})
}

// TestNewLadderDisabled 钉住未启用形态：θ<=0 时返回 nil，且接收者方法安全。
func TestNewLadderDisabled(t *testing.T) {
	ladder := NewLadder(0)
	if ladder.Enabled() {
		t.Fatal("θ=0 应不启用")
	}
	// 全部方法在 nil 接收者上必须安全（门控多处直接调用）。
	ladder.Observe(100, time.Now())
	if ladder.Started() || ladder.PayloadBytes() != 0 || ladder.Stage() != -1 {
		t.Fatal("未启用时不应有时钟、计量或档位")
	}
	if _, ok := ladder.Deadline(); ok {
		t.Fatal("未启用时不应有检查点")
	}
	if got := ladder.Evaluate(time.Now().Add(time.Hour)); got != LadderContinue {
		t.Fatalf("未启用时裁决应为继续，得 %s", got)
	}
}
