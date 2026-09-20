package dataplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/rectify"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// 构造期错误：数据面缺了必需组件时宁可不启动，也不要用一个静默少一道闸的链服务请求。
var (
	// ErrMissingBaseDeps 表示没有提供基础缝隙（生产装配见 assemble.go）。
	ErrMissingBaseDeps = errors.New("dataplane: 缺少守卫链基础依赖")
)

// statefulConversionStatus 把「状态型字段跨协议线 fail-closed」翻成客户端响应；不是该错误时 ok 为假。
//
// 为什么单独成函数：它是流水线上唯一的 status/message 决策点，抽成纯函数才能被用例钉死
// （否则只能靠整条 HTTP 链路才能覆盖到「到底返回了什么码与什么文案」）。
func statefulConversionStatus(err error) (int, string, bool) {
	field := statefulConversionRejection(err)
	if field == "" {
		return 0, "", false
	}
	return http.StatusBadRequest, statefulConversionMessage(field), true
}

// statefulConversionRejection 取「状态型字段跨协议线 fail-closed」的具体字段名；不是该错误时返回空串。
func statefulConversionRejection(err error) string {
	var rejected *forward.StatefulConversionError
	if errors.As(err, &rejected) {
		return rejected.Field
	}
	return ""
}

// statefulConversionMessage 是给客户端自查用的 400 文案。
//
// 必须点名是哪个字段：客户端要能据此自救（改走同协议供应商，或把历史放进 input），
// 只给一个「请求无效」会把它推回提交者那侧反复试探。文案里不含任何上游主机名或凭据。
func statefulConversionMessage(field string) string {
	return "请求字段 " + field + " 依赖服务端会话状态，而本次路由到的供应商协议线无法承载：" +
		"请改走同协议供应商，或把上下文放进 input 后重试"
}

// SelectionFacts 是一次选路所需的请求级事实。
type SelectionFacts struct {
	// Model 是原始请求模型（来自已解析的正文）。
	Model string
	// Format 是客户端入站格式。
	Format convert.ClientFormat
	// Group 是有效分组（密钥级 > 用户级 > 默认）。
	Group string
	// ProviderGroupTag 是选中供应商的分组标签（providers.group_tag）。
	// 它与 Group 是两个不同的东西：前者是供应商被归入哪些组，后者是本请求属于哪个组；
	// 计费要用两者求交（见 cost.go 的 billingProviderGroups）。
	ProviderGroupTag string
	// KeyID 是密钥 id，用于亲和 scope tag。
	KeyID int64
	// ExcludeIDs 是故障转移排除列表。
	ExcludeIDs []int64
	// Body 是已解析的正文，用于亲和指纹与模型白名单判定。
	Body map[string]any
	// RawPassthrough 为真表示本次路由属原始透传端点（Node 的 raw_passthrough）。
	//
	// 它只影响候选的「允许重试/切换」取值：转换与改写由路径映射决定（见 routes.go）。
	RawPassthrough bool
}

// CandidateSource 把一次已选中的供应商投影成转发候选（含密钥、端点与超时口径）。
type CandidateSource interface {
	// Candidate 按选路结果取候选；返回的 route.Result 仅用于落 provider_chain。
	Candidate(ctx context.Context, selection pctx.ProviderSelection, facts SelectionFacts) (*forward.Candidate, route.Result, error)
	// Failover 在失败切换时选出下一个候选；无候选时返回 (nil, Result{}, nil)。
	Failover(ctx context.Context, facts SelectionFacts) (*forward.Candidate, route.Result, error)
}

// Settler 把一次请求的终态事实落库。两个方法分别对应非流式与流式两条终态路径。
type Settler interface {
	// NonStream 承接 forward.Deps.Settle；整次转发只调用一次。
	NonStream(ctx context.Context, pc *pctx.Context, result *forward.Result, failure *forward.Failure) error
	// Stream 承接 forward.StreamSettler；整条流只调用一次。
	Stream(ctx context.Context, pc *pctx.Context, outcome forward.StreamOutcome) error
}

// SettlerFor 按请求给出结算器。
//
// 为什么按请求而不是进程级：终态列（provider_chain、TTFT、模型）里有大量本次请求的留痕，
// 它们不经过 pctx。生产实现在 assemble.go 里把这些留痕绑进结算器。
type SettlerFor func(state *RequestState) Settler

