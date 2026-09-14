package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
)

// 本文件是通知面（webhook_targets / notification_settings / notification_target_bindings）的
// 管理面读写面。
//
// 唯一真源：
//   - SQL 与映射：src/repository/webhook-targets.ts、src/repository/notifications.ts、
//     src/repository/notification-bindings.ts
//   - 列定义：src/drizzle/schema.ts:1095-1205
//
// 分道：读走 control，写走 writer（与其它管理面模块同法）。

// AdminWebhookTarget 是 webhook_targets 的一行。
//
// 字段名用 Node 的 camelCase（WebhookTargetSchema，src/lib/api/v1/schemas/webhook-targets.ts）：
// 本类型同时充当响应体的形状来源，故不能改成 Go 风格命名再映射。
//
// 可空时间列用 *string，可空 jsonb 列用 json.RawMessage（SQL NULL 时为 nil，序列化成 null）。
type AdminWebhookTarget struct {
	ID                    int64           `json:"id"`
	Name                  string          `json:"name"`
	ProviderType          string          `json:"providerType"`
	WebhookURL            *string         `json:"webhookUrl"`
	TelegramBotToken      *string         `json:"telegramBotToken"`
	TelegramChatID        *string         `json:"telegramChatId"`
	DingtalkSecret        *string         `json:"dingtalkSecret"`
	CustomTemplate        json.RawMessage `json:"customTemplate"`
	CustomHeaders         json.RawMessage `json:"customHeaders"`
	ProxyURL              *string         `json:"proxyUrl"`
	ProxyFallbackToDirect bool            `json:"proxyFallbackToDirect"`
	IsEnabled             bool            `json:"isEnabled"`
	LastTestAt            *string         `json:"lastTestAt"`
	LastTestResult        json.RawMessage `json:"lastTestResult"`
	CreatedAt             *string         `json:"createdAt"`
	UpdatedAt             *string         `json:"updatedAt"`
}

// webhookTargetColumns 与 getAllWebhookTargets 的投影一致。
//
// proxyFallbackToDirect / isEnabled 在 Node 侧是可空列 + `?? false` / `?? true`，故这里 COALESCE
// 成同样的默认值——响应体里这两个字段是必填布尔。
const webhookTargetColumns = `id, name, provider_type AS "providerType",
	webhook_url AS "webhookUrl", telegram_bot_token AS "telegramBotToken",
	telegram_chat_id AS "telegramChatId", dingtalk_secret AS "dingtalkSecret",
	custom_template AS "customTemplate", custom_headers AS "customHeaders",
	proxy_url AS "proxyUrl", COALESCE(proxy_fallback_to_direct, false) AS "proxyFallbackToDirect",
	COALESCE(is_enabled, true) AS "isEnabled",
	to_char(last_test_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "lastTestAt",
	last_test_result AS "lastTestResult",
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt",
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "updatedAt"`

// AdminListWebhookTargets 复刻 getAllWebhookTargets：按 id 倒序（`desc(id)`），含禁用行。
func (p *Pools) AdminListWebhookTargets(ctx context.Context) ([]AdminWebhookTarget, error) {
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + webhookTargetColumns +
		` FROM webhook_targets ORDER BY id DESC) t`
	return webhookTargetRows(ctx, p, query)
}

// AdminGetWebhookTarget 复刻 getWebhookTargetById；不存在返回 ErrNotFound。
func (p *Pools) AdminGetWebhookTarget(ctx context.Context, id int64) (AdminWebhookTarget, error) {
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + webhookTargetColumns +
		` FROM webhook_targets WHERE id = $1) t`
	return webhookTargetRowFromJSON(ctx, p, query, id)
}

// AdminCreateWebhookTargetInput 是插入 webhook_targets 的一行新值。
//
// 语义与 repository/webhook-targets.ts 的 createWebhookTarget 一致：可空列传 nil 即写 SQL NULL，
// jsonb 列传 nil 即写 SQL NULL（不是 JSON 的 null 字面量，见 nullableJSON 的说明）。
type AdminCreateWebhookTargetInput struct {
	Name                  string
	ProviderType          string
	WebhookURL            *string
	TelegramBotToken      *string
	TelegramChatID        *string
	DingtalkSecret        *string
	CustomTemplate        json.RawMessage
	CustomHeaders         json.RawMessage
	ProxyURL              *string
	ProxyFallbackToDirect bool
	IsEnabled             bool
}

