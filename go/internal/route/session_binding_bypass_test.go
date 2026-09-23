package route

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住既有会话绑定未被采用时的原因分类；分类供留痕，不门控终态改绑。

// TestSessionBindingBypassTransientOnCircuitOpen 钉住熔断开闸后改选备用仍留痕 transient。
func TestSessionBindingBypassTransientOnCircuitOpen(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	bound := baseProvider(7, convert.ProviderClaude)
	healthy := baseProvider(8, convert.ProviderClaude)

	client := &fakeRedis{hashes: map[string]map[string]string{
		providerStateKey(bound.ID): {
			"circuitState":     string(StateOpen),
			"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
		},
	}}
	selector := NewSelector(Options{
		Source:   &stubSource{providers: []Provider{bound, healthy}, byID: map[int64]Provider{7: bound, 8: healthy}},
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		Health:   newTestHealth(t, client, true),
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})

	result, err := selector.Select(context.Background(), sessionBindingRequest(bound.ID))
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != healthy.ID {
		t.Fatalf("熔断的绑定 provider 不该被选中，实际 %+v", result.Provider)
	}
	if result.SessionBindingBypass != SessionBindingBypassTransient {
		t.Errorf("bypass = %v，期望 transient（熔断属临时原因）", result.SessionBindingBypass)
	}
	if got := reasonOf(t, result.Context, bound.ID); got != ReasonCircuitOpen {
		t.Errorf("绑定 provider 的过滤理由 = %q，期望 %q", got, ReasonCircuitOpen)
	}
}

// TestSessionBindingBypassNoneWhenBoundProviderDisabled 钉住停用属结构性原因而非 transient。
func TestSessionBindingBypassNoneWhenBoundProviderDisabled(t *testing.T) {
	bound := baseProvider(7, convert.ProviderClaude)
	bound.IsEnabled = false
	healthy := baseProvider(8, convert.ProviderClaude)

	selector := NewSelector(Options{
		Source:   &stubSource{providers: []Provider{bound, healthy}, byID: map[int64]Provider{7: bound, 8: healthy}},
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})

	result, err := selector.Select(context.Background(), sessionBindingRequest(bound.ID))
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != healthy.ID {
		t.Fatalf("停用的绑定 provider 不该被选中，实际 %+v", result.Provider)
	}
	if result.SessionBindingBypass != SessionBindingBypassNone {
		t.Errorf("bypass = %v，期望 none（停用属结构性原因）", result.SessionBindingBypass)
	}
}

// TestSessionBindingBypassNoneWhenBindingAdopted 绑定被正常采用时 bypass 恒为零值：
// 成功侧照旧 CAS，那条 CAS 写的就是同一个 provider。
func TestSessionBindingBypassNoneWhenBindingAdopted(t *testing.T) {
	bound := baseProvider(7, convert.ProviderClaude)
	other := baseProvider(8, convert.ProviderClaude)

	selector := NewSelector(Options{
		Source:   &stubSource{providers: []Provider{bound, other}, byID: map[int64]Provider{7: bound, 8: other}},
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})

	result, err := selector.Select(context.Background(), sessionBindingRequest(bound.ID))
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method != MethodSessionReuse {
		t.Fatalf("应短路为会话复用，实际 %q", result.Method)
	}
	if result.SessionBindingBypass != SessionBindingBypassNone {
		t.Errorf("bypass = %v，期望 none", result.SessionBindingBypass)
	}
}

// TestSessionBindingBypassReadsFilterLedger 钉住判定读的是**过滤留痕**本身，而不是另算一遍：
// 留痕里没有该家（行已不存在）时不得当成临时原因。
//
// 另钉一支：**读绑定行失败**不进留痕（候选根本没读出来），必须由 lookupFailed 显式传入，
// 否则它会被误记成「行已不存在」。
func TestSessionBindingBypassReadsFilterLedger(t *testing.T) {
	binding := &SessionBindingSnapshot{SessionID: "s", KeyID: 1, ProviderID: 7}
	cases := []struct {
		name         string
		filtered     []Filtered
		binding      *SessionBindingSnapshot
		lookupFailed bool
		want         SessionBindingBypass
	}{
		{name: "无绑定", filtered: nil, binding: nil, want: SessionBindingBypassNone},
		{name: "空绑定", filtered: nil, binding: &SessionBindingSnapshot{ProviderID: 0}, want: SessionBindingBypassNone},
		{name: "留痕里没有该家", filtered: []Filtered{{ID: 8, Reason: ReasonCircuitOpen}}, binding: binding, want: SessionBindingBypassNone},
		{name: "熔断", filtered: []Filtered{{ID: 7, Reason: ReasonCircuitOpen}}, binding: binding, want: SessionBindingBypassTransient},
		{name: "会话冷却", filtered: []Filtered{{ID: 7, Reason: ReasonSlowRateCooldown}}, binding: binding, want: SessionBindingBypassTransient},
		{name: "停用", filtered: []Filtered{{ID: 7, Reason: ReasonDisabled}}, binding: binding, want: SessionBindingBypassNone},
		{name: "读绑定行失败", filtered: nil, binding: binding, lookupFailed: true, want: SessionBindingBypassTransient},
		{name: "读失败但本无绑定", filtered: nil, binding: nil, lookupFailed: true, want: SessionBindingBypassNone},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := sessionBindingBypass(testCase.filtered, testCase.binding, testCase.lookupFailed)
			if got != testCase.want {
				t.Errorf("sessionBindingBypass = %v，期望 %v", got, testCase.want)
			}
		})
	}
}

// TestTransientRejectionClassifiesTemporaryReasons 钉住分界原则：**不改任何配置就可能恢复的
// 属临时**，要改配置才恢复的属结构性。逐条列全以免选路留痕漏记原因。
func TestTransientRejectionClassifiesTemporaryReasons(t *testing.T) {
	transient := []Reason{
		ReasonCircuitOpen,
		ReasonSlowRateCooldown,
		ReasonScheduleInactive,
		ReasonRateLimited,
		ReasonExcluded,
	}
	structural := []Reason{
		ReasonDisabled,
		ReasonModelNotAllowed,
		ReasonFormatTypeMismatch,
		ReasonProtocolConversionDisabled,
		ReasonClientRestriction,
		ReasonEndpointUnavailable,
		ReasonTypeMismatch,
	}
	for _, reason := range transient {
		if !transientRejection(reason) {
			t.Errorf("%q 应判为临时", reason)
		}
	}
	for _, reason := range structural {
		if transientRejection(reason) {
			t.Errorf("%q 应判为结构性", reason)
		}
	}
}