// RequestState 是一次请求在数据面内的留痕。
type RequestState struct {
	// PC 是本次请求的上下文。
	PC *pctx.Context
	// Format 是客户端入站格式。
	Format convert.ClientFormat
	// StartedAt 是请求进入数据面的时刻。
	StartedAt time.Time
	// trace 收集**链词表装不下**的事实（当前是竞速饱和），供 routing_trace 列使用。
	//
	// 为什么挂在请求状态上而不是全局：饱和事件属于本次请求的竞速过程；
	// 结算时再回读它（见 storeSettler 的 buildRoutingTrace 调用点）。
	trace *routingTraceRecorder
	// selections 记录每次尝试命中的选路留痕，键为供应商 id；落 provider_chain 用。
	// 必须经 recordSelection / selectionFor 访问：竞速的候选是由阈值计时器的协程选的
	// （forward 的 triggerThreshold 在 time.AfterFunc 里跑），而读它的除了终态协程，
	// 还有输家计费协程。
	selectionMu sync.Mutex
	selections  map[int64]route.Result
	// Model 是客户端请求里的原始模型名（计费的备选基准）。
	Model string
	// ProviderGroupTag 是选中供应商的分组标签（providers.group_tag）。
	//	计费要拿它与用户分组求交，故在选路时一次性取回，不在结算路径上重查。
	ProviderGroupTag string
	// UserGroup 是本次请求的有效分组（密钥级 > 用户级 > 默认）。
	UserGroup string
	// ProviderMultiplier 是选中供应商的成本倍率（numeric 列的数值投影）。
	ProviderMultiplier *float64
	// GroupMultiplier 是用户分组倍率；nil 表示未接线（按 1 计）。
	GroupMultiplier *float64
	// replay 是本请求的回放接线（owner claim 与 spool）；未接线时为 nil。
	replay *replaySession
	// telemetry 是本请求的会话观测租约（守卫链通过后开启）；未接线时为零值。
	telemetry TelemetryLease
	// sessionID 是本次请求绑定的物理会话 id（守卫链的会话步骤赋值），供错误体挂
	// cch_session_id 与后续读取共用一份；空串表示本请求没有会话身份。
	sessionID string
	// responseCapture 是响应正文的**有界头尾捕获**；nil 表示本请求不落正文
	// （高并发模式、STORE_SESSION_RESPONSE_BODY 关闭或未接线）。
	//
	// 它只由交付出客户端字节的那条 goroutine 写（pumpStream 的 emit / writeForwardResult）。
	responseCapture *session.ResponseCapture
	// clientHeaders 是客户端请求头的**脱敏副本**（request.before 快照用）；nil 表示未采集。
	clientHeaders map[string]string
	// requestedEffort 是客户端请求侧的思考强度（守卫链之后采集），终态探针用它做对照。
	requestedEffort specialsettings.EffortRequest
	// requestedServiceTier 是客户端请求侧的 codex service_tier（同一处采集）：
	// codex priority 计费档用它判定（见 codex_priority_billing.go）。
	requestedServiceTier string
	// requestSnapshotBody/Messages 是客户端正文的请求前快照（与请求工件同一份数据）。
	requestSnapshotBody     map[string]any
	requestSnapshotMessages any
	requestSnapshotHasMsgs  bool
	// upstreamURL/upstreamMethod 是最终发往的上游（request.after 的 meta）。
	upstreamURL    string
	upstreamMethod string
	// upstreamResponseHeaders/upstreamStatus 是上游原始响应头与状态码（response.before）。
	upstreamResponseHeaders http.Header
	upstreamStatus          int
	// deliveredResponseHeaders 是**已交付客户端**的响应头（response.after）。
	deliveredResponseHeaders map[string]string
	// responseStatus 是回给客户端的状态码（response.after 的 meta）。
	responseStatus int
	// failureStatus 是未走到交付路径时的归因状态码（守卫抢答/失败归因）。
	failureStatus int
	// rectifierAudits 是整流器产生的审计条目（守卫链阶段的 responses `input` 归一 + 转发阶段的
	// 被动/主动整流），按产生顺序落库。
	//
	// 为什么要攒而不是即时写：审计走 `message_request.special_settings` 的 **jsonb 追加**
	// （存明细的建行与终态两个时刻），终态追加只有一次写入（见 store.DetailsPatch），
	// 故整流条目与探针条目要在终态合并成同一个数组（见 specialSettingsAppendEntries）。
	//
	// 锁：写入发生在请求 goroutine（守卫链与尝试循环），读发生在流终态 goroutine，
	// 故与 selections 同样加锁（此处只有追加与整取快照两个动作）。
	rectifierMu     sync.Mutex
	rectifierAudits []map[string]any
}

// appendRectifierAudit 记一条整流器审计条目。
func (s *RequestState) appendRectifierAudit(entry map[string]any) {
	if s == nil || entry == nil {
		return
	}
	s.rectifierMu.Lock()
	defer s.rectifierMu.Unlock()
	s.rectifierAudits = append(s.rectifierAudits, entry)
}

// rectifierAuditsSnapshot 取整流器审计条目的快照，供终态追加。
func (s *RequestState) rectifierAuditsSnapshot() []map[string]any {
	if s == nil {
		return nil
	}
	s.rectifierMu.Lock()
	defer s.rectifierMu.Unlock()
	if len(s.rectifierAudits) == 0 {
		return nil
	}
	return append([]map[string]any(nil), s.rectifierAudits...)
}

