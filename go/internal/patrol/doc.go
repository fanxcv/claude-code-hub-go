// Package patrol 周期巡检「已开行但从未终态」的 message_request 并补写终态。
//
// 存在理由：开行在守卫链的 messageContext 步骤，终态由数据面在流结束（可能晚于 handler
// 返回）时写回；两者之间进程被杀——滚动重启、部署、SIGKILL——就会留下
// status_code IS NULL 且 updated_at = created_at 的行。终态列只允许写一次
// （谓词 status_code IS NULL），因此这些行是**永久**缺口：token、时长、成本全缺，
// 而 usage_ledger 那侧的 is_success 判据是 error_message，NULL 会被当成成功。
// 根因与剂量-反应表见。
// 终态口径（从 Node 源码取证，本包不发明新状态码）：
//
//   - 客户端先于终态断开：src/app/v1/_lib/proxy/response-handler.ts:2337-2339
//     → status_code = 499、error_message = "CLIENT_ABORTED"。
//   - 我方流未正常结束：同文件 2340-2342 → 502 与 abortReason ?? "STREAM_ABORTED"。
//
// 本包取前者。理由：巡检只能看到「行没终态」，看不到是断线还是进程退出（两者在库内没有
// 任何可区分的痕迹——缺失结算的日志留痕实测为 0 条），而断线是已证实的实际触发条件；
// 取值 499/"CLIENT_ABORTED" 与 Node 在「客户端中断」这一情形下写在库里的**完全相同**。
// 两者都保守（不计成功、不计成本），但 499 还有一个可辩护的性质：它在
// fn_compute_message_request_success_rate_outcome 里落 'excluded' 而非 'success'，
// 因此不会把一个来路不明的行抬成成功。已知边界：若某行其实是「进程退出而上游仍在流」，
// Node 会写 502/STREAM_ABORTED——巡检无法区分，故登记为已知差异（报告内）。
//
// 三条不变量（与 go/internal/terminal 同源）：
//
//	P1 只补 status_code IS NULL 的行：复用 store.UpdateDetailsIfUnfinalized 的谓词，
//	   并发两轮或「真实终态后到」都只有一个赢家，已终态行绝不覆盖。
//	P2 单语句终态：message_request_outbox_aiud 只在 status_code 首次非 NULL 的那条
//	   UPDATE 上产生事件，故 status_code 与 error_message/error_stack 同语句落库。
//	   不写 duration_ms：探针实测这类行的 duration_ms 与 provider_chain 同时为 NULL
//	   （335/335，见 go/testdata 与报告），而 I3 的 duration_ms 要求只在
//	   provider_chain 非空时成立；编造一个时长比留空更糟。
//	P3 不写任何成本列：巡检无从知道真实用量，写 0 或写估值都是伪造账目。
//
// 残余风险（须在部署侧知道）：没有任何全局上限约束流式响应的总时长
// （providers.streaming_idle_timeout_ms 默认 0 = 不限制，实测最长 347.5 s、p99.9 300 s），
// 因此阈值取得不足时，巡检可能先给一条**仍在流中**的行补终态，随后真实的终态写入会被
// 谓词挡掉（那次请求的用量与成本就真的丢了）。默认阈值取 30 分钟：>5× 实测 p99.9、
// >3× 实测最大值。任何调小该值的改动都必须先证明「本部署不存在超过阈值的活跃流」。
package patrol
