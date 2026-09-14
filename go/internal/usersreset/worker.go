package usersreset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件复刻 reset-queue.ts 的执行侧（:116-330）：取作业、准备、执行、写终态、释放认领。
//
// 与 Node 的差别集中在「谁保证同一时刻只有一个 worker」这一件事上：
//
//   - Node 的 Bull 用 Redis 的锁与 stalled 检查在**多实例**间做这件事，并把重试次数存在 job 上。
//   - 本包用 **PG advisory lock 选主**（jobs.AcquireLeader，与 internal/jobs 同范式）＋
//     「取出的成员立刻把 score 推到租约到期时刻」。存活的 worker 只有选主成功的那一个，
//     因此单 worker 内串行执行即可保证同一作业不被并发执行；而租约让**崩溃**的 worker
//     留下的作业在租约到期后被下一个主重新取到（等价于 Bull 的 stalled 重试）。
//
// 重试次数存在队列载荷里（不走状态键，见 jobEnvelope 的注释）；退避与 Bull 的
// `backoff: {type: "exponential", delay: 30000}` 对齐：第 n 次重试等待 30s × 2^(n-1)。

const (
	// defaultMaxAttempts 与 reset-queue.ts:158 的 attempts: 5 一致（首次 + 4 次重试）。
	defaultMaxAttempts = 5
	// defaultBackoffBase 与 reset-queue.ts:159 的 backoff.delay = 30000 一致。
	defaultBackoffBase = 30 * time.Second
	// defaultLease 是「作业被取走后多久算 stalled」。
	//
	// Bull 的默认 stalledInterval 是 30 秒（且判定 stalled 需要另一实例的连接），这里取 5 分钟：
	// 一次重置要按批删行，几百万行的用户可能跑上分钟级；租约太短会让一个**正常在跑**的作业被
	// 下一任主重复执行（重复执行是幂等的——同样的切点、同样的谓词——但会白删一遍并写乱进度）。
	defaultLease = 5 * time.Minute
	// defaultTick 是空队列时的轮询间隔。
	defaultTick = 500 * time.Millisecond
	// defaultLeaderName 是选主用的 advisory 锁名。
	//
	// 用 `hashtext(<name>)` 做键（jobs.AcquireLeader 的行为），故这个名字只与同样用 PG 选主的
	// 进程互斥；Node 侧是 Bull（Redis 锁），两端不会互相选主——这正是「队列执行不跨端」的表现，
	// 见包说明第 2 条。名字取 Go 专有值：与 Node 的锁名撞名毫无意义（它不用 PG 锁）。
	defaultLeaderName = "claude-code-hub:go-user-statistics-reset-worker"
)

// JobExecutor 是执行重置本体的一步（实现：*ResetExecutor）。
//
// 抽成接口的理由与 Deps 里那些窄接口同：worker 只该看到「执行一次作业并回报进度」这一件事，
// 拿具体类型会把「分批删行、Redis 键形制」一并变成 worker 的依赖面；同时它让三态（queued →
// running → completed）的用例可以确定性地观察到中间态，而不必靠「跑得够慢」去撞。
type JobExecutor interface {
	Execute(
		ctx context.Context,
		userID int64,
		requestedAt string,
		onProgress func(Progress) error,
	) (Progress, error)
}

// WorkerOptions 是 worker 的构造参数。
type WorkerOptions struct {
	// Queue 提供状态读写与队列操作，必填。
	Queue *Queue
	// Executor 执行重置本体，必填。
	Executor JobExecutor
	// Logger 为 nil 时静默。
	Logger *logx.Logger
	// MaxAttempts / BackoffBase / Lease / Tick 留零时取上面的默认值；测试用短值避免真等。
	MaxAttempts int
	BackoffBase time.Duration
	Lease       time.Duration
	Tick        time.Duration
}

// Worker 是作业执行者。
type Worker struct {
	queue       *Queue
	executor    JobExecutor
	logger      *logx.Logger
	maxAttempts int
	backoffBase time.Duration
	lease       time.Duration
	tick        time.Duration
}

// NewWorker 构造 worker。
func NewWorker(options WorkerOptions) *Worker {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	worker := &Worker{
		queue:       options.Queue,
		executor:    options.Executor,
		logger:      logger,
		maxAttempts: options.MaxAttempts,
		backoffBase: options.BackoffBase,
		lease:       options.Lease,
		tick:        options.Tick,
	}
	if worker.maxAttempts <= 0 {
		worker.maxAttempts = defaultMaxAttempts
	}
	if worker.backoffBase <= 0 {
		worker.backoffBase = defaultBackoffBase
	}
	if worker.lease <= 0 {
		worker.lease = defaultLease
	}
	if worker.tick <= 0 {
		worker.tick = defaultTick
	}
	return worker
}

