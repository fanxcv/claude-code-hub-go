package slowrate

import (
	"context"
	"testing"
	"time"
)

// 本文件钉住两件事：
//  1. SlowRateConfig 与 SlowRateProbeConfig 读的是**同一份渠道快照**，但走两条不同的
//     参数化路径（终态判定 vs 中途探测）——探测阈值绝不能渗进 Params；
//  2. 「列 NULL ⇒ 不探测」这条产品承诺。它由 T <= 0 承载（读取面**不收敛** T），
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

// TestSlowRateConfigCarriesOnlyTerminalParams 钉住探测阈值不进 Params。
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
			// 探测阈值为 NULL：机制对该渠道关闭。
		},
	})

	params, ok := config.SlowRateProbeConfig(context.Background(), 1)
	if !ok {
		t.Fatal("渠道已开启监控，探测参数应可取到（关闭由 T<=0 表达，不是 ok=false）")
	}
	if params.AfterFirstByteSeconds != 0 {
		t.Fatalf("NULL 阈值应折成 0（不探测），实得 %d", params.AfterFirstByteSeconds)
	}
	// 端到端复核：NULL 阈值下无论等多久都不能判出停滞。
	verdict := IsSlowProbe(
		ProbeInput{ElapsedSinceFirstByte: time.Hour},
		params,
	)
	if verdict {
		t.Error("阈值为 0（NULL）时 IsSlowProbe 必须恒 false，否则「默认关闭」的承诺失守")
	}
}

// TestSlowRateProbeConfigKeepsExplicitThreshold 钉住显式配置的阈值原样保留（不被收敛改写）。
func TestSlowRateProbeConfigKeepsExplicitThreshold(t *testing.T) {
	config := NewSnapshotConfig(stubProviderSource{
		found: true,
		config: ProviderConfig{
			Enabled:                    true,
			ProbeAfterFirstByteSeconds: intPtr(12),
		},
	})

	params, ok := config.SlowRateProbeConfig(context.Background(), 1)
	if !ok {
		t.Fatal("应返回 ok=true")
	}
	if params.AfterFirstByteSeconds != 12 {
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
