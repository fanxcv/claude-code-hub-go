package jobs

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"

	"github.com/fanxcv/claude-code-hub-go/go/internal/health"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件把「拨测一次」从调度路径里单独暴露出来，供**管理面**同步调用
// （Node 侧 src/actions/provider-endpoints.ts:667 probeProviderEndpoint →
// src/lib/provider-endpoints/probe.ts:222 probeProviderEndpointAndRecordByEndpoint）。
//
// 为什么不复用 EndpointProbe：那个类型是「一轮调度」的执行体（leader 锁、到期判定、
// 有状态提示位、source 固定 scheduled），而管理面要的是「一个端点、一次、source=manual、
// 超时由请求指定」。两者共用的只有拨测与记账两段；把调度体的状态机塞进请求路径是本末倒置。
//
// 记账语义与调度体**逐字一致**（失败累计到阈值才开闸、成功强制归闭），差别只有 source 与超时：
//
//   - Node 的手动拨测同样喂熔断器（probe.ts:236 recordEndpointFailure），漏掉这一句会让
//     「手动探一次坏端点」变成看不见的副作用缺失。
//   - 成功归闭是**跨实例**的重置（Go 侧 health.Writer.ResetEndpointCircuit 直写 Redis），
//     不是只改本进程内存态。
//   - 拨测结果落库失败**不吞**：Node 的 recordProviderEndpointProbeResult 在事务里抛错，
//     action 的 catch 把它变成 OPERATION_FAILED；这里同样上抛，由调用方作答 5xx。
//     熔断记账失败则只记 warn（Node 侧那两段各自 try/catch）。
//
// 一处与 Node 的登记差异：Node 的 resolveProbeMethod() 每次调用都重读 ENDPOINT_PROBE_METHOD，
// 本实现的探测方法在构造 runner 时解析一次。理由是管理面装配期读环境与 Node 的读法在部署上
// 等价（改这个变量需要重启进程两处都生效），而每次请求重读会让「同一进程内两种方法并存」。

// ProbeOnceOptions 是同步拨测入口的装配参数。
type ProbeOnceOptions struct {
	// Pools 是连接池；nil 时拨测不落库（构造时即报错，见 NewProbeOnceRunner）。
	Pools *store.Pools
	// Redis 用于分布式互斥与熔断记账；nil 时熔断记账降级为进程内（与 Node 无 Redis 同形）。
	Redis redis.UniversalClient
	// Health 是端点熔断写入器；nil 时跳过熔断记账。
	Health *health.Writer
	// Logger 为 nil 时静默。
	Logger *logx.Logger
	// HTTP 是拨测客户端；nil 时用 OpsDefaultHTTPClient（不跟随重定向）。
	HTTP *http.Client
	// Lookup 读环境变量；nil 时用进程环境。测试用它钉住方法与时长的来源。
	Lookup OpsEnv
}

// ProbeOnceResult 是一次拨测的结果（Node EndpointProbeResult）。
type ProbeOnceResult struct {
	OK           bool
	Method       string
	StatusCode   *int
	LatencyMS    *int
	ErrorType    *string
	ErrorMessage *string
}

// ProbeOnceRunner 同步拨测一个端点（管理面三条拨测路由的执行体）。
type ProbeOnceRunner struct {
	deps OpsDeps
	cfg  ProbeConfig
}

// NewProbeOnceRunner 建同步拨测入口；连接池缺失时返回 nil（调用方据此不注册那三条路由）。
func NewProbeOnceRunner(options ProbeOnceOptions) *ProbeOnceRunner {
	if options.Pools == nil {
		return nil
	}
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	lookup := options.Lookup
	if lookup == nil {
		lookup = config.LookupFromOS()
	}
	cfg, warnings := ProbeConfigFromEnv(lookup)
	for _, warning := range warnings {
		logger.Warn("endpoint_probe_invalid_env", map[string]any{"value": warning})
	}
	return &ProbeOnceRunner{
		deps: OpsDeps{
			Pools:  options.Pools,
			Redis:  options.Redis,
			Health: options.Health,
			Logger: logger,
			HTTP:   options.HTTP,
		},
		cfg: cfg,
	}
}

