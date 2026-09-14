package adminapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是管理面的 **audit-logs 资源**（/api/v1/audit-logs），两条端点：
//
//	GET /audit-logs       admin （游标分页 + 分类/成功/时间筛选）
//	GET /audit-logs/{id}  admin （单行；不存在 404）
//
// 唯一真源：src/app/api/v1/resources/audit-logs/{router,handlers}.ts 与
// src/lib/api/v1/schemas/audit-logs.ts（筛选与响应的形状）、src/actions/audit-logs.ts
// （鉴权与筛选归一）、src/repository/audit-log.ts（SQL 与 keyset 游标）。
//
// 三处照抄 Node 的细节：
//
//  1. **success 只认字面 "true"/"false"**（schemas/audit-logs.ts:26-33 用 z.enum 而非
//     z.coerce.boolean()，因为 Boolean("false") === true 会让「只看失败」变成「只看成功」）。
//     其余取值是 400 校验错误，不是「当没传」。
//  2. **游标是 base64url(JSON.stringify({createdAt,id}))**（_shared/pagination.ts:35-37），
//     解码失败或字段类型不对都是 400 `audit_log.invalid_cursor`（handlers.ts:101-121）。
//  3. **limit 的上限是 100**（CursorQuerySchema），而仓库层另有 500 的钳制。
//
// 一处白名单差异（登记）：from/to 用 Go 的 RFC3339 解析，与 zod 的
// `z.string().datetime({offset: true})` 在边缘串上不完全同判（zod 允许不带秒的写法等）。
// 两边都要求带时区偏移，且非法输入都作答 400 `request.validation_failed`。

// auditLogCategories 逐字取自 schemas/audit-logs.ts:4-15 的 AuditCategorySchema。
var auditLogCategories = map[string]bool{
	"auth":            true,
	"user":            true,
	"provider":        true,
	"provider_group":  true,
	"system_settings": true,
	"key":             true,
	"notification":    true,
	"sensitive_word":  true,
	"model_price":     true,
}

// auditLogListQueryLimit 是 limit 的默认值与上界（CursorQuerySchema）。
const (
	auditLogDefaultLimit = 20
	auditLogMaxLimit     = 100
)

// auditLogListQuery 是 /audit-logs 的解析结果。
type auditLogListQuery struct {
	Cursor   *store.AdminAuditLogCursor
	Limit    int
	Category string
	Success  *bool
	From     *time.Time
	To       *time.Time
}

// auditLogAPI 是 audit-logs 资源处理器的依赖集合。
type auditLogAPI struct {
	pools    *store.Pools
	problems ProblemWriter
	logger   *logx.Logger
}

// RegisterAuditLogs 注册 audit-logs 资源的两条端点。
//
// Store 未装配时全部不注册（回退 Node）——与本包其余模块同一条纪律。
func RegisterAuditLogs(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_audit_logs_store_unwired", map[string]any{
				"module": "audit_logs",
				"action": "routes_not_registered",
			})
		}
		return
	}
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	problems := deps.Problems
	if problems == nil {
		problems = NewProblems(logger)
	}
	api := &auditLogAPI{pools: deps.Store, problems: problems, logger: logger}

	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/audit-logs",
		Access:      AccessAdmin,
		Module:      "audit_logs",
		OperationID: "listAuditLogs",
		Handler:     http.HandlerFunc(api.handleList),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/audit-logs/{id}",
		Access:      AccessAdmin,
		Module:      "audit_logs",
		OperationID: "getAuditLog",
		Handler:     http.HandlerFunc(api.handleDetail),
	})
}

