package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// 本文件实现 ProblemWriter：RFC7807 的错误信封。
//
// 唯一真源：src/lib/api/v1/_shared/error-envelope.ts（信封与默认文案）与
// src/lib/api/v1/_shared/status-code-map.ts（状态码到 title/errorCode 的默认表）。字段名、
// 默认值、Content-Type 都逐字取自这两处；改本文件前先改那份 TS 或先确认它以哪份为准。
//
// 与 Node 的一处结构性差异（有意，登记在 A2 的白名单）：Node 的 createProblemJson 用
// `options.detail ?? 默认 title`，`??` 只对 null/undefined 生效，因此显式传空串会得到
// "detail": ""。Go 无法区分「没传」与「传了空串」，故选「空串即取默认 title」。正常路径
// （认证失败、action 错误）都会传非空 detail 或走默认，故不影响对拍。

// problemContentType 与 constants.ts:3 的 PROBLEM_JSON_CONTENT_TYPE 逐字一致。
const problemContentType = "application/problem+json"

// problemTypePrefix 与 error-envelope.ts:37 的 problemTypeFor 逐字一致。
const problemTypePrefix = "urn:claude-code-hub:problem:"

// problemTitles 与 status-code-map.ts:15-28 的 DEFAULT_TITLES 逐字一致。
var problemTitles = map[int]string{
	400: "Bad request",
	401: "Unauthorized",
	403: "Forbidden",
	404: "Not found",
	405: "Method not allowed",
	409: "Conflict",
	410: "Gone",
	415: "Unsupported media type",
	422: "Unprocessable entity",
	429: "Too many requests",
	500: "Internal server error",
	503: "Service unavailable",
}

// problemCodes 与 status-code-map.ts:30-43 的 DEFAULT_ERROR_CODES 逐字一致。
var problemCodes = map[int]string{
	400: "request.invalid",
	401: "auth.invalid",
	403: "auth.forbidden",
	404: "resource.not_found",
	405: "method.not_allowed",
	409: "resource.conflict",
	410: "resource.gone",
	415: "request.unsupported_media_type",
	422: "request.unprocessable",
	429: "rate_limit.exceeded",
	500: "internal.error",
	503: "dependency.unavailable",
}

// unknownStatusTitle 是类型系统外的状态码（Node 的 ProblemStatusCode 是联合类型，运行时
// 不会出现）的兜底 title：取 net/http 的标准文案，缺席时用 500 的文案。
const unknownStatusTitle = "Internal server error"

// problemBody 是响应正文。可选字段零值时省略，与 error-envelope.ts:49-51 的三个展开一致。
type problemBody struct {
	Type       string         `json:"type"`
	Title      string         `json:"title"`
	Status     int            `json:"status"`
	Detail     string         `json:"detail"`
	Instance   string         `json:"instance"`
	ErrorCode  string         `json:"errorCode"`
	ErrorParam map[string]any `json:"errorParams,omitempty"`
	TraceID    string         `json:"traceId,omitempty"`
}

// Problems 是 ProblemWriter 的默认实现。
//
// 零值可用：Logger 为空即不记日志，Now 为空即不填 traceId（Node 侧 traceId 也只在显式
// 传入时出现，默认没有）。
type Problems struct {
	// Logger 记录内部错误；nil 表示静默。
	Logger interface {
		Warn(event string, fields map[string]any)
		Error(event string, fields map[string]any)
	}
	// Now 产出 traceId 的来源函数；nil 表示不写 traceId。
	Now func() string
}

// NewProblems 建默认的 ProblemWriter。
func NewProblems(logger interface {
	Warn(event string, fields map[string]any)
	Error(event string, fields map[string]any)
}) *Problems {
	return &Problems{Logger: logger}
}

