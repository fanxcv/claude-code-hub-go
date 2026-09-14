package pctx

import (
	"context"
	"testing"
)

// recordingWriteback 是测试用的写回能力：只记账，不碰任何外部依赖。
type recordingWriteback struct {
	winner    int64
	tombstone int64
}

func (w *recordingWriteback) RecordWinner(_ context.Context, providerID int64) bool {
	w.winner = providerID
	return true
}

func (w *recordingWriteback) TombstoneOnFailure(_ context.Context, failedProviderID int64) bool {
	w.tombstone = failedProviderID
	return true
}

// TestAffinityWritebackSlot 钉住槽位语义：未装入时读不到、传 nil 是 no-op、装入后取回同一份。
func TestAffinityWritebackSlot(t *testing.T) {
	pc, err := New(Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}

	if _, ok := pc.AffinityWriteback(); ok {
		t.Fatal("未装入时不应读到写回能力")
	}
	pc.SetAffinityWriteback(nil)
	if _, ok := pc.AffinityWriteback(); ok {
		t.Fatal("传 nil 不应改变槽位状态")
	}

	writeback := &recordingWriteback{}
	pc.SetAffinityWriteback(writeback)
	got, ok := pc.AffinityWriteback()
	if !ok {
		t.Fatal("装入后应读到写回能力")
	}
	if !got.RecordWinner(context.Background(), 7) {
		t.Fatal("取回的应是装入的那一份")
	}
	if writeback.winner != 7 {
		t.Fatalf("写回能力未被调用到: %+v", writeback)
	}
}
