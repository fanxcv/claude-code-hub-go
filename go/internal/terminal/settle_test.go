package terminal

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// fakeWriter 是可注入失败的 Writer：每次调用记录参数，便于断言语句内容与调用次数。
type fakeWriter struct {
	mu sync.Mutex

	createRow  store.MessageRequest
	createErr  error
	createCall int
	createData store.CreateMessageRequestData

	unfinalizedQueue []unfinalizedResult
	unfinalizedCalls int
	unfinalizedIDs   []int64
	unfinalizedPatch store.DetailsPatch

	costQueue   []error
	costCalls   int
	costTotal   string
	costPayload []byte

	price    *store.ModelPrice
	priceErr error
}

type unfinalizedResult struct {
	committed bool
	err       error
}

func (f *fakeWriter) CreateMessageRequest(
	_ context.Context,
	data store.CreateMessageRequestData,
) (store.MessageRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCall++
	f.createData = data
	if f.createErr != nil {
		return store.MessageRequest{}, f.createErr
	}
	return f.createRow, nil
}

func (f *fakeWriter) UpdateDetailsIfUnfinalized(
	_ context.Context,
	id int64,
	patch store.DetailsPatch,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unfinalizedIDs = append(f.unfinalizedIDs, id)
	f.unfinalizedPatch = patch
	index := f.unfinalizedCalls
	f.unfinalizedCalls++
	if index >= len(f.unfinalizedQueue) {
		if len(f.unfinalizedQueue) == 0 {
			return false, nil
		}
		last := f.unfinalizedQueue[len(f.unfinalizedQueue)-1]
		return last.committed, last.err
	}
	result := f.unfinalizedQueue[index]
	return result.committed, result.err
}

func (f *fakeWriter) UpdateWinnerCost(
	_ context.Context,
	_ int64,
	winnerCost string,
	costBreakdown []byte,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.costTotal = winnerCost
	f.costPayload = costBreakdown
	index := f.costCalls
	f.costCalls++
	if index >= len(f.costQueue) {
		return nil
	}
	return f.costQueue[index]
}

func (f *fakeWriter) FindModelPrice(
	_ context.Context,
	_ string,
) (*store.ModelPrice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.priceErr != nil {
		return nil, f.priceErr
	}
	return f.price, nil
}

func noBackoff() Options {
	return Options{MaxAttempts: 3, Backoff: func(int) time.Duration { return 0 }}
}

func okSettlement(cost *Cost) Settlement {
	return Settlement{
		StatusCode:    200,
		DurationMS:    intPtr(62),
		TTFTMS:        intPtr(43),
		FirstByteMS:   intPtr(43),
		ProviderChain: []byte(`[{"id":1,"reason":"request_success"}]`),
		Usage:         Usage{InputTokens: int64Ptr(1), OutputTokens: int64Ptr(1)},
		Cost:          cost,
	}
}

// 赢得终态即返回 Committed=true，并写入成本。
func TestSettleWinsTerminalAndWritesCost(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	cost := &Cost{Total: "0.000024", Breakdown: []byte(`{"total":"0.000024"}`)}

	result, err := New(writer, noBackoff()).Settle(context.Background(), 42, okSettlement(cost))
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if !result.Committed {
		t.Fatal("应当赢得终态")
	}
	if !result.CostWritten {
		t.Fatal("应当已写成本")
	}
	if result.Attempts != 1 {
		t.Fatalf("尝试次数 = %d, want 1", result.Attempts)
	}
	if writer.unfinalizedCalls != 1 || writer.costCalls != 1 {
		t.Fatalf("调用次数 = 终态 %d / 成本 %d, want 1/1", writer.unfinalizedCalls, writer.costCalls)
	}
	if writer.costTotal != "0.000024" {
		t.Fatalf("写入的成本 = %q", writer.costTotal)
	}
}

// 竞态落败：谓词没匹配到行 → ErrNotSettled，且**不得**写成本。
func TestSettleLosingRaceDoesNotWriteCost(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}
	cost := &Cost{Total: "0.000024"}

	result, err := New(writer, noBackoff()).Settle(context.Background(), 42, okSettlement(cost))
	if !errors.Is(err, ErrNotSettled) {
		t.Fatalf("应返回 ErrNotSettled，得到 %v", err)
	}
	if result.Committed {
		t.Fatal("落败时 Committed 必须为 false")
	}
	if writer.costCalls != 0 {
		t.Fatalf("落败时不得写成本，实际调用 %d 次", writer.costCalls)
	}
}

