package convert

import (
	"testing"
	"unsafe"
)

// 本文件钉住 citations / annotations 的三线承载（audit-anthropic §2.3 C2、audit-responses §7 #6/#7）
// 与 Block 的尺寸上限。
//
// 改动前网关完全没有引用概念：anthropic 的 `text.citations` 解码时落在未知成员上，
// responses 的 `output_text.annotations` 只记一条损失就丢。两者都是客户端可见内容，丢了
// 用户看到的是「有引用标记却没有出处」。
//
// 形状口径：anthropic 的引用数组自己就是权威形状（原生 cited_text 与 url_citation 共存）；
// OpenAI 两线的 `annotations` 必须是带 `type:"url_citation"` 判别字段的对象数组。故跨到
// OpenAI 线时补判别字段，跨到 anthropic 线时原样带过。

// TestBlockSizeStaysAt192 钉住 Block 的尺寸：它恰在 Go 的一档 size class 上，多 8 字节的承载位
// 就会把实际占位顶到 208，即**每个块、每个请求**都多付 16 B——哪怕该请求根本没有引用。
//
// 引用承载位因此必须是按需分配的指针，且两个 bool（Redacted / FromDataURL）必须紧邻摆放共用
// 一个对齐槽；两者任一被改回「各占一槽」，本用例即红。
func TestBlockSizeStaysAt192(t *testing.T) {
	const want = 192
	got := unsafe.Sizeof(Block{})
	t.Logf("Block=%d", got)
	if got != want {
		t.Fatalf("Block 必须守在 %d 字节，实际 %d 字节（跨出本档即每个块多付 16 B）", want, got)
	}
}

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

// plainResponse 按源线拼一份**无引用**的文本响应，用于「不凭空多出字段」的钉子。
func plainResponse(upstream WireProtocol) string {
	switch upstream {
	case ProtocolAnthropicMessages:
		return `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
			`"content":[{"type":"text","text":"See [1]"}],"stop_reason":"end_turn"}`
	case ProtocolOpenAIChat:
		return `{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,` +
			`"message":{"role":"assistant","content":"See [1]"},"finish_reason":"stop"}]}`
	default:
		return `{"id":"resp_1","object":"response","model":"m","status":"completed","output":[` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"See [1]"}]}]}`
	}
}

