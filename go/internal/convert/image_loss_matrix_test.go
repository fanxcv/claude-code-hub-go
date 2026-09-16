package convert

import (
	"fmt"
	"strings"
	"testing"
)

// 本文件把「同一张逻辑图在一次转换里只记一条、且以**最终结果**为准」钉成矩阵。
//
// 为什么不能只钉一格：审查实测出的两条复现路径（responses→chat 的 GIF、chat assistant
// data URL→responses）都不在原用例那一格里——原用例只覆盖 responses→chat / user / PNG，
// 于是「解码侧记一次表示归一 + 编码侧再记一次丢弃」的双计长期存活。
//
// 矩阵 = 六向（跨协议对）× role（user / assistant）× 媒体形态（PNG data URL / GIF data URL /
// 远程 URL）。每格断言三件事：图片条目数、action、以及**目标正文明明有没有这张图**
// （只对账簿记账、不对账「图到底送出去没有」的话，把图丢掉也能让计数看起来对）。

const (
	testImagePNGBase64 = "iVBORw0KGgoAAAANSUhEUg=="
	testImageGIFBase64 = "R0lGODlhAQABAAAAACw="
	testImageRemoteURL = "https://example.com/cat.png"
)

// mediaForm 是一种客户端媒体形态的三种写法：OpenAI 两线的 wire 值、anthropic 的 source 对象，
// 以及「客户端是否用 data URL 表达」（只有用 data URL 表达时才有表示归一这条损失）。
type mediaForm struct {
	name string
	// wire 是 chat / responses 线上该图在正文里的写法。
	wire string
	// sourceJSON 是 anthropic 线上该图的 `source` 对象（anthropic 不认 data URL）。
	sourceJSON string
	// dataURLClient 报告客户端是否用 data URL 表达这幅图。
	dataURLClient bool
	// mediaType / data 是这幅图在客户端语义上的**内联**取值（远程形态时为空）；
	// url 是**远程**取值。目标线上应有的承载值由这两组值算出（见 expectedTargetImageValue）。
	mediaType string
	data      string
	url       string
}

var imageMediaForms = []mediaForm{
	{
		name:          "PNG data URL",
		wire:          "data:image/png;base64," + testImagePNGBase64,
		sourceJSON:    `{"type":"base64","media_type":"image/png","data":"` + testImagePNGBase64 + `"}`,
		dataURLClient: true,
		mediaType:     "image/png",
		data:          testImagePNGBase64,
	},
	{
		name:          "GIF data URL",
		wire:          "data:image/gif;base64," + testImageGIFBase64,
		sourceJSON:    `{"type":"base64","media_type":"image/gif","data":"` + testImageGIFBase64 + `"}`,
		dataURLClient: true,
		mediaType:     "image/gif",
		data:          testImageGIFBase64,
	},
	{
		name:       "远程 URL",
		wire:       testImageRemoteURL,
		sourceJSON: `{"type":"url","url":"` + testImageRemoteURL + `"}`,
		url:        testImageRemoteURL,
	},
}

// chainOutcome 是一格的期望结果。
type chainOutcome struct {
	// delivered 报告目标线最终收下了这张图。
	delivered bool
	// action 为空串表示「这一格不得有任何 image 条目」。
	action LossAction
	// detail 非空时逐字比对。
	detail string
}

func rewrittenOutcome() chainOutcome {
	return chainOutcome{delivered: true, action: LossRewritten, detail: "data_url_to_base64"}
}

