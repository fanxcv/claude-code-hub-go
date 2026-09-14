// Package pubstatus 是 public-status 的**配置投影发布面**（Node 侧
// src/lib/public-status/{config,config-snapshot,config-publisher,vendor-icon-key}.ts）。
//
// 它做的事只有一件：把「启用公开状态的供应商分组 + 其公开模型 + 最新价格/图标」压成一份
// public-safe 快照，写进 Redis 的版本化键，并推进 current 指针。公开路由
// （/api/public-site-meta、/api/public-status）读的是这份快照，不查库。
//
// 两套快照并行存在（照 Node）：
//
//	public     <prefix>:config:<version>          页面/公开 API 用（已裁剪）
//	internal   <prefix>:config-internal:<version> 内部用（带源分组 id/名）
//
// 键布局与 TTL 是**硬契约**（src/lib/public-status/redis-contract.ts:1-190）：
// 版本化键 30 天 TTL（每次发布都铸新版本，不设 TTL 会无限堆积），三个 current 指针键
// **不设 TTL**（长期不发布就整体变暗）。
//
// 未移植（不在本包范围，如实登记）：聚合侧的 manifest/series/snapshot/rollup 键与
// rebuild-worker（5475 行那套），那是「已发布配置 -> 时间序列聚合」的另一半。
package pubstatus
