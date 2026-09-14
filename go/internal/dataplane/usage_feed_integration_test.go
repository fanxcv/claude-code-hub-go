package dataplane

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
	"github.com/fanxcv/claude-code-hub-go/go/internal/usagefeed"
	"github.com/redis/go-redis/v9"
)

// 本文件是使用记录页**推送模式**的端到端用例：真 PG + 真 Redis + 真 HTTP（httptest）。
//
// 链路逐段都是真的，只有两处是替身，都在报告里点明：
//   - **上游**是 httptest 假上游（与同目录其余集成用例同一套路），代理请求本身是真的；
//   - **管理面认证**用放行替身（本用例考的是推送链路，不是鉴权；鉴权由 adminapi 自己的
//     用例覆盖，未授权不回退的语义见 usage_logs_stream_test.go）。
//
// 门控：CCH_TEST_DSN 与 CCH_TEST_REDIS_URL 缺一即跳过（与仓库其余集成用例一致）。

// feedRedis 连测试 Redis（DB 13，与仓库其余用例一致）。
func feedRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过推送端到端用例")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败（值已隐去）: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client
}

// feedGuard 是放行守卫：SSE 用例只考推送链路。
type feedGuard struct{}

func (feedGuard) Wrap(_ adminapi.AccessLevel, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		next.ServeHTTP(writer, request)
	})
}

// feedSSEServer 起一台只挂 /usage-logs/stream 的管理面服务器。
//
// 用真 Router + 真 HTTP 服务器（而不是直接调 handler）：响应头、flush、连接生命周期
// 都要经过 net/http 才能被验证。
func feedSSEServer(t *testing.T, feed usagefeed.Feed) *httptest.Server {
	t.Helper()
	router := adminapi.New(adminapi.Options{Deps: adminapi.Deps{
		Guard:    feedGuard{},
		Problems: adminapi.NewProblems(nil),
	}})
	adminapi.RegisterUsageLogsStream(router, adminapi.Deps{
		Guard:       feedGuard{},
		Problems:    adminapi.NewProblems(nil),
		NewRowsFeed: feed,
	})
	if router.RouteCount() == 0 {
		t.Fatal("SSE 路由未注册：推送面未装配")
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

// sseClient 是一条已建立的 SSE 连接：后台按行读事件，测试同步取。
type sseClient struct {
	response *http.Response
	events   chan sseEvent
	errors   chan error
	close    func()
}

type sseEvent struct {
	name string
	data string
}

// openSSE 建立连接并等到 ready 事件（否则后续「没收到 new-rows」的结论不成立）。
func openSSE(t *testing.T, serverURL string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/api/v1/usage-logs/stream", nil)
	if err != nil {
		cancel()
		t.Fatalf("构造 SSE 请求失败: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		t.Fatalf("建立 SSE 连接失败: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		cancel()
		_ = response.Body.Close()
		t.Fatalf("SSE 状态码应为 200，实际 %d", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		cancel()
		_ = response.Body.Close()
		t.Fatalf("SSE Content-Type 不符契约: %q", contentType)
	}

	client := &sseClient{
		response: response,
		events:   make(chan sseEvent, 64),
		errors:   make(chan error, 1),
		close: func() {
			cancel()
			_ = response.Body.Close()
		},
	}
	go func() {
		defer close(client.events)
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var pending sseEvent
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				pending.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				pending.data = strings.TrimPrefix(line, "data: ")
				client.events <- pending
				pending = sseEvent{}
			}
		}
		if err := scanner.Err(); err != nil {
			client.errors <- err
		}
	}()
	t.Cleanup(client.close)

	event, err := client.wait(t, 5*time.Second)
	if err != nil {
		t.Fatalf("未收到 ready 事件: %v", err)
	}
	if event.name != "ready" || event.data != "{}" {
		t.Fatalf("首个事件应为 ready/{}，实际 %+v", event)
	}
	return client
}

// wait 等下一个事件。
func (c *sseClient) wait(t *testing.T, timeout time.Duration) (sseEvent, error) {
	t.Helper()
	select {
	case event, open := <-c.events:
		if !open {
			return sseEvent{}, fmt.Errorf("SSE 连接已结束")
		}
		return event, nil
	case err := <-c.errors:
		return sseEvent{}, err
	case <-time.After(timeout):
		return sseEvent{}, fmt.Errorf("等事件超时（%s）", timeout)
	}
}

// waitForNewRows 等在超时内出现的第一个 new-rows 事件。
func (c *sseClient) waitForNewRows(t *testing.T, timeout time.Duration) (usagefeed.Signal, bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case event, open := <-c.events:
			if !open {
				return usagefeed.Signal{}, false
			}
			if event.name != "new-rows" {
				continue
			}
			var signal usagefeed.Signal
			if err := json.Unmarshal([]byte(event.data), &signal); err != nil {
				t.Fatalf("new-rows 载荷不是合法 JSON（%q）: %v", event.data, err)
			}
			return signal, true
		case <-deadline:
			return usagefeed.Signal{}, false
		}
	}
}

