package guard

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件定义守卫链与外界之间的全部缝隙。
//
// 为什么用接口而不是直接实现：这些依赖分别属于限流、回放、选路、终态结算与会话绑定，
// 各有自己的落点包与波次。守卫链只负责「按顺序调用、早退短路、把失败翻译成响应」，
// 一旦把实体逻辑写进来，它就会变成下一个 9368 行的 forwarder。
//
// 缝隙的所有权：接口在本包定义并冻结，实现包只做适配（Go 侧惯用的「消费方定义接口」）。
// nil 缝隙的语义逐个写明——多数守卫在 Node 侧是 fail-open，缺失时按同样语义跳过并留日志。

// 可判别错误。调用方一律用 errors.Is 判定，不要比较文案。
var (
	// ErrKeyNotFound 表示密钥不存在（用户软删也折叠为此，与 Node 语义一致）。
	ErrKeyNotFound = errors.New("guard: API 密钥不存在")
	// ErrKeyDisabled 表示密钥已禁用。
	ErrKeyDisabled = errors.New("guard: API 密钥已禁用")
	// ErrKeyExpired 表示密钥已过期。
	ErrKeyExpired = errors.New("guard: API 密钥已过期")
	// ErrBodyUnavailable 表示正文缝隙缺失或正文不可解析。
	ErrBodyUnavailable = errors.New("guard: 请求正文不可用")
)

// User 是鉴权后随请求携带的用户属性切面。
//
// 只收守卫链真正读的字段：启用态与过期时间（auth）、客户端白/黑名单（client）、
// 模型白名单（model）。
type User struct {
	ID             int64
	Name           string
	IsEnabled      bool
	ExpiresAt      *time.Time
	AllowedClients []string
	BlockedClients []string
	AllowedModels  []string
}

// Key 是密钥属性切面。
type Key struct {
	ID     int64
	Name   string
	UserID int64
}

// AuthResolution 是一次成功的密钥解析结果。
type AuthResolution struct {
	User User
	Key  Key
}

// AuthResolver 解析 API 密钥。
//
// 失败必须返回 ErrKeyNotFound / ErrKeyDisabled / ErrKeyExpired 之一：前者会被认证节流
// 计数（可能是爆破信号），后两者不会——这条区分是 Node 侧刻意设计的，漏掉会让管理员
// 停用一个密钥就等于把自己的 IP 关进小黑屋。
type AuthResolver interface {
	ResolveAPIKey(ctx context.Context, key string) (AuthResolution, error)
}

// UserDirectory 按用户 id 读回用户属性。
//
// 与 Node 的差异（有意）：Node 的守卫直接读 session.authState.user，因为它的会话对象里
// 就带着允许名单；pctx.AuthState 刻意只留最小身份字段，故这里引入一条按 id 读取的缝隙，
// 实现方应复用同一份缓存，不得退化成每请求一次 DB 查询。
type UserDirectory interface {
	User(ctx context.Context, userID int64) (User, error)
}

// UserExpiryMarker 标记用户已过期（惰性过期，尽力而为）。
//
// nil 表示不做标记：守卫链不会因此拒绝请求，只少一次副作用。
type UserExpiryMarker interface {
	MarkUserExpired(ctx context.Context, userID int64) error
}

// SettingsSource 提供 system_settings 的单行视图。
//
// store.Pools 直接满足本接口，无需适配。
type SettingsSource interface {
	FindSystemSettings(ctx context.Context) (*store.SystemSettings, error)
}

// IPExtractor 从入口 headers 解析客户端 IP。
//
// 暴露 X-Forwarded-For 的信任判断与 ip_extraction_config，属入口职责；nil 时认证守卫
// 退回上下文的 ClientIP（入口若已解析则等价）。
type IPExtractor interface {
	ClientIP(ctx context.Context, headers map[string][]string) (string, error)
}

// ThrottleDecision 是认证节流的判定结果。
type ThrottleDecision struct {
	// Allowed 为 false 时认证守卫直接返回 429。
	Allowed bool
	// RetryAfterSeconds 非空时写入 Retry-After 头。
	RetryAfterSeconds *int
}

