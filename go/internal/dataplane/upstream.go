package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件是「选路结果 → 转发候选」与「终态事实 → 落库」两个投影的生产实现。
//
// 为什么投影放在本包而不是 route / terminal：两边的类型都是各自包的最小视角（route.Provider
// 不含上游密钥，forward.Provider 不含分组倍率），把它们粘起来需要同时知道两侧的字段语义，
// 这正是装配层的职责；放进任何一侧都会引入反向依赖。

// candidateSource 从 store 读回转发所需的完整供应商视图。
//
// 读取口径：按 id 取一行 providers + （有厂时）按厂与类型取启用态 provider_endpoints。
// 未加缓存是刻意的取舍：候选只在选路与切换（少数路径）上取，每次 1–2 条按主键/索引的
// 只读查询；先量再优，避免在没有读数之前引入第二份供应商缓存（那正是 Node 侧修过的回归面）。
type candidateSource struct {
	pools  *store.Pools
	router *storeSelector
	// health 是端点级熔断的**只读**判定面（供应商级由选路器内的 healthRejection 判定）。
	// 为什么端点级要在这里判而不是在选路：Node 的端点池在 forwarder 内过滤
	// （endpoint-selector.ts 的 getPreferredProviderEndpoints），粒度是「厂+类型下的启用端点」，
	// 而供应商级候选在选路期就已定下——两层判定的位置不同，照 Node 放。
	// nil 与「开关关闭」同义：route.HealthReader.EndpointOpen 自身处理两种情形（均返回 false）。
	health *route.HealthReader
	logger *logx.Logger
}

// storeSelector 用 route.Selector 做失败切换（初始候选由守卫链的 provider 步骤给出）。
type storeSelector struct {
	selector *route.Selector
}

// newCandidateSource 建投影器。selector 为 nil 时不做故障转移（首次候选失败即终止）。
func newCandidateSource(
	pools *store.Pools,
	selector *route.Selector,
	health *route.HealthReader,
	logger *logx.Logger,
) *candidateSource {
	if logger == nil {
		logger = logx.New(nil)
	}
	return &candidateSource{
		pools:  pools,
		router: &storeSelector{selector: selector},
		health: health,
		logger: logger,
	}
}

// Candidate 把守卫链选中的供应商投影成转发候选。
func (s *candidateSource) Candidate(
	ctx context.Context,
	selection pctx.ProviderSelection,
	facts SelectionFacts,
) (*forward.Candidate, route.Result, error) {
	return s.fromStore(ctx, selection.ProviderID, facts.RawPassthrough)
}

// Failover 在失败切换时选下一个供应商。
//
// 选路输入与守卫链的 provider 步骤同源（模型、格式、分组、密钥、亲和正文），差别有两处：
//   - ExcludeIDs——排除前序失败者，否则会在同一个供应商上打转；
//   - 走 SelectFailover 而非 Select——**该路径不咨询前缀亲和**（Node 的
//     pickRandomProviderWithExclusion 语义，理由见 route.SelectFailover 的注释）。
//     亲和正文仍要传：它本身就是「选路输入同源」的一部分，由 SelectFailover 决定不用它。
//
// 三条经 route.SelectFailover 保证的约束：不提名（故亲和不会把候选跳到既非主选、
// 也不在排除表的第三家）、不做亲和查找（故不触发 Redis 里的命中续期）、
// 不产出写回事实（故终态不会拿该路径的选择去改亲和 generation）。
func (s *candidateSource) Failover(ctx context.Context, facts SelectionFacts) (*forward.Candidate, route.Result, error) {
	if s.router == nil || s.router.selector == nil {
		return nil, route.Result{}, nil
	}
	result, err := s.router.selector.SelectFailover(ctx, route.Request{
		Model:      facts.Model,
		Format:     facts.Format,
		Group:      facts.Group,
		KeyID:      facts.KeyID,
		ExcludeIDs: facts.ExcludeIDs,
		// AffinityBody 刻意不传：该路径不咨询亲和（Node parity，见 route.SelectFailover）。
		// 传了也是死数据，而「传着却不用」还会给后来者把它改回 Select 留下静默复活的机会。
	})
	if err != nil {
		return nil, result, fmt.Errorf("dataplane: 故障转移选路失败: %w", err)
	}
	if result.Provider == nil {
		return nil, result, nil
	}
	candidate, capture, err := s.fromStore(ctx, result.Provider.ID, facts.RawPassthrough)
	if err != nil {
		return nil, result, err
	}
	if capture.Provider == nil {
		// 故障转移的留痕必须带决策上下文：它是 provider_chain 里「为什么换到这家」的唯一证据。
		capture = result
	} else {
		capture = mergeSelectionCapture(capture, result)
	}
	return candidate, capture, nil
}

// mergeSelectionCapture 把「身份留痕」与「选路留痕」合成一条落链结果。
//
// 二者缺一不可：身份/权重/优先级/倍率/分组来自 DB 投影（fromStore），而决策上下文、选路方式、
// 健康快照、亲和详情只有选路器知道。
//
// 为何要合并而不是二选一：旧实现只在 `capture.Provider == nil` 时才用选路结果，而 fromStore
// **总是**返回带 Provider 的 capture——于是主路径（两边都有 Provider）把选路器算出的 Context
// 丢掉，`provider_chain` 里的 decisionContext 恒为零值（`totalProviders: 0`、
// `priorityLevels: null`、`filteredProviders: null`），界面上就是一片 0。
// Node 在 failover 路径是带的（`provider-selector.ts:409` 的 failedContext）。
func mergeSelectionCapture(capture, selected route.Result) route.Result {
	if capture.Provider == nil {
		// 无身份留痕：整份以选路侧为准（它可能连 Provider 都为空，让上层能判断「无候选」）。
		return selected
	}
	if selected.Provider == nil {
		// 选路侧无候选：保留身份留痕（它的上下文本就是零值，补了也不会更差）。
		return capture
	}
	capture.Context = selected.Context
	capture.Method = selected.Method
	capture.Reason = selected.Reason
	capture.CircuitState = selected.CircuitState
	capture.Affinity = selected.Affinity
	return capture
}

