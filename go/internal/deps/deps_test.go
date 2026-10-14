package deps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/httpapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

func loadConfig(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	cfg, err := config.Load(func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("装载测试配置失败: %v", err)
	}
	return cfg
}

// 未配置 DSN / REDIS_URL 时不得报错，必须如实报 not_configured。
func TestNewWithoutDependencies(t *testing.T) {
	bundle, err := New(context.Background(), loadConfig(t, nil), cfgsync.New())
	if err != nil {
		t.Fatalf("缺少依赖配置不应阻断启动: %v", err)
	}
	defer func() { _ = bundle.Close() }()

	ctx := context.Background()
	if status, note := bundle.PingPG(ctx); status != httpapi.StatusNotSet {
		t.Errorf("未配置 DSN 时 pg 状态应为 not_configured，收到 %q (%s)", status, note)
	}
	if status, note := bundle.PingRedis(ctx); status != httpapi.StatusNotSet {
		t.Errorf("未配置 REDIS_URL 时 redis 状态应为 not_configured，收到 %q (%s)", status, note)
	}
	if status, _ := bundle.RulesStatus(); status != httpapi.StatusNotLoaded {
		t.Errorf("快照未装载时 rules 状态应为 not_loaded，收到 %q", status)
	}
}

// 指向不可达地址时必须报 error，而不是谎报 ok。
func TestUnreachableDependenciesReportError(t *testing.T) {
	cfg := loadConfig(t, map[string]string{
		// 保留端口段内的高位端口，测试环境下不会有服务监听。
		"DSN":       "postgresql://user:pw@127.0.0.1:9/db",
		"REDIS_URL": "redis://127.0.0.1:9/0",
	})
	bundle, err := New(context.Background(), cfg, cfgsync.New())
	if err != nil {
		t.Fatalf("建池本身不应失败（惰性连接）: %v", err)
	}
	defer func() { _ = bundle.Close() }()

	// PG 探针走共享池：接上 store 那条 control 道后，指向不可达地址才可探。
	pools, err := store.Open(context.Background(), store.Options{
		DSN: cfg.DSN, Budget: cfg.Pool, Timeouts: cfg.DB,
	})
	if err != nil {
		t.Fatalf("开共享池不应失败（惰性连接）: %v", err)
	}
	defer func() { _ = pools.Close() }()
	bundle.AttachStorePools(pools)

	ctx, cancel := context.WithTimeout(context.Background(), 5e9)
	defer cancel()

	status, note := bundle.PingPG(ctx)
	if status != httpapi.StatusError {
		t.Fatalf("不可达的 PG 应报 error，收到 %q (%s)", status, note)
	}
	status, note = bundle.PingRedis(ctx)
	if status != httpapi.StatusError {
		t.Fatalf("不可达的 Redis 应报 error，收到 %q (%s)", status, note)
	}
	// 凭据纪律：探测错误信息不得回显口令。
	for _, text := range []string{note} {
		if strings.Contains(text, "pw") {
			t.Fatalf("探测错误信息泄漏了口令片段: %q", text)
		}
	}
}

// 命令超时与 TLS 校验必须取环境配置，而不是沿用硬编码的 10s / 强制校验证书。
func TestRedisOptionsHonorConfig(t *testing.T) {
	cfg := loadConfig(t, map[string]string{
		"REDIS_URL":                     "rediss://user:pw@host:6379/0",
		"REDIS_COMMAND_TIMEOUT_MS":      "1500",
		"REDIS_TLS_REJECT_UNAUTHORIZED": "false",
	})
	bundle, err := New(context.Background(), cfg, cfgsync.New())
	if err != nil {
		t.Fatalf("建依赖失败: %v", err)
	}
	defer func() { _ = bundle.Close() }()

	options := bundle.redis.Options()
	if options.ReadTimeout != 1500*time.Millisecond || options.WriteTimeout != 1500*time.Millisecond {
		t.Fatalf("命令超时应取 REDIS_COMMAND_TIMEOUT_MS，收到 read=%s write=%s", options.ReadTimeout, options.WriteTimeout)
	}
	if options.TLSConfig == nil || !options.TLSConfig.InsecureSkipVerify {
		t.Fatalf("REDIS_TLS_REJECT_UNAUTHORIZED=false 时应跳过证书校验")
	}
}

