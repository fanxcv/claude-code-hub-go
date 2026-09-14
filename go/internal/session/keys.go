package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
)

// DefaultBindingTTLSeconds 与 Node 侧 DEFAULT_SESSION_BINDING_TTL_SECONDS 一致。
const DefaultBindingTTLSeconds = 300

// bindingHashTag 是绑定键的 hash tag：sha256(keyId + "\0" + sessionId) 的十六进制。
//
// 三个绑定键共享同一个 tag 是刻意的：绑定 Lua 一次操作 canonical 与两个 legacy 镜像
// 键，Redis Cluster 下不同 slot 会直接 CROSSSLOT 失败（与 Node 的 bindingHashTag 一致）。
func bindingHashTag(sessionID string, keyID int64) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(keyID, 10) + "\x00" + sessionID))
	return hex.EncodeToString(sum[:])
}

// BindingKeys 是绑定状态机使用的三个键。
type BindingKeys struct {
	// Canonical 是规范绑定 HASH：session-binding:v1:{tag}:binding。
	Canonical string
	// LegacyProvider 是旧版 provider 镜像：session:{sessionID}:provider。
	LegacyProvider string
	// LegacyOwner 是旧版 key owner 镜像：session:{sessionID}:key。
	LegacyOwner string
}

// BuildBindingKeys 复刻 buildSessionBindingKeys（无 namespace）。
func BuildBindingKeys(sessionID string, keyID int64) BindingKeys {
	return BindingKeys{
		Canonical:      fmt.Sprintf("session-binding:v1:{%s}:binding", bindingHashTag(sessionID, keyID)),
		LegacyProvider: fmt.Sprintf("session:%s:provider", sessionID),
		LegacyOwner:    fmt.Sprintf("session:%s:key", sessionID),
	}
}

// LegacyProviderKey 是旧版 provider 镜像键（不依赖 keyId，终止预检直接用它读）。
func LegacyProviderKey(sessionID string) string {
	return fmt.Sprintf("session:%s:provider", sessionID)
}

// LegacyOwnerKey 是旧版 key owner 镜像键（不依赖 keyId）。
func LegacyOwnerKey(sessionID string) string {
	return fmt.Sprintf("session:%s:key", sessionID)
}

// ProviderCooldownKey 是供应商冷却键：session-binding:v1:{tag}:provider:{id}:cooldown。
func ProviderCooldownKey(sessionID string, keyID, providerID int64) string {
	return fmt.Sprintf(
		"session-binding:v1:{%s}:provider:%d:cooldown",
		bindingHashTag(sessionID, keyID), providerID,
	)
}

// DiscoveryLeaseKey 是 discovery 租约键：session-binding:v1:{tag}:discovery-lease。
func DiscoveryLeaseKey(sessionID string, keyID int64) string {
	return fmt.Sprintf("session-binding:v1:{%s}:discovery-lease", bindingHashTag(sessionID, keyID))
}

// SeqKey 是会话内请求序号键（Node 侧 session:{sessionId}:seq）。
func SeqKey(sessionID string) string {
	return fmt.Sprintf("session:%s:seq", sessionID)
}

// ResponseBodyGenerationKey 是响应体代际键（Node 侧 buildSessionResponseBodyGenerationKey）。
func ResponseBodyGenerationKey(sessionID string) string {
	return fmt.Sprintf("session:%s:response-body-generation:v1", sessionID)
}

// RequestOwnerKey 是请求工件 owner 键：session:{id}:req:{seq}:owner。
func RequestOwnerKey(sessionID string, sequence int) string {
	return fmt.Sprintf("session:%s:req:%d:owner", sessionID, sequence)
}

// LastSeenKey 是会话最后活动时间键（Node 侧 session:{id}:last_seen）。
func LastSeenKey(sessionID string) string {
	return fmt.Sprintf("session:%s:last_seen", sessionID)
}

// InfoKey 是会话详情信息键（Node 侧 session:{id}:info）。
func InfoKey(sessionID string) string {
	return fmt.Sprintf("session:%s:info", sessionID)
}

// TenantContentHashSessionKey 是「租户内正文哈希 -> 会话」映射键：hash:{keyId}:{hash}:session。
func TenantContentHashSessionKey(keyID int64, contentHash string) string {
	return fmt.Sprintf("hash:%d:%s:session", keyID, contentHash)
}

// ObservedGlobalActiveSessionsKey 是 Dashboard/Sessions 页统一统计用的有效 Session identity ZSET。
//
// 与 limit 包的 ActiveSessionsGlobalKey（{active_sessions}:global:...）不同：那个键同时承载
// 并发判定，这里只承载展示。本包刻意不 import limit（避免未来接线成环），键形制逐字一致。
func ObservedGlobalActiveSessionsKey() string {
	return "{observed_sessions}:global:active_sessions"
}

// ObservedConcurrentCountKey 是展示用的并发计数键（Node 侧 observed_session:{id}:concurrent_count）。
func ObservedConcurrentCountKey(sessionIdentity string) string {
	return fmt.Sprintf("observed_session:%s:concurrent_count", sessionIdentity)
}

// ActiveSessionsGlobalKey 是全局活跃 Session ZSET（观测用）。
// 形制与 limit.ActiveSessionsGlobalKey 逐字一致，见 ObservedGlobalActiveSessionsKey 的说明。
func ActiveSessionsGlobalKey() string {
	return "{active_sessions}:global:active_sessions"
}

// KeyActiveSessionsKey 是 Key 维度活跃 Session ZSET。
func KeyActiveSessionsKey(keyID int64) string {
	return fmt.Sprintf("{active_sessions}:key:%d:active_sessions", keyID)
}

// UserActiveSessionsKey 是 User 维度活跃 Session ZSET。
func UserActiveSessionsKey(userID int64) string {
	return fmt.Sprintf("{active_sessions}:user:%d:active_sessions", userID)
}
