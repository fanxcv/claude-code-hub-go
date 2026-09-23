package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件检验竞速的**接线**：条件判定、二选一分派、输家计费的成对性。
// 竞速本身（阈值计时、胜者仲裁、有界引流）由 forward 的 hedge_test.go 覆盖。
//
// 竞速是时间敏感的：夹具把首字节延迟（几十毫秒）与阈值（1 毫秒）拉出量级差，
// 因此判定不依赖机器快慢，也不需要注入时钟（forward 侧已用注入时钟钉过语义）。

const (
	hedgeTestModel = "claude-sonnet-4-5"
	hedgeTestKey   = "fake-client-key"
)

// hedgeTestUpstream 是按可配延迟回流的假上游，记录命中次数。
type hedgeTestUpstream struct {
	server *httptest.Server
	hits   atomic.Int64
}

// newHedgeTestUpstream 建假上游；delay 是「首字节前的等待」，用来让竞速阈值先到期。
//
// holdFor 是「收尾前的挂住时长」：竞速的输家必须在客户端还在读的时候得出结论，
// 否则请求 ctx 一取消，在途输家就被打断（生产里同样如此），也就无所谓计费了。
func newHedgeTestUpstream(t *testing.T, delay time.Duration, text string, holdFor time.Duration) *hedgeTestUpstream {
	t.Helper()
	upstream := &hedgeTestUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstream.hits.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"%s\",\"usage\":{\"input_tokens\":11}}}\n\n",
			hedgeTestModel)
		_, _ = fmt.Fprintf(w,
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"%s\"}}\n\n",
			text)
		_, _ = io.WriteString(w,
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n")
		// 先 flush：不 flush 的话内容会压在 HTTP 缓冲里，竞速的首字节阈值就量不到东西
		// （慢家反而因为「写完即收尾」先到，判定会被夹具本身误导）。
		flusher.Flush()
		if holdFor > 0 {
			time.Sleep(holdFor)
		}
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// hedgeTestCandidates 首轮给 first，竞速备选给 second；两者各带自己的 URL。
type hedgeTestCandidates struct {
	first, second *forward.Candidate
	captures      map[int64]route.Result
}

func (c hedgeTestCandidates) Candidate(
	_ context.Context,
	_ pctx.ProviderSelection,
	_ SelectionFacts,
) (*forward.Candidate, route.Result, error) {
	if c.first == nil {
		return nil, route.Result{}, nil
	}
	return c.first, c.capture(c.first.Provider.ID), nil
}

// Failover 只在备选未被排除时给出：竞速的备选走的正是这条路。
func (c hedgeTestCandidates) Failover(_ context.Context, facts SelectionFacts) (*forward.Candidate, route.Result, error) {
	if c.second == nil {
		return nil, route.Result{}, nil
	}
	for _, id := range facts.ExcludeIDs {
		if id == c.second.Provider.ID {
			return nil, route.Result{}, nil
		}
	}
	return c.second, c.capture(c.second.Provider.ID), nil
}

func (c hedgeTestCandidates) capture(providerID int64) route.Result {
	if captured, ok := c.captures[providerID]; ok {
		return captured
	}
	return route.Result{Provider: &route.Provider{ID: providerID, CostMultiplier: json.Number("1.0")}}
}

// hedgeTestSettings 是可控的系统设置源。
type hedgeTestSettings struct {
	settings store.SystemSettings
	err      error
}

func (f hedgeTestSettings) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	if f.err != nil {
		return nil, f.err
	}
	settings := f.settings
	return &settings, nil
}

// hedgeTestLosers 记录输家成本的写回。
type hedgeTestLosers struct {
	mu     sync.Mutex
	writes []hedgeTestLoserWrite
}

type hedgeTestLoserWrite struct {
	requestID int64
	delta     string
	entry     store.HedgeLoserEntry
}

