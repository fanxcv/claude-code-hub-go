# 粘性改为会话级 —— 设计文档

## 1. 目标与非目标

### 目标

将供应商粘性的键从**前缀指纹**改为 **sessionID × keyID**，使同一会话的后续请求粘在首次选中的供应商上，而不再按正文前缀相似度粘。

具体改动：
- 会话绑定（`session.Binder`）成为选路的**第一优先级**，短路后续逻辑
- 前缀亲和保留为**兜底层**，只服务无 session id 的客户端（如 curl 测试、不支持会话的旧客户端）
- 新会话首个请求仍从 `effectivePriority` 最小的档开始选（不继承任何历史绑定）
- F3b 的 `theoretical_cache_tokens` 保留指纹链纯计算供数（不参与粘性决策）

### 非目标

- 不改熔断逻辑（会话绑定遇到熔断供应商时的失效由本设计处理，但熔断判定本身不动）
- 不改同档加权随机（选路第三层保持现状）
- 不做全局优先级重排（`group_priorities` / `providers.priority` 语义不变）
- 不改终态写回的整体时机（仍在 `terminal/settle.go` 的 `affinityWriteback` 位置，只是写回内容从前缀键改为会话绑定）

## 2. 两层粘性的优先级与关系

### 优先级（从高到低）

| 层级 | 触发条件 | 短路点 | 依据 |
| --- | --- | --- | --- |
| ① 会话绑定 | `sessionID != "" && sessionBinding.ProviderID != 0` | `route/select.go:~230`（在 `nominateByAffinity` 之前插入新分支） | `session/binding.go:35-41` 的 `BindingSnapshot` |
| ② 前缀亲和（兜底） | `sessionID == "" && fingerprintable` | `route/select.go:246-276`（现有 `nominateByAffinity` 短路位置） | `route/affinity.go:98-130` |
| ③ 加权随机 | 前两者均未命中 | `route/select.go:281-330`（现有同档加权随机） | `route/weighted.go` |

### 进入条件与短路逻辑

**会话绑定层（新增）**：
```go
// 在 resolve() 的 applyFilters 之后、nominateByAffinity 之前插入
if withAffinity && req.SessionBinding != nil && req.SessionBinding.ProviderID != 0 {
    // 从 filtered.healthy 中找 req.SessionBinding.ProviderID
    // 若该 provider 仍在 healthy 池（未熔断、未停用、支持模型、通过黑白名单）则短路返回
    // 否则进入失效处理（写冷却 or 清空绑定）后继续走后续层级
}
```

**前缀亲和层（改为兜底）**：
```go
// 现有 nominateByAffinity 调用处（select.go:238-276）改为
if withAffinity && req.SessionID == "" {
    // 只有无 session id 的请求才查前缀亲和
    nominate, lookup, writeback, affinityIdentity, nominated = s.nominateByAffinity(ctx, req, excluded)
    if nominated {
        // 短路返回（现有逻辑）
    }
}
```

加权随机层保持现状（`select.go:281` 起）。

### 为什么前缀只兜底

依据用户裁决 A："session id 为主，前缀只作无 id 客户端的兜底"。判定分支：
- `req.SessionID != ""`：走会话绑定层，**不再查前缀**
- `req.SessionID == ""`：跳过会话绑定，走前缀亲和兜底（curl / 旧客户端）

前缀代码不删净的原因：
1. F3b 仍需指纹链供数（裁决 C）
2. 测试场景（curl 单发）仍需前缀粘性
3. 降级路径（Redis 写绑定失败时可退回前缀）

## 3. 注入缝设计

### 问题

`route` 包不能 import `session` 包（会成环：`session` → `guard` → `route`，见 `guard/adapters_route.go:196` 注释）。会话身份必须以注入缝传入。

### 方案对比

| 方案 | 具体做法 | 优点 | 缺点 | 推荐 |
| --- | --- | --- | --- | --- |
| A. 在 `route.Request` 加字段 | 新增 `SessionBinding *SessionBindingSnapshot`（自定义类型，不引用 `session.BindingSnapshot`） | 保持选路在守卫链内的位置；对称于现有 `AffinityLookup` | 需定义镜像类型 `SessionBindingSnapshot`；两个类型需手工对齐 | **采用** |
| B. 选路移到守卫之后 | 把 `route.Selector.Select()` 从守卫步骤改为守卫链**之后**的独立阶段 | 可直接用 `session.BindingSnapshot` | 破坏守卫链语义（选路属守卫步骤 provider）；改动面大（`dataplane` 装配全改） | **不采用** |

