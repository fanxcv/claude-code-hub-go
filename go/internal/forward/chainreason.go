package forward

// 本文件是 Go 侧**写入 `provider_chain[].reason` 的唯一词表**。
//
// 为什么需要它：`provider_chain[].reason` 是**跨语言数据契约**——Node 与 Go 写同一列、
// 同一批消费者读（仪表盘、用量日志、公开状态投影、`fn_is_message_request_finalized`）。
// 消费者按**精确词**判定（例如公开状态分类器认 `request_success` 才算成功、认
// `client_error_non_retryable` 才排除），所以写入侧哪怕只是同义改写，也会让消费者
// 判错，而且**对拍形状时看不见**（列照写、类型照对，只是值不在词表里）。
//
// 实测教训（2026-09-13）：Go 曾写 `success` 与 `non_retryable_client_error` 两个
// **只属于自己**的词。前者不在成功词表里 → 每次成功被算成失败、可用率恒 0；后者不在
// 排除词表里 → 本应排除的本地不可重试错误被算成失败。
//
// 词表来源（Node 侧事实来源，改动时同步核对）：
//   - 取值域：`src/types/message.ts` 的 `ProviderChainItem.reason` 联合类型；
//   - 成功/中性/排除分类：`src/lib/public-status/request-outcome.ts` 的
//     `SUCCESS_REASONS` / `NEUTRAL_REASONS` / `EXCLUDED_REASONS`
//     （Go 侧镜像见 `internal/pubstatus/rollup.go`）；
//   - 终态白名单：库内函数 `fn_is_message_request_finalized`
//     （Go 侧镜像见 `internal/terminal/state.go` 的 `finalizedChainReasons`）。
//
// 纪律：**本包只允许写这里列出的词**。新增词前先确认 Node 侧同词存在（钉子在
// `chainreason_test.go`：写侧词表 ⊆ Node 取值域）。
const (
	// ReasonRequestSuccess 是首次尝试成功的结局原因（Node `request_success`）。
	//
	// 注意：Node 侧**不存在**裸 `success` 作为链原因，且 `successReasons` 只认本词
	// 与 `retry_success`/`hedge_winner`——写错会让成功被算成失败。
	ReasonRequestSuccess = "request_success"
	// ReasonRetryFailed 是供应商错误（已计入熔断器）的结局原因。
	ReasonRetryFailed = "retry_failed"
	// ReasonSystemError 是系统/网络错误（不计入熔断器）的结局原因。
	ReasonSystemError = "system_error"
	// ReasonClientAbort 是客户端在响应完成前断开。
	ReasonClientAbort = "client_abort"
	// ReasonClientErrorNonRetryable 是不可重试的客户端错误（Prompt 超限、内容过滤等）。
	//
	// Node 的排除词表（`EXCLUDED_REASONS`）只认本词；写 `non_retryable_client_error`
	// 之类同义改写会让它落在「失败」兜底分支。
	ReasonClientErrorNonRetryable = "client_error_non_retryable"
	// ReasonResourceNotFound 是上游 404（触发故障转移但不计熔断器）。
	ReasonResourceNotFound = "resource_not_found"
	// ReasonLocalOverload 是本进程过载（本地准入拒绝）。
	ReasonLocalOverload = "local_overload"
	// ReasonVendorTypeAllTimeout 是供应商类型全端点超时（触发 vendor-type 临时熔断）。
	ReasonVendorTypeAllTimeout = "vendor_type_all_timeout"

	// 竞速（hedge）结局：Node 同名 `hedge_*` 词，均在链上留痕。
	ReasonHedgeLaunched    = "hedge_launched"
	ReasonHedgeWinner      = "hedge_winner"
	ReasonHedgeLoserCancel = "hedge_loser_cancelled"
	ReasonHedgeLoserBilled = "hedge_loser_billed"
)
