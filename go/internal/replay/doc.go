// Package replay 是请求回放子系统（src/app/v1/_lib/proxy/replay/ 六个文件的移植）。
//
// 布局：
//   - identity.go：回放身份（ReplayID/Verifier/ScopeTag）推导，与 replay-identity.ts 逐条对齐；
//   - text.go：正文分片的边界工具；
//   - budget.go：并发 spool 上限与降级语义；
//   - store.go：双层存储（Redis 热层 + PG 持久层），Lua 脚本逐字节复刻 replay-store.ts；
//   - spool.go：owner 侧 write-behind 写入链（observe -> 冲刷 -> 完成屏障 -> PG 持久化）；
//   - attacher.go：守卫链的 ReplayAttacher 实现（hit 短路返回，miss 放行并 claim 登记）。
//
// 内存不变量（issue-1408 的持有链结论，硬约束）：
//   - 任何时刻本地不得持有完整的客户端可见正文；spool 只留「待写批次」，批次有界
//     （MAX_QUEUED_WRITE_BYTES），超限即 disable（降级为不做回放）。
//   - Redis LIST 元素保持小块（MAX_REDIS_CHUNK_BYTES），读取端按页取、不整段搬入堆。
//   - 服务已完成条目时按页读取后拼接，单请求内存上界即单条响应上限
//     （REPLAY_MAX_PAYLOAD_BYTES，默认 8 MiB）。
//
// 键形制（与 Node 逐字节一致，切换期间两侧互相命中）：
//
//	cch:replay:owner:<replayId>   租约，SET NX EX 45s
//	cch:replay:meta:<replayId>    JSON 状态机（owning/completed/aborted）
//	cch:replay:chunks:<replayId>  LIST，客户端可见文本的切片
//
// 已知简化（本包有意裁剪，升级路径见各注释）：
//   - live attach 不做尾随轮询：owning 条目一律视为 miss（不吐半截流），
//     完整「逐块跟尾」需要数据面侧把守卫响应从静态正文换成流式 Body，属接线波次。
//   - 完成条目的消息请求审计行经 store.CreateMessageRequest 写入（is_replay 标记），
//     blocked_reason 溯源与 usage 投影未搬（store 侧缝隙，接线波次补）。
package replay
