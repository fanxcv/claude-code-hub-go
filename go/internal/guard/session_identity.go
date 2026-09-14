package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 会话身份的两种形制（Node 的 drizzle 列类型：'session_id' | 'prefix_affinity'）。
//
// 使用记录页读这一列区分「新会话新渠道」与「跨会话复用同一渠道」：
//
//	session_id      粘性绑定按客户端会话 id 走（同一会话同一渠道，换会话即换渠道）
//	prefix_affinity 粘性绑定按最长前缀指纹走（不同会话只要前缀相同即复用同一渠道）
const (
	sessionIdentityKindSessionID      = "session_id"
	sessionIdentityKindPrefixAffinity = "prefix_affinity"
)

// sessionIdentityColumns 复刻 Node 在建行时写入的 session_identity / session_identity_kind。
//
// 逐跳出处（Node）：
//   - 写入点：`src/app/v1/_lib/proxy/message-service.ts:119-120`
//     （`session_identity: sessionIdentity.identity || session.sessionId || undefined`、
//     `session_identity_kind: sessionIdentity.kind`）；
//   - 元数据来源：`src/app/v1/_lib/proxy/provider-selector.ts:340-370`——
//     `skipSessionBinding && session.affinity !== null && sessionId !== null` 时
//     kind 为 `prefix_affinity`、identity 为 `pfx:${scopeTag}:${identityFp ?? matchedFp ?? tipFp}`；
//     否则 sessionId 存在时 kind 为 `session_id`、identity 为
//     `buildPublicSessionIdentity(sessionId, keyId ?? "unbound")`；
//   - 兜底元数据：`src/app/v1/_lib/proxy/session.ts:710-718`——kind 为 `session_id`、identity 为空。
//
// 其中 `skipSessionBinding = affinityIgnoreClientSessionId && fingerprintable`
// （`provider-selector.ts:260-272`），`fingerprintable` 只要求指纹链可算，
// **不要求 Redis 可查**；Go 侧的对应事实是 pctx.AffinityIdentity（选路包在提名前算出）。
//
// 返回的 identity 为空字符串表示「该列留 NULL」。
func sessionIdentityColumns(
	sessionID string,
	keyID int64,
	req *pctx.Context,
	ignoreClientSessionID bool,
) (identity string, kind string) {
	if sessionID == "" {
		// Node 的兜底元数据：形制是 session_id，identity 为空（落库为 NULL）。
		return "", sessionIdentityKindSessionID
	}
	if ignoreClientSessionID {
		if aff, ok := req.AffinityIdentity(); ok {
			// 与 Node 一致：前缀身份**不再过** buildPublicSessionIdentity——
			// 它本身已带 pfx: 前缀，再摘要一次会把上游的命名空间藏掉。
			return "pfx:" + aff.ScopeTag + ":" + aff.Fingerprint, sessionIdentityKindPrefixAffinity
		}
	}
	return buildPublicSessionIdentity(sessionID, keyID), sessionIdentityKindSessionID
}

// buildPublicSessionIdentity 复刻 `src/lib/request-identity.ts:49-56`：
// 只有以 `pfx:` / `sid:` 开头的身份才做摘要（这类值可能来自上游或亲和键，不宜明文落库），
// 其余原样返回（普通客户端会话 id 本就是公开值）。
//
// keyID 为 0 时按 Node 的 `keyId ?? "unbound"` 处理。
func buildPublicSessionIdentity(sessionID string, keyID int64) string {
	if !strings.HasPrefix(sessionID, "pfx:") && !strings.HasPrefix(sessionID, "sid:") {
		return sessionID
	}
	key := "unbound"
	if keyID != 0 {
		key = strconv.FormatInt(keyID, 10)
	}
	sum := sha256.Sum256([]byte("session-identity:v1\x00" + key + "\x00" + sessionID))
	return "sid:" + hex.EncodeToString(sum[:])[:32]
}
