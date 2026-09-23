package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 分组标签缓存的容量：分组标签是「几乎不变的管理配置」，条目数等于启用态供应商数。
const providerGroupTagCacheSize = 4096

// ErrNoProviderAvailable 表示选路结果为「无可用供应商」（Node 的 provider === null）。
//
// 它**不是步骤失败**：Node 在此处并不中止请求，而是继续走到转发层并返回
// `503 no_available_providers`（见 `internal/dataplane` 的 forward 与 Node 的
// `provider === null` 分支）。故 dataplane 必须用 `errors.Is` 把它识别成 503，
// 不可归入「步骤失败 → 500」那条路——生产实测两者形状不同，客户端语义也不同。
var ErrNoProviderAvailable = errors.New("guard: 无可用供应商")

// ProviderRouter 把选路包接到 ProviderSelector 缝隙上。
//
// 缝隙契约里 pctx 只带最小身份与协议族，而 route.Request 要的是「原始模型 + 客户端格式 +
// 有效分组 + 亲和指纹正文」。这些取值点由接线方经钩子注入，而不是在适配器里猜：
// 猜出来的分组或格式会静默改变候选集，且出错时看不出来。
type ProviderRouter struct {
	selector *route.Selector
	source   route.Source
	// snapshot 是选路快照缓存（providers + provider_endpoints），用于观测与「不每请求查库」断言。
	snapshot *cachedSource
	// Body 取本次请求正文（亲和指纹用；亲和关闭时 route 内部忽略）。
	Body BodyFactory
	// Group 取有效分组；nil 时用默认分组。
	Group func(context.Context, *pctx.Context) string
	// Format 覆盖客户端格式；nil 时按 pctx.ProtocolFrom 推导。
	Format func(*pctx.Context) convert.ClientFormat
	// tags 缓存 providers.group_tag，按 DomainProviders 失效。
	tags *cfgsync.TTLMap[int64, string]
	// routeOptions 是构造选器用的同一份选项：本适配器需要其中的 Affinity（会话空闲判定）
	// 与 Now。存整份而不只存 store，是因为 Now 必须与选器同源，否则判定与选路会走两个时钟。
	routeOptions route.Options
	// sessionID 是本次请求所属的客户端会话身份，由**会话守卫步骤**（session.go）解析后盖到
	// 本次请求的选路器视图上（见 SetConversationSession 的说明）。空串 ⇒ 未接线或客户端未带，
	// 空闲闸门 fail-open 并在日志里可辨。
	sessionID string
	// sessionBinding 是本次请求的会话绑定快照（会话守卫步骤随 sessionID 一同盖上）。
	// nil 表示无绑定事实（未接线、Redis 不可用或读取冲突）——此时选路走前缀兜底或加权随机。
	sessionBinding *route.SessionBindingSnapshot
	// sessionIdentity 是会话身份的**来源**（客户端显式携带 / 按正文哈希恢复 / 网关生成），
	// 由会话守卫步骤随 sessionID 一同盖上。
	//
	// 为什么需要它：前缀兜底层服务的是「客户端未带 id」的请求，而会话包总会为这类请求
	// 生成或恢复出一个非空 SessionID——只看 SessionID 是否为空，这道兜底永不触发。
	// 零值即「客户端显式携带」，与接线前的行为一致。
	sessionIdentity route.SessionIdentity

	// DetectClient 判定某供应商的客户端名单（Node Step 1 的 isClientAllowedDetailed）。
	// 由 `Adapters.Apply` 注入（复用 `guard/client.go` 里那份 client-detector 移植），
	// 本包不重复实现 UA 匹配。
	DetectClient func(ctx *pctx.Context, allowed, blocked []string) *route.ClientRestriction
	// SystemTimezone 解析系统时区（Node 的 resolveSystemTimezone）。按 Node 口径**每请求**
	// 解析一次，而不是每个候选一次（provider-selector.ts:1293）。nil 时不判活动时段。
	SystemTimezone func(ctx context.Context) string

	logger *logx.Logger
}