// RunOnce 取一个到期作业并执行；返回 false 表示当前没有到期作业。
//
// 单次调用即一个完整生命周期（取→租约→执行→写终态），便于测试逐步驱动而不必启动整个循环。
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if w == nil || w.queue == nil || !w.queue.Available() {
		return false, ErrRedisUnavailable
	}
	member, envelope, ok, err := w.claimDueJob(ctx)
	if err != nil || !ok {
		return ok, err
	}
	if err := w.runJob(ctx, member, envelope); err != nil {
		// 执行失败已经写进状态键，这里的错误只用于日志与控制流：不把它当作 worker 的故障。
		w.logger.Warn("go_usersreset_job_failed", map[string]any{
			"resetId":  envelope.ResetID,
			"attempts": envelope.Attempts,
			"error":    err.Error(),
		})
	}
	return true, nil
}

// Run 持续消费队列直到 ctx 结束。
//
// 选主在调用方完成（jobs.AcquireLeader）：拿不到锁的实例不跑这个循环，
// 故这里不需要任何跨实例互斥。
func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		worked, err := w.RunOnce(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			w.logger.Warn("go_usersreset_poll_failed", map[string]any{"error": err.Error()})
			if !sleepCtx(ctx, w.tick) {
				return
			}
			continue
		}
		if worked {
			continue
		}
		if !sleepCtx(ctx, w.tick) {
			return
		}
	}
}

// claimDueJob 取出一个到期作业，并把它的 score 推到租约到期时刻。
//
// 「取出」与「推 score」分两条命令：先 ZRANGEBYSCORE 找到到期成员，再以 ZADD 覆盖它的 score。
// 这里不追求原子——只有当选主的单一 worker 在执行本方法，且崩溃时最坏结果是同一作业被下一任
// 重复执行一次（幂等，见 defaultLease 的说明）。
func (w *Worker) claimDueJob(ctx context.Context) (string, jobEnvelope, bool, error) {
	now := time.Now().UnixMilli()
	members, err := w.queue.client().ZRangeByScore(ctx, pendingKey, &redis.ZRangeBy{
		Min:    "-inf",
		Max:    fmt.Sprintf("%d", now),
		Offset: 0,
		Count:  1,
	}).Result()
	if err != nil {
		return "", jobEnvelope{}, false, fmt.Errorf("usersreset: 取待办作业失败: %w", err)
	}
	if len(members) == 0 {
		return "", jobEnvelope{}, false, nil
	}
	member := members[0]
	envelope, ok := decodeEnvelope(member)
	if !ok {
		// 坏载荷无法执行，也不可能被谁修好：删掉它并记 warn（否则它会一直顶在最前面，
		// 让队列里后面的作业永远排不上）。返回「本轮没干活」而不是「干了一件空活」：
		// 空作业没有 resetId，往下走只会走出一串无意义的状态查询。
		w.logger.Warn("go_usersreset_invalid_queue_member", map[string]any{"member": member})
		if err := w.queue.client().ZRem(ctx, pendingKey, member).Err(); err != nil {
			return "", jobEnvelope{}, false, fmt.Errorf("usersreset: 丢弃坏队列载荷失败: %w", err)
		}
		return "", jobEnvelope{}, false, nil
	}
	leased, err := json.Marshal(envelope)
	if err != nil {
		return "", jobEnvelope{}, false, fmt.Errorf("usersreset: 序列化队列载荷失败: %w", err)
	}
	if err := w.queue.client().ZAdd(ctx, pendingKey, redis.Z{
		Score:  float64(time.Now().Add(w.lease).UnixMilli()),
		Member: leased,
	}).Err(); err != nil {
		return "", jobEnvelope{}, false, fmt.Errorf("usersreset: 续租作业失败: %w", err)
	}
	// 旧成员（score 已到期的那份）若与续租后的成员不同（attempts 变了），要显式删掉：
	// ZADD 只按 member 判重，member 字符串不同就会留下两份。
	if string(leased) != member {
		if err := w.queue.client().ZRem(ctx, pendingKey, member).Err(); err != nil {
			return "", jobEnvelope{}, false, fmt.Errorf("usersreset: 清理旧队列成员失败: %w", err)
		}
	}
	return string(leased), envelope, true, nil
}

