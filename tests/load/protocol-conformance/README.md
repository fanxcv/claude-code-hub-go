# 协议转换一致性语料（protocol conformance corpus）

本目录存放**语言中立**的协议转换一致性语料，是 Go 侧 codec 的逐字节验收入口（加载点
`go/internal/convert/corpus_test.go`；`corpus/selection.json` 同时被 `internal/convert` 的选路
逻辑引用为金标）。

**语料已冻结**：它记录的是 Node 实现在退役时刻的行为（生成器与 TS 侧实现已随 Node 退役删除，
故**无法再生**）。**语料不能证明实现是对的**，它只固定「Go 与 Node 行为一致」这一个命题；
正确性由 `go/internal/convert/*_test.go` 自行保证。

## 产物

```text
corpus/requests.json    24 条  请求侧：客户端线请求体 → 上游线请求体
corpus/responses.json   15 条  响应侧非流式：上游线响应体 → 客户端线响应体
corpus/streams.json      9 条  响应侧流式：上游线 SSE 字节 → 客户端线 SSE 字节
corpus/selection.json   60 条  选路判定（含 gemini 两线的不可转事实）
corpus/paths.json       27 条  上游路径映射（含跨线不可转与未知路径）
```

## 重新生成（已不可用）

生成器（scripts/export-conformance-corpus.ts 及其 node 转调包装 .mjs、夹具源
scripts/conformance-fixtures.ts）已随 Node 退役删除。语料是**只读金标**：不得手工编辑，
需要新增覆盖时在 `go/internal/convert/*_test.go` 内直接写用例。

自检断言：各文件用例数非零、`id` 全局唯一、`input` 非空、`expected` 非空（或显式
`expectError`）、native 直通用例两侧逐字节相同、两个 kind 的协议对两端不同、流式用例输出帧数非零、
六组跨协议对在三个 kind 上均有覆盖、**两次生成逐字节一致**、gemini 客户端不得被判为可转换。

## 用例 schema（requests / responses / streams）

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `id` | string | 唯一且稳定，形如 `<kind>.<来源线>.<夹具名>[.to.<目标线>]` |
| `familyFrom` | string | **客户端线**：`anthropic-messages` / `openai-chat` / `openai-responses` |
| `familyTo` | string | **上游线**（即 `targetProtocol`） |
| `kind` | string | `request` / `response` / `stream` |
| `input` | string | 输入字节的文本形态（见 `inputEncoding`） |
| `inputEncoding` | string | 当前恒为 `utf8`；`base64` 保留给未来二进制载荷 |
| `expected` | string | 期望输出字节的文本形态；`expectError` 为真时为空 |
| `expectedEncoding` | string | 同上 |
| `expectError` | boolean | 期望抛错。当前语料全部为 `false`（契约 I6 要求编解码不抛错） |
| `notes` | string | 人读说明；含归一化等已执行动作 |
| `passthrough` | boolean | `true` 表示同协议对：转换层不参与，两侧必须逐字节相同 |
| `idNormalizers` | string[] | 生成期已执行的替换规则；消费前须对两侧施以同一替换，再比较 |
| `clientPathname` | string? | 仅 `kind=request`：客户端入站路径 |
| `rewrittenPathname` | string\|null? | 仅 `kind=request`：`resolveUpstreamPath` 的结果 |
| `toolNameRestore` | object? | 仅 `kind=response`：注入的「规范化名 → 客户端原名」逆转表 |
| `frameCount` | number? | 仅 `kind=stream`：输出帧数量 |
| `feedMode` | string? | 仅 `kind=stream`：当前恒为 `split-half`，即输入按 `floor(byteLength/2)` 切成两块依次喂入 |
| `errorMessage` | string? | 仅 `expectError=true`：实际错误信息，供排查 |

### 确定性规则

1. **键序稳定**：所有对象键递归排序后再序列化，2 空格缩进。
2. **数组保序**：数组顺序承载语义（帧序、message 序），不得排序。
3. **合成信封 id 归一化**：跨线编码会为客户端线合成一个信封 id，其随机后缀来自
   `shared/response-id.ts` 的 `randomBytes(8)`（16 位十六进制，前缀为 `msg_cch_` / `resp_cch_` /
   `chatcmpl-cch-`）。该随机性是**有意的**——同一网关内必须唯一，否则客户端无法区分并发的被
   转换请求。故此类用例把后缀替换为 `<generated>` 并在 `idNormalizers` 里声明规则：
   消费者须先对**两侧**施以同一替换，再逐字节比较；
   **不得**断言具体 id 值，只断言前缀与长度。
4. **`feedMode=split-half`**：流式用例的输入在 UTF-8 字符中间被切开，用于覆盖跨 chunk 边界状态。
   Go 侧必须按同一字节偏移切分，否则无法与 `expected` 对齐。

### Go 侧消费要点

1. 读 `corpus/*.json`，按 `kind` 分派：`request` 走 `decodeRequest`+`encodeRequest`，
   `response` 走 `decodeResponse`+`encodeResponse`，`stream` 走流式解码器+编码器。
2. 请求侧上下文与 `proxy/protocol-request-converter.ts:buildUpstreamRequest` 一致：
   `clientFormat` 取 `familyFrom` 对应的路由层格式（`claude` / `openai` / `response`），
   `targetProtocol` 取 `familyTo`，`model` 取输入体的 `model`，`stream` 取输入体的 `stream === true`，
   且**只设置**写向目标线的工具名改写钩子（`toWireToolName`）。
