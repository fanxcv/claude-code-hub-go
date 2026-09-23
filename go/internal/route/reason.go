package route

import "encoding/json"

// Reason 是候选被过滤的理由，取值与 Node 侧 ProviderChainItem.filteredProviders[].reason 一致；
// 例外：ReasonEndpointUnavailable 是 Go 侧新增（Node 在 forwarder 判端点池，见 filter.go）。
type Reason string

const (
	// ReasonDisabled 供应商已禁用。
	ReasonDisabled Reason = "disabled"
	// ReasonExcluded 已在前序尝试中失败（故障转移排除列表）。
	ReasonExcluded Reason = "excluded"
	// ReasonScheduleInactive 不在调度时间窗口内。
	ReasonScheduleInactive Reason = "schedule_inactive"
	// ReasonFormatTypeMismatch 原始格式与供应商类型不兼容。
	ReasonFormatTypeMismatch Reason = "format_type_mismatch"
	// ReasonProtocolConversionDisabled 本可跨协议转换，但该供应商未开启转换开关。
	ReasonProtocolConversionDisabled Reason = "protocol_conversion_disabled"
	// ReasonTypeMismatch 类型不匹配（兼容 Node 枚举，当前实现不产出）。
	ReasonTypeMismatch Reason = "type_mismatch"
	// ReasonModelNotAllowed 供应商的模型允许集不包含请求模型。
	ReasonModelNotAllowed Reason = "model_not_allowed"
	// ReasonClientRestriction 客户端限制（允许/阻止名单）。
	ReasonClientRestriction Reason = "client_restriction"
	// ReasonCircuitOpen 熔断（供应商级或 vendor-type 级）。
	ReasonCircuitOpen Reason = "circuit_open"
	// ReasonRateLimited 金额限额或并发限额已达。
	ReasonRateLimited Reason = "rate_limited"
	// ReasonEndpointUnavailable 供应商厂下无任何启用端点（Go 侧前置排除，默认关闭）。
	ReasonEndpointUnavailable Reason = "endpoint_unavailable"
	// ReasonSlowRateCooldown 本会话对该渠道正在低速冷却期内（设计稿 §8 的会话级强制降级）。
	//
	// 前端已就位（2026-09-22 核）：`src/types/message.ts` 的过滤理由联合类型含本取值；
	// `messages/{zh-CN,en}/provider-chain.json` 的 filterReasons 与 filterDetails 两处均有词条；
	// `LogicTraceTab.test.tsx` 钉住渲染的是本地化文案而非原始 token。
	//
	// 唯一未覆盖的是 `provider-chain-formatter.ts` 的图标映射（一个三元表达式，本取值落默认分支）：
	// 图标属装饰，链上的理由与详情文本已足以事后归因，故不为它新增图标。
	ReasonSlowRateCooldown Reason = "slow_rate_cooldown"
	// ReasonSlowRateQuarantine 该渠道×模型组合被**低速隔离**：窗内慢事实已达触发阈值，
	// 故在有替代候选时不再参与正常选路（只在探针租约命中、或「无替代候选」时放行）。
	//
	// 为何与 ReasonSlowRateCooldown 分开：那条是**本会话**对该渠道的短期回避（60 秒，只影响
	// 单个会话）；本条是**渠道级**隔离（影响所有会话，直到连续干净样本把准入档位抬回）。
	// 合并会把「这家渠道整体被隔离」误报成「这个会话在回避它」。
	//
	// 为何必须算**软信号**（见 softSignalRejection）：隔离的意图是「有替代时让开」，而剔除
	// 唯一候选会让可用性反而变差（与 ReasonNoAlternativeFailOpen 同一条口径）。隔离本身也
	// 不承担「保证不选慢渠道」的职责——那是提交前速率闸门的事。
	ReasonSlowRateQuarantine Reason = "slow_rate_quarantine"
	// ReasonProviderErrorCooldown 本会话对该渠道正在**故障**冷却期内：该家刚在**本会话**里发生供应商侧
	// 失败（上游 5xx / 超时），60 秒内先绕开它。
	//
	// 为何与 ReasonSlowRateCooldown 分开：两者是同一个冷却键的两个写入者（见 `CooldownKind`），语义
	// 不同——本条是**故障回避**，那条是**低速降权**。合并会把「渠道故障」误报成「渠道慢」。
	//
	// 为何不并进 ReasonCircuitOpen：熔断是**全局**硬故障排除（整家渠道对所有会话都不可用），
	// 本理由是**本会话**对该家的短期回避（其他会话照常选它），两者不可互换。
	ReasonProviderErrorCooldown Reason = "provider_error_cooldown"
	// ReasonNoAlternativeFailOpen 表示该候选本因**软信号**（本网关自己加的回避，见
	// softSignalRejection）被排除，但排除后一个候选都不剩，于是被**重新纳入**本次选路。
	//
	// 为什么必须有这个理由（生产实证，2026-09-22）：某模型只有一家供应商，该会话进入 60 秒
	// 低速冷却后唯一候选被剔掉 ⇒ 无候选 ⇒ `POST /v1/chat/completions` 返回 503（30 分钟 33 次）。
	// 冷却的意图是让会话「逃到别家」，只有一家时无处可逃，却把唯一候选剔掉——可用性反而更差。
	//
	// 为何是**新词**而不是改写既有词：`provider_chain` 里的词被 `usage_ledger` 触发器、
	// 状态页分类器与前端渲染按**精确词**消费，改既有词等于改它们的语义。
	ReasonNoAlternativeFailOpen Reason = "no_alternative_fail_open"
)

