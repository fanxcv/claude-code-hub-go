package adminapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件把 Node 的查询参数解析逐条移植（src/lib/api/v1/schemas/usage-logs.ts 的
// UsageLogsQuerySchema 与 handlers.ts 的 parseUsageLogsQuery），包括 zod 的三处易错语义：
//
//   - **coerce 语义**：`Number("") === 0`，所以「给了但为空」的数值参数是 0 而不是缺失；
//     非数值则是校验失败（NaN）。
//   - **默认值**：limit 缺席时是 20（不是 50）——这个 20 会出现在 pageInfo.limit 与
//     偏移路径的 pageSize 兜底里，所以不能按「反正会被别的默认值覆盖」省掉。
//   - **cursor 的装配**：只有 cursorCreatedAt 与 cursorId 同时存在才构成游标。
//
// 校验失败一律 `request.validation_failed` + invalidParams（error-envelope.ts:69 的
// fromZodError）。invalidParams 的 path/code/message 尽量对齐 zod；个别边缘文案的差异登记在
// 白名单里（ 第 3 项的既有裁决：Go 用显式错误码表，
// 不照抄中文子串判定）。

// usageLogsValidationIssue 复刻 error-envelope.ts:22 的 invalidParams 元素。
type usageLogsValidationIssue struct {
	Path    []any  `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// usageLogsValidationProblem 是带 invalidParams 的 Problem 响应体。
//
// 为什么不复用 Problems.WriteProblem：冻结面里的 WriteProblem 表达不了 invalidParams，
// 而 Node 的校验失败**必须**带这个字段（前端按它高亮表单）。加字段要改共享的 problem.go
// （五路并行期间禁止），故在本包这一侧自建。
type usageLogsValidationProblem struct {
	Type          string                     `json:"type"`
	Title         string                     `json:"title"`
	Status        int                        `json:"status"`
	Detail        string                     `json:"detail"`
	Instance      string                     `json:"instance"`
	ErrorCode     string                     `json:"errorCode"`
	InvalidParams []usageLogsValidationIssue `json:"invalidParams"`
}

// usageLogsQuery 是解析后的查询参数（复刻 UsageLogsActionQueryInput）。
type usageLogsQuery struct {
	Limit      int
	hasLimit   bool
	Page       *int
	PageSize   *int
	SessionID  string
	UserID     *int64
	KeyID      *int64
	ProviderID *int64
	Model      string
	Endpoint   string
	StatusCode *int
	StatusExcl *bool // excludeStatusCode200
	Mismatch   bool  // actualResponseModelMismatch
	MinRetry   int
	Replay     store.UsageLogReplayFilter
	StartTime  *int64
	EndTime    *int64
	Cursor     *store.UsageLogCursor
	// SinceID 非 nil 表示**增量读**（只取 id 大于该水位的行，按 id 升序）。
	// 它与 asc 必须成对出现（见 incrementalIssues），也与偏移分页互斥。
	SinceID *int64
	Asc     bool
	Term    string
}

// parseUsageLogsQuery 复刻 parseUsageLogsQuery；校验失败时返回 issue 列表。
func parseUsageLogsQuery(request *http.Request) (usageLogsQuery, []usageLogsValidationIssue) {
	values := request.URL.Query()
	issues := make([]usageLogsValidationIssue, 0, 4)
	query := usageLogsQuery{Limit: 20}

	// limit：int 1..100，默认 20。
	switch raw, ok, err := coerceInt(values, "limit", 1, 100); {
	case err != nil:
		issues = append(issues, *err)
	case ok:
		query.Limit = raw
		query.hasLimit = true
	}

	if raw, ok, err := coerceOptionalInt(values, "page", 1, 0); err != nil {
		issues = append(issues, *err)
	} else if ok {
		query.Page = &raw
	}
	if raw, ok, err := coerceOptionalInt(values, "pageSize", 1, 100); err != nil {
		issues = append(issues, *err)
	} else if ok {
		query.PageSize = &raw
	}

	// cursorId 要求正整数。
	if raw, ok, err := coerceOptionalInt(values, "cursorId", 1, 0); err != nil {
		issues = append(issues, *err)
	} else if ok {
		cursorID := raw
		if createdAt := values.Get("cursorCreatedAt"); createdAt != "" {
			query.Cursor = &store.UsageLogCursor{CreatedAt: createdAt, ID: int64(cursorID)}
		}
	}

	query.SessionID = values.Get("sessionId")
	query.Model = values.Get("model")
	query.Endpoint = values.Get("endpoint")
	query.Term = firstNonEmpty(values.Get("term"), values.Get("q"))

	if raw, ok, err := coerceOptionalNumber(values, "userId"); err != nil {
		issues = append(issues, *err)
	} else if ok {
		userId := int64(raw)
		query.UserID = &userId
	}
	if raw, ok, err := coerceOptionalNumber(values, "keyId"); err != nil {
		issues = append(issues, *err)
	} else if ok {
		keyID := int64(raw)
		query.KeyID = &keyID
	}
	if raw, ok, err := coerceOptionalNumber(values, "providerId"); err != nil {
		issues = append(issues, *err)
	} else if ok {
		providerID := int64(raw)
		query.ProviderID = &providerID
	}
	if raw, ok, err := coerceOptionalNumber(values, "startTime"); err != nil {
		issues = append(issues, *err)
	} else if ok {
		startTime := int64(raw)
		query.StartTime = &startTime
	}
	if raw, ok, err := coerceOptionalNumber(values, "endTime"); err != nil {
		issues = append(issues, *err)
	} else if ok {
		endTime := int64(raw)
		query.EndTime = &endTime
	}
	if raw, ok, err := coerceOptionalInt(values, "statusCode", 0, 0); err != nil {
		issues = append(issues, *err)
	} else if ok {
		query.StatusCode = &raw
	}
	if raw, ok, err := coerceOptionalInt(values, "minRetryCount", 0, 0); err != nil {
		issues = append(issues, *err)
	} else if ok {
		query.MinRetry = raw
	}
	// sinceId：增量水位，必须非负整数（越界/非整数按既有 invalid_type / too_small 作答）。
	if raw, ok, err := coerceOptionalNonNegativeInt64(values, "sinceId"); err != nil {
		issues = append(issues, *err)
	} else if ok {
		query.SinceID = &raw
	}
	// asc：只接受 true/false（与其余布尔参数同形）。
	if raw, ok, err := coerceOptionalBool(values, "asc"); err != nil {
		issues = append(issues, *err)
	} else if ok {
		query.Asc = *raw
	}
	if raw, ok, err := coerceOptionalBool(values, "actualResponseModelMismatch"); err != nil {
		issues = append(issues, *err)
	} else if ok {
		query.Mismatch = *raw
	}
	if raw, ok, err := coerceOptionalBool(values, "excludeStatusCode200"); err != nil {
		issues = append(issues, *err)
	} else if ok {
		query.StatusExcl = raw
	}
	if raw := values.Get("replayFilter"); raw != "" {
		switch store.UsageLogReplayFilter(raw) {
		case store.ReplayFilterAll, store.ReplayFilterReplay, store.ReplayFilterNonReplay:
			query.Replay = store.UsageLogReplayFilter(raw)
		default:
			issues = append(issues, usageLogsValidationIssue{
				Path: []any{"replayFilter"},
				Code: "invalid_enum_value",
				Message: "Invalid enum value. Expected 'all' | 'replay' | 'non-replay', received '" +
					raw + "'",
			})
		}
	}
	issues = append(issues, incrementalIssues(query)...)
	return query, issues
}

// incrementalIssues 校验**增量读**的三个契约约束（跨字段，故不能放在单字段解析里）：
//
//  1. `sinceId` 与 `asc=true` 必须**成对**出现：只给其中一个时语义未定义（契约只定义了
//     「两者同时给 → id > sinceId 升序」与「都不给 → 既有降序行为」），故 fail-closed 报 400，
//     而不是自创一种方向；
//  2. `asc=true` 单独出现同理（没有下界时「升序」只会返回最旧的行，不是增量）。
//
// 前端侧固定发 `?sinceId=<max>&asc=true` 即可，不会碰到这两条。
func incrementalIssues(query usageLogsQuery) []usageLogsValidationIssue {
	incremental := query.SinceID != nil && query.Asc
	if query.SinceID == nil && !query.Asc {
		return nil
	}
	if !incremental {
		missing := "asc"
		message := "增量读需要同时提供 asc=true"
		if query.SinceID == nil {
			missing = "sinceId"
			message = "增量读需要同时提供 sinceId"
		}
		return []usageLogsValidationIssue{{Path: []any{missing}, Code: "custom", Message: message}}
	}
	// 增量与偏移分页互斥：两者的分页语义不同，静默取一个会得到「看着对、其实错」的结果。
	if query.Page != nil || query.PageSize != nil {
		return []usageLogsValidationIssue{{
			Path:    []any{"sinceId"},
			Code:    "custom",
			Message: "增量读（sinceId+asc）不能与 page/pageSize 同时使用",
		}}
	}
	return nil
}

// incremental 把解析结果转成存储层的增量读描述；非增量时为 nil。
func (q usageLogsQuery) incremental() *store.UsageLogIncremental {
	if q.SinceID == nil {
		return nil
	}
	return &store.UsageLogIncremental{SinceID: *q.SinceID}
}

// filters 把解析结果转成存储层筛选条件。
func (q usageLogsQuery) filters() store.UsageLogFilters {
	filters := store.UsageLogFilters{
		UserID:                      q.UserID,
		KeyID:                       q.KeyID,
		ProviderID:                  q.ProviderID,
		SessionID:                   q.SessionID,
		StartTime:                   q.StartTime,
		EndTime:                     q.EndTime,
		StatusCode:                  q.StatusCode,
		Model:                       q.Model,
		ActualResponseModelMismatch: q.Mismatch,
		Endpoint:                    q.Endpoint,
		MinRetryCount:               q.MinRetry,
		ReplayFilter:                q.Replay,
	}
	if q.StatusExcl != nil {
		filters.ExcludeStatusCode200 = *q.StatusExcl
	}
	return filters
}

// coerceInt 解析「带边界与默认值」的整数字段；缺失时返回 ok=false。
func coerceInt(values map[string][]string, key string, min, max int) (int, bool, *usageLogsValidationIssue) {
	raw, present := queryValue(values, key)
	if !present {
		return 0, false, nil
	}
	parsed, ok := parseCoercedNumber(raw)
	if !ok {
		return 0, false, &usageLogsValidationIssue{
			Path: []any{key}, Code: "invalid_type",
			Message: "Expected number, received nan",
		}
	}
	if parsed != float64(int(parsed)) {
		return 0, false, &usageLogsValidationIssue{
			Path: []any{key}, Code: "invalid_type",
			Message: "Expected integer, received float",
		}
	}
	value := int(parsed)
	if min != 0 && value < min {
		return 0, false, &usageLogsValidationIssue{
			Path: []any{key}, Code: "too_small",
			Message: "Number must be greater than or equal to " + strconv.Itoa(min),
		}
	}
	if max != 0 && value > max {
		return 0, false, &usageLogsValidationIssue{
			Path: []any{key}, Code: "too_big",
			Message: "Number must be less than or equal to " + strconv.Itoa(max),
		}
	}
	return value, true, nil
}

// coerceOptionalInt 解析可选的整数字段。
func coerceOptionalInt(values map[string][]string, key string, min, max int) (int, bool, *usageLogsValidationIssue) {
	return coerceInt(values, key, min, max)
}

// coerceOptionalNumber 解析可选的数值字段（无边界，与 NumberQuerySchema 一致）。
func coerceOptionalNumber(values map[string][]string, key string) (float64, bool, *usageLogsValidationIssue) {
	raw, present := queryValue(values, key)
	if !present {
		return 0, false, nil
	}
	parsed, ok := parseCoercedNumber(raw)
	if !ok {
		return 0, false, &usageLogsValidationIssue{
			Path: []any{key}, Code: "invalid_type",
			Message: "Expected number, received nan",
		}
	}
	return parsed, true, nil
}

// coerceOptionalNonNegativeInt64 解析可选的非负整数（用于 sinceId 这类 id 水位）。
// 边界以外的值按既有风格作答：负数 too_small、非整数 invalid_type。
func coerceOptionalNonNegativeInt64(values map[string][]string, key string) (int64, bool, *usageLogsValidationIssue) {
	raw, present := queryValue(values, key)
	if !present {
		return 0, false, nil
	}
	parsed, ok := parseCoercedNumber(raw)
	if !ok {
		return 0, false, &usageLogsValidationIssue{
			Path: []any{key}, Code: "invalid_type",
			Message: "Expected number, received nan",
		}
	}
	if parsed != float64(int64(parsed)) {
		return 0, false, &usageLogsValidationIssue{
			Path: []any{key}, Code: "invalid_type",
			Message: "Expected integer, received float",
		}
	}
	value := int64(parsed)
	if value < 0 {
		return 0, false, &usageLogsValidationIssue{
			Path: []any{key}, Code: "too_small",
			Message: "Number must be greater than or equal to 0",
		}
	}
	return value, true, nil
}

// coerceOptionalBool 复刻 `z.union([literal("true"), literal("false"), boolean])` 的语义：
// 只接受 true/false（大小写敏感）。JS 里 c.req.query 返回字符串，所以「boolean」那支不会命中。
func coerceOptionalBool(values map[string][]string, key string) (*bool, bool, *usageLogsValidationIssue) {
	raw, present := queryValue(values, key)
	if !present {
		return nil, false, nil
	}
	switch raw {
	case "true":
		value := true
		return &value, true, nil
	case "false":
		value := false
		return &value, true, nil
	default:
		return nil, false, &usageLogsValidationIssue{
			Path: []any{key}, Code: "invalid_union", Message: "Invalid input",
		}
	}
}

// queryValue 取查询参数，并区分「缺席」与「给了空串」。
func queryValue(values map[string][]string, key string) (string, bool) {
	items, ok := values[key]
	if !ok || len(items) == 0 {
		return "", false
	}
	return items[0], true
}

// parseCoercedNumber 复刻 z.coerce.number()：Number(trim) 的语义（空串为 0）。
func parseCoercedNumber(raw string) (float64, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		// Number("") === 0，但 Number("   ") 也是 0（空白串同样按 0 处理）。
		return 0, true
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// writeUsageLogsValidationProblem 按 fromZodError 的形状作答。
func writeUsageLogsValidationProblem(writer http.ResponseWriter, request *http.Request, issues []usageLogsValidationIssue) {
	body := usageLogsValidationProblem{
		Type:          problemType("request.validation_failed"),
		Title:         "Validation failed",
		Status:        http.StatusBadRequest,
		Detail:        "One or more fields are invalid.",
		Instance:      problemInstance(request),
		ErrorCode:     "request.validation_failed",
		InvalidParams: issues,
	}
	payload, err := marshalNoEscape(body)
	if err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", problemContentType)
	writer.WriteHeader(http.StatusBadRequest)
	_, _ = writer.Write(payload)
}

// ledgerOnlyCache 复刻 isLedgerOnlyMode（src/lib/ledger-fallback.ts）：60 秒 TTL，
// **查询失败时沿用上次结果**（首次失败则为 false），避免一次抖动把口径翻转。
type ledgerOnlyCache struct {
	pools   *store.Pools
	value   *bool
	expires time.Time
	now     func() time.Time
}

const ledgerOnlyCacheTTL = 60 * time.Second

// ledgerOnly 返回当前是否处于 ledger-only 模式。
func (c *ledgerOnlyCache) ledgerOnly(request *http.Request) bool {
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	current := now()
	if c.value != nil && current.Before(c.expires) {
		return *c.value
	}
	value := false
	switch {
	case c.pools == nil:
		// 没有连接池（未装配）时按「探测失败」处理：沿用上次结果，没有上次结果则 false。
		if c.value != nil {
			value = *c.value
		}
	default:
		exists, err := c.pools.MessageRequestExists(request.Context())
		if err != nil {
			if c.value != nil {
				value = *c.value
			}
		} else {
			value = !exists
		}
	}
	c.value = &value
	c.expires = current.Add(ledgerOnlyCacheTTL)
	return value
}

// resetLedgerOnlyCacheForTest 清空缓存（仅测试用）。
//
// 2026-09-13 复原：它曾在死码清理批次里被判为「零引用」删除，而与此同时另一条 lane 的
// 测试（usage_logs_cache_test.go）正在调用它——两条 lane 并行时 grep 看不到对方的引用，
// 属并行竞态而非真死码。**现有真实调用者，勿再删除**；若要删，请先跑全模块门禁。
func (c *ledgerOnlyCache) resetLedgerOnlyCacheForTest() {
	c.value = nil
	c.expires = time.Time{}
}

// writeOrderedJSONResponse 复刻 jsonResponse：Content-Type 恰为 application/json，
// 并下发 X-API-Version（Node 的 jsonResponse 会设这两项；安全头由路由层统一补）。
func writeOrderedJSONResponse(writer http.ResponseWriter, status int, body any) {
	var payload []byte
	switch typed := body.(type) {
	case jsonObject:
		encoded, err := typed.marshalJSON()
		if err != nil {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		payload = encoded
	default:
		encoded, err := marshalNoEscape(body)
		if err != nil {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		payload = encoded
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set(VersionHeader, APIVersion)
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

// encodeJSONObject 便捷方法：编码有序对象（供测试与导出路径复用）。
func encodeJSONObject(object jsonObject) ([]byte, error) {
	return object.marshalJSON()
}
