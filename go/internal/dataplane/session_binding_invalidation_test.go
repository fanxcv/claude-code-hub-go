package dataplane

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件钉住「上游 404 不该写会话冷却」这条失效规则**跨包**的那一段。
//
// 为什么必须有它：这条链的两半各自都有单测——dataplane 的 tombstoneDirective 有表驱动钉子
// （产出的指令种类正确），terminal 的 sessionBindingWriteback 也有自己的钉子（拿到什么种类
// 就做什么动作）。但「404 产出的种类」与「该种类落到哪个写回方法」之间的接缝谁都盖不住：
// 把 TombstoneKind 那一行摘掉时，两半的单测**全绿**，而资源类失效会重新去写冷却——
// 正是这条 P1 的现场（模型不支持被记成「慢」，等它补上模型还要白背一段冷却）。
//
// 故本用例不再分别测两半，而是把**真实的**指令产出喂给**真实的**终态写回。

// recordingSessionBinding 记录会话绑定写回实际调用了哪个方法（不碰 Redis）。
type recordingSessionBinding struct {
	clearCount int
	cooldowns  []int64
	winners    []int64
}

func (r *recordingSessionBinding) CompareAndSet(_ context.Context, providerID int64) bool {
	r.winners = append(r.winners, providerID)
	return true
}

func (r *recordingSessionBinding) CooldownOnFailure(_ context.Context, providerID int64) bool {
	r.cooldowns = append(r.cooldowns, providerID)
	return true
}

func (r *recordingSessionBinding) ClearBinding(_ context.Context) bool {
	r.clearCount++
	return true
}

// alwaysCommitWriter 让终态写恒成功：成功写回只在提交后发放，本用例要的是能走到写回。
type alwaysCommitWriter struct{}

func (alwaysCommitWriter) CreateMessageRequest(
	_ context.Context, _ store.CreateMessageRequestData,
) (store.MessageRequest, error) {
	return store.MessageRequest{}, nil
}

func (alwaysCommitWriter) UpdateDetailsIfUnfinalized(
	_ context.Context, _ int64, _ store.DetailsPatch,
) (bool, error) {
	return true, nil
}

func (alwaysCommitWriter) UpdateWinnerCost(_ context.Context, _ int64, _ string, _ []byte) error {
	return nil
}

func (alwaysCommitWriter) FindModelPrice(_ context.Context, _ string) (*store.ModelPrice, error) {
	return nil, nil
}

func TestSessionBindingInvalidationFollowsFailureKind(t *testing.T) {
	cases := []struct {
		name         string
		failure      *forward.Failure
		wantClear    int
		wantCooldown []int64
	}{
		{
			name:      "上游 404：只清绑定、不写冷却",
			failure:   &forward.Failure{Category: forward.CategoryResourceNotFound, ProviderID: 9, StatusCode: 404},
			wantClear: 1,
		},
		{
			name:         "供应商故障：写冷却、不清绑定",
			failure:      &forward.Failure{Category: forward.CategoryProviderError, ProviderID: 9, StatusCode: 502},
			wantCooldown: []int64{9},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
			if err != nil {
				t.Fatalf("构造上下文失败: %v", err)
			}
			if err := pc.SetMessageRequestID(77); err != nil {
				t.Fatalf("写入行标识失败: %v", err)
			}
			recorder := &recordingSessionBinding{}
			pc.SetSessionBindingWriteback(recorder)

			directive := affinityDirectiveForNonStream(nil, tc.failure)
			if _, err := terminal.New(alwaysCommitWriter{}, terminal.Options{}).SettleContext(
				context.Background(), pc,
				terminal.Settlement{StatusCode: tc.failure.StatusCode, Affinity: directive},
				nil,
			); err != nil {
				t.Fatalf("终态结算失败: %v", err)
			}

			if recorder.clearCount != tc.wantClear {
				t.Errorf("ClearBinding 调用次数 = %d，期望 %d", recorder.clearCount, tc.wantClear)
			}
			if len(recorder.cooldowns) != len(tc.wantCooldown) {
				t.Fatalf("CooldownOnFailure 调用 = %v，期望 %v", recorder.cooldowns, tc.wantCooldown)
			}
			for i := range tc.wantCooldown {
				if recorder.cooldowns[i] != tc.wantCooldown[i] {
					t.Errorf("冷却的供应商 = %v，期望 %v", recorder.cooldowns, tc.wantCooldown)
				}
			}
			if len(recorder.winners) != 0 {
				t.Errorf("墓碑路径不该有 CompareAndSet，实得 %v", recorder.winners)
			}
		})
	}
}
