package dataplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件钉住竞速输家成本的**租约侧**：每条输家成本入库后按自己的增量与标记结算一次。
//
// 为什么不能只靠胜者那一笔：store.UpdateWinnerCost 只把 winner 自己的成本写进 cost_usd
// （已入库的输家由另一条子查询求和加入），而输家引流是后台 fire-and-forget——两种先后顺序
// 都会出现。若只按胜者成本结算，租约在刷新窗口内比实际更宽，且请求级标记写死后再无补扣机会。

// loserBillOrder 记录两个桩的**调用顺序**。
//
// 为什么需要它：只数「两边都被调用过」无法区分先后——把实现里的写库与结算对调，
// 计数断言照样通过，而那种实现会在写库失败时已经扣了租约（账本没这笔、租约少一片）。
type loserBillOrder struct {
	mu     sync.Mutex
	events []string
}

func (o *loserBillOrder) record(event string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func (o *loserBillOrder) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

// recordingLoserLeaseSettler 记录收到的结算调用。
type recordingLoserLeaseSettler struct {
	mu    sync.Mutex
	calls []loserLeaseCall
	// order 非 nil 时同步记录调用顺序（见 loserBillOrder）。
	order *loserBillOrder
}

type loserLeaseCall struct {
	marker string
	cost   string
	plan   pctx.LeaseSettlementPlan
}

var _ terminal.LeaseSettler = (*recordingLoserLeaseSettler)(nil)

func (r *recordingLoserLeaseSettler) SettleLeases(
	_ context.Context,
	markerID string,
	costText string,
	plan pctx.LeaseSettlementPlan,
) {
	r.mu.Lock()
	r.calls = append(r.calls, loserLeaseCall{marker: markerID, cost: costText, plan: plan})
	r.mu.Unlock()
	r.order.record("settle")
}

func (r *recordingLoserLeaseSettler) recorded() []loserLeaseCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]loserLeaseCall(nil), r.calls...)
}

// loserLeasePC 造一个带切片计划的请求上下文（走租约判定的请求）。
func loserLeasePC(t *testing.T) *pctx.Context {
	t.Helper()
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}
	var plan pctx.LeaseSettlementPlan
	plan.Add(pctx.LeaseSettlementTarget{
		Entity:    pctx.LeaseSettlementEntityKey,
		ID:        7,
		Window:    pctx.LeaseSettlementWindow5h,
		ResetMode: "rolling",
	})
	pc.SetLeaseSettlementPlan(plan)
	return pc
}

// 输家成本入库后必须按自己的增量结算一次，标记与 store 的去重键同构（行 id + provider + attempt）。
func TestHedgeLoserBillerSettlesItsOwnLeaseIncrement(t *testing.T) {
	settler := &recordingLoserLeaseSettler{}
	pc := loserLeasePC(t)
	biller := &hedgeLoserBiller{
		wiring: HedgeWiring{LeaseSettler: settler},
		state:  &RequestState{PC: pc},
	}

	biller.settleLoserLease(context.Background(), forward.HedgeLoserBill{
		RequestID:  42,
		ProviderID: 7,
		Sequence:   2,
	}, "0.03")

	calls := settler.recorded()
	if len(calls) != 1 {
		t.Fatalf("输家成本应触发一次租约结算，实际 %d 次", len(calls))
	}
	// 标记必须与胜者那笔（纯行 id）不同：相同就会互相吞掉，输家那部分成本永远扣不掉。
	if calls[0].marker != "42:loser:7:2" {
		t.Errorf("输家结算标记不符：%q", calls[0].marker)
	}
	if calls[0].cost != "0.03" {
		t.Errorf("结算成本应原样取落库那一份文本，实际 %q", calls[0].cost)
	}
	if plan, ok := pc.LeaseSettlementPlan(); !ok || len(calls[0].plan.Targets) != len(plan.Targets) {
		t.Errorf("结算计划应与判定时一致，实际 %+v", calls[0].plan.Targets)
	}
}

// 未走租约判定的请求（无计划）不得触发结算：否则会拿着空计划去查一圈空键。
func TestHedgeLoserBillerSkipsLeaseWithoutPlan(t *testing.T) {
	settler := &recordingLoserLeaseSettler{}
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}
	biller := &hedgeLoserBiller{
		wiring: HedgeWiring{LeaseSettler: settler},
		state:  &RequestState{PC: pc},
	}

	biller.settleLoserLease(context.Background(), forward.HedgeLoserBill{RequestID: 42, ProviderID: 7, Sequence: 2}, "0.03")

	if calls := settler.recorded(); len(calls) != 0 {
		t.Fatalf("无切片计划不得结算租约，实际 %d 次", len(calls))
	}
}

