# internal/ws — `/v1/responses` 的客户端 WebSocket 通道

本包是 Node `server.js` 里 `handleWebSocketConnection` 的 Go 版本，形态是**薄隧道**：

```
客户端 WS 文本帧 {"type":"response.create", ...}
   → POST /v1/responses（带 x-cch-* 隧道标记，stream 强制为 true）
   → 数据面（守卫链 / 选路 / 转发 / 终态结算，与 HTTP 路径同一套代码）
   → 响应 SSE 的每个事件翻回一个 WS 文本帧
```

与 Node 的唯一结构差异：Node 经私有 loopback 监听器做一次真实 HTTP 往返，本包直接调用进程内的
HTTP 处理器（`cmd/cchd/ws.go` 传入的 `api.Handler()`）。因此归属判定、排空闸门、在途计数与请求
日志对 WS 轮次同样生效，WS 上不需要另建一套账。

## 挂载与归属

`cmd/cchd/ws.go` 在进程入口做一次分流。**升级请求的方法是 GET，而归属白名单通常只写
`POST /v1/responses`**，所以分流必须按「WS 与同路径 HTTP 归属一致」的语义补一次判定：
用同一份规则、以 POST 作为等价事实判归属；判给 Go 才接管，判给 Node 原样下沉（由前门把升级
反代给 Node，与改造前一致）。

## 已实现（与 Node 对齐的部分）

| 行为 | 口径 |
| --- | --- |
| 帧类型校验 | 只接受文本帧；二进制帧回 `invalid_frame_type` 并按 1003 关闭 |
| 帧内容校验 | `invalid_json` / `invalid_frame`（非对象）/ `unsupported_event_type`（type 不是 `response.create`，含数组与缺 type）三类，**都不关闭连接** |
| 正文改写 | 去掉 `type`；`?model=` 仅在帧里缺 model 时补位；强制 `stream: true`；丢弃 `background`；其余字段逐字节保留（`json.RawMessage`，不经 float64 往返） |
| 队列与背压 | 排队上限 64 帧 / 64 MiB，超限回 `too_many_requests` 并按 1008 关闭；同一连接串行一轮（`drain` 同构） |
| SSE 翻译 | 事件分隔符认 `\n\n`、`\r\n\r\n`、`\n\r\n`、`\r\n\n`；`data:` 的 JSON 原样成帧；非 JSON 的 data 翻成 `response.output_text.delta`；`[DONE]` 且无终态时补 `response.completed` |
| 终态 | `response.completed` / `failed` / `incomplete` / `error` 为终态；**终态后不关闭连接**，同一连接可继续下一轮 |
| 缺终态 | SSE 结束但没发过终态 → `stream_ended_without_terminal` 并按 1011 关闭 |
| 非 SSE 响应 | 整体缓冲（上限 1 MiB）：`>= 400` 翻成带 `status` 的 `error` 帧，其余翻成 `response.completed`；HTTP 错误**不关闭**连接 |
| 单帧上限 | 出站 1 MiB（超限 `internal_sse_event_too_large` / `internal_response_too_large`）；入站 32 MiB（Node 的 `WS_MAX_PAYLOAD_BYTES`） |
| 客户端中断 | 读失败或写失败即取消连接上下文，进而取消在途隧道请求 → 数据面按客户端中断中断上游并结算 |
| Origin | 不校验（Node 的 `ws` 没有 `verifyClient`）；安全边界是凭据校验与入站 `x-cch-*` 头剥离 |

## 未实现项（明确回退，不静默丢弃）

1. **上游 WebSocket 建连已实现**（2026-09-19，见 `internal/upws` 与 `internal/forward/ws.go`）：
   资格四条全真时先试上游 WS，走不成则回落 HTTP 且**不留失败痕迹**（`responses_ws_fallback`
   入链、`downgradeReason` 落到链项字段、不计熔断）。本包不参与该路径——它只负责客户端侧
   帧翻译，并写下资格判定要用的那个隧道标记。
2. **端点不支持的短期缓存已实现**：在 `internal/upws` 的拨号器里（进程内 map + TTL），
   命中即不发起握手，也不记成「尝试过但降级」。
3. **本版不做连接池化**：上游单连接串行且服务端有 60 分钟上限
   （`websocket_connection_limit_reached`），池化必须处理这两件事，而收益只是省一次握手。
   当前每 turn 新建连接、turn 结束关闭。
4. **每进程内部密钥不是本包的安全边界**：隧道在进程内完成，没有可被外部伪造的 socket；
   密钥仍随隧道请求携带，使数据面将来若照 Node 的 `verifyInternalRequest` 判定也成立。
   真正的边界是连接建立时剥掉客户端自带的全部 `x-cch-*` 头。

## 验证

- 单元与假数据面路径：`go test ./internal/ws/`（成功往返、鉴权失败、上游提前收尾、客户端中断、
  帧级协议错误不关连接、二进制帧关连接、非升级流量透传、分隔符跨 chunk）。
- 入口分流：`go test ./cmd/cchd/ -run TestWebSocketEdge`（POST 路由归 Go 时接管、归 Node 时下沉、
  普通请求不受影响）。
- 真实进程（mock 上游 64 KiB 流 + `CCH_EGRESS_MODE=go` `CCH_EGRESS_ROUTES='POST /v1/responses'`）：
  同一连接两轮 `response.create` 各收到 delta 与 `response.completed`，`message_request` 恰增两行
  （每轮一行、只结算一次）；错误密钥收到 `{"type":"error","status":401}` 且连接保持打开。
