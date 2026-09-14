package terminal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住 public-status 旁路的三条约束（见 rollup.go 文件头）：
//  1. 失败不得影响结算——**反证**：让行事实读取报错，结算仍须成功且只多一条 warn；
//  2. 时机：旁路只在终态提交（且成本写）之后发生——用一条共享的调用序断言；
//  3. 幂等：未赢得终态的结算不写旁路（重复结算会拿到 committed=false）。

// rollupTestWriter 在既有 fakeWriter 之上补「行事实读取面」，并记录调用序。
//
// 顺序用共享 slice 记而不是各记各的时间戳：两条语句在同一毫秒内完成时时间戳无法定序，
// 而「谁先谁后」正是这里要钉住的东西。
type rollupTestWriter struct {
	*fakeWriter
	sequence   *[]string
	facts      *store.RollupFacts
	factsErr   error
	factsCalls int
	factsID    int64
}

func (w *rollupTestWriter) FindRollupFacts(_ context.Context, id int64) (*store.RollupFacts, error) {
	w.factsCalls++
	w.factsID = id
	*w.sequence = append(*w.sequence, "facts")
	if w.factsErr != nil {
		return nil, w.factsErr
	}
	return w.facts, nil
}

// commitRecordingOrderWriter 把「终态提交」与「成本写入」也记进同一条调用序。
type commitRecordingOrderWriter struct {
	*rollupTestWriter
}

func (w *commitRecordingOrderWriter) UpdateDetailsIfUnfinalized(
	ctx context.Context,
	id int64,
	patch store.DetailsPatch,
) (bool, error) {
	*w.sequence = append(*w.sequence, "terminal")
	return w.rollupTestWriter.UpdateDetailsIfUnfinalized(ctx, id, patch)
}

func (w *commitRecordingOrderWriter) UpdateWinnerCost(
	ctx context.Context,
	id int64,
	winnerCost string,
	costBreakdown []byte,
) error {
	*w.sequence = append(*w.sequence, "cost")
	return w.rollupTestWriter.UpdateWinnerCost(ctx, id, winnerCost, costBreakdown)
}

// recordingRecorder 记录收到的事件（并把它自己记进调用序）。
type recordingRecorder struct {
	sequence *[]string
	events   []pubstatus.RollupEvent
}

func (r *recordingRecorder) RecordTerminal(_ context.Context, event pubstatus.RollupEvent) {
	*r.sequence = append(*r.sequence, "rollup")
	r.events = append(r.events, event)
}

// recordingLogger 收集 warn 事件名。
type recordingLogger struct{ events []string }

func (l *recordingLogger) Warn(event string, _ map[string]any) { l.events = append(l.events, event) }

func newRollupSettleFixture(committed bool, withCost bool) (
	*commitRecordingOrderWriter, *recordingRecorder, *recordingLogger, *[]string,
) {
	inner := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: committed}}}
	sequence := &[]string{}
	writer := &commitRecordingOrderWriter{rollupTestWriter: &rollupTestWriter{
		fakeWriter: inner,
		sequence:   sequence,
		facts: &store.RollupFacts{
			CreatedAt:     time.Date(2026, 9, 12, 23, 56, 0, 0, time.UTC),
			Model:         ptr("claude-sonnet-4"),
			OriginalModel: ptr("claude-sonnet-4-orig"),
			DurationMS:    ptrInt(4100),
		},
	}}
	recorder := &recordingRecorder{sequence: sequence}
	logger := &recordingLogger{}
	if !withCost {
		return writer, recorder, logger, sequence
	}
	return writer, recorder, logger, sequence
}

func ptr(value string) *string { return &value }
func ptrInt(value int) *int    { return &value }