### 推荐方案 A：在 `route.Request` 加字段

**新增字段**（`route/select.go:68` 的 `Request` 结构体）：
```go
// SessionBinding 是本次请求的会话绑定快照（若有）。
// 非 nil 且 ProviderID != 0 时优先于前缀亲和；nil 或 ProviderID == 0 表示无绑定。
SessionBinding *SessionBindingSnapshot
```

**镜像类型定义**（新建 `route/session_binding.go`）：
```go
package route

// SessionBindingSnapshot 是会话绑定的只读快照，供选路层判定会话粘性。
// 镜像 session.BindingSnapshot 的字段（route 不能 import session）。
type SessionBindingSnapshot struct {
    SessionID  string
    KeyID      int64
    Generation string
    ProviderID int64 // 0 表示空绑定
}
```

**填充点**（`guard/adapters_route.go:165`）：

**注意**：不存在名为 `buildRouteRequest()` 的函数。真实构造点是 `adapters_route.go:165` 的 `selectionRequest := route.Request{...}` **内联字面量**——在该字面量里直接加字段，不要去找一个不存在的函数。

```go
// 在 adapters_route.go:165 的 route.Request{...} 字面量内加字段（勿新造 buildRouteRequest）：
SessionBinding: sessionBindingSnapshotFrom(pc), // 返回 nil 表示无绑定
```

辅助函数建议就地定义在同文件（`adapters_route.go`），从 `pctx` 会话上下文取值：

```go
// SessionID 来源：pctx 的会话上下文，由 session 守卫填充（见 guard 链的 session 步）。
func sessionBindingSnapshotFrom(pc *pctx.Context) *route.SessionBindingSnapshot {
    sessionID, binding := pc.SessionBindingSnapshot() // 按 pctx 现有访问面取名，实施时核对
    if sessionID == "" || binding.ProviderID == 0 {
        return nil
    }
    return &route.SessionBindingSnapshot{
        SessionID:  sessionID,
        KeyID:      binding.KeyID,
        Generation: binding.Generation,
        ProviderID: binding.ProviderID,
    }
}
```

## 4. 绑定写入与失效

### 写入时机与方法

复用 `terminal/settle.go` 的 `affinityWriteback` 位置（`:391-413`），但写回内容改为**会话绑定的 CAS 更新**而非前缀键。

**写入调用**（伪代码，实际在 `terminal/settle.go` 或新建 `terminal/session_writeback.go`）：
```go
// 成功响应时
if result.SessionBinding != nil && result.SelectedProviderID != 0 {
    binder.CompareAndSet(
        ctx,
        result.SessionBinding.SessionID,
        result.SessionBinding.KeyID,
        result.SessionBinding.Generation, // CAS fence
        result.SelectedProviderID,
        ttlSeconds,
    )
}
```

### 写入值

| 字段 | 值 | 来源 |
| --- | --- | --- |
| `sessionID` | 本次请求的会话 ID | `route.Result.SessionBinding.SessionID` |
| `keyID` | API key ID | `route.Result.SessionBinding.KeyID` |
| `expectedGeneration` | 选路时读到的 generation | `route.Result.SessionBinding.Generation` |
| `providerID` | 选中的供应商 ID | `route.Result.Provider.ID` |
| `ttlSeconds` | 配置的 TTL | `PREFIX_AFFINITY_TTL_SECONDS`（复用） |

### Generation / CAS 语义

- 选路时调用 `session.Binder.ReadOrReconcile()`（幂等读取，不存在则创建，返回当前 generation）
- 终态写回时调用 `CompareAndSet(expectedGeneration, newProviderID)`
- 若 CAS 失败（`ConflictReason == "generation_mismatch"`），说明**其他请求已更新过绑定**，本次写回静默放弃（不覆盖更新的选择）
- 首次绑定（`source=created`）时 generation 由 `GenerateSessionID()` 随机生成（`binding.go:179`）

### 失效规则

