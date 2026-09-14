package deps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/httpapi"
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
