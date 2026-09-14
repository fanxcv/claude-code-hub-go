package jobs

import (
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// opsLookupFrom 用 map 冒充环境变量，避免测试互相污染进程环境。
func opsLookupFrom(values map[string]string) config.LookupEnvFunc {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// 未设置任何变量时必须逐项等于 Node 的出厂默认值（probe-scheduler.ts 的常量）。
func TestProbeConfigDefaults(t *testing.T) {
	cfg, warnings := ProbeConfigFromEnv(opsLookupFrom(nil))
	if len(warnings) != 0 {
		t.Fatalf("未设置变量不应产生警告: %v", warnings)
	}
	if !cfg.Enabled {
		t.Fatal("默认应启用调度器")
	}
	if cfg.BaseInterval != 60*time.Second {
		t.Fatalf("基础间隔应为 60s，得到 %v", cfg.BaseInterval)
	}
	if cfg.TimeoutRetryInterval != 10*time.Second {
		t.Fatalf("超时重试间隔应为 10s，得到 %v", cfg.TimeoutRetryInterval)
	}
	if cfg.Timeout != 5*time.Second {
		t.Fatalf("拨测超时应为 5s，得到 %v", cfg.Timeout)
	}
	if cfg.Concurrency != 10 {
		t.Fatalf("并发度应为 10，得到 %d", cfg.Concurrency)
	}
	// Node 的 parsePositiveIntWithDefault 把 0 判为非法；抖动的是 parseIntWithDefault（允许 0）。
	if cfg.CycleJitter != time.Second {
		t.Fatalf("抖动应为 1s，得到 %v", cfg.CycleJitter)
	}
	if cfg.LockTTL != 30*time.Second {
		t.Fatalf("锁 TTL 应为 30s，得到 %v", cfg.LockTTL)
	}
	if cfg.Method != "tcp" {
		t.Fatalf("默认拨测方法应为 tcp，得到 %q", cfg.Method)
	}
	// tick 取两者更短的那个，否则 10 秒档的超时重试永远不会被触发。
	if cfg.TickInterval() != 10*time.Second {
		t.Fatalf("tick 应为 10s，得到 %v", cfg.TickInterval())
	}
	if cfg.IdlePollInterval() != 30*time.Second {
		t.Fatalf("空闲查询间隔应为 30s，得到 %v", cfg.IdlePollInterval())
	}
}

// Node 对这几个变量分两族解析，本用例钉住分族边界：
//   - 静默钳制族（INTERVAL / TIMEOUT / CONCURRENCY / JITTER / LOCK_TTL）：非数字回退默认值、不告警；
//     数字则钳到下限（0 变下限，而不是默认值）。
//   - 带警告族（TIMEOUT_RETRY_INTERVAL / ENABLED）：非法即回退默认值并告警。
func TestProbeConfigInvalidValuesFallBack(t *testing.T) {
	cfg, warnings := ProbeConfigFromEnv(opsLookupFrom(map[string]string{
		"ENDPOINT_PROBE_INTERVAL_MS":       "abc",
		"ENDPOINT_PROBE_TIMEOUT_MS":        "0",
		"ENDPOINT_PROBE_CONCURRENCY":       "-3",
		"ENDPOINT_PROBE_CYCLE_JITTER_MS":   "0",
		"ENDPOINT_PROBE_SCHEDULER_ENABLED": "yes",
	}))
	if cfg.BaseInterval != 60*time.Second {
		t.Fatalf("非数字间隔应回退 60s（不告警），得到 %v", cfg.BaseInterval)
	}
	// 钳制族：0 被抬到 1ms，而不是回退 5s。
	if cfg.Timeout != time.Millisecond {
		t.Fatalf("0 应被钳到 1ms，得到 %v", cfg.Timeout)
	}
	if cfg.Concurrency != 1 {
		t.Fatalf("负数并发应钳到 1，得到 %d", cfg.Concurrency)
	}
	// 抖动这档允许 0，必须被接受而不是回退。
	if cfg.CycleJitter != 0 {
		t.Fatalf("抖动 0 应被接受，得到 %v", cfg.CycleJitter)
	}
	if !cfg.Enabled {
		t.Fatal("非法布尔应回退默认值 true")
	}
	if len(warnings) != 1 || warnings[0] != "ENDPOINT_PROBE_SCHEDULER_ENABLED=yes" {
		t.Fatalf("应只记启用开关那一条警告，得到 %v", warnings)
	}
}

// 带警告族：<=0 或非数字都回退默认值并告警。
func TestProbeConfigTimeoutRetryWarningFamily(t *testing.T) {
	for _, value := range []string{"0", "-5", "abc"} {
		cfg, warnings := ProbeConfigFromEnv(opsLookupFrom(map[string]string{
			"ENDPOINT_PROBE_TIMEOUT_RETRY_INTERVAL_MS": value,
		}))
		if cfg.TimeoutRetryInterval != 10*time.Second {
			t.Fatalf("%q 应回退 10s，得到 %v", value, cfg.TimeoutRetryInterval)
		}
		if len(warnings) != 1 {
			t.Fatalf("%q 应记一条警告，得到 %v", value, warnings)
		}
	}
	// 合法值生效且不告警。
	cfg, warnings := ProbeConfigFromEnv(opsLookupFrom(map[string]string{
		"ENDPOINT_PROBE_TIMEOUT_RETRY_INTERVAL_MS": "2500",
	}))
	if cfg.TimeoutRetryInterval != 2500*time.Millisecond || len(warnings) != 0 {
		t.Fatalf("合法值未生效: %v %v", cfg.TimeoutRetryInterval, warnings)
	}
}

// 拨测方法：非法值静默回落 TCP（Node 对此不告警）。
func TestProbeConfigMethod(t *testing.T) {
	cases := map[string]string{
		"HEAD": "head",
		"get":  "get",
		"TCP":  "tcp",
		"":     "tcp",
		"PING": "tcp",
	}
	for input, want := range cases {
		cfg, warnings := ProbeConfigFromEnv(opsLookupFrom(map[string]string{"ENDPOINT_PROBE_METHOD": input}))
		if cfg.Method != want {
			t.Fatalf("ENDPOINT_PROBE_METHOD=%q 应为 %q，得到 %q", input, want, cfg.Method)
		}
		if len(warnings) != 0 {
			t.Fatalf("方法非法不应告警，得到 %v", warnings)
		}
	}
}

func opsProbeEndpointFixture(id, vendorID int64, lastProbed *time.Time, errorType *string, ok *bool) store.ProbeEndpoint {
	return store.ProbeEndpoint{
		ID:                 id,
		URL:                "http://127.0.0.1:1/health",
		VendorID:           vendorID,
		ProviderType:       "claude",
		LastProbedAt:       lastProbed,
		LastProbeOK:        ok,
		LastProbeErrorType: errorType,
	}
}

// 有效间隔的三档优先级：超时重试 > 单端点厂 > 基础间隔。
func TestEffectiveProbeIntervalPriority(t *testing.T) {
	cfg := DefaultProbeConfig()
	now := time.Now()
	timeout := "timeout"
	failed := false
	passed := true

	// 一厂两端点：走基础间隔。
	shared := opsProbeEndpointFixture(1, 7, &now, nil, &passed)
	if got := EffectiveProbeInterval(cfg, shared, map[string]int{"7:claude": 2}); got != cfg.BaseInterval {
		t.Fatalf("多端点厂应为基础间隔，得到 %v", got)
	}

	// 一厂一端点：放缓到 10 分钟。
	single := opsProbeEndpointFixture(2, 8, &now, nil, &passed)
	if got := EffectiveProbeInterval(cfg, single, map[string]int{"8:claude": 1}); got != probeSingleVendorInterval {
		t.Fatalf("单端点厂应为 10 分钟，得到 %v", got)
	}

	// 上次超时且未成功：压到 10 秒档（优先级最高，即使该厂只有一个端点）。
	afterTimeout := opsProbeEndpointFixture(3, 9, &now, &timeout, &failed)
	if got := EffectiveProbeInterval(cfg, afterTimeout, map[string]int{"9:claude": 1}); got != cfg.TimeoutRetryInterval {
		t.Fatalf("超时后应为 10 秒档，得到 %v", got)
	}

	// 超时但最近一次已成功：不再走重试档。
	recovered := opsProbeEndpointFixture(4, 10, &now, &timeout, &passed)
	if got := EffectiveProbeInterval(cfg, recovered, map[string]int{"10:claude": 1}); got != probeSingleVendorInterval {
		t.Fatalf("超时但已恢复应回落单端点档，得到 %v", got)
	}
}

// 到期判定：从未拨测必到期；已拨测要等满有效间隔。
func TestFilterDueProbeEndpoints(t *testing.T) {
	cfg := DefaultProbeConfig()
	now := time.Now()
	counts := map[string]int{"1:claude": 2}

	justProbed := now.Add(-10 * time.Second)
	overdue := now.Add(-90 * time.Second)
	due := FilterDueProbeEndpoints([]store.ProbeEndpoint{
		opsProbeEndpointFixture(1, 1, nil, nil, nil),
		opsProbeEndpointFixture(2, 1, &justProbed, nil, nil),
		opsProbeEndpointFixture(3, 1, &overdue, nil, nil),
	}, counts, cfg, now)

	ids := make([]int64, 0, len(due))
	for _, endpoint := range due {
		ids = append(ids, endpoint.ID)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
		t.Fatalf("应挑出「从未拨测」与「已超期」两个端点，得到 %v", ids)
	}

	// 一个端点到期的时刻：刚拨测过的按基础间隔算。
	next := NextProbeDueAt([]store.ProbeEndpoint{
		opsProbeEndpointFixture(4, 1, &justProbed, nil, nil),
	}, counts, cfg, now)
	if want := justProbed.Add(cfg.BaseInterval); !next.Equal(want) {
		t.Fatalf("下次到期应为 %v，得到 %v", want, next)
	}
	// 从未拨测过：立刻到期（不能把新端点排到未来）。
	if got := NextProbeDueAt([]store.ProbeEndpoint{opsProbeEndpointFixture(5, 1, nil, nil, nil)}, counts, cfg, now); !got.Equal(now) {
		t.Fatalf("从未拨测应立刻到期，得到 %v", got)
	}
}

// 空输入是正常态（新部署还没有端点），不能除零也不能返回错误。
func TestFilterDueProbeEndpointsEmpty(t *testing.T) {
	if due := FilterDueProbeEndpoints(nil, map[string]int{}, DefaultProbeConfig(), time.Now()); len(due) != 0 {
		t.Fatalf("空输入应返回空，得到 %d 条", len(due))
	}
	if got := NextProbeDueAt(nil, map[string]int{}, DefaultProbeConfig(), time.Now()); !got.IsZero() {
		t.Fatalf("空输入的下次到期应为零值，得到 %v", got)
	}
}

// outbox 条目的校验判据（Node parseOutboxEntry 的五道关）。
func TestParseOutboxEntry(t *testing.T) {
	valid := `{"version":1,"requestId":42,"traceUpdatedAt":1700000000000,` +
		`"routingTrace":{"version":1,"updatedAt":1700000000000}}`

	entry, ok := ParseOutboxEntry(valid)
	if !ok || entry == nil {
		t.Fatal("合法条目应被接受")
	}
	if entry.RequestID != 42 || entry.TraceUpdatedAt != 1700000000000 {
		t.Fatalf("解析结果不符: %+v", entry)
	}

	invalid := map[string]string{
		"非 JSON":        `not json`,
		"版本不为 1":        `{"version":2,"requestId":42,"traceUpdatedAt":1,"routingTrace":{"updatedAt":1}}`,
		"缺 requestId":   `{"version":1,"traceUpdatedAt":1,"routingTrace":{"updatedAt":1}}`,
		"requestId 为 0": `{"version":1,"requestId":0,"traceUpdatedAt":1,"routingTrace":{"updatedAt":1}}`,
		// 顶层修订号与载荷内不一致：宁可丢弃也不猜（写错轨迹比不写更难查）。
		"修订号不一致":        `{"version":1,"requestId":42,"traceUpdatedAt":2,"routingTrace":{"updatedAt":1}}`,
		"修订号非数字":        `{"version":1,"requestId":42,"traceUpdatedAt":"x","routingTrace":{"updatedAt":"x"}}`,
		"载荷缺 updatedAt": `{"version":1,"requestId":42,"traceUpdatedAt":1,"routingTrace":{}}`,
	}
	for name, payload := range invalid {
		if _, ok := ParseOutboxEntry(payload); ok {
			t.Fatalf("%s 应被拒绝", name)
		}
	}
}

// 清理任务的环境变量解析：默认保留 1 天、单批 10000。
func TestProbeLogCleanupConfig(t *testing.T) {
	cfg, warnings := ProbeLogCleanupConfigFromEnv(opsLookupFrom(nil))
	if !cfg.Enabled || cfg.Retention != 24*time.Hour || cfg.BatchSize != 10_000 {
		t.Fatalf("默认值不符: %+v", cfg)
	}
	if len(warnings) != 0 {
		t.Fatalf("未设置变量不应告警: %v", warnings)
	}

	// CI=true 时不启动（Node 显式跳过，避免 CI 删共享库历史）。
	ciCfg, _ := ProbeLogCleanupConfigFromEnv(opsLookupFrom(map[string]string{"CI": "true"}))
	if ciCfg.Enabled {
		t.Fatal("CI=true 时应停用探活历史清理")
	}

	five, warnings := ProbeLogCleanupConfigFromEnv(opsLookupFrom(map[string]string{
		"ENDPOINT_PROBE_LOG_RETENTION_DAYS":     "5",
		"ENDPOINT_PROBE_LOG_CLEANUP_BATCH_SIZE": "500",
	}))
	if five.Retention != 5*24*time.Hour || five.BatchSize != 500 {
		t.Fatalf("显式配置未生效: %+v", five)
	}
	if len(warnings) != 0 {
		t.Fatalf("合法值不应告警: %v", warnings)
	}

	// 保留 0 天是合法的（只留当前这一批）。
	zero, _ := ProbeLogCleanupConfigFromEnv(opsLookupFrom(map[string]string{
		"ENDPOINT_PROBE_LOG_RETENTION_DAYS": "0",
	}))
	if zero.Retention != 0 {
		t.Fatalf("保留 0 天应被接受，得到 %v", zero.Retention)
	}

	// 非法批大小回退并告警。
	bad, warned := ProbeLogCleanupConfigFromEnv(opsLookupFrom(map[string]string{
		"ENDPOINT_PROBE_LOG_CLEANUP_BATCH_SIZE": "-1",
	}))
	if bad.BatchSize != 10_000 || len(warned) != 1 {
		t.Fatalf("非法批大小应回退并告警: %+v %v", bad, warned)
	}
}
