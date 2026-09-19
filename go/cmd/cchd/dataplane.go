package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dataplane"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/usagefeed"
)

// 本文件把数据面接进进程启动：开库分道、建处理器、交出释放函数。
//
// 两条纪律：
//
//  1. **没有 DSN 时不装配数据面**（返回 errDataPlaneSkeleton），前门继续把 /v1 全部回退 Node。
//     骨架进程必须能在没有依赖的情况下启动并如实报 not_configured（与 deps 的同款语义）。
//  2. **有 DSN 但装配失败时 fail fast**。带着「判给 Go 却没有数据面」的状态启动，会让前门
//     把请求交给一个必然 503 的处理器——那比不启动更难排障。
//
// 连接池的归属见 boot.go：池由 boot 开、由 boot 最后关，本文件只使用不持有。

// errDataPlaneSkeleton 表示当前是骨架模式（没有数据库），数据面按设计不装配。
var errDataPlaneSkeleton = errors.New("cchd: 未配置 DSN，数据面不装配")

// errSettleIncomplete 表示排空窗口结束时仍有流终态未落库。
//
// 它是审计事件而不是可忽略的收尾瑕疵：紧接着 closeAll 会关掉连接池，
// 尚未发出的终态 UPDATE 会随进程消失，对应的账本行永久留在未终态。
var errSettleIncomplete = errors.New("cchd: 排空后仍有终态未落库")

// dataPlaneOptions 是数据面装配缝的参数；*rulesSync 提供配置失效通道。
type dataPlaneOptions struct {
	Cfg    config.Config
	Logger *logx.Logger
	Rules  *rulesSync
	// Pools 是数据面与管理面共用的同一套连接池（由 boot 开与关）。
	//
	// 为什么不在本函数里开：两个面各自开一套会让 DB_POOL_MAX 变成两倍物理连接，而两边的
	// 准入预算又各算一次——预算的本意是「整进程的并发上限」，不是「每个面各一份」。
	Pools    *store.Pools
	Fallback http.Handler
	// AffinityReport 在装配完成后回填亲和的开关结论（nil 时忽略）。
	// 启动日志与 /readyz 必须能说出「亲和是否启用、依据哪个来源」：静默关闭最难发现。
	AffinityReport func(affinityStatus)
}

