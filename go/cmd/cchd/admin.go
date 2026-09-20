package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/health"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ipgeo"
	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/usagefeed"
)

// 本文件把管理面（/api/v1）接进进程启动：建守卫、审计、失效广播与路由表，并交出释放函数。
//
// 三条纪律：
//
//  1. **没有连接池就不装配**：认证要读库（密钥、角色、Web-UI 登录权限），审计要写库。
//     拿不到池时如实降级为「管理面全在 Node」——绝不让一个未认证的管理面在 Go 侧作答。
//  2. **装配失败降级而不是 fail fast**（与数据面相反）：管理面每个未注册的路由都会原样回退
//     Node，那里有完整实现；因此一个装不起来的管理面不影响可用性，只影响「是否已接管」。
//     数据面不同——判给 Go 的请求没有别的落点，故那边宁可起不来。
//  3. **降级要说出来**：`admin_plane_ready` 的 `authWired` / `routes` / `degraded` 三个字段
//     就是「管理面当前到底谁在答」的唯一事实来源（未接管的路径静默回退，只有日志能看出来）。

// adminOptions 是管理面装配缝的参数。
type adminOptions struct {
	Cfg    config.Config
	Logger *logx.Logger
	// Pools 是与数据面共用的同一套连接池（由 boot 开与关）。
	Pools *store.Pools
	// Rules 提供配置域失效总线（cfgsync.Bus）；未接入订阅时为 nil 总线。
	Rules *rulesSync
	// Fallback 是未注册路由的落点（前门的 Node 回退），必填：没有它，未命中会变成 503。
	Fallback http.Handler
	// NotifyScheduler 重排通知定时任务（Node 的 scheduleNotifications，见 adminapi.Deps 的说明）。
	// nil 表示未装配：notifications 的两条写路径照常注册，只记 warn。
	NotifyScheduler adminapi.NotifyRescheduler
	// SessionResolver 把本次装配出的**只读会话解析入口**（守卫本身）回填给调用方，
	// boot 把它交给 UI 面做壳注入（见 ui.go）。与数据面的 AffinityReport 同形：
	// 装配产物里只有一部分需要外传。nil 时丢弃。
	//
	// 生命周期：回填的是守卫本身，它的缓存在本函数的释放函数里关；UI 面与前门在同一处 closeAll
	// 里停机，故不存在「守卫已关、UI 仍在解析」的窗口。
	SessionResolver func(adminapi.SessionResolver)
}