// WriteProblem 按显式状态码与错误码作答。
//
// instance 取请求路径（与 auth-middleware.ts:83 等调用点一致）；errorCode 为空时取状态的
// 默认码；detail 为空时取状态的默认 title（见文件头差异说明）。
func (p *Problems) WriteProblem(
	writer http.ResponseWriter,
	request *http.Request,
	status int,
	errorCode, detail string,
) {
	body := problemBody{
		Status:   status,
		Title:    problemTitle(status),
		Instance: problemInstance(request),
	}
	if detail == "" {
		body.Detail = body.Title
	} else {
		body.Detail = detail
	}
	if errorCode == "" {
		body.ErrorCode = problemCode(status)
	} else {
		body.ErrorCode = errorCode
	}
	body.Type = problemType(body.ErrorCode)
	if p != nil && p.Now != nil {
		body.TraceID = p.Now()
	}
	p.write(writer, status, body)
}

// ActionError 是 action 层的失败结果，由资源模块（A1）构造。
//
// 为什么带 Status 与 Resource 两个字段：Node 的状态码推导**逐模块不同**——keys/handlers.ts:
// 309-334 只有 404/403/400 三档，users/handlers.ts:365-386 还有 401/500/503。A1 移植某个
// 模块时最清楚自己那份表，故 Status 优先；只有调用方未指定时才走本文件的通用码表
// actionStatus），此时 Resource 决定兜底错误码前缀（key.not_found / user.action_failed 等）。
type ActionError struct {
	// Status 为 0 表示由 actionStatus 推导。
	Status int
	// Resource 是资源名前缀，用于兜底错误码，例如 "key"、"user"、"model_price"。
	Resource string
	// Code 是 Node 的 ActionResult.errorCode；为空时按状态码取兜底码。
	Code string
	// Params 对应 Node 的 errorParams，写进响应但不参与判定。
	Params map[string]any
	// Err 是原始错误，只用于日志；**绝不写进响应**（响应 detail 是公开文案）。
	Err error
}

