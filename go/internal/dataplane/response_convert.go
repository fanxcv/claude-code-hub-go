package dataplane

import (
	"net/http"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// 响应侧方言回译：上游线 → 客户端线。
//
// 为什么必须有这一层：转换生效时上游回答的是**目标线**方言，而客户端按自己的方言解析。
// 不回译就等于把 Responses 的正文塞给 Anthropic 客户端，客户端必然解析失败——这比内存与
// 性能更硬，是「能否切换」的前置条件（对应 Node 的 protocol-response-converter.ts）。
//
// 与请求侧严格成对：请求侧把客户端方言编码为目标线，本层把上游的目标线解回客户端方言。
//
// 四条纪律（与 Node 同构）：
//  1. 计划未施加转换（原生直通）时零开销直接返回，不引入任何行为变化；
//  2. 转换失败一律兜底为「原样透传」并留痕，绝不把一条正常响应打挂；
//  3. 非 2xx 不转换：错误体保持上游原样，错误整形不是本层职责；
//  4. 流式只包一层**逐帧**转换器，绝不整流缓冲（TTFT 与单流驻留都依赖这一点）。
type responseConversion struct {
	upstream convert.WireProtocol
	client   convert.WireProtocol
	ctx      convert.ConvertCtx
}

// newResponseConversion 按本次尝试的计划建响应侧转换器；不该转换时返回 nil。
//
// placeholderThinkingSignature 是占位思考签名开关（CCH_THINKING_SIGNATURE_PLACEHOLDER，
// 默认开）：思考来自 chat/responses 线上游时，Anthropic 客户端拿到的块本就无签名，
// 开了它客户端才能显示思考（见 convert/thinking_placeholder.go）。
func newResponseConversion(
	plan *forward.Plan,
	format convert.ClientFormat,
	model string,
	stream bool,
	placeholderThinkingSignature bool,
) *responseConversion {
	if plan == nil || plan.Conversion == nil {
		return nil
	}
	return &responseConversion{
		upstream: plan.Conversion.TargetProtocol,
		client:   plan.Conversion.ClientProtocol,
		ctx: convert.ConvertCtx{
			ClientFormat:                 format,
			TargetProto:                  plan.Conversion.TargetProtocol,
			Model:                        model,
			Stream:                       stream,
			FromWireToolName:             toolNameRestoreHook(plan.ToolNameRestore),
			PlaceholderThinkingSignature: placeholderThinkingSignature,
		},
	}
}

// applyNonStream 把非流式正文回译成客户端方言。
//
// 返回 false 表示保持上游正文原样（含转换失败与形状不可解两种情况）；调用方据此决定
// 是否重写 content-type 与 content-length。
func (c *responseConversion) applyNonStream(body []byte) ([]byte, bool) {
	if c == nil || len(body) == 0 {
		return body, false
	}
	value, err := convert.ParseJSON(body)
	if err != nil || value == nil || !value.IsObject() {
		// 合法 JSON 但非对象（数组/标量）或非法 JSON：无形状可解，原样透传。
		return body, false
	}
	decoded, ok := convert.DecodeResponse(c.upstream, value, c.ctx)
	if !ok {
		return body, false
	}
	encoded, ok := convert.EncodeResponse(c.client, decoded.Value, c.ctx)
	if !ok || encoded.Body == nil {
		return body, false
	}
	return []byte(encoded.Body.MarshalCompact()), true
}

// newStreamPipe 建逐帧回译管道；本层缺任一端编解码器时不转换（不猜、不降级）。
func (c *responseConversion) newStreamPipe() (*convert.StreamPipe, bool) {
	if c == nil {
		return nil, false
	}
	return convert.NewStreamPipe(c.upstream, c.client, c.ctx)
}

// hasOpaqueContentEncoding 判定正文是否仍处于压缩态。
//
// 压缩字节喂给编解码器只会产出垃圾，故非 identity 一律跳过转换、原样透传。数据面按
// dial.ForceIdentityEncoding 要求上游不压缩，这里是防御性判定。
func hasOpaqueContentEncoding(header http.Header) bool {
	encoding := strings.ToLower(strings.TrimSpace(header.Get("Content-Encoding")))
	return encoding != "" && encoding != "identity"
}

// toolNameRestoreHook 把逆转表包成解码器用的钩子；空表返回 nil（解码器原样放行）。
func toolNameRestoreHook(table map[string]string) func(string) string {
	if len(table) == 0 {
		return nil
	}
	return func(name string) string {
		if original, ok := table[name]; ok {
			return original
		}
		return name
	}
}