3. 响应侧上下文与 `proxy/protocol-response-converter.ts:buildConvertCtx` 一致：
   **只设置**还原钩子（`fromWireToolName`），映射来自用例的 `toolNameRestore`（为空则恒等）。
4. `passthrough=true` 的用例断言的是「native 直通字节不变」——Go 侧这条路径必须完全绕过转换层。
5. 比较顺序：先应用 `idNormalizers`，再逐字节比较；`expected` 为空且 `expectError=true` 时改为
   断言实现不抛错。

## selection.json 与 paths.json 的 schema

这两份语料服务于「转换是否发生」与「打到哪个上游路径」，字段不同，不与上面共用。

`selection.json` 用例字段：`id`、`clientFormat`（`claude` / `openai` / `response` / `gemini` /
`gemini-cli`）、`providerType`（`claude` / `claude-auth` / `codex` / `openai-compatible` / `gemini` /
`gemini-cli`）、`conversionEnabled`、`compat`（`native` / `convertible` / `incompatible`）、
`targetProtocol`、`plan`（`{clientProtocol,targetProtocol}` 或 `null`）。

当前分布：`native` 12 条、`convertible` 8 条、`incompatible` 40 条（三线之间可转的 6 组
协议对 × 开关打开 = 8 条，另两组因供应商类型重名合并）。

`paths.json` 用例字段：`id`、`targetProtocol`、`clientPathname`、`expected`（路径或 `null`）。

## 已记录的陷阱

1. **`resolveTargetProtocol` 不是判定权威。** 语料里存在大量
   `targetProtocol` 非 null 但 `compat=incompatible`、`plan=null` 的组合，两类来源：
   `gemini` / `gemini-cli` 客户端 + 三线供应商（开关打开时仍不可转，见陷阱 2）；
   以及任一跨协议对在 `conversionEnabled=false` 时（如 `selection.claude.codex.off`）。
   决定是否转换的只有 `resolveProtocolCompat` / `planConversion`；只看 `targetProtocol` 会让
   请求按客户端格式原样打给不兼容的上游，或无视总开关强行转换
   （`protocol-convert/index.ts` 的注释已就此说明）。
2. **gemini 两线不在本层范围内。** `protocolOfClientFormat("gemini")` 与
   `protocolOfProviderType("gemini")` 均返回 null，因此不存在「gemini 参与的转换用例」；
   语料以选路判定的形式把这一事实钉住（`compat !== "convertible"` 且 `plan === null`）。
   gemini 的实际转换走的是 `response-handler.ts` 的另一条分支，不在本语料范围。
3. **合成信封 id 不可复现**（见上「确定性规则」第 3 条）。
4. **契约 I6 的 fail-open 行为已被固化。** `request.openai-chat.malformed.*` 与
   `response.anthropic-messages.malformed.*` 记录的是畸形输入下的实际输出，而非抛错：Go 侧若
   改成抛错，这些用例会失败——这是有意的，因为生产接线依赖「解码不抛错」。
5. **`expectError` 当前恒为 false**，字段保留给未来确需断言抛错的用例。

## 无法语言中立化的部分（Node 专用）

| 项 | 位置 | 为什么无法中立化 | Go 侧建议 |
| --- | --- | --- | --- |
| 响应转换接线 | `proxy/protocol-response-converter.ts` | 依赖 `Response`/`Headers` 与 `ProxySession`（Node 专有会话对象）；含响应头重建、`content-encoding` 判定与异常兜底 `restoreUnchangedResponse` | 以等价语义重写（Go 侧 `http.Response` 对应物），并单独补测试；其中「非 2xx 不转换」「非 identity 编码不转换」两条判据必须逐条保留 |
| 请求转换接线与快照 | `proxy/protocol-request-converter.ts` | `primeOriginalBody` 按 **JS 对象身份** 决定是否覆盖快照（整流器 `structuredClone` 出新对象后必须覆盖，否则重试会用到整流前的体） | Go 侧不能照搬对象身份语义，须改为显式「基线请求体」字段 + 显式「整流结果已应用」标记 |
| 工具名钩子的接线时机 | 上述两文件 | 语料只覆盖钩子生效时的纯函数结果，不覆盖「谁在什么时机设置哪个钩子」 | 按语料 `toolNameRestore` 与请求侧 `toWireToolName` 的分工照做：编码器过改写钩子，解码器过还原钩子 |
| 流式逐字节边界 | `codecs/**/__tests__/stream-edges.test.ts` | 逐字节喂入的重放成本高，未导出 | Go 侧自行补逐字节与 UTF-8 中间切分的自测；本语料只固定「对半切」一种 |
| 事件观测与整流 | `proxy/thinking-*-rectifier.ts`、`response-fixer/` 等 | 不在 `protocol-convert` 范围内 | 属另行迁移范围，不在本语料内 |

## 夹具来源（历史）

输入夹具当年**逐字拷贝**自 TS 测试，集中在 scripts/conformance-fixtures.ts（已删除），每个常量
上方曾标注来源文件：

- `protocol-convert/__tests__/conformance.test.ts`（三线同语义请求、三线 SSE 事件）
- `codecs/openai-chat/__tests__/request.test.ts`、`codecs/anthropic-messages/__tests__/request.test.ts`（富请求）
- `codecs/anthropic-messages/__tests__/response.test.ts`、`codecs/openai-chat/__tests__/response.test.ts`、`codecs/openai-responses/__tests__/response.test.ts`（非流式响应）

这些来源文件已随 Node 退役删除；语料保持现状，不再与上游同步。
