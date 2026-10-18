package convert

import (
	"strings"
	"testing"
)

// 本文件钉住 redacted_thinking 的出口形状：它的 Text 是密文，不是可读摘要。
//
// 缺陷实测（2026-10-18 审计 T8/T9/R11）：responsesReasoningItem 只看 Kind/Text，不看 Redacted，
// 于是把 base64 密文写进 summary[].text，客户端可见字段里出现一段 BLOB。
func TestResponsesRedactedThinkingIsNotVisibleSummary(t *testing.T) {
	const blob = "ENCRYPTED_BLOB_PAYLOAD"
	options := &responsesRenderOptions{direction: "response", loss: &LossCollector{}}
	item := responsesReasoningItem([]Block{
		{Kind: BlockThinking, Text: "visible thought"},
		{Kind: BlockThinking, Text: blob, Redacted: true},
	}, options)
	if item == nil {
		t.Fatal("有可见思考块时 reasoning item 不应为空")
	}
	rendered := string(item.MarshalCompact())
	if summary := item.ObjectField("summary"); summary != nil && strings.Contains(string(summary.MarshalCompact()), blob) {
		t.Fatalf("密文不得出现在 summary，实际 %s", rendered)
	}
	if !strings.Contains(rendered, `"encrypted_content":"`+blob+`"`) {
		t.Fatalf("密文应落到 encrypted_content，实际 %s", rendered)
	}
	if !strings.Contains(rendered, "visible thought") {
		t.Fatalf("可见思考不应受影响，实际 %s", rendered)
	}
}

// 只有密文块时也必须产出 reasoning item（否则加密思考整块丢失）。
func TestResponsesRedactedThinkingAloneStillEmitsItem(t *testing.T) {
	options := &responsesRenderOptions{direction: "response", loss: &LossCollector{}}
	item := responsesReasoningItem([]Block{
		{Kind: BlockThinking, Text: "BLOB", Redacted: true},
	}, options)
	if item == nil {
		t.Fatal("只有密文块时也必须产出 reasoning item")
	}
	if item.ObjectField("summary") != nil {
		t.Fatal("只有密文时不应凭空产出 summary")
	}
	if got, _ := item.StringField("encrypted_content"); got != "BLOB" {
		t.Fatalf("encrypted_content 应为 BLOB，实际 %q", got)
	}
}
