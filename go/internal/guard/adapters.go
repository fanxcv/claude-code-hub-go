package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件把真实包接到守卫链的缝隙上。
//
// 纪律（三条，改动前先读）：
//  1. 适配器只做搬运与转换，**不写业务规则**。判定逻辑要么在守卫步骤里，要么在各自的
//     落点包里；适配器里再写一遍匹配/限额/转换，就会出现两套真相。
//  2. 读配置一律走快照或缓存，不得每请求打库（Node 侧修过的性能回归，见 cfgsync 的设计）。
//     本文件里唯一每请求打库的是密钥解析，且它也有缓存（见 AuthStore）。
//  3. 每请求状态不得挂在进程级表里。正文访问器与会话绑定都按「由调用方按请求构造并注入」
//     的方式提供（见 BodyAccessor、MessageWriter.SessionLookup），不引入按 *pctx.Context
//     索引的全局 map——那是一个只会增长、没有释放时机的泄漏面。

// 与 Node 侧同名的默认值。
const (
	// DefaultAPIKeyCacheTTL 对应 api-key-auth-cache.ts 的 API_KEY_AUTH_CACHE_TTL_SECONDS 默认值。
	DefaultAPIKeyCacheTTL = 60 * time.Second
	// DefaultUserCacheTTL 与密钥缓存同源（Node 侧共用同一份 Redis 缓存与 TTL）。
	DefaultUserCacheTTL = 60 * time.Second
	// DefaultAPIKeyCacheSize 是进程内密钥缓存的容量上限，超限按 TTLMap 的 LRU 规则淘汰。
	DefaultAPIKeyCacheSize = 4096
	// DefaultUserCacheSize 是用户缓存容量上限。
	DefaultUserCacheSize = 4096
)

// AdapterOptions 是真实适配器的构造参数。
type AdapterOptions struct {
	// Pools 是数据库分道，必填。
	Pools *store.Pools
	// Bus 是配置失效总线；nil 表示不做失效广播（缓存只靠 TTL 自愈；事件驱动的域因此
	// 永不重载，仅用于单测）。
	Bus *cfgsync.Bus
	// Registry 记录各配置域的装载与失效次数，并负责订阅生命周期；nil 时仅用 Bus 订阅。
	Registry *cfgsync.Registry
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// APIKeyCacheTTL / UserCacheTTL 为 0 时取 Node 侧默认值。
	APIKeyCacheTTL time.Duration
	UserCacheTTL   time.Duration
	// BodyOptions 是入站正文解压的限额；零值时用 ingress 的默认值。
	BodyOptions BodyAccessOptions
	// RouteOptions 覆盖选路器的构造参数（Source 由 Adapters 填 StoreSource）。
	RouteOptions route.Options
	// Settler 是拦截/预热日志的终态结算器；nil 时用 Pools 自建（生产装配）。
	Settler *terminal.Settler
	// RateLimit 是请求级多维限流的实现（生产装配传 internal/limit 的 Service 快照）；nil 时该步跳过。
	//
	// 为何由外部传入而不是本包构造：limit 包依赖本包的常量与判定结构（ThrottleDecision、
	// RateLimitBlock），本包反向引入就会成环。接口仍是本包自己的 RateLimiter。
	RateLimit RateLimiter
	// AuthThrottle 是认证失败节流（防爆破）的实现；nil 时该步跳过。
	AuthThrottle RateLimiter
}

