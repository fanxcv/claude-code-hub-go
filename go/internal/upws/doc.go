// Package upws 是**上游** Responses WebSocket 拨号层（codex 类供应商专用）。
//
// 它把「与上游建 WS → 发一帧 response.create → 把服务端逐帧事件合成 SSE 字节流」收敛成
// 一个产出 *dial.Response 的拨号器：调用方（forward）拿到的东西与 HTTP 拨号**完全同形**
// （状态码、headers、Body 是一个能读出 SSE 的 io.ReadCloser），于是门控、整流、结算、
// 留痕四条下游路径零改动——这正是 Node 的 responses-ws/upstream-adapter.ts 的接缝语义。
//
// 与「客户端侧 WS」（internal/ws）的分工：internal/ws 面向客户端收帧/发帧；
// 本包只面向上游，且只负责把上游帧翻成 SSE 字节——它不知道客户端存在。
//
// 协议事实（来源：openai/codex 源码 + Azure 官方文档，核实记录见仓库审计报告）：
//
//   - 端点：base 拼 `/responses`，scheme `http→ws` / `https→wss`；**无 `?model=`**
//     （模型在首帧 body 内）。两轨：`wss://api.openai.com/v1/responses`（API key 轨）与
//     `wss://chatgpt.com/backend-api/codex/responses`（ChatGPT/OAuth 轨）——本包按端点 URL
//     推导，不区分轨，端点拒不支持即短期不再试。
//   - 握手头：`Authorization`、`OpenAI-Beta: responses_websockets=2026-02-06`（硬编码无条件插入）、
//     `originator`、`User-Agent: codex_cli_rs/<ver>`、`session-id`/`thread-id`/`x-client-request-id`。
//   - 首帧：`{"type":"response.create", <Responses create body>}`。
//   - 服务端每帧即 Responses 事件 JSON：**无 `data:` 前缀、无 `[DONE]`**，以
//     `response.completed` 收口（`response.failed`/`response.incomplete` 亦为终态）。
//   - 生命周期：单连接**串行、不支持多路复用**；服务端有 **60 分钟连接上限**
//     （超限回 `websocket_connection_limit_reached`）。本版**每 turn 新建连接、turn 结束关闭**，
//     不做池化——池化要处理串行与 60 分钟轮换两件事，而收益只是省一次握手，风险不划算。
//
// 一条红线：**上游 WS 失败绝不能计入熔断**。本包只把「为什么没走成」作为事实返回
// （Outcome.Reason），由调用方决定留痕；这里不做任何记账，也不返回 Failure。
package upws
