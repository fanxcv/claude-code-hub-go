package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 本文件是 error_rules 表的管理面读写面（批次 A / lane A1-4）。
//
// 唯一真源：src/repository/error-rules.ts。SQL 形状、排序与 NULL 语义逐条对齐该文件；
// 差异只写在注释里。
//
// 分道：读走 control（与 guard 的错误规则快照同源），写走 writer。

// AdminErrorRule 是 error_rules 的一行。
//
// 字段名取 Node 的 camelCase（ErrorRuleSchema，src/lib/api/v1/schemas/error-rules.ts:18-32）：
// 本类型同时充当响应体的形状来源。OverrideResponse 用 json.RawMessage 原样透传——它是
// jsonb，两端读同一份字节；**形状校验不在这里做**（Node 的 sanitizeOverrideResponse 在
// repository 层做，Go 侧对应 adminapi 层的 errorRuleOverrideResponse）。
type AdminErrorRule struct {
	ID      int64  `json:"id"`
	Pattern string `json:"pattern"`
	// MatchType 的列默认是 'regex'，该列 notNull，故不是指针。
	MatchType   string  `json:"matchType"`
	Category    string  `json:"category"`
	Description *string `json:"description"`
	// OverrideResponse 为 nil 表示该列为 NULL（Node 侧即 overrideResponse: null）。
	OverrideResponse   json.RawMessage `json:"overrideResponse"`
	OverrideStatusCode *int            `json:"overrideStatusCode"`
	IsEnabled          bool            `json:"isEnabled"`
	IsDefault          bool            `json:"isDefault"`
	Priority           int             `json:"priority"`
	// CreatedAt / UpdatedAt 为指针：列可空（schema.ts:825-826 只有 defaultNow），Node 侧用
	// `?? new Date()` 兜底，兜底放在 adminapi 层做。
	CreatedAt *string `json:"createdAt"`
	UpdatedAt *string `json:"updatedAt"`
}

// errorRuleColumns 与 getAllErrorRules 的投影一致。
const errorRuleColumns = `id, pattern, match_type AS "matchType", category, description,
	override_response AS "overrideResponse", override_status_code AS "overrideStatusCode",
	is_enabled AS "isEnabled", is_default AS "isDefault", priority,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt",
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "updatedAt"`

// AdminListErrorRules 复刻 getAllErrorRules：含禁用行，按创建时间倒序。
//
// 排序与 drizzle 的 `desc(createdAt)` 一致：PG 对 DESC 默认 NULLS FIRST，drizzle 不发 NULLS
// 子句，故 Node 侧也是 NULLS FIRST。
func (p *Pools) AdminListErrorRules(ctx context.Context) ([]AdminErrorRule, error) {
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + errorRuleColumns +
		` FROM error_rules ORDER BY created_at DESC NULLS FIRST) t`
	return errorRuleRows(ctx, p, query)
}

// AdminListActiveErrorRules 复刻 getActiveErrorRules（判定与统计用）：只取启用行。
//
// 排序 (priority, category) 升序，逐字对齐 Node——命中即第一条，顺序就是可见行为
// （:test 返回的命中规则随之不同）。注意 guard 的错误规则快照用的是 (priority, id)：
// 两者只在「同优先级且不同类别」的并列行上可能给出不同的首命中，属既有实现，不改。
func (p *Pools) AdminListActiveErrorRules(ctx context.Context) ([]AdminErrorRule, error) {
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + errorRuleColumns +
		` FROM error_rules WHERE is_enabled = true ORDER BY priority ASC, category ASC) t`
	return errorRuleRows(ctx, p, query)
}

// AdminGetErrorRuleByID 复刻 getErrorRuleById。
func (p *Pools) AdminGetErrorRuleByID(ctx context.Context, id int64) (*AdminErrorRule, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + errorRuleColumns +
		` FROM error_rules WHERE id = $1) t`
	row := pool.QueryRow(ctx, query, id)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: 读取错误规则失败: %w", err)
	}
	rule, err := decodeErrorRule(payload)
	if err != nil {
		return nil, err
	}
	return &rule, nil
}

// AdminCreateErrorRuleInput 是一次插入。
//
// OverrideResponse 为 nil 表示该列写 NULL；OverrideStatusCode 同理（Node 侧
// `?? null`，null 与 undefined 落库形态一致）。
type AdminCreateErrorRuleInput struct {
	Pattern            string
	MatchType          string
	Category           string
	Description        *string
	OverrideResponse   json.RawMessage
	OverrideStatusCode *int
	// Priority 为 nil 时写 0（Node 侧 `data.priority ?? 0`）。
	Priority *int
}

