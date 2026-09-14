package jobs

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 语义出处（Node）：src/lib/provider-endpoints/probe-scheduler.ts、probe.ts、probe-log-cleanup.ts。
const (
	// EndpointProbeLockKey 与 Node 的 LOCK_KEY 逐字一致：切换期两侧争同一把锁，
	// 避免 Node 与 Go 同时拨测同一批端点（会双写探活历史并重复熔断记账）。
	EndpointProbeLockKey = "locks:endpoint-probe-scheduler"
	// ProbeLogCleanupLockKey 同理（Node probe-log-cleanup.ts:12）。
	ProbeLogCleanupLockKey = "locks:endpoint-probe-log-cleanup"

	// probeSingleVendorInterval 是「该厂只有一个启用端点」时的拨测间隔（Node 硬编码 10 分钟）。
	probeSingleVendorInterval = 10 * time.Minute
	// probeIdlePollCeiling 是空闲期最长不查库的时长（Node：min(base, 30s)）。
	probeIdlePollCeiling = 30 * time.Second
)

// ProbeConfig 是端点探活的配置（默认值取自 Node 的 parseXxxWithDefault）。
type ProbeConfig struct {
	Enabled              bool
	BaseInterval         time.Duration
	TimeoutRetryInterval time.Duration
	Timeout              time.Duration
	Concurrency          int
	CycleJitter          time.Duration
	LockTTL              time.Duration
	// Method 取 tcp / head / get；Node 默认 tcp（probe.ts:44 resolveProbeMethod）。
	Method string
}

// DefaultProbeConfig 返回 Node 的出厂默认值。
func DefaultProbeConfig() ProbeConfig {
	return ProbeConfig{
		Enabled:              true,
		BaseInterval:         60 * time.Second,
		TimeoutRetryInterval: 10 * time.Second,
		Timeout:              5 * time.Second,
		Concurrency:          10,
		CycleJitter:          time.Second,
		LockTTL:              30 * time.Second,
		Method:               "tcp",
	}
}

// TickInterval 是调度器的最小时间粒度（Node：min(base, timeoutRetry)）。
//
// 取更短的那个是为了让「刚超时过的端点」能在 10 秒档被重试，而不必等满一分钟。
func (c ProbeConfig) TickInterval() time.Duration {
	return opsMinDuration(c.BaseInterval, c.TimeoutRetryInterval)
}

// IdlePollInterval 是没有任何端点到期时的最长查库间隔。
func (c ProbeConfig) IdlePollInterval() time.Duration {
	return opsMinDuration(c.BaseInterval, probeIdlePollCeiling)
}

// ProbeEnvWarnings 记录被忽略的非法环境变量（Node 会各自 warn 一行）。
type ProbeEnvWarnings []string

// ProbeConfigFromEnv 按 Node 的解析规则读配置。
//
// 非法值一律回退默认值并记一条警告（与 Node parsePositiveIntWithDefault 同语义），
// 不因单个变量写错就让整个后台任务起不来。
func ProbeConfigFromEnv(lookup OpsEnv) (ProbeConfig, ProbeEnvWarnings) {
	cfg := DefaultProbeConfig()
	var warnings ProbeEnvWarnings

	if value, ok := opsLookup(lookup, "ENDPOINT_PROBE_SCHEDULER_ENABLED"); ok {
		switch value {
		case "true", "1":
			cfg.Enabled = true
		case "false", "0":
			cfg.Enabled = false
		default:
			warnings = append(warnings, "ENDPOINT_PROBE_SCHEDULER_ENABLED="+value)
		}
	}

	// Node 对这几个变量分两族解析，不能统一处理：
	//   - 带警告的正数族（parsePositiveIntWithDefault）：非法即回退默认值并告警。
	//   - 静默钳制族（Math.max(下限, parseIntWithDefault(...))）：非数字回退默认值但不告警，
	//     数字则钳到下限（0 会变成下限，而不是默认值）。
	// 把两族混同会让「写 0」的行为与 Node 分叉（配错一个数就成了踩坑现场）。
	cfg.BaseInterval = opsClampedDuration(lookup, "ENDPOINT_PROBE_INTERVAL_MS",
		cfg.BaseInterval, time.Millisecond)
	cfg.TimeoutRetryInterval = opsPositiveDurationMs(lookup, "ENDPOINT_PROBE_TIMEOUT_RETRY_INTERVAL_MS",
		cfg.TimeoutRetryInterval, &warnings)
	cfg.Timeout = opsClampedDuration(lookup, "ENDPOINT_PROBE_TIMEOUT_MS", cfg.Timeout, time.Millisecond)
	cfg.CycleJitter = opsClampedDuration(lookup, "ENDPOINT_PROBE_CYCLE_JITTER_MS",
		cfg.CycleJitter, 0)
	cfg.LockTTL = opsClampedDuration(lookup, "ENDPOINT_PROBE_LOCK_TTL_MS",
		cfg.LockTTL, time.Millisecond)
	cfg.Concurrency = opsClampedInt(lookup, "ENDPOINT_PROBE_CONCURRENCY", cfg.Concurrency, 1)

	if value, ok := opsLookup(lookup, "ENDPOINT_PROBE_METHOD"); ok {
		switch strings.ToUpper(value) {
		case "HEAD":
			cfg.Method = "head"
		case "GET":
			cfg.Method = "get"
		case "TCP", "":
			cfg.Method = "tcp"
		default:
			// Node 对非法方法**不警告**，静默回落 TCP（probe.ts:44）。
			cfg.Method = "tcp"
		}
	}
	return cfg, warnings
}

