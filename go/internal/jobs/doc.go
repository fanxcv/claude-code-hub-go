// Package jobs 承载 Go 侧的常驻后台任务：云价格同步、可用性投影回填等。
//
// 移植真源是 Node 侧的 `src/lib/price-sync/*` 与 `src/lib/availability/projection-worker.ts`。
// 每个任务都在自己的文件里注明对应的 TS 文件与行号，口径差异一律写进注释与
// internal/jobs/README.md，不做「等价但不同」的改写。
//
// 开关（Go 专有变量，故不进 go/env-parity.txt 的对账清单）：
//
//	CCH_JOBS_ENABLED                 总开关，默认 true
//	CCH_JOB_PRICE_SYNC_ENABLED       云价格同步，默认 true
//	CCH_JOB_AVAIL_BACKFILL_ENABLED   可用性投影回填，默认 true
//	CCH_JOB_PRICE_SYNC_INTERVAL_MS   云价格同步间隔，默认 1800000（30 分钟）
//	CCH_JOB_NOTIFY_ENABLED           通知调度器，默认 false（Node 并存期避免与 Bull 双发）
//	CCH_JOB_NOTIFY_TICK_MS           通知调度器扫描间隔，默认 30000（30 秒）
//	CCH_JOB_LOG_CLEANUP_ENABLED      日志自动清理，默认 true
//	CCH_JOB_LOG_CLEANUP_TICK_MS      日志清理到点判定间隔，默认 3600000（1 小时）
//	CCH_JOB_AVAIL_BACKFILL_INTERVAL_MS 回填检查间隔，默认 300000（5 分钟）
//	CCH_CLOUD_PRICE_TABLE_URL        价格表地址，默认 https://cch-plus.com/pricing/v1/models.json
//
// 为什么默认开启：本轮目标是 Node 完全下线后由 Go 独占这些职责；若默认关闭，Node 停掉后
// 价格表与可用性投影会静默停摆。代价是 Node 与 Go 并存期间价格同步会双跑——两边的写入内容
// 相同且各自持有 advisory lock，风险与规避见 README.md「并存期」一节。
package jobs

// cptSchemaID 是 CPT v1 价格表的 schema 标识（cpt-schema.ts:14）。
const cptSchemaID = "cchp.pricing-table/v1"
