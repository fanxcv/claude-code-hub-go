package guard

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Response 是守卫链的抢答响应。
//
// 链的语义是「任一步返回非 nil 即短路」，因此响应必须自带状态码、头部与已序列化的正文：
// 后续步骤与转发器都不再参与，调用方（httpapi）只负责把它写回客户端。
type Response struct {
	// Status 是 HTTP 状态码。
	Status int
	// Headers 是响应头；至少含 content-type。
	Headers http.Header
	// Body 是已序列化的正文。
	Body []byte
}

// NewResponse 构造响应；headers 为 nil 时建一个空表。
func NewResponse(status int, headers http.Header, body []byte) *Response {
	if headers == nil {
		headers = http.Header{}
	}
	if body == nil {
		body = []byte{}
	}
	return &Response{Status: status, Headers: headers, Body: body}
}

// WithHeader 返回带附加头部的副本，原响应不被改动。
func (r *Response) WithHeader(key, value string) *Response {
	headers := make(http.Header, len(r.Headers)+1)
	for existingKey, values := range r.Headers {
		copied := make([]string, len(values))
		copy(copied, values)
		headers[existingKey] = copied
	}
	headers.Set(key, value)
	return &Response{Status: r.Status, Headers: headers, Body: r.Body}
}

// JSONResponse 以 JSON 序列化 payload 构造响应。
//
// content-type 逐字对齐 Node 侧抢答路径的写法（含 charset）。payload 无法序列化时不做
// 降级猜测：守卫链宁可让调用方看到 500，也不要发出一个语义不明的 200。
func JSONResponse(status int, payload any) (*Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	headers.Set("content-type", "application/json; charset=utf-8")
	return NewResponse(status, headers, body), nil
}

// errorPayload 是 ProxyResponses.buildError 的载荷形状。
//
// 字段顺序即 JSON 输出顺序，与 Node 侧一致：message、type、code、details 可选。
type errorPayload struct {
	Error struct {
		Message string         `json:"message"`
		Type    string         `json:"type"`
		Code    string         `json:"code"`
		Details map[string]any `json:"details,omitempty"`
	} `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

// BuildError 复刻 ProxyResponses.buildError：状态码 + 文案 + 可选错误类型。
//
// errorType 为空时按状态码取默认值；code 与 type 的取值关系也逐字对齐 Node 侧
// getErrorCode——type 不是 api_error 时 code 直接等于 type。
func BuildError(status int, message, errorType string) *Response {
	return buildErrorPayload(status, message, errorType, nil, "")
}

// BuildErrorWithDetails 是 BuildError 的带详情版本。
func BuildErrorWithDetails(
	status int,
	message, errorType string,
	details map[string]any,
	requestID string,
) *Response {
	return buildErrorPayload(status, message, errorType, details, requestID)
}

func buildErrorPayload(
	status int,
	message, errorType string,
	details map[string]any,
	requestID string,
) *Response {
	finalType := errorType
	if finalType == "" {
		finalType = errorTypeForStatus(status)
	}

	payload := errorPayload{RequestID: requestID}
	payload.Error.Message = message
	payload.Error.Type = finalType
	payload.Error.Code = errorCodeForStatus(status, finalType)
	payload.Error.Details = details

	body, err := json.Marshal(payload)
	if err != nil {
		// 只有 details 里塞了不可序列化的值才会走到这里。降级为不带 details 的同一形状，
		// 保证客户端始终拿到可解析的错误体（Node 侧 JSON.stringify 会静默丢键，此处显式记录）。
		payload.Error.Details = nil
		body, _ = json.Marshal(payload)
	}

	headers := http.Header{}
	headers.Set("content-type", "application/json; charset=utf-8")
	return NewResponse(status, headers, body)
}

// BuildRateLimitError 构造 Node 同形的限流响应（error-handler.ts 的 buildRateLimitResponse）。
//
// 为什么不复用 BuildError：那个形状把机器可读字段按 `error.details` 嵌套，而 Node 的限流响应把
// 七个字段**平铺**在 error 下，且额外带三个 X-RateLimit-* 头。客户端与前端按
// `code` / `limit_type` 分支，形状不同就是行为不同。
//
// 头与 Node 逐条对应：
//   - X-RateLimit-Limit：上限；X-RateLimit-Remaining：max(0, limit-current)；
//   - 仅当有固定重置时刻（滚动窗口为空）时写 X-RateLimit-Reset（Unix 秒）与 Retry-After。
func BuildRateLimitError(block RateLimitBlock) *Response {
	status := block.Status
	if status == 0 {
		status = http.StatusTooManyRequests
	}
	errorType := block.ErrorType
	if errorType == "" {
		errorType = "rate_limit_error"
	}

	payload := struct {
		Error struct {
			Type      string  `json:"type"`
			Message   string  `json:"message"`
			Code      string  `json:"code"`
			LimitType string  `json:"limit_type"`
			Current   float64 `json:"current"`
			Limit     float64 `json:"limit"`
			ResetTime *string `json:"reset_time"`
		} `json:"error"`
	}{}
	payload.Error.Type = errorType
	payload.Error.Message = block.Message
	payload.Error.Code = "rate_limit_exceeded"
	payload.Error.LimitType = block.LimitType
	payload.Error.Current = block.Current
	payload.Error.Limit = block.Limit
	if block.ResetTime != "" {
		reset := block.ResetTime
		payload.Error.ResetTime = &reset
	}

	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	}

	headers := http.Header{}
	headers.Set("content-type", "application/json; charset=utf-8")
	headers.Set("X-RateLimit-Limit", formatLimitNumber(block.Limit))
	headers.Set("X-RateLimit-Remaining", formatLimitNumber(math.Max(0, block.Limit-block.Current)))
	if block.ResetTime != "" {
		if reset, parseErr := time.Parse(time.RFC3339Nano, block.ResetTime); parseErr == nil {
			headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		}
		if block.RetryAfterSeconds != nil {
			headers.Set("Retry-After", strconv.Itoa(*block.RetryAfterSeconds))
		}
	}
	return NewResponse(status, headers, body)
}

// formatLimitNumber 把限流数值写成 Node 的 `Number.toString()` 形态：整数不带小数点。
func formatLimitNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// errorTypeForStatus 对齐 ProxyResponses.getErrorType。
func errorTypeForStatus(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 402:
		return "payment_required_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	case 500:
		return "internal_server_error"
	case 502:
		return "bad_gateway_error"
	case 503:
		return "service_unavailable_error"
	case 504:
		return "gateway_timeout_error"
	default:
		return "api_error"
	}
}

// errorCodeForStatus 对齐 ProxyResponses.getErrorCode。
func errorCodeForStatus(status int, errorType string) string {
	if errorType != "" && errorType != "api_error" {
		return errorType
	}
	switch status {
	case 400:
		return "invalid_request"
	case 401:
		return "unauthorized"
	case 402:
		return "payment_required"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 429:
		return "rate_limit_exceeded"
	case 500:
		return "internal_error"
	case 502:
		return "bad_gateway"
	case 503:
		return "service_unavailable"
	case 504:
		return "gateway_timeout"
	default:
		return "http_" + itoa(status)
	}
}

// itoa 是小整数转十进制，避开 strconv 只为状态码引入一次分配。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		index--
		buffer[index] = '-'
	}
	return string(buffer[index:])
}
