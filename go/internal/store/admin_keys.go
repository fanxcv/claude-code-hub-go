package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件是管理面 keys 资源（A1-2）的 SQL 层。读取沿用本包既定风格：只读路径走
// row_to_json（列名即 JSON 键名，见 read.go 文件头），写入路径用显式列名。
//
// 与数据面读取（read.go 的 FindKeyByValue）的区别：那条查询只取鉴权需要的列并且**不过滤
// deleted_at**（软删的密钥要能被鉴权路径认出来并拒绝）；管理面这几条必须过滤 deleted_at，
// 与 Node 的 findKeyById / findKeyList（src/repository/key.ts:22,55）一致。

// AdminKeyRecord 是 keys 表的管理面视图，只覆盖密钥管理实际消费的列。
//
// 金额列用 json.Number：numeric 在 row_to_json 里既可能是 JSON number 也可能是字符串，
// json.Number 两者都收（同 read.go 的说明）。需要十进制文本时用 .String()，避免 float64
// 把 numeric(10,2) 的值改写掉。
type AdminKeyRecord struct {
	ID                      int64       `json:"id"`
	UserID                  int64       `json:"user_id"`
	Key                     string      `json:"key"`
	Name                    string      `json:"name"`
	IsEnabled               *bool       `json:"is_enabled"`
	ExpiresAt               *time.Time  `json:"expires_at"`
	CanLoginWebUI           *bool       `json:"can_login_web_ui"`
	Limit5hUSD              json.Number `json:"limit_5h_usd"`
	Limit5hResetMode        *string     `json:"limit_5h_reset_mode"`
	LimitDailyUSD           json.Number `json:"limit_daily_usd"`
	DailyResetMode          *string     `json:"daily_reset_mode"`
	DailyResetTime          *string     `json:"daily_reset_time"`
	LimitWeeklyUSD          json.Number `json:"limit_weekly_usd"`
	LimitMonthlyUSD         json.Number `json:"limit_monthly_usd"`
	LimitTotalUSD           json.Number `json:"limit_total_usd"`
	CostResetAt             *time.Time  `json:"cost_reset_at"`
	LimitConcurrentSessions *int32      `json:"limit_concurrent_sessions"`
	ProviderGroup           *string     `json:"provider_group"`
	CacheTTLPreference      *string     `json:"cache_ttl_preference"`
	CreatedAt               *time.Time  `json:"created_at"`
	UpdatedAt               *time.Time  `json:"updated_at"`
}

// AdminUserQuota 是用户侧与密钥限额校验相关的列。
//
// 为什么单独取这几列而不是复用数据面的 SystemSettings：patchKeyLimit 的「Key 限额不得超过用户
// 限额」校验（src/actions/keys.ts:1643-1694）逐字段对应的用户列各不相同——limitDailyUsd 比的是
// users.daily_limit_usd（Node 的 User.dailyQuota 就取自它，见 src/repository/user.ts:74,421 的
// `dailyQuota: users.dailyLimitUsd`），而不是同名不同义的 limit_daily_usd：后者只存在于 keys 表。
// 抄错这一列会让校验恒不触发，而这正是它存在的唯一理由。
type AdminUserQuota struct {
	ID                      int64       `json:"id"`
	DailyLimitUSD           json.Number `json:"daily_limit_usd"`
	Limit5hUSD              json.Number `json:"limit_5h_usd"`
	LimitWeeklyUSD          json.Number `json:"limit_weekly_usd"`
	LimitMonthlyUSD         json.Number `json:"limit_monthly_usd"`
	LimitTotalUSD           json.Number `json:"limit_total_usd"`
	LimitConcurrentSessions *int32      `json:"limit_concurrent_sessions"`
}

// AdminKeyMoneyLimitColumn 是金额限额列的**白名单**。
//
// 列名要拼进 SQL，所以它绝不能让调用方自由构造：枚举类型 + 方法内的 switch 是唯一入口，
// 未识别列一律报错，不做字符串透传。
type AdminKeyMoneyLimitColumn string

