package session

import (
	"context"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// 会话终止：复刻 SessionManager.terminateSession（src/lib/session-manager.ts:3442-3730）。
//
// 为什么不能只调 Binder.Terminate：那条只覆盖「版本化绑定 CAS」一段。Node 的终止是
// 一次性动作共五段——① owner/key 预检、② 版本化绑定 CAS（或 legacy 回退）、③ 响应体
// bundle 的索引与正文清除、④ 会话元数据与 messages 删除、⑤ provider/key/user/global 四组
// 活跃索引摘除。少任何一段都会留下「列表里已终止、实际仍在跑/仍可读」的悬空状态。
//
// 与 Node 的两处**登记差异**（都不影响正常路径）：
//   - Node 的 `readOrReconcileSessionBinding` 返回 `unavailable` 时走 legacy 回退
//     （mutateLegacySessionBindingSafely）。Go 侧脚本经 EVALSHA+NOSCRIPT 回退，拿不到
//     「脚本缺失」这一态；本实现把 EvalConst 失败一律当**终止失败**（返回 false），
//     不做 legacy 回退——与 Node「回退后仍须 CAS 成功才算终止」的净效果同判，只是失败得更早。
//   - Node 用随机 UUID 作新 generation；此处复用 GenerateSessionID（同为 UUID 形制）。
//
// 响应体 bundle 的删除脚本是 session-manager.ts 的**内联**常量（不在 lua/ 清单里，
// 也不参与 verify-lua-parity 的逐字节校验），因此这里原样内联同一段 Lua 走 EVAL：
// 保持单命令原子性，而不是用 Go 多命令改写。

// deleteSessionResponseBodyBundlesLua 逐字复制 session-manager.ts:161 的
// DELETE_SESSION_RESPONSE_BODY_BUNDLES_LUA。
const deleteSessionResponseBodyBundlesLua = `-- cch:session-response-bundle:delete-session:v1
local bundle_keys = redis.call("ZRANGE", KEYS[1], 0, -1)
local deleted = redis.call("DEL", KEYS[1])
redis.call("SETEX", KEYS[2], ARGV[1], ARGV[2])
deleted = deleted + redis.call("DEL", KEYS[3], KEYS[4], KEYS[5])

local bundle_suffix = "response-bodies:v1"
for _, bundle_key in ipairs(bundle_keys) do
  if string.sub(bundle_key, -string.len(bundle_suffix)) == bundle_suffix then
    local request_prefix = string.sub(bundle_key, 1, string.len(bundle_key) - string.len(bundle_suffix))
    deleted = deleted + redis.call(
      "DEL",
      bundle_key,
      request_prefix .. "response",
      request_prefix .. "snapshot:response:before:body",
      request_prefix .. "snapshot:response:after:body",
      request_prefix .. "response-body-generation:v1"
    )
  else
    deleted = deleted + redis.call("DEL", bundle_key)
  end
end
return deleted
`

// ResponseBodyBundleIndexKey 是响应体 bundle 索引 ZSET。
func ResponseBodyBundleIndexKey(sessionID string) string {
	return "session:" + sessionID + ":response-body-bundles:v1"
}

// ResponseBodyBundleKey 是响应体 bundle 键（session:{id}:req:{seq}:response-bodies:v1）。
func ResponseBodyBundleKey(sessionID string, sequence int) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(sequence) + ":response-bodies:v1"
}

// LegacyResponseBodyKey 是旧版响应体单键（session:{id}:response）。
func LegacyResponseBodyKey(sessionID string) string {
	return "session:" + sessionID + ":response"
}

// LegacyResponseBodyRequestKey 是旧版按序号的响应体键（session:{id}:req:{seq}:response）。
func LegacyResponseBodyRequestKey(sessionID string, sequence int) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(sequence) + ":response"
}

// ResponseSnapshotBodyKey 是响应快照正文键
// （session:{id}:req:{seq}:snapshot:response:{before|after}:body）。
func ResponseSnapshotBodyKey(sessionID string, sequence int, phase string) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(sequence) +
		":snapshot:response:" + phase + ":body"
}

// ConcurrentCountKey 是旧版会话并发计数键（session:{id}:concurrent_count）。
func ConcurrentCountKey(sessionID string) string {
	return "session:" + sessionID + ":concurrent_count"
}

