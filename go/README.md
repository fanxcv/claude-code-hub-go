# cchd（Go 数据面）

本目录是 CCH 的后端进程 `cchd`：数据面（代理与协议互转）、管理面（REST）、迁移器与页面服务
都在这里。构建、部署与配置见仓库根 [README.md](../README.md)；**逐包的完整清单**见
[AGENTS.md](../AGENTS.md) 的「目录地图」一节（本文件只做分层速览，避免两处各写一份而同漂）。

已交付的进程语义：

- **端口与归属**：独占公共 `PORT`（默认 23000），承载全部路径——数据面 `/v1`、`/v1beta`，
  管理面 `/api/**`，以及非 API 路径。**进程内没有任何回退后端**（Node 后端已完全退役）：
  未实现的路径一律自答 404（见 [internal/egress/README.md](internal/egress/README.md)），
  绝不返回 501——501 会被客户端当成协议不支持而放弃重试。
- **页面面**：`CCH_EGRESS_PAGES=embed` 时由 `//go:embed` 的静态导出产物作答（[internal/uiapp](internal/uiapp)，
  产物缺失时启动就 fail fast，不静默降级为空页面）；`off` 时不服务页面。默认 `off`，
  容器部署需显式开启（compose 模板已代设）。
- **探针**：`/readyz` 逐项报告自身监听、PG、Redis、规则快照，未就绪时 503 且在 `failing`
  里点名是哪一项；`/v1/_ping` 与 `/readyz` **不经前门**，排空窗口内也照答。
- **启动顺序**：配置（非法即 fail fast）→ 依赖 → 订阅连接 → 登记配置域 → 前门 → 监听。
  监听先于配置域装载完成，冷启动期探针必须有人应答。
- **规则就绪门**：`rulesSync` 的登记表**全部装载成功**才把快照标记为已装载。该登记表当前为空
  （数据面在装配层自行绑定配置域，不经这份表），故快照在订阅连接 Ping 通过后即标记已装载，
  启动日志 `rules_domains_registered` 会写明 `count` 与实际驱动来源。
- **配置域失效订阅**：guard 装配层绑定 `providers`、`provider_endpoints`、`api_keys`、
  `system_settings`、`sensitive_words`、`request_filters`、`error_rules`（见
  [internal/guard/adapters.go](internal/guard/adapters.go)），任一端写库即就地失效对应缓存，
  无需重启进程。
- **关闭**：SIGTERM/SIGINT → 停止接纳新请求（窗口内新请求 503 `backend_switching`）→ 等前门
  在途归零（超时 30s，可用 `DrainTimeout` 调整）→ 关订阅 → 关依赖。排空超时按非零退出码结束，
  在途请求被强退属审计事件，不能被静默吞掉。

## 构建与验证

```bash
cd go
gofmt -l .                  # 须为空
go vet ./...
go test -timeout 300s ./... # 真库用例需 CCH_TEST_DSN / CCH_TEST_REDIS_URL，未注入时整组跳过
go build ./cmd/cchd
```

## 模块布局（分层速览）