| 触发条件 | 动作 | 依据 |
| --- | --- | --- |
| provider_error（上游 5xx / 超时） | **写冷却**：`session.Binder.Clear(expectedGeneration, cooldownProviderID)` | `binding.go:251-273`，`ProviderCooldownKey(sessionID, keyID, providerID)` TTL 60s |
| resource_not_found（模型不支持） | 清空绑定（不写冷却）：`Clear(expectedGeneration, 0)` | 模型不支持不是故障，不该冷却 |
| 熔断（半开/开启） | **跳过该 provider**，继续走后续层级（不写冷却、不清空绑定） | 熔断是暂时的，绑定保留；待熔断恢复后会话仍粘回去 |
| 供应商停用（`is_enabled=false`） | 清空绑定（不写冷却） | 停用是配置决策，不是故障 |
| 不支持模型（格式/端点不兼容） | 清空绑定（不写冷却） | 配置变更导致，不是故障 |

**写冷却的位置**：在 `dataplane/affinity.go` 的 `tombstoneDirective` 附近新增 `sessionBindingCooldownDirective`，供 `terminal/settle.go` 调用。

## 5. 开关与配置

### `affinityIgnoreClientSessionId` 反转

**当前状态**：`system_settings.affinity_ignore_client_session_id` 默认 `true`，表示"忽略客户端会话 ID，强制用前缀"。

**反转后**：默认改为 `false`，语义改为"忽略客户端会话 ID"（false = 不忽略 = 用会话粘性）。

| 层面 | 文件路径 | 行号 | 改动 |
| --- | --- | --- | --- |
| DB 默认值 | `drizzle/0112_complex_sabra.sql` | 56 | `DEFAULT true` → `DEFAULT false` **（需新迁移）** |
| Go 读处 | `store/read.go` | 63-64 | 逻辑反转：`ignoreSession := settings.AffinityIgnoreClientSessionId`; 使用处改为 `!ignoreSession` |
| 管理面白名单 | `adminapi/system_settings.go` | 1335 | `AffinityIgnoreClientSessionId: true` → `false` |
| 前端表单 | `src/components/settings/config/_components/system-settings-form.tsx` | 1249-1259 | 复选框 label 改为"忽略客户端会话 ID（强制前缀粘性）" |
| zh 词条 | `messages/zh-CN/settings/config.json` | 109-110 | `"affinityIgnoreClientSessionId"` / `"affinityIgnoreClientSessionIdDescription"` 文案改为"勾选后忽略会话 ID，强制使用前缀指纹粘性（兼容旧客户端）" |
| en 词条 | `messages/en/settings/config.json` | 168-169 | 同上英文版 |

**新迁移**（`drizzle/0XXX_session_sticky.sql`）：
```sql
-- 反转 affinity_ignore_client_session_id 默认值
ALTER TABLE system_settings
  ALTER COLUMN affinity_ignore_client_session_id SET DEFAULT false;

-- 对既有行不做修改（保持用户当前配置）
```

### 三个 ENV 的处置

| ENV | 当前用途 | 改后处置 | 依据 |
| --- | --- | --- | --- |
| `ENABLE_PREFIX_AFFINITY` | 总开关（默认 false） | **保留**，改名为 `ENABLE_STICKY`；前缀 + 会话共用 | `config/env.go:93` |
| `PREFIX_AFFINITY_TTL_SECONDS` | 前缀键 TTL | **保留**，改名为 `STICKY_TTL_SECONDS`；会话绑定复用 | `config/env.go:121` |
| `PREFIX_AFFINITY_WINDOW` | 指纹窗口（消息数） | **保留**，只为 F3b 供数（裁决 C） | `config/env.go:122` |

**改名步骤**：
1. `config/env.go` 新增 `ENABLE_STICKY` / `STICKY_TTL_SECONDS`，保留 `PREFIX_AFFINITY_*` 作兼容别名（优先读新名，回退旧名）
2. `go/env-parity.txt` 同步（由 `cmd/envlist` 重新生成）
3. `.env.example` 加新行、旧行标注 deprecated
4. 文档更新（`docs/*.md` 提及 PREFIX_AFFINITY 的地方）

## 6. F3b 与观测列

### 指纹链保留供 F3b

**裁决 C 要求**："保留指纹链纯计算（`Fingerprint()` 是纯本地函数），只为 F3b 供数；粘性不再用它。"

