package terminal

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 本文件钉住「选路层判定本次要保留既有绑定时，成功侧不得改绑」这条**跨层契约的消费端**。
//
// 为什么必须单独钉：判定在选路层（route.SessionBindingBypass），动作在终态层，两者隔着
// 守卫链与 pctx。只钉选路侧会漏掉「判定做对了但没人读」——本仓「已定义≠已接线」已犯多次，
// 其形态正是单测全绿而生产无效。摘掉 sessionBindingWriteback 里那道闸门，本文件即红。

// TestSessionBindingWinnerSkippedWhenBindingKept 主线：盖了保留事实后，成功侧一次 CAS 都不发。
//
// 同步与异步各钉一次：两半的时机不同（异步成功侧在队列 flush 之后），闸门必须对两者同时成立。
func TestSessionBindingWinnerSkippedWhenBindingKept(t *testing.T) {
	t.Run("同步", func(t *testing.T) {
		writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
		recorder := &sessionBindingRecorder{}
		pc := newSessionBindingContext(t, recorder, 81)
		pc.SetSessionBindingKeep("transient")

		settlement := okSettlement(nil)
		settlement.Affinity = AffinityDirective{WinnerProviderID: 7}
		if _, err := New(writer, Options{}).SettleContext(context.Background(), pc, settlement, nil); err != nil && !errors.Is(err, ErrNotSettled) {
			t.Fatalf("同步结算失败: %v", err)
		}
		if events, _, _, _ := recorder.snapshot(); len(events) != 0 {
			t.Fatalf("绑定须保留时不得改绑（设计稿 §4：熔断是暂时的，待恢复仍粘回去），收到 %v", events)
		}
	})

	t.Run("异步", func(t *testing.T) {
		writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
		queue, settler := newAsyncFixture(t, writer, AsyncOptions{
			MaxPending: 8, BatchSize: 8, FlushInterval: time.Hour,
		})
		recorder := &sessionBindingRecorder{}
		pc := newSessionBindingContext(t, recorder, 82)
		pc.SetSessionBindingKeep("transient")

		settlement := okSettlement(nil)
		settlement.Affinity = AffinityDirective{WinnerProviderID: 7}
		if _, err := settler.SettleContext(context.Background(), pc, settlement, nil); err != nil {
			t.Fatalf("入队失败: %v", err)
		}
		flushQueue(t, queue)
		if events, _, _, _ := recorder.snapshot(); len(events) != 0 {
			t.Fatalf("绑定须保留时不得改绑（异步 flush 之后也不得），收到 %v", events)
		}
	})
}

// TestSessionBindingFailureStillFiresWhenBindingKept 反向：保留事实**只压成功侧**，
// 失败侧的冷却/清绑定照发。没有这条，把闸门写成「有保留事实就整段返回」也能让主线变绿，
// 而后果是本次失败的那家再也不被冷却。
func TestSessionBindingFailureStillFiresWhenBindingKept(t *testing.T) {
	cases := []struct {
		name      string
		directive AffinityDirective
		want      string
	}{
		{name: "供应商故障仍写冷却", directive: AffinityDirective{TombstoneProviderID: 9}, want: "binding_cooldown"},
		{name: "资源类失效仍清绑定", directive: AffinityDirective{
			TombstoneProviderID: 9,
			TombstoneKind:       AffinityTombstoneResourceNotFound,
		}, want: "binding_clear"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}
			recorder := &sessionBindingRecorder{}
			pc := newSessionBindingContext(t, recorder, 83)
			pc.SetSessionBindingKeep("transient")

			settlement := okSettlement(nil)
			settlement.Affinity = testCase.directive
			if _, err := New(writer, Options{}).SettleContext(context.Background(), pc, settlement, nil); err != nil && !errors.Is(err, ErrNotSettled) {
				t.Fatalf("同步结算失败: %v", err)
			}
			events, _, _, _ := recorder.snapshot()
			if len(events) != 1 || events[0] != testCase.want {
				t.Fatalf("保留事实只压成功侧，失败侧应照发 %s，收到 %v", testCase.want, events)
			}
		})
	}
}

// TestSessionBindingWinnerFiresWhenKeepUnset 零值安全：没盖保留事实时照旧 CAS。
// 这条是接线前行为的护栏——无既有绑定（首绑）与结构性失效后的改绑都靠它。
func TestSessionBindingWinnerFiresWhenKeepUnset(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	recorder := &sessionBindingRecorder{}
	pc := newSessionBindingContext(t, recorder, 84)

	settlement := okSettlement(nil)
	settlement.Affinity = AffinityDirective{WinnerProviderID: 7}
	if _, err := New(writer, Options{}).SettleContext(context.Background(), pc, settlement, nil); err != nil && !errors.Is(err, ErrNotSettled) {
		t.Fatalf("同步结算失败: %v", err)
	}
	events, casIDs, _, _ := recorder.snapshot()
	if len(events) != 1 || events[0] != "binding_cas" || casIDs[0] != 7 {
		t.Fatalf("未盖保留事实时应照旧 CAS 到 7，收到 %v / %v", events, casIDs)
	}
}
