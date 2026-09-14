package route

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件是 `POST /api/v1/dashboard/dispatch-simulator:simulate` 的引擎：
// 复刻 Node 的 `src/actions/dispatch-simulator.ts`（`simulateDispatchDecisionTree`）——
// 按 clientFormat / modelName / groupTags 走一遍选路的**每一轮**，并输出每轮的存活与淘汰原因。
//
// 为什么放在 `route` 包而不是管理面：九个步骤里的谓词（分组匹配、有效优先级、模型允许集、
// 协议兼容）都是本包未导出的**唯一实现**。放在管理面就得把它们再导出一次或抄一份，
// 那正是本仓一直在避免的漂移源（见 rules.go 里 matchProviderGroups 的注释）。
//
// 与真实选路的两处**有意差异**（Node 同样如此，逐字对齐）：
//  1. 只建模「格式」维度，不建模「端点」维度：模拟器一律按「端点未如」处理
//     （Node 的注释：这类请求下模拟器会比真实选路多报可选供应商）；
//  2. `formatCompatibility` 不带 endpointContext，故跨协议组合一律放行
//     （Node 的 canServeConvertibleEndpoint 在无 clientPathname 时返回 true）。

// SimulateEndpointStats 是端点池统计，对应前端 DispatchSimulatorEndpointStats。
type SimulateEndpointStats struct {
	Total       int `json:"total"`
	Enabled     int `json:"enabled"`
	CircuitOpen int `json:"circuitOpen"`
	Available   int `json:"available"`
}

// SimulateProviderSnapshot 对应前端 DispatchSimulatorProviderSnapshot。
//
// 字段可空性与 Node 逐字对齐：`groupTag` **恒出现**（可为 null），
// `details` / `redirectedModel` / `endpointStats` 仅在提供时出现（Node 的 undefined 会被
// JSON.stringify 丢掉）。
type SimulateProviderSnapshot struct {
	ID                int64                  `json:"id"`
	Name              string                 `json:"name"`
	ProviderType      convert.ProviderType   `json:"providerType"`
	GroupTag          *string                `json:"groupTag"`
	Priority          int                    `json:"priority"`
	EffectivePriority int                    `json:"effectivePriority"`
	Weight            int                    `json:"weight"`
	Details           *string                `json:"details,omitempty"`
	RedirectedModel   *string                `json:"redirectedModel,omitempty"`
	EndpointStats     *SimulateEndpointStats `json:"endpointStats,omitempty"`
}

// SimulateStep 对应前端 DispatchSimulatorStep。
type SimulateStep struct {
	StepName    string                     `json:"stepName"`
	StepIndex   int                        `json:"stepIndex"`
	InputCount  int                        `json:"inputCount"`
	OutputCount int                        `json:"outputCount"`
	FilteredOut []SimulateProviderSnapshot `json:"filteredOut"`
	Surviving   []SimulateProviderSnapshot `json:"surviving"`
	Note        *string                    `json:"note,omitempty"`
}

// SimulatePriorityProvider 对应前端 DispatchSimulatorPriorityProvider。
type SimulatePriorityProvider struct {
	SimulateProviderSnapshot
	WeightPercent float64 `json:"weightPercent"`
}

// SimulatePriorityTier 对应前端 DispatchSimulatorPriorityTier。
type SimulatePriorityTier struct {
	Priority   int                        `json:"priority"`
	Providers  []SimulatePriorityProvider `json:"providers"`
	IsSelected bool                       `json:"isSelected"`
}

// SimulateResult 对应前端 DispatchSimulatorResult。
type SimulateResult struct {
	Steps               []SimulateStep         `json:"steps"`
	PriorityTiers       []SimulatePriorityTier `json:"priorityTiers"`
	TotalProviders      int                    `json:"totalProviders"`
	FinalCandidateCount int                    `json:"finalCandidateCount"`
	SelectedPriority    *int                   `json:"selectedPriority"`
}

