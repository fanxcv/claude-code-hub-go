package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/clientver"
)

// 本文件是 Claude metadata.user_id 的**构建与注入**侧，复刻
// `src/lib/claude-code/metadata-user-id.ts`；同一概念的解析侧在 identity.go
// （ParseClaudeMetadataUserID）——一对读写放同一个包，避免形制定义出现第二份。
//
// 形态由客户端类型与版本共同决定（Node resolveClaudeMetadataUserIdFormat）：
// Claude Code 早于 2.1.78 只认旧版形制 `user_{deviceId}_account__session_{sessionId}`，
// 到 2.1.78 起改认 JSON 串。判错的代价是上游读到一条它不认识的身份串，故按 UA 逐条对齐。

// claudeMetadataUserIDJSONSwitchVersion 是切到 JSON 形制的客户端版本阈值
// （Node CLAUDE_CODE_METADATA_USER_ID_JSON_SWITCH_VERSION）。
const claudeMetadataUserIDJSONSwitchVersion = "2.1.78"

// claudeMetadataFormatLegacy / claudeMetadataFormatJSON 是两种注入形制。
const (
	claudeMetadataFormatLegacy = "legacy"
	claudeMetadataFormatJSON   = "json"
)

// claudeMetadataClientTypes 是会用 metadata.user_id 携带会话身份的客户端类型（Node 同集合）。
//
// 不在集合内的一律用 JSON 形制：Node 的默认支就是 json，非 Claude Code 客户端
// （如 anthropic-sdk-typescript）与 UA 解析失败都落这一支。
var claudeMetadataClientTypes = map[string]bool{
	"claude-cli":         true,
	"claude-vscode":      true,
	"claude-cli-unknown": true,
}

// ClaudeMetadataUserIDInjection 是一次注入的判定结果。
//
// Reason 的取值与 Node 的审计字段同名同值（injected / already_exists / missing_key_id /
// missing_session_id）——转发层要拿它写 `claude_metadata_user_id_injection` 审计条目。
type ClaudeMetadataUserIDInjection struct {
	// Applied 为真表示本次写入了 user_id。
	Applied bool
	// Reason 是判定理由（见上方取值）。
	Reason string
	// Value 是写入的值（Applied 为假时为空串）。
	Value string
}

// claudeMetadataUserIDFormat 复刻 resolveClaudeMetadataUserIdFormat。
func claudeMetadataUserIDFormat(userAgent string) string {
	client, ok := clientver.ParseUserAgent(userAgent)
	if !ok || !claudeMetadataClientTypes[client.ClientType] {
		return claudeMetadataFormatJSON
	}
	if clientver.IsVersionLess(client.Version, claudeMetadataUserIDJSONSwitchVersion) {
		return claudeMetadataFormatLegacy
	}
	return claudeMetadataFormatJSON
}

// BuildClaudeMetadataDeviceID 复刻 buildClaudeMetadataDeviceId：sha256("claude_user_{keyId}")。
//
// 它是「同一把密钥 = 同一个设备」的稳定标识，与请求无关，故不含任何每请求输入。
func BuildClaudeMetadataDeviceID(keyID int64) string {
	sum := sha256.Sum256([]byte("claude_user_" + strconv.FormatInt(keyID, 10)))
	return hex.EncodeToString(sum[:])
}

// HasUsableClaudeMetadataUserID 复刻 hasUsableClaudeMetadataUserId。
//
// 两种被判为「已有可用值」的情形：非空字符串；以及任何非 null 的值（数字、对象、布尔都算，
// Node 的判据是 `userId !== undefined && userId !== null`）。空串与 null 才需要注入。
func HasUsableClaudeMetadataUserID(userID any) bool {
	if text, ok := userID.(string); ok {
		return strings.TrimSpace(text) != ""
	}
	return userID != nil
}

// BuildClaudeMetadataUserID 复刻 buildClaudeMetadataUserId。
func BuildClaudeMetadataUserID(keyID int64, sessionID, userAgent string) string {
	deviceID := BuildClaudeMetadataDeviceID(keyID)
	if claudeMetadataUserIDFormat(userAgent) == claudeMetadataFormatLegacy {
		return "user_" + deviceID + "_account__session_" + sessionID
	}
	return encodeClaudeMetadataUserIDJSON(deviceID, sessionID)
}

// encodeClaudeMetadataUserIDJSON 按 Node 对象字面量的字段顺序序列化 JSON 形制。
//
// 必须关掉 HTML 转义：Node 的 JSON.stringify 不转义 `<`/`>`/`&`，而会话 id 可能来自客户端
// （`Session_id` 这类非小写头也照收），转义会让同一会话在上游侧得到两个不同的身份串。
func encodeClaudeMetadataUserIDJSON(deviceID, sessionID string) string {
	payload := struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}{DeviceID: deviceID, AccountUUID: "", SessionID: sessionID}

	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		// 三个字符串字段的编码不可能失败；真失败了也绝不注入半截 JSON。
		return ""
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

// InjectClaudeMetadataUserID 复刻 injectClaudeMetadataUserIdWithContext，并额外回答「为什么没注入」。
//
// 与 Node 的差异（唯一一处，且只在畸形输入上）：`metadata` 是数组时 Node 会把下标当键摊平进
// 新对象，Go 侧按「没有可保留的 metadata」处理。正常客户端不会把 metadata 写成数组。
func InjectClaudeMetadataUserID(
	body map[string]any,
	keyID int64,
	sessionID, userAgent string,
) ClaudeMetadataUserIDInjection {
	if body == nil {
		return ClaudeMetadataUserIDInjection{Reason: "missing_session_id"}
	}
	metadata, _ := body["metadata"].(map[string]any)
	if HasUsableClaudeMetadataUserID(metadata["user_id"]) {
		return ClaudeMetadataUserIDInjection{Reason: "already_exists"}
	}
	if keyID == 0 {
		return ClaudeMetadataUserIDInjection{Reason: "missing_key_id"}
	}
	if sessionID == "" {
		return ClaudeMetadataUserIDInjection{Reason: "missing_session_id"}
	}
	value := BuildClaudeMetadataUserID(keyID, sessionID, userAgent)
	if value == "" {
		return ClaudeMetadataUserIDInjection{Reason: "missing_session_id"}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["user_id"] = value
	body["metadata"] = metadata
	return ClaudeMetadataUserIDInjection{Applied: true, Reason: "injected", Value: value}
}