// RateLimiter 是限流缝隙：认证节流（防爆破）与请求级多维限流。
//
// 实体实现属限流包（rate-limit-guard.ts 的移植），不在本包。nil 表示不节流、不限流：
// 这是显式的接线缺口，链上会各留一条 warn。
type RateLimiter interface {
	// Throttle 检查某个 IP 与候选密钥是否因近期认证失败被临时封禁。
	Throttle(ctx context.Context, clientIP, candidateKey string) (ThrottleDecision, error)
	// RecordAuthSuccess 记录一次认证成功（重置该 IP/密钥的失败计数）。
	RecordAuthSuccess(ctx context.Context, clientIP, candidateKey string)
	// RecordAuthFailure 记录一次认证失败。
	RecordAuthFailure(ctx context.Context, clientIP, candidateKey string)
	// Check 是请求级限流：返回 nil 表示放行。
	//
	// 判定与文案都由实现方给出，因为额度维度与提示语属于限流包的事实，守卫链只负责
	// 把它翻译成响应并短路。
	Check(ctx context.Context, ctx2 *pctx.Context) (*RateLimitBlock, error)
}

// RateLimitBlock 是请求级限流的拦截结果。
//
// 除 Status/Message/ErrorType 外，其余字段对应 Node 限流响应信封里的机器可读部分
// （`error-handler.ts` 的 `buildRateLimitResponse`，7 个核心字段）：客户端与前端会按
// `code` / `limit_type` 分支，故取值必须逐字对齐，不能只对齐人类可读文案。
type RateLimitBlock struct {
	Status            int
	Message           string
	ErrorType         string
	RetryAfterSeconds *int
	// BlockedBy 与 BlockedReason 会随请求日志落库，取值形制由实现方对齐 Node。
	BlockedBy     string
	BlockedReason string
	// LimitType 是触发维度：rpm | usd_5h | usd_weekly | usd_monthly | usd_total |
	// concurrent_sessions | daily_quota。
	LimitType string
	// Current 与 Limit 是该维度的当前用量与上限（响应体里的 current / limit）。
	Current float64
	Limit   float64
	// ResetTime 是窗口重置时刻的 ISO-8601 字符串（形如 2026-09-12T16:28:05.527Z）；
	// 空串表示滚动窗口没有固定重置时刻——此时不写 X-RateLimit-Reset 与 Retry-After。
	ResetTime string
}

// SensitiveWord 是一条敏感词规则。
type SensitiveWord struct {
	// Word 是规则原文。
	Word string
	// MatchType 取 contains、exact、regex 之一，与 Node 侧枚举逐字一致。
	MatchType string
}

// SensitiveWordSource 提供敏感词快照。
//
// 快照靠 cfgsync 的失效通道对齐；本包只要求「一次调用拿到一致快照」，不关心缓存实现。
// nil 表示无词表，按 Node 侧 isEmpty 的快速路径放行。
type SensitiveWordSource interface {
	SensitiveWords(ctx context.Context) ([]SensitiveWord, error)
}

// BlockedRequestLogger 记录被守卫拦截的请求（敏感词等）。
//
// Node 侧是异步、失败不影响拦截；nil 表示不记录。
type BlockedRequestLogger interface {
	// pc 是请求上下文，用于终态上报与亲和写回（可为 nil）；ctx 是派生的运行上下文。
	RecordBlocked(ctx context.Context, pc *pctx.Context, record BlockedRecord) error
}

// BlockedRecord 是一条被拦截请求的日志载荷。
type BlockedRecord struct {
	KeyID        int64
	UserID       int64
	APIKey       string
	Model        string
	SessionID    string
	StatusCode   int
	BlockedBy    string
	Reason       json.RawMessage
	ErrorMessage string
}

// RequestFilter 是一条请求过滤规则（对应 request_filters 表的一行）。
//
// 字段名与表列一致，取值形制见 Node 侧 repository/request-filters.ts 的 RequestFilter。
type RequestFilter struct {
	ID       int64
	Scope    string // header | body
	Action   string // remove | set | json_path | text_replace
	Target   string
	Priority int
	// Replacement 是 JSON 原值：string 时按字符串用，其余按 JSON 序列化后使用。
	Replacement json.RawMessage
	MatchType   string // regex | contains | exact | 空
	// BindingType 取 global、providers、groups 之一。
	BindingType string
	ProviderIDs []int64
	GroupTags   []string
	// RuleMode 取 simple 或 advanced；advanced 时用 Operations。
	RuleMode string
	// ExecutionPhase 取 guard 或 final。
	ExecutionPhase string
	// Operations 是 advanced 模式的操作序列（JSON 原样保存，由本包解释）。
	Operations json.RawMessage
}