// SimulateProvider 是选路视角的供应商 + 模拟器额外需要的两列。
//
// `Provider` 里的字段集是数据面选路的必需列；模拟器还要活动时段（Node 的 activeTimeStart/End）
// 与模型重定向规则（model_redirects），而这两列数据面选路当前不读（见报告里的缺口记录）。
type SimulateProvider struct {
	Provider
	ActiveTimeStart *string         `json:"active_time_start"`
	ActiveTimeEnd   *string         `json:"active_time_end"`
	ModelRedirects  json.RawMessage `json:"model_redirects"`
}

// SimulateOptions 是一次模拟的全部输入。
//
// 所有外部数据源都以回调注入：`route` 包不依赖 store / Redis / 限额服务，
// 测试可用假实现逐轮断言，而生产装配由管理面把真实读数接进来。
type SimulateOptions struct {
	// Format 是客户端入站格式；ModelName 为空表示资源类请求（跳过模型允许集与重定向预览）。
	Format    convert.ClientFormat
	ModelName string
	// GroupTags 是用户分组标签；为空时退化为 ["default"]（Node 的 getGroupFilterValue）。
	GroupTags []string
	// Now 必须已经是目标时区的时刻（与 route.ProviderActiveNow 同一约定）。
	Now time.Time
	// Providers 是**全量**供应商（含禁用态）：Node 用 findAllProvidersFresh()，
	// 禁用态由 enabledCheck 这一步淘汰，故不能先按启用态过滤。
	Providers []SimulateProvider

	// VendorTypeCircuit 判定厂级熔断是否打开（Node 的 isVendorTypeCircuitOpen）。
	VendorTypeCircuit func(ctx context.Context, p SimulateProvider) bool
	// ProviderCircuit 判定供应商级熔断，并给出状态文本（Node 的 getCircuitState，如 "open"）。
	ProviderCircuit func(ctx context.Context, p SimulateProvider) (bool, string)
	// ProviderCostAllowed 是只读的周期限额判定（Node 的 checkCostLimits(id, "provider", …)）。
	ProviderCostAllowed func(ctx context.Context, p SimulateProvider) (bool, string)
	// TotalCostAllowed 是只读的总额度判定（Node 的 checkTotalCostLimit）。
	TotalCostAllowed func(ctx context.Context, p SimulateProvider) (bool, string)
	// EndpointStats 取端点池统计；返回 nil 表示该供应商不参与端点维度（无 vendor）。
	EndpointStats func(ctx context.Context, p SimulateProvider) *SimulateEndpointStats
}