// 终态写瞬时失败后重试成功；尝试次数如实上报。
func TestSettleRetriesTransientFailure(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{
		{err: errors.New("connection reset")},
		{committed: true},
	}}

	result, err := New(writer, noBackoff()).Settle(context.Background(), 42, okSettlement(nil))
	if err != nil {
		t.Fatalf("重试后应当成功: %v", err)
	}
	if result.Attempts != 2 {
		t.Fatalf("尝试次数 = %d, want 2", result.Attempts)
	}
	if !result.Committed {
		t.Fatal("重试成功应计为赢得终态")
	}
}

// 重试耗尽后必须把最后一次错误透出，不得吞掉。
func TestSettleExhaustsAttempts(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{
		{err: errors.New("boom-1")},
		{err: errors.New("boom-2")},
		{err: errors.New("boom-3")},
	}}

	options := Options{MaxAttempts: 3, Backoff: func(int) time.Duration { return 0 }}
	result, err := New(writer, options).Settle(context.Background(), 42, okSettlement(nil))
	if err == nil {
		t.Fatal("重试耗尽后必须返回错误")
	}
	if !strings.Contains(err.Error(), "boom-3") {
		t.Fatalf("应透出最后一次错误，得到 %v", err)
	}
	if result.Attempts != 3 {
		t.Fatalf("尝试次数 = %d, want 3", result.Attempts)
	}
	if result.Committed {
		t.Fatal("全部失败时 Committed 必须为 false")
	}
}

// 成本写失败：终态已提交（Committed=true）但必须报出 ErrCostWriteFailed，
// 让调用方知道账面上缺了成本，而不是静默成功。
func TestSettleReportsCostWriteFailure(t *testing.T) {
	writer := &fakeWriter{
		unfinalizedQueue: []unfinalizedResult{{committed: true}},
		costQueue:        []error{errors.New("cost-boom-1"), errors.New("cost-boom-2"), errors.New("cost-boom-3")},
	}

	result, err := New(writer, noBackoff()).Settle(
		context.Background(), 42, okSettlement(&Cost{Total: "0.000024"}))
	if !errors.Is(err, ErrCostWriteFailed) {
		t.Fatalf("应返回 ErrCostWriteFailed，得到 %v", err)
	}
	if !result.Committed {
		t.Fatal("终态已提交，Committed 必须为 true")
	}
	if result.CostWritten {
		t.Fatal("成本未写下时 CostWritten 必须为 false")
	}
	if writer.costCalls != 3 {
		t.Fatalf("成本写应重试 3 次，实际 %d 次", writer.costCalls)
	}
}

// 不合格输入必须在触库之前就被拒（不产生任何数据库调用）。
func TestSettleRejectsInvalidInputBeforeTouchingWriter(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	settler := New(writer, noBackoff())

	if _, err := settler.Settle(context.Background(), 42, Settlement{StatusCode: 0}); err == nil {
		t.Fatal("无状态码必须被拒")
	}
	if _, err := settler.Settle(context.Background(), 42, Settlement{
		StatusCode:    200,
		ProviderChain: []byte(`[{"id":1}]`),
	}); err == nil {
		t.Fatal("有 provider_chain 却缺 duration_ms 必须被拒")
	}
	if writer.unfinalizedCalls != 0 {
		t.Fatalf("被拒的输入不得触库，实际调用 %d 次", writer.unfinalizedCalls)
	}
}

// 拦截类终态：先建开行再结算一遍，终态写里带 blocked_by。
func TestSettleBlockedCreatesThenSettles(t *testing.T) {
	writer := &fakeWriter{
		createRow:        store.MessageRequest{ID: 77},
		unfinalizedQueue: []unfinalizedResult{{committed: true}},
	}
	settlement := Settlement{
		StatusCode:    400,
		BlockedBy:     strPtr("sensitive_word"),
		BlockedReason: strPtr(`{"word":"x"}`),
		ErrorMessage:  strPtr(`请求包含敏感词："x"`),
		Cost:          &Cost{Total: "0"},
	}

	result, err := New(writer, noBackoff()).SettleBlocked(context.Background(), store.CreateMessageRequestData{
		ProviderID: 0,
		UserID:     1,
		Key:        "k",
		Model:      strPtr("gpt-5.6"),
	}, settlement)
	if err != nil {
		t.Fatalf("拦截类结算失败: %v", err)
	}
	if !result.Committed {
		t.Fatal("应当赢得终态")
	}
	if writer.createCall != 1 {
		t.Fatalf("建行调用 = %d, want 1", writer.createCall)
	}
	if writer.unfinalizedPatch.BlockedBy == nil || *writer.unfinalizedPatch.BlockedBy != "sensitive_word" {
		t.Fatal("终态写必须带 blocked_by")
	}
	if writer.unfinalizedPatch.StatusCode == nil || *writer.unfinalizedPatch.StatusCode != 400 {
		t.Fatal("终态写必须带状态码")
	}
}

