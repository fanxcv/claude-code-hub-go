package route

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/redis/go-redis/v9"
)

// fakeRedis 是最小 Redis 替身：只实现本包用到的 HGetAll / Get。
type fakeRedis struct {
	redis.UniversalClient
	hashes map[string]map[string]string
	values map[string]string
}

func (f *fakeRedis) HGetAll(_ context.Context, key string) *redis.MapStringStringCmd {
	cmd := redis.NewMapStringStringCmd(context.Background())
	cmd.SetVal(f.hashes[key])
	return cmd
}

func (f *fakeRedis) Get(_ context.Context, key string) *redis.StringCmd {
	cmd := redis.NewStringCmd(context.Background())
	if value, ok := f.values[key]; ok {
		cmd.SetVal(value)
		return cmd
	}
	cmd.SetErr(redis.Nil)
	return cmd
}

func newTestHealth(t *testing.T, redisClient redis.UniversalClient, enabled bool) *HealthReader {
	t.Helper()
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	return NewHealthReader(HealthOptions{
		Redis:                         redisClient,
		EndpointCircuitBreakerEnabled: enabled,
		Now:                           func() time.Time { return now },
	})
}

func providerStateKey(id int64) string { return ProviderStateKeyPrefix + strconv.FormatInt(id, 10) }

func TestProviderOpenReadsNodeKeyFormat(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	client := &fakeRedis{hashes: map[string]map[string]string{
		providerStateKey(11): {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(10*time.Minute).UnixMilli(), 10),
		},
		providerStateKey(12): {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10),
		},
		providerStateKey(13): {"circuitState": "half-open"},
	}}
	reader := newTestHealth(t, client, false)

	if open, reason := reader.ProviderOpen(context.Background(), 11); !open || reason != "" {
		t.Errorf("窗口内的 open 应判为打开，实际 open=%v reason=%q", open, reason)
	}
	if open, _ := reader.ProviderOpen(context.Background(), 12); open {
		t.Errorf("已过窗口的 open 应放行（转 half-open）")
	}
	if open, _ := reader.ProviderOpen(context.Background(), 13); open {
		t.Errorf("half-open 应放行试探")
	}
	if open, _ := reader.ProviderOpen(context.Background(), 99); open {
		t.Errorf("无状态即视为 closed")
	}
	if _, reason := reader.ProviderOpen(context.Background(), 11); reason != "" {
		t.Errorf("有 Redis 时不应给出降级原因，实际 %q", reason)
	}
}

func TestProviderOpenWithoutRedisIsFailOpen(t *testing.T) {
	reader := NewHealthReader(HealthOptions{})
	open, reason := reader.ProviderOpen(context.Background(), 1)
	if open || reason != ExitNoRedis {
		t.Errorf("无 Redis 应 fail-open 并给出 ExitNoRedis，实际 open=%v reason=%q", open, reason)
	}
}

func TestProviderOpenHonoursDisabledBreaker(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	client := &fakeRedis{hashes: map[string]map[string]string{
		providerStateKey(21): {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
		},
		ProviderConfigKeyPrefix + "21": {"failureThreshold": "0"},
	}}
	reader := newTestHealth(t, client, false)

	if open, _ := reader.ProviderOpen(context.Background(), 21); open {
		t.Errorf("failureThreshold <= 0 表示熔断器被禁用，应强制放行")
	}
	if state := reader.ProviderState(context.Background(), 21); state != StateClosed {
		t.Errorf("被禁用的熔断器状态应记 closed，实际 %q", state)
	}
}

func TestProviderStateMapsExpiredOpenToHalfOpen(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	client := &fakeRedis{hashes: map[string]map[string]string{
		providerStateKey(31): {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10),
		},
	}}
	reader := newTestHealth(t, client, false)
	if state := reader.ProviderState(context.Background(), 31); state != StateHalfOpen {
		t.Errorf("过期 open 应记 half-open，实际 %q", state)
	}
}