func (f *hedgeTestLosers) AddHedgeLoserCost(
	_ context.Context,
	id int64,
	deltaCost string,
	entry store.HedgeLoserEntry,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, hedgeTestLoserWrite{requestID: id, delta: deltaCost, entry: entry})
	return nil
}

func (f *hedgeTestLosers) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

// waitFor 有界等待写回达到 want 次（输家计费在后台协程里发生）。
func (f *hedgeTestLosers) waitFor(t *testing.T, want int) []hedgeTestLoserWrite {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		writes := append([]hedgeTestLoserWrite(nil), f.writes...)
		f.mu.Unlock()
		if len(writes) >= want {
			return writes
		}
		if time.Now().After(deadline) {
			t.Fatalf("输家计费未在 3s 内达到 %d 次，实际 %d 次", want, len(writes))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// hedgeTestAssembly 是一次竞速测试的装配参数。
type hedgeTestAssembly struct {
	settings     guard.SettingsSource
	candidates   CandidateSource
	losers       hedgeLoserWriter
	costs        *costResolver
	hedgeEnabled bool
}

// newHedgeTestAssembly 造一个不依赖数据库的数据面。
func newHedgeTestAssembly(t *testing.T, assembly hedgeTestAssembly) (*Handler, *fakeSettler, *fakeMessageWriter) {
	t.Helper()
	dialClient, err := dial.New(dial.Options{})
	if err != nil {
		t.Fatalf("拨号器构造失败: %v", err)
	}
	settings := assembly.settings
	if settings == nil {
		settings = hedgeTestSettings{}
	}
	settler := &fakeSettler{}
	messages := &fakeMessageWriter{}
	auth := fakeAuth{
		user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
		key:  guard.Key{ID: 2, Name: "k", UserID: 1},
	}
	handler, err := New(Options{
		Logger: logx.New(nil),
		Base: guard.Deps{
			Auth:           auth,
			Users:          auth,
			Settings:       settings,
			Sensitive:      fakeEmptySource{},
			Filters:        fakeEmptySource{},
			Provider:       fakeProvider{selection: pctx.ProviderSelection{ProviderID: 7, Name: "假供应商", Type: "claude"}},
			MessageContext: messages,
		},
		Candidates: assembly.candidates,
		Settlers: func(*RequestState) Settler {
			return settler
		},
		Forward: forward.Deps{Dial: dialClient},
		Stream: forward.StreamOptions{
			Budget: gate.DefaultBudget(),
		},
		Hedge: HedgeWiring{
			Enabled: assembly.hedgeEnabled,
			Costs:   assembly.costs,
			Pools:   assembly.losers,
			Logger:  logx.New(nil),
		},
	})
	if err != nil {
		t.Fatalf("数据面构造失败: %v", err)
	}
	return handler, settler, messages
}

// hedgeTestCandidate 造一个指向假上游的候选。
func hedgeTestCandidate(t *testing.T, upstream *hedgeTestUpstream, id int64, firstByteMS int) *forward.Candidate {
	t.Helper()
	return &forward.Candidate{
		Provider: forward.Provider{
			ID:                          id,
			Name:                        fmt.Sprintf("供应商-%d", id),
			Type:                        "claude",
			URL:                         upstream.server.URL,
			Key:                         "fake-upstream-key",
			FirstByteTimeoutStreamingMS: firstByteMS,
		},
	}
}

// hedgeTestStreamRequest 发一条流式请求并读完响应体。
func hedgeTestStreamRequest(t *testing.T, handler *Handler) (int, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	body := fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, hedgeTestModel)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", hedgeTestKey)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	return response.StatusCode, string(payload)
}

// ---- 条件判定 ----

// TestHedgeDecisionConditions 逐条钉住竞速的前置条件（与 Node shouldUseStreamingHedge 对齐）。
func TestHedgeDecisionConditions(t *testing.T) {
	streamBody := []byte(`{"model":"` + hedgeTestModel + `","stream":true}`)
	nonStreamBody := []byte(`{"model":"` + hedgeTestModel + `"}`)

	newHandler := func(t *testing.T, settings guard.SettingsSource, enabled bool) *Handler {
		t.Helper()
		handler, _, _ := newHedgeTestAssembly(t, hedgeTestAssembly{
			settings:     settings,
			candidates:   hedgeTestCandidates{},
			losers:       &hedgeTestLosers{},
			costs:        newTestResolverOnly(t),
			hedgeEnabled: enabled,
		})
		return handler
	}

	cases := []struct {
		name      string
		enabled   bool
		preset    guard.Preset
		body      []byte
		firstByte int
		settings  store.SystemSettings
	}{
		{name: "全满足即开", enabled: true, preset: guard.PresetChat, body: streamBody, firstByte: 100,
			settings: store.SystemSettings{LegacyHedgeMaxInFlight: 2, BillHedgeLosers: true}},
		{name: "未接线即关", enabled: false, preset: guard.PresetChat, body: streamBody, firstByte: 100},
		{name: "原始透传端点关", enabled: true, preset: guard.PresetRawPassthrough, body: streamBody, firstByte: 100},
		{name: "非流式关", enabled: true, preset: guard.PresetChat, body: nonStreamBody, firstByte: 100},
		{name: "无 stream 标记关", enabled: true, preset: guard.PresetChat, body: []byte(`{"model":"m"}`), firstByte: 100},
		{name: "首字节阈值为零关", enabled: true, preset: guard.PresetChat, body: streamBody, firstByte: 0},
		{name: "discovery 开启时关", enabled: true, preset: guard.PresetChat, body: streamBody, firstByte: 100,
			settings: store.SystemSettings{DiscoveryEnabled: true, LegacyHedgeMaxInFlight: 2}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := newHandler(t, hedgeTestSettings{settings: testCase.settings}, testCase.enabled)
			spec := routeSpec{Policy: guard.Policy{Preset: testCase.preset}}
			deps := forward.Deps{Facts: forward.PlanFacts{Client: forward.ClientRequest{Body: testCase.body}}}
			candidate := &forward.Candidate{Provider: forward.Provider{ID: 7, FirstByteTimeoutStreamingMS: testCase.firstByte}}
			state := &RequestState{Model: hedgeTestModel}
			_, enabled := handler.hedgeDecision(context.Background(), spec, deps, candidate, state)
			want := testCase.name == "全满足即开"
			if enabled != want {
				t.Fatalf("判定 = %v，期望 %v", enabled, want)
			}
		})
	}
}