// Options 是数据面装配参数。除 Base 外都可为空，空值语义逐个写在字段上。
type Options struct {
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Base 是守卫链的基础缝隙（生产装配：guard.Adapters.Apply 的结果）。
	Base guard.Deps
	// Adapters 是真实适配器集合；非 nil 时按请求注入正文工厂（见 guard 的 WithBody 视图）。
	Adapters *guard.Adapters
	// Candidates 是候选供应商投影，必填（生产装配见 storeCandidateSource）。
	Candidates CandidateSource
	// Settlers 给出结算器，必填。
	Settlers SettlerFor
	// Forward 是转发主干依赖（Dial 必填）。
	Forward forward.Deps
	// Stream 是流式路径的基础配置（Format / ForceGate / Settle 由本包按请求填）。
	Stream forward.StreamOptions
	// BodyOptions 是入站正文的解压与限额参数；零值时用 ingress 默认（不做在途准入）。
	BodyOptions guard.BodyAccessOptions
	// ClientIP 解析客户端 IP；nil 时用 RemoteAddr 与 X-Forwarded-For 的首跳。
	ClientIP func(*http.Request) string
	// EffectiveGroup 取本次请求的有效分组（密钥级 > 用户级 > 默认）。
	// 它同时是选路过滤的分组与计费分组的来源；nil 时退化为「用选中供应商的分组标签」
	// （仅适配无认证态的测试装配，生产装配恒非 nil）。
	EffectiveGroup func(*pctx.Context) string
	// Fallback 是未实现路由的落点（生产：前门的 Node 回退）。nil 时未实现路由返回 404。
	Fallback http.Handler
	// 流式竞速的配置缝（nil 表示本进程未接线竞速：一律走串行）。
	Hedge HedgeWiring
	// Now 可注入时钟；nil 时用 time.Now。
	Now func() time.Time
	// Replay 是回放接线（命中短路 + owner spool）。nil 时 replayAttach 步骤整体跳过，
	// 请求照常转发（与 Node 关闭 ENABLE_REQUEST_REPLAY 同义）。
	Replay *ReplayWiring
	// Telemetry 是会话观测写侧（观察 ZSET、session:{id}:info、并发计数、请求工件）。
	//
	// nil 时整个观测面不写：这是显式的接线缺口（不是「不需要」）——不写它，管理面的
	// 会话列表在纯 Go 世界里恒空、并发数恒 0，而 UI 不会报错。生产装配见 assemble.go。
	Telemetry SessionTelemetry
	// SessionArtifacts 是会话工件的开关与体积上限（STORE_SESSION_MESSAGES /
	// STORE_SESSION_RESPONSE_BODY / 体积上限）。响应侧捕获与写侧共用同一份选项。
	SessionArtifacts session.SessionArtifactOptions
	// ModelCatalog 是聚合式模型列表（`/v1/models` 一族）的供应商视图。
	//
	// nil 表示**本进程不注册这五条路由**（它们照旧回退 Node）：聚合端点要读全量供应商、
	// 分组与系统时区，这些只存在于存储层；没有它就只能把上游某家的模型列表原样透传，
	// 那是错的行为，不如不接管。生产装配点见 assemble.go 的 Options 构造处。
	ModelCatalog ModelCatalog
	// ResponseFix 是响应修复器的接线面（`enable_response_fixer` 的实际消费方）。
	//
	// 零值表示审计不落库：正文仍按设置修复，只是修复事实不留痕（见 ResponseFixWiring）。
	ResponseFix ResponseFixWiring
	// PlaceholderThinkingSignature 是响应侧占位思考签名的开关
	// （CCH_THINKING_SIGNATURE_PLACEHOLDER）：给来自非 Anthropic 上游、没有签名的思考块
	// 补一个占位签名，让 Anthropic 客户端愿意显示它。语义见 convert/thinking_placeholder.go。
	//
	// 零值即关（与 env「未设置即开」不同）：默认值由 config 层给出，装配侧只搬运。
	PlaceholderThinkingSignature bool
	// SettlementBarrier 是「这次请求的终态是否已真正落库」的等待面。
	//
	// nil（默认）表示终态写是同步的：Settle 返回即已落库，等待面上没有额外的事要做，
	// 行为与接线前逐字一致。非 nil 表示 MESSAGE_REQUEST_WRITE_MODE=async：Settle 只入队，
	// 而「已交付客户端但终态未落库」的计数必须等到队列 flush 完成才能归零——
	// 退出序列正是靠它判断能否安全关连接池。
	SettlementBarrier SettlementBarrier
}

// SettlementBarrier 是异步终态写队列的等待面（由 terminal.WriteQueue 实现）。
type SettlementBarrier interface {
	// AwaitSettlement 等 id 这一行的终态**真正落库**。三种结论：
	// nil（已落库或本就不在队列里）、写入失败的错误（该行未落库，不得当排空完成）、
	// ctx 的错误（等待超时，是否落库未知）。
	AwaitSettlement(ctx context.Context, id int64) error
}

// SettlementFlusher 是异步终态写队列的冲刷面：退出序列在关连接池之前先冲干净再停。
type SettlementFlusher interface {
	// FlushSettlements 立刻写入已入队但尚未落库的记录，并等到队列清空。
	FlushSettlements(ctx context.Context) error
	// StopSettlements 停队列（此后的入队一律退回同步写）。
	StopSettlements()
}

// SettlementBacklog 是异步终态写队列的积压视图（可选）。
//
// 退出序列把它并入待落库数：流路径的积压已由结算跟踪器盖住，而非流路径（拦截类终态等）
// 入队后没有跟踪器，不并进来的话日志会报 0，而队列里其实还躺着没写的终态。
type SettlementBacklog interface {
	PendingSettlements() int64
}

// Handler 是 `/v1` 数据面处理器。并发安全：所有可变状态都是每请求本地的。
type Handler struct {
	options     Options
	logger      *logx.Logger
	settlements *settlementTracker
}

// New 装配数据面处理器。
func New(options Options) (*Handler, error) {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	if options.Candidates == nil {
		return nil, fmt.Errorf("%w: Candidates 未提供", ErrMissingBaseDeps)
	}
	if options.Settlers == nil {
		return nil, fmt.Errorf("%w: Settlers 未提供", ErrMissingBaseDeps)
	}
	if options.Base.Auth == nil || options.Base.Users == nil || options.Base.Provider == nil {
		return nil, fmt.Errorf("%w: Auth/Users/Provider 未接线", ErrMissingBaseDeps)
	}
	if options.Forward.Dial == nil {
		return nil, fmt.Errorf("%w: Forward.Dial 未提供", ErrMissingBaseDeps)
	}
	if options.ClientIP == nil {
		options.ClientIP = defaultClientIP
	}
	return &Handler{options: options, logger: logger, settlements: newSettlementTracker()}, nil
}