// 未配置时回退到原硬编码值。
func TestRedisCommandTimeoutFallback(t *testing.T) {
	cfg := loadConfig(t, nil)
	if got := redisCommandTimeout(cfg); got != 10*time.Second {
		t.Fatalf("未配置时应回退 10s，收到 %s", got)
	}
}

// PG 探针的共享池契约：配了 DSN 但池未接上时报 not_configured（不自己拨号），
// 接上后才探活。改前本包自建池，没有「未接上」这个中间态，也就没有这条分支。
func TestPingPGWaitsForSharedPool(t *testing.T) {
	cfg := loadConfig(t, map[string]string{"DSN": "postgresql://user:pw@127.0.0.1:9/db"})
	bundle, err := New(context.Background(), cfg, cfgsync.New())
	if err != nil {
		t.Fatalf("建依赖失败: %v", err)
	}
	defer func() { _ = bundle.Close() }()

	if status, note := bundle.PingPG(context.Background()); status != httpapi.StatusNotSet {
		t.Fatalf("池未接上时应报 not_configured，收到 %q (%s)", status, note)
	}
}

func TestRulesStatusFollowsSnapshot(t *testing.T) {
	rules := cfgsync.New()
	bundle, err := New(context.Background(), loadConfig(t, nil), rules)
	if err != nil {
		t.Fatalf("建依赖失败: %v", err)
	}
	defer func() { _ = bundle.Close() }()

	if status, _ := bundle.RulesStatus(); status != httpapi.StatusNotLoaded {
		t.Fatalf("装载前应为 not_loaded，收到 %q", status)
	}
	rules.MarkLoaded("test-version")
	if status, _ := bundle.RulesStatus(); status != httpapi.StatusOK {
		t.Fatalf("装载后应为 ok，收到 %q", status)
	}
}

// 凭据纪律的兜底：sanitize 必须把原文替换掉，即使错误来自第三方库。
func TestSanitizeRedactsSecrets(t *testing.T) {
	const dsn = "postgresql://user:top-secret@host:5432/db"
	const redisURL = "redis://:another-secret@host:6379/0"
	bundle := &Bundle{secrets: []string{dsn, redisURL}}

	sanitized := bundle.sanitize(errors.New("dial " + dsn + " failed, then " + redisURL))
	message := sanitized.Error()
	if strings.Contains(message, "top-secret") || strings.Contains(message, "another-secret") {
		t.Fatalf("sanitize 未抹掉凭据: %q", message)
	}
	if !strings.Contains(message, "[redacted]") {
		t.Fatalf("sanitize 应留下 [redacted] 标记: %q", message)
	}
	if bundle.sanitize(nil) != nil {
		t.Fatalf("nil 错误应保持 nil")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	bundle, err := New(context.Background(), loadConfig(t, nil), cfgsync.New())
	if err != nil {
		t.Fatalf("建依赖失败: %v", err)
	}
	if err := bundle.Close(); err != nil {
		t.Fatalf("首次关闭不应报错: %v", err)
	}
	if err := bundle.Close(); err != nil {
		t.Fatalf("重复关闭不应报错: %v", err)
	}
}

func TestParseErrorsDoNotLeakCredentials(t *testing.T) {
	cfg := loadConfig(t, map[string]string{"REDIS_URL": "redis://:leak-me@host:6379"})
	bundle := &Bundle{secrets: []string{cfg.RedisURL}}
	if _, err := New(context.Background(), cfg, cfgsync.New()); err != nil {
		if strings.Contains(err.Error(), "leak-me") {
			t.Fatalf("建依赖的错误信息泄漏了凭据: %v", err)
		}
	}
	if sanitized := bundle.sanitize(errors.New("boom " + cfg.RedisURL)); strings.Contains(sanitized.Error(), "leak-me") {
		t.Fatalf("sanitize 未抹掉 REDIS_URL: %v", sanitized)
	}
}