// runJob 执行一个作业：准备→running→执行→终态（reset-queue.ts:220-330）。
func (w *Worker) runJob(ctx context.Context, member string, envelope jobEnvelope) error {
	status, err := w.queue.status.Get(ctx, envelope.ResetID)
	if err != nil {
		return err
	}
	if status == nil {
		// 状态记录没了（过期或被删）：作业无处落进度，直接丢弃成员。
		w.logger.Warn("go_usersreset_job_status_missing", map[string]any{
			"resetId": envelope.ResetID,
		})
		return w.queue.client().ZRem(ctx, pendingKey, member).Err()
	}
	if !isActiveStatus(status.Status) {
		// 已经是终态（例如另一个 worker 刚跑完）：卸掉成员，不再执行。
		return w.queue.client().ZRem(ctx, pendingKey, member).Err()
	}

	current := *status
	startedAt := current.StartedAt
	if startedAt == nil {
		now := isoMillis(time.Now())
		startedAt = &now
	}
	// 基础进度＝记录里已有的累计（上一次尝试的成果）。重试必须从这里继续加，
	// 否则 UI 上的条数会随重试回退（reset-queue.ts:230-238）。
	base := Progress{
		DeletedMessageRequests: current.DeletedMessageRequests,
		DeletedUsageLedger:     current.DeletedUsageLedger,
	}
	attempt := Progress{}

	prepared, err := w.queue.ensurePrepared(ctx, dataFromRecord(current), current)
	if err != nil {
		return w.finishFailed(ctx, envelope, current, startedAt, base, attempt, err)
	}
	current = prepared
	current.Status = StatusRunning
	current.StartedAt = startedAt
	current.ErrorCode = nil
	if err := w.queue.status.Set(ctx, current); err != nil {
		return err
	}
	running := current

	deleted, execErr := w.executor.Execute(
		ctx,
		running.UserID,
		running.RequestedAt,
		func(progress Progress) error {
			attempt = progress
			updated := running
			updated.DeletedMessageRequests = base.DeletedMessageRequests + progress.DeletedMessageRequests
			updated.DeletedUsageLedger = base.DeletedUsageLedger + progress.DeletedUsageLedger
			running = updated
			return w.queue.status.Set(ctx, updated)
		},
	)
	if execErr != nil {
		return w.finishFailed(ctx, envelope, running, startedAt, base, deleted, execErr)
	}

	completed := running
	completed.DeletedMessageRequests = base.DeletedMessageRequests + deleted.DeletedMessageRequests
	completed.DeletedUsageLedger = base.DeletedUsageLedger + deleted.DeletedUsageLedger
	completed.Status = StatusCompleted
	completed.StartedAt = startedAt
	now := isoMillis(time.Now())
	completed.CompletedAt = &now
	completed.ErrorCode = nil
	if err := w.queue.status.Set(ctx, completed); err != nil {
		return err
	}
	if err := w.queue.client().ZRem(ctx, pendingKey, member).Err(); err != nil {
		// 成员没删掉会在一轮租约后被重复执行：此时作业已 completed，runJob 开头会把成员卸掉，
		// 故这里只记 warn——重复执行的那一次不会再删任何行。
		w.logger.Warn("go_usersreset_queue_member_remove_failed", map[string]any{
			"resetId": envelope.ResetID,
			"error":   err.Error(),
		})
	}
	w.releaseActive(ctx, envelope.ResetID, completed.UserID)
	w.logger.Info("go_usersreset_job_completed", map[string]any{
		"resetId":                envelope.ResetID,
		"userId":                 completed.UserID,
		"deletedMessageRequests": completed.DeletedMessageRequests,
		"deletedUsageLedger":     completed.DeletedUsageLedger,
	})
	return nil
}

