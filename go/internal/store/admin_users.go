package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件是管理面 users 资源的落库面。
// 列名与 Node 的读取别名逐字对齐（src/repository/user.ts 的 drizzle select 别名），
// 因此 row_to_json 的键名就是 Node 响应里的键名，无需人工映射。
//
// 只读路径一律走 Data 分道 + row_to_json（与 read.go 同）；写入路径走 Writer 分道并返回
// 是否命中行，由调用方决定 404 还是 200。

// AdminUserRow 是 users 表的管理面读取视图。数值列在 row_to_json 里可能是 JSON number
// 也可能是字符串（numeric 的表示差异），因此金额一律先按字符串接，再按 Node 的
// parseFloat 语义解析（见 NodeJSON）。
type AdminUserRow struct {
	ID                      int64       `json:"id"`
	Name                    string      `json:"name"`
	Description             *string     `json:"description"`
	Role                    *string     `json:"role"`
	RPM                     *int64      `json:"rpm"`
	DailyQuota              json.Number `json:"dailyQuota"`
	ProviderGroup           *string     `json:"providerGroup"`
	Tags                    []string    `json:"tags"`
	CreatedAt               time.Time   `json:"createdAt"`
	UpdatedAt               time.Time   `json:"updatedAt"`
	DeletedAt               *time.Time  `json:"deletedAt"`
	Limit5hUSD              json.Number `json:"limit5hUsd"`
	Limit5hResetMode        *string     `json:"limit5hResetMode"`
	LimitWeeklyUSD          json.Number `json:"limitWeeklyUsd"`
	LimitMonthlyUSD         json.Number `json:"limitMonthlyUsd"`
	LimitTotalUSD           json.Number `json:"limitTotalUsd"`
	CostResetAt             *time.Time  `json:"costResetAt"`
	Limit5hCostResetAt      *time.Time  `json:"limit5hCostResetAt"`
	LimitConcurrentSessions *int64      `json:"limitConcurrentSessions"`
	DailyResetMode          *string     `json:"dailyResetMode"`
	DailyResetTime          *string     `json:"dailyResetTime"`
	IsEnabled               *bool       `json:"isEnabled"`
	ExpiresAt               *time.Time  `json:"expiresAt"`
	AllowedClients          []string    `json:"allowedClients"`
	BlockedClients          []string    `json:"blockedClients"`
	AllowedModels           []string    `json:"allowedModels"`
}

// adminUserColumns 是 Node drizzle select 的列别名清单，**顺序即 Node 响应里的键序**
// （src/repository/user.ts:413 起的 findUserById 与 :197 起的 findUserListBatch 一致）。
const adminUserColumns = `
	id,
	name,
	description,
	role,
	rpm_limit AS "rpm",
	daily_limit_usd AS "dailyQuota",
	provider_group AS "providerGroup",
	tags,
	created_at AS "createdAt",
	updated_at AS "updatedAt",
	deleted_at AS "deletedAt",
	limit_5h_usd AS "limit5hUsd",
	limit_5h_reset_mode AS "limit5hResetMode",
	limit_weekly_usd AS "limitWeeklyUsd",
	limit_monthly_usd AS "limitMonthlyUsd",
	limit_total_usd AS "limitTotalUsd",
	cost_reset_at AS "costResetAt",
	limit_5h_cost_reset_at AS "limit5hCostResetAt",
	limit_concurrent_sessions AS "limitConcurrentSessions",
	daily_reset_mode AS "dailyResetMode",
	daily_reset_time AS "dailyResetTime",
	is_enabled AS "isEnabled",
	expires_at AS "expiresAt",
	allowed_clients AS "allowedClients",
	blocked_clients AS "blockedClients",
	allowed_models AS "allowedModels"`

// FindAdminUserByID 复刻 findUserById：按 id 取未软删用户。
//
// 未命中返回 ErrNotFound（调用方据此作答 404 user.not_found）。
func (p *Pools) FindAdminUserByID(ctx context.Context, id int64) (*AdminUserRow, error) {
	var row AdminUserRow
	if err := p.readSingleRowAs(
		ctx,
		`SELECT row_to_json(t)::text FROM (
			SELECT`+adminUserColumns+`
			FROM users WHERE id = $1 AND deleted_at IS NULL
		) t`,
		&row,
		[]any{id},
	); err != nil {
		return nil, err
	}
	return &row, nil
}

// AdminUserRef 是筛选下拉用的精简引用（Node 返回 {id, name}）。
type AdminUserRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// SearchAdminUsers 复刻 searchUsersForFilterRepository：按 name 的 ILIKE 模糊匹配，
// 管理员排前，再按 id 升序。
//
// limit 的夹取由调用方按 Node 的 normalizeSearchUsersLimit 完成（repository 侧只做
// max(1,min(limit,5000))）。
func (p *Pools) SearchAdminUsers(ctx context.Context, searchTerm string, limit int) ([]AdminUserRef, error) {
	effective := limit
	if effective < 1 {
		effective = 1
	}
	if effective > 5000 {
		effective = 5000
	}
	args := []any{effective}
	pattern := ""
	if trimmed := strings.TrimSpace(searchTerm); trimmed != "" {
		pattern = "%" + trimmed + "%"
		args = append([]any{pattern}, args...)
	}
	where := "deleted_at IS NULL"
	if pattern != "" {
		where += " AND name ILIKE $1"
	}
	if pattern == "" {
		return readRowsAs[AdminUserRef](
			ctx, p,
			`SELECT row_to_json(t)::text FROM (
				SELECT id, name FROM users
				WHERE `+where+`
				ORDER BY CASE WHEN role = 'admin' THEN 0 ELSE 1 END ASC, id ASC
				LIMIT $1
			) t`,
			args...,
		)
	}
	return readRowsAs[AdminUserRef](
		ctx, p,
		`SELECT row_to_json(t)::text FROM (
			SELECT id, name FROM users
			WHERE `+where+`
			ORDER BY CASE WHEN role = 'admin' THEN 0 ELSE 1 END ASC, id ASC
			LIMIT $2
		) t`,
		args...,
	)
}

