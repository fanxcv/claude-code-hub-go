package terminal

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住租约结算旁路的**时机与闸门**（见 lease_settle.go 的文件头）：
// 只有「赢得终态 + 成本写入成功 + 有切片 + 有成本」四件事同时成立时才结算一次，
// 且结算失败不得改变结算结果。

// recordingLeaseSettler 记录收到的租约结算调用（线程安全：结算可能被并发调用）。
type recordingLeaseSettler struct {
	mu    sync.Mutex
	calls []leaseSettleCall
}

type leaseSettleCall struct {
	requestID string
	cost      string
	plan      pctx.LeaseSettlementPlan
}

func (r *recordingLeaseSettler) SettleLeases(
	_ context.Context,
	requestID string,
	cost string,
	plan pctx.LeaseSettlementPlan,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, leaseSettleCall{requestID: requestID, cost: cost, plan: plan})
}

func (r *recordingLeaseSettler) recorded() []leaseSettleCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]leaseSettleCall(nil), r.calls...)
}

// leaseSettlementPlan 是一份「走了租约判定」的计划：只列真正用过的切片（主体 + 窗口 + 生效模式）。
func leaseSettlementPlan() pctx.LeaseSettlementPlan {
	var plan pctx.LeaseSettlementPlan
	plan.Add(pctx.LeaseSettlementTarget{
		Entity: pctx.LeaseSettlementEntityKey, ID: 7, Window: pctx.LeaseSettlementWindow5h, ResetMode: "rolling",
	})
	plan.Add(pctx.LeaseSettlementTarget{
		Entity: pctx.LeaseSettlementEntityKey, ID: 7, Window: pctx.LeaseSettlementWindowDaily, ResetMode: "fixed",
	})
	plan.Add(pctx.LeaseSettlementTarget{
		Entity: pctx.LeaseSettlementEntityUser, ID: 9, Window: pctx.LeaseSettlementWindow5h, ResetMode: "rolling",
	})
	return plan
}

// sameLeasePlan 比较两份计划（计划里是切片，不能直接用 ==）。
func sameLeasePlan(left, right pctx.LeaseSettlementPlan) bool {
	return reflect.DeepEqual(left.Targets, right.Targets)
}

// leaseSettleOptions 给出零退避 + 租约结算接收面的装配参数。
func leaseSettleOptions(settler LeaseSettler) Options {
	return Options{
		MaxAttempts:  3,
		Backoff:      func(int) time.Duration { return 0 },
		LeaseSettler: settler,
	}
}

// 成本落库成功后必须结算一次，且带上行 id 与判定时的切片。
//
// 少了这一步，租约只会在刷新窗口到期时才回收：窗口内无论花掉多少钱，判定都拿着最初那一片，
// 也就是「窗口内可超支」这个缺口本身。
func TestSettleSettlesLeasesOnceAfterCostWrite(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	leases := &recordingLeaseSettler{}
	settler := New(writer, leaseSettleOptions(leases))

	result, err := settler.Settle(context.Background(), 777, Settlement{
		StatusCode:      200,
		Cost:            &Cost{Total: "0.25"},
		LeaseSettlement: leaseSettlementPlan(),
	})
	if err != nil {
		t.Fatalf("结算不应报错: %v", err)
	}
	if !result.Committed || !result.CostWritten {
		t.Fatalf("本用例需要一次「终态赢下且成本已写」的结算: %+v", result)
	}

	calls := leases.recorded()
	if len(calls) != 1 {
		t.Fatalf("应恰好结算一次租约，实际 %d 次", len(calls))
	}
	if calls[0].requestID != "777" {
		t.Errorf("结算标记应取行 id，实际 %q", calls[0].requestID)
	}
	if calls[0].cost != "0.25" {
		t.Errorf("结算成本应取账本里的成本文本，实际 %q", calls[0].cost)
	}
	if !sameLeasePlan(calls[0].plan, leaseSettlementPlan()) {
		t.Errorf("结算计划应与判定时一致，实际 %+v", calls[0].plan)
	}
}

