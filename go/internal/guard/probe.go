package guard

import (
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// probePayload 是探测请求的抢答体。
type probePayload struct {
	InputTokens int `json:"input_tokens"`
}

// probeStep 复刻 guard-pipeline.ts 的 probe 步骤。
//
// 语义：单条消息、content 为字符串且内容为 foo 或 count（忽略大小写与空白）即判定为探测
// 请求，由网关直接抢答 200，不进入会话、限流与选路。判定失败（无正文通道）即放行。
func (d Deps) probeStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		body, err := d.body(ctx)
		if err != nil {
			// 无正文可判，按非同探测请求处理。
			return nil, nil
		}
		if !isProbeRequest(body) {
			return nil, nil
		}
		response, err := JSONResponse(200, probePayload{InputTokens: 0})
		if err != nil {
			return nil, err
		}
		return response, nil
	}
}

// isProbeRequest 复刻 ProxySession.isProbeRequest。
func isProbeRequest(body map[string]any) bool {
	messages, ok := messagesFromBody(body).([]any)
	if !ok || len(messages) != 1 {
		return false
	}
	first, ok := messages[0].(map[string]any)
	if !ok {
		return false
	}
	content, ok := first["content"].(string)
	if !ok {
		return false
	}
	trimmed := strings.ToLower(strings.TrimSpace(content))
	return trimmed == "foo" || trimmed == "count"
}
