<!-- project-rules:START -->
# Project Rules —— 业务编码规范

> 本分割块由 `fpi-project-init` 管理：每次只替换 START/END 之间的内容。标记之外的内容由项目自行维护，不会被覆盖。

## 1. 代码简洁性

- 务简：零冗余、不留虚设
- 重复三见必提为公共组件/工具
- 注释非必要不立；禁留注释块、无人跟进的 TODO、dead code
- 函数贵小：一职一事，滚动多屏方读毕者即拆
- 单文件宜小（约 1200 行内；测试与生成代码除外），逾则按职责拆
- 禁过度设计：不为臆想之需添抽象层

## 2. 命名规范

- 函数/方法：动词起首，述其所为
- 变量：义必明；禁 `temp`/`data`/`info`/`result`/`obj`/`item`（极短 lambda 中除外）
- 常量、类型、文件名：随本项目语言与既有代码惯式（大小写风格各从其俗，勿另立）
- 文件名：与主要导出对应
- 缩写：约定俗成者允（`ctx`/`cfg`/`err`/`req`/`resp`/`db`/`mq`），自创者禁

## 3. 错误处理

- 禁吞错：catch/ignore 而不理，非有注释明因即为缺陷
- 错必带上下文：谁、何事、败于何
- 库/共享代码不抛未受检异常、不 panic，惟按本项目语言惯式返回错误任调用方裁
- 对外接口错：有据可依的错误标识 + 人可解之消息
- 日志与返回分离：日志为排查，返回为调用方处理，两不相涉

## 4. 安全编码

- 参数化查询：禁字符串拼接 SQL，必用参数绑定
- 输入校验：外部输入（HTTP 参数、消息体、文件）必验类型、长度、范围
- 禁硬编码密钥：token、密码、AK/SK 走配置文件或环境变量
- 敏感数据脱敏：密码、token、身份证号禁入日志

## 5. 幂等性（有写接口/消息消费/定时任务者适用）

- 查询必无副作用；写接口、消息/CDC 消费、定时任务必显式处理重复请求
- 重复提交结果必可控：不得重复落库、重复推送、重复触发外部副作用
- 幂等策略：先业务唯一键/请求幂等键，禁依赖自增 ID 或进程内状态判断重复

## 6. 针对性改动

- 惟改需求所及，严禁波及现有功能
- 改前先明旧逻辑，不确则问
- 新增功能不得更动既有接口行为语义

## 7. 测试与提交

- 改代码即担责：交差前必跑受影响范围的编译与测试，不过不交
- 新增或修复逻辑必留一处可运行的验证（测试或最小自检），禁无验交付
- 修 bug 先求根因：查全部调用方，于共用处一次修净，勿逐点补丁
- 提交一事一改，信息格式随本项目历史惯例（无惯例则 `scope: 简述`）；不夹带无关改动、不提交生成物与凭据
- 禁自动 push 远端；force push、reset --hard、删分支等不可逆操作先报后动
<!-- project-rules:END -->

<!-- project-db:START -->
## 数据库与文档目录（依项目实际取舍，无库可删）

### 数据库操作

- 数据库变更经项目既定通道：有 DB MCP 则走 MCP，有迁移工具（golang-migrate、Alembic、Liquibase、Flyway 等）则走迁移脚本
- ⛔ 禁 Bash 直执数据库命令（mysql/psql/mongo）
- ⛔ 禁临时脚本操库（Python/Node 手搓连接同禁）
- 生产库写操作：先询用户，先留退路（备份/快照/事务），后动手
- 变更必留 SQL 脚本存档：`docs/sql/YYYYMMDD_功能.sql`
  ```sql
  -- 日期: YYYY-MM-DD
  -- 功能: 描述
  ```

### 文档目录

- `docs/` 正式文档
- `reference/` 临时/参考文档
<!-- project-db:END -->

# AGENTS.md — CC Hub Go 仓库指南

> 本文件是**供 AI 编码代理使用的唯一真源**；仓库里**不另设**第二份指南（原先的 `CLAUDE.md` 指针已删除，改由
> `tests/unit/docs/doc-paths.test.ts` 钉住「不得再出现第二份」）。
> 本文提到的每个仓库内路径都由 `tests/unit/docs/doc-paths.test.ts` 校验存在——改动本文件后必须让
> 该测试仍然通过（否则说明文档已经与代码脱节）。该钉子范围是**全部受跟踪的 `*.md`**：反引号里
> 的仓库内路径、Markdown 相对链接、以及 `bun run <script>` 都必须真实存在。

