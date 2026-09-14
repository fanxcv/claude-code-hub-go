package forward

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// 重试上限的夹取边界，对齐 PROVIDER_LIMITS.MAX_RETRY_ATTEMPTS 与 clampRetryAttempts。
const (
	MinRetryAttempts = 1
	MaxRetryAttempts = 10
	// DefaultRetryAttempts 对齐 PROVIDER_DEFAULTS.MAX_RETRY_ATTEMPTS。
	DefaultRetryAttempts = 2
	// DefaultProviderSwitches 对齐 MAX_PROVIDER_SWITCHES：最多切换 20 次供应商。
	DefaultProviderSwitches = 20
	// DefaultRetryDelay 对齐 Node 内层重试前的固定等待。
	DefaultRetryDelay = 100 * time.Millisecond
	// DefaultMaxResponseBytes 是非流式正文的硬上限。
	//
	// Node 侧无上限（直接 response.text()），这是本包刻意的收紧：上游异常体积的正文
	// 属于内存风险，而 64 MiB 已远超任何合法非流式响应。
	DefaultMaxResponseBytes int64 = 64 * 1024 * 1024
	// DefaultMaxErrorBodyBytes 是错误正文进入 Failure 前的截断上限。
	DefaultMaxErrorBodyBytes int64 = 64 * 1024
)

// Limits 是转发路径的可配上限。
type Limits struct {
	// MaxProviderSwitches 是外层供应商切换上限；0 取 DefaultProviderSwitches。
	MaxProviderSwitches int
	// DefaultRetryAttempts 是供应商未配置重试上限时的默认值；0 取 DefaultRetryAttempts。
	DefaultRetryAttempts int
	// RetryDelay 是内层重试前的等待；0 取 DefaultRetryDelay。
	RetryDelay time.Duration
	// MaxResponseBytes 是成功响应正文上限；0 取 DefaultMaxResponseBytes。
	MaxResponseBytes int64
	// MaxErrorBodyBytes 是错误正文截断上限；0 取 DefaultMaxErrorBodyBytes。
	MaxErrorBodyBytes int64
}

// withDefaults 补齐零值。
func (l Limits) withDefaults() Limits {
	if l.MaxProviderSwitches <= 0 {
		l.MaxProviderSwitches = DefaultProviderSwitches
	}
	if l.DefaultRetryAttempts <= 0 {
		l.DefaultRetryAttempts = DefaultRetryAttempts
	}
	if l.RetryDelay <= 0 {
		l.RetryDelay = DefaultRetryDelay
	}
	if l.MaxResponseBytes <= 0 {
		l.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if l.MaxErrorBodyBytes <= 0 {
		l.MaxErrorBodyBytes = DefaultMaxErrorBodyBytes
	}
	return l
}

// ResolveMaxAttempts 返回某个供应商的内层重试上限，对齐 resolveMaxAttemptsForProvider：
// 供应商未配置取默认值，配置值夹取到 [1,10]。
func ResolveMaxAttempts(provider Provider, limits Limits) int {
	if provider.MaxRetryAttempts == nil {
		return clampAttempts(limits.DefaultRetryAttempts)
	}
	return clampAttempts(*provider.MaxRetryAttempts)
}

func clampAttempts(value int) int {
	if value < MinRetryAttempts {
		return MinRetryAttempts
	}
	if value > MaxRetryAttempts {
		return MaxRetryAttempts
	}
	return value
}

// timeoutFailureBody 构造超时失败的上游正文，字段与 Node 的 buildResponseTimeoutError 一致。
func timeoutFailureBody(timeout time.Duration, streaming bool) string {
	timeoutType := "non_streaming_total"
	if streaming {
		timeoutType = "streaming_first_byte"
	}
	payload := map[string]any{
		"error": map[string]any{
			"type":         "timeout_error",
			"message":      fmt.Sprintf("Provider failed to respond within %dms", timeout.Milliseconds()),
			"timeout_type": timeoutType,
			"timeout_ms":   timeout.Milliseconds(),
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// timeoutFailureMessage 是超时失败的中文文案，对齐 Node 的「供应商响应超时」。
func timeoutFailureMessage(timeout time.Duration, streaming bool) string {
	if streaming {
		return fmt.Sprintf("供应商首字节响应超时: %dms 内未收到数据", timeout.Milliseconds())
	}
	return fmt.Sprintf("供应商响应超时: %dms 内未收到数据", timeout.Milliseconds())
}

// messageFromErrorBody 从上游错误正文里提取可读文案，对齐 ProxyError.extractErrorMessage 的常用形态。
func messageFromErrorBody(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return ""
	}
	if inner, ok := decoded["error"].(map[string]any); ok {
		if message, ok := inner["message"].(string); ok && message != "" {
			return message
		}
		if message, ok := inner["type"].(string); ok && message != "" {
			return message
		}
	}
	for _, key := range []string{"message", "detail", "msg"} {
		if message, ok := decoded[key].(string); ok && message != "" {
			return message
		}
	}
	return ""
}

// truncateBody 按上限截断正文，返回截断结果与是否发生截断。
//
// 失效方向是保守的：截断只影响归因文案与日志，绝不改变分类所依赖的状态码。
func truncateBody(body []byte, limit int64) (string, bool) {
	if limit <= 0 || int64(len(body)) <= limit {
		return string(body), false
	}
	return string(body[:limit]), true
}

// readAllBounded 读取至多 limit 字节；超限即停止读取并报告截断。
//
// 与 io.ReadAll 的差别在于：超限时不会把剩余内容继续读进内存，从而让上层能按分类决定
// 是否值得继续。
func readAllBounded(reader io.Reader, limit int64) ([]byte, bool, error) {
	if limit <= 0 {
		data, err := io.ReadAll(reader)
		return data, false, err
	}
	limited := io.LimitReader(reader, limit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return data, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}