// AdminCreateErrorRule 复刻 createErrorRule。
func (p *Pools) AdminCreateErrorRule(
	ctx context.Context,
	input AdminCreateErrorRuleInput,
) (AdminErrorRule, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminErrorRule{}, err
	}
	priority := 0
	if input.Priority != nil {
		priority = *input.Priority
	}
	query := `INSERT INTO error_rules
		(pattern, match_type, category, description, override_response, override_status_code, priority)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7) RETURNING ` + errorRuleColumns
	row := pool.QueryRow(ctx, query, input.Pattern, input.MatchType, input.Category,
		input.Description, nullableJSON(input.OverrideResponse), input.OverrideStatusCode, priority)
	created, err := scanErrorRule(row)
	if err != nil {
		return AdminErrorRule{}, fmt.Errorf("store: 写入错误规则失败: %w", err)
	}
	return created, nil
}

// AdminErrorRuleUpdate 是一次部分更新：每个字段的「零值即不写」由包一层可空类型表达，
// 因为 Node 侧 `.set({...data})` 只写请求里出现的键，而可空列「写 NULL」与「不写」是两回事。
type AdminErrorRuleUpdate struct {
	Pattern            *string
	MatchType          *string
	Category           *string
	Description        AdminNullableText
	OverrideResponse   AdminNullableJSON
	OverrideStatusCode AdminNullableInt
	IsEnabled          *bool
	IsDefault          *bool
	Priority           *int
}

// AdminUpdateErrorRule 复刻 updateErrorRule：部分更新 + updated_at 置当前时间。
//
// 记录不存在时返回 ErrNotFound（Node 侧 returning() 为空即 null，handler 映射成 404）。
func (p *Pools) AdminUpdateErrorRule(
	ctx context.Context,
	id int64,
	update AdminErrorRuleUpdate,
) (AdminErrorRule, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminErrorRule{}, err
	}
	assignments := make([]string, 0, 10)
	args := make([]any, 0, 10)
	// set 把值入队并生成 `列 = $n`；jsonValue 为 true 时附加 ::jsonb 转型。
	set := func(column string, value any, jsonValue bool) {
		args = append(args, value)
		placeholder := "$" + strconv.Itoa(len(args))
		if jsonValue {
			placeholder += "::jsonb"
		}
		assignments = append(assignments, column+" = "+placeholder)
	}
	if update.Pattern != nil {
		set("pattern", *update.Pattern, false)
	}
	if update.MatchType != nil {
		set("match_type", *update.MatchType, false)
	}
	if update.Category != nil {
		set("category", *update.Category, false)
	}
	if update.Description.Present {
		set("description", update.Description.Value, false)
	}
	if update.OverrideResponse.Present {
		set("override_response", nullableJSON(update.OverrideResponse.Raw), true)
	}
	if update.OverrideStatusCode.Present {
		set("override_status_code", update.OverrideStatusCode.Value, false)
	}
	if update.IsEnabled != nil {
		set("is_enabled", *update.IsEnabled, false)
	}
	if update.IsDefault != nil {
		set("is_default", *update.IsDefault, false)
	}
	if update.Priority != nil {
		set("priority", *update.Priority, false)
	}
	// updatedAt 恒被写入（Node 侧也如此），因此 SET 列表永不为空。
	assignments = append(assignments, "updated_at = now()")
	args = append(args, id)

	query := fmt.Sprintf("UPDATE error_rules SET %s WHERE id = $%d RETURNING %s",
		strings.Join(assignments, ", "), len(args), errorRuleColumns)
	updated, err := scanErrorRule(pool.QueryRow(ctx, query, args...))
	if err != nil {
		if isNoRows(err) {
			return AdminErrorRule{}, ErrNotFound
		}
		return AdminErrorRule{}, fmt.Errorf("store: 更新错误规则失败: %w", err)
	}
	return updated, nil
}