// fromStore 读回一个供应商并投影成转发候选；同时给出落 provider_chain 用的选路留痕。
//
// 留痕只填「这条链上确实知道的事实」（身份、权重、优先级、成本倍率、分组标签）：
// 决策上下文与健康快照来自选路器，本函数不编造。
func (s *candidateSource) fromStore(
	ctx context.Context,
	providerID int64,
	rawPassthrough bool,
) (*forward.Candidate, route.Result, error) {
	row, err := s.pools.FindProviderByID(ctx, providerID)
	if err != nil {
		return nil, route.Result{}, fmt.Errorf("dataplane: 读取供应商失败: %w", err)
	}
	if row == nil {
		return nil, route.Result{}, fmt.Errorf("dataplane: 供应商 %d 不存在", providerID)
	}
	priority := row.Priority
	// 容错解码：脏值不得让这一跳失败（口径与 route 的桥接一致，详见 store.DecodeGroupPriorities）。
	groupPriorities, _ := store.DecodeGroupPriorities(row.GroupPriorities)
	capture := route.Result{Provider: &route.Provider{
		ID:              row.ID,
		Name:            row.Name,
		ProviderType:    convert.ProviderType(row.ProviderType),
		URL:             row.URL,
		IsEnabled:       row.IsEnabled,
		Weight:          row.Weight,
		Priority:        &priority,
		CostMultiplier:  row.CostMultiplier,
		GroupTag:        row.GroupTag,
		GroupPriorities: groupPriorities,
		AllowedModels:   row.AllowedModels,
	}}
	candidate := &forward.Candidate{
		Provider: forward.Provider{
			ID:               row.ID,
			Name:             row.Name,
			Type:             convert.ProviderType(row.ProviderType),
			Key:              row.Key,
			URL:              row.URL,
			PreserveClientIP: row.PreserveClientIP,
			// 供应商级静态出站头（Node 的 provider.customHeaders）：在 forward.BuildUpstreamHeaders
			// 中于「内置覆盖之后、鉴权头之前」合并，鉴权头始终胜出；鉴权头与保留名在施加点剥离。
			CustomHeaders:                store.DecodeCustomHeaders(row.CustomHeaders),
			MaxRetryAttempts:             row.MaxRetryAttempts,
			RequestTimeoutNonStreamingMS: row.RequestTimeoutNonStreamingMS,
			// 并发会话上限（providers.limit_concurrent_sessions）：列可空，nil 折叠为 0（不限）。
			// 转发层拿它只为「占名额时判不判上限」，不在此处做任何判定。
			LimitConcurrentSessions: limitConcurrentSessions(row.LimitConcurrentSessions),
			// 首字节阈值是流式竞速的准入条件之一（0 表示这家不参与竞速）：不映射它，
			// 竞速在真实进程里永远不会开——单测手填字段是看不出来的，故这里必须有。
			FirstByteTimeoutStreamingMS: row.FirstByteTimeoutStreamingMS,
			ModelRedirects:              row.ModelRedirects,
			// 供应商级参数覆写偏好（Node 的同名列）：转发层据此改写上游正文，见
			// forward 的 ProviderOverrideApplier。列可空，故一律折叠成空串（与 inherit 同义）。
			CacheTTLPreference:                preferenceValue(row.CacheTTLPreference),
			CodexReasoningEffortPreference:    preferenceValue(row.CodexReasoningEffortPreference),
			CodexReasoningSummaryPreference:   preferenceValue(row.CodexReasoningSummaryPreference),
			CodexTextVerbosityPreference:      preferenceValue(row.CodexTextVerbosityPreference),
			CodexParallelToolCallsPreference:  preferenceValue(row.CodexParallelToolCallsPreference),
			CodexImageGenerationPreference:    preferenceValue(row.CodexImageGenerationPreference),
			CodexServiceTierPreference:        preferenceValue(row.CodexServiceTierPreference),
			AnthropicMaxTokensPreference:      preferenceValue(row.AnthropicMaxTokensPreference),
			AnthropicThinkingBudgetPreference: preferenceValue(row.AnthropicThinkingBudgetPreference),
			AnthropicAdaptiveThinking:         row.AnthropicAdaptiveThinking,
			GeminiGoogleSearchPreference:      preferenceValue(row.GeminiGoogleSearchPreference),
		},
		ConversionEnabled: row.ProtocolConversionEnabled,
		// 原始透传端点（count_tokens / responses/compact）不重试、不切换供应商：
		// Node 的 RAW_PASSTHROUGH_ENDPOINT_POLICY 把 allowRetry/allowProviderSwitch 定为 false。
		// 该取值只能来自路由（本包不知道请求路径归哪条族），故由 facts 透传进来。
		RawPassthrough: rawPassthrough,
	}
	// 厂级端点优先：provider_vendor_id 非空时，上游基址来自 provider_endpoints（按 sort_order）。
	if row.ProviderVendorID != nil && *row.ProviderVendorID > 0 {
		endpoints, endpointErr := s.pools.FindEnabledProviderEndpointsByVendorAndType(
			ctx, *row.ProviderVendorID, row.ProviderType,
		)
		if endpointErr != nil {
			return nil, route.Result{}, fmt.Errorf("dataplane: 读取供应商端点失败: %w", endpointErr)
		}
		// 端点级熔断开闸时**不得**把该端点放进候选：Node 的 getPreferredProviderEndpoints
		// 会剔掉 circuitState==="open" 的端点（endpoint-selector.ts，受 ENABLE_ENDPOINT_CIRCUIT_BREAKER
		// 控制，开关关闭时整段跳过）。少了这一步就是「页面显示该端点已熔断、请求却照打」。
		//
		// 已过窗口的 open 由 EndpointOpen 内部按过期判定放行（试探），与供应商级同源。
		//
		// 已知天花板：逐个 HGETALL，端点数为 N 时是 N 次往返；本仓库单厂单类型的端点数是个位数，
		// 先量再优。若将来端点数上量，改为按 id 批量取（Node 侧就是 getAllEndpointHealthStatusAsync 的批读）。
		skippedCircuitOpen := 0
		for _, endpoint := range endpoints {
			if s.health.EndpointOpen(ctx, endpoint.ID) {
				skippedCircuitOpen++
				continue
			}
			label := ""
			if endpoint.Label != nil {
				label = *endpoint.Label
			}
			candidate.Endpoints = append(candidate.Endpoints, forward.Endpoint{
				ID:    endpoint.ID,
				URL:   endpoint.URL,
				Label: label,
			})
		}
		if skippedCircuitOpen > 0 {
			// 全被剔掉时列表为空，endpoints() 会退化为 Provider.URL：这正是 Node 非严格模式的
			// 行为（endpointCandidates.push({endpointId: null, baseUrl: currentProvider.url})），
			// 不是静默降级，故此处只记事实、不改语义。
			s.logger.Info("dataplane.endpoints_circuit_open_skipped", map[string]any{
				"providerId": row.ID,
				"vendorId":   *row.ProviderVendorID,
				"skipped":    skippedCircuitOpen,
				"candidates": len(candidate.Endpoints),
			})
		}
	}
	candidate.Provider.Endpoints = candidate.Endpoints
	return candidate, capture, nil
}