// expectNoNewRows 在一段静默窗口内确认没有 new-rows（用于反证与降级）。
func (c *sseClient) expectNoNewRows(t *testing.T, window time.Duration) {
	t.Helper()
	deadline := time.After(window)
	for {
		select {
		case event, open := <-c.events:
			if !open {
				return
			}
			if event.name == "new-rows" {
				t.Fatalf("不该收到 new-rows，却收到 %q", event.data)
			}
		case <-deadline:
			return
		}
	}
}

// feedHandler 用给定的通知面装配数据面处理器。
//
// 走生产装配函数 NewStoreBacked（而不是手工拼 Options）：这样装配处漏传 NewRows
// 会被这些用例直接抓到。
func feedHandler(t *testing.T, pools *store.Pools, notifier notifierOrNil) http.Handler {
	t.Helper()
	assembly, err := NewStoreBacked(StoreOptions{
		Pools:   pools,
		Logger:  logx.New(nil),
		NewRows: notifier.value(),
	})
	if err != nil {
		t.Fatalf("数据面装配失败: %v", err)
	}
	return assembly.Handler
}

// notifierOrNil 让「装配 / 故意不装配」两种用例读起来是一个参数。
//
// value 返回 nil 时装配出的是**未接线**的数据面（NewRows 为 nil）——反证用例正靠它。
type notifierOrNil struct {
	notifier terminal.NewRowsNotifier
}

func (n notifierOrNil) value() terminal.NewRowsNotifier {
	if n.notifier == nil {
		return nil
	}
	return n.notifier
}

// sendFeedRequest 打一发真实的代理请求（假上游在调用方建好）。
func sendFeedRequest(t *testing.T, server *httptest.Server, apiKey string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"你好"}]}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", apiKey)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("代理请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("代理请求状态码应为 200，实际 %d", response.StatusCode)
	}
}

// feedUpstream 是假上游：非流式 JSON 应答（推送链路不关心响应形状）。
func feedUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5",`+
			`"content":[{"type":"text","text":"ok"}],`+
			`"usage":{"input_tokens":11,"output_tokens":7}}`)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// rowIDAfter 读该供应商在水位之后的**最大**行 id（本次请求落的那一行）。
func rowIDAfter(t *testing.T, provisioned provisioning) int64 {
	t.Helper()
	reader, err := provisioned.pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	var id int64
	if err := reader.QueryRow(context.Background(),
		`SELECT coalesce(max(id), 0) FROM message_request WHERE provider_id = $1 AND id > $2`,
		provisioned.providerID, provisioned.watermark).Scan(&id); err != nil {
		t.Fatalf("读取行 id 失败: %v", err)
	}
	if id == 0 {
		t.Fatal("本次请求没有落任何 message_request 行")
	}
	return id
}