// openAdminPlane 装配管理面处理器，返回释放函数。
//
// 释放函数只收管理面自己持有的东西（守卫的适配器缓存与命令连接）：连接池归 boot。
func openAdminPlane(options adminOptions) (http.Handler, func(), error) {
	if options.Pools == nil {
		return nil, nil, errAdminPlaneNoStore
	}
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}

	// 命令连接：不透明会话的读取（AuthGuard）与缓存失效广播（Invalidator）共用一条命令道。
	// 与订阅连接分开的理由同数据面（订阅会长期占用一条连接）。
	redisClient, err := openCommandRedis(options.Cfg)
	if err != nil {
		return nil, nil, err
	}

	problems := adminapi.NewProblems(logger)

	// 熔断开闸告警：与数据面共用同一份装配（同一套去重与投递语义）。
	alerts := newCircuitBreakerAlerts(logger, options.Pools, redisClient)

	// 使用记录的新行信号（推送模式的 SSE）：发布面与订阅面共用同一个 Hub。
	// 投递只经 Redis，故管理面与数据面各建一个也正确；这里共用只为一个好处——少一条
	// pub/sub 连接。未装配时 RegisterUsageLogsStream 不注册该路由（显式回退 Node，不静默错数）。
	usageRows := usagefeed.NewHub(usagefeed.Options{Redis: redisClient, Logger: logger})

	deps := adminapi.Deps{
		Logger:   logger,
		Problems: problems,
		Store:    options.Pools,
		// 通知调度器由 boot 建（它的装配早于管理面：见 boot 的构造点注释）。
		NotifyScheduler: options.NotifyScheduler,
		NewRowsFeed:     usageRows,
	}

	// 读档用的 Redis 运行态（keys 的三条读档端点）：与守卫、失效广播共用同一条命令连接——
	// 为一次只读计数再开一条连接不值得。装配失败只降级这三条路由（回退 Node），不影响管理面其余部分
	// （与「装配失败降级而不是 fail fast」同一条纪律）。
	// 供应商限额读数两条需要 Provider 维度的 5h 固定窗口读数：
	// 复用同一个 *limit.CostWindows（它直接满足 adminapi.ProviderFixed5hWindowReader），
	// 另建一份会让两侧看到不同的窗口实现。
	limitRuntime := openLimitRuntime(options.Cfg, redisClient, logger)
	deps.SessionCounts = limitRuntime.sessionCounts()
	deps.Fixed5hWindows = limitRuntime.fixed5hWindows()

	// IP 归属地查询器（三条 ip-geo 端点）：四条配置项都是 internal/config 已有字段；
	// 缺 Redis 时缓存层不可用，但查询器仍可直连。
	deps.IPGeo = openIPGeo(options.Cfg, redisClient, logger)
	// 供应商维度的**只读**限额判定（调度模拟器的 healthAndLimits 步骤）。与数据面的限流器
	// 分开装配：那条只判 key/user 两维，且读档路径不该与请求路径共享可变的会话/节流状态
	// （与 openStickySessions 里「读写各自持一个脚本调用层」同一条理由）。
	deps.ProviderCost = openProviderCost(options.Cfg, options.Pools, redisClient, logger)
	// 端点级/厂级熔断的开关（Node 的 ENABLE_ENDPOINT_CIRCUIT_BREAKER，默认 false）。
	// 关闭时两族熔断都不参与判定：模拟器的端点统计给 circuitOpen=0、厂级熔断不排除供应商。
	deps.EndpointCircuitBreaker = options.Cfg.Env.EnableEndpointCircuitBreaker

	// 粘性会话终止面（providers / provider-endpoints 写路径的副作用）：与上面读档的区别在于
	// 它是写，缺装配不给任何路由降级，只让写路径记 warn（见 openStickySessions 的说明）。
	deps.StickySessions = openStickySessions(redisClient, logger)

	// 自服务面（/api/v1/me/*）要的是 **User 维度**的两个读数（见 admin_me.go 的说明）；
	// 缺任一侧时 /me/quota 不注册，原样回退 Node。
	deps.UserSessionCounts = limitRuntime.userSessionCounts()
	deps.UserFixed5hWindows = limitRuntime.userFixed5hWindows()

	// 公开站点元数据（/api/public-site-meta）读的是 public-status 的配置投影快照（Redis）。
	// 拿不到命令连接时它是 nil，那条路由不注册（回退 Node）——与上面两条读档依赖同一条纪律。
	deps.PublicStatusSnapshots = adminapi.NewPublicStatusSnapshotReader(redisClient, logger)
	// 供应商写路径的撤销快照与命令连接共用（撤销窗口只有 10~60 秒，为写一次快照另开连接不值得）。
	// 没有命令连接时该字段为 nil：providers 的写/撤销路由整组不注册，原样回退 Node——
	// 「能删不能撤销」比不接管更坏。
	deps.ProviderUndoKV = adminapi.NewRedisProviderUndoKV(redisClient)
	// 熔断三阈值也要写 Redis（数据面的熔断读只看这个哈希，不回落库）。缺 Redis 时为 nil：
	// 写路径照常注册（与 Node 同容错），但记 warn。
	deps.ProviderCircuitConfig = adminapi.NewRedisProviderCircuitConfig(redisClient)
	// usage-logs 的三条导出路由要 Redis 作业键值面（状态/结果两族键）；缺 Redis 时为 nil，
	// 那三条路由整组不注册，原样回退 Node。
	deps.UsageLogsExports = adminapi.NewRedisUsageLogsExportKV(redisClient)
	// 两族熔断**状态**的读写面（provider-endpoints 的六条 circuit 端点）。与命令连接共用；
	// 设置快照只用于解析高并发模式下的 TTL 收缩（与数据面熔断写入器同一规则）。
	// 缺 Redis 时为 nil：那六条整组不注册，原样回退 Node。
	deps.CircuitStates = adminapi.NewRedisCircuitStates(redisClient, options.Pools, logger)
	// 会话观测读数（dashboard 的并发数与供应商插槽数）与命令连接共用。会话观测窗口取
	// SESSION_TTL（与数据面同一口径），缺 Redis 时该字段为 nil：依赖它的三条 dashboard 路由
	// 不注册，原样回退 Node——恒 0 的并发数在大屏上是静默错数。
	deps.ObservedSessions = adminapi.NewRedisSessionRuntime(
		redisClient,
		int64(options.Cfg.Env.SessionTTL),
		logger,
	)
	// sessions 资源的三条新路由（列表、终止、批量终止）：它们要的是会话包的**写面**（终止）
	// 与观测集合的**读面**。
	//
	// 绑定层构造方式与数据面 assemble 逐条一致（同一个 ratelimit 脚本注册表 + 同一个 Redis
	// 连接）：两侧必须看到同一套键与 Lua，否则「管理面终止的会话」在数据面眼里仍活着。
	// 缺 Redis 或缺脚本注册表时该字段为 nil，三条路由不注册（回退 Node）。
	if redisClient != nil {
		registry, registryErr := ratelimit.Embedded()
		if registryErr != nil {
			logger.Error("admin_sessions_binder_unavailable", map[string]any{
				"error":  registryErr.Error(),
				"action": "sessions_routes_fall_back_to_node",
			})
		} else if scriptClient, scriptErr := ratelimit.New(redisClient, registry); scriptErr != nil {
			logger.Error("admin_sessions_binder_unavailable", map[string]any{
				"error":  scriptErr.Error(),
				"action": "sessions_routes_fall_back_to_node",
			})
		} else {
			binder := session.NewBinder(scriptClient)
			deps.SessionObservations = binder
			deps.SessionTerminations = binder
			deps.SessionArtifacts = binder
			// 亲和存储：**不论开关是否启用都建**。理由是终止路径与开关无关——Node 的
			// terminateResolvedSessionIdentity 总是调 affinityStore.invalidate，而代际围栏键与
			// 开关无关（开关只影响选择器是否查找/提名）。若在关闭时给 nil，两条终止路由会整组
			// 不注册，pfx 会话就再也停不下来了。窗口/TTL 与数据面同源（同两个环境变量），
			// 但本存储只走 Invalidate，不参与写入。
			deps.SessionAffinity = route.NewAffinityStore(route.AffinityOptions{
				Redis:             redisClient,
				Window:            options.Cfg.Env.PrefixAffinityWindow,
				SlidingTTLSeconds: options.Cfg.Env.PrefixAffinityTTLSeconds,
			})
		}
	}
	// 同步拨测入口（provider-endpoints 的 POST /provider-endpoints/{id}:probe）：与探活后台任务
	// 共用同一套拨测与熔断写入器语义，但走**一次性**路径（source=manual、超时由请求指定）。
	// 没有池时是 nil，那一条路由不注册（回退 Node）；熔断写入器缺 Redis 时记账降级为进程内。
	deps.EndpointProbes = jobs.NewProbeOnceRunner(jobs.ProbeOnceOptions{
		Pools: options.Pools,
		Redis: redisClient,
		Health: health.NewWriter(health.Options{
			Redis:                         redisClient,
			EndpointCircuitBreakerEnabled: options.Cfg.Env.EnableEndpointCircuitBreaker,
			Logger:                        logger,
			// 手动拨测也可把熔断打开：与数据面走同一套开闸告警装配，避免两条路径
			// 对「开闸要不要告警」持不同口径。
			OnProviderOpened: alerts.OnProviderOpened,
			OnEndpointOpened: alerts.OnEndpointOpened,
		}),
		Logger: logger,
	})
	// 云端价格表的同步拉取面（/api/prices/cloud-model-count）：只读一次、解析出模型数与版本号。
	// 它是外网依赖（cch-plus.com），故默认 URL 与超时都在 jobs 里定，装配处不重复这些常量。
	deps.CloudPriceTables = jobs.NewCloudPriceTableSource(jobs.CloudPriceFetchOptions{})

	audit, err := adminapi.NewAuditLog(deps, adminapi.AuditLogOptions{})
	if err != nil {
		_ = closeRedis(redisClient)
		return nil, nil, fmt.Errorf("cchd: 建立审计写入器失败: %w", err)
	}
	deps.Audit = audit
	invalidator := adminapi.NewCacheInvalidator(deps, adminapi.InvalidatorOptions{
		Redis: redisClient,
		Bus:   options.Rules.bus,
		Pools: options.Pools,
	})
	deps.Invalidator = invalidator
	// 仪表盘三族缓存（overview/statistics/leaderboard）的清理直接复用同一个失效器：
	// 它已经握着命令连接，且三族的清法就是按前缀 SCAN 后 DEL。
	deps.DashboardCaches = invalidator
	// public-status 配置投影的发布器：system/settings PUT 动了 siteTitle/timezone/窗口/聚合
	// 间隔时重建快照并写 Redis。redisClient 为 nil 时它是 nil，PUT 会如实回失败码。
	deps.PublicStatusPublisher = adminapi.NewPublicStatusPublisher(options.Pools, redisClient, logger)

	// 用户统计重置的作业队列（两条路由：排一次重置、查一次作业状态）。与命令连接共用 Redis；
	// 缺 Redis 或缺池时队列为 nil，那两条路由整组不注册（回退 Node）。消费者在同一处启动。
	resetRuntime := startUsersReset(usersResetOptions{
		Logger: logger,
		Pools:  options.Pools,
		Redis:  redisClient,
	})
	deps.UsersReset = resetRuntime.queue

	guard, err := adminapi.NewAuthGuard(deps, adminapi.GuardOptions{
		Pools:                   options.Pools,
		Redis:                   redisClient,
		Logger:                  logger,
		Problems:                problems,
		AdminToken:              derefString(options.Cfg.Env.AdminToken),
		CSRFSecret:              derefString(options.Cfg.Env.CSRFSecret),
		EnableAPIKeyAdminAccess: options.Cfg.Env.EnableAPIKeyAdminAccess,
		SessionTokenMode:        options.Cfg.Env.SessionTokenMode,
		AuthSessionTTL:          time.Duration(options.Cfg.Env.AuthSessionTTLSeconds) * time.Second,
	})
	if err != nil {
		_ = closeRedis(redisClient)
		return nil, nil, fmt.Errorf("cchd: 建立管理面守卫失败: %w", err)
	}
	deps.Guard = guard
	if options.SessionResolver != nil {
		options.SessionResolver(guard)
	}

	// 根级认证面（POST /api/auth/login|logout）：UI 静态化后登录态由 Go 签发，这条是纯 Go 部署的门。
	// 装配失败只降级这两条（回退 Node），不影响管理面其余部分——与「降级而不是 fail fast」同一条纪律。
	var issuer *adminapi.AuthIssuer
	if built, issuerErr := adminapi.NewAuthIssuer(adminapi.AuthIssuerOptions{
		Deps:                deps,
		Redis:               redisClient,
		AdminToken:          derefString(options.Cfg.Env.AdminToken),
		SessionTokenMode:    options.Cfg.Env.SessionTokenMode,
		AuthSessionTTL:      time.Duration(options.Cfg.Env.AuthSessionTTLSeconds) * time.Second,
		EnableSecureCookies: options.Cfg.Env.EnableSecureCookies,
	}); issuerErr != nil {
		logger.Error("auth_issuer_degraded", map[string]any{
			"error":  issuerErr.Error(),
			"action": "auth_routes_fall_back_to_node",
		})
	} else {
		issuer = built
	}

	router := adminapi.New(adminapi.Options{Deps: deps, EnableHSTS: options.Cfg.Env.EnableSecureCookies})
	// 未注册的管理路由必须原样回退 Node：Go 侧没有 501/裸 404，「还没实现」不是错误。
	router.SetNotFound(options.Fallback)
	registerAdminRoutes(router, deps, guard, issuer, adminapi.NewRedisLeaderboardCache(redisClient), limitRuntime.costWindows(),
		adminapi.PublicStatusReadOptions{Store: pubstatus.NewRedisStatusStore(redisClient, logger)})

	logger.Info("admin_plane_ready", map[string]any{
		"routes":        router.RouteCount(),
		"authWired":     true,
		"authIssuer":    issuer != nil,
		"providerUndo":  deps.ProviderUndoKV != nil,
		"usersReset":    deps.UsersReset != nil,
		"endpointProbe": deps.EndpointProbes != nil,
		"sessionMode":   guard.SessionTokenMode(),
		"degraded":      false,
		"fallback":      "node",
	})
	return router, func() {
		// 先停重置消费者再关连接：在途作业的每一次状态写入都要用那条命令连接（见 usersreset.go 的 stop）。
		resetRuntime.stop()
		guard.Close()
		// 先关订阅面再关 Redis：Hub 的收尾要断开自己的订阅连接（它由 Hub 自己持有）。
		usageRows.Close()
		if closeErr := closeRedis(redisClient); closeErr != nil {
			logger.Warn("admin_redis_close_failed", map[string]any{"error": closeErr.Error()})
		}
	}, nil
}