// FilterSource 提供过滤规则快照。
//
// 返回的规则应已按 (priority, id) 升序排列；本包不重排，因为顺序影响叠加结果。
type FilterSource interface {
	RequestFilters(ctx context.Context) ([]RequestFilter, error)
}

// BodyAccess 是请求正文的读写缝隙。
//
// 为什么需要它：正文解析与字节所有权属于入口/转发器（一次性消费、内存门禁都在那里），
// 而敏感词、过滤器、探测识别这些守卫必须看到解析后的 JSON。缝隙把「谁持有字节」和
// 「谁做判定」分开，避免守卫链自己缓冲一份正文——那正是 Node 侧每流内存的主要放大器。
type BodyAccess interface {
	// JSON 返回解析后的正文对象，可被就地修改（同一请求内多次调用返回同一棵树）。
	JSON() (map[string]any, error)
	// Store 把修改后的正文写回，供后续步骤与上游转发使用。
	Store(body map[string]any) error
}

// BodyFactory 为一次请求取出正文访问器。nil 表示本次运行没有正文通道。
type BodyFactory func(ctx *pctx.Context) (BodyAccess, error)

// ClientVersion 是解析出的客户端类型与版本。
type ClientVersion struct {
	ClientType string
	Version    string
}

// VersionChecker 判断客户端版本是否过旧，并顺手记录用户版本。
//
// 两个方法都按 Node 侧的 fail-open 语义调用：出错即放行，只记日志。
type VersionChecker interface {
	// ParseUserAgent 解析 UA；无法识别时返回 false。
	ParseUserAgent(userAgent string) (ClientVersion, bool)
	// UpdateUserVersion 记录用户当前版本（尽力而为）。
	UpdateUserVersion(ctx context.Context, userID int64, client ClientVersion) error
	// ShouldUpgrade 判断是否需要升级，并返回当前 GA 版本。
	ShouldUpgrade(ctx context.Context, client ClientVersion) (needsUpgrade bool, gaVersion string, err error)
	// DisplayName 是客户端类型的展示名。
	DisplayName(clientType string) string
}

// SessionBinder 承载会话身份（client session id 提取、会话绑定与序号）。
//
// 绑定与并发会话限额走 Redis 与 Lua，属会话包的落点；nil 表示跳过会话分配。
type SessionBinder interface {
	// Ensure 为请求准备会话身份，返回值写回上下文之外由实现自持。
	Ensure(ctx context.Context, request SessionRequest) (SessionResult, error)
}

type CodexSessionCompleter interface {
	// Complete 判定需要补齐哪一侧（正文 prompt_cache_key / 请求头 session_id、x-session-id），
	// 并给出要写进审计条目的 action/source/sessionId。它**不**改写请求——改写是守卫链的职责。
	Complete(ctx context.Context, request CodexSessionCompletionRequest) (CodexSessionCompletionResult, error)
}

// CodexSessionCompletionRequest 是补全需要的入口事实。
type CodexSessionCompletionRequest struct {
	KeyID     int64
	Body      map[string]any
	Headers   map[string][]string
	UserAgent string
}

// CodexSessionCompletionResult 是一次补全的结果。
type CodexSessionCompletionResult struct {
	// Applied 表示确实补写了某个字段。
	Applied bool
	// Action / Source / SessionID 是落审计条目的三个事实（取值域见 session 包）。
	Action    string
	Source    string
	SessionID string
	// SetBodyPromptCacheKey / SetHeaderSessionID / SetHeaderXSessionID 是要补写的侧。
	SetBodyPromptCacheKey bool
	SetHeaderSessionID    bool
	SetHeaderXSessionID   bool
}

// SessionRequest 是会话绑定需要的入口事实。
type SessionRequest struct {
	KeyID           int64
	Body            map[string]any
	Headers         map[string][]string
	UserAgent       string
	AllowRawSession bool
}

