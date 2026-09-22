package slowrate

import (
	"context"
	"testing"
)

// 本文件钉住 SlowRateConfig 的投影：终态参数按列取，且**探测阈值绝不能渗进 Params**
// （它属另一条路，读取面在 dataplane.idleTimeoutCache）。
//
// 「列 NULL ⇒ 不探测」这条产品承诺的钉子不在本文件：探测阈值的读取面已不在 slowrate 包，
// 改由 dataplane 侧的钉子（idle_timeout_probe_gate_test.go）与 probe_test.go 的 T<=0 判据共同把住。

type stubProviderSource struct {
	config ProviderConfig
	found  bool
}

func (s stubProviderSource) SlowRateProvider(context.Context, int64) (ProviderConfig, bool) {
	return s.config, s.found
}

func intPtr(value int) *int { return &value }

func floatPtr(value float64) *float64 { return &value }

// TestSlowRateConfigCarriesOnlyTerminalParams 钉住探测阈值不进 Params。
func TestSlowRateConfigCarriesOnlyTerminalParams(t *testing.T) {
	config := NewSnapshotConfig(stubProviderSource{
		found: true,
		config: ProviderConfig{
			Enabled:                    true,
			WindowMinutes:              intPtr(30),
			TriggerCount:               intPtr(3),
			Ratio:                      floatPtr(0.3),
			PenaltyStep:                intPtr(10),
			PenaltyMax:                 intPtr(30),
			ProbeAfterFirstByteSeconds: intPtr(30),
		},
	})

	params, ok := config.SlowRateConfig(context.Background(), 1)
	if !ok {
		t.Fatal("已开启的渠道应返回 ok=true")
	}
	if params.WindowMinutes != 30 || params.TriggerCount != 3 || params.Ratio != 0.3 {
		t.Errorf("终态参数未按列投影: %+v", params)
	}
}

// TestSlowRateConfigCarriesRecoveryRequests 钉住恢复阈值进 Params（列值优先于出厂默认）。
//
// 与 dataplane 侧的 TestRecoveryRequestsReachesParamsThroughSnapshot 配对：那条钉生产那一跳，
// 这条钉本包投影。两处都绿才说明「列 → Params」整条通。
func TestSlowRateConfigCarriesRecoveryRequests(t *testing.T) {
	const recovery = 2
	config := NewSnapshotConfig(stubProviderSource{
		found: true,
		config: ProviderConfig{
			Enabled:          true,
			RecoveryRequests: intPtr(recovery),
		},
	})

	params, ok := config.SlowRateConfig(context.Background(), 1)
	if !ok {
		t.Fatal("已开启的渠道应返回 ok=true")
	}
	if params.RecoveryRequests != recovery {
		t.Fatalf("恢复阈值应为列值 %d，实得 %d", recovery, params.RecoveryRequests)
	}
	// 列 NULL 时仍须收敛到出厂默认（不能让 0 直接进判定：0 会让 streak 判定恒真）。
	blank := NewSnapshotConfig(stubProviderSource{found: true, config: ProviderConfig{Enabled: true}})
	fallback, _ := blank.SlowRateConfig(context.Background(), 1)
	if got := fallback.normalize().RecoveryRequests; got != DefaultParams().RecoveryRequests {
		t.Fatalf("列 NULL 应收敛到出厂默认 %d，得到 %d", DefaultParams().RecoveryRequests, got)
	}
}
