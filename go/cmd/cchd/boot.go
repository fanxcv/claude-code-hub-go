package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/debugapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/deps"
	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/httpapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

const (
	// defaultDrainTimeout 是收到终止信号后排空在途请求的窗口。
	defaultDrainTimeout = 30 * time.Second
	// defaultShutdownTimeout 是关 listener 与等 handler 结束的上限。
	defaultShutdownTimeout = 10 * time.Second
	// defaultProbeTimeout 限制单次依赖探测的时长。
	defaultProbeTimeout = 2 * time.Second
	// readHeaderTimeout 防慢头攻击。
	readHeaderTimeout = 10 * time.Second
	// subscriberDialTimeout 是订阅连接的握手与读写超时。
	subscriberDialTimeout = 5 * time.Second
	subscriberIOTimeout   = 10 * time.Second
)

// dependencies 是启动流程需要的外部依赖面；*deps.Bundle 满足它。
type dependencies interface {
	httpapi.Prober
	Close() error
}

// startup 是进程装配缝：生产用 newStartup()，测试逐项替换。
type startup struct {
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// LookupEnv 区分「未设置」与「设为空串」，与 Node 的 zod 语义对齐。
	LookupEnv config.LookupEnvFunc
	// OpenDeps 建立 PostgreSQL / Redis 句柄。
	OpenDeps func(ctx context.Context, cfg config.Config, rules *cfgsync.Snapshot) (dependencies, error)
	// OpenSubscriber 建立 cfgsync 订阅连接；未配置 Redis 时返回 nil。
	OpenSubscriber func(cfg config.Config) (redis.UniversalClient, error)
	// RegisterDomains 登记本进程消费的配置域。
	RegisterDomains func(ctx context.Context, rules *rulesSync) error
	// OpenDataPlane 装配数据面处理器（返回处理器与释放函数）。
	//
	// unimplemented 是未实现路径的落点（前门的 404 终端）：数据面只承载已落地的路由，
	// 其余路径如实回 404，而不是自己答 501。
	// Pools 是与管理面共用的连接池（boot 开与关）。
	// 未配置 DSN 时返回 errDataPlaneSkeleton，此时数据面按设计缺席（骨架模式）。
	OpenDataPlane func(ctx context.Context, options dataPlaneOptions) (http.Handler, func(), error)
	// OpenAdminPlane 装配管理面处理器（返回处理器与释放函数）。
	//
	// 与数据面分开成两个缝：两者的失败语义相反——管理面装不起来只降级（路由回退 Node），
	// 数据面装不起来必须 fail fast。合成一个缝会让测试无法分别证明这两条。
	OpenAdminPlane func(options adminOptions) (http.Handler, func(), error)
	// OpenUIPlane 装配 UI 静态面（embed 产物）；仅在 CCH_EGRESS_PAGES=embed 时被调用。
	//
	// 它也 fail fast（与数据面同向）：页面面没有别的落点，静默服务一个空站点比起不来更难排查。
	// 其余两档（node/off）不走这里，行为一字不变。
	OpenUIPlane func(options uiOptions) (http.Handler, uiStatus, error)
	// Listen 建监听；注入以便测试用随机端口。
	Listen func(port int) (net.Listener, error)
	// StartJobs 起后台任务（云价格同步、可用性投影回填）。
	//
	// 与数据面分开成独立缝：任务的失败语义是「降级」（只记日志），数据面是「fail fast」。
	// 未配置 DSN 或全部开关关闭时返回 (nil, nil)。
	StartJobs func(ctx context.Context, options jobsOptions) (*jobsRuntime, error)
	// OpenDebugPlane 起性能剖析面（pprof + 运行时指标）。
	//
	// 失败语义同为「降级」：剖析面是观测手段，起不来不该拖倒数据面（与后台任务同向）。
	OpenDebugPlane func(options debugOptions) (*debugapi.Plane, error)
	// Signals 返回终止信号通道与解绑函数。
	Signals func() (<-chan os.Signal, func())

	DrainTimeout    time.Duration
	ShutdownTimeout time.Duration
	ProbeTimeout    time.Duration
}

