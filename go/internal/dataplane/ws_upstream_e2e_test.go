package dataplane

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/upws"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ws"
)

// 本文件是上游 WS 的**端到端钉子**：真实客户端 WS 通道（internal/ws 的边缘处理器）→ 真实数据面
// 装配（含竞速与 WS 接线）→ 假 codex 上游 WS 服务器。
//
// 为什么必须是端到端：断点全在接线处——入口隧道标记能不能被资格判定读到、竞速路径有没有
// 绕过 WS 发起、链上有没有落下 WS 事实。注入式单测对这三件事都看不见：2026-09-19 生产上
// 「上游 WS 七十二小时零尝试、链上无键、日志无痕」正是竞速路径绕过 wsAttempt 造成的，
// 而当时 forward 与 dataplane 的 WS 用例全绿。

const (
	wsE2EModel = "gpt-5-codex"
	wsE2EKey   = "fake-client-key"
)

// wsE2EUpstream 是假 codex 上游：同一条路径上区分 WS 升级与 HTTP，两者都回 Responses 事件流。
//
// 两个计数器就是本文件的判据：走成时 wsHits=1 / httpHits=0，回落时相反。
type wsE2EUpstream struct {
	server   *httptest.Server
	wsHits   atomic.Int64
	httpHits atomic.Int64
	frames   chan string
	headers  chan http.Header
}

// newWSE2EUpstream 起假上游。WS 分支读走首帧后按真实形状回三帧，终态后不主动关闭
// （真实上游同样如此，收口由拨号侧按终态做）。
func newWSE2EUpstream(t *testing.T) *wsE2EUpstream {
	t.Helper()
	upstream := &wsE2EUpstream{
		frames:  make(chan string, 2),
		headers: make(chan http.Header, 2),
	}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !ws.IsUpgrade(request) {
			upstream.httpHits.Add(1)
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			for _, event := range []string{
				`{"type":"response.created","response":{"id":"resp_http","model":"` + wsE2EModel + `"}}`,
				`{"type":"response.output_text.delta","delta":"http"}`,
				`{"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":1}}}`,
			} {
				_, _ = io.WriteString(writer, "data: "+event+"\n\n")
			}
			return
		}
		upstream.wsHits.Add(1)
		conn, err := websocket.Accept(writer, request, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, first, err := conn.Read(request.Context())
		if err != nil {
			return
		}
		if len(upstream.frames) < cap(upstream.frames) {
			upstream.frames <- string(first)
		}
		if len(upstream.headers) < cap(upstream.headers) {
			upstream.headers <- request.Header.Clone()
		}
		for _, frame := range []string{
			`{"type":"response.created","response":{"id":"resp_ws","model":"` + wsE2EModel + `"}}`,
			`{"type":"response.output_text.delta","delta":"ws"}`,
			`{"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":1}}}`,
		} {
			if err := conn.Write(request.Context(), websocket.MessageText, []byte(frame)); err != nil {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// wsE2ENotices 记录「跳过上游 WS」的上报（生产侧的去重、限频在 newWSNotice，这里只收事实）。
type wsE2ENotices struct {
	mu    sync.Mutex
	items []forward.WSSkip
}

func (n *wsE2ENotices) record(skip forward.WSSkip) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.items = append(n.items, skip)
}

func (n *wsE2ENotices) skips() []forward.WSSkip {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]forward.WSSkip(nil), n.items...)
}

// wsE2EAssembly 是一次端到端用例的装配参数。
type wsE2EAssembly struct {
	// providerType 是候选供应商类型（codex 才具备上游 WS 资格）。
	providerType string
	// firstByteMS 是候选的首字节阈值；> 0 且设置允许时请求会走竞速路径。
	firstByteMS int
	// hedgeEnabled 是竞速总开关（与生产装配同义）。
	hedgeEnabled bool
	// websocketEnabled 是系统设置 `enable_openai_responses_websocket`。
	websocketEnabled bool
}

// wsE2ECandidates 是只需一家候选的候选源（竞速备选返回 nil：本文件的用例都是首轮决胜）。
type wsE2ECandidates struct {
	candidate *forward.Candidate
}

func (c wsE2ECandidates) Candidate(context.Context, pctx.ProviderSelection, SelectionFacts) (*forward.Candidate, route.Result, error) {
	return c.candidate, route.Result{Provider: &route.Provider{ID: c.candidate.Provider.ID}}, nil
}

func (c wsE2ECandidates) Failover(context.Context, SelectionFacts) (*forward.Candidate, route.Result, error) {
	return nil, route.Result{}, nil
}

// newWSE2EAssembly 造一个不依赖数据库的数据面，WS 接线用的是**生产构造函数**
// （wsEligibility / newWSNotice），不是测试自己重写的一份资格判定。
func newWSE2EAssembly(
	t *testing.T,
	upstream *wsE2EUpstream,
	assembly wsE2EAssembly,
) (*Handler, *fakeSettler, *wsE2ENotices, *forward.Candidate) {
	t.Helper()
	dialClient, err := dial.New(dial.Options{})
	if err != nil {
		t.Fatalf("拨号器构造失败: %v", err)
	}
	settings := hedgeTestSettings{settings: store.SystemSettings{
		EnableOpenAIResponsesWebsocket: assembly.websocketEnabled,
		LegacyHedgeMaxInFlight:         2,
	}}
	candidate := &forward.Candidate{
		Provider: forward.Provider{
			ID:                          159,
			Name:                        "codex-甲",
			Type:                        convert.ProviderType(assembly.providerType),
			URL:                         upstream.server.URL,
			Key:                         "fake-upstream-key",
			FirstByteTimeoutStreamingMS: assembly.firstByteMS,
		},
	}
	settler := &fakeSettler{}
	notices := &wsE2ENotices{}
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
			Provider:       fakeProvider{selection: pctx.ProviderSelection{ProviderID: 159, Name: "codex-甲", Type: assembly.providerType}},
			MessageContext: &fakeMessageWriter{},
		},
		Candidates: wsE2ECandidates{candidate: candidate},
		Settlers:   func(*RequestState) Settler { return settler },
		Forward: forward.Deps{
			Dial:       dialClient,
			WS:         upws.New(upws.Options{ConnectTimeout: 2 * time.Second, HeadersTimeout: 2 * time.Second, BodyIdleTimeout: 2 * time.Second, ClientVersion: "9.9.9"}),
			WSEligible: wsEligibility(settings),
			WSNotice:   notices.record,
		},
		Stream: forward.StreamOptions{Budget: gate.DefaultBudget()},
		Hedge:  HedgeWiring{Enabled: assembly.hedgeEnabled, Logger: logx.New(nil)},
	})
	if err != nil {
		t.Fatalf("数据面构造失败: %v", err)
	}
	return handler, settler, notices, candidate
}

