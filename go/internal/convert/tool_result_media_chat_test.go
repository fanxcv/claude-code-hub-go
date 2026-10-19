package convert

import (
	"strings"
	"testing"
)

// 本文件钉住 chat 目标线两处承载/记损行为（审计 audit-anthropic.md §2.2 M4/M6）：
//
//	1. tool_result 内嵌图片不再丢：文本留在 role:tool 消息，图搬到紧随这批 tool 消息之后的
//	   role:user 消息上（chat 的 tool content 只能是字符串，无图片承载位）；
//	2. tool_result 内嵌 document 仍记损（chat 线无 file 内容块的可靠承载形态）。

const testToolResultChatImageB64 = "iVBORw0KGgoAAAANSUhEUg=="

// chatToolResultPayload 造一个「assistant 调工具 → tool_result 带内嵌块」的 anthropic 请求体。
func chatToolResultPayload(toolResultContent string) string {
	return `{"model":"m","max_tokens":64,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"截图"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"screenshot","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` + toolResultContent + `]}]}],` +
		`"tools":[{"name":"screenshot","input_schema":{"type":"object"}}]}`
}

// encodeAnthropicToChat 走真实解码 + 编码，返回 chat 出站 body 与两侧损失合并后的报告。
func encodeAnthropicToChat(t *testing.T, payload string) (*Value, LossReport) {
	t.Helper()
	ctx := ConvertCtx{
		ClientFormat:   FormatClaude,
		TargetProto:    ProtocolOpenAIChat,
		Model:          "m",
		ToWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeRequest(ProtocolAnthropicMessages, mustParsePayload(t, payload), ctx)
	if !ok {
		t.Fatal("anthropic 正文必须能解码")
	}
	encoded, ok := EncodeRequest(ProtocolOpenAIChat, decoded.Value, ctx)
	if !ok {
		t.Fatal("chat 目标必须能编码")
	}
	loss := decoded.Loss
	loss.Entries = append(loss.Entries, encoded.Loss.Entries...)
	return encoded.Body, loss
}

// chatRoles 取出出站 messages 的 role 序列，用于断言消息顺序。
func chatRoles(t *testing.T, body *Value) []string {
	t.Helper()
	messages, present := body.Get("messages")
	if !present || messages == nil || !messages.IsArray() {
		t.Fatalf("出站体没有 messages 数组：%s", body.MarshalCompact())
	}
	roles := []string{}
	for _, message := range messages.Items() {
		role, _ := stringField(message, "role")
		roles = append(roles, role)
	}
	return roles
}

// TestChatToolResultImageMovesToFollowingUserMessage 是 M4 的主断言：图必须送出，且落在
// tool 消息之后那条 user 消息上；文本仍留在 tool 消息里。
func TestChatToolResultImageMovesToFollowingUserMessage(t *testing.T) {
	body, loss := encodeAnthropicToChat(t, chatToolResultPayload(
		`{"type":"text","text":"截图如下"},`+
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+testToolResultChatImageB64+`"}}`))

	if got := strings.Join(chatRoles(t, body), ","); got != "user,assistant,tool,user" {
		t.Fatalf("消息顺序应为 user,assistant,tool,user，实际 %s：%s", got, body.MarshalCompact())
	}
	messages, _ := body.Get("messages")
	items := messages.Items()
	toolContent, _ := stringField(items[2], "content")
	if toolContent != "截图如下" {
		t.Fatalf("文本应留在 tool 消息里，实际 %q", toolContent)
	}
	// 图必须在这一批 tool 消息之后的 user 消息上，且是 chat 的 image_url 承载体。
	imageMessage := items[3]
	if role, _ := stringField(imageMessage, "role"); role != "user" {
		t.Fatalf("紧跟 tool 消息之后应是 user，实际 %q", role)
	}
	content, present := imageMessage.Get("content")
	if !present || content == nil || content.Kind() != "array" {
		t.Fatalf("承载图片的 user 消息 content 应为数组：%s", body.MarshalCompact())
	}
	parts := content.Items()
	if len(parts) != 1 {
		t.Fatalf("应恰好一段图片部件，实际 %d：%s", len(parts), body.MarshalCompact())
	}
	if kind, _ := stringField(parts[0], "type"); kind != "image_url" {
		t.Fatalf("部件类型应为 image_url，实际 %q", kind)
	}
	imageURL, _ := parts[0].Get("image_url")
	url, _ := stringField(imageURL, "url")
	if !strings.HasPrefix(url, "data:image/png;base64,") || !strings.Contains(url, testToolResultChatImageB64) {
		t.Fatalf("图片应以 data URL 承载原字节，实际 %q", url)
	}
	// 图已送达，不得再出现 image/dropped（那代表又丢了）。
	for _, entry := range loss.Entries {
		if entry.Capability == LossImage && entry.Action == LossDropped {
			t.Fatalf("图已送出却记了丢弃：%+v", entry)
		}
	}
}

// TestChatToolResultTextStaysSingleToolMessage 钉住纯文本工具结果的行为不变：
// 不因为这次改动而凭空多出一条 user 消息。
func TestChatToolResultTextStaysSingleToolMessage(t *testing.T) {
	body, _ := encodeAnthropicToChat(t, chatToolResultPayload(`{"type":"text","text":"晴 25"}`))

	if got := strings.Join(chatRoles(t, body), ","); got != "user,assistant,tool" {
		t.Fatalf("纯文本工具结果不得多出消息，实际 %s：%s", got, body.MarshalCompact())
	}
	messages, _ := body.Get("messages")
	toolMessage := messages.Items()[2]
	if content, _ := stringField(toolMessage, "content"); content != "晴 25" {
		t.Fatalf("文本应原样留在 tool 消息里，实际 %q", content)
	}
}

// TestChatToolResultImageKeepsToolBatchContiguousAndReasoningOwner 是 M4 的边界钉子：
// 一轮里两个带图的 tool 结果时，两条 tool 消息必须连续（不能被图片 user 消息插断），图片合并成
// 一条 user 消息；且紧随其后的 assistant 仍持有自己的 reasoning_content（不被抢走、不被插位）。
func TestChatToolResultImageKeepsToolBatchContiguousAndReasoningOwner(t *testing.T) {
	payload := `{"model":"m","max_tokens":64,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"截图"}]},` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_1","name":"screenshot","input":{}},` +
		`{"type":"tool_use","id":"toolu_2","name":"screenshot","input":{}}]},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
		`{"type":"text","text":"a"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + testToolResultChatImageB64 + `"}}]},` +
		`{"type":"tool_result","tool_use_id":"toolu_2","content":[` +
		`{"type":"text","text":"b"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + testToolResultChatImageB64 + `"}}]}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"看完"}]}],` +
		`"tools":[{"name":"screenshot","input_schema":{"type":"object"}}]}`

	body, _ := encodeAnthropicToChat(t, payload)
	if got := strings.Join(chatRoles(t, body), ","); got != "user,assistant,tool,tool,user,assistant" {
		t.Fatalf("两条 tool 消息必须连续、图片合并成一条 user，实际 %s：%s", got, body.MarshalCompact())
	}
	messages, _ := body.Get("messages")
	items := messages.Items()
	content, present := items[4].Get("content")
	if !present || content == nil || content.Kind() != "array" {
		t.Fatalf("图片 user 消息 content 应为数组：%s", body.MarshalCompact())
	}
	if parts := content.Items(); len(parts) != 2 {
		t.Fatalf("两张图应合并进一条 user 消息，实际 %d 段：%s", len(parts), body.MarshalCompact())
	}
	// 末尾 assistant 的正文必须原样保留：图片 user 消息替它占位就会把它的内容挤走。
	if text, _ := stringField(items[5], "content"); text != "看完" {
		t.Fatalf("图片 user 消息之后那条 assistant 的正文必须完好，实际 %q：%s", text, body.MarshalCompact())
	}
}

// TestChatToolResultDocumentRecordedAsLoss 钉住 M6 的本轮结论：chat 线没有可靠的 file 内容块
// 承载形态，故 document 仍丢，但归到 document 专用类别（而非 catch-all），且不得连累同批的图。
func TestChatToolResultDocumentRecordedAsLoss(t *testing.T) {
	body, loss := encodeAnthropicToChat(t, chatToolResultPayload(
		`{"type":"text","text":"见附件"},`+
			`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}`))

	if got := strings.Join(chatRoles(t, body), ","); got != "user,assistant,tool" {
		t.Fatalf("纯文档工具结果不得多出 user 消息，实际 %s：%s", got, body.MarshalCompact())
	}
	if body.MarshalCompact() != "" && strings.Contains(body.MarshalCompact(), "JVBERi0") {
		t.Fatalf("chat 出站不得出现 PDF 字节（无承载位）：%s", body.MarshalCompact())
	}
	found := false
	for _, entry := range loss.Entries {
		if entry.Capability == LossDocument && entry.Action == LossDropped {
			found = true
		}
	}
	if !found {
		t.Fatalf("被丢的 document 必须记 document 类损失，实际 %+v", loss.Entries)
	}
}

// TestChatTrailingToolResultImageKeepsReasoningBackfill 钉住两处修复的交叉点：带图的 tool 结果
// 收尾时，图片 user 消息**不得**把末条变成 user（否则「历史以 tool 结果收尾」的判据失效，
// 末轮 assistant 的空 reasoning_content 补不上，上游回 400）。
func TestChatTrailingToolResultImageKeepsReasoningBackfill(t *testing.T) {
	payload := `{"model":"m","max_tokens":64,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"截图"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"screenshot","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + testToolResultChatImageB64 + `"}}]}]}],` +
		`"tools":[{"name":"screenshot","input_schema":{"type":"object"}}]}`

	body, _ := encodeAnthropicToChat(t, payload)
	if got := strings.Join(chatRoles(t, body), ","); got != "user,assistant,tool,user" {
		t.Fatalf("消息顺序应为 user,assistant,tool,user，实际 %s：%s", got, body.MarshalCompact())
	}
	messages, _ := body.Get("messages")
	owner := messages.Items()[1]
	reasoning, present := owner.Get("reasoning_content")
	if !present || reasoning == nil {
		t.Fatalf("末轮工具调用的 assistant 缺 reasoning_content——上游会回 400：%s", body.MarshalCompact())
	}
	if text, _ := reasoning.String(); text != "" {
		t.Fatalf("reasoning_content = %q，期望空串（不得伪造思考文本）", text)
	}
}
