# 三协议转换矩阵（本地真上游）

- 网关：`http://127.0.0.1:57689`
- 上游：见 setup.sh 的 MATRIX_UPSTREAM_URL（同一台，讲三种协议）
- 约定标记：`MATRIX7F3A`

| 格 | 同协议 | 非流式 | 流式 | 判定 |
| --- | --- | --- | --- | --- |
| `claude>anthropic` | 基线 | 200/10字/marker=y/ttfb=4875ms | 200/term=y/10字 | **PASS** |
| `claude>chat` | 跨协议 | 200/10字/marker=y/ttfb=2256ms | 200/term=y/10字 | **PASS** |
| `claude>responses` | 跨协议 | 200/10字/marker=y/ttfb=1848ms | 200/term=y/10字 | **PASS** |
| `chat>anthropic` | 跨协议 | 200/10字/marker=y/ttfb=2666ms | 200/term=y/10字 | **PASS** |
| `chat>chat` | 基线 | 200/10字/marker=y/ttfb=2183ms | 200/term=y/10字 | **PASS** |
| `chat>responses` | 跨协议 | 200/10字/marker=y/ttfb=2205ms | 200/term=y/10字 | **PASS** |
| `responses>anthropic` | 跨协议 | 200/10字/marker=y/ttfb=1860ms | 200/term=y/10字 | **PASS** |
| `responses>chat` | 跨协议 | 200/10字/marker=y/ttfb=2604ms | 200/term=y/10字 | **PASS** |
| `responses>responses` | 基线 | 200/10字/marker=y/ttfb=2021ms | 200/term=y/10字 | **PASS** |

## 功能穿越

| 格 | tools | 思考强度 | 多轮/回传 |
| --- | --- | --- | --- |
| `claude>anthropic` | 200 调用发生并取到名字/参数 | 200/1字 | first=200 second=200 rejected=否 |
| `claude>chat` | 200 调用发生并取到名字/参数 | 200/1字 | first=200 second=200 rejected=否 |
| `claude>responses` | 200 调用发生并取到名字/参数 | 200/1字 | first=200 second=200 rejected=否 |
| `chat>anthropic` | 200 调用发生并取到名字/参数 | 200/1字 | first=200 second=200 rejected=否 |
| `chat>chat` | 200 调用发生并取到名字/参数 | 200/1字 | first=200 second=200 rejected=否 |
| `chat>responses` | 200 调用发生并取到名字/参数 | 200/1字 | first=200 second=200 rejected=否 |
| `responses>anthropic` | 200 调用发生并取到名字/参数 | 200/1字 | first=200 second=200 rejected=否 |
| `responses>chat` | 200 调用发生并取到名字/参数 | 200/1字 | first=200 second=200 rejected=否 |
| `responses>responses` | 200 调用发生并取到名字/参数 | 200/1字 | first=200 second=200 rejected=否 |
