// Package rectify 移植 Node 的六个请求正文整流器
// （`src/app/v1/_lib/proxy/*-rectifier.ts`，注册与接线见 `forwarder.ts:1244-1500`、`2653-2697`、`3463-3500`）。
//
// 两类，不可混为一谈（Node 本身就是两套机制）：
//   - **被动型（reactive，4 个）**：上游报错文案命中 → 整流请求正文 → **对同一供应商重试一次**。
//     幂等状态按供应商轮次重置（`RetryState`），命中即终结、不走后续整流器。
//   - **主动型（proactive，2 个）**：发送前直接规范化，不带触发词、不带重试。
//
// 三条硬约束（改错任一条都会偏离 Node 语义）：
//  1. **整流的是客户端正文**，不是转换后的上游正文：Node 整流的是 `session.request.message`，
//     协议转换在其后发生。故本包只认客户端方言字段（anthropic `system`/`messages`、
//     gemini `contents`/`request.contents`、responses `input`）。
//  2. **注册表顺序即优先级**：anthropic 组顺序是 effort → signature → budget。effort 必须在前，
//     否则它的具体文案会被 signature 的「invalid request」通用兜底吞掉。
//  3. **正文模型用 convert.Value**（保序、JS JSON.stringify 语义）：Node 用 structuredClone
//     保留键序并原地删键，用 map + encoding/json 会重排键序、丢数字字面量。
//
// 主动型的 responses `input` 归一走的是守卫链的 map 通道（守卫正文缝隙本来就是 map），
// 故本包对它用 map[string]any —— 这是两条既有缝隙各自的既有类型，不是两套实现。
package rectify