// MessagesKey 是旧版会话消息键（session:{id}:messages）。
func MessagesKey(sessionID string) string {
	return "session:" + sessionID + ":messages"
}

// ProviderActiveSessionsKey 是 provider 维度活跃会话 ZSET。
func ProviderActiveSessionsKey(providerID int64) string {
	return "provider:" + strconv.FormatInt(providerID, 10) + ":active_sessions"
}

// ProviderActiveSessionRefsKey 是 provider 维度活跃会话引用 HASH。
func ProviderActiveSessionRefsKey(providerID int64) string {
	return "provider:" + strconv.FormatInt(providerID, 10) + ":active_session_refs"
}

// terminationTTLSeconds 是终止路径续写键的 TTL（Node 的 SessionManager.SESSION_TTL）。
const terminationTTLSeconds = DefaultBindingTTLSeconds

// terminateSessionsChunkSize 是批量终止的分块大小（Node 的 CHUNK_SIZE = 20）。
const terminateSessionsChunkSize = 20

// TerminateSession 复刻 SessionManager.terminateSession。
//
// expectedProviderIDs 非空时是**按供应商范围**的终止：只有当前绑定正好落在该集合里才动，
// 且只清该供应商自己的索引（CAS 之上的其他索引留给可能已经发生的 failover 重新绑定）。
// expectedKeyID 非 nil 时先校验 key owner 未变，变了直接放弃（返回 false，不删任何东西）。
//
// 返回值与 Node 一致：`bindingTerminated || deletedKeys > 0`。
func (b *Binder) TerminateSession(
	ctx context.Context,
	sessionID string,
	expectedProviderIDs []int64,
	expectedKeyID *int64,
) (bool, error) {
	if !b.Ready() {
		return false, ErrNilClient
	}
	if sessionID == "" {
		return false, nil
	}

	// 1. 先查绑定信息（用于从 ZSET 中移除）。
	raw := b.client.rc.Raw()

	lookupPipe := raw.Pipeline()
	providerCmd := lookupPipe.Get(ctx, LegacyProviderKey(sessionID))
	keyCmd := lookupPipe.Get(ctx, LegacyOwnerKey(sessionID))
	userCmd := lookupPipe.HGet(ctx, InfoKey(sessionID), "userId")
	_, lookupErr := lookupPipe.Exec(ctx)

	providerID := positiveIntOrZero(providerCmd)
	keyID := positiveIntOrZero(keyCmd)
	userID := positiveIntOrZero(userCmd)

	if lookupErr != nil && expectedKeyID != nil {
		// Node：预检失败且调用方给了期望 key 时保守放弃。
		return false, nil
	}
	if expectedKeyID != nil && keyID != *expectedKeyID {
		// Node：终止前 owner 已变，放弃。
		return false, nil
	}

	bindingTerminated := false
	if keyID != 0 {
		binding, err := b.ReadOrReconcile(ctx, sessionID, keyID, terminationTTLSeconds)
		switch {
		case err == nil && binding.OK:
			if binding.Snapshot.ProviderID > 0 {
				providerID = binding.Snapshot.ProviderID
			}
			if len(expectedProviderIDs) > 0 && !containsProvider(expectedProviderIDs, binding.Snapshot.ProviderID) {
				return false, nil
			}

			// 版本化 CAS 是线性化点；带范围时只对 CAS 时的 provider 做期望校验。
			expectedProviderID := int64(0)
			if len(expectedProviderIDs) > 0 {
				expectedProviderID = binding.Snapshot.ProviderID
			}
			terminated, err := b.Terminate(ctx, sessionID, keyID, expectedProviderID, terminationTTLSeconds)
			if err != nil {
				return false, err
			}
			if !terminated.OK {
				return false, nil
			}

			if len(expectedProviderIDs) > 0 {
				terminatedProviderID := binding.Snapshot.ProviderID
				if terminatedProviderID <= 0 {
					return false, nil
				}
				// 只摘该 provider 自己的索引：failover 可能已把会话改绑到别家。
				b.clearProviderIndex(ctx, terminatedProviderID, sessionID)
				return true, nil
			}
			bindingTerminated = true

		case err != nil:
			// Node 的 unavailable：脚本层不可用，本实现不做 legacy 回退（见文件头登记差异）。
			return false, err

		default:
			// 绑定冲突（canonical 缺失/腐坏/镜面冲突等）：与 Node 一样放弃，不删任何东西。
			return false, nil
		}
	} else if providerID != 0 || len(expectedProviderIDs) > 0 {
		// 没有 key owner：无法做绑定感知的终止，保守放弃。
		return false, nil
	}

	if keyID != 0 && !bindingTerminated {
		return false, nil
	}

	// 2~4. 删响应体 bundle、元数据、messages，并摘除四组活跃索引。
	cleanup := raw.Pipeline()
	cleanup.Eval(ctx, deleteSessionResponseBodyBundlesLua, []string{
		ResponseBodyBundleIndexKey(sessionID),
		ResponseBodyGenerationKey(sessionID),
		LegacyResponseBodyKey(sessionID),
		LegacyResponseBodyRequestKey(sessionID, 1),
		ResponseSnapshotBodyKey(sessionID, 1, "before"),
	}, strconv.Itoa(terminationTTLSeconds), GenerateSessionID())
	cleanup.Del(ctx, InfoKey(sessionID))
	cleanup.Del(ctx, LastSeenKey(sessionID))
	cleanup.Del(ctx, ConcurrentCountKey(sessionID))
	cleanup.Del(ctx, MessagesKey(sessionID))
	cleanup.ZRem(ctx, ActiveSessionsGlobalKey(), sessionID)
	if providerID != 0 {
		cleanup.ZRem(ctx, ProviderActiveSessionsKey(providerID), sessionID)
		cleanup.HDel(ctx, ProviderActiveSessionRefsKey(providerID), sessionID)
	}
	if keyID != 0 {
		cleanup.ZRem(ctx, KeyActiveSessionsKey(keyID), sessionID)
	}
	if userID != 0 {
		cleanup.ZRem(ctx, UserActiveSessionsKey(userID), sessionID)
	}

	commands, cleanupErr := cleanup.Exec(ctx)
	if cleanupErr != nil {
		return bindingTerminated, cleanupErr
	}

	deletedKeys := int64(0)
	for _, command := range commands {
		if command.Err() != nil {
			continue
		}
		if intCommand, ok := command.(*redis.IntCmd); ok && intCommand.Val() > 0 {
			deletedKeys += intCommand.Val()
		}
	}
	return bindingTerminated || deletedKeys > 0, nil
}