// AdminCreateWebhookTarget 复刻 createWebhookTarget。
//
// created_at 不在 INSERT 列表里：Node 侧只写 updated_at，created_at 取列默认（defaultNow）。
func (p *Pools) AdminCreateWebhookTarget(
	ctx context.Context,
	input AdminCreateWebhookTargetInput,
) (AdminWebhookTarget, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminWebhookTarget{}, err
	}
	query := `INSERT INTO webhook_targets
		(name, provider_type, webhook_url, telegram_bot_token, telegram_chat_id, dingtalk_secret,
		 custom_template, custom_headers, proxy_url, proxy_fallback_to_direct, is_enabled, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9, $10, $11, now())
		RETURNING ` + webhookTargetColumns
	return scanWebhookTargetRow(pool.QueryRow(ctx, query,
		input.Name, input.ProviderType, input.WebhookURL, input.TelegramBotToken,
		input.TelegramChatID, input.DingtalkSecret, nullableJSON(input.CustomTemplate),
		nullableJSON(input.CustomHeaders), input.ProxyURL, input.ProxyFallbackToDirect,
		input.IsEnabled))
}

// AdminWebhookTargetUpdate 是一次部分更新：nil 字段不参与 SET（复刻 Node 的
// `data.x !== undefined` 逐个展开）。
type AdminWebhookTargetUpdate struct {
	Name                  *string
	ProviderType          *string
	WebhookURL            AdminNullableText
	TelegramBotToken      AdminNullableText
	TelegramChatID        AdminNullableText
	DingtalkSecret        AdminNullableText
	CustomTemplate        AdminNullableJSON
	CustomHeaders         AdminNullableJSON
	ProxyURL              AdminNullableText
	ProxyFallbackToDirect *bool
	IsEnabled             *bool
}

// AdminUpdateWebhookTarget 复刻 updateWebhookTarget：部分更新 + updated_at 置当前时间。
//
// 注意与 Node 的一处形状差异：Node 的 action 层做「归一化」后是把**整行**写回（normalize 后的
// 每个字段都会出现在 .set 里），Go 侧同样由调用方（adminapi）传入归一化后的全量字段，
// 故这里保留「出现即写」的部分更新语义只为表达可空列的三态。
func (p *Pools) AdminUpdateWebhookTarget(
	ctx context.Context,
	id int64,
	update AdminWebhookTargetUpdate,
) (AdminWebhookTarget, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminWebhookTarget{}, err
	}
	assignments := make([]string, 0, 12)
	args := make([]any, 0, 12)
	set := func(column string, value any, jsonValue bool) {
		args = append(args, value)
		placeholder := "$" + strconv.Itoa(len(args))
		if jsonValue {
			placeholder += "::jsonb"
		}
		assignments = append(assignments, column+" = "+placeholder)
	}
	if update.Name != nil {
		set("name", *update.Name, false)
	}
	if update.ProviderType != nil {
		set("provider_type", *update.ProviderType, false)
	}
	if update.WebhookURL.Present {
		set("webhook_url", update.WebhookURL.Value, false)
	}
	if update.TelegramBotToken.Present {
		set("telegram_bot_token", update.TelegramBotToken.Value, false)
	}
	if update.TelegramChatID.Present {
		set("telegram_chat_id", update.TelegramChatID.Value, false)
	}
	if update.DingtalkSecret.Present {
		set("dingtalk_secret", update.DingtalkSecret.Value, false)
	}
	if update.CustomTemplate.Present {
		set("custom_template", nullableJSON(update.CustomTemplate.Raw), true)
	}
	if update.CustomHeaders.Present {
		set("custom_headers", nullableJSON(update.CustomHeaders.Raw), true)
	}
	if update.ProxyURL.Present {
		set("proxy_url", update.ProxyURL.Value, false)
	}
	if update.ProxyFallbackToDirect != nil {
		set("proxy_fallback_to_direct", *update.ProxyFallbackToDirect, false)
	}
	if update.IsEnabled != nil {
		set("is_enabled", *update.IsEnabled, false)
	}
	// updatedAt 恒被写入（Node 侧也如此），故 SET 列表永不为空。
	assignments = append(assignments, "updated_at = now()")
	args = append(args, id)

	query := fmt.Sprintf("UPDATE webhook_targets SET %s WHERE id = $%d RETURNING %s",
		strings.Join(assignments, ", "), len(args), webhookTargetColumns)
	return scanWebhookTargetRow(pool.QueryRow(ctx, query, args...))
}

