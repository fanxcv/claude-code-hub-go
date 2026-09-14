package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 本文件是 request_filters 表的管理面读写面（批次 A / lane A1-4）。
//
// 唯一真源：src/repository/request-filters.ts。SQL 形状、排序与 NULL 语义逐条对齐该文件；
// 差异只写在注释里。
//
// 分道：读走 control，写走 writer。
//
// jsonb 列（replacement / provider_ids / group_tags / operations）一律经 nullableJSON 处理：
// SQL NULL 与 JSON null 是两回事，而响应里两者都渲染成 null——写错会让 `IS NULL` 判定失效。

// AdminRequestFilter 是 request_filters 的一行。
//
// 字段名取 Node 的 camelCase（RequestFilterSchema，src/lib/api/v1/schemas/request-filters.ts:25-45）：
// 本类型同时充当响应体的形状来源，故不能改成 Go 风格的命名再去映射。
type AdminRequestFilter struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Scope       string  `json:"scope"`
	Action      string  `json:"action"`
	// MatchType 列可空（varchar 无 notNull），Node 侧 `row.matchType ?? null`。
	MatchType   *string         `json:"matchType"`
	Target      string          `json:"target"`
	Replacement json.RawMessage `json:"replacement"`
	Priority    int             `json:"priority"`
	IsEnabled   bool            `json:"isEnabled"`
	BindingType string          `json:"bindingType"`
	ProviderIDs json.RawMessage `json:"providerIds"`
	GroupTags   json.RawMessage `json:"groupTags"`
	RuleMode    string          `json:"ruleMode"`
	// ExecutionPhase 同 RuleMode：列 notNull 且有默认值，故不是指针。
	ExecutionPhase string          `json:"executionPhase"`
	Operations     json.RawMessage `json:"operations"`
	CreatedAt      *string         `json:"createdAt"`
	UpdatedAt      *string         `json:"updatedAt"`
}

// requestFilterColumns 与 getAllRequestFilters 的投影一致。
const requestFilterColumns = `id, name, description, scope, action, match_type AS "matchType", target,
	replacement, priority, is_enabled AS "isEnabled", binding_type AS "bindingType",
	provider_ids AS "providerIds", group_tags AS "groupTags", rule_mode AS "ruleMode",
	execution_phase AS "executionPhase", operations,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt",
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "updatedAt"`

// AdminListRequestFilters 复刻 getAllRequestFilters：含禁用行，按创建时间倒序。
func (p *Pools) AdminListRequestFilters(ctx context.Context) ([]AdminRequestFilter, error) {
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + requestFilterColumns +
		` FROM request_filters ORDER BY created_at DESC NULLS FIRST) t`
	return requestFilterRows(ctx, p, query)
}

// AdminListActiveRequestFilters 复刻 getActiveRequestFilters：只取启用行，按 (priority, id) 升序。
//
// 顺序即引擎的装载序（request-filter-engine.ts:427-437 的 byPriority 同序），故照抄。
func (p *Pools) AdminListActiveRequestFilters(ctx context.Context) ([]AdminRequestFilter, error) {
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + requestFilterColumns +
		` FROM request_filters WHERE is_enabled = true ORDER BY priority ASC, id ASC) t`
	return requestFilterRows(ctx, p, query)
}

// AdminCountActiveRequestFilters 复刻 requestFilterEngine.getStats() 的 count。
//
// 引擎把每个启用行恰好放进四个桶之一（global/provider × guard/final），故 count 就是启用行数。
func (p *Pools) AdminCountActiveRequestFilters(ctx context.Context) (int64, error) {
	pool, err := p.Control()
	if err != nil {
		return 0, err
	}
	var total int64
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*)::bigint FROM request_filters WHERE is_enabled = true").Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计启用请求过滤器失败: %w", err)
	}
	return total, nil
}

// AdminGetRequestFilterByID 复刻 getRequestFilterById。
func (p *Pools) AdminGetRequestFilterByID(ctx context.Context, id int64) (*AdminRequestFilter, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + requestFilterColumns +
		` FROM request_filters WHERE id = $1) t`
	row := pool.QueryRow(ctx, query, id)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: 读取请求过滤器失败: %w", err)
	}
	filter, err := decodeRequestFilter(payload)
	if err != nil {
		return nil, err
	}
	return &filter, nil
}