// newStartup 返回生产装配。
func newStartup() startup {
	return startup{
		Logger:    logx.New(os.Stderr),
		LookupEnv: config.LookupFromOS(),
		OpenDeps: func(ctx context.Context, cfg config.Config, rules *cfgsync.Snapshot) (dependencies, error) {
			return deps.New(ctx, cfg, rules)
		},
		OpenSubscriber: openSubscriber,
		RegisterDomains: func(ctx context.Context, rules *rulesSync) error {
			return registerDomains(ctx, rules)
		},
		OpenDataPlane:   openDataPlane,
		OpenAdminPlane:  openAdminPlane,
		OpenDebugPlane:  openDebugPlane,
		OpenUIPlane:     openUIPlane,
		Listen:          listenOn,
		StartJobs:       startJobs,
		Signals:         notifySignals,
		DrainTimeout:    defaultDrainTimeout,
		ShutdownTimeout: defaultShutdownTimeout,
		ProbeTimeout:    defaultProbeTimeout,
	}
}

// runWith 执行完整的启动、服务与关闭流程。
//
// 顺序：装载配置（非法即 fail fast）→ 建依赖 → 建订阅连接并登记配置域 → 建前门 →
// 监听并起服务（此刻 /v1/_ping 与 /readyz 已可答，后者在冷启动期报 503）→
// 收信号 → 停止接纳新请求并排空在途 → 关订阅 → 关依赖 → 退出。
//
// 与「先订阅再监听」的差别：探针在冷启动期必须有人应答，否则编排层无法区分
// 「正在预热」与「进程没起来」，因此监听先于配置域装载完成。
func runWith(rootCtx context.Context, options startup) error {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}

	cfg, err := config.LoadLookup(options.LookupEnv)
	if err != nil {
		// 配置错误直接 fail fast：带着错配启动会在数据面上分叉，比不启动更糟。
		logger.Error("config_invalid", map[string]any{"error": err.Error()})
		return err
	}
	logger.Info("config_loaded", cfg.Redacted())
	if !cfg.MemoryLimitSet {
		logger.Warn("gomemlimit_unset", map[string]any{
			"hint": "部署侧建议显式设置 GOMEMLIMIT，超限应表现为拒绝而不是 OOM 退出",
		})
	}

	rules := cfgsync.New()
	rulesState := newRulesSync(logger, rules)

	rootCtx, cancelRoot := context.WithCancel(rootCtx)
	defer cancelRoot()

	// 迁移必须在任何面装配之前：schema 未就位时装配出的 handler 只会以 500 暴露缺列，
	// 而 Node 的对应步骤同样早于应用装配（src/instrumentation.ts:584-598）。
	// 失败即返回错误，由 main 退出 1，与 Node 的 process.exit(1) 同义。
	//
	// 不需要额外开关来「在测试里跳过迁移」：用例只需在 LookupEnv 里答 AUTO_MIGRATE=false，
	// 走的正是生产那一条判定路径。
	if err := runStartupMigrations(
		rootCtx, cfg.DSN, options.LookupEnv, logger, defaultMigrationRetry(),
	); err != nil {
		logger.Error("migration_aborted", map[string]any{"error": err.Error()})
		return err
	}

	bundle, err := options.OpenDeps(rootCtx, cfg, rules)
	if err != nil {
		logger.Error("deps_init_failed", map[string]any{"error": err.Error()})
		return err
	}

	// 订阅连接与命令连接分离：Pub/Sub 会长期占用一条连接，混用会挤占命令道的延迟。
	subscriber, err := options.OpenSubscriber(cfg)
	if err != nil {
		logger.Error("subscriber_init_failed", map[string]any{"error": err.Error()})
		_ = bundle.Close()
		return err
	}
	if subscriber != nil {
		rulesState.attach(subscriber)
	}

	// 连接池：数据面与管理面共用同一套。
	//
	// 为什么不各自开一套：DB_POOL_MAX 是「整进程的物理连接上限」，两面各开一份就是两倍连接，
	// 而两边的准入预算又各算一次。池的**收口排在两个面之后**（见 closeAll）——handler 仍可能
	// 在返回途中用它写终态。store.Open 是惰性的，这里不建任何物理连接。
	var storePools *store.Pools
	if cfg.DSN != "" {
		opened, openErr := store.Open(rootCtx, store.Options{
			DSN:                 cfg.DSN,
			Budget:              cfg.Pool,
			Timeouts:            cfg.DB,
			ApplicationNameBase: storeAppName,
		})
		if openErr != nil {
			logger.Error("store_pools_init_failed", map[string]any{"error": openErr.Error()})
			_ = bundle.Close()
			return openErr
		}
		storePools = opened
	}

	// 通知的冷却去重连接（缓存命中率告警的「读」与「写」共用一条命令连接）。
	// 与订阅连接分开：Pub/Sub 会长期占用一条连接，混用会挤占命令道的延迟。
	// 起不来只降级：没有去重最多重复发一次告警，不该让进程起不来。
	notifyRedis, notifyRedisErr := openCommandRedis(cfg)
	if notifyRedisErr != nil {
		logger.Warn("notify_redis_unavailable", map[string]any{
			"reason": "命令连接建立失败，缓存命中率告警将不去重",
			"error":  notifyRedisErr.Error(),
		})
	}

	// 数据面池的释放函数在装配成功后才赋值；closeAll 按引用捕获，故这里先声明。
	var closeDataPlane func()
	// 管理面的释放函数同理（守卫的适配器缓存与命令连接）。
	var closeAdminPlane func()
	// 自愈巡检的停止函数同理（它只读写共享池，故也必须早于池关闭而停）。
	var stopPatrol func()

	// 收口顺序固定为「订阅 → 订阅连接 → 管理面 → 数据面 → 依赖 → 共享池」。
	// 反过来会让装载回调撞上已关闭的依赖；两个面必须早于依赖与共享池关——handler 还可能在返回
	// 途中用它写终态（管理面的审计是 fire-and-forget，同样可能在返回后才落库）。
	// 任何早退路径都由 defer 兜底，closeAll 幂等，故不会重复收口。
	var closeOnce sync.Once
	closeAll := func() {
		closeOnce.Do(func() {
			if closeErr := rulesState.Close(); closeErr != nil {
				logger.Warn("rules_close_failed", map[string]any{"error": closeErr.Error()})
			}
			if subscriber != nil {
				if closeErr := subscriber.Close(); closeErr != nil {
					logger.Warn("subscriber_close_failed", map[string]any{"error": closeErr.Error()})
				}
			}
			if closeAdminPlane != nil {
				closeAdminPlane()
			}
			if closeDataPlane != nil {
				closeDataPlane()
			}
			if notifyRedis != nil {
				if closeErr := closeRedis(notifyRedis); closeErr != nil {
					logger.Warn("notify_redis_close_failed", map[string]any{"error": closeErr.Error()})
				}
			}
			// 巡检在关池之前停：它每轮都要写库，池关了之后的写只会变成无用的报错。
			if stopPatrol != nil {
				stopPatrol()
			}
			if closeErr := bundle.Close(); closeErr != nil {
				logger.Warn("deps_close_failed", map[string]any{"error": closeErr.Error()})
			}
			// 共享池最后关：两个面的终态写入与审计都经过它。
			if storePools != nil {
				if closeErr := storePools.Close(); closeErr != nil {
					logger.Warn("store_pools_close_failed", map[string]any{"error": closeErr.Error()})
				}
			}
		})
	}
	defer closeAll()
	if err := options.RegisterDomains(rootCtx, rulesState); err != nil {
		logger.Error("rules_register_failed", map[string]any{"error": err.Error()})
		return err
	}

	frontDoor := egress.New(logger)

	// 数据面：本进程承载全部路由，未落地的路由回 404（unimplemented）。
	dataPlane := frontDoor.Unimplemented()
	// 亲和开关结论由装配回填，供 /readyz 如实报告（装配前为「未装配」。
	var affinity affinityStatus
	// 结算等待面：由数据面自行提供（未装配数据面时为 nil）。
	// 退出序列必须等它归零之后才能关依赖——关连接池会把尚未发出的终态 UPDATE 一并带走。
	var settlements settlementWaiter
	// 异步终态写队列的冲刷面（同步写模式下数据面不提供）：退出序列要在关池之前先把队列写干净。
	var settlementQueue settlementFlusher
	if options.OpenDataPlane != nil {
		handler, closeFn, dataPlaneErr := options.OpenDataPlane(rootCtx, dataPlaneOptions{
			Cfg:            cfg,
			Logger:         logger,
			Rules:          rulesState,
			Pools:          storePools,
			Fallback:       frontDoor.Unimplemented(),
			AffinityReport: func(status affinityStatus) { affinity = status },
		})
		switch {
		case errors.Is(dataPlaneErr, errDataPlaneSkeleton):
			logger.Info("dataplane_absent", map[string]any{"reason": "DSN not configured"})
		case dataPlaneErr != nil:
			// 有依赖却装不上数据面属装配错误：宁可起不来，也不要让前门把请求交给空处理器。
			logger.Error("dataplane_init_failed", map[string]any{"error": dataPlaneErr.Error()})
			closeAll()
			return dataPlaneErr
		case handler != nil:
			dataPlane = handler
			if waiter, ok := handler.(settlementWaiter); ok {
				settlements = waiter
			}
			if flusher, ok := handler.(settlementFlusher); ok {
				settlementQueue = flusher
			}
			if closeFn != nil {
				closeDataPlane = closeFn
			}
		}
	}

	// 管理面：已实现的管理路由由本进程作答，未注册的路径回 404（unimplemented）。
	//
	// 装配失败只降级不 fail fast（与数据面相反）：一个装不起来的管理面不影响数据面可用性，
	// 只影响「管理面是否可用」——而这件事由日志如实报出。
	adminPlane := frontDoor.Unimplemented()
	// 通知调度器：settings PUT / 绑定 PUT 的重排入口（Node 的 scheduleNotifications）。
	// 构造点必须早于管理面装配——管理面的 Deps 要拿它；而任务注册在后台任务装配处。
	notifyScheduler := newNotifyScheduler(logger, storePools, notifyRedis, options.LookupEnv)
	// nil 指针不能直接塞进接口（那会得到一个非 nil 的「空实现」，让 adminapi 的未装配判断失效）。
	var notifyRescheduler adminapi.NotifyRescheduler
	if notifyScheduler != nil {
		notifyRescheduler = notifyScheduler
	}
	// 管理面守卫的只读会话解析入口：装配成功后回填，交给 UI 面做壳注入（见 ui.go）。
	// 装配失败时保持 nil——那时管理面整个缺席，壳也拿不到身份，与降级语义一致。
	var adminSessions adminapi.SessionResolver
	if storePools != nil && options.OpenAdminPlane != nil {
		adminHandler, adminClose, adminErr := options.OpenAdminPlane(adminOptions{
			Cfg:             cfg,
			Logger:          logger,
			Pools:           storePools,
			Rules:           rulesState,
			Fallback:        frontDoor.Unimplemented(),
			NotifyScheduler: notifyRescheduler,
			SessionResolver: func(resolver adminapi.SessionResolver) {
				adminSessions = resolver
			},
		})
		if adminErr != nil {
			logger.Error("admin_plane_degraded", map[string]any{
				"error":  adminErr.Error(),
				"action": "management_requests_return_404",
			})
		} else {
			adminPlane = adminHandler
			closeAdminPlane = adminClose
		}
	} else {
		// 无 DSN：认证与审计都没有落点，管理面整个缺席。
		logger.Info("admin_plane_degraded", map[string]any{
			"reason": "DSN not configured",
			"action": "management_requests_return_404",
		})
	}

	// 自愈巡检：补写「已开行但从未终态」的行（断线搮上进程退出的缺陷，见
	// 它与请求归属无关：谁写的行都该被修。
	if storePools != nil {
		patrolStop, patrolErr := startPatrol(rootCtx, cfg, logger, storePools)
		switch {
		case patrolErr != nil:
			logger.Warn("patrol_init_failed", map[string]any{
				"error":  patrolErr.Error(),
				"action": "unsettled_rows_remain_unrepaired",
			})
		case patrolStop != nil:
			stopPatrol = patrolStop
		}
	}

	// 页面面是「非 API 路径」的终端：off 档由本进程自答（见 httpapi 的运维面），
	// embed 档换成 UI 静态处理器（下）。
	ui := uiStatus{Mode: cfg.EgressPages}
	pages := frontDoor.Middleware
	if cfg.EgressPages == config.EgressPagesEmbed {
		if options.OpenUIPlane == nil {
			// 装配缝缺失属装配错误：宁可带着明确报错起不来，也不要空指针崩在前门中间件里。
			err := errors.New("CCH_EGRESS_PAGES=embed 但未装配 UI 面（OpenUIPlane）")
			logger.Error("ui_plane_init_failed", map[string]any{"error": err.Error()})
			closeAll()
			return err
		}
		uiHandler, opened, uiErr := options.OpenUIPlane(uiOptions{
			Logger:   logger,
			Pools:    storePools,
			Sessions: adminSessions,
		})
		if uiErr != nil {
			// 产物缺失不能静默降级：起一个只回白屏的进程比起不来更难排查。
			logger.Error("ui_plane_init_failed", map[string]any{"error": uiErr.Error()})
			closeAll()
			return uiErr
		}
		ui = opened
		// 页面面全部归本进程，故终端就是 UI 处理器，不传 httpapi 的运维面落点。
		pages = func(http.Handler) http.Handler { return frontDoor.Middleware(uiHandler) }
		logger.Info("ui_plane_ready", map[string]any{
			"mode":    string(ui.Mode),
			"files":   ui.Build.Files,
			"bytes":   ui.Build.Bytes,
			"locales": ui.Build.Locales,
			"buildId": ui.Build.BuildID,
		})
	}

	var listening atomic.Bool
	prober := readyProber{dependencies: bundle, rules: rulesState, affinity: affinity, ui: ui}
	api := httpapi.New(httpapi.ServerOptions{
		Logger:        logger,
		Prober:        prober,
		Configuration: prober.DataPlaneConfiguration,
		Listening:     listening.Load,
		Draining:      frontDoor.Draining,
		FrontDoor:     frontDoor.Middleware,
		Pages:         pages,
		// 版本与 Node 的 `getAppVersion()` 同源（环境变量 → VERSION 文件 → 常量），
		// 供 `/api/health*` 的 version 字段使用；展示值由 httpapi 去 `v` 前缀。
		Version:      func() string { return uiAppVersion(os.Getenv) },
		DataPlane:    dataPlane,
		AdminPlane:   adminPlane,
		ProbeTimeout: options.ProbeTimeout,
	})

	listener, err := options.Listen(cfg.PublicPort)
	if err != nil {
		logger.Error("listen_failed", map[string]any{
			"port":  cfg.PublicPort,
			"error": err.Error(),
		})
		return err
	}
	listening.Store(true)

	httpServer := &http.Server{
		// 边缘处理器包在最外层：整个 HTTP 处理器都是它的内部处理器（见 ws.go 的文件注释）。
		Handler:           newWebSocketEdge(api.Handler(), logger),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	logger.Info("server_listening", map[string]any{
		"port":        cfg.PublicPort,
		"addr":        listener.Addr().String(),
		"egressPages": string(cfg.EgressPages),
	})

	// 剖析面起在监听之后、后台任务之前：作业启动期的 CPU 正是要解释的尖峰来源之一，
	// 起晚了就抓不到它。默认关闭，关闭态只有 404。
	var debugPlane *debugapi.Plane
	if options.OpenDebugPlane != nil {
		plane, debugErr := options.OpenDebugPlane(debugOptions{
			Cfg:        cfg,
			Logger:     logger,
			Collectors: newDebugCollectors(storePools),
		})
		if debugErr != nil {
			logger.Error("debug_plane_unavailable", map[string]any{
				"enabled": cfg.Pprof.Enabled,
				"addr":    cfg.Pprof.Addr,
				"error":   debugErr.Error(),
			})
		} else {
			debugPlane = plane
		}
	}

	// 后台任务在监听之后、收信号之前启动：
	//   - 在监听之后：任务的首次执行要拉 28 MiB 并扫库，不能挡在除了探针以外的任何东西前面；
	//   - 起不来不阻塞：后台任务缺失是运维面降级，不该让数据面一起倒下（与 Node 一致）。
	// 任务各自的启动延迟由 scheduler 错峰（默认每档 3 秒）。
	var jobRuntime *jobsRuntime
	if options.StartJobs != nil {
		started, startJobsErr := options.StartJobs(rootCtx, jobsOptions{
			Logger:    logger,
			LookupEnv: options.LookupEnv,
			Pools:     storePools,
			Notify:    notifyScheduler,
		})
		if startJobsErr != nil {
			logger.Error("jobs_init_failed", map[string]any{"error": startJobsErr.Error()})
		} else {
			jobRuntime = started
		}
	}

	// 配置域订阅在后台推进：冷启动期 /readyz 报 503，装载完成后转 ok。
	rulesReady := make(chan error, 1)
	go func() {
		startCtx, cancel := context.WithTimeout(rootCtx, ruleLoadTimeout)
		defer cancel()
		rulesReady <- rulesState.Start(startCtx)
	}()

	// 后台任务（探活 / 清理 / outbox 回收）：起在监听之后、写在收口之前。
	//
	// 位置理由：就绪探针必须早于后台任务可用（任务首轮可能碰库里的大表）；
	// 而收口必须晚于任务停——任务还在写探活结果时关连接池，那一批结果就随进程消失。
	ops, err := startOpsRuntime(rootCtx, cfg, storePools, logger, options.LookupEnv)
	if err != nil {
		// 后台任务起不来不是致命：数据面与管理面照常对外，只是拨测与清理缺席。
		logger.Error("ops_jobs_init_failed", map[string]any{"error": err.Error()})
	}

	signals, stopSignals := options.Signals()
	defer stopSignals()

	select {
	case received := <-signals:
		logger.Info("shutdown_started", map[string]any{
			"signal":             received.String(),
			"drainWindow":        options.DrainTimeout.Milliseconds(),
			"inFlight":           frontDoor.InFlight(),
			"pendingSettlements": pendingSettlements(settlements),
		})
	case serveErrValue := <-serveErr:
		if serveErrValue != nil {
			logger.Error("serve_failed", map[string]any{"error": serveErrValue.Error()})
			return serveErrValue
		}
		return nil
	case startErr := <-rulesReady:
		if startErr != nil {
			// 订阅不可用时不算致命：依赖探测会把 Redis 项报成故障，/readyz 保持 503。
			logger.Error("rules_sync_degraded", map[string]any{"error": startErr.Error()})
		} else {
			logger.Info("rules_sync_ready", map[string]any{
				"domains": len(rulesState.RegisteredDomains()),
				"version": rules.Version(),
			})
		}
		// 继续等待终止信号：就绪与退出是两件事。
		select {
		case received := <-signals:
			logger.Info("shutdown_started", map[string]any{
				"signal":             received.String(),
				"drainWindow":        options.DrainTimeout.Milliseconds(),
				"inFlight":           frontDoor.InFlight(),
				"pendingSettlements": pendingSettlements(settlements),
			})
		case serveErrValue := <-serveErr:
			if serveErrValue != nil {
				logger.Error("serve_failed", map[string]any{"error": serveErrValue.Error()})
				return serveErrValue
			}
			return nil
		}
	}

	drainErr := drainAndShutdown(httpServer, frontDoor, settlements, settlementQueue, logger, options.DrainTimeout, options.ShutdownTimeout)
	// 剖析面先关：它可能在跑一个长 profile 请求，先关才轮得到「停写 → 关依赖」这套顺序。
	closeDebugPlane(debugPlane, logger)
	// 后台任务必须先于连接池收口：任务在用池，先关池会让在途写入直接报错。
	// 两条运行时共用同一个连接池，故两个都先停（顺序无依赖，都是「停写、再关池」）。
	jobRuntime.Stop()
	ops.Stop()
	closeAll()

	logger.Info("shutdown_complete", map[string]any{
		"rulesLoaded":        rules.Loaded(),
		"inFlight":           frontDoor.InFlight(),
		"pendingSettlements": pendingSettlements(settlements),
	})
	return drainErr
}

