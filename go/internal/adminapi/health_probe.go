package adminapi

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// 本文件实现 Node 三条健康端点所依赖的组件探针
// （Node 侧：src/lib/health/checker.ts 的 checkDatabase / checkRedis / checkProxy）。
//
// 超时与失败文案逐字取自 checker.ts:20-24 —— 它们是外部监控按字符串断言的对象，不是内部常量。

const (
	healthDatabaseTimeout = 3 * time.Second
	healthRedisTimeout    = 2 * time.Second
	healthProxyTimeout    = 2 * time.Second

	healthDatabaseFailureMessage = "Database connection failed"
	healthRedisFailureMessage    = "Redis connection failed"
	healthProxyFailureMessage    = "Proxy request failed"

	// healthProxyPingPath 与 Node 的自检目标一致（checker.ts:118 的 v1App.request("/v1/_ping")）。
	healthProxyPingPath = "/v1/_ping"
)

// 组件状态取值（Node 的 ComponentStatus，src/lib/health/types.ts:1）。
const (
	healthComponentUp        = "up"
	healthComponentDown      = "down"
	healthComponentUnchecked = "unchecked"
)

// healthProcessStart 近似 Node 的 process.uptime()：两者都是「进程起来的那一刻」。
//
// 用包初始化时刻而不是 main 里的显式打点：adminapi 在本进程启动路径上被初始化，
// 且这样不必为一个 uptime 数字去改装配缝（boot.go）。
var healthProcessStart = time.Now()

// HealthComponent 对应 Node 的 ComponentHealth（src/lib/health/types.ts:5-9）。
//
// LatencyMs 用指针而非值：Node 只在「探测真的跑了」的分支带 latencyMs（快探测得到的 0 也带），
// 而未配置的分支只带 message。值类型 + omitempty 会把 0 吃掉，形状就与 Node 分叉了。
type HealthComponent struct {
	Status    string `json:"status"`
	LatencyMs *int64 `json:"latencyMs,omitempty"`
	Message   string `json:"message,omitempty"`
}

// HealthProbe 探测三个组件的即时状态。
//
// 抽成接口而不是直接读 Deps：管理面 Deps 里**没有 Redis 入口**（见 deps.go 的说明），
// 而健康端点必须如实报告 Redis。探针从装配处注入，Deps 的形状不为这三条端点改动。
type HealthProbe interface {
	// Database 探测 PostgreSQL；未配置 DSN 时报 down + "Database not configured"。
	Database(ctx context.Context) HealthComponent
	// Redis 探测 Redis；未配置 REDIS_URL 时报 unchecked + "Redis not configured"。
	Redis(ctx context.Context) HealthComponent
	// Proxy 自检数据面：请求本进程监听地址上的 GET /v1/_ping。
	// localAddr 由处理器从请求上下文的 LocalAddrContextKey 取出（真实监听地址，不依赖 PORT 环境变量）。
	Proxy(ctx context.Context, localAddr string) HealthComponent
}

// HealthProbeOptions 是探针装配参数。
type HealthProbeOptions struct {
	// Pools 为 nil 表示未配置 DSN。
	Pools *store.Pools
	// Redis 为 nil 表示未配置 REDIS_URL。
	Redis  redis.UniversalClient
	Logger *logx.Logger
	// Client 是 proxy 自检用的 HTTP 客户端；nil 时用带 healthProxyTimeout 的默认客户端。
	// 注入点只为测试（单测不该真起监听），生产不必设置。
	Client *http.Client
}

// NewHealthProbe 建一个可用的探针实现。
func NewHealthProbe(options HealthProbeOptions) HealthProbe {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: healthProxyTimeout}
	}
	return &healthProbe{
		pools:  options.Pools,
		redis:  options.Redis,
		logger: logger,
		client: client,
	}
}

type healthProbe struct {
	pools  *store.Pools
	redis  redis.UniversalClient
	logger *logx.Logger
	client *http.Client
}

