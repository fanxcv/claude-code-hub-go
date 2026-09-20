// Package adminapi 承载管理面（/api/v1）在 Go 侧的挂载点：路由表、路径匹配与归属回退。
//
// 三条硬约束：
//
//  1. 与数据面同构：请求先过前门中间件（egress.FrontDoor.Middleware）判归属，判给 Go 且命中
//     本包路由的由本进程作答，其余一律交 Node 反代。**本包没有任何 501/裸 404**——「Go 还没
//
// 实现」不是错误，只是还没切过去。
//  2. 漏注册路由不会报错，只会静默落回 Node。这正是 A2 的「72 条路由存在性契约测试」存在的
//     理由：静默回退只能被测试发现，不能靠运行时发现。
//  3. 认证缺失时 fail-closed：Deps.Guard 为 nil 时 Router 拒绍注册任何路由（并记 Error 日志），
//     这些请求照常回退 Node——那里有完整的认证。绝不出现「Go 放行未认证的管理请求」。
//
// 本文件是 A0-1 的冻结面：A0-2（auth/problem/audit/invalidate）与 A1（资源模块）按此实现。
//
// A0-3 扩面，理由：失败审计与脱敏需要。本轮为 AuditEvent 补上 Category / Success /
// ErrorMessage / TargetName / Before / UserAgent，为 Principal 补上 KeyName——A0-2 的实现当时以
// 「frozen 面表达不了」把这些字段逐个记在 audit.go 顶部并退化成恒 NULL / 恒 TRUE；
// 不补则失败审计无法落库（columns: target_name, before_value, operator_key_name,
// user_agent, success, error_message 见 src/drizzle/schema.ts:1308-1337）。
//
// A1-2 续扩面，理由：keys 的写路由要复刻 Node 的 Web-UI 会话门（见 Principal.WebSession /
// Principal.CanLoginWebUI）。这两项只在认证层可知（auth.go 的 resolvedPrincipal 早已算出
// keyCanLoginWebUI 与 credential.source），认证层不传给处理器，处理器就只能瞎猜——那正是
// 「只读密钥能自提权」的弯路，比不实现更糟。
//
// A1-2 续扩面（读档），理由：keys 的三条读档端点（GET /keys/{keyId}、/limit-usage、/quota）
// 的 `concurrentSessions.current` 与 5h 固定窗口累计值只存在于 **Redis 运行态**，而当时 Deps
// 里没有任何 Redis 入口（Store 是纯 PG）。为此加 `SessionCounts` 与 `Fixed5hWindows` 两个窄接口；
// 两者缺失时那三条路由**不注册**（回退 Node）——恒 0 的 current 是静默的错数，比不实现更坏。
//
// A1-3 续扩面（写档），理由：providers 的删除/更新要写**撤销快照**
// （`cch:prov:undo-del:<token>` / `cch:prov:undo-patch:<token>`）。Deps 里仍没有 Redis 入口，
// 而「软删成功、撤销必失败」是静默的错行为，所以加 `ProviderUndoKV`；缺失时删除/更新/撤销
// 这一组路由**整组不注册**（回退 Node），而不是只注册能跑的那半边。
package adminapi

import (
	"context"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/usagefeed"
)

const (
	// MountPrefix 是管理面在 cchd 上的挂载前缀。
	MountPrefix = "/api/v1"
	// APIVersion 是响应头 X-API-Version 的取值，逐字取自 Node
	// （src/lib/api/v1/_shared/constants.ts:1 的 MANAGEMENT_API_VERSION）。
	APIVersion = "1.0.0"
	// VersionHeader 是版本响应头名（同上 :4 的 API_VERSION_HEADER）。
	VersionHeader = "X-API-Version"
)

// AccessLevel 是端点的权限档位，逐字取自 Node 的 AuthTier 与 OpenAPI 的 x-required-access
// （src/lib/api/v1/_shared/auth-middleware.ts:11）。
type AccessLevel string

