# 三协议转换矩阵测试台

用**一台真·多协议上游**（同一 URL 同时讲 anthropic / openai-chat / openai-responses）在**本地**端到端跑
「客户端协议 × 上游协议」的 9 个组合，并穿过 tools / 思考强度 / 多轮三件事。

不做的事：不动生产、不依赖 Node（用 `CCH_EGRESS_PAGES=off` + `CCH_EGRESS_ROUTES='* /v1/*'` 让 Go 自答数据面）。

## 三段用法

```bash
# 0) 二进制
cd go && CGO_ENABLED=0 go build -o /tmp/cchd-matrix ./cmd/cchd

# 1) 造环境（幂等；在一个**可弃库**里造 1 用户 + 1 密钥 + 3 供应商）
MATRIX_DSN=postgres://postgres:postgres@127.0.0.1:5432/cch_smoke \
  bash tests/load/protocol-matrix/setup.sh

# 2) 起本地网关（端口自选空闲端口，PID/端口写在同目录）
bash tests/load/protocol-matrix/start.sh          # 停：bash start.sh stop

# 3) 跑矩阵（9 格 × 非流式/流式 + 三项功能穿越）
node tests/load/protocol-matrix/run.mjs --all
```

产物：`results/matrix-<时间戳>.json`（逐格原始读数）与 `results/matrix.md`（人类可读表）。

## 两种选路模式（都要跑，用途不同）

| 模式 | 怎么选上游 | 用途 |
| --- | --- | --- |
| `MATRIX_SELECT=model`（默认） | 客户端发**模型别名**（`mock-anthropic` / `mock-chat` / `mock-resp`），别名各只放行给一个供应商；上游真名靠 `model_redirects` 映射 | 走「**带模型重定向**」的真实路径 |
| `MATRIX_SELECT=enable` | 客户端发**上游真名**，每格只启用目标供应商（其余禁用） | **绕开重定向**，单独看转换本身 |

两个模式**共用一套夹具**：`allowed_models` 同时放行「别名 + 真名」，故两种模式都不依赖额外改动。

## 断言与归因

每格断言：HTTP 200 · **响应形状 = 客户端协议**（抓「上游方言泄漏给客户端」）· 助手文本非空且含约定标记 `MATRIX7F3A` · usage 取到 input/output tokens · 流式收到**客户端协议**的终止事件且拼出文本。
归因规则：同协议三格是**基线**；基线也失败 → 判上游；仅跨协议失败 → 判**转换缺陷**。

## 上游模型探活（很重要）

该上游的模型会**下线**。测试前先探一把，别把上游故障误判成我们的缺陷：

```bash
for p in /v1/messages /v1/chat/completions /v1/responses; do
  curl -s -o /dev/null -w "$p %{http_code}\n" --max-time 20 -X POST \
    "$MATRIX_UPSTREAM_URL$p" \
    -H "Authorization: Bearer sk-test" -H "x-api-key: sk-test" \
    -H "anthropic-version: 2023-06-01" -H "content-type: application/json" \
    --data '{"model":"deepseek-v4.1-flash","input":"hi","max_output_tokens":8,"messages":[{"role":"user","content":"hi"}]}'
done
```

实测记录（2026-09-13）：`deepseek-v4-flash` 三协议均 **503** `auth_unavailable: no auth available (providers=openai-compatible-hc-test, …)`
⇒ 该模型在上游侧无可用渠道；`deepseek-v4.1-flash` 三协议均 200。故 `setup.sh` 默认用后者，可用 `MATRIX_MODEL` 覆盖。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `MATRIX_DSN` | `CCH_TEST_DSN` 或 `postgres://postgres:postgres@127.0.0.1:5432/cch_smoke` | 可弃库（**别用生产库**） |
| `MATRIX_REDIS_URL` | `CCH_TEST_REDIS_URL` 或 `redis://127.0.0.1:6379/15` | 建议独立 db 号 |
| `MATRIX_UPSTREAM_URL` | **必填**（无默认） | 形如 `http://<your-upstream>:4321`；三协议同一台 |
| `MATRIX_UPSTREAM_KEY` | `sk-test` | 上游测试凭据 |
| `MATRIX_MODEL` | `deepseek-v4.1-flash` | 上游真实模型名 |
| `MATRIX_CLIENT_KEY` | `sk-matrix-client` | 打本地网关用的客户端密钥 |
| `MATRIX_SELECT` | `model` | `model` / `enable`（见上表） |
| `MATRIX_BASE` | 读 `.cchd.port` | 网关基址（也可指到别处） |

## 已知的驱动自身坑（踩过并已修）

- **`reasoningRejected` 假阳性**：第一版把整个响应体做正则匹配，而成功响应里本来就带 `reasoning_content` 字段名 →
  200 被误判成「上游拒绝」。现改为**仅在 `status >= 400` 时**判定。
- **psql 命令标签污染捕获**：本机 psql 在 `-tA` 下单条 `INSERT … RETURNING` 仍会把 `INSERT 0 1` 打到 stdout，
  故 `setup.sh` 的 `q()` 必须带 `-q`。
- **shell 会把 `>` 当重定向**：`--cell claude>chat` 要加引号（`--cell 'claude>chat'`）。
