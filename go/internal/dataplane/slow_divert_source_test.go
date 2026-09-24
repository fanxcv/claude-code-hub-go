package dataplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件钉住「因低速被改道」计数（D1）的**取值来源**与端到端落点。
//
// 生产事实（修复前）：当日 slow_rate_cooldown 剔除 844 次，而 Redis 的 cch:slowdivert:* 零键。
// 根因：slowDiverts 只扫 state.selections 的 capture，而主路径那条 capture 由 candidateSource
// 从 DB 投影（route.Result.Context 恒为零值）；选路器算出的 DecisionContext 只经
// pctx.SetSelectionChainEntry 落进请求上下文（guard/adapters_route.go:231）。
//
// 反证：把 slowDiverts 里的 decodeSelectionChainEntry 分支删掉，本文件的主路径用例转红。

// chainEntryProvider 复刻 guard/adapters_route.go 的 provider 步骤：选中后把选路结果投影成
// 选择期链条目装进 pctx。生产里这是链条目的**唯一**写入点，测试沿用同一投影
// （route.Result.ChainItem），不自造第二份形状。
type chainEntryProvider struct {
	selection pctx.ProviderSelection
	entry     []byte
}

func (p chainEntryProvider) Select(_ context.Context, pc *pctx.Context) (pctx.ProviderSelection, error) {
	pc.SetProvider(p.selection)
	pc.SetSelectionChainEntry(p.entry)
	return p.selection, nil
}

// slowDivertChainEntry 造守卫链形态的选择期链条目：167 被会话冷却剔除，156 当选。
func slowDivertChainEntry(t *testing.T, cooledID, winnerID int64) []byte {
	t.Helper()
	payload, err := json.Marshal(route.Result{
		Provider: &route.Provider{ID: winnerID, Name: "当选者", ProviderType: convert.ProviderClaude},
		Reason:   route.ReasonSelectedInitial,
		Method:   route.MethodWeightedRandom,
		Context: route.DecisionContext{
			TotalProviders: 2,
			FilteredProviders: []route.Filtered{
				{ID: cooledID, Reason: route.ReasonSlowRateCooldown},
			},
		},
	}.ChainItem())
	if err != nil {
		t.Fatalf("序列化链条目失败: %v", err)
	}
	return payload
}

// TestSlowDivertsReadsSelectionChainEntry 主路径（守卫链初选）的改道必须从 pctx 链条目取值。
//
// 夹具刻意让 selections 的 capture 保持零值 Context：这正是生产里主路径 capture 的形态
// （candidateSource 只填身份/权重/优先级/倍率/分组，不编造决策上下文）。
func TestSlowDivertsReadsSelectionChainEntry(t *testing.T) {
	settler := chainTestSettler(t)
	settler.state.PC.SetSelectionChainEntry(slowDivertChainEntry(t, 167, 156))
	settler.state.recordSelection(156, route.Result{Provider: &route.Provider{ID: 156, Name: "当选者"}})

	diverts := settler.slowDiverts()
	if len(diverts) != 1 {
		t.Fatalf("主路径应产生一条改道（167 被会话冷却剔除），实际 %d 条：%+v", len(diverts), diverts)
	}
	if diverts[0].ProviderID != 167 || diverts[0].Cause != "cooldown" {
		t.Fatalf("改道条目应为 {167 cooldown}，实际 %+v", diverts[0])
	}
}

// TestSlowDivertsDedupesAcrossSources 同一 (渠道, 成因) 在链条目与无候选留痕里各出现一次时按请求计一次。
func TestSlowDivertsDedupesAcrossSources(t *testing.T) {
	settler := chainTestSettler(t)
	settler.state.PC.SetSelectionChainEntry(slowDivertChainEntry(t, 167, 156))
	settler.state.recordDiverts([]route.Divert{{ProviderID: 167, Cause: route.DivertCauseCooldown}})

	if diverts := settler.slowDiverts(); len(diverts) != 1 {
		t.Fatalf("同一渠道同一成因只应计一次，实际 %+v", diverts)
	}
}

// TestSlowDivertsEmptyWhenNoCooldown 无低速剔除时不得凭空造改道（正常加权落选不算改道）。
func TestSlowDivertsEmptyWhenNoCooldown(t *testing.T) {
	settler := chainTestSettler(t)
	payload, err := json.Marshal(route.Result{
		Provider: &route.Provider{ID: 156, Name: "当选者", ProviderType: convert.ProviderClaude},
		Reason:   route.ReasonSelectedInitial,
		Context:  route.DecisionContext{TotalProviders: 2},
	}.ChainItem())
	if err != nil {
		t.Fatalf("序列化链条目失败: %v", err)
	}
	settler.state.PC.SetSelectionChainEntry(payload)
	settler.state.recordSelection(156, route.Result{Provider: &route.Provider{ID: 156}})

	if diverts := settler.slowDiverts(); len(diverts) != 0 {
		t.Fatalf("无低速剔除时不该有改道，实际 %+v", diverts)
	}
}

