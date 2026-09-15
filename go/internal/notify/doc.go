// Package notify 是通知的**数据生成器**（Node 时代的 src/lib/notification/tasks/*）。
//
// 三份数据各有自己的取数面与判定：
//   - DailyLeaderboard：近 24 小时用户榜的前 N 名 + 全量合计（tasks/daily-leaderboard.ts）
//   - CostAlerts：各密钥/供应商在 5h、本周、本月三个窗口的已花与其限额的比较
//     （tasks/cost-alert.ts）
//   - CacheHitRateAlert：按供应商×模型的缓存命中率相对基线下降（tasks/cache-hit-rate-alert.ts）
//
// 数据契约的真源在仓库里仍在：`src/lib/webhook/types.ts` 定义了四个 Data 形状
// （DailyLeaderboardData / CostAlertData / CacheHitRateAlertData / CircuitBreakerAlertData），
// `src/lib/webhook/templates/placeholders.ts` 定义模板会读哪些键。本包的 Go 结构体与之一一对齐，
// 改字段名即改契约。
//
// 调度与投递不在这里：`go/internal/jobs` 决定「何时跑」并复检开关，`go/internal/adminapi`
// 负责投递（信封/签名/响应判定）。本包只回答「这一刻的数据是什么」。
//
// 登记：Node 的 tasks/* 源码随 Node 层一起删除，且不在本仓 git 历史里（仓库是压缩导入）。
// 因此凡契约未钉住的判定细节（缓存告警的阈值组合方式）都在本文件对应的实现处标注为
// **重建**，并把策略收敛到单个纯函数（cache_alert_decision.go）以便一处纠正。
package notify