// SessionResult 是会话绑定的结果。
type SessionResult struct {
	SessionID string
	Sequence  int
	// IdentitySource 是本次会话身份的**来源**。
	//
	// 为什么必须带它：前缀兜底层（route 的前缀亲和）服务的是**客户端未带会话 id** 的请求
	// （设计稿 §2），而会话包在那种情形下总会生成或恢复出一个非空 SessionID。只看 SessionID
	// 是否为空，这道兜底在生产链上永不触发——无 id 的客户端（curl、旧客户端）就此失去旧版
	// 已有的前缀粘性（§9 验收项 2 要求它们记 prefix_affinity）。
	IdentitySource SessionIdentitySource
	// Binding 是本次会话的绑定快照，供选路层判定会话粘性（nil 表示无绑定事实）。
	//
	// 为何带上 ProviderID 与 Generation：前者是「粘到哪一家」，后者是终态写回 CAS 的基准
	// （generation fence 拒绝迟到写入）。选路层拿到它们才可能短路与回写；
	// 丢了它们就只能每请求再读一次 Redis（且 CAS 无基准）。
	Binding *SessionBindingFacts
}

// SessionIdentitySource 是会话身份的来源（guard 包的中性视图）。
//
// 三个取值对应会话包 Ensure 的三条分支：客户端显式携带 / 按正文哈希找回既有会话 /
// 网关生成。后两者同属「客户端身份缺失」，前缀兜底层对它们一视同仁。
type SessionIdentitySource int

const (
	// SessionIdentityClient 是客户端显式携带了会话 id。
	SessionIdentityClient SessionIdentitySource = iota
	// SessionIdentityRecovered 是客户端未带 id，但按正文哈希找回了既有会话。
	SessionIdentityRecovered
	// SessionIdentityGenerated 是客户端未带 id 且未找到既有会话，由网关生成。
	SessionIdentityGenerated
)

// SessionBindingFacts 是会话绑定的事实快照（guard 包的中性视图）。
//
// 为什么不直接用 route.SessionBindingSnapshot：本包不为一个纯数据搬运引入选路包类型，
// 且 route 与 session 都依赖 guard 方向不明；两个结构字段手工对齐，对齐点是
// session/binding.go 的 BindingSnapshot 与 route/session_binding.go 的 SessionBindingSnapshot。
type SessionBindingFacts struct {
	KeyID      int64
	Generation string
	// ProviderID 为 0 表示空绑定（新会话或已清空）。
	ProviderID int64
	// Writeback 是终态写会话绑定的能力句柄（实现在会话包）。
	//
	// 为何由会话包一并交回而不是守卫链另建：键形制、generation 基准与 TTL 都在会话包；
	// guard 只做一次转交（同 pctx.AffinityWriteback 的注入模式）。
	// nil 表示本次不写（会话包未装配或 binder 未就绪）。
	Writeback pctx.SessionBindingWriteback
}

// WarmupLogWriter 记录被抢答的 warmup 请求（provider_id = 0，不计费）。
type WarmupLogWriter interface {
	// pc 是请求上下文，用于终态上报与亲和写回（可为 nil）；ctx 是派生的运行上下文。
	RecordWarmup(ctx context.Context, pc *pctx.Context, record WarmupRecord) error
}

// WarmupSessionArtifactWriter 把本地抢答的 warmup 响应写进会话详情（Redis）。
//
// 为什么单列一条缝隙而不是复用 WarmupLogWriter：前者写账本行（数据库），后者写会话调试
// 工件（Redis 的四条键）——两者的失败处理与开关都不同（工件受高并发模式节制）。
type WarmupSessionArtifactWriter interface {
	StoreWarmupResponse(ctx context.Context, request WarmupArtifactRequest) error
}

// WarmupArtifactRequest 是一次 warmup 抢答要落的四份工件所需的输入。
type WarmupArtifactRequest struct {
	SessionID  string
	Sequence   int
	KeyID      int64
	Method     string
	Body       string
	Headers    map[string]string
	StatusCode int
}

// WarmupRecord 是一条 warmup 抢答的日志载荷。
type WarmupRecord struct {
	KeyID         int64
	UserID        int64
	APIKey        string
	Model         string
	OriginalModel string
	SessionID     string
	Sequence      int
	UserAgent     string
	Endpoint      string
	MessagesCount int
	DurationMS    int64
}

// ProviderSelector 选定供应商；nil 表示不选路（链上留 warn 并继续）。
type ProviderSelector interface {
	Select(ctx context.Context, req *pctx.Context) (pctx.ProviderSelection, error)
}

// MessageContextWriter 在转发前建立请求日志上下文（创建 message_request 行）。
//
// 只创建、不落终态：终态属终态结算包。
type MessageContextWriter interface {
	EnsureContext(ctx context.Context, req *pctx.Context) error
}