// debugPlaneShutdownTimeout 是剖析面的关闭期限。
//
// 与 ShutdownTimeout 分开：剖析请求可以长跑（`/debug/pprof/profile?seconds=60`），
// 等它会把退出无限延后；1.5s 之后硬关，那个 profile 随之失败——观测面失败可接受，
// 拖住进程退出不可接受。
const debugPlaneShutdownTimeout = 1500 * time.Millisecond

// closeDebugPlane 关剖析面；未装配时为 no-op。
func closeDebugPlane(plane *debugapi.Plane, logger *logx.Logger) {
	if plane == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), debugPlaneShutdownTimeout)
	defer cancel()
	if err := plane.Close(ctx); err != nil {
		logger.Warn("debug_plane_close_incomplete", map[string]any{"error": err.Error()})
	}
}

// settlementWaiter 是数据面提供的「终态尚未落库」视图。
//
// 与 egress 在途计数的分工：在途计数跟请求走（handler 返回即归零），结算可能比请求活得久
// （客户端中断计量会刻意多引流一段）。两者都归零之前关依赖，就会永久丢掉终态
// 数据面未装配时不存在这个面，取 0。
type settlementWaiter interface {
	PendingSettlements() int64
	WaitSettlements(ctx context.Context) bool
}

func pendingSettlements(waiter settlementWaiter) int64 {
	if waiter == nil {
		return 0
	}
	return waiter.PendingSettlements()
}