const (
	// AdminKeyLimit5hUSD 对应 keys.limit_5h_usd。
	AdminKeyLimit5hUSD AdminKeyMoneyLimitColumn = "limit_5h_usd"
	// AdminKeyLimitDailyUSD 对应 keys.limit_daily_usd。
	AdminKeyLimitDailyUSD AdminKeyMoneyLimitColumn = "limit_daily_usd"
	// AdminKeyLimitWeeklyUSD 对应 keys.limit_weekly_usd。
	AdminKeyLimitWeeklyUSD AdminKeyMoneyLimitColumn = "limit_weekly_usd"
	// AdminKeyLimitMonthlyUSD 对应 keys.limit_monthly_usd。
	AdminKeyLimitMonthlyUSD AdminKeyMoneyLimitColumn = "limit_monthly_usd"
	// AdminKeyLimitTotalUSD 对应 keys.limit_total_usd。
	AdminKeyLimitTotalUSD AdminKeyMoneyLimitColumn = "limit_total_usd"
)

// FindAdminKeyByID 复刻 findKeyById（src/repository/key.ts:22）：按主键取未软删的密钥。
func (p *Pools) FindAdminKeyByID(ctx context.Context, keyID int64) (*AdminKeyRecord, error) {
	var record AdminKeyRecord
	if err := p.readSingleRowAs(
		ctx,
		`SELECT row_to_json(t)::text FROM (
			SELECT * FROM keys WHERE id = $1 AND deleted_at IS NULL LIMIT 1
		) t`,
		&record,
		[]any{keyID},
	); err != nil {
		return nil, err
	}
	return &record, nil
}

// AdminKeyQuotaRow 是读档端点（getKeyLimitUsage / getKeyQuotaUsage）需要的行：密钥自身**加上**
// 属主的并发上限与成本重置标记（逐字复刻 Node 侧 keysTable.leftJoin(usersTable) 的那次查询，
// src/actions/keys.ts:1029-1037 与 src/actions/key-quota.ts:52-60）。
//
// 为什么要一次左连而不是分两次查：Node 的 join 条件是 `users.deleted_at IS NULL` 的**左连**，
// 属主被软删时用户侧两列是 NULL，于是并发上限只按 Key 自身解析、成本重置标记也只看 Key
// （resolveKeyConcurrentSessionLimit / resolveKeyCostResetAt）。分两次查会把「属主已软删」
// 变成一次未找到错误，语义就偏了。
//
// 嵌入 AdminKeyRecord 而不是重列 22 个字段：列名即 JSON 键名，row_to_json 的扁平输出
// 直接反序列化到嵌入结构（用户侧两列另给别名，避免与 keys 表同名列互盖）。
type AdminKeyQuotaRow struct {
	AdminKeyRecord
	UserLimitConcurrentSessions *int32     `json:"user_limit_concurrent_sessions"`
	UserCostResetAt             *time.Time `json:"user_cost_reset_at"`
}

// FindAdminKeyQuotaRow 复刻上面那次左连查询：按主键取未软删的密钥并附上属主两列。
func (p *Pools) FindAdminKeyQuotaRow(ctx context.Context, keyID int64) (*AdminKeyQuotaRow, error) {
	var row AdminKeyQuotaRow
	if err := p.readSingleRowAs(
		ctx,
		`SELECT row_to_json(t)::text FROM (
			SELECT k.id, k.user_id, k.key, k.name, k.is_enabled, k.expires_at, k.can_login_web_ui,
			       k.limit_5h_usd, k.limit_5h_reset_mode, k.limit_daily_usd, k.daily_reset_mode,
			       k.daily_reset_time, k.limit_weekly_usd, k.limit_monthly_usd, k.limit_total_usd,
			       k.cost_reset_at, k.limit_concurrent_sessions, k.provider_group,
			       k.cache_ttl_preference, k.created_at, k.updated_at,
			       u.limit_concurrent_sessions AS user_limit_concurrent_sessions,
			       u.cost_reset_at AS user_cost_reset_at
			FROM keys k
			LEFT JOIN users u ON u.id = k.user_id AND u.deleted_at IS NULL
			WHERE k.id = $1 AND k.deleted_at IS NULL
			LIMIT 1
		) t`,
		&row,
		[]any{keyID},
	); err != nil {
		return nil, err
	}
	return &row, nil
}