| 层 | 包 | 职责 |
| --- | --- | --- |
| 入口与装配 | [cmd/cchd](cmd/cchd) | 启动顺序、前门装配、管理面装配、排空关闭；[cmd/envlist](cmd/envlist)、[cmd/jobsrun](cmd/jobsrun)、[cmd/uipoc](cmd/uipoc) 是运维/验证小工具 |
| | [internal/dataplane](internal/dataplane) | 把守卫链、选路、转发、门控、结算串成一条请求链（唯一装配点） |
| | [internal/pctx](internal/pctx) | 跨步传递的请求上下文：归属一次性决定、正文一次性消费、headers 只读视图 |
| 入站与配置 | [internal/ingress](internal/ingress) | 入站正文读取、流式解压、进程级内存与在途字节准入 |
| | [internal/config](internal/config) | 环境变量装载与校验（非法即 fail fast） |
| | [internal/cfgsync](internal/cfgsync) | 配置域快照与失效订阅（含 in-flight 合并） |
| | [internal/deps](internal/deps) | PostgreSQL（pgx/v5）与 Redis 连接与探测 |
| 数据面链 | [internal/guard](internal/guard) | 守卫链（顺序即契约）与各步骤的真实适配器 |
| | [internal/route](internal/route) | 选路：前缀亲和、分组与优先级、熔断过滤、候选与决策留痕 |
| | [internal/dial](internal/dial) | 上游拨号：请求构造、三档超时、字节透传、连接准入 |
| | [internal/forward](internal/forward) | 转发主干：重试、竞速（hedge）、流式泵与客户端断线计量 |
| | [internal/gate](internal/gate) | 流式门控：首个可提交内容之前的缓冲与上游切换时机 |
| | [internal/convert](internal/convert) | 三线（anthropic / chat / responses）协议编解码 |
| | [internal/health](internal/health) | 供应商熔断状态机与等待阶梯 |
| | [internal/limit](internal/limit)、[internal/ratelimit](internal/ratelimit) | 限流与配额的语义层、Redis 脚本调用与降级分类 |
| | [internal/session](internal/session)、[internal/replay](internal/replay) | 客户端会话身份与绑定、请求回放的两层存储 |
| | [internal/ws](internal/ws) | `/v1/responses` 的客户端 WebSocket 通道 |
| 存储与结算 | [internal/store](internal/store) | 持久化面：池分道与准入、请求行与账本的读写 |
| | [internal/terminal](internal/terminal) | 终态结算：建行、终态落库、用量与成本定点计算 |
| | [internal/migrate](internal/migrate) | 迁移器：内嵌 drizzle 迁移、水位账本、自愈与索引 preflight |
| 管理面 | [internal/adminapi](internal/adminapi) | 管理面 REST：路由注册表、守卫、审计条目与各资源处理器 |
| | [internal/adminauth](internal/adminauth) | 管理会话令牌的签发与验签（浏览器登录态） |
| 后台与运维 | [internal/jobs](internal/jobs)、[internal/patrol](internal/patrol) | 云价格同步与可用性回填、未终态行巡检自愈 |
| | [internal/pricing](internal/pricing)、[internal/pubstatus](internal/pubstatus)、[internal/usersreset](internal/usersreset)、[internal/usagefeed](internal/usagefeed) | 价格同步入参编排、公开状态页投影、用户统计重置、新行落库信号订阅 |
| | [internal/specialsettings](internal/specialsettings) | 随请求记录落库的审计条目 |
| 页面与工具 | [internal/uiapp](internal/uiapp)、[internal/egress](internal/egress)、[internal/httpapi](internal/httpapi) | 内嵌静态页面的读取与作答、前门归属与排空闸门、探针与出站压缩 |
| | [internal/logx](internal/logx)、[internal/debugapi](internal/debugapi)、[internal/appversion](internal/appversion)、[internal/clientver](internal/clientver)、[internal/ipgeo](internal/ipgeo)、[internal/providertest](internal/providertest) | 结构化日志与凭据遮蔽、剖析面、版本取值、客户端版本比对、IP 归属地、供应商连通性探测 |

数据面主链一句话：`ingress` 读体 → `guard` 十四步守卫 → `route` 选路 → `dial` 拨号 →
`forward` 重试/竞速与流式泵 → `gate` 门控 → `convert` 互转 → `terminal` 终态结算 → `store` 落库，
整体由 [internal/dataplane](internal/dataplane) 装配。守卫链的步骤顺序**即契约**，改动顺序属语义变更。

## 环境变量

沿用上游既有变量，[.env.example](../.env.example) 是唯一真源；Go 侧只新增下面这组（不进 env 契约，
也不进 [env-parity.txt](env-parity.txt)）：

| 变量 | 默认 | 含义 |
| --- | --- | --- |
| `PORT` | `23000` | 公共监听端口（唯一对外入口） |
| `CCH_EGRESS_PAGES` | `off` | 页面面归属：`embed` 用内嵌产物作答；`off` 不服务页面 |
| `CCH_SAME_PROTOCOL_WEIGHT_K` | `2` | 同协议候选的权重倍率（选路偏好）；`1` 即关闭 |
| `CCH_GO_MAX_INFLIGHT_BYTES` | `67108864` | 在途请求正文字节上限 |
| `CCH_GO_MAX_STREAMS` | `64` | 同时存在的流式响应数上限 |
| `CCH_PATROL_*` | 见 `.env.example` | 未落终态行的自愈巡检 |
| `CCH_PPROF_*` | 关闭 | 剖析面（开启后只应绑回环） |
| `GOMEMLIMIT` | 未设 | 未设时启动日志会告警：超限应表现为拒绝而不是 OOM 退出 |

历史：双后端时期的 `CCH_INTERNAL_PORT`（回退目标）、`CCH_EGRESS_MODE`（归属总开关）、
`CCH_EGRESS_ROUTES`（逐路由白名单）三条已随回退能力一并删除，本进程**不再解析**它们。

`DSN`、`REDIS_URL`、`ADMIN_TOKEN` 等凭据不进任何日志或错误信息；需要表达「已配置」时只写布尔。
