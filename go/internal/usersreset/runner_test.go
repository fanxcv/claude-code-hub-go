package usersreset

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
)

// 本文件覆盖两类未被三态用例触到的面：
//   - 生命周期与选主（Runner）：接管、待命、Stop 后释放锁、未装配时的降级；
//   - 失败与降级语义：依赖未装配、坏载荷、陈旧认领、错误的进度合并。

// usersResetTestLeaderName 已删：私有化锁名是错的方向。
//
// 试过并推翻：“给每个用例一把私有选主锁名，重叠运行就不互扰”。实测两进程并行时它反而变成 3/3
// 红：两边各持自己的锁 → 各成 leader → **互相偷走对方队列里的作业**，于是
// `待命期间作业不应被推进`（作业被别的进程完成了）与 `重试必须累加进度`（被别的进程重试过）
// 双双失败。原因在产品注释里写着：队列的 `pendingKey` 是**全库共享的 ZSET**
// cleanupReset 的注释），选主锁则是环境里唯一能保证「只有一个 runner」的机制。
//
// requireExclusiveUsersResetWorker 把「本用例必须是环境里唯一的 usersreset runner」这一前提
// 显式化：拿不到锁就 Skip 并说明原因。
//
// 为何不能直接等：症状是**误导性的**——实测 20,072 条 `go_usersreset_worker_standby`、
// 0 条 `leader_acquire_failed`，作业卡在 `queued`（没被认领）直到上限，看上去像「产品不完成
// 作业」。门禁不该把「环境里有第二个 runner」报成「产品缺陷」。
func requireExclusiveUsersResetWorker(t *testing.T, env *testEnv, ctx context.Context) {
	t.Helper()
	lock, acquired, err := jobs.AcquireLeader(ctx, env.pools, defaultLeaderName)
	if err != nil {
		t.Fatalf("预检取选主锁失败: %v", err)
	}
	if !acquired {
		t.Skip("另一个 usersreset runner 正在运行（选主锁被持有）：本用例要求独占（队列是全库共享 ZSET），跳过而不是报假红")
	}
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("预检释放选主锁失败: %v", err)
	}
}

// holdExclusiveLeaderLock 取得产品默认选主锁并**交给调用方持有**。
//
// 为何「待命 → 接管」类用例必须用它，而不是「先预检释放、用例再抢」：后者两段之间有
// 一个 TOCTOU 窗口，别的进程（例如启动真 runner 的包）可在此期间抢走那把全局锁，
// 于是用例在后续的「夹具自检」处报出一个与代码无关的假红
// （实测：全模块 38 包并行时约 1/8 轮命中；单包复跑必绿）。
// 锁一次拿到直接持有，窗口就不存在了。
//
// 抢不到时的处理与 requireExclusiveUsersResetWorker 一致：Skip（环境不独占）而不是失败。
func holdExclusiveLeaderLock(t *testing.T, env *testEnv, ctx context.Context) *jobs.LeaderLock {
	t.Helper()
	lock, acquired, err := jobs.AcquireLeader(ctx, env.pools, defaultLeaderName)
	if err != nil {
		t.Fatalf("申请选主锁失败: %v", err)
	}
	if !acquired {
		t.Skip("另一个 usersreset runner 正在运行（选主锁被持有）：本用例要求独占（队列是全库共享 ZSET），跳过而不是报假红")
	}
	return lock
}

// takeoverWaitBudget 是「释放选主锁后作业应被接管并完成」的等待上限。
//
// 为何不是几秒：这条断言证明的是**正确性**（接管一定会发生），而不是接管有多快。设成 5s
// 等于把「全模块并行时共享 PG/Redis 的尾延迟」当成了契约——本仓实测：8 轮 `go test ./...`
// （38 包并行）中出现过 1 次「未在 5s 内进入 completed」；而同包与四个重包并行跑 12 轮却
// 12/12 绿，说明它不是数据互扰，而是**聚合负载下的时延抖动**（那一轮 pg_stat_activity 峰值
// 仅 12/100，也不是连接耗尽），靠咨询锁治不了。改成有界但宽松的上限：真挂住仍会在上限内
// 失败，且下面的终态分支会让「真的失败了」**立刻**报出来，不会拖满上限。
const takeoverWaitBudget = 60 * time.Second