// openDataPlane 装配数据面处理器，返回释放函数。
//
// 释放函数的**收口范围**只含数据面自己的连接（亲和存储与命令连接）：连接池由 boot 统一收口，
// 且排在两个面之后——handler 仍可能在返回途中用它写终态。
func openDataPlane(ctx context.Context, options dataPlaneOptions) (http.Handler, func(), error) {
	if options.Cfg.DSN == "" {
		return nil, nil, errDataPlaneSkeleton
	}
	if options.Pools == nil {
		// 有 DSN 却没拿到共享池：装配缝接错了，不猜着继续。
		return nil, nil, errors.New("cchd: 数据面装配缺少连接池")
	}
	pools := options.Pools

	// 亲和：存储与开关一起装配，否则选择器拿到的 Affinity 为 nil，亲和静默失效。
	affinity := openAffinity(ctx, options.Cfg, pools, options.Logger)
	if options.AffinityReport != nil {
		options.AffinityReport(affinity.status)
	}

	// 数据面的 Redis 命令连接：会话绑定与回放共用。与订阅连接分开——订阅会长期占用一条连接，
	// 命令道混进去会挤占热路径延迟（与 cmd 里打开订阅处的理由相同）。
	redisClient, err := openCommandRedis(options.Cfg)
	if err != nil {
		affinity.close()
		return nil, nil, err
	}

	// 使用记录的新行信号（发布面）：终态结算后广播一条「有新行」信号，供管理面 SSE 订阅。
	// 与管理面各建一个 Hub 是正确的——投递只经 Redis，发布与订阅不必共享内存。
	usageRows := usagefeed.NewHub(usagefeed.Options{Redis: redisClient, Logger: options.Logger})

	// 限流与鉴权节流：同一个服务实例兼任两个缝隙（认证节流与请求级限流本就共享一套节流状态）。
	// 依赖不全时 openRateLimiter 返回 nil，装配层会把它记进 gaps——本轮之前那正是生产上
	// 「走 Go 的请求不查配额」的可见痕迹，不能静默丢掉。
	rateLimiter := openRateLimiter(ctx, options.Cfg, pools, redisClient, options.Logger)

	assembly, err := dataplane.NewStoreBacked(dataplane.StoreOptions{
		Pools:        pools,
		Redis:        redisClient,
		Logger:       options.Logger,
		Registry:     options.Rules.registry,
		Bus:          options.Rules.bus,
		Fallback:     options.Fallback,
		RateLimit:    rateLimiter,
		AuthThrottle: rateLimiter,
		// 三档拨号超时：FETCH_CONNECT/HEADERS/BODY_TIMEOUT（毫秒）。不接进来的话拨号层只会用
		// 自己的硬默认，运维改这三个变量就是静默无效——而它们恰是「上游卡住」时最先被调的旋钮。
		DialOptions: dialOptionsFromEnv(options.Cfg.Env),
		// 租约结算：由限流实例兼任（判定与结算必须落在同一组键上，见 leaseSettlerFor）。
		LeaseSettler: leaseSettlerFor(rateLimiter),
		// 新行信号：不装配时照旧写库、只是不发信号（前端仍可用轮询），不静默错数。
		NewRows: usageRows,
		RouteOptions: route.Options{
			Affinity: affinity.store,
			// 日志身份形制（使用记录页的「渠道复用 / 新会话新渠道」）靠它判定。
			// 与亲和是否装配是两件事：这里传的是系统设置的原值。
			AffinityIgnoreClientSessionID: affinity.status.IgnoreClientSessionID,
			// 同协议优先（用户需求）：优先级一致时优先走不需要协议转换的供应商。
			// 取值来自 CCH_SAME_PROTOCOL_WEIGHT_K（默认 2；1 即关闭）。
			SameProtocolWeightK: options.Cfg.SameProtocolWeightK,
		},
		// 端点级与厂级熔断复用这个开关（Node 也在同一开关上短路，见 health 包注释）。
		EndpointCircuitBreakerEnabled: options.Cfg.Env.EnableEndpointCircuitBreaker,
		// 占位思考签名（CCH_THINKING_SIGNATURE_PLACEHOLDER，Go 专有，默认开）：思考来自
		// chat/responses 线上游时，给无签名的思考块补占位签名，Anthropic 客户端才会显示。
		PlaceholderThinkingSignature: options.Cfg.PlaceholderThinkingSignature,
		// 回放：开关、TTL 与两道资源上限都取自与 Node 同一组环境变量（REPLAY_*），
		// 切换期间两侧才会对「什么都缓存、缓存多久」持同一套口径。
		ReplayEnabled:             options.Cfg.Env.EnableRequestReplay,
		ReplayTTLSeconds:          options.Cfg.Env.ReplayTTLSeconds,
		ReplayMaxPayloadBytes:     int64(options.Cfg.Env.ReplayMaxPayloadBytes),
		ReplayMaxConcurrentSpools: int64(options.Cfg.Env.ReplayMaxConcurrentSpools),
		// 竞速输家引流上限：与 Node 同一个环境变量（HEDGE_LOSER_DRAIN_TIMEOUT_MS，已在
		// config 层做过 >= 1000 的钳制），否则同一批请求在两种归属下的引流耐心不一致。
		HedgeLoserDrainTimeoutMS: options.Cfg.Env.HedgeLoserDrainTimeoutMS,
		// 请求侧会话工件：开关与体积上限取自与 Node 同一组变量（STORE_SESSION_MESSAGES /
		// SESSION_REQUEST_ARTIFACT_MAX_BYTES）。切换期间两侧对「工件落不落、多大会被丢」
		// 必须持同一套口径，否则同一会话在两种归属下的详情页会一个有一个没有。
		SessionArtifacts: session.SessionArtifactOptions{
			StoreMessages: options.Cfg.Env.StoreSessionMessages,
			MaxBytes:      options.Cfg.Env.SessionRequestArtifactMaxBytes,
			// 响应正文的独立总开关（STORE_SESSION_RESPONSE_BODY，默认 true）：它是唯一一笔
			// 「把上游正文留在服务端」的写入，故与请求侧三条工件分开授开关。
			StoreResponseBody: options.Cfg.Env.StoreSessionResponseBody,
		},
		// 熔断初次开闸的告警：交给通知栈投递（三重闸门与去重都在产生点里）。
		CircuitAlerts: newCircuitBreakerAlerts(options.Logger, pools, redisClient),
	})
	if err != nil {
		affinity.close()
		_ = closeRedis(redisClient)
		return nil, nil, err
	}

	options.Logger.Info("dataplane_ready", map[string]any{
		"fallback": "node",
		"gaps":     assembly.Missing,
		"affinity": affinity.status.describe(),
	})
	return assembly.Handler, func() {
		// 亲和连接先于命令连接释放：它只在转发与终态写回期间被用，而此时 handler 已在收口。
		affinity.close()
		// 新行信号的发布面：释放它自己持有的连接，再关命令连接。
		usageRows.Close()
		if closeErr := closeRedis(redisClient); closeErr != nil {
			options.Logger.Warn("dataplane_redis_close_failed", map[string]any{"error": closeErr.Error()})
		}
	}, nil
}