**实现**：
- `route/affinity.go:98-130` 的 `Fingerprint()` **保留不动**（纯本地计算，不碰 Redis）
- `terminal/cachescore.go:71-113` 的 `ComputeCacheScoreFields()` **保留不动**（仍调用 `Fingerprint` 并填充五列）
- `dataplane/cachescore.go:79-118` 的 `cacheScoreFacts` 接口从 `route.AffinityWriteback.CacheScoreFacts()` 取值 —— 本次改造后，`AffinityWriteback` 为 nil（因为不再写前缀键），需**新增取值路径**：
  ```go
  // 在 dataplane 装配时，若 result.SessionBinding != nil 则仍调用 Fingerprint() 纯计算
  if result.SessionBinding != nil && req.AffinityBody != nil {
      fp := route.Fingerprint(req.AffinityBody, req.Format, window)
      facts = route.CacheScoreFacts{
          ScopeTag: route.ScopeTag(req.KeyID, req.Format, req.Model),
          MatchedFP: "", // 会话粘性无"命中的指纹"
          TipFP: fp.Tip().Fingerprint,
          TipPrefixBytes: fp.Tip().PrefixBytes,
          HasTip: fp.Tip().Fingerprint != "",
      }
  }
  ```

### 五列语义改后

| 列名 | 当前语义 | 改后语义 | 是否需改 |
| --- | --- | --- | --- |
| `theoretical_cache_tokens` | 指纹链 tip 的 `PrefixBytes / 4` | **不变**（仍由指纹链供数） | 否 |
| `cache_compatibility_key` | `scopeTag + ":" + matched fingerprint`（`cachescore.go:79`），是**管理面缓存效果聚合的分组键**（`store/admin_cache_effectiveness_job.go:105` 用 `IS NOT NULL` 过滤后按它聚合命中率） | **不改**。会话粘性**另立键**，不借用此列 | **否** |
| `cache_score_eligible` | 是否参与缓存评分 | **不变**（仍由 F3b 参数决定） | 否 |
| `cache_score_excluded_reason` | 为何不参与评分 | **不变**。**不要新增** `"session_sticky"` 值——该列的取值集被下游 switch 消费（`cachescore.go:97-110`），加值需同步全部消费方 | 否 |
| `cache_ttl_bucket` | 缓存 TTL 档位 | **不变** | 否 |

### 为何 `cache_compatibility_key` 不能改（关键决策，勿动）

初稿曾主张把它改成 `sessionID + keyID + providerID`。**该主张已撤销**，三条理由：

1. **它是缓存效果报表的分组键**。`store/admin_cache_effectiveness_job.go:105` 以 `cache_compatibility_key IS NOT NULL` 为过滤条件，按该键聚合「同前缀请求的实际缓存命中率」。改成会话级后，**同一前缀的请求不再同键**，聚合退化为「几乎全是一会话一键」，报表直接失效。
2. **它的语义是「可共享缓存的前缀标识」，与会话唯一性正交**。会话级键的本质是「一个会话一个值」，与「哪些请求共享同一前缀」是相反方向的需求。
3. **写入条件不同**。该列只在流式终态且缓存效果开关开启时写（`terminal/patch.go:42-49`），非流式与关闭时保持 NULL；而会话粘性需要**所有请求**都能定位绑定，不能依赖这个条件。

**会话粘性所需的键**：直接用 `session.BuildBindingKeys(sessionID, keyID).Canonical`（`session/keys.go:32-38`），即 Redis 键 `session-binding:v1:{<sha256 tag>}:binding`。它是 Redis 键，**不落 `message_request` 任何列**（会话绑定全走 Redis）。

若将来确实需要在 `message_request` 上追溯「本次粘到了哪条会话」，正确做法是**新增列**（如 `session_binding_key`），而不是复用 `cache_compatibility_key`。该需求是否进 MVP，见 §5 开关部分——**默认为否**（现有 `session_identity_kind` 审计条目已给出会话身份线索）。

### 长度约束的真实情况（原「61 字符」推算已废）

初稿写「会话粘性键 ≈ 61 字符」，其推算基于错误假设。实际：

