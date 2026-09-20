// Package tracing 把每次请求的终态事实上报到 Langfuse（出站 HTTP，旁路）。
//
// 存在理由：LANGFUSE_* 这组变量早已进配置契约（`config/env.go` 的 envSpecs）并在启动摘要里
// 用 `langfuseConfigured` 报告「已配置」，但 Go 侧从来没有上报实现——即运维填了 key 也不会有
// 任何数据出现。本包补上这条链路。
//
// 四条设计约束（每条都有对应测试）：
//
//  1. **旁路，不进请求关键路径**。`RecordTerminal` **不返回错误、不阻塞、不 panic**：入队是
//     非阻塞的（队列满即丢并计数），出站在本包自己的 goroutine 上。上报失败不得影响结算，
//     调用方也没有可做的补救——与 `terminal.RollupRecorder` / `NewRowsNotifier` 同一取舍。
//
//  2. **默认不上报正文**。上报的是行 id、用户 id、模型、token 计数、耗时、状态码与错误文本，
//     没有 prompt / completion 字段可填（`terminal.TraceRecord` 结构上就没有）。这是 PII 面
//     最小化的硬约束：打开上报不等于把用户对话送出去。
//
//  3. **凭据只进 Authorization 头**。public/secret 只在构造时折成一次 base64 串存在字段里，
//     不落日志、不进错误文案（`client_test.go` 用「日志里不得出现 key 原文」钉住）。
//
//  4. **缺 key 即整体关闭**。`LANGFUSE_PUBLIC_KEY` 或 `LANGFUSE_SECRET_KEY` 任一为空时
//     `New` 返回 nil 接口，装配层照旧接线、行为与未接完全一致。
//
// 与 `usagefeed` / `pubstatus` 的关系：只复用范式（旁路接口 + 终态后触发 + 不重试降级），
// 不共享代码与队列——那两个是进程内信号面，本包是出站 HTTP，失败模型与生命周期都不同。
//
// 协议：`POST {LANGFUSE_BASE_URL}/api/public/ingestion`，Basic 认证
// （username=PUBLIC_KEY、password=SECRET_KEY），body 为 `{"batch":[...]}`，gzip 压缩，
// 期望 207 Multi-Status 逐事件结果。**官方无 Go SDK**（仅 Python/JS，其他语言走原生 OTel），
// 故按公开 ingestion 协议自实现，不引入依赖。
//
// 已知未核验项：本包无法在开发环境对真收集器核验字段名。usage 与成本同时按 v2 形状
// （usage/totalCost）与 v3 形状（usageDetails/costDetails）给出，以兼顾两侧；
// 上线联调时按收集器回显核对一次，再删掉多余的那一份。
package tracing