const (
	// AccessPublic 不加认证也不提取凭据（auth-middleware.ts:69-76：public 档位直接返回空身份），
	// 用于管理面前门自身的存活路由。
	AccessPublic AccessLevel = "public"
	// AccessRead 允许任意已认证用户。
	AccessRead AccessLevel = "read"
	// AccessAdmin 仅管理员。
	AccessAdmin AccessLevel = "admin"
)

// Principal 是已认证的调用方身份。
type Principal struct {
	UserID   int64
	Username string
	IsAdmin  bool
	// KeyID 非零表示以 API key 认证。
	KeyID int64
	// KeyName 是密钥名（audit_log.operator_key_name）；ADMIN_TOKEN 身份下为空。
	KeyName string
	// Token 是浏览器会话令牌：CSRF 校验与审计归属用，不得写进日志。
	Token string
	// CanLoginWebUI 是**本次凭据所用密钥**的 can_login_web_ui（Node 的 session.key.canLoginWebUi）。
	// 它是 keys 写路径的门：Node 的 requireKeyWriteSession 与 denyKeyWriteForReadOnlySession
	// 都按「角色是 admin，或该密钥允许登录 Web UI」放行
	// （keys/handlers.ts:129-148、actions/keys.ts:41-53）。读档认证会放行 canLoginWebUi=false 的
	// 只读会话，于是这个字段是「只读会话不得改密钥」的唯一依据。ADMIN_TOKEN 合成的密钥取 true。
	CanLoginWebUI bool
	// WebSession 表示凭据来自浏览器会话（Cookie），而非 Authorization/X-API-Key 头。
	// 它不参与授权判定——只读会话门看的是 CanLoginWebUI；Node 里 source==="cookie" 只用于决定
	// 是否校验 CSRF（auth-middleware.ts:117-128，Go 侧在 AuthGuard 内完成）。此处保留它是因为
	// 审计与「写路径被拒」的日志要能区分「浏览器会话」与「程序化调用」。
	WebSession bool
}

// Guard 是管理面的守卫链：四次门（tier 校验、api key admin 开关、CSRF、Web-UI 会话门）与失败时
// 的 Problem 作答都由实现负责（A0-2）。Wrap 在路由注册时调用一次，返回的处理器必须并发安全。
type Guard interface {
	// Wrap 返回包装后的处理器；认证或授权失败时由实现直接作答，不得调用 next。
	Wrap(level AccessLevel, next http.Handler) http.Handler
}

// ProblemWriter 按 Node 的 RFC7807 形状作答
// （src/lib/api/v1/_shared/error-envelope.ts:40-62；detail 文案表见同文件 :64）。
type ProblemWriter interface {
	// WriteProblem 按显式状态码与错误码作答。
	WriteProblem(writer http.ResponseWriter, request *http.Request, status int, errorCode, detail string)
	// WriteActionError 把 action 层错误按显式错误码表映射作答（Node 侧同表见
	// src/app/api/v1/resources/keys/handlers.ts:309-334 与 users/handlers.ts:365-386）。
	WriteActionError(writer http.ResponseWriter, request *http.Request, err error)
	// WriteValidationError 作答 zod 形状的 400（Node 的 fromZodError，error-envelope.ts:40-62）：
	// title 固定 "Validation failed"，正文含 invalidParams 明细。
	WriteValidationError(writer http.ResponseWriter, request *http.Request, params []InvalidParam)
}

