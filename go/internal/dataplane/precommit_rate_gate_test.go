package dataplane

import (
	"bytes"
	"context"
	"os"
	"strings"
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

// TestPrecommitRateGateRefusesMultiModelCandidates 钉住边界 5：
// **多模型渠道不得凭基线推导阈值**。
//
// 为何必须拒绝：装配缝按 provider 粒度，拿不到本次请求的模型名，而基线与状态是 provider×model
// 粒度。多模型渠道下取哪个模型都是猜：若取中的那个模型基线高，而实际请求的是正常就慢的另一个模型，
// 正常流会被判 FailSlowRate（524 + 换家）。这不再是「少判」而是**误判**。
//
// 本例故意只给**排第一**的模型写基线：旧实现（取第一个精确模型）会命中它而把闸打开，故本用例
// 在旧实现上必红——这正是它要拖住的回归。
func TestPrecommitRateGateRefusesMultiModelCandidates(t *testing.T) {
	client := feedRedis(t)
	const providerID = 167
	const fastModel = "fast-model"
	const slowModel = "slow-model"
	key := route.SlowRateBaselineKey(providerID, fastModel)
	ctx := context.Background()

	reader := newProbeGateRowReader()
	cache := newIdleTimeoutCache(reader, client, nil, logx.New(nil))
	// 只给排第一的模型写可用基线：若实现仍取第一个模型，这条就会让闸打开。
	if err := client.Set(ctx, key, `{"median":242.6,"source":"primary"}`, time.Minute).Err(); err != nil {
		t.Fatalf("写入基线失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(ctx, key).Err() })

	t.Run("恰好一个精确模型 ⇒ 照常推导", func(t *testing.T) {
		reader.set(providerID, precommitRow(true, precommitBoolRef(true), nil, `["`+fastModel+`"]`))
		cache.ttl.Clear()
		if got := cache.precommitRate(providerID); got <= 0 {
			t.Fatalf("单模型渠道应能凭基线推导，实得 %d", got)
		}
	})

	t.Run("两个精确模型 ⇒ 一律不启用", func(t *testing.T) {
		reader.set(providerID, precommitRow(true, precommitBoolRef(true), nil,
			`["`+fastModel+`","`+slowModel+`"]`))
		cache.ttl.Clear()
		if got := cache.precommitRate(providerID); got != 0 {
			t.Fatalf("多模型渠道不得凭基线推导（会按错的模型误判正常流），期望 0，实得 %d", got)
		}
	})

	t.Run("通配规则 ⇒ 不启用", func(t *testing.T) {
		reader.set(providerID, precommitRow(true, precommitBoolRef(true), nil,
			`[{"matchType":"prefix","pattern":"claude-"}]`))
		cache.ttl.Clear()
		if got := cache.precommitRate(providerID); got != 0 {
			t.Fatalf("通配规则展开不出确定模型名，必须不启用，实得 %d", got)
		}
	})

	t.Run("同一模型写两遍不算多模型", func(t *testing.T) {
		reader.set(providerID, precommitRow(true, precommitBoolRef(true), nil,
			`["`+fastModel+`","`+fastModel+`"]`))
		cache.ttl.Clear()
		if got := cache.precommitRate(providerID); got <= 0 {
			t.Fatalf("去重后只有一个模型，应照常推导，实得 %d", got)
		}
	})
}

// TestPrecommitShadowDefaultsToOff 钉住影子期默认值：未设环境变量时必须**按渠道开关裁决**。
//
// 为何重要：影子为真时 precommitRate 直接返回 0，且**优先于**渠道开关 slow_rate_precommit_enabled
// ⇒ 运维把渠道开关打开也毫无效果（开关在、行为不在）。2026-09-22 生产实测：wb 的开关开了很久，
// 慢请求一条都没被中断，根因就是出厂值曾为 true。故默认值必须是 false：是否裁决只由渠道开关决定。
func TestPrecommitShadowDefaultsToOff(t *testing.T) {
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
	if precommitShadowEnabled() {
		t.Fatal("环境变量未设置时应为出厂值 false（按渠道开关裁决）")
	}

	// 空串只表示「设了但没值」，必须回落到出厂值，而不是误判为别的。
	t.Setenv("CCH_SLOW_PRECOMMIT_SHADOW", "")
	if precommitShadowEnabled() {
		t.Fatal("空串应回落到出厂值 false")
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
	// 无法识别的取值回出厂值，而不是静默当成 true 转采集。
	t.Setenv("CCH_SLOW_PRECOMMIT_SHADOW", "maybe")
	if precommitShadowEnabled() {
		t.Fatal("无法识别的取值应回落到出厂值 false")
	}
}

// TestPrecommitRateWarnsWhenShadowSuppressesGate 钉住「开关开了、闸却因影子不动」的告警。
//
// 这条静默失效在线上极难察觉（渠道开关为真、日志却全是纯采集、慢请求照旧跑完），故必须有一条
// 可检索的告警把它变成可观测事实。
func TestPrecommitRateWarnsWhenShadowSuppressesGate(t *testing.T) {
	enabled := true
	minBytes := 240
	row := &store.Provider{
		ID:                                 167,
		SlowRatePrecommitEnabled:           &enabled,
		SlowRatePrecommitMinBytesPerSecond: &minBytes,
	}

	t.Setenv("CCH_SLOW_PRECOMMIT_SHADOW", "1")
	if !precommitShadowEnabled() {
		t.Fatal("前置：影子应为真")
	}
	logs := &bytes.Buffer{}
	cache := &idleTimeoutCache{logger: logx.New(logs)}
	if rate := cache.precommitRateFromRow(context.Background(), row); rate != minBytes {
		t.Fatalf("取值不应受影子影响，得 %d 期望 %d", rate, minBytes)
	}
	if !strings.Contains(logs.String(), "precommit_gate_suppressed_by_shadow") {
		t.Fatalf("渠道开关已开而影子为真时必须告警；实际日志：%q", logs.String())
	}

	// 影子关闭时不得告警（否则热路径徒增噪音）。
	logs.Reset()
	t.Setenv("CCH_SLOW_PRECOMMIT_SHADOW", "0")
	_ = cache.precommitRateFromRow(context.Background(), row)
	if strings.Contains(logs.String(), "precommit_gate_suppressed_by_shadow") {
		t.Fatalf("影子关闭时不得告警；实际日志：%q", logs.String())
	}
}
