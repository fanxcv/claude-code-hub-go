// Package terminal 负责 /v1 数据面的终态结算：请求建行、终态落库、成本与用量的
// 定点计算。它复刻 src/repository/message.ts 的写入编排与
// src/lib/utils/cost-calculation.ts 的成本口径，且不重复实现 SQL——所有语句都经
// go/internal/store 已有的写入 API。
//
// 不变量：
// I1 归属唯一：终态写带 `status_code IS NULL` 谓词（store.UpdateDetailsIfUnfinalized），
// 只有拿到 true 的调用者拥有该行的终态以及其后的对外可见副作用。
//
// I2 不写账本：usage_ledger 由 trg_upsert_usage_ledger → fn_upsert_usage_ledger 产生，
// 本包绝不直接写它。
//
// I3 单语句终态：message_request_outbox_aiud 仅在 status_code 首次变为非 NULL 的那条
// UPDATE 上产生 outbox_events（TG_OP='UPDATE' AND OLD.status_code IS NOT NULL 时提前返回），
// 事件载荷取自该语句执行后的行状态。故使 status_code 非 NULL 的语句必须同时带上
// duration_ms，否则事件会永久缺少 duration_ms（见 ValidateTerminalPatch）。
//
// I4 迟到写不得带监视列：终态提交之后的补写（routing_trace 等）不得触碰触发器监视的列，
// 否则账本行被重写、outbox 语义被破坏（见 MonitoredColumnsInLatePatch）。
//
// I5 等待可注入：重试与退避由 Options 注入，测试用毫秒级，绝不等待真实长超时。
//
// I6 异步写不改变终态语义：MESSAGE_REQUEST_WRITE_MODE=async 时终态与成本由 batch.go 的
// 单 writer 队列写入，但**提交后的副作用仍在写入返回 committed 之后才发**（与同步同闸门）；
// 队列有界，满时降级为同步写而不是丢弃。默认 sync 时队列根本不存在，路径与接线前一致。
//
// 有意未搬运（不在本波范围，留待后续波次）：
//   - long-context 分层价格、priority service tier、图片 token 计费；
//   - 按 id 合并 patch（同一行的多次写入分属不同阶段且列不重叠，合并收益低、风险高）；
//   - hedge 输家成本的迟到累加（本波的成本写入已按 hedge 安全语义走 WinnerCost，
//     输家入口留待 W3）。
package terminal
