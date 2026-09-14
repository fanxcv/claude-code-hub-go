package usersreset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 usersreset 的集成测试：状态键/认领键/5h 准备/入队对账/三态执行/失败可见/重置口径。
//
// 门控变量与仓库其余集成测试一致：CCH_TEST_DSN（PG）与 CCH_TEST_REDIS_URL（Redis）。缺任一个即跳过。
// 夹具纪律：每个用例自建专属用户（名字带唯一标记）并按精确 id 清理；Redis 只按自己构造的键名清理，
// **不 FLUSHDB**（压测/其它 lane 共用这个 Redis 实例）。

const testRedisEnv = "CCH_TEST_REDIS_URL"

// usersResetTestQueueLockName 串行化**本包所有集成用例**（跨进程）。
//
// 为何必须有：本包测试的运行前提是「队列只有自己在用」，而事实并非如此——
// 队列的 `pendingKey` 是**全库共享的 ZSET**（见 cleanupReset 的注释），且 worker 级用例
// （如 TestWorkerRetriesAndAccumulatesProgress）是**直接消费、不取选主锁**的。于是两个测试进程
// 一旦重叠（门禁轮次背靠背、CI 重试、开发者本地与套件并行），后者的 worker 会**偷走**前者
// runner 正在等的作业。实测（刻意让同包两进程并行）：
//   - `待命期间作业不应被推进`（作业被对方完成了）
//   - `重试必须累加进度（3+5）` 得到 11/7、`应至少调用两次执行器，实际 1`
//   - 以及 runner 永远拿不到选主锁（20,072 条 standby、0 条 acquire_failed）导致作业卡在
//     `queued`/`running` 直到上限——症状看着像「产品不完成作业」，实为环境里有第二个测试进程。
//
// 改锁名或给每个用例私有锁名都是**错的方向**（已试过：双方各成 leader、互换作业）；
// 唯一可行的是把本包测试**跨进程串行化**，这就是这把锁的作用（与 model_prices 那一套同法，
// 见）。
const usersResetTestQueueLockName = "claude-code-hub:user-statistics-reset-tests"

// testEnv 是集成测试的环境。
type testEnv struct {
	pools  *store.Pools
	redis  redis.UniversalClient
	logger *logx.Logger
}