// newProviderRouter 建选路适配器。
//
// Source 一律用 route.NewStoreSource（真实快照面）：调用方可以在 RouteOptions 里覆盖
// Health/Affinity/Gates/Rand，但 Source 由本函数填，避免出现「用的是假快照却以为在测真实选路」。
func newProviderRouter(
	pools *store.Pools,
	registry *cfgsync.Registry,
	options route.Options,
	logger *logx.Logger,
) *ProviderRouter {
	// 一律套快照缓存：route 把缓存明确留给调用方，而选路在每请求热路径上。
	snapshot := newCachedSource(route.NewStoreSource(pools), registry)
	source := route.Source(snapshot)
	options.Source = source
	if options.Logger == nil {
		options.Logger = logger
	}
	return &ProviderRouter{
		selector:     route.NewSelector(options),
		routeOptions: options,
		source:       source,
		snapshot:     snapshot,
		tags:         cfgsync.NewTTLMap[int64, string](cfgsync.Spec(cfgsync.DomainProviders).TTL, providerGroupTagCacheSize),
		// 系统时区：与 Node 的 resolveSystemTimezone 同一条降级链
		// （system_settings.timezone → env TZ → UTC），实现在 store 侧。
		SystemTimezone: func(ctx context.Context) string {
			return pools.AdminSystemTimezoneOrUTC(ctx)
		},
		logger: logger,
	}
}

// scheduleGate 构造本次请求的活动时段判定。
//
// 为什么按请求构造：Node 在 pickRandomProvider 里 `await resolveSystemTimezone()` **一次**，
// 再用同一个 systemTimezone 逐候选判定（provider-selector.ts:1293,1305,1355）——逐候选
// 重新解析时区会多出 N 次设置读取，且同一请求内的判定基准可能跳变。
//
// 时区名无效时返回 nil（不判定）：Node 的 resolveSystemTimezone 已在源头保证 IANA 合法，
// 这里遇到非法值只能当配置脏数据，退到「不判定」比把请求判死安全（与两侧 fail-open 一致）。
func (r *ProviderRouter) scheduleGate(ctx context.Context) func(route.Provider) bool {
	if r.SystemTimezone == nil {
		return nil
	}
	location, err := time.LoadLocation(r.SystemTimezone(ctx))
	if err != nil {
		r.logger.Warn("guard.adapters.schedule_timezone_invalid", map[string]any{
			"error": err.Error(),
		})
		return nil
	}
	now := time.Now().In(location)
	return func(p route.Provider) bool {
		return route.ProviderActiveNow(p.ActiveTimeStart, p.ActiveTimeEnd, now)
	}
}

// clientGate 构造本次请求的供应商级客户端名单判定。
//
// 名单两侧都空时返回 nil：Node 在那种情况下**不进判定**（provider-selector.ts:1268-1272），
// 因此不应当产出任何留痕。
func (r *ProviderRouter) clientGate(req *pctx.Context) func(route.Provider) *route.ClientRestriction {
	if r.DetectClient == nil {
		return nil
	}
	return func(p route.Provider) *route.ClientRestriction {
		allowed := clientPatterns(p.AllowedClients)
		blocked := clientPatterns(p.BlockedClients)
		if len(allowed) == 0 && len(blocked) == 0 {
			return nil
		}
		return r.DetectClient(req, allowed, blocked)
	}
}

// clientPatterns 解析供应商名单列（jsonb 数组）。
//
// 空/NULL/非法一律当**空名单**：与 Node 的 `p.allowedClients ?? []` 同向；这里额外容错
// 非法 JSON，因为把脏数据当「无白名单」与 Node 的「白名单为空即放行」结果一致。
func clientPatterns(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var patterns []string
	if err := json.Unmarshal(raw, &patterns); err != nil {
		return nil
	}
	return patterns
}