// AuditEvent 是一次写操作的审计记录（表与列见 src/drizzle/schema.ts:1308-1337）。
//
// 字段名与 Node 的 EmitActionAuditArgs（src/lib/audit/emit.ts:29-42）对齐：success 与 category
// 在 Node 是必填参数，这里同样要求调用方显式填写（见各自注释）。
type AuditEvent struct {
	// Category 是 action_category，取值域见 src/types/audit-log.ts:1-10。
	// 为空时按 Action 的前缀表推导；两者都推不出时**放弃写入**（写错分类会污染按分类筛选）。
	Category   string
	Principal  Principal
	Action     string
	TargetType string
	TargetID   string
	// TargetName 是目标对象名（audit_log.target_name）。
	TargetName string
	// Before 与 Details 是操作前 / 操作后快照，写入前必须经脱敏（src/lib/audit/redact.ts）；
	// 不得含凭据原文。前者落 before_value，后者落 after_value。
	Before  map[string]any
	Details map[string]any
	IP      string
	// UserAgent 从请求头取（audit_log.user_agent）。
	UserAgent string
	// Success 必须显式填写：零值即「失败审计」。Node 侧同为必填参数，漏填是调用方缺陷，
	// 且失败行在审计列表里可见，不会被静默吞掉。
	Success bool
	// ErrorMessage 仅在 Success 为 false 时有意义（audit_log.error_message）。
	ErrorMessage string
}

// AuditSink 记录审计事件。语义与 Node 一致（src/lib/audit/emit.ts:38-60）：
// fire-and-forget，失败只告警，绝不因审计失败让请求失败或变慢。
type AuditSink interface {
	Emit(ctx context.Context, event AuditEvent)
}

// Invalidator 广播缓存失效：Node 侧的 Redis 缓存与本进程缓存都要清。
//
// 接口按 Node 的调用点一一声明，而不是做成通用 scope 结构：四处失效目标的键布局互不相同
// （api-key-auth-cache 以 key 串为键，cost-cache 以 id 为键，配置域走 cfgsync 通道）。
type Invalidator interface {
	// InvalidateKeyAuth 使某个 API key 的认证缓存失效（src/lib/security/api-key-auth-cache）。
	InvalidateKeyAuth(ctx context.Context, apiKey string)
	// InvalidateUserAuth 使某个用户的认证缓存失效。
	InvalidateUserAuth(ctx context.Context, userID int64)
	// InvalidateKeyCost 使某个 key 的成本缓存失效（src/lib/redis/cost-cache-cleanup）。
	InvalidateKeyCost(ctx context.Context, keyID int64)
	// InvalidateUserCost 使某个用户的成本缓存失效。
	InvalidateUserCost(ctx context.Context, userID int64)
	// PublishDomain 广播配置域失效（cfgsync.Bus）。
	PublishDomain(ctx context.Context, domain cfgsync.Domain)
}

// SessionCounter 读 Key 级活跃会话数（Node 的 SessionTracker.getKeySessionCount）。
//
// 为什么单列一个接口而不直接要求 *limit.SessionTracker：管理面只该看到「读一次计数」这一件事，
// 拿具体类型会把限流包的写路径（占位/释放）一起变成管理面的依赖面。
// nil 表示未装配：keys 那三条读档路由会因此不注册（原样回退 Node，见 keys.go）。
type SessionCounter interface {
	// KeySessionCount 返回该密钥当前活跃会话数；出错时调用方按 Node 的 fail-open 语义取 0。
	KeySessionCount(ctx context.Context, keyID int64) (int, error)
}

// ProviderCostReader 只读判定**供应商维度**的周期限额与总额度
// （Node 的 `RateLimitService.checkCostLimits(id, "provider", …)` 与 `checkTotalCostLimit`）。
//
// 为什么要单独声明：数据面那条限流器（`limit.Service.Check`）只判 key / user 两维，
// 供应商维度在选路包里留了 `route.Gates.Limits` 缝隙但**当前无装配方**；而调度模拟器
// 必须能复刻 Node 在选路阶段对供应商做的只读判定，否则 healthAndLimits 那一步会
// 把 Node 会排除的供应商报成存活（前端会看到一个偏乐观的决策树）。
//
// 只读是硬约束：实现不得刷新 lease、不得写 Redis（预览不得改变线上状态）。
type ProviderCostReader interface {
	CheckProviderCostLimits(
		ctx context.Context,
		providerID int64,
		in limit.ProviderCostLimits,
	) (allowed bool, detail string)
}

