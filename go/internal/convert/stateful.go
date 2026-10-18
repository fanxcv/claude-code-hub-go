package convert

// 本文件承载两类「请求侧高层字段在跨线转换时无承载位」的处置：
//
//	1. 状态型字段（previous_response_id / conversation / store:true / prompt /
//	   context_management）——丢失的不是约束而是上下文或落库语义，故由调用方在**转换真的
//	   发生时** fail-closed（见 forward.BuildPlan）；
//	2. 约束型字段（store:false / text 的非 format 成员）——丢失后可降级继续，但必须记损，
//	   否则用户看到的是「约束静默失效」（prompt_cache_key / response_format 例外：两线都有
//	   承载位，故进枢纽原样回写，见 Request.PromptCacheKey / Request.ResponseFormat）。
//
// 两者都只对**外线**字段生效：各编码器只读本线 passthrough（Node 亦然），而 request 侧的
// passthrough 全仓没有任何消费者（`passthroughFor` 的调用点都取目标线），所以外线字段在
// 跨线转换里 100% 会丢。工具声明那批已由 reportForeignPreservedTools 记损，本文件补齐其余。

// StatefulConversionConflict 报告客户端正文里「目标线无承载位、且丢失会改变本次请求语义」的
// 状态型字段名；无冲突时返回空串。
//
// 判据与边界：
//   - 只对 responses 线判定：这些键是 Responses API 的服务端状态语义（续接上一次响应、
//     绑定会话、引用服务端模板、编辑上下文、显式落库）。其它方言里出现同名字段只是杂项键，
//     不该因一个无意义的键把请求打回；
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
	// prompt（服务端模板引用）与 context_management（上下文编辑指令）同样只是本线的服务端
	// 状态语义：目标线既无承载位、也无等价能力，静默丢掉会让上游按**另一套上下文**作答。
	// relaykit 对这两个键是报错而非丢弃（to_oai_chat_req.go 的 validateResponsesRequestChatUnsupportedFields），
	// 本函数与之同口径。
	if prompt, present := body.Get("prompt"); present && isMeaningful(prompt) {
		return "prompt"
	}
	if management, present := body.Get("context_management"); present && isMeaningful(management) {
		return "context_management"
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

// response_format / text 不在此表：两者都能进枢纽（Request.ResponseFormat）并由 chat /
// responses 两线互转（见 codec_chat.go 的 chatResponseFormatValue 与 codec_responses.go 的
// responsesTextFormatValue）；`text` 里除 format 外的成员才记损，见 reportResidualTextControls。
//
// `web_search_options` / legacy `functions[]` / `function_call` 是 chat 线独有的工具声明与
// 强制调用形态，目标线都无承载位；它们不在 knownKeys 里，故只活在 chat 线 passthrough 中，
// 而 reportForeignPreservedTools 只看 `tools` 键——不在此表就完全没有痕迹。web_search_options
// 与 responses 线的 `web_search` 工具同属一个能力（故共用 LossWebSearchTool），legacy 的
// `functions[]` / `function_call` 是 function 声明，归 LossUnknownFieldTool。
//
// `prompt_cache_key` **不在本表**：它已进枢纽（Request.PromptCacheKey）——chat 与 responses
// 两线的解码器都读它（decodeChatRequest / decodeResponsesRequest），两线的编码器都原样回写，
// 只有 anthropic（无此概念）在编码器里记损。留在这里会把它重复记一遍。
var foreignDroppableFields = []foreignDroppableField{
	{key: "store", class: LossStoreFlag, requireTruthy: true},

	// 身份/观测类元数据：目标线不承载时**生成内容一点不变**，故归 info 档而非 rewrite。
	// 判据见 hub.go 的 lossSeverityByCapability 注释（rewrite = 上游看到的东西变了）。
	{key: "metadata", class: LossUnknownFieldIdentity},
	{key: "client_metadata", class: LossUnknownFieldIdentity},
	{key: "user", class: LossUnknownFieldIdentity},
	{key: "safety_identifier", class: LossUnknownFieldIdentity},
	{key: "service_tier", class: LossUnknownFieldIdentity},

	// include 只声明「返回哪些可选字段」（如 reasoning.encrypted_content），不影响生成内容本身，
	// 故归 info 档；top_logprobs 同理（只影响返回的 logprob 字段）。
	{key: "include", class: LossUnknownFieldIdentity},
	{key: "top_logprobs", class: LossUnknownFieldIdentity},
	{key: "truncation", class: LossUnknownFieldParam},
	// background:false 是默认语义（同步作答），目标线本来也是同步 ⇒ 与 store:false 同因不记。
	{key: "background", class: LossUnknownFieldParam, requireTruthy: true},
	{key: "presence_penalty", class: LossUnknownFieldParam},
	{key: "frequency_penalty", class: LossUnknownFieldParam},

	// 顶层 cache_control（请求级自动缓存）：块级提示由各编码器记 LossCacheControl，顶层这一处
	// 此前无人读（不在任何 knownKeys 里，落 passthrough 后无消费者），故与块级同类别补在这里。
	{key: "cache_control", class: LossCacheControl},

	{key: "web_search_options", class: LossWebSearchTool},
	{key: "functions", class: LossUnknownFieldTool},
	{key: "function_call", class: LossUnknownFieldTool},
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
		reportResidualTextControls(fields, loss, direction)
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

// hubCarriedRequestKeys 是「落在 passthrough 里、但已由枢纽或编码器读走」的键。
//
// 为何单列：它们不在任何线的 knownKeys 里（故会落进 passthrough），但均有消费者——
// prompt_cache_key 由解码器进 Request.PromptCacheKey 并在 chat/responses 编码器回写（anthropic
// 那条另有 LossPromptCacheKey 专用记录），metadata 进 Request.Metadata。不列入就是重复记损。
var hubCarriedRequestKeys = knownKeys{
	"prompt_cache_key": true,
	"metadata":         true,
	// text 是 responses 线的结构化输出载体：format 进枢纽、其余成员（verbosity 等）由
	// reportResidualTextControls 单独记损。不列入就会被 reportUnknownForeignFields 当成
	// 「未知顶层键」再记一遍（实测：text.verbosity 与整键 text 两条重复台账）。
	"text": true,
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
		// 已消费键集合 = 该线 knownKeys ∪ 白名单键 ∪ 枢纽承载键。三层缺一即重复记损
		// （stateful.go 开头那段注释里的 prompt_cache_key 就是踩过的坑）。
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

// reportResidualTextControls 记 responses 的 `text` 对象里除 format 之外成员的丢失。
//
// format 已进枢纽（其约束换上 `response_format` 或 `text.format` 两种形状都能承载），
// 而 verbosity 这类成员两条目标线都没有等价物——丢的是显示与详尽度偏好，故仍要留痕。
func reportResidualTextControls(fields *Value, loss *LossCollector, direction string) {
	text, present := fields.Get("text")
	if !present || !isRecord(text) {
		return
	}
	for _, member := range text.Members() {
		if member.Key == "format" || !isMeaningful(member.Value) {
			continue
		}
		loss.Dropped(LossTextControls, direction, "text."+member.Key)
	}
}