// preferenceValue 把可空的供应商偏好列折叠成空串。
//
// 为什么折叠而不留 nil：转发层的偏好一律用「空串 = 未配置」表达（与 DB 里的 `inherit`
// 同义），这样覆写实现不需要在每处都判一遍指针。
func preferenceValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// limitConcurrentSessions 把可空的并发上限列折成 int；nil 与负值一律归为 0（不限）。
//
// 为何把负值也归 0：列由管理面写入，历史上没有取值下限校验；负数在「占名额」缝里
// 会被当成「有上限」而使每个请求都被拒——一个脏值不该把一条渠道彻底打停。
func limitConcurrentSessions(value *int) int {
	if value == nil || *value < 0 {
		return 0
	}
	return *value
}

// settleTimeout 是终态写入的独立时间上限。
//
// 为什么不用请求上下文：终态入账必须活过客户端断开。客户端中断、上游截断、静默超时这三类
// 终态恰恰都发生在请求上下文已取消之后——用请求上下文写库会让「断线计量」这件事必然丢账。
// 独立上下文有界（本值），因此进程退出时不会无限等待。
const settleTimeout = 15 * time.Second

// storeSettler 把终态事实落进 message_request。
type storeSettler struct {
	settler *terminal.Settler
	writer  terminal.Writer
	state   *RequestState
	logger  *logx.Logger
	// costs 为 nil 表示本进程未接计费（无设置源或价格表不可读），终态照写但不带金额。
	costs *costResolver
	// cacheScore 是 F3b 缓存模拟列的开关与事实装配；nil 表示未接线，此时不写那五列。
	// 它与计费相互独立：Node 的 F3b 门控只看缓存效果开关，与能否取到价格无关。
	cacheScore *cacheScoreGate
	// codexPriority 是 codex priority（Fast Mode）计费档的判定面；nil 表示未接线（不计 priority 档）。
	codexPriority *codexPriorityGate
	// now 可注入时钟；nil 时用 time.Now。
	now func() time.Time
}

// NonStream 承接非流式终态。
func (s *storeSettler) NonStream(
	ctx context.Context,
	pc *pctx.Context,
	result *forward.Result,
	failure *forward.Failure,
) error {
	settlement := s.baseSettlement(pc, failure, servingProviderID(result))
	if result != nil {
		// 只有拿到真实上游状态码时才覆盖：全部尝试耗尽时 result.StatusCode 是 0，
		// 覆盖会把失败归因（来自 failure）抹成「无状态码」，而终态写必须带合法状态码。
		if result.StatusCode > 0 {
			settlement.StatusCode = result.StatusCode
		}
		settlement.DurationMS = s.nonStreamDuration(result)
		// ttfb_ms / first_byte_ms 的口径与 Node 一致：Node 的非流式终态写
		// `ttftMs: session.ttftMs ?? duration` 与 `firstByteMs: session.firstByteMs ?? duration`
		// （`response-handler.ts:3212-3213`），而这两个 session 值只在流式路径被记录，
		// 故非流式行落的就是 duration。两列不写会让它们恒 NULL，进而使
		// 公开页 TPS（`ComputeTokensPerSecond` 要求 first_byte_ms 非空）与排行榜 tok/s 榜
		// （`admin_leaderboard.go` 的 `first_byte_ms IS NOT NULL`）双双缺数。
		settlement.FirstByteMS = settlement.DurationMS
		settlement.TTFTMS = settlement.DurationMS
		usage, actualModel := nonStreamFacts(result.Plan, result.Body)
		if usage != nil {
			settlement.Usage = usageFromConvert(*usage)
		}
		if actualModel != "" {
			settlement.ActualResponseModel = &actualModel
		}
		if len(result.Attempts) > 0 {
			settlement.ProviderChain = s.providerChain(result.Attempts)
		}
		// 提交前判慢事实：与 provider_chain 同一处收——尝试留痕只在这两处可见，
		// 而它正是「哪家被判废」的唯一来源。
		settlement.SlowPrecommit = s.slowPrecommit(pc, result.Attempts)
		// routing_trace：把本次真实跑过的路径与实测事件落库（Node 侧由 discovery/竞速子系统写）。
		// 事实源全部来自 forward 层的测量；无真实留痕时返回 nil，列保持原值。
		settlement.RoutingTrace = s.routingTrace(result.Attempts, result.EndedAt, result.StatusCode, settlement.TTFTMS, settlement.FirstByteMS)
		// 终态追加的审计：转换后思考强度探针（取转换器产物 plan.Body 里的实际值）+ 协议转换记录。
		// 两段写入靠存储层的 jsonb 追加语义与建行时的客户端侧审计共存
		// （见 store.DetailsPatch.SpecialSettingsAppend）。
		settlement.SpecialSettingsAppend = specialSettingsAppendEntries(s.state, result.Plan)
	}
	if settlement.StatusCode == 0 {
		settlement.StatusCode = 502
	}
	// 计费用客户端请求的原始模型名与**实际生效**的重定向目标：计划里没有重定向时两者
	// 相同，此时无论取价基准是哪一个都得到同一个价格。
	redirected := s.state.Model
	if result != nil && result.Plan != nil && result.Plan.Redirect != nil {
		redirected = result.Plan.Redirect.Target
	}
	settlement.Cost = s.costs.resolve(context.Background(), costInput{
		RequestedModel:     s.state.Model,
		RedirectedModel:    redirected,
		StatusCode:         settlement.StatusCode,
		Usage:              settlement.Usage,
		ProviderMultiplier: s.state.ProviderMultiplier,
		ProviderGroupTag:   s.state.ProviderGroupTag,
		UserGroup:          s.state.UserGroup,
		// codex priority 档：非流式可拿到响应侧 service_tier（见 codex_priority_billing.go）。
		PriorityServiceTierApplied: s.codexPriority.applied(context.Background(),
			string(result.Provider.Type), s.state.requestedServiceTier, parseServiceTierFromResponseText(result.Body)),
	})
	// 成本倍率列：非流式不写 F3b（Node 只在流式路径产出，见 cachescore.go）。
	applyCostMultipliers(&settlement, s.state)
	// 亲和写回时机：非流式成功在拿到响应时、供应商侧失败在失败判定处（见 affinity.go）。
	settlement.Affinity = affinityDirectiveForNonStream(result, failure)
	return s.logSettle(ctx, pc, settlement)
}