// PendingSettlements 返回「已交付客户端但终态尚未落库」的流数，并**并入异步终态写队列
// 的积压**（未装配队列即同步写，与接线前逐字一致）。
//
// 退出序列据此如实上报：这个数不为 0 时关依赖（尤其是关连接池）会永久丢掉这些终态。
func (h *Handler) PendingSettlements() int64 {
	pending := h.settlements.pending()
	if backlog, ok := h.options.SettlementBarrier.(SettlementBacklog); ok {
		pending += backlog.PendingSettlements()
	}
	return pending
}

// WaitSettlements 等到没有待落库终态；ctx 先结束返回 false（此时不得当作排空完成）。
func (h *Handler) WaitSettlements(ctx context.Context) bool { return h.settlements.waitEmpty(ctx) }

// FlushSettlements 把异步终态写队列里已入队但尚未落库的记录立刻写掉并等到队列清空。
//
// 同步模式（未装配 barrier）与队列不支持冲刷面时是 no-op：那时没有队列可冲。
func (h *Handler) FlushSettlements(ctx context.Context) error {
	flusher, ok := h.options.SettlementBarrier.(SettlementFlusher)
	if !ok {
		return nil
	}
	return flusher.FlushSettlements(ctx)
}

// StopSettlements 停异步终态写队列。它排在关连接池之前（见 cmd/cchd 的退出序列）：
// 队列的 worker 仍在写库，先关池会让这些写直接报错。
func (h *Handler) StopSettlements() {
	flusher, ok := h.options.SettlementBarrier.(SettlementFlusher)
	if !ok {
		return
	}
	flusher.StopSettlements()
}