// imageChainDirections 是六个跨协议方向及其逐格期望。
//
// 期望的推导规则只有三条：
//  1. 目标线收下图且客户端用 data URL 表达 → 一条 `rewritten`（表示归一）；
//  2. 目标线收下图但客户端给的本来就是 base64 / 远程 URL → 无条目（编码侧的承载形态变化不算损失）；
//  3. 目标线整块丢掉这张图 → 一条 `dropped`（且**不得**同时留下解码侧那条 rewritten）。
func imageChainDirections() []struct {
	name   string
	source WireProtocol
	target WireProtocol
	expect func(role string, form mediaForm) chainOutcome
} {
	return []struct {
		name   string
		source WireProtocol
		target WireProtocol
		expect func(role string, form mediaForm) chainOutcome
	}{
		{
			name: "chat → responses", source: ProtocolOpenAIChat, target: ProtocolOpenAIResponses,
			expect: func(role string, form mediaForm) chainOutcome {
				if role == "assistant" {
					// responses 线没有 assistant 图的承载位（responsesEncodeInput 的 assistant_image）。
					return chainOutcome{action: LossDropped, detail: "assistant_image"}
				}
				if form.dataURLClient {
					return rewrittenOutcome()
				}
				return chainOutcome{delivered: true}
			},
		},
		{
			name: "chat → anthropic", source: ProtocolOpenAIChat, target: ProtocolAnthropicMessages,
			expect: func(role string, form mediaForm) chainOutcome {
				if form.dataURLClient {
					return rewrittenOutcome()
				}
				return chainOutcome{delivered: true}
			},
		},
		{
			name: "responses → chat", source: ProtocolOpenAIResponses, target: ProtocolOpenAIChat,
			expect: func(role string, form mediaForm) chainOutcome {
				if form.name == "远程 URL" {
					// 远程 URL 原样透传：responses 的 input_image 不带媒体类型，而 chat 的
					// image_url 不需要它（闸门只对需合成 data URL 的内联块成立）。
					// 此前这一格是 dropped / `media_type:`（空类型）——图整幅没送出去。
					return chainOutcome{delivered: true}
				}
				// chat 线不收 GIF（chatImageBlockToPart 的 image/gif 分支）。
				if form.name == "GIF data URL" {
					return chainOutcome{action: LossDropped, detail: "image/gif"}
				}
				if form.dataURLClient {
					return rewrittenOutcome()
				}
				return chainOutcome{delivered: true}
			},
		},
		{
			name: "responses → anthropic", source: ProtocolOpenAIResponses, target: ProtocolAnthropicMessages,
			expect: func(role string, form mediaForm) chainOutcome {
				if form.dataURLClient {
					return rewrittenOutcome()
				}
				return chainOutcome{delivered: true}
			},
		},
		{
			name: "anthropic → chat", source: ProtocolAnthropicMessages, target: ProtocolOpenAIChat,
			expect: func(role string, form mediaForm) chainOutcome {
				if form.name == "远程 URL" {
					// 与 responses→chat 同一根因（`source.type=url` 不带媒体类型），同一修法：送达。
					return chainOutcome{delivered: true}
				}
				if form.name == "GIF data URL" {
					return chainOutcome{action: LossDropped, detail: "image/gif"}
				}
				// anthropic 的 base64 源不是 data URL：编码侧转成 data URL 不记损。
				return chainOutcome{delivered: true}
			},
		},
		{
			name: "anthropic → responses", source: ProtocolAnthropicMessages, target: ProtocolOpenAIResponses,
			expect: func(role string, form mediaForm) chainOutcome {
				if role == "assistant" {
					return chainOutcome{action: LossDropped, detail: "assistant_image"}
				}
				return chainOutcome{delivered: true}
			},
		},
	}
}