// Error 实现 error。
func (e *ActionError) Error() string {
	if e == nil {
		return "adminapi: action failed"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Code != "" {
		return e.Code
	}
	return "adminapi: action failed"
}

// Unwrap 让 errors.Is/As 能穿到原始错误。
func (e *ActionError) Unwrap() error { return e.Err }

// NewActionError 建一个 action 错误；status 为 0 时由码表推导。
func NewActionError(resource, code string, status int, err error) *ActionError {
	return &ActionError{Resource: resource, Code: code, Status: status, Err: err}
}

// WriteActionError 把 action 层错误按显式错误码表映射作答。
//
// 三条与 Node 对齐的语义：
//  1. 响应 detail 是**公开文案**（publicActionErrorDetail，error-envelope.ts:64），不是原始
//     错误消息——原始消息可能含 pg 约束名或用户输入。
//  2. 非 ActionError 的 error 说明是程序错误：500 + internal.error（Node 侧会抛出并被框架
//     兜成 500）。
//  3. 子串判定（detail.includes("不存在")）**不移植**：按裁决改为显式错误码表，无法用码表
//
// 表达的边缘情形登记为白名单差异。
func (p *Problems) WriteActionError(writer http.ResponseWriter, request *http.Request, err error) {
	var action *ActionError
	if !errors.As(err, &action) {
		if p != nil && p.Logger != nil {
			p.Logger.Error("admin_action_error_untyped", map[string]any{"error": errText(err)})
		}
		p.WriteProblem(writer, request, http.StatusInternalServerError, "", "")
		return
	}

	status := action.Status
	if status == 0 {
		status = actionStatus(action.Resource, action.Code)
	}
	code := action.Code
	if code == "" {
		code = actionFallbackCode(action.Resource, status)
	}
	// 原始错误进服务端日志（脱敏后）；客户端可见的 detail 仍是公开常量，形状不变。
	//
	// 为什么 4xx 也记：Node 的 action 层 catch 不区分状态码，一律 logger.error 原文
	// （actions/providers.ts:985-997）。Go 此前只在 5xx 记，于是「400 provider.action_failed、
	// detail 恒为 Bad request」这类响应在生产日志里**没有任何成因线索**——2026-09-15 的
	// PATCH /api/v1/providers/149（DB 已提交、撤销快照写失败）就因此无从下手。
	//
	// 4xx 且无底层 error 时不记：那是码表驱动的正常拒绝（不存在、校验失败），没有成因可报。
	if p != nil && p.Logger != nil && (status >= http.StatusInternalServerError || action.Err != nil) {
		event := "admin_action_error"
		if status >= http.StatusInternalServerError {
			event = "admin_action_error_500"
		}
		// 错误文案是自由文本，凭据以 `sk-…` / `Bearer …` / URL 内嵌 `user:pass@` 这类**形态**
		// 出现，键名表对它无效，故用值形态脱敏（redactErrorText，与 Node 的
		// sanitizeErrorTextForDetail 同源）。
		text, redacted := redactErrorText(errText(action.Err))
		fields := map[string]any{
			"resource": action.Resource,
			"code":     code,
			"status":   status,
			"error":    text,
		}
		// redacted 标记让排障者知道「这是改写过的文案，不是上游原话」。
		if redacted {
			fields["redacted"] = true
		}
		p.Logger.Warn(event, fields)
	}

	body := problemBody{
		Type:      problemType(code),
		Title:     problemTitle(status),
		Status:    status,
		Detail:    problemTitle(status),
		Instance:  problemInstance(request),
		ErrorCode: code,
	}
	if len(action.Params) > 0 {
		body.ErrorParam = action.Params
	}
	if p != nil && p.Now != nil {
		body.TraceID = p.Now()
	}
	p.write(writer, status, body)
}

// WriteValidationError 作答 zod 形状的 400（Node 的 fromZodError）。
//
// 与 WriteProblem 的 400 有何不同：fromZodError 的 title 恒为 "Validation failed"（不是
// status-code-map 的 "Bad request"），detail 恒为 "One or more fields are invalid."，且正文
// **多一个 invalidParams 数组**（error-envelope.ts:40-62）。problemBody 表达不了这个字段，
// 所以这里另组一份正文——字段顺序与 Node 逐字一致，A2 对拍可直接比对。
func (p *Problems) WriteValidationError(
	writer http.ResponseWriter,
	request *http.Request,
	params []InvalidParam,
) {
	if params == nil {
		params = []InvalidParam{}
	}
	body := struct {
		Type          string         `json:"type"`
		Title         string         `json:"title"`
		Status        int            `json:"status"`
		Detail        string         `json:"detail"`
		Instance      string         `json:"instance"`
		ErrorCode     string         `json:"errorCode"`
		InvalidParams []InvalidParam `json:"invalidParams"`
	}{
		Type:          problemType("request.validation_failed"),
		Title:         validationFailedTitle,
		Status:        http.StatusBadRequest,
		Detail:        validationFailedDetail,
		Instance:      problemInstance(request),
		ErrorCode:     "request.validation_failed",
		InvalidParams: params,
	}
	header := writer.Header()
	header.Set("Content-Type", problemContentType)
	writer.WriteHeader(http.StatusBadRequest)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil && p != nil && p.Logger != nil {
		p.Logger.Warn("admin_validation_problem_write_failed", map[string]any{"error": err.Error()})
	}
}

// validationFailedTitle/Detail 逐字取自 error-envelope.ts:41-46 的 fromZodError。
const (
	validationFailedTitle  = "Validation failed"
	validationFailedDetail = "One or more fields are invalid."
)

// write 落盘响应：Content-Type 与状态码都与 createProblemResponse 一致。
func (p *Problems) write(writer http.ResponseWriter, status int, body problemBody) {
	header := writer.Header()
	header.Set("Content-Type", problemContentType)
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil && p != nil && p.Logger != nil {
		p.Logger.Warn("admin_problem_write_failed", map[string]any{"error": err.Error()})
	}
}

// problemTitle 复刻 getDefaultProblemTitle。
func problemTitle(status int) string {
	if title, ok := problemTitles[status]; ok {
		return title
	}
	if text := http.StatusText(status); text != "" {
		return text
	}
	return unknownStatusTitle
}

// problemCode 复刻 getDefaultErrorCode。
func problemCode(status int) string {
	if code, ok := problemCodes[status]; ok {
		return code
	}
	return problemCodes[http.StatusInternalServerError]
}

// problemType 复刻 problemTypeFor（encodeURIComponent 的逐字等价实现）。
func problemType(errorCode string) string {
	return problemTypePrefix + encodeURIComponent(errorCode)
}

// problemInstance 复刻 `options.instance ?? MANAGEMENT_API_BASE_PATH` 的默认值，
// 以及在 API 路由里普遍传入的 `new URL(c.req.url).pathname`。
func problemInstance(request *http.Request) string {
	if request != nil && request.URL != nil && request.URL.Path != "" {
		return request.URL.Path
	}
	return MountPrefix
}

// actionStatus 是 action 层状态码的显式码表。
//
// 两张表逐条对应 Node：keys 表窄（keys/handlers.ts:309-334，另有 NOT_FOUND 后缀与 FORBIDDEN
// 子串两条宽化），users 表宽（users/handlers.ts:365-386，只认精确码，**没有**后缀/子串规则）。
// 其余资源模块在 Node 侧各有自己的一份，本文件统一用 users 的宽表作默认；A1 移植某个模块时
// 若发现该模块的表更窄或更宽，应显式设置 ActionError.Status 而不是改这里。
//
// 两个表都**不含** Node 里的中文子串分支（detail.includes("不存在") 等），这是裁决过的取舍：
// 按文案判定状态码会在改文案时静静改语义。受影响的只有「码为空且 detail 是中文未找到文案」的
// 调用——A1 遇到这类 action 时必须显式给码或给 Status。
func actionStatus(resource, code string) int {
	normalized := strings.ToUpper(code)
	if resource == "key" {
		switch {
		case isNotFoundActionCode(normalized):
			return http.StatusNotFound
		case isPermissionActionCode(normalized):
			return http.StatusForbidden
		default:
			return http.StatusBadRequest
		}
	}
	switch normalized {
	case "UNAUTHORIZED":
		return http.StatusUnauthorized
	case "PERMISSION_DENIED":
		return http.StatusForbidden
	case "NOT_FOUND":
		return http.StatusNotFound
	case "DATABASE_ERROR", "CONNECTION_FAILED", "TIMEOUT", "NETWORK_ERROR":
		return http.StatusServiceUnavailable
	case "INTERNAL_ERROR", "OPERATION_FAILED", "CREATE_FAILED", "UPDATE_FAILED", "DELETE_FAILED":
		return http.StatusInternalServerError
	}
	return http.StatusBadRequest
}

// isNotFoundActionCode 复刻 keys/handlers.ts:326-329 的判定（不含子串分支）。
func isNotFoundActionCode(normalized string) bool {
	return normalized == "NOT_FOUND" || strings.HasSuffix(normalized, "_NOT_FOUND")
}

// isPermissionActionCode 复刻 keys/handlers.ts:331-338 的判定（不含子串分支）。
func isPermissionActionCode(normalized string) bool {
	return normalized == "PERMISSION_DENIED" ||
		normalized == "UNAUTHORIZED" ||
		strings.Contains(normalized, "FORBIDDEN")
}

// actionFallbackCode 复刻 `code ?? (status === 404 ? "<resource>.not_found" : "<resource>.action_failed")`。
//
// 资源名为空（A1 未填）时退回状态码默认码，避免产出 "*.action_failed" 这种无前缀的形态。
func actionFallbackCode(resource string, status int) string {
	if resource == "" {
		return problemCode(status)
	}
	if status == http.StatusNotFound {
		return resource + ".not_found"
	}
	return resource + ".action_failed"
}

// encodeURIComponent 与 JS 的 encodeURIComponent 逐字等价。
//
// 只对本包用到的形态（错误码）有实际影响：字母数字与 - _ . ! ~ * ' ( ) 原样保留，其余按
// UTF-8 逐字节转成大写十六进制百分号转义。用自己的一份而不复用 net/url：QueryEscape 把
// 空格编成 "+"、PathEscape 保留 "@" 与 "$"，两者都与 JS 不同。
func encodeURIComponent(value string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		character := value[index]
		if strings.IndexByte(unreserved, character) >= 0 {
			builder.WriteByte(character)
			continue
		}
		builder.WriteByte('%')
		const hexDigits = "0123456789ABCDEF"
		builder.WriteByte(hexDigits[character>>4])
		builder.WriteByte(hexDigits[character&0x0f])
	}
	return builder.String()
}

// errText 取错误的可读文本；nil 时返回空串。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