// Filtered 是一个被过滤的候选及其理由，对应 Node 的 decisionContext.filteredProviders[]。
type Filtered struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Reason  Reason `json:"reason"`
	Details string `json:"details,omitempty"`
	// ClientRestrictionContext 与 Node 的 filteredProviders[].clientRestrictionContext 逐字对齐，
	// 只在「客户端名单拒绝」的条目上出现（其余理由为 nil，与 Node 相同）。
	ClientRestrictionContext *ClientRestrictionContext `json:"clientRestrictionContext,omitempty"`
}

// ClientRestrictionContext 是供应商级客户端限制的留痕，键名与 Node 落库的
// `filteredProviders[].clientRestrictionContext` 逐字对齐（provider-selector.ts:1283-1291）：
// `matchType` / `matchedPattern` / `detectedClient` / `providerAllowlist` / `providerBlocklist`。
type ClientRestrictionContext struct {
	// MatchType 取值与 Node 的 ClientRestrictionResult.matchType 一致：
	// `no_restriction` / `allowed` / `blocklist_hit` / `allowlist_miss`。
	MatchType string `json:"matchType"`
	// MatchedPattern 是命中的模式（黑名单命中时必有；白名单命中时是命中的那条）。
	MatchedPattern string `json:"matchedPattern,omitempty"`
	// DetectedClient 是识别到的客户端（子客户端关键字优先，否则 UA）。
	DetectedClient string `json:"detectedClient,omitempty"`
	// ProviderAllowlist / ProviderBlocklist 是本次判定用到的名单原文（Node 同名字段）。
	ProviderAllowlist []string `json:"providerAllowlist"`
	ProviderBlocklist []string `json:"providerBlocklist"`
}

// ClientRestriction 是一次供应商级客户端限制判定的结果。
//
// 由 `internal/guard`（client-detector 的移植方）构造并交给选路，避免把 UA 匹配实现两份。
type ClientRestriction struct {
	// Allowed 为假即该供应商被客户端名单排除。
	Allowed bool
	// MatchType / MatchedPattern / DetectedClient 与 Node 的判定结果同名同义。
	MatchType        string
	MatchedPattern   string
	DetectedClient   string
	CheckedAllowlist []string
	CheckedBlocklist []string
}