// Select 选路并返回写回上下文槽位的结果。
//
// 无可用供应商时返回 ErrNoProviderAvailable：调用方（dataplane）要把它映射成
// Node 同形的 503 no_available_providers，而不是当成守卫步骤失败（500）。
func (r *ProviderRouter) Select(ctx context.Context, req *pctx.Context) (pctx.ProviderSelection, error) {
	if r == nil || r.selector == nil {
		return pctx.ProviderSelection{}, errors.New("guard: 选路器未接线")
	}
	body := requestBody(r.Body, req)
	selectionRequest := route.Request{
		Model:  bodyModel(body),
		Format: r.formatOf(req),
		Group:  r.groupOf(ctx, req),
		KeyID:  r.keyIDOf(req),
		// 会话身份：会话级低速冷却的过滤依据（同值已在亲和空闲闸门用）。
		// 未接线或客户端未带时为空串，冷却过滤整段不判定（fail-open）。
		SessionID: r.sessionID,
		// 会话绑定：会话粘性的第一优先级输入。
		// 非 nil 且 ProviderID != 0 时短路前缀亲和与加权随机（见 route.nominateBySessionBinding）。
		SessionBinding: r.sessionBinding,
		// 会话身份来源：前缀兜底层按它判定是否参与（客户端未带 id 时才兜底）。
		SessionIdentity: r.sessionIdentity,
		// Endpoint 维度的判定（端点族、端点策略）属入口与端点包；零值表示不按端点维度排除。
		AffinityBody: body,
		// 两个请求级门槛：Node 在 pickRandomProvider 里每请求解析一次 systemTimezone 后做
		// 活动时段判定（Step 2a-2），并用本次请求的头部/正文做供应商级客户端名单判定（Step 1）。
		ScheduleGate: r.scheduleGate(ctx),
		ClientGate:   r.clientGate(req),
	}
	result, err := r.selector.Select(ctx, selectionRequest)
	if err != nil {
		return pctx.ProviderSelection{}, fmt.Errorf("guard: 选路失败: %w", err)
	}
	// 会话空闲闸门：亲和命中后还要问一句「这次对话是不是刚醒」。
	//
	// 为什么必须在守卫层而不在 route 内部：会话身份是**每请求事实**（走既有会话缝合道），
	// 而 route 的 Lookup 只拿得到指纹链（session 包 import guard，反向引用成环，route 拿不到它）。
	//
	// 撤销手法用现成的注入缝：route.Request.AffinityLookup 本就是「调用方已完成的查找」，
	// 传一个 **无 hint 但保留 identity/generation** 的 lookup 进去，选器就会跳过 Redis 并走
	// 与真实未命中完全相同的分支——包括终态写回（写回靠 IdentityFP/Generation，不能丢）。
	if suppressed, expiry := r.applyConversationIdle(ctx, req, &selectionRequest, result); suppressed {
		r.logger.Warn("guard.adapters.affinity_expired", expiry)
		reSelected, reErr := r.selector.Select(ctx, selectionRequest)
		if reErr != nil {
			return pctx.ProviderSelection{}, fmt.Errorf("guard: 空闲撤销后重选失败: %w", reErr)
		}
		result = reSelected
	}
	if result.Provider == nil {
		// 附带归因：同样的 503 有两种完全不同的成因（模型无人支持 / 供应商真不可用），
		// 而响应体逐字节一致，不带上事实就无法在事后分辨。见 NoProviderDiagnostic。
		return pctx.ProviderSelection{}, NewNoProviderError(result.Context, string(r.formatOf(req)))
	}
	// 亲和终态写回事实必预装上：不装就等于写回静默失效（粘性退化且无告警），
	// 这是 Node 的 session.affinity 在本进程里的对应槽位。
	if result.AffinityWriteback != nil {
		req.SetAffinityWriteback(*result.AffinityWriteback)
	}
	// 亲和身份事实：日志的 session_identity_kind 取它（Node 的 session.affinity 对应槽位）。
	// 它与写回不同条件，故单独装：Redis 故障时写回为 nil，身份仍在。
	if result.AffinityIdentity != nil {
		req.SetAffinityIdentity(result.AffinityIdentity.ScopeTag, result.AffinityIdentity.Fingerprint)
	}
	// 选择期链条目（provider_chain 链首）：界面弹窗读链首判定「渠道复用 / 新会话」
	// 并展示决策上下文，故把选路结果原样落进上下文（见 pctx.SelectionChainEntry）。
	// 序列化失败只告警不失败：链首缺失是观测缺陷，不应变成请求失败。
	if encoded, encodeErr := json.Marshal(result.ChainItem()); encodeErr != nil {
		r.logger.Warn("guard.selection_chain_entry_unencodable", map[string]any{
			"providerId": result.Provider.ID,
			"error":      encodeErr.Error(),
		})
	} else {
		req.SetSelectionChainEntry(encoded)
	}
	r.logger.Debug("guard.adapters.provider_selected", map[string]any{
		"providerId":           result.Provider.ID,
		"method":               string(result.Method),
		"reason":               string(result.Reason),
		"sessionBindingBypass": result.SessionBindingBypass.String(),
	})
	return pctx.ProviderSelection{
		ProviderID: result.Provider.ID,
		Name:       result.Provider.Name,
		Type:       string(result.Provider.ProviderType),
		// Endpoint 给供应商自身 URL；厂级端点（provider_endpoints）由转发层按厂与类型再取。
		Endpoint: result.Provider.URL,
		// 探针事实：本次请求是被隔离组合的定向试探。数据面据此关掉竞速（否则探针被快家
		// 取消，样本失真），见 pctx.ProviderSelection.SlowProbe 与 dataplane.slowProbeTarget。
		SlowProbe: result.SlowProbe != nil,
	}, nil
}