// waitForRowID 轮询到该请求的行落库为止（返回行 id）。
//
// 需要它是因为「行落库」与「信号到达」是两条独立链路，本用例要断言的是两者的**一致性**
// （signal.maxId == 该行 id），所以先把行 id 拿到手再比对。
func waitForRowID(t *testing.T, provisioned provisioning) int64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		reader, err := provisioned.pools.Data()
		if err != nil {
			t.Fatalf("取读分道失败: %v", err)
		}
		var id int64
		if err := reader.QueryRow(context.Background(),
			`SELECT coalesce(max(id), 0) FROM message_request WHERE provider_id = $1 AND id > $2`,
			provisioned.providerID, provisioned.watermark).Scan(&id); err != nil {
			t.Fatalf("读取行 id 失败: %v", err)
		}
		if id > 0 {
			return id
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("等不到本次请求落库的行")
	return 0
}

// waitMergedSignals 收集一段安静期内的所有 new-rows，返回事件数、计数合计、最大 id。
//
// 安静期判定：连续 quietWindow 收不到新事件即认为窗口结束。用它量「合并」效果，
// 而不是睡固定时长——固定时长会让用例要么慢要么不稳。
// 收集突发期间的合并信号，返回（事件数、计数合计、最大 id、最小 id）。
//
// 最小 id 是 window 下界的实测值：它必须落在本次突发实际落库的 id 区间内，
// 否则「晚结算的低 id 行」就不可能被前端重拉回来。
func waitMergedSignals(t *testing.T, client *sseClient, quietWindow, hardLimit time.Duration) (int, int64, int64, int64) {
	t.Helper()
	events := 0
	var total int64
	var maxID int64
	var minID int64
	hard := time.After(hardLimit)
	for {
		select {
		case event, open := <-client.events:
			if !open {
				return events, total, maxID, minID
			}
			if event.name != "new-rows" {
				continue
			}
			var signal usagefeed.Signal
			if err := json.Unmarshal([]byte(event.data), &signal); err != nil {
				t.Fatalf("new-rows 载荷不是合法 JSON（%q）: %v", event.data, err)
			}
			events++
			total += signal.Count
			if signal.MaxID > maxID {
				maxID = signal.MaxID
			}
			// 窗口下界必须存在且不超过上界：缺失（0）或反了都说明字段没带上/带错了。
			if signal.MinID <= 0 {
				t.Fatalf("信号的 minId 应为正数，实际 %d（载荷 %q）", signal.MinID, event.data)
			}
			if signal.MinID > signal.MaxID {
				t.Fatalf("minId 不得超过 maxId：minId=%d maxId=%d", signal.MinID, signal.MaxID)
			}
			if minID == 0 || signal.MinID < minID {
				minID = signal.MinID
			}
		case <-time.After(quietWindow):
			return events, total, maxID, minID
		case <-hard:
			return events, total, maxID, minID
		}
	}
}

// 端到端：真实代理请求落库 → 真实 Redis 广播 → 真实 SSE 连接收到 new-rows，且 maxId 与那一行一致。
func TestIntegrationUsageFeedNotifiesNewRowOverSSE(t *testing.T) {
	pools := integrationStore(t)
	redisClient := feedRedis(t)
	upstream := feedUpstream(t)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))

	hub := usagefeed.NewHub(usagefeed.Options{Redis: redisClient, Logger: logx.New(nil)})
	t.Cleanup(func() { _ = hub.Close() })

	proxy := httptest.NewServer(feedHandler(t, pools, notifierOrNil{notifier: hub}))
	t.Cleanup(proxy.Close)

	// 先建 SSE 连接（订阅就绪）再打请求，否则信号可能在订阅建立前发出而丢失。
	server := feedSSEServer(t, hub)
	client := openSSE(t, server.URL)

	sendFeedRequest(t, proxy, provisioned.apiKey)
	rowID := waitForRowID(t, provisioned)

	signal, ok := client.waitForNewRows(t, 10*time.Second)
	if !ok {
		t.Fatal("代理请求落库后未收到 new-rows 信号")
	}
	if signal.MaxID != rowID {
		t.Fatalf("signal.maxId 应等于本次落库的行 id（%d），实际 %d", rowID, signal.MaxID)
	}
	if signal.Count != 1 {
		t.Fatalf("单发请求的 count 应为 1，实际 %d", signal.Count)
	}
	// 窗口下界：单发时它就是这一行的 id（三键之一，且 minId <= maxId 恒成立）。
	if signal.MinID != rowID {
		t.Fatalf("signal.minId 应等于本次落库的行 id（%d），实际 %d", rowID, signal.MinID)
	}
	// 把实测值打进日志：报告与排障都用它（不靠重跑猜数）。
	t.Logf("单发：行 id=%d，signal={maxId:%d,minId:%d,count:%d}",
		rowID, signal.MaxID, signal.MinID, signal.Count)
}

