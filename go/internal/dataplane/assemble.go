package dataplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/clientver"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/health"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ingress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/replay"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
	"github.com/redis/go-redis/v9"
)

// shouldRecordEndpointFailure 复刻 Node 的端点级记账判定（forwarder.ts:2569）：
// 只有超时与系统错误算端点问题，普通 5xx 不算。
//
// 为什么必须窄：端点失败远比供应商失败频繁，照搬「供应商失败即端点失败」会让一个端点
// 因上游偶发 5xx 被熔断，而它其实是健康的。
func shouldRecordEndpointFailure(failure *forward.Failure) bool {
	if failure == nil || failure.EndpointID <= 0 {
		return false
	}
	return failure.StatusCode == timeoutStatusCode || failure.Category == forward.CategorySystemError
}

// timeoutStatusCode 是 Node 判定「超时」的状态码（forwarder.ts:2560 的 isTimeoutError）。
// 它同时决定端点级熔断是否记账：超时算端点问题，普通 5xx 不算。
const timeoutStatusCode = 524

// 本文件是生产装配：把真实包接成 dataplane.Options。cmd/cchd 只调用 NewStoreBacked，
// 不关心内部接了哪些包——装配细节集中在这里，接线缺口也集中在这里（见 Assembly 字段说明）。

// 出厂常量。
const (
	// idleTimeoutCacheSize 是供应商静默超时的进程内缓存容量（条目数等于启用态供应商数）。
	idleTimeoutCacheSize = 4096
)

// StoreOptions 是生产装配参数。
type StoreOptions struct {
	// Pools 是数据库分道，必填。
	Pools *store.Pools
	// Redis 是会话绑定与回放共用的命令连接；nil 表示未配置 Redis（会话退化为每请求独立）。
	Redis redis.UniversalClient
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Registry / Bus 是配置失效通道；nil 表示缓存只靠 TTL 自愈。
	Registry *cfgsync.Registry
	Bus      *cfgsync.Bus
	// Fallback 是未实现路由的落点（生产：前门的 Node 回退）。
	Fallback http.Handler
	// RateLimit / AuthThrottle 是限流实现（限流包装配好后从外部注入）；nil 表示本波未接线，
	// 链上会各留一条 warn（语义见 guard.RateLimiter 的说明）。
	RateLimit    guard.RateLimiter
	AuthThrottle guard.RateLimiter
	// Settler 复用给拦截/预热日志；nil 时自建。
	Settler *terminal.Settler
	// NewRows 是「请求日志有新行落库」的通知面（terminal.Options.NewRows；
	// 生产实现见 usagefeed.Hub）。
	//
	// nil 表示未装配：使用记录页的推送模式收不到信号（前端仍可轮询）。
	NewRows terminal.NewRowsNotifier
	// RouteOptions 覆盖选路器参数（健康、亲和、闸门）；Source 恒由本函数填。
	RouteOptions route.Options
	// DialOptions 覆盖拨号参数（超时、上游连接上限）。
	DialOptions dial.Options
	// Limits 是转发路径上限；零值取 forward 的默认。
	Limits forward.Limits
	// ReplayTTLSeconds / ReplayMaxPayloadBytes / ReplayMaxConcurrentSpools 是回放的资源上限；
	// 零值取 replay 包默认。
	ReplayTTLSeconds          int
	ReplayMaxPayloadBytes     int64
	ReplayMaxConcurrentSpools int64
	// ReplayEnabled 是 ENABLE_REQUEST_REPLAY 的出厂值：system_settings.replay_enabled 为 NULL
	// 时用它（与 Node 的 `settings.replayEnabled ?? env` 同序）。
	ReplayEnabled bool
	// HedgeLoserDrainTimeoutMS 是竞速输家后台引流的上限（HEDGE_LOSER_DRAIN_TIMEOUT_MS）；
	// 0 取 forward 出厂默认（120s）。
	HedgeLoserDrainTimeoutMS int
	// MaxUpstreamConnections 为 0 时不限制在途上游请求数。
	MaxUpstreamConnections int
	// ClientIP 覆盖客户端 IP 解析；nil 时用数据面的保守取值。
	ClientIP func(*http.Request) string
	// EndpointCircuitBreakerEnabled 取自 ENABLE_ENDPOINT_CIRCUIT_BREAKER：关闭时端点级与
	// 厂级熔断一律不写（Node 在同一开关上短路，见 endpoint-circuit-breaker.ts:334）。
	EndpointCircuitBreakerEnabled bool
	// SessionArtifacts 是会话工件的开关与体积上限（STORE_SESSION_MESSAGES /
	// SESSION_REQUEST_ARTIFACT_MAX_BYTES / STORE_SESSION_RESPONSE_BODY）。
	SessionArtifacts session.SessionArtifactOptions
	// Now 可注入时钟；nil 时用 time.Now。
	Now func() time.Time
}

