package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件把限流服务接进数据面：限额快照来源、时区、脚本客户端，以及服务本身的装配。
//
// 为什么快照要查库：Node 侧的限额字段来自 `authState.key` / `authState.user`（鉴权阶段一次性
// 读出的整行），而 Go 的认证快照（`pctx.AuthState`）只带 id 与展示名。两行都在主键上，且限额是
// 低频变更的管理配置，故按 id 现读。`internal/limit` 自述的「不查库」指的是**额度累计值**不查库
// （那是 Redis 窗口 + 账本回退的职责），不是限额字段。

// storeQuotas 是 limit.QuotaSource 的 store 实现。
//
// 列名与 Node 的映射（`src/repository/key.ts:594,722`、`src/repository/user.ts:73`）：
//   - `user.dailyQuota` 是**属性名**，落库列是 `users.daily_limit_usd`（不是 `limit_daily_usd`）；
//   - `user.rpm` 落库列是 `users.rpm_limit`；
//   - 其余字段与列名同名。
type storeQuotas struct {
	pools *store.Pools
}

// 数值列一律 `::text` 后自行解析：本仓的读取层（store）统一走 `row_to_json`，从未依赖 pgx 的
// numeric 编解码器；这里沿用同一口径，免得 NULL 与精度语义在两种路径下分叉。
const keyQuotaSQL = `SELECT
	user_id,
	key,
	limit_5h_usd::text,
	limit_5h_reset_mode::text,
	limit_daily_usd::text,
	daily_reset_mode::text,
	daily_reset_time,
	limit_weekly_usd::text,
	limit_monthly_usd::text,
	limit_total_usd::text,
	limit_concurrent_sessions,
	cost_reset_at
FROM keys WHERE id = $1`

const userQuotaSQL = `SELECT
	limit_5h_usd::text,
	limit_5h_reset_mode::text,
	daily_limit_usd::text,
	daily_reset_mode::text,
	daily_reset_time,
	limit_weekly_usd::text,
	limit_monthly_usd::text,
	limit_total_usd::text,
	limit_concurrent_sessions,
	rpm_limit,
	cost_reset_at,
	limit_5h_cost_reset_at
FROM users WHERE id = $1`

// KeyQuota 读回密钥维度的限额快照。
func (q storeQuotas) KeyQuota(ctx context.Context, keyID int64) (limit.KeyQuota, error) {
	pool, err := q.pools.Data()
	if err != nil {
		return limit.KeyQuota{}, err
	}
	var (
		quota                                 limit.KeyQuota
		keyHash                               string
		five5h, daily, weekly, monthly, total *string
		five5hMode, dailyMode                 *string
		dailyResetTime                        *string
		concurrent                            *int
		costResetAt                           *time.Time
	)
	row := pool.QueryRow(ctx, keyQuotaSQL, keyID)
	if err := row.Scan(
		&quota.UserID, &keyHash,
		&five5h, &five5hMode, &daily, &dailyMode, &dailyResetTime,
		&weekly, &monthly, &total,
		&concurrent, &costResetAt,
	); err != nil {
		return limit.KeyQuota{}, fmt.Errorf("limit: 读密钥限额失败（key=%d）: %w", keyID, err)
	}
	quota.KeyID = keyID
	// KeyHash 是账本 `usage_ledger.key` 列存的密钥明文：额度累计按密钥字符串聚合，不按 id。
	quota.KeyHash = keyHash
	if quota.Limit5hUSD, err = floatOrNil(five5h); err != nil {
		return limit.KeyQuota{}, err
	}
	if quota.LimitDailyUSD, err = floatOrNil(daily); err != nil {
		return limit.KeyQuota{}, err
	}
	if quota.LimitWeeklyUSD, err = floatOrNil(weekly); err != nil {
		return limit.KeyQuota{}, err
	}
	if quota.LimitMonthlyUSD, err = floatOrNil(monthly); err != nil {
		return limit.KeyQuota{}, err
	}
	if quota.LimitTotalUSD, err = floatOrNil(total); err != nil {
		return limit.KeyQuota{}, err
	}
	quota.Limit5hResetMode = resetModeOr(five5hMode)
	quota.DailyResetMode = resetModeOr(dailyMode)
	if dailyResetTime != nil {
		quota.DailyResetTime = *dailyResetTime
	}
	if concurrent != nil {
		quota.LimitConcurrentSessions = *concurrent
	}
	quota.CostResetAt = costResetAt
	return quota, nil
}