// FindAdminUserQuota 取用户侧限额列（users 表；同样只看未软删的行）。
func (p *Pools) FindAdminUserQuota(ctx context.Context, userID int64) (*AdminUserQuota, error) {
	var record AdminUserQuota
	if err := p.readSingleRowAs(
		ctx,
		`SELECT row_to_json(t)::text FROM (
			SELECT id, daily_limit_usd, limit_5h_usd, limit_weekly_usd, limit_monthly_usd,
			       limit_total_usd, limit_concurrent_sessions
			FROM users WHERE id = $1 AND deleted_at IS NULL LIMIT 1
		) t`,
		&record,
		[]any{userID},
	); err != nil {
		return nil, err
	}
	return &record, nil
}

// SetAdminKeyMoneyLimit 覆写一个金额限额列。value 为 nil 时写 NULL（清空限额）。
//
// 金额以文本传入并在 SQL 里显式 ::numeric 转型：numeric(10,2) 的精度不由 Go 的 float64 决定，
// 也不依赖驱动的隐式转换。
func (p *Pools) SetAdminKeyMoneyLimit(
	ctx context.Context,
	keyID int64,
	column AdminKeyMoneyLimitColumn,
	value *string,
) (bool, error) {
	var literal string
	switch column {
	case AdminKeyLimit5hUSD:
		literal = "limit_5h_usd"
	case AdminKeyLimitDailyUSD:
		literal = "limit_daily_usd"
	case AdminKeyLimitWeeklyUSD:
		literal = "limit_weekly_usd"
	case AdminKeyLimitMonthlyUSD:
		literal = "limit_monthly_usd"
	case AdminKeyLimitTotalUSD:
		literal = "limit_total_usd"
	default:
		return false, fmt.Errorf("store: 未知的密钥金额限额列 %q", column)
	}
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	query := fmt.Sprintf(
		`UPDATE keys SET %s = $2::numeric, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`,
		literal,
	)
	tag, err := pool.Exec(ctx, query, keyID, value)
	if err != nil {
		return false, fmt.Errorf("store: 更新密钥金额限额失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetAdminKeyConcurrentLimit 覆写并发会话上限（整数列，故与金额列分开：传值类型不同，
// 且整数列写 NULL 在 Node 侧只发生在整表更新里，限额快捷编辑要求传值）。
func (p *Pools) SetAdminKeyConcurrentLimit(
	ctx context.Context,
	keyID int64,
	value int32,
) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	tag, err := pool.Exec(
		ctx,
		`UPDATE keys SET limit_concurrent_sessions = $2, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`,
		keyID, value,
	)
	if err != nil {
		return false, fmt.Errorf("store: 更新密钥并发会话上限失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ResetAdminKeyCostResetAt 复刻 resetKeyCostResetAt（src/repository/key.ts:469）：写 cost_reset_at
// 并回传明文密钥——调用方要拿它清 Redis 成本缓存（Node 侧在同函数里调 invalidateCachedKey，
// 这里把「写库」与「广播失效」分开：缓存失效属于 adminapi 的 Invalidator 职责）。
//
// 返回 (明文密钥, 是否命中行, 错误)。
func (p *Pools) ResetAdminKeyCostResetAt(
	ctx context.Context,
	keyID int64,
	resetAt time.Time,
) (string, bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return "", false, err
	}
	var keyValue string
	err = pool.QueryRow(
		ctx,
		`UPDATE keys SET cost_reset_at = $2, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL
		 RETURNING key`,
		keyID, resetAt,
	).Scan(&keyValue)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: 重置密钥成本基准失败: %w", err)
	}
	return keyValue, true, nil
}

// 以下是 A1-2 续写的写路径（keys 资源的 create/update/delete 与计数）。
//
// 与读取路径的差异：写入一律走 Control 分道（与 admin_users.go 的写路径同规），且都用显式
// 列名 + `row_to_json(keys)` 回读整行——回读而不是让调用方猜，是因为 Node 的 createKey /
// updateKey / deleteKey 都 .returning(...) 整行，审计的 before/after 与缓存失效都吃这份回读值。

// Nullable 表达 Node 的三态字段：未提供 / 显式为 null / 有值。
//
// 为什么不能用 *string：`PATCH /keys/{keyId}` 里「未传 expiresAt」表示保持不动，而
// 「传了 null」表示清空该列（actions/keys.ts:508-520 用 Object.hasOwn 判存在）。一个 *string
// 的 nil 无法同时表达这两件事，猜错就会把限额悄悄清空。
type Nullable[T any] struct {
	// Provided 为 false 表示请求里没有这个字段。
	Provided bool
	// Null 为 true 表示请求显式传了 null（写 SQL NULL）——仅在 Provided 为 true 时有意义。
	Null bool
	// Value 是实际值（Null 为 true 时忽略）。
	Value T
}

// SomeValue 造一个「有值」的三态字段。
func SomeValue[T any](value T) Nullable[T] { return Nullable[T]{Provided: true, Value: value} }

// ExplicitNull 造一个「显式 null」的三态字段。
func ExplicitNull[T any]() Nullable[T] { return Nullable[T]{Provided: true, Null: true} }

// AdminKeyPatch 是 updateKey 的列级补丁（src/repository/key.ts:221-280）。
//
// 指针字段的 nil 表示「未提供」（金额与可空列用 Nullable 表达三态）。
type AdminKeyPatch struct {
	Name                    *string
	IsEnabled               *bool
	ExpiresAt               Nullable[time.Time]
	CanLoginWebUI           *bool
	Limit5hUSD              Nullable[string]
	Limit5hResetMode        *string
	LimitDailyUSD           Nullable[string]
	DailyResetMode          *string
	DailyResetTime          *string
	LimitWeeklyUSD          Nullable[string]
	LimitMonthlyUSD         Nullable[string]
	LimitTotalUSD           Nullable[string]
	CostResetAt             Nullable[time.Time]
	LimitConcurrentSessions Nullable[int32]
	ProviderGroup           Nullable[string]
	CacheTTLPreference      Nullable[string]
}

// UpdateAdminKey 复刻 updateKey：按补丁更新未软删的密钥并回读整行。
//
// 无字段可写时仍推进 updated_at——Node 在 keyData 为空对象的短路分支里只 findKeyById，
// **不**推进 updated_at；这里有字段才会真的发 SQL，空补丁由调用方短路。
func (p *Pools) UpdateAdminKey(
	ctx context.Context,
	keyID int64,
	patch AdminKeyPatch,
) (*AdminKeyRecord, bool, error) {
	clauses := make([]string, 0, 20)
	args := make([]any, 0, 20)
	args = append(args, keyID)

	addColumn := func(column string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	// addMoney 写 numeric 列：值以文本传入并显式转型（精度不由 float64 决定）。
	addMoney := func(column string, value *string) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("%s = $%d::numeric", column, len(args)))
	}
	// timeOrNil 把可选时刻转成驱动可写的形式（nil 即 SQL NULL）。
	timeOrNil := func(value Nullable[time.Time]) any {
		if value.Null {
			return nil
		}
		return value.Value
	}
	// textOrNil 与 intOrNil 同理：三态字段里 Null 写成 nil，让驱动写 SQL NULL。
	textOrNil := func(value Nullable[string]) *string {
		if value.Null {
			return nil
		}
		text := value.Value
		return &text
	}
	int32OrNil := func(value Nullable[int32]) *int32 {
		if value.Null {
			return nil
		}
		number := value.Value
		return &number
	}
	if patch.Name != nil {
		addColumn("name", *patch.Name)
	}
	if patch.IsEnabled != nil {
		addColumn("is_enabled", *patch.IsEnabled)
	}
	if patch.ExpiresAt.Provided {
		addColumn("expires_at", timeOrNil(patch.ExpiresAt))
	}
	if patch.CanLoginWebUI != nil {
		addColumn("can_login_web_ui", *patch.CanLoginWebUI)
	}
	if patch.Limit5hUSD.Provided {
		addMoney("limit_5h_usd", textOrNil(patch.Limit5hUSD))
	}
	if patch.Limit5hResetMode != nil {
		addColumn("limit_5h_reset_mode", *patch.Limit5hResetMode)
	}
	if patch.LimitDailyUSD.Provided {
		addMoney("limit_daily_usd", textOrNil(patch.LimitDailyUSD))
	}
	if patch.DailyResetMode != nil {
		addColumn("daily_reset_mode", *patch.DailyResetMode)
	}
	if patch.DailyResetTime != nil {
		addColumn("daily_reset_time", *patch.DailyResetTime)
	}
	if patch.LimitWeeklyUSD.Provided {
		addMoney("limit_weekly_usd", textOrNil(patch.LimitWeeklyUSD))
	}
	if patch.LimitMonthlyUSD.Provided {
		addMoney("limit_monthly_usd", textOrNil(patch.LimitMonthlyUSD))
	}
	if patch.LimitTotalUSD.Provided {
		addMoney("limit_total_usd", textOrNil(patch.LimitTotalUSD))
	}
	if patch.CostResetAt.Provided {
		addColumn("cost_reset_at", timeOrNil(patch.CostResetAt))
	}
	if patch.LimitConcurrentSessions.Provided {
		addColumn("limit_concurrent_sessions", int32OrNil(patch.LimitConcurrentSessions))
	}
	if patch.ProviderGroup.Provided {
		addColumn("provider_group", textOrNil(patch.ProviderGroup))
	}
	if patch.CacheTTLPreference.Provided {
		addColumn("cache_ttl_preference", textOrNil(patch.CacheTTLPreference))
	}
	if len(clauses) == 0 {
		return nil, false, nil
	}

	pool, err := p.Control()
	if err != nil {
		return nil, false, err
	}
	query := fmt.Sprintf(
		`UPDATE keys SET %s, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL
		 RETURNING row_to_json(keys)::text`,
		strings.Join(clauses, ", "),
	)
	var payload string
	if err := pool.QueryRow(ctx, query, args...).Scan(&payload); err != nil {
		if isNoRows(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("store: 更新密钥失败: %w", err)
	}
	var record AdminKeyRecord
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		return nil, false, fmt.Errorf("store: 反序列化密钥行失败: %w", err)
	}
	return &record, true, nil
}

// DeleteAdminKey 复刻 deleteKey（src/repository/key.ts:456）：软删并回传明文密钥。
//
// 回传密钥是为了让调用方清认证缓存（Node 在同函数里调 invalidateCachedKey）。
func (p *Pools) DeleteAdminKey(ctx context.Context, keyID int64) (string, bool, error) {
	pool, err := p.Control()
	if err != nil {
		return "", false, err
	}
	var keyValue string
	err = pool.QueryRow(
		ctx,
		`UPDATE keys SET deleted_at = now()
		 WHERE id = $1 AND deleted_at IS NULL
		 RETURNING key`,
		keyID,
	).Scan(&keyValue)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: 删除密钥失败: %w", err)
	}
	return keyValue, true, nil
}

// CountAdminActiveKeysByUser 复刻 countActiveKeysByUser（src/repository/key.ts:447）：
// 只数启用且未软删的密钥——「最后一个启用的密钥」保护靠它。
func (p *Pools) CountAdminActiveKeysByUser(ctx context.Context, userID int64) (int64, error) {
	pool, err := p.Data()
	if err != nil {
		return 0, err
	}
	var total int64
	if err := pool.QueryRow(
		ctx,
		`SELECT count(*) FROM keys WHERE user_id = $1 AND is_enabled = true AND deleted_at IS NULL`,
		userID,
	).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计启用密钥数失败: %w", err)
	}
	return total, nil
}