// Assembly 是装配结果：处理器 + 释放函数。
type Assembly struct {
	// Handler 是数据面处理器，注入 httpapi.ServerOptions.DataPlane。
	Handler http.Handler
	// Adapters 是守卫链适配器集合（便于排障与观测，例如已开行数）。
	Adapters *guard.Adapters
	// SessionBinder 是会话绑定实现；nil 表示未接线（会话 id/序号两列写 NULL）。
	SessionBinder guard.SessionBinder
	// Missing 列出本波仍未接线的缝隙，供启动日志一次性说明（不是错误）。
	Missing []string
}

// NewStoreBacked 用真实依赖装配数据面。
//
// 已接线的缝隙：会话绑定（需 Redis）、客户端 IP 信任链、错误规则匹配、假 200 检测、计费、
// 熔断记账、版本检查、回放（命中短路 + owner spool，见 replay.go）。
//
// 仍留的缺口（在 Assembly.Missing 里如实列出，不静默）：
//   - RateLimit / AuthThrottle：仅当调用方注入实现（见 StoreOptions）。
//   - SessionBinder：仅在未配置 Redis 时缺失（配置缺口，非接线缺口）。
//
// 计费（见 cost.go）：取价基准由 system_settings.billing_model_source 决定（主模型无价格
// 时回落另一个），倍率取选中供应商与分组求交两侧；重定向后的模型名经 Plan 随终态事实到达。
// 有意未搬运的分支（参数缺口）记在 terminal/cost.go 的文件头。
// resolveHealth 给出熔断读取面（供应商级 ProviderOpen 与端点级 EndpointOpen 共用）。
//
// 为什么需要兜底而不是只读 RouteOptions.Health：生产装配（cmd/cchd）只填了 Affinity 与同协议
// 权重，**从未注入 Health**，而 route.Selector 的 healthRejection 与 candidateSource 的端点判定在
// Health 为 nil 时**一律放行**（见 upstream.go 对 nil 的说明）。后果极隐蔽：写侧照常记账、管理面
// 照常显示「已熔断」，只有选路一侧永不生效——故这条缺口不会报错，只表现为「明明熔断了还去请求它」。
// Redis 与开关本来就在本层的 StoreOptions 里，故在这里补齐，让正确行为不依赖调用方记得接线；
// 已注入的实现优先（保留测试与将来显式装配的注入点）。
//
// 无 Redis 时返回 nil：与 Node 在无 Redis 时同义（fail-open），且此时写侧也无处记账。
func resolveHealth(options StoreOptions) *route.HealthReader {
	if options.RouteOptions.Health != nil {
		return options.RouteOptions.Health
	}
	if options.Redis == nil {
		return nil
	}
	return route.NewHealthReader(route.HealthOptions{
		Redis:                         options.Redis,
		EndpointCircuitBreakerEnabled: options.EndpointCircuitBreakerEnabled,
		Now:                           options.Now,
	})
}