// ServeHTTP 承载一条数据面请求。
//
// 顺序即契约：路由查表 → 建上下文（不读正文）→ 跑守卫链（鉴权先于解压）→ 选路 → 转发 →
// 写响应；任一步的失败都翻译成 Node 一致形状的错误响应，绝不把内部错误原样吐给客户端。
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// 聚合式模型列表先判：它们的形状与开行语义与其它端点完全不同（不转发、不开行、
	// 只做一次认证），故不进下面的守卫链路径。未接线上游目录时不接管（回退 Node）。
	if spec, ok := matchModelListRoute(strings.ToUpper(request.Method), strings.ToLower(egress.NormalizePath(request.URL.Path))); ok &&
		h.options.ModelCatalog != nil {
		h.serveModelListRequest(writer, request, spec)
		return
	}
	spec, ok := matchRoute(request.Method, request.URL.Path)
	if !ok {
		h.fallback(writer, request)
		return
	}

	startedAt := nowOr(h.options.Now)
	// 每请求一个可取消上下文：客户端断开时它先取消，转发与拨号据此收敛。
	requestCtx, cancel := context.WithCancel(request.Context())
	defer cancel()

	pc, err := pctx.New(pctx.Init{
		Method:       request.Method,
		Path:         request.URL.Path,
		Query:        request.URL.RawQuery,
		Headers:      request.Header,
		Body:         request.Body,
		ClientIP:     h.options.ClientIP(request),
		ProtocolFrom: spec.Family,
		Logger:       h.logger,
		Now:          h.options.Now,
	})
	if err != nil {
		h.writeGuardResponse(writer, nil, guard.BuildError(http.StatusBadRequest, "请求上下文构造失败", ""))
		return
	}
	if err := pc.SetOwner(egress.OwnerGo); err != nil {
		// 归属只能决定一次：前门已经判过，重复判定说明装配出错。按冲突处理而不是继续。
		h.logger.Warn("dataplane.owner_conflict", map[string]any{"path": request.URL.Path, "error": err.Error()})
	}

	state := &RequestState{
		PC:         pc,
		Format:     spec.Format,
		StartedAt:  startedAt,
		selections: map[int64]route.Result{},
		trace:      newRoutingTraceRecorder(startedAt),
	}

	// 正文访问器按需构造：第一次有人要正文时读体（于是解压发生在鉴权之后——鉴权是链的首步）。
	body := newBodyAccess(requestCtx, pc, h.options.BodyOptions)

	deps := h.options.Base
	deps.Body = body.factory
	// responses 路由的 `input` 归一（主动型整流器）：Node 在 guard pipeline **之前**归一
	// （proxy-handler.ts:113），使过滤器、建行审计与转换器看到同一份形状。
	// 只在 /v1/responses 生效（Node 的判定就是 originalFormat === "response"）。
	if spec.Format == convert.FormatResponse {
		deps.Body = wrapResponsesInputBodyFactory(deps.Body, state, func() rectify.Switches {
			return rectifySwitches(requestCtx, h.options.Base.Settings, h.logger)
		}, h.logger)
	}
	deps.RequestContext = func(*pctx.Context) context.Context { return requestCtx }
	// 端点因子（Node session.getEndpointPolicy().allowRawCrossProviderFallback）：只有原始透传
	// 端点（count_tokens / responses/compact）为真，源与转发候选取的是同一个 spec.RawPassthrough
	// （见 routes.go 的族表），故不另建一份路径表。会话守卫拿它乘系统设置才是
	// Node `isRawCrossProviderFallbackEnabled()` 的完整值。
	deps.EndpointRawPassthrough = spec.RawPassthrough
	// 会话绑定结果是每请求事实，而 pctx 刻意不带会话状态：这里用「按请求的记录视图」把它
	// 交给请求日志开行（messageContext 步骤），不引入按上下文索引的全局表。
	sessions := newSessionCapture(deps.Sessions)
	deps.Sessions = sessions
	deps.SessionLookup = sessions.lookup
	// 限流：进程级实例共享窗口与并发记账状态，但并发额度的原子记账必须用**本次请求**的
	// 物理会话 id（会话步骤排在限流步骤之前，结果就在上面那个视图里）。
	bindSessionLimiter(&deps, sessions)
	// 回放同理按请求构造：身份来自本请求过滤后的正文，owner claim 也只能属于本请求。
	if session := newReplaySession(requestCtx, h.options.Replay, pc, body, spec.Format, h.logger); session != nil {
		state.replay = session
		deps.Replay = session
	}
	if h.options.Adapters != nil {
		// 适配器是进程级共享实例，正文与会话查询是每请求事实：用视图注入，不改共享字段。
		if h.options.Adapters.Provider != nil {
			deps.Provider = h.options.Adapters.Provider.WithBody(body.factory)
		}
		if h.options.Adapters.Message != nil {
			deps.MessageContext = h.options.Adapters.Message.WithBody(body.factory, deps.SessionLookup)
		}
	}
	chain, err := guard.Assemble(deps, spec.Policy)
	if err != nil {
		h.logger.Error("dataplane.chain_build_failed", map[string]any{"path": request.URL.Path, "error": err.Error()})
		h.writeGuardResponse(writer, state, guard.BuildError(http.StatusInternalServerError, "守卫链装配失败", ""))
		return
	}

	response, err := chain.Run(pc)
	// 会话身份在链上已定（会话步骤早于限流与选路），此刻取一次留给错误体与会话观测共用。
	if bound, ok := sessions.lookup(pc); ok {
		state.sessionID = bound.SessionID
	}
	if err != nil {
		// 无可用供应商**不是步骤失败**：Node 的 resolver 返回 null 后继续走到转发层，
		// 由转发层给出 503 `no_available_providers`（生产实测与 Node 逐字段对照得来）。
		// 若并入下面的 500，客户端会看到 internal_server_error 而非可重试语义。
		if errors.Is(err, guard.ErrNoProviderAvailable) {
			fields := map[string]any{"path": request.URL.Path}
			// 归因：响应契约不可动（Node 同形），但日志必须能把两种成因分开——
			// 「模型没人支持」与「供应商全不可用」此前共用这一条 503，用户只能靠猜。
			// 事件名保持可聚合的 dataplane.no_provider_available，成因放在 diagnostic.cause。
			var noProvider *guard.NoProviderError
			if errors.As(err, &noProvider) {
				diagnostic := noProvider.Diagnostic()
				fields["model"] = diagnostic.RequestedModel
				fields["clientFormat"] = diagnostic.ClientFormat
				fields["cause"] = diagnostic.Cause
				fields["modelSupportedProviders"] = diagnostic.ModelSupportedProviders
				fields["totalProviders"] = diagnostic.TotalProviders
				fields["reasonCounts"] = diagnostic.ReasonCountsLine()
				fields["summary"] = diagnostic.Summary()
			}
			h.logger.Warn("dataplane.no_provider_available", fields)
			h.settleFailure(requestCtx, state, http.StatusServiceUnavailable, "无可用供应商")
			// 详细体只在 verbose_provider_error 打开时给出；关闭时是逐字节固定的简洁体
			// （改造前的行为，也是 Node 的默认支，见 no_provider_verbose.go）。
			response := guard.BuildError(
				http.StatusServiceUnavailable, "No available providers", "no_available_providers",
			)
			if noProvider != nil && h.verboseProviderError(requestCtx) {
				response = verboseNoProviderResponse(noProvider.Diagnostic())
			}
			h.writeGuardResponse(writer, state, response)
			return
		}
		// 步骤自身失败（不是拦截）：Node 同样翻成 500，只是文案更具体。
		h.logger.Error("dataplane.guard_step_failed", map[string]any{
			"path":  request.URL.Path,
			"error": err.Error(),
		})
		h.writeGuardResponse(writer, state, guard.BuildError(http.StatusInternalServerError, "请求处理失败", ""))
		return
	}
	if response != nil {
		h.writeGuardResponse(writer, state, response)
		return
	}

	// 会话观测在这之后开启：链上早退（限流拦截、预热抢答等）不算一次真实会话，
	// 与 Node 的 `!warmupMaybeIntercepted` 排除同判。收尾用 defer 而不是在每条
	// 返回路径上都写一遍——它必须在成功、失败、客户端断开三条路上都执行（计数不能泄漏）。
	//
	// 同时在这里（**守卫链之后**，鉴权已完成）取一次客户端请求侧的思考强度：
	// 它要进终态探针（`thinking_effort_forwarded`）与客户端侧审计做对照。
	// 不放在请求入口是因为读体即解压：必须等鉴权通过，与上面「正文访问器按需构造」同一条纪律。
	h.captureRequestedEffort(state, spec, body)
	captureRequestedServiceTier(state, body)
	lease := h.startTelemetry(requestCtx, state, spec, body, sessions, clientRequestURL(request))
	state.telemetry = lease
	// 响应正文的有界捕获：开关与上限都在装配时定，这里只判「本请求要不要捕」。
	//
	// 它是**旁路**捕获：只记录已交给客户端的字节，不参与交付路径，也不缓冲整流。
	state.responseCapture = h.newResponseCapture(state)
	// request.before 相位快照的头来自**客户端原始头**（Node 的 filterClientRequestSnapshotHeaders
	// 在守卫链里采集），正文与 messages 复用请求工件那一份（同一批数据，不另存一次）。
	state.clientHeaders = h.clientSnapshotHeaders(request)
	h.captureRequestSnapshot(state, body)
	defer h.finishResponseArtifacts(state, lease)
	defer h.finishTelemetry(state, lease)

	h.forward(writer, request, requestCtx, state, spec, body, deps)
}

