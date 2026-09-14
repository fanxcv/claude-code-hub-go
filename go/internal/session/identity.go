package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 会话身份提取与生成：复刻 SessionManager.extractClientSessionId /
// extractCodexSessionId / generateSessionId / calculateMessagesHash。

// Codex 会话 id 的取值范围（与 Node 侧常量逐字一致）。
const (
	// codexSessionIDMinLength 是 Codex session_id 的最小长度（UUID 风格 > 20 字符）。
	codexSessionIDMinLength = 21
	// codexSessionIDMaxLength 防止恶意输入撑爆 Redis 键。
	codexSessionIDMaxLength = 256
)

var codexSessionIDPattern = regexp.MustCompile(`^[\w\-.:]+$`)

// legacyUserIDPattern 是 Claude Code 旧版 user_id 的形制：user_{deviceId}_account__session_{sessionId}。
var legacyUserIDPattern = regexp.MustCompile(`^user_(.+?)_account__session_(.+)$`)

// MetadataUserParse 是 parseClaudeMetadataUserId 的结果。
type MetadataUserParse struct {
	SessionID   string
	Format      string // "legacy" | "json"
	DeviceID    string
	AccountUuid string
}

// ParseClaudeMetadataUserID 复刻 parseClaudeMetadataUserId。
//
// 优先 JSON 格式（{"session_id":...,"device_id":...}），失败再试旧版正则。
// 两种格式都不中返回空 SessionID。
func ParseClaudeMetadataUserID(userID any) MetadataUserParse {
	s, ok := userID.(string)
	if !ok {
		return MetadataUserParse{}
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return MetadataUserParse{}
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err == nil && obj != nil {
		sessionID, _ := obj["session_id"].(string)
		sessionID = strings.TrimSpace(sessionID)
		if sessionID != "" {
			deviceID, _ := obj["device_id"].(string)
			accountUuid, _ := obj["account_uuid"].(string)
			return MetadataUserParse{SessionID: sessionID, Format: "json", DeviceID: deviceID, AccountUuid: accountUuid}
		}
	}

	match := legacyUserIDPattern.FindStringSubmatch(trimmed)
	if len(match) != 3 {
		return MetadataUserParse{}
	}
	sessionID := strings.TrimSpace(match[2])
	if sessionID == "" {
		return MetadataUserParse{}
	}
	return MetadataUserParse{SessionID: sessionID, Format: "legacy", DeviceID: match[1]}
}

// NormalizeCodexSessionID 复刻 normalizeCodexSessionId：接受字符串，长度与字符集双检。
func NormalizeCodexSessionID(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) < codexSessionIDMinLength {
		return ""
	}
	if len(trimmed) > codexSessionIDMaxLength {
		return ""
	}
	if !codexSessionIDPattern.MatchString(trimmed) {
		return ""
	}
	return trimmed
}

// headerValue 取单值请求头（Node 的 headers.get 语义：取第一个值）。
func headerValue(headers map[string][]string, name string) string {
	values := headers[name]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// parseMetadataBody 取正文顶层 metadata 字典。
func parseMetadataBody(body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	return metadata
}

// ExtractCodexSessionID 复刻 extractCodexSessionId：仅当正文顶层有 input 数组时走此路径。
//
// 优先级：session_id 头 > x-session-id 头 > body.prompt_cache_key > metadata.session_id >
// body.previous_response_id（带 codex_prev_ 前缀）。全部不合法返回空。
func ExtractCodexSessionID(headers map[string][]string, body map[string]any) string {
	if headerSessionID := NormalizeCodexSessionID(headerValue(headers, "session_id")); headerSessionID != "" {
		return headerSessionID
	}
	if headerSessionID := NormalizeCodexSessionID(headerValue(headers, "x-session-id")); headerSessionID != "" {
		return headerSessionID
	}
	if bodySessionID := NormalizeCodexSessionID(body["prompt_cache_key"]); bodySessionID != "" {
		return bodySessionID
	}
	if metadata := parseMetadataBody(body); metadata != nil {
		if bodySessionID := NormalizeCodexSessionID(metadata["session_id"]); bodySessionID != "" {
			return bodySessionID
		}
	}
	if prevResponseID := NormalizeCodexSessionID(body["previous_response_id"]); prevResponseID != "" {
		sessionID := "codex_prev_" + prevResponseID
		if len(sessionID) <= codexSessionIDMaxLength {
			return sessionID
		}
	}
	return ""
}

// ExtractClientSessionID 复刻 SessionManager.extractClientSessionId。
//
// 优先级：Codex 请求（body.input 为数组）走 Codex 提取；否则 metadata.user_id
// （JSON 优先、旧版正则兜底），再退 metadata.session_id。找不到返回空。
func ExtractClientSessionID(body map[string]any, headers map[string][]string) string {
	if body == nil {
		return ""
	}
	if _, isCodex := body["input"].([]any); isCodex {
		return ExtractCodexSessionID(headers, body)
	}

	metadata := parseMetadataBody(body)
	if metadata == nil {
		return ""
	}
	if userID := ParseClaudeMetadataUserID(metadata["user_id"]); userID.SessionID != "" {
		return userID.SessionID
	}
	if metadataSessionID, ok := metadata["session_id"].(string); ok && metadataSessionID != "" {
		return metadataSessionID
	}
	return ""
}

// GenerateSessionID 复刻 SessionManager.generateSessionId：sess_{ts36}_{randomhex}。
func GenerateSessionID() string {
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 36)
	var random [6]byte
	// crypto/rand.Read 在 Linux 上不会失败；万一失败就用全零，不阻断分配。
	_, _ = rand.Read(random[:])
	return "sess_" + timestamp + "_" + hex.EncodeToString(random[:])
}

// CalculateMessagesHash 复刻 SessionManager.calculateMessagesHash。
//
// 降级方案：取前 min(len,3) 条消息的 text 内容拼接（"|" 分隔）后做 sha256，截前 16 字符。
// 无有效内容返回空。
func CalculateMessagesHash(messages any) string {
	list, ok := messages.([]any)
	if !ok || len(list) == 0 {
		return ""
	}
	count := len(list)
	if count > 3 {
		count = 3
	}
	var contents []string
	for _, message := range list[:count] {
		messageObj, ok := message.(map[string]any)
		if !ok {
			continue
		}
		content := messageObj["content"]

		switch c := content.(type) {
		case string:
			if c != "" {
				contents = append(contents, c)
			}
		case []any:
			var textParts []string
			for _, item := range c {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if block["type"] != "text" {
					continue
				}
				if text, ok := block["text"].(string); ok {
					textParts = append(textParts, text)
				}
			}
			joined := strings.Join(textParts, "")
			if joined != "" {
				contents = append(contents, joined)
			}
		}
	}

	if len(contents) == 0 {
		return ""
	}
	combined := strings.Join(contents, "|")
	sum := sha256.Sum256([]byte(combined))
	return hex.EncodeToString(sum[:])[:16]
}