// ListAdminUserTags 复刻 getAllUserTags：取全部未软删用户的 tags 去重后字典序排序。
func (p *Pools) ListAdminUserTags(ctx context.Context) ([]string, error) {
	rows, err := readRowsAs[struct {
		Tags []string `json:"tags"`
	}](ctx, p, `SELECT row_to_json(t)::text FROM (SELECT tags FROM users WHERE deleted_at IS NULL) t`)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	tags := make([]string, 0, 16)
	for _, row := range rows {
		for _, tag := range row.Tags {
			if _, ok := seen[tag]; ok {
				continue
			}
			seen[tag] = struct{}{}
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)
	return tags, nil
}

// ListAdminUserProviderGroups 复刻 getAllUserProviderGroups：把 provider_group 按
// [,，\n\r]+ 切分（parseProviderGroups 的语义）后去重并字典序排序。
//
// 切分放在 Go 侧而不是 SQL：TS 侧就是纯字符串切分，用 SQL 的 regexp_split_to_array
// 复刻会把「空值跳过」等细节分散到两处，得不偿失。
func (p *Pools) ListAdminUserProviderGroups(ctx context.Context) ([]string, error) {
	rows, err := readRowsAs[struct {
		ProviderGroup *string `json:"providerGroup"`
	}](ctx, p, `SELECT row_to_json(t)::text FROM (
		SELECT provider_group AS "providerGroup" FROM users WHERE deleted_at IS NULL
	) t`)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	groups := make([]string, 0, 16)
	for _, row := range rows {
		for _, group := range SplitProviderGroups(row.ProviderGroup) {
			if _, ok := seen[group]; ok {
				continue
			}
			seen[group] = struct{}{}
			groups = append(groups, group)
		}
	}
	sort.Strings(groups)
	return groups, nil
}

// SplitProviderGroups 复刻 parseProviderGroups：按 [,，\n\r]+ 切分、去空白、丢空段。
func SplitProviderGroups(value *string) []string {
	if value == nil {
		return nil
	}
	fields := strings.FieldsFunc(*value, func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == '\r'
	})
	groups := make([]string, 0, len(fields))
	for _, field := range fields {
		trimmed := strings.TrimSpace(field)
		if trimmed == "" {
			continue
		}
		groups = append(groups, trimmed)
	}
	return groups
}

// AdminUserPatch 是 updateUser 的列级补丁：nil 表示该列未出现在请求里，保持不动。
//
// 用显式列清单而不是 map：列名是 SQL 拼接的一部分，map 会把「拼错列名」变成运行期
// 语法错误，显式字段则在编译期就错。
type AdminUserPatch struct {
	Name                    *string
	Description             *string
	ProviderGroup           *string
	Tags                    *[]string
	RPM                     *int64
	DailyQuota              *string
	Limit5hUSD              *string
	Limit5hResetMode        *string
	LimitWeeklyUSD          *string
	LimitMonthlyUSD         *string
	LimitTotalUSD           *string
	LimitConcurrentSessions *int64
	DailyResetMode          *string
	DailyResetTime          *string
	IsEnabled               *bool
	ExpiresAt               *time.Time
	ClearExpiresAt          bool
	AllowedClients          *[]string
	BlockedClients          *[]string
	AllowedModels           *[]string
}

// columns 返回 SET 子句的列与值，以及「补丁是否为空」。
//
// 顺序固定（与结构体字段同序），便于测试直接断言 SQL 形状。Node 侧 updateUser 恒写
// updated_at（src/repository/user.ts:451 起的 dbData 初始化），这里同样恒写。
func (patch AdminUserPatch) columns() ([]string, []any) {
	names := make([]string, 0, 20)
	values := make([]any, 0, 20)
	add := func(column string, value any) {
		names = append(names, column)
		values = append(values, value)
	}
	if patch.Name != nil {
		add("name", *patch.Name)
	}
	if patch.Description != nil {
		add("description", *patch.Description)
	}
	if patch.ProviderGroup != nil {
		add("provider_group", *patch.ProviderGroup)
	}
	if patch.Tags != nil {
		add("tags", *patch.Tags)
	}
	if patch.RPM != nil {
		add("rpm_limit", *patch.RPM)
	}
	if patch.DailyQuota != nil {
		add("daily_limit_usd", numericArg(patch.DailyQuota))
	}
	if patch.Limit5hUSD != nil {
		add("limit_5h_usd", numericArg(patch.Limit5hUSD))
	}
	if patch.Limit5hResetMode != nil {
		add("limit_5h_reset_mode", *patch.Limit5hResetMode)
	}
	if patch.LimitWeeklyUSD != nil {
		add("limit_weekly_usd", numericArg(patch.LimitWeeklyUSD))
	}
	if patch.LimitMonthlyUSD != nil {
		add("limit_monthly_usd", numericArg(patch.LimitMonthlyUSD))
	}
	if patch.LimitTotalUSD != nil {
		add("limit_total_usd", numericArg(patch.LimitTotalUSD))
	}
	if patch.LimitConcurrentSessions != nil {
		add("limit_concurrent_sessions", *patch.LimitConcurrentSessions)
	}
	if patch.DailyResetMode != nil {
		add("daily_reset_mode", *patch.DailyResetMode)
	}
	if patch.DailyResetTime != nil {
		add("daily_reset_time", *patch.DailyResetTime)
	}
	if patch.IsEnabled != nil {
		add("is_enabled", *patch.IsEnabled)
	}
	if patch.ClearExpiresAt {
		add("expires_at", nil)
	} else if patch.ExpiresAt != nil {
		add("expires_at", *patch.ExpiresAt)
	}
	if patch.AllowedClients != nil {
		add("allowed_clients", *patch.AllowedClients)
	}
	if patch.BlockedClients != nil {
		add("blocked_clients", *patch.BlockedClients)
	}
	if patch.AllowedModels != nil {
		add("allowed_models", *patch.AllowedModels)
	}
	return names, values
}

// IsEmpty 判断补丁是否没有任何列。
//
// 空补丁在 Node 侧直接退化为 findUserById（src/repository/user.ts:424-426），
// 调用方据此决定「不回写、只读回」。
func (patch AdminUserPatch) IsEmpty() bool {
	names, _ := patch.columns()
	return len(names) == 0
}

// UpdateAdminUser 复刻 updateUser：按补丁写列，恒写 updated_at，命中未软删行才返回 true。
//
// 补丁为空时按 Node 语义等价于「读回」：返回行是否存在，不改任何列。
//
// 写面走 control 分道而不是 writer 分道：writer 分道是终态结算的专属道（它决定
// 断线/退出时账目是否漏写，见 settle barrier 那组用例），管理面的人手操作不该占它的在途额度。
func (p *Pools) UpdateAdminUser(ctx context.Context, id int64, patch AdminUserPatch) (bool, error) {
	pool, err := p.Control()
	if err != nil {
		return false, err
	}
	names, values := patch.columns()
	if len(names) == 0 {
		var exists int64
		err := pool.QueryRow(
			ctx,
			`SELECT id FROM users WHERE id = $1 AND deleted_at IS NULL`,
			id,
		).Scan(&exists)
		if err == pgx.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("store: 读取用户失败: %w", err)
		}
		return true, nil
	}
	assignments := make([]string, 0, len(names)+1)
	args := make([]any, 0, len(values)+1)
	for index, name := range names {
		args = append(args, values[index])
		assignments = append(assignments, fmt.Sprintf("%s = $%d", name, len(args)))
	}
	assignments = append(assignments, `updated_at = now()`)
	args = append(args, id)
	query := fmt.Sprintf(
		`UPDATE users SET %s WHERE id = $%d AND deleted_at IS NULL RETURNING id`,
		strings.Join(assignments, ", "),
		len(args),
	)
	var returnedID int64
	err = pool.QueryRow(ctx, query, args...).Scan(&returnedID)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: 更新用户失败: %w", err)
	}
	return true, nil
}

