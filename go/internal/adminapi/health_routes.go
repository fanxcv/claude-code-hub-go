package adminapi

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/appversion"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件复刻 Node 的三条健康端点。三条都是**公开**的（Node 侧不取会话），
// 且都不在 /api/v1 下，故不出管理面信封：
//
//	GET /api/health       -> src/app/api/health/route.ts（handleReadinessRequest("health_check_failed")）
//	GET /api/health/ready -> src/app/api/health/ready/route.ts（handleReadinessRequest("health_readiness_check_failed")）
//	GET /api/health/live  -> src/app/api/health/live/route.ts
//
// 综合判定逐条对齐 src/lib/health/checker.ts:142-199：
//   - 数据库 down            => status=unhealthy，HTTP 503；
//   - Redis 或 Proxy down    => status=degraded，HTTP 200（降级但不摘流量）；
//   - 其余                   => status=healthy，HTTP 200；
//   - 探测自身抛错           => {status:"unhealthy",timestamp,error:"Health check failed"}，HTTP 503。
//
// 与 Node 的**一处已知差异**（有意记录，不要当缺陷修）：
// 排空期的判定。Node 在 checker.ts:147-160 用进程内的 isShuttingDown() 直接返回
// unhealthy + 503；Go 侧的同一信号只存在于前门内部（egress.FrontDoor.Draining()），
// 而管理面装配缝 adminOptions 在 boot.go 里，本包拿不到它。实际影响有界：排空时进程已停止
// 接纳新连接，外部探针打不进来；只有既有的 keep-alive 连接会命中，此时 Go 因 proxy 自检失败
// 落到 degraded/200 而不是 unhealthy/503。要对齐只需把 frontDoor.Draining 传进 adminOptions。

const (
	healthBodyUnhealthy = "unhealthy"
	healthBodyDegraded  = "degraded"
	healthBodyHealthy   = "healthy"

	healthFailureError = "Health check failed"
)

// healthComponents 按 Node 的字段顺序固定下来（map 会丢顺序，而对拍台逐字段比对）。
type healthComponents struct {
	Database HealthComponent `json:"database"`
	Redis    HealthComponent `json:"redis"`
	Proxy    HealthComponent `json:"proxy"`
}

// healthCheckResponse 对应 Node 的 HealthCheckResponse（src/lib/health/types.ts:11-21）。
type healthCheckResponse struct {
	Status     string           `json:"status"`
	Timestamp  string           `json:"timestamp"`
	Version    string           `json:"version"`
	Uptime     int64            `json:"uptime"`
	Components healthComponents `json:"components"`
}

// healthLiveResponse 对应 /api/health/live 的字面响应体。
type healthLiveResponse struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
}

// healthFailureResponse 对应 handleReadinessRequest 的 catch 分支（checker.ts:194-197）。
type healthFailureResponse struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
	Error     string `json:"error"`
}

// RegisterHealthRoutes 注册三条健康端点。
//
// probe 为 nil 时不注册 /api/health 与 /api/health/ready（原样回退 Node，「不答」好过「乱答」）；
// /api/health/live 不依赖任何探针，故照常注册——它是纯粹的存活性回答，没有可降级的内容。
func RegisterHealthRoutes(router *Router, deps Deps, probe HealthProbe) {
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	if probe == nil {
		logger.Warn("admin_health_probe_unwired", map[string]any{
			"module": "health",
			"action": "readiness_routes_not_registered",
		})
	} else {
		api := &healthAPI{probe: probe, logger: logger}
		for _, route := range []struct {
			path        string
			operationID string
		}{
			{"/api/health", "getHealth"},
			{"/api/health/ready", "getHealthReady"},
		} {
			router.Add(Route{
				Method:               http.MethodGet,
				Path:                 route.path,
				Access:               AccessPublic,
				Module:               "health",
				OperationID:          route.operationID,
				NoManagementEnvelope: true,
				Handler:              http.HandlerFunc(api.handleReadiness),
			})
		}
	}

	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/health/live",
		Access:               AccessPublic,
		Module:               "health",
		OperationID:          "getHealthLive",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(handleHealthLive),
	})
}

type healthAPI struct {
	probe  HealthProbe
	logger *logx.Logger
}

// handleHealthLive 复刻 GET /api/health/live：不碰任何依赖，恒 200。
func handleHealthLive(writer http.ResponseWriter, _ *http.Request) {
	adminWriteJSON(writer, http.StatusOK, healthLiveResponse{
		Status:    "alive",
		Timestamp: formatJSDate(time.Now()),
	})
}