// TestHedgeDecisionDisabledForSlowProbe 钉住「低速隔离的探针请求不开竞速」。
//
// 为何单独成例：其余条件与「全满足即开」逐字相同，只多一个探针标记——竞速一旦开启，
// 首字节阈值到期就会并行起第二家，快的那家先赢即取消探针 attempt，该组合拿不到干净样本，
// 隔离阶梯永远抬不回档（见 route.quarantinePermilleForStreak）。
//
// 反证：摘掉 hedgeDecision 里的 slowProbeTarget 判据 ⇒ 本例红。
func TestHedgeDecisionDisabledForSlowProbe(t *testing.T) {
	handler, _, _ := newHedgeTestAssembly(t, hedgeTestAssembly{
		settings:     hedgeTestSettings{settings: store.SystemSettings{LegacyHedgeMaxInFlight: 2, BillHedgeLosers: true}},
		candidates:   hedgeTestCandidates{},
		losers:       &hedgeTestLosers{},
		costs:        newTestResolverOnly(t),
		hedgeEnabled: true,
	})
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("请求上下文构造失败: %v", err)
	}
	pc.SetProvider(pctx.ProviderSelection{ProviderID: 7, SlowProbe: true})
	deps := forward.Deps{Facts: forward.PlanFacts{Client: forward.ClientRequest{
		Body: []byte(`{"model":"` + hedgeTestModel + `","stream":true}`),
	}}}
	candidate := &forward.Candidate{Provider: forward.Provider{ID: 7, FirstByteTimeoutStreamingMS: 100}}
	if _, enabled := handler.hedgeDecision(
		context.Background(), routeSpec{Policy: guard.ChatPolicy()}, deps, candidate,
		&RequestState{PC: pc},
	); enabled {
		t.Fatal("探针请求必须不开竞速（否则探针被快家取消，该组合拿不到干净样本）")
	}
}

