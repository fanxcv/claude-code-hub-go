// cchd 的熔断开闸告警装配：把 health 的回调接到通知栈上。
//
// 为什么要在这里而不是 dataplane：告警要读通知设置与绑定、要投递 webhook，这些都在
// 通知栈里；dataplane 只把开闸事件交出来（见 dataplane.CircuitAlerts）。本文件负责
// 「事件 → 通知栈」这一段，并让数据面、管理面拨测、运维任务三处共用同一份实现，
// 避免三个装配点各写一套参数（去重 TTL、投递语义必须一致）。
package main

import (
	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dataplane"
	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// newCircuitBreakerAlerts 建熔断告警的产生点，返回可以直接塞进 health.Options 的两个回调。
//
// 缺件时的行为（都是「不发告警」而不是「起不来」）：
//   - pools 为 nil：没有任何读面，直接返回空回调；
//   - redis 为 nil：去重降级为不去重（与 Node 无 Redis 的分支一致），其它照常。
//
// 注意这是**开闸路径上的同步调用**（转发失败 → 记账 → 开闸 → 回调），故产生点内部
// 自行异步派发并吃 panic，装配处不需要再包一层。
func newCircuitBreakerAlerts(
	logger *logx.Logger,
	pools *store.Pools,
	redisClient redis.UniversalClient,
) dataplane.CircuitAlerts {
	if pools == nil {
		return dataplane.CircuitAlerts{}
	}
	publisher := jobs.NewCircuitBreakerAlertPublisher(jobs.CircuitBreakerAlertOptions{
		Pools: pools,
		// 与通知调度器同一种投递（信封、签名、响应判定），只是投递的是事件型 payload。
		Deliverer: adminapi.NewNotificationDelivery(pools, logger),
		Dedup:     jobs.NewRedisNotifyDeduper(redisClient),
		Logger:    logger,
	})
	return dataplane.CircuitAlerts{
		OnProviderOpened: publisher.OnProviderOpened,
		OnEndpointOpened: publisher.OnEndpointOpened,
	}
}
