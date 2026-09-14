# internal/dataplane

数据面装配层：把入站 `/v1` 请求接成「守卫链 → 选路 → 转发 → 终态结算 → HTTP 响应」。
业务判定不在本包（分别属 `internal/guard`、`internal/route`、`internal/forward`、
`internal/terminal`），本包只负责接线、投影与把结果写回。

## 由 Go 承载的路由

| 方法 | 路径 | 守卫预设 | 客户端格式 | 协议族 |
| --- | --- | --- | --- | --- |
| POST | `/v1/messages` | chat | claude | anthropic-messages |
| POST | `/v1/chat/completions` | chat | openai | openai-chat |
| POST | `/v1/responses` | chat | response | openai-responses |

同一路径的流式与非流式共用一条处理器：客户端是否要流由转发计划里的 `ClientStream` 决定
（`stream: true`），响应是不是流由上游的 `Content-Type` 决定（SSE 逐帧 flush，非流式整段写回）。

## 回退 Node（不是 501）

未列在 `routes.go` 的 `routeTable` 里的路径一律交回前门给 Node：

- `GET /v1/models`、`/v1/responses/models`、`/v1/chat/completions/models`、`/v1/chat/models`：
  聚合式模型列表（可用模型 + 价格 + 厂端点探测），不是转发路径。
- `POST /v1/messages/count_tokens`、`POST /v1/responses/compact`：属**原始透传**策略，
  它要求「不做转发前预处理」，而 `forward` 目前只表达「不重试、不切换」
  （`Candidate.RawPassthrough`），没有「不转换、不改写」的开关。
- `/v1/embeddings`：同上传处理由后续波次按需加入。
- `/v1beta/*`（Gemini 原生）：尚无 codec。

Responses WebSocket 不在此列，也不在本包的 `routeTable` 里：`/v1/responses` 的升级请求由
`internal/ws` 在**进程入口**按「与同路径 HTTP 同一份归属规则」分流（升级请求是 GET，而归属
白名单通常只写 POST，故以 POST 作等价事实判定），判给 Go 才接管，否则连升级请求一起下沉 Node。
本包只承非升级的 HTTP 流量。

**为什么回退而不是 501**：501 会让客户端把「Go 还没实现」当成「协议不支持」而放弃重试。
回退是双跑过渡期的正常形态——前门按 `CCH_EGRESS_ROUTES` 判定归属，判给 Go 但本包没有
路由的路径原样反代到 Node。

## CCH_EGRESS_ROUTES 用法

归属规则决定「哪些请求归 Go」。语法：`<method> <path>[:<from>-><to>][@<provider>]`，
逗号分隔，`*` 结尾表示前缀匹配。渐进接管的两步：

```bash
# 1) 先只接管一条路由，其余全部回退 Node
CCH_EGRESS_ROUTES="POST /v1/messages" CCH_EGRESS_MODE=go

# 2) 三条主路由全接管（模型列表等仍在 Node）
CCH_EGRESS_ROUTES="POST /v1/messages,POST /v1/chat/completions,POST /v1/responses" CCH_EGRESS_MODE=go

# 3) 按方言收窄（只有客户端侧声明 anthropic 的才归 Go）
CCH_EGRESS_ROUTES="POST /v1/messages:anthropic-messages->anthropic-messages" CCH_EGRESS_MODE=go
```

进程侧还有两点约束（见 `cmd/cchd/boot.go`）：

- 没有 DSN 时不装配数据面（骨架模式），前门把 `/v1` 全部回退 Node。
- 有 DSN 但装配失败时**不启动**：带着「判给 Go 却没有数据面」的状态启动，会让前门把请求
  交给一个必然 503 的处理器，比不启动更难排障。

## 已接线的缝隙与仍留的缺口

启动日志的 `dataplane_ready.gaps` 会如实列出仍留的缺口，排障时先看它。

已接线（装配见 `assemble.go`，各有单测与真实依赖集成测试）：

