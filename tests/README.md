# 测试指南

本仓的测试分三层，**互不替代**：

| 层 | 位置 | 跑法 |
| --- | --- | --- |
| 前端（界面）单测 | [unit/](unit) | `bun run test`（Vitest） |
| Go 侧单测与集成 | `go/**/*_test.go` | `cd go && go test -timeout 300s ./...` |
| 负载与对拍夹具 | [load/](load) | 各自 README（多为独立脚本，需真实依赖） |

用例数量会随开发变动，**以命令输出为准**，本文不写死数字（写死的数字是上一版腐烂最快的地方）。

## 前端单测

```bash
bun run test              # 全量
bun run test:ui           # Vitest UI（可视化，调试用）
bun run test:coverage     # 覆盖率
bun run test:ci           # CI 用：输出 junit 到 reports/
```

- 配置：仓库根 `vitest.config.mts`，共享基线在 [vitest.base.mts](vitest.base.mts)。
- 全局 setup 与替身：[setup.ts](setup.ts) 与同目录的 `*.mock.ts(x)`（Next.js 运行时、framer-motion、fluent-emoji、server-only 等）。
- 夹具助手在 [helpers/](helpers)。
- 按被测面分目录（`unit/dashboard`、`unit/i18n`、`unit/lib`…），**测什么就放在对应目录**，别堆在根。

## Go 单测与集成

```bash
cd go
gofmt -l .                    # 须为空
go vet ./...
go test -timeout 300s ./...   # 纯单测（无需外部依赖）
```

**真库用例的口径**：需要 PostgreSQL 与 Redis 的用例通过环境变量开启，未注入时**整组跳过**（CI 就是这样跑的）：

```bash
cd go
CCH_TEST_DSN='postgres://<user>:<pw>@127.0.0.1:5432/<db>' \
CCH_TEST_REDIS_URL='redis://127.0.0.1:6379/15' \
go test -timeout 600s ./...
```

要验证持久化语义、Redis 脚本、迁移与限流，必须自己注入这两项——否则「全绿」只代表纯单测通过。`-timeout` 一律带上：仓内既有用例有过挂住的记录。

## 夹具纪律（踩过的坑，别再踩）

1. **共享库互扰会造出假红**：多个包并行跑时若都在写同一批表（价格表、投影标记等），会出现「单跑绿、全跑红」。已用**跨包互斥（PG 咨询锁）**处理；新增这类夹具时按同一模式加锁，别靠调大重试。
2. **夹具必须自清理**：写入的行要带可识别前缀/标记并在结束时删除，否则残留行会破坏其它用例的聚合断言。
3. **不得让用例等真实长超时**：默认路径要立刻返回，真实超时用注入时钟或极小窗口触发；`go test` 必须带 `-timeout`。
4. **不要动共享库里的既有数据**：整表删除、写全局标记这类语义用 fake store 覆盖，不要拿真库做破坏性验证。
5. **测试数据清理**：前端侧不再有清理助手（原 `cleanup-utils.ts` 依赖的 `@/drizzle/db` 早已随 Node 退役删除，链路在运行期必抛错，已移除）；Go 侧由各用例自行收尾。

## 目录结构

```
tests/
├── unit/         # 前端单测（按被测面分目录，含 docs/ 文档钉子）
├── load/         # 负载、对拍与切换验收夹具（各有独立 README）
├── helpers/      # 测试助手
├── setup.ts      # 全局 setup
└── vitest.base.mts
```

## 提交前建议跑的门禁

```bash
bun run typecheck && bun run lint && bun run test
cd go && gofmt -l . && go vet ./... && go test -timeout 300s ./...
```

跨层改动（协议契约、词表、构建链）另跑 `bun run build`（静态导出 + 压缩嵌入）。