// settlementFlusher 是数据面在**异步终态写模式**下提供的队列冲刷面。
//
// 同步写（默认）时数据面不提供它（断言失败即为 nil，整段跳过）。
// 异步时必须先冲再停，顺序在关连接池之前、jobRuntime/ops 停止之前：队列 worker 仍在写库，
// 先关池会让这些写直接报错，而它们是已经交付给客户端的请求的终态。
type settlementFlusher interface {
	FlushSettlements(ctx context.Context) error
	StopSettlements()
}

// drainAndShutdown 停止接纳新请求、排空在途、等终态落库、关闭服务器。
//
// 排空未完成会返回错误（退出码非零）而不是静默成功：在途请求被强退是审计事件，
// 编排层必须看得见，否则「重启后丢请求」会无声无息。终态未落库同理：
// 关连接池会把尚未发出的终态 UPDATE 一并带走，那些账本行会永久留在未终态。
func drainAndShutdown(
	httpServer *http.Server,
	frontDoor *egress.FrontDoor,
	settlements settlementWaiter,
	queue settlementFlusher,
	logger *logx.Logger,
	drainTimeout time.Duration,
	shutdownTimeout time.Duration,
) error {
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainTimeout)
	drainErr := frontDoor.Drain(drainCtx)
	cancelDrain()
	if drainErr != nil {
		logger.Error("drain_incomplete", map[string]any{
			"inFlight":           frontDoor.InFlight(),
			"pendingSettlements": pendingSettlements(settlements),
			"drainWindow":        drainTimeout.Milliseconds(),
			"error":              drainErr.Error(),
		})
	}
	settleErr := waitSettlements(settlements, logger, drainTimeout)
	// 异步终态写：等结算归零只能保证「队列里的都写完了」，而 flush 是幂等的，故再显式冲一次
	// （排空窗口里最后入队的那些正是它盖住的），随后停队列——此后的写入退回同步，不再有后台协程
	// 在关池之后动手。窗口与排空同宽。
	flushErr := flushSettlements(queue, logger, drainTimeout)
	// 关 listener 并等 handler 结束：排空只等前门的在途计数，连接层仍需显式收口。
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	shutdownErr := httpServer.Shutdown(shutdownCtx)
	cancelShutdown()
	if shutdownErr != nil {
		logger.Warn("shutdown_incomplete", map[string]any{"error": shutdownErr.Error()})
		_ = httpServer.Close()
	}

	// 终态未落库有两类互不包含的原因，各自都要能凭 errors.Is 认出来：
	//
	//   - settleErr：流 tracker 未归零（可能是排空窗口超时）；
	//   - flushErr：异步队列里有写入失败（terminal.ErrQueueWriteFailed）。
	//
	// 两类都不得被吞：任一非 nil 都意味着关池之后仍有终态没落库，进程不能以成功退出，
	// 否则这类缺口在日志之外完全不可见（那几条只能等 patrol 按年龄兜底，成本无从补回）。
	switch {
	case drainErr != nil && !errors.Is(drainErr, egress.ErrDrainTimeout):
		return errors.Join(drainErr, settleErr, flushErr)
	case drainErr != nil:
		return errors.Join(fmt.Errorf("排空超时：仍有在途请求未结束（%w）", drainErr), settleErr, flushErr)
	default:
		return errors.Join(settleErr, flushErr)
	}
}

