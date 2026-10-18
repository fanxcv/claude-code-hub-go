# CCH 协议转换：对齐 relaykit 后的剩余缺口（截至 v1.12.1，2026-10-18）

参考实现：`relaykit`（new-api 仓内独立 Go module）。本轮已补齐的项见 git 历史
`v1.10.1..v1.12.1`（20 个提交，32 文件，+4021/−154）。

## 一、需扩枢纽（结构性，单条 >200 行）

1. **hosted tools 完整承载**：`server_tool_use` / `web_search_tool_result` / `mcp_call` /
   `mcp_tool_result` / `code_execution_*` / `computer_*` 目前只记损失（归因已按 Kind 细分），
   不承载。要真正跨线需在 `hub.Block` 加 hosted 变体或 `Item` 加 HostedItem，并三线编解码。
2. **工具声明 Kind**：`Tool` 只有 function 语义；`web_search` / `file_search` / `web_fetch` /
   `code_execution` / `computer_use` / `url_context` / `mcp` / `image_generation` / `native`
   九种声明跨线丢。relaykit 用 `Definition.Kind` 建模。
3. **`ToolChoice.Kind` / `NativeType` / `Raw` / `AllowedNames`**：四维缺失，未知形态不可原样回写。
4. **`image` source=file（Files API）**：已记 `LossImage`，不承载（无跨线等价物）。

## 二、受冻结语料阻断

5. **流式输入侧 usage**：`message_start.usage` 在 chat / responses 编码器被丢，修则打红
   `streams.json` 的 12+ 条金标（Node 行为快照）。需先裁定「是否偏离 Node 语料」。

## 三、需产品决策

6. **`max_output_tokens` → chat `max_completion_tokens`**：需新增「上游是否严格 OpenAI 新模型」
   能力位（跨 `dataplane`→`convert` 透传）。当前写 `max_tokens` 兼容面更广。

## 四、已知但成本/收益不划算

7. **流式 citations 的边缘形状**：三线基本通路已实现，未穷尽所有上游变体。
8. **`reasoning.summary` 与 `reasoning_text` 的分隔**：已由 `reasoningSeparatorEncoder` 统一实现。
