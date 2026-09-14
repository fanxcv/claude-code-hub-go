package replay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// BypassHeader 与 Node REPLAY_BYPASS_HEADER 一致：置 1 时跳过 attach（有意重复采样），
// 仍可成为 owner；已完成条目不覆写。
const BypassHeader = "x-cch-no-replay"

// Identity 是一次请求的回放身份（对应 ReplayIdentity）。
//
// replayId 含 scopeTag（scope 含 keyId），跨租户不可能命中；verifier 仅内容维度，attach
// 时严格比对，防 replayId 哈希碰撞。
type Identity struct {
	ReplayID string
	Verifier string
	ScopeTag string
	KeyID    int64
	UserID   int64
	Format   string
	Model    string
	Endpoint string
}

// DeriveIdentity 从入口事实与逻辑请求体推导回放身份，与 replay-identity.ts 的语义对齐：
//
//   - messageJSON 是 requestFilter 过滤后的逻辑请求体（JSON 字节），不取过滤前原始 buffer；
//     过滤规则变更后自然产生新身份，绝不命中旧规则时代的条目。
//   - format 是客户端协议格式（与 buildScopeTag 的输入一致），由接线波次从入口/转换层提供。
//
// 不合格（功能关/非 POST/非流式/缺鉴权主体/正文不可解析）时返回 nil, nil——请求按现状处理。
func DeriveIdentity(req *pctx.Context, messageJSON []byte, format string) (*Identity, error) {
	if req == nil || req.Method() != "POST" {
		return nil, nil
	}
	auth, ok := req.Auth()
	if !ok || auth.KeyID <= 0 || auth.UserID <= 0 {
		return nil, nil
	}

	var message any
	if err := json.Unmarshal(messageJSON, &message); err != nil {
		return nil, fmt.Errorf("replay: 逻辑请求体不可解析: %w", err)
	}
	object, isObject := message.(map[string]any)
	if !isObject {
		return nil, nil
	}
	if stream, _ := object["stream"].(bool); !stream {
		return nil, nil
	}
	model, _ := object["model"].(string)

	endpoint := req.Path()
	if endpoint == "" {
		endpoint = "/"
	}

	scopeTag := buildScopeTag(auth.KeyID, format, model)
	bodyHash := sha256Hex(stableStringify(message))

	idempotencyKey := headerValue(req, "idempotency-key")
	if idempotencyKey == "" {
		idempotencyKey = headerValue(req, "x-idempotency-key")
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)

	replayID := sha256Hex(fmt.Sprintf(
		"cch_replay_v1|%s|%s|%s|stream|ik=%s|%s",
		scopeTag, endpoint, model, idempotencyKey, bodyHash,
	))[:32]
	verifier := sha256Hex(fmt.Sprintf(
		"cch_replay_vf1|%s|%s|%s|stream|ik=%s",
		bodyHash, endpoint, model, idempotencyKey,
	))[:32]

	return &Identity{
		ReplayID: replayID,
		Verifier: verifier,
		ScopeTag: scopeTag,
		KeyID:    auth.KeyID,
		UserID:   auth.UserID,
		Format:   format,
		Model:    model,
		Endpoint: endpoint,
	}, nil
}

// headerValue 取 headers 的首值；HeaderView.Get 按 Go 的规范化键查找。
func headerValue(req *pctx.Context, key string) string {
	return req.Headers().Get(key)
}

// buildScopeTag 复刻 buildScopeTag：sha256(keyId|format|model) 截 16 hex。
func buildScopeTag(keyID int64, format, model string) string {
	return sha256Hex(fmt.Sprintf("%d|%s|%s", keyID, format, model))[:16]
}

func sha256Hex(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

// stableStringify 复刻 stableStringify：对象键按字典序排序，数组保序，原始值按
// Node JSON.stringify 语义序列化。用于从解析后的 message 派生确定性字节。
//
// 数值格式差异（已知边界）：Node 对极小/极大浮点用 ECMAScript 最短表示（如 1e-7），
// encoding/json 的 %g 可能输出指数零填充（1e-07）。实际请求体以整数/字符串为主，
// 该差异只会造成身份不同（等价 miss），不会错发他人响应。
func stableStringify(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case []any:
		var builder strings.Builder
		builder.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				builder.WriteByte(',')
			}
			builder.WriteString(stableStringify(item))
		}
		builder.WriteByte(']')
		return builder.String()
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var builder strings.Builder
		builder.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				builder.WriteByte(',')
			}
			builder.WriteString(nodeJSONQuote(key))
			builder.WriteByte(':')
			builder.WriteString(stableStringify(v[key]))
		}
		builder.WriteByte('}')
		return builder.String()
	case string:
		return nodeJSONQuote(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64:
		// 与 JSON.stringify 对齐：整数不带小数点。
		return strconv.FormatFloat(v, 'g', -1, 64)
	case json.Number:
		return v.String()
	default:
		// 理论不可达（json.Unmarshal 只产出上述类型），兜底不 panic。
		encoded, _ := json.Marshal(v)
		return string(encoded)
	}
}

// nodeJSONQuote 按 Node JSON.stringify 的字符串转义规则输出带引号的字符串：
// 引号/反斜杠/常见控制符转义，其余原样输出（JSON.stringify 默认不转义 < > &）。
//
// 注意：encoding/json 默认会把 < > & 转成 \u003c 等，会破坏与 Node 的逐字节一致，故此处
// 不用 json.Marshal 处理字符串。
func nodeJSONQuote(s string) string {
	var builder bytes.Buffer
	builder.WriteByte('"')
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		switch r {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\t':
			builder.WriteString(`\t`)
		case '\n':
			builder.WriteString(`\n`)
		case '\f':
			builder.WriteString(`\f`)
		case '\r':
			builder.WriteString(`\r`)
		default:
			if r < 0x20 {
				builder.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				builder.WriteString(s[:size])
			}
		}
		s = s[size:]
	}
	builder.WriteByte('"')
	return builder.String()
}
