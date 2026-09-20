package route

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// Options 是选路器的构造选项。
type Options struct {
	// Source 提供供应商与端点快照，必填。
	Source Source
	// Health 为 nil 时不做熔断判定（等于「全健康」）。
	Health *HealthReader
	// Affinity 为 nil 时不启用前缀亲和。
	Affinity *AffinityStore
	// AffinityIgnoreClientSessionID 对应系统设置 affinityIgnoreClientSessionId：
	// 为真时粘性交给最长前缀亲和（Node 的 skipSessionBinding 分支），
	// 请求日志的 session_identity_kind 记作 prefix_affinity；为假时记 session_id。
	// 它本身不参与选路判定（选路只看 Affinity 是否装配），只决定日志里的身份形制。
	AffinityIgnoreClientSessionID bool
	// Gates 是三个外部维度的钩子，零值即全部不判定。
	Gates Gates
	// Rand 是加权选择的随机源，返回 [0,1)；nil 时用全局随机源。
	// 注入它才能让「同一快照 + 同一随机序列」复现同一选择。
	Rand func() float64
	// Now 可注入时钟。
	Now func() time.Time
	// Logger 为 nil 时不输出。
	Logger *logx.Logger
	// EndpointGate 为真时在候选期排除「厂下无启用端点」的供应商（默认关闭，见 basicFilterRejection）。
	EndpointGate bool
	// SameProtocolWeightK 是同协议候选的权重倍率。
	//
	// 需求（用户）：优先级一致时优先走「不需要协议转换」的供应商，以吃到同协议带来的稳定性。
	// 语义边界（实现必须守住的四条）：
	//   1. 「同协议」复用 convert.IsNativePair —— 与 guard 的格式兼容过滤同一判据
	//      （filter.go 经 ResolveProtocolCompat 调它），不另写近似判断；
	//   2. 只在**同一最高优先组内**生效（selectTopPriority 的结果），不跳优先级；
	//   3. 是偏好而非独占：跨协议候选不被排除，只是权重更低（权重全为 0 的等概率兜底路径
	//      会在同协议子集内取，详见 selectOptimal）；
	//   4. 客户端格式未知（Request.Format == ""）时不判定——那时连格式兼容过滤也不判定，
	//      无法区分同协议。
	//
	// 取值：≤ 1 表示不启用（零值即不启用，保证未装配时不改变行为）；生产取值来自
	// config 的 CCH_SAME_PROTOCOL_WEIGHT_K。
	SameProtocolWeightK int
}

// Selector 是一次进程内共享的选路器，可并发使用。
type Selector struct {
	opts Options
	// warnedGroupPriorities 记录已就「脏 group_priorities」告警过的 (供应商, 键, 类型)。
	//
	// resolve 是**每请求**调用的，而脏值是库里的存量事实：不记已告警过的项，一行脏值
	// 会把日志在每请求上刷一遍（选路器与进程同生命周期，恰好是「每处只报一次」的天然作用域）。
	warnedGroupPriorities sync.Map
}

// Request 是一次选路的输入。它刻意不含请求正文以外的任何 session 内部状态。
type Request struct {
	// Model 是原始请求模型（Node 的 session.getOriginalModel）；空串表示资源类端点不带模型。
	Model string
	// Format 是客户端入站格式；空串表示未知（此时不做格式兼容判定）。
	Format convert.ClientFormat
	// Group 是有效分组（Node 的 getEffectiveProviderGroup：key.providerGroup > user.providerGroup > default）。
	// 空串表示不做分组过滤。
	Group string
	// KeyID 是 API Key 的 ID，用于亲和 scope tag；0 表示不参与亲和。
	KeyID int64
	// Endpoint 是端点维度上下文。
	Endpoint EndpointContext
	// ExcludeIDs 是故障转移排除列表（前序尝试失败的供应商）。
	ExcludeIDs []int64
	// AffinityBody 是已解析的请求正文，仅用于前缀亲和指纹；nil 表示本次不参与亲和。
	// 指纹是按需解析的事实，不是常驻状态：调用方应在亲和开关关闭时直接传 nil。
	AffinityBody map[string]any
	// AffinityLookup 是调用方已完成的亲和查找（Node 的 session.affinity.lookup）：
	// 非 nil 时选路直接复用，不再访问 Redis。
	AffinityLookup *AffinityLookup

	// ScheduleGate 是本次请求的调度窗口判定：返回 false 即该供应商不在活动时段内。
	//
	// 为何是请求级而不是 Options.Gates.Schedule：Node 在每次 pickRandomProvider 里
	// `await resolveSystemTimezone()` 后拿**同一个** systemTimezone 逐候选判定
	// （provider-selector.ts:1293,1305）；时区每请求解析一次，才是 Node 的口径。
	// nil 表示不判定（回退到 Options.Gates.Schedule）。
	ScheduleGate func(p Provider) bool
	// ClientGate 是本次请求的供应商客户端名单判定（Node Step 1 的 isClientAllowedDetailed）。
	// 返回 nil 表示该供应商未配名单（Node 在两侧名单都空时直接放行，不做判定）。
	// nil 字段本身表示不判定（回退到 Options.Gates.Client）。
	ClientGate func(p Provider) *ClientRestriction
}