// TestHedgeDecisionClampsMaxInFlight 钉住并发上限的钳制（Node clampLegacyHedgeMaxInFlight）。
func TestHedgeDecisionClampsMaxInFlight(t *testing.T) {
	for _, testCase := range []struct {
		configured int
		want       int
	}{
		{configured: 0, want: forward.DefaultHedgeMaxInFlight},
		{configured: -3, want: forward.DefaultHedgeMaxInFlight},
		{configured: 1, want: 1},
		{configured: 3, want: 3},
		{configured: 9, want: 4},
	} {
		t.Run(fmt.Sprintf("configured=%d", testCase.configured), func(t *testing.T) {
			handler, _, _ := newHedgeTestAssembly(t, hedgeTestAssembly{
				settings:     hedgeTestSettings{settings: store.SystemSettings{LegacyHedgeMaxInFlight: testCase.configured}},
				candidates:   hedgeTestCandidates{},
				losers:       &hedgeTestLosers{},
				costs:        newTestResolverOnly(t),
				hedgeEnabled: true,
			})
			deps := forward.Deps{Facts: forward.PlanFacts{Client: forward.ClientRequest{
				Body: []byte(`{"stream":true}`),
			}}}
			candidate := &forward.Candidate{Provider: forward.Provider{ID: 7, FirstByteTimeoutStreamingMS: 100}}
			options, enabled := handler.hedgeDecision(
				context.Background(), routeSpec{Policy: guard.ChatPolicy()}, deps, candidate, &RequestState{})
			if !enabled {
				t.Fatal("条件全满足时应当开竞速")
			}
			if options.MaxInFlight != testCase.want {
				t.Fatalf("MaxInFlight = %d，期望 %d", options.MaxInFlight, testCase.want)
			}
		})
	}
}

// TestHedgeDecisionClosedOnSettingsFailure 钉住「读不到设置就不开」的保守侧。
func TestHedgeDecisionClosedOnSettingsFailure(t *testing.T) {
	handler, _, _ := newHedgeTestAssembly(t, hedgeTestAssembly{
		settings:     hedgeTestSettings{err: fmt.Errorf("设置不可用")},
		candidates:   hedgeTestCandidates{},
		losers:       &hedgeTestLosers{},
		costs:        newTestResolverOnly(t),
		hedgeEnabled: true,
	})
	deps := forward.Deps{Facts: forward.PlanFacts{Client: forward.ClientRequest{Body: []byte(`{"stream":true}`)}}}
	candidate := &forward.Candidate{Provider: forward.Provider{ID: 7, FirstByteTimeoutStreamingMS: 100}}
	if _, enabled := handler.hedgeDecision(
		context.Background(), routeSpec{Policy: guard.ChatPolicy()}, deps, candidate, &RequestState{},
	); enabled {
		t.Fatal("设置读失败时必须不开竞速")
	}
}