// Adapters 是全部真实适配器的集合。
//
// 一个结构体而不是散落的构造函数：接线方要能一眼看出「这一波接了什么、还缺什么」，
// 而缺口（nil 字段）必须显式出现在 Apply 里并附上归属波次。
type Adapters struct {
	// Auth 解析 API 密钥并顺带缓存用户属性（AuthResolver + UserDirectory + UserExpiryMarker）。
	Auth *AuthStore
	// Settings 是 system_settings 快照（SettingsSource）。
	Settings *SettingsCache
	// Sensitive 是敏感词快照（SensitiveWordSource）。
	Sensitive *SensitiveCache
	// Filters 是请求过滤器快照（FilterSource）。
	Filters *FilterCache
	// Blocked 记录被拦截的请求（BlockedRequestLogger）。
	Blocked *BlockedRecorder
	// Warmup 记录被抢答的 warmup 请求（WarmupLogWriter）。
	Warmup *WarmupRecorder
	// Message 建立请求日志上下文（MessageContextWriter）。
	Message *MessageWriter
	// Provider 是选路器适配（ProviderSelector）。
	Provider *ProviderRouter
	// IP 是客户端 IP 解析（IPExtractor）：规则链来自 system_settings.ip_extraction_config。
	IP *IPExtractorAdapter
	// Rules 是错误规则匹配（供转发层实现 forward.RuleMatcher）。
	Rules *ErrorRuleCache
	// Detector 是假 200 检测（供转发层实现 forward.BodyErrorDetector）。
	Detector *Fake200Detector
	// RateLimit / AuthThrottle 是限流缝隙的实现（由 AdapterOptions 注入）。
	RateLimit    RateLimiter
	AuthThrottle RateLimiter
	// Body 是入口已解析好正文时的共享工厂；通常由调用方按请求构造并注入 Deps.Body，
	// 此字段保持 nil（见 SetBodyFactory 的提醒）。
	Body BodyFactory

	registry *cfgsync.Registry
	dispose  []func()
}

// NewAdapters 构造全部适配器。Pools 缺失即返回错误：没有数据库就没有任何真实快照，
// 让它在构造期失败比让守卫链在运行期逐条 warn 更省事。
func NewAdapters(opts AdapterOptions) (*Adapters, error) {
	if opts.Pools == nil {
		return nil, errors.New("guard: 适配器需要 store.Pools")
	}
	logger := opts.Logger
	if logger == nil {
		logger = defaultLogger
	}
	apiKeyTTL := opts.APIKeyCacheTTL
	if apiKeyTTL <= 0 {
		apiKeyTTL = DefaultAPIKeyCacheTTL
	}
	userTTL := opts.UserCacheTTL
	if userTTL <= 0 {
		userTTL = DefaultUserCacheTTL
	}
	if opts.Registry == nil && opts.Bus != nil {
		opts.Registry = cfgsync.NewRegistry(opts.Bus)
	}

	adapters := &Adapters{registry: opts.Registry}
	bind := func(domain cfgsync.Domain, invalidate func()) {
		if opts.Registry == nil {
			return
		}
		dispose, err := opts.Registry.Bind(domain, invalidate)
		if err != nil {
			logger.Error("guard.adapters.bind_failed", map[string]any{
				"domain": string(domain),
				"error":  err.Error(),
			})
			return
		}
		adapters.dispose = append(adapters.dispose, dispose)
	}

	adapters.Auth = newAuthStore(authStoreOptions{
		pools:    opts.Pools,
		userTTL:  userTTL,
		keyTTL:   apiKeyTTL,
		logger:   logger,
		registry: opts.Registry,
	})
	bind(cfgsync.DomainAPIKeys, adapters.Auth.Invalidate)
	adapters.Settings = newSettingsCache(opts.Pools, opts.Registry, logger)
	bind(cfgsync.DomainSystemSettings, adapters.Settings.Invalidate)
	adapters.Sensitive = newSensitiveCache(opts.Pools, opts.Registry, logger)
	bind(cfgsync.DomainSensitiveWords, adapters.Sensitive.Invalidate)
	adapters.Filters = newFilterCache(opts.Pools, opts.Registry, logger)
	bind(cfgsync.DomainRequestFilters, adapters.Filters.Invalidate)

	// 拦截与预热日志走终态包的「建行 + 终态」两步（store 的插入面不表达 status_code /
	// blocked_by，重复一份 INSERT 会让账本与 outbox 触发器的行为分叉）。
	settler := opts.Settler
	if settler == nil {
		settler = terminal.New(terminal.StoreWriter{Pools: opts.Pools}, terminal.Options{})
	}
	adapters.Blocked = newBlockedRecorder(settler, logger)
	adapters.Warmup = newWarmupRecorder(settler, logger)
	adapters.Message = newMessageWriter(opts.Pools, logger, opts.RouteOptions.AffinityIgnoreClientSessionID)
	adapters.Provider = newProviderRouter(opts.Pools, opts.Registry, opts.RouteOptions, logger)
	adapters.IP = newIPExtractor(adapters.Settings, logger)
	adapters.Rules = newErrorRuleCache(opts.Pools, opts.Registry, logger)
	bind(cfgsync.DomainErrorRules, adapters.Rules.Invalidate)
	adapters.Detector = newFake200Detector()
	// 限流缝隙：由接线方传入已装配好的实现（internal/limit）。本包不自建，见 AdapterOptions.RateLimit。
	adapters.RateLimit = opts.RateLimit
	adapters.AuthThrottle = opts.AuthThrottle
	bind(cfgsync.DomainProviders, adapters.Provider.Invalidate)
	bind(cfgsync.DomainProviderEndpoints, adapters.Provider.Invalidate)
	return adapters, nil
}

