# go/internal/egress — 前门在途闸门与未实现路径的终端

本包曾是 node/go 双后端切换的**唯一入口判定**：解析归属规则、在入口一次性判归属、把判给 Node
的请求原样反代回退。Node 退役后归属仲裁没有对象了——本进程承载全部路径——于是归属判定、
路由白名单、Node 反代与回退目标全部删除。

留下的两件事：

1. **在途闸门**：`Middleware` 在请求入口计一次在途，`Drain` 停止接纳并按 `ErrDrainTimeout`
   等在途归零；`Draining()` 供 `/readyz` 在排空窗口内报 503。
2. **未实现路径的终端**：`Unimplemented()` 返回 404 `{"error":"not_found"}`，装配层把它设为
   数据面与管理面未命中路由的落点。

## 排空为什么不可撤销

```go
frontDoor := egress.New(logger)
frontDoor.Middleware(next)  // 入口闸门：过闸则 next，排空窗口内回 503
frontDoor.Drain(ctx)        // 停止接纳，等在途归零；ctx 先结束则返回 ErrDrainTimeout
frontDoor.InFlight()        // 当前在途数
frontDoor.Draining()        // 是否已进入排空窗口
frontDoor.Unimplemented()   // 未实现路径的 404 终端
```

一旦进入排空就不再重新开放（重开需新建实例）：**「在途归零」是退出序列的唯一安全点**——此刻
关连接池、停后台任务才不会把尚未落库的终态一并带走（`cmd/cchd` 的 `drainAndShutdown` 与
`waitSettlements`）。中途重新放行会把归零时刻无限推迟。

排空窗口内到达的新请求得到 `503 {"error":"shutting_down"}`，由调用方重试或改打新实例（滚动重启
的正常代价）。

## 未实现路径为什么是 404 而不是 501/503

`Unimplemented()` 的旧语义是「继续走 Node 原路径」——归属规则可以先声明尚未落地的路由，此时
返回 501/503 会被客户端误判成协议不支持而放弃重试。Node 退役后不再有第三方承载者：这类路径在
本进程就是**没有实现**，如实回 404。

有意且经用户裁决的行为差异：白名单内 `/api/admin/database/{export,import}` 与
`/api/internal/data-gen` 三条端点此前在无 Node 的环境里回 502 `node_backend_unreachable`，
现在回 404 `not_found`——它们本就是「裁决接受下线」的端点。

## 保留的词汇

| 符号 | 为什么留 |
| --- | --- |
| `Owner` / `OwnerGo` | `internal/pctx` 的 `SetOwner` 校验契约；数据面与模型面在建上下文时恒传 `OwnerGo` |
| `OwnerNode` | 只作 pctx 取值契约的词汇保留（已无生产者）；删除须同时改 `internal/pctx` |
| `Family` + 四个具名族 | 协议方言词汇，消费者在 `internal/dataplane/routes.go` 与 `internal/pctx` |
| `NormalizePath` | 数据面与管理面路由表的键归一化（`/v1//messages` 必须与 `/v1/messages` 同键） |

## 配置

本包不再读取任何环境变量。页面面档位（`CCH_EGRESS_PAGES`，`off`/`embed`）由 `cmd/cchd` 消费：
`off` 档把页面面的终端交回 `httpapi` 的运维面，`embed` 档换成 `internal/uiapp` 的静态处理器。
`CCH_EGRESS_MODE`、`CCH_EGRESS_ROUTES`、`CCH_INTERNAL_PORT` 三条变量已随本包的归属判定与回退
目标一并删除。

## 测试

```bash
cd go && go test ./internal/egress/
```

覆盖：排空语义（在途未清则等待、归零即成功、超时可判别、排空窗口拒新请求）、闸门透传
（请求交给 next 且在途归零）、未实现终端的 404 形状与 JSON 头、路径归一化。
