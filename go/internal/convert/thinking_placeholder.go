package convert

import (
	"encoding/base64"
	"sync"
)

// 本文件造「占位 thinking signature」：思考来自 chat/responses 线上游（没有 Anthropic 签名）
// 而客户端说 Anthropic 协议时，补一个**合法 wire 形状**的签名，让客户端愿意显示这段思考。
//
// 为什么必须是合法形状而不是随便一串 base64：Anthropic 的 signature 是不透明 protobuf，
// 读它的一侧按固定字段路径解（参照实现见 CLIProxyAPI 的
// internal/signature/claude_validation.go；字段路径 [2][1][1] channel_id、[2][1][6] model_text）。
// 造出的占位要能被这套解析走通，否则下游按签名取模型名的工具会解出垃圾。
//
// 为什么模型名要显式写成占位值：占位不是真实签名，识别方（含我们自己，见
// rectify.StripPlaceholderSignature）要一眼认得出。真实 Anthropic 上游会校验签名，占位只能
// 发给客户端，回传上游必被 400——故该值与剥离逻辑共用同一处常量。
//
// 为什么不留「伪造真签名」的余地：Node 时代的 signature 整流器是「删」不是「补」
// （thinking-signature-rectifier.ts），为的是不让不兼容签名触发上游 400。本能力是显式新增的
// 客户端显示能力，受 CCH_THINKING_SIGNATURE_PLACEHOLDER 门控，绝不改上游已带的真实签名。
const placeholderThinkingModelText = "placeholder"

var (
	placeholderSignatureOnce  sync.Once
	placeholderSignatureValue string
)

// PlaceholderThinkingSignature 返回占位签名（进程内只造一次）。
//
// 值一旦生成即不可变：回程剥离按字符串相等判定，两侧必须同值。
func PlaceholderThinkingSignature() string {
	placeholderSignatureOnce.Do(func() {
		placeholderSignatureValue = buildPlaceholderThinkingSignature()
	})
	return placeholderSignatureValue
}

// IsPlaceholderThinkingSignature 判定签名是否为本进程造出的占位。
//
// 只认全等：真实签名与我们造出的这段 base64 撞车的概率可忽略，而放宽匹配（前缀/长度）
// 会让真实签名被误剥。
func IsPlaceholderThinkingSignature(signature string) bool {
	return signature != "" && signature == PlaceholderThinkingSignature()
}

// buildPlaceholderThinkingSignature 按实签名的 wire 形状造占位：
//
//	payload      = field 2 (bytes) = container
//	container    = field 1 (bytes) = channelBlock
//	channelBlock = field 1 (varint) channel_id=11 + field 6 (bytes) model_text
//
// 单层（E 形）即可：payload 首字节 0x12，其 base64 自然以 'E' 开头——这不是额外信封，
// 而是 0x12 的高 6 位恰为 4 的必然结果，故无需再叠一层。
func buildPlaceholderThinkingSignature() string {
	// 字段 1 varint：tag 0x08；channel_id 取 11（实签名里出现的路由类之一）。
	channelBlock := []byte{0x08, 0x0B}
	// 字段 6 bytes：tag 0x32（(6<<3)|2）+ 长度 + model_text。
	channelBlock = append(channelBlock, 0x32, byte(len(placeholderThinkingModelText)))
	channelBlock = append(channelBlock, placeholderThinkingModelText...)

	container := append([]byte{0x0A, byte(len(channelBlock))}, channelBlock...)
	payload := append([]byte{0x12, byte(len(container))}, container...)
	return base64.StdEncoding.EncodeToString(payload)
}
