package rectify

import (
	"regexp"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// thinking signature 整流器的三种触发类型，与 Node 的联合类型同值。
const (
	triggerInvalidSignatureInThinkingBlock = "invalid_signature_in_thinking_block"
	triggerAssistantMustStartWithThinking  = "assistant_message_must_start_with_thinking"
	triggerInvalidRequest                  = "invalid_request"
)

// expectedThinkingOrRedacted 复刻 Node 的正则（thinking-signature-rectifier.ts:39-42）：
// JS 的 /.../i 不跨行匹配，RE2 的 `.` 默认同样不跨行。
var expectedThinkingOrRedacted = regexp.MustCompile("expected\\s*`?thinking`?\\s*or\\s*`?redacted_thinking`?.*found\\s*`?tool_use`?")

// invalidRequestPattern 复刻 Node 的 /非法请求|illegal request|invalid request/i。
var invalidRequestPattern = regexp.MustCompile("(?i)非法请求|illegal request|invalid request")

// detectThinkingSignature 复刻 detectThinkingSignatureRectifierTrigger
// （thinking-signature-rectifier.ts:28-94）：按序判定，命中即返回（顺序即优先级）。
func detectThinkingSignature(errorMessage string) string {
	if errorMessage == "" {
		return ""
	}
	lower := strings.ToLower(errorMessage)

	if strings.Contains(lower, "must start with a thinking block") ||
		expectedThinkingOrRedacted.MatchString(errorMessage) {
		return triggerAssistantMustStartWithThinking
	}
	if strings.Contains(lower, "invalid") && strings.Contains(lower, "signature") &&
		strings.Contains(lower, "thinking") && strings.Contains(lower, "block") {
		return triggerInvalidSignatureInThinkingBlock
	}
	// signature 字段缺失（"xxx.signature: Field required"）
	if strings.Contains(lower, "signature") && strings.Contains(lower, "field required") {
		return triggerInvalidSignatureInThinkingBlock
	}
	// signature 字段不被上游接受（"Extra inputs are not permitted"）
	if strings.Contains(lower, "signature") && strings.Contains(lower, "extra inputs are not permitted") {
		return triggerInvalidSignatureInThinkingBlock
	}
	// thinking/redacted_thinking 块被修改
	if (strings.Contains(lower, "thinking") || strings.Contains(lower, "redacted_thinking")) &&
		strings.Contains(lower, "cannot be modified") {
		return triggerInvalidSignatureInThinkingBlock
	}
	if invalidRequestPattern.MatchString(errorMessage) {
		return triggerInvalidRequest
	}
	return ""
}

// rectifyThinkingSignature 复刻 rectifyAnthropicRequestMessage
// （thinking-signature-rectifier.ts:96-193）：删 thinking/redacted_thinking 块、
// 剥非 thinking 块上遗留的 signature 字段，并在「thinking 启用但最后一条 assistant 消息
// 未以 thinking 开头且含 tool_use」时删顶层 thinking（否则必定继续 400）。
func rectifyThinkingSignature(body *convert.Value) (map[string]any, bool) {
	messages := body.ArrayField("messages")
	fields := map[string]any{
		"removedThinkingBlocks":         0,
		"removedRedactedThinkingBlocks": 0,
		"removedSignatureFields":        0,
	}
	if messages == nil {
		return fields, false
	}

	removedThinking := 0
	removedRedacted := 0
	removedSignatures := 0
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
		contentModified := false
		for _, block := range content {
			if block == nil || !block.IsObject() {
				kept = append(kept, block)
				continue
			}
			switch blockType, _ := block.StringField("type"); blockType {
			case "thinking":
				removedThinking++
				contentModified = true
				continue
			case "redacted_thinking":
				removedRedacted++
				contentModified = true
				continue
			}
			if block.Has("signature") {
				block.Delete("signature")
				removedSignatures++
				contentModified = true
			}
			kept = append(kept, block)
		}
		if contentModified {
			applied = true
			message.Set("content", convert.NewArray(kept...))
		}
	}

	if deleteThinkingWithoutPrefix(body, messages) {
		applied = true
	}

	fields["removedThinkingBlocks"] = removedThinking
	fields["removedRedactedThinkingBlocks"] = removedRedacted
	fields["removedSignatureFields"] = removedSignatures
	return fields, applied
}

// deleteThinkingWithoutPrefix 是 Node 的兜底分支：thinking 显式启用、最后一条 assistant 消息
// （倒序找第一条带数组 content 的 assistant）首块不是 thinking/redacted_thinking、且含 tool_use 时，
// 删掉顶层 thinking。返回是否删除。
func deleteThinkingWithoutPrefix(body *convert.Value, messages []*convert.Value) bool {
	thinking := body.ObjectField("thinking")
	if thinking == nil {
		return false
	}
	if thinkingType, _ := thinking.StringField("type"); thinkingType != "enabled" {
		return false
	}

	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message == nil || !message.IsObject() {
			continue
		}
		if role, _ := message.StringField("role"); role != "assistant" {
			continue
		}
		content := message.ArrayField("content")
		if content == nil || len(content) == 0 {
			continue
		}

		firstType := ""
		if first := content[0]; first != nil && first.IsObject() {
			firstType, _ = first.StringField("type")
		}
		if firstType == "thinking" || firstType == "redacted_thinking" {
			return false
		}
		for _, block := range content {
			if block == nil || !block.IsObject() {
				continue
			}
			if blockType, _ := block.StringField("type"); blockType == "tool_use" {
				body.Delete("thinking")
				return true
			}
		}
		return false
	}
	return false
}
