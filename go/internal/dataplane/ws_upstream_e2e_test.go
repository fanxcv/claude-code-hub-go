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
// 两个计数器就是本文件的判据：走成时 wsHits=1 / httpHits=0，回落时相反（wsHits 数的是
// **升级请求**，被拒的握手同样计一次——那正是「我们真的尝试过」的证据）。
type wsE2EUpstream struct {
	server *httptest.Server
	// rejectUpgrade 为真时对 WS 升级请求回 401：握手被拒，上游 WS 必然回落 HTTP。
	rejectUpgrade bool
	// brokenHTTP 为真时 HTTP 分支接管连接后直接关掉：这是**拨号层**失败（还没拿到响应头），
	// 与「拿到 4xx/5xx 再按非 2xx 收口」是两条不同的路径。
	brokenHTTP bool
	// silentAfterCreated 为真时 WS 分支只回一帧 response.created 后静默：门控不把生命周期
	// 首帧当可提交内容，故本 attempt 永不产出内容——用于造竞速输家。
	silentAfterCreated bool
	// hangHTTP 为真时 HTTP 分支收下请求后挂着不答（造「回落之后还卡在拨号里」的输家）。
	// 这类请求会让 httptest 的 Close 一直等，故 cleanup 会先放行它们（见构造函数）。
	hangHTTP   bool
	release    chan struct{}
	releaseOne sync.Once
	wsHits     atomic.Int64
	httpHits   atomic.Int64
	frames     chan string
	headers    chan http.Header
}

// newWSE2EUpstream 起假上游。WS 分支读走首帧后按真实形状回三帧，终态后不主动关闭
// （真实上游同样如此，收口由拨号侧按终态做）。
func newWSE2EUpstream(t *testing.T) *wsE2EUpstream {
	t.Helper()
	upstream := &wsE2EUpstream{
		frames:  make(chan string, 2),
		headers: make(chan http.Header, 2),
		release: make(chan struct{}),
	}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !ws.IsUpgrade(request) {
			upstream.httpHits.Add(1)
			if upstream.hangHTTP {
				select {
				case <-upstream.release:
				case <-request.Context().Done():
				}
				return
			}
			if upstream.brokenHTTP {
				if hijacker, ok := writer.(http.Hijacker); ok {
					if conn, _, err := hijacker.Hijack(); err == nil {
						_ = conn.Close()
					}
				}
				return
			}
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
		if upstream.rejectUpgrade {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
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
		if upstream.silentAfterCreated {
			if err := conn.Write(request.Context(), websocket.MessageText,
				[]byte(`{"type":"response.created","response":{"id":"resp_silent","model":"`+wsE2EModel+`"}}`)); err != nil {
				return
			}
			<-request.Context().Done()
			return
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
	// 注册顺序即执行顺序（LIFO）：先放行挂住的请求，再关服务——否则 Close 会一直等它们。
	t.Cleanup(func() { upstream.releaseOne.Do(func() { close(upstream.release) }) })
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
	// second 是竞速备选（nil 表示只有一家候选）；secondFirstByteMS 是备选自己的阈值。
	second            *wsE2EUpstream
	secondFirstByteMS int
}

// 候选源直接用 hedgeTestCandidates：它已实现「首轮给 first、竞速备选给 second（排除已试过的）」
// 与本文件的候选形状完全一致，不另造一份。

// newWSE2EAssembly 造一个不依赖数据库的数据面，WS 接线用的是**生产构造函数**
// （wsEligibility / newWSNotice），不是测试自己重写的一份资格判定。
//
// 返回值的 candidate 是首轮候选；竞速备选（装配里给了 second 时）由 hedgeTestCandidates 在
// 竞速触发后给出。
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
	candidates := hedgeTestCandidates{first: candidate}
	if assembly.second != nil {
		candidates.second = &forward.Candidate{
			Provider: forward.Provider{
				ID:                          160,
				Name:                        "codex-乙",
				Type:                        convert.ProviderType(assembly.providerType),
				URL:                         assembly.second.server.URL,
				Key:                         "fake-upstream-key",
				FirstByteTimeoutStreamingMS: assembly.secondFirstByteMS,
			},
		}
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
		Candidates: candidates,
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

// wsE2ERunTurn 起真实客户端 WS 通道，发一轮 response.create，读回本次事件帧（要求本轮以终态收场）。
func wsE2ERunTurn(t *testing.T, handler *Handler) []string {
	t.Helper()
	events, ended := wsE2ERunTurnTolerant(t, handler)
	if !ended {
		t.Fatalf("本轮没收到终态也没收到错误帧，就不该断开：%v", events)
	}
	if len(events) == 0 || !strings.Contains(events[len(events)-1], "response.completed") {
		t.Fatalf("客户端应收齐到终态事件，得到 %v", events)
	}
	return events
}

// wsE2ERunTurnTolerant 与 wsE2ERunTurn 同形，但允许本轮以失败收场（上游失败时边缘发一帧
// `{"type":"error"}`，也可能直接断开）。第二个返回值表示“收到了终止帧”而不是连接断掉。
func wsE2ERunTurnTolerant(t *testing.T, handler *Handler) ([]string, bool) {
	t.Helper()
	edge := ws.New(ws.Options{Inner: handler, Logger: logx.New(nil), Secret: "test-secret"})
	server := httptest.NewServer(edge)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
			return events, false
		}
		events = append(events, string(payload))
		if strings.Contains(string(payload), "response.completed") || strings.Contains(string(payload), `"type":"error"`) {
			return events, true
		}
	}
}

// wsE2EChainItems 取本轮结算的尝试留痕，并编码成 provider_chain 的原始 JSON 条目。
//
// 成功轮次走流式结算缝（StreamOutcome.Attempts），失败轮次走非流式缝（Result.Attempts）——
// 两条缝各只有一份事实，取哪一份由本轮怎么收场决定。
func wsE2EChainItems(t *testing.T, settler *fakeSettler) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		settler.mu.Lock()
		streams := len(settler.stream)
		nonStreams := len(settler.nonStream)
		settler.mu.Unlock()
		if streams > 0 || nonStreams > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("本轮没有任何结算，链事实无从断言")
		}
		time.Sleep(5 * time.Millisecond)
	}
	settler.mu.Lock()
	var attempts []forward.AttemptOutcome
	if len(settler.stream) > 0 {
		attempts = settler.stream[len(settler.stream)-1].Attempts
	}
	if len(settler.nonStream) > 0 {
		attempts = settler.nonStream[len(settler.nonStream)-1].Attempts
	}
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