// 多实例：发布方与订阅方是**两个独立的 Hub**（各自一条 Redis 连接池），
// 等价于两台进程共享一个 Redis。A 实例处理请求，B 实例上的 SSE 必须收到信号。
func TestIntegrationUsageFeedCrossInstanceSSE(t *testing.T) {
	pools := integrationStore(t)
	upstream := feedUpstream(t)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))

	// 两个 Hub，各自的 Redis 客户端（真·跨实例：不经任何进程内共享）。
	publisherRedis := redis.NewClient(feedRedisOptions(t))
	t.Cleanup(func() { _ = publisherRedis.Close() })
	subscriberRedis := redis.NewClient(feedRedisOptions(t))
	t.Cleanup(func() { _ = subscriberRedis.Close() })

	publisher := usagefeed.NewHub(usagefeed.Options{Redis: publisherRedis, Logger: logx.New(nil)})
	t.Cleanup(func() { _ = publisher.Close() })
	subscriber := usagefeed.NewHub(usagefeed.Options{Redis: subscriberRedis, Logger: logx.New(nil)})
	t.Cleanup(func() { _ = subscriber.Close() })

	// 请求走 A 实例（发布方），SSE 挂在 B 实例（订阅方）。
	proxy := httptest.NewServer(feedHandler(t, pools, notifierOrNil{notifier: publisher}))
	t.Cleanup(proxy.Close)

	server := feedSSEServer(t, subscriber)
	client := openSSE(t, server.URL)

	sendFeedRequest(t, proxy, provisioned.apiKey)
	rowID := waitForRowID(t, provisioned)

	signal, ok := client.waitForNewRows(t, 10*time.Second)
	if !ok {
		t.Fatal("另一实例上的 SSE 未收到跨实例广播")
	}
	if signal.MaxID != rowID {
		t.Fatalf("跨实例信号的 maxId 应等于行 id（%d），实际 %d", rowID, signal.MaxID)
	}
	// 跨实例路径同样要带上窗口下界（经 Redis 序列化后不得丢字段）。
	if signal.MinID != rowID {
		t.Fatalf("跨实例信号的 minId 应等于行 id（%d），实际 %d", rowID, signal.MinID)
	}
	t.Logf("跨实例：行 id=%d，signal={maxId:%d,minId:%d,count:%d}",
		rowID, signal.MaxID, signal.MinID, signal.Count)
}

