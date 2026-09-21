package route

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住「既有会话绑定因**临时**原因被跳过时，成功终态不得改绑」这条契约。
//
// 为什么必须单独钉：绑定未被采用时选路会照常选备用，而终态成功侧的 CAS 原先只看
// 「WinnerProviderID > 0 && committed」——于是备用一成功就把绑定改成备用。设计稿 §4
// 对熔断明定「跳过该 provider、不清空绑定、待恢复后会话仍粘回去」；改绑后这一条即为假，
// 且此后每次熔断都把会话永久搬走一次。缺陷形态是「单测、日志、配置面全都看不见」：
// 只有对比「本次选了谁」与「绑定还指向谁」才看得出，故这里把 bypass 判据逐条钉住。

// TestSessionBindingBypassKeepsBindingOnCircuitOpen 是主线：绑定 provider 熔断开闸时，
// 本次仍能选别家，但 bypass 必须标为 transient——终态据此跳过成功侧 CAS。
func TestSessionBindingBypassKeepsBindingOnCircuitOpen(t *testing.T) {
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
		t.Errorf("bypass = %v，期望 transient（临时原因下不得改绑）", result.SessionBindingBypass)
	}
	if !result.SessionBindingBypass.KeepsBinding() {
		t.Error("KeepsBinding 应为真：终态成功侧据此跳过 CAS")
	}
	if got := reasonOf(t, result.Context, bound.ID); got != ReasonCircuitOpen {
		t.Errorf("绑定 provider 的过滤理由 = %q，期望 %q", got, ReasonCircuitOpen)
	}
}

// TestSessionBindingBypassAllowsRebindWhenBoundProviderDisabled 反向：绑定 provider 被**停用**
// （结构性失效）时必须允许改绑——旧绑定已经死了，新 winner 才是该会话该去的地方。
//
// 没有这条反向，把 transientRejection 写成「恒真」也能让主线变绿，而后果是会话永远
// 钉在一家已被停用的渠道上。
func TestSessionBindingBypassAllowsRebindWhenBoundProviderDisabled(t *testing.T) {
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
		t.Errorf("bypass = %v，期望 none（结构性失效应允许改绑）", result.SessionBindingBypass)
	}
	if result.SessionBindingBypass.KeepsBinding() {
		t.Error("KeepsBinding 应为假：停用是配置决策，不是暂时故障")
	}
}

// TestSessionBindingBypassNoneWhenBindingAdopted 绑定被正常采用时 bypass 恒为零值：
// 接线前行为逐字不变（成功侧照旧 CAS，那条 CAS 写的就是同一个 provider）。
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
// 否则它会被当成「行已不存在」而允许改绑（本 lane 修的缺陷）。
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
// 属临时**（绑定该留着等它回来），要改配置才恢复的属结构性（允许改绑）。
//
// 为何要逐条列全：这张表是「熔断恢复后会话仍粘回去」的唯一判据来源；漏一条就会让某类
// 临时故障悄悄把会话永久搬走，且只有对比绑定键才看得出。
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
			t.Errorf("%q 应判为临时（绑定保留）", reason)
		}
	}
	for _, reason := range structural {
		if transientRejection(reason) {
			t.Errorf("%q 应判为结构性（允许改绑）", reason)
		}
	}
}
