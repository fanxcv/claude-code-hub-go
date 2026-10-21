package convert

import (
	"strings"
	"testing"
)

// redacted_thinking 的 Text 是密文，不是可读摘要：写进 summary[].text 就是把一段 base64
// 糊到客户端可见字段上。本线的承载位是 encrypted_content，密文必须走那里。

func TestRedactedThinkingGoesToEncryptedContent(t *testing.T) {
	const blob = "ENCRYPTED_BLOB_PAYLOAD"
	ctx := ConvertCtx{ClientFormat: FormatResponse, TargetProto: ProtocolOpenAIResponses, Model: "m"}
	decoded, ok := DecodeRequest(ProtocolAnthropicMessages, mustParsePayload(t, `{
		"model":"claude","max_tokens":16,
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"先想想","signature":"sig"},
			{"type":"redacted_thinking","data":"`+blob+`"},
			{"type":"text","text":"ans"}]}]}`), ctx)
	if !ok {
		t.Fatal("anthropic 线必须能解码")
	}
	encoded, ok := EncodeRequest(ProtocolOpenAIResponses, decoded.Value, ctx)
	if !ok {
		t.Fatal("responses 线必须能编码")
	}

	// 密文只许出现在 encrypted_content，summary[].text 里一个字都不许有。
	output := encoded.Body.ArrayField("input")
	summaryText := ""
	encrypted := ""
	for _, item := range output {
		if itemType, _ := stringField(item, "type"); itemType != "reasoning" {
			continue
		}
		if got, _ := stringField(item, "encrypted_content"); got != "" {
			encrypted = got
		}
		for _, part := range asUnknownArray(fieldOrNil(item, "summary")) {
			text, _ := stringField(part, "text")
			summaryText += text
		}
	}
	if strings.Contains(summaryText, blob) {
		t.Fatalf("密文不得出现在 summary[].text：%s", encoded.Body.MarshalCompact())
	}
	if encrypted != blob {
		t.Fatalf("密文应落在 encrypted_content=%q，实际 %q（%s）", blob, encrypted, encoded.Body.MarshalCompact())
	}
	if !strings.Contains(summaryText, "先想想") {
		t.Fatalf("可读思考不应受影响：%s", encoded.Body.MarshalCompact())
	}

	// 只有密文块时也必须产出 reasoning 项（否则加密思考整块丢失），且不得凭空造 summary。
	only, _ := DecodeRequest(ProtocolAnthropicMessages, mustParsePayload(t, `{
		"model":"claude","max_tokens":16,
		"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"BLOB"}]}]}`), ctx)
	onlyEncoded, _ := EncodeRequest(ProtocolOpenAIResponses, only.Value, ctx)
	items := onlyEncoded.Body.ArrayField("input")
	if len(items) != 1 {
		t.Fatalf("只有密文块时仍须产出一条 reasoning 项：%s", onlyEncoded.Body.MarshalCompact())
	}
	if items[0].ObjectField("summary") != nil {
		t.Fatalf("只有密文时不应凭空产出 summary：%s", onlyEncoded.Body.MarshalCompact())
	}
	if got, _ := items[0].StringField("encrypted_content"); got != "BLOB" {
		t.Fatalf("encrypted_content 应为 BLOB，实际 %q", got)
	}
}
