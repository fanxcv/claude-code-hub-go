package route

import (
	"context"
	"slices"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// EndpointContext 是本次请求在端点维度上的上下文（对应 Node 的 ConversionEndpointContext）。
type EndpointContext struct {
	// Pathname 是入站端点路径；为空时不做端点维度判定（保持与 Node 缺省一致的宽松语义）。
	Pathname string
	// MultipartImageRequest 表示 multipart 图片请求：body 由图片兼容层独占处理，跨协议一律不可服务。
	MultipartImageRequest bool
}

// Gates 是三个「本包不实现判定、只留钩子」的维度。
//
// 为什么是钩子而不是实现：调度时间窗口列、客户端识别（client-detector）与金额/并发限额
// 分别依赖 store 尚未暴露的列与尚未移植的模块；在选路里凭空实现一套不同的判定，会让
// 「谁决定了这次排除」失去唯一答案。nil 表示该维度不判定（不产出对应理由）。
//
// **判定发生的步骤以 Node 为准**（`provider-selector.ts` 的 pickRandomProvider）：
//   - Schedule / Client 属 **Step 1–2**（分组 → 客户端限制 → 基础过滤里的调度窗口）；
//   - Limits 属 **Step 4**（`filterByLimits`：vendor-type 熔断 → 供应商熔断 → 金额限额 →
//     总额度）。因此 Limits **不在** basicFilterRejection 里判定——放到那里会让
//     enabledProviders / afterGroupFilter 的口径与 Node 分叉（Node 把超限供应商算作
//     「已启用但不健康」，`filteredProviders[].reason` 记 `rate_limited`）。
//
// 需要**请求级**输入的两个维度（调度窗口要看系统时区下的「现在」、客户端限制要看本请求
// 的 UA/头部/正文）走 `Request.ScheduleGate` / `Request.ClientGate`；这里的 Gates 字段
// 是选择器级兼容面（管理面模拟器等无请求上下文的调用方用）。
type Gates struct {
	// Schedule 返回 false 即判定为不在调度时间窗口内（ReasonScheduleInactive）。
	Schedule func(p Provider) bool
	// Client 返回 allowed=false 即判定为客户端限制（ReasonClientRestriction）。
	Client func(p Provider) (allowed bool, details string)
	// Limits 返回 allowed=false 即判定为限额已达（ReasonRateLimited）；在 **Step 4** 判定。
	Limits func(ctx context.Context, p Provider) (allowed bool, details string)
}

// filterInput 是一次候选过滤的全部输入。
type filterInput struct {
	requestedModel string
	format         convert.ClientFormat
	group          string
	excluded       map[int64]bool
	endpoints      EndpointContext
	endpointGate   bool
	gates          Gates
	// scheduleGate / clientGate 是本次请求的判定（非 nil 时优先于 gates 的同名钩子）。
	scheduleGate func(p Provider) bool
	clientGate   func(p Provider) *ClientRestriction
	// cooldown 是本次请求的「本会话冷却中」渠道及其成因（nil 即不判定，回落到只有渠道级降权）。
	// 成因必须带过来：同一个冷却键有两个写入者（故障回避 / 低速降权），两者记的理由不同，
	// 而「渠道故障」不该在链上被写成「渠道慢」（见 cooldownRejection）。
	cooldown map[int64]CooldownKind
	// quarantined 是本次「因低速隔离而不该参与竞争」的候选集合（值为真即排除）。
	//
	// 判定（准入档位 + 按请求的准入闸门）在 resolve 里一次算好再传进来：applyFilters 是纯过滤，
	// 不该自己拿时钟与请求身份去掷骰子（那会让同一次选路在不同调用处得出不同结论）。
	quarantined map[int64]bool
}

// filterResult 是候选过滤的结果与留痕。
type filterResult struct {
	healthy []Provider
	context DecisionContext
	// quarantined 是**仅因低速隔离**被排除的候选（与 healthy 互斥）。
	//
	// 为什么要单独留一份：探针租约的消费者需要「被隔离的那几家」这个集合——它是唯一能
	// 把请求定向回去的入口。从 context.FilteredProviders 反推需要再走一遍理由匹配，
	// 而那里已经过软信号 fail-open 的改写（理由会被改成 no_alternative_fail_open）。
	quarantined []Provider
}

// applyFilters 复刻 pickRandomProvider 的过滤顺序与留痕口径。
//
// 口径要点（与 Node 逐条对齐，勿改）：
//   - groupFilterApplied 为真时 totalProviders 记的是「分组过滤后」的数量；
//   - enabledProviders 记的是「基础过滤（含客户端限制）后」的数量，beforeHealthCheck 与之相等；
//   - afterHealthCheck 记的是「熔断过滤后」的数量；
//   - 每个被排除项都要有一条带枚举理由的记录。
func (s *Selector) applyFilters(
	ctx context.Context,
	providers []Provider,
	in filterInput,
) filterResult {
	dc := DecisionContext{
		TargetType:           TargetTypeOf(in.format),
		RequestedModel:       in.requestedModel,
		PriorityLevels:       []int{},
		CandidatesAtPriority: []Candidate{},
		FilteredProviders:    []Filtered{},
	}
	if in.group != "" {
		dc.UserGroup = in.group
		dc.GroupFilterApplied = true
	}
	if len(in.excluded) > 0 {
		dc.ExcludedProviderIDs = sortedIDs(in.excluded)
	}

	if in.group != "" {
		groupFiltered := make([]Provider, 0, len(providers))
		for _, p := range providers {
			if checkProviderGroupMatch(p.GroupTag, in.group) {
				groupFiltered = append(groupFiltered, p)
			}
		}
		if len(groupFiltered) == 0 {
			// 严格分组隔离：该分组下没有任何供应商，直接判无可用（Node 在此提前返回）。
			zero := 0
			dc.TotalProviders = 0
			dc.AfterGroupFilter = &zero
			return filterResult{context: dc}
		}
		providers = groupFiltered
	}

	dc.TotalProviders = len(providers)

	// 模型覆盖：独立于过滤顺序统计「有多少家声明支持这个模型」。
	//
	// 为何单独数一遍：下面的 basicFilterRejection 是**逐家短路**的（先命中即返回），
	// 所以「模型不被支持」会被「格式不兼容」「熔断」等先序理由盖住，数字就不可信了。
	// 这里用**同一个** providerSupportsModel 纯函数（无 I/O）单独数，
	// 与真正的过滤共用一套判定，不会分叉。
	if in.requestedModel != "" {
		supported := 0
		for _, p := range providers {
			if providerSupportsModel(p, in.requestedModel) {
				supported++
			}
		}
		dc.ModelSupportedProviders = supported
	}

	visible := providers
	if in.clientGate != nil || in.gates.Client != nil {
		clientPassed := make([]Provider, 0, len(visible))
		for _, p := range visible {
			if in.clientGate != nil {
				// 请求级判定（数据面）：Node 在 pickRandomProvider 的 Step 1 里对每个供应商调
				// isClientAllowedDetailed，名单两侧都空时**不进判定**（返回 nil 即此意）。
				restriction := in.clientGate(p)
				if restriction == nil || restriction.Allowed {
					clientPassed = append(clientPassed, p)
					continue
				}
				record := Filtered{
					ID: p.ID, Name: p.Name,
					Reason:  ReasonClientRestriction,
					Details: clientReasonDetails(restriction.MatchType),
				}
				if context := restriction.context(); context != nil {
					record.ClientRestrictionContext = context
				}
				dc.FilteredProviders = append(dc.FilteredProviders, record)
				continue
			}

			allowed, details := in.gates.Client(p)
			if !allowed {
				dc.FilteredProviders = append(dc.FilteredProviders, Filtered{
					ID: p.ID, Name: p.Name, Reason: ReasonClientRestriction, Details: details,
				})
				continue
			}
			clientPassed = append(clientPassed, p)
		}
		visible = clientPassed
	}

	enabled := make([]Provider, 0, len(visible))
	for _, p := range visible {
		if blocked, record := s.basicFilterRejection(ctx, p, in); blocked {
			dc.FilteredProviders = append(dc.FilteredProviders, record)
			continue
		}
		enabled = append(enabled, p)
	}

	dc.EnabledProviders = len(enabled)
	// afterGroupFilter 在 Node 侧记的是「基础过滤后」的数量（pickRandomProvider 的 Step 3 口径），
	// 不是「分组过滤后」——名字容易误导，取值以 Node 为准。
	afterGroup := len(enabled)
	dc.AfterGroupFilter = &afterGroup
	dc.BeforeHealthCheck = len(enabled)

	healthy := make([]Provider, 0, len(enabled))
	// healthFiltered 是本步（熔断 + 会话冷却）的排除记录，**延后**并入留痕：软信号 fail-open
	// 需要回头改写其中的理由，边滤边写会让同一家既在留痕里「被排除」又无法标出回退。
	healthFiltered := make([]Filtered, 0, len(enabled))
	softRejected := make([]Provider, 0, len(enabled))
	quarantineRejected := make([]Provider, 0, len(enabled))
	hardRejected := false
	reject := func(p Provider, record Filtered) {
		healthFiltered = append(healthFiltered, record)
		if softSignalRejection(record.Reason) {
			softRejected = append(softRejected, p)
			return
		}
		hardRejected = true
	}
	for _, p := range enabled {
		if blocked, record := s.healthRejection(ctx, p); blocked {
			reject(p, record)
			continue
		}
		// 会话级低速冷却在**熔断之后**判定（软回避不应改写硬故障的归因）。
		// 它不改变 afterHealthCheck 的口径：那个计数是熔断步的产物，本判定属另一个维度。
		if blocked, record := s.cooldownRejection(p, in); blocked {
			reject(p, record)
			continue
		}
		// 渠道级低速隔离在**会话冷却之后**判定：顺序决定归因——本会话刚在这家出过事时记的是
		// 会话冷却（那件事更具体），隔离只覆盖「没有更具体的会话级理由」的其余请求。
		if in.quarantined[p.ID] {
			quarantineRejected = append(quarantineRejected, p)
			reject(p, Filtered{
				ID: p.ID, Name: p.Name,
				Reason:  ReasonSlowRateQuarantine,
				Details: string(ReasonSlowRateQuarantine),
			})
			continue
		}
		healthy = append(healthy, p)
	}
	// 软信号 fail-open：排除后一个候选都不剩、且排除原因**全部**属软信号时，重新纳入这些候选。
	//
	// 只对软信号：硬信号（熔断、故障冷却、限额）代表上游已知故障或需改配置才恢复，把它们放行
	// 只会让失败请求继续打过去。任一硬信号在场即整组不放行（含「一家冷却 + 一家熔断」的混合）。
	//
	// 为何要 `len(softRejected) > 0`：enabled 为空时本循环一次都没跑，「全部属软信号」是空真，
	// 不加这道门会把「压根没有候选」误判成「可回退」。
	if len(healthy) == 0 && len(softRejected) > 0 && !hardRejected {
		healthy = softRejected
		for index := range healthFiltered {
			if !softSignalRejection(healthFiltered[index].Reason) {
				continue
			}
			healthFiltered[index].Reason = ReasonNoAlternativeFailOpen
			healthFiltered[index].Details = string(ReasonNoAlternativeFailOpen)
		}
	}
	dc.FilteredProviders = append(dc.FilteredProviders, healthFiltered...)
	dc.AfterHealthCheck = len(healthy)

	return filterResult{healthy: healthy, context: dc, quarantined: quarantineRejected}
}

// basicFilterRejection 判定「基础过滤」维度：启用态、排除列表、调度窗口、格式兼容、
// 模型允许集、限额。理由取值与 Node 的记录循环逐条对齐。
// cooldownRejection 判定该候选是否因「本会话对它正在冷却期内」而不该参与本次竞争。
//
// 与熔断的关系：两者独立不合并（设计稿 §5 边界表）。熔断是硬故障排除（`healthRejection`），
// 本判定是「本会话刚在这家出过事」的软回避，只作用于**本会话**。因此先跑熔断、再跑本判定，
// 且本判定命中时记的是专门理由（而不是 circuit_open）。
//
// 三种成因分开记理由，因为它们是三件事：
//   - provider_error_cooldown：供应商侧真实故障（上游 5xx/超时/连接错）后的 60 秒回避，与「渠道慢不慢」无关；
//   - upstream_stream_cut_cooldown：上游在正文中途干净断流（无标记）后的 60 秒回避；
//   - slow_rate_cooldown：低速降权写下的冷却。
//
// 把后者当成前者会误报（「渠道故障」写成「渠道慢」），反之则会让故障冷却在界面上消失；
// 断流与故障冷却共用键与 TTL，不分开就只能在「硬/软」二选一里牺牲一侧。
//
// 只在 Options.SlowRate 已装配且本次请求带会话身份时判定；in.cooldown 为 nil 时直接返回。
func (s *Selector) cooldownRejection(p Provider, in filterInput) (bool, Filtered) {
	record := Filtered{ID: p.ID, Name: p.Name}
	kind, ok := in.cooldown[p.ID]
	if !ok {
		return false, record
	}
	record.Reason, record.Details = cooldownReason(kind)
	return true, record
}

// softSignalRejection 报告某条排除理由是否属**软信号**：本网关按**本会话**加的、等窗口过去
// 即自愈的回避（不代表上游持续故障）。
//
// 分界原则与 transientRejection 同源但不共用：那条回答「绑定要不要留着等它回来」，本条回答
// 「无替代候选时能不能把它放行」。逐条依据：
//
//	slow_rate_cooldown          低速降权写下的本会话回避，等窗口过去即自愈，非上游故障
//	slow_rate_quarantine        渠道级低速隔离：有替代时让开，无替代时必须放行（否则可用性更差）
//	upstream_stream_cut_cooldown 上游偶发的收尾瑕疵（正文中途干净 FIN）后的本会话回避：
//	                            它不是渠道持续故障的证据，无替代候选时必须放行
//	provider_error_cooldown     上游 5xx/超时后的回避：上游确实出过事，放行等于继续打过去
//	circuit_open / rate_limited 硬故障与限额，需改配置或等窗口，放行会让失败请求继续
func softSignalRejection(reason Reason) bool {
	return reason == ReasonSlowRateCooldown ||
		reason == ReasonSlowRateQuarantine ||
		reason == ReasonUpstreamStreamCutCooldown
}

// cooldownReason 把冷却成因折算成过滤理由与 i18n 键。
//
// Details 取 i18n 键形态（与 circuit_open / rate_limited 等同例）：前端先按 filterDetails.<值>
// 查词表，查不到才回落原值。写死中文会让英文界面露出中文，违反「用户可见文案走 i18n」。
// 两个取值同名（理由与详情逐字一致），是因为它们本就是一回事；分开取名只会多一处可漂移的映射。
func cooldownReason(kind CooldownKind) (Reason, string) {
	switch kind {
	case CooldownSlowRate:
		return ReasonSlowRateCooldown, string(ReasonSlowRateCooldown)
	case CooldownUpstreamStreamCut:
		return ReasonUpstreamStreamCutCooldown, string(ReasonUpstreamStreamCutCooldown)
	}
	return ReasonProviderErrorCooldown, string(ReasonProviderErrorCooldown)
}

func (s *Selector) basicFilterRejection(
	ctx context.Context,
	p Provider,
	in filterInput,
) (bool, Filtered) {
	record := Filtered{ID: p.ID, Name: p.Name}

	switch {
	case !p.IsEnabled:
		record.Reason = ReasonDisabled
		record.Details = "供应商已禁用"
		return true, record
	case in.excluded[p.ID]:
		record.Reason = ReasonExcluded
		record.Details = "已在前序尝试中失败"
		return true, record
	case excludedSchedule(in, p):
		record.Reason = ReasonScheduleInactive
		record.Details = scheduleInactiveDetails(p)
		return true, record
	}

	if in.format != "" {
		compat := convert.ResolveProtocolCompat(in.format, p.ProviderType, p.ConversionEnabled())
		served := compat != convert.CompatIncompatible
		if compat == convert.CompatConvertible {
			served = convertibleEndpointServed(in.format, p, in.endpoints)
		}
		if !served {
			if !p.ConversionEnabled() &&
				convert.ResolveProtocolCompat(in.format, p.ProviderType, true) == convert.CompatConvertible {
				// 本可跨协议转换，但该供应商开关未开：记专门理由，便于用户自查（设计 §8.2）。
				record.Reason = ReasonProtocolConversionDisabled
				record.Details = "原始格式与供应商类型跨协议，但该供应商未开启协议转换"
				return true, record
			}
			record.Reason = ReasonFormatTypeMismatch
			record.Details = "原始格式与供应商类型不兼容"
			return true, record
		}
	}

	if in.requestedModel != "" && !providerSupportsModel(p, in.requestedModel) {
		record.Reason = ReasonModelNotAllowed
		record.Details = "不支持模型 " + in.requestedModel
		return true, record
	}

	if in.endpointGate && p.ProviderVendorID != nil && *p.ProviderVendorID > 0 {
		endpoints, err := s.opts.Source.Endpoints(ctx, *p.ProviderVendorID, p.ProviderType)
		if err == nil && len(endpoints) == 0 {
			// 与 Node 的差异：Node 在 forwarder 判端点池（endpoint_pool_exhausted），
			// 本包把「厂下无任何启用端点」提前到候选期。默认关闭，见 Options.EndpointGate。
			record.Reason = ReasonEndpointUnavailable
			record.Details = "供应商厂下无启用端点"
			return true, record
		}
	}

	return false, record
}

// healthRejection 判定「健康度过滤」维度（Node 的 Step 4 `filterByLimits`）：
// vendor-type 熔断 → 供应商熔断 → **金额限额**（含总额度）。顺序与理由取值逐条对齐 Node：
// Node 在记录阶段重查两种熔断，两者都不成立时统一记 `rate_limited` + details `rate_limited`。
func (s *Selector) healthRejection(ctx context.Context, p Provider) (bool, Filtered) {
	record := Filtered{ID: p.ID, Name: p.Name}
	if s.opts.Health == nil && s.opts.Gates.Limits == nil {
		return false, record
	}

	if s.opts.Health != nil {
		if p.ProviderVendorID != nil && *p.ProviderVendorID > 0 &&
			s.opts.Health.VendorTypeOpen(ctx, *p.ProviderVendorID, p.ProviderType) {
			record.Reason = ReasonCircuitOpen
			record.Details = "vendor_type_circuit_open"
			return true, record
		}

		open, _ := s.opts.Health.ProviderOpen(ctx, p.ID)
		if open {
			record.Reason = ReasonCircuitOpen
			if s.opts.Health.ProviderState(ctx, p.ID) == StateOpen {
				record.Details = "circuit_open"
			} else {
				record.Details = "circuit_half_open"
			}
			return true, record
		}
	}

	// 金额限额在熔断之后判定（Node 的 filterByLimits 顺序）。
	if s.opts.Gates.Limits != nil {
		if allowed, _ := s.opts.Gates.Limits(ctx, p); !allowed {
			record.Reason = ReasonRateLimited
			// Node 在此分支不区分具体窗口，details 恒为 `rate_limited`（provider-selector.ts:1441）。
			record.Details = "rate_limited"
			return true, record
		}
	}

	return false, record
}

// excludedSchedule 判定调度窗口：请求级钩子优先（数据面），否则用选择器级钩子（模拟器等）。
func excludedSchedule(in filterInput, p Provider) bool {
	if in.scheduleGate != nil {
		return !in.scheduleGate(p)
	}
	if in.gates.Schedule != nil {
		return !in.gates.Schedule(p)
	}
	return false
}

// scheduleInactiveDetails 复刻 Node 的理由文案：`outside active window ${start}-${end}`
// （provider-selector.ts:1356）。两个值在此分支必非 nil：任一端为 nil 时判定为「恒活跃」，
// 不会走到这里；仍按 JS 模板语义把 nil 渲染成 `null`，免得边界情况出现空串。
func scheduleInactiveDetails(p Provider) string {
	return "outside active window " + clockText(p.ActiveTimeStart) + "-" + clockText(p.ActiveTimeEnd)
}

func clockText(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

// convertibleEndpointServed 复刻 canServeConvertibleEndpoint（仅跨协议时调用）：
// 端点路径为空时不判定；multipart 图片请求一律不可服务；其余要求路径能映射到目标协议线。
func convertibleEndpointServed(
	format convert.ClientFormat,
	p Provider,
	endpoints EndpointContext,
) bool {
	if endpoints.Pathname == "" {
		return true
	}
	if endpoints.MultipartImageRequest {
		return false
	}
	target, ok := convert.ResolveTargetProtocol(format, p.ProviderType)
	if !ok {
		return false
	}
	_, mapped := convert.ResolveUpstreamPath(target, endpoints.Pathname)
	return mapped
}

// TargetTypeOf 复刻 Node 由 originalFormat 推断目标供应商类型的映射；
// 未知格式回退 claude（向后兼容），与 Node 的 default 分支一致。
func TargetTypeOf(format convert.ClientFormat) TargetType {
	switch format {
	case convert.FormatClaude:
		return TargetClaude
	case convert.FormatResponse:
		return TargetCodex
	case convert.FormatOpenAI:
		return TargetOpenAICompatible
	case convert.FormatGemini:
		return TargetGemini
	case convert.FormatGeminiCLI:
		return TargetGeminiCLI
	default:
		return TargetClaude
	}
}

func sortedIDs(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}
