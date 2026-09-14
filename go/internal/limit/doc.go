// Package limit 复刻 Node 侧的限流与配额语义（src/lib/rate-limit/ 与
// src/lib/redis/active-session-keys.ts），并把它适配成 guard.RateLimiter。
//
// 分工（刻意如此）：
//   - 本包只做「语义编排」：决定用哪段脚本、参数是什么、返回值怎么解释、何时降级。
//   - 脚本调用层与 Lua 正文在 internal/ratelimit（内嵌 18 段脚本 + 通用 EVALSHA + 黄金重放），
//     本包不重复实现，也不自己拼多条命令去凑原子语义。
//
// 数据来源（都不查库、不查配置表的中转缓存）：
//   - 限额字段来自 QuotaSource（由接线波次从认证快照/cfgsync 注入）；
//   - 时间窗时区来自 Config.Location（由接线波次按 system_settings.timezone 解析）；
//   - 唯一直接读库的路径是「Redis 不可用/缓存未命中」时的账本回退（LedgerReader），
//     这是 Node 侧 checkCostLimitsFromDatabase 的等价物。
//
// 有意未移植（都属于独立模块，不是本包的一部分）：
//   - lease 系列（lease-service.ts 的预算租约与结算）：5h/daily 的额度租借走 PostgreSQL
//     权威用量，属另一波次的账务闭环；
//   - 会话绑定与发现（SessionManager / discovery-coordinator）：本包只做并发额度记账。
package limit