// newTestEnv 打开 PG 与 Redis；缺门控变量即跳过。
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	redisURL := os.Getenv(testRedisEnv)
	if redisURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	ctx := context.Background()
	pools, err := store.Open(ctx, store.Options{
		DSN:      dsn,
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败（值已隐去）: %v", err)
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Skipf("Redis 不可用，跳过 Redis 集成测试: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	// 拿到「本包测试专属」的跨进程锁后才算环境就绪：拿不到就在测试内部**排队**（上限见下），
	// 而不是与另一个测试进程争同一个共享队列。
	lockUsersResetTestQueue(t, pools)
	return &testEnv{pools: pools, redis: client, logger: logx.New(nil)}
}

// lockUsersResetTestQueue 取本包测试的跨进程互斥锁，并注册在用例结束时释放。
//
// 四个实现要点都沿用 model_prices 那一套（同样不是可选）：
//   - **独占连接**（pools.OpenDedicatedConn）：会话级 advisory lock 要求持锁与放锁同一条会话；
//     从分道池借还会占住分道连接，被测用例自己再取连接就会饿死。
//   - **等待有上限**（SET LOCAL lock_timeout，与取锁同一事务）：持锁者异常未释放时，门禁应当
//     「明确失败」而不是无限挂住。上限取 300s：本包全套用例不到 1s，够长到不会因它变红。
//   - **自证锁生效**（查 pg_locks）：会话级锁写错锁名或换错连接都不报错，不查等于没锁。
//   - **先注册关连接再注册放锁**：t.Cleanup 后进先出，故放锁先于关连接执行。
func lockUsersResetTestQueue(t *testing.T, pools *store.Pools) {
	t.Helper()
	ctx := context.Background()

	conn, lockPool, err := pools.OpenDedicatedConn(ctx)
	if err != nil {
		t.Fatalf("建立互斥用独立连接失败: %v", err)
	}
	t.Cleanup(func() {
		conn.Release()
		lockPool.Close()
	})

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("开启取锁事务失败: %v", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '300s'`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("设置取锁等待上限失败: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, usersResetTestQueueLockName); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("取 usersreset 测试互斥锁失败（等待超上限，可能有另一个测试进程持锁未释放）: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交取锁事务失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, usersResetTestQueueLockName)
	})

	var held int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted`).Scan(&held); err != nil {
		t.Fatalf("校验咨询锁失败: %v", err)
	}
	if held == 0 {
		t.Fatal("咨询锁未生效：本会话在 pg_locks 里查不到 granted 的 advisory lock")
	}
}

// TestUsersResetTestLockIsActuallyTaken 钉住「newTestEnv 真的取了那把跨进程锁」。
//
// 为何要钉：这把锁是**静默失败**——若有人（或一次重构）把它从 newTestEnv 里删掉，本包用例
// 照旧全绿（单进程跑时锁本来就不起作用），只在「两个测试进程重叠」时才以「别人的作业被偷走」
// 的形式现形（症状见 usersResetTestQueueLockName 的注释）。故从源码文本里断言调用点存在。
func TestUsersResetTestLockIsActuallyTaken(t *testing.T) {
	raw, err := os.ReadFile("integration_test.go")
	if err != nil {
		t.Fatalf("读取 integration_test.go 失败（工作目录变了？）: %v", err)
	}
	source := string(raw)
	if !strings.Contains(source, "lockUsersResetTestQueue(t, pools)") {
		t.Fatal("newTestEnv 里没有取 usersreset 测试互斥锁：重叠运行会重新变成「作业被偷」+「卡到上限」")
	}
	// 反向：**常量定义**必须恰好一处。注意模式串本身也会出现在源码文本里（首版用裸字符串模式
	// 就把自己也数了进去，自报 2 处而变红），故这里用带转义换行的模式：它在源码里是字面的
	// `\n`，而真实定义前是**真换行**，两者不相等，不会自匹配。单副本没有漂移风险；这么写是为了
	// 将来有人把它复制到另一个文件时立刻变红——那时必须补跨副本一致性钉子（照 model_prices 写法）。
	if n := strings.Count(source, "\nconst usersResetTestQueueLockName "); n != 1 {
		t.Fatalf("锁名常量应恰好定义一次，实际 %d 处（若复制到别的文件，必须补跨副本一致性钉子）", n)
	}
}

// newUser 建一个专属用户并登记清理。
func (env *testEnv) newUser(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := env.pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	name := fmt.Sprintf("go-usersreset-it-%d-%d", time.Now().UnixNano(), os.Getpid())
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool, err := env.pools.Writer()
		if err != nil {
			return
		}
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM usage_ledger WHERE user_id = $1`, id)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM message_request WHERE user_id = $1`, id)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM keys WHERE user_id = $1`, id)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// createRequest 建一条 message_request 并把 created_at 改成给定时刻。
//
// 用 store 的写入方法建行（它负责全部必填列与触发器），再直接改 created_at：本用例要控制切点
// 两侧的数据，而 store 的写入面没有 created_at 入参。
func (env *testEnv) createRequest(t *testing.T, userID int64, key string, createdAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	model := "go-usersreset-it-model"
	endpoint := "/v1/messages"
	cost := "0.000000000000000"
	row, err := env.pools.CreateMessageRequest(ctx, store.CreateMessageRequestData{
		ProviderID:   1,
		UserID:       userID,
		Key:          key,
		Model:        &model,
		Endpoint:     &endpoint,
		CostUSD:      &cost,
		RoutingTrace: []byte(`{"version":1}`),
	})
	if err != nil {
		t.Fatalf("建 message_request 失败: %v", err)
	}
	pool, err := env.pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE message_request SET created_at = $2 WHERE id = $1`, row.ID, createdAt); err != nil {
		t.Fatalf("改 created_at 失败: %v", err)
	}
	return row.ID
}