// Result 是一次选路的结果与留痕。
type Result struct {
	// Provider 为 nil 表示无可用供应商（Node 返回 null 触发 503）。
	Provider *Provider
	// Context 是决策留痕，直接对应 provider_chain[].decisionContext。
	Context DecisionContext
	// Method 与 Reason 对应 provider_chain[].selectionMethod / reason。
	Method SelectionMethod
	Reason SelectionReason
	// CircuitState 是选中项的健康快照，对应 provider_chain[].circuitState。
	CircuitState CircuitState
	// Affinity 非 nil 表示本次系亲和命中。
	Affinity *AffinityNomination
	// AffinityLookup 是本次请求使用的亲和查找结果（**未命中时也非 nil**）：
	// 终态写回需要它的 IdentityFP 与 Generation 做 generation CAS。
	// 未参与亲和（开关关闭 / 无法指纹化 / 查找不可用）时为 nil。
	AffinityLookup *AffinityLookup
	// AffinityWriteback 是本次请求的亲和终态写回事实（**未命中时也非 nil**）。
	// 接线层把它装进 pctx，终态层在提交后调用；未参与亲和时为 nil。
	AffinityWriteback *AffinityWriteback

	// AffinityIdentity 非 nil 表示本次请求有亲和身份事实（对应 Node 的 session.affinity
	// 存在）。**与 AffinityLookup 不同**：查找不可用（Redis 故障）时仍非 nil，
	// 因为身份事实只依赖指纹链与键，不依赖 Redis。请求日志的 session_identity_kind 取它。
	AffinityIdentity *AffinityIdentity

	// timestamp 是结果产出的毫秒时间戳，仅供落链使用，不参与选路语义。
	timestamp int64
}

// NewSelector 构造选路器。
func NewSelector(opts Options) *Selector {
	return &Selector{opts: opts}
}

// Select 是**首次选择**的入口，对应 Node 的 ProxyProviderResolver.ensure 的选择部分，
// 以及紧邻其前的亲和提名（provider-selector.ts:326 tryPrefixAffinityNomination，
// 守卫是 `!session.provider`）。
// （会话复用与故障转移循环属接线波次，本包只负责「给定输入选出供应商并留痕」。）
func (s *Selector) Select(ctx context.Context, req Request) (Result, error) {
	return s.resolve(ctx, req, true) // 首次选择：咨询亲和
}

// SelectFailover 是故障转移 / 重试 / 竞速取候选的入口，对应 Node 的
// ProxyProviderResolver.pickRandomProviderWithExclusion（provider-selector.ts:620）。
//
// 它必须**不咨询亲和**——这是 Node 的既有语义，不是本实现的取舍：Node 只在首次选择时
// 跑一次亲和提名，而故障转移走 forwarder.ts:4880 selectAlternative →
// pickRandomProviderWithExclusion → pickRandomProvider（provider-selector.ts:1168-1515），
// 该函数体内没有任何 affinity 引用，取候选是纯「selectTopPriority 分层 + 同档加权随机」。
//
// Go 先前在故障转移时复用 Select 并带上请求正文，于是先做了一次亲和提名。差别只在
// **亲和 hint 指向一家既非当前主选、又不在排除表的供应商**时才显现（典型成因：兄弟会话
// 刚写回更近的前缀记录）：那时 Go 会跳去那家，而 Node 会按分层重挑。主选总在排除表内的
// 常见情形被 `excluded` 挡住，所以这条分叉长期不可见（用例见 select_failover_affinity_test.go）。
func (s *Selector) SelectFailover(ctx context.Context, req Request) (Result, error) {
	return s.resolve(ctx, req, false) // 故障转移：绝不咨询亲和
}