// 未赢得终态（重复结算、重放）不得结算租约：否则一次请求会被扣两遍。
func TestSettleDoesNotSettleLeasesWhenNotCommitted(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}
	leases := &recordingLeaseSettler{}
	settler := New(writer, leaseSettleOptions(leases))

	if _, err := settler.Settle(context.Background(), 778, Settlement{
		StatusCode:      200,
		Cost:            &Cost{Total: "0.25"},
		LeaseSettlement: leaseSettlementPlan(),
	}); !errors.Is(err, ErrNotSettled) {
		t.Fatalf("未赢得终态应返回 ErrNotSettled，实际 %v", err)
	}
	if calls := leases.recorded(); len(calls) != 0 {
		t.Fatalf("未赢得终态不得结算租约，实际 %d 次", len(calls))
	}
}

// 成本写失败时不得结算租约：账本没有这笔而租约少一片，而少掉的切片不会随刷新回来。
func TestSettleDoesNotSettleLeasesWhenCostWriteFails(t *testing.T) {
	writer := &fakeWriter{
		unfinalizedQueue: []unfinalizedResult{{committed: true}},
		// 三次尝试全部失败：UpdateWinnerCost 会按 MaxAttempts 重试，只给一条错误会被第二次成功洗掉。
		costQueue: []error{errors.New("成本写失败"), errors.New("成本写失败"), errors.New("成本写失败")},
	}
	leases := &recordingLeaseSettler{}
	settler := New(writer, leaseSettleOptions(leases))

	_, err := settler.Settle(context.Background(), 779, Settlement{
		StatusCode:      200,
		Cost:            &Cost{Total: "0.25"},
		LeaseSettlement: leaseSettlementPlan(),
	})
	if !errors.Is(err, ErrCostWriteFailed) {
		t.Fatalf("成本写失败应返回 ErrCostWriteFailed，实际 %v", err)
	}
	if calls := leases.recorded(); len(calls) != 0 {
		t.Fatalf("成本未入库不得结算租约，实际 %d 次", len(calls))
	}
}

// 不计费的终态（价格缺失、拦截、replay）不得结算租约：没有成本可扣。
func TestSettleDoesNotSettleLeasesWithoutCost(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	leases := &recordingLeaseSettler{}
	settler := New(writer, leaseSettleOptions(leases))

	if _, err := settler.Settle(context.Background(), 780, Settlement{
		StatusCode:      200,
		LeaseSettlement: leaseSettlementPlan(),
	}); err != nil {
		t.Fatalf("不计费结算不应报错: %v", err)
	}
	if calls := leases.recorded(); len(calls) != 0 {
		t.Fatalf("无成本不得结算租约，实际 %d 次", len(calls))
	}
}

// 没有切片计划（本次请求未走租约判定）不得结算：否则会拿着零值计划去查一圈空键。
func TestSettleDoesNotSettleLeasesWithoutPlan(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	leases := &recordingLeaseSettler{}
	settler := New(writer, leaseSettleOptions(leases))

	if _, err := settler.Settle(context.Background(), 781, Settlement{
		StatusCode: 200,
		Cost:       &Cost{Total: "0.25"},
	}); err != nil {
		t.Fatalf("无切片计划的结算不应报错: %v", err)
	}
	if calls := leases.recorded(); len(calls) != 0 {
		t.Fatalf("无切片计划不得结算租约，实际 %d 次", len(calls))
	}
}

// 结算失败不影响结算结果本身：租约是判定的加速面，权威额度在 DB。
//
// 实现方按约定「无返回值、自带降级」，此用例钉住调用方这一侧：接收面返回后，
// 结算结果与未装配时完全一致。
func TestSettleResultUnaffectedByLeaseSettler(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	settler := New(writer, leaseSettleOptions(&recordingLeaseSettler{}))

	result, err := settler.Settle(context.Background(), 782, Settlement{
		StatusCode:      200,
		Cost:            &Cost{Total: "0.25"},
		LeaseSettlement: leaseSettlementPlan(),
	})
	if err != nil {
		t.Fatalf("结算不应报错: %v", err)
	}
	if !result.Committed || !result.CostWritten {
		t.Fatalf("结算结果应与装配租约前一致: %+v", result)
	}
	if writer.costTotal != "0.25" {
		t.Errorf("成本应已入库，实际 %q", writer.costTotal)
	}
}

// statusCodeClientClosed 是客户端中断的终态码。
//
// 终端包不导出这个码（它由转发侧与 patrol 各自持有：patrol.RepairStatusCode、forward 的
// 499 留痕），本用例按仓库既有取值写定，只用来钉「取消但计费」这条路径。
const statusCodeClientClosed = 499

