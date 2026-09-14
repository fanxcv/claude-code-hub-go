# 观测器夹具：真实上游流的规模与来源

本目录的夹具是**对生产上游的真实抓取**，用于钉住观测器的驻留上限与「帧路径 vs 窗口回退」
这条分水岭——用合成数据无法体现真实帧有多大。

## 文件

| 文件 | 来源 | 规模 |
| --- | --- | --- |
| `ollama-codex-responses-small.sse` | `POST https://ollama.com/v1/responses`（provider 145 `Ollama Codex`，`provider_type=codex`），请求体 `{"model":"deepseek-v4.1-flash","input":"say hi","max_output_tokens":16,"stream":true}` | 4,977 B / 14 帧 / 最长行 1,113 B；以 `response.incomplete`（触顶）结束，含 `usage{input 32, output 16}` |

## 抓取到的关键规模（2026-09-13，同一次会话）

用真实客户端规模的请求体（213,713 B：`instructions` ~180 KB + 24 个带描述的 `tools`）：

```
响应 641,697 B / 仅 18 行
行长 max=213,635  次大=213,331  中位=147
超过 64 KiB 的行 = 3
  帧 #1  response.created    213,327 B   ← 回显完整请求体
  帧 #3  response.in_progress 213,331 B   ← 同样回显
  帧 #17 response.completed  213,635 B   ← 终态，带真实 usage{input 56812, output 4}
```

结论（决定了 `DefaultObserverParserBytes` / `DefaultObserverEchoFrameBytes` 的取值）：

1. **请求回显帧**是真实客户端流的常态，单帧可达数百 KB——必须像门控那样豁免
   （`gate.IsRequestEchoFrame`），否则真实客户端的第一帧就超限，整条流退化成 4 KiB 窗口猜测。
2. **终态帧本身**也可能远超 64 KiB（实测 213 KB），故常规上限抬到 256 KiB，
   让 usage 由帧路径给出而不是靠窗口猜。
3. `usage` 在终态帧的**尾部**（实测距帧尾约 200 B），所以窗口回退在溢出时通常仍能兜住——
   这解释了「修掉 UsageSeen 后缺失率从 45% 降到约 5%」，也说明窗口回退是兜底而非主路径。

## 刷新夹具

```bash
# 凭据从生产库取（provider 145），不要写进任何文件
KEY=$(...)
curl -sS -N --max-time 120 -X POST https://ollama.com/v1/responses \
  -H "Authorization: Bearer $KEY" -H "content-type: application/json" \
  -H "accept: text/event-stream" \
  --data '{"model":"deepseek-v4.1-flash","input":"say hi","max_output_tokens":16,"stream":true}' \
  > ollama-codex-responses-small.sse
```

夹具体积刻意保持小（< 8 KiB）：大帧的规模效应由
`observe_w70_echo_cap_test.go` 按**实测尺寸**合成复现，避免把几百 KB 的抓取塞进仓库。