// TestHedgeDecisionWithoutBillingSeam 钉住「计费缝缺失不影响开竞速」：Node 的输家计费是可选
// 的，缺它只该退化为「输家直接取消」，而不是整条竞速失效。
func TestHedgeDecisionWithoutBillingSeam(t *testing.T) {
	handler, _, _ := newHedgeTestAssembly(t, hedgeTestAssembly{
		settings:     hedgeTestSettings{settings: store.SystemSettings{BillHedgeLosers: true}},
		candidates:   hedgeTestCandidates{},
		losers:       nil,
		costs:        nil,
		hedgeEnabled: true,
	})
	deps := forward.Deps{Facts: forward.PlanFacts{Client: forward.ClientRequest{Body: []byte(`{"stream":true}`)}}}
	candidate := &forward.Candidate{Provider: forward.Provider{ID: 7, FirstByteTimeoutStreamingMS: 100}}
	options, enabled := handler.hedgeDecision(
		context.Background(), routeSpec{Policy: guard.ChatPolicy()}, deps, candidate, &RequestState{})
	if !enabled {
		t.Fatal("计费缝缺失不应关掉竞速")
	}
	if options.LoserBiller != nil {
		t.Fatal("计费缝缺失时不应给出输家计费器")
	}
}

// ---- 分派与竞速 ----

// TestHedgeDisabledSendsSingleUpstreamRequest 钉住未接线时只打一次上游（今天的行为）。
func TestHedgeDisabledSendsSingleUpstreamRequest(t *testing.T) {
	slow := newHedgeTestUpstream(t, 25*time.Millisecond, "慢家", 0)
	fast := newHedgeTestUpstream(t, 0, "快家", 150*time.Millisecond)
	first := hedgeTestCandidate(t, slow, 7, 1)
	second := hedgeTestCandidate(t, fast, 8, 1)
	handler, settler, _ := newHedgeTestAssembly(t, hedgeTestAssembly{
		candidates: hedgeTestCandidates{first: first, second: second},
	})

	status, body := hedgeTestStreamRequest(t, handler)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}
	if !strings.Contains(body, "慢家") {
		t.Fatalf("关竞速时应由初始候选作答，收到 %q", body)
	}
	if slow.hits.Load() != 1 || fast.hits.Load() != 0 {
		t.Fatalf("关竞速时上游命中数应为 初始=1 备选=0，实际 %d/%d", slow.hits.Load(), fast.hits.Load())
	}
	outcomes := hedgeTestWaitOutcomes(t, settler, 1)
	if len(outcomes[0].Attempts) != 1 {
		t.Fatalf("关竞速时尝试留痕应只有一条，实际 %d 条：%+v", len(outcomes[0].Attempts), outcomes[0].Attempts)
	}
	if reason := outcomes[0].Attempts[0].Reason; reason == "hedge_winner" {
		t.Fatalf("关竞速时不应出现 hedge_winner，实际 %q", reason)
	}
}