func TestVendorTypeOpenRespectsSwitchAndManualOpen(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	key := VendorTypeStateKeyPrefix + "7:" + string(convert.ProviderCodex)
	client := &fakeRedis{hashes: map[string]map[string]string{
		key: {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
		},
		VendorTypeStateKeyPrefix + "8:" + string(convert.ProviderCodex): {"manualOpen": "1"},
		VendorTypeStateKeyPrefix + "9:" + string(convert.ProviderCodex): {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(-time.Hour).UnixMilli(), 10),
		},
	}}

	disabled := newTestHealth(t, client, false)
	if disabled.VendorTypeOpen(context.Background(), 7, convert.ProviderCodex) {
		t.Errorf("端点熔断开关关闭时 vendor-type 一律不算打开")
	}

	enabled := newTestHealth(t, client, true)
	if !enabled.VendorTypeOpen(context.Background(), 7, convert.ProviderCodex) {
		t.Errorf("窗口内的 open 应判为打开")
	}
	if !enabled.VendorTypeOpen(context.Background(), 8, convert.ProviderCodex) {
		t.Errorf("manualOpen 应判为打开")
	}
	if enabled.VendorTypeOpen(context.Background(), 9, convert.ProviderCodex) {
		t.Errorf("已过窗口应放行")
	}
	if enabled.VendorTypeOpen(context.Background(), 42, convert.ProviderCodex) {
		t.Errorf("无状态即视为 closed")
	}
}

func TestEndpointOpen(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	client := &fakeRedis{hashes: map[string]map[string]string{
		EndpointStateKeyPrefix + "101": {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(time.Minute).UnixMilli(), 10),
		},
		EndpointStateKeyPrefix + "102": {"circuitState": "half-open"},
	}}
	reader := newTestHealth(t, client, true)
	if !reader.EndpointOpen(context.Background(), 101) {
		t.Errorf("窗口内 open 应判为打开")
	}
	if reader.EndpointOpen(context.Background(), 102) {
		t.Errorf("half-open 应放行")
	}
	if reader.EndpointOpen(context.Background(), 103) {
		t.Errorf("无状态应放行")
	}
}

func TestHealthRejectionProducesCircuitReasons(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	vendorProvider := baseProvider(1, convert.ProviderClaude)
	vendorProvider.ProviderVendorID = i64Ptr(7)
	openProvider := baseProvider(2, convert.ProviderClaude)

	client := &fakeRedis{hashes: map[string]map[string]string{
		VendorTypeStateKeyPrefix + "7:" + string(convert.ProviderClaude): {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
		},
		providerStateKey(2): {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
		},
	}}
	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{vendorProvider, openProvider}},
		Health: newTestHealth(t, client, true),
	})

	result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider != nil {
		t.Fatalf("全部熔断时不应选出供应商")
	}
	if result.Context.BeforeHealthCheck != 2 || result.Context.AfterHealthCheck != 0 {
		t.Errorf("健康过滤计数 = %d/%d，期望 2/0",
			result.Context.BeforeHealthCheck, result.Context.AfterHealthCheck)
	}
	details := map[int64]string{}
	for _, record := range result.Context.FilteredProviders {
		if record.Reason != ReasonCircuitOpen {
			t.Errorf("供应商 %d 的理由 = %q，期望 circuit_open", record.ID, record.Reason)
		}
		details[record.ID] = record.Details
	}
	if details[1] != "vendor_type_circuit_open" || details[2] != "circuit_open" {
		t.Errorf("熔断细节不符: %+v", details)
	}
}