// TerminateSessionsBatch 复刻 SessionManager.terminateSessionsBatch（session-manager.ts:3822-3857）。
//
// 分块（每批 20 条）是为了别让大批量终止把 Redis 压垮。Node 在块内并发、块间串行；此处块内也
// 串行——每条 CAS 作用在不同会话的键上，先后不影响任一条的结果，也就不需要那点并发。
//
// 单条失败静默计 0：Node 的 terminateSession 在 catch 里吞掉异常并返回 false，不冒泡、
// 不阻断其余条目。返回值与 Node 同义：成功终止的条数。
func (b *Binder) TerminateSessionsBatch(
	ctx context.Context,
	sessionIDs []string,
	expectedProviderIDs []int64,
) (int, error) {
	if len(sessionIDs) == 0 {
		return 0, nil
	}
	if !b.Ready() {
		return 0, ErrNilClient
	}

	terminated := 0
	for start := 0; start < len(sessionIDs); start += terminateSessionsChunkSize {
		end := min(start+terminateSessionsChunkSize, len(sessionIDs))
		for _, sessionID := range sessionIDs[start:end] {
			ok, err := b.TerminateSession(ctx, sessionID, expectedProviderIDs, nil)
			if err != nil {
				continue
			}
			if ok {
				terminated++
			}
		}
	}
	return terminated, nil
}