// registerAdminRoutes 是管理面路由表的**唯一注册点**。
//
// 为什么必须只有一个：`Router.Add` 对未注册的路径不做任何事——它会静默回退 Node（那里有完整
// 实现）。所以「registrar 写好了但没人调」不会报错，只会让那一批端点在生产上永远由 Node 作答。
// 实测踩过**两次**：第一次是第一批资源模块齐备后，本文件只调了 RegisterShellRoutes，于是已实现的
// 39 条资源路由（keys 3 / users 17 / usage-logs 7 / model-prices 6 / sensitive-words 6）全是死码
// ——启动日志里 routes 只有 2（那两条是前门自己的 /health 与 /auth/csrf）；第二次是 error-rules
// 与 request-filters 落地后没有接进来，14 条路由又是死码。
//
// 因此**新增 registrar 必须同时改这里**：`admin_register_test.go` 从 `internal/adminapi` 的源码里
// 枚举全部 registrar 定义，并从本函数的源码里读出实际调用了哪些，两者比对——漏调必红（早先那版
// 测试自己维护一份手写清单，新增 registrar 时清单与注册点一起漏，测试照样绿，故已改成结构性发现）。
// A2 的路由存在性契约测试是第二道防线。
//
// 顺序依据（重复注册时 Router.Add 首次胜，故顺序是唯一有语义的地方）：
//
//  1. 前门自身的能力面先接：`/health` 是探活，`/auth/csrf` 与守卫共用同一份 secret，
//     二者必须在守卫就绪后立刻可用。
//
// 2. 资源模块按 §5.2 的分组顺序接（keys → users →
// usage-logs → model-prices → sensitive-words → error-rules → request-filters），
//
//	与批次划分一致，便于逐组回退。同组内顺序无路由语义（各组路径不重叠，没有首胜之争），
//	只用于阅读与逐组回退。
func registerAdminRoutes(
	router *adminapi.Router,
	deps adminapi.Deps,
	issuer adminapi.CSRFIssuer,
	authIssuer *adminapi.AuthIssuer,
	leaderboardCache adminapi.LeaderboardCacheStore,
	limitWindows *limit.CostWindows,
	publicStatusRead adminapi.PublicStatusReadOptions,
) {
	adminapi.RegisterShellRoutes(router, deps, issuer)
	// 根级认证面（/api/auth/*）：不在管理面挂载点下，但共用同一张路由表与同一条回退（见其文件头）。
	adminapi.RegisterAuthRoutes(router, deps, authIssuer)
	adminapi.RegisterKeysRoutes(router, deps)
	adminapi.RegisterUsersRoutes(router, deps)
	// 配额页的「累计成本」批量读数（Node 靠 SSR 绕过，从无 REST 端点；见 ui-parity-quotas-cost.md）。
	adminapi.RegisterCostBatchRoutes(router, deps)
	// users 的统计重置两条路由自成一档（依赖作业队列而不是纯 PG）：单独 registrar，
	// 这样「队列没装配」的降级粒度刚好是这两条。
	adminapi.RegisterUsersResetRoutes(router, deps)
	// RegisterUsageLogs 是 RegisterUsageLogsWith 的空选项封装：出厂默认即 Node 语义；
	// 导出作业的键值面走 Deps.UsageLogsExports（未装配时三条导出路由不注册）。
	adminapi.RegisterUsageLogs(router, deps)
	// 推送模式（SSE 信号）：与轮询并存的另一种刷新方式，默认不启用（前端默认 pull）。
	adminapi.RegisterUsageLogsStream(router, deps)
	adminapi.RegisterModelPrices(router, deps)
	adminapi.RegisterSensitiveWords(router, deps)
	adminapi.RegisterErrorRules(router, deps)
	adminapi.RegisterRequestFilters(router, deps)
	adminapi.RegisterMeRoutes(router, deps)
	adminapi.RegisterAuditLogs(router, deps)
	adminapi.RegisterDashboardRoutes(router, deps)
	// sessions 资源：目前只接 requests 时间线一条（其余八条依赖 Node 的 SessionManager
	// 与账本聚合，未就绪即整组回退 Node，清单见 internal/adminapi/sessions.go 文件头）。
	adminapi.RegisterSessionsRoutes(router, deps)
	adminapi.RegisterSystemRoutes(router, deps)
	// system 的设置读写（/api/v1/system/settings）与两条根级伴生面：
	// 旧管理端点 /api/admin/system-config、版本查询 /api/version、公开站点元数据 /api/public-site-meta。
	adminapi.RegisterSystemSettingsRoutes(router, deps)
	adminapi.RegisterSystemConfigRoutes(router, deps)
	adminapi.RegisterVersionRoutes(router, deps, adminapi.VersionOptions{})
	adminapi.RegisterPublicStatusMetaRoutes(router, deps)
	// 文档面（`/api/v1/openapi.json` 由已注册路由表生成；`/docs`、`/scalar` 302 指向 spec）
	// 与运维只读面（`/api/admin/database/status`）：Store 未装配时各自不注册（回退 Node）。
	adminapi.RegisterDocsRoutes(router, deps)
	adminapi.RegisterDatabaseStatusRoutes(router, deps)
	adminapi.RegisterProviders(router, deps)
	adminapi.RegisterProvidersWrite(router, deps)
	// 熔断日志查看（用户需求）：返回当前熔断状态 + 该供应商的近期失败请求，供排障。
	adminapi.RegisterProviderCircuitLogs(router, deps)
	adminapi.RegisterProviderGroups(router, deps)
	// provider-endpoints 资源的读面（厂/端点列表、探活日志）与两条根级价格读端点：
	// 它们只需 PG，与上面六条熔断端点的装配条件不同，故分开注册。
	adminapi.RegisterProviderEndpointRoutes(router, deps)
	// 两族熔断状态端点：要 PG（可见性）与 Redis（状态）两侧齐备才注册（见其文件头）。
	adminapi.RegisterProviderCircuitRoutes(router, deps)
	// providers 协议测试族（六条）：unified / by-id / 四条定型。
	// 引擎在 internal/providertest；需 PG（可见性 + 凭据）与拨号能力齐备，否则整组不注册。
	adminapi.RegisterProvidersTestRoutes(router, deps)
	// providers 限额读数两条（单条 + 批量）：需 Store、会话观测与 5h 固定窗口读数三件齐备，否则不注册。
	adminapi.RegisterProvidersLimitRoutes(router, deps, adminapi.ProvidersLimitOptions{
		Fixed5h: limitWindows,
	})
	// 厂商重挂（预览/应用双态）：仅需 Store + Redis 失效通道。
	adminapi.RegisterProvidersVendorsRoutes(router, deps)
	// providers batchPatch 预览/应用：**必须成对注册**——预览快照写在 `cch:prov:preview:`
	// 同一前缀下，只接管一条会让另一条落到「缓存由对方写、我方读」的未验证路径。
	// 缺 ProviderUndoKV 或 Store 时整组不注册（回退 Node）。
	adminapi.RegisterProviderBatchPatch(router, deps)
	// model-prices 同步三兄弟（syncLitellm / syncLitellmCheck / upload）：
	// 需 Store + 拨号能力；缺依赖时整组不注册（回退 Node）。
	adminapi.RegisterModelPriceSyncRoutes(router, deps)
	// public-status 读侧两条（`/api/v1/public/status` 与根级 `/api/public-status`）：
	// 共用同一内核，只是外壳（信封/错误形状/503+no-store）不同；缺 Redis 时整组不注册。
	adminapi.RegisterPublicStatusReadRoutes(router, deps, publicStatusRead)
	// 杂项（ip-geo 三条经 deps.IPGeo、公开状态写侧、会话响应体）：各自缺依赖时不注册（回退 Node）。
	adminapi.RegisterIPGeoRoutes(router, deps)
	adminapi.RegisterPublicStatusSettingsRoute(router, deps)
	adminapi.RegisterSessionResponseRoute(router, deps)
	// 根级可用性读端点（/api/availability/current 与 endpoints 两族，以及按时间桶聚合的
	// /api/availability）：页面自己 fetch 的私有面，作答不带管理面信封（见其文件头）。
	adminapi.RegisterAvailabilityRoutes(router, deps)
	// 根级排行榜（/api/leaderboard）：乐观缓存与 Node 同键同 TTL（60 秒），redisClient 缺位时
	// 传 nil，路由照注册、直查不缓存（与 Node 的「Redis 不可用则直查」同降级）。
	adminapi.RegisterLeaderboardRoutes(router, deps, leaderboardCache)
	// 云端模型数（/api/prices/cloud-model-count）：带 60 秒缓存的同步拉取（见其文件头）。
	adminapi.RegisterCloudModelCountRoutes(router, deps)
	// 通知面：推送目标（webhook-targets 六条）与通知设置/绑定（notifications 五条）。
	adminapi.RegisterWebhookTargets(router, deps)
	adminapi.RegisterNotifications(router, deps)
	// 用户洞察三条（overview / model-breakdown / provider-breakdown）：key-trend 依赖统计缓存，
	// 未移植，故不注册（原样回退 Node，见该文件头）。
	adminapi.RegisterAdminUserInsights(router, deps)
	// 两条根级 admin ops（/api/admin/log-level 与 /api/admin/log-cleanup/manual）：
	// 作答不带管理面信封（见其文件头）。
	adminapi.RegisterAdminOps(router, deps)
}