// waitForStatus 轮询等待作业进入某状态（有界，上限见 takeoverWaitBudget）。
//
// 若作业已进入与 want 不同的**终态**（terminalMismatch），立即失败：那种情况下再等下去不可能
// 变绿，早报一秒就少一秒的排障成本（上限放到 60s 后，这条分支就是「真回归也不被拖慢」的保证）。
func (env *testEnv) waitForStatus(
	t *testing.T,
	status *StatusStore,
	resetID string,
	want Status,
	timeout time.Duration,
) *Record {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		record := env.statusOf(t, status, resetID)
		if record != nil && record.Status == want {
			return record
		}
		if terminalMismatch(record, want) {
			t.Fatalf("作业已进入终态 %s（不可能再进入 %s）：%+v", record.Status, want, record)
		}
		if time.Now().After(deadline) {
			t.Fatalf("作业未在 %s 内进入 %s：%+v", timeout, want, record)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// terminalMismatch 判定「记录已进入终态且与期望不同」——那种情况下继续等不可能变绿。
//
// 终态只有两个（state.go）：StatusCompleted 与 StatusFailed。本函数只关心「期望 completed、
// 实际已 failed」这种**永远不会再变**的情形；running/queued 是中间态，仍应继续等。
func terminalMismatch(record *Record, want Status) bool {
	return record != nil && record.Status == StatusFailed && want != StatusFailed
}

// TestWaitForStatusTreatsFailedAsTerminal 钉住「终态失败不被白等」这条分支的四个方向。
//
// 为何要钉：takeoverWaitBudget 放宽到 60s 后，若这条分支写错（比如条件取反），真回归会从
// 「立即报错」退化成「白等一分钟才报错」，而测试依旧会红——只是慢了 60 倍，没人会注意到。
func TestWaitForStatusTreatsFailedAsTerminal(t *testing.T) {
	if !terminalMismatch(&Record{Status: StatusFailed}, StatusCompleted) {
		t.Fatal("已 failed 而期望 completed：必须判为终态不符（否则白等到上限）")
	}
	if terminalMismatch(&Record{Status: StatusCompleted}, StatusCompleted) {
		t.Fatal("已 completed 而期望 completed：不是终态不符")
	}
	if terminalMismatch(&Record{Status: StatusRunning}, StatusCompleted) {
		t.Fatal("running 是中间态：应继续等，不得判为终态不符")
	}
	if terminalMismatch(nil, StatusCompleted) {
		t.Fatal("记录尚未产生：应继续等，不得判为终态不符")
	}
}

// TestRunnerConsumesJobsAndReleasesLock 钉住生命周期：起 → 消费 → Stop → 释放选主锁。
func TestRunnerConsumesJobsAndReleasesLock(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	// 独占前提：本用例要求环境里只有自己一个 usersreset runner（队列是全库共享 ZSET）。
	// 前置检查放在入队之前——跳过时不留残留作业。
	requireExclusiveUsersResetWorker(t, env, ctx)
	userID := env.newUser(t)
	queue, status := env.newQueue(t)

	record, err := queue.Enqueue(ctx, userID)
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	env.cleanupReset(t, userID, record.ResetID)

	executor := &fakeExecutor{progress: Progress{DeletedMessageRequests: 2, DeletedUsageLedger: 1}}
	runner := NewRunner(RunnerOptions{
		Worker:      env.newWorker(t, queue, executor, 3),
		Pools:       env.pools,
		Logger:      env.logger,
		LeaderRetry: 5 * time.Millisecond,
	})
	done := runner.Start(ctx)
	env.waitForStatus(t, status, record.ResetID, StatusCompleted, takeoverWaitBudget)

	runner.Stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 后消费循环未退出")
	}
	// Stop 必须释放选主锁：不释放就等于「实例重启后没人能接管」。
	// 断言用产品默认名（本用例没有覆盖 LeaderName）——这正是上一步 runner 应当持有的那把锁。
	lock, acquired, err := jobs.AcquireLeader(ctx, env.pools, defaultLeaderName)
	if err != nil {
		t.Fatalf("申请选主锁失败: %v", err)
	}
	if !acquired {
		t.Fatal("Stop 后选主锁应已释放")
	}
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("释放选主锁失败: %v", err)
	}
	// 重复 Stop 是安全的。
	runner.Stop()
}