## 1. 这是什么

自研 LLM 网关（CC Hub Go）。对外提供 Anthropic Messages、OpenAI Chat Completions、
OpenAI Responses 与 Gemini 兼容入口，并承担：鉴权、限流与配额、供应商选路（分组优先级、
前缀亲和、熔断过滤、竞速）、协议互转、流式转发与内容门控、用量与成本结算、请求回放与
未终态自愈；另带管理面 REST 与内嵌 Web UI。

对外路径（唯一进程 `go/cmd/cchd` 全部自答，**进程内没有任何回退后端**）：

| 路径 | 内容 |
| --- | --- |
| `/v1/messages`、`/v1/chat/completions`、`/v1/responses` | 数据面三线入口（含 `/v1/responses` 的 WebSocket 通道） |
| `/v1beta` | Gemini 兼容入口（注册的是 `/v1beta/` 前缀，裸路径由 ServeMux 301 到带斜杠形态） |
| `/api/v1` | 管理面 REST（OpenAPI 由 Go 注册表生成） |
| `/readyz`、`/v1/_ping` | 探针：不经前门，排空窗口内也照答（排空期间 `/readyz` 如实报 503） |
| 其余非 API 路径 | 内嵌静态 UI（`CCH_EGRESS_PAGES=embed`）或 404；未实现的路径自答 404，**不返回 501** |

**历史事实（读代码时会撞到）**：本仓的 Node 后端已整体退役并删除——原来 `src` 下的
app/api、actions、repository、app/v1 等目录都已不存在。现存 Go 代码里的注释常写
「复刻 Node 的 X」，那是**当时的移植对应关系**，不是要求你去对照一份已不存在的实现。

（说明：本文对**已删除**的路径不加反引号，以免与「路径钉子」的“提到的路径必须存在”冲突。）

## 2. 三层结构（改错层等于白干）

| 层 | 位置 | 真源地位 |
| --- | --- | --- |
| 后端进程 | `go/` | 唯一服务端实现：数据面、管理面、迁移器、页面服务 |
| 前端 | `src/`、`messages/` | Next.js，**只做静态导出**：不启动服务，导出产物被压缩嵌入 Go 二进制 |
| 数据库迁移 | `drizzle/` | 迁移真源（SQL + journal），被 `go/internal/migrate` 逐字节嵌入 |

## 3. 常用命令

| 目的 | 命令 |
| --- | --- |
| UI 导出 + 压缩嵌入（Go 的页面产物） | `bun run build` |
| 前端类型检查 / 静态检查 / 测试 | `bun run typecheck`、`bun run lint`、`bun run test` |
| 前端开发服务器（**仅 UI 调试，不提供后端**） | `bun run dev` |
| Go 格式化检查 / vet / 测试 / 构建 | `cd go && gofmt -l . && go vet ./... && go test -timeout 300s ./... && go build ./cmd/cchd` |
| Go 真库集成用例（PG + Redis） | `cd go && CCH_TEST_DSN=... CCH_TEST_REDIS_URL=... go test -timeout 600s ./...` |
| 本地依赖（PG + Redis 容器） | `make db`（根 Makefile 转发到 `dev/Makefile`；`make dev-help` 看全部目标） |
| 发布 | 打 `vX.Y.Z` tag 并推送：`release.yml` 出六目标归档，`docker.yml` 出多架构镜像 |

要点：

- **Go 测试不注入 `CCH_TEST_DSN` / `CCH_TEST_REDIS_URL` 时，真库集成用例整组跳过**——
  CI 正是这样跑的（只跑纯单测、无需外部依赖）。要验证持久化与 Redis 语义必须自己注入。
- 前端测试与 Go 测试相互独立；跨层改动（改了 API 契约或 i18n 词表）要两边都跑。
- `go test` 一律带 `-timeout`（仓内既有用例有过挂住的记录），CI 用 300s。

## 4. 目录地图

### 4.1 Go 侧

