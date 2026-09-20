package terminal

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// recordingTracer 记录收到的上报事实（线程安全：结算可被并发调用）。
type recordingTracer struct {
	mu      sync.Mutex
	records []TraceRecord
}

func (r *recordingTracer) RecordTerminal(record TraceRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
}

func (r *recordingTracer) recorded() []TraceRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TraceRecord(nil), r.records...)
}

// traceTestContext 造一个已开行、已鉴权的请求上下文。
func traceTestContext(t *testing.T, id int64) *pctx.Context {
	t.Helper()
	started := time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC)
	ctx, err := pctx.New(pctx.Init{
		Method: "POST",
		Path:   "/v1/messages",
		Now:    func() time.Time { return started },
	})
	if err != nil {
		t.Fatalf("建上下文失败: %v", err)
	}
	ctx.SetAuth(pctx.AuthState{UserID: 7, KeyID: 3})
	if err := ctx.SetMessageRequestID(id); err != nil {
		t.Fatalf("写行标识失败: %v", err)
	}
	return ctx
}

// traceTestContextWithoutRow 造一个已鉴权、但**没有行标识**的请求上下文。
//
// 这是拦截类路径的形态：行在结算内部建出，pc 里永远不会有 id。
func traceTestContextWithoutRow(t *testing.T) *pctx.Context {
	t.Helper()
	ctx, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("建上下文失败: %v", err)
	}
	ctx.SetAuth(pctx.AuthState{UserID: 7, KeyID: 3})
	return ctx
}

// 终态提交后必须上报一次，且字段逐条来自结算与上下文。
func TestSettleReportsCommittedTerminal(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	tracer := &recordingTracer{}
	settler := New(writer, Options{Tracer: tracer})
	pc := traceTestContext(t, 4242)

	model := "claude-sonnet-4"
	result, err := settler.SettleContext(context.Background(), pc, Settlement{
		StatusCode: 200,
		DurationMS: intPtr(1500),
		Model:      &model,
		Cost:       &Cost{Total: "0.012345"},
		Usage:      Usage{InputTokens: int64Ptr(11)},
	}, nil)
	if err != nil {
		t.Fatalf("结算不应报错: %v", err)
	}
	if !result.Committed {
		t.Fatalf("本次应赢得终态")
	}

	records := tracer.recorded()
	if len(records) != 1 {
		t.Fatalf("应上报恰好一条事实，得到 %d", len(records))
	}
	record := records[0]
	if record.ID != 4242 || record.UserID != 7 {
		t.Fatalf("行 id 与用户 id 不符: %+v", record)
	}
	if record.Name != "POST /v1/messages" {
		t.Fatalf("展示名应为「方法 路径」: %q", record.Name)
	}
	if record.StatusCode != 200 || record.Model != model {
		t.Fatalf("状态码或模型不符: %+v", record)
	}
	if !record.HasCost || record.CostUSD != "0.012345" {
		t.Fatalf("成本应原样上报十进制串: %+v", record)
	}
	if record.EndedAt.Sub(record.StartedAt) != 1500*time.Millisecond {
		t.Fatalf("结束时刻应由耗时推出: %+v", record)
	}
	if record.Usage.InputTokens == nil || *record.Usage.InputTokens != 11 {
		t.Fatalf("用量应随事实带上: %+v", record)
	}
}

// 未赢得终态不上报：重复结算不该重复上报（闸门与亲和写回一致）。
func TestSettleDoesNotReportWhenNotCommitted(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}
	tracer := &recordingTracer{}
	settler := New(writer, Options{Tracer: tracer})

	_, err := settler.SettleContext(context.Background(), traceTestContext(t, 5), Settlement{StatusCode: 200}, nil)
	if err == nil {
		t.Fatalf("未赢得终态应返回 ErrNotSettled")
	}
	if records := tracer.recorded(); len(records) != 0 {
		t.Fatalf("未赢得终态不得上报: %+v", records)
	}
}