// Fixed5hWindowReader 读 5h 固定窗口的累计值与重置时刻
// （Node 的 RateLimitService.getFixed5hWindowState，src/lib/rate-limit/service.ts:160-183）。
//
// 为什么不能只看账本：密钥的 limit5hResetMode 为 fixed 时，5h 累计值存在 Redis 运行态窗口里
// （由 TTL 决定重置时刻），读账本聚合会得到另一个数（两套口径在 Node 里就是两条分支）。
type Fixed5hWindowReader interface {
	Fixed5hWindowState(ctx context.Context, keyID int64, now time.Time) (limit.Fixed5hState, error)
}

// Deps 是管理面各资源模块共享的依赖。零值合法：Router 按「缺什么拒什么」处理，
// 不会因为某个依赖没装配就把请求误判成成功（缺 Guard 时路由根本不会注册）。
type Deps struct {
	// Logger 为请求日志与内部事件使用。
	Logger *logx.Logger
	// Guard 为 nil 时 Router 拒绍注册任何路由（fail-closed，见 Router.Add）。
	Guard Guard
	// Problems 为 nil 时 Router 用内置兜底形状作答（仅程序错误用，正常路径不该发生）。
	Problems ProblemWriter
	// Audit 为 nil 时审计调用是空操作。
	Audit AuditSink
	// Invalidator 为 nil 时失效广播是空操作。
	Invalidator Invalidator
	// Store 是分道连接池集合；nil 表示未装配。
	Store *store.Pools
	// SessionCounts 读 Key 级活跃会话数；nil 表示未装配（依赖它的读档路由不注册）。
	SessionCounts SessionCounter
	// UserSessionCounts 读 User 级活跃会话数（自服务面 /me/quota 的
	// userCurrentConcurrentSessions），与 SessionCounts 分开的理由见 me.go 的
	// UserSessionCounter：Node 读的是 User 维度 ZSET，不是各密钥计数之和。
	// nil 表示未装配（则 /me/quota 不注册，原样回退 Node）。
	UserSessionCounts UserSessionCounter
	// Fixed5hWindows 读 5h 固定窗口运行态；nil 表示未装配（依赖它的读档路由不注册）。
	Fixed5hWindows Fixed5hWindowReader
	// ProviderCost 只读判定供应商维度限额（调度模拟器的 healthAndLimits 步骤）。
	// nil 表示未装配：该步骤退化为「只判熔断」，启动时会记 warn——与 Node 相比会少排除一批
	// 已触顶的供应商，属可见的偏离而不是静默降级。
	ProviderCost ProviderCostReader
	// EndpointCircuitBreaker 对应 ENABLE_ENDPOINT_CIRCUIT_BREAKER（Node 默认 false）。
	// 关闭时端点级与厂级熔断**都不参与判定**：模拟器的端点统计给 circuitOpen=0 / available=enabled，
	// 厂级熔断也不排除任何供应商（Node 的 isVendorTypeCircuitOpen 首行就是这个开关）。
	EndpointCircuitBreaker bool
	// UserFixed5hWindows 读 User 维度的 5h 固定窗口运行态（自服务面 /me/quota）；
	// nil 表示未装配（则 /me/quota 不注册）。
	UserFixed5hWindows UserFixed5hWindowReader
	// PublicStatusSnapshots 读 public-status 的配置投影快照（公开路由 /api/public-site-meta
	// 的数据源）。它是**Redis** 而不是 PG：快照由配置发布方写进版本化键。
	// nil 表示未装配（则那条公开路由不注册，回退 Node）。
	PublicStatusSnapshots PublicStatusSnapshotReader
	// PublicStatusPublisher 重建并发布 public-status 的**配置投影**（写版本化键 + 推进指针）。
	// nil 表示未装配（无 Redis 命令连接）：system/settings PUT 在需要重发时如实回
	// PUBLIC_STATUS_PROJECTION_PUBLISH_FAILED，而不是假装发布成功。
	PublicStatusPublisher PublicStatusPublisher
	// DashboardCaches 清「随时区变化而失效」的三族缓存（overview/statistics/leaderboard）。
	// nil 表示未装配：settings PUT 改时区时记 warn（与 Node 的 catch-并-warn 同语义）。
	DashboardCaches DashboardCacheInvalidator
	// ProviderUndoKV 读写供应商写操作的撤销快照（删除 60s / 更新 10s 窗口）。
	// nil 表示未装配：providers 的写路径（创建除外）与撤销路由整组不注册，原样回退 Node。
	// 它不能像 Audit 那样「缺了就降级成空操作」——空操作的撤销写等于让 UI 的撤销按钮恒失败。
	ProviderUndoKV ProviderUndoKV
	// ObservedSessions 读会话观测运行态（dashboard 的并发数 / 供应商插槽数）。
	// nil 表示未装配：overview / concurrent-sessions / provider-slots 三条路由不注册，
	// 原样回退 Node——恒 0 的并发数在大屏上是静默错数。
	ObservedSessions ObservedSessionRuntime
	// ProviderCircuitConfig 把熔断三阈值同步进 Redis（circuit_breaker:config:<id>）。
	// nil 表示未装配：写路径仍注册（Node 侧同样容错），但会记 warn——数据面的熔断读只看 Redis，
	// 不写哈希等于「管理员改了阈值而闸门不变」。
	ProviderCircuitConfig ProviderCircuitConfigWriter
	// UsageLogsExports 是 usage-logs 导出作业的状态与结果存储（Redis）。nil 表示未装配：
	// 三条导出路由（POST /usage-logs/exports 与状态/下载）整组不注册，原样回退 Node——
	// 半个导出（投了永远查不到，或状态查得到而下载必失败）比不接管更坏。
	UsageLogsExports UsageLogsExportKV
	// NewRowsFeed 是「使用记录有新行落库」的信号订阅面（实现见 usagefeed.Hub）。
	//
	// nil 表示未装配：`GET /usage-logs/stream`（推送模式）**不注册**，请求原样回退 Node——
	// 一条「连得上但永远没有信号」的流会让前端把推送误判为可用并关掉轮询，比不接管更坏。
	NewRowsFeed usagefeed.Feed
	// CircuitStates 读写端点级与厂级两族熔断**状态**（provider-endpoints 的六条 circuit 端点）。
	// nil 表示未装配：那六条整组不注册，原样回退 Node——恒 closed 的熔断状态是静默错数
	// （管理页显示正常、闸门实际在拒），比不接管更坏。
	CircuitStates CircuitStateStore
	// UsersReset 是用户统计重置的作业队列（两条路由：排一次重置、查一次作业状态）。
	// nil 表示未装配：那两条路由**整组不注册**，原样回退 Node——排不进去的作业会让 UI 显示一个
	// 永远 queued 的状态，查不到的状态会让轮询一直 404，两者都比不接管更坏。
	UsersReset UsersResetQueue
	// EndpointProbes 同步拨测一个端点（provider-endpoints 的 POST /provider-endpoints/{id}:probe）。
	// nil 表示未装配：那一条不注册，原样回退 Node——「探了但没落库」的假结果比不接管更坏。
	EndpointProbes EndpointProbeRunner
	// CloudPriceTables 抓一次云端 CPT 价格表（/api/prices/cloud-model-count 的数据源）。
	// nil 表示未装配：那一条不注册，原样回退 Node。
	CloudPriceTables CloudPriceTableSource
	// NotifyScheduler 重排通知定时任务（Node 的 scheduleNotifications()：settings PUT 与
	// 绑定 PUT 之后都要「撤销旧任务 + 按新配置重排」）。
	// nil 表示未装配：两条写路径照常注册，只记 warn（与 Node 的 fail-open 同判——重排失败
	// 不阻断已经落库的设置写入）。恒不重排的后果是「定时通知按旧时刻触发」，
	// 故装配缺失要说出来，见 notificationReschedule。
	NotifyScheduler NotifyRescheduler
	// StickySessions 终止供应商名下的粘性会话（providers 与 provider-endpoints 写路径的副作用，
	// 见 provider_sticky.go 的触发条件表）。它与上面几项不同：**不是一条路由的依赖**，而是写操作的
	// 后续动作，所以 nil 不会让任何路由不注册——写路径照常注册，只是记 warn（与 Node 的 fail-open
	// 同判：终止失败/未装配都不阻断已经落库的写操作）。
	StickySessions StickySessionTerminator
	// SessionObservations 读会话观测集合（GET /sessions 的会话 id 来源：观测 ZSET + 并发计数 +
	// `session:*:info` 扫描）。nil 表示未装配：/sessions 不注册，原样回退 Node——半个列表
	// （有聚合无并发数）会让「活跃/非活跃」分组一起错。
	SessionObservations SessionObservationReader
	// SessionTerminations 终止物理会话（DELETE /sessions/{id} 与 POST /sessions:batchTerminate）。
	// nil 表示未装配：那两条不注册。「终止了但绑定还在」比不接管更坏。
	SessionTerminations SessionTerminator
	// SessionAffinity 推进前缀亲和的代际围栏（终止 pfx 会话的前置，见 sessions_terminate.go）。
	// nil 表示未装配：**两条终止路由一并**不注册——代际不推进的终止会留下可复活的绑定。
	SessionAffinity SessionAffinityInvalidator
	// SessionArtifacts 读会话工件并判所有权（详情面的 detail / messages / messages-exists）。
	// nil 表示未装配：依赖工件的详情面端点不注册，原样回退 Node——只接读侧而没有写入方时
	// 这些端点会恒答 404，比不接管更坏（UI 的详情按钮会静默消失）。
	SessionArtifacts SessionArtifactReader
	// IPGeo 是 IP 归属地查询器（三条 ip-geo 端点共用）。
	//
	// nil 表示未装配：三条端点都不注册，整组回退 Node——没有上游客户端与缓存时它们只能答
	// 「查不到」，而 UI 拿到的会是一个看起来成功但恒空的归属地弹窗。
	// 装配点：`ipgeo.New(redisClient, ipgeo.Options{BaseURL: cfg.Env.IPGeoAPIURL, …}, logger)`。
	IPGeo IPGeoLookup
}

