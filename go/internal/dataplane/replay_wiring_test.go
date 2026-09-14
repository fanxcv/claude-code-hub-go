package dataplane

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/replay"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是回放接线的验收：真 Redis + 真 PG + httptest 假上游，覆盖三件事——
//
//  1. 写入 -> 命中 -> 逐字节读回一致（命中短路且不再拨上游）；
//  2. 失败终态必须 abort（半截流绝不被命中）；
//  3. 单流本地驻留 < 1 MiB（8 MiB 流），即 issue-1408 的持有链结论在接线层同样成立。
//
// 门控：未设置 CCH_TEST_REDIS_URL / CCH_TEST_DSN 时整组跳过。

// replayTestClient 建 Redis 命令客户端；未设置门控变量时跳过。
func replayTestClient(t *testing.T) *redis.Client {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过回放接线集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败: %v", err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// keyIDFor 读回密钥行标识：身份里的 keyId 必须与鉴权层认定的同一个值，
// 自造一个数字会让测试推导出的 replayId 与生产实际使用的不一致（测试恒 miss 却看不出来）。
func keyIDFor(t *testing.T, pools *store.Pools, apiKey string) int64 {
	t.Helper()
	reader, err := pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	var keyID int64
	if err := reader.QueryRow(context.Background(),
		`SELECT id FROM keys WHERE key = $1 LIMIT 1`, apiKey,
	).Scan(&keyID); err != nil {
		t.Fatalf("读取密钥 id 失败: %v", err)
	}
	return keyID
}

// replayIdentityFor 用生产代码推导本次请求的回放身份（不得在测试里另算一遍哈希：
// 那会变成「测试与实现各算一套」，两边一旦分叉，测试仍会绿）。
func replayIdentityFor(t *testing.T, apiKey string, keyID, userID int64, body string) replay.Identity {
	t.Helper()
	pc, err := pctx.New(pctx.Init{
		Method:   http.MethodPost,
		Path:     "/v1/messages",
		Headers:  http.Header{"Content-Type": []string{"application/json"}},
		Body:     io.NopCloser(strings.NewReader(body)),
		ClientIP: "127.0.0.1",
		Logger:   logx.New(nil),
	})
	if err != nil {
		t.Fatalf("构造 pctx 失败: %v", err)
	}
	pc.SetAuth(pctx.AuthState{KeyID: keyID, UserID: userID, APIKey: apiKey, KeyName: "replay-it"})
	identity, err := replay.DeriveIdentity(pc, []byte(body), "claude")
	if err != nil || identity == nil {
		t.Fatalf("身份推导失败: %v", err)
	}
	return *identity
}

// cleanupReplayEntry 删掉本次用例在热层与持久层留下的条目（夹具自清）。
func cleanupReplayEntry(t *testing.T, client *redis.Client, pools *store.Pools, replayID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, prefix := range []string{"cch:replay:owner:", "cch:replay:meta:", "cch:replay:chunks:"} {
			_ = client.Del(ctx, prefix+replayID).Err()
		}
		writer, err := pools.Writer()
		if err != nil {
			return
		}
		_, _ = writer.Exec(ctx, `DELETE FROM replay_payloads WHERE replay_id = $1`, replayID)
	})
}

// sseFrames 是本次用例的假上游帧：决定性内容帧 + 终止帧（有终止标记才算「干净完成」）。
func sseFrames(payloadBytes int) []string {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"甲\"}}\n\n",
	}
	if payloadBytes > 0 {
		filler := strings.Repeat("乙", payloadBytes)
		frames = append(frames, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\""+filler+"\"}}\n\n")
	}
	frames = append(frames,
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	return frames
}