// Database 复刻 checkDatabase（checker.ts:49-70）。
func (p *healthProbe) Database(ctx context.Context) HealthComponent {
	if p.pools == nil {
		// Node：DSN 未配置且 NODE_ENV 非 test 时 status=down、message="Database not configured"
		// （checker.ts:53-58）。Go 侧没有 NODE_ENV=test 这一档，故恒取生产分支。
		return HealthComponent{Status: healthComponentDown, Message: "Database not configured"}
	}
	probeCtx, cancel := context.WithTimeout(ctx, healthDatabaseTimeout)
	defer cancel()

	start := time.Now()
	pool, err := p.pools.Control()
	if err == nil {
		err = pool.Ping(probeCtx)
	}
	latency := time.Since(start).Milliseconds()
	if err != nil {
		p.logFailure("database", err)
		return HealthComponent{
			Status:    healthComponentDown,
			LatencyMs: &latency,
			Message:   healthDatabaseFailureMessage,
		}
	}
	return HealthComponent{Status: healthComponentUp, LatencyMs: &latency}
}

// Redis 复刻 checkRedis（checker.ts:74-107）。
func (p *healthProbe) Redis(ctx context.Context) HealthComponent {
	if p.redis == nil {
		return HealthComponent{Status: healthComponentUnchecked, Message: "Redis not configured"}
	}
	probeCtx, cancel := context.WithTimeout(ctx, healthRedisTimeout)
	defer cancel()

	start := time.Now()
	err := p.redis.Ping(probeCtx).Err()
	latency := time.Since(start).Milliseconds()
	if err != nil {
		p.logFailure("redis", err)
		return HealthComponent{
			Status:    healthComponentDown,
			LatencyMs: &latency,
			Message:   healthRedisFailureMessage,
		}
	}
	return HealthComponent{Status: healthComponentUp, LatencyMs: &latency}
}

// Proxy 复刻 checkProxy（checker.ts:113-138）。
//
// 与 Node 的差别只有一处、且是有意的：Node 直接调进程内 Hono 应用（v1App.request），
// Go 侧管理面拿不到数据面处理器，故改为请求**本进程真实监听地址**上的 /v1/_ping。
// 语义等价（都在问「数据面还在服务吗」），且顺带覆盖了「监听已关」这一 Node 版本看不到的情形。
func (p *healthProbe) Proxy(ctx context.Context, localAddr string) HealthComponent {
	target, ok := healthLoopbackTarget(localAddr)
	if !ok {
		p.logger.Warn("health_check_failed", map[string]any{
			"component": "proxy",
			"error":     "local listen address unavailable",
			"localAddr": localAddr,
		})
		return HealthComponent{Status: healthComponentDown, Message: healthProxyFailureMessage}
	}
	probeCtx, cancel := context.WithTimeout(ctx, healthProxyTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target, nil)
	if err != nil {
		p.logFailure("proxy", err)
		return HealthComponent{Status: healthComponentDown, Message: healthProxyFailureMessage}
	}
	start := time.Now()
	response, err := p.client.Do(request)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		p.logFailure("proxy", err)
		return HealthComponent{
			Status:    healthComponentDown,
			LatencyMs: &latency,
			Message:   healthProxyFailureMessage,
		}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		// Node 在 !res.ok 时报 "Proxy returned HTTP <status>"（checker.ts:128）。
		return HealthComponent{
			Status:    healthComponentDown,
			LatencyMs: &latency,
			Message:   fmt.Sprintf("Proxy returned HTTP %d", response.StatusCode),
		}
	}
	return HealthComponent{Status: healthComponentUp, LatencyMs: &latency}
}

func (p *healthProbe) logFailure(component string, err error) {
	p.logger.Warn("health_check_failed", map[string]any{
		"component": component,
		"error":     err.Error(),
	})
}

// healthLoopbackTarget 把监听地址换算成可拨号的自检 URL。
//
// 监听地址可能是 ":3000"、"0.0.0.0:3000"、"[::]:3000"（Go 的监听写法）或 "127.0.0.1:3000"；
// 前三种要换成回环地址才拨得通。为空或不可解析时返回 false——处理器据此如实报 down，
// 而不是猜一个端口（猜错会把「本机健康」报成故障）。
func healthLoopbackTarget(localAddr string) (string, bool) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(localAddr))
	if err != nil || port == "" {
		return "", false
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + healthProxyPingPath, true
}