| 缝隙 | 实现 | 接线要点 |
| --- | --- | --- |
| `SessionBinder` | `session.NewSessionBinderAdapter` | 需 `StoreOptions.Redis`（会话与 Node 共用同一套键与 Lua）；绑定结果经 `sessionCapture` 按请求记录，供请求日志开行取用 |
| `IPExtractor` | `guard.IPExtractorAdapter` | 规则链取自 `system_settings.ip_extraction_config` 快照；形状非法时退回默认链 |
| `ForwardRules` | `guard.ErrorRuleCache` | `error_rules` 快照（`cfgsync.DomainErrorRules`）；判定顺序 contains -> exact -> regex，与 `error-rule-detector.ts` 一致 |
| `Fake200Detector` | `guard.Fake200Detector` | 只认强信号（HTML 文档 / 顶层 `error` 非空 / OpenAI Responses failed），状态码推断不出时回 502 |
| `VersionChecker` | `clientver.NewChecker` | UA 解析（`ua-parser.ts` 同正则与同 `includes` 判定）、用户版本记录（`client_version:{type}:{id}`，TTL 7 天）、GA 版本比对（`ga_version:{type}` 缓存 + 活跃用户分布计算，阈值 2）。fail-open：任何一步出错都放行 |
| `CircuitBreakerAccounting` | `health.NewWriter` | 失败与成功**成对**接线（`forward.Deps.RecordFailure` / `RecordSuccess`）：只接失败会让开闸后再无归闭路径。供应商级、端点级、厂级三档写入，键名/字段/TTL 逐字对齐 Node（见 `internal/health` 包注释的 TS 行号）；端点级只记超时（524）与系统错误，与 Node `forwarder.ts:2569` 同一判定 |
| `ReplayAttacher` | `replay.NewAttacher` + `dataplane/replay.go` | 命中短路（守卫步直接返回缓存响应，不拨上游、不占配额）；未命中登记 owner 并在**响应路径**上建 spool、逐块喂入客户端可见字节；`completed` 只在终态为「干净完成」且**计费落库之后**出现（见 `streamSettler.SettleStream` 的次序注释）。失败终态一律 abort——半截流比 miss 更糟。已知简化：owning 条目仍视为 miss（不吐半截流、不做 live dedup 跟尾），并发重复请求各自拨上游，见 `internal/replay/doc.go` |
| 流式竞速（hedge） | `forward.ForwardStreamHedge` + `dataplane/hedge.go` | 条件与 Node `shouldUseStreamingHedge`（`forwarder.ts:4910-4921`）逐条对齐：端点策略允许重试与切换、客户端正文 `stream === true`、选中供应商 `first_byte_timeout_streaming_ms > 0`、未处于绑定/租约冲突态。并发上限取 `system_settings.legacy_hedge_max_in_flight` 并钳到 `[1,4]`，输家计费取 `bill_hedge_losers`，引流上限取 `HEDGE_LOSER_DRAIN_TIMEOUT_MS`。输家成本累加走 `store.AddHedgeLoserCost`（幂等在 SQL 谓词里）。**已知差异（保守侧）**：`discovery_enabled=true` 时 Go 一律走串行（Node 走 Discovery 竞速、只在预备失败时回落 legacy hedge）；等 Discovery 移植时收口 |
| `CostAccounting` | `newCostResolver` + `storeSettler.costs` | 取价基准由 `system_settings.billing_model_source` 决定，**重定向后**的模型名随计划到达结算面，倍率取供应商与分组求交 |

仍留的缺口（口径同 `Assembly.Missing`：**只在缺件时**出现，不是「未实现」的同义词）：

| 缺口 | 出现条件 | 运行期表现 | 卡点 |
| --- | --- | --- | --- |
| `RateLimit` | `StoreOptions.RateLimit` 未注入 | 不限流 | 实现来自 `internal/limit`，由装配方注入（见 `guard.AdapterOptions`）；未注入时链上各留一条 warn，启动日志同步列出 |
| `AuthThrottle` | `StoreOptions.AuthThrottle` 未注入 | 不节流 | 同上 |
| `SessionBinder` | 未配置 Redis | 会话 id/序号两列写 NULL | 配置缺口而非接线缺口（会话绑定与 Node 共用 Redis 键与 Lua） |

熔断记账的已知边界（都不影响「开闸/归闭」这对主循环，属通知与策略面）：

- 开闸告警 webhook 未接：`health.Options.OnProviderOpened` / `OnEndpointOpened` 是留给通知层的回调缝，
  当前为 nil（Node 在开闸时查供应商名并发 webhook）。
- 端点策略开关 `allowCircuitBreakerAccounting` 未建模：Node 在端点策略里可以逐端点关闭熔断记账，
  Go 侧 `forward.Endpoint` 尚无该字段，故当前一律记账（比 Node 更严，不会漏记）。

`provider_chain` 已落库（流式与非流式都写）：其中只有「本层确实知道的事实」（身份、权重、优先级、成本倍率、
分组标签 + 尝试号与耗时）；`reason` 取**尝试自己的结局原因**（`request_success` / `hedge_winner` /
`hedge_loser_billed` / `hedge_slot_saturated`…），与 Node 的 `addProviderToChain` 同口径；
选路器的 `decisionContext` 尚需由选路包回传，属后续波次。

## 测试

```bash
# 不依赖数据库：假守卫缝隙 + httptest 上游（链 → 选路 → 转发 → 写回）
go test ./internal/dataplane/

# 真实依赖：三条路由 × 流式/非流式，断言状态码、SSE 逐事件、一行终态、用量落库
CCH_TEST_DSN="postgres://user:pw@127.0.0.1:5432/db" go test ./internal/dataplane/ -run TestIntegration
```

集成测试会自造一个最高优先级的供应商并用完即删；它借用库里既有的可用密钥，因此**不会**
改动用户与密钥数据。
