package dataplane

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/slowrate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住「提交前速率闸」的配置面接线：**阈值从哪来**、**什么时候不启用**。
//
// 判据一律走 precommitRate（真正的消费缝），不是只测内部推导函数——否则「接线被摘掉」
// 照样全绿（同目录 slow_probe_wiring_nail_test.go 记的正是这个盲区）。
//
// 该闸的四条安全边界（本文件逐条钉）：
//  1. 必须**显式打开**（列 NULL = 未覆盖 ⇒ false）：它改变首字时延，不得随监控开关自动生效；
//  2. 阈值列显式设值即用之（运维可覆盖推导结果）；
//  3. 未设值则由**基线**推导；**没有可用基线就不启用**（fail-open，宁可少判不可误判）；
//  4. 监控总闸关 ⇒ 一律不启用（与停滞探测同口径）。

func precommitBoolRef(value bool) *bool { return &value }
func precommitIntRef(value int) *int    { return &value }

// precommitRow 造一行：静默超时固定 5000ms，监控开关与速率闸各列按参数。
func precommitRow(monitor bool, enabled *bool, threshold *int, allowedModels string) *store.Provider {
	row := &store.Provider{
		ID:                                 167,
		StreamingIdleTimeoutMS:             5000,
		SlowRateMonitorEnabled:             monitor,
		SlowRatePrecommitEnabled:           enabled,
		SlowRatePrecommitMinBytesPerSecond: threshold,
	}
	if allowedModels != "" {
		row.AllowedModels = []byte(allowedModels)
	}
	return row
}

func precommitCache(t *testing.T, row *store.Provider) *idleTimeoutCache {
	t.Helper()
	reader := newProbeGateRowReader()
	reader.set(167, row)
	return newIdleTimeoutCache(reader, nil, nil, logx.New(nil))
}

// TestPrecommitRateGateRequiresExplicitEnable 钉住边界 1 与 2。
func TestPrecommitRateGateRequiresExplicitEnable(t *testing.T) {
	cases := []struct {
		name      string
		monitor   bool
		enabled   *bool
		threshold *int
		want      int
	}{
		{"监控关+闸开+阈值80 ⇒ 不启用", false, precommitBoolRef(true), precommitIntRef(80), 0},
		{"监控开+闸NULL(未覆盖) ⇒ 不启用", true, nil, precommitIntRef(80), 0},
		{"监控开+闸显式false ⇒ 不启用", true, precommitBoolRef(false), precommitIntRef(80), 0},
		{"监控开+闸开+阈值80 ⇒ 80", true, precommitBoolRef(true), precommitIntRef(80), 80},
		// 显式设 0/负数视作「关闭这个闸」，不算「未覆盖」——否则会被误当成「由基线推导」。
		{"监控开+闸开+阈值显式0 ⇒ 不启用", true, precommitBoolRef(true), precommitIntRef(0), 0},
		{"监控开+闸开+阈值显式-5 ⇒ 不启用", true, precommitBoolRef(true), precommitIntRef(-5), 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cache := precommitCache(t, precommitRow(testCase.monitor, testCase.enabled, testCase.threshold,
				`["deepseek-v4.1-flash"]`))
			if got := cache.precommitRate(167); got != testCase.want {
				t.Fatalf("阈值应为 %d，实得 %d（monitor=%v enabled=%v threshold=%v）",
					testCase.want, got, testCase.monitor, testCase.enabled, testCase.threshold)
			}
		})
	}
}

// TestPrecommitRateGateDisabledWhenBaselineMissing 钉住边界 3：**没有基线就不启用**。
//
// 三种「读不到基线」的成因都得覆盖，因为它们的漏洞方向不同：
//   - Redis 未装配（nil）：部署未配 Redis 时整条机制必须静默不启用；
//   - 模型分量取不出（allowed_models 为空/非精确规则）：拼不出组合键；
//   - 键不存在/来源不可用：由真 Redis 用例覆盖（见下一条）。
func TestPrecommitRateGateDisabledWhenBaselineMissing(t *testing.T) {
	cases := []struct {
		name          string
		allowedModels string
	}{
		{"Redis 未装配", `["deepseek-v4.1-flash"]`},
		{"allowed_models 为空", ``},
		{"allowed_models 为 null", `null`},
		{"allowed_models 无精确规则", `[{"matchType":"prefix","pattern":"deepseek"}]`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cache := precommitCache(t, precommitRow(true, precommitBoolRef(true), nil, testCase.allowedModels))
			if got := cache.precommitRate(167); got != 0 {
				t.Fatalf("没有可用基线时必须不启用（0），实得 %d", got)
			}
		})
	}
}