// applyConversationIdle 判定本次亲和命中是否应因「本会话空闲超过阈值」而撤销。
//
// 返回 true 时已把 selectionRequest 的 AffinityLookup 换成「无 hint 但保留 identity/generation」
// 的版本（调用方据此重选），第二个返回值为留痕字段。
//
// 三个门限逐层挡掉不该动的请求：
//  1. 亲和未装配（Options.Affinity 为 nil）或**本次命中未被采纳**（Result.Affinity 为 nil，
//     含硬校验回落）⇒ 不动；
//  2. 客户端未带会话 id / 缝合道未接线 ⇒ **fail-open 不撤销**，但仍留一条可辨的痕迹
//     （否则「不带 id 的客户端从不被撤销」在事后看是隐形的）；
//  3. 记录不可信（首条请求、记录已过期、Redis 故障）⇒ fail-open 不撤销。
func (r *ProviderRouter) applyConversationIdle(
	ctx context.Context,
	req *pctx.Context,
	selectionRequest *route.Request,
	result route.Result,
) (bool, map[string]any) {
	if r.routeOptions.Affinity == nil || result.Affinity == nil || result.AffinityLookup == nil {
		return false, nil
	}
	scopeTag := route.ScopeTag(selectionRequest.KeyID, selectionRequest.Format, selectionRequest.Model)
	sessionID, ok := r.conversationID()
	if !ok {
		r.logger.Warn("guard.adapters.affinity_idle_unknown", map[string]any{
			"scopeTag":   scopeTag,
			"providerId": result.Affinity.Provider.ID,
			"note":       "客户端未带会话身份（或会话缝合道未接线），空闲闸门 fail-open，本次仍按亲和粘性选路",
		})
		return false, nil
	}
	now := r.now().Unix()
	idle, threshold, known := r.routeOptions.Affinity.ConversationIdle(ctx, scopeTag, sessionID, now)
	// 无论是否撤销都刷新活跃时刻：本次请求本身就是该会话的一次活跃。
	// （不反向补偿绑定刚做的命中续期：被撤销的绑定多活一个 TTL 无副作用，本次请求会把新选中的
	// 供应商写成更深的前缀绑定，下次自然命中新的那个。）
	r.routeOptions.Affinity.NoteConversationActivity(ctx, scopeTag, sessionID, now)
	if !known || idle <= threshold {
		return false, nil
	}
	// 保留 identity/generation：终态写回靠它们做 generation CAS，丢了写回会静默失效。
	selectionRequest.AffinityLookup = &route.AffinityLookup{
		IdentityFP: result.AffinityLookup.IdentityFP,
		Generation: result.AffinityLookup.Generation,
	}
	return true, map[string]any{
		"scopeTag":    scopeTag,
		"sessionId":   sessionID,
		"providerId":  result.Affinity.Provider.ID,
		"matchedFP":   result.Affinity.Hint.MatchedFP,
		"idleSeconds": idle,
		"ttlSeconds":  threshold,
		"note":        "本会话空闲已超过亲和阈值，本次不采信亲和，从全部候选重新选举",
	}
}

// conversationID 取本次请求的会话身份。
//
// 它与落库列 session_id 同源：会话守卫步骤解析出 `SessionResult.SessionID` 后呼叫
// `SetConversationSession` 盖上，与交给 MessageWriter 的 `SessionLookup` 钩子取自同一值。
func (r *ProviderRouter) conversationID() (string, bool) {
	if r == nil || r.sessionID == "" {
		return "", false
	}
	return r.sessionID, true
}

// SetConversationSession 盖上本次请求的会话身份（会话守卫步骤调用）。
//
// **为何不由选路器自己去拿**：会话 id 的提取在会话包，而 session import guard（反向会成环），
// 选路包与选路器都不得反向依赖它；`Deps.SessionLookup` 那个钩子又是**按请求**在装配链之前
// 交给 MessageWriter 的（见 dataplane 的 WithBody 调用），选路器没有对应入口，
// 也不宜新增（本轮文件面不包括 deps.go / dataplane）。
//
// **为何写在视图上而非共享实例上**：调用它的是每请求的选路器副本
// （dataplane 每请求 `Provider.WithBody(...)` 出新副本），故写入不跨请求。
// 如果日后有人不经 WithBody 直接把进程级共享实例接进 Deps，这个字段会跨请求残留：
// 表现是拿别人的会话身份判定空闲（最坏是少粘，不会误报错误），不致命但不难查，登记在此。
func (r *ProviderRouter) SetConversationSession(sessionID string) {
	if r == nil {
		return
	}
	r.sessionID = sessionID
}