// 节流：并发打 10 发请求，事件数必须**远小于 10**，且计数合计等于落库行数（不丢计数）。
func TestIntegrationUsageFeedThrottlesBurst(t *testing.T) {
	pools := integrationStore(t)
	redisClient := feedRedis(t)
	upstream := feedUpstream(t)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))

	hub := usagefeed.NewHub(usagefeed.Options{Redis: redisClient, Logger: logx.New(nil)})
	t.Cleanup(func() { _ = hub.Close() })

	proxy := httptest.NewServer(feedHandler(t, pools, notifierOrNil{notifier: hub}))
	t.Cleanup(proxy.Close)

	server := feedSSEServer(t, hub)
	client := openSSE(t, server.URL)

	const burst = 10
	var wait sync.WaitGroup
	for index := 0; index < burst; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			// 并发打请求：它们应落在同一个（或极少数）合并窗口里。
			request, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages",
				strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"你好"}]}`))
			if err != nil {
				return
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("x-api-key", provisioned.apiKey)
			response, err := proxy.Client().Do(request)
			if err != nil {
				return
			}
			_, _ = io.ReadAll(response.Body)
			_ = response.Body.Close()
		}()
	}
	wait.Wait()

	events, total, maxID, minID := waitMergedSignals(t, client, 800*time.Millisecond, 15*time.Second)
	if events == 0 {
		t.Fatal("并发请求后未收到任何 new-rows 信号")
	}
	if events >= burst {
		t.Fatalf("%d 发并发请求不该产生 %d 条信号（合并没有生效）", burst, events)
	}
	if total != burst {
		t.Fatalf("合并后的计数合计应为 %d（不得丢计数），实际 %d", burst, total)
	}
	if maxID <= 0 {
		t.Fatalf("合并后的 maxId 应为正数，实际 %d", maxID)
	}
	// 下界必落在本次突发的 id 区间里：maxID - minID 应 ≤ 突发行数-1（区间内无缝隙）。
	if minID <= 0 || minID > maxID {
		t.Fatalf("合并后的 minId 应落在 (0, maxId] 内，实际 minId=%d maxId=%d", minID, maxID)
	}
	if maxID-minID >= burst {
		t.Fatalf("下界偏离太远：maxId=%d minId=%d（跨度 %d 已超本次突发 %d 行）",
			maxID, minID, maxID-minID, burst)
	}
	t.Logf("节流：%d 发并发请求 → %d 条信号，计数合计=%d，maxId=%d，minId=%d（合并比 %.1fx）",
		burst, events, total, maxID, minID, float64(burst)/float64(events))
}

// 降级：Redis 不可达时，主请求照常成功（旁路不得影响转发与结算）。
func TestIntegrationUsageFeedRedisFailureKeepsRequestsWorking(t *testing.T) {
	pools := integrationStore(t)
	upstream := feedUpstream(t)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))

	// 指向必然拒连的端口。
	broken := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = broken.Close() })
	hub := usagefeed.NewHub(usagefeed.Options{Redis: broken, Logger: logx.New(nil)})
	t.Cleanup(func() { _ = hub.Close() })

	proxy := httptest.NewServer(feedHandler(t, pools, notifierOrNil{notifier: hub}))
	t.Cleanup(proxy.Close)

	// 主请求必须仍然 200（feedRedis 失败只记 warn）。
	sendFeedRequest(t, proxy, provisioned.apiKey)

	// 行照常落库。
	if id := rowIDAfter(t, provisioned); id == 0 {
		t.Fatal("Redis 不可达时请求行未落库")
	}
}

// 反证（配置层）：不装配通知面时，代理请求照常成功且**不发信号**。
// 这条保证上面那些绿色来自真实发布链路，而不是订阅端自造信号。
func TestIntegrationUsageFeedUnwiredSendsNoSignal(t *testing.T) {
	pools := integrationStore(t)
	redisClient := feedRedis(t)
	upstream := feedUpstream(t)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))

	hub := usagefeed.NewHub(usagefeed.Options{Redis: redisClient, Logger: logx.New(nil)})
	t.Cleanup(func() { _ = hub.Close() })

	// 数据面**不**接通知面（NewRows 为 nil）。
	proxy := httptest.NewServer(feedHandler(t, pools, notifierOrNil{}))
	t.Cleanup(proxy.Close)

	server := feedSSEServer(t, hub)
	client := openSSE(t, server.URL)

	sendFeedRequest(t, proxy, provisioned.apiKey)
	if id := waitForRowID(t, provisioned); id == 0 {
		t.Fatal("请求行未落库")
	}

	client.expectNoNewRows(t, 1500*time.Millisecond)
}

// feedRedisOptions 每次返回一份独立的客户端参数（多实例用例要两条独立连接池）。
func feedRedisOptions(t *testing.T) *redis.Options {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过推送端到端用例")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败（值已隐去）: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	return options
}