// errAdminPlaneNoStore 表示没有连接池可用，管理面按设计不装配。
var errAdminPlaneNoStore = errors.New("cchd: 未配置 DSN，管理面不装配")

// keyFixed5hReader 把成本窗口层适配成管理面要的窄接口（把维度固定为 Key）。
//
// 适配层放在装配处而不是让管理面自己传 limit.EntityKey：管理面不该知道限额维度是靠 Entity
// 字符串参数区分的（那是数据面的内部形式）。
type keyFixed5hReader struct{ windows *limit.CostWindows }

func (r keyFixed5hReader) Fixed5hWindowState(ctx context.Context, keyID int64, now time.Time) (limit.Fixed5hState, error) {
	return r.windows.Fixed5hWindowState(ctx, limit.EntityKey, keyID, now)
}

// limitRuntime 是一次装配出的 Redis 运行态（管理面读档与自服务面共用同一份）。
//
// 为什么只装配一次：会话计数与成本窗口都建在**同一条脚本调用层**上，而原先管理面与
// 自服务面各建一份，Lua 注册表与客户端因此各建一次。Embedded 本身是 sync.Once、重复取无代价，
// 但两份客户端会让「同一份运行态」出现两个实例，排查时先要分辨读的是哪一份。
type limitRuntime struct {
	sessionTracker *limit.SessionTracker
	windows        *limit.CostWindows
}

