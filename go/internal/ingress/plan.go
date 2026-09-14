package ingress

import (
	"net/http"
	"strconv"
	"strings"
)

// Action 是请求体的处置方式。
type Action string

const (
	// ActionPass 表示原样透传（无编码、空体、或不支持的编码——交给上游处理）。
	ActionPass Action = "pass"
	// ActionDecode 表示需要按 DecodeOrder 逐层解压。
	ActionDecode Action = "decode"
)

// Plan 是读体前的准入结论。
type Plan struct {
	Action Action
	// Reason 仅在 ActionPass 时有值：no-encoding | empty | unsupported。
	Reason string
	// Unsupported 列出不支持的编码 token（透传时非空），供调用方告警。
	Unsupported []string
	// DecodeOrder 是按 HTTP 语义**反向**排列的解码顺序（如 "br, gzip" 输入 → gzip, br）。
	DecodeOrder []string
}

// supportedEncodings 与 Node 侧 SUPPORTED_ENCODINGS 一致；小写，identity 在解析时已被剔除。
var supportedEncodings = map[string]bool{
	"zstd":    true,
	"gzip":    true,
	"x-gzip":  true,
	"deflate": true,
	"br":      true,
}

// ParseContentEncoding 把 content-encoding 头解析为编码 token 列表：小写、去空白、剔除
// 空 token 与 identity。HTTP 语义为「按列出顺序逐层应用」，因此解码需反向进行（见 Plan）。
func ParseContentEncoding(header string) []string {
	if header == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		token := strings.ToLower(strings.TrimSpace(part))
		if token == "" || token == "identity" {
			continue
		}
		out = append(out, token)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// PlanRequestBody 是读体前/读体后共用的唯一一份准入判定，避免两处漂移。
//
// compressedSize 是压缩体字节数（-1 表示未知，例如没有 content-length 的分块请求）。判定顺序与
// Node 侧 planRequestBody 逐项一致：无编码 → 空体 → 层数 → 支持集 → 压缩体体积 → 解码。
// 返回的 error 为 ErrTooManyLayers（400）或 ErrCompressedTooLarge（413）。
func PlanRequestBody(compressedSize int64, contentEncoding string, opts Options) (Plan, error) {
	o := opts.normalized()
	encodings := ParseContentEncoding(contentEncoding)
	if len(encodings) == 0 {
		return Plan{Action: ActionPass, Reason: "no-encoding"}, nil
	}

	// 空体不可能是有效压缩流：在层数与支持集校验之前直接透传，避免对安全的空请求误报 400。
	if compressedSize == 0 {
		return Plan{Action: ActionPass, Reason: "empty"}, nil
	}

	if len(encodings) > o.MaxLayers {
		return Plan{}, wrap(
			ErrTooManyLayers,
			"content-encoding 有 %d 层，最多允许 %d 层",
			len(encodings),
			o.MaxLayers,
		)
	}

	unsupported := make([]string, 0, len(encodings))
	for _, enc := range encodings {
		if !supportedEncodings[enc] {
			unsupported = append(unsupported, enc)
		}
	}
	if len(unsupported) > 0 {
		// 透传：不解压、保留原始字节与 content-encoding 头，交给上游处理。
		return Plan{Action: ActionPass, Reason: "unsupported", Unsupported: unsupported}, nil
	}

	// 解压前先按压缩体本身字节数拒绝过大输入。仅对「支持的单层编码」生效。
	if compressedSize > o.MaxCompressedBytes {
		return Plan{}, wrap(
			ErrCompressedTooLarge,
			"压缩请求体 %d 字节超过上限 %d 字节",
			compressedSize,
			o.MaxCompressedBytes,
		)
	}

	order := make([]string, 0, len(encodings))
	for i := len(encodings) - 1; i >= 0; i-- {
		order = append(order, encodings[i])
	}
	return Plan{Action: ActionDecode, DecodeOrder: order}, nil
}

// PeekSize 在**不读请求体**的前提下，仅按 content-length 与 content-encoding 头给出结构与体积判定。
//
// 返回：
//   - compressed：压缩体声明的字节数；头里没有 content-length（分块或未知）时为 -1；
//   - decodedHint：解压后字节数的**已知下界**。无编码时等于 compressed（可精确预分配）；
//     有编码时为 -1——压缩比不可预知，不做猜测，避免按猜测量预分配造成放大。
//
// 命中拒绝条件时返回 ErrTooManyLayers（400）或 ErrCompressedTooLarge（413）；不支持的编码
// 不报错（透传语义）。判定与 PlanRequestBody 完全一致，只是入参来自头部。
func PeekSize(header http.Header, opts Options) (compressed int64, decodedHint int64, err error) {
	compressed = -1
	if header != nil {
		if raw := strings.TrimSpace(header.Get("Content-Length")); raw != "" {
			if n, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil && n >= 0 {
				compressed = n
			}
		}
	}

	contentEncoding := ""
	if header != nil {
		contentEncoding = header.Get("Content-Encoding")
	}

	// 体积未知时以 0 参与判定：只做层数与支持集判定，不做体积拒绝（体积由流式计数兜底）。
	sizeForPlan := compressed
	if sizeForPlan < 0 {
		sizeForPlan = 0
	}
	plan, err := PlanRequestBody(sizeForPlan, contentEncoding, opts)
	if err != nil {
		return compressed, -1, err
	}
	if plan.Action == ActionPass && compressed >= 0 {
		// 无编码时解压后字节数与压缩体相同；不支持的编码会被原样透传，同样如此。
		return compressed, compressed, nil
	}
	return compressed, -1, nil
}
