# go/internal/config

本包装载并校验 `cchd` 的全部环境变量。契约真源是 TS 侧
`src/lib/config/env.schema.ts`，机器可读形态为
[`tests/load/env-parity/env-matrix.json`](../../../tests/load/env-parity/env-matrix.json)
（由 `bun scripts/export-env-matrix.ts` 导出）。

目标只有一个：**同一份 `.env` 在 Node 数据面与 Go 数据面上必须得到同一个配置**，否则切换后端
时会出现「Node 能跑、Go 起不来」或更糟的「两边都起来但行为不同」。

## 覆盖范围

`env.go` 的规格表 `envSpecs` 覆盖矩阵里的全部 **70** 项：

| 类别 | 项数 | 说明 |
| --- | ---: | --- |
| `kindNumber` | 26 | `z.coerce.number()`，其中 19 项为整数、7 项允许小数 |
| `kindBool` | 18 | `z.string().default(...).transform(booleanTransform)` |
| `kindOptionalNumber` | 9 | `optionalNumber(...)`，未配置为 nil |
| `kindEnum` | 6 | `z.enum([...]).default(...)` |
| `kindString` | 4 | `z.string().default(...)` |
| `kindOptionalString` | 4 | `z.string().optional()` |
| `kindCredential` | 3 | `optionalPreprocessed(...)`：`ADMIN_TOKEN`、`CSRF_SECRET`、`DSN` |

Go 专有变量（`CCH_EGRESS_PAGES`、`CCH_GO_MAX_INFLIGHT_BYTES`、`CCH_GO_MAX_STREAMS`、
`CCH_PATROL_*`、`CCH_PPROF_*`、`CCH_SAME_PROTOCOL_WEIGHT_K`、`GOMEMLIMIT`）不在契约内，单独解析，
**不写进** `go/env-parity.txt`，否则会被对账脚本判为「多配」。
历史：双后端时期的 `CCH_INTERNAL_PORT`、`CCH_EGRESS_MODE`、`CCH_EGRESS_ROUTES` 已随回退能力删除。

## 一张表驱动三处

`envSpecs` 同时驱动装载校验、结构体字段赋值（按 `env` 标签反射）与对账清单，三者同源，
因此不存在「清单里有、装载器没实现」这类静默分叉。三条不变量由测试钉住：

- `TestEnvSpecsStructAndParityListAreBijective`：规格表 ↔ `EnvConfig` 字段与标签 ↔ 对账清单，
  双向一一对应，总数 70。
- `TestEverySpecIsConsumedByLoader`：装载器必须真正读过每一项。
- `TestParityListMatchesMatrix`：对账清单与矩阵真源逐名一致。

## 生成与对账

```bash
# 在 go/ 目录下刷新清单（勿手工编辑 go/env-parity.txt）
go run ./cmd/envlist

# 在仓库根对账；差异数必须为 0
node scripts/check-env-parity.mjs --strict
```

## 语义要点（逐条复刻 zod）

1. **「未设置」与「设为空串」是两种输入。** 装载入口是 `LookupEnvFunc`
   （与 `os.LookupEnv` 同形）。`Load(getenv func(string) string)` 是兼容入口，把空串当未设置。
2. **布尔**：`booleanTransform(s) = s != "false" && s != "0"`，大小写敏感，不做归一。
   故 `AUTO_MIGRATE=""` 得到 `true`，`AUTO_MIGRATE="False"` 也得到 `true`。
3. **数值**：`z.coerce.number()` 对空串得到 `0`（JS `Number("")`），非数字即错；
   带 `.int()` 的项拒绝小数；随后校验上下界。
4. **`optionalNumber`**：未设置或空串走 `undefined`；内层 schema 带 `default` 的项
   （`DB_LOCK_TIMEOUT_MS`、`DB_STATEMENT_TIMEOUT_MS`、`REDIS_COMMAND_TIMEOUT_MS`）取该默认值。
5. **凭据类**：未设置、空串、`change-me` 一律视为未配置；`DSN` 另认占位符模板
   `user:password@host:port`。任何错误信息与脱敏摘要都不得回显原文。
6. **跨字段约束**（对应 `superRefine` 两条）：
   `DETACHED_STREAM_METERING_RESERVE_BYTES <= DETACHED_STREAM_BUDGET_BYTES`；
   `STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP >= 4 * STREAM_GATE_PREBUFFER_BYTE_CAP`。

## 与 TS 侧的刻意偏离（全部是「更严」，不影响合法配置）

| 编号 | 偏离 | 理由 |
| --- | --- | --- |
| D1 | `DSN` 的 scheme 必须是 `postgres` / `postgresql` | TS 只校验 URL 形式。Go 侧提前拒绝，避免带着非 PG 的 DSN 启动后在首个查询上失败 |
| D2 | `REDIS_URL` 的 scheme 必须是 `redis` / `rediss` | TS 完全不校验 |
| D3 | `PORT` / `CCH_INTERNAL_PORT` 必须落在 1..65535 | 契约里 `PORT` 无边界。端口 0 会让网关静默绑随机端口，比启动失败更难排查 |
| D4 | 兼容入口 `Load` 无法区分未设置与空串 | 它接收 `func(string) string`。生产入口应改用 `LoadLookup(LookupFromOS())`；`cmd/cchd` 目前仍走兼容入口，差异仅影响「显式设为空串」的变量 |
| D5 | 字符串长度界按 UTF-8 字符数计 | zod 按 UTF-16 code unit 计。阈值仅 1（`ADMIN_TOKEN`）与 16（`CSRF_SECRET`），差异只在含非 BMP 字符的凭据上出现 |
| D6 | 数值不接受 JS 的十六进制字面量（`Number("0x10")`） | 前导空白裁剪、空串得 0、非数字报错三项与 JS 一致；十六进制字面量在实际部署中不出现 |

## 门禁

```bash
cd go && gofmt -l . && go vet ./... && go test -timeout 120s -count=1 ./...
```
