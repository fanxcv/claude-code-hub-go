package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// opsTestPools 读门控变量建连接池；未设置 CCH_TEST_DSN 时跳过。
// 分道预算取 6（与 store 包的门禁用例同量级），避免与其它包的集成测试抢连接。
func opsTestPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
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

// opsTestRedis 读门控变量建客户端；库号固定落 13（URL 自带库号时以其为准）。
func opsTestRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
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

// opsFixtureKey 生成本次用例的唯一标记：按精确 id 清理，绝不按前缀批量删。
func opsFixtureKey(t *testing.T) string {
	t.Helper()
	return "go-jobs-it-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// opsCreateRequestRow 建一行调用记录并登记精确清理（含触发器写出的账本行）。
func opsCreateRequestRow(t *testing.T, pools *store.Pools, key string) int64 {
	t.Helper()
	ctx := context.Background()
	cost := "0.000000000000000"
	model := "go-jobs-it-model"
	endpoint := "/v1/messages"
	request, err := pools.CreateMessageRequest(ctx, store.CreateMessageRequestData{
		ProviderID: 1,
		UserID:     1,
		Key:        key,
		Model:      &model,
		Endpoint:   &endpoint,
		CostUSD:    &cost,
		IsReplay:   false,
	})
	if err != nil {
		t.Fatalf("创建 message_request 失败: %v", err)
	}
	t.Cleanup(func() {
		pool, poolErr := pools.Control()
		if poolErr != nil {
			t.Errorf("清理时取分道失败: %v", poolErr)
			return
		}
		cleanupCtx := context.Background()
		for _, statement := range []string{
			`DELETE FROM proj_applied_requests WHERE request_id = $1`,
			`DELETE FROM usage_ledger WHERE request_id = $1`,
			`DELETE FROM message_request WHERE id = $1`,
		} {
			if _, execErr := pool.Exec(cleanupCtx, statement, request.ID); execErr != nil {
				t.Errorf("清理失败 (%s): %v", statement, execErr)
			}
		}
	})
	return request.ID
}

// opsReadRoutingTrace 读回某行的 routing_trace 原文。
func opsReadRoutingTrace(t *testing.T, pools *store.Pools, requestID int64) string {
	t.Helper()
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var trace *string
	if err := pool.QueryRow(context.Background(),
		`SELECT routing_trace::text FROM message_request WHERE id = $1`, requestID,
	).Scan(&trace); err != nil {
		t.Fatalf("读取 routing_trace 失败: %v", err)
	}
	if trace == nil {
		return ""
	}
	return *trace
}

// opsStageOutboxEntry 按 Node stageRoutingTraceOutbox 的载荷形状写入 Redis（含有界索引）。
func opsStageOutboxEntry(t *testing.T, client redis.UniversalClient, requestID int64, revision float64) string {
	t.Helper()
	trace := fmt.Sprintf(`{"version":1,"updatedAt":%d,"steps":[]}`, int64(revision))
	payload := fmt.Sprintf(
		`{"version":1,"requestId":%d,"traceUpdatedAt":%d,"routingTrace":%s}`,
		requestID, int64(revision), trace,
	)
	ctx := context.Background()
	field := strconv.FormatInt(requestID, 10)
	if err := client.HSet(ctx, OutboxHashKey, field, payload).Err(); err != nil {
		t.Fatalf("写入 outbox 失败: %v", err)
	}
	if err := client.ZAdd(ctx, OutboxIndexKey, redis.Z{
		Score:  float64(time.Now().UnixMilli()),
		Member: field,
	}).Err(); err != nil {
		t.Fatalf("写入 outbox 索引失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = client.HDel(cleanupCtx, OutboxHashKey, field).Err()
		_ = client.ZRem(cleanupCtx, OutboxIndexKey, field).Err()
	})
	return payload
}

func opsNewTestDeps(t *testing.T, pools *store.Pools, client redis.UniversalClient) OpsDeps {
	t.Helper()
	return OpsDeps{
		Pools:  pools,
		Redis:  client,
		Logger: opsTestLogger(t),
	}
}

// outbox 回放端到端：Node 侧的暂存条目被 Go 认领、落库、并从 Redis 删除。
func TestIntegrationOutboxReplayEndToEnd(t *testing.T) {
	pools := opsTestPools(t)
	client := opsTestRedis(t)
	ctx := context.Background()

	key := opsFixtureKey(t)
	requestID := opsCreateRequestRow(t, pools, key)

	// 初始轨迹为空：outbox 回放是「补写轨迹」的唯一路径。
	if got := opsReadRoutingTrace(t, pools, requestID); got != "" && got != "null" {
		t.Fatalf("新行不应有轨迹，得到 %q", got)
	}

	revision := float64(time.Now().UnixMilli())
	payload := opsStageOutboxEntry(t, client, requestID, revision)

	replay := NewOutboxReplay(opsNewTestDeps(t, pools, client))
	outcome, err := replay.Run(ctx)
	if err != nil {
		t.Fatalf("回放失败: %v", err)
	}
	if outcome.Fields["replayed"].(int) < 1 {
		t.Fatalf("应至少回放一条，得到 %v", outcome.Fields)
	}

	// 落库内容必须是条目里的 routingTrace 原文（修订号一并带上）。
	stored := opsReadRoutingTrace(t, pools, requestID)
	var parsed struct {
		UpdatedAt float64 `json:"updatedAt"`
	}
	if err := json.Unmarshal([]byte(stored), &parsed); err != nil {
		t.Fatalf("落库轨迹不是合法 JSON: %q (%v)", stored, err)
	}
	if parsed.UpdatedAt != revision {
		t.Fatalf("落库修订号应为 %v，得到 %v", revision, parsed.UpdatedAt)
	}

	// 条目必须已被认领（删除）；未删除会让下一轮重复回放。
	remaining, err := client.HExists(ctx, OutboxHashKey, strconv.FormatInt(requestID, 10)).Result()
	if err != nil {
		t.Fatalf("查询 outbox 失败: %v", err)
	}
	if remaining {
		t.Fatal("回放成功后条目应从 Redis 删除")
	}
	if indexed, err := client.ZScore(ctx, OutboxIndexKey, strconv.FormatInt(requestID, 10)).Result(); err == nil {
		t.Fatalf("索引也应清掉，实际分数 %v", indexed)
	}
	_ = payload
}

// 单调栅栏：库里已有更高修订时，旧条目不得回退覆盖（重放顺序不影响最终值）。
func TestIntegrationOutboxReplayKeepsNewerRevision(t *testing.T) {
	pools := opsTestPools(t)
	client := opsTestRedis(t)
	ctx := context.Background()

	key := opsFixtureKey(t)
	requestID := opsCreateRequestRow(t, pools, key)

	newer := float64(time.Now().UnixMilli())
	older := newer - 60_000

	// 先写一份较新的轨迹（模拟请求路径已经写过），再让一个更旧的条目回放。
	if _, err := pools.PersistRoutingTraceMonotonic(
		ctx, requestID, []byte(fmt.Sprintf(`{"version":1,"updatedAt":%d,"steps":[]}`, int64(newer))), newer,
	); err != nil {
		t.Fatalf("写入新轨迹失败: %v", err)
	}
	opsStageOutboxEntry(t, client, requestID, older)

	replay := NewOutboxReplay(opsNewTestDeps(t, pools, client))
	if _, err := replay.Run(ctx); err != nil {
		t.Fatalf("回放失败: %v", err)
	}

	stored := opsReadRoutingTrace(t, pools, requestID)
	var parsed struct {
		UpdatedAt float64 `json:"updatedAt"`
	}
	if err := json.Unmarshal([]byte(stored), &parsed); err != nil {
		t.Fatalf("落库轨迹不是合法 JSON: %q (%v)", stored, err)
	}
	if parsed.UpdatedAt != newer {
		t.Fatalf("旧修订不得覆盖新修订：期望 %v，得到 %v", newer, parsed.UpdatedAt)
	}
	// 条目仍要被认领：它的语义已经达成（库里已是更新的版本），留在 Redis 只会堆积。
	remaining, err := client.HExists(ctx, OutboxHashKey, strconv.FormatInt(requestID, 10)).Result()
	if err != nil {
		t.Fatalf("查询 outbox 失败: %v", err)
	}
	if remaining {
		t.Fatal("已无意义的旧条目应被认领删除")
	}
}

// 目标行不存在时条目丢弃（请求行尚未开或已被删），不留下永远无法回放的东西。
func TestIntegrationOutboxReplayDiscardsMissingTarget(t *testing.T) {
	pools := opsTestPools(t)
	client := opsTestRedis(t)

	// 用一个不可能存在的 id：int4 上限。
	missingID := int64(2147483647)
	opsStageOutboxEntry(t, client, missingID, float64(time.Now().UnixMilli()))

	replay := NewOutboxReplay(opsNewTestDeps(t, pools, client))
	outcome, err := replay.Run(context.Background())
	if err != nil {
		t.Fatalf("回放失败: %v", err)
	}
	if outcome.Fields["discarded"].(int) < 1 {
		t.Fatalf("应丢弃对不存在的目标行的条目，得到 %v", outcome.Fields)
	}
	remaining, err := client.HExists(context.Background(), OutboxHashKey,
		strconv.FormatInt(missingID, 10)).Result()
	if err != nil {
		t.Fatalf("查询 outbox 失败: %v", err)
	}
	if remaining {
		t.Fatal("目标不存在的条目应被认领删除")
	}
}

// 非法载荷：既不能落库，也不能留在 Redis 里永远重试。
func TestIntegrationOutboxReplayDiscardsInvalidPayload(t *testing.T) {
	pools := opsTestPools(t)
	client := opsTestRedis(t)
	ctx := context.Background()

	field := strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := client.HSet(ctx, OutboxHashKey, field, `{"version":9,"requestId":1}`).Err(); err != nil {
		t.Fatalf("写入 outbox 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = client.HDel(context.Background(), OutboxHashKey, field).Err()
		_ = client.ZRem(context.Background(), OutboxIndexKey, field).Err()
	})

	replay := NewOutboxReplay(opsNewTestDeps(t, pools, client))
	if _, err := replay.Run(ctx); err != nil {
		t.Fatalf("回放失败: %v", err)
	}
	remaining, err := client.HExists(ctx, OutboxHashKey, field).Result()
	if err != nil {
		t.Fatalf("查询 outbox 失败: %v", err)
	}
	if remaining {
		t.Fatal("非法载荷应被丢弃而不是无限重试")
	}
}

// 领导锁：同一把键只允许一个持有者，续约与释放都用持有者标识做 CAS。
func TestIntegrationLeaderLockExclusion(t *testing.T) {
	client := opsTestRedis(t)
	ctx := context.Background()
	key := "locks:go-jobs-it-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	deps := OpsDeps{Redis: client, Logger: opsTestLogger(t)}
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	first, acquired, err := deps.AcquireOpsLeaderLock(ctx, key, 5*time.Second)
	if err != nil || !acquired {
		t.Fatalf("首个持有者应拿到锁: acquired=%v err=%v", acquired, err)
	}

	// 第二个持有者（模拟 Node 或另一个 Go 实例）必须取不到：
	// 这正是切换期两侧不会同时拨测同一批端点的依据。
	second, acquired, err := deps.AcquireOpsLeaderLock(ctx, key, 5*time.Second)
	if err != nil {
		t.Fatalf("竞争取锁报错: %v", err)
	}
	if acquired || second != nil {
		t.Fatal("同键的第二个持有者不应取到锁")
	}

	if renewed, err := first.Renew(ctx, 5*time.Second); err != nil || !renewed {
		t.Fatalf("持有者续约应成功: renewed=%v err=%v", renewed, err)
	}

	// 释放后他人可持有（锁不泄漏）。
	if err := first.Release(ctx); err != nil {
		t.Fatalf("释放锁失败: %v", err)
	}
	// 释放两次是空操作而不是报错（Node 同样容忍）。
	if err := first.Release(ctx); err != nil {
		t.Fatalf("重复释放不应报错: %v", err)
	}
	third, acquired, err := deps.AcquireOpsLeaderLock(ctx, key, 5*time.Second)
	if err != nil || !acquired {
		t.Fatalf("释放后应可再次取锁: acquired=%v err=%v", acquired, err)
	}
	if err := third.Release(ctx); err != nil {
		t.Fatalf("释放第三次锁失败: %v", err)
	}
}

// 无 Redis 时退化为进程内互斥（Node 的 memory 降级），任务仍应运行。
func TestLeaderLockWithoutRedis(t *testing.T) {
	deps := OpsDeps{Logger: opsTestLogger(t)}
	lock, acquired, err := deps.AcquireOpsLeaderLock(context.Background(), "locks:no-redis", time.Second)
	if err != nil || !acquired {
		t.Fatalf("无 Redis 时应直接持锁: acquired=%v err=%v", acquired, err)
	}
	if renewed, err := lock.Renew(context.Background(), time.Second); err != nil || !renewed {
		t.Fatalf("无 Redis 的续约应为空操作成功: renewed=%v err=%v", renewed, err)
	}
	if err := lock.Release(context.Background()); err != nil {
		t.Fatalf("无 Redis 的释放应是空操作: %v", err)
	}
}

// replay 清理：advisory lock 被他人持有时必须跳过（跨进程只跑一个）。
func TestIntegrationReplayCleanupAdvisoryLock(t *testing.T) {
	pools := opsTestPools(t)
	ctx := context.Background()

	cleanupCalls := 0
	cleanup := NewReplayCleanup(OpsDeps{Pools: pools, Logger: opsTestLogger(t)}, 100,
		func(context.Context, time.Time) (int, error) {
			cleanupCalls++
			return 0, nil
		})

	// 先由「另一个进程」占住同名 advisory lock。
	//
	// 用一条**独立直连**而不是从共享池借：会话级 advisory lock 要求持锁与放锁同一条会话，
	// 且从池里借会把分道连接占住，被测任务自己再取连接就会饿死（这正是本用例暴露的第一个问题）。
	connection, err := pgx.Connect(ctx, os.Getenv("CCH_TEST_DSN"))
	if err != nil {
		t.Fatalf("建立独立连接失败: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	var locked bool
	if err := connection.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`,
		ReplayCleanupAdvisoryLock).Scan(&locked); err != nil {
		t.Fatalf("取 advisory lock 失败: %v", err)
	}
	if !locked {
		t.Fatal("测试自身应能取到 advisory lock")
	}
	defer func() {
		_, _ = connection.Exec(context.Background(),
			`SELECT pg_advisory_unlock(hashtext($1))`, ReplayCleanupAdvisoryLock)
	}()

	outcome, err := cleanup.Run(ctx)
	if err != nil {
		t.Fatalf("清理报错: %v", err)
	}
	if outcome.Fields["skipped"] != "locked" {
		t.Fatalf("锁被占用时应跳过，得到 %v", outcome.Fields)
	}
	if cleanupCalls != 0 {
		t.Fatal("跳过时不得触碰数据")
	}
}

// replay 清理：没删满即停，且不超过单轮批数上限。
func TestIntegrationReplayCleanupBatching(t *testing.T) {
	pools := opsTestPools(t)
	ctx := context.Background()

	// 假清理函数每批都删满 -> 必须恰好在 maxBatches 处停下（不许无限循环）。
	full := NewReplayCleanup(OpsDeps{Pools: pools, Logger: opsTestLogger(t)}, 100,
		func(context.Context, time.Time) (int, error) { return 100, nil })
	outcome, err := full.Run(ctx)
	if err != nil {
		t.Fatalf("清理报错: %v", err)
	}
	if outcome.Fields["batches"].(int) != replayCleanupMaxBatches {
		t.Fatalf("删满时应恰好跑 %d 批，得到 %v", replayCleanupMaxBatches, outcome.Fields)
	}
	if outcome.Processed != replayCleanupMaxBatches*100 {
		t.Fatalf("删除总数应为 %d，得到 %d", replayCleanupMaxBatches*100, outcome.Processed)
	}

	// 首批就没删满 -> 立即停（空表时的正常态，不能空转到批数上限）。
	calls := 0
	empty := NewReplayCleanup(OpsDeps{Pools: pools, Logger: opsTestLogger(t)}, 100,
		func(context.Context, time.Time) (int, error) {
			calls++
			return 0, nil
		})
	outcome, err = empty.Run(ctx)
	if err != nil {
		t.Fatalf("清理报错: %v", err)
	}
	if calls != 1 || outcome.Fields["batches"].(int) != 1 {
		t.Fatalf("未删满应立即停，实际调用 %d 次，结果 %v", calls, outcome.Fields)
	}
}

// 清理出错时把已删数量带回去（部分成功可见），并返回错误。
func TestIntegrationReplayCleanupErrorIsPartialVisible(t *testing.T) {
	pools := opsTestPools(t)
	attempt := 0
	cleanup := NewReplayCleanup(OpsDeps{Pools: pools, Logger: opsTestLogger(t)}, 100,
		func(context.Context, time.Time) (int, error) {
			attempt++
			if attempt == 2 {
				return 0, fmt.Errorf("模拟第二批失败")
			}
			return 100, nil
		})
	outcome, err := cleanup.Run(context.Background())
	if err == nil {
		t.Fatal("清理失败必须报错（不能静默）")
	}
	if outcome.Processed != 100 {
		t.Fatalf("部分成功的删除数应可见，得到 %d", outcome.Processed)
	}
}

// opsTestLogger 把后台任务的日志收进测试输出，便于失败时看到任务侧的真实视图。
func opsTestLogger(t *testing.T) *logx.Logger {
	t.Helper()
	return logx.New(&opsTestWriter{t: t})
}

type opsTestWriter struct{ t *testing.T }

func (w *opsTestWriter) Write(payload []byte) (int, error) {
	w.t.Logf("%s", string(payload))
	return len(payload), nil
}