// Apply 把适配器注入依赖。
//
// 覆盖而不是「仅当为 nil 时填」：接线方显式传入的替换（例如测试桩）应当先 Apply
// 再逐字段覆盖，顺序写反会得到「看起来注入了但其实没生效」的静默行为。
//
// 本波仍留 nil 的缝隙与归属波次：
//   - ReplayAttacher：回放波次。
//   - SessionBinder：会话包尚未落地，会话 id/序号因此也进不了请求日志（见 MessageWriter）。
//   - VersionChecker：客户端识别与 GA 版本来源包尚未落地。
//   - IPExtractor：出口/入口波次负责（ip_extraction_config 的信任链在那里）。
//   - RateLimit / AuthThrottle：仅当 AdapterOptions 传入了实现（见 NewAdapters）。
//   - ProviderGroupTag：已由本包的 AuthStore.ProviderGroup 提供（见 Apply）。
func (a *Adapters) Apply(deps *Deps) {
	if deps == nil {
		return
	}
	deps.Auth = a.Auth
	deps.Users = a.Auth
	deps.ExpiryMarker = a.Auth
	deps.Settings = a.Settings
	deps.Sensitive = a.Sensitive
	deps.Filters = a.Filters
	deps.BlockedLog = a.Blocked
	deps.WarmupLog = a.Warmup
	deps.MessageContext = a.Message
	deps.Provider = a.Provider
	// 客户端 IP 解析：配置链来自 system_settings 快照（与 Node 的信任边界同一份配置）。
	deps.IP = a.IP
	// 限流缝隙：仅在确实注入了实现时写入。不用 nil 覆盖调用方已接的限流器——那会把
	// 「本包没提供实现」意外地变成「取下调用方的实现」。
	if a.RateLimit != nil {
		deps.RateLimit = a.RateLimit
	}
	if a.AuthThrottle != nil {
		deps.AuthThrottle = a.AuthThrottle
	}
	// 分组绑定：providers.group_tag 是落库列，取值来自供应商快照，不在 pctx 槽位里。
	deps.ProviderGroupTag = a.Provider.ProviderGroupTag
	// 选路的有效分组同样不经过 pctx：由参数化的 AuthStore 从密钥缓存里取，零额外查询。
	a.Provider.Group = a.Auth.ProviderGroup
	a.Provider.Body = deps.Body
	// 供应商级客户端名单的判定复用同一份 client-detector（Node 在 pickRandomProvider 的
	// Step 1 调 isClientAllowedDetailed，与密钥级限制同一套匹配语义）。
	//
	// **必须晚绑定**：不能写成 `a.Provider.DetectClient = deps.ProviderClientRestriction`——
	// 方法值在 Apply 当时就把 `*deps` 的副本绑定了，而 `deps.Body` 是在本函数后半段才从
	// `a.Body` 补上的。早绑定会让判定器拿到一个 Body 为 nil 的 Deps，于是
	// `metadata.user_id` 读不到、信号永远 confirmed=false，**内置关键字（claude-code-*）
	// 这类模式会静默永不命中**（黑名单/白名单都失效）。闭包在调用时才解引用 deps，
	// 因此与 Apply 内部的装配顺序无关。
	a.Provider.DetectClient = func(ctx *pctx.Context, allowed, blocked []string) *route.ClientRestriction {
		return deps.ProviderClientRestriction(ctx, allowed, blocked)
	}
	a.Message.Body = deps.Body
	a.Message.SessionLookup = deps.SessionLookup
	// 正文缝隙若是适配器自带的工厂，交给适配器持有；调用方按请求构造的工厂优先。
	if deps.Body == nil && a.Body != nil {
		deps.Body = a.Body
	}
}

