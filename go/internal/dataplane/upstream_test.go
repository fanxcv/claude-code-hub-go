package dataplane

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 本文件钉住非流式终态的两个事实来源：用量/实际模型来自**上游正文**（按上游协议线解码），
// 耗时起点是**请求进入数据面**而不是某次尝试。
//
// 为什么值得单测：这两处在集成测试里只能看到「列非空」，而列非空的错值（例如把尝试耗时
// 当请求耗时、把 chat 的 prompt_tokens 当输入 token）同样非空，只有断言具体数字才拦得住。

// TestNonStreamFactsDecodePerUpstreamProtocol 三线各解一次，断言归一后的用量与模型名。
func TestNonStreamFactsDecodePerUpstreamProtocol(t *testing.T) {
	cases := []struct {
		name     string
		protocol convert.WireProtocol
		body     string
		model    string
		input    float64
		output   float64
		cache    float64
		write    float64
	}{
		{
			name:     "anthropic 线取 usage.input_tokens",
			protocol: convert.ProtocolAnthropicMessages,
			body:     `{"id":"msg_1","model":"claude-sonnet-4-5","usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":5}}`,
			model:    "claude-sonnet-4-5",
			input:    11,
			output:   7,
			cache:    3,
			write:    5,
		},
		{
			name:     "openai-chat 线取 prompt_tokens 并减去 cached_tokens",
			protocol: convert.ProtocolOpenAIChat,
			body:     `{"id":"c1","model":"gpt-5.6","usage":{"prompt_tokens":14,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":4}}}`,
			model:    "gpt-5.6",
			input:    10,
			output:   9,
			cache:    4,
			write:    0,
		},
		{
			name:     "openai-responses 线取 input_tokens 并减去 cached_tokens",
			protocol: convert.ProtocolOpenAIResponses,
			body:     `{"id":"r1","model":"gpt-5.6","usage":{"input_tokens":14,"output_tokens":9,"input_tokens_details":{"cached_tokens":4}}}`,
			model:    "gpt-5.6",
			input:    10,
			output:   9,
			cache:    4,
			write:    0,
		},
		{
			// Gemini 线只借这条只读解码路径给记账用（它不在转换矩阵内，见 codec_gemini.go）。
			// promptTokenCount **包含** cachedContentTokenCount，故输入必须相减（Node: response-handler.ts:6045-6068），
			// 上游模型名取 modelVersion（Node: actual-response-model.ts:145-146）。
			name:     "gemini 线取 usageMetadata 并减去 cachedContentTokenCount",
			protocol: convert.ProtocolGemini,
			body:     `{"candidates":[],"modelVersion":"gemini-2.0-flash-001","usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":2,"cachedContentTokenCount":4}}`,
			model:    "gemini-2.0-flash-001",
			input:    8,
			output:   2,
			cache:    4,
			write:    0,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			plan := &forward.Plan{Protocol: testCase.protocol}
			usage, model := nonStreamFacts(plan, []byte(testCase.body))
			if usage == nil {
				t.Fatal("正文里有 usage，应解出用量")
			}
			if model != testCase.model {
				t.Errorf("实际模型应为 %q，收到 %q", testCase.model, model)
			}
			assertToken(t, "输入", usage.InputTokens, testCase.input)
			assertToken(t, "输出", usage.OutputTokens, testCase.output)
			assertToken(t, "缓存读", usage.CacheReadTokens, testCase.cache)
			assertToken(t, "缓存写", usage.CacheWriteTokens, testCase.write)
		})
	}
}