// AdminDeleteWebhookTarget 复刻 deleteWebhookTarget：硬删除，返回是否删掉了行。
func (p *Pools) AdminDeleteWebhookTarget(ctx context.Context, id int64) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	tag, err := pool.Exec(ctx, "DELETE FROM webhook_targets WHERE id = $1", id)
	if err != nil {
		return false, fmt.Errorf("store: 删除推送目标失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AdminUpdateWebhookTargetTestResult 复刻 updateTestResult：写 last_test_at / last_test_result，
// 并同步推进 updated_at。
func (p *Pools) AdminUpdateWebhookTargetTestResult(
	ctx context.Context,
	id int64,
	result json.RawMessage,
) error {
	pool, err := p.Writer()
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `UPDATE webhook_targets
		SET last_test_at = now(), last_test_result = $2::jsonb, updated_at = now()
		WHERE id = $1`, id, string(result))
	if err != nil {
		return fmt.Errorf("store: 写推送目标测试结果失败: %w", err)
	}
	return nil
}

// webhookTargetRows 用 control 分道读多行（row_to_json 文本行）。
func webhookTargetRows(
	ctx context.Context,
	p *Pools,
	query string,
	args ...any,
) ([]AdminWebhookTarget, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询推送目标失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminWebhookTarget, 0, 8)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取推送目标行失败: %w", err)
		}
		target, err := decodeWebhookTarget(payload)
		if err != nil {
			return nil, err
		}
		results = append(results, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历推送目标行失败: %w", err)
	}
	return results, nil
}

// webhookTargetRowFromJSON 读单行（row_to_json 文本行）并区分「不存在」。
func webhookTargetRowFromJSON(
	ctx context.Context,
	p *Pools,
	query string,
	args ...any,
) (AdminWebhookTarget, error) {
	pool, err := p.Control()
	if err != nil {
		return AdminWebhookTarget{}, err
	}
	var payload *string
	if err := pool.QueryRow(ctx, query, args...).Scan(&payload); err != nil {
		if isNoRows(err) {
			return AdminWebhookTarget{}, ErrNotFound
		}
		return AdminWebhookTarget{}, fmt.Errorf("store: 查询推送目标失败: %w", err)
	}
	if payload == nil {
		// 子查询无行时 row_to_json 返回 NULL 而不是无行。
		return AdminWebhookTarget{}, ErrNotFound
	}
	return decodeWebhookTarget(*payload)
}

// scanWebhookTargetRow 按 webhookTargetColumns 的列顺序扫描 RETURNING 的单行。
//
// RETURNING 给的是列（jsonb 列以文本形式回来），故这里逐列扫，而不是走 row_to_json。
func scanWebhookTargetRow(row pgx.Row) (AdminWebhookTarget, error) {
	var target AdminWebhookTarget
	var customTemplate, customHeaders, lastTestResult []byte
	if err := row.Scan(&target.ID, &target.Name, &target.ProviderType, &target.WebhookURL,
		&target.TelegramBotToken, &target.TelegramChatID, &target.DingtalkSecret,
		&customTemplate, &customHeaders, &target.ProxyURL, &target.ProxyFallbackToDirect,
		&target.IsEnabled, &target.LastTestAt, &lastTestResult, &target.CreatedAt,
		&target.UpdatedAt); err != nil {
		if isNoRows(err) {
			return AdminWebhookTarget{}, ErrNotFound
		}
		return AdminWebhookTarget{}, fmt.Errorf("store: 写推送目标失败: %w", err)
	}
	target.CustomTemplate = json.RawMessage(customTemplate)
	target.CustomHeaders = json.RawMessage(customHeaders)
	target.LastTestResult = json.RawMessage(lastTestResult)
	return target, nil
}

// decodeWebhookTarget 反序列化一行；标签与 AdminWebhookTarget 的 json 标签一致。
func decodeWebhookTarget(payload string) (AdminWebhookTarget, error) {
	var target AdminWebhookTarget
	if err := json.Unmarshal([]byte(payload), &target); err != nil {
		return AdminWebhookTarget{}, fmt.Errorf("store: 推送目标行反序列化失败: %w", err)
	}
	return target, nil
}