// forward 执行选路后的转发，并把结果写成客户端响应。
func (h *Handler) forward(
	writer http.ResponseWriter,
	request *http.Request,
	requestCtx context.Context,
	state *RequestState,
	spec routeSpec,
	body *bodyAccess,
	deps guard.Deps,
) {
	pc := state.PC
	selection, ok := pc.Provider()
	if !ok || selection.ProviderID == 0 {
		// Node 在无可用供应商时返回 503（provider === null 分支）。
		h.settleFailure(requestCtx, state, http.StatusServiceUnavailable, "无可用供应商")
		h.writeGuardResponse(writer, state, guard.BuildError(
			http.StatusServiceUnavailable, "无可用供应商", "",
		))
		return
	}

	facts := h.selectionFacts(state, body, deps)
	// 原始透传端点的「不重试、不切换」由候选携带：它同时是 forward 的竞速前置条件，
	// 两处必须取同一个来源（见 routes.go 的 RawPassthrough 注释）。
	facts.RawPassthrough = spec.RawPassthrough
	candidate, capture, err := h.options.Candidates.Candidate(requestCtx, selection, facts)
	if err != nil || candidate == nil {
		h.logger.Error("dataplane.candidate_failed", map[string]any{
			"providerId": selection.ProviderID,
			"error":      errorText(err),
		})
		h.settleFailure(requestCtx, state, http.StatusServiceUnavailable, "供应商候选不可用")
		h.writeGuardResponse(writer, state, guard.BuildError(
			http.StatusServiceUnavailable, "供应商候选不可用", "",
		))
		return
	}
	state.recordSelection(selection.ProviderID, capture)
	state.ProviderGroupTag = facts.ProviderGroupTag
	state.UserGroup = facts.Group
	state.ProviderMultiplier = costMultiplierOf(capture)
	// 会话详情里的供应商字段此刻才存在（Node 在 forwarder 选中供应商后写），用与观测
	// 上下文同源的请求上下文写：这条不受客户端断开影响（转发才开始）。
	if h.options.Telemetry != nil && state.telemetry.Identity != "" {
		h.options.Telemetry.ProviderSelected(requestCtx, state.telemetry, selection.ProviderID, candidate.Provider.Name)
	}

	fwd := h.options.Forward
	// 整流器开关：六个 enable_*_rectifier，回落到 Node 默认（全开）的语义见 rectifySwitches。
	fwd.RectifySwitches = func(ctx context.Context) rectify.Switches {
		return rectifySwitches(ctx, h.options.Base.Settings, h.logger)
	}
	fwd.Settle = func(ctx context.Context, target *pctx.Context, result *forward.Result, failure *forward.Failure) error {
		return h.options.Settlers(state).NonStream(ctx, target, result, failure)
	}
	fwd.Select = func(ctx context.Context, excludeIDs []int64) (*forward.Candidate, error) {
		nextFacts := facts
		nextFacts.ExcludeIDs = excludeIDs
		next, nextCapture, selectErr := h.options.Candidates.Failover(ctx, nextFacts)
		if selectErr != nil {
			return nil, selectErr
		}
		if next == nil {
			return nil, nil
		}
		state.recordSelection(next.Provider.ID, nextCapture)
		// 故障转移后的倍率取**新的**选中供应商：计费基准是实际服务本次请求的那家。
		state.ProviderMultiplier = costMultiplierOf(nextCapture)
		return next, nil
	}
	fwd.Facts = h.planFacts(pc, spec, body)
	// 整流器审计的落点：与 responses `input` 归一共用同一份条目集合，终态一次追加（见
	// specialSettingsAppendEntries）。用回调而不是让 forward 依赖请求状态，保持转发层
	// 只做协议与重试。
	fwd.RectifierAudit = state.appendRectifierAudit
	// 计费的备选基准是**客户端请求的**模型名：重定向后的名字由计划决定，两者都在结算时
	// 才用得上，故在此一次性捕获（结算发生在响应之后，那时正文已不可读）。
	state.Model = fwd.Facts.Client.Model

	streamOptions := h.options.Stream
	streamOptions.Format = spec.Format
	streamOptions.ForceGate = spec.ForceGate
	// 门控模式逐请求解析：设置快照优先、env 兜底，回放 owner 强制 enforce
	// （见 gate_mode.go 的判据与出处）。
	streamOptions.GateMode = h.gateModeForRequest(requestCtx, state)
	streamOptions.StartedAt = state.StartedAt
	streamOptions.Settle = streamSettler{handler: h, state: state}

	result, err := h.forwardStream(requestCtx, pc, candidate, fwd, streamOptions, spec, state)
	if result == nil {
		h.logger.Error("dataplane.forward_failed", map[string]any{"error": errorText(err)})
		status, message := h.failoverStatusFor(requestCtx, errorFailure(nil, err))
		// 状态型字段跨线无承载属**客户端请求**的问题（不是上游/网关故障）：计划阶段就已 fail-closed，
		// 且 attempt 对计划错误不重试、不换供应商。故这里翻成 400 并把字段名交给客户端自查，
		// 不走「上游不可用」类文案。
		if mappedStatus, mappedMessage, ok := statefulConversionStatus(err); ok {
			status, message = mappedStatus, mappedMessage
		}
		h.settleFailure(requestCtx, state, status, message)
		h.writeGuardResponse(writer, state, guard.BuildError(status, message, ""))
		return
	}
	if result.Stream != nil {
		// 流已提交：正文由 forward 的门控与泵按需交付，本包只负责写与 flush。
		h.recordStreamUpstream(state, result)
		h.pumpStream(writer, request, result, state)
		return
	}
	if err != nil {
		// 全部尝试耗尽：终态已由 forward 的结算缝落库，这里只把最后归因翻成响应。
		status, message := h.failoverStatusFor(requestCtx, errorFailure(&result.Result, err))
		// 没走到交付路径也要记归因码：否则 response.after 的 meta 会缺 statusCode，
		// 详情页会把「失败的请求」显示成「没有响应」。
		h.recordFailureStatus(state, status)
		h.recordUpstream(state, planViewOf(&result.Result), result.Headers)
		h.writeGuardResponse(writer, state, guard.BuildError(status, message, ""))
		return
	}
	h.recordUpstream(state, planViewOf(&result.Result), result.Headers)
	h.writeForwardResult(writer, requestCtx, result, state)
}