// TestNonStreamFactsSkipsWhenNoUsage 没有用量时不得给出用量（不能按 0 写行）。
func TestNonStreamFactsSkipsWhenNoUsage(t *testing.T) {
	cases := []struct {
		name string
		plan *forward.Plan
		body string
	}{
		{"正文无 usage 字段", &forward.Plan{Protocol: convert.ProtocolAnthropicMessages}, `{"id":"msg_1","model":"claude-sonnet-4-5","content":[]}`},
		{"usage 里全是 null", &forward.Plan{Protocol: convert.ProtocolAnthropicMessages}, `{"usage":{"input_tokens":null,"output_tokens":null}}`},
		{"正文不是 JSON", &forward.Plan{Protocol: convert.ProtocolAnthropicMessages}, `not json at all`},
		{"正文为空", &forward.Plan{Protocol: convert.ProtocolAnthropicMessages}, ``},
		{"没有计划（无上游协议线可依）", nil, `{"usage":{"input_tokens":11}}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			usage, _ := nonStreamFacts(testCase.plan, []byte(testCase.body))
			if usage != nil {
				t.Fatalf("没有可用量，应返回 nil，收到 %+v", usage)
			}
		})
	}
}

// TestNonStreamDurationStartsAtRequest 耗时起点必须是请求进入数据面的时刻。
func TestNonStreamDurationStartsAtRequest(t *testing.T) {
	started := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	settler := &storeSettler{state: &RequestState{StartedAt: started}}

	// 尝试比请求晚 200ms 开始、晚 250ms 结束：口径是请求起点，故应为 250 而不是 50。
	result := &forward.Result{
		StartedAt: started.Add(200 * time.Millisecond),
		EndedAt:   started.Add(250 * time.Millisecond),
	}
	duration := settler.nonStreamDuration(result)
	if duration == nil {
		t.Fatal("应给出耗时")
	}
	if *duration != 250 {
		t.Errorf("耗时应为 250ms（请求起点起算），收到 %d", *duration)
	}

	// 没有终态时刻时用注入时钟补齐，而不是留空。
	clock := started.Add(400 * time.Millisecond)
	settler.now = func() time.Time { return clock }
	fallback := settler.nonStreamDuration(nil)
	if fallback == nil || *fallback != 400 {
		t.Fatalf("缺终态时刻时应按注入时钟算出 400ms，收到 %v", fallback)
	}

	// 时钟回拨：宁可记 0，不写负耗时。
	settler.now = func() time.Time { return started.Add(-5 * time.Millisecond) }
	if rolled := settler.nonStreamDuration(nil); rolled == nil || *rolled != 0 {
		t.Fatalf("时钟回拨时应记 0ms，收到 %v", rolled)
	}

	// 没有请求起点（未接线/构造异常）时宁可不写，也不凭尝试耗时充数。
	if absent := (&storeSettler{state: &RequestState{}}).nonStreamDuration(nil); absent != nil {
		t.Fatalf("无请求起点时应返回 nil，收到 %v", *absent)
	}
}

func assertToken(t *testing.T, label string, value *float64, want float64) {
	t.Helper()
	switch {
	case value == nil && want == 0:
		// 上游没报的字段保持 nil：不写 0 是刻意的口径。
	case value == nil:
		t.Errorf("%s token 应为 %v，收到 nil", label, want)
	case *value != want:
		t.Errorf("%s token 应为 %v，收到 %v", label, want, *value)
	}
}

// TestSettlementProviderPrefersServingAttempt 钉住**行级供应商 = 实际作答的那一家**。
//
// 真实事故（生产 `message_request` 行 946915，同形态另见 946908 / 946876）：
// 链路为「HC_Chat 上游 500 → ARK Codex 上游 404 → Ollama Codex 竞速取胜（200）」，
// 行的 `ttfb_ms = 18417` 与 Ollama Codex 那次 attempt 的 finish 时刻**逐毫秒吻合**，
// 即真正作答的是 Ollama Codex；但行的 `provider_id = 156`（HC_Chat）——记成了**首个候选**。
//
// 根因：`pctx.SetProvider` 只在 provider 守卫里调用一次，回退/竞速换家后不更新；旧实现让
// 它**覆盖**失败归属，成功路径又从不写作答者。Node 口径见
// `response-handler.ts:1912` 与 `repository/message.ts:632`（「支持更新最终供应商ID」）。
//
// 反证：把 `resolveSettlementProviderID` 里「作答者优先」改回「选中项覆盖」，第一条用例转红。
func TestSettlementProviderPrefersServingAttempt(t *testing.T) {
	newPC := func(t *testing.T, selectedID int64) *pctx.Context {
		t.Helper()
		pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
		if err != nil {
			t.Fatalf("构造 pctx 失败: %v", err)
		}
		if selectedID != 0 {
			pc.SetProvider(pctx.ProviderSelection{ProviderID: selectedID, Name: "首选"})
		}
		return pc
	}

	cases := []struct {
		name      string
		selected  int64
		serving   int64
		failureID int64
		want      int64
	}{
		{
			name:     "作答者是竞速/回退换来的那一家 → 记作答者",
			selected: 156, serving: 145, want: 145,
		},
		{
			name:     "作答者未知（上游前失败）→ 记失败归属",
			selected: 156, failureID: 149, want: 149,
		},
		{
			name:     "两者都未知 → 才兜底到入口选中的候选",
			selected: 156, want: 156,
		},
		{
			name: "连选中项都没有 → 留空（不编造供应商）",
			want: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var failure *forward.Failure
			if tc.failureID != 0 {
				failure = &forward.Failure{ProviderID: tc.failureID, StatusCode: 500}
			}
			settler := &storeSettler{state: &RequestState{}}
			settlement := settler.baseSettlement(newPC(t, tc.selected), failure, tc.serving)
			if tc.want == 0 {
				if settlement.ProviderID != nil {
					t.Fatalf("provider_id 应留空，实际 %d", *settlement.ProviderID)
				}
				return
			}
			if settlement.ProviderID == nil {
				t.Fatalf("provider_id 为空，期望 %d", tc.want)
			}
			if *settlement.ProviderID != tc.want {
				t.Fatalf("provider_id = %d，期望 %d", *settlement.ProviderID, tc.want)
			}
		})
	}

	// 接线钉子：两条终态路径必须把「作答者」传进来。只测纯函数会被「接线漏传」绕过——
	// 本次事故正是接线错，而不是算法错。
	source, err := os.ReadFile("upstream.go")
	if err != nil {
		t.Fatalf("读 upstream.go 失败: %v", err)
	}
	text := string(source)
	for _, want := range []string{
		"s.baseSettlement(pc, failure, servingProviderID(result))",
		"s.baseSettlement(pc, nil, outcome.Provider.ID)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("终态路径未传作答者：找不到 %q（供应商归属会退回首个候选）", want)
		}
	}
}

// TestServingProviderIDReadsResultProvider 钉住作答者的取值来源是 Result.Provider
// （非流式）——它与 affinity 的 WinnerProviderID 同源，不是另一套口径。
func TestServingProviderIDReadsResultProvider(t *testing.T) {
	if got := servingProviderID(nil); got != 0 {
		t.Fatalf("nil 结果应返回 0，实际 %d", got)
	}
	provider := forward.Provider{ID: 145, Name: "Ollama Codex"}
	result := &forward.Result{Provider: provider}
	if got := servingProviderID(result); got != 145 {
		t.Fatalf("作答者 ID = %d，期望 145", got)
	}
}

// TestFailoverCaptureKeepsDecisionContext 钉住**故障转移的链项必须带决策上下文**。
//
// 真实事故：生产 `provider_chain` 里换家得到的链项 decisionContext 恒为零值
// （`totalProviders: 0`、`priorityLevels: null`、`filteredProviders: null`，连
// `requestedModel` 都是空的），界面上就是一片 0——用户看到「决策链没有决策依据」。
//
// 根因：`Failover` 只在 `capture.Provider == nil` 时才用选路器的结果，而 `fromStore` 总是
// 返回带 Provider 的 capture（它只填身份/权重/优先级/倍率/分组），于是选路器算出的
// Context / Method / CircuitState 被**丢掉**。Node 在 failover 路径带上下文
// （`provider-selector.ts:409` 的 failedContext）。
//
// 反证：删掉 merge 分支（回到只判空），本用例转红。
func TestFailoverCaptureKeepsDecisionContext(t *testing.T) {
	// 直接钉住合并语义：两侧都有 Provider 时，选路侧的上下文必须留下。
	storeSide := route.Result{Provider: &route.Provider{ID: 145, Name: "Ollama Codex"}}
	selectorSide := route.Result{
		Provider: &route.Provider{ID: 145, Name: "Ollama Codex"},
		Context: route.DecisionContext{
			RequestedModel: "deepseek-v4-flash",
			TotalProviders: 21,
			TargetType:     "openai-compatible",
		},
		Method:       route.MethodWeightedRandom,
		CircuitState: route.CircuitState("closed"),
	}
	merged := mergeSelectionCapture(storeSide, selectorSide)
	if merged.Context.RequestedModel != "deepseek-v4-flash" || merged.Context.TotalProviders != 21 {
		t.Fatalf("决策上下文被丢掉：%+v", merged.Context)
	}
	if merged.Method != route.MethodWeightedRandom || merged.CircuitState != route.CircuitState("closed") {
		t.Fatalf("选路方式/健康快照被丢掉：method=%q state=%q", merged.Method, merged.CircuitState)
	}
	if merged.Provider == nil || merged.Provider.ID != 145 {
		t.Fatalf("身份被覆盖：%+v", merged.Provider)
	}

	// 选路侧没有 Provider 时（无候选）：整份取选路侧，不能留下空壳。
	empty := mergeSelectionCapture(route.Result{}, selectorSide)
	if empty.Provider == nil || empty.Context.TotalProviders != 21 {
		t.Fatalf("无候选时应整份取选路侧：%+v", empty)
	}
}

// TestTailPreview 钉住「缺用量现场」的尾部截取：它决定现场里能看到什么。
//
// 为何按字符而非字节切：窗口文本是上游 SSE 原文，中英混排时按字节切会把最后一个字符
// 劈成半个 UTF-8 序列，日志里就成了乱码，反而看不出尾部停在哪里。
func TestTailPreview(t *testing.T) {
	cases := []struct {
		name string
		text string
		n    int
		want string
	}{
		{"空文本", "", 10, ""},
		{"非正长度", "abc", 0, ""},
		{"短于上限原样返回", "abc", 10, "abc"},
		{"恰好等长原样返回", "abcde", 5, "abcde"},
		{"超长取末 n 字符", "abcdefg", 3, "efg"},
		{"多字节不劈字符", "中文尾部", 2, "尾部"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tailPreview(c.text, c.n); got != c.want {
				t.Fatalf("tailPreview(%q, %d) = %q，期望 %q", c.text, c.n, got, c.want)
			}
		})
	}
}