// AdminNotificationSettings 是 notification_settings 的单行配置。
//
// 字段与 NotificationSettingsSchema 逐字对应；数值列在 PG 里是 numeric，按 Node 侧的类型
// （`string | null`）原样透传字符串。
type AdminNotificationSettings struct {
	ID                                      int64   `json:"id"`
	Enabled                                 bool    `json:"enabled"`
	UseLegacyMode                           bool    `json:"useLegacyMode"`
	CircuitBreakerEnabled                   bool    `json:"circuitBreakerEnabled"`
	CircuitBreakerWebhook                   *string `json:"circuitBreakerWebhook"`
	DailyLeaderboardEnabled                 bool    `json:"dailyLeaderboardEnabled"`
	DailyLeaderboardWebhook                 *string `json:"dailyLeaderboardWebhook"`
	DailyLeaderboardTime                    *string `json:"dailyLeaderboardTime"`
	DailyLeaderboardTopN                    *int    `json:"dailyLeaderboardTopN"`
	CostAlertEnabled                        bool    `json:"costAlertEnabled"`
	CostAlertWebhook                        *string `json:"costAlertWebhook"`
	CostAlertThreshold                      *string `json:"costAlertThreshold"`
	CostAlertCheckInterval                  *int    `json:"costAlertCheckInterval"`
	CacheHitRateAlertEnabled                bool    `json:"cacheHitRateAlertEnabled"`
	CacheHitRateAlertWebhook                *string `json:"cacheHitRateAlertWebhook"`
	CacheHitRateAlertWindowMode             *string `json:"cacheHitRateAlertWindowMode"`
	CacheHitRateAlertCheckInterval          *int    `json:"cacheHitRateAlertCheckInterval"`
	CacheHitRateAlertHistoricalLookbackDays *int    `json:"cacheHitRateAlertHistoricalLookbackDays"`
	CacheHitRateAlertMinEligibleRequests    *int    `json:"cacheHitRateAlertMinEligibleRequests"`
	CacheHitRateAlertMinEligibleTokens      *int    `json:"cacheHitRateAlertMinEligibleTokens"`
	CacheHitRateAlertAbsMin                 *string `json:"cacheHitRateAlertAbsMin"`
	CacheHitRateAlertDropRel                *string `json:"cacheHitRateAlertDropRel"`
	CacheHitRateAlertDropAbs                *string `json:"cacheHitRateAlertDropAbs"`
	CacheHitRateAlertCooldownMinutes        *int    `json:"cacheHitRateAlertCooldownMinutes"`
	CacheHitRateAlertTopN                   *int    `json:"cacheHitRateAlertTopN"`
	CreatedAt                               *string `json:"createdAt"`
	UpdatedAt                               *string `json:"updatedAt"`
}

// notificationSettingsColumns 是单行配置的投影。
//
// numeric 列显式 ::text：Node 侧读到的是 pg 驱动给的字符串（drizzle numeric 映射为 string），
// 直接扫进 *string 也可以，但显式转型能挡住驱动版本差异。
const notificationSettingsColumns = `id, enabled, use_legacy_mode AS "useLegacyMode",
	circuit_breaker_enabled AS "circuitBreakerEnabled",
	circuit_breaker_webhook AS "circuitBreakerWebhook",
	daily_leaderboard_enabled AS "dailyLeaderboardEnabled",
	daily_leaderboard_webhook AS "dailyLeaderboardWebhook",
	daily_leaderboard_time AS "dailyLeaderboardTime",
	daily_leaderboard_top_n AS "dailyLeaderboardTopN",
	cost_alert_enabled AS "costAlertEnabled", cost_alert_webhook AS "costAlertWebhook",
	cost_alert_threshold::text AS "costAlertThreshold",
	cost_alert_check_interval AS "costAlertCheckInterval",
	cache_hit_rate_alert_enabled AS "cacheHitRateAlertEnabled",
	cache_hit_rate_alert_webhook AS "cacheHitRateAlertWebhook",
	COALESCE(cache_hit_rate_alert_window_mode, 'auto') AS "cacheHitRateAlertWindowMode",
	cache_hit_rate_alert_check_interval AS "cacheHitRateAlertCheckInterval",
	cache_hit_rate_alert_historical_lookback_days AS "cacheHitRateAlertHistoricalLookbackDays",
	cache_hit_rate_alert_min_eligible_requests AS "cacheHitRateAlertMinEligibleRequests",
	cache_hit_rate_alert_min_eligible_tokens AS "cacheHitRateAlertMinEligibleTokens",
	cache_hit_rate_alert_abs_min::text AS "cacheHitRateAlertAbsMin",
	cache_hit_rate_alert_drop_rel::text AS "cacheHitRateAlertDropRel",
	cache_hit_rate_alert_drop_abs::text AS "cacheHitRateAlertDropAbs",
	cache_hit_rate_alert_cooldown_minutes AS "cacheHitRateAlertCooldownMinutes",
	cache_hit_rate_alert_top_n AS "cacheHitRateAlertTopN",
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt",
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "updatedAt"`

// AdminNotificationSettingsFallback 复刻 createFallbackSettings：表或列缺失时的默认配置。
//
// 为什么要有这条路径：Node 在表缺失（迁移未跑）时返回默认配置而不是 500，管理页因此仍可用。
// id 为 0 是 Node 的显式约定（不是真实行）。
func AdminNotificationSettingsFallback() AdminNotificationSettings {
	return AdminNotificationSettings{
		Enabled:                                 false,
		UseLegacyMode:                           false,
		DailyLeaderboardTime:                    adminStringPtr("09:00"),
		DailyLeaderboardTopN:                    adminIntPtr(5),
		CostAlertThreshold:                      adminStringPtr("0.80"),
		CostAlertCheckInterval:                  adminIntPtr(60),
		CacheHitRateAlertWindowMode:             adminStringPtr("auto"),
		CacheHitRateAlertCheckInterval:          adminIntPtr(5),
		CacheHitRateAlertHistoricalLookbackDays: adminIntPtr(7),
		CacheHitRateAlertMinEligibleRequests:    adminIntPtr(20),
		CacheHitRateAlertMinEligibleTokens:      adminIntPtr(0),
		CacheHitRateAlertAbsMin:                 adminStringPtr("0.05"),
		CacheHitRateAlertDropRel:                adminStringPtr("0.3"),
		CacheHitRateAlertDropAbs:                adminStringPtr("0.1"),
		CacheHitRateAlertCooldownMinutes:        adminIntPtr(30),
		CacheHitRateAlertTopN:                   adminIntPtr(10),
	}
}