// FindAdminActiveKeyByUserAndName 复刻 findActiveKeyByUserIdAndName（src/repository/key.ts:302）：
// 「同名且正在生效」的密钥（启用、未软删、未过期）。
func (p *Pools) FindAdminActiveKeyByUserAndName(
	ctx context.Context,
	userID int64,
	name string,
) (*AdminKeyRecord, error) {
	var record AdminKeyRecord
	if err := p.readSingleRowAs(
		ctx,
		`SELECT row_to_json(t)::text FROM (
			SELECT * FROM keys
			WHERE user_id = $1 AND name = $2 AND deleted_at IS NULL AND is_enabled = true
			  AND (expires_at IS NULL OR expires_at > now())
			LIMIT 1
		) t`,
		&record,
		[]any{userID, name},
	); err != nil {
		return nil, err
	}
	return &record, nil
}

// AdminKeyCreateInput 是 createKey 的列值（src/repository/key.ts:142-165）。
//
// 金额字段用 *string（numeric 文本），与 SetAdminKeyMoneyLimit 同一约定：精度不由 float64 决定。
type AdminKeyCreateInput struct {
	UserID                  int64
	Key                     string
	Name                    string
	IsEnabled               bool
	ExpiresAt               *time.Time
	CanLoginWebUI           bool
	Limit5hUSD              *string
	Limit5hResetMode        string
	LimitDailyUSD           *string
	DailyResetMode          string
	DailyResetTime          string
	LimitWeeklyUSD          *string
	LimitMonthlyUSD         *string
	LimitTotalUSD           *string
	LimitConcurrentSessions *int32
	ProviderGroup           *string
	CacheTTLPreference      *string
}