// TestHedgeRaceLaunchesAlternative 钉住开启后阈值到期会启动备选，且胜者是最先给出内容的那个。
func TestHedgeRaceLaunchesAlternative(t *testing.T) {
	slow := newHedgeTestUpstream(t, 20*time.Millisecond, "慢家", 0)
	fast := newHedgeTestUpstream(t, 0, "快家", 200*time.Millisecond)
	first := hedgeTestCandidate(t, slow, 7, 1)
	second := hedgeTestCandidate(t, fast, 8, 1)
	losers := &hedgeTestLosers{}
	// 计费用例必须给价格表：无价格时本来就不写库，那是另一个用例（见下）。
	resolver, _, _ := newTestResolver(t, "redirected", map[string]string{hedgeTestModel: priceJSON}, nil)
	handler, settler, _ := newHedgeTestAssembly(t, hedgeTestAssembly{
		settings:     hedgeTestSettings{settings: store.SystemSettings{BillHedgeLosers: true}},
		candidates:   hedgeTestCandidates{first: first, second: second},
		losers:       losers,
		costs:        resolver,
		hedgeEnabled: true,
	})

	status, body := hedgeTestStreamRequest(t, handler)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}
	if !strings.Contains(body, "快家") {
		t.Fatalf("胜者应为先给出内容的备选，收到 %q", body)
	}
	if slow.hits.Load() != 1 || fast.hits.Load() != 1 {
		t.Fatalf("竞速时两家都应被拨过一次，实际 %d/%d", slow.hits.Load(), fast.hits.Load())
	}
	// 输家成本只写一次，且带的是输家自己的用量。
	writes := losers.waitFor(t, 1)
	if len(writes) != 1 {
		t.Fatalf("输家应只计费一次，实际 %d 次", len(writes))
	}
	if writes[0].entry.ProviderID != 7 || writes[0].entry.AttemptNumber != 1 {
		t.Fatalf("输家条目应指向供应商 7 的第 1 次尝试，收到 %+v", writes[0].entry)
	}
	if writes[0].entry.InputTokens == nil || *writes[0].entry.InputTokens != 11 {
		t.Fatalf("输家输入用量应为 11，收到 %v", writes[0].entry.InputTokens)
	}
	if writes[0].entry.OutputTokens == nil || *writes[0].entry.OutputTokens != 7 {
		t.Fatalf("输家输出用量应为 7，收到 %v", writes[0].entry.OutputTokens)
	}
	// 11 输入 * 1e-6 + 7 输出 * 2e-6 = 0.000025（与胜者同一档口径）。
	if writes[0].delta != "0.000025" {
		t.Fatalf("输家成本应为 0.000025，收到 %q", writes[0].delta)
	}
	if writes[0].requestID != 1 {
		t.Fatalf("输家成本应累加到守卫链开的那一行（1），收到 %d", writes[0].requestID)
	}

	// 留痕：胜者 hedge_winner + 输家 hedge_loser_billed（链上的标记在写库之前就追加）。
	outcomes := hedgeTestWaitOutcomes(t, settler, 1)
	reasons := hedgeTestReasons(outcomes[0].Attempts)
	if !hedgeTestContains(reasons, "hedge_winner") {
		t.Fatalf("留痕应含 hedge_winner，实际 %v", reasons)
	}
	if !hedgeTestContains(reasons, "hedge_loser_billed") {
		t.Fatalf("留痕应含 hedge_loser_billed，实际 %v", reasons)
	}
	winner := hedgeTestAttemptByReason(outcomes[0].Attempts, "hedge_winner")
	if winner.ProviderID != 8 {
		t.Fatalf("胜者应为供应商 8，实际 %d", winner.ProviderID)
	}
}

// TestHedgeSlotSaturated 钉住并发上限为 1 时备选被拒并留痕。
func TestHedgeSlotSaturated(t *testing.T) {
	slow := newHedgeTestUpstream(t, 20*time.Millisecond, "慢家", 0)
	fast := newHedgeTestUpstream(t, 0, "快家", 150*time.Millisecond)
	first := hedgeTestCandidate(t, slow, 7, 1)
	second := hedgeTestCandidate(t, fast, 8, 1)
	handler, settler, _ := newHedgeTestAssembly(t, hedgeTestAssembly{
		settings:     hedgeTestSettings{settings: store.SystemSettings{LegacyHedgeMaxInFlight: 1}},
		candidates:   hedgeTestCandidates{first: first, second: second},
		losers:       &hedgeTestLosers{},
		costs:        newTestResolverOnly(t),
		hedgeEnabled: true,
	})

	status, body := hedgeTestStreamRequest(t, handler)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}
	if !strings.Contains(body, "慢家") {
		t.Fatalf("并发已满时不该启动备选，收到 %q", body)
	}
	if fast.hits.Load() != 0 {
		t.Fatalf("并发已满时备选不该被拨号，实际命中 %d 次", fast.hits.Load())
	}
	outcomes := hedgeTestWaitOutcomes(t, settler, 1)
	// 链上不得出现 hedge_slot_saturated：Node 把饱和记为 routing-trace 事件，
	// provider_chain 里没有这个词；链上的未知 reason 会被分类器判成失败。
	if reasons := hedgeTestReasons(outcomes[0].Attempts); hedgeTestContains(reasons, "hedge_slot_saturated") {
		t.Fatalf("留痕不得含 hedge_slot_saturated（应改为 debug 日志），实际 %v", reasons)
	}
	if reasons := hedgeTestReasons(outcomes[0].Attempts); !hedgeTestContains(reasons, forward.ReasonRequestSuccess) {
		t.Fatalf("胜者留痕应为 %s，实际 %v", forward.ReasonRequestSuccess, reasons)
	}
}

