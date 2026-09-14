// cchd 的用户统计重置装配：队列、执行器、选主 worker 与生命周期。
//
// 装配粒度：**缺 Redis 或缺连接池就整组不装配**（返回空的 runtime，queue 为 nil）。这不是保守，
// 而是这两条路由没有可用的降级——排不进去的作业会让 UI 显示一个永远 queued 的状态，查不到的
// 状态会让轮询一直 404，两者都比「不接管、回退 Node」更坏。
package main

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/usersreset"
)

// usersResetOptions 是装配缝的参数。
type usersResetOptions struct {
	Logger *logx.Logger
	Pools  *store.Pools
	// Redis 与管理面其余部分共用同一条命令连接（与 ProviderUndoKV / public-status 同一取舍）。
	Redis redis.UniversalClient
}

// usersResetRuntime 持有队列与 worker 的生命周期。
type usersResetRuntime struct {
	// queue 为 nil 时管理面那两条路由不注册（回退 Node）。
	queue  adminapi.UsersResetQueue
	runner *usersreset.Runner
}

// startUsersReset 装配并启动重置队列的消费者。
func startUsersReset(options usersResetOptions) *usersResetRuntime {
	if options.Pools == nil || options.Redis == nil {
		return &usersResetRuntime{}
	}
	queue := usersreset.NewQueue(usersreset.QueueOptions{
		Status: usersreset.NewStatusStore(options.Redis),
		Pools:  options.Pools,
		Logger: options.Logger,
	})
	executor := usersreset.NewResetExecutor(
		options.Pools,
		usersreset.NewCostCleaner(options.Redis, options.Logger),
		options.Logger,
	)
	worker := usersreset.NewWorker(usersreset.WorkerOptions{
		Queue:    queue,
		Executor: executor,
		Logger:   options.Logger,
	})
	runner := usersreset.NewRunner(usersreset.RunnerOptions{
		Worker: worker,
		Pools:  options.Pools,
		Logger: options.Logger,
	})
	// ctx 归 runner 自己（Stop 会取消它）：管理面的装配函数拿不到进程级 ctx，
	// 而这条消费者的生命周期就是「管理面装配到释放」这一段。
	runner.Start(context.Background())
	return &usersResetRuntime{queue: queue, runner: runner}
}

// stop 停掉消费者并等待在途作业退出。
//
// 必须在关命令连接**之前**调用：worker 的每一次状态写入都要用那条连接，先关连接会让在途作业
// 以「状态写不下去」收场（作业停在 running，只能等租约到期后由下一任主重跑）。
func (r *usersResetRuntime) stop() {
	if r == nil || r.runner == nil {
		return
	}
	r.runner.Stop()
}