// countRequests 统计该用户在切点及之前的 message_request 行数。
func (env *testEnv) countRequests(t *testing.T, userID int64, cut time.Time) int {
	t.Helper()
	pool, err := env.pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM message_request WHERE user_id = $1 AND (created_at IS NULL OR created_at <= $2)`,
		userID, cut).Scan(&count); err != nil {
		t.Fatalf("统计 message_request 失败: %v", err)
	}
	return count
}

// statusOf 读状态键。
func (env *testEnv) statusOf(t *testing.T, store *StatusStore, resetID string) *Record {
	t.Helper()
	record, err := store.Get(context.Background(), resetID)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	return record
}

// activeOf 读认领键的值。
func (env *testEnv) activeOf(t *testing.T, userID int64) string {
	t.Helper()
	value, err := env.redis.Get(context.Background(), activeKey(userID)).Result()
	if errors.Is(err, redis.Nil) {
		return ""
	}
	if err != nil {
		t.Fatalf("读认领键失败: %v", err)
	}
	return value
}

// pendingLen 统计待办 ZSET 的成员数。
//
// 仅用于诊断输出：队列是**全库共享**的（同一 Redis 实例可能有其它测试/进程的作业），
// 故断语一律按自己的 resetId 限定（jobQueuedOf / memberCountOf），不断全局条数。
func (env *testEnv) pendingLen(t *testing.T) int {
	t.Helper()
	count, err := env.redis.ZCard(context.Background(), pendingKey).Result()
	if err != nil {
		t.Fatalf("读队列长度失败: %v", err)
	}
	return int(count)
}

// memberCountOf 统计队列里属于该作业的成员数（0 或 1）。
func (env *testEnv) memberCountOf(t *testing.T, resetID string) int {
	t.Helper()
	members, err := env.redis.ZRange(context.Background(), pendingKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("读队列失败: %v", err)
	}
	count := 0
	for _, member := range members {
		if envelope, ok := decodeEnvelope(member); ok && envelope.ResetID == resetID {
			count++
		}
	}
	return count
}

// statusKeyCountForUser 统计属于该用户的状态键数（按内容判定，不受其它进程的键干扰）。
func (env *testEnv) statusKeyCountForUser(t *testing.T, userID int64) int {
	t.Helper()
	ctx := context.Background()
	count := 0
	iterator := env.redis.Scan(ctx, 0, statusPrefix+"*", 64).Iterator()
	for iterator.Next(ctx) {
		raw, err := env.redis.Get(ctx, iterator.Val()).Bytes()
		if err != nil {
			continue
		}
		var record Record
		if err := decodeRecord(raw, &record); err != nil {
			continue
		}
		if record.UserID == userID {
			count++
		}
	}
	if err := iterator.Err(); err != nil {
		t.Fatalf("扫描状态键失败: %v", err)
	}
	return count
}

// dropQueuedMember 把某个作业的成员从队列里删掉（模拟「作业被落下」）。
func (env *testEnv) dropQueuedMember(t *testing.T, resetID string) {
	t.Helper()
	members, err := env.redis.ZRange(context.Background(), pendingKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("读队列失败: %v", err)
	}
	for _, member := range members {
		if envelope, ok := decodeEnvelope(member); ok && envelope.ResetID == resetID {
			if err := env.redis.ZRem(context.Background(), pendingKey, member).Err(); err != nil {
				t.Fatalf("删队列成员失败: %v", err)
			}
		}
	}
}

// newQueue 构造队列门面并登记清理（只清本用例构造的键）。
func (env *testEnv) newQueue(t *testing.T) (*Queue, *StatusStore) {
	t.Helper()
	status := NewStatusStore(env.redis)
	return NewQueue(QueueOptions{Status: status, Pools: env.pools, Logger: env.logger}), status
}

// cleanupReset 清掉一次作业相关的三个键与队列成员。
//
// 队列成员也要清：`pendingKey` 是全库共享的 ZSET，留下成员会让后续用例的「取最到期作业」
// 先取到一个没有状态记录的幽灵作业（worker 会把它当作状态缺失而丢掉，但那会浪费一轮）。
func (env *testEnv) cleanupReset(t *testing.T, userID int64, resetID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_ = env.redis.Del(ctx, statusKey(resetID), activeKey(userID), fixed5hKey(resetID)).Err()
		members, err := env.redis.ZRange(ctx, pendingKey, 0, -1).Result()
		if err != nil {
			return
		}
		for _, member := range members {
			if envelope, ok := decodeEnvelope(member); ok && envelope.ResetID == resetID {
				_ = env.redis.ZRem(ctx, pendingKey, member).Err()
			}
		}
	})
}

// TestStatusStoreRoundTripClaimAndPrepare 钉住状态键、认领与 5h 准备三件事的 Redis 语义。
func TestStatusStoreRoundTripClaimAndPrepare(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	_, status := env.newQueue(t)
	resetID, err := newResetID()
	if err != nil {
		t.Fatalf("生成作业 id 失败: %v", err)
	}
	env.cleanupReset(t, userID, resetID)

	record := createQueuedRecord(jobData{ResetID: resetID, UserID: userID, RequestedAt: isoMillis(time.Now())})
	if err := status.Set(ctx, record); err != nil {
		t.Fatalf("写状态失败: %v", err)
	}
	got := env.statusOf(t, status, resetID)
	if got == nil || got.Status != StatusQueued || got.UserID != userID {
		t.Fatalf("读回的状态不符：%+v", got)
	}
	// TTL 必须存在（7 天）：没有 TTL 的状态键会把一次重置的临时记录永久留在 Redis 里。
	ttl, err := env.redis.TTL(ctx, statusKey(resetID)).Result()
	if err != nil {
		t.Fatalf("读 TTL 失败: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("状态键必须带 TTL，实际 %s", ttl)
	}
	if unknown := env.statusOf(t, status, "00000000-0000-4000-8000-000000000000"); unknown != nil {
		t.Fatalf("不存在的作业应返回 nil，实际 %+v", unknown)
	}

	// 认领：首个成功，第二个拿到已有持有者；释放必须只删「值还是自己」的键。
	acquired, holder, err := status.ClaimActive(ctx, userID, resetID)
	if err != nil || !acquired || holder != resetID {
		t.Fatalf("首次认领失败：acquired=%v holder=%s err=%v", acquired, holder, err)
	}
	acquired, holder, err = status.ClaimActive(ctx, userID, "00000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatalf("二次认领报错: %v", err)
	}
	if acquired || holder != resetID {
		t.Fatalf("二次认领应返回已有持有者：acquired=%v holder=%s", acquired, holder)
	}
	if err := status.ReleaseActive(ctx, userID, "00000000-0000-4000-8000-000000000002"); err != nil {
		t.Fatalf("释放他人认领报错: %v", err)
	}
	if env.activeOf(t, userID) != resetID {
		t.Fatal("释放他人认领不得删掉持有者的键")
	}
	if err := status.ReleaseActive(ctx, userID, resetID); err != nil {
		t.Fatalf("释放认领报错: %v", err)
	}
	if env.activeOf(t, userID) != "" {
		t.Fatal("释放自己的认领后键应消失")
	}

	// 5h 固定窗口准备：幂等（第二次回同一个切点），并删掉窗口累计键。
	windowKey := fmt.Sprintf("user:%d:cost_5h_fixed", userID)
	if err := env.redis.Set(ctx, windowKey, "12.5", 0).Err(); err != nil {
		t.Fatalf("造窗口键失败: %v", err)
	}
	t.Cleanup(func() { _ = env.redis.Del(context.Background(), windowKey).Err() })
	first, err := status.PrepareFixed5h(ctx, resetID, userID, nil)
	if err != nil {
		t.Fatalf("准备 5h 窗口失败: %v", err)
	}
	if first.IsZero() {
		t.Fatal("准备必须回切点")
	}
	exists, err := env.redis.Exists(ctx, windowKey).Result()
	if err != nil {
		t.Fatalf("查窗口键失败: %v", err)
	}
	if exists != 0 {
		t.Fatal("准备阶段必须删掉 5h 固定窗口累计键")
	}
	if err := env.redis.Set(ctx, windowKey, "99", 0).Err(); err != nil {
		t.Fatalf("重建窗口键失败: %v", err)
	}
	second, err := status.PrepareFixed5h(ctx, resetID, userID, nil)
	if err != nil {
		t.Fatalf("二次准备失败: %v", err)
	}
	if !second.Equal(first) {
		t.Fatalf("二次准备必须回同一个切点：%s vs %s", first, second)
	}
	if exists, err := env.redis.Exists(ctx, windowKey).Result(); err != nil || exists == 0 {
		t.Fatalf("标记已存在时不得再删窗口键（幂等）: exists=%d err=%v", exists, err)
	}
}

// TestQueueEnqueueIsIdempotentPerUser 钉住「同一用户同一时刻只有一个在途作业」。
func TestQueueEnqueueIsIdempotentPerUser(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	queue, status := env.newQueue(t)

	first, err := queue.Enqueue(ctx, userID)
	if err != nil {
		t.Fatalf("首次入队失败: %v", err)
	}
	env.cleanupReset(t, userID, first.ResetID)
	second, err := queue.Enqueue(ctx, userID)
	if err != nil {
		t.Fatalf("二次入队失败: %v", err)
	}
	if second.ResetID != first.ResetID {
		t.Fatalf("同一用户的二次入队必须返回同一个作业：%s vs %s", first.ResetID, second.ResetID)
	}
	if second.Status != StatusQueued {
		t.Fatalf("入队后的状态应为 queued，实际 %s", second.Status)
	}
	if got := env.memberCountOf(t, first.ResetID); got != 1 {
		t.Fatalf("队列里应恰好一个属于该作业的成员，实际 %d（队列全局 %d）", got, env.pendingLen(t))
	}
	if env.activeOf(t, userID) != first.ResetID {
		t.Fatal("认领键应指向该作业")
	}
	// 二次入队写下的那条孤儿记录必须被删掉（按用户计数，不受其它进程的键干扰）。
	if got := env.statusKeyCountForUser(t, userID); got != 1 {
		t.Fatalf("该用户应只剩一条状态键（孤儿记录要被删掉），实际 %d 条", got)
	}

	// 查询按用户隔离：换个用户查同一作业应查不到。
	otherUser := env.newUser(t)
	found, err := queue.Find(ctx, otherUser, first.ResetID)
	if err != nil {
		t.Fatalf("跨用户查询报错: %v", err)
	}
	if found != nil {
		t.Fatalf("跨用户查询不得返回作业：%+v", found)
	}
	if record := env.statusOf(t, status, first.ResetID); record.UserID != userID {
		t.Fatalf("状态键里的用户不符：%d", record.UserID)
	}
}

// TestQueueRecoversDroppedJob 钉住对账：作业被落下（队列里没有）时，重发同一条请求把它重新入队。
func TestQueueRecoversDroppedJob(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	queue, _ := env.newQueue(t)

	first, err := queue.Enqueue(ctx, userID)
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	env.cleanupReset(t, userID, first.ResetID)
	env.dropQueuedMember(t, first.ResetID)
	if got := env.memberCountOf(t, first.ResetID); got != 0 {
		t.Fatalf("夹具自检：成员应被删掉，实际 %d", got)
	}

	recovered, err := queue.Enqueue(ctx, userID)
	if err != nil {
		t.Fatalf("恢复入队失败: %v", err)
	}
	if recovered.ResetID != first.ResetID {
		t.Fatalf("恢复后应仍是同一个作业：%s vs %s", recovered.ResetID, first.ResetID)
	}
	if got := env.memberCountOf(t, first.ResetID); got != 1 {
		t.Fatalf("恢复后应重新入队，实际 %d 个成员", got)
	}
	// 恢复路径必须带上 5h 准备版本号（切点已定，不能重算）。
	if recovered.RequestedAt != first.RequestedAt {
		t.Fatalf("恢复不得改切点：%s vs %s", recovered.RequestedAt, first.RequestedAt)
	}
}

// fakeExecutor 是可控的执行器替身：用于确定性地观察 running 中间态与失败/重试路径。
type fakeExecutor struct {
	// entered 在进入执行时关闭（测试据此断言「已进入执行」）。
	entered chan struct{}
	// release 非 nil 时，执行会阻塞到它被关闭（用于把状态钉在 running）。
	release chan struct{}
	// progress 是执行过程中回报的进度。
	progress Progress
	// failures 是前若干次调用要返回的错误；用尽后返回成功。
	failures []error
	// calls 记录被调用次数。
	calls int
	mu    sync.Mutex
}

func (f *fakeExecutor) Execute(
	ctx context.Context,
	_ int64,
	_ string,
	onProgress func(Progress) error,
) (Progress, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	if f.entered != nil {
		select {
		case <-f.entered:
		default:
			close(f.entered)
		}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return Progress{}, ctx.Err()
		}
	}
	if len(f.failures) >= call {
		return errorProgress(f.failures[call-1]), f.failures[call-1]
	}
	if onProgress != nil && f.progress != (Progress{}) {
		if err := onProgress(f.progress); err != nil {
			return Progress{}, err
		}
	}
	return f.progress, nil
}

func (f *fakeExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newWorker 构造 worker（重试退避压到毫秒级：测试不得真等 30 秒）。
func (env *testEnv) newWorker(t *testing.T, queue *Queue, executor JobExecutor, attempts int) *Worker {
	t.Helper()
	return NewWorker(WorkerOptions{
		Queue:       queue,
		Executor:    executor,
		Logger:      env.logger,
		MaxAttempts: attempts,
		BackoffBase: time.Millisecond,
		Lease:       time.Minute,
		Tick:        time.Millisecond,
	})
}

// runUntilTerminal 反复跑 RunOnce 直到该作业进入终态。
//
// 为何要循环而不是单次 RunOnce：队列是全库共享的（同一 Redis 实例上可能有其它测试/进程留下的
// 成员），而 worker 每次只取最到期的一个。循环让本用例的断言与其它成员的多少无关。
func (env *testEnv) runUntilTerminal(
	t *testing.T,
	worker *Worker,
	status *StatusStore,
	resetID string,
	timeout time.Duration,
) *Record {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := worker.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce 报错: %v", err)
		}
		record := env.statusOf(t, status, resetID)
		if record != nil && (record.Status == StatusCompleted || record.Status == StatusFailed) {
			return record
		}
		if time.Now().After(deadline) {
			t.Fatalf("作业未在 %s 内进入终态：%+v（队列全局 %d 个成员）", timeout, record, env.pendingLen(t))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestWorkerThreeStates 是验收要求的三态证据：queued → running → completed。
func TestWorkerThreeStates(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	queue, status := env.newQueue(t)

	record, err := queue.Enqueue(ctx, userID)
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	env.cleanupReset(t, userID, record.ResetID)

	// 一态：入队后是 queued，且没有任何进度（startedAt 为空）。
	queued := env.statusOf(t, status, record.ResetID)
	if queued == nil || queued.Status != StatusQueued || queued.StartedAt != nil {
		t.Fatalf("一态（queued）不符：%+v", queued)
	}
	// 认领在排队时就已建立（这是「同一用户不得并发重置」的凭据）。
	if env.activeOf(t, userID) != record.ResetID {
		t.Fatal("入队后认领键应指向该作业")
	}

	// 二态：执行中。执行器阻塞在 release 上，故这一刻状态必然是 running。
	executor := &fakeExecutor{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		progress: Progress{DeletedMessageRequests: 4, DeletedUsageLedger: 6},
	}
	worker := env.newWorker(t, queue, executor, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := worker.RunOnce(ctx); err != nil {
				t.Errorf("RunOnce 报错: %v", err)
				return
			}
			if executor.callCount() > 0 {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	select {
	case <-executor.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("执行器未被调用（队列全局 %d 个成员）", env.pendingLen(t))
	}
	running := env.statusOf(t, status, record.ResetID)
	if running == nil || running.Status != StatusRunning {
		t.Fatalf("二态（running）不符：%+v", running)
	}
	if running.StartedAt == nil {
		t.Fatal("running 状态必须带 startedAt")
	}
	if running.UserID != userID {
		t.Fatalf("状态里的用户不符：%d", running.UserID)
	}

	close(executor.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce 未在预期时间内返回")
	}

	// 三态：completed，条数落账、认领释放、队列成员卸掉。
	completed := env.statusOf(t, status, record.ResetID)
	if completed == nil || completed.Status != StatusCompleted {
		t.Fatalf("三态（completed）不符：%+v", completed)
	}
	if completed.DeletedMessageRequests != 4 || completed.DeletedUsageLedger != 6 {
		t.Fatalf("进度未落账：%+v", completed)
	}
	if completed.CompletedAt == nil || completed.ErrorCode != nil {
		t.Fatalf("completed 必须带 completedAt 且无错误码：%+v", completed)
	}
	if env.activeOf(t, userID) != "" {
		t.Fatal("完成后必须释放认领")
	}
	if got := env.memberCountOf(t, completed.ResetID); got != 0 {
		t.Fatalf("完成后应卸掉队列成员，实际 %d", got)
	}
	// 公开投影不含对内字段。
	public := completed.Public()
	if public.ResetID != record.ResetID || public.Status != StatusCompleted {
		t.Fatalf("公开投影不符：%+v", public)
	}
}

// TestWorkerFailureIsVisible 钉住「执行失败必须落到状态里可见」，且重试次数用尽才写 failed。
func TestWorkerFailureIsVisible(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	queue, status := env.newQueue(t)

	record, err := queue.Enqueue(ctx, userID)
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	env.cleanupReset(t, userID, record.ResetID)

	// MaxAttempts=1：第一次失败即终态。
	executor := &fakeExecutor{failures: []error{
		&Error{Code: ErrCodeRowsLocked, Progress: Progress{DeletedMessageRequests: 2}},
	}}
	worker := env.newWorker(t, queue, executor, 1)
	failed := env.runUntilTerminal(t, worker, status, record.ResetID, 5*time.Second)
	if failed.Status != StatusFailed {
		t.Fatalf("终态失败未写入：%+v", failed)
	}
	if failed.ErrorCode == nil || *failed.ErrorCode != ErrCodeRowsLocked {
		t.Fatalf("错误码不符：%+v", failed.ErrorCode)
	}
	if failed.DeletedMessageRequests != 2 {
		t.Fatalf("失败时的进度必须累计：%+v", failed)
	}
	if failed.CompletedAt == nil {
		t.Fatal("失败终态必须带 completedAt")
	}
	if env.activeOf(t, userID) != "" {
		t.Fatal("失败终态必须释放认领（否则该用户再也排不进新作业）")
	}
	if got := env.memberCountOf(t, record.ResetID); got != 0 {
		t.Fatalf("失败终态应卸掉队列成员，实际 %d", got)
	}
}

// TestWorkerRetriesAndAccumulatesProgress 钉住重试语义：非终态失败回到 queued 并累加进度。
func TestWorkerRetriesAndAccumulatesProgress(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	queue, status := env.newQueue(t)

	record, err := queue.Enqueue(ctx, userID)
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	env.cleanupReset(t, userID, record.ResetID)

	executor := &fakeExecutor{
		failures: []error{&Error{Code: ErrCodeRowsLocked, Progress: Progress{DeletedMessageRequests: 3}}},
		progress: Progress{DeletedMessageRequests: 5, DeletedUsageLedger: 7},
	}
	worker := env.newWorker(t, queue, executor, 3)

	// 第一次尝试：非终态失败。循环跑（队列共享，本作业不一定第一个被取到），直到看到第一次调用
	// 的痕迹（状态回到 queued 且带上了本次进度）。
	deadline := time.Now().Add(5 * time.Second)
	var queued *Record
	for {
		if _, err := worker.RunOnce(ctx); err != nil {
			t.Fatalf("首次 RunOnce 报错: %v", err)
		}
		record := env.statusOf(t, status, record.ResetID)
		if executor.callCount() > 0 && record != nil && record.DeletedMessageRequests == 3 {
			queued = record
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("首次尝试未产生可见痕迹：%+v（调用 %d 次）", record, executor.callCount())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if queued.Status != StatusQueued {
		t.Fatalf("非终态失败应回到 queued：%+v", queued)
	}
	if queued.ErrorCode != nil {
		t.Fatalf("回到 queued 时不得留错误码：%+v", queued.ErrorCode)
	}

	// 退避（1ms）后第二次取到作业；进度必须累加（base 3 + 本次 5）。
	completed := env.runUntilTerminal(t, worker, status, record.ResetID, 5*time.Second)
	if completed.Status != StatusCompleted {
		t.Fatalf("重试后应完成：%+v", completed)
	}
	if completed.DeletedMessageRequests != 3+5 {
		t.Fatalf("重试必须累加进度（3+5）：%+v", completed)
	}
	if executor.callCount() < 2 {
		t.Fatalf("应至少调用两次执行器，实际 %d", executor.callCount())
	}
	if got := env.memberCountOf(t, record.ResetID); got != 0 {
		t.Fatalf("完成后应卸掉队列成员，实际 %d", got)
	}
}

// TestWorkerIgnoresTerminalJobs 钉住「终态作业不会被执行第二遍」（租约到期后的重复取到）。
func TestWorkerIgnoresTerminalJobs(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	queue, status := env.newQueue(t)

	record, err := queue.Enqueue(ctx, userID)
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	env.cleanupReset(t, userID, record.ResetID)
	stored := env.statusOf(t, status, record.ResetID)
	stored.Status = StatusCompleted
	if err := status.Set(ctx, *stored); err != nil {
		t.Fatalf("写状态失败: %v", err)
	}

	executor := &fakeExecutor{}
	worker := env.newWorker(t, queue, executor, 3)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := worker.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce 报错: %v", err)
		}
		if env.memberCountOf(t, record.ResetID) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("终态作业的成员未被卸掉（队列全局 %d）", env.pendingLen(t))
		}
		time.Sleep(2 * time.Millisecond)
	}
	if executor.callCount() != 0 {
		t.Fatalf("终态作业不得被执行，实际调用 %d 次", executor.callCount())
	}
}

// TestResetExecutorDrainsCutoffAndClearsCaches 钉住重置口径：只删切点及之前的行、清标记、清缓存，
// 且**保留** 5h 固定窗口的累计键（那对键在准备阶段已删并记下切点）。
func TestResetExecutorDrainsCutoffAndClearsCaches(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	key := fmt.Sprintf("go-usersreset-it-key-%d", time.Now().UnixNano())
	keyID, err := env.pools.CreateAdminUserDefaultKey(ctx, userID, key, "default", nil)
	if err != nil {
		t.Fatalf("建密钥失败: %v", err)
	}
	oldRequest := env.createRequest(t, userID, key, time.Now().Add(-2*time.Hour))
	env.createRequest(t, userID, key, time.Now().Add(-1*time.Hour))
	newRequest := env.createRequest(t, userID, key, time.Now())

	cut := time.Now().Add(-30 * time.Minute)
	if env.countRequests(t, userID, cut) != 2 {
		t.Fatalf("夹具应有 2 条切点之前的行，实际 %d", env.countRequests(t, userID, cut))
	}

	// 成本重置标记：一个在切点之前（应被清空）、一个在之后（应保留）。
	pool, err := env.pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE users SET cost_reset_at = $2, limit_5h_cost_reset_at = $3 WHERE id = $1`,
		userID, time.Now().Add(-3*time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("设置成本重置标记失败: %v", err)
	}

	// Redis 夹具：成本累计键、总额缓存、租约、认证缓存，以及一个必须被保留的 5h 固定窗口键。
	costKeys := map[string]string{
		fmt.Sprintf("user:%d:cost_5h_rolling", userID):            "1.5",
		fmt.Sprintf("user:%d:cost_5h_fixed", userID):              "2.5",
		fmt.Sprintf("key:%d:cost_daily_rolling", keyID):           "3.5",
		fmt.Sprintf("total_cost:user:%d", userID):                 "4.5",
		fmt.Sprintf("total_cost:key:%s", key):                     "5.5",
		fmt.Sprintf("lease:user:%d:5h:rolling", userID):           "6.5",
		fmt.Sprintf("lease:key:%d:5h:rolling", keyID):             "7.5",
		fmt.Sprintf("api_key_auth:v1:user:%d", userID):            "cache",
		fmt.Sprintf("cch_loadtest_keep_go_usersreset_%d", userID): "untouched",
	}
	for name, value := range costKeys {
		if err := env.redis.Set(ctx, name, value, 0).Err(); err != nil {
			t.Fatalf("造 Redis 夹具 %s 失败: %v", name, err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		for name := range costKeys {
			_ = env.redis.Del(cleanupCtx, name).Err()
		}
	})

	executor := NewResetExecutor(env.pools, NewCostCleaner(env.redis, env.logger), env.logger)
	progress, err := executor.Execute(ctx, userID, isoMillis(cut), nil)
	if err != nil {
		t.Fatalf("执行重置失败: %v", err)
	}
	if progress.DeletedMessageRequests != 2 {
		t.Fatalf("应删掉 2 条 message_request，实际 %d", progress.DeletedMessageRequests)
	}
	if env.countRequests(t, userID, cut) != 0 {
		t.Fatal("切点及之前的行必须删干净")
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_request WHERE id = $1`, newRequest).Scan(&remaining); err != nil {
		t.Fatalf("查新行失败: %v", err)
	}
	if remaining != 1 {
		t.Fatal("切点之后的行不得被删")
	}
	if oldRequest == newRequest {
		t.Fatal("夹具自检：两条记录不应是同一行")
	}

	// 标记：切点之前的被清空，之后的保留。
	var costResetAt, limit5hResetAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT cost_reset_at, limit_5h_cost_reset_at FROM users WHERE id = $1`, userID).
		Scan(&costResetAt, &limit5hResetAt); err != nil {
		t.Fatalf("读成本重置标记失败: %v", err)
	}
	if costResetAt != nil {
		t.Fatalf("切点之前的 cost_reset_at 应被清空，实际 %s", costResetAt)
	}
	if limit5hResetAt == nil {
		t.Fatal("切点之后的 limit_5h_cost_reset_at 必须保留")
	}

	// 缓存：该清的都清了，5h 固定窗口键与无关键保留。
	gone := []string{
		fmt.Sprintf("user:%d:cost_5h_rolling", userID),
		fmt.Sprintf("key:%d:cost_daily_rolling", keyID),
		fmt.Sprintf("total_cost:user:%d", userID),
		fmt.Sprintf("total_cost:key:%s", key),
		fmt.Sprintf("lease:user:%d:5h:rolling", userID),
		fmt.Sprintf("lease:key:%d:5h:rolling", keyID),
		fmt.Sprintf("api_key_auth:v1:user:%d", userID),
	}
	for _, name := range gone {
		exists, err := env.redis.Exists(ctx, name).Result()
		if err != nil {
			t.Fatalf("查键失败: %v", err)
		}
		if exists != 0 {
			t.Fatalf("缓存键 %s 应被清掉", redactName(name))
		}
	}
	kept := []string{
		fmt.Sprintf("user:%d:cost_5h_fixed", userID),
		fmt.Sprintf("cch_loadtest_keep_go_usersreset_%d", userID),
	}
	for _, name := range kept {
		exists, err := env.redis.Exists(ctx, name).Result()
		if err != nil {
			t.Fatalf("查键失败: %v", err)
		}
		if exists == 0 {
			t.Fatalf("键 %s 应被保留", name)
		}
	}
}

// TestResetExecutorRejectsInvalidCutoff 钉住非法切点的错误码。
func TestResetExecutorRejectsInvalidCutoff(t *testing.T) {
	env := newTestEnv(t)
	executor := NewResetExecutor(env.pools, NewCostCleaner(env.redis, env.logger), env.logger)
	_, err := executor.Execute(context.Background(), 1, "not-a-time", nil)
	if errorCode(err) != ErrCodeInvalidCutoff {
		t.Fatalf("非法切点应报 %s，实际 %v", ErrCodeInvalidCutoff, err)
	}
}

// TestCostCleanerPreservesFixed5hOnlyWhenAsked 钉住 preserveFixed5h 的开关语义。
func TestCostCleanerPreservesFixed5hOnlyWhenAsked(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	cleaner := NewCostCleaner(env.redis, env.logger)
	fixed := fmt.Sprintf("user:%d:cost_5h_fixed", userID)
	rolling := fmt.Sprintf("user:%d:cost_5h_rolling", userID)
	if err := env.redis.Set(ctx, fixed, "1", 0).Err(); err != nil {
		t.Fatalf("造键失败: %v", err)
	}
	if err := env.redis.Set(ctx, rolling, "1", 0).Err(); err != nil {
		t.Fatalf("造键失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = env.redis.Del(cleanupCtx, fixed, rolling).Err()
	})

	result, err := cleaner.ClearUserCostCache(ctx, userID, nil, nil, true)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if result.CleanupFailed {
		t.Fatal("清理不应失败")
	}
	if result.CostKeysDeleted != 1 {
		t.Fatalf("应只删掉 rolling 键，实际删了 %d 个", result.CostKeysDeleted)
	}
	exists, err := env.redis.Exists(ctx, fixed).Result()
	if err != nil {
		t.Fatalf("查键失败: %v", err)
	}
	if exists == 0 {
		t.Fatal("preserveFixed5h=true 时必须保留 5h 固定窗口键")
	}

	if _, err := cleaner.ClearUserCostCache(ctx, userID, nil, nil, false); err != nil {
		t.Fatalf("二次清理失败: %v", err)
	}
	exists, err = env.redis.Exists(ctx, fixed).Result()
	if err != nil {
		t.Fatalf("查键失败: %v", err)
	}
	if exists != 0 {
		t.Fatal("preserveFixed5h=false 时应删掉 5h 固定窗口键")
	}
}

// redactName 把键名里的密钥原文抹掉（日志/报错里不得回显）。
func redactName(name string) string {
	if strings.HasPrefix(name, "total_cost:key:") {
		return "total_cost:key:<redacted>"
	}
	return name
}
