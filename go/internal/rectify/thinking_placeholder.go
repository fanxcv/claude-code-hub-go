package rectify

import "github.com/fanxcv/claude-code-hub-go/go/internal/convert"

// 主动型「占位签名」剥离：把客户端回传的**本进程造出的**占位 thinking 签名从出站正文里拿掉。
//
// 为什么必须在发送前做：占位签名是我们造给客户端看的（见 convert/thinking_placeholder.go），
// 真实 Anthropic 上游会校验签名，回传上去只会换来一次 400（"signature: Field required" /
// "cannot be modified"）。被动的签名整流器要等失败之后才动手，那就已经白烧一次上游调用与
// 一个熔断失败计数（4xx 计入熔断，见 forward 的 CountsTowardCircuit），故这里用主动剥离前置。
//
// 为什么整块丢弃而不是只删签名：无签名的 thinking 块对 Anthropic 上游同样非法——被动整流器
// 命中后做的也是同一件事（删 thinking 块），两处结论一致。真实签名一律不动。
func StripPlaceholderSignature(body *convert.Value) (map[string]any, bool) {
	fields := map[string]any{"removedPlaceholderThinkingBlocks": 0}
	if body == nil || !body.IsObject() {
		return fields, false
	}
	messages := body.ArrayField("messages")
	if messages == nil {
		return fields, false
	}

	removed := 0
	applied := false
	for _, message := range messages {
		if message == nil || !message.IsObject() {
			continue
		}
		content := message.ArrayField("content")
		if content == nil {
			continue
		}
		kept := make([]*convert.Value, 0, len(content))
		modified := false
		for _, block := range content {
			if block == nil || !block.IsObject() {
				kept = append(kept, block)
				continue
			}
			blockType, _ := block.StringField("type")
			if blockType != "thinking" && blockType != "redacted_thinking" {
				kept = append(kept, block)
				continue
			}
			signature, _ := block.StringField("signature")
			if !convert.IsPlaceholderThinkingSignature(signature) {
				kept = append(kept, block)
				continue
			}
			removed++
			modified = true
		}
		if modified {
			applied = true
			message.Set("content", convert.NewArray(kept...))
		}
	}

	fields["removedPlaceholderThinkingBlocks"] = removed
	return fields, applied
}