// wsE2ERunTurn 起真实客户端 WS 通道，发一轮 response.create，读回本次事件帧。
func wsE2ERunTurn(t *testing.T, handler *Handler) []string {
	t.Helper()
	edge := ws.New(ws.Options{Inner: handler, Logger: logx.New(nil), Secret: "test-secret"})
	server := httptest.NewServer(edge)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{"x-api-key": []string{wsE2EKey}, "Authorization": []string{"Bearer " + wsE2EKey}},
	})
	if err != nil {
		t.Fatalf("客户端 WS 建连失败: %v", err)
	}
	defer conn.CloseNow()

	frame := `{"type":"response.create","model":"` + wsE2EModel + `","input":"你好"}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("发首帧失败: %v", err)
	}
	events := []string{}
	for {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("读帧失败（已收 %v）: %v", events, err)
		}
		events = append(events, string(payload))
		if strings.Contains(string(payload), "response.completed") {
			return events
		}
	}
}

// wsE2EChainItems 取本轮流式结算的尝试留痕，并编码成 provider_chain 的原始 JSON 条目。
func wsE2EChainItems(t *testing.T, settler *fakeSettler) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		settler.mu.Lock()
		count := len(settler.stream)
		settler.mu.Unlock()
		if count > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("本轮没有流式结算，链事实无从断言")
		}
		time.Sleep(5 * time.Millisecond)
	}
	settler.mu.Lock()
	attempts := settler.stream[len(settler.stream)-1].Attempts
	settler.mu.Unlock()
	return decodeChainItems(t, chainDetailsSettler().providerChain(attempts))
}

// wsE2EAssertConnectedFacts 断言链上出现了「上游 WS 走成」的两条事实：
// 信息性条目的 upstreamWsConnected 与尝试条目的正常结局。
func wsE2EAssertConnectedFacts(t *testing.T, items []map[string]any) {
	t.Helper()
	for _, item := range items {
		if item["reason"] != forward.ReasonResponsesWSAttempted {
			continue
		}
		if item["upstreamWsConnected"] != true {
			t.Fatalf("走成时 upstreamWsConnected 应为 true，得到 %v（条目 %v）", item["upstreamWsConnected"], item)
		}
		if item["clientTransport"] != "websocket" {
			t.Fatalf("clientTransport 应为 websocket，得到 %v", item["clientTransport"])
		}
		return
	}
	t.Fatalf("链上没有 %q 信息性条目——上游 WS 的「走成了」没有留下任何痕迹：%v",
		forward.ReasonResponsesWSAttempted, items)
}

// TestUpstreamWSConnecteWhenHedgeEnabled 是本文件的核心用例，也是 2026-09-19 那次生产
// 「上游 WS 从不发起」的复现：竞速一旦生效（首字节阈值 > 0 且设置允许），所有流式请求都走
// ForwardStreamHedge——它在修复前直接调 dialAttempt，绕过 wsAttempt。
//
// 生产事实（修复前实测）：供应商 codex + first_byte_timeout_streaming_ms=60000 +
// discovery_enabled=false → 全量流式请求命中竞速；72 小时内 15677 条 /v1/responses
// 无一携带 WS 事实键，日志无任何 upws.* 或回落 warn。
func TestUpstreamWSConnecteWhenHedgeEnabled(t *testing.T) {
	upstream := newWSE2EUpstream(t)
	handler, settler, notices, candidate := newWSE2EAssembly(t, upstream, wsE2EAssembly{
		providerType:     "codex",
		firstByteMS:      60000,
		hedgeEnabled:     true,
		websocketEnabled: true,
	})

	// 前置：本用例必须真的跑在竞速路径上，否则它就退化成串行路径的重复用例，
	// 修好串行路径却漏掉竞速的缺口就再也测不出来。
	decisionDeps := forward.Deps{
		Facts: forward.PlanFacts{Client: forward.ClientRequest{
			Body: []byte(`{"model":"` + wsE2EModel + `","stream":true}`),
		}},
	}
	if _, enabled := handler.hedgeDecision(
		context.Background(),
		routeSpec{Policy: guard.Policy{Preset: guard.PresetChat}},
		decisionDeps,
		candidate,
		&RequestState{Model: wsE2EModel},
	); !enabled {
		t.Fatal("前置不成立：本装配下竞速未生效，用例失去意义")
	}

	events := wsE2ERunTurn(t, handler)
	if got := upstream.wsHits.Load(); got != 1 {
		t.Fatalf("上游应收到 1 次 WS 升级，实际 %d 次（HTTP %d 次）", got, upstream.httpHits.Load())
	}
	if got := upstream.httpHits.Load(); got != 0 {
		t.Fatalf("WS 走成时不该再有 HTTP 请求，实际 %d 次", got)
	}
	firstFrame := <-upstream.frames
	if !strings.Contains(firstFrame, `"type":"response.create"`) {
		t.Fatalf("上游首帧必须是 response.create，得到 %s", firstFrame)
	}
	wsE2EAssertConnectedFacts(t, wsE2EChainItems(t, settler))
	if skips := notices.skips(); len(skips) != 0 {
		t.Fatalf("真的走了 WS，不该上报跳过：%v", skips)
	}
	if len(events) == 0 || !strings.Contains(events[len(events)-1], "response.completed") {
		t.Fatalf("客户端应收齐到终态事件，得到 %v", events)
	}
}

// TestUpstreamWSConnectedWithoutHedge 是同一条事实的串行路径用例：它在本修复前就是绿的，
// 留着是为了让「两条路径都接上了 WS」成为断言，而不是靠人记得。
func TestUpstreamWSConnectedWithoutHedge(t *testing.T) {
	upstream := newWSE2EUpstream(t)
	handler, settler, _, _ := newWSE2EAssembly(t, upstream, wsE2EAssembly{
		providerType:     "codex",
		firstByteMS:      0,
		hedgeEnabled:     true,
		websocketEnabled: true,
	})

	wsE2ERunTurn(t, handler)
	if got := upstream.wsHits.Load(); got != 1 {
		t.Fatalf("串行路径应走 WS，实际升级 %d 次（HTTP %d 次）", got, upstream.httpHits.Load())
	}
	wsE2EAssertConnectedFacts(t, wsE2EChainItems(t, settler))
}

// TestUpstreamWSSkipIsObservable 断言「没走 WS」不再静默：开关关闭时回落 HTTP，且产生一条
// 带原因的上报（这正是生产排障时最缺的那条信息）。
func TestUpstreamWSSkipIsObservable(t *testing.T) {
	upstream := newWSE2EUpstream(t)
	handler, _, notices, _ := newWSE2EAssembly(t, upstream, wsE2EAssembly{
		providerType:     "codex",
		firstByteMS:      0,
		hedgeEnabled:     true,
		websocketEnabled: false,
	})

	wsE2ERunTurn(t, handler)
	if got := upstream.wsHits.Load(); got != 0 {
		t.Fatalf("开关关闭时不得发起握手，实际升级 %d 次", got)
	}
	if got := upstream.httpHits.Load(); got != 1 {
		t.Fatalf("开关关闭时应回落 HTTP 一次，实际 %d 次", got)
	}
	skips := notices.skips()
	if len(skips) != 1 {
		t.Fatalf("应上报恰好一条跳过事实，得到 %v", skips)
	}
	if skips[0].Cause != forward.WSSkipCauseNotEligible {
		t.Fatalf("跳过原因应为 %q，得到 %q", forward.WSSkipCauseNotEligible, skips[0].Cause)
	}
	if skips[0].ProviderID != 159 || skips[0].ProviderType != "codex" {
		t.Fatalf("跳过事实应带上供应商身份，得到 %+v", skips[0])
	}
}
