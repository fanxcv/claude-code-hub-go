package convert

// 本文件承载两类「请求侧高层字段在跨线转换时无承载位」的处置：
//
//	1. 状态型字段（previous_response_id / conversation / store:true）——丢失的不是约束而是
//	   上下文或落库语义，故由调用方在**转换真的发生时** fail-closed（见 forward.BuildPlan）；
//	2. 约束型字段（prompt_cache_key / response_format / text / store）——丢失后可降级继续，
//	   但必须记损，否则用户看到的是「约束静默失效」。
//
// 两者都只对**外线**字段生效：各编码器只读本线 passthrough（Node 亦然），而 request 侧的
// passthrough 全仓没有任何消费者（`passthroughFor` 的调用点都取目标线），所以外线字段在
// 跨线转换里 100% 会丢。工具声明那批已由 reportForeignPreservedTools 记损，本文件补齐其余。

// StatefulConversionConflict 报告客户端正文里「目标线无承载位、且丢失会改变本次请求语义」的
// 状态型字段名；无冲突时返回空串。
//
// 判据与边界：
//   - 只对 responses 线判定：这三个键是 Responses API 的服务端状态语义（续接上一次响应、
//     绑定会话、显式落库）。其它方言里出现同名字段只是杂项键，不该因一个无意义的键把请求打回；
//   - `store: false` 不算冲突：它是多数 Responses 客户端的默认值，且语义等价于「不额外落库」，
//     目标线本来就不落库——把它判成冲突会打断正常流量。它的丢弃仍由
//     reportForeignDroppableFields 记损；
//   - 同线直通不经过本函数（原生对不转换，字段原样承载）。
func StatefulConversionConflict(sourceWire WireProtocol, body *Value) string {
	if sourceWire != ProtocolOpenAIResponses || body == nil {
		return ""
	}
	if previous, present := body.Get("previous_response_id"); present && isMeaningful(previous) {
		return "previous_response_id"
	}
	if conversation, present := body.Get("conversation"); present && isMeaningful(conversation) {
		return "conversation"
	}
	if store, present := body.Get("store"); present && isTrue(store) {
		return "store"
	}
	return ""
}

func isTrue(value *Value) bool {
	if value == nil {
		return false
	}
	boolean, ok := value.Bool()
	return ok && boolean
}

// foreignDroppableField 是一个「外线有、目标线无承载位」的请求侧顶层字段及其归因类别。
type foreignDroppableField struct {
	key   string
	class string
	// requireTruthy 为真时只有值为真才记损。
	//
	// 为何 `store` 需要它：`store:false` 是多数 Responses 客户端的默认值，语义等价于「不额外
	// 落库」，而目标线本来就不落库——记它等于给每个转换请求加一条恒定噪声（生产实测：
	// 每一行 +1）。真正要求落库（`store:true`）的 responses 请求由 StatefulConversionConflict
	// 拦在前面的 fail-closed，不会走到这里；但 chat 线的 `store:true` 仍会记损（其目标线
	// 不落库，而本函数只对 responses 源线判冲突）。
	requireTruthy bool
}

// foreignDroppableFields 是有承载体但与目标线不兼容的顶层字段清单。
//
// 键名与线的关系：prompt_cache_key / store 在 OpenAI 两线都有；response_format 是 chat 线的
// 结构化输出载体，responses 线走 text.format——故 responses 侧记的是整键 `text.controls`
// （含 format 与 verbosity，两者在 anthropic/chat 上都没有等价物）。
var foreignDroppableFields = []foreignDroppableField{
	{key: "prompt_cache_key", class: LossPromptCacheKey},
	{key: "response_format", class: LossResponseFormat},
	{key: "text", class: LossTextControls},
	{key: "store", class: LossStoreFlag, requireTruthy: true},
}

// reportForeignDroppableFields 把「留在外线 passthrough 里、目标线无法承载的高层字段」记入损失。
//
// 与 reportForeignPreservedTools 同源同因（同一处调用、同一类事实），只是字段清单不同。
// 为什么记损而不是 fail-closed：这些字段丢失后本次作答仍然正确，只是约束降级——静默丢弃才是
// 缺陷（客户端按 schema 解析拿到自由文本、客户端以为命中了前缀缓存而实际没有）。
func reportForeignDroppableFields(request *Request, target WireProtocol, loss *LossCollector, direction string, ctx ConvertCtx) {
	if request == nil || loss == nil {
		return
	}
	for wire, fields := range request.Passthrough {
		if wire == target || fields == nil {
			continue
		}
		for _, item := range foreignDroppableFields {
			value := fieldOrNil(fields, item.key)
			if !isMeaningful(value) {
				continue
			}
			if item.requireTruthy && !isTrue(value) {
				continue
			}
			// 网关注入的字段不算客户端声明的约束（见 ConvertCtx.GatewayInjectedBodyFields）。
			if ctx.isGatewayInjectedField(item.key) {
				continue
			}
			loss.Dropped(item.class, direction, item.key)
		}
	}
}