// AdminGetNotificationSettings 复刻 getNotificationSettings：读单行，空表则插默认行再读回。
//
// 表/列缺失（迁移未跑）时返回 AdminNotificationSettingsFallback，与 Node 的 warn-并-降级一致。
func (p *Pools) AdminGetNotificationSettings(
	ctx context.Context,
) (AdminNotificationSettings, error) {
	settings, err := p.adminReadNotificationSettings(ctx)
	if err == nil {
		return settings, nil
	}
	if err != ErrNotFound {
		if isMissingSchema(err) {
			return AdminNotificationSettingsFallback(), nil
		}
		return AdminNotificationSettings{}, err
	}

	inserted, err := p.adminInsertDefaultNotificationSettings(ctx)
	if err == nil {
		return inserted, nil
	}
	if isMissingSchema(err) {
		return AdminNotificationSettingsFallback(), nil
	}
	// 并发插入时可能读到另一请求写下的行；Node 同样在此刻重查一次。
	return p.adminReadNotificationSettings(ctx)
}

// adminReadNotificationSettings 读单行（limit 1，不带 order by，与 Node 的 select().limit(1) 一致）。
func (p *Pools) adminReadNotificationSettings(
	ctx context.Context,
) (AdminNotificationSettings, error) {
	pool, err := p.Control()
	if err != nil {
		return AdminNotificationSettings{}, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + notificationSettingsColumns +
		` FROM notification_settings LIMIT 1) t`
	var payload *string
	if err := pool.QueryRow(ctx, query).Scan(&payload); err != nil {
		if isNoRows(err) {
			return AdminNotificationSettings{}, ErrNotFound
		}
		return AdminNotificationSettings{}, fmt.Errorf("store: 查询通知设置失败: %w", err)
	}
	if payload == nil {
		return AdminNotificationSettings{}, ErrNotFound
	}
	var settings AdminNotificationSettings
	if err := json.Unmarshal([]byte(*payload), &settings); err != nil {
		return AdminNotificationSettings{}, fmt.Errorf("store: 通知设置行反序列化失败: %w", err)
	}
	return settings, nil
}

// adminInsertDefaultNotificationSettings 复刻 getNotificationSettings 的默认行插入。
func (p *Pools) adminInsertDefaultNotificationSettings(
	ctx context.Context,
) (AdminNotificationSettings, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminNotificationSettings{}, err
	}
	query := `INSERT INTO notification_settings
		(enabled, circuit_breaker_enabled, daily_leaderboard_enabled, daily_leaderboard_time,
		 daily_leaderboard_top_n, cost_alert_enabled, cost_alert_threshold, cost_alert_check_interval,
		 cache_hit_rate_alert_enabled, cache_hit_rate_alert_window_mode,
		 cache_hit_rate_alert_check_interval, cache_hit_rate_alert_historical_lookback_days,
		 cache_hit_rate_alert_min_eligible_requests, cache_hit_rate_alert_min_eligible_tokens,
		 cache_hit_rate_alert_abs_min, cache_hit_rate_alert_drop_rel, cache_hit_rate_alert_drop_abs,
		 cache_hit_rate_alert_cooldown_minutes, cache_hit_rate_alert_top_n)
		VALUES (false, false, false, '09:00', 5, false, '0.80', 60,
			false, 'auto', 5, 7, 20, 0, '0.05', '0.3', '0.1', 30, 10)
		RETURNING ` + notificationSettingsColumns
	var settings AdminNotificationSettings
	row := pool.QueryRow(ctx, query)
	if err := scanNotificationSettings(row, &settings); err != nil {
		return AdminNotificationSettings{}, err
	}
	return settings, nil
}

