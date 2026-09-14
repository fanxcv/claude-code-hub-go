package terminal

import (
	"context"
	"sync"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// recordingNotifier 记录收到的行 id（线程安全：结算可能被并发调用）。
type recordingNotifier struct {
	mu  sync.Mutex
	ids []int64
}

func (r *recordingNotifier) NotifyNewRow(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
}

func (r *recordingNotifier) recorded() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.ids...)
}

// 终态提交后必须通知一次，且带上正确的行 id。
//
// 这是使用记录页「推送模式」的起点：少了它，SSE 只发心跳，前端会一直等不到新行。
func TestSettleNotifiesOncePerCommittedRow(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	notifier := &recordingNotifier{}
	settler := New(writer, Options{NewRows: notifier})

	result, err := settler.Settle(context.Background(), 777, Settlement{StatusCode: 200})
	if err != nil {
		t.Fatalf("结算不应报错: %v", err)
	}
	if !result.Committed {
		t.Fatal("本用例需要一次赢得终态的结算")
	}

	got := notifier.recorded()
	if len(got) != 1 || got[0] != 777 {
		t.Fatalf("应恰好通知一次且 id=777，实际 %v", got)
	}
}

// 重复结算（未赢得终态）不得再通知：否则并发收尾会让同一行被广播多次，
// 前端会为一条不存在的「新行」多做一次增量拉取。
func TestSettleDoesNotNotifyWhenNotCommitted(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}
	notifier := &recordingNotifier{}
	settler := New(writer, Options{NewRows: notifier})

	if _, err := settler.Settle(context.Background(), 778, Settlement{StatusCode: 200}); err != ErrNotSettled {
		t.Fatalf("未赢得终态应返回 ErrNotSettled，实际 %v", err)
	}
	if got := notifier.recorded(); len(got) != 0 {
		t.Fatalf("未提交的行不该产生通知，实际 %v", got)
	}
}

// 不计费的行同样要通知：它照样是使用记录页上的一行（拦截、replay、价格缺失）。
func TestSettleNotifiesUnpricedRow(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	notifier := &recordingNotifier{}
	settler := New(writer, Options{NewRows: notifier})

	// Cost 为 nil 即不计费。
	if _, err := settler.Settle(context.Background(), 779, Settlement{StatusCode: 403}); err != nil {
		t.Fatalf("结算不应报错: %v", err)
	}
	if got := notifier.recorded(); len(got) != 1 || got[0] != 779 {
		t.Fatalf("不计费的行也应通知一次，实际 %v", got)
	}
}

// 拦截类终态（建行 + 写终态两步）也必须通知——它们占使用记录的一大部分。
func TestSettleBlockedNotifies(t *testing.T) {
	writer := &fakeWriter{
		createRow:        store.MessageRequest{ID: 880},
		unfinalizedQueue: []unfinalizedResult{{committed: true}},
	}
	notifier := &recordingNotifier{}
	settler := New(writer, Options{NewRows: notifier})

	if _, err := settler.SettleBlocked(
		context.Background(),
		store.CreateMessageRequestData{},
		Settlement{StatusCode: 403},
	); err != nil {
		t.Fatalf("拦截结算不应报错: %v", err)
	}
	if got := notifier.recorded(); len(got) != 1 || got[0] != 880 {
		t.Fatalf("拦截路径应通知建行后的 id=880，实际 %v", got)
	}
}

// 未装配通知面时静默跳过：结算路径行为与接线前一致（这是灰度期与测试装配的常态）。
func TestSettleWithoutNotifierIsSilent(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	settler := New(writer, Options{})

	result, err := settler.Settle(context.Background(), 781, Settlement{StatusCode: 200})
	if err != nil || !result.Committed {
		t.Fatalf("未装配通知面时结算应照常成功，实际 result=%+v err=%v", result, err)
	}
}
