// Package forward 实现上游转发主干：把「客户端请求 + 选中的供应商」编译成上游请求计划，
// 按 Node forwarder.ts 的尝试循环语义逐个供应商尝试，并把结果交给调用方结算。
//
// # 对齐范围
//
//   - 请求计划：URL（复用 dial.BuildUpstreamURL，保住 base URL 自带的路径前缀）、
//     出站 headers（黑名单过滤 + 覆盖顺序 + 鉴权头 + 自定义头 + 客户端 IP 头）、
//     正文（客户端协议 → 目标协议的转换，或同协议原样透传）。
//   - 尝试循环：外层供应商切换（上限 20）、内层同供应商重试（provider.max_retry_attempts）、
//     网络错误推进端点索引、失败等待 100ms、失败供应商进入排除列表。
//   - 错误分类：对齐 errors.ts 的 categorizeErrorAsync 优先级链（真实 5xx > 客户端中断 >
//     本地过载 > 传输错误 > 404 > 400 存储容量 > 错误规则命中 > 其余 HTTP > 空响应 > 系统错误）。
//   - 非流式：读完整正文（有硬上限，Node 侧无上限）、状态码分类、失败即按分类重试或切换，
//     成功则产出待结算结果。
//   - 流式（ForwardStream）：与非流式共用同一份尝试循环，差别在成功判定被推迟到流终态——
//     HTTP 200 的正文可能是错误 JSON 或畸形流，在「收到响应头」时无法判真假。
//     门控在 precommit 期间缓冲前缀，失败即换供应商且客户端零字节；提交后前缀与上游正文
//     一起交给调用方（Stream），由调用方按需拉取。
//   - 流式观测（Observer）：只保留 O(1) 计数与终态事实（用量/模型/终止标记/错误/字节数/TTFT），
//     加一个有界头尾窗口供调试工件；驻留量与正文长度无关。
//   - 断线计量（DetachedStreamBudget）：客户端断开后按预算决定是否继续读上游以拿到终态用量；
//     预算不足时立即放弃，不排队。
//
// # 结算纪律
//
// 非流式在拿到完整正文时就地结算；流式推迟到流终态，且**整条流只结算一次**
// （Stream 的终态路径与泵的幂等 settle 共同保证）。重试与门控失败都不产生任何终态落库。
//
// # 刻意不做
//
//   - 供应商参数覆写（anthropic/openai/codex 各自的 max_tokens、thinking 等）只留
//     OverrideApplier 钩子，实体属后续波次。
//   - fake-200 检测（HTTP 200 正文实为错误）只留 BodyErrorDetector 钩子，实体属后续波次；
//     在钩子为空时，这类响应会被当作成功——这是已知缺口，已在 Result.DetectorMissing 留痕。
//     流式路径不受此缺口影响：它靠门控判定（错误帧/空流都在提交前被拒）。
//   - 观测窗口默认（HeadBytes/TailBytes 各 4 KiB）刻意小于 Node 的 10 MiB 快照上限：
//     Node 的上限服务于整条流的字节级快照，而 700 MiB 预算下按流保留 10 MiB 会吃掉全部预算。
//     需要与 Node 同口径时由接线层把窗口抬到 NodeStreamHeadBytes 一档。
//   - 落库与结算实体属 terminal 包；本包只通过 StreamSettler 接缝与 pctx 的一次性断言交接。
//   - 竞速（hedge）与回放（replay）的引流只用本包的预算口径占位，实体属后续波次。
//   - 模型重定向规则匹配在此包内实现（Node 的 provider-model-redirects），与 route 包的
//     私有 matcher 存在重复；待该 matcher 上提到共享包后应删除本包副本。
package forward