// context 返回留痕（与 Node 的 clientRestrictionContext 同字段）。无名单时不产出。
func (r *ClientRestriction) context() *ClientRestrictionContext {
	if r == nil {
		return nil
	}
	return &ClientRestrictionContext{
		MatchType:         r.MatchType,
		MatchedPattern:    r.MatchedPattern,
		DetectedClient:    r.DetectedClient,
		ProviderAllowlist: r.CheckedAllowlist,
		ProviderBlocklist: r.CheckedBlocklist,
	}
}

// clientReasonDetails 复刻 Node 写入 details 的取值：黑名单命中记 `blocklist_hit`，
// 白名单未命中记 `allowlist_miss`（provider-selector.ts:1275）；其余取值不出现。
func clientReasonDetails(matchType string) string {
	switch matchType {
	case MatchTypeBlocklistHit:
		return MatchTypeBlocklistHit
	case MatchTypeAllowlistMiss:
		return MatchTypeAllowlistMiss
	default:
		return matchType
	}
}

const (
	// MatchTypeBlocklistHit 命中黑名单。
	MatchTypeBlocklistHit = "blocklist_hit"
	// MatchTypeAllowlistMiss 未命中白名单。
	MatchTypeAllowlistMiss = "allowlist_miss"
)

// Candidate 是某一优先级下的候选，对应 Node 的 decisionContext.candidatesAtPriority[]。
type Candidate struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Weight 与 CostMultiplier 与 Node 的记录形态一致：数字，不加引号。
	// Weight 恒是供应商**配置的**权重（原值，不因同协议偏好而变），概率另见 Probability。
	Weight         int         `json:"weight"`
	CostMultiplier json.Number `json:"costMultiplier"`
	Probability    float64     `json:"probability"`

	// SameProtocol 报告该候选是否是「同协议」（客户端方言与供应商类型天然配对，不需要协议转换）。
	//
	// 为何用指针 + omitempty：同协议偏好关闭时本次**不做**该判定，nil 使 JSON 与加该字段之前
	// 逐字节一致（既有对拍与黄金样本不受影响）；启用时 true/false 都会输出，故跨协议候选
	// 不会被误读成「字段缺失」。
	SameProtocol *bool `json:"sameProtocol,omitempty"`
	// EffectiveWeight 是本次加权选择实际使用的权重（同协议候选为 weight×K，其余为 weight）。
	// 与 SameProtocol 同时出现/同时缺席；Probability 由它算出，故用户能看出「为什么被选中」。
	EffectiveWeight *float64 `json:"effectiveWeight,omitempty"`
}

// ConsideredCandidate 是一项**通过全部硬校验**、进入本轮候选池的供应商。
//
// 为什么需要它（用户实证，2026-09-14）：`priorityLevels` 只说「有哪些档位」，而
// `candidatesAtPriority` 只记**最高档**的候选。于是「档位更低而落选」的家在链上完全不可见——
// 用户看着链里只有一家，报「我看这个会话的决策链，还是看不到 opencode 那几个渠道参与呢？」，
// 而那几家其实**全部通过了硬校验、进了候选池**，只是档位更低没被选中。两种完全不同的结局
// （「这家不行」与「这家行，只是这一档没轮到它」）在界面上被压成了一个。
//
// 两个优先级数字都给，因为它们回答的问题不同：
//   - `effectivePriority` 是**分层依据**（决定它落在第几档），也是唯一能与 `selectedPriority`
//     比较、从而说出「为何落选」的数；
//   - `priority` 是**配置值**。两者不同时正说明「分组覆盖改写了这家的档位」——
//     生产实证：CommandCode 在 `fan` 组下配置值 4、分层值 0，只看配置值就会把「选了某档」
//     读成「选了低优先级」，从而把一次正确选择误判成缺陷。
type ConsideredCandidate struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Priority 是**未做分组覆盖**的配置优先级（providers.priority，与链项 priority 同一口径）。
	Priority int `json:"priority"`
	// EffectivePriority 是**分组覆盖后**的分层值（resolveEffectivePriority）。数值小 = 更优先。
	EffectivePriority int         `json:"effectivePriority"`
	Weight            int         `json:"weight"`
	CostMultiplier    json.Number `json:"costMultiplier"`
	// Selected 为真表示本次最终选中该家。
	Selected bool `json:"selected"`

	// SlowPenalty 是本次选路给该渠道叠加的低速降权量（0 = 未降权）。
	//
	// **为何必须落在本结构**：降权改的是排序依据，若链上不记它，就会出现「同一批候选、
	// 同一 userGroup，某家却排在后面」而界面无从归因的情形——那正是仓内已经栽过的
	// 「不可见排除层」。记下它，用户才能回答「这个渠道为何排在后面」。
	//
	// **为何带 omitempty**：`decisionContext` 是落链契约，`chain_test.go` 对键集有精确相等断言
	// （黄金样本 13 键）。未开启低速监控时降权恒为 0，本键不出现在 JSON 里，既有对拍逐字节不受影响；
	// 开了监控才多出这个键——那时链上多一个键正是预期行为（与 SameProtocol 同一手法）。
	// 仓内先例：reason.go 的 ModelSupportedProviders 用 `json:"-"` 避开对拍，代价是界面看不到；
	// 本字段是排障必需，故取出路 omitempty 而非隐藏。
	SlowPenalty int `json:"slowPenalty,omitempty"`
}