// Stream 承接流式终态。
func (s *storeSettler) Stream(ctx context.Context, pc *pctx.Context, outcome forward.StreamOutcome) error {
	settlement := s.baseSettlement(pc, nil, outcome.Provider.ID)
	settlement.StatusCode = outcome.StatusCode
	if settlement.StatusCode == 0 {
		settlement.StatusCode = 200
	}
	if !s.state.StartedAt.IsZero() && !outcome.At.IsZero() {
		duration := int(outcome.At.Sub(s.state.StartedAt).Milliseconds())
		settlement.DurationMS = &duration
	}
	observation := outcome.Observation
	if observation.BufferOverflow {
		// 诊断信号：分帧器曾因超长单行丢帧，本次的 model/usage 只能靠窗口回退补回。
		// 生产上「同一供应商成批行没用量、而模型名有值」需靠这条与紧随的 settle 结果对账，
		// 区分「上游没报」与「我们没接住」。
		s.logger.Debug("dataplane.stream_framing_overflow", map[string]any{
			"provider_id":   outcome.Provider.ID,
			"provider_name": outcome.Provider.Name,
			"usage_seen":    observation.UsageSeen,
			"model":         observation.Model,
		})
	}
	if !observation.UsageSeen {
		// 流式成功但一条用量都没解析到：这是「账务缺数」的入口，必须留可对账的现场。
		// 新加到弃用：Bytes/Frames 的关系就能区分两种成因——
		//   Frames 远少于 Bytes/行宽（例如几十帧 vs 几百 KB）→ 分帧器丢帧（BufferOverflow）；
		//   Frames 正常但上游从未发 usage → 上游侧事实，不是本进程漏接。
		s.logger.Debug("dataplane.stream_usage_missing", map[string]any{
			"provider_id":     outcome.Provider.ID,
			"provider_name":   outcome.Provider.Name,
			"bytes":           observation.Bytes,
			"frames":          observation.Frames,
			"buffer_overflow": observation.BufferOverflow,
			"completion":      observation.CompletionMarker,
			"model":           observation.Model,
			"kind":            string(outcome.Kind),
		})
	}
	if observation.TTFT > 0 {
		ttft := int(observation.TTFT.Milliseconds())
		settlement.TTFTMS = &ttft
	}
	// first_byte_ms = **真 TTFB**：上游首个非空 chunk 相对请求开始的延迟（Node 的
	// `recordFirstByte(attempt.firstByteAt)`，即 `session.firstByteMs`）。
	// 它与 ttfb_ms 是两个口径：Node 的 ttftMs 取「门控提交后的首个内容帧」，
	// firstByteMs 取上游侧时刻（见 aggregation-core.ts 的 TPS 注释）。
	if observation.FirstByte > 0 {
		firstByte := int(observation.FirstByte.Milliseconds())
		settlement.FirstByteMS = &firstByte
	}
	if observation.Model != "" {
		model := observation.Model
		settlement.ActualResponseModel = &model
	}
	if observation.UsageSeen {
		usage := usageFromConvert(observation.Usage)
		settlement.Usage = usage
	}
	// 流式请求的 provider_chain 只能在这一次终态写里落库（终态列只允许写一次），故它是
	// 终态时刻的快照：竞速的输家留痕在胜者之后仍会被追加，能赶上就算上（Node 同样在
	// 流结束时落链）。
	if len(outcome.Attempts) > 0 {
		settlement.ProviderChain = s.providerChain(outcome.Attempts)
	}
	// 提交前判慢事实：同非流式口径，与 provider_chain 同一处收。
	settlement.SlowPrecommit = s.slowPrecommit(pc, outcome.Attempts)
	// routing_trace：同非流式口径（事实来自 forward 的实测留痕）。
	settlement.RoutingTrace = s.routingTrace(outcome.Attempts, outcome.At, outcome.StatusCode, settlement.TTFTMS, settlement.FirstByteMS)
	// 终态追加的审计：与信息式路径同一口径（取转换器产物而非二次推导）。
	settlement.SpecialSettingsAppend = specialSettingsAppendEntries(s.state, outcome.Plan)
	if outcome.Err != nil {
		message := outcome.Err.Error()
		settlement.ErrorMessage = &message
	}
	if outcome.Kind != forward.TerminalCompleted && outcome.Kind != forward.TerminalUpstreamTruncated {
		if settlement.ErrorMessage == nil {
			message := string(outcome.Kind)
			settlement.ErrorMessage = &message
		}
	}
	redirected := s.state.Model
	if outcome.Plan != nil && outcome.Plan.Redirect != nil {
		redirected = outcome.Plan.Redirect.Target
	}
	settlement.Cost = s.costs.resolve(context.Background(), costInput{
		RequestedModel:     s.state.Model,
		RedirectedModel:    redirected,
		StatusCode:         settlement.StatusCode,
		Usage:              settlement.Usage,
		ProviderMultiplier: s.state.ProviderMultiplier,
		ProviderGroupTag:   s.state.ProviderGroupTag,
		UserGroup:          s.state.UserGroup,
		// 流式只给请求侧档位：actual 需扫整段 SSE（见文件头注释），缺失时与 Node
		// 「响应未返回 service_tier」同支，回退 requested。
		PriorityServiceTierApplied: s.codexPriority.applied(context.Background(),
			string(outcome.Provider.Type), s.state.requestedServiceTier, ""),
	})
	applyCostMultipliers(&settlement, s.state)
	// F3b 缓存模拟列（仅流式，与 Node 同一处产出）。
	s.applyCacheScoreFields(ctx, &settlement, pc, outcome)
	settlement.Affinity = affinityDirectiveForStream(outcome)
	return s.logSettle(ctx, pc, settlement)
}

