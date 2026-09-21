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