// selectionFacts 取本次选路所需的请求级事实。
func (h *Handler) selectionFacts(state *RequestState, body *bodyAccess, deps guard.Deps) SelectionFacts {
	facts := SelectionFacts{Format: state.Format}
	if auth, ok := state.PC.Auth(); ok {
		facts.KeyID = auth.KeyID
	}
	if body != nil {
		if tree, err := body.json(); err == nil {
			facts.Body = tree
			facts.Model = bodyModel(tree)
		}
	}
	if deps.ProviderGroupTag != nil {
		if selection, ok := state.PC.Provider(); ok {
			facts.ProviderGroupTag = deps.ProviderGroupTag(selection)
		}
	}
	// 选路过滤与计费都用**有效分组**（密钥级 > 用户级 > 默认）：route 侧拿到该值时是拿它
	// 与供应商的 group_tag 求交，故不能用供应商自己的标签反过来当用户分组（那会让故障转移
	// 按「上一家的分组」过滤候选）。未接上该钩子的测试装配退化为供应商标签。
	facts.Group = facts.ProviderGroupTag
	if h.options.EffectiveGroup != nil {
		if effective := h.options.EffectiveGroup(state.PC); effective != "" {
			facts.Group = effective
		}
	}
	return facts
}

// planFacts 构造转发计划所需的会话级事实。
func (h *Handler) planFacts(pc *pctx.Context, spec routeSpec, body *bodyAccess) forward.PlanFacts {
	headers := pc.Headers().Clone()
	payload, _ := body.bytes()
	model := ""
	if tree, err := body.json(); err == nil {
		model = bodyModel(tree)
	}
	return forward.PlanFacts{
		Client: forward.ClientRequest{
			Method:  pc.Method(),
			Path:    pc.Path(),
			Query:   pc.Query(),
			Headers: headers,
			Format:  spec.Format,
			Model:   model,
			Body:    payload,
			HasBody: len(payload) > 0,
			// 网关注入的正文键要在转换层排除：它们是网关为同线上游缓存补的，不是客户端约束。
			GatewayInjectedBodyFields: pc.GatewayInjectedBodyFields(),
		},
		ClientUserAgent: headers.Get("User-Agent"),
		// 供应商级参数覆写：每请求一份——gemini 的覆写要看客户端路径，而审计条目与缓存 TTL
		// 都是本次请求的产物（见 forward.ProviderOverrideApplier）。
		Overrides: forward.NewProviderOverrideApplier(pc.Path()),
	}
}

// settleFailure 在没有转发的失败路径上补一次终态入账（有转发时由 forward 负责，绝不重复）。
func (h *Handler) settleFailure(ctx context.Context, state *RequestState, status int, message string) {
	if _, settled := state.PC.Settlement(); settled {
		return
	}
	failure := &forward.Failure{StatusCode: status, Internal: true, Message: message}
	if err := h.options.Settlers(state).NonStream(ctx, state.PC, &forward.Result{
		StatusCode: status,
		StartedAt:  state.StartedAt,
		EndedAt:    nowOr(h.options.Now),
	}, failure); err != nil {
		h.logger.Warn("dataplane.settle_failed", map[string]any{"error": err.Error()})
	}
}

// streamSettler 把流式终态交给按请求绑定的结算器。
type streamSettler struct {
	handler *Handler
	state   *RequestState
}

// SettleStream 实现 forward.StreamSettler。
//
// 次序即回放的终态屏障：先落账（计费），再允许 completed 出现。反过来会出现「缓存里已有
// completed 条目、账本行却仍未终态」的窗口，重放命中时账目就少一行。
func (s streamSettler) SettleStream(ctx context.Context, outcome forward.StreamOutcome) error {
	if s.handler == nil {
		return nil
	}
	err := s.handler.options.Settlers(s.state).Stream(ctx, s.state.PC, outcome)
	// 无论结算成败都收尾回放：失败终态必须 abort（绝不让半截流被重放命中），
	// 而结算失败本身已有自己的告警路径。
	s.state.replay.completeAfterSettle(ctx, outcome)
	return err
}

// fallback 把未实现的路由交回 Node；没有回退目标时如实 404。
func (h *Handler) fallback(writer http.ResponseWriter, request *http.Request) {
	if h.options.Fallback == nil {
		// 未承载的路由没有会话步骤，故没有会话 id 可挂。
		h.writeGuardResponse(writer, nil, guard.BuildErrorWithDetails(
			http.StatusNotFound, "本进程未承载该路由", "",
			map[string]any{"path": request.URL.Path}, "",
		))
		return
	}
	h.options.Fallback.ServeHTTP(writer, request)
}

// bodyModel 复刻 ProxySession.request.model 的取法（含 gemini 的 request.model 嵌套）。
func bodyModel(body map[string]any) string {
	if body == nil {
		return ""
	}
	if model, ok := body["model"].(string); ok {
		return model
	}
	if nested, ok := body["request"].(map[string]any); ok {
		if model, ok := nested["model"].(string); ok {
			return model
		}
	}
	return ""
}