// EndpointProbe 是端点拨测任务。
//
// 有状态的部分只有两个「下一轮何时可能有工作」的提示（Node 的 NEXT_DUE / NEXT_DB_POLL）：
// 端点都还没到期时直接跳过查库，避免 10 秒一跳把库拖成热点。
type EndpointProbe struct {
	deps OpsDeps
	cfg  ProbeConfig

	// mu 保护下面两个提示：续约 goroutine 会清空它们（失去领导权时 Node 同样 clearNextWorkHints）。
	mu         sync.Mutex
	nextDueAt  time.Time
	nextPollAt time.Time
	random     *rand.Rand
}

// NewEndpointProbe 构造探活任务。
func NewEndpointProbe(deps OpsDeps, cfg ProbeConfig) *EndpointProbe {
	return &EndpointProbe{
		deps:   deps,
		cfg:    cfg,
		random: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Run 执行一轮拨测（Node runProbeCycle）。
func (p *EndpointProbe) Run(ctx context.Context) (OpsOutcome, error) {
	if p.deps.Pools == nil {
		return OpsOutcome{}, errors.New("jobs: 端点探活需要数据库连接池")
	}

	lock, acquired, err := p.deps.AcquireOpsLeaderLock(ctx, EndpointProbeLockKey, p.cfg.LockTTL)
	if err != nil {
		return OpsOutcome{}, err
	}
	if !acquired {
		return OpsOutcome{Fields: map[string]any{"skipped": "not_leader"}}, nil
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()

	// 续约与工作同生命周期：失去领导权立刻停手，避免两个进程同时拨测。
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	stopKeepAlive := p.startKeepAlive(workCtx, lock, cancelWork)
	defer stopKeepAlive()

	now := p.deps.now()
	if !p.workDue(now) {
		return OpsOutcome{Fields: map[string]any{"skipped": "idle"}}, nil
	}

	if jitter := p.cfg.CycleJitter; jitter > 0 {
		// 多实例同时启动时错峰，避免整齐地一起查库（Node 同款 CYCLE_JITTER）。
		delay := time.Duration(p.random.Int63n(int64(jitter)))
		if err := opsSleep(workCtx, delay); err != nil {
			return OpsOutcome{}, nil
		}
	}

	endpoints, err := p.deps.probeTargets(workCtx)
	if err != nil {
		return OpsOutcome{}, err
	}
	if len(endpoints) == 0 {
		p.setHints(time.Time{}, p.deps.now().Add(p.cfg.IdlePollInterval()))
		return OpsOutcome{Fields: map[string]any{"endpoints": 0}}, nil
	}

	counts := probeEndpointCounts(endpoints)
	cycleNow := p.deps.now()
	due := FilterDueProbeEndpoints(endpoints, counts, p.cfg, cycleNow)
	if len(due) == 0 {
		p.setHints(NextProbeDueAt(endpoints, counts, p.cfg, cycleNow), cycleNow.Add(p.cfg.IdlePollInterval()))
		return OpsOutcome{Fields: map[string]any{
			"endpoints": len(endpoints),
			"due":       0,
		}}, nil
	}

	// 洗牌后按下标分发：并发度由配置决定，单个端点的结果不依赖顺序。
	p.random.Shuffle(len(due), func(i, j int) { due[i], due[j] = due[j], due[i] })

	concurrency := min(p.cfg.Concurrency, len(due))
	if concurrency < 1 {
		concurrency = 1
	}

	var (
		next    int
		mu      sync.Mutex
		probed  int
		okCount int
	)
	worker := func() {
		for {
			mu.Lock()
			if next >= len(due) || workCtx.Err() != nil {
				mu.Unlock()
				return
			}
			endpoint := due[next]
			next++
			mu.Unlock()

			result := p.probeOne(workCtx, endpoint)
			p.record(workCtx, endpoint, result)

			mu.Lock()
			probed++
			if result.OK {
				okCount++
			}
			mu.Unlock()
		}
	}

	var group sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			worker()
		}()
	}
	group.Wait()

	if workCtx.Err() != nil {
		// 失去领导权或被取消：不更新提示，下一轮重新全量评估。
		p.setHints(time.Time{}, time.Time{})
		return OpsOutcome{Fields: map[string]any{"probed": probed, "leadershipLost": true}}, nil
	}

	finished := p.deps.now()
	p.setHints(NextProbeDueAt(endpoints, counts, p.cfg, finished), finished.Add(p.cfg.IdlePollInterval()))
	return OpsOutcome{
		Processed: probed,
		Fields: map[string]any{
			"endpoints": len(endpoints),
			"due":       len(due),
			"probed":    probed,
			"ok":        okCount,
			"failed":    probed - okCount,
		},
	}, nil
}

// startKeepAlive 起续约 goroutine；返回停止函数。
func (p *EndpointProbe) startKeepAlive(
	ctx context.Context,
	lock *OpsLeaderLock,
	onLost context.CancelFunc,
) func() {
	interval := opsMaxDuration(time.Second, p.cfg.LockTTL/2)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(ctx, interval)
				renewed, err := lock.Renew(renewCtx, p.cfg.LockTTL)
				cancel()
				if err == nil && renewed {
					continue
				}
				p.deps.logger().Warn("endpoint_probe_lock_lost", map[string]any{
					"lockKey": lock.Key(),
					"error":   opsErrorText(err),
				})
				p.setHints(time.Time{}, time.Time{})
				onLost()
				return
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// workDue 判断是否到了「可能真的有工作」的时刻。
func (p *EndpointProbe) workDue(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	var hint time.Time
	switch {
	case p.nextDueAt.IsZero() && p.nextPollAt.IsZero():
		return true
	case p.nextDueAt.IsZero():
		hint = p.nextPollAt
	case p.nextPollAt.IsZero():
		hint = p.nextDueAt
	default:
		hint = p.nextDueAt
		if p.nextPollAt.Before(hint) {
			hint = p.nextPollAt
		}
	}
	return !now.Before(hint)
}

// setHints 记录下一轮的两个时间提示（零值表示「立即重评」）。
func (p *EndpointProbe) setHints(nextDueAt, nextPollAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextDueAt = nextDueAt
	p.nextPollAt = nextPollAt
}

// probeOne 拨测单个端点。
func (p *EndpointProbe) probeOne(ctx context.Context, endpoint store.ProbeEndpoint) probeOutcome {
	probeCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	switch p.cfg.Method {
	case "head", "get":
		return probeEndpointHTTP(probeCtx, p.deps.client(), p.cfg.Method, endpoint.URL)
	default:
		return probeEndpointTCP(ctx, endpoint.URL, p.cfg.Timeout)
	}
}

// record 落库探活结果并做端点熔断记账（Node probeProviderEndpointAndRecordByEndpoint 的尾段）。
//
// 失败与成功的记账**不对称**：失败走 RecordEndpointFailure（累计到阈值才开闸），
// 成功走 ResetEndpointCircuit（强制归闭 + 删键）——见 health.ResetEndpointCircuit 的注释。
func (p *EndpointProbe) record(ctx context.Context, endpoint store.ProbeEndpoint, result probeOutcome) {
	probedAt := p.deps.now()

	if p.deps.Health != nil {
		cause := result.circuitCause()
		if result.OK {
			previous, err := p.deps.Health.ResetEndpointCircuit(ctx, endpoint.ID)
			if err != nil {
				p.deps.logger().Warn("endpoint_probe_circuit_reset_failed", map[string]any{
					"endpointId": endpoint.ID,
					"error":      err.Error(),
				})
			} else if previous != "" && previous != "closed" {
				p.deps.logger().Info("endpoint_probe_circuit_reset", map[string]any{
					"endpointId":    endpoint.ID,
					"previousState": string(previous),
				})
			}
		} else if err := p.deps.Health.RecordEndpointFailure(ctx, endpoint.ID, errors.New(cause)); err != nil {
			p.deps.logger().Warn("endpoint_probe_circuit_record_failed", map[string]any{
				"endpointId": endpoint.ID,
				"error":      err.Error(),
			})
		}
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.Timeout)
	defer cancel()
	if err := p.deps.Pools.RecordProbeResult(writeCtx, store.ProbeResultInput{
		EndpointID:   endpoint.ID,
		Source:       "scheduled",
		OK:           result.OK,
		StatusCode:   result.StatusCode,
		LatencyMS:    result.LatencyMS,
		ErrorType:    result.ErrorType,
		ErrorMessage: result.ErrorMessage,
		ProbedAt:     probedAt,
	}); err != nil {
		p.deps.logger().Warn("endpoint_probe_record_failed", map[string]any{
			"endpointId": endpoint.ID,
			"error":      err.Error(),
		})
	}
}

// probeOutcome 是一次拨测的结果（Node EndpointProbeResult）。
type probeOutcome struct {
	OK           bool
	Method       string
	StatusCode   *int
	LatencyMS    *int
	ErrorType    *string
	ErrorMessage *string
}

// circuitCause 给熔断记账用的错误描述（Node：HTTP 码优先，其次 errorType，兜底 probe_failed）。
//
// 刻意不带上游原始错误串：熔断日志会进告警，原始串可能含内网地址或凭据片段。
func (r probeOutcome) circuitCause() string {
	if r.StatusCode != nil {
		return fmt.Sprintf("HTTP %d", *r.StatusCode)
	}
	if r.ErrorType != nil {
		return *r.ErrorType
	}
	return "probe_failed"
}

// probeEndpointHTTP 走 HTTP 拨测：HEAD 优先，传输层失败再试 GET（Node probe.ts:168）。
func probeEndpointHTTP(ctx context.Context, client *http.Client, method, rawURL string) probeOutcome {
	verb := "HEAD"
	if method == "get" {
		verb = "GET"
	}
	result := probeHTTPOnce(ctx, client, verb, rawURL)
	if result.ErrorType != nil && *result.ErrorType != "http_5xx" {
		// 只有「拿不到状态码」才回落 GET；5xx 是明确答复，不该再打一次。
		return probeHTTPOnce(ctx, client, "GET", rawURL)
	}
	return result
}

func probeHTTPOnce(ctx context.Context, client *http.Client, verb, rawURL string) probeOutcome {
	request, err := http.NewRequestWithContext(ctx, verb, rawURL, nil)
	if err != nil {
		return probeFailure("invalid_url", "invalid_url", nil)
	}
	request.Header.Set("cache-control", "no-store")

	startedAt := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return probeFailure(probeErrorType(err), probeErrorText(err), nil)
	}
	defer func() { _ = response.Body.Close() }()

	latency := int(time.Since(startedAt).Milliseconds())
	status := response.StatusCode
	// Node：statusCode < 500 视为活着（4xx 说明服务在，鉴权/路径问题不该判死端点）。
	if status < 500 {
		return probeOutcome{
			OK:         true,
			Method:     verb,
			StatusCode: &status,
			LatencyMS:  &latency,
		}
	}
	errorType, errorMessage := "http_5xx", fmt.Sprintf("HTTP %d", status)
	return probeOutcome{
		Method:       verb,
		StatusCode:   &status,
		ErrorType:    &errorType,
		ErrorMessage: &errorMessage,
	}
}

// probeEndpointTCP 只做一次 TCP 握手（Node 默认方法，probe.ts:110）。
//
// 默认走 TCP 的理由：拨测不该消耗上游额度，也不该被 4xx/5xx 语义污染——
// 端口能连上就说明这一层是活的。
func probeEndpointTCP(ctx context.Context, rawURL string, timeout time.Duration) probeOutcome {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return probeFailure("invalid_url", "invalid_url", nil)
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	dialer := &net.Dialer{Timeout: timeout}
	startedAt := time.Now()
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(parsed.Hostname(), port))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return probeFailure("timeout", "timeout", nil)
		}
		return probeFailure("network_error", err.Error(), nil)
	}
	latency := int(time.Since(startedAt).Milliseconds())
	_ = connection.Close()
	return probeOutcome{OK: true, Method: "TCP", LatencyMS: &latency}
}

