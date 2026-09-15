// Package notify 是通知的**数据生成器**（Node 时代的 src/lib/notification/tasks/*）。
//
// 三种定时数据各有自己的取数面与判定：
//   - DailyLeaderboard：近 24 小时用户榜的前 N 名 + 全量合计（tasks/daily-leaderboard.ts:11）
//   - CostAlerts：各密钥/供应商在 5 小时、本周、本月窗口的已花与其限额的比较
//     （tasks/cost-alert.ts:14）
//   - CacheHitRateAlert：按供应商×模型的缓存命中率相比基线下降，带冷却去重
//     （tasks/cache-hit-rate-alert.ts:209，判定在 src/lib/cache-hit-rate-alert/decision.ts）
//
// 第四种（熔断告警）是事件型：正文由转发路径在开闸那一刻产生（Node 的 sendCircuitBreakerAlert），
// 本包不参与。
//
// 数据契约的真源在仓库里仍在：`src/lib/webhook/types.ts` 定义了各 Data 形状，
// `src/lib/webhook/templates/placeholders.ts` 定义模板会读哪些键。本包的 Go 结构体与之一一对齐，
// 改字段名即改契约。
//
// 调度与投递不在这里：`go/internal/jobs` 决定「何时跑」并复检开关，`go/internal/adminapi`
// 负责投递（信封/签名/响应判定）。本包只回答「这一刻的数据是什么」。
//
// **窗口切分一律用系统时区**（Node 的三个生成器都调 resolveSystemTimezone()），任务时区
// （binding.scheduleTimezone）只用于文案里的时间戳——两者是不同的东西，别混用。
//
// 移植对应：本包的注释按「源文件:行号」逐条标注，行号指 Node 仓的
// `src/lib/notification/tasks/*.ts` 与 `src/lib/cache-hit-rate-alert/decision.ts`。
package notify
