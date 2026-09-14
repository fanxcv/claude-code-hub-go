# 环境变量契约矩阵（env-parity）

本目录是一份**机器可读 + 人类可读**的环境变量契约快照，用于 Go 数据面装载器
逐项对齐 Node 侧 `env.schema.ts`。

## 文件

| 文件 | 生成方式 | 用途 |
| --- | --- | --- |
| `env-matrix.json` | `bun scripts/export-env-matrix.ts` | 机器可读契约：名称、类型、默认值、约束、枚举、说明注释、消费方 |
| `env-matrix.md` | 同上 | 按域分组的人类可读表 |
| `README.md` | 手写 | 本文件 |

矩阵**由脚本生成，不要手工编辑**。源文件是 `src/lib/config/env.schema.ts`，其 SHA256 记录在
JSON 的 `sourceSha256` 字段里；schema 改动后重跑导出即可看到 diff。

## 重生成

```bash
bun scripts/export-env-matrix.ts --check
```

`--check` 会额外做一次**运行时 zod 交叉校验**：把矩阵的键集与运行时 `EnvSchema.shape` 比对，
并对每个键用 `safeParse(undefined)` 取运行时默认值，与矩阵里的默认值逐项比对。任何差异都会
打印 `FAIL` 并以非零退出码结束。CI 里应始终带 `--check`。

导出是确定性的（相同的源文件产生逐字节相同的 JSON），因此矩阵可以安全地进 git 并在 review
里看 diff。JSON 内不含时间戳。

## 字段

| 字段 | 含义 |
| --- | --- |
| `name` | 环境变量名 |
| `schemaType` | `number` / `string` / `boolean` / `enum` / `unknown`（来自 zod 链式调用的静态判定） |
| `wrapper` | `raw` / `optionalNumber` / `optionalPreprocessed` / `optional`（源码里的封装方式） |
| `optional` | 是否允许缺省（有默认值或显式 optional 即视为是） |
| `int` | 是否带 `.int()` |
| `default` | 默认值的字面量结果；`null` 表示无默认值（运行时为 `undefined`） |
| `defaultRaw` | 默认值的源码原样文本（如 `64 * 1024 * 1024`），便于人工核对算术式 |
| `min` / `max` / `boundsKind` | 数值或长度边界；`boundsKind=value` 为取值边界，`length` 为字符串长度边界 |
| `enumValues` | `z.enum([...])` 的取值集合 |
| `descriptionComment` | 条目紧邻上方的 `//` 注释块原文 |
| `consumerFiles` | `src/` 内引用该变量的文件（按引用次数排序，最多 5 个；不含测试与 schema 自身） |
| `consumerFileCount` | 引用该变量的 `src/` 文件总数 |

另有 `countsByType` / `countsByDomain` 两个汇总字段，以及 Markdown 末尾的
`## 交叉字段约束`（`EnvSchema.superRefine` 引用的变量，Go 侧必须复刻同样的关系校验）。

## 消费方统计的口径

- 统计范围是 `src/**` 下的 `.ts` / `.tsx`，**排除** `*.test.*`、`__tests__/`、以及 schema 自身。
- 口径是"文本级全大写 token 出现"，因此可能出现同名字符串常量导致的假阳性；`consumerFiles`
  只用于定位"这个变量在哪一层被读"，不作为唯一真源。
- `无 src/ 内静态引用` 的变量通常是运行时消费（如 `PORT`/`TZ` 由 Next 与容器读，`ENABLE_LEGACY_ACTIONS_API`
  由 `server.js` 读）——不是"死变量"，导出时会单独列出。

## Go 侧怎么用

1. **对账**：Go 侧把装载器支持的变量名逐行写进 `go/env-parity.txt`（`#` 开头为注释），然后：

   ```bash
   node scripts/check-env-parity.mjs            # 报告差异，退出码 0
   node scripts/check-env-parity.mjs --strict   # 有差异即退出码 1，适合 CI
   ```

   脚本报告三类差异：矩阵有而 Go 清单缺（Go 漏配）、Go 清单有而矩阵没有（多配或拼写错误）、
   Go 清单内重复行。清单文件不存在时只提示不报错（退出码 0），便于在 Go 侧尚未提供清单时
   先把导出与矩阵纳入 CI。

2. **fail-fast 与默认值**：Go 装载器要求
   - 每个 `enumValues` 非空的变量按同一集合校验；
   - `min`/`max` 按同一 `boundsKind` 解释（数值边界 vs 长度边界）；
   - 缺省时取 `default`（`null` 表示无默认值，必须显式提供或保持零值语义）；
   - `descriptionComment` 是语义说明的唯一真源，Go 侧注释应与之同义；
   - `## 交叉字段约束` 里的关系校验必须复刻（例如
     `DETACHED_STREAM_METERING_RESERVE_BYTES <= DETACHED_STREAM_BUDGET_BYTES`、
     `STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP >= 4 * STREAM_GATE_PREBUFFER_BYTE_CAP`）。

3. **不要手抄默认值**。默认值会随 schema 变化，抄写必然漂移；把 JSON 当作唯一输入，
   必要时由构建期代码生成 Go 侧的默认值表。
