# go/internal/ingress — 入站请求体解压与内存准入

本包负责 `/v1` 数据面入站侧的三件事：**读体前的结构性拒绝**（`PeekSize`）、**流式解压与双向限额**
（`NewReader`）、**进程级内存与在途字节准入**（`Admission`）。它只提供能力，不决定调用顺序。

对应实现已随 Node 后端退役删除（原 src/app/v1/_lib/proxy/request-body-codec.ts，仅供考古）。

## 与已退役 Node 实现的两处有意差异

1. **解压在鉴权之后**。Node 侧在 `ProxySession.fromContext` 内、鉴权 guard 之前解压，会把未鉴权的
   输入展开到堆上。Go 侧只提供能力，
   调用顺序由守卫链决定；`PeekSize` 让调用方在**不读体**的前提下先做体积与层数拒绝。
2. **流式解压 + deflate 头嗅探**。Node 侧先把整个压缩体读进内存再逐层解压，deflate 用「先试 zlib、
   失败再试裸流」的回退。Go 侧全程流式：deflate 只看两字节头（zlib 头满足 CMF 低四位为 8 且
   `(CMF<<8|FLG) % 31 == 0`）即决定走 zlib 还是裸流，无需重试，也不需要整体驻留。

## 三条不变量

- **请求体永不落盘**。解压与缓存只发生在堆上；要「留一份」必须显式申请在途预算。
- **同一份字节不得同时以 `[]byte` 与 `string` 两种形式长期持有**。解析后立即释放原切片。
- **压缩体与解压体各有独立上限**，任一越界即拒绝，不得静默截断。

## 配置

| 变量 | 默认 | 语义 |
| --- | --- | --- |
| `MAX_DECOMPRESSED_REQUEST_BYTES` | 100 MiB | 单请求解压输出上限（防御解压炸弹），越界 413 |
| `MAX_COMPRESSED_REQUEST_BYTES` | 跟随解压上限 | 单请求压缩体上限（读体前的结构性天花板），越界 413 |
| `MAX_CONCURRENT_REQUEST_DECOMPRESSIONS` | 4 | 同时在途的解压任务数，饱和即拒绝（不排队） |
| `MAX_INFLIGHT_REQUEST_DECOMPRESSION_BYTES` | 128 MiB | 在途解压占用的压缩体字节总量 |
| `CCH_GO_MAX_INFLIGHT_BYTES` | 256 MiB | 进程级在途请求体（含解压后明文）总量 |

前四项与 Node 同名同默认，便于逐项对账（解析语义一致：非法值回退默认，不接受 `10MiB` 后缀）。
`CCH_GO_MAX_INFLIGHT_BYTES` 是 Go 侧新增的总量闸门，与 `GOMEMLIMIT` 叠加生效。
层数上限是常量（1 层）：真实客户端（含 Codex）只发单层编码，多层只是一次解压放大的攻击面。

## 错误语义

| 错误 | 状态码 | 触发条件 |
| --- | --- | --- |
| `ErrTooManyLayers` | 400 | `content-encoding` 层数超过上限 |
| `ErrCorruptBody` | 400 | 解压流损坏或截断 |
| `ErrCompressedTooLarge` | 413 | 压缩体超过上限（构造期或流式计数） |
| `ErrDecodedTooLarge` | 413 | 解压输出超过上限 |
| `ErrDecompressionBusy` | 503 | 在途解压预算饱和 |
| `ErrBodyBudgetExhausted` | 503 | 在途请求体预算饱和 |
| `ErrInsufficientMemory` | 503 | 堆在用逼近 `GOMEMLIMIT` 比例上限 |

一律 `errors.Is` 判定，`StatusOf` 取状态码，`IsCapacityError` 区分「服务端瞬时容量」与「请求本身非法」。
**拒绝都是立即失败，不排队**：排队会让峰值内存事后才出现，正是准入层要消灭的东西。

## 内存判定的边界

`MemoryGuard` 读两个数：`runtime/metrics` 的 `/memory/classes/heap/objects:bytes`（堆在用）与
`debug.SetMemoryLimit(-1)`（GOMEMLIMIT 的官方读取方式，负值入参不改变设置）。比例默认 0.9。
**未设 GOMEMLIMIT 时上限不可知，判定一律放行**——不知道就不假装知道，由 runtime 兜底；
此时只有两个在途字节预算在保护进程。

## 实测（i7-12700，`-benchtime 1s`）

```text
BenchmarkDecode1MiB/identity-16      133107     8874 ns/op   118158 MB/s   128 B/op    2 allocs/op
BenchmarkDecode1MiB/gzip-16            8299   137398 ns/op     7631 MB/s   78793 B/op  21 allocs/op
BenchmarkDecode1MiB/zstd-16            2029   573402 ns/op     1828 MB/s  9616794 B/op 32 allocs/op
BenchmarkDecode8MiB/identity-16        9938   127502 ns/op    65792 MB/s    129 B/op     2 allocs/op
BenchmarkDecode8MiB/gzip-16            1179  1006351 ns/op     8335 MB/s   78688 B/op  21 allocs/op
BenchmarkDecode8MiB/zstd-16             769  1511186 ns/op     5551 MB/s  9617038 B/op 32 allocs/op
BenchmarkPeekSize-16                8086269     145.1 ns/op                    64 B/op    4 allocs/op
BenchmarkLimiterAcquireRelease-16   9810421     123.4 ns/op                    32 B/op    1 allocs/op
```

两点读法：

- **`B/op` 与正文体积无关即流式证据**：8 倍正文下 gzip 仍是 78.7 KB/op（Node 侧整体缓冲会接近 2 倍正文）。
- **zstd 恒为 9.6 MB/op**：那是帧声明的 8 MiB 窗口分配，与正文体积无关。这是流式 zstd 的固有成本
  （解压需要窗口才能解析回溯引用），也正是 `MAX_CONCURRENT_REQUEST_DECOMPRESSIONS` 必须收紧的原因：
  4 × 8 MiB 窗口 ≈ 32 MB 常驻，外加各自解压输出。

## 已知未完成项

- 首字节前的调用顺序（解压放在哪个 guard 之后）由守卫链 lane 负责接线，本包不落顺序。
- `zstd` 窗口上限是常量 16 MiB，未做成可配置项：参考实现只声明 8 MiB，留 2 倍余量即可；
  若将来出现声明更大窗口的合法客户端，再把它提成环境变量。
