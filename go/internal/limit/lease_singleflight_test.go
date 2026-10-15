package limit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// countingLedger 是并发安全的账本桩：数回表次数，并让每次查询停留一会以拉开单飞窗口
// （单飞只合并时间上重叠的调用，回表够快时并发调用本就该各算一次）。
type countingLedger struct {
	mu    sync.Mutex
	calls int
	wait  time.Duration
}

func (l *countingLedger) SumLedgerCostInTimeRange(_ context.Context, _ store.LedgerEntityType, _ any, _ time.Time, _ time.Time) (string, error) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	time.Sleep(l.wait)
	return "0", nil
}

func (l *countingLedger) SumLedgerTotalCost(_ context.Context, _ store.LedgerEntityType, _ any, _ *time.Time) (string, error) {
	return "0", nil
}

func (l *countingLedger) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// TestLeaseRefreshSingleFlight 钉住「租约过期的瞬间并发请求只回表一次」：同一份租约（同参数）
// 的 N 个并发调用共享一次回表 + 一次设置读取；单飞只合并重叠调用，不把结果当缓存。
func TestLeaseRefreshSingleFlight(t *testing.T) {
	settings := &leaseSettingsStub{settings: QuotaLeaseSettings{RefreshIntervalSeconds: 60}}
	ledger := &countingLedger{wait: 200 * time.Millisecond}
	service := NewLeaseService(deadRedisClient(t), settings, ledger, time.UTC, func() time.Time { return fixedNow }, nil)
	if service == nil {
		t.Fatal("租约服务未组装")
	}
	params := GetCostLeaseParams{
		Entity: EntityUser, EntityID: 5, Window: LeaseWindowDaily,
		LimitAmount: 100, ResetMode: ResetFixed,
	}

	const callers = 8
	start := make(chan struct{})
	leases := make([]bool, callers)
	var waitGroup sync.WaitGroup
	for index := 0; index < callers; index++ {
		waitGroup.Add(1)
		go func(slot int) {
			defer waitGroup.Done()
			<-start
			_, leases[slot] = service.GetCostLease(context.Background(), params)
		}(index)
	}
	close(start)
	waitGroup.Wait()

	if got := ledger.callCount(); got != 1 {
		t.Errorf("并发 %d 次取租约应只回表一次，实际 %d 次", callers, got)
	}
	if settings.calls != 1 {
		t.Errorf("并发刷新应只读一次租约设置，实际 %d 次", settings.calls)
	}
	for slot, ok := range leases {
		if !ok {
			t.Errorf("第 %d 个调用方没拿到租约（应共享首次结果）", slot)
		}
	}

	// 单飞不是结果缓存：错开的下一次调用照常回表。
	if _, ok := service.GetCostLease(context.Background(), params); !ok {
		t.Fatal("串行调用应照常拿到租约")
	}
	if got := ledger.callCount(); got != 2 {
		t.Errorf("错开的调用应各自回表，实际累计 %d 次", got)
	}
}