| 路径 | 职责 |
| --- | --- |
| `go/cmd/cchd` | 进程入口：配置装载 → 依赖 → 订阅 → 前门装配 → 监听；SIGTERM 排空后关闭 |
| `go/cmd/envlist` | 生成 `go/env-parity.txt`（Go 侧受契约覆盖的环境变量清单） |
| `go/cmd/jobsrun` | 手动驱动后台任务一轮（价格同步、可用性投影回填） |
| `go/cmd/lossseverity` | 由 `go/internal/convert` 的档位表渲染前端 `src/lib/utils/loss-severity.gen.ts`（生成物入库，供校验与再生成） |
| `go/internal/adminapi` | 管理面 REST：路由注册表、守卫、审计条目、各资源处理器 |
| `go/internal/adminauth` | 管理会话令牌的签发与验签（浏览器登录态） |
| `go/internal/appversion` | 进程版本号取值的唯一实现 |
| `go/internal/cfgsync` | 配置域快照装载与失效订阅（含 in-flight 合并） |
| `go/internal/clientver` | 客户端版本解析与 GA 比对 |
| `go/internal/config` | 环境变量装载与校验（非法即 fail fast） |
| `go/internal/convert` | 三线协议编解码的统一入口（anthropic / chat / responses） |
| `go/internal/dataplane` | 数据面装配与转发编排：把守卫链、选路、转发、门控、结算串起来 |
| `go/internal/debugapi` | 性能剖析面：pprof 与运行时指标（默认关闭，开启只应绑回环） |
| `go/internal/deps` | 外部依赖：PostgreSQL（pgx/v5）与 Redis（go-redis/v9）的连接与探测 |
| `go/internal/dial` | 上游拨号层：请求构造、三档超时、字节透传、上游连接准入 |
| `go/internal/egress` | 前门路由归属与排空闸门；未接管的路径自答 404 |
| `go/internal/forward` | 上游转发主干：请求计划、重试与竞速（hedge）、响应处理 |
| `go/internal/gate` | 流式内容门控：按帧分类等待首个可提交内容，控制上游切换时机 |
| `go/internal/guard` | 守卫链（14 步，顺序即契约）与各步骤的真实适配器 |
| `go/internal/health` | 供应商熔断写入面：状态转移落 Redis、等待阶梯、开闸与半开推进（读侧在 route） |
| `go/internal/httpapi` | HTTP 面：探针端点、前门挂载点、出站压缩、CORS |
| `go/internal/ingress` | 入站正文读取与进程级内存准入 |
| `go/internal/ipgeo` | IP 归属地查询与私网判定 |
| `go/internal/jobs` | 常驻后台任务：云价格同步、可用性投影回填等 |
| `go/internal/limit` | 限流与配额语义（多维窗口、会话计数、滥用检测） |
| `go/internal/logx` | 结构化 JSON 日志与凭据遮蔽 |
| `go/internal/migrate` | 迁移器：内嵌 drizzle 迁移、水位账本、自愈与索引 preflight |
| `go/internal/notify` | 通知数据生成：日榜与成本告警等定时取数与判定 |
| `go/internal/patrol` | 周期巡检「已建行但从未终态」的请求并补写终态 |
| `go/internal/pctx` | 请求上下文：归属一次性决定、正文一次性消费、headers 只读视图 |
| `go/internal/pricing` | 模型价格同步的入参解析与编排 |
| `go/internal/providertest` | 供应商连通性探测引擎（含流式与工具调用解析） |
| `go/internal/pubstatus` | 公开状态页的配置投影发布 |
| `go/internal/ratelimit` | Redis 限流脚本调用与错误分类（只报告错误种类，是否降级由 limit 决定） |
| `go/internal/rectify` | 请求正文整流器：上游报错文案命中后整流并对同一供应商重试 |
| `go/internal/replay` | 请求回放子系统：身份推导、双层存储、owner spool |
| `go/internal/responsefix` | 响应修复器：截断 JSON、坏 UTF-8、畸形 SSE 帧的自愈 |
| `go/internal/route` | 供应商选路：亲和、分组与优先级、熔断过滤、候选留痕 |
| `go/internal/session` | 客户端会话身份、绑定、租约与跟踪 |
| `go/internal/specialsettings` | 随请求记录落库的审计条目 |
| `go/internal/store` | 持久化面：连接池分道与准入、请求行的写入与终态、账本（`usage_ledger`）只读口径与投影写入 |
| `go/internal/terminal` | 终态结算：请求建行、终态落库、用量与成本定点计算 |
| `go/internal/tracing` | 终态事实旁路上报 Langfuse（出站 HTTP） |
| `go/internal/uiapp` | 内嵌静态 UI 的读取与作答（产物缺失时启动即失败，不静默降级为空页面） |
| `go/internal/upws` | 上游 Responses WebSocket 拨号层（codex 类供应商） |
| `go/internal/usagefeed` | 「使用记录有新行落库」的信号订阅面 |
| `go/internal/usersreset` | 用户统计重置子系统 |
| `go/internal/ws` | `/v1/responses` 的客户端 WebSocket 通道 |
| `go/deploy/Dockerfile` | 生产镜像（多阶段：Go 构建 + 内嵌 UI 产物 + 运行时） |
| `go/deploy/Dockerfile.prebuilt` | 离线变体：二进制在宿主预先构建，容器内不做网络操作 |
| `go/testdata` | Go 测试夹具与黄金样本 |