// replayAwareHandler 用真 Redis + 真 PG 装配数据面并打开回放。
func replayAwareHandler(t *testing.T, provisioned provisioning, client *redis.Client) http.Handler {
	t.Helper()
	assembly, err := NewStoreBacked(StoreOptions{
		Pools:  provisioned.pools,
		Redis:  client,
		Logger: logx.New(nil),
		// 出厂开关打开；设置表若为 NULL 则由它决定（与 Node 同序）。
		ReplayEnabled:         true,
		ReplayTTLSeconds:      60,
		ReplayMaxPayloadBytes: 8 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("数据面装配失败: %v", err)
	}
	return assembly.Handler
}

// doReplayRequest 发一次请求并完整读回正文。
func doReplayRequest(t *testing.T, client *http.Client, url, apiKey, body string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", apiKey)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return response, string(raw)
}

// waitReplayCompleted 等 owner 把条目置为 completed（完成屏障在流终态之后异步执行）。
func waitReplayCompleted(t *testing.T, client *redis.Client, replayID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	ctx := context.Background()
	for {
		raw, err := client.Get(ctx, "cch:replay:meta:"+replayID).Result()
		if err == nil && strings.Contains(raw, `"status":"completed"`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("回放条目未在 10s 内完成：最后一次读取 err=%v raw=%s", err, raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 写入 -> 命中 -> 逐字节读回一致，且命中不再拨上游。
func TestReplayWiringWriteThenHitServesByteIdentical(t *testing.T) {
	client := replayTestClient(t)
	pools := integrationStore(t)

	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range sseFrames(0) {
			_, _ = io.WriteString(w, frame)
		}
		flusher.Flush()
	}))
	defer upstream.Close()

	provisioned := provision(t, pools, upstream.URL, "claude")
	handler := replayAwareHandler(t, provisioned, client)
	server := httptest.NewServer(handler)
	defer server.Close()

	// idempotency-key 让本用例的身份唯一：共享 Redis 里的既有条目不会串到本次断言上。
	const body = `{"model":"claude-sonnet-4-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"回放接线"}]}`
	keyID := keyIDFor(t, pools, provisioned.apiKey)
	identity := replayIdentityFor(t, provisioned.apiKey, keyID, provisioned.userID, body)
	cleanupReplayEntry(t, client, pools, identity.ReplayID)

	// 第一遍：owner 写入。
	first, firstBody := doReplayRequest(t, server.Client(), server.URL+"/v1/messages", provisioned.apiKey, body)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("首遍状态码应为 200，收到 %d", first.StatusCode)
	}
	if first.Header.Get("x-cch-replay") != "" {
		t.Fatalf("首遍不应带重放标记，收到 %q", first.Header.Get("x-cch-replay"))
	}
	if hits := upstreamHits.Load(); hits != 1 {
		t.Fatalf("首遍应拨上游一次，实际 %d", hits)
	}
	waitReplayCompleted(t, client, identity.ReplayID)

	// 键形制与持久层都要如实存在：切换期间「两侧互相命中」靠的就是这三个键与 replay_payloads
	// 的同一行。缺任一项，另一侧（Node）就看不到本次缓存。
	ctx := context.Background()
	for _, key := range []string{
		"cch:replay:meta:" + identity.ReplayID,
		"cch:replay:chunks:" + identity.ReplayID,
	} {
		if exists, err := client.Exists(ctx, key).Result(); err != nil || exists != 1 {
			t.Fatalf("热层键 %s 应存在（err=%v exists=%d）", key, err, exists)
		}
	}
	// owner 租约在 completed 翻转时同时释放（complete_owned 的 DEL）：留着它会把同一请求的
	// 重试挡满整个 TTL，而条目本身已经可服务。
	if exists, err := client.Exists(ctx, "cch:replay:owner:"+identity.ReplayID).Result(); err != nil || exists != 0 {
		t.Fatalf("completed 后 owner 租约应已释放（err=%v exists=%d）", err, exists)
	}
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	var persistedRows int
	if err := writer.QueryRow(ctx,
		`SELECT count(*) FROM replay_payloads WHERE replay_id = $1`, identity.ReplayID,
	).Scan(&persistedRows); err != nil {
		t.Fatalf("查询持久层失败: %v", err)
	}
	if persistedRows != 1 {
		t.Fatalf("持久层应有且只有一行完成条目，实际 %d 行", persistedRows)
	}

	// 第二遍：同一个请求体必须命中热层，且正文与首遍逐字节一致。
	second, secondBody := doReplayRequest(t, server.Client(), server.URL+"/v1/messages", provisioned.apiKey, body)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("命中状态码应为 200，收到 %d", second.StatusCode)
	}
	if marker := second.Header.Get("x-cch-replay"); marker != "completed" {
		t.Fatalf("命中应带 x-cch-replay: completed，收到 %q", marker)
	}
	if secondBody != firstBody {
		t.Fatalf("命中正文与首遍不一致（%d != %d 字节）", len(secondBody), len(firstBody))
	}
	if hits := upstreamHits.Load(); hits != 1 {
		t.Fatalf("命中不得再拨上游，实际上游命中 %d 次", hits)
	}
	// 事件序列仍可逐事件读：命中应答必须是同一条 SSE，而不是一坨正文。
	events := make([]string, 0, 4)
	scanner := bufio.NewScanner(strings.NewReader(secondBody))
	for scanner.Scan() {
		if line := scanner.Text(); strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	want := "message_start,content_block_delta,message_delta,message_stop"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("命中事件序列应为 %s，收到 %s", want, got)
	}

	// 命中也不得因重放而多开请求行：同一请求体只落一行账（首遍那一行）。
	waitForFinalizedRow(t, provisioned)
	if rows := countRequestRows(t, provisioned); rows != 1 {
		t.Fatalf("重放命中不应开新请求行，实际 %d 行", rows)
	}
}