// logSettle 结算并留痕失败原因。
//
// 为什么这里必须自己记日志：流式路径的调用方（forward）刻意丢弃结算错误——它只保证
// 「整条流只结算一次」这条纪律，不让入账失败改变已定终态。若本层也不记，结算失败就会
// 变成静默的账务缺口（终态列悄悄留空），那正是最难发现的一类缺陷。
func (s *storeSettler) logSettle(_ context.Context, pc *pctx.Context, settlement terminal.Settlement) error {
	_, hasRow := pc.MessageRequestID()
	// 租约结算事实随终态一起交给结算器：判定时用过的切片只在 pctx 里（守卫链的限流步写），
	// 而结算器只看得到 Settlement 这一份输入。未走租约判定时保持零值，旁路自动跳过。
	if plan, ok := pc.LeaseSettlementPlan(); ok {
		settlement.LeaseSettlement = plan
	}
	// 低速样本事实：两条路径（NonStream / Stream）在这里汇合，故构造一次即可覆盖两者。
	settlement.SlowRate = s.slowRateSample(pc, settlement)
	// 低速改道事实：与样本同处汇合，同样一次覆盖两条路径。
	//
	// 为何放在终态而不是选路处就计：选路处只知道「本次选了谁」，不知道「这次请求最终成不成」
	// ——而计数的语义是「本该选它、实际选了别家」，那是一次已完成的选路的事实，
	// 且必须走旁路（不得在选路热路径上写 Redis）。
	settlement.SlowDiverts = s.slowDiverts()
	settleCtx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	err := s.settle(settleCtx, pc, settlement)
	if err != nil {
		s.logger.Warn("dataplane.settle_failed", map[string]any{
			"status_code": settlement.StatusCode,
			"hasRow":      hasRow,
			"error":       err.Error(),
		})
	}
	return err
}

// slowRateSample 把一次终态折算成低速样本事实（不判定，判定在 slowrate 包）。
// 三处取值口径：
//   - ProviderID 用行级 provider_id（resolveSettlementProviderID 已算好，直接复用）——
//     慢的是「实际作答的那一家」，不是入口首选的候选（回退/竞速换家后两者不同）；
//   - ModelKey 取客户端原始模型名，与公开状态同一口径（跨供应商别名归一，
//     `pubstatus.ResolveSuccessRateModelKey`）；此处只有原始名一份事实，故直接传它；
//   - SessionID / KeyID 供会话级冷却键：会话身份来自本请求的会话步骤记录（state.sessionID），
//     密钥 id 来自鉴权槽位。未接线时保持零值，slowrate 会只做渠道级统计。
func (s *storeSettler) slowRateSample(pc *pctx.Context, settlement terminal.Settlement) terminal.SlowRateSample {
	scope := s.slowScope(pc)
	sample := terminal.SlowRateSample{
		ModelKey:     scope.ModelKey,
		StatusCode:   settlement.StatusCode,
		DurationMS:   settlement.DurationMS,
		FirstByteMS:  settlement.FirstByteMS,
		OutputTokens: settlement.Usage.OutputTokens,
		SessionID:    scope.SessionID,
		KeyID:        scope.KeyID,
		RequestID:    scope.RequestID,
	}
	if settlement.ProviderID != nil {
		sample.ProviderID = *settlement.ProviderID
	}
	return sample
}

// slowScope 见 storeSettler.slowScope。
type slowScope struct {
	ModelKey  string
	SessionID string
	KeyID     int64
	RequestID int64
}

// slowScope 是两类低速事实（终态速率样本、提交前判废）共同的**作用域原料**。
//
// 为何抽到一处：两者必须落到**同一把键**上。模型键或请求 id 任一不一致，写进的滑窗就
// 不是读侧要数的那个（读侧按请求的模型键查 state 与滑窗），标记于是静默隐形——这正是
// 本仓反复出现的一类缺陷（「已定义≠未接线」的同构形态：值算了但没落到读侧看的地方）。
func (s *storeSettler) slowScope(pc *pctx.Context) slowScope {
	scope := slowScope{
		ModelKey: pubstatus.ResolveSuccessRateModelKey(&s.state.Model, nil),
		// 会话身份来自本请求的会话步骤记录（state.sessionID），密钥 id 来自鉴权槽位；
		// 未接线时保持零值，slowrate 会只做渠道级统计、不写会话冷却。
		SessionID: s.state.sessionID,
	}
	if pc != nil {
		if auth, ok := pc.Auth(); ok {
			scope.KeyID = auth.KeyID
		}
		if id, ok := pc.MessageRequestID(); ok {
			scope.RequestID = id
		}
	}
	return scope
}

// slowPrecommit 取本次请求里「因提交前探测判废」的渠道。
//
// 为何要在这里收：判废事实本来只挂在尝试留痕上（forward.AttemptOutcome.ProbeSlow），而低速
// 写侧只采**作答者**的终态样本——判废后由别家作答成功时，那条样本落在别家，被判废的慢家
// 不会因此被标慢。本函数把该事实从尝试留痕里捞出来交给终态旁路，闭环才接上。
//
// 按渠道去重（同一请求对同一家可能多次尝试：重试、竞速），且**只认 ProbeSlow**：普通的
// 尝试失败（超时、5xx、被竞速淘汰）不构成「这家慢」的结论。
//
// 两条终态路径都要调（非流式也不能漏）：闸门跑在 forward 的尝试处理里，当响应是 JSON 等
// 非流形态且被 gated/ForceGate 命中时同样会走到探测，故 ProbeSlow 并非流式独有。
func (s *storeSettler) slowPrecommit(pc *pctx.Context, attempts []forward.AttemptOutcome) []terminal.SlowPrecommit {
	if s == nil || s.state == nil || len(attempts) == 0 {
		return nil
	}
	scope := s.slowScope(pc)
	// 模型键或请求 id 缺失就不写：请求 id 是滑窗成员的幂等键，没有它就无从保证
	// 「同一请求只计一次」（与 slowrate.Record 的同一道前置）。
	if scope.ModelKey == "" || scope.RequestID <= 0 {
		return nil
	}
	seen := map[int64]struct{}{}
	out := make([]terminal.SlowPrecommit, 0, 1)
	for _, attempt := range attempts {
		if !attempt.ProbeSlow || attempt.ProviderID <= 0 {
			continue
		}
		if _, ok := seen[attempt.ProviderID]; ok {
			continue
		}
		seen[attempt.ProviderID] = struct{}{}
		out = append(out, terminal.SlowPrecommit{
			ProviderID: attempt.ProviderID,
			ModelKey:   scope.ModelKey,
			RequestID:  scope.RequestID,
			SessionID:  scope.SessionID,
			KeyID:      scope.KeyID,
		})
	}
	return out
}

