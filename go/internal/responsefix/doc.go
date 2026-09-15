// Package responsefix 移植 Node 的响应修复器（`src/app/v1/_lib/proxy/response-fixer/`）。
//
// 存在的理由：上游偶发返回**截断 JSON**、坏 UTF-8、畸形 SSE 帧或与客户端协议不符的
// 惰性帧。Node 在这一层能自愈（客户端看不到破损），Go 若原样透传，客户端会直接解析失败。
// 生产 `system_settings.enable_response_fixer=true` 且 `response_fixer_config` 齐全，
// 本包是它的唯一消费方。
//
// 边界（与 Node 逐条对齐，勿自行放宽）：
//
//   - 修复只针对「上游原始字节」。它在协议转换**之前**生效，因此进入本包的字节是上游线
//     方言（这也解释了惰性帧过滤为何能识别 `chat.completion.chunk`：那是上游 chat 线的
//     帧混进了 responses 客户端的流）。
//   - 修不了就不动：JSON 补全先做语法校验，补完仍非法则回退原字节（`repair_failed` /
//     `validate_repaired_failed`），绝不把「改坏的字节」交给客户端。
//   - 流式路径有内存上限：缓冲超过 maxFixSize 即降级为透传（不再修复），防止上游长时间
//     不发换行导致缓冲无界增长。
//   - 编码有损修复用 Go 标准库：Node 的 TextDecoder(fatal:false) 按 WHATWG 的「最大非法
//     子序列」产出替换字符，Go 的 strings.ToValidUTF8 按「连续非法字节」产出。两者对
//     绝大多数损坏输入等价（每个非法序列一个 U+FFFD），差异只在多字节序列的切分边界。
//     ponytail: 标准库优先；若将来出现具体的字节级对拍失败，再换成手写 WHATWG 解码器。
//
// 配置语义：`enable_response_fixer=false` 时数据面完全不动（连字节都不读）；
// `response_fixer_config` 的五个子项逐项生效（见 Config）。
package responsefix