func TestGatesProduceScheduleClientAndLimitReasons(t *testing.T) {
	provider := baseProvider(1, convert.ProviderClaude)
	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{provider}},
		Gates: Gates{
			Schedule: func(Provider) bool { return false },
		},
	})
	result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got := reasonOf(t, result.Context, 1); got != ReasonScheduleInactive {
		t.Errorf("理由 = %q，期望 schedule_inactive", got)
	}

	clientGate := NewSelector(Options{
		Source: &stubSource{providers: []Provider{provider}},
		Gates: Gates{
			Client: func(Provider) (bool, string) { return false, "allowlist_miss" },
		},
	})
	result, err = clientGate.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got := reasonOf(t, result.Context, 1); got != ReasonClientRestriction {
		t.Errorf("理由 = %q，期望 client_restriction", got)
	}
	if result.Context.EnabledProviders != 0 {
		t.Errorf("客户端限制发生在基础过滤之前，enabledProviders 应为 0，实际 %d", result.Context.EnabledProviders)
	}

	limitGate := NewSelector(Options{
		Source: &stubSource{providers: []Provider{provider}},
		Gates: Gates{
			Limits: func(context.Context, Provider) (bool, string) { return false, "5h 限额已达" },
		},
	})
	result, err = limitGate.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got := reasonOf(t, result.Context, 1); got != ReasonRateLimited {
		t.Errorf("理由 = %q，期望 rate_limited", got)
	}
}

// TestEffectiveStateAgreesWithProviderOpen 是「显示侧与决策侧同源」的回归钉子。
//
// 生产分叉（用户实报）：供应商页面显示「已熔断」，请求却照常通过。机制是管理面徽标直接读 Redis
// 原值（原值 open），而选路按「open 且窗口已过期 ⇒ half-open」放行——两侧各判各的。
// 现在两侧都走 EffectiveProviderState：显示侧取它的返回值，决策侧 ProviderOpen 的到期判定也经它。
// 本用例把同一组 Redis 状态同时喂给两侧，断言结论一致。
func TestEffectiveStateAgreesWithProviderOpen(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	future := strconv.FormatInt(now.Add(10*time.Minute).UnixMilli(), 10)
	past := strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10)

	cases := []struct {
		name      string
		hash      map[string]string
		wantState CircuitState
		// wantBlocked 是决策侧的期望：ProviderOpen 为 true 表示「拦下，不放行」。
		wantBlocked bool
	}{
		{"open 且窗口内", map[string]string{"circuitState": "open", "circuitOpenUntil": future}, StateOpen, true},
		{"open 且窗口已过期", map[string]string{"circuitState": "open", "circuitOpenUntil": past}, StateHalfOpen, false},
		{"open 且无窗口（恒拦）", map[string]string{"circuitState": "open"}, StateOpen, true},
		{"half-open", map[string]string{"circuitState": "half-open"}, StateHalfOpen, false},
		{"closed", map[string]string{"circuitState": "closed"}, StateClosed, false},
		{"键缺失", nil, StateClosed, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			id := int64(700)
			client := &fakeRedis{hashes: map[string]map[string]string{}}
			if testCase.hash != nil {
				client.hashes[providerStateKey(id)] = testCase.hash
			}
			reader := newTestHealth(t, client, false)

			// 显示侧：管理面 /providers/health 用的就是这个纯函数（同参数口径）。
			raw := CircuitState(testCase.hash["circuitState"])
			if testCase.hash == nil {
				raw = StateClosed // 键缺失 = 出厂闭态（与 providerCircuitSnapshotFromRaw 同判）
			}
			gotState := EffectiveProviderState(raw, parseInt64(testCase.hash["circuitOpenUntil"]), now.UnixMilli())
			if gotState != testCase.wantState {
				t.Errorf("显示侧 EffectiveProviderState = %q，期望 %q", gotState, testCase.wantState)
			}

			// 决策侧。
			gotBlocked, _ := reader.ProviderOpen(context.Background(), id)
			if gotBlocked != testCase.wantBlocked {
				t.Errorf("决策侧 ProviderOpen = %v，期望 %v", gotBlocked, testCase.wantBlocked)
			}

			// 同源断言：把两侧的结论折算成同一个布尔，必须相等。
			// （「拦下」⟺ 有效态是 open；这条等价关系就是两侧不分叉的定义。）
			if blockedByState := gotState == StateOpen; blockedByState != gotBlocked {
				t.Errorf("显示侧与决策侧分叉：有效态 %q ⇒ 拦下=%v，但 ProviderOpen=%v",
					gotState, blockedByState, gotBlocked)
			}
		})
	}
}
