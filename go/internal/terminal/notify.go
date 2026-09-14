package terminal

// 本文件是「请求日志行有新内容落库」的**事件捕获旁路**：终态提交之后，把这次写入折算成
// 一个「有新行」信号交给下游（发布端见 go/internal/usagefeed）。
//
// 用途：管理面使用记录页的**推送模式**。前端收到信号后走增量接口只取那几行，
// 而不是每 3 秒把已加载的每一页整份重取（每页 284 KB，公网 p50 861ms）。
//
// 三条约束与 RollupRecorder 同构（见 rollup.go 的文件头），理由也相同：
//
//  1. **失败不得影响结算**：通知接口**没有返回值**，实现必须自带降级；调用方在旁路前后
//     不改变控制流。反证见 `notify_test.go` 的 `TestSettleNotifiesOncePerCommittedRow`。
//
//  2. **时机：终态提交之后、且只在赢得终态时**。用 `Result.Committed` 当闸门同时解决
//     **重复通知**：重复结算会拿到 `Committed=false`，不会重复广播。
//
//  3. **不做重试**。信号是**幂等提示**：丢了只让前端晚一个周期看到那几行（下一轮增量拉取
//     仍会取到），因此不值得为它引入重试队列与背压。这与 rollup 的取舍不同——rollup 是
//     统计事实，丢了要等下一轮聚合窗口。

// NewRowsNotifier 是「请求日志有新行落库」的接收面。
//
// 用窄接口而不是直接要求 `*usagefeed.Hub`：terminal 只负责把事实交出去，不关心对方是
// Redis 广播、内存队列还是 nil（未装配）。
type NewRowsNotifier interface {
	// NotifyNewRow 报告 id 这一行刚落库。**不得阻塞调用方，不得 panic，不得返回错误**
	// （签名上就没有错误可返）：实现应把发布做成 fire-and-forget。
	NotifyNewRow(id int64)
}

// notifier 在未装配时是 nil，调用点靠这个包装把「未装配」收敛成一次判断。
func (s *Settler) notifyNewRow(id int64) {
	if s == nil || s.newRows == nil || id <= 0 {
		return
	}
	s.newRows.NotifyNewRow(id)
}
