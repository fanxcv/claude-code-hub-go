// Package limit 复刻 Node 侧的限流与配额语义（src/lib/rate-limit/ 与
// src/lib/redis/active-session-keys.ts），并把它适配成 guard.RateLimiter。
//
// 分工（刻意如此）：
//   - 本包只做「语义编排」：决定用哪段脚本、参数是什么、返回值怎么解释、何时降级。
//   - 脚本调用层与 Lua 正文在 internal/ratelimit（内嵌 18 段脚本 + 通用 EVALSHA + 黄金重放），
//     本包不重复实现，也不自己拼多条命令去凑原子语义。
//
// 数据来源（都不查配置表的中转缓存）：
//   - 限额字段来自 QuotaSource（由接线波次从认证快照/cfgsync 注入）；
//   - 时间窗时区来自 Config.Location（由接线波次按 system_settings.timezone 解析）；
//   - 两条直接读库的路径：①「Redis 不可用/缓存未命中」时的账本回退（LedgerReader，Node 侧
//     checkCostLimitsFromDatabase 的等价物）；②租约刷新（LeaseService，见下）按刷新间隔读一次账本。
//
// 租约（lease，客户端份自 src/lib/rate-limit/lease.ts 与 lease-service.ts）：
//   - 已移植：租约模型与键形制（lease.go）、两段 Lua（lease_lua.go）、刷新/扣减/批量结算
//     （lease_service.go）、设置面（lease_settings.go）、把周期限额判定切到租约
//     （lease_check.go，对齐 Node rate-limit-guard 的 checkCostLimitsWithLease）。
//   - 未接线：结算与扣减的**调用点**（Node 在终态响应处理里随 trackCost 一起调）属数据面波次；
//     未接线期间租约的实际效力 = 「每 quota_db_refresh_interval_seconds 刷新一次的 DB 用量快照」，
//     超过一个刷新周期的消耗不计入判定（有界超支），接上结算后方与 Node 一致。
//
// 有意未移植（属于独立模块，不是本包的一部分）：
//   - 会话绑定与发现（SessionManager / discovery-coordinator）：本包只做并发额度记账。
package limit