// Simulate 跑一遍调度模拟，返回与前端契约同形的结果。
func Simulate(ctx context.Context, opts SimulateOptions) (SimulateResult, error) {
	modelName := strings.TrimSpace(opts.ModelName)
	groupFilter := simulateGroupFilter(opts.GroupTags)
	steps := make([]SimulateStep, 0, 9)

	current := opts.Providers

	// 第 1 步：分组过滤。
	groupKept := filterProviders(current, func(p SimulateProvider) bool {
		return checkProviderGroupMatch(p.GroupTag, groupFilter)
	})
	steps = append(steps, buildSimulateStep(
		"groupFilter", 1, current, groupKept,
		snapshotsExcept(current, groupKept, func(p SimulateProvider) *string {
			return stringPtr("provider_group_mismatch")
		}, groupFilter, nil, nil),
		groupFilter, nil,
	))
	current = groupKept

	// 第 2 步：格式兼容（不建模端点维度，见文件头）。
	formatKept := filterProviders(current, func(p SimulateProvider) bool {
		return simulateFormatCompatible(opts.Format, p.Provider)
	})
	steps = append(steps, buildSimulateStep(
		"formatCompatibility", 2, current, formatKept,
		snapshotsExcept(current, formatKept, func(p SimulateProvider) *string {
			return stringPtr("format " + string(opts.Format) + " incompatible with " + string(p.ProviderType))
		}, groupFilter, nil, nil),
		groupFilter, nil,
	))
	current = formatKept

	// 第 3 步：启用态。
	enabledKept := filterProviders(current, func(p SimulateProvider) bool { return p.IsEnabled })
	steps = append(steps, buildSimulateStep(
		"enabledCheck", 3, current, enabledKept,
		snapshotsExcept(current, enabledKept, func(SimulateProvider) *string {
			return stringPtr("provider_disabled")
		}, groupFilter, nil, nil),
		groupFilter, nil,
	))
	current = enabledKept

	// 第 4 步：活动时段。
	activeKept := filterProviders(current, func(p SimulateProvider) bool {
		return ProviderActiveNow(p.ActiveTimeStart, p.ActiveTimeEnd, opts.Now)
	})
	steps = append(steps, buildSimulateStep(
		"activeTime", 4, current, activeKept,
		snapshotsExcept(current, activeKept, func(p SimulateProvider) *string {
			return stringPtr("outside active window " + dashIfNil(p.ActiveTimeStart) + "-" + dashIfNil(p.ActiveTimeEnd))
		}, groupFilter, nil, nil),
		groupFilter, nil,
	))
	current = activeKept

	// 第 5 步：模型允许集（模型名为空时跳过，与 Node 的 note 一致）。
	allowKept := current
	var allowNote *string
	if modelName == "" {
		allowNote = stringPtr("model_filter_skipped_for_resource_request")
	} else {
		allowKept = filterProviders(current, func(p SimulateProvider) bool {
			return providerSupportsModel(p.Provider, modelName)
		})
	}
	var allowFiltered []SimulateProviderSnapshot
	if modelName != "" {
		allowFiltered = snapshotsExcept(current, allowKept, func(SimulateProvider) *string {
			return stringPtr("model " + modelName + " did not match allowlist")
		}, groupFilter, nil, nil)
	}
	steps = append(steps, buildSimulateStep(
		"modelAllowlist", 5, current, allowKept, allowFiltered, groupFilter, allowNote,
	))
	current = allowKept

	// 第 6 步：熔断与限额（厂级熔断 → 供应商熔断 → 周期限额 → 总额度，顺序与 Node 一致）。
	healthy := make([]SimulateProvider, 0, len(current))
	healthFiltered := make([]SimulateProviderSnapshot, 0)
	for _, p := range current {
		if p.ProviderVendorID != nil && *p.ProviderVendorID > 0 &&
			opts.VendorTypeCircuit != nil && opts.VendorTypeCircuit(ctx, p) {
			healthFiltered = append(healthFiltered, snapshotOf(p, groupFilter, stringPtr("vendor_type_circuit_open"), nil, nil))
			continue
		}
		if opts.ProviderCircuit != nil {
			if open, state := opts.ProviderCircuit(ctx, p); open {
				healthFiltered = append(healthFiltered, snapshotOf(p, groupFilter, stringPtr("provider_circuit_"+state), nil, nil))
				continue
			}
		}
		if opts.ProviderCostAllowed != nil {
			allowed, reason := opts.ProviderCostAllowed(ctx, p)
			if !allowed {
				healthFiltered = append(healthFiltered, snapshotOf(p, groupFilter, stringPtr(reason), nil, nil))
				continue
			}
		}
		if opts.TotalCostAllowed != nil {
			allowed, reason := opts.TotalCostAllowed(ctx, p)
			if !allowed {
				healthFiltered = append(healthFiltered, snapshotOf(p, groupFilter, stringPtr(reason), nil, nil))
				continue
			}
		}
		healthy = append(healthy, p)
	}
	steps = append(steps, SimulateStep{
		StepName: "healthAndLimits", StepIndex: 6,
		InputCount: len(current), OutputCount: len(healthy),
		FilteredOut: healthFiltered,
		Surviving:   snapshotsOf(healthy, groupFilter, nil, nil),
	})
	current = healthy

	// 第 7 步：优先级梯队（只保留最小有效优先级那一档）。
	tiers := buildSimulatePriorityTiers(current, groupFilter, modelName, opts.EndpointStats == nil)
	selectedTier := -1
	for i := range tiers {
		if tiers[i].IsSelected {
			selectedTier = i
			break
		}
	}
	selectedIDs := make(map[int64]bool)
	selectedProviders := make([]SimulateProvider, 0)
	if selectedTier >= 0 {
		for _, item := range tiers[selectedTier].Providers {
			selectedIDs[item.ID] = true
		}
		for _, p := range current {
			if selectedIDs[p.ID] {
				selectedProviders = append(selectedProviders, p)
			}
		}
	}
	tierFiltered := make([]SimulateProviderSnapshot, 0)
	for _, p := range current {
		if selectedIDs[p.ID] {
			continue
		}
		tierFiltered = append(tierFiltered, snapshotOf(
			p, groupFilter,
			stringPtr("effective_priority="+strconv.Itoa(resolveEffectivePriority(p.Provider, groupFilter))),
			redirectedModelPtr(modelName, p), nil,
		))
	}
	steps = append(steps, SimulateStep{
		StepName: "priorityTiers", StepIndex: 7,
		InputCount: len(current), OutputCount: len(selectedProviders),
		FilteredOut: tierFiltered,
		Surviving:   snapshotsOf(selectedProviders, groupFilter, nil, func(p SimulateProvider) *string { return redirectedModelPtr(modelName, p) }),
	})
	current = selectedProviders

	// 第 8 步：模型重定向预览（不过滤，只标注）。
	redirectFiltered := make([]SimulateProviderSnapshot, 0)
	redirectSurviving := make([]SimulateProviderSnapshot, 0, len(current))
	for _, p := range current {
		target := redirectedModelPtr(modelName, p)
		var details string
		switch {
		case modelName == "":
			details = "no_model_name_provided"
		case target != nil && *target != modelName:
			details = "redirect_rule_matched"
		default:
			details = "no_redirect_rule_matched"
		}
		redirectSurviving = append(redirectSurviving, snapshotOf(p, groupFilter, stringPtr(details), target, nil))
	}
	redirectNote := "redirects_apply_after_provider_selection"
	if modelName == "" {
		redirectNote = "redirect_preview_skipped_for_resource_request"
	}
	steps = append(steps, SimulateStep{
		StepName: "modelRedirect", StepIndex: 8,
		InputCount: len(current), OutputCount: len(current),
		FilteredOut: redirectFiltered, Surviving: redirectSurviving,
		Note: stringPtr(redirectNote),
	})

	// 第 9 步：端点池摘要（不过滤，只标注）。
	endpointSurviving := make([]SimulateProviderSnapshot, 0, len(current))
	for _, p := range current {
		var stats *SimulateEndpointStats
		if opts.EndpointStats != nil {
			stats = opts.EndpointStats(ctx, p)
		}
		endpointSurviving = append(endpointSurviving, snapshotOf(
			p, groupFilter, stringPtr("endpoint_pool_is_reported_as_downstream_risk_only"), nil, stats,
		))
	}
	steps = append(steps, SimulateStep{
		StepName: "endpointSummary", StepIndex: 9,
		InputCount: len(current), OutputCount: len(current),
		FilteredOut: make([]SimulateProviderSnapshot, 0), Surviving: endpointSurviving,
		Note: stringPtr("endpoint_status_does_not_change_provider_preselection"),
	})

	result := SimulateResult{
		Steps:               steps,
		PriorityTiers:       tiers,
		TotalProviders:      len(opts.Providers),
		FinalCandidateCount: len(selectedProviders),
	}
	if selectedTier >= 0 {
		priority := tiers[selectedTier].Priority
		result.SelectedPriority = &priority
	}
	return result, nil
}