// TerminateProviderSessionsBatch 复刻 SessionManager.terminateProviderSessionsBatch
// （session-manager.ts:3733-3798）：先按 provider 的活跃会话索引取出会话 id 集合，
// 再按「供应商范围」批量终止。
//
// 逐条命令判错（Node 在 pipeline 结果上也是逐条判 err 后 continue），索引读不出来的 provider
// 直接跳过；一个会话都没取到则返回 0。
func (b *Binder) TerminateProviderSessionsBatch(ctx context.Context, providerIDs []int64) (int, error) {
	uniqueProviderIDs := positiveUniqueProviderIDs(providerIDs)
	if len(uniqueProviderIDs) == 0 {
		return 0, nil
	}
	if !b.Ready() {
		return 0, ErrNilClient
	}

	pipe := b.client.rc.Raw().Pipeline()
	commands := make([]*redis.StringSliceCmd, 0, len(uniqueProviderIDs))
	for _, providerID := range uniqueProviderIDs {
		commands = append(commands, pipe.ZRange(ctx, ProviderActiveSessionsKey(providerID), 0, -1))
	}
	// 聚合错误不看：命令级错误已落在各自的 Cmd 上，逐条判更接近 Node。
	_, _ = pipe.Exec(ctx)

	sessionIDs := make([]string, 0, 16)
	seen := make(map[string]struct{}, 16)
	for _, command := range commands {
		if command.Err() != nil {
			continue
		}
		for _, member := range command.Val() {
			// 成员是「会话身份 + 尝试 token」（见 ProviderAttemptMember）：取回会话身份，
			// 否则后续按会话终止会拿 token 去查绑定、静默失效。
			sessionID := SessionIDFromProviderAttemptMember(member)
			if strings.TrimSpace(sessionID) == "" {
				continue
			}
			if _, duplicate := seen[sessionID]; duplicate {
				continue
			}
			seen[sessionID] = struct{}{}
			sessionIDs = append(sessionIDs, sessionID)
		}
	}
	if len(sessionIDs) == 0 {
		return 0, nil
	}

	return b.TerminateSessionsBatch(ctx, sessionIDs, uniqueProviderIDs)
}

// positiveUniqueProviderIDs 复刻 `Array.from(new Set(ids.filter(Number.isInteger && id > 0)))`
// （session-manager.ts:3734-3736）：只留正整数，去重且保持首次出现顺序。
func positiveUniqueProviderIDs(providerIDs []int64) []int64 {
	unique := make([]int64, 0, len(providerIDs))
	seen := make(map[int64]struct{}, len(providerIDs))
	for _, providerID := range providerIDs {
		if providerID <= 0 {
			continue
		}
		if _, duplicate := seen[providerID]; duplicate {
			continue
		}
		seen[providerID] = struct{}{}
		unique = append(unique, providerID)
	}
	return unique
}

// clearProviderIndex 摘除某个 provider 自己的活跃索引（Node 的 providerCleanup 管道）。
//
// 清理失败不改变结论：Node 同样只记 warn 并继续——CAS 已经生效，索引残留会随 TTL 过期。
func (b *Binder) clearProviderIndex(ctx context.Context, providerID int64, sessionID string) {
	raw := b.client.rc.Raw()
	pipe := raw.Pipeline()
	pipe.ZRem(ctx, ProviderActiveSessionsKey(providerID), sessionID)
	pipe.HDel(ctx, ProviderActiveSessionRefsKey(providerID), sessionID)
	_, _ = pipe.Exec(ctx)
}

// TerminateObservedSessionForIdentity 终止展示用的有效 Session identity（Node 的
// SessionTracker.terminateObservedSession），与 Binder.TerminateObservedSession 同义，
// 供终止路径按 identity 调用时语义自明。
func (b *Binder) TerminateObservedSessionForIdentity(ctx context.Context, identity string) (bool, error) {
	return b.TerminateObservedSession(ctx, identity)
}

// positiveIntOrZero 把 Redis 取回的字符串转成正整数，非法或非正值一律归 0（Node 的
// Number.isSafeInteger 校验 + `> 0` 过滤）。
func positiveIntOrZero(command redis.Cmder) int64 {
	value, err := commandString(command)
	if err != nil || value == "" {
		return 0
	}
	parsed, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

// commandString 取 GET/HGET 的字符串回复；键不存在（redis.Nil）视为空串。
func commandString(command redis.Cmder) (string, error) {
	stringCmd, ok := command.(*redis.StringCmd)
	if !ok {
		return "", nil
	}
	value, err := stringCmd.Result()
	if err == redis.Nil {
		return "", nil
	}
	return value, err
}

// containsProvider 判断 providerID 是否在期望集合里（0 永不匹配）。
func containsProvider(list []int64, providerID int64) bool {
	if providerID <= 0 {
		return false
	}
	for _, candidate := range list {
		if candidate == providerID {
			return true
		}
	}
	return false
}