// 失败终态必须 abort：被 aborted 的条目绝不被命中（半截流比 miss 更糟）。
//
// 三种失败收尾（客户端中断 / 上游截断 / 静默超时）都走同一条判定：终态不是「干净完成」
// 就不许置 completed。此处直接驱动终态判定，避开真实引流窗口（客户端中断后上游默认还会
// 被引流一段以拿真实用量，那是另一条链的语义，与本条无关）。
func TestReplaySessionAbortsOnFailureTerminals(t *testing.T) {
	for _, testCase := range []struct {
		name string
		kind forward.TerminalKind
	}{
		{"客户端中断", forward.TerminalClientAborted},
		{"上游截断", forward.TerminalUpstreamTruncated},
		{"静默超时", forward.TerminalIdleTimeout},
		{"本地错误", forward.TerminalLocalError},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := replayTestClient(t)
			pools := integrationStore(t)
			replayStore, err := replay.NewStore(replay.StoreOptions{Redis: client, Pools: pools, TTL: 60 * time.Second})
			if err != nil {
				t.Fatalf("回放存储构造失败: %v", err)
			}
			body := `{"model":"claude-sonnet-4-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"终态"}]}`
			identity := replayIdentityFor(t, "sk-replay-terminal", 737373, 646464, body)
			cleanupReplayEntry(t, client, pools, identity.ReplayID)

			ctx := context.Background()
			if !replayStore.TryClaimOwner(ctx, identity.ReplayID, "terminal-token") {
				t.Fatal("claim owner 失败")
			}
			session := &replaySession{
				wiring: &ReplayWiring{Store: replayStore, Pools: pools},
				logger: logx.New(nil),
				claim:  &replay.Claim{ID: identity, OwnerToken: "terminal-token"},
			}
			spool := session.startStream(ctx, http.StatusOK, http.Header{"Content-Type": []string{"text/event-stream"}})
			if spool == nil {
				t.Fatal("流式交付应建 spool")
			}
			session.observe([]byte("event: message_start\ndata: {}\n\n"))

			session.completeAfterSettle(ctx, forward.StreamOutcome{Kind: testCase.kind})

			persisted, err := replayStore.FindCompleted(ctx, identity.ReplayID)
			if err != nil {
				t.Fatalf("查询持久层失败: %v", err)
			}
			if persisted != nil {
				t.Fatalf("%s 的终态不得落成可重放条目（status=%d）", testCase.name, persisted.StatusCode)
			}
		})
	}
}

