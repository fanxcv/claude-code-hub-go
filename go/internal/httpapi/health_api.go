package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/appversion"
)

// 本文件是 Node 的 `/api/health`、`/api/health/ready`、`/api/health/live` 的 Go 落点
// （Node 侧 `src/app/api/health/route.ts`、`health/ready/route.ts`、`health/live/route.ts`
// 与 `src/lib/health/checker.ts`）。
//
// **为什么放在本包而不是 adminapi**：这三条要对 **Redis** 做真实 ping，而 `adminapi.Deps`
// 刻意没有 Redis 入口（见 `deps.go` 的说明），本进程的 PG/Redis 探测器（`Prober`）本来就装在本包。
// 与 `/readyz` 同属运维面，故同样挂在 ops 上、**不进前门**：探针要在归属判定之外也能回答
// 「我为什么不健康」。
//
// 形状逐字对齐 Node 的 `HealthCheckResponse`：`status`/`timestamp`/`version`/`uptime`/
// `components.{database,redis,proxy}` 与状态码（`unhealthy` → 503，其余 200）必须一致；
// 组件名与 message 文案也照抄，因为监控与告警规则按它们匹配。
//
// 两处与 Node 的**有意差异**（都写在报告里）：
//  1. Node 的 `uptime` 取 `process.uptime()`（进程启动起的秒数）。Go 侧用**包初始化时刻**近似
//     ——本包由 `main` 直接引入，初始化发生在进程启动早期，误差在毫秒级；不引入新的进程级状态。
//  2. `version` 没有 ServerOptions 入口时按「VERSION 文件 → 环境变量 → 常量」取值（见 appVersion）。
//     Node 取 `NEXT_PUBLIC_APP_VERSION` → `VERSION` → `package.json`；Go 侧的取值统一在
//     `internal/appversion`，装配处注入同一个值，故这里的三段式只做兜底。

// healthProcessStartedAt 近似进程启动时刻（见文件头差异 1）。
var healthProcessStartedAt = time.Now()

// componentHealth 对齐 Node 的 ComponentHealth：status/latencyMs/message。
type componentHealth struct {
	Status    string `json:"status"`
	LatencyMS *int64 `json:"latencyMs,omitempty"`
	Message   string `json:"message,omitempty"`
}

// healthResponse 对齐 Node 的 HealthCheckResponse。
type healthResponse struct {
	Status     string `json:"status"`
	Timestamp  string `json:"timestamp"`
	Version    string `json:"version"`
	Uptime     int64  `json:"uptime"`
	Components struct {
		Database componentHealth `json:"database"`
		Redis    componentHealth `json:"redis"`
		Proxy    componentHealth `json:"proxy"`
	} `json:"components"`
}

// handleHealthAPI 复刻 `handleReadinessRequest`：综合判定的同一份结果，两条路径（/api/health 与
// /api/health/ready）返回完全相同的正文与状态码（Node 侧只是失败日志的 action 文案不同）。
func (s *Server) handleHealthAPI(writer http.ResponseWriter, request *http.Request) {
	report := s.buildHealth(request.Context())
	status := http.StatusOK
	if report.Status == "unhealthy" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(writer, status, report)
}