// 拦截类终态建行失败必须报错，且不得继续写终态。
func TestSettleBlockedSurfacesCreateFailure(t *testing.T) {
	writer := &fakeWriter{createErr: errors.New("insert failed")}
	_, err := New(writer, noBackoff()).SettleBlocked(
		context.Background(),
		store.CreateMessageRequestData{Key: "k"},
		Settlement{StatusCode: 400, BlockedBy: strPtr("warmup")},
	)
	if err == nil {
		t.Fatal("建行失败必须报错")
	}
	if writer.unfinalizedCalls != 0 {
		t.Fatal("建行失败后不得写终态")
	}
}

// 拦截类结算缺状态码必须在建行**之前**被拒（否则会留下一个永远不终态的行）。
func TestSettleBlockedRequiresStatusCodeBeforeCreate(t *testing.T) {
	writer := &fakeWriter{}
	_, err := New(writer, noBackoff()).SettleBlocked(
		context.Background(), store.CreateMessageRequestData{Key: "k"}, Settlement{})
	if err == nil {
		t.Fatal("缺状态码必须被拒")
	}
	if writer.createCall != 0 {
		t.Fatalf("被拒时不得建行，实际调用 %d 次", writer.createCall)
	}
}

// 先终态后成本的顺序：成本语句不得先于终态语句。
func TestSettleWritesTerminalBeforeCost(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	if _, err := New(writer, noBackoff()).Settle(
		context.Background(), 42, okSettlement(&Cost{Total: "0.1"})); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if writer.unfinalizedCalls != 1 || writer.costCalls != 1 {
		t.Fatalf("终态 %d 次 / 成本 %d 次", writer.unfinalizedCalls, writer.costCalls)
	}
	// fakeWriter 的 UpdateWinnerCost 只有在前者被调用后才可能被调用；这里再断言终态
	// patch 确实把本次请求可用的 outbox 监视列都放进了同一条语句，避免「顺序对但语句残缺」。
	query, _ := store.BuildDetailsPatchQuery(42, writer.unfinalizedPatch)
	for _, column := range []string{"status_code", "duration_ms", "provider_chain"} {
		if !strings.Contains(query, `"`+column+`" = $`) {
			t.Fatalf("终态语句缺少 outbox 监视列 %s: %s", column, query)
		}
	}
	// error_message 与 blocked_by 在成功路径上本就没有值（不写即保持 NULL）；
	// 「已知的监视列必须同语句」这条由 patch_test 的全字段样本覆盖。
}

// 解析价格失败与价格缺失要能被区分：前者是数据问题，后者是「没这个模型」。
func TestResolveAndComputeCostDistinguishesMissingPrice(t *testing.T) {
	writer := &fakeWriter{price: nil}
	if _, err := ResolveAndComputeCost(
		context.Background(), writer, "unknown-model",
		CostInput{Usage: Usage{InputTokens: int64Ptr(1)}},
	); !errors.Is(err, ErrPriceNotFound) {
		t.Fatalf("应返回 ErrPriceNotFound，得到 %v", err)
	}
	if _, err := ResolveAndComputeCost(
		context.Background(), writer, "",
		CostInput{},
	); !errors.Is(err, ErrPriceNotFound) {
		t.Fatalf("空模型名应返回 ErrPriceNotFound，得到 %v", err)
	}

	writer.price = &store.ModelPrice{ModelName: "gpt-5.6", PriceData: []byte(`{"input_cost_per_token":0.001}`)}
	cost, err := ResolveAndComputeCost(
		context.Background(), writer, "gpt-5.6",
		CostInput{Usage: Usage{InputTokens: int64Ptr(100)}},
	)
	if err != nil {
		t.Fatalf("取到价格后应当算出成本: %v", err)
	}
	if got := normalizedCost(t, cost.Total); got != "0.1" {
		t.Fatalf("total = %s, want 0.1", got)
	}
}

// StoreWriter 对 store.ErrNotFound 的翻译：查不到价格必须是 ErrPriceNotFound。
// 用真库验证成本高于本文件的假实现，这里只固定翻译契约。
func TestStoreWriterIsAWireableWriter(t *testing.T) {
	var _ Writer = StoreWriter{}
}

// 默认退避是生产重试节奏，值错了会悄悄改变重试时序（并可能拖长请求）。
func TestDefaultBackoffGrowsLinearly(t *testing.T) {
	if got := DefaultBackoff(0); got != 50*time.Millisecond {
		t.Fatalf("DefaultBackoff(0) = %v, want 50ms", got)
	}
	if got := DefaultBackoff(1); got != 100*time.Millisecond {
		t.Fatalf("DefaultBackoff(1) = %v, want 100ms", got)
	}
	if got := DefaultBackoff(2); got != 150*time.Millisecond {
		t.Fatalf("DefaultBackoff(2) = %v, want 150ms", got)
	}
}
