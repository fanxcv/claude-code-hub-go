package convert

// WireProtocol 是协议线标识，对应 TS 侧 WireProtocol。
type WireProtocol string

const (
	ProtocolAnthropicMessages WireProtocol = "anthropic-messages"
	ProtocolOpenAIChat        WireProtocol = "openai-chat"
	ProtocolOpenAIResponses   WireProtocol = "openai-responses"
	// ProtocolGemini 是 Gemini 线的**观测标签**，不是转换矩阵里的一条线。
	//
	// 金标语料 tests/load/protocol-conformance/corpus/selection.json 铉死：gemini 的
	// targetProtocol 恒为 null，兼容性只能是 native / incompatible。因此：
	//   - ProtocolOfProviderType / ProtocolOfClientFormat **不返回它**（它们仍需对齐 Node 的 null）；
	//   - 它**不注册 codec**（注册会让 CanConvert 把跨线组合误判为可转，违反语料）；
	//   - 它仅用于标记「本次正文属于 Gemini 线」，让只读的响应解码（取用量/模型）与
	//     日志留痕有名字可用，见 codec_gemini.go 的说明。
	ProtocolGemini WireProtocol = "gemini"
)

// ClientFormat 是客户端入站格式，对应 TS 侧 ClientFormat。
type ClientFormat string

const (
	FormatClaude    ClientFormat = "claude"
	FormatOpenAI    ClientFormat = "openai"
	FormatResponse  ClientFormat = "response"
	FormatGemini    ClientFormat = "gemini"
	FormatGeminiCLI ClientFormat = "gemini-cli"
)

// ProviderType 是供应商类型，对应 TS 侧 ProviderType。
type ProviderType string

const (
	ProviderClaude           ProviderType = "claude"
	ProviderClaudeAuth       ProviderType = "claude-auth"
	ProviderCodex            ProviderType = "codex"
	ProviderOpenAICompatible ProviderType = "openai-compatible"
	ProviderGemini           ProviderType = "gemini"
	ProviderGeminiCLI        ProviderType = "gemini-cli"
)

// ProtocolCompat 是选路三态。
type ProtocolCompat string

const (
	CompatNative       ProtocolCompat = "native"
	CompatConvertible  ProtocolCompat = "convertible"
	CompatIncompatible ProtocolCompat = "incompatible"
)

// ConversionPlan 是一次上游尝试的转换计划。
type ConversionPlan struct {
	ClientProtocol WireProtocol
	TargetProtocol WireProtocol
}

// codecRegistry 记录已注册编解码器的协议线。
// 与 TS 侧 index.ts 的模块级 Map 等价：注册发生在 init()，进程内不变。
var codecRegistry = map[WireProtocol]bool{}

// registerCodec 注册一条协议线；同一协议重复注册为幂等。
func registerCodec(protocol WireProtocol) {
	codecRegistry[protocol] = true
}

// HasCodec 报告该协议线是否已注册编解码器。
func HasCodec(protocol WireProtocol) bool { return codecRegistry[protocol] }

// RegisteredProtocols 列出已注册协议线（顺序不稳定，仅用于诊断与测试）。
func RegisteredProtocols() []WireProtocol {
	out := make([]WireProtocol, 0, len(codecRegistry))
	for protocol := range codecRegistry {
		out = append(out, protocol)
	}
	return out
}

// ProtocolOfProviderType 把供应商类型映射到上游协议线。
// gemini / gemini-cli 不在本期范围，返回 (零值, false)。
func ProtocolOfProviderType(providerType ProviderType) (WireProtocol, bool) {
	switch providerType {
	case ProviderClaude, ProviderClaudeAuth:
		return ProtocolAnthropicMessages, true
	case ProviderCodex:
		return ProtocolOpenAIResponses, true
	case ProviderOpenAICompatible:
		return ProtocolOpenAIChat, true
	default:
		return "", false
	}
}