// 拦截类终态也要上报：行已在库里（行 id 由**建行结果**给出，pc 里还没有），
// 而它同样是一条真实服务过的请求。
//
// 这是敏感词拦截与 warmup 抢答的形态：pc 无行标识 + 带开行载荷。
func TestSettleBlockedReportsCreatedRow(t *testing.T) {
	writer := &fakeWriter{
		createRow:        store.MessageRequest{ID: 77},
		unfinalizedQueue: []unfinalizedResult{{committed: true}},
	}
	tracer := &recordingTracer{}
	settler := New(writer, Options{Tracer: tracer})

	ctx, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("建上下文失败: %v", err)
	}
	ctx.SetAuth(pctx.AuthState{UserID: 7, KeyID: 3})
	create := store.CreateMessageRequestData{}
	blockedBy := "sensitive_word"
	if _, err := settler.SettleContext(context.Background(), ctx, Settlement{
		StatusCode: 403,
		BlockedBy:  &blockedBy,
	}, &create); err != nil {
		t.Fatalf("结算不应报错: %v", err)
	}

	records := tracer.recorded()
	if len(records) != 1 {
		t.Fatalf("拦截类终态应上报恰好一条，得到 %d", len(records))
	}
	if records[0].ID != 77 || records[0].StatusCode != 403 {
		t.Fatalf("行标识应取建行结果、状态码应取拦截码: %+v", records[0])
	}
	if records[0].UserID != 7 || records[0].Name != "POST /v1/messages" {
		t.Fatalf("请求属性应来自上下文: %+v", records[0])
	}
}

// 直接走 SettleBlocked（敏感词与 warmup 适配器的入口）同样上报。
func TestSettleBlockedDirectEntryReports(t *testing.T) {
	writer := &fakeWriter{
		createRow:        store.MessageRequest{ID: 91},
		unfinalizedQueue: []unfinalizedResult{{committed: true}},
	}
	tracer := &recordingTracer{}
	settler := New(writer, Options{Tracer: tracer})

	blockedBy := "warmup"
	result, err := settler.SettleBlocked(
		context.Background(),
		traceTestContextWithoutRow(t),
		store.CreateMessageRequestData{},
		Settlement{StatusCode: 200, BlockedBy: &blockedBy},
	)
	if err != nil {
		t.Fatalf("拦截结算不应报错: %v", err)
	}
	if !result.Committed {
		t.Fatalf("应赢得终态: %+v", result)
	}
	records := tracer.recorded()
	if len(records) != 1 || records[0].ID != 91 {
		t.Fatalf("应上报建行结果那一行: %+v", records)
	}
}

// 没有请求上下文（pc 为 nil）时不上报：上报要的是方法/路径/用户，缺了就发不出一条
// 对得上任何请求的 trace。结算本身照常。
func TestSettleBlockedWithoutContextDoesNotReport(t *testing.T) {
	writer := &fakeWriter{
		createRow:        store.MessageRequest{ID: 5},
		unfinalizedQueue: []unfinalizedResult{{committed: true}},
	}
	tracer := &recordingTracer{}
	settler := New(writer, Options{Tracer: tracer})

	blockedBy := "warmup"
	result, err := settler.SettleBlocked(
		context.Background(),
		nil,
		store.CreateMessageRequestData{},
		Settlement{StatusCode: 200, BlockedBy: &blockedBy},
	)
	if err != nil {
		t.Fatalf("拦截结算不应报错: %v", err)
	}
	if !result.Committed {
		t.Fatalf("无上下文不影响结算: %+v", result)
	}
	if records := tracer.recorded(); len(records) != 0 {
		t.Fatalf("无上下文不得上报: %+v", records)
	}
}

// 成本写失败不得报出金额：终态已提交但钱没进账本，报成「有成本」会让观测面与账务面不符。
func TestTraceCostNotReportedWhenCostWriteFailed(t *testing.T) {
	writer := &fakeWriter{
		unfinalizedQueue: []unfinalizedResult{{committed: true}},
		costQueue:        []error{errors.New("writer lane down")},
	}
	tracer := &recordingTracer{}
	settler := New(writer, Options{
		MaxAttempts: 1,
		Backoff:     func(int) time.Duration { return 0 },
		Tracer:      tracer,
	})

	_, err := settler.SettleContext(context.Background(), traceTestContext(t, 17), Settlement{
		StatusCode: 200,
		Cost:       &Cost{Total: "0.012345"},
	}, nil)
	if !errors.Is(err, ErrCostWriteFailed) {
		t.Fatalf("应报 ErrCostWriteFailed，收到 %v", err)
	}
	records := tracer.recorded()
	if len(records) != 1 {
		t.Fatalf("终态已提交，事实仍应上报一条: %+v", records)
	}
	if records[0].HasCost || records[0].CostUSD != "" {
		t.Fatalf("成本未落库时不得上报成本: %+v", records[0])
	}
}