// ProbeEndpoint 同步拨测一个端点、喂熔断器、并把结果写进端点快照与探活历史。
//
// timeoutMS 为 0 时用环境里的 ENDPOINT_PROBE_TIMEOUT_MS（Node 的 DEFAULT_TIMEOUT_MS）；
// 由请求指定的超时（ProviderEndpointProbeSchema 的 1000..120000）只在拨测阶段生效，
// 落库阶段另给一个同样长度的预算——Node 侧落库没有独立超时，用一个上限防止写卡死请求。
func (r *ProbeOnceRunner) ProbeEndpoint(
	ctx context.Context,
	endpoint store.ProbeEndpoint,
	source string,
	timeoutMS int,
) (ProbeOnceResult, error) {
	timeout := r.cfg.Timeout
	if timeoutMS > 0 {
		timeout = time.Duration(timeoutMS) * time.Millisecond
	}

	outcome := r.probeOnce(ctx, endpoint.URL, timeout)
	r.recordCircuit(ctx, endpoint.ID, outcome)

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := r.deps.Pools.RecordProbeResult(writeCtx, store.ProbeResultInput{
		EndpointID:   endpoint.ID,
		Source:       source,
		OK:           outcome.OK,
		StatusCode:   outcome.StatusCode,
		LatencyMS:    outcome.LatencyMS,
		ErrorType:    outcome.ErrorType,
		ErrorMessage: outcome.ErrorMessage,
		ProbedAt:     r.deps.now(),
	}); err != nil {
		return outcome.result(), err
	}
	return outcome.result(), nil
}

// probeOnce 执行一次拨测（按解析出的方法走 HTTP 或 TCP）。
//
// 失败态没有 `method` 可读（probeFailure 不带它），而 Node 的结果里失败也带方法名；这里按
// Node 的回落规则补出真实方法：HTTP 路径下没拿到状态码就说明已回落到 GET（probeEndpointHTTP），
// 否则是 HEAD 的答复；TCP 路径恒为 TCP。
func (r *ProbeOnceRunner) probeOnce(ctx context.Context, rawURL string, timeout time.Duration) probeOutcome {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var outcome probeOutcome
	switch r.cfg.Method {
	case "head", "get":
		outcome = probeEndpointHTTP(probeCtx, r.deps.client(), r.cfg.Method, rawURL)
		if outcome.Method == "" {
			if outcome.StatusCode == nil {
				outcome.Method = "GET"
			} else {
				outcome.Method = "HEAD"
			}
		}
	default:
		outcome = probeEndpointTCP(ctx, rawURL, timeout)
		if outcome.Method == "" {
			outcome.Method = "TCP"
		}
	}
	return outcome
}

// recordCircuit 做端点熔断记账，失败只记 warn（Node 的两段各自 try/catch）。
func (r *ProbeOnceRunner) recordCircuit(ctx context.Context, endpointID int64, result probeOutcome) {
	if r.deps.Health == nil {
		return
	}
	if result.OK {
		previous, err := r.deps.Health.ResetEndpointCircuit(ctx, endpointID)
		if err != nil {
			r.deps.logger().Warn("endpoint_probe_circuit_reset_failed", map[string]any{
				"endpointId": endpointID,
				"error":      err.Error(),
			})
			return
		}
		if previous != "" && previous != "closed" {
			r.deps.logger().Info("endpoint_probe_circuit_reset", map[string]any{
				"endpointId":    endpointID,
				"previousState": string(previous),
				"source":        "manual",
			})
		}
		return
	}
	if err := r.deps.Health.RecordEndpointFailure(ctx, endpointID, errors.New(result.circuitCause())); err != nil {
		r.deps.logger().Warn("endpoint_probe_circuit_record_failed", map[string]any{
			"endpointId": endpointID,
			"error":      err.Error(),
		})
	}
}

// result 把内部结果转成对外的形状。
func (r probeOutcome) result() ProbeOnceResult {
	return ProbeOnceResult{
		OK:           r.OK,
		Method:       r.Method,
		StatusCode:   r.StatusCode,
		LatencyMS:    r.LatencyMS,
		ErrorType:    r.ErrorType,
		ErrorMessage: r.ErrorMessage,
	}
}