// openLimitRuntime 建管理面读档与自服务面共用的 Redis 运行态。
//
// 拿不到时返回 nil——注册函数据此不注册那几条依赖它的路由（宁可不答，不可乱答）。
// 没有任何 Redis 配置时直接返回 nil：此时 Node 侧的这些读也只会得到 0/不存在的键，
// 但那仍由 Node 作答（它的降级语义与我们无关，不要在这里替它降级）。
func openLimitRuntime(
	cfg config.Config,
	redisClient redis.UniversalClient,
	logger *logx.Logger,
) *limitRuntime {
	if redisClient == nil {
		logger.Info("admin_limit_runtime_skipped", map[string]any{
			"reason": "redis_unconfigured",
			"effect": "keys_read_routes_fallback_node",
		})
		return nil
	}
	registry, err := ratelimit.Embedded()
	if err != nil {
		logger.Warn("admin_limit_runtime_unavailable", map[string]any{
			"stage": "registry",
			"error": err.Error(),
		})
		return nil
	}
	scriptClient, err := ratelimit.New(redisClient, registry)
	if err != nil {
		logger.Warn("admin_limit_runtime_unavailable", map[string]any{
			"stage": "client",
			"error": err.Error(),
		})
		return nil
	}
	// SESSION_TTL 的单位与 Node 一致（秒，可小数）；<=0 时由 NewSessionTracker 取 300s 默认值。
	sessionTTL := time.Duration(cfg.Env.SessionTTL * float64(time.Second))
	return &limitRuntime{
		sessionTracker: limit.NewSessionTracker(scriptClient, sessionTTL, logger),
		windows:        limit.NewCostWindows(scriptClient, logger),
	}
}