// warnGroupPrioritiesIssues 就脏 group_priorities 告警（每处值只报一次）。
//
// 为什么告警在这里而不是在读库处：容错解码在 store 的只读层与 route 的桥接处，两者都是
// 纯函数、没有 logger；选路器有（Options.Logger）。做法对齐 guard 侧 error_rules 的既有
// 口径（`guard.error_rules.unknown_match_type`：脏值跳过 + 告警，事件名「域.字段.问题」）。
// 日志里**只带定位信息**（供应商、分组键、jsonb 类型），不带原始值。
func (s *Selector) warnGroupPrioritiesIssues(providers []Provider) {
	logger := s.opts.Logger
	if logger == nil {
		return
	}
	for _, provider := range providers {
		for _, issue := range provider.groupPrioritiesIssues {
			stamp := fmt.Sprintf("%d|%s|%s|%t", provider.ID, issue.Key, issue.JSONType, issue.TopLevel)
			if _, already := s.warnedGroupPriorities.LoadOrStore(stamp, struct{}{}); already {
				continue
			}
			fields := map[string]any{
				"providerId": provider.ID,
				"jsonType":   issue.JSONType,
				"topLevel":   issue.TopLevel,
			}
			if issue.Key != "" {
				fields["group"] = issue.Key
			}
			logger.Warn("route.group_priorities.invalid_value", fields)
		}
	}
}