// 接线层的内存不变量：8 MiB 流全程 spool 本地驻留 < 1 MiB。
//
// 与 replay 包自身的驻留用例互补：那条钉住 spool 的内部预算，本条钉住**接线不会自己攒正文**
// （session 只搬运，累积全部发生在 spool 的有界批次里）。
func TestReplaySessionBoundedResidency(t *testing.T) {
	client := replayTestClient(t)
	pools := integrationStore(t)
	logger := logx.New(nil)
	replayStore, err := replay.NewStore(replay.StoreOptions{Redis: client, Pools: pools, TTL: 60 * time.Second})
	if err != nil {
		t.Fatalf("回放存储构造失败: %v", err)
	}

	const (
		payloadBytes = 8 * 1024 * 1024
		chunkBytes   = 64 * 1024
		residencyCap = 1024 * 1024
	)
	body := `{"model":"claude-sonnet-4-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"驻留"}]}`
	identity := replayIdentityFor(t, "sk-replay-residency", 424242, 31337, body)
	cleanupReplayEntry(t, client, pools, identity.ReplayID)

	ctx := context.Background()
	if !replayStore.TryClaimOwner(ctx, identity.ReplayID, "residency-token") {
		t.Fatal("claim owner 失败")
	}
	session := &replaySession{
		wiring: &ReplayWiring{Store: replayStore, Pools: pools},
		logger: logger,
		claim:  &replay.Claim{ID: identity, OwnerToken: "residency-token"},
	}
	headers := http.Header{"Content-Type": []string{"text/event-stream"}}
	spool := session.startStream(ctx, http.StatusOK, headers)
	if spool == nil {
		t.Fatal("流式交付应建 spool")
	}
	t.Cleanup(func() { spool.Abort("test_cleanup") })

	chunk := make([]byte, chunkBytes)
	for offset := 0; offset < payloadBytes; offset += chunkBytes {
		session.observe(chunk)
		// 热路径刚返回就断言：此刻未冲刷的待写批次也在预算内。
		if held := spool.HeldBytes(); held >= residencyCap {
			t.Fatalf("接线层本地驻留超限：%d bytes", held)
		}
		time.Sleep(time.Millisecond) // 让写入链排空，模拟真实节奏（与 replay 包同款夹具）
		if held := spool.HeldBytes(); held >= residencyCap {
			t.Fatalf("接线层本地驻留超限（排空后）：%d bytes", held)
		}
	}
	// 字节必须真的过了接线（否则「驻留有界」会因为 observe 是空操作而变成假绿）。
	deadline := time.Now().Add(10 * time.Second)
	for spool.ChunkCount()*64*1024 < payloadBytes {
		if time.Now().After(deadline) {
			t.Fatalf("接线未把全部字节喂给 spool：已确认 %d 块", spool.ChunkCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if held := spool.HeldBytes(); held >= residencyCap {
		t.Fatalf("收尾时本地驻留超限：%d bytes", held)
	}
}

// 不可回放的交付必须释放 owner 租约，否则同一请求的重试会被残留租约挡满整个 TTL。
func TestReplaySessionReleasesOwnershipWhenIneligible(t *testing.T) {
	client := replayTestClient(t)
	pools := integrationStore(t)
	replayStore, err := replay.NewStore(replay.StoreOptions{Redis: client, Pools: pools, TTL: 60 * time.Second})
	if err != nil {
		t.Fatalf("回放存储构造失败: %v", err)
	}
	body := `{"model":"claude-sonnet-4-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"不合格"}]}`
	identity := replayIdentityFor(t, "sk-replay-ineligible", 515151, 424242, body)
	cleanupReplayEntry(t, client, pools, identity.ReplayID)

	ctx := context.Background()
	if !replayStore.TryClaimOwner(ctx, identity.ReplayID, "ineligible-token") {
		t.Fatal("claim owner 失败")
	}
	session := &replaySession{
		wiring: &ReplayWiring{Store: replayStore, Pools: pools},
		logger: logx.New(nil),
		claim:  &replay.Claim{ID: identity, OwnerToken: "ineligible-token"},
	}
	// 非 2xx（上游错误）不建 spool：必须把租约还回去。
	if spool := session.startStream(ctx, http.StatusBadGateway, http.Header{"Content-Type": []string{"text/event-stream"}}); spool != nil {
		t.Fatal("非 2xx 不应建 spool")
	}
	if !replayStore.TryClaimOwner(ctx, identity.ReplayID, "second-token") {
		t.Fatal("释放后应能重新 claim（否则重试会被残留租约挡住）")
	}
	replayStore.ReleaseOwner(ctx, identity.ReplayID, "second-token")

	// 交付类型不匹配（stream 交付却非 SSE）同样释放。
	if !replayStore.TryClaimOwner(ctx, identity.ReplayID, "third-token") {
		t.Fatal("claim owner 失败")
	}
	session.claim = &replay.Claim{ID: identity, OwnerToken: "third-token"}
	if spool := session.startStream(ctx, http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}); spool != nil {
		t.Fatal("stream 交付遇到非 SSE 不应建 spool")
	}
	if !replayStore.TryClaimOwner(ctx, identity.ReplayID, "fourth-token") {
		t.Fatal("交付类型不匹配时也应释放租约")
	}
	replayStore.ReleaseOwner(ctx, identity.ReplayID, "fourth-token")
}

// 并发 spool 上限：超限即放弃回放并释放租约（不排队），与 Node 的 declineOwnership 同。
func TestReplaySessionCapDegradesWithoutQueueing(t *testing.T) {
	client := replayTestClient(t)
	pools := integrationStore(t)
	replayStore, err := replay.NewStore(replay.StoreOptions{Redis: client, Pools: pools, TTL: 60 * time.Second})
	if err != nil {
		t.Fatalf("回放存储构造失败: %v", err)
	}
	body := `{"model":"claude-sonnet-4-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"上限"}]}`
	identity := replayIdentityFor(t, "sk-replay-cap", 616161, 525252, body)
	cleanupReplayEntry(t, client, pools, identity.ReplayID)

	ctx := context.Background()
	if !replayStore.TryClaimOwner(ctx, identity.ReplayID, "cap-token") {
		t.Fatal("claim owner 失败")
	}
	session := &replaySession{
		// MaxConcurrentSpools=0 走 replay 包默认；本用例用 1 压满名额后断言第二路降级。
		wiring: &ReplayWiring{Store: replayStore, Pools: pools, MaxConcurrentSpools: 1},
		logger: logx.New(nil),
		claim:  &replay.Claim{ID: identity, OwnerToken: "cap-token"},
	}
	headers := http.Header{"Content-Type": []string{"text/event-stream"}}
	first := session.startStream(ctx, http.StatusOK, headers)
	if first == nil {
		t.Fatal("首个 spool 应建立")
	}
	t.Cleanup(func() { first.Abort("test_cleanup") })

	// 第二路：不同身份、同一并发名额，必须降级为「不做回放」并释放自己的租约。
	secondIdentity := replayIdentityFor(t, "sk-replay-cap-2", 616162, 525252, body)
	cleanupReplayEntry(t, client, pools, secondIdentity.ReplayID)
	if !replayStore.TryClaimOwner(ctx, secondIdentity.ReplayID, "cap-token-2") {
		t.Fatal("claim owner 失败")
	}
	second := &replaySession{
		wiring: session.wiring,
		logger: logx.New(nil),
		claim:  &replay.Claim{ID: secondIdentity, OwnerToken: "cap-token-2"},
	}
	if spool := second.startStream(ctx, http.StatusOK, headers); spool != nil {
		t.Fatal("并发上限已满时应降级为不回放")
	}
	if !replayStore.TryClaimOwner(ctx, secondIdentity.ReplayID, "cap-token-3") {
		t.Fatalf("降级后应释放租约，否则 %s 会被挡满整个 TTL", secondIdentity.ReplayID)
	}
	replayStore.ReleaseOwner(ctx, secondIdentity.ReplayID, "cap-token-3")
}