// AdminCreateRequestFilterInput 是一次插入。
//
// 默认值由调用方（adminapi 层）按 Node 的 action 补齐后再传进来（`data.priority ?? 0` 等）：
// 默认值是**业务规则**，与 SQL 列的默认值不必一致（例如 rule_mode 的列默认是 'simple'，但
// Node 显式写入 'simple'，两者的区别出现在未来改列默认值时）。
type AdminCreateRequestFilterInput struct {
	Name           string
	Description    *string
	Scope          string
	Action         string
	Target         string
	MatchType      *string
	Replacement    json.RawMessage
	Priority       int
	IsEnabled      bool
	BindingType    string
	ProviderIDs    json.RawMessage
	GroupTags      json.RawMessage
	RuleMode       string
	ExecutionPhase string
	Operations     json.RawMessage
}

// AdminCreateRequestFilter 复刻 createRequestFilter。
func (p *Pools) AdminCreateRequestFilter(
	ctx context.Context,
	input AdminCreateRequestFilterInput,
) (AdminRequestFilter, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminRequestFilter{}, err
	}
	query := `INSERT INTO request_filters
		(name, description, scope, action, match_type, target, replacement, priority, is_enabled,
		 binding_type, provider_ids, group_tags, rule_mode, execution_phase, operations)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11::jsonb, $12::jsonb, $13, $14, $15::jsonb)
		RETURNING ` + requestFilterColumns
	created, err := scanRequestFilter(pool.QueryRow(ctx, query,
		input.Name, input.Description, input.Scope, input.Action, input.MatchType, input.Target,
		nullableJSON(input.Replacement), input.Priority, input.IsEnabled, input.BindingType,
		nullableJSON(input.ProviderIDs), nullableJSON(input.GroupTags), input.RuleMode,
		input.ExecutionPhase, nullableJSON(input.Operations)))
	if err != nil {
		return AdminRequestFilter{}, fmt.Errorf("store: 写入请求过滤器失败: %w", err)
	}
	return created, nil
}

// AdminRequestFilterUpdate 是一次部分更新。
//
// 每个字段「出现即写」（Node 的 `.set({...data})` 只写请求里出现的键），故标量与可空列分别用
// 指针与 AdminNullable* 表达——可空列的「写 NULL」与「不写」是两回事。
type AdminRequestFilterUpdate struct {
	Name           *string
	Description    AdminNullableText
	Scope          *string
	Action         *string
	MatchType      AdminNullableText
	Target         *string
	Replacement    AdminNullableJSON
	Priority       *int
	IsEnabled      *bool
	BindingType    *string
	ProviderIDs    AdminNullableJSON
	GroupTags      AdminNullableJSON
	RuleMode       *string
	ExecutionPhase *string
	Operations     AdminNullableJSON
}

// AdminUpdateRequestFilter 复刻 updateRequestFilter：部分更新 + updated_at 置当前时间。
//
// 记录不存在时返回 ErrNotFound（Node 侧 returning() 为空即 null，action 映射成 404）。
func (p *Pools) AdminUpdateRequestFilter(
	ctx context.Context,
	id int64,
	update AdminRequestFilterUpdate,
) (AdminRequestFilter, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminRequestFilter{}, err
	}
	assignments := make([]string, 0, 16)
	args := make([]any, 0, 16)
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
	if update.Description.Present {
		set("description", update.Description.Value, false)
	}
	if update.Scope != nil {
		set("scope", *update.Scope, false)
	}
	if update.Action != nil {
		set("action", *update.Action, false)
	}
	if update.MatchType.Present {
		set("match_type", update.MatchType.Value, false)
	}
	if update.Target != nil {
		set("target", *update.Target, false)
	}
	if update.Replacement.Present {
		set("replacement", nullableJSON(update.Replacement.Raw), true)
	}
	if update.Priority != nil {
		set("priority", *update.Priority, false)
	}
	if update.IsEnabled != nil {
		set("is_enabled", *update.IsEnabled, false)
	}
	if update.BindingType != nil {
		set("binding_type", *update.BindingType, false)
	}
	if update.ProviderIDs.Present {
		set("provider_ids", nullableJSON(update.ProviderIDs.Raw), true)
	}
	if update.GroupTags.Present {
		set("group_tags", nullableJSON(update.GroupTags.Raw), true)
	}
	if update.RuleMode != nil {
		set("rule_mode", *update.RuleMode, false)
	}
	if update.ExecutionPhase != nil {
		set("execution_phase", *update.ExecutionPhase, false)
	}
	if update.Operations.Present {
		set("operations", nullableJSON(update.Operations.Raw), true)
	}
	// updatedAt 恒被写入（Node 侧也如此），因此 SET 列表永不为空。
	assignments = append(assignments, "updated_at = now()")
	args = append(args, id)

	query := fmt.Sprintf("UPDATE request_filters SET %s WHERE id = $%d RETURNING %s",
		strings.Join(assignments, ", "), len(args), requestFilterColumns)
	updated, err := scanRequestFilter(pool.QueryRow(ctx, query, args...))
	if err != nil {
		if isNoRows(err) {
			return AdminRequestFilter{}, ErrNotFound
		}
		return AdminRequestFilter{}, fmt.Errorf("store: 更新请求过滤器失败: %w", err)
	}
	return updated, nil
}

