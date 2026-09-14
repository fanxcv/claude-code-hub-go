// Package store 复刻 src/repository/ 与 src/drizzle/ 的持久化面：连接池分道与准入、
// message_request 的创建与终态写入、usage_ledger 的只读口径、proj_applied_requests 投影写入，
// 以及数据面所需的只读查询。
//
// 与 TS 侧的一一对应关系（任何 parity 争议都能据此定位）：
//
//	src/drizzle/db.ts                    -> pool.go（分道、准入、application_name）
//	src/drizzle/admitted-client.ts       -> pool.go（AdmissionError 与错误码）
//	src/lib/utils/currency.ts            -> money.go（formatCostForStorage）
//	src/repository/message.ts            -> message.go
//	src/repository/usage-ledger.ts       -> ledger.go
//	src/repository/_shared/ledger-conditions.ts -> ledger.go（计费条件）
//	src/lib/availability/projection-worker.ts   -> projection.go
//	src/repository/{provider,provider-endpoints,key,system-config,model-price}.ts -> read.go
//
// 本包不做 DDL、不建表、不改 schema：表结构由 TS 侧的 drizzle 迁移管理，Go 只消费。
// 表与列的对应关系由 schema_drift_test.go 对着 information_schema 实测断言，
// 而不是靠人工抄写列清单。
package store