// handleList 复刻 listAuditLogs（audit-logs/handlers.ts:19-57）。
func (api *auditLogAPI) handleList(writer http.ResponseWriter, request *http.Request) {
	query, issues, invalidCursor := parseAuditLogListQuery(request)
	if invalidCursor {
		api.problems.WriteProblem(writer, request, http.StatusBadRequest,
			"audit_log.invalid_cursor", "Cursor is invalid.")
		return
	}
	if len(issues) > 0 {
		api.problems.WriteValidationError(writer, request, issues)
		return
	}

	rows, nextCursor, err := api.pools.AdminListAuditLogs(
		request.Context(),
		store.AdminAuditLogFilter{
			Category: query.Category,
			Success:  query.Success,
			From:     query.From,
			To:       query.To,
		},
		query.Cursor,
		query.Limit,
	)
	if err != nil {
		api.writeAuditLogFailure(writer, request, err)
		return
	}

	items := make([]store.AdminAuditLogRow, 0, len(rows))
	items = append(items, rows...)
	payload := struct {
		Items    []store.AdminAuditLogRow `json:"items"`
		PageInfo struct {
			NextCursor *string `json:"nextCursor"`
			HasMore    bool    `json:"hasMore"`
			Limit      int     `json:"limit"`
		} `json:"pageInfo"`
	}{Items: items}
	payload.PageInfo.HasMore = nextCursor != nil
	payload.PageInfo.Limit = query.Limit
	if nextCursor != nil {
		encoded := encodeAuditLogCursor(*nextCursor)
		payload.PageInfo.NextCursor = &encoded
	}
	writeAuditLogJSON(writer, http.StatusOK, payload)
}

// handleDetail 复刻 getAuditLog（audit-logs/handlers.ts:59-80）。
func (api *auditLogAPI) handleDetail(writer http.ResponseWriter, request *http.Request) {
	params := ParamsFrom(request.Context())
	raw := params["id"]
	if raw == "" {
		api.problems.WriteValidationError(writer, request, []InvalidParam{{
			Path: []any{"id"}, Code: "invalid_type", Message: "Required",
		}})
		return
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		api.problems.WriteValidationError(writer, request, []InvalidParam{{
			Path: []any{"id"}, Code: "invalid_type", Message: "Expected number, received nan",
		}})
		return
	}

	row, loadErr := api.pools.AdminGetAuditLog(request.Context(), id)
	if loadErr != nil {
		if errors.Is(loadErr, store.ErrNotFound) {
			api.problems.WriteProblem(writer, request, http.StatusNotFound,
				"audit_log.not_found", "Audit log was not found.")
			return
		}
		api.writeAuditLogFailure(writer, request, loadErr)
		return
	}
	writeAuditLogJSON(writer, http.StatusOK, row)
}

// parseAuditLogListQuery 复刻 AuditLogListQuerySchema 的解析与游标校验。
//
// 顺序与 Node 一致：**先 zod 校验，再解游标**（handlers.ts:25-36）。因此
// `?cursor=garbage&limit=0` 报的是 limit 的错，不是游标的错。
//
// 两个失败出口是分开的，因为它们作答的形状不同：issues 非空 → 400 `request.validation_failed`
// （带 invalidParams）；invalidCursor → 400 `audit_log.invalid_cursor`（不带 invalidParams）。
func parseAuditLogListQuery(request *http.Request) (auditLogListQuery, []InvalidParam, bool) {
	values := request.URL.Query()
	query := auditLogListQuery{Limit: auditLogDefaultLimit}
	issues := []InvalidParam{}

	if limit, present, issue := coerceInt(values, "limit", 1, auditLogMaxLimit); issue != nil {
		issues = append(issues, InvalidParam{
			Path: issue.Path, Code: issue.Code, Message: issue.Message,
		})
	} else if present {
		query.Limit = limit
	}

	if raw, present := queryValue(values, "category"); present {
		if !auditLogCategories[raw] {
			issues = append(issues, InvalidParam{
				Path: []any{"category"}, Code: "invalid_enum_value",
				Message: "Invalid enum value. Expected 'auth' | 'user' | 'provider' | " +
					"'provider_group' | 'system_settings' | 'key' | 'notification' | " +
					"'sensitive_word' | 'model_price', received '" + raw + "'",
			})
		} else {
			query.Category = raw
		}
	}

	if value, present, issue := coerceOptionalBool(values, "success"); issue != nil {
		issues = append(issues, InvalidParam{
			Path: []any{"success"}, Code: "invalid_enum_value",
			Message: "Invalid enum value. Expected 'true' | 'false'",
		})
	} else if present {
		query.Success = value
	}

	for _, field := range []struct {
		key    string
		target **time.Time
	}{
		{"from", &query.From},
		{"to", &query.To},
	} {
		raw, present := queryValue(values, field.key)
		if !present {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			issues = append(issues, InvalidParam{
				Path: []any{field.key}, Code: "invalid_string",
				Message: "Invalid datetime",
			})
			continue
		}
		*field.target = &parsed
	}

	if len(issues) > 0 {
		return auditLogListQuery{}, issues, false
	}

	if raw, present := queryValue(values, "cursor"); present {
		cursor, ok := decodeAuditLogCursor(raw)
		if !ok {
			return auditLogListQuery{}, nil, true
		}
		query.Cursor = cursor
	}
	return query, nil, false
}