// TestSettleRollupRunsAfterTerminalAndCost 钉住调用序：终态 → 成本 → 行事实 → 旁路。
func TestSettleRollupRunsAfterTerminalAndCost(t *testing.T) {
	writer, recorder, logger, sequence := newRollupSettleFixture(true, true)
	settler := New(writer, Options{Rollup: recorder, Logger: logger, Backoff: func(int) time.Duration { return 0 }})

	total := "0.25"
	result, err := settler.Settle(context.Background(), 42, Settlement{
		StatusCode:          200,
		DurationMS:          ptrInt(4000),
		TTFTMS:              ptrInt(700),
		FirstByteMS:         ptrInt(400),
		Usage:               Usage{OutputTokens: int64Ptr(800)},
		ProviderChain:       []byte(`[{"reason":"request_success","groupTag":"default"}]`),
		Cost:                &Cost{Total: total},
		ActualResponseModel: ptr("claude-sonnet-4-actual"),
	})
	if err != nil {
		t.Fatalf("结算不应失败: %v", err)
	}
	if !result.Committed || !result.CostWritten {
		t.Fatalf("应提交终态并写入成本：%+v", result)
	}

	want := []string{"terminal", "cost", "facts", "rollup"}
	if len(*sequence) != len(want) {
		t.Fatalf("调用序不符：got %v want %v", *sequence, want)
	}
	for index := range want {
		if (*sequence)[index] != want[index] {
			t.Fatalf("调用序第 %d 步应为 %s：got %v", index, want[index], *sequence)
		}
	}

	if len(recorder.events) != 1 {
		t.Fatalf("应恰好收到一个事件，实际 %d", len(recorder.events))
	}
	event := recorder.events[0]
	if !event.CreatedAt.Equal(time.Date(2026, 9, 12, 23, 56, 0, 0, time.UTC)) {
		t.Fatalf("事件时刻应取行创建时刻，实际 %v", event.CreatedAt)
	}
	if event.Model == nil || *event.Model != "claude-sonnet-4-actual" {
		t.Fatalf("model 应取终态给的响应模型，实际 %v", event.Model)
	}
	if event.OriginalModel == nil || *event.OriginalModel != "claude-sonnet-4-orig" {
		t.Fatalf("originalModel 应取行上的原始模型列，实际 %v", event.OriginalModel)
	}
	if event.DurationMs == nil || *event.DurationMs != 4000 {
		t.Fatalf("时长应取结算载荷（优先于行上的 4100），实际 %v", event.DurationMs)
	}
	if event.TTFTMs == nil || *event.TTFTMs != 700 {
		t.Fatalf("ttftMs 应取结算载荷，实际 %v", event.TTFTMs)
	}
	if event.OutputTokens == nil || *event.OutputTokens != 800 {
		t.Fatalf("outputTokens 应取结算载荷，实际 %v", event.OutputTokens)
	}
	if len(event.ProviderChain) != 1 || event.ProviderChain[0].Reason == nil ||
		*event.ProviderChain[0].Reason != "request_success" {
		t.Fatalf("链应被解出，实际 %+v", event.ProviderChain)
	}
	if len(logger.events) != 0 {
		t.Fatalf("正常路径不该有 warn，实际 %v", logger.events)
	}
}

// TestSettleRollupFailureDoesNotAffectSettlement 是**反证**：人为让旁路报错 →
// 结算仍成功（终态已提交、成本已写）、返回值没有错误、只多一条 warn。
func TestSettleRollupFailureDoesNotAffectSettlement(t *testing.T) {
	writer, recorder, logger, _ := newRollupSettleFixture(true, true)
	writer.rollupTestWriter.factsErr = errors.New("boom: 行事实读不到")

	settler := New(writer, Options{Rollup: recorder, Logger: logger, Backoff: func(int) time.Duration { return 0 }})
	total := "0.25"
	result, err := settler.Settle(context.Background(), 7, Settlement{
		StatusCode: 200,
		Cost:       &Cost{Total: total},
	})
	if err != nil {
		t.Fatalf("旁路失败不得让结算报错，实际 %v", err)
	}
	if !result.Committed || !result.CostWritten {
		t.Fatalf("旁路失败不得影响终态与成本：%+v", result)
	}
	if len(recorder.events) != 0 {
		t.Fatalf("旁路失败时不该有事件，实际 %d", len(recorder.events))
	}
	if len(logger.events) != 1 || logger.events[0] != "terminal.rollup_facts_failed" {
		t.Fatalf("应恰好一条 rollup_facts_failed warn，实际 %v", logger.events)
	}
}

