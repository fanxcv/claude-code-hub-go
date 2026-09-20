package dataplane

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 这条钉子的由来（2026-09-20 生产事故）：
//
// 退出序列靠 Handler.FlushSettlements 把异步终态写队列冲干净，而它内部是
// `h.options.SettlementBarrier.(SettlementFlusher)` 的类型断言。生产装配把
// *terminal.WriteQueue 裸装进 SettlementBarrier，而队列当时只有 Flush/Stop
// 两个方法名——断言恒失败、FlushSettlements 静默返回 nil，于是关池前那批终态
// 再也没被写出去，账本行永久留在 status_code IS NULL。
//
// 断言失败之所以没被任何用例发现：既有用例都直接调队列的 Flush/Stop，或给
// Handler 注入假 barrier，**没有人把真队列装进真 Handler**。这条钉子用编译期
// 接口断言把这条路钉死：名字对不上就编不过。
var (
	_ SettlementBarrier = (*terminal.WriteQueue)(nil)
	_ SettlementBacklog = (*terminal.WriteQueue)(nil)
	_ SettlementFlusher = (*terminal.WriteQueue)(nil)
)

// TestWriteQueueSatisfiesDataplaneSeams 让上面的编译期断言在测试里也可见，
// 并在失败时给出人能读懂的说明（编译期断言本身不会输出解释）。
func TestWriteQueueSatisfiesDataplaneSeams(t *testing.T) {
	var queue *terminal.WriteQueue
	if _, ok := any(queue).(SettlementFlusher); !ok {
		t.Fatal("terminal.WriteQueue 未实现 dataplane.SettlementFlusher：退出序列的冲队列会静默失效，关池前未落库的终态会被丢掉")
	}
	if _, ok := any(queue).(SettlementBacklog); !ok {
		t.Fatal("terminal.WriteQueue 未实现 dataplane.SettlementBacklog：退出日志会少报队列积压")
	}
}