// CreateAdminKey 复刻 createKey：插入并回读整行。
//
// Node 在插入后还做两件最佳努力的事（写 Vacuum Filter、写 Redis 缓存并广播 key 集合变更），
// 它们在 Go 侧归 Invalidator / 数据面的密钥缓存职责，本函数不重复。
func (p *Pools) CreateAdminKey(
	ctx context.Context,
	input AdminKeyCreateInput,
) (*AdminKeyRecord, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `INSERT INTO keys (
			user_id, key, name, is_enabled, expires_at, can_login_web_ui,
			limit_5h_usd, limit_5h_reset_mode, limit_daily_usd, daily_reset_mode, daily_reset_time,
			limit_weekly_usd, limit_monthly_usd, limit_total_usd, limit_concurrent_sessions,
			provider_group, cache_ttl_preference
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7::numeric, $8, $9::numeric, $10, $11,
			$12::numeric, $13::numeric, $14::numeric, $15,
			$16, $17
		) RETURNING row_to_json(keys)::text`
	var payload string
	err = pool.QueryRow(ctx, query,
		input.UserID, input.Key, input.Name, input.IsEnabled, input.ExpiresAt, input.CanLoginWebUI,
		input.Limit5hUSD, input.Limit5hResetMode, input.LimitDailyUSD, input.DailyResetMode,
		input.DailyResetTime, input.LimitWeeklyUSD, input.LimitMonthlyUSD, input.LimitTotalUSD,
		input.LimitConcurrentSessions, input.ProviderGroup, input.CacheTTLPreference,
	).Scan(&payload)
	if err != nil {
		return nil, fmt.Errorf("store: 创建密钥失败: %w", err)
	}
	var record AdminKeyRecord
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		return nil, fmt.Errorf("store: 反序列化密钥行失败: %w", err)
	}
	return &record, nil
}