// handleReadiness 复刻 handleReadinessRequest（checker.ts:183-199）。
//
// 三个组件**并行**探测：Node 用 Promise.all，串行会把最坏耗时相加（3s+2s+2s），
// 而健康端点的调用方通常按秒级超时判死。
//
// 探测**自身抛错**（不是「探测出组件 down」）走 Node 的 catch 分支：503 + 三键失败形状。
// 这条归一在本函数里有两个不可省的落点：每个探测 goroutine 一个 recover（未捕获的
// goroutine panic 会终止整个进程，而就绪端点被编排器高频轮询 ⇒ crash loop），
// 以及**先于成功体组装**的 failed 判定（理由见 failed 的声明）。
func (api *healthAPI) handleReadiness(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	localAddr := requestLocalAddr(request)

	// committed 表示响应已写出：兜底 recover 只在**尚未写出**时补失败响应，
	// 否则就成了同一请求写出两次响应（协议错误）。已知上界：若 panic 发生在 writeProbeFailure
	// 内部的 WriteHeader 之后，net/http 只会记一条 superfluous WriteHeader，不会崩。
	committed := false
	defer func() {
		if rec := recover(); rec != nil {
			api.logProbePanic("handler", rec)
			if !committed {
				api.writeProbeFailure(writer)
			}
		}
	}()

	var (
		database HealthComponent
		redis    HealthComponent
		proxy    HealthComponent
		// failed 记录「任一探测自身抛错」。
		//
		// 它必须**先于**成功体组装被检查，否则会误报健康：goroutine 在 probe.X(ctx) 处 panic 时
		// 赋值语句从未执行，对应组件留下零值 Status:""；readinessStatus 只在 Status=="down" 时
		// 判 unhealthy，"" 会落到 default ⇒ 汇报成 healthy/200。编排器据此不摘流量，
		// 而进程随即被未捕获的 goroutine panic 终止——是「误报健康 + 进程崩溃」双失。
		failed atomic.Bool
		wait   sync.WaitGroup
	)
	wait.Add(3)
	go func() {
		defer wait.Done()
		defer func() {
			if rec := recover(); rec != nil {
				failed.Store(true)
				api.logProbePanic("database", rec)
			}
		}()
		database = api.probe.Database(ctx)
	}()
	go func() {
		defer wait.Done()
		defer func() {
			if rec := recover(); rec != nil {
				failed.Store(true)
				api.logProbePanic("redis", rec)
			}
		}()
		redis = api.probe.Redis(ctx)
	}()
	go func() {
		defer wait.Done()
		defer func() {
			if rec := recover(); rec != nil {
				failed.Store(true)
				api.logProbePanic("proxy", rec)
			}
		}()
		proxy = api.probe.Proxy(ctx, localAddr)
	}()
	wait.Wait()

	if failed.Load() {
		committed = true
		api.writeProbeFailure(writer)
		return
	}

	body := healthCheckResponse{
		Status:    readinessStatus(database, redis, proxy),
		Timestamp: formatJSDate(time.Now()),
		Version:   healthAppVersion(),
		Uptime:    healthUptimeSeconds(),
		Components: healthComponents{
			Database: database,
			Redis:    redis,
			Proxy:    proxy,
		},
	}
	status := http.StatusOK
	if body.Status == healthBodyUnhealthy {
		status = http.StatusServiceUnavailable
	}
	committed = true
	adminWriteJSON(writer, status, body)
}

// writeProbeFailure 输出 Node catch 分支的形状（checker.ts:194-197）：503 + 三键，
// **不带** components/version/uptime——失败形状与成功形状是两个形状。
func (api *healthAPI) writeProbeFailure(writer http.ResponseWriter) {
	adminWriteJSON(writer, http.StatusServiceUnavailable, healthFailureResponse{
		Status:    healthBodyUnhealthy,
		Timestamp: formatJSDate(time.Now()),
		Error:     healthFailureError,
	})
}

// logProbePanic 记一条探测 panic。日志不含组件读数，只记组件名与 panic 值，便于定位。
//
// logger 在此不必判空：healthAPI 只由 RegisterHealthRoutes 构造，那里的 logger 已保证非 nil
// （deps.Logger 为 nil 时回落 logx.New(nil)）。
func (api *healthAPI) logProbePanic(component string, rec any) {
	api.logger.Warn("admin_health_probe_panicked", map[string]any{
		"component": component,
		"panic":     fmt.Sprint(rec),
	})
}

// readinessStatus 复刻 checker.ts:165-170：数据库必需，Redis/Proxy 只降级。
//
// 注意 unchecked（Redis 未配置）**不**触发 degraded —— Node 只判 "down"。
func readinessStatus(database, redis, proxy HealthComponent) string {
	switch {
	case database.Status == healthComponentDown:
		return healthBodyUnhealthy
	case redis.Status == healthComponentDown, proxy.Status == healthComponentDown:
		return healthBodyDegraded
	default:
		return healthBodyHealthy
	}
}

// requestLocalAddr 取本次请求被接受的监听地址（"127.0.0.1:23000"、":23000" 一类）。
//
// 用它而不是 PORT 环境变量：探针要问的是「本进程此刻在哪个地址上服务」，
// 那正是这条连接的事实；环境变量可能与实际监听不一致（改端口只改了 env 的场景）。
func requestLocalAddr(request *http.Request) string {
	addr, ok := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || addr == nil {
		return ""
	}
	return addr.String()
}

// healthAppVersion 复刻 getAppVersion（checker.ts:12-16）：版本号去掉开头的 v。
//
// 版本解析走 `internal/appversion`（注入的 APP_VERSION 为真源）——与 /api/version、UI 壳注入
// 同一个实现，故三处不可能再报出不同的版本号（旧实现各自读文件/常量，同一镜像里出现过三个值）。
func healthAppVersion() string {
	return appversion.ResolveBare(os.Getenv)
}

// healthUptimeSeconds 复刻 Math.round(process.uptime())：自进程启动起的秒数，四舍五入。
func healthUptimeSeconds() int64 {
	return int64(math.Round(time.Since(healthProcessStart).Seconds()))
}
