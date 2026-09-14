package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住 keys 三条读档路由的**生产装配**：它们是条件注册的（缺 Redis 运行态依赖就不注册，
// 回退 Node），因此「依赖真的被接上」必须单独验证——注册点那条测试只证明注册点调了 registrar，
// 证明不了 `openLimitRuntime` 在生产配置下拿得到两个读取器。
//
// 两条断言分别对着两类缺陷：没配 Redis 时**不该**装配（否则等于假装有数）、配了 Redis 时
// **必须**装配（否则那三条端点永远由 Node 作答，且只有启动日志能看出来）。

// TestOpenLimitRuntimeWithoutRedisStaysUnwired 覆盖降级分支：没有命令连接就不装配，
// 并留下可检索的日志（否则「少了三条路由」在生产上无从发现）。
func TestOpenLimitRuntimeWithoutRedisStaysUnwired(t *testing.T) {
	var logs strings.Builder
	counts, windows, costWindows := openLimitRuntime(config.Config{}, nil, logx.New(&logs))
	if counts != nil || windows != nil || costWindows != nil {
		t.Fatalf("没有 Redis 连接时不得装配运行态读取器：counts=%v windows=%v costWindows=%v", counts, windows, costWindows)
	}
	if !strings.Contains(logs.String(), "admin_limit_runtime_skipped") {
		t.Fatalf("必须留下 admin_limit_runtime_skipped 日志：%s", logs.String())
	}
}

// TestIntegrationOpenLimitRuntimeWiresKeysReadRoutes 是生产装配的集成验证：
// 真 Redis 下两个读取器都被建出，且注入后 keys 的 14 条路由全部注册（含三条读档）。
func TestIntegrationOpenLimitRuntimeWiresKeysReadRoutes(t *testing.T) {
	redisURL := os.Getenv("CCH_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过集成测试")
	}
	cfg := config.Config{RedisURL: redisURL}
	cfg.Env.SessionTTL = 300
	client, err := openCommandRedis(cfg)
	if err != nil {
		t.Fatalf("建 Redis 连接失败: %v", err)
	}
	t.Cleanup(func() { _ = closeRedis(client) })

	counts, windows, costWindows := openLimitRuntime(cfg, client, logx.New(io.Discard))
	if counts == nil || windows == nil || costWindows == nil {
		t.Fatal("配了 Redis 就必须装配三个运行态读取器（否则读档与供应商限额端点永远回退 Node）")
	}

	// 读取器必须真的能打在真 Redis 上（而不是只被判了非 nil）。
	if _, err := counts.KeySessionCount(context.Background(), 1); err != nil {
		t.Fatalf("会话计数读取失败: %v", err)
	}
	if _, err := windows.Fixed5hWindowState(context.Background(), 1, time.Now()); err != nil {
		t.Fatalf("5h 固定窗口读取失败: %v", err)
	}

	router := adminapi.New(adminapi.Options{Deps: adminapi.Deps{Guard: &stubAdminGuard{}}})
	adminapi.RegisterKeysRoutes(router, adminapi.Deps{
		Logger:         logx.New(io.Discard),
		Guard:          &stubAdminGuard{},
		Problems:       adminapi.NewProblems(nil),
		Store:          new(store.Pools),
		SessionCounts:  counts,
		Fixed5hWindows: windows,
	})
	if got := router.RouteCount(); got != keysRegistrarRouteCount {
		t.Fatalf("装配运行态依赖后 RegisterKeysRoutes 应注册 %d 条，实际 %d", keysRegistrarRouteCount, got)
	}
}

// 让 redis 的导入与 stubAdminGuard 的用法在本文件内明确（openCommandRedis 的返回类型）。
var _ redis.UniversalClient = (*redis.Client)(nil)