// SetBodyFactory 装上一次运行共享的正文工厂。
//
// 提醒：BodyFactory 会按同一 *pctx.Context 被多次调用，因此工厂必须在请求内返回**同一个**
// 访问器。通常由调用方按请求构造（见 NewBodyAccessor 的用法），此处只保留给
// 「入口已经解析好正文、希望所有请求共用同一个工厂实现」的接线方式。
func (a *Adapters) SetBodyFactory(factory BodyFactory) {
	a.Body = factory
}

// Close 释放订阅。
func (a *Adapters) Close() {
	for _, dispose := range a.dispose {
		dispose()
	}
	a.dispose = nil
}

// ---- 共用小工具 ----

// decodeStringArray 解 jsonb 数组列。
//
// null 与 [] 都解成空切片：Node 侧 `allowedClients ?? []` 的语义，空数组表示不限。
func decodeStringArray(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	return values
}

// decodeInt64Array 解 jsonb 数字数组列。
func decodeInt64Array(raw json.RawMessage) []int64 {
	if len(raw) == 0 {
		return nil
	}
	var values []int64
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	return values
}

// parseTimestamp 解 row_to_json 输出的时间戳。
//
// row_to_json 把 timestamptz 渲染成 ISO8601（带偏移），与 time.RFC3339Nano 兼容；
// 少数库表用不带时区的 timestamp，故一并接受无时区形制（按 UTC 解释，与 TS 的
// new Date(...) 对这些串的处理一致）。
func parseTimestamp(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999", "2006-01-02 15:04:05.999999"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("guard: 无法解析时间戳 %q", raw)
}

// counter 是给测试与观测用的装载计数器。
type counter struct {
	value atomic.Int64
}

func (c *counter) add()       { c.value.Add(1) }
func (c *counter) get() int64 { return c.value.Load() }

// markLoaded 记录一次成功装载，供 /readyz 观测该域是否已就绪。
func markLoaded(registry *cfgsync.Registry, domain cfgsync.Domain) {
	if registry != nil {
		registry.MarkLoaded(domain)
	}
}

// clientFormatOf 把入口解析出的协议族映射回客户端格式。
//
// pctx 只保留协议族（入口按路径判定），而选路的格式兼容判定要的是客户端格式。两者的
// 对应关系是确定的：anthropic-messages→claude、openai-chat→openai、openai-responses→response。
// gemini 族无法在这里区分 gemini 与 gemini-cli（那是客户端识别的事），故按 gemini 处理，
// 偏差仅在「格式兼容」维度上体现。
func clientFormatOf(family egress.Family) convert.ClientFormat {
	switch family {
	case egress.FamilyAnthropicMessages:
		return convert.FormatClaude
	case egress.FamilyOpenAIChat:
		return convert.FormatOpenAI
	case egress.FamilyOpenAIResponses:
		return convert.FormatResponse
	case egress.FamilyGemini:
		return convert.FormatGemini
	default:
		return ""
	}
}

// requestBody 取请求正文，缺缝隙或解析失败时返回 nil（调用方按各自 fail-open 语义处理）。
func requestBody(factory BodyFactory, ctx *pctx.Context) map[string]any {
	if factory == nil {
		return nil
	}
	access, err := factory(ctx)
	if err != nil || access == nil {
		return nil
	}
	body, err := access.JSON()
	if err != nil {
		return nil
	}
	return body
}

// bodyModel 取请求模型（复刻 ProxySession.request.model 的取法）。
func bodyModel(body map[string]any) string {
	if body == nil {
		return ""
	}
	if model, ok := body["model"].(string); ok {
		return model
	}
	if request, ok := body["request"].(map[string]any); ok {
		if model, ok := request["model"].(string); ok {
			return model
		}
	}
	return ""
}

// bodyMessageCount 取消息条数（复刻 session.getMessagesLength）。
func bodyMessageCount(body map[string]any) int {
	return messagesCount(body)
}

// contextOrBackground 兜底上下文，避免把 nil context 传进驱动。
func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