// sessionCounts 给管理面读档用的 Key 维度计数；runtime 为 nil 时返回 nil（路由不注册）。
func (r *limitRuntime) sessionCounts() adminapi.SessionCounter {
	if r == nil {
		return nil
	}
	return r.sessionTracker
}

// fixed5hWindows 给管理面读档用的 Key 维度 5h 窗口读数。
func (r *limitRuntime) fixed5hWindows() adminapi.Fixed5hWindowReader {
	if r == nil {
		return nil
	}
	return keyFixed5hReader{windows: r.windows}
}

// userSessionCounts 给自服务面（/api/v1/me/*）用的 User 维度计数。
//
// 与 Key 维度不是同一个键：同一会话跨密钥续用时，User 维度 ZSET 去重、各密钥计数各算一次。
func (r *limitRuntime) userSessionCounts() adminapi.UserSessionCounter {
	if r == nil {
		return nil
	}
	return meUserSessionCounter{tracker: r.sessionTracker}
}

// userFixed5hWindows 给自服务面用的 User 维度 5h 窗口读数。
func (r *limitRuntime) userFixed5hWindows() adminapi.UserFixed5hWindowReader {
	if r == nil {
		return nil
	}
	return meUserFixed5hReader{windows: r.windows}
}

// costWindows 给 providers 限额读数用的窗口对象（同一实例，避免两份窗口实现）。
func (r *limitRuntime) costWindows() *limit.CostWindows {
	if r == nil {
		return nil
	}
	return r.windows
}