// TestHedgeLoserMarkedInChainAtWinnerCommit 钉住「输家结局在胜者裁决时即落链」。
//
// 场景是慢输家 + 快而短的胜者：胜者的流在几毫秒内结束，输家要到几十毫秒后才得出结论。
// 若等输家自己落链，流终态写下的 provider_chain 就会缺掉输家那一段（dashboard 的竞速视图
// 靠的正是它）；Node 在 abortAttempt 里就是裁决时记录的，故此处必须同序。
func TestHedgeLoserMarkedInChainAtWinnerCommit(t *testing.T) {
	slowLoser := newHedgeTestUpstream(t, 40*time.Millisecond, "慢家", 0)
	fastWinner := newHedgeTestUpstream(t, 0, "快家", 0)
	first := hedgeTestCandidate(t, slowLoser, 7, 1)
	second := hedgeTestCandidate(t, fastWinner, 8, 1)
	handler, settler, _ := newHedgeTestAssembly(t, hedgeTestAssembly{
		settings:     hedgeTestSettings{settings: store.SystemSettings{BillHedgeLosers: true}},
		candidates:   hedgeTestCandidates{first: first, second: second},
		losers:       &hedgeTestLosers{},
		costs:        newTestResolverOnly(t),
		hedgeEnabled: true,
	})

	status, body := hedgeTestStreamRequest(t, handler)
	if status != http.StatusOK || !strings.Contains(body, "快家") {
		t.Fatalf("胜者应为快家（状态 %d，正文 %q）", status, body)
	}
	// 快照取自流终态（胜者的流已结束、输家尚未得出结论）。
	outcomes := hedgeTestWaitOutcomes(t, settler, 1)
	reasons := hedgeTestReasons(outcomes[0].Attempts)
	if !hedgeTestContains(reasons, "hedge_winner") {
		t.Fatalf("留痕应含 hedge_winner，实际 %v", reasons)
	}
	if !hedgeTestContains(reasons, "hedge_loser_billed") {
		t.Fatalf("输家结局应随裁决落链（不等输家自结），实际 %v", reasons)
	}
	// 同一 attempt 不得落第二条：留痕里输家只应出现一次。
	count := 0
	for _, attempt := range outcomes[0].Attempts {
		if attempt.ProviderID == 7 {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("输家留痕应恰有一条，实际 %d 条：%+v", count, outcomes[0].Attempts)
	}
}

// TestHedgeLoserNotBilledWithoutPrice 钉住「无价格即不写库」：链上仍标记竞速输家，
// 但成本无处可算时不落库（与 Node 的 `if (!cost.gt(0)) return null` 同效）。
func TestHedgeLoserNotBilledWithoutPrice(t *testing.T) {
	slow := newHedgeTestUpstream(t, 20*time.Millisecond, "慢家", 0)
	fast := newHedgeTestUpstream(t, 0, "快家", 200*time.Millisecond)
	first := hedgeTestCandidate(t, slow, 7, 1)
	second := hedgeTestCandidate(t, fast, 8, 1)
	losers := &hedgeTestLosers{}
	// 价格表里没有任何模型：取价必然落空。
	resolver, _, _ := newTestResolver(t, "redirected", map[string]string{}, map[string]float64{})
	handler, settler, _ := newHedgeTestAssembly(t, hedgeTestAssembly{
		settings:     hedgeTestSettings{settings: store.SystemSettings{BillHedgeLosers: true}},
		candidates:   hedgeTestCandidates{first: first, second: second},
		losers:       losers,
		costs:        resolver,
		hedgeEnabled: true,
	})

	if status, _ := hedgeTestStreamRequest(t, handler); status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}
	outcomes := hedgeTestWaitOutcomes(t, settler, 1)
	if reasons := hedgeTestReasons(outcomes[0].Attempts); !hedgeTestContains(reasons, "hedge_loser_billed") {
		t.Fatalf("链上仍应标记输家，实际 %v", reasons)
	}
	// 给后台 drain 一点时间，确认它确实没有写库。
	time.Sleep(50 * time.Millisecond)
	if count := losers.count(); count != 0 {
		t.Fatalf("无价格时不应写库，实际写了 %d 次", count)
	}
}

