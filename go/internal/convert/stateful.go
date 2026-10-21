package convert

// 本文件承载三类「请求侧高层字段在跨线转换时无承载位」的处置：
//
//	1. 状态型字段（previous_response_id / conversation / store:true）——丢失的不是约束而是
//	   上下文或落库语义，故由调用方在**转换真的发生时** fail-closed（见 forward.BuildPlan）；
//	2. 约束型字段（prompt_cache_key / store）——丢失后可降级继续，但必须记损，否则用户
//	   看到的是「约束静默失效」；
//	3. 结构化输出（response_format ↔ responses 的 text.format）——有承载位、能等价互转的
//	   就转（见 structured_output.go），只有目标线无承载位（anthropic）时才落回记损。
//
// 后两类都只对**外线**字段生效：各编码器只读本线 passthrough（Node 亦然），而 request 侧的
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
	// structuredOutput 为真表示该键是结构化输出约束：它由枢纽承载（Request.ResponseFormat）
	// 且 chat ↔ responses 两线能等价互转，故只在目标线无承载位时才记损。
	structuredOutput bool
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
// 键名与线的关系：response_format 是 chat 线的结构化输出载体，responses 线走 text.format——
// 两者可互转（structuredOutput 标记的那一对），故它们只对 anthropic 目标记损；text 里除 format
// 之外的成员（verbosity）在另两线上同样没有等价物，仍逐成员留痕。
//
// `prompt_cache_key` 列在表里：responses 源线的解码器把它抬进枢纽（故不再落 passthrough），
// chat / anthropic 源线则留在本线 passthrough 中、跨线时目标线读不到——正是本表要补的那一侧。
// responses 源线到 anthropic 的那一条由编码器记（见 codec_anthropic.go），本表不会重复。
var foreignDroppableFields = []foreignDroppableField{
	{key: "response_format", class: LossResponseFormat, structuredOutput: true},
	{key: "text", class: LossTextControls, structuredOutput: true},
	{key: "store", class: LossStoreFlag, requireTruthy: true},

	{key: "prompt_cache_key", class: LossPromptCacheKey},

	// 以下十一个键是 Responses / Chat 线的请求级开关与采样参数：枢纽没有承载位，跨线时整键丢。
	// 类别沿用 catch-all 家族的 `unknown_field.param`（seed / stop / parallel_tool_calls 等同属
	// 这一类），而不新造常量：它们正是「源线有、枢纽无」的同一类事实，detail 取键名，故台账里
	// 仍能按键定位。
	//
	// metadata 例外：它是身份/观测类元数据，目标线不承载时**生成内容一点不变**，故归 info 档
	// 而非 rewrite。判据见 hub.go 的 lossSeverityByCapability 注释（rewrite = 上游看到的东西变了）。
	{key: "metadata", class: LossUnknownFieldIdentity},
	{key: "client_metadata", class: LossUnknownFieldParam},
	{key: "include", class: LossUnknownFieldParam},
	{key: "truncation", class: LossUnknownFieldParam},
	// background:false 是默认语义（同步作答），目标线本来也是同步 ⇒ 与 store:false 同因不记。
	{key: "background", class: LossUnknownFieldParam, requireTruthy: true},
	{key: "top_logprobs", class: LossUnknownFieldParam},
	{key: "presence_penalty", class: LossUnknownFieldParam},
	{key: "frequency_penalty", class: LossUnknownFieldParam},
	{key: "user", class: LossUnknownFieldParam},
	{key: "service_tier", class: LossUnknownFieldParam},
	{key: "safety_identifier", class: LossUnknownFieldParam},

	// context_management 是 Responses 线的上下文编辑指令（如清空思考）：目标线无承载位也无
	// 等价能力，丢的是线级结构而非可降级约束，故走结构子类（改写档）。
	{key: "context_management", class: LossUnknownFieldStructure},
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
			// 结构化输出能带就带：枢纽持有该约束且目标线有承载位时不丢。
			if item.structuredOutput && request.ResponseFormat != nil && supportsStructuredOutput(target) {
				if item.key == "text" {
					// text 容器里除 format 之外的成员（verbosity）仍然无等价物。
					reportResidualTextMembers(value, loss, direction)
				}
				continue
			}
			loss.Dropped(item.class, direction, item.key)
		}
	}
}

// hubCarriedRequestKeys 是「落在 passthrough 里、但已由枢纽读走」的键。
//
// 为何单列：prompt_cache_key 不在任何线的 knownKeys 里（故会落进 passthrough），但两个解码器
// 都把它读进 Request.PromptCacheKey 并在 chat/responses 编码器回写（anthropic 那条另有
// LossPromptCacheKey 专用记录）。不列入就是重复记损。metadata 同属枢纽承载键，但它已在
// foreignDroppableFields 清单里（上一条记过），故这里不重复列。
var hubCarriedRequestKeys = knownKeys{
	"prompt_cache_key": true,
}

// reportUnknownForeignFields 把「白名单外的外线顶层字段」也记入损失。
//
// 为何需要兜底：reportForeignDroppableFields 只认清单——清单外的键（`container`、
// `max_tool_calls` 之类）落进 passthrough 后无任何消费者，跨线即零痕迹。漏一个键就是一条
// 静默丢弃，而清单永远是滞后的那一侧；故以「不在清单、也不在本线 knownKeys」为判据反推。
func reportUnknownForeignFields(request *Request, target WireProtocol, loss *LossCollector, direction string, ctx ConvertCtx) {
	if request == nil || loss == nil {
		return
	}
	for wire, fields := range request.Passthrough {
		if wire == target || fields == nil {
			continue
		}
		// 已消费键集合 = 该线 knownKeys ∪ 白名单键 ∪ 枢纽承载键。三层缺一即重复记损。
		consumed := knownRequestKeysFor(wire)
		for _, member := range fields.Members() {
			if consumed.contains(member.Key) || hubCarriedRequestKeys.contains(member.Key) ||
				isForeignDroppableKey(member.Key) {
				continue
			}
			if !isMeaningful(member.Value) {
				continue
			}
			if ctx.isGatewayInjectedField(member.Key) {
				continue
			}
			loss.Dropped(LossUnknownFieldParam, direction, member.Key)
		}
	}
}

// isForeignDroppableKey 报告键是否已在 foreignDroppableFields 清单里（已由上一个函数记过）。
func isForeignDroppableKey(key string) bool {
	for _, item := range foreignDroppableFields {
		if item.key == key {
			return true
		}
	}
	return false
}