// TestRunnerStandsByThenTakesOver 钉住待命：没抢到锁时不消费，锁空出来后由重试周期接管。
func TestRunnerStandsByThenTakesOver(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	queue, status := env.newQueue(t)

	record, err := queue.Enqueue(ctx, userID)
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	env.cleanupReset(t, userID, record.ResetID)

	// 独占前提 + 直接持住同一把锁（模拟「另一个实例在消费」）。
	//
	// **刻意用产品默认锁名**：本用例断言「待命期间作业不得被推进」，而队列是全库共享的 ZSET，
	// 故只有那把进程全局的默认锁能保证「环境里没有第二个 runner 能消费本作业」。
	// 把锁名改成用例私有是错的方向（实测两进程并行 3/3 红：双方各成 leader、互换作业）。
	//
	// 一次拿到就持住：不要写成「预检释放、用例再抢」，那个窗口正是本用例曾经的假红来源。
	hold := holdExclusiveLeaderLock(t, env, ctx)

	executor := &fakeExecutor{progress: Progress{DeletedMessageRequests: 1}}
	runner := NewRunner(RunnerOptions{
		Worker:      env.newWorker(t, queue, executor, 3),
		Pools:       env.pools,
		Logger:      env.logger,
		LeaderRetry: 10 * time.Millisecond,
	})
	done := runner.Start(ctx)
	time.Sleep(150 * time.Millisecond)
	if executor.callCount() != 0 {
		t.Fatalf("未持有选主锁时不得消费，实际执行 %d 次", executor.callCount())
	}
	if record := env.statusOf(t, status, record.ResetID); record.Status != StatusQueued {
		t.Fatalf("待命期间作业不应被推进：%+v", record)
	}

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("释放选主锁失败: %v", err)
	}
	env.waitForStatus(t, status, record.ResetID, StatusCompleted, takeoverWaitBudget)
	if executor.callCount() == 0 {
		t.Fatal("接管后必须消费")
	}
	runner.Stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 后消费循环未退出")
	}
}

// TestRunnerUnwiredIsSafe 钉住未装配时 Start/Stop 都不出错（装配缝的 fail-safe）。
func TestRunnerUnwiredIsSafe(t *testing.T) {
	runner := NewRunner(RunnerOptions{})
	done := runner.Start(context.Background())
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("未装配时 Start 应立即返回已结束的通道")
	}
	runner.Stop()

	var nilRunner *Runner
	nilRunner.Stop()
	if closed := closedChan(); closed == nil {
		t.Fatal("closedChan 不得返回 nil")
	}
}