// AdminDeleteErrorRule 复刻 deleteErrorRule：硬删除，返回是否删掉了行。
func (p *Pools) AdminDeleteErrorRule(ctx context.Context, id int64) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	tag, err := pool.Exec(ctx, "DELETE FROM error_rules WHERE id = $1", id)
	if err != nil {
		return false, fmt.Errorf("store: 删除错误规则失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AdminErrorRuleSyncResult 是 syncDefaultErrorRules 的计数（字段名即响应体形状）。
type AdminErrorRuleSyncResult struct {
	Inserted int `json:"inserted"`
	Updated  int `json:"updated"`
	Skipped  int `json:"skipped"`
	Deleted  int `json:"deleted"`
}

// AdminSyncDefaultErrorRules 复刻 syncDefaultErrorRules：把代码里的默认规则同步进库。
//
// 策略与 Node 一致：pattern 不存在则插入；存在且 is_default=true 则更新为最新默认值；存在但
// 已被用户改过（is_default=false）则跳过；库里的默认规则若已不在默认表里则删除。
//
// 单事务完成：Node 侧也是 `db.transaction(...)`，中途失败不能留下半同步状态。
func (p *Pools) AdminSyncDefaultErrorRules(ctx context.Context) (AdminErrorRuleSyncResult, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminErrorRuleSyncResult{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return AdminErrorRuleSyncResult{}, fmt.Errorf("store: 开启默认规则同步事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	patterns := make([]string, 0, len(adminDefaultErrorRules))
	for _, rule := range adminDefaultErrorRules {
		patterns = append(patterns, rule.Pattern)
	}

	var result AdminErrorRuleSyncResult

	// 1. 删除库里已不在默认表里的默认规则。
	staleRows, err := tx.Query(ctx, "SELECT id, pattern FROM error_rules WHERE is_default = true")
	if err != nil {
		return AdminErrorRuleSyncResult{}, fmt.Errorf("store: 读取现有默认规则失败: %w", err)
	}
	staleIDs := make([]int64, 0, 8)
	for staleRows.Next() {
		var id int64
		var pattern string
		if err := staleRows.Scan(&id, &pattern); err != nil {
			staleRows.Close()
			return AdminErrorRuleSyncResult{}, fmt.Errorf("store: 读取现有默认规则失败: %w", err)
		}
		if !slicesContains(patterns, pattern) {
			staleIDs = append(staleIDs, id)
		}
	}
	staleRows.Close()
	if err := staleRows.Err(); err != nil {
		return AdminErrorRuleSyncResult{}, fmt.Errorf("store: 遍历现有默认规则失败: %w", err)
	}
	for _, id := range staleIDs {
		if _, err := tx.Exec(ctx, "DELETE FROM error_rules WHERE id = $1", id); err != nil {
			return AdminErrorRuleSyncResult{}, fmt.Errorf("store: 删除过期默认规则失败: %w", err)
		}
		result.Deleted++
	}

	// 2. 取这些 pattern 在库里的 is_default 现状（含用户自定义行）。
	existing, err := tx.Query(ctx,
		"SELECT pattern, is_default FROM error_rules WHERE pattern = ANY($1)", patterns)
	if err != nil {
		return AdminErrorRuleSyncResult{}, fmt.Errorf("store: 读取同 pattern 规则失败: %w", err)
	}
	defaults := make(map[string]bool, len(patterns))
	for existing.Next() {
		var pattern string
		var isDefault bool
		if err := existing.Scan(&pattern, &isDefault); err != nil {
			existing.Close()
			return AdminErrorRuleSyncResult{}, fmt.Errorf("store: 读取同 pattern 规则失败: %w", err)
		}
		defaults[pattern] = isDefault
	}
	existing.Close()
	if err := existing.Err(); err != nil {
		return AdminErrorRuleSyncResult{}, fmt.Errorf("store: 遍历同 pattern 规则失败: %w", err)
	}

	// 3. 逐条插入或更新。
	for _, rule := range adminDefaultErrorRules {
		isDefault, present := defaults[rule.Pattern]
		switch {
		case !present:
			tag, err := tx.Exec(ctx, `INSERT INTO error_rules
				(pattern, category, match_type, description, override_response, override_status_code,
				 is_enabled, is_default, priority)
				VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, true, $8)
				ON CONFLICT (pattern) DO NOTHING`,
				rule.Pattern, rule.Category, rule.MatchType, rule.Description,
				nullableJSON(rule.OverrideResponse), rule.OverrideStatusCode, rule.IsEnabled, rule.Priority)
			if err != nil {
				return AdminErrorRuleSyncResult{}, fmt.Errorf("store: 插入默认规则失败: %w", err)
			}
			// ON CONFLICT DO NOTHING 命中时行数为 0：Node 侧把它计入 skipped。
			if tag.RowsAffected() > 0 {
				result.Inserted++
			} else {
				result.Skipped++
			}
		case isDefault:
			if _, err := tx.Exec(ctx, `UPDATE error_rules SET
				match_type = $1, category = $2, description = $3, override_response = $4::jsonb,
				override_status_code = $5, is_enabled = $6, is_default = true, priority = $7,
				updated_at = now()
				WHERE pattern = $8`,
				rule.MatchType, rule.Category, rule.Description, nullableJSON(rule.OverrideResponse),
				rule.OverrideStatusCode, rule.IsEnabled, rule.Priority, rule.Pattern); err != nil {
				return AdminErrorRuleSyncResult{}, fmt.Errorf("store: 更新默认规则失败: %w", err)
			}
			result.Updated++
		default:
			// 已被用户自定义（is_default=false）：保留用户版本。
			result.Skipped++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return AdminErrorRuleSyncResult{}, fmt.Errorf("store: 提交默认规则同步失败: %w", err)
	}
	return result, nil
}

// AdminNullableText / AdminNullableJSON / AdminNullableInt 表示一次「可空列写入」：
// Present 为 true 时该列进入 SET，Value/Raw 为 nil 即写 NULL。
//
// 三者与 AdminErrorRuleUpdate / AdminRequestFilterUpdate 共用：Node 侧 `.set({...data})` 的
// 「键是否出现」语义无法用 Go 的零值表达，包一层是唯一不靠约定的写法。
type AdminNullableText struct {
	Present bool
	Value   *string
}

// AdminNullableJSON 见 AdminNullableText 的说明。
type AdminNullableJSON struct {
	Present bool
	Raw     json.RawMessage
}

// AdminNullableInt 见 AdminNullableText 的说明。
type AdminNullableInt struct {
	Present bool
	Value   *int
}

// nullableJSON 把「JSON 字段」转成可绑定的参数：空值写 SQL NULL，非空写字节串（配合 ::jsonb）。
//
// 为什么要显式区分：Node 的 drizzle jsonb 列在值为 null/undefined 时写 SQL NULL，而不是 JSON
// 的 null 字面量。写错会让 `replacement` / `override_response` 在库里变成 'null'::jsonb，
// 于是响应里出现 null 而 DB 判空（IS NULL）失效。
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return string(raw)
}

// errorRuleRows 用 control 分道读多行（row_to_json 文本行）。
func errorRuleRows(ctx context.Context, p *Pools, query string, args ...any) ([]AdminErrorRule, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询错误规则失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminErrorRule, 0, 16)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取错误规则行失败: %w", err)
		}
		rule, err := decodeErrorRule(payload)
		if err != nil {
			return nil, err
		}
		results = append(results, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历错误规则行失败: %w", err)
	}
	return results, nil
}

// scanErrorRule 读 RETURNING 的单行：按 errorRuleColumns 的列顺序扫。
func scanErrorRule(row interface {
	Scan(dest ...any) error
}) (AdminErrorRule, error) {
	var rule AdminErrorRule
	if err := row.Scan(&rule.ID, &rule.Pattern, &rule.MatchType, &rule.Category, &rule.Description,
		&rule.OverrideResponse, &rule.OverrideStatusCode, &rule.IsEnabled, &rule.IsDefault,
		&rule.Priority, &rule.CreatedAt, &rule.UpdatedAt); err != nil {
		return AdminErrorRule{}, err
	}
	return rule, nil
}

// decodeErrorRule 反序列化一行（query 用 row_to_json，标签与 AdminErrorRule 的 json 标签一致）。
func decodeErrorRule(payload string) (AdminErrorRule, error) {
	var rule AdminErrorRule
	if err := json.Unmarshal([]byte(payload), &rule); err != nil {
		return AdminErrorRule{}, fmt.Errorf("store: 错误规则行反序列化失败: %w", err)
	}
	return rule, nil
}

// slicesContains 是「小切片包含」的本地实现：默认表只有几十条，不值得为它拉进 slices 包。
func slicesContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// adminRuleText / adminRuleInt 让默认规则表里的可空字段保持一行一条。
func adminRuleText(value string) *string { return &value }

func adminRuleInt(value int) *int { return &value }

// AdminDefaultErrorRule 是默认错误规则表的一行（Node 的 DEFAULT_ERROR_RULES 元素）。
type AdminDefaultErrorRule struct {
	Pattern            string
	Category           string
	MatchType          string
	Description        *string
	OverrideResponse   json.RawMessage
	OverrideStatusCode *int
	IsEnabled          bool
	Priority           int
}

// adminNodeDefaultErrorRules 是 Node 的 DEFAULT_ERROR_RULES（src/repository/error-rules.ts:291-895）
// 的原样副本。
//
// 顺序不重要（同步按 pattern 匹配），但**内容必须逐字一致**：pattern 参与唯一索引与用户自定义
// 判定，override_response 是用户可见的覆写体。改动这里等于改动线上默认规则。
//
// Go 侧增补的规则另立 adminUnsupportedInputErrorRules，由 adminDefaultErrorRules 拼在其后。
var adminNodeDefaultErrorRules = []AdminDefaultErrorRule{
	{
		Pattern:          "Missing or invalid 'alt' query parameter. Expected 'alt=sse'",
		Category:         "parameter_error",
		MatchType:        "contains",
		Description:      adminRuleText("Not supported non-streaming request"),
		Priority:         105,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"error\":{\"code\":400,\"status\":\"INVALID_ARGUMENT\",\"message\":\"当前中转站不支持 Gemini generateContent 端点\"}}"),
	},
	{
		Pattern:          "prompt is too long.*(tokens.*maximum|maximum.*tokens)",
		Category:         "prompt_limit",
		MatchType:        "regex",
		Description:      adminRuleText("Prompt token limit exceeded"),
		Priority:         100,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"prompt_limit\",\"message\":\"输入内容过长，请减少 Prompt 中的 token 数量后重试\"}}"),
	},
	{
		Pattern:          "Input is too long",
		Category:         "input_limit",
		MatchType:        "contains",
		Description:      adminRuleText("Input content length exceeds provider limit"),
		Priority:         95,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"input_limit\",\"message\":\"输入内容超过供应商限制，请减少输入长度后重试\"}}"),
	},
	{
		Pattern:          "CONTENT_LENGTH_EXCEEDS_THRESHOLD",
		Category:         "input_limit",
		MatchType:        "contains",
		Description:      adminRuleText("AWS Bedrock content length threshold exceeded"),
		Priority:         94,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"input_limit\",\"message\":\"内容长度超过阈值限制，请减少输入内容后重试\"}}"),
	},
	{
		Pattern:          "ValidationException",
		Category:         "validation_error",
		MatchType:        "contains",
		Description:      adminRuleText("AWS/Bedrock validation error (non-retryable)"),
		Priority:         93,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"validation_error\",\"message\":\"请求参数验证失败，请检查请求格式是否正确\"}}"),
	},
	{
		Pattern:          "context.*(length|window|limit).*exceed|exceed.*(context|token|length).*(limit|window)",
		Category:         "context_limit",
		MatchType:        "regex",
		Description:      adminRuleText("Context window or token limit exceeded"),
		Priority:         92,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"context_limit\",\"message\":\"上下文长度超过模型限制，请减少对话历史或输入内容\"}}"),
	},
	{
		Pattern:          "max_tokens.*exceed|exceed.*max_tokens|maximum.*tokens.*allowed",
		Category:         "token_limit",
		MatchType:        "regex",
		Description:      adminRuleText("Max tokens parameter exceeds model limit"),
		Priority:         90,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"token_limit\",\"message\":\"max_tokens 参数超过模型允许的最大值，请降低该参数\"}}"),
	},
	{
		Pattern:          "pricing plan does not include Long Context",
		Category:         "context_limit",
		MatchType:        "contains",
		Description:      adminRuleText("Provider pricing plan does not support Long Context prompts"),
		Priority:         91,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"context_limit\",\"message\":\"当前供应商套餐不支持长上下文，请切换供应商或减少输入\"}}"),
	},
	{
		Pattern:          "blocked by.*content filter",
		Category:         "content_filter",
		MatchType:        "regex",
		Description:      adminRuleText("Content blocked by safety filters"),
		Priority:         90,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"content_filter\",\"message\":\"内容被安全过滤器拦截，请修改输入内容后重试\"}}"),
	},
	{
		Pattern:            "cyber_policy|flagged for possible cybersecurity risk",
		Category:           "content_filter",
		MatchType:          "regex",
		Description:        adminRuleText("OpenAI cyber policy violation (non-retryable)"),
		Priority:           90,
		IsEnabled:          true,
		OverrideResponse:   json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"内容触发了安全策略拦截 (cyber_policy)，请调整输入后重试\"}}"),
		OverrideStatusCode: adminRuleInt(400),
	},
	{
		Pattern:          "`tool_use` ids must be unique|tool_use.*ids must be unique",
		Category:         "validation_error",
		MatchType:        "regex",
		Description:      adminRuleText("Duplicate tool_use IDs in request (client error)"),
		Priority:         89,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"validation_error\",\"message\":\"tool_use ID 重复，请确保每个工具调用使用唯一 ID\"}}"),
	},
	{
		Pattern:          "all messages must have non-empty content",
		Category:         "validation_error",
		MatchType:        "contains",
		Description:      adminRuleText("Message content is empty (client error)"),
		Priority:         89,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"validation_error\",\"message\":\"消息内容不能为空，请确保所有消息都有有效内容（最后一条 assistant 消息除外）\"}}"),
	},
	{
		Pattern:          "Tool names must be unique",
		Category:         "validation_error",
		MatchType:        "contains",
		Description:      adminRuleText("Duplicate tool names in request (client error, related to MCP server configuration)"),
		Priority:         89,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"validation_error\",\"message\":\"工具名称重复，请检查 MCP 服务器配置确保工具名称唯一\"}}"),
	},
	{
		Pattern:          "String should match pattern.*srvtoolu_|server_tool_use.*id.*should.*match",
		Category:         "validation_error",
		MatchType:        "regex",
		Description:      adminRuleText("server_tool_use.id format validation error (client error)"),
		Priority:         89,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"validation_error\",\"message\":\"server_tool_use.id 格式错误，必须以 srvtoolu_ 开头且仅包含字母、数字和下划线\"}}"),
	},
	{
		Pattern:          "unexpected.*['\"]tool_use_id['\"].*found in.*['\"]tool_result['\"]|messages\\..*\\.content\\..*: unexpected ['\"]tool_use_id['\"].*['\"]tool_result['\"]",
		Category:         "validation_error",
		MatchType:        "regex",
		Description:      adminRuleText("tool_use_id field incorrectly placed in tool_result blocks (client error)"),
		Priority:         89,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"validation_error\",\"message\":\"tool_result 块中不应包含 tool_use_id 字段，请检查消息格式\"}}"),
	},
	{
		Pattern:          "unexpected.*tool_use_id.*tool_result|tool_result.*must have.*corresponding.*tool_use",
		Category:         "validation_error",
		MatchType:        "regex",
		Description:      adminRuleText("tool_result block missing corresponding tool_use (client error)"),
		Priority:         88,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"validation_error\",\"message\":\"tool_result 缺少对应的 tool_use，请检查工具调用链\"}}"),
	},
	{
		Pattern:          "tool_use.*ids were found without.*tool_result.*immediately after|tool_use.*block must have.*corresponding.*tool_result.*block in the next message",
		Category:         "validation_error",
		MatchType:        "regex",
		Description:      adminRuleText("tool_use block missing corresponding tool_result in next message (client error)"),
		Priority:         88,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"validation_error\",\"message\":\"tool_use 块缺少对应的 tool_result，请确保每个 tool_use 在下一条消息中有对应的 tool_result 块\"}}"),
	},
	{
		Pattern:          "\"actualModel\" is null|actualModel.*null",
		Category:         "model_error",
		MatchType:        "regex",
		Description:      adminRuleText("Model parameter is null (Java NPE)"),
		Priority:         88,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"model_error\",\"message\":\"模型参数为空，请检查请求中的 model 字段\"}}"),
	},
	{
		Pattern:          "unknown model|model.*not.*found|model.*does.*not.*exist",
		Category:         "model_error",
		MatchType:        "regex",
		Description:      adminRuleText("Unknown or non-existent model"),
		Priority:         87,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"model_error\",\"message\":\"未知模型，请检查模型名称是否正确\"}}"),
	},
	{
		Pattern:          "model is required",
		Category:         "model_error",
		MatchType:        "contains",
		Description:      adminRuleText("Model parameter is required but missing"),
		Priority:         86,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"model_error\",\"message\":\"缺少必需的 model 参数，请在请求中指定模型名称\"}}"),
	},
	{
		Pattern:          "模型名称.*为空|模型名称不能为空|未指定模型",
		Category:         "model_error",
		MatchType:        "regex",
		Description:      adminRuleText("Model name is empty or not specified (Chinese)"),
		Priority:         86,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"model_error\",\"message\":\"模型名称不能为空，请指定有效的模型名称\"}}"),
	},
	{
		Pattern:          "PDF has too many pages|maximum of.*PDF pages",
		Category:         "pdf_limit",
		MatchType:        "regex",
		Description:      adminRuleText("PDF page limit exceeded"),
		Priority:         80,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"pdf_limit\",\"message\":\"PDF 页数超过限制（通常为 100 页），请减少页数后重试\"}}"),
	},
	{
		Pattern:          "Too much media",
		Category:         "media_limit",
		MatchType:        "contains",
		Description:      adminRuleText("Total media count (document pages + images) exceeds API limit"),
		Priority:         79,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"media_limit\",\"message\":\"媒体数量超过限制（文档页数 + 图片数量 > 100），请减少图片或文档页数后重试\"}}"),
	},
	{
		Pattern:          "expected\\s*`?thinking`?\\s*or\\s*`?redacted_thinking`?[^\\n]*found\\s*`?tool_use`?",
		Category:         "thinking_error",
		MatchType:        "regex",
		Description:      adminRuleText("Thinking enabled but the last assistant tool_use message does not start with thinking/redacted_thinking"),
		Priority:         68,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"thinking_error\",\"message\":\"thinking 已启用，但工具调用续写的 assistant 消息未以 thinking/redacted_thinking 块开头。请在 tool_result 续写请求中原样回传上一轮 assistant 的 thinking/redacted_thinking 块（含 signature/data），或关闭 thinking 后重试。\"}}"),
	},
	{
		Pattern:          "must start with a thinking block",
		Category:         "thinking_error",
		MatchType:        "contains",
		Description:      adminRuleText("Thinking enabled but the last assistant tool_use message does not start with thinking/redacted_thinking"),
		Priority:         69,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"thinking_error\",\"message\":\"thinking 已启用，但工具调用续写的 assistant 消息未以 thinking/redacted_thinking 块开头。请在 tool_result 续写请求中原样回传上一轮 assistant 的 thinking/redacted_thinking 块（含 signature/data），或关闭 thinking 后重试。\"}}"),
	},
	{
		Pattern:          "thinking.*format.*invalid|Expected.*thinking.*but found|clear_thinking.*requires.*thinking.*enabled",
		Category:         "thinking_error",
		MatchType:        "regex",
		Description:      adminRuleText("Invalid thinking block format or configuration"),
		Priority:         70,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"thinking_error\",\"message\":\"thinking 块格式无效，请检查配置或请求参数\"}}"),
	},
	{
		Pattern:          "Invalid `signature` in `thinking` block",
		Category:         "thinking_error",
		MatchType:        "contains",
		Description:      adminRuleText("Invalid signature in thinking block (occurs when switching between Anthropic and non-Anthropic channels)"),
		Priority:         71,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"thinking_error\",\"message\":\"thinking 块签名无效，通常发生在 Anthropic 渠道与非 Anthropic 渠道切换时，请检查请求格式\"}}"),
	},
	{
		Pattern:          "signature.*Field required",
		Category:         "thinking_error",
		MatchType:        "regex",
		Description:      adminRuleText("Missing signature field in thinking block (cross-channel format incompatibility)"),
		Priority:         72,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"thinking_error\",\"message\":\"thinking block 缺少必需的 signature 字段。该问题通常发生在从非 Anthropic 渠道切换到 Anthropic 渠道时。请确保 tool_result 续写请求中原样回传上一轮 assistant 的 thinking/redacted_thinking 块（含 signature/data），或关闭 thinking 后重试。\"}}"),
	},
	{
		Pattern:          "Missing required parameter|Extra inputs.*not permitted",
		Category:         "parameter_error",
		MatchType:        "regex",
		Description:      adminRuleText("Request parameter validation failed"),
		Priority:         60,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"parameter_error\",\"message\":\"缺少必需参数或包含不允许的参数，请检查请求格式\"}}"),
	},
	{
		Pattern:          "非法请求|illegal request|invalid request",
		Category:         "invalid_request",
		MatchType:        "regex",
		Description:      adminRuleText("Invalid request format"),
		Priority:         50,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"invalid_request\",\"message\":\"请求格式非法，请检查请求结构是否符合 API 规范\"}}"),
	},
	{
		Pattern:          "(cache_control.*(limit|maximum).*blocks|(maximum|limit).*blocks.*cache_control)",
		Category:         "cache_limit",
		MatchType:        "regex",
		Description:      adminRuleText("Cache control limit exceeded"),
		Priority:         40,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"cache_limit\",\"message\":\"cache_control 块数量超过限制，请减少缓存块数量\"}}"),
	},
	{
		Pattern:          "image exceeds.*maximum.*bytes",
		Category:         "invalid_request",
		MatchType:        "regex",
		Description:      adminRuleText("Image size exceeds maximum limit"),
		Priority:         35,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"invalid_request\",\"message\":\"图片大小超过最大限制，请压缩图片后重试\"}}"),
	},
	{
		Pattern:          "Unsupported value.*is not supported with.*model.*Supported values",
		Category:         "thinking_error",
		MatchType:        "regex",
		Description:      adminRuleText("Reasoning effort (thinking intensity) not supported by the Codex model"),
		Priority:         72,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"type\":\"error\",\"error\":{\"type\":\"thinking_error\",\"message\":\"当前思考强度不支持该模型，请调整 reasoning_effort 参数或切换模型（如 'high' 仅支持部分模型）\"}}"),
	},
	{
		Pattern:          "Items are not persisted when",
		Category:         "store_error",
		MatchType:        "contains",
		Description:      adminRuleText("OpenAI Responses API item not found due to store=false"),
		Priority:         73,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"error\":{\"message\":\"引用的 Response 项已失效（上游 store=false 导致项未持久化）。请在 Provider 自定义请求体中设置 store=true，或移除输入中的历史项 ID（rs_xxx）后重试\",\"type\":\"invalid_request_error\",\"param\":null,\"code\":null}}"),
	},
	{
		Pattern:          "Input must be a list",
		Category:         "parameter_error",
		MatchType:        "contains",
		Description:      adminRuleText("OpenAI Responses API input field is not an array"),
		Priority:         74,
		IsEnabled:        true,
		OverrideResponse: json.RawMessage("{\"error\":{\"message\":\"Responses API 的 input 参数必须为数组格式。请检查请求体中 input 字段是否为列表\",\"type\":\"invalid_request_error\",\"param\":\"input\",\"code\":null}}"),
	},
}