// SurvivingCandidate 是一项**通过全部硬校验却未参与竞争**的候选，用于回填「因前缀亲和短路而
// 没轮到」的那批供应商。
//
// 为什么需要它（生产实证，2026-09-14）：前缀亲和一旦命中就短路整场选路，`priorityLevels` 与
// `candidatesAtPriority` 只记被选中的那一家，于是「支持本模型、已通过全部硬校验、只是没参与
// 竞争」的家在界面上**完全不可见**。用户看到链里只有 P4 一家，报「优先级 1/2/3 的渠道都没参与
// 决策」，而它与 `filteredProviders`（真被滤、带 reason）呈现成**同一种观感**——两类完全不同
// 的事实被压成了一个。
//
// 内嵌 ConsideredCandidate 而非另立字段：两个数组描述的是同一件事（同一批通过者），字段必须同形，
// 前端才能用同一套渲染读取，也不会各自漂移。
//
// 落链口径：只在**确有候选因此未参与竞争**时才写这个键（见 affinitySurvivors）。无可报之事时
// 键不出现，使既有落链形态与黄金样本的键集精确相等断言不受影响（与 Candidate.SameProtocol
// 「关闭即与加该字段之前逐字节一致」同一手法）。
type SurvivingCandidate struct {
	ConsideredCandidate
	// AffinitySkipped 为真表示：本次系前缀亲和命中，该候选虽通过全部硬校验，但未参与竞争。
	AffinitySkipped bool `json:"affinitySkipped"`
}

// affinitySurvivors 把「通过全部硬校验的候选」投影为亲和短路的留痕。
//
// userGroup 必传：两个优先级数字都由它解析（见 ConsideredCandidate）。漏传会让「分组覆盖改写
// 了档位」这一事实在链上消失，正是本条留痕要回答的问题。
//
// 顺序沿用通过集自身的顺序（与 filteredProviders 的记法一致），**不重排**：排序是展示层的事，
// 记录层改动顺序会让同一事实在不同版本产生不同 JSON，白白制造对拍噪声。
//
// 返回 nil 即**无可报之事**：通过集里除被选中者外没有别人（那种情形 candidatesAtPriority 已经
// 把它写全了），于是落链里不出现这个键。
//
// 边界：若提名者不在通过集内（选择器级闸门放行、而请求级闸门把它滤掉了），整表都记
// affinitySkipped，不会有 Selected 项。这是如实记录，不做补偿——把非通过者也塞进来会让
// 「通过集」这个词失去意义。
func affinitySurvivors(healthy []Provider, selectedID int64, userGroup string, penalties penaltyTable) []SurvivingCandidate {
	if len(healthy) == 0 {
		return nil
	}
	skipped := 0
	out := make([]SurvivingCandidate, 0, len(healthy))
	for _, p := range healthy {
		selected := p.ID == selectedID
		if !selected {
			skipped++
		}
		out = append(out, SurvivingCandidate{
			ConsideredCandidate: consideredCandidate(p, selectedID, userGroup, penalties),
			AffinitySkipped:     !selected,
		})
	}
	if skipped == 0 {
		return nil
	}
	return out
}

