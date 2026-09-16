package forward

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住「状态型字段跨线无承载位」在**候选层**的处置。
//
// 判据本身（哪个字段、哪个方言判、store:false 不判）由 plan_stateful_test.go 钉住；本文件钉的是
// 拒绝的**作用域**：它是候选级的，不是请求级的。
//
// 为什么必须候选级：拒绝的成因是「这个候选会转正文」，而不是「这份正文有问题」。若把它当成请求级
// 结论，候选池里明明有能承载的原生同线供应商也会被 400——把可用性白扔掉。反过来，全池都不可服务时
// 必须仍是 400 + 字段名（客户端据此改道），不能退化成「供应商耗尽」的 5xx（那会让人以为该重试）。

// countingServer 是「不该被拨号」的上游：记录命中次数并返回固定 200。
func countingServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"must-not-be-called"}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// statefulCandidate 构造一个指定线别的候选。
func statefulCandidate(id int64, name string, providerType convert.ProviderType, url string) *Candidate {
	return &Candidate{
		Provider: Provider{
			ID:   id,
			Name: name,
			Type: providerType,
			Key:  "sk-test",
			URL:  url,
			// 复用测试夹具的指针辅助：重试上限 1，避免用例里出现无意义的等待。
			MaxRetryAttempts: intPointer(1),
		},
		ConversionEnabled: true,
	}
}

// TestForwardSkipsCandidateThatCannotCarryStatefulFields 是那次可用性回归的复现用例。
//
// 候选池 = [需跨线的 openai-compatible, 原生 Responses 的 codex]，客户端是带
// previous_response_id 的 Responses 请求。期望：跳过首个候选、由原生候选作答，既不是 400，
// 也不向上游发过那份「缺了历史的对话」。
func TestForwardSkipsCandidateThatCannotCarryStatefulFields(t *testing.T) {
	var crossLineHits int32
	crossLine := countingServer(t, &crossLineHits)
	native := fakeServer(t, http.StatusOK, `{"id":"resp_1","output":[{"type":"message","content":[]}]}`)

	initial := statefulCandidate(1, "跨线供应商", convert.ProviderOpenAICompatible, crossLine.URL)
	nativeCandidate := statefulCandidate(2, "原生 Responses 供应商", convert.ProviderCodex, native.URL)

	var excluded []int64
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  PlanFacts{Client: responsesClient(statefulResponsesBody)},
		Limits: Limits{RetryDelay: time.Millisecond},
		Select: func(_ context.Context, excludeIDs []int64) (*Candidate, error) {
			excluded = append(excluded, excludeIDs...)
			return nativeCandidate, nil
		},
	}

	result, err := Forward(context.Background(), nil, initial, deps)
	if err != nil {
		t.Fatalf("池中有能承载的候选时不该 fail-closed：%v", err)
	}
	if result.Provider.ID != 2 {
		t.Fatalf("应由原生候选作答，实际 provider#%d", result.Provider.ID)
	}
	if hits := atomic.LoadInt32(&crossLineHits); hits != 0 {
		t.Fatalf("不可服务的候选被拨号 %d 次", hits)
	}
	if len(excluded) == 0 || excluded[0] != 1 {
		t.Fatalf("换候选时未把不可服务的供应商记入排除集：%v", excluded)
	}
	if !strings.Contains(string(result.Body), "resp_1") {
		t.Fatalf("正文 = %s", result.Body)
	}
}

// TestForwardReportsStatefulRejectionWhenNoCandidateCanCarry 钉住穷尽条件：全池候选都要跨线时，
// 结论仍是「状态型字段无承载位」（400 + 字段名），而不是「供应商耗尽」。
func TestForwardReportsStatefulRejectionWhenNoCandidateCanCarry(t *testing.T) {
	var hits int32
	unservable := countingServer(t, &hits)

	initial := statefulCandidate(1, "跨线甲", convert.ProviderOpenAICompatible, unservable.URL)
	second := statefulCandidate(2, "跨线乙", convert.ProviderClaude, unservable.URL)

	selectCalls := 0
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  PlanFacts{Client: responsesClient(statefulResponsesBody)},
		Limits: Limits{RetryDelay: time.Millisecond},
		Select: func(_ context.Context, _ []int64) (*Candidate, error) {
			selectCalls++
			if selectCalls == 1 {
				return second, nil
			}
			return nil, nil
		},
	}

	result, err := Forward(context.Background(), nil, initial, deps)
	if err == nil {
		t.Fatal("全池候选都不可服务时应 fail-closed")
	}
	var rejected *StatefulConversionError
	if !errors.As(err, &rejected) {
		t.Fatalf("错误应带冲突字段名：%v", err)
	}
	if rejected.Field != "previous_response_id" {
		t.Fatalf("字段名 = %q", rejected.Field)
	}
	if !errors.Is(err, ErrStatefulConversion) {
		t.Fatalf("错误应可 errors.Is 成 ErrStatefulConversion：%v", err)
	}
	if errors.Is(err, ErrProvidersExhausted) {
		t.Fatal("不得退化成「供应商耗尽」——客户端据此选择的处置完全不同")
	}
	if hits := atomic.LoadInt32(&hits); hits != 0 {
		t.Fatalf("不可服务的候选被拨号 %d 次", hits)
	}
	if len(result.Attempts) != 0 {
		t.Fatalf("跳过不产生尝试留痕，实际 %d 条", len(result.Attempts))
	}
}

