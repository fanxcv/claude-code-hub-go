package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住数据面限流的**生产装配**。三条断言各自对着一种失败形态：
//   1. 没有 Redis 时不得装配（否则等于假装有数）；
//   2. 有依赖时必须装配，且启动日志的 gaps 不含 RateLimit/AuthThrottle（本轮之前的缺口语义）；
//   3. 限额快照必须按 Node 的列名映射（两个易错点：users.daily_limit_usd 与 users.rpm_limit）。

// TestOpenRateLimiterWithoutRedisStaysUnwired 覆盖降级分支：没有命令连接就不装配，
// 并留下可检索的日志——否则「限流没接线」在生产上只剩 gaps 一行，难定位到原因。
func TestOpenRateLimiterWithoutRedisStaysUnwired(t *testing.T) {
	var logs strings.Builder
	limiter := openRateLimiter(context.Background(), config.Config{}, nil, nil, logx.New(&logs))
	if limiter != nil {
		t.Fatalf("没有 Redis 时不得装配限流器：%v", limiter)
	}
	if !strings.Contains(logs.String(), "ratelimit_unavailable") {
		t.Fatalf("必须留下 ratelimit_unavailable 日志：%s", logs.String())
	}
	if !strings.Contains(logs.String(), "redis_missing") {
		t.Fatalf("日志应说明缺的是 Redis：%s", logs.String())
	}
}