// wsE2EFindItem 按判据找一条链项；找不到返回 nil。
func wsE2EFindItem(items []map[string]any, match func(map[string]any) bool) map[string]any {
	for _, item := range items {
		if match(item) {
			return item
		}
	}
	return nil
}

// wsE2EAssertFallbackFacts 断言链上有一家供应商留下了完整的「试过 WS、回落 HTTP」事实。
//
// 为什么它必须是**信息性条目**而不是尝试条目：尝试条目记的是本次尝试的结局（成功/失败），
// WS 事实是传输层的前置事实，两者混为一条会把结局覆盖掉。
func wsE2EAssertFallbackFacts(t *testing.T, items []map[string]any, providerID float64) map[string]any {
	t.Helper()
	info := wsE2EFindItem(items, func(item map[string]any) bool {
		return item["reason"] == forward.ReasonResponsesWSFallback && item["id"] == providerID
	})
	if info == nil {
		t.Fatalf("供应商 %v 的「已回落 HTTP」事实丢了（链上应有 %q 信息性条目）：%v",
			providerID, forward.ReasonResponsesWSFallback, items)
	}
	if info["upstreamWsAttempted"] != true {
		t.Fatalf("回落条目的 upstreamWsAttempted 应为 true，得到 %v", info["upstreamWsAttempted"])
	}
	if info["downgradedToHttp"] != true {
		t.Fatalf("回落条目的 downgradedToHttp 应为 true，得到 %v", info["downgradedToHttp"])
	}
	if info["downgradeReason"] == nil || info["downgradeReason"] == "" {
		t.Fatalf("回落条目必须带 downgradeReason，得到 %v", info["downgradeReason"])
	}
	return info
}