// convertResponseAcross 走「源线 → 目标线」的非流式响应转换，返回出站正文与两侧合并的损失。
func convertResponseAcross(t *testing.T, upstream WireProtocol, body string, target WireProtocol) (*Value, LossReport) {
	t.Helper()
	ctx := ConvertCtx{
		ClientFormat:     clientFormatOfProtocol(target),
		TargetProto:      upstream,
		Model:            "m",
		FromWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeResponse(upstream, mustParsePayload(t, body), ctx)
	if !ok {
		t.Fatalf("%s 线必须能解码响应", upstream)
	}
	encoded, ok := EncodeResponse(target, decoded.Value, ctx)
	if !ok {
		t.Fatalf("目标线 %s 必须能编码响应", target)
	}
	loss := decoded.Loss
	loss.Entries = append(loss.Entries, encoded.Loss.Entries...)
	return encoded.Body, loss
}

// TestCitationsAnthropicToChatAnnotations：anthropic → chat 应落为 `message.annotations`。
func TestCitationsAnthropicToChatAnnotations(t *testing.T) {
	body, loss := convertResponseAcross(t, ProtocolAnthropicMessages, citedResponseAnthropic(), ProtocolOpenAIChat)

	message := body.ArrayField("choices")[0].ObjectField("message")
	if content, _ := message.StringField("content"); content != "See [1]" {
		t.Fatalf("正文不应受影响，实际 %q", content)
	}
	annotations := message.ArrayField("annotations")
	if len(annotations) != 1 {
		t.Fatalf("引用应承载为一条 annotations，实际 %s", body.MarshalCompact())
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
	if start, ok := citationIntField(annotations[0], "start_index"); !ok || start != 4 {
		t.Fatalf("start_index 应原样保留，实际 %v", start)
	}
	if len(loss.Entries) != 0 {
		t.Fatalf("引用全程可承载，不应有损失条目，实际 %+v", loss.Entries)
	}
}

// TestCitationsAnthropicToResponsesAnnotations：anthropic → responses 应落在 output_text 分片上。
func TestCitationsAnthropicToResponsesAnnotations(t *testing.T) {
	body, loss := convertResponseAcross(t, ProtocolAnthropicMessages, citedResponseAnthropic(), ProtocolOpenAIResponses)

	output := body.ArrayField("output")
	if len(output) != 1 {
		t.Fatalf("应当只有一个 message 项，实际 %s", body.MarshalCompact())
	}
	part := output[0].ArrayField("content")[0]
	if partType, _ := part.StringField("type"); partType != "output_text" {
		t.Fatalf("assistant 文本分片应为 output_text，实际 %q", partType)
	}
	annotations := part.ArrayField("annotations")
	if len(annotations) != 1 {
		t.Fatalf("引用应承载为一条 annotations，实际 %s", body.MarshalCompact())
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

// TestCitationsResponsesToAnthropic：responses → anthropic 应落回 `text.citations`。
func TestCitationsResponsesToAnthropic(t *testing.T) {
	body, loss := convertResponseAcross(t, ProtocolOpenAIResponses, citedOutputResponses(), ProtocolAnthropicMessages)

	content := body.ArrayField("content")
	if len(content) != 1 {
		t.Fatalf("应当只有一个文本块，实际 %s", body.MarshalCompact())
	}
	citations := content[0].ArrayField("citations")
	if len(citations) != 1 {
		t.Fatalf("引用应承载为一条 citations，实际 %s", body.MarshalCompact())
	}
	if url, _ := citations[0].StringField("url"); url != "https://example.com/a" {
		t.Fatalf("url 应原样保留，实际 %q", url)
	}
	if end, ok := citationIntField(citations[0], "end_index"); !ok || end != 7 {
		t.Fatalf("end_index 应原样保留，实际 %v", end)
	}
	if len(loss.Entries) != 0 {
		t.Fatalf("引用全程可承载，不应有损失条目，实际 %+v", loss.Entries)
	}
}

// TestCitationsChatToAnthropic：chat → anthropic 应把 message.annotations 落回 citations。
func TestCitationsChatToAnthropic(t *testing.T) {
	const upstream = `{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"See [1]","annotations":[{"type":"url_citation",` +
		`"url":"https://example.com/a","title":"A","start_index":4,"end_index":7}]},` +
		`"finish_reason":"stop"}]}`

	body, loss := convertResponseAcross(t, ProtocolOpenAIChat, upstream, ProtocolAnthropicMessages)
	if len(loss.Entries) != 0 {
		t.Fatalf("message.annotations 应被承载，不应记损，实际 %+v", loss.Entries)
	}

	content := body.ArrayField("content")
	if len(content) != 1 {
		t.Fatalf("应当只有一个文本块，实际 %s", body.MarshalCompact())
	}
	if text, _ := content[0].StringField("text"); text != "See [1]" {
		t.Fatalf("正文不应受影响，实际 %q", text)
	}
	citations := content[0].ArrayField("citations")
	if len(citations) != 1 {
		t.Fatalf("引用应承载为一条 citations，实际 %s", body.MarshalCompact())
	}
	if url, _ := citations[0].StringField("url"); url != "https://example.com/a" {
		t.Fatalf("url 应原样保留，实际 %q", url)
	}
	if start, ok := citationIntField(citations[0], "start_index"); !ok || start != 4 {
		t.Fatalf("start_index 应原样保留，实际 %v", start)
	}
}

// TestCitationsAbsentWritesNoFields：无引用时不得凭空多出 citations / annotations（空数组也是噪声：
// 上游会把它当作「本块有注解集」并覆盖自己的判定）。
func TestCitationsAbsentWritesNoFields(t *testing.T) {
	targets := []WireProtocol{ProtocolAnthropicMessages, ProtocolOpenAIChat, ProtocolOpenAIResponses}
	upstreams := []WireProtocol{ProtocolAnthropicMessages, ProtocolOpenAIChat, ProtocolOpenAIResponses}
	for _, upstream := range upstreams {
		for _, target := range targets {
			t.Run(string(upstream)+"-to-"+string(target), func(t *testing.T) {
				body, _ := convertResponseAcross(t, upstream, plainResponse(upstream), target)
				if containsAnyKey(body, "citations", "annotations") {
					t.Fatalf("无引用时不应写出引用字段：%s", body.MarshalCompact())
				}
			})
		}
	}
}

// BenchmarkCitationsAbsentResponseConvert 量**无引用**路径的每请求成本：承载位必须只在真有
// 引用时才付钱，所以本基准的分配数不得因 citations 承载而变化（改前改后同值）。
func BenchmarkCitationsAbsentResponseConvert(b *testing.B) {
	payload, err := ParseJSON([]byte(plainResponse(ProtocolOpenAIResponses)))
	if err != nil {
		b.Fatalf("解析夹具失败: %v", err)
	}
	ctx := ConvertCtx{
		ClientFormat: FormatClaude,
		TargetProto:  ProtocolOpenAIResponses,
		Model:        "m",
	}
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		decoded, ok := DecodeResponse(ProtocolOpenAIResponses, payload, ctx)
		if !ok {
			b.Fatal("responses 线必须能解码响应")
		}
		if _, ok := EncodeResponse(ProtocolAnthropicMessages, decoded.Value, ctx); !ok {
			b.Fatal("anthropic 线必须能编码响应")
		}
	}
}

// citationIntField 取引用对象里的数字索引；缺字段返回 0,false。
func citationIntField(record *Value, key string) (int64, bool) {
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
		for _, item := range value.Items() {
			if containsAnyKey(item, keys...) {
				return true
			}
		}
	case value.IsObject():
		for _, member := range value.Members() {
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