// TestMainPathDivertReachesTerminalRecorder 是 D1 的端到端回归：一条真实请求走完
// 守卫链 → 转发 → 终态，改道事实必须到达终态接收面（Redis 桶的入参）。
//
// 与「无可用供应商」分支的分别：这里候选存在、请求成功（200），改道来自**选路期**的冷却剔除。
// 修复前这条路径的 settlement.SlowDiverts 恒为空——链条目根本没被读过。
func TestMainPathDivertReachesTerminalRecorder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)

	handler, writer, recorder := newTerminalCaptureHandler(t, terminalCaptureAssembly{
		upstreamURL: upstream.URL,
		provider: chainEntryProvider{
			selection: pctx.ProviderSelection{ProviderID: 156, Name: "当选者", Type: "claude"},
			entry:     slowDivertChainEntry(t, 167, 156),
		},
	})

	status := terminalCaptureRequest(t, handler, false)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}
	if got := recorder.snapshot(); len(got) != 1 || got[0].ProviderID != 167 || got[0].Cause != "cooldown" {
		t.Fatalf("改道事实应到达终态接收面（167/cooldown），实际 %+v", got)
	}
	if patch, ok := writer.lastPatch(); !ok || len(patch.ProviderChain) == 0 {
		t.Fatalf("主路径应留下 provider_chain，实际 %+v", patch)
	}
}

// terminalCaptureAssembly 是一次「真实结算器 + 替身写面」的装配参数。
type terminalCaptureAssembly struct {
	upstreamURL string
	provider    guard.ProviderSelector
	// firstByteMS > 0 且 hedge 为真时走竞速；候选形状由 terminalCaptureCandidates 给出。
	firstByteMS  int
	hedgeEnabled bool
	settings     guard.SettingsSource
}

// terminalCaptureCandidates 给首轮候选，不做故障转移（竞速耗尽路径由 D2 用例驱动）。
type terminalCaptureCandidates struct {
	url         string
	firstByteMS int
}

func (c terminalCaptureCandidates) Candidate(
	_ context.Context,
	selection pctx.ProviderSelection,
	_ SelectionFacts,
) (*forward.Candidate, route.Result, error) {
	return &forward.Candidate{Provider: forward.Provider{
		ID:                          selection.ProviderID,
		Name:                        selection.Name,
		Type:                        convert.ProviderClaude,
		URL:                         c.url,
		Key:                         "fake-upstream-key",
		FirstByteTimeoutStreamingMS: c.firstByteMS,
	}}, route.Result{Provider: &route.Provider{ID: selection.ProviderID, Name: selection.Name}}, nil
}

func (c terminalCaptureCandidates) Failover(context.Context, SelectionFacts) (*forward.Candidate, route.Result, error) {
	return nil, route.Result{}, nil
}

// terminalCaptureWriter 是终态写面的替身：记录 patch，不碰数据库。
type terminalCaptureWriter struct {
	mu      sync.Mutex
	patches []store.DetailsPatch
}

func (w *terminalCaptureWriter) CreateMessageRequest(context.Context, store.CreateMessageRequestData) (store.MessageRequest, error) {
	return store.MessageRequest{}, nil
}

func (w *terminalCaptureWriter) UpdateDetailsIfUnfinalized(_ context.Context, _ int64, patch store.DetailsPatch) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.patches = append(w.patches, patch)
	return true, nil
}

func (w *terminalCaptureWriter) UpdateWinnerCost(context.Context, int64, string, []byte) error {
	return nil
}

func (w *terminalCaptureWriter) FindModelPrice(context.Context, string) (*store.ModelPrice, error) {
	return nil, nil
}

func (w *terminalCaptureWriter) lastPatch() (store.DetailsPatch, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.patches) == 0 {
		return store.DetailsPatch{}, false
	}
	return w.patches[len(w.patches)-1], true
}

func (w *terminalCaptureWriter) patchCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.patches)
}

// divertRecorder 是终态改道计数的接收面替身。
type divertRecorder struct {
	mu    sync.Mutex
	items []terminal.SlowDivert
}

func (r *divertRecorder) RecordSlowDiverts(_ context.Context, diverts []terminal.SlowDivert) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, diverts...)
}

func (r *divertRecorder) snapshot() []terminal.SlowDivert {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]terminal.SlowDivert(nil), r.items...)
}

// newTerminalCaptureHandler 造一个用**真实 storeSettler + 真实 terminal.Settler**的数据面，
// 终态写落在替身 writer、改道计数落在替身接收面。
func newTerminalCaptureHandler(
	t *testing.T,
	assembly terminalCaptureAssembly,
) (*Handler, *terminalCaptureWriter, *divertRecorder) {
	t.Helper()
	dialClient, err := dial.New(dial.Options{})
	if err != nil {
		t.Fatalf("拨号器构造失败: %v", err)
	}
	writer := &terminalCaptureWriter{}
	recorder := &divertRecorder{}
	settler := terminal.New(writer, terminal.Options{SlowDiverts: recorder})
	settings := assembly.settings
	if settings == nil {
		settings = fakeSettings{}
	}
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
			Provider:       assembly.provider,
			MessageContext: &fakeMessageWriter{},
		},
		Candidates: terminalCaptureCandidates{url: assembly.upstreamURL, firstByteMS: assembly.firstByteMS},
		Settlers: func(state *RequestState) Settler {
			return &storeSettler{
				settler: settler,
				writer:  writer,
				state:   state,
				logger:  logx.New(nil),
				now:     time.Now,
			}
		},
		Forward: forward.Deps{Dial: dialClient},
		Hedge:   HedgeWiring{Enabled: assembly.hedgeEnabled, Logger: logx.New(nil)},
	})
	if err != nil {
		t.Fatalf("数据面构造失败: %v", err)
	}
	return handler, writer, recorder
}

// terminalCaptureRequest 发一条 /v1/messages 请求；stream 决定正文是否带 stream:true。
func terminalCaptureRequest(t *testing.T, handler *Handler, stream bool) int {
	t.Helper()
	payload := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
	if stream {
		payload = `{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code
}
