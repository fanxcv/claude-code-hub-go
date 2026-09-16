package convert

import (
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
	// needle 是目标正文里「这张图还在」的判据（两种承载形态里都出现的那段）。
	needle string
}

var imageMediaForms = []mediaForm{
	{
		name:          "PNG data URL",
		wire:          "data:image/png;base64," + testImagePNGBase64,
		sourceJSON:    `{"type":"base64","media_type":"image/png","data":"` + testImagePNGBase64 + `"}`,
		dataURLClient: true,
		needle:        testImagePNGBase64,
	},
	{
		name:          "GIF data URL",
		wire:          "data:image/gif;base64," + testImageGIFBase64,
		sourceJSON:    `{"type":"base64","media_type":"image/gif","data":"` + testImageGIFBase64 + `"}`,
		dataURLClient: true,
		needle:        testImageGIFBase64,
	},
	{
		name:       "远程 URL",
		wire:       testImageRemoteURL,
		sourceJSON: `{"type":"url","url":"` + testImageRemoteURL + `"}`,
		needle:     testImageRemoteURL,
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
					// 此处钉的是**当前行为**，而它是一处已上报、本次未修的缺陷：
					// responses 线解码出的 URL 图不带媒体类型（Block.MediaType 为空），
					// chat 编码器的 media-type 闸门把「类型未知」当成「不是图片」而整幅丢弃。
					// 记损本身是真的（detail 里类型为空串），但图没能送出去——传法与证据见报告。
					// 矩阵不替它背书：一旦闸门改成只对携带 data 的块生效（或按 URL 猜类型），
					// 这四格会转红并提醒改期望。
					return chainOutcome{action: LossDropped, detail: "media_type:"}
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
					// 与 responses→chat 同一缺陷同一根因：anthropic 的 `source.type=url` 也不带媒体类型。
					return chainOutcome{action: LossDropped, detail: "media_type:"}
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
					marshaled := string(encoded.Body.MarshalCompact())
					if got := strings.Contains(marshaled, form.needle); got != want.delivered {
						t.Fatalf("目标正文里这张图的存在性 = %v，期望 %v（正文 %s）", got, want.delivered, marshaled)
					}
				})
			}
		}
	}
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