// resolve 是两个入口的共同实现：硬校验过滤 → 亲和提名（可跳过）→ 同档加权随机。
//
// withAffinity 是两者唯一的差别。用具名形参而非两个近似函数体，是为了让「跳过的到底是
// 哪一段」只有一处定义：亲和提名还会顺带做一次 Redis 查找，而 AffinityStore.Lookup 的 Lua
// 在命中时**原子续期**，那属亲和状态改写——故障转移路径不该触发它。
func (s *Selector) resolve(ctx context.Context, req Request, withAffinity bool) (Result, error) {
	if s.opts.Source == nil {
		return Result{}, errNoSource
	}
	nowMS := s.nowOr().UnixMilli()

	providers, err := s.opts.Source.Providers(ctx)
	if err != nil {
		return Result{}, err
	}
	s.warnGroupPrioritiesIssues(providers)

	excluded := make(map[int64]bool, len(req.ExcludeIDs))
	for _, id := range req.ExcludeIDs {
		excluded[id] = true
	}

	filtered := s.applyFilters(ctx, providers, filterInput{
		requestedModel: req.Model,
		format:         req.Format,
		group:          req.Group,
		excluded:       excluded,
		endpoints:      req.Endpoint,
		endpointGate:   s.opts.EndpointGate,
		gates:          s.opts.Gates,
		scheduleGate:   req.ScheduleGate,
		clientGate:     req.ClientGate,
	})
	dc := filtered.context

	// 亲和提名优先于加权随机（Node：显式 session 绑定 > 亲和 > 加权随机；
	// 会话绑定属接线波次，本包只做亲和与加权随机两级）。
	//
	// 故障转移路径整段跳过：此时 lookup / writeback / identity 一概为 nil——既不提名，
	// 也不产生终态写回事实（那会带上 generation 做 CAS），更不做命中续期。
	var (
		nominate         AffinityNomination
		lookup           *AffinityLookup
		writeback        *AffinityWriteback
		affinityIdentity *AffinityIdentity
		nominated        bool
	)
	if withAffinity {
		nominate, lookup, writeback, affinityIdentity, nominated = s.nominateByAffinity(ctx, req, excluded)
	}
	if nominated {
		// 留痕「因亲和短路而未参与竞争」的那批候选。必须在短路返回之前记下：此后
		// filtered.health 就不再被读过，漏在这里即永久丢失（用户看到的「1/2/3 都没参与决策」
		// 就是漏记造成的）。
		//
		// 优先级一律取**分组覆盖后**的值：分层是拿它算的，记配置值会让「选中哪一档」与
		// priorityLevels 自相矛盾（生产实证：fan 组下配置 4、分层 0，链上却写 selectedPriority=4）。
		// 两个键都写，且由**同一次计算**投影而来（见 consideredFromSurvivors）：consideredCandidates
		// 是跨路径统一的「参与池」（界面读它），survivingCandidates 多带一位 affinitySkipped，
		// 回答「为何未参与竞争」。只写后者的话，统一读前者的界面在亲和行上什么也看不到——
		// 生产 3 小时窗口 1549 行 affinity_hit 正是如此（considered 合计 0、surviving 合计 5108）。
		selectedPriority := resolveEffectivePriority(nominate.Provider, req.Group)
		survivors := affinitySurvivors(filtered.healthy, nominate.Provider.ID, req.Group)
		dc.SurvivingCandidates = survivors
		dc.ConsideredCandidates = consideredFromSurvivors(survivors)
		dc.PriorityLevels = []int{selectedPriority}
		dc.SelectedPriority = selectedPriority
		dc.CandidatesAtPriority = []Candidate{{
			ID:             nominate.Provider.ID,
			Name:           nominate.Provider.Name,
			Weight:         nominate.Provider.Weight,
			CostMultiplier: nominate.Provider.CostMultiplier,
		}}
		return Result{
			Provider:          &nominate.Provider,
			Context:           dc,
			Method:            MethodPrefixAffinity,
			Reason:            ReasonSelectedAffinity,
			CircuitState:      s.circuitState(ctx, nominate.Provider.ID),
			Affinity:          &nominate,
			AffinityLookup:    lookup,
			AffinityWriteback: writeback,
			AffinityIdentity:  affinityIdentity,
			timestamp:         nowMS,
		}, nil
	}

	if len(filtered.healthy) == 0 {
		return Result{
			Context:           dc,
			Method:            MethodWeightedRandom,
			AffinityLookup:    lookup,
			AffinityWriteback: writeback,
			AffinityIdentity:  affinityIdentity,
			timestamp:         nowMS,
		}, nil
	}

	top := selectTopPriority(filtered.healthy, req.Group)
	dc.PriorityLevels = priorityLevels(filtered.healthy, req.Group)
	// 与分层同源：排序用的是 resolveEffectivePriority（含分组覆盖），这里若用配置值，就会把
	// 「选中了 0 档」记成「选中了配置档位」，使用者据此误判成「低优先级被选中」。
	dc.SelectedPriority = resolveEffectivePriority(top[0], req.Group)

	pref := newSameProtocolPreference(req.Format, s.opts.SameProtocolWeightK)
	weights := make([]int64, len(top))
	sameFlags := make([]bool, len(top))
	var totalWeight int64
	sameCount := 0
	for index, p := range top {
		weight := int64(p.Weight)
		same := pref.isSame(req.Format, p)
		sameFlags[index] = same
		if same {
			weight *= int64(pref.k)
			sameCount++
		}
		weights[index] = weight
		totalWeight += weight
	}
	// 权重全为 0 时加权随机退化为等概率（Node 语义）；若此时启用了同协议偏好，
	// 「等概率」也应当体现偏好：只在同协议候选里取（无同协议候选时仍是全体等概率）。
	preferSameOnly := pref.enabled && totalWeight == 0 && sameCount > 0

	dc.CandidatesAtPriority = make([]Candidate, 0, len(top))
	for index, p := range top {
		probability := 0.0
		switch {
		case totalWeight > 0:
			probability = float64(weights[index]) / float64(totalWeight)
		case preferSameOnly && sameFlags[index]:
			probability = 1 / float64(sameCount)
		}
		candidate := Candidate{
			ID:             p.ID,
			Name:           p.Name,
			Weight:         p.Weight,
			CostMultiplier: p.CostMultiplier,
			Probability:    probability,
		}
		if pref.enabled {
			same := sameFlags[index]
			effective := float64(weights[index])
			candidate.SameProtocol = &same
			candidate.EffectiveWeight = &effective
		}
		dc.CandidatesAtPriority = append(dc.CandidatesAtPriority, candidate)
	}

	selected := s.selectOptimal(top, weights, preferSameOnly, sameFlags)
	dc.ConsideredCandidates = consideredCandidates(filtered.healthy, selected.ID, req.Group)
	method := MethodWeightedRandom
	if dc.GroupFilterApplied {
		method = MethodGroupFiltered
	}
	return Result{
		Provider:          &selected,
		Context:           dc,
		Method:            method,
		Reason:            ReasonSelectedInitial,
		CircuitState:      s.circuitState(ctx, selected.ID),
		AffinityLookup:    lookup,
		AffinityWriteback: writeback,
		AffinityIdentity:  affinityIdentity,
		timestamp:         nowMS,
	}, nil
}

// consideredCandidates 把「通过全部硬校验的候选池」投影为决策留痕。
//
// 为什么需要（用户实证，2026-09-14）：priorityLevels 只说「有哪些档位」，candidatesAtPriority 只记
// **最高档**的候选。于是「档位更低而落选」的家在链上完全不可见——用户看着链里只有一家，报
// 「我看这个会话的决策链，还是看不到 opencode 那几个渠道参与呢？」，而它们其实都在候选池里。
//
// 顺序沿用通过集自身的顺序（与 filteredProviders、affinitySurvivors 的记法一致），不重排：
// 排序是展示层的事，记录层改动顺序会让同一事实在不同版本产生不同 JSON，白白制造对拍噪声。
//
// 返回 nil 即**无可报之事**：通过集里只有一家，它必然被选中，没有「参与了却没选中」可说。
// 此时键不出现，与黄金样本的键集精确相等断言保持一致（与 survivingCandidates 同一手法）。
func consideredCandidates(
	healthy []Provider,
	selectedID int64,
	userGroup string,
) []ConsideredCandidate {
	if len(healthy) < 2 {
		return nil
	}
	out := make([]ConsideredCandidate, 0, len(healthy))
	for _, p := range healthy {
		out = append(out, consideredCandidate(p, selectedID, userGroup))
	}
	return out
}