func NewStoreBacked(options StoreOptions) (*Assembly, error) {
	if options.Pools == nil {
		return nil, ErrNoStore
	}
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	if options.Now == nil {
		options.Now = time.Now
	}

	// 拦截类终态（敏感词、预热等）的结算器。刻意与下面的主结算器**分成两个实例**：
	// 主结算器带 public-status 投影旁路（rollup），而拦截路径历来不带（guard 侧自建时
	// 就是空 Options）。这里给拦截路径加上「有新行」通知，但不给它加 rollup——
	// 与接线前的行为差异因此仅限于新增的信号本身。
	interceptSettler := options.Settler
	if interceptSettler == nil {
		interceptSettler = terminal.New(terminal.StoreWriter{Pools: options.Pools}, terminal.Options{
			NewRows: options.NewRows,
			Logger:  logger,
		})
	}

	// 熔断读取面（供应商级 ProviderOpen 与端点级 EndpointOpen 共用）。
	//
	// 必须在这里算：**真正选供应商的是守卫链里的选路器**（guard.AdapterOptions.RouteOptions
	// → adapters.go 的 newProviderRouter），本函数后面那个 selector 只负责故障转移。
	// 两处加候选投影共三个消费点任意一处拿到 nil，熔断就只影响展示、不影响行为。
	healthReader := resolveHealth(options)
	routeOptions := options.RouteOptions
	routeOptions.Health = healthReader

	adapters, err := guard.NewAdapters(guard.AdapterOptions{
		Pools:        options.Pools,
		Bus:          options.Bus,
		Registry:     options.Registry,
		Logger:       logger,
		RouteOptions: routeOptions,
		Settler:      interceptSettler,
		RateLimit:    options.RateLimit,
		AuthThrottle: options.AuthThrottle,
		BodyOptions: guard.BodyAccessOptions{
			// 解压限额进程级共享：在途解压任务与字节都受预算约束（解压发生在鉴权之后）。
			Ingress: ingress.DefaultOptions(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("dataplane: 守卫适配器构造失败: %w", err)
	}

	// 会话绑定：与 Node 共用同一套 Redis 键与 Lua 脚本（切换期间两侧互相看得见），
	// 脚本注册表取自 ratelimit 的内嵌清单——会话包只经 EvalConst 按常量名调用，不另存一份脚本。
	var sessionBinder guard.SessionBinder
	// telemetry 与绑定**共用同一个 Binder**：同一套键名与 TTL 口径，分开两个实例只会在
	// 未来某次改键名时让写侧与读侧静默分叉。
	var telemetry SessionTelemetry
	if options.Redis != nil {
		registry, registryErr := ratelimit.Embedded()
		if registryErr != nil {
			return nil, fmt.Errorf("dataplane: 限流脚本注册表装载失败: %w", registryErr)
		}
		scriptClient, scriptErr := ratelimit.New(options.Redis, registry)
		if scriptErr != nil {
			return nil, fmt.Errorf("dataplane: 会话绑定脚本层构造失败: %w", scriptErr)
		}
		binder := session.NewBinder(scriptClient)
		sessionBinder = session.NewSessionBinderAdapter(session.BinderOptions{
			Client: binder,
			Logger: logger,
		})
		telemetry = newSessionTelemetry(binder, options.SessionArtifacts, logger)
	}

	// 会话观测写侧：写活跃 ZSET、session:{id}:info、并发计数与请求工件。
	//
	settler := options.Settler
	if settler == nil {
		settler = terminal.New(terminal.StoreWriter{Pools: options.Pools}, terminal.Options{
			// public-status 投影的事件捕获旁路（Node: `src/repository/message.ts:246`）。
			// 缺 Redis 时 recorder 为 nil → 旁路整段跳过，结算路径行为与接线前完全一致。
			Rollup: publicStatusRollupRecorder(options, logger),
			// 使用记录页推送模式的信号源（终态提交后发一条「有新行」）。
			NewRows: options.NewRows,
			Logger:  logger,
		})
	}

	// 计费取价：设置源与价格表都走真实依赖；任一不可用时解析器为 nil（终态照写，只是不带金额）。
	costs := newCostResolver(adapters.Settings, options.Pools, logger)

	// 熔断记账：失败与成功成对接线。只接失败会让开闸后再无归闭路径（见 health 包注释）。
	healthWriter := health.NewWriter(health.Options{
		Redis:                         options.Redis,
		EndpointCircuitBreakerEnabled: options.EndpointCircuitBreakerEnabled,
		Settings:                      adapters.Settings,
		Now:                           options.Now,
		Logger:                        logger,
	})
	recordFailure := func(ctx context.Context, failure *forward.Failure) {
		if failure == nil {
			return
		}
		if err := healthWriter.RecordProviderFailure(ctx, failure.ProviderID, failure.Err); err != nil {
			logger.Warn("circuit_breaker_provider_failure_failed", map[string]any{
				"provider_id": failure.ProviderID,
				"error":       err.Error(),
			})
		}
		// 端点级只记超时与系统错误（Node forwarder.ts:2569 的同一判定）；供应商级失败
		// 远多于端点级失败，照搬会让端点熔断被普通的 5xx 拖开。
		if shouldRecordEndpointFailure(failure) {
			if err := healthWriter.RecordEndpointFailure(ctx, failure.EndpointID, failure.Err); err != nil {
				logger.Warn("circuit_breaker_endpoint_failure_failed", map[string]any{
					"endpoint_id": failure.EndpointID,
					"error":       err.Error(),
				})
			}
		}
	}
	recordSuccess := func(ctx context.Context, providerID int64, endpointID int64) {
		if err := healthWriter.RecordProviderSuccess(ctx, providerID); err != nil {
			logger.Warn("circuit_breaker_provider_success_failed", map[string]any{
				"provider_id": providerID,
				"error":       err.Error(),
			})
		}
		if err := healthWriter.RecordEndpointSuccess(ctx, endpointID); err != nil {
			logger.Warn("circuit_breaker_endpoint_success_failed", map[string]any{
				"endpoint_id": endpointID,
				"error":       err.Error(),
			})
		}
	}

	// 回放：热层与持久层共用同一套 Redis 键与 Lua（切换期间与 Node 互相命中）。
	// 没有 Redis 就不接线：回放的热层就是 Redis 本身，缺它只能全量 miss 并白白消耗租约。
	var replayWiring *ReplayWiring
	if options.Redis != nil {
		replayStore, replayErr := replay.NewStore(replay.StoreOptions{
			Redis: options.Redis,
			Pools: options.Pools,
			TTL:   time.Duration(options.ReplayTTLSeconds) * time.Second,
			Now:   options.Now,
		})
		if replayErr != nil {
			return nil, fmt.Errorf("dataplane: 回放存储构造失败: %w", replayErr)
		}
		envReplayEnabled := options.ReplayEnabled
		replayWiring = &ReplayWiring{
			Store:               replayStore,
			Pools:               options.Pools,
			MaxPayloadBytes:     options.ReplayMaxPayloadBytes,
			MaxConcurrentSpools: options.ReplayMaxConcurrentSpools,
			Now:                 options.Now,
			// 运行时开关优先取设置快照，NULL 时才回落出厂值（Node 同序）。
			Enabled: func(ctx context.Context) bool {
				settings, settingsErr := adapters.Settings.FindSystemSettings(ctx)
				if settingsErr == nil && settings != nil && settings.ReplayEnabled != nil {
					return *settings.ReplayEnabled
				}
				return envReplayEnabled
			},
		}
	}

	dialOptions := options.DialOptions
	if options.MaxUpstreamConnections > 0 {
		dialOptions.MaxUpstreamConnections = options.MaxUpstreamConnections
	}
	dialClient, err := dial.New(dialOptions)
	if err != nil {
		return nil, fmt.Errorf("dataplane: 拨号器构造失败: %w", err)
	}

	// 故障转移用的选路器：Source 由本函数固定为真实快照面，避免「测试用假源、生产用真源」的分叉。
	gates, providerCostUnwired := buildGates(options)

	selector := route.NewSelector(route.Options{
		Source:       route.NewStoreSource(options.Pools),
		Health:       healthReader,
		Affinity:     options.RouteOptions.Affinity,
		Gates:        gates,
		Rand:         options.RouteOptions.Rand,
		Now:          options.Now,
		Logger:       logger,
		EndpointGate: options.RouteOptions.EndpointGate,
	})

	deps := guard.Deps{Logger: logger}
	adapters.Apply(&deps)
	// 会话绑定在 Apply 之后注入：Apply 不覆盖调用方显式接上的缝隙，顺序写反会得到静默的空会话。
	if sessionBinder != nil {
		deps.Sessions = sessionBinder
	}
	// 版本检查：UA 解析 + 用户版本记录 + GA 版本比对。fail-open，缺 Redis 时自动退化为放行。
	deps.Versions = clientver.NewChecker(clientver.Options{
		Redis:  options.Redis,
		Users:  options.Pools,
		Logger: logger,
		Now:    options.Now,
	})

	idleTimeouts := newIdleTimeoutCache(options.Pools, logger, options.Now)
	idles := idleTimeouts.lookup

	budget := gate.DefaultBudget()
	handler, err := New(Options{
		Logger:     logger,
		Base:       deps,
		Adapters:   adapters,
		Candidates: newCandidateSource(options.Pools, selector, healthReader, logger),
		// 聚合式模型列表（`/v1/models` 一族）需读全量供应商/分组/系统时区，只存在于存储层；
		// 不装配时这五条不注册并回退 Node（见 Options.ModelCatalog 的注释）。
		ModelCatalog: StoreModelCatalog{Pools: options.Pools},
		Settlers: func(state *RequestState) Settler {
			return &storeSettler{
				settler: settler,
				writer:  terminal.StoreWriter{Pools: options.Pools},
				state:   state,
				logger:  logger,
				costs:   costs,
				// F3b 缓存模拟列的开关（设置行优先、env 兜底）；与计费相互独立。
				cacheScore: newCacheScoreGate(adapters.Settings, cacheEffectivenessEnvDefault()),
				now:        options.Now,
			}
		},
		Forward: forward.Deps{
			Dial:   dialClient,
			Limits: options.Limits,
			Logger: logger,
			Now:    options.Now,
			// 错误规则与假 200 检测共用守卫侧的快照（同一个 cfgsync 通道，避免两套真相）。
			Rules:    adapters.Rules,
			Detector: adapters.Detector,
			// 熔断记账：与选路器的只读健康判定（RouteOptions.Health）成对，读写分属两侧。
			RecordFailure: recordFailure,
			RecordSuccess: recordSuccess,
		},
		Stream: forward.StreamOptions{
			Budget:              budget,
			Logger:              logger,
			Now:                 options.Now,
			IdleTimeoutFor:      idles,
			CaptureCommitMarker: false,
		},
		BodyOptions: guard.BodyAccessOptions{Ingress: ingress.DefaultOptions()},
		ClientIP:    options.ClientIP,
		// 有效分组来自密钥/用户缓存（与守卫链的分组过滤同一份读取），不另起一条查询链。
		EffectiveGroup: func(pc *pctx.Context) string {
			return adapters.Auth.ProviderGroup(context.Background(), pc)
		},
		Replay:    replayWiring,
		Telemetry: telemetry,
		// 工件选项同时进 Handler.Options：响应侧捕获要在这里判「本请求捕不捕正文」，
		// 而 Stream 的选项是每请求从 Handler.Options.Stream 拷贝的（见 forward 的
		// streamOptions 组装），在那里读会拿到未填的零值。
		SessionArtifacts: options.SessionArtifacts,
		Fallback:         options.Fallback,
		Now:              options.Now,
		// 流式竞速：开关与并发上限在设置里（判定见 hedge.go），本层只给计费缝与引流上限。
		// 输家字节上限取 forward 出厂值（Node 侧走共享预算的准入，没有对应环境变量）。
		Hedge: HedgeWiring{
			Enabled:            true,
			Costs:              costs,
			Pools:              options.Pools,
			LoserDrainTimeout:  time.Duration(options.HedgeLoserDrainTimeoutMS) * time.Millisecond,
			LoserMaxDrainBytes: 0,
			Logger:             logger,
		},
	})
	if err != nil {
		return nil, err
	}

	missing := make([]string, 0, 1)
	if sessionBinder == nil {
		// 没有 Redis 就没有会话绑定：这是配置缺口而不是接线缺口，如实报出来。
		missing = append(missing, "SessionBinder")
		// 会话观测挂在同一套 Redis 键上，没 Redis 就一起缺——两条缺口同源，分开报只会
		// 让启动日志看起来像两个问题。
		missing = append(missing, "SessionTelemetry")
	}
	if options.RateLimit == nil {
		// 限流未注入：链上会各留一条 warn，启动日志里必须看得见。
		missing = append(missing, "RateLimit")
	}
	if options.AuthThrottle == nil {
		missing = append(missing, "AuthThrottle")
	}
	if providerCostUnwired {
		// 供应商级金额限额没接上：走 Go 的请求就不判供应商额度（Node 在 Step 4 判）。
		missing = append(missing, "ProviderCostLimits")
	}
	return &Assembly{
		Handler:       handler,
		Adapters:      adapters,
		SessionBinder: sessionBinder,
		Missing:       missing,
	}, nil
}

// providerCostLimits 把选路视图的限额列投影成 `internal/limit` 的入参对象。
//
// 为什么在装配层转：`route` 不能依赖 `limit`（选路包在下、限额包在上），于是限额列的
// 「选路视角类型」（route.ProviderCostLimits）与「判定器入参类型」（limit.ProviderCostLimits）
// 是两套同形结构；字段映射集中放在这里一份，避免多处各拼一遍。
func providerCostLimits(in route.ProviderCostLimits) limit.ProviderCostLimits {
	return limit.ProviderCostLimits{
		Limit5hUSD:       in.Limit5hUSD,
		Limit5hResetMode: in.Limit5hResetMode,
		LimitDailyUSD:    in.LimitDailyUSD,
		DailyResetMode:   in.DailyResetMode,
		DailyResetTime:   in.DailyResetTime,
		LimitWeeklyUSD:   in.LimitWeeklyUSD,
		LimitMonthlyUSD:  in.LimitMonthlyUSD,
		LimitTotalUSD:    in.LimitTotalUSD,
		CostResetAt:      in.CostResetAt,
	}
}

// buildGates 决定选路门槛（`route.Gates`）的装配。
//
// Gates.Limits（供应商级金额限额）：Node 在 Step 4 的 `filterByLimits` 里逐候选判定。判据窗口、
// 重置模式与理由文案全部复用 `internal/limit`（它已有与 Node 逐字对齐的 costLimit/totalCost
// 读路径），本函数只做「把限额列的选路视图换成现成入参对象」。
//
// 返回的第二个值表示**限额门槛未接上**（用于进 Assembly.Missing，让启动日志可见）：
// 调用方既没自行注入 Gates.Limits，也没提供 `*limit.Service`。这种情况不静默——否则
// 「走 Go 的请求不判供应商额度」会变成只在行为上看得出的差异。
func buildGates(options StoreOptions) (route.Gates, bool) {
	gates := options.RouteOptions.Gates
	if gates.Limits != nil {
		// 调用方（测试或特殊部署）显式注入的优先。
		return gates, false
	}
	service, ok := options.RateLimit.(*limit.Service)
	if !ok {
		return gates, true
	}
	gates.Limits = func(ctx context.Context, p route.Provider) (bool, string) {
		return service.CheckProviderCostLimits(ctx, p.ID, providerCostLimits(p.CostLimits))
	}
	return gates, false
}

// idleTimeoutCache 缓存 providers.streaming_idle_timeout_ms。
//
// 为什么必须缓存：Stream 每发起一次上游读都会问一次静默超时，逐次查库会把热路径变成
// 「每 chunk 一次 SQL」。TTL 沿用 providers 域的失效周期，容量等于启用态供应商数。
type idleTimeoutCache struct {
	pools  *store.Pools
	ttl    *cfgsync.TTLMap[int64, int]
	logger *logx.Logger
	now    func() time.Time
}

// newIdleTimeoutCache 建静默超时缓存。
func newIdleTimeoutCache(pools *store.Pools, logger *logx.Logger, now func() time.Time) *idleTimeoutCache {
	return &idleTimeoutCache{
		pools:  pools,
		ttl:    cfgsync.NewTTLMap[int64, int](cfgsync.Spec(cfgsync.DomainProviders).TTL, idleTimeoutCacheSize),
		logger: logger,
		now:    now,
	}
}

// lookup 返回某供应商的流式静默超时；0 表示不限制（与 Node 的默认一致）。
func (c *idleTimeoutCache) lookup(providerID int64) time.Duration {
	if c == nil || providerID == 0 {
		return 0
	}
	if value, ok := c.ttl.Get(providerID); ok {
		return time.Duration(value) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	row, err := c.pools.FindProviderByID(ctx, providerID)
	if err != nil || row == nil {
		if err != nil {
			c.logger.Debug("dataplane.idle_timeout_lookup_failed", map[string]any{
				"providerId": providerID,
				"error":      err.Error(),
			})
		}
		return 0
	}
	c.ttl.Set(providerID, row.StreamingIdleTimeoutMS)
	return time.Duration(row.StreamingIdleTimeoutMS) * time.Millisecond
}

// 编译期断言：未使用的辅助函数与错误值在此显式保留，避免误删。
var (
	_ = errors.Is
	_ = numericToFloat
)

// publicStatusRollupRecorder 建 public-status 投影的事件捕获器（终态结算的旁路）。
//
// 需要三样：Redis 写面（桶的哈希累加 + 覆盖起点 NX）、内部配置快照读面（分组与模型白名单）、
// 以及日志。缺 Redis 时返回 nil——此时 `terminal` 侧整段跳过，不产生任何额外查询。
func publicStatusRollupRecorder(options StoreOptions, logger *logx.Logger) terminal.RollupRecorder {
	if options.Redis == nil {
		logger.Info("dataplane.rollup_recorder_skipped", map[string]any{
			"reason": "redis_unconfigured",
			"effect": "public_status_rollups_not_captured",
		})
		return nil
	}
	store := pubstatus.NewRedisStatusStore(options.Redis, logger)
	if store == nil {
		return nil
	}
	return pubstatus.NewRollupRecorder(
		pubstatus.NewRedisRollupWriter(options.Redis),
		pubstatus.NewSnapshotGroupSource(store, "", options.Now, logger),
		"",
		logger,
		nil,
	)
}