// TestForwardKeepsUpstreamFailureOverStatefulSkip 钉住「跳过不等于成功」：换到的候选真失败时，
// 结论是那次上游失败，而不是被跳过的候选带来的状态型拒绝。
func TestForwardKeepsUpstreamFailureOverStatefulSkip(t *testing.T) {
	var crossLineHits int32
	crossLine := countingServer(t, &crossLineHits)
	failing := fakeServer(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)

	initial := statefulCandidate(1, "跨线供应商", convert.ProviderOpenAICompatible, crossLine.URL)
	nativeCandidate := statefulCandidate(2, "原生 Responses 供应商", convert.ProviderCodex, failing.URL)

	selectCalls := 0
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  PlanFacts{Client: responsesClient(statefulResponsesBody)},
		Limits: Limits{RetryDelay: time.Millisecond},
		Select: func(_ context.Context, _ []int64) (*Candidate, error) {
			selectCalls++
			if selectCalls == 1 {
				return nativeCandidate, nil
			}
			return nil, nil
		},
	}

	result, err := Forward(context.Background(), nil, initial, deps)
	if err == nil {
		t.Fatal("应返回失败")
	}
	var rejected *StatefulConversionError
	if errors.As(err, &rejected) {
		t.Fatalf("换到的候选真失败时不该报状态型拒绝：%v", err)
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("错误应带失败归因：%v", err)
	}
	if failure.Category != CategoryProviderError {
		t.Fatalf("分类 = %v", failure.Category)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].ProviderID != 2 {
		t.Fatalf("留痕应只含真正拨号的候选：%+v", result.Attempts)
	}
}

// TestForwardDoesNotSkipServableStatefulCombinations 钉住「跳过判据不因本次改动放宽」：
// 原生对、store:false、非 OpenAI 客户端、转换开关关闭——四种组合都必须照首个候选作答。
func TestForwardDoesNotSkipServableStatefulCombinations(t *testing.T) {
	claudeLineWithSameField := newClaudeRequest(
		`{"model":"claude-sonnet-4-5-20250929","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"previous_response_id":"resp_1"}`,
	)

	cases := []struct {
		name          string
		client        ClientRequest
		providerType  convert.ProviderType
		conversionOff bool
	}{
		{
			name:         "原生对：responses → codex 不转换，字段原样承载",
			client:       responsesClient(statefulResponsesBody),
			providerType: convert.ProviderCodex,
		},
		{
			name:         "store:false 不判冲突",
			client:       responsesClient(`{"model":"gpt-5","input":"hi","store":false}`),
			providerType: convert.ProviderOpenAICompatible,
		},
		{
			name:         "claude 客户端里的同名字段是杂项键",
			client:       claudeLineWithSameField,
			providerType: convert.ProviderOpenAICompatible,
		},
		{
			name:          "转换开关关闭时不转正文",
			client:        responsesClient(statefulResponsesBody),
			providerType:  convert.ProviderOpenAICompatible,
			conversionOff: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			server := countingServer(t, &hits)

			candidate := statefulCandidate(1, "首选", tc.providerType, server.URL)
			candidate.ConversionEnabled = !tc.conversionOff

			switched := false
			deps := Deps{
				Dial:   newTestDial(t),
				Facts:  PlanFacts{Client: tc.client},
				Limits: Limits{RetryDelay: time.Millisecond},
				Select: func(context.Context, []int64) (*Candidate, error) {
					switched = true
					return nil, nil
				},
			}

			if _, err := Forward(context.Background(), nil, candidate, deps); err != nil {
				t.Fatalf("该组合不该被跳过：%v", err)
			}
			if hits := atomic.LoadInt32(&hits); hits != 1 {
				t.Fatalf("首个候选应被拨号 1 次，实际 %d", hits)
			}
			if switched {
				t.Fatal("不该换候选")
			}
		})
	}
}