// AdminNotificationSettingsUpdate 是一次部分更新（复刻 NotificationSettingsUpdateSchema 的
// `.partial()`）：未出现的键不参与 SET。
//
// 可空列用 AdminNullableText/AdminNullableInt 表达「写 NULL」与「不写」的区别；布尔与整数
// 只有「写」与「不写」两态，用指针。
type AdminNotificationSettingsUpdate struct {
	Enabled                                 *bool
	UseLegacyMode                           *bool
	CircuitBreakerEnabled                   *bool
	CircuitBreakerWebhook                   AdminNullableText
	DailyLeaderboardEnabled                 *bool
	DailyLeaderboardWebhook                 AdminNullableText
	DailyLeaderboardTime                    AdminNullableText
	DailyLeaderboardTopN                    AdminNullableInt
	CostAlertEnabled                        *bool
	CostAlertWebhook                        AdminNullableText
	CostAlertThreshold                      AdminNullableText
	CostAlertCheckInterval                  AdminNullableInt
	CacheHitRateAlertEnabled                *bool
	CacheHitRateAlertWebhook                AdminNullableText
	CacheHitRateAlertWindowMode             AdminNullableText
	CacheHitRateAlertCheckInterval          AdminNullableInt
	CacheHitRateAlertHistoricalLookbackDays AdminNullableInt
	CacheHitRateAlertMinEligibleRequests    AdminNullableInt
	CacheHitRateAlertMinEligibleTokens      AdminNullableInt
	CacheHitRateAlertAbsMin                 AdminNullableText
	CacheHitRateAlertDropRel                AdminNullableText
	CacheHitRateAlertDropAbs                AdminNullableText
	CacheHitRateAlertCooldownMinutes        AdminNullableInt
	CacheHitRateAlertTopN                   AdminNullableInt
}

// AdminUpdateNotificationSettings 复刻 updateNotificationSettings：按行 id 部分更新 + updated_at。
func (p *Pools) AdminUpdateNotificationSettings(
	ctx context.Context,
	id int64,
	update AdminNotificationSettingsUpdate,
) (AdminNotificationSettings, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminNotificationSettings{}, err
	}
	assignments := make([]string, 0, 24)
	args := make([]any, 0, 24)
	set := func(column string, value any) {
		args = append(args, value)
		assignments = append(assignments, column+" = $"+strconv.Itoa(len(args)))
	}
	if update.Enabled != nil {
		set("enabled", *update.Enabled)
	}
	if update.UseLegacyMode != nil {
		set("use_legacy_mode", *update.UseLegacyMode)
	}
	if update.CircuitBreakerEnabled != nil {
		set("circuit_breaker_enabled", *update.CircuitBreakerEnabled)
	}
	if update.CircuitBreakerWebhook.Present {
		set("circuit_breaker_webhook", update.CircuitBreakerWebhook.Value)
	}
	if update.DailyLeaderboardEnabled != nil {
		set("daily_leaderboard_enabled", *update.DailyLeaderboardEnabled)
	}
	if update.DailyLeaderboardWebhook.Present {
		set("daily_leaderboard_webhook", update.DailyLeaderboardWebhook.Value)
	}
	if update.DailyLeaderboardTime.Present {
		set("daily_leaderboard_time", update.DailyLeaderboardTime.Value)
	}
	if update.DailyLeaderboardTopN.Present {
		set("daily_leaderboard_top_n", update.DailyLeaderboardTopN.Value)
	}
	if update.CostAlertEnabled != nil {
		set("cost_alert_enabled", *update.CostAlertEnabled)
	}
	if update.CostAlertWebhook.Present {
		set("cost_alert_webhook", update.CostAlertWebhook.Value)
	}
	if update.CostAlertThreshold.Present {
		set("cost_alert_threshold", update.CostAlertThreshold.Value)
	}
	if update.CostAlertCheckInterval.Present {
		set("cost_alert_check_interval", update.CostAlertCheckInterval.Value)
	}
	if update.CacheHitRateAlertEnabled != nil {
		set("cache_hit_rate_alert_enabled", *update.CacheHitRateAlertEnabled)
	}
	if update.CacheHitRateAlertWebhook.Present {
		set("cache_hit_rate_alert_webhook", update.CacheHitRateAlertWebhook.Value)
	}
	if update.CacheHitRateAlertWindowMode.Present {
		set("cache_hit_rate_alert_window_mode", update.CacheHitRateAlertWindowMode.Value)
	}
	if update.CacheHitRateAlertCheckInterval.Present {
		set("cache_hit_rate_alert_check_interval", update.CacheHitRateAlertCheckInterval.Value)
	}
	if update.CacheHitRateAlertHistoricalLookbackDays.Present {
		set("cache_hit_rate_alert_historical_lookback_days",
			update.CacheHitRateAlertHistoricalLookbackDays.Value)
	}
	if update.CacheHitRateAlertMinEligibleRequests.Present {
		set("cache_hit_rate_alert_min_eligible_requests",
			update.CacheHitRateAlertMinEligibleRequests.Value)
	}
	if update.CacheHitRateAlertMinEligibleTokens.Present {
		set("cache_hit_rate_alert_min_eligible_tokens",
			update.CacheHitRateAlertMinEligibleTokens.Value)
	}
	if update.CacheHitRateAlertAbsMin.Present {
		set("cache_hit_rate_alert_abs_min", update.CacheHitRateAlertAbsMin.Value)
	}
	if update.CacheHitRateAlertDropRel.Present {
		set("cache_hit_rate_alert_drop_rel", update.CacheHitRateAlertDropRel.Value)
	}
	if update.CacheHitRateAlertDropAbs.Present {
		set("cache_hit_rate_alert_drop_abs", update.CacheHitRateAlertDropAbs.Value)
	}
	if update.CacheHitRateAlertCooldownMinutes.Present {
		set("cache_hit_rate_alert_cooldown_minutes", update.CacheHitRateAlertCooldownMinutes.Value)
	}
	if update.CacheHitRateAlertTopN.Present {
		set("cache_hit_rate_alert_top_n", update.CacheHitRateAlertTopN.Value)
	}
	assignments = append(assignments, "updated_at = now()")
	args = append(args, id)

	query := fmt.Sprintf("UPDATE notification_settings SET %s WHERE id = $%d RETURNING %s",
		strings.Join(assignments, ", "), len(args), notificationSettingsColumns)
	var settings AdminNotificationSettings
	if err := scanNotificationSettings(pool.QueryRow(ctx, query, args...), &settings); err != nil {
		return AdminNotificationSettings{}, err
	}
	return settings, nil
}