// wsE2EAssertHedgePrecondition 断言本装配真的走竞速：否则用例测的是串行路径，缺口再也不会被发现。
func wsE2EAssertHedgePrecondition(t *testing.T, handler *Handler, candidate *forward.Candidate) {
	t.Helper()
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
	wsE2EAssertHedgePrecondition(t, handler, candidate)

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

// TestUpstreamWSFallbackKeepsHTTPOutcome 钉住「握手被拒 → 回落 HTTP 走成」这条链路两件事：
// 客户端拿到的是 HTTP 那一发的结果（回落对客户端不可见），但链上**必须**留下 WS 事实。
//
// 为什么落在竞速装配上：生产 codex 供应商的阈值 60000ms 让每个流式请求都走竞速路径，
// 串行路径上的用例对它没有任何约束力（这正是 2026-09-19 那次漏检的形态）。
func TestUpstreamWSFallbackKeepsHTTPOutcome(t *testing.T) {
	upstream := newWSE2EUpstream(t)
	upstream.rejectUpgrade = true
	handler, settler, notices, candidate := newWSE2EAssembly(t, upstream, wsE2EAssembly{
		providerType:     "codex",
		firstByteMS:      60000,
		hedgeEnabled:     true,
		websocketEnabled: true,
	})
	wsE2EAssertHedgePrecondition(t, handler, candidate)

	wsE2ERunTurn(t, handler)
	if got := upstream.wsHits.Load(); got != 1 {
		t.Fatalf("应向被拒的上游发 1 次握手，实际 %d 次", got)
	}
	if got := upstream.httpHits.Load(); got != 1 {
		t.Fatalf("握手被拒后应回落 HTTP 恰好一次，实际 %d 次", got)
	}

	items := wsE2EChainItems(t, settler)
	info := wsE2EAssertFallbackFacts(t, items, 159)
	if info["downgradeReason"] != string(upws.DowngradeUpgradeRejected) {
		t.Fatalf("回落原因应为 %q，得到 %v", upws.DowngradeUpgradeRejected, info["downgradeReason"])
	}
	// 回落不是失败：本次尝试的结局必须仍是成功（HTTP 那一发的结论）。
	outcome := wsE2EFindItem(items, func(item map[string]any) bool {
		return item["id"] == float64(159) && item["statusCode"] == float64(200)
	})
	if outcome == nil {
		t.Fatalf("回落后的 HTTP 走成应留下 200 的尝试条目：%v", items)
	}
	if skips := notices.skips(); len(skips) != 0 {
		t.Fatalf("真的尝试过了，不该上报跳过：%v", skips)
	}
}

// TestUpstreamWSFallbackFailureKeepsWSFacts 是 P1-A 的回归钉子：WS 回落之后那发 HTTP 在**拨号层就失败**时，
// 失败留痕必须仍带着 WS 事实——否则「试过 WS、回落了、HTTP 没打成」在链上看起来
// 与「压根没试过 WS」完全一样，而那正是排障时最需要区分的一件事。
//
// 为何用「拨号层失败」而不是 4xx/5xx：拿到状态码的失败走的是另一条分支（事实在修前也已发布），
// 区分不出这条时序缺陷。
//
// 为何让备选接手：失败留痕只有在流提交后才会经流式快照进链（见本文件末尾的残余说明）；
// 本用例要钉的是事实内容，不是另一条结算缝隙。
func TestUpstreamWSFallbackFailureKeepsWSFacts(t *testing.T) {
	down := newWSE2EUpstream(t)
	down.rejectUpgrade = true
	down.brokenHTTP = true
	up := newWSE2EUpstream(t)
	handler, settler, _, candidate := newWSE2EAssembly(t, down, wsE2EAssembly{
		providerType:      "codex",
		firstByteMS:       60000,
		hedgeEnabled:      true,
		websocketEnabled:  true,
		second:            up,
		secondFirstByteMS: 0,
	})
	wsE2EAssertHedgePrecondition(t, handler, candidate)

	wsE2ERunTurn(t, handler)
	if got := down.wsHits.Load(); got != 1 {
		t.Fatalf("首候选应先试一次 WS 握手，实际 %d 次", got)
	}
	if got := down.httpHits.Load(); got != 1 {
		t.Fatalf("首候选回落后应发一次 HTTP，实际 %d 次", got)
	}
	if got := up.wsHits.Load(); got != 1 {
		t.Fatalf("备选应接手并走 WS，实际 %d 次", got)
	}

	items := wsE2EChainItems(t, settler)
	wsE2EAssertFallbackFacts(t, items, 159)
	failed := wsE2EFindItem(items, func(item map[string]any) bool {
		if item["id"] != float64(159) {
			return false
		}
		reason, _ := item["reason"].(string)
		return reason != "" && !strings.HasPrefix(reason, "responses_ws") && !strings.HasPrefix(reason, "hedge_")
	})
	if failed == nil {
		t.Fatalf("两跳都失败的首候选应留下失败尝试条目：%v", items)
	}
	// 备选走成的 WS 事实也必须在（两个供应商各一份，不互相覆盖）。
	wsE2EAssertConnectedFacts(t, items)
}

// TestUpstreamWSFactsSurviveHedgeLoser 是 P1-B 的回归钉子：竞速输家的结局由**胜者协程**落定，
// 而 WS 事实由输家自己的协程发布——发布得晚一点（旧代码里发布点在回落 HTTP 之后），
// 输家条目就会丢事实（“输家也走过 WS”无从查证）。
//
// 如何造输家：首候选的握手被拒（留下降级事实）后回落 HTTP，而那道 HTTP 挂住不答；阈值 300ms
// 到点后启动备选，备选吐出内容即胜——胜者裁决时首候选仍在 dialAttempt 里，正是旧代码丢事实的窗口。
func TestUpstreamWSFactsSurviveHedgeLoser(t *testing.T) {
	down := newWSE2EUpstream(t)
	down.rejectUpgrade = true
	down.hangHTTP = true
	up := newWSE2EUpstream(t)
	handler, settler, _, candidate := newWSE2EAssembly(t, down, wsE2EAssembly{
		providerType:      "codex",
		firstByteMS:       300,
		hedgeEnabled:      true,
		websocketEnabled:  true,
		second:            up,
		secondFirstByteMS: 0,
	})
	wsE2EAssertHedgePrecondition(t, handler, candidate)

	wsE2ERunTurn(t, handler)
	if got := up.wsHits.Load(); got != 1 {
		t.Fatalf("备选应接手并走 WS，实际 %d 次（HTTP %d 次）", got, up.httpHits.Load())
	}

	items := wsE2EChainItems(t, settler)
	loserOutcome := wsE2EFindItem(items, func(item map[string]any) bool {
		if item["id"] != float64(159) {
			return false
		}
		reason, _ := item["reason"].(string)
		return strings.HasPrefix(reason, "hedge_loser")
	})
	if loserOutcome == nil {
		t.Fatalf("首候选应作为竞速输家留下结局条目：%v", items)
	}
	// 输家的 WS 事实：它真的试过握手（被拒）并回落了 HTTP。
	info := wsE2EFindItem(items, func(item map[string]any) bool {
		if item["id"] != float64(159) {
			return false
		}
		reason, _ := item["reason"].(string)
		return strings.HasPrefix(reason, "responses_ws")
	})
	if info == nil {
		t.Fatalf("输家的上游 WS 事实丢了（链上应有它自己的 responses_ws_* 信息性条目）：%v", items)
	}
	if info["upstreamWsAttempted"] != true || info["downgradedToHttp"] != true {
		t.Fatalf("输家条目应记「尝试过且已回落」，得到 %v", info)
	}
	if info["downgradeReason"] != string(upws.DowngradeUpgradeRejected) {
		t.Fatalf("回落原因应为 %q，得到 %v", upws.DowngradeUpgradeRejected, info["downgradeReason"])
	}
}

// TestUpstreamWSConnectedFactsSurviveHedgeLoser 是同一个事实的另一半：输家的 WS **连上了**
// （只是没产出内容）时，connected 同样要留在它自己的条目上。
//
// 为何另起一条：它的发布时点在旧代码里就已经靠前（握手成功不经过回落拨号），故它不区分修前修后；
// 留着是为了让「输家也走成了 WS」成为断言而不是靠人记得。
func TestUpstreamWSConnectedFactsSurviveHedgeLoser(t *testing.T) {
	slow := newWSE2EUpstream(t)
	slow.silentAfterCreated = true
	fast := newWSE2EUpstream(t)
	handler, settler, _, candidate := newWSE2EAssembly(t, slow, wsE2EAssembly{
		providerType:      "codex",
		firstByteMS:       300,
		hedgeEnabled:      true,
		websocketEnabled:  true,
		second:            fast,
		secondFirstByteMS: 0,
	})
	wsE2EAssertHedgePrecondition(t, handler, candidate)

	wsE2ERunTurn(t, handler)
	if got := fast.wsHits.Load(); got != 1 {
		t.Fatalf("备选应接手并走 WS，实际 %d 次（HTTP %d 次）", got, fast.httpHits.Load())
	}

	items := wsE2EChainItems(t, settler)
	loserWS := wsE2EFindItem(items, func(item map[string]any) bool {
		if item["id"] != float64(159) {
			return false
		}
		reason, _ := item["reason"].(string)
		return strings.HasPrefix(reason, "responses_ws")
	})
	if loserWS == nil {
		t.Fatalf("输家的上游 WS 事实丢了（链上应有它自己的 responses_ws_* 信息性条目）：%v", items)
	}
	if loserWS["upstreamWsAttempted"] != true {
		t.Fatalf("输家条目应记「尝试过」，得到 %v", loserWS["upstreamWsAttempted"])
	}
	if loserWS["upstreamWsConnected"] != true {
		t.Fatalf("输家已连上上游 WS（只是没产出内容），应记 connected，得到 %v", loserWS["upstreamWsConnected"])
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

// 本文件的一处已知缺口（不在本轮范围，仅记事实）：竞速一轮**彻底失败**时
// ForwardStreamHedge 返回 (nil, err)，数据面走 settleFailure，而那条结算造的 Result 不带
// Attempts；链载荷只在 len(result.Attempts) > 0 时才写（storeSettler.NonStream）
// ⇒ 失败轮次的 provider_chain 为空（本文件修前跑出的红就是一个空链）。
//
// 影响：上游 WS 的失败留痕只可能在**有胜者**的轮次里被看见（例如首候选失败、备选接手）。
// 修它要给失败轮次补一条带 Attempts 的结算路径（属数据面结算缝的改动），本轮不做。
