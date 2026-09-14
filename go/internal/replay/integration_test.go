package replay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 集成测试门控：CCH_TEST_REDIS_URL + CCH_TEST_DSN 未设置时整组跳过。
// Redis 固定落 DB 13（仓库约定），跑完删除本测试写入的键与行。

const integrationDSNEnv = "CCH_TEST_DSN"
const integrationDB = 13
const testRedisEnv = "CCH_TEST_REDIS_URL"

func integrationRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv(testRedisEnv)
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 replay 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	options.DB = integrationDB
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func openTestPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv(integrationDSNEnv)
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过 replay 集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:      dsn,
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// testScenario 把「请求 -> 身份 -> 存储」绑定在一起，保证 spool 写入的条目与
// Attach 推导的条目是同一个（确定性身份契约）。
type testScenario struct {
	req     *pctx.Context
	message []byte
	store   *Store
}

func newScenario(t *testing.T, client redis.UniversalClient, pools *store.Pools, idempotencyKey, apiKey string) testScenario {
	t.Helper()
	message := []byte(`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	header := http.Header{}
	if idempotencyKey != "" {
		header.Set("idempotency-key", idempotencyKey)
	}
	req, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages", Headers: header})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	req.SetAuth(pctx.AuthState{KeyID: 7, UserID: 3, APIKey: apiKey})
	identity, err := DeriveIdentity(req, message, "anthropic")
	if err != nil || identity == nil {
		t.Fatalf("推导身份失败: %v", err)
	}
	storeInstance, err := NewStore(StoreOptions{Redis: client, Pools: pools, TTL: 5 * time.Minute})
	if err != nil {
		t.Fatalf("构造存储失败: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Del(context.Background(), ownerKey(identity.ReplayID), metaKey(identity.ReplayID), chunksKey(identity.ReplayID)).Err()
		if pools != nil {
			if pool, err := pools.Control(); err == nil {
				_, _ = pool.Exec(context.Background(), `DELETE FROM replay_payloads WHERE replay_id = $1`, identity.ReplayID)
			}
			// 审计行按 apiKey 清理。
			if pool, err := pools.Control(); err == nil {
				_, _ = pool.Exec(context.Background(),
					`DELETE FROM proj_applied_requests WHERE request_id IN (SELECT id FROM message_request WHERE key = $1)`, apiKey)
				_, _ = pool.Exec(context.Background(), `DELETE FROM message_request WHERE key = $1`, apiKey)
			}
		}
	})
	return testScenario{req: req, message: message, store: storeInstance}
}

func (sc *testScenario) identity(t *testing.T) Identity {
	t.Helper()
	identity, err := DeriveIdentity(sc.req, sc.message, "anthropic")
	if err != nil || identity == nil {
		t.Fatalf("推导身份失败: %v", err)
	}
	return *identity
}

func (sc *testScenario) attacher(pools *store.Pools, enabled bool) *Attacher {
	return NewAttacher(AttacherOptions{
		Store:         sc.store,
		Pools:         pools,
		ReplayEnabled: enabled,
		MessageFromRequest: func(context.Context, *pctx.Context) ([]byte, bool) {
			return sc.message, true
		},
		FormatOfRequest: func(context.Context, *pctx.Context) string { return "anthropic" },
	})
}

// TestWriteOwnedAndReadGeneration：owner 写入 -> 分代读取逐字节一致；换代读取被拒。
func TestWriteOwnedAndReadGeneration(t *testing.T) {
	client := integrationRedis(t)
	storeInstance, err := NewStore(StoreOptions{Redis: client, TTL: 5 * time.Minute})
	if err != nil {
		t.Fatalf("构造存储失败: %v", err)
	}
	replayID := identityReplayFor("write-read-" + itNonce())
	token := "owner-token-1"
	t.Cleanup(func() {
		_ = client.Del(context.Background(), ownerKey(replayID), metaKey(replayID), chunksKey(replayID)).Err()
	})
	ctx := context.Background()

	if ok := storeInstance.TryClaimOwner(ctx, replayID, token); !ok {
		t.Fatal("首次 claim 应成功")
	}
	if ok := storeInstance.TryClaimOwner(ctx, replayID, "其他-token"); ok {
		t.Fatal("重复 claim 应被拒绝")
	}

	meta := &Meta{Status: MetaOwning, Verifier: "v1", ScopeTag: "s1", Delivery: DeliveryStream}
	if _, outcome := storeInstance.WriteOwned(ctx, replayID, "wrong-token", meta, []string{"a"}); outcome != WriteLeaseLost {
		t.Fatalf("错误 token 写入应返回 WriteLeaseLost，收到 %v", outcome)
	}
	count, outcome := storeInstance.WriteOwned(ctx, replayID, token, meta, []string{"a", "b", "c"})
	if outcome != WriteOK || count != 3 {
		t.Fatalf("写入失败: count=%d outcome=%v", count, outcome)
	}
	if _, outcome := storeInstance.WriteOwned(ctx, replayID, token, meta, []string{"d"}); outcome != WriteOK {
		t.Fatalf("追加失败: %v", outcome)
	}

	messageRequestID := int64(99)
	metaWithID := &Meta{
		Status: MetaOwning, Verifier: "v1", ScopeTag: "s1", Delivery: DeliveryStream,
		MessageRequestID: &messageRequestID,
	}
	if _, outcome := storeInstance.WriteOwned(ctx, replayID, token, metaWithID, nil); outcome != WriteOK {
		t.Fatalf("写 meta 失败: %v", outcome)
	}

	chunks, readOutcome := storeInstance.ReadChunksForGeneration(ctx, replayID, messageRequestID, 0, 10, 0)
	if readOutcome != ReadOK {
		t.Fatalf("分代读取失败: %v", outcome)
	}
	if got := strings.Join(chunks, ""); got != "abcd" {
		t.Fatalf("读回内容不符: %q", got)
	}

	if _, readOutcome := storeInstance.ReadChunksForGeneration(ctx, replayID, messageRequestID+1, 0, 10, 0); readOutcome != ReadGenerationChanged {
		t.Fatalf("换代读取应被拒，收到 %v", outcome)
	}
}

// TestSpoolCompleteAndAttachRoundTrip：spool 写入 -> PG 持久化 -> Attach 命中，
// 正文逐字节一致。
func TestSpoolCompleteAndAttachRoundTrip(t *testing.T) {
	client := integrationRedis(t)
	pools := openTestPools(t)
	sc := newScenario(t, client, pools, "ik-roundtrip", "sk-it-1")
	ctx := context.Background()
	identity := sc.identity(t)

	if !sc.store.TryClaimOwner(ctx, identity.ReplayID, "owner-token-2") {
		t.Fatal("spool 创建前应先 claim owner")
	}

	var content bytes.Buffer
	for i := 0; i < 2600; i++ {
		content.WriteString(fmt.Sprintf("data: {\"index\":%d}\n\n", i))
	}

	spool := NewSpool(
		sc.store, identity, "owner-token-2", 200,
		map[string]string{"content-type": "text/event-stream"}, DeliveryStream,
		SpoolOptions{SourceMessageRequestID: 42, MaxConcurrentSpools: 8},
	)
	t.Cleanup(func() { spool.Abort("test_cleanup") })
	if spool == nil {
		t.Fatal("spool 创建失败")
	}
	raw := content.Bytes()
	for offset := 0; offset < len(raw); offset += 32 * 1024 {
		end := offset + 32*1024
		if end > len(raw) {
			end = len(raw)
		}
		spool.Observe(raw[offset:end])
	}
	if err := spool.CompleteAndPersist(ctx, 42); err != nil {
		t.Fatalf("完成屏障失败: %v", err)
	}

	persisted, err := sc.store.FindCompleted(ctx, identity.ReplayID)
	if err != nil {
		t.Fatalf("读回持久行失败: %v", err)
	}
	if persisted == nil {
		t.Fatal("持久行不存在")
	}
	if persisted.Payload != content.String() {
		t.Fatalf("持久正文不一致: %d != %d", len(persisted.Payload), len(raw))
	}
	if persisted.SourceMessageRequestID == nil || *persisted.SourceMessageRequestID != 42 {
		t.Fatalf("sourceMessageRequestId 丢失: %+v", persisted.SourceMessageRequestID)
	}

	attacher := sc.attacher(pools, true)
	response, err := attacher.Attach(ctx, sc.req)
	if err != nil {
		t.Fatalf("Attach 报错: %v", err)
	}
	if response == nil {
		t.Fatal("重复请求应命中重放")
	}
	if response.Status != 200 {
		t.Fatalf("命中响应状态码不对: %d", response.Status)
	}
	if !bytes.Equal(response.Body, raw) {
		t.Fatalf("命中正文不一致: %d != %d", len(response.Body), len(raw))
	}
	if response.Headers.Get("x-cch-replay") != "completed" {
		t.Fatalf("缺少 x-cch-replay 标记")
	}
	if ct := response.Headers.Get("content-type"); ct != "text/event-stream" {
		t.Fatalf("content-type 不对: %q", ct)
	}
}

// TestSpoolBoundedResidency：8 MiB 流全程本地驻留 < 1 MiB（issue-1408 的持有链结论）。
func TestSpoolBoundedResidency(t *testing.T) {
	client := integrationRedis(t)
	pools := openTestPools(t)
	sc := newScenario(t, client, pools, "ik-resident", "sk-it-2")
	identity := sc.identity(t)
	if !sc.store.TryClaimOwner(context.Background(), identity.ReplayID, "owner-token-3") {
		t.Fatal("spool 创建前应先 claim owner")
	}

	// 恰好压满默认缓存上限（8 MiB）：不再触发 payload_too_large 自失效，才能同时验证「驻留有界」
	// 与「完成后可持久化」两件事。超限情形由 TestSpoolPayloadCapSelfInvalidates 钉住。
	payload := makeStreamPayload(defaultMaxPayloadBytes)
	spool := NewSpool(
		sc.store, identity, "owner-token-3", 200,
		map[string]string{"content-type": "text/event-stream"}, DeliveryStream,
		SpoolOptions{SourceMessageRequestID: 43, MaxConcurrentSpools: 8},
	)
	t.Cleanup(func() { spool.Abort("test_cleanup") })
	if spool == nil {
		t.Fatal("spool 创建失败")
	}

	const chunkSize = 64 * 1024
	for offset := 0; offset < len(payload); offset += chunkSize {
		end := offset + chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		spool.Observe(payload[offset:end])
		// 热路径刚返回就断言：此刻未冲刷的待写批次也在预算内。
		if held := spool.HeldBytes(); held >= minResidencyLimit {
			t.Fatalf("本地驻留超限（热路径）: %d bytes", held)
		}
		time.Sleep(time.Millisecond) // 让写入链排空，模拟真实节奏
		if held := spool.HeldBytes(); held >= minResidencyLimit {
			t.Fatalf("本地驻留超限: %d bytes", held)
		}
	}
	if spool.IsReleased() {
		t.Fatal("8 MiB 恰好等于上限，不应自失效")
	}
	if err := spool.CompleteAndPersist(context.Background(), 43); err != nil {
		t.Fatalf("完成屏障失败: %v", err)
	}
	persisted, err := sc.store.FindCompleted(context.Background(), identity.ReplayID)
	if err != nil || persisted == nil {
		t.Fatalf("持久化结果缺失: %v", err)
	}
	if persisted.ByteSize != int64(len(payload)) || len(persisted.Payload) != len(payload) {
		t.Fatalf("持久化正文不完整: %d != %d", len(persisted.Payload), len(payload))
	}
	if persisted.Payload != string(payload) {
		t.Fatal("持久化正文与写入字节不逐字节一致")
	}
	// 全部字节都以 ≤64 KiB 的块落到了 Redis（chunkCount 是热层已确认的块数）。
	if chunks := spool.ChunkCount(); chunks*maxRedisChunkBytes < int64(len(payload)) {
		t.Fatalf("热层块数不足: %d", chunks)
	}
}

// TestSpoolPayloadCapSelfInvalidates：超限即自失效（fail-open），绝不落成不完整的回放。
func TestSpoolPayloadCapSelfInvalidates(t *testing.T) {
	client := integrationRedis(t)
	pools := openTestPools(t)
	sc := newScenario(t, client, pools, "ik-cap", "sk-it-2b")
	ctx := context.Background()
	identity := sc.identity(t)
	if !sc.store.TryClaimOwner(ctx, identity.ReplayID, "owner-token-cap") {
		t.Fatal("spool 创建前应先 claim owner")
	}

	// 恰好超出默认上限 1 字节。
	payload := makeStreamPayload(defaultMaxPayloadBytes + 1)
	var inactiveCalls atomic.Int64
	spool := NewSpool(
		sc.store, identity, "owner-token-cap", 200,
		map[string]string{"content-type": "text/event-stream"}, DeliveryStream,
		SpoolOptions{
			SourceMessageRequestID: 44,
			MaxConcurrentSpools:    8,
			OnInactive:             func() { inactiveCalls.Add(1) },
		},
	)
	if spool == nil {
		t.Fatal("spool 创建失败")
	}

	const chunkSize = 64 * 1024
	for offset := 0; offset < len(payload); offset += chunkSize {
		end := offset + chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		spool.Observe(payload[offset:end])
	}
	waitForRelease(t, spool)

	if err := spool.CompleteAndPersist(ctx, 44); err != nil {
		t.Fatalf("已失效的 spool 收尾应为空操作: %v", err)
	}
	persisted, err := sc.store.FindCompleted(ctx, identity.ReplayID)
	if err != nil {
		t.Fatalf("读回出错: %v", err)
	}
	if persisted != nil {
		t.Fatal("超限条目绝不落库")
	}
	chunks, outcome := sc.store.ReadChunksForGeneration(ctx, identity.ReplayID, 44, 0, 8, time.Second)
	if outcome != ReadOK || len(chunks) != 0 {
		t.Fatalf("自失效后不应再留响应块: outcome=%v chunks=%d", outcome, len(chunks))
	}
	if calls := inactiveCalls.Load(); calls != 1 {
		t.Fatalf("失去活跃写角色应恰好回调一次: %d", calls)
	}
}

// makeStreamPayload 构造恰好 size 字节的 SSE 流（超出最后一帧即截断，字节内容不影响判定）。
func makeStreamPayload(size int64) []byte {
	frame := []byte("data: big\n\n")
	payload := bytes.Repeat(frame, int(size)/len(frame)+1)
	return payload[:size]
}

// waitForRelease 等待 spool 终态清理释放（自失效走异步写入链）。
func waitForRelease(t *testing.T, spool *Spool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if spool.IsReleased() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("spool 未在期限内释放")
}

// minResidencyLimit 是内存不变量断言的阈值（1 MiB）。
const minResidencyLimit = int64(1024 * 1024)

// TestAttachMissRegistersOwner：未命中 -> 放行，claim 成功（ClaimHook 被调）。
func TestAttachMissRegistersOwner(t *testing.T) {
	client := integrationRedis(t)
	sc := newScenario(t, client, nil, "ik-miss", "sk-it-3")

	var claimed *Claim
	attacher := NewAttacher(AttacherOptions{
		Store:         sc.store,
		ReplayEnabled: true,
		MessageFromRequest: func(context.Context, *pctx.Context) ([]byte, bool) {
			return sc.message, true
		},
		FormatOfRequest: func(context.Context, *pctx.Context) string { return "anthropic" },
		ClaimHook: func(_ *pctx.Context, claim Claim) {
			claimed = &claim
		},
	})
	response, err := attacher.Attach(context.Background(), sc.req)
	if err != nil || response != nil {
		t.Fatalf("未命中应放行（nil, nil），收到 %v, %v", response, err)
	}
	if claimed == nil || claimed.OwnerToken == "" || claimed.ID.ReplayID != sc.identity(t).ReplayID {
		t.Fatalf("claim 回调缺失或不对: %+v", claimed)
	}
	if ok := sc.store.RenewOwnerLease(context.Background(), claimed.ID.ReplayID, claimed.OwnerToken); !ok {
		t.Fatal("claim 后的租约应仍有效")
	}
}

// TestAttachVerifierMismatch：verifier 不符（哈希碰撞语义）绝不命中。
func TestAttachVerifierMismatch(t *testing.T) {
	client := integrationRedis(t)
	sc := newScenario(t, client, nil, "ik-vf", "sk-it-4")
	ctx := context.Background()
	identity := sc.identity(t)

	// 用错误 verifier 伪造一条 completed 热层条目。
	token := "owner-token-4"
	if !sc.store.TryClaimOwner(ctx, identity.ReplayID, token) {
		t.Fatal("claim 失败")
	}
	requestID := int64(1)
	requestIDPtr := &requestID
	meta := &Meta{
		Status: MetaCompleted, Verifier: strings.Repeat("x", 32), ScopeTag: identity.ScopeTag,
		Delivery: DeliveryStream, ChunkCount: 1, HeartbeatAt: time.Now().UnixMilli(),
		MessageRequestID: requestIDPtr,
	}
	if _, outcome := sc.store.WriteOwned(ctx, identity.ReplayID, token, meta, []string{"data: x\n\n"}); outcome != WriteOK {
		t.Fatalf("写入失败: %v", outcome)
	}
	if !sc.store.CompleteOwned(ctx, identity.ReplayID, token, meta) {
		t.Fatal("翻转 completed 失败")
	}

	response, err := sc.attacher(nil, true).Attach(ctx, sc.req)
	if err != nil || response != nil {
		t.Fatalf("verifier 不符绝不错发: %v, %v", response, err)
	}
}

// TestSpoolAbortBlocksHit：aborted 条目绝不被命中。
func TestSpoolAbortBlocksHit(t *testing.T) {
	client := integrationRedis(t)
	pools := openTestPools(t)
	sc := newScenario(t, client, pools, "ik-aborted", "sk-it-5")
	ctx := context.Background()
	identity := sc.identity(t)

	token := "owner-token-5"
	if !sc.store.TryClaimOwner(ctx, identity.ReplayID, token) {
		t.Fatal("claim 失败")
	}
	if !sc.store.PrepareOwned(ctx, identity.ReplayID, token) {
		t.Fatal("prepare 失败")
	}
	meta := &Meta{Status: MetaOwning, Verifier: identity.Verifier, ScopeTag: identity.ScopeTag,
		Delivery: DeliveryStream, HeartbeatAt: time.Now().UnixMilli()}
	if _, outcome := sc.store.WriteOwned(ctx, identity.ReplayID, token, meta, []string{"data: half\n\n"}); outcome != WriteOK {
		t.Fatalf("写入失败: %v", outcome)
	}
	aborted := &Meta{Status: MetaAborted, Verifier: identity.Verifier, ScopeTag: identity.ScopeTag,
		Delivery: DeliveryStream, HeartbeatAt: time.Now().UnixMilli(), AbortReason: "test"}
	if !sc.store.AbortOwned(ctx, identity.ReplayID, token, aborted) {
		t.Fatal("abort 失败")
	}

	response, err := sc.attacher(pools, true).Attach(ctx, sc.req)
	if err != nil || response != nil {
		t.Fatalf("aborted 条目不得命中: %v, %v", response, err)
	}
}

// TestPersistCompletedConflictAndCleanup：持久层一致/冲突/过期清理。
func TestPersistCompletedConflictAndCleanup(t *testing.T) {
	pools := openTestPools(t)
	client := integrationRedis(t)
	storeInstance, err := NewStore(StoreOptions{Redis: client, Pools: pools, TTL: 5 * time.Minute})
	if err != nil {
		t.Fatalf("构造存储失败: %v", err)
	}
	ctx := context.Background()
	replayID := identityReplayFor("persist-conflict-" + itNonce())
	t.Cleanup(func() {
		_ = client.Del(context.Background(), ownerKey(replayID), metaKey(replayID), chunksKey(replayID)).Err()
		if pool, err := pools.Control(); err == nil {
			_, _ = pool.Exec(ctx, `DELETE FROM replay_payloads WHERE replay_id = $1`, replayID)
		}
	})

	row := PersistedRow{
		ReplayID: replayID, Verifier: "v1", ScopeTag: "s1", KeyID: 7, UserID: 3,
		Format: "anthropic", StatusCode: 200,
		Headers: map[string]string{"content-type": "text/event-stream"},
		Payload: "data: a\n\n", ByteSize: 9,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	result, err := storeInstance.PersistCompleted(ctx, row)
	if err != nil || result != PersistWritten {
		t.Fatalf("首次持久化应写入: %s, %v", result, err)
	}
	result, err = storeInstance.PersistCompleted(ctx, row)
	if err != nil || result != PersistExisting {
		t.Fatalf("一致行应返回 existing: %s, %v", result, err)
	}
	row.Payload = "data: b\n\n"
	if _, err := storeInstance.PersistCompleted(ctx, row); err == nil {
		t.Fatal("不一致行必须报冲突")
	} else if !isDurableConflict(err) {
		t.Fatalf("应判别为 DurableConflictError: %v", err)
	}
	deleted, err := storeInstance.CleanupExpired(ctx, time.Now().Add(time.Hour))
	if err != nil || deleted == 0 {
		t.Fatalf("过期清理应删行: %d, %v", deleted, err)
	}
	if persisted, _ := storeInstance.FindCompleted(ctx, replayID); persisted != nil {
		t.Fatal("过期行清理后不应读回")
	}
}

// TestNewSpoolCapRejects：并发上限拒绝新 spool（降级不排队）。
func TestNewSpoolCapRejects(t *testing.T) {
	client := integrationRedis(t)
	storeInstance, err := NewStore(StoreOptions{Redis: client, TTL: time.Minute})
	if err != nil {
		t.Fatalf("构造存储失败: %v", err)
	}
	identity := fixtureIdentity()
	first := NewSpool(storeInstance, identity, "cap-token", 200, map[string]string{},
		DeliveryStream, SpoolOptions{MaxConcurrentSpools: 1})
	if first == nil {
		t.Fatal("第一个 spool 应能创建")
	}
	second := NewSpool(storeInstance, identity, "cap-token-2", 200, map[string]string{},
		DeliveryStream, SpoolOptions{MaxConcurrentSpools: 1})
	if second != nil {
		t.Fatal("超上限时应放弃（nil），不排队")
	}
	first.Abort("test")
}

// fixtureIdentity 是并发上限用例用的固定身份（不涉及身份推导）。
func fixtureIdentity() Identity {
	return Identity{
		ReplayID: "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr",
		Verifier: "vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv",
		ScopeTag: "ssssssssssssssss",
		KeyID:    7,
		UserID:   3,
		Format:   "anthropic",
		Model:    "gpt-test",
		Endpoint: "/v1/messages",
	}
}

// identityReplayFor 按固定盐推导确定性 replayId（测试夹具用）。
func identityReplayFor(seed string) string {
	return sha256Hex("test|" + seed)[:32]
}

// itNonce 生成本次运行的唯一后缀，避免确定性 id 与中断运行留下的旧数据撞车。
func itNonce() string {
	return fmt.Sprint(time.Now().UnixNano())
}

// isDurableConflict 判读 DurableConflictError。
func isDurableConflict(err error) bool {
	var conflict *DurableConflictError
	return errors.As(err, &conflict)
}