// TestImageLossOncePerChainAcrossDirections 是矩阵主体。
func TestImageLossOncePerChainAcrossDirections(t *testing.T) {
	for _, direction := range imageChainDirections() {
		for _, role := range []string{"user", "assistant"} {
			for _, form := range imageMediaForms {
				t.Run(direction.name+"/"+role+"/"+form.name, func(t *testing.T) {
					want := direction.expect(role, form)
					body := imageChainBody(direction.source, role, form)
					ctx := ConvertCtx{
						ClientFormat:   clientFormatFor(direction.source),
						TargetProto:    direction.target,
						Model:          "m",
						ToWireToolName: NormalizeToolName,
					}
					decoded, ok := DecodeRequest(direction.source, mustParsePayload(t, body), ctx)
					if !ok {
						t.Fatalf("%s 必须能解码", direction.source)
					}
					encoded, ok := EncodeRequest(direction.target, decoded.Value, ctx)
					if !ok {
						t.Fatalf("%s 必须能编码", direction.target)
					}
					// 与 forward.convertBody 同口径：解码侧与编码侧的损失合并成一份台账。
					imageEntries := []LossEntry{}
					for _, entry := range append(append([]LossEntry{}, decoded.Loss.Entries...), encoded.Loss.Entries...) {
						if entry.Capability == LossImage {
							imageEntries = append(imageEntries, entry)
						}
					}

					if want.action == "" {
						if len(imageEntries) != 0 {
							t.Fatalf("这一格不该有 image 条目，实际 %d 条：%+v", len(imageEntries), imageEntries)
						}
					} else {
						if len(imageEntries) != 1 {
							t.Fatalf("一张图只该记一条，实际 %d 条：%+v", len(imageEntries), imageEntries)
						}
						if imageEntries[0].Action != want.action {
							t.Fatalf("action 应为 %s（以最终结果为准），实际 %s", want.action, imageEntries[0].Action)
						}
						if want.detail != "" && imageEntries[0].Detail != want.detail {
							t.Fatalf("detail 应为 %q，实际 %q", want.detail, imageEntries[0].Detail)
						}
					}

					// 记账必须与「图到底送出去没有」一致，否则「只记一条」可以靠丢图换来。
					//
					// 判据是**按目标协议路径取到的 image 节点**，不是子串：子串只问字节在不在，不问挂在哪，
					// 图落进无效字段或被塞进别的块里都能绿。
					nodes := targetImages(direction.target, encoded.Body)
					marshaled := string(encoded.Body.MarshalCompact())
					if !want.delivered {
						if len(nodes) != 0 {
							t.Fatalf("目标 %s 不该收下这张图，却在 %s 取到 %d 个节点（正文 %s）",
								direction.target, nodes[0].path, len(nodes), marshaled)
						}
						return
					}
					if len(nodes) != 1 {
						t.Fatalf("目标 %s 应恰好有 1 个 image 节点，实际 %d 个（正文 %s）",
							direction.target, len(nodes), marshaled)
					}
					if wantValue := expectedTargetImageValue(direction.target, form); nodes[0].value != wantValue {
						t.Fatalf("%s 的承载值应为 %q，实际 %q（正文 %s）",
							nodes[0].path, wantValue, nodes[0].value, marshaled)
					}
					// anthropic 的承载位分 data / url 两态，媒体类型只在内联态存在：漏带类型即无法还原。
					if direction.target == ProtocolAnthropicMessages && nodes[0].mediaType != form.mediaType {
						t.Fatalf("%s 的 media_type 应为 %q，实际 %q（正文 %s）",
							nodes[0].path, form.mediaType, nodes[0].mediaType, marshaled)
					}
				})
			}
		}
	}
}

// targetImageNode 是按目标协议路径取到的一个 image 节点。
type targetImageNode struct {
	// path 是从正文根到承载值的路径（失败信息里用它指出图到底挂在哪儿）。
	path string
	// value 是承载值：chat / responses 线上的 URL 字符串，anthropic 的 source.data 或 source.url。
	value string
	// mediaType 是 anthropic 的 source.media_type（其余两线没有这一层，固定为空）。
	mediaType string
}

// targetImages 按**目标协议定义的 JSON 路径**取出正文里的 image 节点。
//
// 为何不能只查子串：子串只问字节在不在，不问挂在哪——图落进无效字段、或被塞到别的块里，
// 它都照样绿。这里逐层走到协议规定的位置，并把承载值与媒体类型一并取出。
func targetImages(proto WireProtocol, body *Value) []targetImageNode {
	container, nodeType := "messages", "image"
	switch proto {
	case ProtocolOpenAIChat:
		container, nodeType = "messages", "image_url"
	case ProtocolOpenAIResponses:
		container, nodeType = "input", "input_image"
	}
	nodes := []targetImageNode{}
	for messageIndex, message := range body.ArrayField(container) {
		for blockIndex, block := range message.ArrayField("content") {
			if kind, _ := block.StringField("type"); kind != nodeType {
				continue
			}
			base := fmt.Sprintf("%s[%d].content[%d]", container, messageIndex, blockIndex)
			switch proto {
			case ProtocolOpenAIChat:
				url, _ := block.ObjectField("image_url").StringField("url")
				nodes = append(nodes, targetImageNode{path: base + ".image_url.url", value: url})
			case ProtocolOpenAIResponses:
				url, _ := block.StringField("image_url")
				nodes = append(nodes, targetImageNode{path: base + ".image_url", value: url})
			default:
				source := block.ObjectField("source")
				mediaType, _ := source.StringField("media_type")
				if data, ok := source.StringField("data"); ok {
					nodes = append(nodes, targetImageNode{
						path: base + ".source.data", value: data, mediaType: mediaType})
					continue
				}
				url, _ := source.StringField("url")
				nodes = append(nodes, targetImageNode{
					path: base + ".source.url", value: url, mediaType: mediaType})
			}
		}
	}
	return nodes
}