// simulateGroupFilter 复刻 Node 的 getGroupFilterValue：空数组退化为 "default"。
func simulateGroupFilter(groupTags []string) string {
	cleaned := make([]string, 0, len(groupTags))
	for _, tag := range groupTags {
		trimmed := strings.TrimSpace(tag)
		if trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	if len(cleaned) == 0 {
		return GroupDefault
	}
	return strings.Join(cleaned, ",")
}

// simulateFormatCompatible 复刻 Node 的 checkFormatProviderTypeCompatibility
// **在无 endpointContext 时**的取值：兼容三态里只要不是 incompatible 就放行
// （convertible 时 Node 会调 canServeConvertibleEndpoint(undefined) → 恒 true）。
func simulateFormatCompatible(format convert.ClientFormat, p Provider) bool {
	if format == "" {
		return true
	}
	return convert.ResolveProtocolCompat(format, p.ProviderType, p.ConversionEnabled()) != convert.CompatIncompatible
}

// buildSimulatePriorityTiers 复刻 Node 的 buildPriorityTiers。
func buildSimulatePriorityTiers(
	providers []SimulateProvider,
	groupFilter string,
	modelName string,
	_ bool,
) []SimulatePriorityTier {
	if len(providers) == 0 {
		return make([]SimulatePriorityTier, 0)
	}
	priorities := make([]int, 0, len(providers))
	seen := make(map[int]bool)
	for _, p := range providers {
		priority := resolveEffectivePriority(p.Provider, groupFilter)
		if !seen[priority] {
			seen[priority] = true
			priorities = append(priorities, priority)
		}
	}
	sort.Ints(priorities)
	selected := priorities[0]

	tiers := make([]SimulatePriorityTier, 0, len(priorities))
	for _, priority := range priorities {
		at := make([]SimulateProvider, 0)
		totalWeight := 0
		for _, p := range providers {
			if resolveEffectivePriority(p.Provider, groupFilter) != priority {
				continue
			}
			at = append(at, p)
			totalWeight += p.Weight
		}
		mapped := make([]SimulatePriorityProvider, 0, len(at))
		for _, p := range at {
			percent := 0.0
			if totalWeight > 0 {
				percent = float64(p.Weight) / float64(totalWeight) * 100
			} else if len(at) > 0 {
				percent = 100 / float64(len(at))
			}
			mapped = append(mapped, SimulatePriorityProvider{
				SimulateProviderSnapshot: snapshotOf(p, groupFilter, nil, redirectedModelPtr(modelName, p), nil),
				WeightPercent:            percent,
			})
		}
		tiers = append(tiers, SimulatePriorityTier{
			Priority: priority, Providers: mapped, IsSelected: priority == selected,
		})
	}
	return tiers
}

func filterProviders(in []SimulateProvider, keep func(SimulateProvider) bool) []SimulateProvider {
	out := make([]SimulateProvider, 0, len(in))
	for _, p := range in {
		if keep(p) {
			out = append(out, p)
		}
	}
	return out
}

func buildSimulateStep(
	name string,
	index int,
	input []SimulateProvider,
	output []SimulateProvider,
	filtered []SimulateProviderSnapshot,
	groupFilter string,
	note *string,
) SimulateStep {
	return SimulateStep{
		StepName: name, StepIndex: index,
		InputCount: len(input), OutputCount: len(output),
		FilteredOut: filtered,
		Surviving:   snapshotsOf(output, groupFilter, nil, nil),
		Note:        note,
	}
}

// snapshotsExcept 产出「被本步淘汰」的快照列表，顺序与输入一致（Node 用 filter 保序）。
func snapshotsExcept(
	input []SimulateProvider,
	kept []SimulateProvider,
	details func(SimulateProvider) *string,
	groupFilter string,
	redirect func(SimulateProvider) *string,
	stats func(SimulateProvider) *SimulateEndpointStats,
) []SimulateProviderSnapshot {
	keptIDs := make(map[int64]bool, len(kept))
	for _, p := range kept {
		keptIDs[p.ID] = true
	}
	out := make([]SimulateProviderSnapshot, 0)
	for _, p := range input {
		if keptIDs[p.ID] {
			continue
		}
		var redirectPtr *string
		if redirect != nil {
			redirectPtr = redirect(p)
		}
		var statsPtr *SimulateEndpointStats
		if stats != nil {
			statsPtr = stats(p)
		}
		out = append(out, snapshotOf(p, groupFilter, details(p), redirectPtr, statsPtr))
	}
	return out
}

func snapshotsOf(
	providers []SimulateProvider,
	groupFilter string,
	details func(SimulateProvider) *string,
	redirect func(SimulateProvider) *string,
) []SimulateProviderSnapshot {
	out := make([]SimulateProviderSnapshot, 0, len(providers))
	for _, p := range providers {
		var detailsPtr *string
		if details != nil {
			detailsPtr = details(p)
		}
		var redirectPtr *string
		if redirect != nil {
			redirectPtr = redirect(p)
		}
		out = append(out, snapshotOf(p, groupFilter, detailsPtr, redirectPtr, nil))
	}
	return out
}

// snapshotOf 复刻 Node 的 buildProviderSnapshot 字段集与可空性。
func snapshotOf(
	p SimulateProvider,
	groupFilter string,
	details *string,
	redirectedModel *string,
	endpointStats *SimulateEndpointStats,
) SimulateProviderSnapshot {
	return SimulateProviderSnapshot{
		ID:                p.ID,
		Name:              p.Name,
		ProviderType:      p.ProviderType,
		GroupTag:          p.GroupTag,
		Priority:          p.EffectivePriority(),
		EffectivePriority: resolveEffectivePriority(p.Provider, groupFilter),
		Weight:            p.Weight,
		Details:           details,
		RedirectedModel:   redirectedModel,
		EndpointStats:     endpointStats,
	}
}

// RedirectTarget 复刻 Node 的 getProviderModelRedirectTarget：
// 按规则顺序取**第一条命中**规则的 target；无规则/未命中时返回原模型名。
func RedirectTarget(modelName string, rules json.RawMessage) string {
	if modelName == "" {
		return modelName
	}
	parsed := normalizeRedirectRules(rules)
	for _, rule := range parsed {
		if matchesPattern(modelName, rule.MatchType, rule.Source) {
			return rule.Target
		}
	}
	return modelName
}

type redirectRule struct {
	MatchType string
	Source    string
	Target    string
}

var redirectMatchTypes = map[string]bool{
	"exact": true, "prefix": true, "suffix": true, "contains": true, "regex": true,
}

// normalizeRedirectRules 复刻 Node 的 normalizeProviderModelRedirectRules：
// 规则数组逐条 trim 后原样保留（必须每条都合法，否则整份作废）；
// 映射对象（{source: target}）转成 exact 规则，空键/空值丢弃。
func normalizeRedirectRules(raw json.RawMessage) []redirectRule {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var list []any
	if err := json.Unmarshal(raw, &list); err == nil {
		out := make([]redirectRule, 0, len(list))
		for _, item := range list {
			record, ok := item.(map[string]any)
			if !ok {
				return nil
			}
			matchType, _ := record["matchType"].(string)
			source, _ := record["source"].(string)
			target, _ := record["target"].(string)
			source = strings.TrimSpace(source)
			target = strings.TrimSpace(target)
			if !redirectMatchTypes[matchType] || source == "" || target == "" {
				return nil
			}
			out = append(out, redirectRule{MatchType: matchType, Source: source, Target: target})
		}
		return out
	}

	var mapped map[string]any
	if err := json.Unmarshal(raw, &mapped); err != nil {
		return nil
	}
	out := make([]redirectRule, 0, len(mapped))
	for source, value := range mapped {
		trimmedSource := strings.TrimSpace(source)
		target, ok := value.(string)
		if !ok {
			continue
		}
		trimmedTarget := strings.TrimSpace(target)
		if trimmedSource == "" || trimmedTarget == "" {
			continue
		}
		out = append(out, redirectRule{MatchType: "exact", Source: trimmedSource, Target: trimmedTarget})
	}
	return out
}

// redirectedModelPtr 给出快照里的 redirectedModel：
// Node 只在「模型名非空」时才计算重定向目标，否则给 null。
func redirectedModelPtr(modelName string, p SimulateProvider) *string {
	if modelName == "" {
		return nil
	}
	target := RedirectTarget(modelName, p.ModelRedirects)
	return &target
}

func stringPtr(value string) *string { return &value }

func dashIfNil(value *string) string {
	if value == nil {
		return "-"
	}
	return *value
}