// waitSettlements 在关闭依赖之前等终态落库，窗口与排空同宽。
//
// 为什么要等：handler 返回不等于终态已落库。退出序列的最后一步是关依赖（含连接池），
// 此刻尚未发出的终态 UPDATE 会随进程消失，而这类缺口在日志里完全不可见（见 §四的静默性）。
//
// 它排在 httpServer.Shutdown 之前：排空已经放行了（或已超时），此时要做的唯一一件事
// 就是让已经开始的终态写入落库；再等 handler 结束并不增加覆盖率，反而拉长退出时长。
func waitSettlements(waiter settlementWaiter, logger *logx.Logger, window time.Duration) error {
	if waiter == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	if waiter.WaitSettlements(ctx) {
		return nil
	}
	pending := waiter.PendingSettlements()
	logger.Error("settle_incomplete", map[string]any{
		"pendingSettlements": pending,
		"drainWindow":        window.Milliseconds(),
	})
	return fmt.Errorf("%w: 仍有 %d 条流终态未落库", errSettleIncomplete, pending)
}

// flushSettlements 把异步终态写队列冲干净并停掉；未装配（同步写模式）时是 no-op。
//
// 冲失败**返回错误**（含 terminal.ErrQueueWriteFailed 与前两者之一并合的超时），由
// drainAndShutdown 并进退出结论：队列里有写入失败就是「终态没落库」的一种，只记日志
// 会让进程带着未落库的终态以成功退出。失败的那几条最终由 patrol 按既有语义兑底。
func flushSettlements(queue settlementFlusher, logger *logx.Logger, window time.Duration) error {
	if queue == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	var flushErr error
	if err := queue.FlushSettlements(ctx); err != nil {
		flushErr = err
		logger.Error("settle_flush_incomplete", map[string]any{
			"drainWindow": window.Milliseconds(),
			"error":       err.Error(),
		})
	}
	// 停队列在关池之前：停掉之后不再有后台协程写库（此后的入队一律退回同步写）。
	// 停与冲分开：即使冲失败也要停，否则退出后仍有 worker 在写库。
	queue.StopSettlements()
	return flushErr
}