// consideredFromSurvivors 把亲和短路的留痕投影成跨路径统一的「参与池」留痕。
//
// 为什么不直接再调一次 consideredCandidates：那会让两个键成为**两次独立计算**，成员集一旦分叉
// （例如通过集只有一家而提名者不在其中时，consideredCandidates 的 `len < 2` 规则与
// affinitySurvivors 的 `skipped == 0` 规则给出不同答案），界面就会同时看到两份互相矛盾的候选。
// 一个计算、两个投影，分叉在构造上不可能发生。
//
// 返回 nil 即两个键**一起缺席**——沿用 affinitySurvivors 的「无可报之事」口径，使既有落链形态
// 与黄金样本的键集精确相等断言不受影响。
func consideredFromSurvivors(survivors []SurvivingCandidate) []ConsideredCandidate {
	if len(survivors) == 0 {
		return nil
	}
	out := make([]ConsideredCandidate, 0, len(survivors))
	for _, survivor := range survivors {
		out = append(out, survivor.ConsideredCandidate)
	}
	return out
}

// TargetType 是由客户端格式推断出的目标供应商类型（Node 的 decisionContext.targetType）。
type TargetType string

const (
	TargetClaude           TargetType = "claude"
	TargetCodex            TargetType = "codex"
	TargetOpenAICompatible TargetType = "openai-compatible"
	TargetGemini           TargetType = "gemini"
	TargetGeminiCLI        TargetType = "gemini-cli"
)

