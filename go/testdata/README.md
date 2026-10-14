# go/testdata —— 冻结金标（Node 退役前抓取，**不可再生**）

本目录是**共享**夹具：Node 实现退役（2026-09）前从它那里逐字节抓下来的黄金样本。
生产者（原 `src/**` 的 Node 后端、`scripts/*-golden.ts` 之类）已随退役删除，因此这里的每份
文件都**冻结**了——没有 Node 源码，就无法再生成、只能人工校改，故**不要删、不要"顺手"重写**。

各包自己的 `testdata/` 目录（例如 `go/internal/pubstatus/testdata/`、
`go/internal/pricing/testdata/`）是同包私有的样本，与本目录无关。

## 文件与读它的测试

| 文件 | 内容 | 读取方 |
| --- | --- | --- |
| `node_affinity_lua.txt` | Node 版亲和 Lua 原文的冻结副本 | `go/internal/route/affinity_lua_test.go` |
| `node_chain_reasons.txt` | Node 版链路原因文案 | `go/internal/forward/chainreason_test.go` |
| `node_limit_errors_ja.txt` | Node 版限流报错文案（日） | `go/internal/limit/messages_test.go` |
| `node_limit_errors_ru.txt` | Node 版限流报错文案（俄） | `go/internal/limit/messages_test.go` |
| `node_limit_errors_zh-TW.txt` | Node 版限流报错文案（繁中） | `go/internal/limit/messages_test.go` |
| `notify_webhook_golden.json` | Node 版通知 webhook 载荷 | `go/internal/notify/golden_test.go` |
| `golden/message_request_row.json` | Node 版请求行落库形状 | `go/internal/route/chain_test.go`、`go/internal/terminal/patch_test.go` |
| `golden/usage_ledger_row.json` | Node 版账本行形状 | `go/internal/terminal/patch_test.go` |

另有 `go/internal/convert/value_fastpath_test.go` 以 glob 扫本目录做 JSON 对拍。

## 纪律

- **只读不改**：改动这里的样本等于改金标，必须写清依据（旧实现行为出处）并单独提交。
- **不得删除**：删了会让上述漂移闸门要么报红、要么（若靠 glob 存在才跑）**静默变空闸**——
  一个静默跳过的闸门比没有闸门更危险，它会让人以为比对过。