| 项 | 真实值 | 出处 |
| --- | --- | --- |
| `sessionID` 形制 | `sess_` + base36 毫秒时间戳 + `_` + 12 位 hex，**约 24 字符** | `session/identity.go:181-187` |
| `bindingHashTag` 输出 | sha256 全量 hex，**固定 64 字符** | `session/keys.go:17-20` |
| Canonical 键全长 | `session-binding:v1:{` + 64 + `}:binding` ≈ **89 字符** | 推算 |
| `cache_compatibility_key` 列定义 | `varchar(64)` | `drizzle/0112_complex_sabra.sql:50` |

关键结论：**Canonical 键（89 字符）装不进 `cache_compatibility_key`（64）**——这从长度上二次证明两者不能复用。而 Canonical 是 **Redis 键**，Redis 无长度限制，89 字符无任何问题。

原稿建议的「在 `terminal/patch.go` 写入前加长度断言」**已删除**：该列值形态不变（`scopeTag:fingerprint`），无需新断言。

### `estimateNote` 文案

**当前**：`messages/*/dashboard.json:272`，"基于指纹链 tip 的字节数 / 4"。

**改后**：会话粘性下指纹链仍计算，F3b 列仍有值，文案**无需改**（仍是"理论估算"，只是粘性机制不同）。

## 7. 迁移与回滚

### 需要的 migration

**一条新迁移**（`drizzle/0XXX_session_sticky.sql`）：
```sql
-- 1. 反转 affinity_ignore_client_session_id 默认值
ALTER TABLE system_settings
  ALTER COLUMN affinity_ignore_client_session_id SET DEFAULT false;

-- 2. cache_compatibility_key 无需改列定义（varchar(64) 够用）

-- 3. 无需新增列（会话绑定走 Redis，不落 message_request）
```

### trigger.sql 与 terminal/columns.go 同步

**无需改**：
- `src/lib/ledger-backfill/trigger.sql:276` 监视的三列（`affinity_scope_tag` / `affinity_fingerprint` / `affinity_fingerprint_chain`）**不删除**（F3b 仍需它们）
- `terminal/columns.go:8-46` 的 37 列清单保持不变
- `patch_test.go:136` 的钉数 37 不变

但需注意：生产实测这三列**从未写入**（0 行非空，见任务描述），改造后也不会写（会话粘性不写前缀键）。若 F3b 依赖这三列的**历史值**，需确认 F3b 的降级路径。

### 旧前缀键与在途生成

**旧前缀键**（`cch:pfx:{scopeTag}:fp:{fingerprint}`）：
- Redis 中的存量键**自然过期**（TTL 3600s，最多 1 小时后全部消失）
- 不做主动清理（避免误删其他环境的键）
- 监控：部署后观察前缀键的 TTL 分布，1 小时后应降至 0

**在途生成**（改造期间仍有前缀键写入）：
- 账本触发器依赖 `affinity_scope_tag` 等三列（`trigger.sql:202-203,214-215,238-240,292-294`）
- 改造前这三列已是**全 NULL**（0 行非空），触发器的 `NEW.affinity_scope_tag IS NOT NULL` 分支**从未执行过**
- 改造后仍全 NULL，触发器逻辑**零影响**（分支仍不执行）

### 回滚路径

**代码回滚**（Git revert）：
1. 回退 `route/select.go` 的会话绑定层
2. 回退 `terminal` 的写回改动
3. 回退 `guard/adapters_route.go` 的 `SessionBinding` 填充
4. 前缀亲和层自动恢复（代码未删）

**配置回滚**：
- 管理面设置 `affinityIgnoreClientSessionId = true`（强制前缀粘性）
- 或环境变量 `ENABLE_STICKY=false`（关闭粘性）

**数据回滚**：
- 会话绑定在 Redis（无需清理，TTL 自然过期）
- `system_settings.affinity_ignore_client_session_id` 的默认值已改（需手工 `UPDATE` 改回 `true`，或下一次迁移反向操作）

## 8. 实施切分与风险

### 拆分块与并行

