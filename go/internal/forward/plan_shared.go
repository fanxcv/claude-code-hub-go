package forward

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件承载「单次请求内与候选无关的正文转换产物」的复用：客户端正文 → 解析树 → 枢纽请求
// → 目标协议正文。竞速的多个 attempt 共用同一份产物，而不是各自动一遍。
//
// 为什么可以跨候选复用（逐阶段核过 BuildPlan 的正文段，见 plan.go「4) 正文」）：
//   - 解析、状态型字段判定、解码只读**客户端**正文与客户端协议线；
//   - 编码只读枢纽请求、**目标**协议线与最终模型名。
//
// 供应商、端点、参数覆写都在编码**之后**施加，且都是「无改写则原样返回入参」，
// 故它们仍逐 attempt 各算各的，不会拿到别的候选的正文。
//
// 两级复用，键各不相同：
//
//   - 解析与解码的键是「客户端正文快照」——它不含模型重定向，故跨候选、跨目标协议线都只算一次；
//   - 编码的键是「目标协议线 + 最终模型名」。协议线不可省：不同线各得一份产物（绝不把一种
//     协议的正文发到另一种协议的上游）。模型名也不可省：模型重定向是**供应商级**的
//     （Provider.ModelRedirects），同一场竞速里两个同协议候选可以指向不同的目标模型，
//     少了它就会把甲的模型名写进发给乙的正文——上游要么 404，要么把甲的用量记到乙账上。
//
// 模型名怎么进正文：编码前浅拷贝一次枢纽请求（`*Request` 结构体，切片与 map 仍指向原物），
// 只换它的 Model 字段。正文按目标协议现编一份，**不**在编码后整份复制正文去改 model。
//
// 生命周期：由调用方（ForwardStreamHedge）按一次请求新建一份，随竞速结束失去引用；不得跨
// 请求复用（供应商配置随时可变，跨请求缓存要处理失效，属另一议题）。

// decodeProduct 是「客户端正文 → 枢纽请求」的产物：与目标协议线无关。
type decodeProduct struct {
	request *convert.Request
	loss    *convert.LossReport
	restore map[string]string
	stream  bool
	// failure 非 nil 表示解码这一环失败了（非法 JSON / 无解码器），调用方按 Node 语义回退直通。
	failure error
	// rejected 非 nil 表示目标线没有状态型字段的承载位：候选级不可服务，调用方换候选。
	rejected *StatefulConversionError
}

// conversionProduct 是「客户端正文 → 目标协议正文」的产物：解码损失与编码损失已合并。
type conversionProduct struct {
	body     []byte
	loss     *convert.LossReport
	restore  map[string]string
	failure  error
	rejected *StatefulConversionError
}

// encodeKey 是编码产物的键：同一目标协议线 + 同一最终模型名才可复用（理由见文件头）。
//
// redirectTarget 为空串表示「无重定向，模型名取 client.Model」——空串是安全的哨兵：
// 规则解析在 target 为空时就跳过该条（见 parseModelRedirectRules），故生效的重定向必有目标名。
type encodeKey struct {
	protocol convert.WireProtocol
	model    string
}

// PlanShared 复用单次请求内与候选无关的转换产物；nil 指针即不共享（每次现算）。
type PlanShared struct {
	mu      sync.Mutex
	source  []byte // 产物对应的客户端正文快照
	decoded *decodeProduct
	encoded map[encodeKey]conversionProduct
}

// NewPlanShared 构造一份请求作用域的共享产物表，由竞速持有并交给每一路 attempt。
func NewPlanShared() *PlanShared { return &PlanShared{} }

// drop 释放缓存的产物：竞速终局调用，让仍在途的 attempt 无法再借 race 把整场竞速的正文拖着。
// 已经取走产物的 plan 不受影响（它自己持有那份切片）。
func (s *PlanShared) drop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.source, s.decoded, s.encoded = nil, nil, nil
	s.mu.Unlock()
}