// TestPrecommitRateGateDerivesFromBaseline 钉住边界 3 的正面：基线在场时按同一套系数推导。
//
// 用真 Redis（未设 CCH_TEST_REDIS_URL 即跳过，与仓库其余 Redis 集成用例一致）：基线键的
// 形制与来源判据都是跨包契约，替身会比真依赖宽容，正是本仓反复缺陷④。
func TestPrecommitRateGateDerivesFromBaseline(t *testing.T) {
	client := feedRedis(t)
	const providerID = 167
	const modelKey = "deepseek-v4.1-flash"
	key := route.SlowRateBaselineKey(providerID, modelKey)
	ctx := context.Background()

	reader := newProbeGateRowReader()
	reader.set(providerID, precommitRow(true, precommitBoolRef(true), nil, `["`+modelKey+`"]`))
	cache := newIdleTimeoutCache(reader, client, nil, logx.New(nil))
	t.Cleanup(func() { _ = client.Del(ctx, key).Err() })

	// 不可用来源（extended_stale）⇒ 不启用。设计稿明定它只做会话级降级、不做渠道级判定。
	if err := client.Set(ctx, key, `{"median":242.6,"source":"extended_stale"}`, time.Minute).Err(); err != nil {
		t.Fatalf("写入基线失败: %v", err)
	}
	if got := cache.precommitRate(providerID); got != 0 {
		t.Fatalf("extended_stale 不算可用基线，必须不启用（0），实得 %d", got)
	}

	// 可用来源（primary）⇒ 按 DerivePrecommitMinBytesPerSecond 推导。
	if err := client.Set(ctx, key, `{"median":242.6,"source":"primary"}`, time.Minute).Err(); err != nil {
		t.Fatalf("写入基线失败: %v", err)
	}
	cache.ttl.Clear()
	want := slowrate.DerivePrecommitMinBytesPerSecond(242.6, slowrate.DefaultParams().Ratio)
	if want <= 0 {
		t.Fatalf("推导结果不应为非正：%d", want)
	}
	if got := cache.precommitRate(providerID); got != want {
		t.Fatalf("阈值应为 %d（由基线 %v × 系数 %v 推导），实得 %d",
			want, 242.6, slowrate.DefaultParams().Ratio, got)
	}

	// 显式设值优先于推导：同一条基线下换成显式阈值必须改读数。
	reader.set(providerID, precommitRow(true, precommitBoolRef(true), precommitIntRef(77), `["`+modelKey+`"]`))
	cache.ttl.Clear()
	if got := cache.precommitRate(providerID); got != 77 {
		t.Fatalf("显式阈值应覆盖推导，期望 77，实得 %d", got)
	}
}

// TestPrecommitShadowDefaultsToOn 钉住影子期默认值：未设环境变量时必须只记录不裁决。
//
// 为何重要：影子期是「先取证再执法」的闸门。若默认值被改成 false，上线即改变首字时延，
// 而三档阈值尚未由生产数据标定。
func TestPrecommitShadowDefaultsToOn(t *testing.T) {
	// 完全未设置：回出厂值。
	saved, had := os.LookupEnv("CCH_SLOW_PRECOMMIT_SHADOW")
	if err := os.Unsetenv("CCH_SLOW_PRECOMMIT_SHADOW"); err != nil {
		t.Fatalf("清空环境变量失败: %v", err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("CCH_SLOW_PRECOMMIT_SHADOW", saved)
			return
		}
		_ = os.Unsetenv("CCH_SLOW_PRECOMMIT_SHADOW")
	})
	if !precommitShadowEnabled() {
		t.Fatal("环境变量未设置时应为出厂值 true（影子期）")
	}

	// 空串只表示「设了但没值」，必须回落到出厂值，而不是解析成 false 转执法。
	t.Setenv("CCH_SLOW_PRECOMMIT_SHADOW", "")
	if !precommitShadowEnabled() {
		t.Fatal("空串应回落到出厂值 true")
	}

	for _, raw := range []string{"0", "false", "FALSE", "off", "no"} {
		t.Setenv("CCH_SLOW_PRECOMMIT_SHADOW", raw)
		if precommitShadowEnabled() {
			t.Fatalf("%q 应解析为 false（转执法）", raw)
		}
	}
	for _, raw := range []string{"1", "true", "on", "yes"} {
		t.Setenv("CCH_SLOW_PRECOMMIT_SHADOW", raw)
		if !precommitShadowEnabled() {
			t.Fatalf("%q 应解析为 true（影子）", raw)
		}
	}
	// 无法识别的取值回出厂值，而不是静默当成 false 转执法。
	t.Setenv("CCH_SLOW_PRECOMMIT_SHADOW", "maybe")
	if !precommitShadowEnabled() {
		t.Fatal("无法识别的取值应回落到出厂值 true")
	}
}