| 块 | 改动文件 | 量级 | 可并行 |
| --- | --- | --- | --- |
| ① 注入缝与选路层 | `route/select.go` (+50)、`route/session_binding.go`（新增 +20）、`guard/adapters_route.go` (+30) | ~100 行 | 否（主干） |
| ② 终态写回 | `terminal/settle.go` (+40) 或新建 `terminal/session_writeback.go` (+80)、`dataplane/affinity.go` (+30) | ~110 行 | 可与 ④ 并行 |
| ③ 配置与开关 | `config/env.go` (+20)、`store/read.go` (+10)、`adminapi/system_settings.go` (+5) | ~35 行 | 可与 ① ② 并行 |
| ④ 前端与 i18n | `system-settings-form.tsx` (+10)、`messages/zh-CN/*.json` (+4)、`messages/en/*.json` (+4) | ~18 行 | 可与 ② 并行 |
| ⑤ F3b 供数 | `dataplane/cachescore.go` (+20)、`terminal/cachescore.go`（若需改 +10） | ~30 行 | 可与 ② 并行 |
| ⑥ 迁移与文档 | `drizzle/0XXX_session_sticky.sql` (+10)、`docs/design-session-sticky.md`（本文档）、`go/env-parity.txt`（重新生成） | ~10 行 SQL | 最后 |

**总量级**：约 **300 行代码改动 + 1 条迁移 + 本设计文档**。

**并行策略**：
- 主干（① ② ⑤）串行（互相依赖）
- 配置侧（③ ④）可单独一路并行
- 迁移（⑥）等代码全部就绪后最后合并

### 三个最高风险

| 风险 | 描述 | 缓解 |
| --- | --- | --- |
| **CAS 竞态** | 同一会话的并发请求（用户快速连发）可能导致 generation_mismatch，后到的请求写回失败 | ① CAS 失败静默放弃（不报错）；② 下次请求会读到更新后的绑定，自动修正；③ 监控 `ConflictReason=generation_mismatch` 的频率 |
| **熔断期绑定失效** | 会话绑定的 provider 进入熔断状态时，若不清空绑定，会话会一直尝试该 provider 直到熔断恢复 | ① **不清空绑定**（设计决策：熔断是暂时的，保留绑定待恢复）；② 在选路层**跳过熔断 provider**，继续走后续层级；③ 若用户不接受此行为，可在失效规则里加"熔断时清空绑定" |
| **F3b 列空值** | 改造后 `affinity_scope_tag` / `affinity_fingerprint` / `affinity_fingerprint_chain` 仍全 NULL（会话粘性不写前缀键），若 F3b 依赖这些列的**非空值**则失效 | ① 确认 F3b 的**当前状态**（生产已是全 NULL，F3b 是否已降级？）；② 若 F3b 需要，在 `terminal/patch.go` 里**仍写这三列**（纯供 F3b，不参与粘性）；③ 或 F3b 改为从 `cache_compatibility_key` 取会话粘性键 |

## 9. 验证方案

### 门禁（代码正确性）

| 检查项 | 命令 | 通过判据 |
| --- | --- | --- |
| Go 格式 | `cd go && gofmt -l .` | 输出为空 |
| Go 静态检查 | `cd go && go vet ./...` | exit 0 |
| Go 测试 | `cd go && go test -timeout 600s ./...` | exit 0，全包 ok |
| 前端类型 | `bun run typecheck` | exit 0 |
| 前端静态检查 | `bun run lint` | exit 0 |
| 前端测试 | `bun run test` | exit 0 |

**新增测试**（必须写）：
- `route/select_test.go`：会话绑定优先级、前缀兜底分支、熔断时跳过绑定 provider
- `terminal/session_writeback_test.go`（若新建）：CAS 写回、失效规则、冷却写入
- `guard/adapters_route_test.go`：`SessionBinding` 注入缝填充

### 生产侧可观测判据

**部署后 1 小时内**：
1. **会话绑定生效**：
   - `message_request.session_identity_kind = 'client_session'` 的请求，同一 `session_id` 的后续请求 `provider_id` 保持不变（除非故障转移）
   - 观测 Redis：`KEYS session-binding:v1:*:binding`，应有新增键；`TTL` 约 3600s
   - 观测 Redis：`KEYS cch:pfx:*`，存量键在 1 小时内全部消失（TTL 递减至 0）

2. **前缀兜底生效**：
   - curl 单发（无 session id）的请求，仍走前缀亲和（`session_identity_kind = 'prefix_affinity'`）
   - 观测：`provider_chain[].selectionMethod = 'prefix_affinity'`

