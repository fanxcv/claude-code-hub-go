package dataplane

import (
	"context"
	"testing"
)

// backlogStub 同时实现终态等待面与积压面（生产实现是 terminal.WriteQueue）。
type backlogStub struct{ pending int64 }

func (s *backlogStub) AwaitSettlement(context.Context, int64) bool { return true }

func (s *backlogStub) PendingSettlements() int64 { return s.pending }

// 退出序列的待落库数必须并入异步队列积压。
//
// 流路径的积压由结算跟踪器盖住（它的屏障等到 flush），而**非流路径**（拦截类终态等）入队后
// 没有跟踪器——漏掉这一项，日志会在队列里还躺着终态时报 0，而紧接着的关池会把它们带走。
func TestPendingSettlementsIncludesQueueBacklog(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	handler := &Handler{
		options:     Options{SettlementBarrier: &backlogStub{pending: 5}},
		settlements: newSettlementTracker(),
	}
	handler.settlements.begin(func() { <-release })
	handler.settlements.begin(func() { <-release })

	if got := handler.PendingSettlements(); got != 7 {
		t.Fatalf("待落库数 = %d, want 7（2 条在途流 + 5 条队列积压）", got)
	}

	// 同步写模式：没有队列，只看跟踪器（与接线前逐字一致）。
	syncHandler := &Handler{settlements: newSettlementTracker()}
	syncHandler.settlements.begin(func() { <-release })
	if got := syncHandler.PendingSettlements(); got != 1 {
		t.Fatalf("未装配队列时待落库数 = %d, want 1", got)
	}
}
