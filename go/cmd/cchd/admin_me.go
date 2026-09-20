package main

import (
	"context"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
)

// 本文件把**自服务面**（/api/v1/me/*）需要的两个 **User 维度** Redis 运行态读数接进进程启动。
//
// 为什么单列这两个：me/quota 的 userCurrentConcurrentSessions 与 userCurrent5hUsd 在 Node 侧
// 读的是 **User 维度**的键（SessionTracker.getUserSessionCount、
// RateLimitService.getCurrentCost(user.id, "user", "5h", ...)），而管理面此前只装配了 Key 维度
// 的两个读数（keys 的三条读档端点用）。两者不是同一个键：同一会话跨密钥续用时，User 维度 ZSET
// 去重、各密钥计数各算一次。
//
// 与 admin.go 的 openLimitRuntime 的关系：Redis 运行态（脚本客户端、会话跟踪器、成本窗口）
// 只装配一次，本文件只提供把同一份运行态适配成 **User 维度**接口的两个薄包装
// （Key 维度与 User 维度不是同一个键，见上）。

// meUserSessionCounter 把 limit.SessionTracker 适配成自服务面要的 User 维度计数接口。
type meUserSessionCounter struct{ tracker *limit.SessionTracker }

func (c meUserSessionCounter) UserSessionCount(ctx context.Context, userID int64) (int, error) {
	return c.tracker.UserSessionCount(ctx, userID)
}

// meUserFixed5hReader 把成本窗口层适配成 User 维度的 5h 固定窗口读取器。
type meUserFixed5hReader struct{ windows *limit.CostWindows }

func (r meUserFixed5hReader) UserFixed5hWindowState(
	ctx context.Context,
	userID int64,
	now time.Time,
) (limit.Fixed5hState, error) {
	return r.windows.Fixed5hWindowState(ctx, limit.EntityUser, userID, now)
}