// 未装配结算面（或请求状态缺失）时整段跳过，不 panic。
func TestHedgeLoserBillerSkipsLeaseWhenUnwired(t *testing.T) {
	biller := &hedgeLoserBiller{
		state: &RequestState{PC: loserLeasePC(t)},
	}
	biller.settleLoserLease(context.Background(), forward.HedgeLoserBill{RequestID: 42, ProviderID: 7, Sequence: 2}, "0.03")
	biller.settleLoserLease(context.Background(), forward.HedgeLoserBill{RequestID: 42, ProviderID: 7, Sequence: 2}, "0.03")

	// 未装配时没有可观测的调用面：本用例只要求不 panic、不阻塞（行为与接线前一致）。
}

// recordingHedgeLoserWriter 是输家成本的落点桩。
type recordingHedgeLoserWriter struct {
	mu      sync.Mutex
	calls   int
	lastID  int64
	lastVal string
	err     error
	// order 非 nil 时在**写库成功后**记录顺序（见 loserBillOrder）。
	order *loserBillOrder
}

func (w *recordingHedgeLoserWriter) AddHedgeLoserCost(
	_ context.Context,
	id int64,
	deltaCost string,
	_ store.HedgeLoserEntry,
) error {
	w.mu.Lock()
	w.calls++
	w.lastID = id
	w.lastVal = deltaCost
	err := w.err
	w.mu.Unlock()
	if err == nil {
		w.order.record("write")
	}
	return err
}

// 调用点：输家成本**成功入库之后**才结算租约，且结算用的是刚写入的那一份成本文本。
//
// 这条用例钉的是 BillLoser 的接线（上一条用例只钉 settleLoserLease 本身）：
// 少了这步接线，输家成本只进 DB、不进租约，窗口内判定会拿着偏高的余额放行。
// 顺序靠 loserBillOrder 钉住：只数调用次数放不出「先结算后写库」这种实现。
func TestHedgeLoserBillerSettlesLeaseAfterCostWrite(t *testing.T) {
	resolver, _, _ := newTestResolver(t, "redirected", map[string]string{"m": priceJSON}, nil)
	order := &loserBillOrder{}
	writer := &recordingHedgeLoserWriter{order: order}
	settler := &recordingLoserLeaseSettler{order: order}
	biller := &hedgeLoserBiller{
		wiring: HedgeWiring{Costs: resolver, Pools: writer, LeaseSettler: settler},
		state:  &RequestState{PC: loserLeasePC(t), Model: "m"},
	}
	input, output := 11.0, 7.0

	if err := biller.BillLoser(context.Background(), forward.HedgeLoserBill{
		RequestID:  42,
		ProviderID: 7,
		Sequence:   2,
		Usage:      convert.Usage{InputTokens: &input, OutputTokens: &output},
	}); err != nil {
		t.Fatalf("输家计费不应报错: %v", err)
	}

	if writer.calls != 1 {
		t.Fatalf("输家成本应写入一次，实际 %d 次", writer.calls)
	}
	calls := settler.recorded()
	if len(calls) != 1 {
		t.Fatalf("输家成本入库后应结算一次租约，实际 %d 次", len(calls))
	}
	if calls[0].marker != "42:loser:7:2" {
		t.Errorf("输家结算标记不符：%q", calls[0].marker)
	}
	if calls[0].cost != writer.lastVal {
		t.Errorf("结算成本应与落库文本一致：结算 %q 落库 %q", calls[0].cost, writer.lastVal)
	}
	// 顺序：写库在前、结算在后；对调即说明租约可能在账本没有这笔时就扣了。
	if got := strings.Join(order.snapshot(), ","); got != "write,settle" {
		t.Fatalf("输家必须先写库后结算租约，实际调用顺序 %q", got)
	}
}

// 成本写库失败时不得结算租约：账本没有这笔而租约少一片，少掉的切片不会随刷新回来。
func TestHedgeLoserBillerSkipsLeaseWhenCostWriteFails(t *testing.T) {
	resolver, _, _ := newTestResolver(t, "redirected", map[string]string{"m": priceJSON}, nil)
	order := &loserBillOrder{}
	writer := &recordingHedgeLoserWriter{err: errors.New("写库失败"), order: order}
	settler := &recordingLoserLeaseSettler{order: order}
	biller := &hedgeLoserBiller{
		wiring: HedgeWiring{Costs: resolver, Pools: writer, LeaseSettler: settler},
		state:  &RequestState{PC: loserLeasePC(t), Model: "m"},
	}
	input, output := 11.0, 7.0

	if err := biller.BillLoser(context.Background(), forward.HedgeLoserBill{
		RequestID:  42,
		ProviderID: 7,
		Sequence:   2,
		Usage:      convert.Usage{InputTokens: &input, OutputTokens: &output},
	}); err == nil {
		t.Fatal("写库失败应原样冒泡给调用方")
	}
	if calls := settler.recorded(); len(calls) != 0 {
		t.Fatalf("成本未入库不得结算租约，实际 %d 次", len(calls))
	}
}
