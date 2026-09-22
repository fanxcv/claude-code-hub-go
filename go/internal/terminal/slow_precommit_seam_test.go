package terminal

import (
	"context"
	"testing"
)

// 本文件钉「提交前判慢」旁路缝：装配时透传、未装配时整段跳过（与改道计数同一口径）。

type recordingSlowRateRecorder struct {
	samples   []SlowRateSample
	precommit []SlowPrecommit
}

func (r *recordingSlowRateRecorder) RecordSlowRate(_ context.Context, sample SlowRateSample) {
	r.samples = append(r.samples, sample)
}

func (r *recordingSlowRateRecorder) RecordSlowPrecommit(_ context.Context, facts []SlowPrecommit) {
	r.precommit = append(r.precommit, facts...)
}

func TestRecordSlowPrecommitPassesFactsToRecorder(t *testing.T) {
	recorder := &recordingSlowRateRecorder{}
	settler := &Settler{slowRate: recorder}

	settler.recordSlowPrecommit(context.Background(), []SlowPrecommit{
		{ProviderID: 167, ModelKey: "deepseek-v4.1-flash", RequestID: 4242, SessionID: "s"},
		{ProviderID: 121, ModelKey: "deepseek-v4.1-flash", RequestID: 4242, SessionID: "s"},
	})

	if len(recorder.precommit) != 2 {
		t.Fatalf("两条事实应原样透传给旁路，实际 %d 条", len(recorder.precommit))
	}
	if recorder.precommit[0].ProviderID != 167 || recorder.precommit[1].ProviderID != 121 {
		t.Fatalf("透传顺序应与入参一致，实际 %+v", recorder.precommit)
	}
}

// 未装配（nil）时整段跳过：与接线前逐字一致，且不得 panic。
func TestRecordSlowPrecommitIsNoopWhenUnwired(t *testing.T) {
	settler := &Settler{}
	settler.recordSlowPrecommit(context.Background(), []SlowPrecommit{{ProviderID: 167}})
}

// 空事实列表时不调用旁路（每次终态都会调本函数，不该有无谓的跨层调用）。
func TestRecordSlowPrecommitSkipsEmptyFacts(t *testing.T) {
	recorder := &recordingSlowRateRecorder{}
	settler := &Settler{slowRate: recorder}

	settler.recordSlowPrecommit(context.Background(), nil)

	if len(recorder.precommit) != 0 {
		t.Fatalf("空事实不该调用旁路，实际 %d 条", len(recorder.precommit))
	}
}