// scanNotificationSettings 按 notificationSettingsColumns 的列顺序扫描单行。
func scanNotificationSettings(row pgx.Row, out *AdminNotificationSettings) error {
	if err := row.Scan(&out.ID, &out.Enabled, &out.UseLegacyMode, &out.CircuitBreakerEnabled,
		&out.CircuitBreakerWebhook, &out.DailyLeaderboardEnabled, &out.DailyLeaderboardWebhook,
		&out.DailyLeaderboardTime, &out.DailyLeaderboardTopN, &out.CostAlertEnabled,
		&out.CostAlertWebhook, &out.CostAlertThreshold, &out.CostAlertCheckInterval,
		&out.CacheHitRateAlertEnabled, &out.CacheHitRateAlertWebhook,
		&out.CacheHitRateAlertWindowMode, &out.CacheHitRateAlertCheckInterval,
		&out.CacheHitRateAlertHistoricalLookbackDays, &out.CacheHitRateAlertMinEligibleRequests,
		&out.CacheHitRateAlertMinEligibleTokens, &out.CacheHitRateAlertAbsMin,
		&out.CacheHitRateAlertDropRel, &out.CacheHitRateAlertDropAbs,
		&out.CacheHitRateAlertCooldownMinutes, &out.CacheHitRateAlertTopN,
		&out.CreatedAt, &out.UpdatedAt); err != nil {
		if isNoRows(err) {
			return ErrNotFound
		}
		return fmt.Errorf("store: 写通知设置失败: %w", err)
	}
	return nil
}

// AdminNotificationBinding 是一条通知类型到推送目标的绑定（含目标快照）。
//
// 形状对应 NotificationBindingSchema：target 是**未脱敏**的完整目标，脱敏在 adminapi 层做
// （与 Node 的 sanitizeBinding 同层）。
type AdminNotificationBinding struct {
	ID               int64              `json:"id"`
	NotificationType string             `json:"notificationType"`
	TargetID         int64              `json:"targetId"`
	IsEnabled        bool               `json:"isEnabled"`
	ScheduleCron     *string            `json:"scheduleCron"`
	ScheduleTimezone *string            `json:"scheduleTimezone"`
	TemplateOverride json.RawMessage    `json:"templateOverride"`
	CreatedAt        *string            `json:"createdAt"`
	Target           AdminWebhookTarget `json:"target"`
}