// conversion 返回本次候选的转换产物：可复用即复用，否则现算并记下（见 plan.go 的转换分支）。
//
// redirectTarget 是该候选的模型重定向目标名（无重定向传空串），只参与编码那一段的键与正文。
//
// 锁覆盖整段计算：并发 attempt 需要同一份产物时只算一次，后到者等前者的结果——那正是共享的
// 目的（重复算一遍等于什么都没省）。竞速的候选是错峰启动的，故这里几乎不会真的排队。
// 副作用是好的：同一份枢纽请求的编码也被串行化，共享的结构不会同时被两路 encode 读/写。
// （编码器本身只读入参，钉子在 convert 的 TestEncodeRequestHasNoHiddenState。）
func (s *PlanShared) conversion(
	client ClientRequest,
	conversion *convert.ConversionPlan,
	redirectTarget string,
) conversionProduct {
	if s == nil {
		return computeConversion(client, conversion, redirectTarget)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 正文换了即整份作废：串行路径的整流器会在重试之间改写正文快照，复用整流前的产物等于
	// 把整流前的正文发上游。其余客户端事实（方言、模型、路径）在同一场竞速内恒定，由调用方
	// 保证——PlanShared 只服务一次请求。
	if !bytes.Equal(s.source, client.Body) {
		s.source, s.decoded, s.encoded = client.Body, nil, nil
	}
	if s.decoded == nil {
		s.decoded = parseClientValue(client, conversion)
	}
	if s.decoded.failure != nil || s.decoded.rejected != nil {
		return conversionProduct{failure: s.decoded.failure, rejected: s.decoded.rejected}
	}
	key := encodeKey{protocol: conversion.TargetProtocol, model: redirectTarget}
	if product, ok := s.encoded[key]; ok {
		return product
	}
	product := encodeConvertedBody(client, conversion, s.decoded, redirectTarget)
	if s.encoded == nil {
		s.encoded = make(map[encodeKey]conversionProduct, 1)
	}
	s.encoded[key] = product
	return product
}

// computeConversion 现算一份转换产物（不共享）。
func computeConversion(
	client ClientRequest,
	conversion *convert.ConversionPlan,
	redirectTarget string,
) conversionProduct {
	decoded := parseClientValue(client, conversion)
	if decoded.failure != nil || decoded.rejected != nil {
		return conversionProduct{failure: decoded.failure, rejected: decoded.rejected}
	}
	return encodeConvertedBody(client, conversion, decoded, redirectTarget)
}

// parseClientValue 解析客户端正文、判定状态型字段冲突并解码为枢纽请求。
func parseClientValue(client ClientRequest, conversion *convert.ConversionPlan) *decodeProduct {
	value, err := convert.ParseJSON(client.Body)
	if err != nil {
		// 与原先转换函数内部的 fmt.Errorf("%w: %v", ErrInvalidBody, err) 文案逐字相同。
		return &decodeProduct{failure: fmt.Errorf("%w: %v", ErrInvalidBody, err)}
	}
	if rejected := rejectUnservableStateful(client, conversion, value); rejected != nil {
		return &decodeProduct{rejected: rejected}
	}
	return decodeClientValue(client, conversion, value)
}

// decodeClientValue 把已解析的客户端正文解码为枢纽请求。
//
// ctx 刻意不含目标协议线：解码器不读它（convert 的三个 decodeXxxRequest 只读 Model 与
// FromWireToolName），产物与目标协议解耦这件事因此是类型层面可见的，而不是靠注释约定。
func decodeClientValue(
	client ClientRequest,
	conversion *convert.ConversionPlan,
	value *convert.Value,
) *decodeProduct {
	ctx := convert.ConvertCtx{
		ClientFormat:              clientFormatOfProtocol(conversion.ClientProtocol),
		Model:                     client.Model,
		ToWireToolName:            convert.NormalizeToolName,
		GatewayInjectedBodyFields: client.GatewayInjectedBodyFields,
	}
	decoded, ok := convert.DecodeRequest(conversion.ClientProtocol, value, ctx)
	if !ok {
		return &decodeProduct{failure: fmt.Errorf(
			"%w: 客户端协议 %s 无解码器", ErrUnsupportedProviderType, conversion.ClientProtocol,
		)}
	}
	loss := decoded.Loss
	// 逆转表取自被解码的客户端请求：编码器写进上游的是规范化名，响应侧要还原回原名。
	restore := convert.BuildToolNameRestoreMap(toolNamesOf(decoded.Value))
	if len(restore) == 0 {
		restore = nil
	}
	return &decodeProduct{
		request: decoded.Value,
		loss:    &loss,
		restore: restore,
		stream:  ClientStreamRequestedFromValue(client.Path, client.Query, value),
	}
}

// encodeConvertedBody 把枢纽请求编码成目标协议正文。
//
// 模型名在这里就位，**不是**编码后再由 rewriteModelField 改写已转换正文：那会在转换路上凭空
// 多出一份与正文等长的拷贝（见 rewriteModelField 的注释），而这棵枢纽请求就在手边。
//
// redirectTarget 非空时浅拷贝一次枢纽请求、只换 Model：三个编码器取顶层 model 的次序不同——
// anthropic / responses 是 firstString(request.Model, ctx.Model)（codec_anthropic.go:715 /
// codec_responses.go:857），chat 反序 firstString(ctx.Model, request.Model)（codec_chat.go:1016）。
// 树与 ctx 两处都要写，只改一处必剩一条线发出客户端别名。浅拷贝只复制结构体（切片与 map 仍
// 指向原物），不复制任何正文，故仍是一份正文。
//
// 编码器只读入参（钉子在 convert 的 TestEncodeRequestHasNoHiddenState），故同一份枢纽请求
// 可以安全地按目标协议各编一次、并跨 goroutine 复用。
func encodeConvertedBody(
	client ClientRequest,
	conversion *convert.ConversionPlan,
	decoded *decodeProduct,
	redirectTarget string,
) conversionProduct {
	model := client.Model
	request := decoded.request
	if redirectTarget != "" {
		model = redirectTarget
		redirected := *request
		redirected.Model = model
		request = &redirected
	}
	ctx := convert.ConvertCtx{
		ClientFormat:              clientFormatOfProtocol(conversion.ClientProtocol),
		TargetProto:               conversion.TargetProtocol,
		Model:                     model,
		Stream:                    decoded.stream,
		ToWireToolName:            convert.NormalizeToolName,
		GatewayInjectedBodyFields: client.GatewayInjectedBodyFields,
	}
	encoded, ok := convert.EncodeRequest(conversion.TargetProtocol, request, ctx)
	if !ok || encoded.Body == nil {
		return conversionProduct{failure: fmt.Errorf(
			"%w: 目标协议 %s 无编码器", ErrUnsupportedProviderType, conversion.TargetProtocol,
		)}
	}
	return conversionProduct{
		body:    []byte(encoded.Body.MarshalCompact()),
		loss:    mergeLoss(decoded.loss, encoded.Loss),
		restore: decoded.restore,
	}
}

// mergeLoss 合并解码与编码的损失。
//
// 必须建新切片：解码损失在竞速内被多个 attempt 共享，往它上面 append 会写进共享的底层数组。
func mergeLoss(decoded *convert.LossReport, encoded convert.LossReport) *convert.LossReport {
	entries := make([]convert.LossEntry, 0, len(decoded.Entries)+len(encoded.Entries))
	entries = append(entries, decoded.Entries...)
	entries = append(entries, encoded.Entries...)
	return &convert.LossReport{Entries: entries}
}

// toolNamesOf 取枢纽请求里声明的工具名（响应侧逆转表的来源）。
func toolNamesOf(request *convert.Request) []string {
	if request == nil {
		return nil
	}
	names := make([]string, 0, len(request.Tools))
	for _, tool := range request.Tools {
		names = append(names, tool.Name)
	}
	return names
}
