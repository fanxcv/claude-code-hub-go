// Package gate 是 /v1 数据面的**流式内容门控**：在向上游客户端透传之前，
// 按帧分类等待「首个有效内容帧」，据此决定提交透传还是判该供应商失败并 failover。
//
// 它复刻 TS 侧 src/app/v1/_lib/proxy/stream-gate/ 的四个模块：
//
//	sse.go        <- sse-frames.ts          增量 SSE 分帧（含缓冲硬上限）
//	classify.go   <- frame-classifier.ts   帧五态分类与协议家族规则表
//	budget.go     <- prebuffer-budget.ts   进程级前缀预算与租约
//	gate.go       <- stream-content-gate.ts 门控主流程与失败原因
//	observer.go   <- stream-content-gate.ts 的 shadow 观测部分
//
// # 不变量
//
// G1 只看字节：本包不读请求正文、不做协议转换、不写数据库、不写 Redis、不发起网络调用。
// 分类作用于**上游原生 wire 格式**（协议转换之前），因此家族按供应商类型选择
// （MapProviderTypeToFamily），而不是按客户端入站格式。
//
// G2 有界：前缀缓冲、parser 保留状态、shadow 观测三处都有硬上限；任何上游输入都不能
// 让它们无界增长（超限即 prebuffer_overflow，不是 OOM）。上限单位是**字节**（TS 侧是
// UTF-16 码点，见「与 TS 的有意差异」）。
//
// G3 提交前客户端可见字节恒为 0：失败时整段前缀被丢弃，由调用方按失败来源归类
// （供应商故障 / 请求作用域空结果，见 IsRequestScopedGateFailure）。
//
// G4 失败时上游正文所有权仍归调用方：本包不关闭、不排空上游正文——调用方需要按既有语义
// 决定是取消、还是排空并计费（drain 计费路径依赖正文仍可读）。
//
// # 与 TS 的有意差异
//
//   - 按字节而非 UTF-16 码点切分与计数：SSE 分隔符（LF/CR）恒为单字节，字节切分对 UTF-8
//     跨块天然安全。上限数值含义随之从「字符」变为「字节」，对 ASCII 协议正文等价。
//   - 不做 TextDecoder 的 U+FFFD 替换：非法 UTF-8 原样透传，最终由分类阶段的 JSON 解析失败
//     归为 malformed（TS 侧会先替换成 U+FFFD 再判 malformed，判定结果相同）。
//   - data: 后的单个前导空白按 ASCII 空白剥离（TS 用 /\s/，还包含 NBSP 等 Unicode 空白）；
//     分类阶段的 TrimSpace 会剥掉 Unicode 空白，故分类结果不受影响。
//   - 不用 SegmentedTextBuffer 做分块归并：Go 的 append 均摊 O(1)，且 parser 保留状态有硬上限。
//   - PrecommitError 不携带 HTTP 状态码：TS 侧会用错误规则把上游错误文本推断成 4xx/5xx，
//     那是错误规则层的职责（TS 的 ProxyError + inferUpstreamErrorStatusCodeFromText），
//     本包只交付可判别原因与错误体，状态码由调用方决定。
//
// # 不属于本包
//
// 门控模式（off/shadow/enforce）的解析、进程内共享预算的装配、以及前缀字节之后如何透传，
// 都是调用方（接线层）的职责：本包提供 Mode 字面量、DefaultBudget 与 Run 的返回值。
package gate