3. **失效规则生效**：
   - 故意触发 provider_error（停用某渠道后发请求），观测该会话的下一个请求**切换到其他 provider**
   - 观测 Redis：`KEYS cch:sess:cooldown:*`，应有冷却键（TTL 60s）

4. **F3b 列仍有值**：
   - 查询 `SELECT theoretical_cache_tokens, cache_compatibility_key FROM message_request WHERE created_at > now() - interval '10 minutes' LIMIT 10`
   - `theoretical_cache_tokens` 非 NULL（指纹链仍计算）
   - `cache_compatibility_key` 形如 `sess_xxx + key_xxx + prov_xxx`

**告警不增**：
- 部署前后对比日志 `level=warn` 的事件种类与频率
- 新增 `session.binding.*` 事件属预期（绑定层的正常 warn）
- 不应出现 `route.selection.failed` / `terminal.writeback.failed` 激增

### 回归面

**不得破坏的现有行为**：
- 熔断逻辑（供应商进入熔断/半开/恢复的判定与留痕）
- 同档加权随机（无会话绑定、无前缀命中时的选路分布）
- 故障转移（`ExcludeIDs` 排除、多次切换）
- F3b 五列的计算与落库
- 账本触发器（`usage_ledger` upsert 逻辑）

**回归测试类型**：
- 单元测试：`go test ./internal/route/` / `./internal/session/` / `./internal/terminal/`
- 集成测试（若有真库）：`CCH_TEST_DSN=... go test ./...`
- 负载测试（可选）：复跑历史 probe，观察选路分布与响应时间

---

## 附：与现状的差异表

| 维度 | 现状 | 改后 | 依据 |
| --- | --- | --- | --- |
| **粘性键** | `cch:pfx:{scopeTag}:fp:{fingerprint}` | `session-binding:v1:{<sha256(sessionID,keyID)>}:binding`（即 `session.BuildBindingKeys().Canonical`，见 `session/keys.go:32-38`） | `session/keys.go:17-38` |
| **优先级** | 前缀亲和 > 加权随机 | 会话绑定 > 前缀亲和（兜底）> 加权随机 | 本设计 §2 |
| **进入条件** | `ENABLE_PREFIX_AFFINITY=true && fingerprintable` | 会话层：`sessionID != ""`；前缀层：`sessionID == ""` | 本设计 §2 |
| **短路位置** | `route/select.go:246-276` | 会话层：`~230`（新增）；前缀层：`246-276`（改判定） | 本设计 §2 |
| **注入缝** | `route.Request.AffinityLookup` | 新增 `route.Request.SessionBinding` | 本设计 §3 |
| **写回方法** | `AffinityStore.RecordWinner()` | `session.Binder.CompareAndSet()` | 本设计 §4 |
| **失效规则** | provider_error / resource_not_found 写墓碑（`cch:pfx:*:tomb`） | provider_error 写冷却（`cch:sess:cooldown:*`）；resource_not_found 清空绑定 | 本设计 §4 |
| **开关默认** | `affinity_ignore_client_session_id = true` | 改为 `false`（不忽略会话 ID） | 本设计 §5 |
| **ENV 命名** | `ENABLE_PREFIX_AFFINITY` / `PREFIX_AFFINITY_TTL_SECONDS` | 改名 `ENABLE_STICKY` / `STICKY_TTL_SECONDS` | 本设计 §5 |
| **F3b 指纹链** | 参与粘性决策 + 供 F3b | 只供 F3b，不参与粘性 | 裁决 C，本设计 §6 |
| **cache_compatibility_key** | `scopeTag + ":" + matched fingerprint` | **不变**（它是缓存效果报表的分组键，不可挪用；会话粘性全走 Redis 键） | `cachescore.go:79`、`store/admin_cache_effectiveness_job.go:105` |
| **三列写入** | `affinity_scope_tag` / `affinity_fingerprint` / `affinity_fingerprint_chain` 应写入但生产全 NULL | 仍全 NULL（会话粘性不写前缀键） | 任务描述 + 本设计 §7 |
| **迁移** | 无（现有列已齐） | 1 条（反转 `affinity_ignore_client_session_id` 默认值） | 本设计 §7 |
| **回滚** | 无（现状即回滚态） | 代码 revert + 配置改回 + Redis 键自然过期 | 本设计 §7 |