// adminUnsupportedInputErrorRules 是 Go 侧增补的一族默认规则（Node 的默认表里没有）：
// 上游**明确声明该客户端输入形态不受支持**时，命中即归 CategoryNonRetryableClientError
// （不重试当前供应商、不切换，见 forward/errors.go 的 Classify 第 7 步）。
//
// 动机（2026-09-16 生产实测）：上游对「图片以 URL 形态送达」回 `image URLs are not currently
// supported, please use base64 encoded data instead`，此前无规则命中 ⇒ 归 CategoryProviderError
// （可重试且可切换）⇒ 同一份必然再被拒的输入被逐家重试，实测耗掉 20 次尝试、14.7s 才放弃。
//
// 判据刻意**窄**：必须同时出现「模态」（image/audio/video/file/pdf/document）与「载体形态」
// （url/link），即争的是这**一种送达形态**而非模态本身。因此下列情形**有意不命中**，仍走
// 重试与切换（换一家供应商可能成功，属供应商能力差异而非客户端输入错误）：
//   - 模态陈述：「This model does not support image inputs」「本模型不支持图片识别」「不支持图片」
//   - 模型/参数能力：「unsupported value for reasoning_effort」「model does not support tool calling」
//
// 有意不给 override_response：本仓的覆写响应尚未落到响应路径（见 guard/adapters_rules.go 的
// 说明与 Assembly.Missing），写了也是空承诺；上游原文如实回给客户端。
var adminUnsupportedInputErrorRules = []AdminDefaultErrorRule{
	{
		Pattern:     `(image|audio|video|file|pdf|document|media)s?\s+(urls?|links?)\s+(is|are)\s+not\s+(currently\s+)?supported`,
		Category:    "invalid_request",
		MatchType:   "regex",
		Description: adminRuleText("媒体以 URL/链接形态送达不被上游支持（非重试）"),
		Priority:    78,
		IsEnabled:   true,
	},
	{
		Pattern:     `unsupported\s+(image|audio|video|file|pdf|document)\s+(url|urls|link|links)`,
		Category:    "invalid_request",
		MatchType:   "regex",
		Description: adminRuleText("上游声明该媒体 URL 形态不受支持（非重试）"),
		Priority:    77,
		IsEnabled:   true,
	},
	{
		Pattern:     `(图片|音频|视频|文件)(URL|url|链接)(形式|格式|类型|方式)?(暂|当前|目前)?不支持|不支持(图片|音频|视频|文件)(URL|url|链接)`,
		Category:    "invalid_request",
		MatchType:   "regex",
		Description: adminRuleText("中文上游声明该媒体 URL 形态不受支持（非重试）"),
		Priority:    76,
		IsEnabled:   true,
	},
}

// adminDefaultErrorRules 是同步进库的完整默认表：Node 原表在前，Go 侧增补在后。
//
// 顺序只影响装载顺序——判定是「任一命中即命中」（guard/adapters_rules.go 的 matches），
// 故增补放尾部不改变既有规则的命中结果。
var adminDefaultErrorRules = append(
	append([]AdminDefaultErrorRule{}, adminNodeDefaultErrorRules...),
	adminUnsupportedInputErrorRules...,
)