// readyProber 组合依赖探测与规则就绪，并在规则门无域可管时如实说明。
type readyProber struct {
	dependencies
	rules *rulesSync
	// affinity 是数据面装配出的亲和开关结论；零值（未装配）也会如实报告。
	affinity affinityStatus
	// ui 是页面面的装配结论（node/off/embed 与 embed 产物摘要）；零值同样如实报告。
	ui uiStatus
}

// DataPlaneConfiguration 把与就绪无关但排障必需的配置结论送进 /readyz 的 notes。
//
// 为什么必须报：亲和关掉只表现为「请求在多供应商间抖动」，没有任何错误；
// 页面归属同理——三种取值都不报错，只表现成行为不同。启动日志会被滚动掉，
// 而 /readyz 是随时可查的事实。
func (p readyProber) DataPlaneConfiguration() map[string]string {
	return map[string]string{
		"affinity": p.affinity.describe(),
		"pages":    p.ui.describe(),
	}
}

// RulesStatus 在「订阅就绪但尚未消费任何域」时补一句说明，避免读数被误当成「规则已装载」。
func (p readyProber) RulesStatus() (httpapi.DependencyStatus, string) {
	status, note := p.dependencies.RulesStatus()
	if status == httpapi.StatusOK && note == "" && len(p.rules.RegisteredDomains()) == 0 {
		return status, "订阅已就绪；本进程当前未登记任何配置域"
	}
	return status, note
}

// listenOn 在给定端口上监听全部地址。
func listenOn(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", port))
}

// openSubscriber 建 cfgsync 的订阅连接；未配置 Redis 时返回 nil。
func openSubscriber(cfg config.Config) (redis.UniversalClient, error) {
	if cfg.RedisURL == "" {
		return nil, nil
	}
	options, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		// 错误信息可能回显 URL，故只报类型不回显原文。
		return nil, fmt.Errorf("解析 REDIS_URL 失败（值已隐去）: %w", err)
	}
	options.MaxRetries = 2
	options.DialTimeout = subscriberDialTimeout
	options.ReadTimeout = subscriberIOTimeout
	options.WriteTimeout = subscriberIOTimeout
	return redis.NewClient(options), nil
}

// notifySignals 订阅 SIGTERM / SIGINT。
func notifySignals() (<-chan os.Signal, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	return signals, func() { signal.Stop(signals) }
}