// DecisionContext 是选路决策的完整留痕，字段名与 Node 落库 JSON 逐字一致。
type DecisionContext struct {
	TotalProviders   int        `json:"totalProviders"`
	EnabledProviders int        `json:"enabledProviders"`
	TargetType       TargetType `json:"targetType"`
	RequestedModel   string     `json:"requestedModel,omitempty"`

	UserGroup          string `json:"userGroup,omitempty"`
	AfterGroupFilter   *int   `json:"afterGroupFilter,omitempty"`
	GroupFilterApplied bool   `json:"groupFilterApplied"`

	BeforeHealthCheck int `json:"beforeHealthCheck"`
	AfterHealthCheck  int `json:"afterHealthCheck"`
	// FilteredProviders 恒出现（无过滤项时为 []），与黄金样本一致。
	FilteredProviders []Filtered `json:"filteredProviders"`

	PriorityLevels       []int       `json:"priorityLevels"`
	SelectedPriority     int         `json:"selectedPriority"`
	CandidatesAtPriority []Candidate `json:"candidatesAtPriority"`

	// ConsideredCandidates 是本次**通过全部硬校验、进入候选池**的全部供应商（含最终选中者）。
	//
	// 与两个既有键的分工：priorityLevels 说「有哪些档位」，candidatesAtPriority 说「选中那一档里
	// 有谁、各自命中概率多少」，本字段说「**谁参与了、各自哪一档、有没有被选中**」——只有它能让
	// 用户看出「优先级更低而落选」的家（见 ConsideredCandidate 的说明）。未选中者的落选原因由
	// EffectivePriority 与 SelectedPriority 比较得出：更大 ⇒ 档位更低；相等 ⇒ 同档未被抽中。
	//
	// **两条路径都写**：加权随机路径由 consideredCandidates 直接投影；亲和短路路径由
	// consideredFromSurvivors 投影**同一批通过者**。生产实证（2026-09-14，cchd-1.4.0，3 小时窗口）：
	// affinity_hit 1549 行的 considered 合计为 **0**、surviving 合计 5108；初始选路 272 行则反之
	// （considered 35、surviving 0）。于是「统一读本字段」的界面在亲和行上**一个候选都看不到**——
	// 而用户报「看不到那几个渠道参与」的那几行（960131/960132/960133）恰好全走亲和短路。
	//
	// 只在**确有落选候选**时才写（见 consideredCandidates / affinitySurvivors）：唯一候选必然被
	// 选中，没有可说之事，键不出现，使既有落链形态与黄金样本的键集精确相等断言不受影响。
	ConsideredCandidates []ConsideredCandidate `json:"consideredCandidates,omitempty"`

	// SurvivingCandidates 是「通过全部硬校验但未参与竞争」的候选留痕。
	//
	// 三类别在链上由此可分：filteredProviders（真被滤，带 reason）／本字段里 AffinitySkipped
	// （通过却未竞争）／本字段里 Selected 与链项自身的 id、name（最终选中）。
	// 只在亲和短路且确有候选被跳过时才出现（见 affinitySurvivors）。
	//
	// 与 ConsideredCandidates 的关系：**成员与顺序完全相同**（同一次计算、两个投影，见
	// consideredFromSurvivors），本字段多的是 AffinitySkipped 这一位——它回答「为何未参与竞争」。
	// 两者在亲和行上并存是有意的过渡形态：待前端统一读 ConsideredCandidates 后本字段可以退场
	// （把 AffinitySkipped 并进 ConsideredCandidate 即可），届时这份冗余自行消失。
	SurvivingCandidates []SurvivingCandidate `json:"survivingCandidates,omitempty"`

	SessionID           string  `json:"sessionId,omitempty"`
	ExcludedProviderIDs []int64 `json:"excludedProviderIds,omitempty"`

	// ModelSupportedProviders 是「声明支持本次请求模型」的供应商家数（统计范围与
	// totalProviders 相同）。
	//
	// **不进 JSON**：decisionContext 是落链契约（黄金样本对键集有精确相等断言），
	// 加键即破坏对拍。它是**进程内诊断事实**：无可用供应商时用它区分两种完全不同的成因——
	// 0 表示这个模型名没有任何供应商声明支持（用户报的 `dsf4` 即此形态），
	// >0 则表示模型是已知的，候选是因为熔断/限流/分组等其它原因没剩下。
	//
	// 为什么不能直接数 FilteredProviders 里 ReasonModelNotAllowed 的条数：过滤是**逐家短路**的，
	// 格式不兼容（或熔断）的家根本走不到模型检查，于是「模型没人支持」会被那些理由盖住。
	ModelSupportedProviders int `json:"-"`
}

// SelectionMethod 是选中该项的方式，取值与 Node 的 ProviderChainItem.selectionMethod 一致。
type SelectionMethod string

const (
	// MethodWeightedRandom 加权随机。
	MethodWeightedRandom SelectionMethod = "weighted_random"
	// MethodGroupFiltered 分组筛选后随机。
	MethodGroupFiltered SelectionMethod = "group_filtered"
	// MethodSessionReuse 会话复用。
	MethodSessionReuse SelectionMethod = "session_reuse"
	// MethodFailOpenFallback Fail Open 降级。
	MethodFailOpenFallback SelectionMethod = "fail_open_fallback"
	// MethodPrefixAffinity 最长前缀亲和。
	MethodPrefixAffinity SelectionMethod = "prefix_affinity"
)

// SelectionReason 是选中该项的原因，取值与 Node 的 ProviderChainItem.reason 相关子集一致。
type SelectionReason string

const (
	// ReasonSelectedInitial 首次选择（成功）。
	ReasonSelectedInitial SelectionReason = "initial_selection"
	// ReasonSelectedAffinity 最长前缀亲和命中（软提名，已通过全套硬校验）。
	ReasonSelectedAffinity SelectionReason = "affinity_hit"
)