// TestIntegrationRateLimitGapsClosed 是接线验收：真库 + 真 Redis 起一个真进程，启动日志里
// `dataplane_ready` 的 gaps 不得再含 RateLimit/AuthThrottle，且必须出现 ratelimit_ready。
//
// 为什么不钉等值 gaps：未来接线项（如回放）会往里加；这里要钉的是「限流不再是缺口」。
func TestIntegrationRateLimitGapsClosed(t *testing.T) {
	dsn := os.Getenv("CCH_TEST_DSN")
	redisURL := os.Getenv("CCH_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("未设置 CCH_TEST_DSN / CCH_TEST_REDIS_URL，跳过集成测试")
	}

	binary := filepath.Join(t.TempDir(), "cchd")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("编译失败: %v\n%s", err, output)
	}

	publicPort := freePort(t)
	var logs bytes.Buffer
	command := exec.Command(binary)
	command.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", publicPort),
		"DSN="+dsn,
		"REDIS_URL="+redisURL,
		"ADMIN_TOKEN=cchd-ratelimit-integration-token",
		"NODE_ENV=production",
		// 数据面归属 Go：本用例验装配而不是路由归属，但归属开起来才能走到 dataplane_ready。
		"CCH_EGRESS_MODE=go",
		"AUTO_MIGRATE=false",
	)
	command.Stderr = &logs
	command.Stdout = &logs
	if err := command.Start(); err != nil {
		t.Fatalf("启动进程失败: %v", err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", publicPort)
	waitFor(t, "进程开始应答", func() bool {
		response, err := http.Get(baseURL + "/v1/_ping")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusOK
	})
	waitFor(t, "进程转为就绪", func() bool {
		status, report := probeReady(baseURL)
		return status == http.StatusOK && report.Ready
	})

	startup := logs.String()
	if !strings.Contains(startup, "ratelimit_ready") {
		t.Fatalf("启动日志必须报出限流已装配：%s", tail(startup, 2000))
	}
	line := findEvent(startup, "dataplane_ready")
	if line == "" {
		t.Fatalf("启动日志缺少 dataplane_ready：%s", tail(startup, 2000))
	}
	var event struct {
		Gaps []string `json:"gaps"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatalf("解析 dataplane_ready 失败: %v\n%s", err, line)
	}
	for _, gap := range event.Gaps {
		if gap == "RateLimit" || gap == "AuthThrottle" {
			t.Fatalf("限流仍被记为缺口（gaps=%v），接线没生效", event.Gaps)
		}
	}
}

// TestIntegrationStoreQuotasReadsNodeColumns 是限额快照映射的真库验收：造一行密钥与一行用户，
// 把每个字段都设成可辨识的值，读回后逐字段比对。
//
// 重点覆盖两处**列名与属性名不一致**的地方（照 Node 的 repository 映射）：
//   - `user.dailyQuota` 来自 `users.daily_limit_usd`（不是 `limit_daily_usd`）；
//   - `user.rpm` 来自 `users.rpm_limit`。
func TestIntegrationStoreQuotasReadsNodeColumns(t *testing.T) {
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过集成测试")
	}
	ctx := context.Background()
	pools, err := store.Open(ctx, store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(4),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-ratelimit-boot",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取数据分道失败: %v", err)
	}

	suffix := time.Now().UnixNano()
	userName := fmt.Sprintf("ratelimit-probe-%d", suffix)
	keyValue := fmt.Sprintf("sk-ratelimit-probe-%d", suffix)

	var userID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (name, rpm_limit, daily_limit_usd, limit_5h_usd, limit_weekly_usd,
			limit_monthly_usd, limit_total_usd, limit_concurrent_sessions, cost_reset_at, limit_5h_cost_reset_at)
		VALUES ($1, 7, 11.25, 1.50, 3.75, 5.25, 9.50, 4, '2030-01-02 03:04:05+00', '2030-02-03 04:05:06+00')
		RETURNING id`, userName).Scan(&userID); err != nil {
		t.Fatalf("插入用户夹具失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	var keyID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO keys (user_id, key, name, limit_5h_usd, limit_daily_usd, limit_weekly_usd,
			limit_monthly_usd, limit_total_usd, limit_concurrent_sessions, cost_reset_at)
		VALUES ($1, $2, $2, 0.50, 2.50, 4.50, 6.50, 8.50, 3, '2030-03-04 05:06:07+00')
		RETURNING id`, userID, keyValue).Scan(&keyID); err != nil {
		t.Fatalf("插入密钥夹具失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM keys WHERE id = $1`, keyID)
	})

	quotas := storeQuotas{pools: pools}
	keyQuota, err := quotas.KeyQuota(ctx, keyID)
	if err != nil {
		t.Fatalf("读密钥限额失败: %v", err)
	}
	if keyQuota.KeyID != keyID {
		t.Fatalf("KeyID 应为 %d，收到 %d", keyID, keyQuota.KeyID)
	}
	if keyQuota.UserID != userID {
		t.Fatalf("UserID 应为 %d，收到 %d", userID, keyQuota.UserID)
	}
	if keyQuota.KeyHash != keyValue {
		t.Fatalf("KeyHash 必须是账本用的密钥明文：收到 %q", keyQuota.KeyHash)
	}
	assertFloat(t, "key.Limit5hUSD", keyQuota.Limit5hUSD, 0.5)
	assertFloat(t, "key.LimitDailyUSD", keyQuota.LimitDailyUSD, 2.5)
	assertFloat(t, "key.LimitWeeklyUSD", keyQuota.LimitWeeklyUSD, 4.5)
	assertFloat(t, "key.LimitMonthlyUSD", keyQuota.LimitMonthlyUSD, 6.5)
	assertFloat(t, "key.LimitTotalUSD", keyQuota.LimitTotalUSD, 8.5)
	if keyQuota.LimitConcurrentSessions != 3 {
		t.Fatalf("key 并发上限应为 3，收到 %d", keyQuota.LimitConcurrentSessions)
	}
	if keyQuota.CostResetAt == nil || !keyQuota.CostResetAt.Equal(time.Date(2030, 3, 4, 5, 6, 7, 0, time.UTC)) {
		t.Fatalf("key 成本重置时刻不符: %v", keyQuota.CostResetAt)
	}

	userQuota, err := quotas.UserQuota(ctx, userID)
	if err != nil {
		t.Fatalf("读用户限额失败: %v", err)
	}
	if userQuota.UserID != userID {
		t.Fatalf("UserID 应为 %d，收到 %d", userID, userQuota.UserID)
	}
	assertFloat(t, "user.Limit5hUSD", userQuota.Limit5hUSD, 1.5)
	// 这两条是本用例的存在理由：列名与 Node 的属性名不一致。
	assertFloat(t, "user.LimitDailyUSD（users.daily_limit_usd）", userQuota.LimitDailyUSD, 11.25)
	assertFloat(t, "user.LimitWeeklyUSD", userQuota.LimitWeeklyUSD, 3.75)
	assertFloat(t, "user.LimitMonthlyUSD", userQuota.LimitMonthlyUSD, 5.25)
	assertFloat(t, "user.LimitTotalUSD", userQuota.LimitTotalUSD, 9.5)
	if userQuota.RPM != 7 {
		t.Fatalf("RPM 应取自 users.rpm_limit（期望 7），收到 %d", userQuota.RPM)
	}
	if userQuota.LimitConcurrentSessions != 4 {
		t.Fatalf("user 并发上限应为 4，收到 %d", userQuota.LimitConcurrentSessions)
	}
	if userQuota.CostResetAt == nil || !userQuota.CostResetAt.Equal(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("user 成本重置时刻不符: %v", userQuota.CostResetAt)
	}
	if userQuota.Limit5hCostResetAt == nil ||
		!userQuota.Limit5hCostResetAt.Equal(time.Date(2030, 2, 3, 4, 5, 6, 0, time.UTC)) {
		t.Fatalf("user 5h 成本重置时刻不符: %v", userQuota.Limit5hCostResetAt)
	}
}

