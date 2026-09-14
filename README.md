<p align="right">
  <a href="./README.en.md" aria-label="Switch to English version of this README">English</a> | <strong>中文</strong>
</p>

<div align="center">

# CC Hub Go

**claude-code-hub-go** —— 把 [claude-code-hub](https://github.com/ding113/claude-code-hub) 的后端**整体重写为 Go 单二进制**

[![Release](https://img.shields.io/github/v/release/fanxcv/claude-code-hub-go?label=release)](https://github.com/fanxcv/claude-code-hub-go/releases)
[![Docker Pulls](https://img.shields.io/docker/pulls/fanxcv/claude-code-hub-go)](https://hub.docker.com/r/fanxcv/claude-code-hub-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](./LICENSE)

</div>

> [!IMPORTANT]
> **本仓是二开（fork），不是上游的替代品。**
> 上游 [ding113/claude-code-hub](https://github.com/ding113/claude-code-hub) 是 Node.js 实现（Next.js 全栈）。
> 本仓保留上游的**数据模型、Redis 键空间与前端界面**，把**后端整体重写为 Go 单二进制**：数据面（代理与协议互转）、管理面（REST API）、迁移器、页面服务全部在 Go 内完成。
> 原项目以 MIT 许可发布，版权归上游作者；本仓同样以 MIT 发布。

---

## 目录

- [1. 它是什么](#1-它是什么)
- [2. 相对上游改了什么](#2-相对上游改了什么)
- [3. 快速开始](#3-快速开始)
- [4. 配置](#4-配置)
- [5. 数据与迁移（可与上游镜像互换）](#5-数据与迁移可与上游镜像互换)
- [6. 容量与性能实测](#6-容量与性能实测)
- [7. 开发](#7-开发)
- [8. 与上游同步](#8-与上游同步)
- [9. 许可与致谢](#9-许可与致谢)

---

## 1. 它是什么

给 Claude Code / Codex / Gemini 等客户端用的**自托管 LLM 网关**：把多个上游供应商聚合成一个入口，做选路、协议互转、限流熔断、用量与计费记账，并提供一个 Web 管理台。

形态上是**单进程、单端口、单镜像**：一个静态链接的 Go 二进制，界面资源（Next.js 静态导出产物，brotli 压缩）已内嵌，运行时不需要 Node、不需要额外的静态文件目录。

对外入口：

| 路径 | 说明 |
| --- | --- |
| `/v1/messages` | Anthropic 协议（Claude Code 直连） |
| `/v1/chat/completions` | OpenAI Chat Completions |
| `/v1/responses` | OpenAI Responses（含 WebSocket 变体） |
| `/v1beta/**` | Gemini 协议 |
| `/v1/models`、`/v1/_ping` | 模型列表、探活 |
| `/api/v1/**` | 管理面 REST（当前版本 **179 条路径 / 209 个操作**，可查 `/api/v1/openapi.json`） |
| `/readyz`、`/api/health` | 就绪探针（逐项报 PG / Redis / 规则快照 / 排空状态）与健康面 |
| `/debug/metrics`、`/debug/pprof/**` | 运行时指标与剖析（**默认关闭**，开启后只绑回环） |
| `/**` | Web 管理台（使用内嵌 UI 产物作答） |

## 2. 相对上游改了什么

### 2.1 后端整体重写（不是打补丁）

| 维度 | 上游（Node.js） | 本仓（Go） |
| --- | --- | --- |
| 运行时 | Node 22 + Next.js standalone | 单个静态链接二进制（`CGO_ENABLED=0`） |
| 数据面 | TypeScript（`src/app/v1/_lib/proxy/**`） | Go：守卫链 → 选路 → 拨号 → 流式门控 → 流式泵 → 终态结算 |
| 协议互转 | TypeScript 转换器 | Go 内置（Anthropic ↔ OpenAI Chat ↔ OpenAI Responses ↔ Gemini） |
| 管理面 | Next.js route handlers + server actions | Go 原生 REST（179 条路径 / 209 个操作，OpenAPI 由路由注册表生成） |
| 页面服务 | Next SSR + API 路由 | 静态导出 → brotli 压缩 → `go:embed`，由 Go 直接作答 |
| 数据库迁移 | `drizzle-kit` / `drizzle-orm` 迁移器 | Go 内置迁移器，**逐字节对齐** drizzle 的 journal/hash/statement 语义 |
| 限流 | TypeScript 常量 + Lua | 18 段 Lua **逐字节一致**，附 golden 重放比对 |
| 镜像 | 333 MB | **45.8 MB** |

### 2.2 新增能力（上游没有的）

**可靠性与正确性**

- **决策链完整留痕**：链上记录「参与候选池（`consideredCandidates`）」「同档候选（`candidatesAtPriority`）」「因前缀亲和短路而通过硬校验却未参与竞争（`survivingCandidates`）」三类事实，每个尝试项带 `statusCode` / `errorMessage` / `modelRedirect`，管理台弹窗可逐项展开。
- **熔断等待阶梯**：可配「基础时长 + 递增 × 次数（封顶）」，半开未恢复则窗口按阶递增（默认关闭，保持与上游同行为）。
- **熔断日志查看器**：按供应商查看近期失败的真实错误（含脱敏），替代「只看到计数、看不到原因」。
- **协议转换失败单独留痕**：转换回退是静默的，本仓额外落一条 `protocol_conversion_failed` 审计条目，便于定位「为什么这个请求走到了非预期的上游协议」。
- **结算屏障**：进程退出前等待在途请求与待结算落库，避免滚动重启窗口内的账本空洞（上游无此机制）。
- **未落终态行自愈巡检**：定期扫描长期无终态的行并补终态。
- **迁移能力内建**：启动按 `AUTO_MIGRATE` 自动应用 drizzle 迁移，不再需要 Node 侧迁移命令。

**性能与资源**

- **单流驻留 < 1 MiB**：8 MiB 响应体在流式转发下不整块驻留（上游同负载约 8.7 MiB/流）。
- **出站压缩**：管理面大 JSON 响应开启 br/gzip 协商。对与生产同形状的载荷（50 行 × 54 字段），仓库内实测 br **269,524 → 11,254 字节（23.95×）**、gzip **17,585 字节（15.33×）**；生产实测 `/api/v1/usage-logs` 约 **22×**。
- **数据库连接一律关 JIT**：`/dashboard/overview` 由 **100.9–102.5 ms** 降到 **9.1–9.9 ms**（约 11 倍）。根因是规划器把该查询高估 236 倍，代价越过 `jit_above_cost` 触发了 PG 的 JIT 编译。
- **零分配流式判定**：解析请求体只用顶层扫描判断 `stream`，不建整棵 JSON 树（同一 50 KiB 请求体：分配 564,835 → 39,144 B，分配次数 9,555 → 47）。
- **统计短 TTL 缓存 + 增量拉取**：使用记录页支持 `sinceId` 增量，统计面板带短缓存。

**可观测与运维**

- **`/debug/metrics` + pprof**：Go 运行时指标（GC、堆、goroutine）与 CPU/heap/block/mutex 剖析，默认关闭且只绑回环。
- **单一版本真源**：`/api/version`、`/api/health`、UI 页脚三处同值（上游曾出现三处各报一个版本号）。
- **就绪探针分级**：`/readyz` 逐项报告依赖并点名失败项；排空窗口内返回 503，`/v1/_ping` 仍答。
- **限流与鉴权节流接入数据面**，限流拒绝信封与上游同形（`code` / `limit_type` / `current` / `limit` 等七字段）。

**界面与功能**

- 使用记录页支持**轮询 / 推送（SSE）双模式**切换，可配刷新间隔（1/2/3/5/10 s）。
- **同协议优先**：优先级一致时优先选择**无需协议转换**的供应商（`CCH_SAME_PROTOCOL_WEIGHT_K`，默认 `2`；设为 `1` 即关闭，等价于不装配）。判据复用 guard 做格式兼容过滤的同一张配对表，因此偏好不可能指向一个仍需转换的供应商。
- **会话空闲闸门**：前缀亲和在「同一客户端会话长期闲置」后不再续命，避免亲和压过优先级导致请求一直粘在旧的（可能更慢/更贵）供应商上。
- 界面语种为简体中文与英文；退役的 `ja` / `ru` / `zh-TW` 前缀保留**重定向壳**（带旧前缀的旧链接不会 404）。

### 2.3 与上游的一致性，以及已知差异

对齐方式不是靠人工比对，而是靠可复现的对拍：黄金样本（含上游逐字节录制的响应）、Lua golden 重放、协议一致性矩阵、切换矩阵（同一批请求分别由两代后端作答后逐字段比对）。

有意保留的差异（**这些是修上游的缺陷，不是回归**）：

- 上游管理面有一处 SQL 的 `id` 列歧义，导致其活动流恒返回空数组；本仓返回的是与上游类型定义一致的非空结果（在切换对拍中单列为 `node-defect`，不计入失败）。
- 上游在「仅思考、无正文、无工具调用」的 assistant 消息上会产出空 `content`，被严格上游判 400；本仓改为携带思考文本并保留 reasoning 回传。

## 3. 快速开始

### 3.1 Docker Compose（自带 PostgreSQL 与 Redis）

```bash
git clone https://github.com/fanxcv/claude-code-hub-go.git
cd claude-code-hub-go
cp .env.example .env
# 至少改这三项：ADMIN_TOKEN、DB_PASSWORD，以及按需调整 APP_PORT
$EDITOR .env
docker compose up -d
```

打开 `http://<主机>:23000`，用 `ADMIN_TOKEN` 登录。首次启动会自动建表（`AUTO_MIGRATE=true`）。

### 3.2 单个容器（外接已有的 PostgreSQL 与 Redis）

```bash
docker run -d --name cchd \
  -p 23000:23000 \
  -e DSN='postgres://user:pass@<pg-host>:5432/claude_code_hub' \
  -e REDIS_URL='redis://<redis-host>:6379' \
  -e ADMIN_TOKEN='<你的管理令牌>' \
  -e CCH_EGRESS_PAGES=embed \
  -e TZ=Asia/Shanghai \
  --restart unless-stopped \
  fanxcv/claude-code-hub-go:latest
```

`CCH_EGRESS_PAGES` 决定页面面归属：`embed` 由内嵌 UI 作答，`off` 完全不服务页面。程序默认值是 `off`（保守取值：缺省不假设一定有页面产物），但**本仓官方镜像已在运行阶段内置 `CCH_EGRESS_PAGES=embed`**（见 `go/deploy/Dockerfile`），`docker-compose.yaml` 模板也显式设了——所以用官方镜像或 compose 起步时**无需手动设置**，上面那行 `-e CCH_EGRESS_PAGES=embed` 只是把生效值写明。想关掉页面面（例如前面另有独立托管的静态站）显式覆盖为 `off` 即可。

### 3.3 Release 二进制

从 [Releases](https://github.com/fanxcv/claude-code-hub-go/releases) 下载对应平台的压缩包（无需额外安装运行时，界面已内嵌）。**六个平台目标**，每个归档内含二进制、`LICENSE` 与 `README.md`：

| 平台 | 归档 | 内含二进制 |
| --- | --- | --- |
| Linux x86-64 | `cchd-linux-amd64.tar.gz` | `cchd` |
| Linux arm64 | `cchd-linux-arm64.tar.gz` | `cchd` |
| macOS Intel | `cchd-darwin-amd64.tar.gz` | `cchd` |
| macOS Apple Silicon | `cchd-darwin-arm64.tar.gz` | `cchd` |
| Windows x64 | `cchd-windows-amd64.zip` | `cchd.exe` |
| Windows on Arm | `cchd-windows-arm64.zip` | `cchd.exe` |

动态依赖（实测）：Linux 为纯静态（无 `NEEDED` 段）；macOS 只链系统自带的 `libSystem`/`libresolv`/`CoreFoundation`/`Security`（macOS 不允许静态链接 libSystem）；Windows 仅导入系统自带的 `kernel32.dll`。

六个归档共用一份 `SHA256SUMS`：

```bash
sha256sum -c SHA256SUMS             # Linux
shasum -a 256 -c SHA256SUMS         # macOS（系统自带 shasum）
Get-FileHash .\cchd-windows-amd64.zip -Algorithm SHA256   # Windows（对照 SHA256SUMS 里的值）
```

Linux / macOS：

```bash
tar -xzf cchd-linux-amd64.tar.gz && cd cchd-linux-amd64
chmod +x cchd
DSN='postgres://user:pass@127.0.0.1:5432/claude_code_hub' \
REDIS_URL='redis://127.0.0.1:6379' \
ADMIN_TOKEN='<你的管理令牌>' \
PORT=23000 CCH_EGRESS_PAGES=embed ./cchd
```

Windows（PowerShell）：

```powershell
Expand-Archive cchd-windows-amd64.zip -DestinationPath .
cd cchd-windows-amd64
$env:DSN='postgres://user:pass@127.0.0.1:5432/claude_code_hub'
$env:REDIS_URL='redis://127.0.0.1:6379'
$env:ADMIN_TOKEN='<你的管理令牌>'
$env:PORT='23000'; $env:CCH_EGRESS_PAGES='embed'
.\cchd.exe
```

### 3.4 从源码构建

前置：Go 1.25+、Bun（或 Node 22+ 与 bun）。界面产物是**构建期**生成并内嵌的，所以先构建界面、再构建二进制：

```bash
bun install
bun run build          # 静态导出 UI → brotli 压缩 → 收编进 go/internal/uiapp/assets
cd go
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o cchd ./cmd/cchd
```

构建镜像（多架构可用 `docker buildx`）：

```bash
# 先按上面的步骤生成 UI 产物，再：
docker build -f go/deploy/Dockerfile -t fanxcv/claude-code-hub-go:local .
```

## 4. 配置

完整变量清单与说明见 [`.env.example`](./.env.example)。最常用的：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `DSN` | — | PostgreSQL 连接串（**必填**） |
| `REDIS_URL` | — | Redis 连接串（**必填**，会话/限流/亲和/回放都依赖它） |
| `ADMIN_TOKEN` | — | 管理台与 Admin API 令牌（**必填**，请勿用默认值） |
| `PORT` | `23000` | 监听端口（镜像内 `EXPOSE 23000`，探针自动跟随该值） |
| `AUTO_MIGRATE` | `true` | 启动时自动应用数据库迁移 |
| `ENABLE_RATE_LIMIT` | `true` | 是否启用限流 |
| `SESSION_TTL` | `300` | 会话/上下文缓存 TTL（秒） |
| `CCH_EGRESS_PAGES` | `off`（**官方镜像内置 `embed`**） | 页面面归属：`embed` 由内嵌 UI 作答，`off` 不服务任何页面。镜像与 compose 模板都已设为 `embed`，**无需手动设置**；显式设 `off` 可关掉页面面 |
| `CCH_SAME_PROTOCOL_WEIGHT_K` | `2` | 同协议候选的权重倍率（选路偏好）；`1` 表示关闭该偏好 |
| `DB_POOL_MAX` 等 | 见 `.env.example` | 连接池预算与各阶段超时 |
| `GOMEMLIMIT` | — | Go 运行时软内存上限（容器限额内建议设置，例如 `512MiB`） |
| `CCH_PPROF_ENABLED` / `CCH_PPROF_ADDR` | `false` / `127.0.0.1:3101` | 剖析面开关与监听地址（只应绑回环） |

> 未知变量会被忽略（不会导致启动失败），因此从上游 Node 版本沿用过来的 `.env` 一般可直接使用。

## 5. 数据与迁移（可与上游镜像互换）

本仓与上游**共用同一套 PostgreSQL schema 与 Redis 键空间**，因此：

- 上游迁移过来的库可以直接用：迁移真源仍是仓库根的 `drizzle/`（含 `meta/_journal.json`），Go 内置迁移器逐字节复刻 drizzle 的 journal 解析、`sha256(sql)` 指纹与 `--> statement-breakpoint` 切分语义，已应用水位与 drizzle 完全一致。
- 限流相关的 18 段 Lua 脚本与本仓 Go 侧常量逐字节一致，并有 golden 重放用例比对终态与键形制。
- **回滚**：换回上游镜像即可（数据无需回退）。本仓的 `pre-node-removal` git tag 是 Node 版本退役前的最后一个状态。

## 6. 容量与性能实测

同一台机器（2 vCPU / 1.9 GB）与同一批 mock 上游，测同一负载：

| 指标 | 上游（Node） | 本仓（Go） |
| --- | --- | --- |
| **每流驻留增量**（主判据：与冷启基线无关，跨机器可比） | ≈**8.57 MiB/流** | ≈**0.56 MiB/流** |
| 镜像体积 | 333 MB（332,888,216 B） | **45.8 MB** |
| 50 条并发 × 8 MiB 流式响应，峰值 RSS（同一夹具；**峰值含冷启基线，不可跨机器直接比**） | 797.5 MiB（基线 368.9 + 增量 428.6）；另一轮同负载实测 643.6 MiB（基线 217.5 + 增量 426.1） | **52.5 MiB**（基线 24.7 + 增量 27.9） |
| 生产实例闲置（含内嵌 UI，PG 与 Redis 已连） | ≈300 MiB | **184 MiB**（限额 768 MiB 下余量充足） |

本仓生产实例（2 vCPU / 1.9 GB）CPU 实测 2.5%–4.1%。

> 口径：上游一列为重写期间的实测记录（Node 镜像 `cch:0.9.6-local`，Node 22 + Next standalone）；本仓一列可用 `docker images` 与 `tests/load/` 下的并发夹具复核。注意**峰值受冷启基线影响**（上表两个 Node 峰值差 154 MiB，逐流增量却只差 0.05 MiB），故跨机器比较请用「每流驻留增量」。你的并发数、响应体大小与上游延迟不同，结果会有差异，建议按自己的负载实测后预留内存。

## 7. 开发

```
go/                       Go 后端（数据面 + 管理面 + 迁移器 + 内嵌 UI）
  cmd/cchd/               进程入口（启动、装配、排空关闭）
  internal/               各功能包（guard / route / forward / gate / convert / limit / store …）
src/                      前端（Next.js App Router，静态导出后由 Go 内嵌）
messages/                 界面文案（zh-CN / en）
drizzle/                  数据库迁移真源（Go 侧只读并在启动时应用）
lua/                      限流与亲和用的 Lua 脚本（与 Go 侧常量逐字节一致）
tests/                    vitest 单测与负载/对拍夹具
deploy/、dev/             部署模板与本地开发编排
scripts/build-ui-*.mjs    界面导出与压缩内嵌（构建链的关键两步）
```

常用命令：

```bash
# 前端
bun run typecheck && bun run lint && bun run test

# Go（真库集成需要这两个变量；不设则集成用例自动跳过）
cd go && gofmt -l . && go vet ./... && go test -timeout 600s ./...

# 本地起 PG + Redis（dev/ 里是编排）
cd dev && make db
```

Go 侧集成测试需要真实 PostgreSQL 与 Redis：

```bash
cd go
CCH_TEST_DSN='postgres://user:pass@127.0.0.1:5432/cch_loadtest' \
CCH_TEST_REDIS_URL='redis://127.0.0.1:6379/15' \
go test -timeout 600s ./...
```

## 8. 与上游同步

```bash
git remote add upstream https://github.com/ding113/claude-code-hub.git
git fetch upstream
# 上游的前端改动通常可以直接 cherry-pick；后端改动需要翻译到 Go 侧
git log --oneline upstream/main -- src/ messages/ | head
```

前端（`src/**`、`messages/**`）与上游保持同构，便于跟进；Go 后端是重写，需按语义移植。

## 9. 许可与致谢

- 本仓以 **MIT** 许可发布，与上游一致；上游版权归 [ding113](https://github.com/ding113) 与贡献者所有。
- 界面与数据模型源自上游 [claude-code-hub](https://github.com/ding113/claude-code-hub)，感谢原作者的出色工作。
- 若你在生产使用本仓，建议先跑一遍 `tests/load/` 下的协议一致性与切换夹具，确认与你的上游供应商组合相符。