// ---- provider_chain 落库形态 ----

// TestProviderChainEncodesHedgeReasons 钉住三种竞速结局原因都能进 provider_chain 的形态。
//
// 饱和（原 hedge_slot_saturated）**不在其中**：Node 只把它记进 routing-trace，
// 写进链会让公开状态分类器把它当失败（见 forward/chainreason.go 的词表说明）。
func TestProviderChainEncodesHedgeReasons(t *testing.T) {
	settler := &storeSettler{state: &RequestState{StartedAt: time.Now()}, logger: logx.New(nil)}
	attempts := []forward.AttemptOutcome{
		{ProviderID: 7, ProviderName: "甲", Attempt: 1, Reason: "hedge_loser_cancelled"},
		{ProviderID: 8, ProviderName: "乙", Attempt: 2, Reason: "hedge_winner"},
		{ProviderID: 9, ProviderName: "丙", Attempt: 3, Reason: "hedge_loser_billed"},
	}
	payload := settler.providerChain(attempts)
	for _, want := range []string{
		"hedge_loser_cancelled", "hedge_winner", "hedge_loser_billed",
	} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("provider_chain 缺少 %q：%s", want, payload)
		}
	}
	var decoded []map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("provider_chain 不是合法 JSON: %v", err)
	}
	if len(decoded) != len(attempts) {
		t.Fatalf("provider_chain 应有 %d 项，实际 %d", len(attempts), len(decoded))
	}
	if decoded[1]["reason"] != "hedge_winner" || decoded[1]["id"] != float64(8) {
		t.Fatalf("第二项应为供应商 8 的 hedge_winner，收到 %v", decoded[1])
	}
}

// ---- 夹具 ----

// newTestResolverOnly 造一个「有价格表但没有该模型」的解析器：只用于判定与分派测试，
// 这些用例不关心金额。
func newTestResolverOnly(t *testing.T) *costResolver {
	t.Helper()
	resolver, _, _ := newTestResolver(t, "redirected", map[string]string{}, map[string]float64{})
	return resolver
}

// hedgeTestWaitOutcomes 等流式结算落地并返回（有界）。
func hedgeTestWaitOutcomes(t *testing.T, settler *fakeSettler, want int) []forward.StreamOutcome {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		settler.mu.Lock()
		outcomes := append([]forward.StreamOutcome(nil), settler.stream...)
		settler.mu.Unlock()
		if len(outcomes) >= want {
			return outcomes
		}
		if time.Now().After(deadline) {
			t.Fatalf("流式结算未在 3s 内落地 %d 次，实际 %d 次", want, len(outcomes))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// hedgeTestReasons 取留痕里的原因列表。
func hedgeTestReasons(attempts []forward.AttemptOutcome) []string {
	reasons := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		reasons = append(reasons, attempt.Reason)
	}
	return reasons
}

// hedgeTestAttemptByReason 按原因取一条留痕。
func hedgeTestAttemptByReason(attempts []forward.AttemptOutcome, reason string) forward.AttemptOutcome {
	for _, attempt := range attempts {
		if attempt.Reason == reason {
			return attempt
		}
	}
	return forward.AttemptOutcome{}
}

func hedgeTestContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