// openStickySessions 建粘性会话终止面（providers 与 provider-endpoints 写路径的副作用，
// 见 adminapi/provider_sticky.go 的触发条件表）。
//
// 与 openLimitRuntime 共用同一条命令连接与同一份嵌入脚本表（Embedded 是 sync.Once，重复取无代价），
// 但各自持一个脚本调用层：这里走**写**（终止会话），那里走**读**（计数与窗口），
// 共用会把「读档降级」的策略绑到写路径上。
//
// 拿不到时返回 nil——写路径**照常注册**（不像上面三条读档路由那样退出），因为终止是写成功之后的
// 副作用：宁可不终止并记 warn，也不能因为缺 Redis 就把已经落库的写操作变成错误（与 Node 的
// try/catch-并-warn 同判）。
func openStickySessions(redisClient redis.UniversalClient, logger *logx.Logger) adminapi.StickySessionTerminator {
	if redisClient == nil {
		logger.Info("admin_sticky_sessions_skipped", map[string]any{
			"reason": "redis_unconfigured",
			"effect": "写路径照常，终止只记 warn",
		})
		return nil
	}
	registry, err := ratelimit.Embedded()
	if err != nil {
		logger.Warn("admin_sticky_sessions_unavailable", map[string]any{
			"stage": "registry",
			"error": err.Error(),
		})
		return nil
	}
	scriptClient, err := ratelimit.New(redisClient, registry)
	if err != nil {
		logger.Warn("admin_sticky_sessions_unavailable", map[string]any{
			"stage": "client",
			"error": err.Error(),
		})
		return nil
	}
	return session.NewBinder(scriptClient)
}