// blocked_by 按上限截断（声明了截断就得真的截断）。
func TestTraceBlockedByIsTruncated(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	tracer := &recordingTracer{}
	settler := New(writer, Options{Tracer: tracer})

	long := strings.Repeat("b", maxTraceBlockedByBytes*3)
	if _, err := settler.SettleContext(context.Background(), traceTestContext(t, 23), Settlement{
		StatusCode: 403,
		BlockedBy:  &long,
	}, nil); err != nil {
		t.Fatalf("结算不应报错: %v", err)
	}
	records := tracer.recorded()
	if len(records) != 1 {
		t.Fatalf("应上报一条事实: %+v", records)
	}
	if len(records[0].BlockedBy) > maxTraceBlockedByBytes+len("(truncated)") {
		t.Fatalf("blocked_by 未截断: %d 字节", len(records[0].BlockedBy))
	}
}

// 异步模式下上报在 flush 之后发、且只发一次（同步与异步共用 settleNow 那一处收口）。
func TestAsyncSettleReportsOnceAfterFlush(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	queue := NewWriteQueue(AsyncOptions{MaxPending: 8, BatchSize: 1, FlushInterval: time.Hour})
	defer queue.Stop()
	tracer := &recordingTracer{}
	settler := New(writer, Options{
		MaxAttempts: 1,
		Backoff:     func(int) time.Duration { return 0 },
		Tracer:      tracer,
		Queue:       queue,
	})

	pc := traceTestContext(t, 33)
	result, err := settler.SettleContext(context.Background(), pc, Settlement{
		StatusCode: 200,
		Cost:       &Cost{Total: "0.01"},
	}, nil)
	if err != nil || !result.Queued {
		t.Fatalf("异步模式应先入队: %+v err=%v", result, err)
	}
	if records := tracer.recorded(); len(records) != 0 {
		t.Fatalf("入队即上报是错的（提交结论还没出）: %+v", records)
	}
	if err := queue.Flush(context.Background()); err != nil {
		t.Fatalf("flush 不应报错: %v", err)
	}
	records := tracer.recorded()
	if len(records) != 1 {
		t.Fatalf("flush 后应恰好上报一条，得到 %d", len(records))
	}
	if records[0].ID != 33 || !records[0].HasCost {
		t.Fatalf("上报内容不符: %+v", records[0])
	}
}

// 未装配（nil）即无副作用：这是「未配置 Langfuse key」时的生产形态。
func TestSettleWithoutTracerHasNoSideEffect(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	settler := New(writer, Options{})

	result, err := settler.SettleContext(context.Background(), traceTestContext(t, 5), Settlement{StatusCode: 200}, nil)
	if err != nil {
		t.Fatalf("结算不应报错: %v", err)
	}
	if !result.Committed {
		t.Fatalf("未装配不应改变结算结果: %+v", result)
	}
}

// 错误文本按上限截断：它是唯一可能带内容的字段（上游回显可能很长）。
func TestTraceErrorTextIsTruncated(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	tracer := &recordingTracer{}
	settler := New(writer, Options{Tracer: tracer})

	long := strings.Repeat("x", maxTraceErrorBytes*3)
	_, err := settler.SettleContext(context.Background(), traceTestContext(t, 9), Settlement{
		StatusCode:    502,
		DurationMS:    intPtr(10),
		ProviderChain: []byte(`[]`),
		ErrorMessage:  &long,
	}, nil)
	if err != nil {
		t.Fatalf("结算不应报错: %v", err)
	}
	records := tracer.recorded()
	if len(records) != 1 {
		t.Fatalf("应上报一条事实: %+v", records)
	}
	if len(records[0].ErrorMessage) > maxTraceErrorBytes+len("(truncated)") {
		t.Fatalf("错误文本未截断: %d 字节", len(records[0].ErrorMessage))
	}
	if !strings.HasSuffix(records[0].ErrorMessage, "(truncated)") {
		t.Fatalf("截断应显式标注: %q", records[0].ErrorMessage)
	}
}

// 截断不得切碎多字节字符（中文错误文本是最常见的情形）。
func TestTruncateTextKeepsRunesIntact(t *testing.T) {
	text := strings.Repeat("错", maxTraceErrorBytes)
	truncated := truncateText(text, maxTraceErrorBytes)
	if !strings.HasSuffix(truncated, "(truncated)") {
		t.Fatalf("超长文本应被标注截断: %q", truncated[len(truncated)-16:])
	}
	body := strings.TrimSuffix(truncated, "(truncated)")
	for _, r := range body {
		if r == '\uFFFD' {
			t.Fatalf("截断产生了非法字节序列（半个字符）")
		}
	}
}
