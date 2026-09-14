package main

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
)

// 本文件把**自服务面**（/api/v1/me/*）需要的两个 **User 维度** Redis 运行态读数接进进程启动。
//
// 为什么单列这两个：me/quota 的 userCurrentConcurrentSessions 与 userCurrent5hUsd 在 Node 侧
// 读的是 **User 维度**的键（SessionTracker.getUserSessionCount、
// RateLimitService.getCurrentCost(user.id, "user", "5h", ...)），而管理面此前只装配了 Key 维度
// 的两个读数（keys 的三条读档端点用）。两者不是同一个键：同一会话跨密钥续用时，User 维度 ZSET
// 去重、各密钥计数各算一次。
//
// 与 openLimitRuntime（admin.go）的关系：那是 Key 维度、返回 adminapi.SessionCounter /
// Fixed5hWindowReader 两个窄接口，签名已被 admin_limit_runtime_test.go 钉住，故此处另建一份
// 同源实现而不是改它的签名——代价是 Lua 注册表与脚本客户端各建一次（只发生在启动期）。
// 若将来两者合并，请一并删掉这段说明。

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

// openMeRuntime 建自服务面所需的两个 User 维度读取器。
//
// 拿不到时返回 (nil, nil)：RegisterMeRoutes 据此**不注册** /me/quota（那些读数恒 0 是静默错数，
// 回退 Node 至少是对的）。metadata 与 today 不依赖 Redis，照常注册。
func openMeRuntime(
	cfg config.Config,
	redisClient redis.UniversalClient,
	logger *logx.Logger,
) (adminapi.UserSessionCounter, adminapi.UserFixed5hWindowReader) {
	if redisClient == nil {
		logger.Info("admin_me_runtime_skipped", map[string]any{
			"reason": "redis_unconfigured",
			"effect": "me_quota_fallback_node",
		})
		return nil, nil
	}
	registry, err := ratelimit.Embedded()
	if err != nil {
		logger.Warn("admin_me_runtime_unavailable", map[string]any{
			"stage": "registry",
			"error": err.Error(),
		})
		return nil, nil
	}
	scriptClient, err := ratelimit.New(redisClient, registry)
	if err != nil {
		logger.Warn("admin_me_runtime_unavailable", map[string]any{
			"stage": "client",
			"error": err.Error(),
		})
		return nil, nil
	}
	// SESSION_TTL 的单位与 Node 一致（秒，可小数）；<=0 时由 NewSessionTracker 取 300s 默认值。
	sessionTTL := time.Duration(cfg.Env.SessionTTL * float64(time.Second))
	return meUserSessionCounter{tracker: limit.NewSessionTracker(scriptClient, sessionTTL, logger)},
		meUserFixed5hReader{windows: limit.NewCostWindows(scriptClient, logger)}
}