### 4.2 其他

| 路径 | 职责 |
| --- | --- |
| `src/app` | Next.js 页面与路由（`src/app/[locale]` 为界面主体） |
| `src/components`、`src/hooks`、`src/lib`、`src/types`、`src/i18n` | 前端组件、hooks、客户端工具、类型、i18n 装配 |
| `messages/zh-CN`、`messages/en` | 界面词表（仅这两种语言；**用户可见文案一律走 i18n，不得硬编码**） |
| `drizzle` | 迁移真源：`*.sql` 与 `meta/_journal.json` |
| `lua` | 限流与会话相关的 Redis Lua 脚本（另有黄金样本对拍） |
| `scripts` | 构建链（UI 导出/嵌入）、环境变量矩阵、发布与运维工具 |
| `tests/unit`、`tests/load`、`tests/helpers`、`tests/setup.ts` | 前端单测、负载与验收夹具、测试助手与全局 setup |
| `.github/workflows/ci.yml` | CI：UI（typecheck/lint/test）与 Go（gofmt/vet/test） |
| `.github/workflows/release.yml` | tag `v*` 触发：UI 产物 → 六目标静态二进制 → GitHub Release |
| `.github/workflows/docker.yml` | tag `v*` 或手动触发：多架构镜像（amd64 + arm64） |
| `dev/Makefile`、`dev/docker-compose.yaml` | 本地开发依赖（PG/Redis）与常用目标 |
| `data` | 本地 compose 的持久化目录 |
| `docker-compose.yaml`、`.env.example`、`Makefile`、`package.json` | 部署编排、环境变量真源、快捷命令、前端脚本 |

**管理面契约的唯一真源是 Go 的已注册路由表**（见 `go/internal/adminapi`，spec 在运行时生成）。
Node 时代那份静态生成物（约 3.9 万行、零消费方）已在 2026-09 的过度设计清理中删除——
**不要试图恢复它**，也不要据任何静态快照推断 API 形状。

## 5. 请求管线（`/v1` 数据面）

守卫链是**预设的有序步骤表**，定义在 `go/internal/guard/guard.go`：**顺序即契约，不得重排**
（顺序变化会改变错误优先级与计费时点，属于语义变更）。共四个预设：

| 预设 | 用途 | 步骤 |
| --- | --- | --- |
| `CHAT_PIPELINE` | 普通对话（14 步） | auth → sensitive → client → model → version → probe → session → warmup → requestFilter → replayAttach → rateLimit → provider → providerRequestFilter → messageContext |
| `RAW_PASSTHROUGH_PIPELINE` | 原样透传（6 步） | auth → client → model → version → probe → provider |
| `RAW_SAFE_SESSION_PIPELINE` | 带会话的安全链（8 步） | auth → client → model → version → probe → session → provider → messageContext |
| `COUNT_TOKENS_PIPELINE` | count_tokens | 复用上面那条安全会话链 |

请求类型到预设的映射见同文件的 `FromRequestType`；装配在 `go/internal/guard/assemble.go`，
每一步的真实实现在 `go/internal/guard/steps.go` 与 `adapters_*.go`。
`go test ./internal/guard/` 里有两条钉子兜住这里：缺步骤实现即报错（不静默跳过），
以及「全部预设都能被完整装配」——加了步骤键却不给实现会直接红。

其后：转发主干 `go/internal/forward`（拨号在 `go/internal/dial`，重试与竞速同包）→
流式门控 `go/internal/gate` → 终态结算 `go/internal/terminal`（建行、终态落库、用量与成本）。
整体装配在 `go/internal/dataplane`；跨步传递的请求上下文在 `go/internal/pctx`。

## 6. 数据库与迁移

- **schema 与迁移真源**：`drizzle`（`*.sql` + `meta/_journal.json`）。Go 侧持有一份**逐字节副本**
  在 `go/internal/migrate/sql/`，由 `go/internal/migrate` 嵌入二进制。
- **应用**：进程启动时按 `AUTO_MIGRATE` 自动应用（`go/internal/migrate`）。生产不要手工用 `psql`
  改结构；账本行（`usage_ledger`）由数据库触发器产生，**不要直写**。