// UserQuota 读回用户维度的限额快照。
func (q storeQuotas) UserQuota(ctx context.Context, userID int64) (limit.UserQuota, error) {
	pool, err := q.pools.Data()
	if err != nil {
		return limit.UserQuota{}, err
	}
	var (
		quota                                 limit.UserQuota
		five5h, daily, weekly, monthly, total *string
		five5hMode, dailyMode                 *string
		dailyResetTime                        *string
		concurrent, rpm                       *int
		costResetAt, five5hCostResetAt        *time.Time
	)
	row := pool.QueryRow(ctx, userQuotaSQL, userID)
	if err := row.Scan(
		&five5h, &five5hMode, &daily, &dailyMode, &dailyResetTime,
		&weekly, &monthly, &total,
		&concurrent, &rpm,
		&costResetAt, &five5hCostResetAt,
	); err != nil {
		return limit.UserQuota{}, fmt.Errorf("limit: 读用户限额失败（user=%d）: %w", userID, err)
	}
	quota.UserID = userID
	if quota.Limit5hUSD, err = floatOrNil(five5h); err != nil {
		return limit.UserQuota{}, err
	}
	if quota.LimitDailyUSD, err = floatOrNil(daily); err != nil {
		return limit.UserQuota{}, err
	}
	if quota.LimitWeeklyUSD, err = floatOrNil(weekly); err != nil {
		return limit.UserQuota{}, err
	}
	if quota.LimitMonthlyUSD, err = floatOrNil(monthly); err != nil {
		return limit.UserQuota{}, err
	}
	if quota.LimitTotalUSD, err = floatOrNil(total); err != nil {
		return limit.UserQuota{}, err
	}
	quota.Limit5hResetMode = resetModeOr(five5hMode)
	quota.DailyResetMode = resetModeOr(dailyMode)
	if dailyResetTime != nil {
		quota.DailyResetTime = *dailyResetTime
	}
	if concurrent != nil {
		quota.LimitConcurrentSessions = *concurrent
	}
	// rpm_limit 为 NULL 或 0 都是「不限速」（Node 侧 `user.rpm` 同样以 0 表示不限）。
	if rpm != nil {
		quota.RPM = *rpm
	}
	quota.CostResetAt = costResetAt
	quota.Limit5hCostResetAt = five5hCostResetAt
	return quota, nil
}

// floatOrNil 把 numeric 的文本形式解析成 *float64；NULL 与空串都是「未设限额」。
func floatOrNil(raw *string) (*float64, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	value, err := strconv.ParseFloat(*raw, 64)
	if err != nil {
		return nil, fmt.Errorf("limit: 限额字段不是合法数字 %q: %w", *raw, err)
	}
	return &value, nil
}

// resetModeOr 保留原值：空/NULL 由 limit 包按各维度的默认值补齐（5h 默认 rolling、每日默认 fixed），
// 这里擅自填默认会把「两处各有一套默认」的风险带进来。
func resetModeOr(raw *string) limit.ResetMode {
	if raw == nil {
		return ""
	}
	return limit.ResetMode(*raw)
}

// openRateLimiter 组装数据面限流服务；依赖不全时返回 nil。
//
// 返回 nil 是**有意的降级**：调用方会把它记进 `dataplane_ready.gaps`（本轮之前那正是生产上
// 「走 Go 的请求完全不检查配额」的可见痕迹），而不是把半可用的实现当成已接线。
func openRateLimiter(
	ctx context.Context,
	cfg config.Config,
	pools *store.Pools,
	redisClient redis.UniversalClient,
	logger *logx.Logger,
) guard.RateLimiter {
	if redisClient == nil {
		// 没有 Redis 就没有窗口计数与并发记账：注入一个必然 fail-open 的实现只会让人以为
		// 限流已生效。保持 nil，让 gap 在启动日志里继续可见。
		logger.Warn("ratelimit_unavailable", map[string]any{"reason": "redis_missing"})
		return nil
	}
	registry, err := ratelimit.Embedded()
	if err != nil {
		logger.Warn("ratelimit_unavailable", map[string]any{"reason": "registry", "error": err.Error()})
		return nil
	}
	scriptClient, err := ratelimit.New(redisClient, registry)
	if err != nil {
		logger.Warn("ratelimit_unavailable", map[string]any{"reason": "client", "error": err.Error()})
		return nil
	}

	// 自然窗口（每日 fixed / 周 / 月）的时区：与 Node 同一条取值链
	// `system_settings.timezone -> env TZ -> UTC`，非法候选逐级下降。
	location := cfg.ResolveLocation(readSystemTimezone(ctx, pools, logger))

	service, err := limit.New(limit.Config{
		Quotas: storeQuotas{pools: pools},
		// Redis 不可用或缓存未命中时回退到账本求和（Node 的 checkCostLimitsFromDatabase）。
		Ledger: pools,
		Redis:  scriptClient,
		// 租约设置（quota_lease_*）：非空且 Redis 可用时，周期限额改走租约判定
		// （对齐 Node rate-limit-guard 的 checkCostLimitsWithLease）。读不到设置即退回窗口/账本路径。
		LeaseSettings: limit.NewCachedQuotaLeaseSettings(storeQuotaLeaseSettings{pools: pools}, 30*time.Second),
		Location:      location,
		// SESSION_TTL 单位与 Node 一致（秒，可小数）。
		SessionTTL: time.Duration(cfg.Env.SessionTTL * float64(time.Second)),
		// 数据面的防爆破阈值（auth-guard.ts:58-63 的 proxyAuthPolicy）：20 次 / 5 分钟 / 封 10 分钟。
		Abuse:  limit.ProxyAuthAbuseConfig,
		Logger: logger,
	})
	if err != nil {
		logger.Warn("ratelimit_unavailable", map[string]any{"reason": "assemble", "error": err.Error()})
		return nil
	}
	logger.Info("ratelimit_ready", map[string]any{
		"location": location.String(),
		"abuse":    limit.ProxyAuthAbuseConfig,
	})
	return service
}

