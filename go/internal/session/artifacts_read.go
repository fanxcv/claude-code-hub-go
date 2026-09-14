package session

import (
	"context"
	"encoding/json"
	"strconv"
)

// 本文件是会话**请求工件的只读面与所有者围栏**（详情面四条端点读它）。
//
// 唯一真源：src/lib/session-manager.ts 的 isSessionRequestOwnedByKey（:738-758）、
// refreshSessionRequestOwner（:760-771）、getSessionMessages（:2252-2284）与
// hasAnySessionMessages（:2292-2340）。
//
// 为什么所有者键是详情面的地基：工件本身**不带权限信息**——`session:{id}:req:{seq}:messages`
// 这个键里没有 keyId，谁能读全凭调用方自觉。Node 的判据是另一个键
// `session:{id}:req:{seq}:owner` 存着写这份工件的 keyId，读侧拿 locator 给出的 keyId 比对。
// 少了这条围栏，「知道 sessionId 的任何普通用户」都能读到别人的会话正文——这是安全边界，
// 不是优化项。
//
// 三处照抄 Node 的细节：
//
//  1. **读失败与不存在同判**。Node 的两条读在 Redis 故障时也返回「不存在」（getSessionMessages
//     返回 null、isSessionRequestOwnedByKey 返回 false）。本实现同样把故障折成「无此工件」，
//     把 error 只留给真正需要区分的调用方（本包让调用方按 Node 的读语义折平）。
//  2. **序号非法即无工件**。normalizeRequestSequence 把非正数判成 null，而 null 序号在
//     getSessionMessages 里直接返回 null（不是回退到 legacy 键）——「传了 0」与「没传」在读
//     消息时**不同判**，与定位器那套判据不是一个语义。
//  3. **存在性检查含旧格式**。hasAnySessionMessages 先查 legacy 键，再 SCAN 新格式；
//     只查新格式会把升级前写入的会话答成「没有详情」。

// SessionRequestOwnerKey 是工件所有者键：session:{id}:req:{seq}:owner。
func SessionRequestOwnerKey(sessionID string, sequence int) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(normalizeArtifactSequence(sequence)) + ":owner"
}

// LegacyMessagesKey 是旧格式的 messages 键：session:{id}:messages（无序号时的回退入口）。
func LegacyMessagesKey(sessionID string) string {
	return "session:" + sessionID + ":messages"
}

// StoreSessionRequestOwner 复刻 refreshSessionRequestOwner：写所有者键并刷 TTL。
//
// sequence 用 Node 的 normalizeRequestSequence 口径（非正数按 1）；keyID<=0 时**不写**
// （Node 的 `keyId === undefined` 判定）——写了 0 会让所有读侧的所有权比对全部失败，
// 比不写更坏（不写至少保持「无围栏」的原状）。
func (b *Binder) StoreSessionRequestOwner(
	ctx context.Context, sessionID string, sequence int, keyID int64,
) error {
	if !b.Ready() || sessionID == "" || keyID <= 0 {
		return nil
	}
	return b.client.rc.Raw().Set(
		ctx, SessionRequestOwnerKey(sessionID, sequence), strconv.FormatInt(keyID, 10),
		b.sessionTTL(),
	).Err()
}

// IsSessionRequestOwnedByKey 复刻 isSessionRequestOwnedByKey。
//
// 字符串比对而不是数值比对：Node 的判据就是 `(await redis.get(key)) === String(expectedKeyId)`，
// 沿用字符串能同时覆盖「键里存着别的形态」（历史数据）与「键不存在」两种落空。
func (b *Binder) IsSessionRequestOwnedByKey(
	ctx context.Context, sessionID string, sequence int, expectedKeyID int64,
) bool {
	if !b.Ready() || sessionID == "" || expectedKeyID <= 0 {
		return false
	}
	stored, err := b.client.rc.Raw().Get(
		ctx, SessionRequestOwnerKey(sessionID, sequence)).Result()
	if err != nil {
		return false
	}
	return stored == strconv.FormatInt(expectedKeyID, 10)
}

// SessionMessages 复刻 getSessionMessages。
//
// 返回 (值, 是否存在, error)：值可能是任意 JSON（数组、对象、标量），故不解成固定类型。
//
// **只读按序号的新格式键**：Node 的两条会话端点总是把定位器给出的序号原样传下来
// （`getSessionMessages(sourceSessionId, locator.requestSequence)`），故 legacy 分支
// （无序号时的 `session:{id}:messages`）从这两条路径**不可达**——它只服务于旧数据的
// 存在性检查（见 HasAnySessionMessages）。序号非正时 Node 的 normalizeRequestSequence
// 判成 null 并直接返回 null（**不**回退 legacy），这里同判。
func (b *Binder) SessionMessages(
	ctx context.Context, sessionID string, sequence int,
) (any, bool, error) {
	if !b.Ready() || sessionID == "" || sequence <= 0 {
		return nil, false, nil
	}
	raw, err := b.client.rc.Raw().Get(ctx, MessagesSequenceKey(sessionID, sequence)).Result()
	if err != nil {
		// redis.Nil 与 Redis 故障同判：Node 的 catch 也返回 null。
		return nil, false, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		// 坏 JSON 同样按「无此工件」：Node 的 JSON.parse 抛错后 catch 返回 null。
		return nil, false, nil
	}
	return decoded, true, nil
}

// RawDel 删一个键（用例清理用；生产路径不用它——生产只删自己刚判超限的那一个键）。
func (b *Binder) RawDel(ctx context.Context, key string) error {
	if !b.Ready() || key == "" {
		return nil
	}
	return b.client.rc.Raw().Del(ctx, key).Err()
}

// HasAnySessionMessages 复刻 hasAnySessionMessages：先查旧格式，再 SCAN 新格式。
func (b *Binder) HasAnySessionMessages(ctx context.Context, sessionID string) bool {
	if !b.Ready() || sessionID == "" {
		return false
	}
	raw := b.client.rc.Raw()
	exists, err := raw.Exists(ctx, LegacyMessagesKey(sessionID)).Result()
	if err != nil {
		return false
	}
	if exists > 0 {
		return true
	}

	pattern := "session:" + sessionID + ":req:*:messages"
	cursor := uint64(0)
	for {
		keys, next, err := raw.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return false
		}
		if len(keys) > 0 {
			return true
		}
		cursor = next
		if cursor == 0 {
			return false
		}
	}
}
