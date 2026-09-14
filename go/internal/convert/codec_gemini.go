package convert

// Gemini 线的**观测解码**。它刻意只做「读」，不做「写」。
//
// 为什么 gemini 不注册 codec：金标语料 `tests/load/protocol-conformance/corpus/selection.json`
// 钉死了它的选择语义——gemini 两端组合（gemini 客户端 + gemini 供应商、gemini-cli 两端）
// 的 `targetProtocol` 恒为 null、兼容性只能是 native / incompatible，**永不 convertible**；
// 跨组合一律 incompatible。一旦 `registerCodec(ProtocolGemini)`，`CanConvert` /
// `ResolveProtocolCompat` 就会把「claude 客户端 + gemini 供应商」判成可转，
// 于是供应商进入候选、再在 `PlanConversion` 处失败——直接违反语料，也违反 Node 的
// `resolveProtocolCompat`（它同时查客户端线，见 src/app/v1/_lib/protocol-convert/index.ts:134-150）。
//
// 那为什么还要解码：Node 对 gemini 响应照样读用量与上游模型名（数据面记账与
// `message_request` 的「上游实际模型」都靠它）：
//   - 用量：`usageMetadata.{promptTokenCount, candidatesTokenCount, cachedContentTokenCount}`
//     （`src/app/v1/_lib/proxy/response-handler.ts:6045-6068`；input 必须减去 cached 以免重复计费）；
//   - 模型：`modelVersion` / `model_version`（`src/app/v1/_lib/proxy/actual-response-model.ts:145-146`）。
// 故 `DecodeResponse` 认识这条线（供 forward 的非流式事实提取复用），
// 而 `DecodeRequest` / `EncodeRequest` / `EncodeResponse` **保持不认识**（对它们返回 false）。

// decodeGeminiResponse 从 Gemini 响应里提取可观测事实（用量与模型名）。
//
// 只填 usage 与 model：gemini 不参与协议转换，故 Blocks / Passthrough / StopReason 对其
// 消费方（非流式记账与「上游实际模型」）无意义，留空比造假数据更诚实。
func decodeGeminiResponse(body *Value, ctx ConvertCtx) DecodeResult[*Response] {
	loss := &LossCollector{}
	source := body
	if !isRecord(source) {
		source = NewObject()
	}

	response := &Response{}

	if model, ok := stringField(source, "modelVersion"); ok && model != "" {
		response.Model = model
	} else if model, ok := stringField(source, "model_version"); ok && model != "" {
		response.Model = model
	} else {
		response.Model = ctx.Model
	}

	if usageValue := fieldOrNil(source, "usageMetadata"); usageValue != nil && !usageValue.IsNull() {
		if usage := usageFromGemini(usageValue); !usage.IsEmpty() {
			response.Usage = usage
		}
	}

	return DecodeResult[*Response]{Value: response, Loss: loss.Report()}
}