- **新增迁移**：本仓已无 `db:generate` 这类脚本，也不再有 TS schema（Node 退役时一并删除）——
  因此新迁移以「SQL + journal 条目」的形式加入真源，然后**必须同步副本**，两条路径各自对应：
  `drizzle/*.sql` → `go/internal/migrate/sql/`；`drizzle/meta/_journal.json` →
  `go/internal/migrate/sql/meta/_journal.json`。嵌入点是 `sql/meta/_journal.json`（见
  `go/internal/migrate/journal.go`），层级放错测试看不见。
- **漂移由测试兜住**：`go test ./internal/migrate/` 会逐字节比对副本与真源（副本多/少一个文件
  都会红，并提示同步命令）。改迁移后必须跑它。

## 7. 改文件与提交纪律

- **源码一律用编辑工具修改**，不要用 `sed -i` 或 shell 重定向「代笔」：既难审，也容易误伤。
- 提交前按影响面跑门禁：

  ```bash
  # Go
  cd go && gofmt -l . && go vet ./... && go test -timeout 300s ./...
  # 前端
  bun run typecheck && bun run lint && bun run test
  # 跨层（改契约、词表、构建链、发布）
  bun run build
  ```

- 提交信息：`scope: 简述`（中文），例如 `fix(go-route): 分组覆盖须在组内才生效`；正文写清
  **为什么**以及**怎么验证的**。一个提交只做一件事，便于回退。
- 改动界面文案：五类词表相关的东西只有两种语言（`messages/zh-CN`、`messages/en`），改一处要
  同步另一处；禁止硬编码用户可见文案。

## 8. 禁止事项

- **不得在任何代码、注释、字符串、文档里使用 emoji**（仓库既有约定；词表侧另有强制审计
  `bun run i18n:audit-messages-no-emoji`）。
- **提交里不得出现内部信息**。这条没有「酌情」：一次泄漏就随公开仓与镜像永久留下，而清理它要
  改写历史、强推、重建仓库。具体到：
  - **内部域名与主机名**（自建 registry、部署机、内网服务名）、**内网或公网 IP**、**真实子网**；
    网段要举例就用文档段（`198.51.100.0/24`）或协议定义段（`10.0.0.0/8`、`172.16.0.0/12`、
    `192.168.0.0/16`）——生产里实际用的那个子网不得出现；
  - **凭据**：token、API key、私钥、DSN 口令（含 URL 里的 `user:pass@host`）；
  - **个人路径**：`/home/<user>/…`、`/mnt/<user>/…`、`C:\Users\<user>\…`。
  写示例一律用**保留段**与**占位符**：文档地址用 RFC 5737（`192.0.2.10`、`198.51.100.7`、`203.0.113.9`）
  与 RFC 3849（`2001:db8::`），域名用 `example.com` / `demo.internal` / `<your-db-host>` 这类形态，
  凭据用 `sk-ant-<占位>`。夹具同理：**不要**把生产抓包（真实 IP、设备名、上游回显）直接粘进测试，
  要按形状重写成占位值——仓库里那个 CGN 夹具就是按这条改成占位的。
  提交身份也守这条：用 `<用户名>@users.noreply.github.com`（`git config user.email`），不要带私人邮箱。
  **机器检查**：`tests/unit/docs/sensitive-patterns.test.ts` 对**全部受跟踪文件**与**最近一次提交的
  元数据**执行上述判据（唯一豁免是 `ALLOWLIST`——里面每条都写明了理由）。`bun run test` 会跑到它。
  新增一处例外时，改 `ALLOWLIST` 并写明理由；**不要**为了让钉子变绿而删规则。
- **不得提交构建产物**：`out/`、`go/internal/uiapp/assets/`、`go/cchd`、各类 `*.log` 都不入库。
- **不得手改 `drizzle` 下的历史迁移 SQL**（见 §6）；也不要直写账本表。
- **不得新增「回退到 Node」这类已删除能力**：进程内没有第二后端，未实现路径一律 404。

## 9. 遇到不确定时

1. 先读该包内的 `doc.go` / `README.md`——多数包写明了设计约束与「为什么这样做」。
2. 生产行为的判据优先级：**代码 > 测试 > 文档**（文档可能滞后；本文件的路径钉子只保证路径存在）。
3. 涉及协议互转的改动，务必同时看 `go/internal/convert` 与 `go/testdata` 里的语料/黄金样本。