// consideredCandidate 把一家供应商投影为候选留痕（两个数组共用，保证字段同形）。
func consideredCandidate(p Provider, selectedID int64, userGroup string) ConsideredCandidate {
	return ConsideredCandidate{
		ID:                p.ID,
		Name:              p.Name,
		Priority:          p.EffectivePriority(),
		EffectivePriority: resolveEffectivePriority(p, userGroup),
		Weight:            p.Weight,
		CostMultiplier:    p.CostMultiplier,
		Selected:          p.ID == selectedID,
	}
}

// selectTopPriority 复刻 selectTopPriority：只保留有效优先级最小（数值最小 = 最高优先）的一组。
func selectTopPriority(providers []Provider, userGroup string) []Provider {
	if len(providers) == 0 {
		return nil
	}
	lowest := resolveEffectivePriority(providers[0], userGroup)
	for _, p := range providers[1:] {
		if value := resolveEffectivePriority(p, userGroup); value < lowest {
			lowest = value
		}
	}
	out := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if resolveEffectivePriority(p, userGroup) == lowest {
			out = append(out, p)
		}
	}
	return out
}

// priorityLevels 复刻 Node 的 priorityLevels：去重后升序。
func priorityLevels(providers []Provider, userGroup string) []int {
	out := make([]int, 0, len(providers))
	for _, p := range providers {
		out = append(out, resolveEffectivePriority(p, userGroup))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// selectOptimal 复刻 selectOptimal：先按成本倍率升序稳定排序，再按权重加权随机。
//
// weights 是本轮加权选择实际使用的权重（同协议候选已乘 K；未启用偏好时等于 provider.Weight）。
// preferSameOnly 为真时只在同协议候选里等概率取——这是「权重全为 0 且启用偏好」这一退化的
// 兜底路径；此时候选仍不被排除，只是不参与本次取值。
func (s *Selector) selectOptimal(
	providers []Provider,
	weights []int64,
	preferSameOnly bool,
	sameFlags []bool,
) Provider {
	if len(providers) == 1 {
		return providers[0]
	}
	order := make([]int, len(providers))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(a, b int) bool {
		return providers[order[a]].CostMultiplierFloat() < providers[order[b]].CostMultiplierFloat()
	})
	if preferSameOnly {
		subset := make([]int, 0, len(order))
		for _, index := range order {
			if sameFlags[index] {
				subset = append(subset, index)
			}
		}
		if len(subset) > 0 {
			return s.weightedRandom(providers, weights, subset)
		}
	}
	return s.weightedRandom(providers, weights, order)
}

// weightedRandom 复刻 weightedRandom：按 order 给定的顺序对候选加权随机；
// 总权重为 0 时退化为等概率随机取一项（与 Node 的 selectOptimal 一致：
// Node 在那里不重排，因此本函数的 order 参数就是「候选的取值顺序」）。
func (s *Selector) weightedRandom(providers []Provider, weights []int64, order []int) Provider {
	total := int64(0)
	for _, index := range order {
		total += weights[index]
	}
	if total == 0 {
		index := int(s.rand() * float64(len(order)))
		if index >= len(order) {
			index = len(order) - 1
		}
		if index < 0 {
			index = 0
		}
		return providers[order[index]]
	}
	target := s.rand() * float64(total)
	cumulative := int64(0)
	for _, index := range order {
		cumulative += weights[index]
		if target < float64(cumulative) {
			return providers[index]
		}
	}
	return providers[order[len(order)-1]]
}

func (s *Selector) rand() float64 {
	if s.opts.Rand != nil {
		value := s.opts.Rand()
		return clampUnit(value)
	}
	return rand.Float64()
}

func clampUnit(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value >= 1:
		// 与 Math.random() 一致：取值区间为 [0,1)，1 视为接近 1 的最大值。
		return 0.9999999999999999
	default:
		return value
	}
}

func (s *Selector) circuitState(ctx context.Context, providerID int64) CircuitState {
	if s.opts.Health == nil {
		return StateClosed
	}
	return s.opts.Health.ProviderState(ctx, providerID)
}
