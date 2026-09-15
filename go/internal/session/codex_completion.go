package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件复刻 Node 的 Codex 会话标识补全（`src/app/v1/_lib/codex/session-completer.ts`）。
//
// 为什么需要它：Codex 客户端只用 `session_id` 请求头或正文 `prompt_cache_key` 之一表达对话身份，
// 且两者都缺时上游拿不到任何稳定标识——缓存前缀无处归属、路由粘性也无从谈起（上游华创的
// 《缓存命中优化规范 v1.0》把它列为必填）。补全规则是「互为补全 + 缺失时生成稳定 UUID v7」。
//
// 语义真源是上面那份 TS（逐分支对齐，勿凭记忆改）。三处容易写错的地方已就地注明：
//   - `x-session-id` 只是兼容头，**不满足**「session_id 已存在」这一条件；
//   - 头与正文都齐时**直接返回**（不改 x-session-id），这是 Node 的既有行为，不是漏写；
//   - 指纹缓存的键含 keyId/客户端 IP/UA/首条消息文本哈希，TTL 默认 300 秒（Node 的 SESSION_TTL）。

// Codex 会话补全的动作与来源取值域（与前端 `CodexSessionIdCompletionSpecialSetting` 逐字一致）。
const (
	ActionNone                   = "none"
	ActionCompletedMissingFields = "completed_missing_fields"
	ActionGeneratedUUIDV7        = "generated_uuid_v7"
	ActionReusedFingerprintCache = "reused_fingerprint_cache"

	SourceHeaderSessionID    = "header_session_id"
	SourceHeaderXSessionID   = "header_x_session_id"
	SourceBodyPromptCacheKey = "body_prompt_cache_key"
	SourceFingerprintCache   = "fingerprint_cache"
	SourceGeneratedUUIDV7    = "generated_uuid_v7"
)

// codexFingerprintTTLSeconds 对齐 Node `getSessionTtlSeconds` 的默认值。
//
// Node 还读环境变量 SESSION_TTL；该变量不在本仓的环境变量契约（go/env-parity.txt）内，
// 故这里只提供构造期覆盖（测试与运维按需传），不新开一个环境变量入口。
const codexFingerprintTTLSeconds = 300

const (
	codexFingerprintKeyPrefix = "codex:fingerprint:"
	codexFingerprintKeySuffix = ":session_id"
)

// CodexSessionCompletion 是一次补全的结果。
//
// 三个 Set* 布尔量是「要写入哪一侧」的显式声明：调用方（守卫链）据此改写正文与请求头，
// 本包不直接改上下文——守卫链是唯一有权改写请求的地方。
type CodexSessionCompletion struct {
	// Applied 表示本次确实补写了某个字段（对齐 Node 的 `applied`）。
	Applied bool
	// Action / Source / SessionID 是要落进审计条目的三个事实。
	Action    string
	Source    string
	SessionID string

	// SetBodyPromptCacheKey 为真：正文缺 `prompt_cache_key`，需补成 SessionID。
	SetBodyPromptCacheKey bool
	// SetHeaderSessionID / SetHeaderXSessionID 为真：对应请求头缺失，需补齐。
	SetHeaderSessionID  bool
	SetHeaderXSessionID bool
}

// CodexCompleteArgs 是补全的输入事实（Node 的 CompleteArgs）。
type CodexCompleteArgs struct {
	KeyID     int64
	Headers   map[string][]string
	Body      map[string]any
	UserAgent string
}

// CodexSessionStore 是补全所需的最小 Redis 能力。
//
// 为什么要接口：Redis 不可用时 Node 直接降级为「生成新 UUID v7」（不缓存），
// 这条降级路径必须可测——用一个替身比让测试依赖真 Redis 更省事。
type CodexSessionStore interface {
	// Get 取键值；键不存在返回空串与 nil 错误（对齐 redis.Nil 的归一）。
	Get(ctx context.Context, key string) (string, error)
	// SetNX 仅在键不存在时写入，返回是否写入成功。
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	// Set 覆写键值（Node 在 NX 竞争失败且回读为空时的兜底写入）。
	Set(ctx context.Context, key, value string, ttl time.Duration) error
}

// RedisCodexSessionStore 用 go-redis 实现 CodexSessionStore。
type RedisCodexSessionStore struct {
	rdb redis.UniversalClient
}

// NewRedisCodexSessionStore 组装 Redis 实现；rdb 为 nil 时返回 nil（调用方按未接线处理）。
func NewRedisCodexSessionStore(rdb redis.UniversalClient) *RedisCodexSessionStore {
	if rdb == nil {
		return nil
	}
	return &RedisCodexSessionStore{rdb: rdb}
}

// Get 实现 CodexSessionStore。
func (s *RedisCodexSessionStore) Get(ctx context.Context, key string) (string, error) {
	if s == nil || s.rdb == nil {
		return "", nil
	}
	value, err := s.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return value, err
}

// SetNX 实现 CodexSessionStore。
func (s *RedisCodexSessionStore) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	if s == nil || s.rdb == nil {
		return false, nil
	}
	return s.rdb.SetNX(ctx, key, value, ttl).Result()
}