// TestIntegrationStoreQuotasTreatsNullAsUnlimited 钉住 NULL 语义：未设限额读成 nil/0，
// 由 limit 包按各维度默认值处理（5h 默认 rolling、每日默认 fixed、RPM 0 表示不限速）。
func TestIntegrationStoreQuotasTreatsNullAsUnlimited(t *testing.T) {
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过集成测试")
	}
	ctx := context.Background()
	pools, err := store.Open(ctx, store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(4),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-ratelimit-boot",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取数据分道失败: %v", err)
	}

	suffix := time.Now().UnixNano()
	var userID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (name) VALUES ($1) RETURNING id`,
		fmt.Sprintf("ratelimit-null-%d", suffix)).Scan(&userID); err != nil {
		t.Fatalf("插入用户夹具失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	quotas := storeQuotas{pools: pools}
	quota, err := quotas.UserQuota(ctx, userID)
	if err != nil {
		t.Fatalf("读用户限额失败: %v", err)
	}
	if quota.Limit5hUSD != nil || quota.LimitDailyUSD != nil || quota.LimitTotalUSD != nil {
		t.Fatalf("未设限额应读成 nil（由 limit 包按维度默认值处理）：%+v", quota)
	}
	if quota.RPM != 0 {
		t.Fatalf("rpm_limit 为 NULL 时应为 0（不限速），收到 %d", quota.RPM)
	}
	// 重置模式留空即交给 limit 包补默认值：这里写入 DB 默认值，故断言读回的是库里的真值。
	if quota.Limit5hResetMode != limit.ResetRolling {
		t.Fatalf("users.limit_5h_reset_mode 默认应为 rolling，收到 %q", quota.Limit5hResetMode)
	}
	if quota.DailyResetMode != limit.ResetFixed {
		t.Fatalf("users.daily_reset_mode 默认应为 fixed，收到 %q", quota.DailyResetMode)
	}
}

func assertFloat(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s 应为 %v，收到 nil", name, want)
	}
	if *got != want {
		t.Fatalf("%s 应为 %v，收到 %v", name, want, *got)
	}
}

// findEvent 从 JSON 行日志里取出某个 event 的那一行（日志是逐行 JSON）。
func findEvent(logs string, event string) string {
	for _, line := range strings.Split(logs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		if strings.Contains(line, `"event":"`+event+`"`) {
			return line
		}
	}
	return ""
}

// tail 截取日志尾部，用于失败时报出上下文而不是整份日志。
func tail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return "..." + text[len(text)-limit:]
}

var _ = io.Discard