// expectedTargetImageValue 由客户端语义（内联 / 远程）算出这幅图在目标线上应有的承载值。
func expectedTargetImageValue(proto WireProtocol, form mediaForm) string {
	if form.data == "" {
		// 远程 URL：三种目标线都原样承载这个 URL（chat 的 image_url 不需要媒体类型）。
		return form.url
	}
	if proto == ProtocolAnthropicMessages {
		// anthropic 的 source.type=base64 承载裸 base64，不合成 data URL。
		return form.data
	}
	return "data:" + form.mediaType + ";base64," + form.data
}

// clientFormatFor 把源协议映射成客户端形态（与 dataplane 的入站格式一致）。
func clientFormatFor(source WireProtocol) ClientFormat {
	switch source {
	case ProtocolAnthropicMessages:
		return FormatClaude
	case ProtocolOpenAIResponses:
		return FormatResponse
	default:
		return FormatOpenAI
	}
}

// TestChatImageLossDetailIsReadable 钉住「记损 detail 必须可判读」。
//
// anthropic 的 source.type=base64 允许省略 media_type，而 chat 线要合成 data URL 就必须知道
// 类型，故这一形态确实送不出去，如实记 dropped——但 detail 不得留成空类型的 `media_type:`，
// 须写明 unknown，否则事后无法分辨「客户端没给」与「我们漏写」。
func TestChatImageLossDetailIsReadable(t *testing.T) {
	body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"base64","data":"` + testImagePNGBase64 + `"}}]}]}`
	ctx := ConvertCtx{
		ClientFormat:   FormatClaude,
		TargetProto:    ProtocolOpenAIChat,
		Model:          "m",
		ToWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeRequest(ProtocolAnthropicMessages, mustParsePayload(t, body), ctx)
	if !ok {
		t.Fatal("anthropic 正文必须能解码")
	}
	encoded, ok := EncodeRequest(ProtocolOpenAIChat, decoded.Value, ctx)
	if !ok {
		t.Fatal("chat 目标必须能编码")
	}
	imageEntries := []LossEntry{}
	for _, entry := range append(append([]LossEntry{}, decoded.Loss.Entries...), encoded.Loss.Entries...) {
		if entry.Capability == LossImage {
			imageEntries = append(imageEntries, entry)
		}
	}
	if len(imageEntries) != 1 {
		t.Fatalf("一张图只该记一条，实际 %d 条：%+v", len(imageEntries), imageEntries)
	}
	if imageEntries[0].Action != LossDropped || imageEntries[0].Detail != "media_type:unknown" {
		t.Fatalf("应为 dropped / media_type:unknown，实际 %s / %q", imageEntries[0].Action, imageEntries[0].Detail)
	}
	if strings.Contains(string(encoded.Body.MarshalCompact()), testImagePNGBase64) {
		t.Fatal("没有媒体类型的内联图无法合成 data URL，不该出现在目标正文里")
	}
}

// imageChainBody 造一条只含一幅图的客户端正文。
func imageChainBody(source WireProtocol, role string, form mediaForm) string {
	switch source {
	case ProtocolOpenAIChat:
		return `{"model":"m","messages":[{"role":"` + role + `","content":[` +
			`{"type":"image_url","image_url":{"url":"` + form.wire + `"}}]}]}`
	case ProtocolOpenAIResponses:
		return `{"model":"m","input":[{"type":"message","role":"` + role + `","content":[` +
			`{"type":"input_image","image_url":"` + form.wire + `"}]}]}`
	default:
		return `{"model":"m","max_tokens":64,"messages":[{"role":"` + role + `","content":[` +
			`{"type":"image","source":` + form.sourceJSON + `}]}]}`
	}
}