// noProviderDivertedAll 从无可用供应商的归因里抽出「被低速会话冷却剔掉」的家。
//
// 为何另开一条路而不复用 route.DivertedAll：那条路径的输入是**选路留痕**（decisionContext），
// 而无候选时压根没有选路结果——留痕只剩归因里的 filtered 列表（id + reason）。
//
// 只认冷却这一个成因是有意的：降权（penalty）在无候选时无从成立——降权只是把档位抬高，
// 仍会进候选池与加权随机，不会让候选变成 0（能变 0 的只有硬性剔除，冷却就是其一）。
func noProviderDivertedAll(diagnostic guard.NoProviderDiagnostic) []route.Divert {
	out := make([]route.Divert, 0, len(diagnostic.Filtered))
	for _, filtered := range diagnostic.Filtered {
		if route.Reason(filtered.Reason) != route.ReasonSlowRateCooldown {
			continue
		}
		out = append(out, route.Divert{ProviderID: filtered.ID, Cause: route.DivertCauseCooldown})
	}
	return out
}

// slowDiverts 取本次请求里「因低速机制被改道」的渠道及其成因。
//
// 扫全部选路留痕（初选 + 竞速 + 故障转移各一次），按 (渠道, 成因) **去重**：
// 一次请求里同一家可能被多次选路都挤掉（例如重试时重跑选路），按请求计一次即可
// ——用户问的是「多少**请求**因撞上低速而被换渠道」，不是「多少候选被剔」。
func (s *storeSettler) slowDiverts() []terminal.SlowDivert {
	if s == nil || s.state == nil {
		return nil
	}
	seen := map[terminal.SlowDivert]struct{}{}
	out := make([]terminal.SlowDivert, 0, 2)
	appendDivert := func(providerID int64, cause string) {
		if providerID <= 0 || cause == "" {
			return
		}
		entry := terminal.SlowDivert{ProviderID: providerID, Cause: cause}
		if _, ok := seen[entry]; ok {
			return
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	for _, capture := range s.state.selectionsSnapshot() {
		for _, divert := range route.DivertedAll(capture.Context) {
			appendDivert(divert.ProviderID, string(divert.Cause))
		}
	}
	// 无候选那条路径上的改道（503）单独收：那里没有选路结果可供扫描。
	for _, divert := range s.state.divertsSnapshot() {
		appendDivert(divert.ProviderID, string(divert.Cause))
	}
	return out
}

// baseSettlement 填两侧共有的字段（供应商、模型归属、失败归因）。
func (s *storeSettler) baseSettlement(pc *pctx.Context, failure *forward.Failure, servingProviderID int64) terminal.Settlement {
	settlement := terminal.Settlement{}
	if failure != nil {
		if failure.StatusCode > 0 {
			settlement.StatusCode = failure.StatusCode
		}
		if failure.Message != "" {
			message := failure.Message
			settlement.ErrorMessage = &message
		}
	}
	settlement.ProviderID = resolveSettlementProviderID(pc, failure, servingProviderID)
	return settlement
}

// resolveSettlementProviderID 决定落库的行级供应商（Node 的 providerIdForPersistence）。
//
// 优先级：**实际作答的那一家** > 失败归属 > 入口选中的候选。
//
// 为何作答者优先：`pctx.SetProvider` 只在 provider 守卫里调用**一次**，记录的是入口选中的
// 首个候选；回退/竞速换家后它不会更新。若让它覆盖，行的 provider_id 就变成「第一个尝试的
// 供应商」，而真实作答的是另一家（真实事故：链里 `hedge_winner` 是 Ollama Codex，行的
// provider_id 却是首选 HC_Chat）。Node 侧口径：`providerIdForPersistence =
// meta?.providerId ?? provider?.id`（`response-handler.ts:1912`），且
// `repository/message.ts:632` 的注释即「支持更新最终供应商ID（重试切换后）」。
func resolveSettlementProviderID(pc *pctx.Context, failure *forward.Failure, servingProviderID int64) *int64 {
	if servingProviderID != 0 {
		id := servingProviderID
		return &id
	}
	if failure != nil && failure.ProviderID > 0 {
		id := failure.ProviderID
		return &id
	}
	// 以上都未知时（例如请求在上游之前就被拦下）才用选中的候选兜底。
	if pc != nil {
		if selection, ok := pc.Provider(); ok && selection.ProviderID != 0 {
			id := selection.ProviderID
			return &id
		}
	}
	return nil
}

// servingProviderID 取作答者的供应商 ID；无结果时为 0。
func servingProviderID(result *forward.Result) int64 {
	if result == nil {
		return 0
	}
	return result.Provider.ID
}

// providerChain 把尝试留痕编译成 provider_chain 列。
//
// 链的形状与 Node 一致（两类条目，选择期在前）：
//
//	[0]  选择期条目：initial_selection / affinity_hit / session_reuse，来自守卫链的选路结果
//	[1..] 尝试期条目：request_success / retry_failed / hedge_*，来自 forward 的每次尝试
//
// 两类条目缺一不可，且不可混为一条：界面弹窗读链首判定「渠道复用 / 新会话」与决策上下文
// （provider-chain-popover.tsx:522,526,533），而尝试期条目带的是「这一刻的结局」
// （端点、状态码、失败说明）。此前本函数只由尝试生成条目，于是链首永远是首次尝试——
// 界面上既看不到亲和命中，也看不到选择的决策上下文，正是 2026-09-13 的生产形态。
func (s *storeSettler) providerChain(attempts []forward.AttemptOutcome) []byte {
	items := make([]route.ChainItem, 0, len(attempts)+1)
	if entry, ok := s.selectionChainEntry(attempts); ok {
		items = append(items, entry)
	}
	for index, attempt := range attempts {
		item := s.attemptChainItem(attempt, index)
		if attempt.Reason != "" {
			item.Reason = attempt.Reason
		}
		// 端点与结局：Node 的 chain[1..] 带这三项（黄金样本 chain[1] 的键集含
		// endpointId / endpointUrl / statusCode；失败时另有 errorMessage）。
		if attempt.StatusCode != 0 {
			statusCode := attempt.StatusCode
			item.StatusCode = &statusCode
		}
		item.ErrorMessage = attempt.Message
		if ws := attempt.WS; ws != nil {
			// 上游 WS 的事实单独成一条**信息性**条目：它描述的是「尝试之前的传输层前置事实」，
			// 而尝试条目的 reason 记的是这次尝试的结局。两者混为一条会把真实结局
			// （request_success 等）覆盖成非成功词，让可用率把成功算成失败。
			//
			// 两个 WS 词在 pubstatus 里是 neutral（既不算成功也不算失败），与 http2_fallback 同地位。
			info := s.attemptChainItem(attempt, index)
			info.Reason = wsChainReason(ws)
			applyWSAttemptDetails(&info, ws)
			items = append(items, info)
		}
		applyAttemptDetails(&item, attempt)
		items = append(items, item)
	}
	payload, err := json.Marshal(items)
	if err != nil {
		s.logger.Warn("dataplane.provider_chain_encode_failed", map[string]any{"error": err.Error()})
		return nil
	}
	return payload
}

// attemptChainItem 构造尝试期条目的**静态部分**：身份、选路快照、端点与时间戳。
//
// 结局（reason / statusCode / errorMessage / 重定向细节）不由它写：那是每次尝试各自的结局，
// 由调用方按条目性质分别填——WS 信息性条目与尝试条目共用静态部分，但结局不同。
func (s *storeSettler) attemptChainItem(attempt forward.AttemptOutcome, index int) route.ChainItem {
	item := route.ChainItem{
		ID:            attempt.ProviderID,
		Name:          attempt.ProviderName,
		AttemptNumber: attempt.Attempt,
	}
	if captured, ok := s.state.selectionFor(attempt.ProviderID); ok && captured.Provider != nil {
		// 取选路留痕**只为静态配置快照**（身份、优先级、权重、倍率、分组标签、端点快照）：
		// 尝试期条目的 reason 记的是「这次尝试的结局」，而选路留痕里的 reason/method 是
		// 「怎么被选中的」——那属于链首，不属尝试条目，故在此清掉。
		item = captured.ChainItem()
		item.Reason = ""
		item.SelectionMethod = ""
		item.Affinity = nil
		item.DecisionContext = nil
		item.AttemptNumber = attempt.Attempt
	}
	if attempt.EndpointID != 0 {
		endpointID := attempt.EndpointID
		item.EndpointID = &endpointID
	}
	item.EndpointURL = attempt.EndpointURL
	if item.Timestamp == 0 {
		item.Timestamp = s.state.StartedAt.Add(time.Duration(index) * time.Millisecond).UnixMilli()
	}
	return item
}

// wsChainReason 给出 WS 信息性条目的 reason 词（两个词均取自冻结的 Node 词表）。
func wsChainReason(ws *forward.AttemptWSFacts) string {
	if ws.DowngradedToHTTP {
		return forward.ReasonResponsesWSFallback
	}
	return forward.ReasonResponsesWSAttempted
}

// applyWSAttemptDetails 把上游 WS 事实写到链项上。无值时**不写键**
// （Node 侧是 `undefined`，序列化后该键不存在；写 false 会让界面把「压根没走这条传输」
// 渲染成「走了但没连上」）。
func applyWSAttemptDetails(item *route.ChainItem, ws *forward.AttemptWSFacts) {
	item.ClientTransport = ws.ClientTransport
	if ws.Attempted {
		attempted := true
		item.UpstreamWSAttempted = &attempted
	}
	if ws.Connected {
		connected := true
		item.UpstreamWSConnected = &connected
	}
	if ws.DowngradedToHTTP {
		downgraded := true
		item.DowngradedToHTTP = &downgraded
	}
	item.DowngradeReason = ws.DowngradeReason
}

// applyAttemptDetails 把一次尝试的结局细节补到链项上（Node 的 addProviderToChain 就写在这三处）。
//
// 为何在这里补而不是在选择期填：选路留痕（`captured.ChainItem()`）描述的是「怎么被选中的」，
// 它天然**没有** HTTP 状态码与上游报错；这两类事实只在尝试结束后才有。Node 的选路类条目
// （initial_selection / session_reuse / affinity_hit）也同样不写这三个键。
//
// 无值时**不写键**（而非写 0/空串）：Node 侧是 undefined，序列化后键不存在；写 0 会让前端把
// 「本轮没发生 HTTP 交换」渲染成「HTTP 0」。
func applyAttemptDetails(item *route.ChainItem, attempt forward.AttemptOutcome) {
	if attempt.StatusCode > 0 {
		status := attempt.StatusCode
		item.StatusCode = &status
	}
	if attempt.Message != "" {
		item.ErrorMessage = attempt.Message
	}
	if redirect := attempt.ModelRedirect; redirect != nil {
		item.ModelRedirect = &route.ChainModelRedirect{
			OriginalModel:   redirect.OriginalModel,
			RedirectedModel: redirect.RedirectedModel,
			BillingModel:    redirect.BillingModel,
			// matchedRule 恒随重定向一起落库：Node 的 redirectInfo 字面量里就带它，
			// 且规则源模式（source）是判断「哪条规则命中」的唯一凭据。
			MatchedRule: &route.ChainModelRedirectRule{
				MatchType: redirect.MatchType,
				Source:    redirect.Source,
				Target:    redirect.Target,
			},
		}
	}
}

// selectionChainEntry 取出守链的选路留痕，并按 Node 的条件决定它该不该当链首。
//
// Node 的两条支路（provider-selector.ts）：
//   - affinity_hit / session_reuse：在**选择时**写入，故无条件保留（即使后续重试、竞速换家，
//     链首仍然是「第一次是怎么选的」——生产样本形如 affinity_hit -> retry_failed -> hedge_launched）；
//   - initial_selection：写在**首尝试成功**的支路上（`attemptCount === 1`），失败重试后的成功
//     不再补该条目（Node 原注释：否则决策链看起来全部是加权随机初选）。
//
// 留痕缺失时返回 false，调用方跳过链首，而不是编造一条。
func (s *storeSettler) selectionChainEntry(attempts []forward.AttemptOutcome) (route.ChainItem, bool) {
	if s == nil || s.state == nil || s.state.PC == nil {
		return route.ChainItem{}, false
	}
	raw, ok := s.state.PC.SelectionChainEntry()
	if !ok {
		return route.ChainItem{}, false
	}
	var entry route.ChainItem
	if err := json.Unmarshal(raw, &entry); err != nil {
		// 留痕是自产自读的 JSON；解不动说明两侧版本不匹配，记一条告警后跳过。
		s.logger.Warn("dataplane.selection_chain_entry_undecodable", map[string]any{"error": err.Error()})
		return route.ChainItem{}, false
	}
	if entry.Reason == string(route.ReasonSelectedInitial) && !firstAttemptSucceeded(attempts) {
		return route.ChainItem{}, false
	}
	return entry, true
}

// firstAttemptSucceeded 报告首次尝试是否成功（对应 Node 的 `attemptCount === 1` 成功支路）。
func firstAttemptSucceeded(attempts []forward.AttemptOutcome) bool {
	if len(attempts) == 0 {
		return false
	}
	return attempts[0].Reason == forward.ReasonRequestSuccess
}

// settle 执行一次终态写入。
//
// 行标识来自 pctx（守卫链的 messageContext 步骤开的行）：没有标识时按 store 的语义返回
// ErrNoRow，即「本次请求不记账」——本层绝不自行补一行（那会与 Node 的账本口径分叉）。
func (s *storeSettler) settle(ctx context.Context, pc *pctx.Context, settlement terminal.Settlement) error {
	if _, ok := pc.MessageRequestID(); !ok {
		s.logger.Warn("dataplane.settle_skipped", map[string]any{"reason": "no request row"})
		return nil
	}
	result, err := s.settler.SettleContext(ctx, pc, settlement, nil)
	if err != nil {
		return fmt.Errorf("dataplane: 终态结算失败: %w", err)
	}
	s.logSettleNotCommitted(result)
	return nil
}

// logSettleNotCommitted 在「既未提交、也未入队」时留一条 debug。
//
// 为什么必须一并排除 Queued：异步写模式（MESSAGE_REQUEST_WRITE_MODE=async）下入队成功即返回
// `Result{Queued:true}`，而 `Committed` 此时是零值 false——提交结论只有队列 flush 之后才有
// （见 terminal.Result 与 terminal.SettleContext 的注释）。只看 Committed 会让**每个请求**
// 都记一条「未提交」（生产实测 87 行 / 84 请求、attempts 恒 0），把真失败淹掉。
// 入队之后的真失败由队列自报（terminal_async_flush_failed，带 error 与 cost_gap）。
func (s *storeSettler) logSettleNotCommitted(result terminal.Result) {
	if result.Committed || result.Queued {
		return
	}
	s.logger.Debug("dataplane.settle_not_committed", map[string]any{"attempts": result.Attempts})
}

// nonStreamDuration 给出「请求进入数据面 → 上游正文读完」的耗时。
//
// 为什么起点不用 result.StartedAt：那是**本次尝试**的开始时刻，不含守卫链与选路的耗时；
// 流式路径与 Node 都从请求开始计（见 Stream 与 response-handler.ts 的 session.startTime）。
// 两条终态缝用不同起点，会让同一批请求的 duration_ms 列不可比。
func (s *storeSettler) nonStreamDuration(result *forward.Result) *int {
	if s.state.StartedAt.IsZero() {
		return nil
	}
	ended := nowOr(s.now)
	if result != nil && !result.EndedAt.IsZero() {
		ended = result.EndedAt
	}
	duration := int(ended.Sub(s.state.StartedAt).Milliseconds())
	if duration < 0 {
		// 时钟回拨：宁可记 0，也不写负耗时（负值会污染统计口径）。
		duration = 0
	}
	return &duration
}

// nonStreamFacts 从非流式上游正文里取用量与上游回报的实际模型名。
//
// 按**上游实际协议线**解码（result.Plan.Protocol：转换生效时为目标线，否则为客户端线），
// 复用三线解码器里的 usageFrom* 与模型字段——口径与流式观测器同源，不另造一套判定。
// 正文缺失、不是合法 JSON、或正文里没有用量对象时用量返回 nil：绝不按 0 写行，
// 否则「上游没报」与「真的零用量」会混成同一列。
func nonStreamFacts(plan *forward.Plan, body []byte) (*convert.Usage, string) {
	if plan == nil || len(body) == 0 {
		return nil, ""
	}
	value, err := convert.ParseJSON(body)
	if err != nil {
		return nil, ""
	}
	decoded, ok := convert.DecodeResponse(plan.Protocol, value, convert.ConvertCtx{})
	if !ok || decoded.Value == nil {
		return nil, ""
	}
	usage := decoded.Value.Usage
	if usage.IsEmpty() {
		// 解码器对「无 usage 对象」与「usage 里全是 null」都给出非 nil 的空结构，
		// 故以 IsEmpty 判定，而不是非 nil。
		usage = nil
	}
	return usage, decoded.Value.Model
}

// usageFromConvert 把枢纽用量投影成落库口径。
//
// 枢纽用量是 float64（各协议线的上游用量并不都是整数），落库列是 bigint：一律就近截断。
// 缺口如实记录：cache_creation 的 5m/1h 拆分来自 Anthropic 的 usage 明细，观测器只保留
// 总量，故这里只填 cache_creation_input_tokens（拆分留给 usage 归一化波次）。
func usageFromConvert(usage convert.Usage) terminal.Usage {
	out := terminal.Usage{}
	if usage.InputTokens != nil {
		value := int64(*usage.InputTokens)
		out.InputTokens = &value
	}
	if usage.OutputTokens != nil {
		value := int64(*usage.OutputTokens)
		out.OutputTokens = &value
	}
	if usage.CacheReadTokens != nil {
		value := int64(*usage.CacheReadTokens)
		out.CacheReadInputTokens = &value
	}
	if usage.CacheWriteTokens != nil {
		value := int64(*usage.CacheWriteTokens)
		out.CacheCreationInputTokens = &value
	}
	return out
}

// numericToFloat 把 numeric 列的 json.Number 投影成倍率浮点。
func numericToFloat(value json.Number) *float64 {
	if value.String() == "" {
		return nil
	}
	parsed, err := strconv.ParseFloat(value.String(), 64)
	if err != nil {
		return nil
	}
	return &parsed
}

// ErrNoStore 表示没有数据库时无法装配数据面。
var ErrNoStore = errors.New("dataplane: 需要 store.Pools")