// Set 实现 CodexSessionStore。
func (s *RedisCodexSessionStore) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if s == nil || s.rdb == nil {
		return nil
	}
	return s.rdb.Set(ctx, key, value, ttl).Err()
}

// CodexCompleterOptions 是补全器的构造参数。
type CodexCompleterOptions struct {
	// Store 为 nil 表示 Redis 未接线：按 Node 的降级语义每次生成新 UUID v7。
	Store CodexSessionStore
	// TTL 为指纹缓存存活时间；<=0 取 300 秒。
	TTL time.Duration
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Now / Rand 可注入，便于测试；nil 时分别取 time.Now 与 crypto/rand.Reader。
	Now  func() time.Time
	Rand func([]byte) error
}

// CodexCompleter 是 Codex 会话标识补全器。
type CodexCompleter struct {
	store  CodexSessionStore
	ttl    time.Duration
	log    *logx.Logger
	now    func() time.Time
	random func([]byte) error
}

// NewCodexCompleter 组装补全器。
func NewCodexCompleter(opts CodexCompleterOptions) *CodexCompleter {
	if opts.TTL <= 0 {
		opts.TTL = codexFingerprintTTLSeconds * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = logx.New(nil)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Rand == nil {
		opts.Rand = func(buffer []byte) error {
			_, err := rand.Read(buffer)
			return err
		}
	}
	return &CodexCompleter{store: opts.Store, ttl: opts.TTL, log: opts.Logger, now: opts.Now, random: opts.Rand}
}

// Complete 执行一次补全（Node 的 completeCodexSessionIdentifiers）。
func (c *CodexCompleter) Complete(ctx context.Context, args CodexCompleteArgs) CodexSessionCompletion {
	if c == nil {
		return CodexSessionCompletion{Action: ActionNone}
	}
	headerSessionID := NormalizeCodexSessionID(headerValue(args.Headers, "session_id"))
	headerXSessionID := NormalizeCodexSessionID(headerValue(args.Headers, "x-session-id"))
	bodyCacheKey := normalizeBodySessionID(args.Body["prompt_cache_key"])

	existing := ""
	source := ""
	switch {
	case headerSessionID != "":
		existing, source = headerSessionID, SourceHeaderSessionID
	case headerXSessionID != "":
		existing, source = headerXSessionID, SourceHeaderXSessionID
	case bodyCacheKey != "":
		existing, source = bodyCacheKey, SourceBodyPromptCacheKey
	}

	// 头与正文都齐：原样返回（注意此时**不**补 x-session-id，对齐 Node 的提前返回）。
	if headerSessionID != "" && bodyCacheKey != "" {
		return CodexSessionCompletion{Applied: false, Action: ActionNone, Source: source, SessionID: existing}
	}

	sessionID := existing
	action := ActionNone
	if sessionID == "" {
		sessionID, source, action = c.fromFingerprint(ctx, args)
	}

	result := CodexSessionCompletion{SessionID: sessionID, Source: source, Action: action}
	changed := false
	switch {
	case headerSessionID == "" && headerXSessionID == "":
		result.SetHeaderSessionID, result.SetHeaderXSessionID = true, true
		changed = true
	case headerSessionID == "" && headerXSessionID != "":
		// 只有兼容头：把它提升为主头。
		result.SetHeaderSessionID = true
		changed = true
	case headerSessionID != "" && headerXSessionID == "":
		result.SetHeaderXSessionID = true
		changed = true
	}
	if bodyCacheKey == "" {
		result.SetBodyPromptCacheKey = true
		changed = true
	}
	result.Applied = changed
	if existing != "" {
		// 已有标识（只是补另一侧）：动作固定为 completed_missing_fields，nothing changed 则 none。
		if changed {
			result.Action = ActionCompletedMissingFields
		} else {
			result.Action = ActionNone
		}
	}
	return result
}

// fromFingerprint 复刻 getOrCreateSessionIdFromFingerprint：同一身份指纹在 TTL 内复用同一会话 id。
func (c *CodexCompleter) fromFingerprint(ctx context.Context, args CodexCompleteArgs) (string, string, string) {
	hash := codexFingerprintHash(args)
	if c.store == nil || hash == "" {
		// Node：Redis 未就绪即降级生成（不缓存）。
		return c.generateUUIDV7(), SourceGeneratedUUIDV7, ActionGeneratedUUIDV7
	}
	key := codexFingerprintKeyPrefix + hash + codexFingerprintKeySuffix

	existing, err := c.store.Get(ctx, key)
	if err != nil {
		c.warnFallback(err)
		return c.generateUUIDV7(), SourceGeneratedUUIDV7, ActionGeneratedUUIDV7
	}
	if reused := NormalizeCodexSessionID(existing); reused != "" {
		return reused, SourceFingerprintCache, ActionReusedFingerprintCache
	}

	candidate := c.generateUUIDV7()
	written, err := c.store.SetNX(ctx, key, candidate, c.ttl)
	if err != nil {
		c.warnFallback(err)
		return candidate, SourceGeneratedUUIDV7, ActionGeneratedUUIDV7
	}
	if written {
		return candidate, SourceGeneratedUUIDV7, ActionGeneratedUUIDV7
	}
	// NX 竞争失败：回读赢家；回读仍为空则直接覆写（Node 的兜底分支）。
	after, err := c.store.Get(ctx, key)
	if err == nil {
		if reused := NormalizeCodexSessionID(after); reused != "" {
			return reused, SourceFingerprintCache, ActionReusedFingerprintCache
		}
	}
	if err := c.store.Set(ctx, key, candidate, c.ttl); err != nil {
		c.warnFallback(err)
	}
	return candidate, SourceGeneratedUUIDV7, ActionGeneratedUUIDV7
}

// warnFallback 记录一次降级（Node 同处写 warn 后继续，不阻断请求）。
func (c *CodexCompleter) warnFallback(err error) {
	c.log.Warn("session.codex_completion.redis_failed", map[string]any{
		"error": err.Error(),
		"note":  "指纹缓存不可用，降级为生成新 UUID v7",
	})
}

// generateUUIDV7 产出 UUID v7（48 位毫秒时间戳 + 随机，版本与变体位按 RFC 4122 写入）。
func (c *CodexCompleter) generateUUIDV7() string {
	return GenerateUUIDV7(c.now, c.random)
}

// GenerateUUIDV7 是 UUID v7 生成器的可注入实现（Node 的 generateUuidV7 逐位对齐）。
//
// 为什么不用第三方库：Node 侧的字节布局是契约（同对话 id 的形状会被客户端与上游记住），
// 移植时按位对齐即可，引一个依赖只为拼 16 字节不划算。
func GenerateUUIDV7(now func() time.Time, random func([]byte) error) string {
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = func(buffer []byte) error {
			_, err := rand.Read(buffer)
			return err
		}
	}
	buffer := make([]byte, 16)
	// 随机源失败不阻断：全零时间戳仍有 74 位随机度，比让请求失败更可取。
	_ = random(buffer)

	timestamp := now().UnixMilli()
	for index := 5; index >= 0; index-- {
		buffer[index] = byte(timestamp % 256)
		timestamp /= 256
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x70
	buffer[8] = (buffer[8] & 0x3f) | 0x80

	encoded := hex.EncodeToString(buffer)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

// codexFingerprintHash 复刻 calculateFingerprintHash：`v1|key:<id>|ip:<ip>|ua:<ua>|m:<首条消息哈希>`。
func codexFingerprintHash(args CodexCompleteArgs) string {
	ip := clientIPFromHeaders(args.Headers)
	if ip == "" {
		ip = "unknown"
	}
	userAgent := args.UserAgent
	if userAgent == "" {
		userAgent = headerValue(args.Headers, "user-agent")
	}
	if userAgent == "" {
		userAgent = "unknown"
	}
	messageHash := initialMessageTextHash(args.Body)
	if messageHash == "" {
		messageHash = "unknown"
	}
	fingerprint := fmt.Sprintf("v1|key:%d|ip:%s|ua:%s|m:%s", args.KeyID, ip, userAgent, messageHash)
	sum := sha256.Sum256([]byte(fingerprint))
	return hex.EncodeToString(sum[:])
}

// initialMessageTextHash 取正文 `input` 的前 3 条 message 文本并哈希（Node 同名前缀取法）。
func initialMessageTextHash(body map[string]any) string {
	if body == nil {
		return ""
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) == 0 {
		return ""
	}
	texts := make([]string, 0, 3)
	for _, item := range input {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// 只有 message 条目参与指纹；缺 type 的条目按 Node 的 `itemType && ...` 语义也参与。
		if itemType, ok := object["type"].(string); ok && itemType != "" && itemType != "message" {
			continue
		}
		switch content := object["content"].(type) {
		case string:
			if strings.TrimSpace(content) != "" {
				texts = append(texts, content)
			}
		case []any:
			var parts strings.Builder
			for _, part := range content {
				partObject, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if text, ok := partObject["text"].(string); ok && text != "" {
					parts.WriteString(text)
				}
			}
			if strings.TrimSpace(parts.String()) != "" {
				texts = append(texts, parts.String())
			}
		}
		if len(texts) >= 3 {
			break
		}
	}
	if len(texts) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(texts, "|")))
	return hex.EncodeToString(sum[:])[:16]
}

// clientIPFromHeaders 复刻 extractClientIp：x-forwarded-for 首段优先，其次 x-real-ip。
func clientIPFromHeaders(headers map[string][]string) string {
	if forwarded := headerValue(headers, "x-forwarded-for"); forwarded != "" {
		for _, part := range strings.Split(forwarded, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				return trimmed
			}
		}
	}
	return strings.TrimSpace(headerValue(headers, "x-real-ip"))
}

// normalizeBodySessionID 归一正文里的会话标识（body 里的值可能不是字符串）。
func normalizeBodySessionID(value any) string {
	return NormalizeCodexSessionID(value)
}