// openIPGeo 建 IP 归属地查询器（三条 ip-geo 端点共用）。
//
// 缺 Redis 时缓存层为 nil（`NewRedisCache` 对 nil 客户端返回 nil），查询器退化为直查——
// 与 Node 在 `getRedisClient()` 为 nil 时同判。四个配置项均取自 internal/config 的既有字段，
// 不另设默认值（默认值已在 env 规格表里，重复写会成第二份真相）。
func openIPGeo(cfg config.Config, redisClient redis.UniversalClient, logger *logx.Logger) adminapi.IPGeoLookup {
	return ipgeo.New(ipgeo.NewRedisCache(redisClient), ipgeo.Options{
		BaseURL:  cfg.Env.IPGeoAPIURL,
		Token:    derefString(cfg.Env.IPGeoAPIToken),
		Timeout:  time.Duration(cfg.Env.IPGeoTimeoutMS) * time.Millisecond,
		CacheTTL: time.Duration(cfg.Env.IPGeoCacheTTLSeconds) * time.Second,
	}, logger)
}

// derefString 取指针配置项的字符串值；未配置（nil）时返回空串。
//
// 与 Node 的 zod 语义对齐：「未设置」与「设为空串」在凭据类配置上等价（两者都通不过 minLen）。
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// openProviderCost 建「供应商维度只读限额判定」适配器（调度模拟器用）。
//
// 为什么每次都新建一个 Service：Node 的模拟器同样每次都 resolveSystemTimezone()，
// 自然窗口（每日固定 / 周 / 月）的时区必须与数据面**当刻**的口径一致，否则同一条
// 「是否触顶」会在两处给出不同答案。Service 自身只持有窗口读法，代价是几次小分配，
// 而这条端点是管理面预览（低频）。
func openProviderCost(
	cfg config.Config,
	pools *store.Pools,
	redisClient redis.UniversalClient,
	logger *logx.Logger,
) adminapi.ProviderCostReader {
	if redisClient == nil || pools == nil {
		logger.Info("admin_provider_cost_skipped", map[string]any{
			"reason": "redis_or_store_unconfigured",
			"effect": "dispatch_simulator_health_limits_step_degrades",
		})
		return nil
	}
	registry, err := ratelimit.Embedded()
	if err != nil {
		logger.Warn("admin_provider_cost_unavailable", map[string]any{"stage": "registry", "error": err.Error()})
		return nil
	}
	scriptClient, err := ratelimit.New(redisClient, registry)
	if err != nil {
		logger.Warn("admin_provider_cost_unavailable", map[string]any{"stage": "client", "error": err.Error()})
		return nil
	}
	return providerCostAdapter{cfg: cfg, pools: pools, client: scriptClient, logger: logger}
}

// providerCostAdapter 每次调用按当刻时区取链构造 Service，再走只读判定。
type providerCostAdapter struct {
	cfg    config.Config
	pools  *store.Pools
	client *ratelimit.Client
	logger *logx.Logger
}

func (a providerCostAdapter) CheckProviderCostLimits(
	ctx context.Context,
	providerID int64,
	in limit.ProviderCostLimits,
) (bool, string) {
	location := a.cfg.ResolveLocation(readSystemTimezone(ctx, a.pools, a.logger))
	service, err := limit.New(limit.Config{
		Quotas:   storeQuotas{pools: a.pools},
		Ledger:   a.pools,
		Redis:    a.client,
		Location: location,
		Logger:   a.logger,
	})
	if err != nil {
		// Fail Open：读不到限额就别说「该供应商被排除」——预览不该平白少报候选。
		a.logger.Warn("admin_provider_cost_service_failed", map[string]any{"error": err.Error()})
		return true, ""
	}
	return service.CheckProviderCostLimits(ctx, providerID, in)
}
