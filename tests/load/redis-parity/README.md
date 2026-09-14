# Redis Lua 与键空间一致性（语言中立产物）

本目录与仓库根的 `lua/` 是「Redis 原子语义」的**语言中立副本与黄金样本**。目的是让未来的 Go
数据面用**同一段 Lua 原文、同一组 KEYS/ARGV** 得到与 Node 侧逐字节相同的结果，而不是用 Go 多命令
重写后靠人工比对。

真源仍是 `src/lib/redis/lua-scripts.ts`；本目录只做搬运、校验与采样，**从不修改真源**。

## 交接断言的形状

Go 侧实现完成后必须满足：

```text
对每个 lua/<name>.lua 与 tests/load/redis-parity/golden/<name>.json 中的每个场景：
  EVAL(sha256 与 golden.sha256 相同的 Lua, keys.length, keys..., argv...)
    == golden.calls[].returned
  且调用后 keys 的终态 == golden.calls[].after
```

比较必须包含首尾空白（Lua 本体逐字节相同）与返回值类型（例如 `{"ok","updated","101","7"}` 与
`"101"` 不可互换）。

## 文件

| 路径 | 内容 |
| --- | --- |
| `lua/*.lua` | 18 段 Lua 原文，按常量名 kebab-case 命名 |
| `lua/MANIFEST.json` | 每段脚本的 `{constName, file, sha256, bytes, keysArityNote}` |
| `scripts/verify-lua-parity.ts` | 导出与逐字节校验（含 MANIFEST 双向一致性检查） |
| `scripts/capture-lua-golden.ts` | 黄金样本采集（真实 Redis 执行） |
| `tests/load/redis-parity/golden/*.json` | 每段脚本一份样本，含每个场景的入参、返回值与调用后键终态 |

## 校验与重采集

```bash
bun scripts/verify-lua-parity.ts           # 校验；不一致即非零退出
bun scripts/verify-lua-parity.ts --write   # 重新导出 lua/ 与 MANIFEST.json
bun scripts/capture-lua-golden.ts          # 重新采集 golden（需要可达 Redis）
REDIS_URL=redis://127.0.0.1:6379 CCH_GOLDEN_REDIS_DB=15 bun scripts/capture-lua-golden.ts
```

校验器覆盖的三个失败面：磁盘 `.lua` 与 TS 常量不一致、MANIFEST 与磁盘目录不成一一对应、
TS 侧新增脚本但未登记 KEYS/ARGV 契约（`ARITY_NOTES`）。

采集器只使用 DB 15（`CCH_GOLDEN_REDIS_DB` 可覆盖），只操作 `cchgolden:` 前缀的键，每个场景
执行前与整轮结束后都清理该前缀，跑完 DB 内残留键数为 0。脚本内固定时间戳
`NOW = 1757000000000`，保证样本可复现。

## 场景覆盖

| 脚本 | 场景数 | 覆盖点 |
| --- | ---: | --- |
| `batch-check-session-limits` | 2 | 全部未超限；首个供应商超限 |
| `cas-session-binding` | 3 | 正常更新；规范态缺失；generation 不符 |
| `check-and-track-key-user-session` | 3 | 限额内新建；Key 超限拒绝；User 超限拒绝 |
| `check-and-track-session` | 4 | 新建追踪；已追踪且已有引用；达到上限拒绝新建；非法 TTL 回落 |
| `clear-session-binding` | 2 | 带 cooldown 清除；规范态缺失 |
| `delete-legacy-provider-if-value` | 2 | 值匹配即删；值不符不动 |
| `force-terminate-key-user-session` | 1 | 三个索引同时移除 |
| `force-terminate-provider-session` | 1 | 引用与成员同时移除 |
| `get-cost-5h-rolling-window` | 1 | 窗口内求和 |
| `get-cost-daily-rolling-window` | 1 | 窗口内求和 |
| `read-or-reconcile-session-binding` | 4 | 全新创建；规范态已存在；legacy 升级带 provider；key 不匹配冲突 |
| `release-provider-session` | 3 | 最后一个引用移除成员；仍有引用只减计数；无引用空操作 |
| `release-session-discovery-lease` | 2 | 本 owner 释放；非 owner 不释放 |
| `renew-session-discovery-lease` | 2 | 本 owner 续期；非 owner 不续期 |
| `restore-legacy-provider-if-absent` | 2 | 键不存在才写；键已存在不动 |
| `terminate-session-binding` | 2 | 无条件终止；带期望 provider 的不匹配拒绝 |
| `touch-session-binding` | 2 | null 绑定续期；provider 不匹配拒绝 |
| `track-cost-rolling-window` | 3 | 带 requestId 追加；不带 requestId 追加；非法参数触发 `error_reply` |

合计 18 段 / 40 个场景。

## 未采集项（明确不编造）

| 项 | 原因 |
| --- | --- |
| `read-or-reconcile-session-binding` 的「规范态存在但 legacy owner 缺失」组合 | 该组合是滚动升级中的瞬时中间态，其出现顺序由 `session-binding.ts` 的调用序列决定；单线程直接调用脚本会把一个并非真实序列的中间态固化成基准 |
| 并发交错的原子性 | 单线程顺序调用无法观察；须由 Go 侧的并发集成测试（多客户端同时打同一 key）覆盖 |
| Redis Cluster 的 `CROSSSLOT` 行为 | 本机是单实例 Redis；`CHECK_AND_TRACK_KEY_USER_SESSION` 等脚本要求同 hash tag，该约束只能在集群模式验证 |
| TTL 自然到期后的键状态 | 依赖真实时间流逝；采集器只记录 `TTL` 返回值，不等待过期 |
| 非 `error_reply` 的 Redis 侧错误（如 OOM、只读副本） | 环境无法构造 |

## 与数据面接管计划的关系

`` §9 要求「18 段 Lua 逐段移植，不得用 Go 多命令
改写」。本目录提供该要求的两件配套证据：**原文副本**（`lua/`）与**行为基准**（`golden/`）。
接管期间若 Node 侧 Lua 发生变更，`bun scripts/verify-lua-parity.ts` 会立即失败，提示重新导出并重采集。