---

## 核心方案摘要（≤30 行）


**① 两层粘性的优先级与短路点**
- 会话绑定（新增）：`sessionID != "" && ProviderID != 0` → 短路点 `route/select.go:~230`（在 `nominateByAffinity` 之前）
- 前缀亲和（兜底）：`sessionID == ""` → 短路点 `select.go:246-276`（现有位置，改为只在无 session id 时进入）
- 加权随机：前两者均未命中 → `select.go:281-330`（保持现状）

**② 注入缝的最终选型**
- 方案 A：在 `route.Request` 加 `SessionBinding *SessionBindingSnapshot` 字段
- 镜像类型：新建 `route/session_binding.go`，定义 `SessionBindingSnapshot{SessionID, KeyID, Generation, ProviderID}`（不引用 `session.BindingSnapshot`）
- 填充点：`guard/adapters_route.go:165` 的 `selectionRequest := route.Request{...}` 内联字面量（**不存在 `buildRouteRequest()` 函数**），从 `pctx` 会话上下文取值并填充

**③ 写回时机与失效规则**
- 写回时机：复用 `terminal/settle.go:391-413` 的 `affinityWriteback` 位置，调用 `session.Binder.CompareAndSet(sessionID, keyID, expectedGeneration, providerID, ttl)`
- CAS fence：用选路时读到的 `generation` 做 CAS；失败（`generation_mismatch`）时静默放弃
- 失效规则：
  - provider_error（5xx/超时）→ 写冷却 `ProviderCooldownKey(sessionID, keyID, providerID)` TTL 60s
  - resource_not_found → 清空绑定（不写冷却）
  - 熔断 → **跳过该 provider**，继续后续层级（不清空绑定，待熔断恢复）
  - 停用/不支持模型 → 清空绑定

**④ 需要改的迁移/触发器清单**
- 迁移：1 条（`drizzle/0XXX_session_sticky.sql`）—— 反转 `system_settings.affinity_ignore_client_session_id` 默认值 `true` → `false`
- 触发器：**无需改**（`trigger.sql` 监视的三列 `affinity_scope_tag` 等保留供 F3b，但会话粘性不写它们；生产已全 NULL，改后仍全 NULL）
- `terminal/columns.go` 的 37 列清单：**无需改**

**⑤ 实施切分与文件数量级**
- 6 块：① 注入缝与选路（3 文件 ~100 行）、② 终态写回（2 文件 ~110 行）、③ 配置（3 文件 ~35 行）、④ 前端与 i18n（3 文件 ~18 行）、⑤ F3b 供数（2 文件 ~30 行）、⑥ 迁移与文档（3 文件 ~10 行 SQL）
- 总量级：约 **300 行代码改动 + 1 条迁移 + 设计文档**
- 并行：主干（① ② ⑤）串行；配置侧（③ ④）可并行；迁移（⑥）最后

**⑥ 三个最高风险**
1. **CAS 竞态**：并发请求 generation_mismatch → 缓解：静默放弃 + 下次自动修正 + 监控频率
2. **熔断期绑定失效**：会话绑定遇熔断时的行为 → 缓解：**不清空绑定**（保留待恢复）+ 选路层跳过熔断 provider
3. **F3b 列空值**：三列（`affinity_scope_tag` 等）改后仍全 NULL → 缓解：确认 F3b 当前状态（生产已全 NULL）+ 若需要则仍写这三列（纯供 F3b）

---

**补充说明**：
- 前缀代码不删净（F3b 仍需指纹链、测试场景需前缀兜底、降级路径保留）
- 新会话首个请求仍从 `effectivePriority` 最小的档开始选（裁决 D，现状不变）
- `cache_compatibility_key` **不改**（它是缓存效果报表分组键，`store/admin_cache_effectiveness_job.go:105`；会话粘性全走 Redis 键 `session-binding:v1:*:binding`）
- ENV 改名：`ENABLE_PREFIX_AFFINITY` → `ENABLE_STICKY`，`PREFIX_AFFINITY_TTL_SECONDS` → `STICKY_TTL_SECONDS`
- `estimateNote` 文案无需改（指纹链仍计算，F3b 列仍有值）