func probeFailure(errorType, errorMessage string, status *int) probeOutcome {
	return probeOutcome{
		StatusCode:   status,
		ErrorType:    &errorType,
		ErrorMessage: &errorMessage,
	}
}

// probeErrorType 把传输层错误归类（Node toErrorInfo）。
func probeErrorType(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		if errors.Is(urlErr.Err, context.DeadlineExceeded) || isTimeout(urlErr.Err) {
			return "timeout"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return "timeout"
	}
	return "network_error"
}

func probeErrorText(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return err.Error()
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// probeEndpointCounts 按 (vendor, type) 统计启用端点数（Node countEndpointsByVendorType）。
func probeEndpointCounts(endpoints []store.ProbeEndpoint) map[string]int {
	counts := make(map[string]int, len(endpoints))
	for _, endpoint := range endpoints {
		counts[probeVendorKey(endpoint)]++
	}
	return counts
}

func probeVendorKey(endpoint store.ProbeEndpoint) string {
	return fmt.Sprintf("%d:%s", endpoint.VendorID, endpoint.ProviderType)
}

// EffectiveProbeInterval 返回单个端点的有效拨测间隔。
//
// 优先级（Node getEffectiveIntervalMs）：超时重试 > 单端点厂 > 基础间隔。
// 中间那档的意义：厂里只有一个端点时，每次拨测都是「全厂唯一证据」，
// 一分钟一次纯属噪声，放缓到十分钟。
func EffectiveProbeInterval(
	cfg ProbeConfig,
	endpoint store.ProbeEndpoint,
	vendorCounts map[string]int,
) time.Duration {
	if endpoint.LastProbeErrorType != nil && *endpoint.LastProbeErrorType == "timeout" &&
		(endpoint.LastProbeOK == nil || !*endpoint.LastProbeOK) {
		return cfg.TimeoutRetryInterval
	}
	if vendorCounts[probeVendorKey(endpoint)] == 1 {
		return probeSingleVendorInterval
	}
	return cfg.BaseInterval
}

// FilterDueProbeEndpoints 挑出到期的端点（Node filterDueEndpoints）。
func FilterDueProbeEndpoints(
	endpoints []store.ProbeEndpoint,
	vendorCounts map[string]int,
	cfg ProbeConfig,
	now time.Time,
) []store.ProbeEndpoint {
	due := make([]store.ProbeEndpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint.LastProbedAt == nil {
			// 从未拨测过：一律视为到期（首次上线要立刻拿到结果）。
			due = append(due, endpoint)
			continue
		}
		if !now.Before(endpoint.LastProbedAt.Add(EffectiveProbeInterval(cfg, endpoint, vendorCounts))) {
			due = append(due, endpoint)
		}
	}
	return due
}

// NextProbeDueAt 返回全部端点中「最早到期」的时刻（Node computeNextDueAtMs）。
func NextProbeDueAt(
	endpoints []store.ProbeEndpoint,
	vendorCounts map[string]int,
	cfg ProbeConfig,
	now time.Time,
) time.Time {
	next := time.Time{}
	for _, endpoint := range endpoints {
		if endpoint.LastProbedAt == nil {
			return now
		}
		dueAt := endpoint.LastProbedAt.Add(EffectiveProbeInterval(cfg, endpoint, vendorCounts))
		if next.IsZero() || dueAt.Before(next) {
			next = dueAt
		}
	}
	return next
}

func opsLookup(lookup OpsEnv, name string) (string, bool) {
	if lookup == nil {
		return "", false
	}
	return lookup(name)
}

// opsPositiveDurationMs 读正数毫秒变量：非数字、非整数、<=0 一律回退默认值并告警
// （Node parsePositiveIntWithDefault）。
func opsPositiveDurationMs(
	lookup OpsEnv,
	name string,
	fallback time.Duration,
	warnings *ProbeEnvWarnings,
) time.Duration {
	value, ok := opsLookup(lookup, name)
	if !ok {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		*warnings = append(*warnings, name+"="+value)
		return fallback
	}
	return time.Duration(parsed) * time.Millisecond
}

// opsClampedDuration 读毫秒变量并钳到下限：非数字回退默认值（不告警），
// 数字则取 max(下限, 值)（Node 的 Math.max(min, parseIntWithDefault(...))）。
func opsClampedDuration(
	lookup OpsEnv,
	name string,
	fallback time.Duration,
	floor time.Duration,
) time.Duration {
	value, ok := opsLookup(lookup, name)
	if !ok {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return opsMaxDuration(floor, time.Duration(parsed)*time.Millisecond)
}

// opsClampedInt 与 opsClampedDuration 同族，用于并发度这类整数。
func opsClampedInt(lookup OpsEnv, name string, fallback, floor int) int {
	value, ok := opsLookup(lookup, name)
	if !ok {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	if parsed < floor {
		return floor
	}
	return parsed
}

func opsMinDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func opsMaxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// opsSleep 可被取消的等待。
func opsSleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func opsErrorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