// responsesStreamBody 是带状态型字段的 Responses 流式请求体。
const responsesStreamBody = `{"model":"gpt-5","stream":true,"previous_response_id":"resp_123","input":"继续"}`

// responsesStreamChunks 是一条最小的 Responses SSE（native 对，不转换）。
func responsesStreamChunks() []string {
	return []string{
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"pong\"}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":2}}}\n\n",
	}
}

// TestHedgeSkipsUnservableInitialAndRacesTheNativeCandidate 钉住竞速路径同样跳过，
// 且跳过发生的候选不会被当成竞速输家留痕。
func TestHedgeSkipsUnservableInitialAndRacesTheNativeCandidate(t *testing.T) {
	harness := newHedgeHarness(t)
	harness.deps.Facts = PlanFacts{Client: responsesClient(responsesStreamBody)}
	harness.options.Format = convert.FormatResponse

	var crossLineHits int32
	crossLine := countingServer(t, &crossLineHits)
	native := streamServer(t, "text/event-stream", responsesStreamChunks(), true)

	initial := statefulCandidate(1, "跨线供应商", convert.ProviderOpenAICompatible, crossLine.URL)
	initial.Provider.FirstByteTimeoutStreamingMS = 0
	harness.setSelect([]*Candidate{statefulCandidate(2, "原生 Responses 供应商", convert.ProviderCodex, native.URL)})

	result, err := ForwardStreamHedge(context.Background(), harness.pc, initial, harness.deps, harness.options, harness.cfg)
	if err != nil {
		t.Fatalf("池中有能承载的候选时不该失败：%v", err)
	}
	if result == nil || result.Stream == nil {
		t.Fatal("应拿到可读的流")
	}
	if hits := atomic.LoadInt32(&crossLineHits); hits != 0 {
		t.Fatalf("不可服务的候选被拨号 %d 次", hits)
	}

	received, _ := consumeStream(t, result.Stream)
	if !strings.Contains(string(received), "pong") {
		t.Fatalf("正文 = %q", received)
	}

	// 被跳过的候选不得出现在留痕里：它从未拨号，既不是成功也不是输家。
	for _, outcome := range result.Attempts {
		if outcome.ProviderID == 1 {
			t.Fatalf("被跳过的候选不该留痕：%+v", outcome)
		}
	}
}

// TestHedgeExhaustsToStatefulRejectionWithoutCountingHealthFailure 钉住竞速穷尽时的两条纪律：
// 结论是「无承载位」（不是 5xx），且跳过不计供应商健康度失败。
func TestHedgeExhaustsToStatefulRejectionWithoutCountingHealthFailure(t *testing.T) {
	harness := newHedgeHarness(t)
	harness.deps.Facts = PlanFacts{Client: responsesClient(responsesStreamBody)}
	harness.options.Format = convert.FormatResponse
	// 打开「网络类失败计入熔断」，把「跳过被当成供应商故障」这件事变成可观测的副作用。
	harness.deps.CountNetworkFailureTowardCircuit = true

	var healthFailures int32
	harness.deps.RecordFailure = func(context.Context, *Failure) {
		atomic.AddInt32(&healthFailures, 1)
	}

	var hits int32
	unservable := countingServer(t, &hits)
	initial := statefulCandidate(1, "跨线供应商", convert.ProviderOpenAICompatible, unservable.URL)
	harness.setSelect(nil)

	result, err := ForwardStreamHedge(context.Background(), harness.pc, initial, harness.deps, harness.options, harness.cfg)
	if result != nil {
		t.Fatalf("穷尽时不该有结果：%+v", result)
	}
	var rejected *StatefulConversionError
	if !errors.As(err, &rejected) {
		t.Fatalf("穷尽时应返回状态型拒绝（400 + 字段名）：%v", err)
	}
	if rejected.Field != "previous_response_id" {
		t.Fatalf("字段名 = %q", rejected.Field)
	}
	if hits := atomic.LoadInt32(&hits); hits != 0 {
		t.Fatalf("不可服务的候选被拨号 %d 次", hits)
	}
	if failures := atomic.LoadInt32(&healthFailures); failures != 0 {
		t.Fatalf("跳过候选不得计入供应商健康度，实际记了 %d 次", failures)
	}
}