// SoftDeleteAdminUser 复刻 deleteUser：软删（写 deleted_at），命中未软删行才返回 true。
//
// 账本行刻意不随用户删除（Node 同：src/actions/users.ts:1881 的注释）。
// 分道选择同 UpdateAdminUser。
func (p *Pools) SoftDeleteAdminUser(ctx context.Context, id int64) (bool, error) {
	pool, err := p.Control()
	if err != nil {
		return false, err
	}
	var returnedID int64
	err = pool.QueryRow(
		ctx,
		`UPDATE users SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL RETURNING id`,
		id,
	).Scan(&returnedID)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: 软删用户失败: %w", err)
	}
	return true, nil
}

// NodeJSON 按 Node 的 toUser（src/repository/shared/transformers.ts:29）整形为响应对象。
//
// 逐条复刻 toUser 的默认值与零值语义：
//   - description 为空串（不是 null）；
//   - role 缺省 "user"；limit5hResetMode 缺省 "rolling"；dailyResetMode 缺省 "fixed"；
//     dailyResetTime 缺省 "00:00"；isEnabled 缺省 true；
//   - rpm / dailyQuota 走「> 0 才算数」的分支（0 与负值都回落 null）；
//   - limit*Usd 走 parseOptionalNumber（0 保留 0）；
//   - tags / allowedClients / blockedClients / allowedModels 缺省空数组；
//   - 时间列一律 Date 语义，由 JSONDate 输出 Node 的 ISO 字符串（毫秒三位）。
func (row AdminUserRow) NodeJSON() map[string]any {
	role := "user"
	if row.Role != nil && *row.Role != "" {
		role = *row.Role
	}
	description := ""
	if row.Description != nil {
		description = *row.Description
	}
	var rpm any
	if row.RPM != nil && *row.RPM > 0 {
		rpm = *row.RPM
	}
	dailyQuota := positiveNumber(row.DailyQuota)
	limit5hResetMode := "rolling"
	if row.Limit5hResetMode != nil && *row.Limit5hResetMode != "" {
		limit5hResetMode = *row.Limit5hResetMode
	}
	dailyResetMode := "fixed"
	if row.DailyResetMode != nil && *row.DailyResetMode != "" {
		dailyResetMode = *row.DailyResetMode
	}
	dailyResetTime := "00:00"
	if row.DailyResetTime != nil && *row.DailyResetTime != "" {
		dailyResetTime = *row.DailyResetTime
	}
	isEnabled := true
	if row.IsEnabled != nil {
		isEnabled = *row.IsEnabled
	}
	return map[string]any{
		"id":                      row.ID,
		"name":                    row.Name,
		"description":             description,
		"role":                    role,
		"rpm":                     rpm,
		"dailyQuota":              dailyQuota,
		"providerGroup":           nilIfNilString(row.ProviderGroup),
		"tags":                    nonNilStrings(row.Tags),
		"createdAt":               JSONDate(row.CreatedAt),
		"updatedAt":               JSONDate(row.UpdatedAt),
		"deletedAt":               JSONDatePtr(row.DeletedAt),
		"limit5hUsd":              optionalNumber(row.Limit5hUSD),
		"limit5hResetMode":        limit5hResetMode,
		"limitWeeklyUsd":          optionalNumber(row.LimitWeeklyUSD),
		"limitMonthlyUsd":         optionalNumber(row.LimitMonthlyUSD),
		"limitTotalUsd":           optionalNumber(row.LimitTotalUSD),
		"costResetAt":             JSONDatePtr(row.CostResetAt),
		"limit5hCostResetAt":      JSONDatePtr(row.Limit5hCostResetAt),
		"limitConcurrentSessions": nilIfNilInt64(row.LimitConcurrentSessions),
		"dailyResetMode":          dailyResetMode,
		"dailyResetTime":          dailyResetTime,
		"isEnabled":               isEnabled,
		"expiresAt":               JSONDatePtr(row.ExpiresAt),
		"allowedClients":          nonNilStrings(row.AllowedClients),
		"blockedClients":          nonNilStrings(row.BlockedClients),
		"allowedModels":           nonNilStrings(row.AllowedModels),
	}
}