// decodeAuditLogCursor 复刻 decodeCursor + handlers.ts:101-121 的字段校验。
//
// 第二个返回值为 false 表示「游标非法」：调用方据此作答 400 `audit_log.invalid_cursor`。
func decodeAuditLogCursor(raw string) (*store.AdminAuditLogCursor, bool) {
	var payload map[string]any
	if !decodeBase64URLJSON(raw, &payload) {
		return nil, false
	}
	createdAt, ok := payload["createdAt"].(string)
	if !ok {
		return nil, false
	}
	// JSON 数字在 Go 里解成 float64：整数判定要与 JS 的 Number.isInteger 同判（5.5 不是游标）。
	rawID, ok := payload["id"].(float64)
	if !ok || rawID != float64(int64(rawID)) {
		return nil, false
	}
	return &store.AdminAuditLogCursor{CreatedAt: createdAt, ID: int64(rawID)}, true
}

// decodeBase64URLJSON 解 base64url 的 JSON 文本。
//
// Node 的 Buffer.from(str, "base64url") 对填充与非法字符都比 Go 宽松，故两种编码都试一次。
func decodeBase64URLJSON(raw string, target any) bool {
	for _, encoding := range []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
	} {
		decoded, err := encoding.DecodeString(raw)
		if err != nil {
			continue
		}
		var parsed any
		if jsonErr := json.Unmarshal(decoded, &parsed); jsonErr != nil {
			return false
		}
		object, isObject := parsed.(map[string]any)
		if !isObject {
			return false
		}
		if value, isMap := target.(*map[string]any); isMap {
			*value = object
			return true
		}
	}
	return false
}

// encodeAuditLogCursor 复刻 encodeCursor：base64url(JSON.stringify({createdAt, id}))。
//
// 键序必须与 Node 一致（createdAt 在前），因为游标串会原样回给前端并被拿回来比对。
func encodeAuditLogCursor(cursor store.AdminAuditLogCursor) string {
	payload := struct {
		CreatedAt string `json:"createdAt"`
		ID        int64  `json:"id"`
	}{CreatedAt: cursor.CreatedAt, ID: cursor.ID}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// writeAuditLogFailure 作答仓库层失败（Node 侧被 action 兜成 400 operation_failed）。
func (api *auditLogAPI) writeAuditLogFailure(
	writer http.ResponseWriter,
	request *http.Request,
	err error,
) {
	api.logger.Warn("admin_audit_logs_request_failed", map[string]any{
		"path":  request.URL.Path,
		"error": strings.TrimSpace(err.Error()),
	})
	api.problems.WriteProblem(writer, request, http.StatusBadRequest,
		"OPERATION_FAILED", "")
}

// writeAuditLogJSON 作答 JSON 正文。
//
// 用 SetEscapeHTML(false)：JS 的 JSON.stringify 不转义 < > &，而 Go 的 json.Marshal 会——
// 审计行里有 target_name / user_agent 这类外部输入，转义与否在原始字节上可对拍出差异。
func writeAuditLogJSON(writer http.ResponseWriter, status int, body any) {
	encoded, err := marshalNoEscape(body)
	if err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}
