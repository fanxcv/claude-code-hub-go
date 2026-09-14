package usersreset

import (
	"context"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 worker 的进程内生命周期：选主（PG advisory lock）、跑消费循环、优雅停机。
//
// 为什么需要选主而不是「所有实例都消费」：Node 的 Bull 允许多实例并发消费（它自带锁与 stalled
// 检查）；本包的队列是一个 ZSET + 状态键，若多实例同时消费，两个实例会取到同一个到期成员并
// 各执行一次——重置是幂等的（同切点、同谓词），但进度会互相覆盖（两边各自 base+attempt 写同一记录）。
// 与其复刻 Bull 的分布式锁，不如让**只有一个实例消费**：这正是 internal/jobs 既有范式
// （jobs.AcquireLeader 用 hashtext 名字做 session 级 advisory 锁）。

// defaultLeaderRetry 是没抢到消费锁时的重试间隔。
//
// 必须重试而不是「没抢到就退出」：持有锁的实例若在跑后被 OOM 杀掉，它的会话结束、锁自动释放，
// 但已经放弃的实例不会自己醒来——那样这套作业就再没人执行，而 UI 会一直显示 queued。
const defaultLeaderRetry = 30 * time.Second

// RunnerOptions 是 Runner 的构造参数。
type RunnerOptions struct {
	// Worker 是消费者，必填。
	Worker *Worker
	// Pools 用于申请选主锁（jobs.AcquireLeader）。nil 时不启动并记 Error。
	Pools *store.Pools
	// Logger 为 nil 时静默。
	Logger *logx.Logger
	// LeaderName 覆盖选主锁名（测试用；留空取 defaultLeaderName）。
	LeaderName string
	// LeaderRetry 覆盖没抢到锁时的重试间隔（测试用）。
	LeaderRetry time.Duration
}

// Runner 持有消费循环与选主锁。
type Runner struct {
	worker      *Worker
	pools       *store.Pools
	logger      *logx.Logger
	leaderName  string
	leaderRetry time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewRunner 构造 Runner。
func NewRunner(options RunnerOptions) *Runner {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	runner := &Runner{
		worker:      options.Worker,
		pools:       options.Pools,
		logger:      logger,
		leaderName:  options.LeaderName,
		leaderRetry: options.LeaderRetry,
	}
	if runner.leaderName == "" {
		runner.leaderName = defaultLeaderName
	}
	if runner.leaderRetry <= 0 {
		runner.leaderRetry = defaultLeaderRetry
	}
	return runner
}

// Start 启动消费循环；不可用（缺队列/缺执行器/缺池）时记 Error 并返回 nil。
//
// 返回的 done 通道在循环退出后关闭：调用方（进程关闭路径）据此等待在途作业跑完当前一轮。
func (r *Runner) Start(ctx context.Context) <-chan struct{} {
	if r == nil || r.worker == nil || r.pools == nil {
		if r != nil {
			r.logger.Error("go_usersreset_runner_unwired", map[string]any{
				"action": "worker_not_started",
			})
		}
		return closedChan()
	}
	inner, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.mu.Lock()
	r.cancel = cancel
	r.done = done
	r.mu.Unlock()

	go func() {
		defer close(done)
		r.supervise(inner)
	}()
	return done
}

// Stop 取消消费循环并等待它退出。
func (r *Runner) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.cancel, r.done = nil, nil
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
}

// supervise 循环抢消费锁并消费；没抢到锁则退避重试，ctx 结束则退出。
//
// Stop 取消 ctx 时会连带取消在途作业的 ctx：删行的语句会被中断，状态停在 running，租约到期后由
// 下一任主重试。这与「宁可重跑一遍也不无限期挡住关闭」的取舍一致（重跑是幂等的）。
func (r *Runner) supervise(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		lock, acquired, err := jobs.AcquireLeader(ctx, r.pools, r.leaderName)
		switch {
		case err != nil:
			// 连接或语句失败：记 warn 后重试。advisory 锁是跨实例互斥的唯一凭据，
			// 拿不到就消费等于两个实例同跑同一个作业。
			r.logger.Warn("go_usersreset_leader_acquire_failed", map[string]any{"error": err.Error()})
		case !acquired:
			r.logger.Info("go_usersreset_worker_standby", map[string]any{
				"lock": r.leaderName,
			})
		default:
			r.logger.Info("go_usersreset_worker_leader", map[string]any{
				"lock": r.leaderName,
			})
			r.worker.Run(ctx)
			// 消费循环只在 ctx 结束时返回；释放锁后再看 ctx，已取消就退出。
			_ = lock.Release(context.WithoutCancel(ctx))
			if ctx.Err() != nil {
				return
			}
		}
		if !sleepCtx(ctx, r.leaderRetry) {
			return
		}
	}
}

// closedChan 返回一个已关闭的通道（不可用时的占位 done）。
func closedChan() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