// TestUnavailableDependenciesFailClosed 钉住「依赖未装配即报 REDIS_UNAVAILABLE，不静默成功」。
func TestUnavailableDependenciesFailClosed(t *testing.T) {
	ctx := context.Background()
	status := NewStatusStore(nil)
	if status.Available() {
		t.Fatal("nil 连接时 Available 应为假")
	}
	if err := status.Set(ctx, Record{}); !errors.Is(err, ErrRedisUnavailable) {
		t.Fatalf("写状态应报 REDIS_UNAVAILABLE，实际 %v", err)
	}
	if _, err := status.Get(ctx, "x"); !errors.Is(err, ErrRedisUnavailable) {
		t.Fatalf("读状态应报 REDIS_UNAVAILABLE，实际 %v", err)
	}
	if err := status.Delete(ctx, "x"); !errors.Is(err, ErrRedisUnavailable) {
		t.Fatalf("删状态应报 REDIS_UNAVAILABLE，实际 %v", err)
	}
	if _, _, err := status.ClaimActive(ctx, 1, "x"); !errors.Is(err, ErrRedisUnavailable) {
		t.Fatalf("认领应报 REDIS_UNAVAILABLE，实际 %v", err)
	}
	if err := status.ReleaseActive(ctx, 1, "x"); !errors.Is(err, ErrRedisUnavailable) {
		t.Fatalf("释放认领应报 REDIS_UNAVAILABLE，实际 %v", err)
	}
	if _, err := status.PrepareFixed5h(ctx, "x", 1, nil); !errors.Is(err, ErrRedisUnavailable) {
		t.Fatalf("准备 5h 应报 REDIS_UNAVAILABLE，实际 %v", err)
	}

	queue := NewQueue(QueueOptions{})
	if queue.Available() {
		t.Fatal("无依赖时 Available 应为假")
	}
	if _, err := queue.Enqueue(ctx, 1); !errors.Is(err, ErrRedisUnavailable) {
		t.Fatalf("入队应报 REDIS_UNAVAILABLE，实际 %v", err)
	}
	if _, err := queue.Find(ctx, 1, "x"); !errors.Is(err, ErrRedisUnavailable) {
		t.Fatalf("查询应报 REDIS_UNAVAILABLE，实际 %v", err)
	}

	cleaner := NewCostCleaner(nil, nil)
	if cleaner.Available() {
		t.Fatal("nil 连接时 Available 应为假")
	}
	if _, err := cleaner.ClearUserCostCache(ctx, 1, nil, nil, true); errorCode(err) != ErrCodeCacheCleanupFailed {
		t.Fatalf("缓存清理应报 %s，实际 %v", ErrCodeCacheCleanupFailed, err)
	}
	// 不清缓存也不得 panic（它只记 warn）。
	cleaner.InvalidateCachedUser(ctx, 1)

	executor := NewResetExecutor(nil, cleaner, nil)
	if _, err := executor.Execute(ctx, 1, "2026-01-02T03:04:05.000Z", nil); errorCode(err) != ErrCodeOperationFailed {
		t.Fatalf("无连接池应报 %s，实际 %v", ErrCodeOperationFailed, err)
	}

	worker := NewWorker(WorkerOptions{})
	if _, err := worker.RunOnce(ctx); !errors.Is(err, ErrRedisUnavailable) {
		t.Fatalf("未装配的 worker 应报 REDIS_UNAVAILABLE，实际 %v", err)
	}
}

