// Package usersreset 移植用户统计重置子系统（src/lib/user-statistics-reset/*，829 行）。
//
// 对应关系：
//
//	src/lib/user-statistics-reset/types.ts              -> state.go
//	src/lib/user-statistics-reset/reset-status-store.ts -> status.go（键布局与两段 Lua 逐字照抄）
//	src/lib/user-statistics-reset/reset-service.ts      -> reset.go（落库语句在 store/admin_users_reset.go）
//	src/lib/user-statistics-reset/reset-queue.ts        -> queue.go + worker.go
//	src/lib/redis/cost-cache-cleanup.ts（user 清理）     -> cleanup.go
//
// 四条硬约束：
//
//  1. **状态键与认领键与 Node 逐字一致**（`cch:user-statistics-reset:{status,active}:*`，TTL 7 天），
//     因此切换期两侧互见：Node 的 GET /users/{id}/statistics-resets/{resetId} 能读到 Go 排的作业，
//     Node 也不会重复排一个 Go 正在跑的作业（认领用的是同一个 active 键）。
//  2. **队列本身是 Go 专有的**：Node 用 Bull（`bull:user-statistics-reset:*`），本包用 Redis ZSET
//     + PG advisory lock 选主。后果是作业的**执行**不跨端——Node 排的作业只有 Node 执行，反之亦然。
//     切换期靠前门的归属规则保证同一时刻只有一侧在跑这两条路由；被落下的作业（status 停在
//     queued 且无人执行）由 queue.go 的对账路径恢复：管理员重发同一条 POST 即会重新入队。
//  3. **进度与终态只写在 status 键里**：这是作业可恢复的唯一凭据（进程重启后 worker 从它续跑），
//     也是 UI 轮询的唯一数据源。故任何失败路径都必须先落状态、再放认领。
//  4. **不得把密钥原文写进日志**：清 total_cost 缓存时模式里嵌着 API Key 原文（Node 既有布局），
//     日志只记条数与键 id，绝不回显扫描到的键名。
package usersreset