// readSystemTimezone 读 system_settings.timezone，供时区取值链使用；读不到时返回 nil（下一级兜底）。
//
// 为什么单独查一次：store 的 SystemSettings 结构没有 timezone 字段（JSON 解码会把它丢掉），
// 而改 store 超出本次改动范围。
// storeQuotaLeaseSettings 是 limit.QuotaLeaseSettingsReader 的 store 实现：读租约族的六个配置
// （`quota_db_refresh_interval_seconds` 与 `quota_lease_percent_*` / `quota_lease_cap_usd`）。
//
// 为什么单独查一次：store 已有的 `EnsureAdminSystemSettings` 会在缺行时**插入**，而这是读路径
// （每 30 秒至多一次），不该由读取行为建行；故与 readSystemTimezone 同法直读一行。
// 数值列一律 `::text` 后自行解析：本仓读取层不依赖 pgx 的 numeric 编解码器（NULL 与精度语义
// 在两条路径下不能分叉）。
type storeQuotaLeaseSettings struct {
	pools *store.Pools
}

const quotaLeaseSettingsSQL = `SELECT
  quota_db_refresh_interval_seconds::text,
  quota_lease_percent_5h::text,
  quota_lease_percent_daily::text,
  quota_lease_percent_weekly::text,
  quota_lease_percent_monthly::text,
  quota_lease_cap_usd::text
FROM system_settings
ORDER BY id ASC
LIMIT 1`

// QuotaLeaseSettings 实现 limit.QuotaLeaseSettingsReader。读不到行（空库/骨架库）不算失败：
// 返回零值，由 limit 侧按 Node 缺省值补齐（10 秒 / 5%），否则数据面会因为「设置表还没建行」
// 而整条退回账本路径。
func (s storeQuotaLeaseSettings) QuotaLeaseSettings(ctx context.Context) (limit.QuotaLeaseSettings, error) {
	if s.pools == nil {
		return limit.QuotaLeaseSettings{}, nil
	}
	pool, err := s.pools.Data()
	if err != nil {
		return limit.QuotaLeaseSettings{}, err
	}
	var (
		refresh *string
		p5h     *string
		pdaily  *string
		pweekly *string
		pmonth  *string
		capUSD  *string
	)
	if err := pool.QueryRow(ctx, quotaLeaseSettingsSQL).Scan(&refresh, &p5h, &pdaily, &pweekly, &pmonth, &capUSD); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return limit.QuotaLeaseSettings{}, nil
		}
		return limit.QuotaLeaseSettings{}, err
	}
	return limit.QuotaLeaseSettings{
		RefreshIntervalSeconds: intOr(parseIntOrZero(refresh), limit.DefaultQuotaLeaseRefreshSeconds),
		Percent5h:              parseFloatOr(p5h, 0),
		PercentDaily:           parseFloatOr(pdaily, 0),
		PercentWeekly:          parseFloatOr(pweekly, 0),
		PercentMonthly:         parseFloatOr(pmonth, 0),
		CapUSD:                 parseOptionalFloat(capUSD),
	}, nil
}

// parseIntOrZero 把可能为 NULL 的整数文本转成 int（NULL/非法为 0）。
func parseIntOrZero(raw *string) int {
	if raw == nil {
		return 0
	}
	value, err := strconv.Atoi(*raw)
	if err != nil {
		return 0
	}
	return value
}

// intOr 取 value；为 0 时用 fallback。
func intOr(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

// parseFloatOr 把可能为 NULL 的 numeric 文本转成 float64（NULL/非法为 fallback）。
func parseFloatOr(raw *string, fallback float64) float64 {
	if raw == nil {
		return fallback
	}
	value, err := strconv.ParseFloat(*raw, 64)
	if err != nil {
		return fallback
	}
	return value
}

// parseOptionalFloat 把可能为 NULL 的 numeric 文本转成可空 float64（NULL 表示不设上限）。
func parseOptionalFloat(raw *string) *float64 {
	if raw == nil {
		return nil
	}
	value, err := strconv.ParseFloat(*raw, 64)
	if err != nil {
		return nil
	}
	return &value
}

func readSystemTimezone(ctx context.Context, pools *store.Pools, logger *logx.Logger) *string {
	if pools == nil {
		return nil
	}
	pool, err := pools.Data()
	if err != nil {
		logger.Warn("ratelimit_timezone_unreadable", map[string]any{"error": err.Error()})
		return nil
	}
	var timezone *string
	if err := pool.QueryRow(ctx, `SELECT timezone FROM system_settings ORDER BY id ASC LIMIT 1`).Scan(&timezone); err != nil {
		// 没有 system_settings 行（空库/骨架库）不是错误：回退到 env TZ。
		if !errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("ratelimit_timezone_unreadable", map[string]any{"error": err.Error()})
		}
		return nil
	}
	return timezone
}