// TestQueueReleasesStaleClaimAndReenqueues 钉住对账的两个分支：陈旧认领与终态作业的认领。
func TestQueueReleasesStaleClaimAndReenqueues(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	queue, status := env.newQueue(t)

	// 分支一：认领指向一个不存在的作业（状态键已过期/被删）。
	staleID, err := newResetID()
	if err != nil {
		t.Fatalf("生成作业 id 失败: %v", err)
	}
	env.cleanupReset(t, userID, staleID)
	if err := env.redis.Set(ctx, activeKey(userID), staleID, 0).Err(); err != nil {
		t.Fatalf("造陈旧认领失败: %v", err)
	}
	record, err := queue.Enqueue(ctx, userID)
	if err != nil {
		t.Fatalf("陈旧认领下入队失败: %v", err)
	}
	env.cleanupReset(t, userID, record.ResetID)
	if record.ResetID == staleID {
		t.Fatal("陈旧认领不得被当成有效作业")
	}
	if env.activeOf(t, userID) != record.ResetID {
		t.Fatalf("陈旧认领应被释放并换成新作业，实际 %q", env.activeOf(t, userID))
	}

	// 分支二：认领指向一个已终态的作业（进程死在写状态与放认领之间）。
	completed := env.statusOf(t, status, record.ResetID)
	completed.Status = StatusCompleted
	if err := status.Set(ctx, *completed); err != nil {
		t.Fatalf("写终态失败: %v", err)
	}
	next, err := queue.Enqueue(ctx, userID)
	if err != nil {
		t.Fatalf("终态认领下入队失败: %v", err)
	}
	env.cleanupReset(t, userID, next.ResetID)
	if next.ResetID == record.ResetID {
		t.Fatal("终态作业的认领应被释放并重排一个新作业")
	}
	if env.activeOf(t, userID) != next.ResetID {
		t.Fatalf("认领应指向新作业，实际 %q", env.activeOf(t, userID))
	}
	if record := env.statusOf(t, status, next.ResetID); record.Status != StatusQueued {
		t.Fatalf("重排的作业应为 queued：%+v", record)
	}
}

// TestStatusStoreRejectsCorruptPayload 钉住坏内容报错而不是当作不存在（当作不存在会让调用方重排）。
func TestStatusStoreRejectsCorruptPayload(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	_, status := env.newQueue(t)
	resetID, err := newResetID()
	if err != nil {
		t.Fatalf("生成作业 id 失败: %v", err)
	}
	env.cleanupReset(t, userID, resetID)
	if err := env.redis.Set(ctx, statusKey(resetID), "not-json", 0).Err(); err != nil {
		t.Fatalf("造坏状态失败: %v", err)
	}
	if _, err := status.Get(ctx, resetID); errorCode(err) != ErrCodeStatusInvalid {
		t.Fatalf("坏状态应报 %s，实际 %v", ErrCodeStatusInvalid, err)
	}
}

// TestWorkerDropsCorruptQueueMember 钉住坏载荷被丢弃（否则它会顶在最前面让队列永远排不动）。
func TestWorkerDropsCorruptQueueMember(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	queue, _ := env.newQueue(t)
	if err := env.redis.ZAdd(ctx, pendingKey, redis.Z{Score: 1, Member: "{broken"}).Err(); err != nil {
		t.Fatalf("造坏载荷失败: %v", err)
	}
	worker := env.newWorker(t, queue, &fakeExecutor{}, 1)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce 报错: %v", err)
	}
	score, err := env.redis.ZScore(ctx, pendingKey, "{broken").Result()
	if !errors.Is(err, redis.Nil) {
		t.Fatalf("坏载荷应被丢弃，实际 score=%v err=%v", score, err)
	}
}

// TestWorkerBackoffIsExponentialWithCap 钉住退避曲线（Bull 的 delay × 2^(n-1)，封顶 30 分钟）。
func TestWorkerBackoffIsExponentialWithCap(t *testing.T) {
	worker := NewWorker(WorkerOptions{BackoffBase: 30 * time.Second})
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 30 * time.Second},
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 120 * time.Second},
		{4, 240 * time.Second},
		{8, 30 * time.Minute}, // 封顶
		{20, 30 * time.Minute},
	}
	for _, item := range cases {
		if got := worker.backoff(item.attempts); got != item.want {
			t.Fatalf("第 %d 次尝试的退避应为 %s，实际 %s", item.attempts, item.want, got)
		}
	}
}