// SyncAdminUserProviderGroupFromKeys 复刻 syncUserProviderGroupFromKeys
// （src/actions/users.ts:437-457）：用户分组 = 其全部密钥分组的并集（字典序），空则 "default"。
//
// 返回写入 users.provider_group 的值，供调用方记日志/审计。
func (p *Pools) SyncAdminUserProviderGroupFromKeys(ctx context.Context, userID int64) (string, error) {
	keys, err := p.ListAdminUserKeys(ctx, []int64{userID})
	if err != nil {
		return "", err
	}
	union := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		// Node：`key.providerGroup || "default"`（#400 之后 Key 分组不再允许 null，
		// 这里保留同一回落以兼容历史行）。
		group := key.ProviderGroup
		if group == nil || strings.TrimSpace(*group) == "" {
			fallback := "default"
			group = &fallback
		}
		for _, item := range SplitProviderGroups(group) {
			union[item] = struct{}{}
		}
	}
	groups := make([]string, 0, len(union))
	for item := range union {
		groups = append(groups, item)
	}
	sort.Strings(groups)
	normalized := "default"
	if len(groups) > 0 {
		normalized = strings.Join(groups, ",")
	}
	pool, err := p.Control()
	if err != nil {
		return "", err
	}
	if _, err := pool.Exec(
		ctx,
		`UPDATE users SET provider_group = $2, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`,
		userID, normalized,
	); err != nil {
		return "", fmt.Errorf("store: 同步用户分组失败: %w", err)
	}
	return normalized, nil
}