// defaultClientIP 取 X-Forwarded-For 首跳，退化到 RemoteAddr。
//
// 与 Node 的 ip_extraction_config 信任链不同：那是入口与出口包的职责（含可信代理判定），
// 本函数只是「入口未接线」时的保守取值，绝不把它当作用户身份依据。
func defaultClientIP(request *http.Request) string {
	if forwarded := request.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first := strings.TrimSpace(strings.Split(forwarded, ",")[0]); first != "" {
			return first
		}
	}
	host, _, err := splitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	return host
}

// errorText 安全取错误文案。
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// errOrNil 把「可能为 nil 的 error」归一为空串。
func splitHostPort(address string) (string, string, error) {
	index := strings.LastIndexByte(address, ':')
	if index < 0 {
		return address, "", errors.New("dataplane: 地址不含端口")
	}
	return address[:index], address[index+1:], nil
}

// limitSessionBinder 是限流实现**可选**支持的「按请求会话来源」视图。
//
// 用局部接口而不是 import 具体限流包：数据面不该依赖某个实现，而这条能力是增强项——
// 不支持时退化为「现场生成会话 id + warn」，即今日行为（参见 limit.Config.SessionID）。
type limitSessionBinder interface {
	WithSession(func(*pctx.Context) (string, error)) guard.RateLimiter
}

// bindSessionLimiter 把本请求的会话来源注入限流实现（与 Provider/Message 的 WithBody 同法：
// 进程级共享实例不动，每请求注入一个视图）。
//
// 之所以要这层：并发会话额度记账得用**同一个物理会话 id** 才有原子性，而会话绑定结果由
// sessionCapture 每请求自持——限流服务自己看不到它。
func bindSessionLimiter(deps *guard.Deps, sessions *sessionCapture) {
	if deps == nil || deps.RateLimit == nil {
		return
	}
	if bound, ok := deps.RateLimit.(limitSessionBinder); ok {
		deps.RateLimit = bound.WithSession(sessions.sessionID)
	}
}

// sessionCapture 是会话绑定的每请求记录视图：把 Ensure 的结果留给请求日志开行读取。
//
// 为什么必须是每请求实例：绑定结果与请求一一对应，挂在进程级会泄漏且会串号；而
// guard.Deps.SessionLookup 是「按请求构造」的钩子（见 MessageWriter.WithBody 的说明），
// 本类型正是它的来源。
type sessionCapture struct {
	inner   guard.SessionBinder
	mu      sync.Mutex
	result  guard.SessionResult
	matched bool
}

// newSessionCapture 包裹真实绑定实现；inner 为 nil 时退化为「有调用、无结果」。
func newSessionCapture(inner guard.SessionBinder) *sessionCapture {
	return &sessionCapture{inner: inner}
}

// Ensure 实现 guard.SessionBinder：转发给真实实现并记录结果。
func (c *sessionCapture) Ensure(ctx context.Context, request guard.SessionRequest) (guard.SessionResult, error) {
	if c.inner == nil {
		return guard.SessionResult{}, nil
	}
	result, err := c.inner.Ensure(ctx, request)
	if err != nil {
		return result, err
	}
	c.mu.Lock()
	c.result = result
	c.matched = true
	c.mu.Unlock()
	return result, nil
}

// lookup 供请求日志开行取会话身份；未绑定过时返回 false（两列写 NULL）。
func (c *sessionCapture) lookup(*pctx.Context) (guard.SessionResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result, c.matched
}

// sessionID 是限流视图的会话来源：返回本次请求的物理会话 id。
//
// 未绑定过时返回空串（不是错误）——限流服务会按 cfg.SessionID 的兜底语义现场生成并留痕，
// 两处各写一套生成口径会比一处退化更难排障。
func (c *sessionCapture) sessionID(*pctx.Context) (string, error) {
	result, ok := c.lookup(nil)
	if !ok {
		return "", nil
	}
	return result.SessionID, nil
}

// bodyAccess 是每请求的正文通路：延迟构造访问器，整个请求复用同一个实例。
type bodyAccess struct {
	once    sync.Once
	access  *guard.BodyAccessor
	err     error
	ctx     *pctx.Context
	options guard.BodyAccessOptions
}

// newBodyAccess 建一个未触发的正文通路。
func newBodyAccess(_ context.Context, pc *pctx.Context, options guard.BodyAccessOptions) *bodyAccess {
	return &bodyAccess{ctx: pc, options: options}
}

// factory 是注入守卫链的工厂：同一请求内多次调用返回同一访问器。
func (b *bodyAccess) factory(*pctx.Context) (guard.BodyAccess, error) {
	b.once.Do(func() {
		b.access, b.err = guard.NewBodyAccessor(b.ctx, b.options)
	})
	if b.err != nil {
		return nil, b.err
	}
	return b.access, nil
}

// json 取解析后的正文；通道未触发时按需触发。
func (b *bodyAccess) json() (map[string]any, error) {
	access, err := b.factory(nil)
	if err != nil {
		return nil, err
	}
	return access.JSON()
}

// bytes 取正文当前字节（守卫链改写过则重新序列化）。
func (b *bodyAccess) bytes() ([]byte, error) {
	access, err := b.factory(nil)
	if err != nil {
		return nil, err
	}
	if accessor, ok := access.(*guard.BodyAccessor); ok {
		return accessor.Bytes()
	}
	return nil, errors.New("dataplane: 正文访问器类型不符")
}
