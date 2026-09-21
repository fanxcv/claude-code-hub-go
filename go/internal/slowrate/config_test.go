package slowrate

import (
	"context"
	"testing"
	"time"
)

// 本文件钉住两件事：
//  1. SlowRateConfig 与 SlowRateProbeConfig 读的是**同一份渠道快照**，但走两条不同的
//     参数化路径（终态判定 vs 中途探测）——探测两列绝不能渗进 Params；
//  2. 「列 NULL ⇒ 不探测」这条产品承诺。它由 T <= 0 承载，而 normalize **不收** T，
//     所以 NULL 折成的 0 必须原样传到 IsSlowProbe 并让判定恒 false。若哪天有人「顺手」
//     把 T 也收敛到默认 30，本文件会红。

type stubProviderSource struct {
	config ProviderConfig
	found  bool
}

func (s stubProviderSource) SlowRateProvider(context.Context, int64) (ProviderConfig, bool) {
	return s.config, s.found
}

func intPtr(value int) *int { return &value }

// TestSlowRateConfigCarriesOnlyTerminalParams 钉住探测两列不进 Params。
func TestSlowRateConfigCarriesOnlyTerminalParams(t *testing.T) {
	config := NewSnapshotConfig(stubProviderSource{
		found: true,
		config: ProviderConfig{
			Enabled:                    true,
			WindowSeconds:              intPtr(1800),
			TriggerCount:               intPtr(3),
			RatioPerMille:              intPtr(300),
			PenaltyStep:                intPtr(10),
			PenaltyMax:                 intPtr(30),
			ProbeAfterFirstByteSeconds: intPtr(30),
			ProbeMinTokens:             intPtr(50),
		},
	})

	params, ok := config.SlowRateConfig(context.Background(), 1)
	if !ok {
		t.Fatal("已开启的渠道应返回 ok=true")
	}
	if params.WindowSeconds != 1800 || params.TriggerCount != 3 || params.RatioPerMille != 300 {
		t.Errorf("终态参数未按列投影: %+v", params)
	}
}

// TestSlowRateProbeConfigNullThresholdDisablesProbe 是那条产品承诺的钉子。
func TestSlowRateProbeConfigNullThresholdDisablesProbe(t *testing.T) {
	config := NewSnapshotConfig(stubProviderSource{
		found: true,
		config: ProviderConfig{
			Enabled:       true,
			RatioPerMille: intPtr(300),
			// 两个探测列都是 NULL：机制对该渠道关闭。
		},
	})

	params, ok := config.SlowRateProbeConfig(context.Background(), 1)
	if !ok {
		t.Fatal("渠道已开启监控，探测参数应可取到（关闭由 T<=0 表达，不是 ok=false）")
	}
	if params.AfterFirstByteSeconds != 0 {
		t.Fatalf("NULL 阈值应折成 0（不探测），实得 %d", params.AfterFirstByteSeconds)
	}
	// 端到端复核：NULL 阈值下无论等多久、吐多少 token，都不能判出慢。
	verdict := IsSlowProbe(
		ProbeInput{TokensSoFar: 100000, ElapsedSinceFirstByte: time.Hour, Baseline: 240},
		params,
	)
	if verdict {
		t.Error("阈值为 0（NULL）时 IsSlowProbe 必须恒 false，否则「默认关闭」的承诺失守")
	}
}

// TestSlowRateProbeConfigDefaultsMinTokens 钉住「列留空取出厂值」只作用于 MinTokens，
// 不作用于阈值 T（两者的零值语义相反）。
func TestSlowRateProbeConfigDefaultsMinTokens(t *testing.T) {
	config := NewSnapshotConfig(stubProviderSource{
		found: true,
		config: ProviderConfig{
			Enabled:                    true,
			ProbeAfterFirstByteSeconds: intPtr(30),
			// ProbeMinTokens 留空。
		},
	})

	params, ok := config.SlowRateProbeConfig(context.Background(), 1)
	if !ok {
		t.Fatal("应返回 ok=true")
	}
	if params.MinTokens != DefaultProbeMinTokens {
		t.Errorf("留空的最低 token 数应取出厂值 %d，实得 %d", DefaultProbeMinTokens, params.MinTokens)
	}
	if params.AfterFirstByteSeconds != 30 {
		t.Errorf("显式配置的阈值应原样保留，实得 %d", params.AfterFirstByteSeconds)
	}
}

// TestSlowRateProbeConfigDisabledProviderReturnsFalse 钉住未开启监控的渠道零开销跳过。
func TestSlowRateProbeConfigDisabledProviderReturnsFalse(t *testing.T) {
	for name, source := range map[string]stubProviderSource{
		"渠道未开启":  {found: true, config: ProviderConfig{Enabled: false}},
		"渠道不在快照": {found: false},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := NewSnapshotConfig(source).SlowRateProbeConfig(context.Background(), 1); ok {
				t.Error("未开启监控的渠道应返回 ok=false")
			}
		})
	}
}
