package convert

import "testing"

// 本文件钉住 citations / annotations 的三线承载（audit-anthropic §2.3 C2、audit-responses §7 #6/#7）。
//
// 改动前网关完全没有引用概念：anthropic 的 `text.citations` 解码时直接落在未知成员上，
// responses 的 `output_text.annotations` 只记一条损失就丢。两者都是客户端可见内容，丢了
// 用户看到的是「有引用标记却没有出处」。
//
// 形状口径：anthropic 的引用数组自己就是权威形状（原生 cited_text 与 url_citation 共存）；
// OpenAI 两线的 `annotations` 必须是带 `type:"url_citation"` 判别字段的对象数组。故跨到
// OpenAI 线时补判别字段，跨到 anthropic 线时原样带过。

// citedResponseAnthropic 拼一份带 citations 的 anthropic 响应。
func citedResponseAnthropic() string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"See [1]",` +
		`"citations":[{"type":"url_citation","url":"https://example.com/a","title":"A",` +
		`"start_index":4,"end_index":7}]}],"stop_reason":"end_turn"}`
}

// citedOutputResponses 拼一份带 annotations 的 responses 响应。
func citedOutputResponses() string {
	return `{"id":"resp_1","object":"response","model":"m","status":"completed","output":[` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"See [1]",` +
		`"annotations":[{"type":"url_citation","url":"https://example.com/a","title":"A",` +
		`"start_index":4,"end_index":7}]}]}]}`
}

// convertAnthropicResponseTo 走 anthropic 源 → 目标线的非流式响应转换。
func convertAnthropicResponseTo(t *testing.T, body string, target WireProtocol) (*Value, LossReport) {
	t.Helper()
	ctx := ConvertCtx{
		ClientFormat:     clientFormatOfProtocol(target),
		TargetProto:      target,
		Model:            "m",
		FromWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeResponse(ProtocolAnthropicMessages, mustParsePayload(t, body), ctx)
	if !ok {
		t.Fatal("anthropic 线必须能解码响应")
	}
	encoded, ok := EncodeResponse(target, decoded.Value, ctx)
	if !ok {
		t.Fatalf("目标线 %s 必须能编码响应", target)
	}
	loss := decoded.Loss
	loss.Entries = append(loss.Entries, encoded.Loss.Entries...)
	return encoded.Body, loss
}

// TestAnthropicCitationsReachChatAnnotations：anthropic → chat 应落为 `message.annotations`。
func TestAnthropicCitationsReachChatAnnotations(t *testing.T) {
	body, loss := convertAnthropicResponseTo(t, citedResponseAnthropic(), ProtocolOpenAIChat)

	message := body.ArrayField("choices")[0].ObjectField("message")
	if content, _ := message.StringField("content"); content != "See [1]" {
		t.Fatalf("正文不应受影响，实际 %q", content)
	}
	annotations := message.ArrayField("annotations")
	if len(annotations) != 1 {
		t.Fatalf("引用应承载为一条 annotations，实际 %s", string(body.MarshalCompact()))
	}
	if kind, _ := annotations[0].StringField("type"); kind != "url_citation" {
		t.Fatalf("chat 侧注释必须带 url_citation 判别字段，实际 %q", kind)
	}
	if url, _ := annotations[0].StringField("url"); url != "https://example.com/a" {
		t.Fatalf("url 应原样保留，实际 %q", url)
	}
	if title, _ := annotations[0].StringField("title"); title != "A" {
		t.Fatalf("title 应原样保留，实际 %q", title)
	}
	if start, ok := startIndex(annotations[0], "start_index"); !ok || start != 4 {
		t.Fatalf("start_index 应原样保留，实际 %v", start)
	}
	if len(loss.Entries) != 0 {
		t.Fatalf("引用全程可承载，不应有损失条目，实际 %+v", loss.Entries)
	}
}

// TestAnthropicCitationsReachResponsesAnnotations：anthropic → responses 应落在 output_text 分片上。
func TestAnthropicCitationsReachResponsesAnnotations(t *testing.T) {
	body, loss := convertAnthropicResponseTo(t, citedResponseAnthropic(), ProtocolOpenAIResponses)

	output := body.ArrayField("output")
	if len(output) != 1 {
		t.Fatalf("应当只有一个 message 项，实际 %s", string(body.MarshalCompact()))
	}
	part := output[0].ArrayField("content")[0]
	if partType, _ := part.StringField("type"); partType != "output_text" {
		t.Fatalf("assistant 文本分片应为 output_text，实际 %q", partType)
	}
	annotations := part.ArrayField("annotations")
	if len(annotations) != 1 {
		t.Fatalf("引用应承载为一条 annotations，实际 %s", string(body.MarshalCompact()))
	}
	if kind, _ := annotations[0].StringField("type"); kind != "url_citation" {
		t.Fatalf("responses 侧注释必须带 url_citation 判别字段，实际 %q", kind)
	}
	if url, _ := annotations[0].StringField("url"); url != "https://example.com/a" {
		t.Fatalf("url 应原样保留，实际 %q", url)
	}
	if len(loss.Entries) != 0 {
		t.Fatalf("引用全程可承载，不应有损失条目，实际 %+v", loss.Entries)
	}
}

// TestResponsesAnnotationsReachAnthropicCitations：responses → anthropic 应落回 `text.citations`。
func TestResponsesAnnotationsReachAnthropicCitations(t *testing.T) {
	body, loss := convertResponsesResponseTo(t, citedOutputResponses(), ProtocolAnthropicMessages)

	content := body.ArrayField("content")
	if len(content) != 1 {
		t.Fatalf("应当只有一个文本块，实际 %s", string(body.MarshalCompact()))
	}
	citations := content[0].ArrayField("citations")
	if len(citations) != 1 {
		t.Fatalf("引用应承载为一条 citations，实际 %s", string(body.MarshalCompact()))
	}
	if url, _ := citations[0].StringField("url"); url != "https://example.com/a" {
		t.Fatalf("url 应原样保留，实际 %q", url)
	}
	if end, ok := startIndex(citations[0], "end_index"); !ok || end != 7 {
		t.Fatalf("end_index 应原样保留，实际 %v", end)
	}
	if len(loss.Entries) != 0 {
		t.Fatalf("引用全程可承载，不应有损失条目，实际 %+v", loss.Entries)
	}
}

// TestChatAnnotationsReachAnthropicCitations：chat → anthropic 应把 message.annotations 落回 citations。
func TestChatAnnotationsReachAnthropicCitations(t *testing.T) {
	const upstream = `{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"See [1]","annotations":[{"type":"url_citation",` +
		`"url":"https://example.com/a","title":"A","start_index":4,"end_index":7}]},` +
		`"finish_reason":"stop"}]}`

	ctx := ConvertCtx{
		ClientFormat:     FormatClaude,
		TargetProto:      ProtocolAnthropicMessages,
		Model:            "m",
		FromWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeResponse(ProtocolOpenAIChat, mustParsePayload(t, upstream), ctx)
	if !ok {
		t.Fatal("chat 线必须能解码响应")
	}
	encoded, ok := EncodeResponse(ProtocolAnthropicMessages, decoded.Value, ctx)
	if !ok {
		t.Fatal("anthropic 线必须能编码响应")
	}
	if len(decoded.Loss.Entries) != 0 {
		t.Fatalf("message.annotations 应被承载，不应记损，实际 %+v", decoded.Loss.Entries)
	}

	content := encoded.Body.ArrayField("content")
	if len(content) != 1 {
		t.Fatalf("应当只有一个文本块，实际 %s", string(encoded.Body.MarshalCompact()))
	}
	if text, _ := content[0].StringField("text"); text != "See [1]" {
		t.Fatalf("正文不应受影响，实际 %q", text)
	}
	citations := content[0].ArrayField("citations")
	if len(citations) != 1 {
		t.Fatalf("引用应承载为一条 citations，实际 %s", string(encoded.Body.MarshalCompact()))
	}
	if url, _ := citations[0].StringField("url"); url != "https://example.com/a" {
		t.Fatalf("url 应原样保留，实际 %q", url)
	}
	if start, ok := startIndex(citations[0], "start_index"); !ok || start != 4 {
		t.Fatalf("start_index 应原样保留，实际 %v", start)
	}
}

// TestNoCitationsAddsNoFields：无引用时不得凭空多出 citations / annotations（空数组也是噪声：
// 上游会把它当作「本块有注解集」并覆盖自己的判定）。
func TestNoCitationsAddsNoFields(t *testing.T) {
	const plain = `{"id":"resp_1","object":"response","model":"m","status":"completed","output":[` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"See [1]"}]}]}`

	for _, target := range []WireProtocol{ProtocolAnthropicMessages, ProtocolOpenAIChat} {
		body, _ := convertResponsesResponseTo(t, plain, target)
		if containsAnyKey(body, "citations", "annotations") {
			t.Fatalf("目标线 %s 无引用时不应写出引用字段：%s", target, string(body.MarshalCompact()))
		}
	}
}

// startIndex 取引用对象里的数字索引；缺字段返回 0,false。
func startIndex(record *Value, key string) (int64, bool) {
	field, ok := record.Get(key)
	if !ok {
		return 0, false
	}
	return field.Int64()
}

// containsAnyKey 递归检查正文是否出现给定键。
func containsAnyKey(value *Value, keys ...string) bool {
	if value == nil {
		return false
	}
	switch {
	case value.IsArray():
		items := value.Items()
		for _, item := range items {
			if containsAnyKey(item, keys...) {
				return true
			}
		}
	case value.IsObject():
		members := value.Members()
		for _, member := range members {
			for _, key := range keys {
				if member.Key == key {
					return true
				}
			}
			if containsAnyKey(member.Value, keys...) {
				return true
			}
		}
	}
	return false
}