// AdminDeleteRequestFilter 复刻 deleteRequestFilter：硬删除，返回是否删掉了行。
func (p *Pools) AdminDeleteRequestFilter(ctx context.Context, id int64) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	tag, err := pool.Exec(ctx, "DELETE FROM request_filters WHERE id = $1", id)
	if err != nil {
		return false, fmt.Errorf("store: 删除请求过滤器失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AdminProviderOption 是绑定选择器用的供应商选项（Node 的 `{ id, name }`）。
type AdminProviderOption struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// AdminListProviderOptions 复刻 listProvidersForFilterAction 用的 findAllProviders 投影。
//
// 两个语义要点：软删行（deleted_at 非空）不出现；**禁用行仍然出现**（findAllProvidersFresh 只
// 过滤 deleted_at），故这里也不加 is_enabled 条件。排序按 created_at 倒序，与 Node 的
// `orderBy(desc(providers.createdAt))` 一致。
//
// 差异（登记进白名单）：Node 的 findAllProviders 走 30 秒进程缓存，Go 侧每次现读——库刚改动时
// Go 的答案更新。
func (p *Pools) AdminListProviderOptions(ctx context.Context) ([]AdminProviderOption, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx,
		`SELECT id, name FROM providers WHERE deleted_at IS NULL ORDER BY created_at DESC NULLS FIRST`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询供应商选项失败: %w", err)
	}
	defer rows.Close()

	options := make([]AdminProviderOption, 0, 16)
	for rows.Next() {
		var option AdminProviderOption
		if err := rows.Scan(&option.ID, &option.Name); err != nil {
			return nil, fmt.Errorf("store: 读取供应商选项失败: %w", err)
		}
		options = append(options, option)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历供应商选项失败: %w", err)
	}
	return options, nil
}

// AdminDistinctProviderGroups 复刻 getDistinctProviderGroupsAction 的 selectDistinct：软删排除。
//
// 返回的是**原始 group_tag 值**（含 NULL 与多分组串），展开与排序由 adminapi 层做
// （对应 Node 把 row.groupTag 交给 resolveProviderGroupsWithDefault 的那一步）。
func (p *Pools) AdminDistinctProviderGroups(ctx context.Context) ([]*string, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT group_tag FROM providers WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询供应商分组失败: %w", err)
	}
	defer rows.Close()

	tags := make([]*string, 0, 8)
	for rows.Next() {
		var tag *string
		if err := rows.Scan(&tag); err != nil {
			return nil, fmt.Errorf("store: 读取供应商分组失败: %w", err)
		}
		tags = append(tags, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历供应商分组失败: %w", err)
	}
	return tags, nil
}

// requestFilterRows 用 control 分道读多行（row_to_json 文本行）。
func requestFilterRows(ctx context.Context, p *Pools, query string, args ...any) ([]AdminRequestFilter, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询请求过滤器失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminRequestFilter, 0, 16)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取请求过滤器行失败: %w", err)
		}
		filter, err := decodeRequestFilter(payload)
		if err != nil {
			return nil, err
		}
		results = append(results, filter)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历请求过滤器行失败: %w", err)
	}
	return results, nil
}

// scanRequestFilter 读 RETURNING 的单行：按 requestFilterColumns 的列顺序扫。
func scanRequestFilter(row interface {
	Scan(dest ...any) error
}) (AdminRequestFilter, error) {
	var filter AdminRequestFilter
	if err := row.Scan(&filter.ID, &filter.Name, &filter.Description, &filter.Scope, &filter.Action,
		&filter.MatchType, &filter.Target, &filter.Replacement, &filter.Priority, &filter.IsEnabled,
		&filter.BindingType, &filter.ProviderIDs, &filter.GroupTags, &filter.RuleMode,
		&filter.ExecutionPhase, &filter.Operations, &filter.CreatedAt, &filter.UpdatedAt); err != nil {
		return AdminRequestFilter{}, err
	}
	return filter, nil
}

// decodeRequestFilter 反序列化一行（query 用 row_to_json，标签与 AdminRequestFilter 一致）。
func decodeRequestFilter(payload string) (AdminRequestFilter, error) {
	var filter AdminRequestFilter
	if err := json.Unmarshal([]byte(payload), &filter); err != nil {
		return AdminRequestFilter{}, fmt.Errorf("store: 请求过滤器行反序列化失败: %w", err)
	}
	return filter, nil
}
