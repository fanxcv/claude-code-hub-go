# 供应商自定义请求头 · 端到端验收

验证「按供应商配置的静态出站请求头」从库里一路到达上游，并把两处最容易退化的语义钉住：
**未配置的供应商不得被注入**、**保留名与鉴权头不得被自定义头改写**。

## 用法

```bash
# 1) 二进制（工作树根）
cd go && CGO_ENABLED=0 go build -o /tmp/cchd-matrix ./cmd/cchd

# 2) 跑（自建「要求该头」的回显上游 + 自起本地网关；不动生产）
bash tests/load/provider-custom-headers/run.sh

# 保留网关便于复看：KEEP=1 bash tests/load/provider-custom-headers/run.sh
```

依赖：本地 PG（可弃库 `cch_smoke`，会被幂等清理 `custom-headers%` 前缀的行）与本地 Redis。
真上游**必填**（`CCH_REAL_UPSTREAM`，无内置默认值，避免默认跑时误连到他人的环境）：形如 `http://<your-upstream>:4321`。

## 为什么自建上游

真实 OpenCode 渠道对 `x-opencode-session` 是「缺即 400」，但我们验证时用的真实渠道
并不强制，**无法反证「我们发出的头确实到达了上游」**。`upstream.mjs` 因此做两件事：

1. 缺 `x-opencode-session` → 400（与真实渠道同款报错原文）；
2. 收到时把**全部请求头**回显进 `x-echo-headers`（base64 JSON）——断言既能看状态码，也能看头部本身。

## 断言清单（11 条）

| # | 断言 | 诊断价值 |
| --- | --- | --- |
| ① | 配了 `custom_headers` 的供应商 → **200** | 改前必 400（头根本没发出去） |
| ①b | 上游回显的 `x-opencode-session` == 配置值 | 证明头**到达**，不是「恰好上游不校验」 |
| ② | 未配置的供应商 → **400**，且不带该头 | 证明没有全局注入 / 不污染其它供应商 |
| ③ | 真多协议上游 + 自定义头 → **200** | 证明配置在真实传输路径上不破坏请求 |
| ④ | 配置里混入 `host`/`content-length`/`authorization`/`x-api-key` → 仍 200 | 防「校验一紧就拒合法配置」 |
| ④b | 上游收到的 `host`/`authorization`/`x-api-key`/`content-length` **未被改写**，且合法头 `x-ok` 生效 | 保留名与凭据头的剥离（安全边界） |
| ⑤×6 | 管理面拒绝六种非法形态，错误码分别为 `protected_name`/`not_object`/`invalid_value`/`duplicate_name`/`invalid_name`/`crlf` | 写入侧校验（Node 自定义码原文） |
| ⑥ | 管理面接受合法值 → **201**，且库里能读到归一化结果 | 写路径真的落库 |

## 反证（摘接线即红）

`go/internal/dataplane/upstream.go` 里那一行 `CustomHeaders: store.DecodeCustomHeaders(row.CustomHeaders)`
被摘掉后，用同一脚本复跑：**① 与 ④ 变 HTTP 400**（② 仍 400、③ 仍 200——后者正说明真上游并不校验该头，
测试的区分度来自回显上游）。逐字节还原后 `sha256sum -c` 一致、四条恢复绿。
