// Package route 复刻 src/app/v1/_lib/proxy/provider-selector.ts 的供应商选路。
//
// 语义来源（逐条对照，勿凭印象改）：
//   - 分组可见性：src/lib/utils/provider-group.ts 的 parseProviderGroups /
//     resolveProviderGroupsWithDefault（分隔符为英文逗号、中文逗号、换行、回车；trim 后丢弃空项）；
//   - 模型允许集：src/lib/allowed-model-rules.ts 的 matchesAllowedModelRules +
//     src/lib/model-pattern-matcher.ts（exact/prefix/suffix/contains/regex，regex 解析失败时回退 glob）；
//   - 格式兼容与跨协议可服务：Go 侧 convert.ResolveProtocolCompat / ResolveUpstreamPath；
//   - 优先级分层：provider-selector.ts 的 selectTopPriority（分组覆盖取最小值）；
//   - 加权选择：provider-selector.ts 的 selectOptimal（先按成本倍率升序，再按权重加权随机）；
//   - 熔断状态：src/lib/circuit-breaker.ts / vendor-type-circuit-breaker.ts /
//     endpoint-circuit-breaker.ts 的 Redis 键形制与开闭判定；
//   - 前缀亲和：src/app/v1/_lib/proxy/affinity/fingerprint.ts 与 affinity-store.ts。
//
// 三条设计纪律：
//  1. **选路可解释**：每次选择都产出「选中项 + 候选 + 每个被过滤项的枚举理由」，落 DecisionContext，
//     与 Node 的 provider_chain[].decisionContext 同形（JSON 字段名逐字对齐）。
//  2. **选路不读正文**：本包只吃快照、端点上下文与已解析的模型名；正文只在亲和指纹处按需解析，
//     且由调用方显式提供解析后的 body（见 affinity.go 的 FingerprintInput）。
//  3. **随机源可注入**：加权选择的随机数由 Options.Rand 提供，同一快照 + 同一随机序列必须复现同一结果。
//
// 与本包有意保留的差异（后续波次按需收敛，见各文件注释）：
//   - 金额与并发限额、调度时间窗口、客户端限制三个维度在本包只留 Gates 钩子（nil = 不判定），
//     因为它们依赖 store 尚未暴露的列与尚未移植的 client-detector；
//   - 亲和为「最小可用」：只做最长前缀读取与软提名，generation fence 的校验与墓碑写入留在 W3；
//   - gemini / gemini-cli 指纹未实现（Node 有四条线，本包先做 claude / openai / response）。
//
// 进程内缓存不在本包：Node 侧 providers 与 provider_endpoints 各有 TTL 缓存 + pub/sub 失效，
// Go 侧由接线波次用 cfgsync.KeyedCache 包住 Source 实现，本包保持「每次调用即一次读」的纯语义。
package route
