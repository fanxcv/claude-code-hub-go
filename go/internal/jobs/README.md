# internal/jobs：常驻后台任务

云价格同步与可用性投影回填的 Go 实现。Node 后端已完全退役，本包是**唯一实现**：下表登记的
「上游对应」只用于口径追溯与考古（那些文件已不在仓库里）。

## 任务与对应关系

| 任务 | Go 落点 | 上游对应（已删除，仅供考古） | 触发 |
| --- | --- | --- | --- |
| 云价格同步 | `pricesync.go` + `cpt.go` + `cpt_convert.go` + `vendors.go` | 上游 price-sync 四件套（cloud-price-updater / cloud-price-table / cpt-schema / cpt-convert）与 model-prices 服务端动作 | 启动即跑一次，之后每 30 分钟 |
| 可用性投影回填 | `availproj.go` | 上游 availability projection worker | 启动即跑一次，之后每 5 分钟复查标记 |
| 调度 | `scheduler.go` | 各自的 setInterval / 启动期一次性调用 | — |
| 单例锁 | `leader.go` | 上游的 withAdvisoryLock | 每轮任务 |
| outbox 已发布行保留期清理 | `outbox_retention.go` + `store/outbox_cleanup.go` | 无（Go 专有；上游从不删这张表） | 启动即跑一次，之后每 1 小时 |

手工跑一轮（验收与排障用）：

```bash
cd go
CCH_TEST_DSN=... go run ./cmd/jobsrun -job=price-sync          # 走版本短路
CCH_TEST_DSN=... go run ./cmd/jobsrun -job=price-sync -force  # 强制重放整表
CCH_TEST_DSN=... go run ./cmd/jobsrun -job=avail-backfill
```

## 与已退役 Node 实现的差异（全部有意为之）

| # | 差异 | 理由 |
| --- | --- | --- |
| 1 | 价格同步用 PG advisory 锁（`claude-code-hub:cloud-price-table-sync`） | 上游只有进程内 `AsyncTaskManager` 去重，多实例会各拉一次 28 MiB。锁名是 Go 自有的：双跑期两端无法互斥（Node 下线后该约束消失）。 |
| 2 | 回填沿用上游的锁名（`claude-code-hub:availability-projection-backfill`） | 键是 `hashtext(name)`，同名即互斥：双跑期只有一侧真正回填，Node 下线后无影响。 |
| 3 | 价格表正文有 128 MiB 上限 | Node 无上限。对端异常返回超大正文时应当直接失败，而不是把内存吃光。 |
| 4 | 每 2000 个模型记一条 `price_sync_progress` | 1.1 万行的长任务需要可观察的进度。 |
| 5 | 回填按 `CCH_JOB_AVAIL_BACKFILL_INTERVAL_MS` 复查标记（默认 5 分钟） | Node 只在进程启动时跑一次 bootstrap；复查让「首次回填被中断」无需重启进程即可续跑。复查本身是一条主键查询。 |
| 6 | 调度器是固定间隔（无 cron 表达式） | 两个任务都是固定间隔，没有日历语义；引入 cron 解析只多一个可错的输入面。 |
| 7 | `ListManualPriceModelNames` 只返回名字集合 | Node 返回整行 Map，调用点只做 `has(modelName)`。差异不影响判定。 |
| 8 | 锁连接不占分道预算（`store.OpenDedicatedConn`） | 从池里借锁连接会让第二个等锁实例**阻塞在借连接上**而不是快速得到「锁被占了」；预算为 1 的分道会直接死锁。Node 同样是另开 `max: 1` 的客户端。 |
| 9 | `RequestSync` 尚无生产调用点 | Node 在响应处理命中未知模型时触发（`response-handler.ts:6735`）；Go 数据面的对应位置未接线。接线只需调一次本方法。 |
| 10 | outbox 按时间删**已发布**行（保留 7 天，分批 1000） | 上游从不删。这张表只增不减（生产实测 88 万行 / 483 MB），而已发布行没有任何读方（唯一读路径带 `WHERE published_at IS NULL`），幂等去重落在 `proj_applied_requests` 主键上。删除谓词必含 `published_at IS NOT NULL`，未发布行任何情况下不删：删了即投影永久缺口且无补偿路径（回填已 done）。 |

## 口径一致性（已实测）

一次强制重放（`-force`）针对真实库跑了 10902 个模型：**added=0 / updated=0 / unchanged=10902**。
即 Go 的转换器对同一张云端价格表算出的 `price_data`，与库中由 Node 写入的每一行都判为相等。
版本指纹也一致：Go 算出 `4a1d3f803b0acc11+cvt1`，与库中 `cloud_pricing_catalog.version` 逐字相同
（`CPT_CONVERTER_REV` 两端共用同一修订号）。

转换器的行为钉子见 `cpt_convert_test.go`；`vendors_test.go` 把内嵌图标映射与 `VENDOR_DISPLAY_NAMES`
逐字/逐项对回 TS 原文，Node 侧一改即报红。

## 历史：双跑期（Node 与 Go 同时运行）的风险

Node 已下线，此节只留结论备查：价格同步双跑最坏是同一模型被写两次（两侧写入内容相同且幂等）；
回填因锁名相同而互斥，不可能双跑；版本短路只在对端写入同内容时触发，无正确性影响。

## 测试

```bash
cd go
go test ./internal/jobs/                     # 无库：转换器、同步语义、回填分块、调度器、漂移钉子
CCH_TEST_DSN=... go test ./internal/jobs/    # 真库：锁互斥、新 store 方法的 SQL
CCH_TEST_SYNC_DSN=... go test ./internal/jobs/ -run 'OutboxRetention'   # 可弃库：真删行的保留期清理
```

`CCH_TEST_SYNC_DSN` 指向的必须是**可弃库**（`CREATE DATABASE x TEMPLATE cch_loadtest` 一类）：
那条用例会真删 `outbox_events` 的行，落在共享测试库上等于替别的套件清库。
它包含最关键的一条负例：未发布（`published_at IS NULL`）但很旧的行**必须保留**。

真库用例**刻意不覆盖会污染共享库的路径**（整表删除、写 `backfill_done`、写 `cloud_pricing_catalog`），
那三条语义由无库的 fake store 用例覆盖——用合成夹具驱动真库的整表切换等于清空该库的云端价格。