// ReplayAttacher 是回放命中钩子。nil 表示本波未接线，直接跳过。
type ReplayAttacher interface {
	// Attach 命中活跃或已完成的重放时返回抢答响应；未命中返回 nil。
	Attach(ctx context.Context, req *pctx.Context) (*Response, error)
}

// Deps 是守卫链的全部依赖。
//
// 每个字段的可为 nil 语义都在对应接口上写明；链在构造时不做完整性校验，因为过渡期
// 必然存在「部分接线」的中间态，硬校验会让 Go 无法按路由灰度。
type Deps struct {
	Auth         AuthResolver
	AuthThrottle RateLimiter
	Users        UserDirectory
	ExpiryMarker UserExpiryMarker
	Settings     SettingsSource
	IP           IPExtractor
	Versions     VersionChecker
	Sessions     SessionBinder
	// CodexCompletion 是 Codex 会话标识补全（Node codex/session-completer.ts）；
	// nil 表示未接线（补全整体跳过，请求照常放行）。
	CodexCompletion CodexSessionCompleter
	Sensitive       SensitiveWordSource
	BlockedLog      BlockedRequestLogger
	Filters         FilterSource
	Body            BodyFactory
	RateLimit       RateLimiter
	Provider        ProviderSelector
	MessageContext  MessageContextWriter
	Replay          ReplayAttacher
	WarmupLog       WarmupLogWriter
	// WarmupArtifacts 落 warmup 抢答的会话工件；nil 表示未接线（不落，请求照常抢答）。
	WarmupArtifacts WarmupSessionArtifactWriter
	// QueryAPIKey 取 Gemini CLI 的 key 查询参数。pctx.Path 不含查询串，该凭据必须由入口注入。
	// nil 表示入口未提供，按无此凭据处理。
	QueryAPIKey func(*pctx.Context) string
	// BypassRequestFilters 报告本次请求是否跳过请求过滤器（端点策略）。nil 表示不跳过。
	BypassRequestFilters func(*pctx.Context) bool
	// EndpointRawPassthrough 报告本次请求是否属**原始透传端点**（count_tokens 与
	// responses/compact；对齐 Node EndpointPolicy.allowRawCrossProviderFallback）。
	//
	// 它是 Node `session.isRawCrossProviderFallbackEnabled()` 的**端点因子**，调用方须自行与
	// 系统设置 allow_non_conversation_endpoint_provider_fallback 取与，才是那个两因子判定的
	// 完整值（session.ts:574-582）。少了它就会把「设置」当成全局开关：生产该设置为 true 时，
	// /v1/responses 这类普通端点的闸门会被永久关死。
	EndpointRawPassthrough bool
	// ProviderGroupTag 取选定供应商的分组标签（providers.group_tag）。
	// pctx.ProviderSelection 只带路由必需字段，故由接线波次从供应商快照补一个取值函数；
	// nil 表示分组绑定的过滤规则不生效。
	ProviderGroupTag func(pctx.ProviderSelection) string
	// SessionLookup 取本次请求已绑定的会话身份（会话 id 与序号）。
	//
	// 为什么用钩子而不是 pctx 槽位：会话绑定属会话包，pctx 刻意不带会话状态；而请求日志
	// 开行（messageContext）需要这两列。nil 表示会话包未接线，两列写 NULL。
	SessionLookup func(*pctx.Context) (SessionResult, bool)
	// RequestContext 把 pctx 上的请求映射回标准库 context，供缝隙里的数据库、Redis 与
	// 出站调用使用。nil 时退化为 context.Background()：守卫链不断言取消，但实现方应当提供。
	RequestContext func(*pctx.Context) context.Context
	// ClaudeMetadata 注入 Claude 线的 metadata.user_id（Node claude-code/metadata-user-id.ts 的
	// 构建侧，落在 session 包）。返回是否写入。nil 表示未接线：注入整体跳过，请求照常放行。
	//
	// 参数依次是正文、密钥 id、本次会话 id、客户端 UA；正文就地改写，调用方负责写回。
	ClaudeMetadata func(body map[string]any, keyID int64, sessionID, userAgent string) bool
	// Logger 是守卫链自身的日志器；nil 时写 stderr。
	Logger *logx.Logger
	// Locale 覆盖默认语种（默认 zh-CN，与 i18n/config.ts 的 defaultLocale 一致），仅供测试与管理员配置使用。
	Locale string
}