// SetConversationBinding 盖上本次请求的会话绑定（会话守卫步骤调用，紧随 SetConversationSession）。
//
// 与 sessionID 同源同纪律：由会话守卫步骤解析后盖上，选路器自己不得反向取会话包
// （session import guard，反向成环）。nil 表示无绑定事实（未接线/Redis 不可用/读取冲突）。
func (r *ProviderRouter) SetConversationBinding(binding *route.SessionBindingSnapshot) {
	if r == nil {
		return
	}
	r.sessionBinding = binding
}

// SetConversationIdentity 盖上本次请求的会话身份来源（会话守卫步骤调用，紧随 SetConversationSession）。
//
// 与 sessionID 同源同纪律。零值（route.SessionIdentityClient）即客户端显式携带，
// 与未接线时的行为一致。
func (r *ProviderRouter) SetConversationIdentity(source route.SessionIdentity) {
	if r == nil {
		return
	}
	r.sessionIdentity = source
}

// now 取选路使用的时钟（与 route.Options.Now 同源，便于用例注入）。
func (r *ProviderRouter) now() time.Time {
	if r.routeOptions.Now != nil {
		return r.routeOptions.Now()
	}
	return time.Now()
}

// WithBody 返回绑定了本次请求正文工厂的选路器视图。
//
// 理由同 MessageWriter.WithBody：Body 是每请求事实（模型名、亲和指纹都从它取），直接改
// 共享字段会让并发请求选出错误的供应商。快照缓存、分组标签缓存与选路器仍是同一份。
func (r *ProviderRouter) WithBody(body BodyFactory) *ProviderRouter {
	if r == nil {
		return nil
	}
	view := *r
	view.Body = body
	return &view
}

// ProviderGroupTag 取选定供应商的分组标签（providers.group_tag）。
//
// 取值不在 pctx 槽位里（只带路由必需字段），故按 id 反查并缓存：这是每请求都要用的
// 维度，不缓存就等于每请求一次供应商查询——那正是 Node 侧修过的性能回归。
func (r *ProviderRouter) ProviderGroupTag(selection pctx.ProviderSelection) string {
	if r == nil || selection.ProviderID == 0 {
		return ""
	}
	if tag, ok := r.tags.Get(selection.ProviderID); ok {
		return tag
	}
	provider, err := r.source.Provider(context.Background(), selection.ProviderID)
	if err != nil || provider == nil {
		if err != nil {
			r.logger.Debug("guard.adapters.group_tag_lookup_failed", map[string]any{
				"providerId": selection.ProviderID,
				"error":      err.Error(),
			})
		}
		return ""
	}
	tag := ""
	if provider.GroupTag != nil {
		tag = *provider.GroupTag
	}
	r.tags.Set(selection.ProviderID, tag)
	return tag
}

// Invalidate 清空分组标签缓存与选路快照（绑定 providers 失效通道）。
func (r *ProviderRouter) Invalidate() {
	r.tags.Clear()
	if r.snapshot != nil {
		r.snapshot.Invalidate()
	}
}

// SnapshotLoads 报告选路快照的装载次数。
func (r *ProviderRouter) SnapshotLoads() int64 {
	if r == nil || r.snapshot == nil {
		return 0
	}
	return r.snapshot.Loads()
}

// formatOf 取客户端格式。
func (r *ProviderRouter) formatOf(req *pctx.Context) convert.ClientFormat {
	if r.Format != nil {
		return r.Format(req)
	}
	return clientFormatOf(req.ProtocolFrom())
}

// groupOf 取有效分组。
//
// 未注入钩子时用 default 而不是空串：空串在 route 里表示「不做分组过滤」，用它会让
// 非默认分组的请求拿到全量候选，属于静默放宽。
func (r *ProviderRouter) groupOf(ctx context.Context, req *pctx.Context) string {
	if r.Group == nil {
		return route.GroupDefault
	}
	group := r.Group(ctx, req)
	if group == "" {
		return route.GroupDefault
	}
	return group
}

// keyIDOf 取密钥 id（亲和 scope tag 用）。
func (r *ProviderRouter) keyIDOf(req *pctx.Context) int64 {
	auth, ok := req.Auth()
	if !ok {
		return 0
	}
	return auth.KeyID
}