// AdminKeyBatchPatch 是 batchUpdateKeys 允许改的列（src/actions/keys.ts:1436-1453）。
//
// 比 AdminKeyPatch 窄得多：批量编辑只覆盖这 8 个字段，且**不**支持清空限额之外的语义
// （isEnabled / canLoginWebUi / providerGroup 三列不可为 NULL）。
type AdminKeyBatchPatch struct {
	IsEnabled        *bool
	CanLoginWebUI    *bool
	ProviderGroup    *string
	Limit5hUSD       Nullable[string]
	Limit5hResetMode *string
	LimitDailyUSD    Nullable[string]
	LimitWeeklyUSD   Nullable[string]
	LimitMonthlyUSD  Nullable[string]
}

// Empty 判断补丁里一个字段都没有（Node 的 hasAnyUpdate）。
func (patch AdminKeyBatchPatch) Empty() bool {
	return patch.IsEnabled == nil &&
		patch.CanLoginWebUI == nil &&
		patch.ProviderGroup == nil &&
		!patch.Limit5hUSD.Provided &&
		patch.Limit5hResetMode == nil &&
		!patch.LimitDailyUSD.Provided &&
		!patch.LimitWeeklyUSD.Provided &&
		!patch.LimitMonthlyUSD.Provided
}

// AdminKeyBatchError 是批量更新的失败：带 Node 的错误码与缺失 id，供调用方映射响应。
//
// 用类型而不是只返 error 字符串：Node 的 BatchUpdateError 也带 errorCode，且「哪些 id 不存在」
// 会写进错误文案与响应，两者都必须在跨层时不丢失。
type AdminKeyBatchError struct {
	// Code 取 Node 的 ERROR_CODES（NOT_FOUND / CANNOT_DISABLE_LAST_KEY / UPDATE_FAILED）。
	Code string
	// MissingIDs 仅 Code 为 NOT_FOUND 时有值。
	MissingIDs []int64
}

// Error 实现 error。
func (e *AdminKeyBatchError) Error() string {
	if len(e.MissingIDs) > 0 {
		return fmt.Sprintf("store: 部分 Key 不存在: %v", e.MissingIDs)
	}
	return "store: 批量更新密钥失败: " + e.Code
}