// NotifyRescheduler 重排通知定时任务（实现见 internal/jobs 的 NotifyScheduler）。
//
// 与其它注入缝的区别：它**不是任何路由的依赖**，而是写操作的后续动作（同 StickySessions）。
// 因此 nil 不会让任何路由不注册，只让写路径记一条 warn。
type NotifyRescheduler interface {
	// Reschedule 读设置重排全部通知任务；返回错误表示本轮未重排（调用方只记日志）。
	Reschedule(ctx context.Context) error
}

// StickySessionTerminator 终止供应商维度的粘性会话
// （Node 的 SessionManager.terminateProviderSessionsBatch，session-manager.ts:3733-3798）。
//
// 为什么单列一个接口而不直接要求 *session.Binder：管理面只该看到「按供应商终止会话」这一件事，
// 拿具体类型会把绑定读写的整面（CAS/租约/索引）都变成管理面的依赖面。
// *session.Binder 的结构方法集直接满足本接口，装配处无需适配器。
type StickySessionTerminator interface {
	// TerminateProviderSessionsBatch 终止这些供应商名下当前活跃的会话，返回成功终止的条数。
	TerminateProviderSessionsBatch(ctx context.Context, providerIDs []int64) (int, error)
}

// InvalidParam 是校验失败的单项明细（error-envelope.ts:24-28 的 ZodIssue 投影）。
//
// Message 的文案不逐字对齐 zod：zod 的英文默认消息随版本变化，抄一份只会得到会腐烂的文案表。
// Code 取 zod 的语义码（invalid_type / too_big / invalid_enum_value / invalid_string 等），
// A2 对拍时把消息文案登记为允许差异（同 users_schema.go 文件头的取舍）。
type InvalidParam struct {
	Path    []any  `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