// TestSettleRollupSkippedWhenNotCommitted 钉住幂等：未赢得终态不读行、不发旁路。
//
// 这条同时是「重复计数」的闸：重复结算会拿到 committed=false，于是不会写第二次增量。
func TestSettleRollupSkippedWhenNotCommitted(t *testing.T) {
	writer, recorder, logger, sequence := newRollupSettleFixture(false, true)
	settler := New(writer, Options{Rollup: recorder, Logger: logger, Backoff: func(int) time.Duration { return 0 }})

	if _, err := settler.Settle(context.Background(), 9, Settlement{StatusCode: 200, Cost: &Cost{Total: "0.1"}}); !errors.Is(err, ErrNotSettled) {
		t.Fatalf("未赢得终态应返回 ErrNotSettled，实际 %v", err)
	}
	if writer.rollupTestWriter.factsCalls != 0 {
		t.Fatalf("未提交时不该回读行事实，实际 %d 次", writer.rollupTestWriter.factsCalls)
	}
	if len(recorder.events) != 0 {
		t.Fatalf("未提交时不该发旁路，实际 %d", len(recorder.events))
	}
	if len(*sequence) != 1 || (*sequence)[0] != "terminal" {
		t.Fatalf("调用序应只有终态一次，实际 %v", *sequence)
	}
}

// TestSettleWithoutRollupReadsNothing 钉住「未装配零成本」：Rollup 为 nil 时不产生任何回读。
func TestSettleWithoutRollupReadsNothing(t *testing.T) {
	writer, _, logger, sequence := newRollupSettleFixture(true, true)
	settler := New(writer, Options{Logger: logger, Backoff: func(int) time.Duration { return 0 }})

	if _, err := settler.Settle(context.Background(), 11, Settlement{StatusCode: 200, Cost: &Cost{Total: "0.1"}}); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if writer.rollupTestWriter.factsCalls != 0 {
		t.Fatalf("未装配旁路时不该回读行事实，实际 %d 次", writer.rollupTestWriter.factsCalls)
	}
	for _, step := range *sequence {
		if step == "facts" {
			t.Fatalf("未装配旁路时调用序里不该有 facts：%v", *sequence)
		}
	}
}

// TestNormalizeRollupReasonMapsGoVocabulary 钉住词汇归一（尤其是「成功」那一处：
// 不归一的话可用率会恒 0）。
func TestNormalizeRollupReasonMapsGoVocabulary(t *testing.T) {
	cases := map[string]string{
		"success":                    "request_success",
		"non_retryable_client_error": "client_error_non_retryable",
		"local_overload":             "concurrent_limit_failed",
		"request_success":            "request_success",
		"hedge_winner":               "hedge_winner",
		"client_abort":               "client_abort",
		"whatever_unknown":           "whatever_unknown",
	}
	for input, want := range cases {
		if got := normalizeRollupReason(input); got != want {
			t.Fatalf("归一 %q 应为 %q，实际 %q", input, want, got)
		}
	}

	// 端到端形态：Go 写出的成功链（reason=success、无 statusCode）必须被分类成成功。
	raw := []byte(`[{"reason":"success","groupTag":"default"}]`)
	items := decodeRollupChain(raw, nil)
	if len(items) != 1 || items[0].Reason == nil || *items[0].Reason != "request_success" {
		t.Fatalf("链解码后 reason 应已归一，实际 %+v", items)
	}
	taxonomy, ok := pubstatus.ClassifyProviderChainItemOutcome(items[0])
	if !ok || taxonomy.Outcome != pubstatus.OutcomeSuccess {
		t.Fatalf("Go 的成功链必须被判成 success，实际 ok=%v taxonomy=%+v", ok, taxonomy)
	}
}

// TestDecodeRollupChainErrorFallbackOnlyForLastItem 钉住链解码的兜底口径。
func TestDecodeRollupChainErrorFallbackOnlyForLastItem(t *testing.T) {
	message := "no available providers"
	raw := []byte(`[{"reason":"system_error","groupTag":"a"},{"reason":"retry_failed","groupTag":"b"}]`)
	items := decodeRollupChain(raw, &message)
	if len(items) != 2 {
		t.Fatalf("应解出 2 项，实际 %d", len(items))
	}
	if items[0].ErrorMessage != nil {
		t.Fatalf("前面的尝试不该吃结算文案，实际 %v", *items[0].ErrorMessage)
	}
	if items[1].ErrorMessage == nil || *items[1].ErrorMessage != message {
		t.Fatalf("最后一项应吃结算文案兜底，实际 %v", items[1].ErrorMessage)
	}
	if got := len(decodeRollupChain(nil, &message)); got != 0 {
		t.Fatalf("空链应解出 0 项，实际 %d", got)
	}
	if got := len(decodeRollupChain([]byte("{not json"), &message)); got != 0 {
		t.Fatalf("坏链应静默解出 0 项（不得 panic），实际 %d", got)
	}
}