// BatchUpdateAdminKeys 复刻 batchUpdateKeys 的事务块（src/actions/keys.ts:1345-1490）。
//
// 事务内的四步与 Node 同序：① 校验全部 id 存在（缺失即回滚）② 禁用时先算「禁用后是否还剩启用
// 密钥」（不足即回滚）③ 整批更新，行数不匹配即回滚 ④ 禁用后再验一次（挡住并发禁用）。
// 第 ④ 步是 Node 特意写的竞态兜底（注释标为 CRITICAL），不能省。
//
// 返回（更新的 id、受影响的用户 id）。失败时两者均无意义，错误为 *AdminKeyBatchError
// 或 store 内部错误。
func (p *Pools) BatchUpdateAdminKeys(
	ctx context.Context,
	ids []int64,
	patch AdminKeyBatchPatch,
) ([]int64, []int64, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	type keyRow struct {
		ID        int64
		UserID    int64
		IsEnabled bool
	}
	rows, err := tx.Query(
		ctx,
		`SELECT id, user_id, is_enabled FROM keys WHERE id = ANY($1) AND deleted_at IS NULL`,
		ids,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("store: 批量校验密钥失败: %w", err)
	}
	existing := make([]keyRow, 0, len(ids))
	for rows.Next() {
		var row keyRow
		if err := rows.Scan(&row.ID, &row.UserID, &row.IsEnabled); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("store: 读取密钥行失败: %w", err)
		}
		existing = append(existing, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: 遍历密钥行失败: %w", err)
	}

	found := make(map[int64]struct{}, len(existing))
	for _, row := range existing {
		found[row.ID] = struct{}{}
	}
	missing := make([]int64, 0)
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return nil, nil, &AdminKeyBatchError{Code: "NOT_FOUND", MissingIDs: missing}
	}

	affectedUsers := make([]int64, 0, len(existing))
	seenUsers := make(map[int64]struct{}, len(existing))
	for _, row := range existing {
		if _, ok := seenUsers[row.UserID]; ok {
			continue
		}
		seenUsers[row.UserID] = struct{}{}
		affectedUsers = append(affectedUsers, row.UserID)
	}

	disabling := patch.IsEnabled != nil && !*patch.IsEnabled
	if disabling {
		// 将本批中被禁用的「当前启用」密钥按用户计数。
		pending := make(map[int64]int, len(affectedUsers))
		for _, row := range existing {
			if row.IsEnabled {
				pending[row.UserID]++
			}
		}
		for userID, count := range pending {
			var current int64
			if err := tx.QueryRow(
				ctx,
				`SELECT count(*) FROM keys
				 WHERE user_id = $1 AND is_enabled = true AND deleted_at IS NULL`,
				userID,
			).Scan(&current); err != nil {
				return nil, nil, fmt.Errorf("store: 统计启用密钥数失败: %w", err)
			}
			if current-int64(count) < 1 {
				return nil, nil, &AdminKeyBatchError{Code: "CANNOT_DISABLE_LAST_KEY"}
			}
		}
	}

	assignments := make([]string, 0, 8)
	args := make([]any, 0, 10)
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if patch.IsEnabled != nil {
		assignments = append(assignments, "is_enabled = "+addArg(*patch.IsEnabled))
	}
	if patch.CanLoginWebUI != nil {
		assignments = append(assignments, "can_login_web_ui = "+addArg(*patch.CanLoginWebUI))
	}
	if patch.ProviderGroup != nil {
		assignments = append(assignments, "provider_group = "+addArg(*patch.ProviderGroup))
	}
	money := func(column string, value Nullable[string]) {
		var number *string
		if !value.Null {
			text := value.Value
			number = &text
		}
		assignments = append(assignments, column+" = "+addArg(number)+"::numeric")
	}
	if patch.Limit5hUSD.Provided {
		money("limit_5h_usd", patch.Limit5hUSD)
	}
	if patch.Limit5hResetMode != nil {
		assignments = append(assignments, "limit_5h_reset_mode = "+addArg(*patch.Limit5hResetMode))
	}
	if patch.LimitDailyUSD.Provided {
		money("limit_daily_usd", patch.LimitDailyUSD)
	}
	if patch.LimitWeeklyUSD.Provided {
		money("limit_weekly_usd", patch.LimitWeeklyUSD)
	}
	if patch.LimitMonthlyUSD.Provided {
		money("limit_monthly_usd", patch.LimitMonthlyUSD)
	}
	if len(assignments) == 0 {
		return nil, nil, fmt.Errorf("store: 批量更新密钥缺少可写字段")
	}
	// updated_at 与 Node 的 dbUpdates.updatedAt 同义，放在最后（列顺序不影响结果）。
	assignments = append(assignments, "updated_at = now()")

	query := fmt.Sprintf(
		`UPDATE keys SET %s WHERE id = ANY(%s) AND deleted_at IS NULL RETURNING id`,
		strings.Join(assignments, ", "),
		addArg(ids),
	)
	updatedRows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: 批量更新密钥失败: %w", err)
	}
	updated := make([]int64, 0, len(ids))
	for updatedRows.Next() {
		var id int64
		if err := updatedRows.Scan(&id); err != nil {
			updatedRows.Close()
			return nil, nil, fmt.Errorf("store: 读取更新结果失败: %w", err)
		}
		updated = append(updated, id)
	}
	updatedRows.Close()
	if err := updatedRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: 遍历更新结果失败: %w", err)
	}
	if len(updated) != len(ids) {
		return nil, nil, &AdminKeyBatchError{Code: "UPDATE_FAILED"}
	}

	if disabling {
		for _, userID := range affectedUsers {
			var remaining int64
			if err := tx.QueryRow(
				ctx,
				`SELECT count(*) FROM keys
				 WHERE user_id = $1 AND is_enabled = true AND deleted_at IS NULL`,
				userID,
			).Scan(&remaining); err != nil {
				return nil, nil, fmt.Errorf("store: 复核启用密钥数失败: %w", err)
			}
			if remaining < 1 {
				return nil, nil, &AdminKeyBatchError{Code: "CANNOT_DISABLE_LAST_KEY"}
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("store: 提交批量更新失败: %w", err)
	}
	return updated, affectedUsers, nil
}