// 取消路径：客户端中断（499）但**仍计费**的终态照样要结算租约。
//
// 为什么值得单独钉：取消是唯一一类「没把响应交给客户端却花了钱」的终态，若日后有人在
// 结算旁路上按状态码加白名单（只结算 2xx），这些请求的成本就会从租约里漏掉。
func TestSettleSettlesLeasesForBilledCancellation(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	leases := &recordingLeaseSettler{}
	settler := New(writer, leaseSettleOptions(leases))

	result, err := settler.Settle(context.Background(), 790, Settlement{
		StatusCode:      statusCodeClientClosed,
		Cost:            &Cost{Total: "0.25"},
		LeaseSettlement: leaseSettlementPlan(),
	})
	if err != nil {
		t.Fatalf("取消结算不应报错: %v", err)
	}
	if !result.Committed || !result.CostWritten {
		t.Fatalf("本用例需要一次「终态赢下且成本已写」的结算: %+v", result)
	}
	calls := leases.recorded()
	if len(calls) != 1 || calls[0].requestID != "790" || calls[0].cost != "0.25" {
		t.Fatalf("计费的取消应结算一次且带行 id 与成本，实际 %+v", calls)
	}
}

// 取消路径：不计费的取消（无成本）不得结算——判定侧从未扣减，这里也没有要扣的。
func TestSettleDoesNotSettleLeasesForUnbilledCancellation(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	leases := &recordingLeaseSettler{}
	settler := New(writer, leaseSettleOptions(leases))

	if _, err := settler.Settle(context.Background(), 791, Settlement{
		StatusCode:      statusCodeClientClosed,
		LeaseSettlement: leaseSettlementPlan(),
	}); err != nil {
		t.Fatalf("不计费的取消不应报错: %v", err)
	}
	if calls := leases.recorded(); len(calls) != 0 {
		t.Fatalf("无成本不得结算租约，实际 %d 次", len(calls))
	}
}

// 并发结算同一条请求：终态屏障（`status_code IS NULL` 谓词）只让一个调用赢，
// 故租约只结算一次；输的那次连成本写都不会发生。
func TestSettleConcurrentCallsSettleLeasesOnce(t *testing.T) {
	writer := &onceWriter{}
	leases := &recordingLeaseSettler{}
	settler := New(writer, leaseSettleOptions(leases))

	const callers = 4
	var wg sync.WaitGroup
	results := make([]Result, callers)
	errs := make([]error, callers)
	for index := 0; index < callers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = settler.Settle(context.Background(), 792, Settlement{
				StatusCode:      200,
				Cost:            &Cost{Total: "0.25"},
				LeaseSettlement: leaseSettlementPlan(),
			})
		}(index)
	}
	wg.Wait()

	committed := 0
	for index, result := range results {
		switch {
		case errs[index] == nil && result.Committed:
			committed++
		case errors.Is(errs[index], ErrNotSettled) && !result.Committed:
		default:
			t.Fatalf("并发结算出现了第三种结局: %+v err=%v", result, errs[index])
		}
	}
	if committed != 1 {
		t.Fatalf("终态屏障只应让一个调用赢下，实际 %d 个", committed)
	}
	if calls := leases.recorded(); len(calls) != 1 {
		t.Fatalf("并发结算应恰好扣减一次，实际 %d 次", len(calls))
	}
	if writer.costCalls != 1 {
		t.Fatalf("成本只应写一次，实际 %d 次", writer.costCalls)
	}
}

// onceWriter 只让第一次终态写赢下：复刻 store 的 `status_code IS NULL` 谓词在并发下的结果。
type onceWriter struct {
	mu        sync.Mutex
	committed bool
	costCalls int
}

func (w *onceWriter) CreateMessageRequest(
	_ context.Context,
	_ store.CreateMessageRequestData,
) (store.MessageRequest, error) {
	return store.MessageRequest{}, nil
}

func (w *onceWriter) UpdateDetailsIfUnfinalized(
	_ context.Context,
	_ int64,
	_ store.DetailsPatch,
) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return false, nil
	}
	w.committed = true
	return true, nil
}

func (w *onceWriter) UpdateWinnerCost(_ context.Context, _ int64, _ string, _ []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.costCalls++
	return nil
}

func (w *onceWriter) FindModelPrice(_ context.Context, _ string) (*store.ModelPrice, error) {
	return nil, nil
}