// JSONDate 输出 Node Date.toJSON 的字符串形式：UTC + 毫秒三位。
//
// 不能直接用 time.Time 的默认编码：它会按纳秒有效位裁剪小数位（"…T00:00:00.123456Z"），
// 与 Node 的固定三位分叉。
func JSONDate(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// JSONDatePtr 输出可空时间的 Node 形式；nil 返回 nil（JSON null）。
func JSONDatePtr(value *time.Time) any {
	if value == nil {
		return nil
	}
	return JSONDate(*value)
}

func nilIfNilString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nilIfNilInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nonNilStrings(value []string) []string {
	if value == nil {
		return []string{}
	}
	return value
}

// positiveNumber 复刻 toUser 里「null / 非数 / 不大于 0 → null」的分支。
func positiveNumber(value json.Number) any {
	parsed, ok := parseJSONNumber(value)
	if !ok || parsed <= 0 {
		return nil
	}
	return parsed
}

// optionalNumber 复刻 parseOptionalNumber：空为 null，非数也回落 null，0 保留。
func optionalNumber(value json.Number) any {
	parsed, ok := parseJSONNumber(value)
	if !ok {
		return nil
	}
	return parsed
}

func parseJSONNumber(value json.Number) (float64, bool) {
	text := strings.TrimSpace(value.String())
	if text == "" || text == "null" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

// AdminUserListFilters 是 findUserListBatch 的入参（src/repository/user.ts:197）。
//
// Limit 由调用方按 Node 的 schema 校验（1..100，默认 50）后再传；本函数仍按 Node 语义
// 夹到 [1,200]。
type AdminUserListFilters struct {
	Cursor          string
	Limit           int
	SearchTerm      string
	TagFilters      []string
	KeyGroupFilters []string
	StatusFilter    string
	SortBy          string
	SortOrder       string
}

// AdminUserListResult 是 users 列表页：行 + 下一页游标 + 是否还有更多。
type AdminUserListResult struct {
	Users      []AdminUserRow
	NextCursor *string
	HasMore    bool
}

// adminUserListMaxLimit 复刻 findUserListBatch 的硬上限。
const adminUserListMaxLimit = 200

// listSortColumns 是 Node 侧 KEYSET_SORT_COLUMNS：只有 NOT NULL 且可比较的列才走 keyset，
// 其余列退化为 offset 游标。
var listSortColumns = map[string]string{
	"name":            "name",
	"tags":            "tags",
	"expiresAt":       "expires_at",
	"rpm":             "rpm_limit",
	"limit5hUsd":      "limit_5h_usd",
	"limitDailyUsd":   "daily_limit_usd",
	"limitWeeklyUsd":  "limit_weekly_usd",
	"limitMonthlyUsd": "limit_monthly_usd",
	"createdAt":       "created_at",
}

// keysetSortKeys 复刻 KEYSET_SORT_COLUMNS。
var keysetSortKeys = map[string]struct{}{"name": {}, "createdAt": {}}

// ListAdminUsers 复刻 findUserListBatch：混合游标分页（NOT NULL 列走 keyset，可空列走 offset）。
//
// 逐条对齐的语义：有效 limit 夹到 [1,200]；取 limit+1 行判断 hasMore；排序恒以 id ASC 收尾；
// createdAt 游标比较用 date_trunc('milliseconds') 以匹配 JS Date 的毫秒精度。
func (p *Pools) ListAdminUsers(ctx context.Context, filters AdminUserListFilters) (AdminUserListResult, error) {
	limit := filters.Limit
	if limit < 1 {
		limit = 1
	}
	if limit > adminUserListMaxLimit {
		limit = adminUserListMaxLimit
	}
	sortBy := filters.SortBy
	if sortBy == "" {
		sortBy = "createdAt"
	}
	sortColumn, ok := listSortColumns[sortBy]
	if !ok {
		return AdminUserListResult{}, fmt.Errorf("store: 未知的排序字段 %q", sortBy)
	}
	sortOrder := filters.SortOrder
	if sortOrder == "" {
		sortOrder = "asc"
	}

	args := []any{}
	conditions := []string{"deleted_at IS NULL"}
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	if trimmed := strings.TrimSpace(filters.SearchTerm); trimmed != "" {
		placeholder := addArg("%" + trimmed + "%")
		conditions = append(conditions, fmt.Sprintf(`(
			name ILIKE %[1]s
			OR description ILIKE %[1]s
			OR provider_group ILIKE %[1]s
			OR EXISTS (
				SELECT 1 FROM jsonb_array_elements_text(coalesce(tags, '[]'::jsonb)) AS tag
				WHERE tag ILIKE %[1]s
			)
			OR EXISTS (
				SELECT 1 FROM keys
				WHERE keys.user_id = users.id
					AND keys.deleted_at IS NULL
					AND (keys.name ILIKE %[1]s OR keys.key ILIKE %[1]s OR keys.provider_group ILIKE %[1]s)
			)
		)`, placeholder))
	}

	tags := make([]string, 0, len(filters.TagFilters))
	for _, tag := range filters.TagFilters {
		if trimmed := strings.TrimSpace(tag); trimmed != "" {
			tags = append(tags, trimmed)
		}
	}
	if len(tags) > 0 {
		clauses := make([]string, 0, len(tags))
		for _, tag := range tags {
			encoded, err := json.Marshal([]string{tag})
			if err != nil {
				return AdminUserListResult{}, fmt.Errorf("store: 标签过滤编码失败: %w", err)
			}
			clauses = append(clauses, fmt.Sprintf("tags @> %s::jsonb", addArg(string(encoded))))
		}
		conditions = append(conditions, "("+strings.Join(clauses, " OR ")+")")
	}

	groups := make([]string, 0, len(filters.KeyGroupFilters))
	for _, group := range filters.KeyGroupFilters {
		if trimmed := strings.TrimSpace(group); trimmed != "" {
			groups = append(groups, trimmed)
		}
	}
	if len(groups) > 0 {
		clauses := make([]string, 0, len(groups))
		for _, group := range groups {
			clauses = append(clauses, fmt.Sprintf(
				`%s = ANY(regexp_split_to_array(coalesce(provider_group, ''), '\s*[,，\n\r]+\s*'))`,
				addArg(group),
			))
		}
		conditions = append(conditions, "("+strings.Join(clauses, " OR ")+")")
	}

	switch filters.StatusFilter {
	case "", "all":
	case "active":
		conditions = append(conditions, "(expires_at IS NULL OR expires_at >= NOW()) AND is_enabled = true")
	case "expired":
		conditions = append(conditions, "expires_at < NOW()")
	case "expiringSoon":
		conditions = append(conditions,
			"expires_at IS NOT NULL AND expires_at >= NOW() AND expires_at <= NOW() + INTERVAL '7 days'")
	case "enabled":
		conditions = append(conditions, "is_enabled = true")
	case "disabled":
		conditions = append(conditions, "is_enabled = false")
	default:
		return AdminUserListResult{}, fmt.Errorf("store: 未知的状态过滤 %q", filters.StatusFilter)
	}

	offset := 0
	if filters.Cursor != "" {
		if _, keyset := keysetSortKeys[sortBy]; keyset {
			keysetValue, keysetID, ok := parseKeysetCursor(filters.Cursor)
			if ok {
				if sortBy == "createdAt" {
					parsed, err := time.Parse(time.RFC3339Nano, keysetValue)
					if err == nil {
						iso := parsed.UTC().Format("2006-01-02T15:04:05.000Z")
						truncated := "date_trunc('milliseconds', " + sortColumn + ")"
						if sortOrder == "asc" {
							conditions = append(conditions, fmt.Sprintf(
								"(%s, id) > (%s::timestamptz, %s)",
								truncated, addArg(iso), addArg(keysetID),
							))
						} else {
							conditions = append(conditions, fmt.Sprintf(
								"(%s < %s::timestamptz OR (%s = %s::timestamptz AND id > %s))",
								truncated, addArg(iso), truncated, addArg(iso), addArg(keysetID),
							))
						}
					}
				} else if sortOrder == "asc" {
					conditions = append(conditions, fmt.Sprintf(
						"(%s, id) > (%s, %s)", sortColumn, addArg(keysetValue), addArg(keysetID),
					))
				} else {
					conditions = append(conditions, fmt.Sprintf(
						"(%s < %s OR (%s = %s AND id > %s))",
						sortColumn, addArg(keysetValue), sortColumn, addArg(keysetValue), addArg(keysetID),
					))
				}
			} else {
				offset = maxInt(parseCursorOffset(filters.Cursor), 0)
			}
		} else {
			offset = maxInt(parseCursorOffset(filters.Cursor), 0)
		}
	}

	fetchLimit := limit + 1
	orderDirection := "ASC"
	if sortOrder != "asc" {
		orderDirection = "DESC"
	}
	query := fmt.Sprintf(
		`SELECT row_to_json(t)::text FROM (
			SELECT%s
			FROM users
			WHERE %s
			ORDER BY %s %s, id ASC
			LIMIT %s%s
		) t`,
		adminUserColumns,
		strings.Join(conditions, " AND "),
		sortColumn,
		orderDirection,
		addArg(fetchLimit),
		offsetClause(offset, addArg),
	)

	rows, err := readRowsAs[AdminUserRow](ctx, p, query, args...)
	if err != nil {
		return AdminUserListResult{}, err
	}

	hasMore := len(rows) > limit
	page := rows
	if hasMore {
		page = rows[:limit]
	}
	result := AdminUserListResult{Users: page, HasMore: hasMore}
	if hasMore && len(page) > 0 {
		last := page[len(page)-1]
		var cursor string
		if _, keyset := keysetSortKeys[sortBy]; keyset {
			value := last.Name
			if sortBy == "createdAt" {
				value = JSONDate(last.CreatedAt)
			}
			encoded, err := json.Marshal(map[string]any{"v": value, "id": last.ID})
			if err != nil {
				return AdminUserListResult{}, fmt.Errorf("store: 游标编码失败: %w", err)
			}
			cursor = string(encoded)
		} else {
			cursor = strconv.Itoa(offset + limit)
		}
		result.NextCursor = &cursor
	}
	return result, nil
}

// offsetClause 生成 offset 子句；offset 为 0 时不出现（与 Node 的 offset > 0 判断一致）。
func offsetClause(offset int, addArg func(any) string) string {
	if offset <= 0 {
		return ""
	}
	return " OFFSET " + addArg(offset)
}

// parseKeysetCursor 复刻 parseKeysetCursor：长度上限 1024，JSON 形如 {"v":..., "id":...}，
// id 必须为正整数。
func parseKeysetCursor(raw string) (string, int64, bool) {
	if len(raw) > 1024 {
		return "", 0, false
	}
	var parsed struct {
		V  any `json:"v"`
		ID any `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", 0, false
	}
	if parsed.V == nil || parsed.ID == nil {
		return "", 0, false
	}
	id, err := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(parsed.ID)), 10, 64)
	if err != nil || id <= 0 {
		return "", 0, false
	}
	return fmt.Sprint(parsed.V), id, true
}

// parseCursorOffset 复刻 `Number(cursor) || 0`：非数字一律 0。
func parseCursorOffset(raw string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return parsed
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

// AdminUserKey 是 findKeyListBatch 的键视图（列别名与 src/repository/key.ts:99 一致）。
type AdminUserKey struct {
	ID                      int64       `json:"id"`
	UserID                  int64       `json:"userId"`
	Key                     string      `json:"key"`
	Name                    string      `json:"name"`
	IsEnabled               *bool       `json:"isEnabled"`
	ExpiresAt               *time.Time  `json:"expiresAt"`
	CanLoginWebUI           *bool       `json:"canLoginWebUi"`
	Limit5hUSD              json.Number `json:"limit5hUsd"`
	Limit5hResetMode        *string     `json:"limit5hResetMode"`
	LimitDailyUSD           json.Number `json:"limitDailyUsd"`
	DailyResetMode          *string     `json:"dailyResetMode"`
	DailyResetTime          *string     `json:"dailyResetTime"`
	LimitWeeklyUSD          json.Number `json:"limitWeeklyUsd"`
	LimitMonthlyUSD         json.Number `json:"limitMonthlyUsd"`
	LimitTotalUSD           json.Number `json:"limitTotalUsd"`
	CostResetAt             *time.Time  `json:"costResetAt"`
	LimitConcurrentSessions *int64      `json:"limitConcurrentSessions"`
	ProviderGroup           *string     `json:"providerGroup"`
	CacheTTLPreference      *string     `json:"cacheTtlPreference"`
	CreatedAt               time.Time   `json:"createdAt"`
	UpdatedAt               time.Time   `json:"updatedAt"`
	DeletedAt               *time.Time  `json:"deletedAt"`
}

// ListAdminUserKeys 复刻 findKeyListBatch：取这批用户的全部未软删密钥。
//
// 按 id 排序（Node 未显式排序，实际取插入序；按主键排是与之最接近的确定性行为）。
func (p *Pools) ListAdminUserKeys(ctx context.Context, userIDs []int64) ([]AdminUserKey, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	return readRowsAs[AdminUserKey](
		ctx, p,
		`SELECT row_to_json(t)::text FROM (
			SELECT
				id, user_id AS "userId", key, name,
				is_enabled AS "isEnabled", expires_at AS "expiresAt", can_login_web_ui AS "canLoginWebUi",
				limit_5h_usd AS "limit5hUsd", limit_5h_reset_mode AS "limit5hResetMode",
				limit_daily_usd AS "limitDailyUsd", daily_reset_mode AS "dailyResetMode",
				daily_reset_time AS "dailyResetTime", limit_weekly_usd AS "limitWeeklyUsd",
				limit_monthly_usd AS "limitMonthlyUsd", limit_total_usd AS "limitTotalUsd",
				cost_reset_at AS "costResetAt", limit_concurrent_sessions AS "limitConcurrentSessions",
				provider_group AS "providerGroup", cache_ttl_preference AS "cacheTtlPreference",
				created_at AS "createdAt", updated_at AS "updatedAt", deleted_at AS "deletedAt"
			FROM keys
			WHERE user_id = ANY($1) AND deleted_at IS NULL
			ORDER BY id ASC
		) t`,
		userIDs,
	)
}

// AdminUserModelStat 是 modelStats 的一项（src/repository/key.ts:1050 起的形状）。
type AdminUserModelStat struct {
	Model               string      `json:"model"`
	CallCount           int64       `json:"callCount"`
	TotalCost           json.Number `json:"totalCost"`
	InputTokens         json.Number `json:"inputTokens"`
	OutputTokens        json.Number `json:"outputTokens"`
	CacheCreationTokens json.Number `json:"cacheCreationTokens"`
	CacheReadTokens     json.Number `json:"cacheReadTokens"`
}

// AdminUserTodayUsage 是某把密钥当日的成本与用量合计。
type AdminUserTodayUsage struct {
	KeyID       int64       `json:"keyId"`
	TotalCost   json.Number `json:"totalCost"`
	TotalTokens json.Number `json:"totalTokens"`
}

// AdminUserKeyStatistics 是某把密钥的统计（KeyStatistics 形状）。
type AdminUserKeyStatistics struct {
	KeyID            int64                `json:"keyId"`
	TodayCallCount   int64                `json:"todayCallCount"`
	LastUsedAt       *time.Time           `json:"lastUsedAt"`
	LastProviderName *string              `json:"lastProviderName"`
	ModelStats       []AdminUserModelStat `json:"modelStats"`
}

// AdminUserKeyUsage 汇总某把密钥当日用量与统计，供 UserDisplay 组装。
type AdminUserKeyUsage struct {
	Today          AdminUserTodayUsage
	Statistics     AdminUserKeyStatistics
	StatisticsSeen bool
}

// LoadAdminUserKeyUsage 复刻 findKeyUsageTodayBatch + findKeysStatisticsBatchFromKeys：
// 一次给出这批密钥的当日成本/Token、当日调用数、最近一次调用（含供应商名）与当日模型统计。
//
// 「当日」的边界与 Node 一致：进程本地时区的零点到次日零点（Node 用 setHours(0,0,0,0)，
// **不是** system_settings 里的时区）。
func (p *Pools) LoadAdminUserKeyUsage(ctx context.Context, keys []AdminUserKey) (map[int64]AdminUserKeyUsage, error) {
	usage := make(map[int64]AdminUserKeyUsage, len(keys))
	if len(keys) == 0 {
		return usage, nil
	}
	for _, key := range keys {
		usage[key.ID] = AdminUserKeyUsage{
			Today:      AdminUserTodayUsage{KeyID: key.ID, TotalCost: "0", TotalTokens: "0"},
			Statistics: AdminUserKeyStatistics{KeyID: key.ID},
		}
	}

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	tomorrow := today.AddDate(0, 0, 1)

	keyStrings := make([]string, 0, len(keys))
	keyStringToID := make(map[string]int64, len(keys))
	for _, key := range keys {
		keyStrings = append(keyStrings, key.Key)
		if _, ok := keyStringToID[key.Key]; !ok {
			keyStringToID[key.Key] = key.ID
		}
	}

	todayRows, err := readRowsAs[struct {
		Key         string      `json:"key"`
		TotalCost   json.Number `json:"totalCost"`
		TotalTokens json.Number `json:"totalTokens"`
	}](ctx, p, fmt.Sprintf(
		`SELECT row_to_json(t)::text FROM (
			SELECT ul.key AS key,
				COALESCE(SUM(ul.cost_usd), 0)::text AS "totalCost",
				COALESCE(SUM(
					COALESCE(ul.input_tokens, 0)::double precision +
					COALESCE(ul.output_tokens, 0)::double precision +
					COALESCE(ul.cache_creation_input_tokens, 0)::double precision +
					COALESCE(ul.cache_read_input_tokens, 0)::double precision
				), 0::double precision)::text AS "totalTokens"
			FROM usage_ledger ul
			WHERE ul.key = ANY($1) AND %s AND ul.created_at >= $2 AND ul.created_at < $3
			GROUP BY ul.key
		) t`,
		LedgerBillingConditionFor("ul"),
	), keyStrings, today, tomorrow)
	if err != nil {
		return nil, err
	}
	for _, row := range todayRows {
		id, ok := keyStringToID[row.Key]
		if !ok {
			continue
		}
		entry := usage[id]
		entry.Today.TotalCost = roundCostText(row.TotalCost, 6)
		entry.Today.TotalTokens = row.TotalTokens
		usage[id] = entry
	}

	countRows, err := readRowsAs[struct {
		Key   string `json:"key"`
		Count int64  `json:"count"`
	}](ctx, p, fmt.Sprintf(
		`SELECT row_to_json(t)::text FROM (
			SELECT ul.key AS key, COUNT(*)::bigint AS "count"
			FROM usage_ledger ul
			WHERE ul.key = ANY($1) AND %s AND ul.created_at >= $2 AND ul.created_at < $3
			GROUP BY ul.key
		) t`,
		LedgerBillingConditionFor("ul"),
	), keyStrings, today, tomorrow)
	if err != nil {
		return nil, err
	}
	for _, row := range countRows {
		id, ok := keyStringToID[row.Key]
		if !ok {
			continue
		}
		entry := usage[id]
		entry.Statistics.KeyID = id
		entry.Statistics.TodayCallCount = row.Count
		entry.StatisticsSeen = true
		usage[id] = entry
	}

	lastRows, err := readRowsAs[struct {
		Key          string     `json:"key"`
		LastUsedAt   *time.Time `json:"lastUsedAt"`
		ProviderName *string    `json:"providerName"`
	}](ctx, p,
		`SELECT row_to_json(t)::text FROM (
			SELECT k.key_val AS key,
				lr.created_at AS "lastUsedAt",
				p.name AS "providerName"
			FROM unnest($1::varchar[]) AS k(key_val)
			LEFT JOIN LATERAL (
				SELECT ul.created_at, ul.final_provider_id
				FROM usage_ledger ul
				WHERE ul.key = k.key_val AND ul.blocked_by IS NULL AND ul.is_replay = false
				ORDER BY ul.created_at DESC
				LIMIT 1
			) lr ON true
			LEFT JOIN providers p ON lr.final_provider_id = p.id
		) t`,
		keyStrings,
	)
	if err != nil {
		return nil, err
	}
	for _, row := range lastRows {
		id, ok := keyStringToID[row.Key]
		if !ok {
			continue
		}
		entry := usage[id]
		entry.Statistics.KeyID = id
		entry.Statistics.LastUsedAt = row.LastUsedAt
		entry.Statistics.LastProviderName = row.ProviderName
		entry.StatisticsSeen = true
		usage[id] = entry
	}

	modelRows, err := readRowsAs[struct {
		Key                 string      `json:"key"`
		Model               *string     `json:"model"`
		CallCount           int64       `json:"callCount"`
		TotalCost           json.Number `json:"totalCost"`
		InputTokens         json.Number `json:"inputTokens"`
		OutputTokens        json.Number `json:"outputTokens"`
		CacheCreationTokens json.Number `json:"cacheCreationTokens"`
		CacheReadTokens     json.Number `json:"cacheReadTokens"`
	}](ctx, p, fmt.Sprintf(
		`SELECT row_to_json(t)::text FROM (
			SELECT ul.key AS key, ul.model AS model, COUNT(*)::int AS "callCount",
				COALESCE(SUM(ul.cost_usd), 0)::text AS "totalCost",
				COALESCE(SUM(ul.input_tokens), 0)::double precision::text AS "inputTokens",
				COALESCE(SUM(ul.output_tokens), 0)::double precision::text AS "outputTokens",
				COALESCE(SUM(ul.cache_creation_input_tokens), 0)::double precision::text AS "cacheCreationTokens",
				COALESCE(SUM(ul.cache_read_input_tokens), 0)::double precision::text AS "cacheReadTokens"
			FROM usage_ledger ul
			WHERE ul.key = ANY($1) AND %s AND ul.created_at >= $2 AND ul.created_at < $3
				AND ul.model IS NOT NULL
			GROUP BY ul.key, ul.model
			ORDER BY ul.key ASC, COUNT(*) DESC
		) t`,
		LedgerBillingConditionFor("ul"),
	), keyStrings, today, tomorrow)
	if err != nil {
		return nil, err
	}
	for _, row := range modelRows {
		id, ok := keyStringToID[row.Key]
		if !ok {
			continue
		}
		model := "unknown"
		if row.Model != nil && *row.Model != "" {
			model = *row.Model
		}
		entry := usage[id]
		entry.Statistics.KeyID = id
		entry.Statistics.ModelStats = append(entry.Statistics.ModelStats, AdminUserModelStat{
			Model:               model,
			CallCount:           row.CallCount,
			TotalCost:           roundCostText(row.TotalCost, 6),
			InputTokens:         row.InputTokens,
			OutputTokens:        row.OutputTokens,
			CacheCreationTokens: row.CacheCreationTokens,
			CacheReadTokens:     row.CacheReadTokens,
		})
		usage[id] = entry
	}
	return usage, nil
}

// LedgerBillingConditionFor 把 LEDGER_BILLING_CONDITION 里的裸列名加上表别名前缀。
//
// 用字符串替换而不是再抄一份条件：条件本身是跨语言契约（src/repository/_shared/
// ledger-conditions.ts），抄第二份就会有两个会漂移的真相来源。
func LedgerBillingConditionFor(alias string) string {
	replaced := strings.ReplaceAll(BillingCondition, "blocked_by", alias+".blocked_by")
	replaced = strings.ReplaceAll(replaced, "is_replay", alias+".is_replay")
	replaced = strings.ReplaceAll(replaced, "endpoint", alias+".endpoint")
	return replaced
}

// roundCostText 复刻 Node 的 toDecimalPlaces(6)：把 numeric 文本四舍五入到 6 位小数，
// 并去掉尾随零（JSON number 没有尾零）。
func roundCostText(value json.Number, places int) json.Number {
	text := strings.TrimSpace(value.String())
	if text == "" || text == "null" {
		return json.Number("0")
	}
	parsed, err := decimalRound(text, places)
	if err != nil {
		return json.Number("0")
	}
	return json.Number(parsed)
}

// decimalRound 对十进制字符串做半进位舍入（Node Decimal.toDecimalPlaces 的默认 ROUND_HALF_UP）。
//
// 自己实现而不是过 float64：金额舍入到 6 位时，二进制浮点的误差恰好会落在引发分位跳变的
// 边界值上（例如 0.0000005）。
func decimalRound(value string, places int) (string, error) {
	if places < 0 {
		return "", fmt.Errorf("store: 非法小数位 %d", places)
	}
	trimmed := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(value), "-"), "+")
	negative := strings.HasPrefix(strings.TrimSpace(value), "-")

	integerPart, fractionPart := trimmed, ""
	if dot := strings.IndexByte(trimmed, '.'); dot >= 0 {
		integerPart, fractionPart = trimmed[:dot], trimmed[dot+1:]
	}
	if integerPart == "" {
		integerPart = "0"
	}
	for _, digit := range integerPart + fractionPart {
		if digit < '0' || digit > '9' {
			return "", fmt.Errorf("store: 非法十进制 %q", value)
		}
	}
	// 小数位补到 places+1 位：多出的那一位是舍入判位。
	for len(fractionPart) < places+1 {
		fractionPart += "0"
	}
	roundUp := fractionPart[places] >= '5'
	digits := []byte(integerPart + fractionPart[:places])
	if roundUp {
		carried := true
		for index := len(digits) - 1; index >= 0 && carried; index-- {
			if digits[index] == '9' {
				digits[index] = '0'
				continue
			}
			digits[index]++
			carried = false
		}
		if carried {
			digits = append([]byte{'1'}, digits...)
		}
	}

	text := string(digits)
	for len(text) < places+1 {
		text = "0" + text
	}
	integerOut := strings.TrimLeft(text[:len(text)-places], "0")
	if integerOut == "" {
		integerOut = "0"
	}
	formatted := integerOut
	if places > 0 {
		fractionOut := strings.TrimRight(text[len(text)-places:], "0")
		if fractionOut != "" {
			formatted += "." + fractionOut
		}
	}
	if negative && formatted != "0" {
		formatted = "-" + formatted
	}
	return formatted, nil
}

// AdminUserCreateInput 是 createUser 的写入面（src/repository/user.ts:45 的 dbData）。
//
// 零值语义与 Node 一致：tags / allowed* 为空切片即空数组；limit5hResetMode 空取 "rolling"；
// dailyResetMode 空取 "fixed"；dailyResetTime 空取 "00:00"；IsEnabled 空取 true。
type AdminUserCreateInput struct {
	Name                    string
	Description             string
	ProviderGroup           *string
	Tags                    []string
	RPM                     *int64
	DailyQuota              *string
	Limit5hUSD              *string
	Limit5hResetMode        string
	LimitWeeklyUSD          *string
	LimitMonthlyUSD         *string
	LimitTotalUSD           *string
	LimitConcurrentSessions *int64
	DailyResetMode          string
	DailyResetTime          string
	IsEnabled               *bool
	ExpiresAt               *time.Time
	AllowedClients          []string
	BlockedClients          []string
	AllowedModels           []string
}

// CreateAdminUser 复刻 createUser：插入并读回管理面视图行。
//
// 空 ProviderGroup 写 NULL（Node 侧 handler 已把空值归一成 "default"，故这里通常非空）。
func (p *Pools) CreateAdminUser(ctx context.Context, input AdminUserCreateInput) (*AdminUserRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	limit5hResetMode := input.Limit5hResetMode
	if limit5hResetMode == "" {
		limit5hResetMode = "rolling"
	}
	dailyResetMode := input.DailyResetMode
	if dailyResetMode == "" {
		dailyResetMode = "fixed"
	}
	dailyResetTime := input.DailyResetTime
	if dailyResetTime == "" {
		dailyResetTime = "00:00"
	}
	isEnabled := true
	if input.IsEnabled != nil {
		isEnabled = *input.IsEnabled
	}
	tags := input.Tags
	if tags == nil {
		tags = []string{}
	}
	allowedClients := input.AllowedClients
	if allowedClients == nil {
		allowedClients = []string{}
	}
	blockedClients := input.BlockedClients
	if blockedClients == nil {
		blockedClients = []string{}
	}
	allowedModels := input.AllowedModels
	if allowedModels == nil {
		allowedModels = []string{}
	}

	var payload string
	err = pool.QueryRow(ctx,
		`WITH inserted AS (
			INSERT INTO users (
				name, description, rpm_limit, daily_limit_usd, provider_group, tags,
				limit_5h_usd, limit_5h_reset_mode, limit_weekly_usd, limit_monthly_usd,
				limit_total_usd, limit_concurrent_sessions, daily_reset_mode, daily_reset_time,
				is_enabled, expires_at, allowed_clients, blocked_clients, allowed_models
			) VALUES (
				$1, $2, $3, $4::numeric, $5, $6,
				$7::numeric, $8, $9::numeric, $10::numeric,
				$11::numeric, $12, $13, $14,
				$15, $16, $17, $18, $19
			)
			RETURNING *
		)
		SELECT row_to_json(t)::text FROM (SELECT`+adminUserColumns+` FROM inserted) t`,
		input.Name, input.Description, input.RPM, numericArg(input.DailyQuota), input.ProviderGroup, tags,
		numericArg(input.Limit5hUSD), limit5hResetMode, numericArg(input.LimitWeeklyUSD),
		numericArg(input.LimitMonthlyUSD), numericArg(input.LimitTotalUSD),
		input.LimitConcurrentSessions, dailyResetMode, dailyResetTime,
		isEnabled, input.ExpiresAt, allowedClients, blockedClients, allowedModels,
	).Scan(&payload)
	if err != nil {
		return nil, fmt.Errorf("store: 创建用户失败: %w", err)
	}
	var row AdminUserRow
	if err := json.Unmarshal([]byte(payload), &row); err != nil {
		return nil, fmt.Errorf("store: 反序列化新建用户失败: %w", err)
	}
	return &row, nil
}

// AdminUserBatchPatch 是 batchUpdateUsers 的列补丁（只含该 action 允许的 8 个字段）。
type AdminUserBatchPatch struct {
	Description      *string
	Tags             *[]string
	RPM              *int64
	DailyQuota       *string
	Limit5hUSD       *string
	Limit5hResetMode *string
	LimitWeeklyUSD   *string
	LimitMonthlyUSD  *string
}

// IsEmpty 判断补丁是否没有任何字段（Node 侧对应 EMPTY_UPDATE）。
func (patch AdminUserBatchPatch) IsEmpty() bool {
	return patch.Description == nil && patch.Tags == nil && patch.RPM == nil &&
		patch.DailyQuota == nil && patch.Limit5hUSD == nil && patch.Limit5hResetMode == nil &&
		patch.LimitWeeklyUSD == nil && patch.LimitMonthlyUSD == nil
}

// BatchUpdateAdminUsers 复刻 batchUpdateUsers 的事务块：先校验全部存在，再整批更新，
// 更新行数不匹配即回滚。
//
// 返回（已存在并更新成功的 id 列表，缺失的 id 列表）。缺失非空时事务已回滚，updatedIDs 为空。
func (p *Pools) BatchUpdateAdminUsers(ctx context.Context, ids []int64, patch AdminUserBatchPatch) ([]int64, []int64, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	existingRows, err := tx.Query(ctx, `SELECT id FROM users WHERE id = ANY($1) AND deleted_at IS NULL`, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("store: 批量校验用户失败: %w", err)
	}
	existing := make(map[int64]struct{}, len(ids))
	for existingRows.Next() {
		var id int64
		if err := existingRows.Scan(&id); err != nil {
			existingRows.Close()
			return nil, nil, fmt.Errorf("store: 读取用户 id 失败: %w", err)
		}
		existing[id] = struct{}{}
	}
	existingRows.Close()
	if err := existingRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: 遍历用户 id 失败: %w", err)
	}

	missing := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := existing[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return nil, missing, nil
	}

	assignments := []string{"updated_at = now()"}
	args := []any{}
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if patch.Description != nil {
		assignments = append(assignments, "description = "+addArg(*patch.Description))
	}
	if patch.Tags != nil {
		assignments = append(assignments, "tags = "+addArg(*patch.Tags))
	}
	if patch.RPM != nil {
		assignments = append(assignments, "rpm_limit = "+addArg(*patch.RPM))
	}
	if patch.DailyQuota != nil {
		assignments = append(assignments, "daily_limit_usd = "+addArg(numericArg(patch.DailyQuota))+"::numeric")
	}
	if patch.Limit5hUSD != nil {
		assignments = append(assignments, "limit_5h_usd = "+addArg(numericArg(patch.Limit5hUSD))+"::numeric")
	}
	if patch.Limit5hResetMode != nil {
		assignments = append(assignments, "limit_5h_reset_mode = "+addArg(*patch.Limit5hResetMode))
	}
	if patch.LimitWeeklyUSD != nil {
		assignments = append(assignments, "limit_weekly_usd = "+addArg(numericArg(patch.LimitWeeklyUSD))+"::numeric")
	}
	if patch.LimitMonthlyUSD != nil {
		assignments = append(assignments, "limit_monthly_usd = "+addArg(numericArg(patch.LimitMonthlyUSD))+"::numeric")
	}

	query := fmt.Sprintf(
		`UPDATE users SET %s WHERE id = ANY(%s) AND deleted_at IS NULL RETURNING id`,
		strings.Join(assignments, ", "),
		addArg(ids),
	)
	updatedRows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: 批量更新用户失败: %w", err)
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
		return nil, nil, fmt.Errorf("store: 批量更新行数不匹配（期望 %d，实际 %d）", len(ids), len(updated))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("store: 提交批量更新失败: %w", err)
	}
	return updated, nil, nil
}

// CreateAdminUserDefaultKey 复刻 createKey 在「新建用户附带默认密钥」路径上的最小写入
// （src/repository/key.ts:142）：只写 Node 该路径实际给出的字段，其余列取列默认值。
//
// 为什么放在本文件而不是 admin_keys.go：这是**用户创建流程**的一部分（addUser 的后半步），
// 与密钥资源的 CRUD 端点无关；放在用户侧可避免两个 lane 争同一文件。
func (p *Pools) CreateAdminUserDefaultKey(
	ctx context.Context,
	userID int64,
	key string,
	name string,
	providerGroup *string,
) (int64, error) {
	pool, err := p.Control()
	if err != nil {
		return 0, err
	}
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO keys (user_id, key, name, is_enabled, provider_group)
		 VALUES ($1, $2, $3, true, $4)
		 RETURNING id`,
		userID, key, name, providerGroup,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: 创建默认密钥失败: %w", err)
	}
	return id, nil
}

// ResetAdminUserCostMarkers 复刻 updateUserCostResetMarkers：把 cost_reset_at 与
// limit_5h_cost_reset_at 一并推进到 resetAt（Node 的 resetUserLimitsOnly 同时写两个标记，
// 避免 later-of 仍读到旧边界）。
func (p *Pools) ResetAdminUserCostMarkers(ctx context.Context, userID int64, resetAt time.Time) (bool, error) {
	pool, err := p.Control()
	if err != nil {
		return false, err
	}
	var returnedID int64
	err = pool.QueryRow(ctx,
		`UPDATE users SET cost_reset_at = $1, limit_5h_cost_reset_at = $1, updated_at = now()
		 WHERE id = $2 AND deleted_at IS NULL
		 RETURNING id`,
		resetAt, userID,
	).Scan(&returnedID)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: 重置用户成本标记失败: %w", err)
	}
	return true, nil
}

// AdminSystemTimezone 复刻 resolveSystemTimezone 的第一步：读 system_settings.timezone。
//
// 单列读取而不是复用 FindSystemSettings：后者是数据面的只读视图，管理面的时区解析只需要
// 一列，把它加进那个结构体会为了一个管理面用途去改数据面契约。
func (p *Pools) AdminSystemTimezone(ctx context.Context) (*string, error) {
	var payload struct {
		Timezone *string `json:"timezone"`
	}
	if err := p.readSingleRowAs(
		ctx,
		`SELECT row_to_json(t)::text FROM (
			SELECT timezone FROM system_settings ORDER BY id ASC LIMIT 1
		) t`,
		&payload,
		[]any{},
	); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return payload.Timezone, nil
}
