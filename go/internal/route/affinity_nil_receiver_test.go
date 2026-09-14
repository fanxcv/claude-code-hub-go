package route

import (
	"context"
	"testing"
)

// TestAffinityPutNilReceiverReturnsFalse 钉住 Put 的 godoc 承诺：
// 「返回值 false 表示未写入，原因只有两类：参数不足/未配置 Redis，或 generation CAS 失败」。
//
// nil 接收者属于「参数不足/未配置」那一类（同文件 Lookup/Invalidate 都是先判 nil 再解引用），
// 故这里必须**返回 false**，而不是在取 slidingTTLSeconds 时 panic。
func TestAffinityPutNilReceiverReturnsFalse(t *testing.T) {
	var store *AffinityStore

	if got := store.Put(context.Background(), "scope", "tip-fp", 7, "identity-fp", "gen-1"); got {
		t.Fatal("nil 接收者应返回 false（未写入）")
	}
}

// TestAffinityPutNilReceiverIgnoresContextCancellation 说明 nil 接收者的早退不依赖 ctx：
// 传一个已取消的 ctx 也必须返回 false 而非 panic（调用方在收尾路径上常带已取消的 ctx）。
func TestAffinityPutNilReceiverIgnoresContextCancellation(t *testing.T) {
	var store *AffinityStore
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := store.Put(ctx, "scope", "tip-fp", 7, "identity-fp", "gen-1"); got {
		t.Fatal("nil 接收者 + 已取消 ctx 应返回 false")
	}
}