// finishFailed 处理一次失败尝试（reset-queue.ts:280-330）。
//
// 两次机会的判断与 Node 一致：还有重试次数就回到 queued（保留已删条数），用尽则写 failed 并释放认领。
func (w *Worker) finishFailed(
	ctx context.Context,
	envelope jobEnvelope,
	current Record,
	startedAt *string,
	base Progress,
	attempt Progress,
	err error,
) error {
	mergedAttempt := Progress{
		DeletedMessageRequests: max(attempt.DeletedMessageRequests, errorProgress(err).DeletedMessageRequests),
		DeletedUsageLedger:     max(attempt.DeletedUsageLedger, errorProgress(err).DeletedUsageLedger),
	}
	failed := current
	failed.DeletedMessageRequests = base.DeletedMessageRequests + mergedAttempt.DeletedMessageRequests
	failed.DeletedUsageLedger = base.DeletedUsageLedger + mergedAttempt.DeletedUsageLedger
	failed.StartedAt = startedAt

	attempts := envelope.Attempts + 1
	final := attempts >= w.maxAttempts
	if final {
		failed.Status = StatusFailed
		now := isoMillis(time.Now())
		failed.CompletedAt = &now
		code := errorCode(err)
		failed.ErrorCode = &code
	} else {
		failed.Status = StatusQueued
		failed.CompletedAt = nil
		failed.ErrorCode = nil
	}
	if statusErr := w.queue.status.Set(ctx, failed); statusErr != nil {
		// 状态写不进去：如实上报（调用方记日志），但**不要**因此不重排/不放认领——
		// 那会让作业卡在队列里以 running 的状态被反复取到。
		w.logger.Error("go_usersreset_status_write_failed", map[string]any{
			"resetId": envelope.ResetID,
			"error":   statusErr.Error(),
		})
	}

	if !final {
		delay := w.backoff(attempts)
		next := jobEnvelope{ResetID: envelope.ResetID, Attempts: attempts}
		payload, marshalErr := json.Marshal(next)
		if marshalErr != nil {
			return marshalErr
		}
		// 先把新载荷按退避时刻入队，再删旧成员：反过来会有一段「作业不在队列里」的窗口，
		// 那期间若进程退出，作业就只剩一个 queued 状态记录而无人执行。
		if err := w.queue.client().ZAdd(ctx, pendingKey, redis.Z{
			Score:  float64(time.Now().Add(delay).UnixMilli()),
			Member: payload,
		}).Err(); err != nil {
			return fmt.Errorf("usersreset: 重试入队失败: %w", err)
		}
		if delErr := w.queue.client().ZRem(ctx, pendingKey, memberOf(envelope)).Err(); delErr != nil {
			w.logger.Warn("go_usersreset_retry_member_cleanup_failed", map[string]any{
				"resetId": envelope.ResetID,
				"error":   delErr.Error(),
			})
		}
		w.logger.Warn("go_usersreset_job_retry_scheduled", map[string]any{
			"resetId":  envelope.ResetID,
			"attempts": attempts,
			"delayMs":  delay.Milliseconds(),
			"error":    err.Error(),
		})
		return err
	}

	if remErr := w.queue.client().ZRem(ctx, pendingKey, memberOf(envelope)).Err(); remErr != nil {
		w.logger.Warn("go_usersreset_final_member_remove_failed", map[string]any{
			"resetId": envelope.ResetID,
			"error":   remErr.Error(),
		})
	}
	w.releaseActive(ctx, envelope.ResetID, failed.UserID)
	w.logger.Error("go_usersreset_job_failed_final", map[string]any{
		"resetId":  envelope.ResetID,
		"userId":   failed.UserID,
		"attempts": attempts,
		"error":    err.Error(),
	})
	return err
}

// backoff 返回第 attempts 次尝试后的退避时长（Bull 的指数退避：delay × 2^(attempts-1)）。
func (w *Worker) backoff(attempts int) time.Duration {
	if attempts <= 1 {
		return w.backoffBase
	}
	delay := w.backoffBase
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= 30*time.Minute {
			// ponytail: 退避封顶 30 分钟。Bull 不封顶（delay × 2^(n-1)，attempts=5 时最坏 4 分钟），
			// 封顶只是防止将来把 maxAttempts 调大后等待时间无界增长；当前默认值下永不触发。
			return 30 * time.Minute
		}
	}
	return delay
}

// releaseActive 释放认领；失败只记 warn（作业已终态，认领有 7 天 TTL 会自己过期）。
func (w *Worker) releaseActive(ctx context.Context, resetID string, userID int64) {
	if err := w.queue.status.ReleaseActive(ctx, userID, resetID); err != nil {
		w.logger.Warn("go_usersreset_release_active_failed", map[string]any{
			"resetId": resetID,
			"userId":  userID,
			"error":   err.Error(),
		})
	}
}

// memberOf 把载荷还原成它入队时的成员串（ZREM 需要精确 member）。
func memberOf(envelope jobEnvelope) string {
	payload, err := json.Marshal(envelope)
	if err != nil {
		return ""
	}
	return string(payload)
}

// sleepCtx 睡一段时间或被 ctx 打断；返回 false 表示 ctx 已结束。
func sleepCtx(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