// dialOptionsFromEnv 把三档 Fetch 超时配置换算成拨号参数。
//
// 单位是**毫秒**：与 src/lib/config/env.schema.ts 的同名变量一致（契约默认 30000 / 600000 / 600000），
// 也正是 dial.Default* 兜底值的来源。这里只做换算，不替调用方决定兜底数值。
//
// MaxUpstreamConnections 不在这里取：配置契约里没有对应的环境变量，取零即「不限制」。
func dialOptionsFromEnv(env config.EnvConfig) dial.Options {
	return dial.Options{
		ConnectTimeout:  fetchTimeout(env.FetchConnectTimeout),
		HeadersTimeout:  fetchTimeout(env.FetchHeadersTimeout),
		BodyIdleTimeout: fetchTimeout(env.FetchBodyTimeout),
	}
}

// fetchTimeout 把毫秒配置换算为 Duration；非正数（含 0 与负数）回零，
// 由 dial 的 Default* 兜底——「0 表示用默认」是这一层的显式约定。
func fetchTimeout(milliseconds float64) time.Duration {
	if milliseconds <= 0 {
		return 0
	}
	return time.Duration(milliseconds * float64(time.Millisecond))
}

// openCommandRedis 建命令面的 Redis 连接（数据面与管理面共用同一个构造：两边的超时口径
// 与重试次数一致，分开写迟早会分叉）；未配置 REDIS_URL 时返回 nil。
//
// 返回 nil 不是错误：没有 Redis 时会话绑定与回放按各自的降级语义运行（会话退化为
// 「每请求独立」，两列写 NULL），进程照常服务。
func openCommandRedis(cfg config.Config) (redis.UniversalClient, error) {
	if cfg.RedisURL == "" {
		return nil, nil
	}
	options, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		// 错误信息可能回显 URL，故只报类型不回显原文。
		return nil, fmt.Errorf("解析 REDIS_URL 失败（值已隐去）: %w", err)
	}
	options.MaxRetries = 2
	options.DialTimeout = commandRedisDialTimeout
	options.ReadTimeout = commandRedisIOTimeout
	options.WriteTimeout = commandRedisIOTimeout
	return redis.NewClient(options), nil
}

// closeRedis 关连接；nil 时是空操作。
func closeRedis(client redis.UniversalClient) error {
	if client == nil {
		return nil
	}
	return client.Close()
}

// storeAppName 是两个面共用的连接池 application_name 前缀，便于在 pg_stat_activity 里分辨进程。
const storeAppName = "claude-code-hub-go"

// 命令连接的 Redis 超时：热路径上的会话绑定与回放都不该把请求拖住（与订阅连接同口径）。
const (
	commandRedisDialTimeout = 5 * time.Second
	commandRedisIOTimeout   = 10 * time.Second
)
