package terminal

import (
	"context"
	"testing"
)

// 本文件钉住会话绑定终态写回的三路分流（设计稿 §4 的失效规则表）：
// 成功写 CAS / 供应商故障写冷却 / 资源类失效只清绑定。
//
// 跨包接缝另有一条钉子：`dataplane/session_binding_invalidation_test.go` 把**真实的**
// 指令产出喂给**真实的**写回。本文件只管「拿到某个种类就调哪个方法」这一半。

// stubSessionBinding 只记录被调用了哪个方法，不做任何 IO。
type stubSessionBinding struct {
	clearCount int
	cooldowns  []int64
	winners    []int64
}

func (s *stubSessionBinding) CompareAndSet(_ context.Context, providerID int64) bool {
	s.winners = append(s.winners, providerID)
	return true
}

func (s *stubSessionBinding) CooldownOnFailure(_ context.Context, providerID int64) bool {
	s.cooldowns = append(s.cooldowns, providerID)
	return true
}

func (s *stubSessionBinding) ClearBinding(_ context.Context) bool {
	s.clearCount++
	return true
}

func TestSessionBindingWritebackSplitsByTombstoneKind(t *testing.T) {
	cases := []struct {
		name         string
		directive    AffinityDirective
		committed    bool
		wantClear    int
		wantCooldown []int64
		wantWinner   []int64
	}{
		{
			name:      "资源类墓碑只清绑定、不写冷却",
			directive: AffinityDirective{TombstoneProviderID: 9, TombstoneKind: AffinityTombstoneResourceNotFound},
			wantClear: 1,
		},
		{
			name:         "故障墓碑写冷却、不清绑定",
			directive:    AffinityDirective{TombstoneProviderID: 9},
			wantCooldown: []int64{9},
		},
		{
			name:       "成功且已提交写 CAS",
			directive:  AffinityDirective{WinnerProviderID: 7},
			committed:  true,
			wantWinner: []int64{7},
		},
		{
			name:      "成功但未赢得终态提交时不写 CAS",
			directive: AffinityDirective{WinnerProviderID: 7},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := newTestContext(t)
			recorder := &stubSessionBinding{}
			pc.SetSessionBindingWriteback(recorder)

			// 同步路径对同一条指令发两次：失败半 + 成功半（与 affinityWriteback 同形）。
			// 两半走同一个分派器，故这里的断言仍然覆盖三种结局的分流。
			settler := New(&fakeWriter{}, Options{})
			settler.sessionBindingWriteback(
				context.Background(), pc, tc.directive, sessionBindingFailure, tc.committed,
			)
			settler.sessionBindingWriteback(
				context.Background(), pc, tc.directive, sessionBindingWinner, tc.committed,
			)

			if recorder.clearCount != tc.wantClear {
				t.Errorf("ClearBinding 调用次数 = %d，期望 %d", recorder.clearCount, tc.wantClear)
			}
			if len(recorder.cooldowns) != len(tc.wantCooldown) || len(recorder.winners) != len(tc.wantWinner) {
				t.Fatalf("冷却 = %v（期望 %v）、CAS = %v（期望 %v）",
					recorder.cooldowns, tc.wantCooldown, recorder.winners, tc.wantWinner)
			}
			for i := range tc.wantCooldown {
				if recorder.cooldowns[i] != tc.wantCooldown[i] {
					t.Errorf("冷却的供应商 = %v，期望 %v", recorder.cooldowns, tc.wantCooldown)
				}
			}
			for i := range tc.wantWinner {
				if recorder.winners[i] != tc.wantWinner[i] {
					t.Errorf("CAS 的供应商 = %v，期望 %v", recorder.winners, tc.wantWinner)
				}
			}
		})
	}
}