// ProtocolOfClientFormat 把客户端格式映射到协议线。
// gemini / gemini-cli 不在本期范围，返回 (零值, false)。
func ProtocolOfClientFormat(clientFormat ClientFormat) (WireProtocol, bool) {
	switch clientFormat {
	case FormatClaude:
		return ProtocolAnthropicMessages, true
	case FormatResponse:
		return ProtocolOpenAIResponses, true
	case FormatOpenAI:
		return ProtocolOpenAIChat, true
	default:
		return "", false
	}
}

// IsNativePair 判断客户端格式与供应商类型是否天然同协议（无需转换）。
// 未知格式返回 true（不主动过滤），与 TS 侧 isNativePair 的 default 分支一致。
func IsNativePair(clientFormat ClientFormat, providerType ProviderType) bool {
	switch clientFormat {
	case FormatClaude:
		return providerType == ProviderClaude || providerType == ProviderClaudeAuth
	case FormatResponse:
		return providerType == ProviderCodex
	case FormatOpenAI:
		return providerType == ProviderOpenAICompatible
	case FormatGemini:
		return providerType == ProviderGemini
	case FormatGeminiCLI:
		return providerType == ProviderGeminiCLI
	default:
		return true
	}
}

// ResolveTargetProtocol 解析目标上游协议线。
// 同协议对返回供应商协议线；跨协议仅在目标协议已注册 codec 时返回，否则 (零值, false)。
func ResolveTargetProtocol(
	clientFormat ClientFormat,
	providerType ProviderType,
) (WireProtocol, bool) {
	providerProtocol, ok := ProtocolOfProviderType(providerType)
	if IsNativePair(clientFormat, providerType) {
		return providerProtocol, ok
	}
	if !ok {
		return "", false
	}
	if !HasCodec(providerProtocol) {
		return "", false
	}
	return providerProtocol, true
}

// CanConvert 报告「跨协议且两侧 codec 齐备」。
func CanConvert(clientFormat ClientFormat, providerType ProviderType) bool {
	if IsNativePair(clientFormat, providerType) {
		return false
	}
	source, sourceOK := ProtocolOfClientFormat(clientFormat)
	target, targetOK := ProtocolOfProviderType(providerType)
	return sourceOK && targetOK && HasCodec(source) && HasCodec(target)
}

// ResolveProtocolCompat 是选路三态判定，判定顺序固定：
// 1. 同协议 -> native；2. 任一端不在三线矩阵内 -> incompatible；
// 3. 未开启转换 -> incompatible；4. 两侧 codec 齐备 -> convertible；5. 其余 -> incompatible。
func ResolveProtocolCompat(
	clientFormat ClientFormat,
	providerType ProviderType,
	conversionEnabled bool,
) ProtocolCompat {
	if IsNativePair(clientFormat, providerType) {
		return CompatNative
	}
	target, targetOK := ProtocolOfProviderType(providerType)
	source, sourceOK := ProtocolOfClientFormat(clientFormat)
	if !targetOK || !sourceOK {
		return CompatIncompatible
	}
	if !conversionEnabled {
		return CompatIncompatible
	}
	if HasCodec(source) && HasCodec(target) {
		return CompatConvertible
	}
	return CompatIncompatible
}

// PlanConversion 是请求侧与响应侧共用的唯一转换判定点：返回 nil 表示本次尝试不转换。
func PlanConversion(
	clientFormat ClientFormat,
	providerType ProviderType,
	conversionEnabled bool,
) *ConversionPlan {
	if ResolveProtocolCompat(clientFormat, providerType, conversionEnabled) != CompatConvertible {
		return nil
	}
	clientProtocol, clientOK := ProtocolOfClientFormat(clientFormat)
	targetProtocol, targetOK := ProtocolOfProviderType(providerType)
	if !clientOK || !targetOK {
		return nil
	}
	return &ConversionPlan{ClientProtocol: clientProtocol, TargetProtocol: targetProtocol}
}