// AdminListNotificationBindings 复刻 getBindingsByType：按类型取绑定并内联目标，id 倒序。
func (p *Pools) AdminListNotificationBindings(
	ctx context.Context,
	notificationType string,
) ([]AdminNotificationBinding, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (
		SELECT b.id,
			b.notification_type AS "notificationType",
			b.target_id AS "targetId",
			COALESCE(b.is_enabled, true) AS "isEnabled",
			b.schedule_cron AS "scheduleCron",
			b.schedule_timezone AS "scheduleTimezone",
			b.template_override AS "templateOverride",
			to_char(b.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt",
			json_build_object(
				'id', w.id,
				'name', w.name,
				'providerType', w.provider_type,
				'webhookUrl', w.webhook_url,
				'telegramBotToken', w.telegram_bot_token,
				'telegramChatId', w.telegram_chat_id,
				'dingtalkSecret', w.dingtalk_secret,
				'customTemplate', w.custom_template,
				'customHeaders', w.custom_headers,
				'proxyUrl', w.proxy_url,
				'proxyFallbackToDirect', COALESCE(w.proxy_fallback_to_direct, false),
				'isEnabled', COALESCE(w.is_enabled, true),
				'lastTestAt', to_char(w.last_test_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
				'lastTestResult', w.last_test_result,
				'createdAt', to_char(w.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
				'updatedAt', to_char(w.updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
			) AS "target"
		FROM notification_target_bindings b
		JOIN webhook_targets w ON w.id = b.target_id
		WHERE b.notification_type = $1
		ORDER BY b.id DESC) t`
	rows, err := pool.Query(ctx, query, notificationType)
	if err != nil {
		return nil, fmt.Errorf("store: 查询通知绑定失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminNotificationBinding, 0, 4)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取通知绑定行失败: %w", err)
		}
		var binding AdminNotificationBinding
		if err := json.Unmarshal([]byte(payload), &binding); err != nil {
			return nil, fmt.Errorf("store: 通知绑定行反序列化失败: %w", err)
		}
		results = append(results, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历通知绑定行失败: %w", err)
	}
	return results, nil
}

// AdminNotificationBindingInput 是一次替换里的单条绑定（已在 adminapi 层归一化）。
type AdminNotificationBindingInput struct {
	TargetID         int64
	IsEnabled        bool
	ScheduleCron     *string
	ScheduleTimezone *string
	TemplateOverride json.RawMessage
}

// AdminReplaceNotificationBindings 复刻 upsertBindings：同一事务里「删掉不在列表里的绑定 + upsert
// 列表里的绑定」。
//
// 归一化（过滤非法 targetId、补默认时区）在 adminapi 层完成，存储层只负责这两步 SQL。
func (p *Pools) AdminReplaceNotificationBindings(
	ctx context.Context,
	notificationType string,
	items []AdminNotificationBindingInput,
) error {
	pool, err := p.Writer()
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开始通知绑定事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	targetIDs := make([]int64, 0, len(items))
	for _, item := range items {
		targetIDs = append(targetIDs, item.TargetID)
	}
	if len(targetIDs) == 0 {
		if _, err := tx.Exec(ctx,
			"DELETE FROM notification_target_bindings WHERE notification_type = $1",
			notificationType); err != nil {
			return fmt.Errorf("store: 清空通知绑定失败: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `DELETE FROM notification_target_bindings
			WHERE notification_type = $1 AND target_id <> ALL($2::int[])`,
			notificationType, targetIDs); err != nil {
			return fmt.Errorf("store: 清理通知绑定失败: %w", err)
		}
	}

	for _, item := range items {
		if _, err := tx.Exec(ctx, `INSERT INTO notification_target_bindings
			(notification_type, target_id, is_enabled, schedule_cron, schedule_timezone, template_override)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb)
			ON CONFLICT (notification_type, target_id) DO UPDATE SET
				is_enabled = EXCLUDED.is_enabled,
				schedule_cron = EXCLUDED.schedule_cron,
				schedule_timezone = EXCLUDED.schedule_timezone,
				template_override = EXCLUDED.template_override`,
			notificationType, item.TargetID, item.IsEnabled, item.ScheduleCron,
			item.ScheduleTimezone, nullableJSON(item.TemplateOverride)); err != nil {
			return fmt.Errorf("store: 写入通知绑定失败: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: 提交通知绑定事务失败: %w", err)
	}
	return nil
}

// isMissingSchema 判断错误是否为「表/列缺失」（PG 42P01 undefined_table / 42703 undefined_column）。
//
// 用途与 Node 的 isTableMissingError / isColumnMissingError 相同：迁移未跑时降级返回默认配置，
// 而不是把 500 抛给管理页。
func isMissingSchema(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "42p01") || strings.Contains(message, "42703") ||
		strings.Contains(message, "does not exist")
}

func adminStringPtr(value string) *string { return &value }

func adminIntPtr(value int) *int { return &value }

// AdminSystemTimezoneOrUTC 复刻 resolveSystemTimezone 的降级链：
// DB -> 环境变量 TZ（未设置取 `Asia/Shanghai`）-> UTC。
//
// 只在候选值是合法 IANA 名字时采用（与 Node 的 isValidIANATimezone 同义）；读库失败不报错，
// 继续往下退，因为它的调用点（绑定默认时区）不该因读不到设置而失败。
// 默认值与校验集中在 config：过去这里自读 os.Getenv("TZ")，未设置时得空串而直接落 UTC。
func (p *Pools) AdminSystemTimezoneOrUTC(ctx context.Context) string {
	if name, err := p.AdminSystemTimezone(ctx); err == nil && name != nil && isIANAName(*name) {
		return *name
	}
	return config.ResolveLocationFromEnv(nil).String()
}

// isIANAName 判断字符串是否是运行时可加载的 IANA 时区名。
func isIANAName(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}