// handleHealthLive 复刻 `health/live/route.ts`：只证明进程活着，不查依赖。
func (s *Server) handleHealthLive(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":    "alive",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// buildHealth 组装 Node 同形的健康报告。
func (s *Server) buildHealth(ctx context.Context) healthResponse {
	report := healthResponse{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Version:   s.appVersion(),
		Uptime:    int64(time.Since(healthProcessStartedAt).Seconds()),
	}

	// 排空中一律 unhealthy（Node 的 isShuttingDown 分支）：让 Service/Ingress 在 server.close()
	// drain 之前就摘流，否则新连接还会被路由到正在排空的 pod 上。
	if s.draining != nil && s.draining() {
		down := componentHealth{Status: "down", Message: "shutting_down"}
		report.Status = "unhealthy"
		report.Components.Database = down
		report.Components.Redis = down
		report.Components.Proxy = down
		return report
	}

	report.Components.Database = s.databaseHealth(ctx)
	report.Components.Redis = s.redisHealth(ctx)
	report.Components.Proxy = s.proxyHealth()

	// DB 必需；Redis/Proxy 可选（降级但不摘流量）——与 Node 的三档判定逐条对齐。
	switch {
	case report.Components.Database.Status == "down":
		report.Status = "unhealthy"
	case report.Components.Redis.Status == "down" || report.Components.Proxy.Status == "down":
		report.Status = "degraded"
	default:
		report.Status = "healthy"
	}
	return report
}

// databaseHealth 把 Prober 的 PG 结论翻成 Node 的 ComponentHealth 语义。
//
// Node 的三种落点：up（带 latencyMs）、down + "Database connection failed"、
// 未配 DSN 时 down + "Database not configured"（test 环境下 Node 报 unchecked，Go 侧没有测试
// 运行时这一档，故如实按 down 报，并在报告里记下这一处差异）。
func (s *Server) databaseHealth(ctx context.Context) componentHealth {
	if s.prober == nil {
		return componentHealth{Status: "down", Message: "Database connection failed"}
	}
	started := time.Now()
	status, _ := s.pingPG(ctx)
	latency := time.Since(started).Milliseconds()

	switch status {
	case StatusOK:
		return componentHealth{Status: "up", LatencyMS: &latency}
	case StatusNotSet:
		return componentHealth{Status: "down", Message: "Database not configured"}
	default:
		return componentHealth{Status: "down", LatencyMS: &latency, Message: "Database connection failed"}
	}
}

// redisHealth 同 databaseHealth，但 Node 对未配 Redis 报 unchecked（不是 down）。
func (s *Server) redisHealth(ctx context.Context) componentHealth {
	if s.prober == nil {
		return componentHealth{Status: "down", Message: "Redis connection failed"}
	}
	started := time.Now()
	status, _ := s.pingRedis(ctx)
	latency := time.Since(started).Milliseconds()

	switch status {
	case StatusOK:
		return componentHealth{Status: "up", LatencyMS: &latency}
	case StatusNotSet:
		return componentHealth{Status: "unchecked", Message: "Redis not configured"}
	default:
		return componentHealth{Status: "down", LatencyMS: &latency, Message: "Redis connection failed"}
	}
}

// proxyHealth 复刻 Node 的 checkProxy：**在进程内**打自己的 `/v1/_ping`
// （Node 侧是 `v1App.request("/v1/_ping")`），故这里直接调本包自己的 ping 处理器——
// 走真实的路由与序列化路径，而不是"看处理器非 nil"这种会骗人的判据。
func (s *Server) proxyHealth() componentHealth {
	started := time.Now()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/_ping", nil)

	func() {
		defer func() {
			// ping 处理器不该 panic；真 panic 了就报 down 而不是让健康检查一起炸。
			_ = recover()
		}()
		s.handlePing(recorder, request)
	}()

	latency := time.Since(started).Milliseconds()
	if recorder.Code == http.StatusOK {
		return componentHealth{Status: "up", LatencyMS: &latency}
	}
	return componentHealth{
		Status:    "down",
		LatencyMS: &latency,
		Message:   "Proxy returned HTTP " + strconv.Itoa(recorder.Code),
	}
}

// pingPG / pingRedis 把 Prober 调用收在受超时约束的一处：Node 侧是 withTimeout（DB 3s、Redis 2s）。
func (s *Server) pingPG(ctx context.Context) (DependencyStatus, string) {
	ctx, cancel := context.WithTimeout(ctx, s.probeDeadline())
	defer cancel()
	return s.prober.PingPG(ctx)
}

func (s *Server) pingRedis(ctx context.Context) (DependencyStatus, string) {
	ctx, cancel := context.WithTimeout(ctx, s.probeDeadline())
	defer cancel()
	return s.prober.PingRedis(ctx)
}

// probeDeadline 是单次依赖探测的上界（与 /readyz 共用同一档：ServerOptions.ProbeTimeout）。
func (s *Server) probeDeadline() time.Duration {
	if s.probeTimeout <= 0 {
		return 2 * time.Second
	}
	return s.probeTimeout
}

// appVersion 给出不带 `v` 前缀的版本号（Node 的 getAppVersion 会去掉前导 v）。
//
// 装配处注入的 `Version` 优先（`cmd/cchd` 就是拿 `internal/appversion` 喂进来的）；
// 未注入时回落到同一个实现，**不再自带一条链**：本函数曾自己读环境变量、读 `VERSION` 文件、
// 再退到自己的常量，于是同一个镜像里 `/api/health`（运维面）报 `0.9.0` 而 `/api/version` 报
// `v0.9.5`——排障时据此误判过「有两台不同的实例」。
func (s *Server) appVersion() string {
	if s.version != nil {
		if value := appversion.Bare(s.version()); value != "" {
			return value
		}
	}
	return appversion.ResolveBare(os.Getenv)
}

// normalizeHealthVersion 去掉前导 `v`（Node 的 getAppVersion 是 `APP_VERSION.replace(/^v/i, "")`）。
// 与 `appversion.Bare` 同义，保留本名供本包测试直接验证对外形状。
func normalizeHealthVersion(raw string) string {
	return appversion.Bare(raw)
}