// TestExecuteErrorMerging 钉住进度合并与错误分类（reset-service.ts 外层 catch 的逐条语义）。
func TestExecuteErrorMerging(t *testing.T) {
	executor := NewResetExecutor(nil, nil, nil)

	// 尚无进度且不是本包错误：原样返回（例如 ctx 取消），不伪装成 OPERATION_FAILED。
	cancelled := context.Canceled
	if _, err := executor.fail(Progress{}, cancelled); !errors.Is(err, cancelled) {
		t.Fatalf("无进度时应原样返回错误，实际 %v", err)
	}
	// 有进度：包装成 OPERATION_FAILED 并带上进度。
	progress, err := executor.fail(Progress{DeletedMessageRequests: 5}, cancelled)
	if errorCode(err) != ErrCodeOperationFailed {
		t.Fatalf("有进度时应归为 %s，实际 %v", ErrCodeOperationFailed, err)
	}
	if progress.DeletedMessageRequests != 5 {
		t.Fatalf("进度必须带出来：%+v", progress)
	}
	// 本包错误：保留码，并按字段取最大进度。
	progress, err = executor.fail(
		Progress{DeletedMessageRequests: 5, DeletedUsageLedger: 1},
		&Error{Code: ErrCodeRowsLocked, Progress: Progress{DeletedUsageLedger: 9}},
	)
	if errorCode(err) != ErrCodeRowsLocked {
		t.Fatalf("错误码应保留，实际 %v", err)
	}
	if progress.DeletedMessageRequests != 5 || progress.DeletedUsageLedger != 9 {
		t.Fatalf("进度应逐字段取最大：%+v", progress)
	}

	if got := tableProgress("message_request", 3); got.DeletedMessageRequests != 3 || got.DeletedUsageLedger != 0 {
		t.Fatalf("message_request 的进度不符：%+v", got)
	}
	if got := tableProgress("usage_ledger", 4); got.DeletedUsageLedger != 4 || got.DeletedMessageRequests != 0 {
		t.Fatalf("usage_ledger 的进度不符：%+v", got)
	}
	merged := mergeProgress(Progress{DeletedMessageRequests: 2}, &Error{
		Code:     ErrCodeOperationFailed,
		Progress: Progress{DeletedUsageLedger: 7},
	})
	if merged.DeletedMessageRequests != 2 || merged.DeletedUsageLedger != 7 {
		t.Fatalf("合并结果不符：%+v", merged)
	}
}

// TestQueueErrorsAreClassified 钉住入队失败的错误分类（用于路由映射）。
func TestQueueErrorsAreClassified(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	userID := env.newUser(t)
	queue, _ := env.newQueue(t)
	// 连接池缺失（配置错误）时入队必须报错，而不是排一个永远跑不起来的作业。
	broken := NewQueue(QueueOptions{Status: NewStatusStore(env.redis), Pools: nil})
	if _, err := broken.Enqueue(ctx, userID); err == nil {
		t.Fatal("缺连接池时入队必须报错")
	}
	// 与入队相反：查询状态只读 Redis 的状态键，**不需要池**。Node 侧同理——
	// findUserStatisticsReset（reset-queue.ts:418-424）只调 getUserStatisticsResetStatus
	// （reset-status-store.ts:65-80，纯 redis.get），全程不碰 PG。所以缺池时这条路径
	// 必须照常工作、对未知 id 返回 not-found，而不是跟着入队一起失败（否则「配置错了一个池」
	// 会让 UI 连作业状态都查不到，把可诊断的配置错伪装成 404）。
	found, err := broken.Find(ctx, userID, "x")
	if err != nil {
		t.Fatalf("缺连接池时查询状态仍应可用（Find 不需要池），实际报错: %v", err)
	}
	if found != nil {
		t.Fatalf("未知作业应返回 nil：%+v", found)
	}
	// 正常队列的 Find 对未知 id 同样返回 nil。
	found, err = queue.Find(ctx, userID, "0b9f1c2e-3d4a-4b5c-8d6e-7f8091a2b3c4")
	if err != nil {
		t.Fatalf("查询未知作业报错: %v", err)
	}
	if found != nil {
		t.Fatalf("未知作业应返回 nil：%+v", found)
	}
}
