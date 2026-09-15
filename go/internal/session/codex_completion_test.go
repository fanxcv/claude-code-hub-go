package session

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件的用例逐条对齐 Node `src/app/v1/_lib/codex/session-completer.ts` 的分支表
// （该 TS 文件没有自带用例，故这里按分支逐条钉住）。

// fakeCodexStore 是 CodexSessionStore 的假实现。
type fakeCodexStore struct {
	values map[string]string
	// nxFails 为真时 set NX 恒失败（模拟并发竞争）。
	nxFails bool
	// getSequence 非空时按调用次序作答，用于构造「先空后满」的竞争场景。
	getSequence []string
	getCalls    int
	getErr      error
	nxErr       error
	// setKeys 记录非 NX 覆写的键。
	setKeys []string
}

func newFakeCodexStore() *fakeCodexStore {
	return &fakeCodexStore{values: map[string]string{}}
}

func (s *fakeCodexStore) Get(_ context.Context, key string) (string, error) {
	if s.getErr != nil {
		return "", s.getErr
	}
	if len(s.getSequence) > 0 {
		value := ""
		if s.getCalls < len(s.getSequence) {
			value = s.getSequence[s.getCalls]
		}
		s.getCalls++
		return value, nil
	}
	return s.values[key], nil
}

func (s *fakeCodexStore) SetNX(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	if s.nxErr != nil {
		return false, s.nxErr
	}
	if s.nxFails {
		return false, nil
	}
	if _, exists := s.values[key]; exists {
		return false, nil
	}
	s.values[key] = value
	return true, nil
}

func (s *fakeCodexStore) Set(_ context.Context, key, value string, _ time.Duration) error {
	s.values[key] = value
	s.setKeys = append(s.setKeys, key)
	return nil
}

// testCompleter 造一个固定时钟与固定随机源的补全器。
func testCompleter(store CodexSessionStore) *CodexCompleter {
	return NewCodexCompleter(CodexCompleterOptions{
		Store:  store,
		Logger: logx.New(io.Discard),
		Now:    func() time.Time { return time.UnixMilli(1_700_000_000_000) },
		Rand: func(buffer []byte) error {
			_, err := io.ReadFull(strings.NewReader(strings.Repeat("A", len(buffer))), buffer)
			return err
		},
	})
}

// validSessionID 是一个形状合法的会话标识（UUID v7 形态，长度落在归一规则内）。
const validSessionID = "01a0a2a1-c7ff-7747-81cc-4e27411e8938"

func TestCompleteCodexSessionIdentifiersBothMissing(t *testing.T) {
	store := newFakeCodexStore()
	completer := testCompleter(store)

	result := completer.Complete(context.Background(), CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{},
		Body:    map[string]any{"input": []any{}},
	})

	if !result.Applied {
		t.Fatalf("applied = false，期望 true")
	}
	if result.Action != ActionGeneratedUUIDV7 || result.Source != SourceGeneratedUUIDV7 {
		t.Fatalf("action/source = %s/%s，期望 generated_uuid_v7/generated_uuid_v7", result.Action, result.Source)
	}
	if !result.SetBodyPromptCacheKey || !result.SetHeaderSessionID || !result.SetHeaderXSessionID {
		t.Fatalf("要补的侧不全: %+v", result)
	}
	if !strings.HasPrefix(result.SessionID, "018bcfe5") {
		// 1_700_000_000_000 ms 的 UUID v7 前缀（前 48 位时间戳 = 0x018bcfe56800）。
		t.Fatalf("sessionId = %q，时间戳前缀不符", result.SessionID)
	}
	if store.values[codexFingerprintKeyPrefix+codexFingerprintHash(CodexCompleteArgs{
		KeyID: 7, Headers: map[string][]string{}, Body: map[string]any{"input": []any{}},
	})+codexFingerprintKeySuffix] != result.SessionID {
		t.Fatalf("指纹缓存未落盘: %v", store.values)
	}
}

func TestCompleteCodexSessionIdentifiersHeaderOnly(t *testing.T) {
	store := newFakeCodexStore()
	completer := testCompleter(store)

	result := completer.Complete(context.Background(), CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{"session_id": {validSessionID}},
		Body:    map[string]any{"input": []any{}},
	})

	if result.SessionID != validSessionID || result.Source != SourceHeaderSessionID {
		t.Fatalf("sessionId/source = %s/%s", result.SessionID, result.Source)
	}
	if result.Action != ActionCompletedMissingFields {
		t.Fatalf("action = %s，期望 completed_missing_fields", result.Action)
	}
	if result.SetHeaderSessionID {
		t.Fatalf("session_id 已存在，不该再补")
	}
	if !result.SetHeaderXSessionID || !result.SetBodyPromptCacheKey {
		t.Fatalf("缺 x-session-id 与正文的补写标记: %+v", result)
	}
	if len(store.values) != 0 {
		t.Fatalf("已有标识时不该写指纹缓存: %v", store.values)
	}
}

func TestCompleteCodexSessionIdentifiersCompatibilityHeaderOnly(t *testing.T) {
	completer := testCompleter(newFakeCodexStore())

	result := completer.Complete(context.Background(), CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{"X-Session-Id": {validSessionID}},
		Body:    map[string]any{"input": []any{}},
	})

	// 兼容头满足「有标识」，故源记 header_x_session_id，值提升为主头。
	if result.Source != SourceHeaderXSessionID || result.SessionID != validSessionID {
		t.Fatalf("source/sessionId = %s/%s", result.Source, result.SessionID)
	}
	if !result.SetHeaderSessionID || result.SetHeaderXSessionID {
		t.Fatalf("应只补 session_id: %+v", result)
	}
	if result.Action != ActionCompletedMissingFields {
		t.Fatalf("action = %s", result.Action)
	}
}

func TestCompleteCodexSessionIdentifiersBodyOnly(t *testing.T) {
	completer := testCompleter(newFakeCodexStore())

	result := completer.Complete(context.Background(), CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{},
		Body:    map[string]any{"input": []any{}, "prompt_cache_key": validSessionID},
	})

	if result.Source != SourceBodyPromptCacheKey || result.SessionID != validSessionID {
		t.Fatalf("source/sessionId = %s/%s", result.Source, result.SessionID)
	}
	if result.SetBodyPromptCacheKey {
		t.Fatalf("正文已有 prompt_cache_key，不该再补")
	}
	if !result.SetHeaderSessionID || !result.SetHeaderXSessionID {
		t.Fatalf("两个头都该补: %+v", result)
	}
}

func TestCompleteCodexSessionIdentifiersBothPresentIsIdempotent(t *testing.T) {
	store := newFakeCodexStore()
	completer := testCompleter(store)

	result := completer.Complete(context.Background(), CodexCompletionArgsWithCacheKey(validSessionID))

	if result.Applied {
		t.Fatalf("两侧齐全时 applied 应为 false")
	}
	if result.Action != ActionNone {
		t.Fatalf("action = %s，期望 none", result.Action)
	}
	if result.SetHeaderSessionID || result.SetHeaderXSessionID || result.SetBodyPromptCacheKey {
		// Node 的提前返回**不补** x-session-id，这是既有行为。
		t.Fatalf("两侧齐全时不该补任何侧: %+v", result)
	}
	if len(store.values) != 0 {
		t.Fatalf("两侧齐全时不该碰 Redis: %v", store.values)
	}
}

// CodexCompletionArgsWithCacheKey 造「session_id 头 + 正文 prompt_cache_key 都在」的入参。
func CodexCompletionArgsWithCacheKey(sessionID string) CodexCompleteArgs {
	return CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{"session_id": {sessionID}},
		Body:    map[string]any{"input": []any{}, "prompt_cache_key": sessionID},
	}
}

func TestCompleteCodexSessionIdentifiersReusesFingerprintCache(t *testing.T) {
	store := newFakeCodexStore()
	completer := testCompleter(store)
	args := CodexCompleteArgs{KeyID: 7, Headers: map[string][]string{}, Body: map[string]any{"input": []any{}}}
	key := codexFingerprintKeyPrefix + codexFingerprintHash(args) + codexFingerprintKeySuffix
	store.values[key] = validSessionID

	result := completer.Complete(context.Background(), args)

	if result.SessionID != validSessionID {
		t.Fatalf("sessionId = %q，期望复用缓存值", result.SessionID)
	}
	if result.Source != SourceFingerprintCache || result.Action != ActionReusedFingerprintCache {
		t.Fatalf("source/action = %s/%s", result.Source, result.Action)
	}
}

func TestCompleteCodexSessionIdentifiersFingerprintKeyVaries(t *testing.T) {
	base := CodexCompleteArgs{KeyID: 7, Headers: map[string][]string{}, Body: map[string]any{"input": []any{}}}
	sameAgain := CodexCompleteArgs{KeyID: 7, Headers: map[string][]string{}, Body: map[string]any{"input": []any{}}}
	otherKey := CodexCompleteArgs{KeyID: 8, Headers: map[string][]string{}, Body: map[string]any{"input": []any{}}}
	otherAgent := CodexCompleteArgs{
		KeyID: 7, Headers: map[string][]string{}, Body: map[string]any{"input": []any{}}, UserAgent: "codex-cli/1.0",
	}

	if codexFingerprintHash(base) != codexFingerprintHash(sameAgain) {
		t.Fatalf("同参指纹应稳定")
	}
	if codexFingerprintHash(base) == codexFingerprintHash(otherKey) {
		t.Fatalf("keyId 变化应改变指纹")
	}
	if codexFingerprintHash(base) == codexFingerprintHash(otherAgent) {
		t.Fatalf("UA 变化应改变指纹")
	}
}

func TestCompleteCodexSessionIdentifiersFingerprintUsesForwardedIPAndUAMessage(t *testing.T) {
	// UA 从请求头兜底（Node 的 `args.userAgent ?? headers.get("user-agent")`）。
	fromHeader := CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{"user-agent": {"codex-cli/1.0"}, "x-forwarded-for": {"203.0.113.9, 198.51.100.7"}},
		Body:    map[string]any{"input": []any{}},
	}
	explicit := CodexCompleteArgs{
		KeyID:     7,
		Headers:   map[string][]string{"x-forwarded-for": {"203.0.113.9, 198.51.100.7"}},
		Body:      map[string]any{"input": []any{}},
		UserAgent: "codex-cli/1.0",
	}
	if codexFingerprintHash(fromHeader) != codexFingerprintHash(explicit) {
		t.Fatalf("头里取到的 UA 应与显式 UA 同指纹")
	}
	// 首条消息文本参与指纹：前 3 条 message 的 content 文本。
	withMessage := CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{"x-forwarded-for": {"203.0.113.9, 198.51.100.7"}},
		Body: map[string]any{
			"input": []any{
				map[string]any{"type": "message", "content": "第一轮提问"},
				map[string]any{"type": "reasoning", "content": "不应参与"},
			},
		},
		UserAgent: "codex-cli/1.0",
	}
	if codexFingerprintHash(explicit) == codexFingerprintHash(withMessage) {
		t.Fatalf("消息文本变化应改变指纹")
	}
}

func TestCompleteCodexSessionIdentifiersExtractsXRealIPFallback(t *testing.T) {
	fromForwarded := CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{"x-forwarded-for": {"203.0.113.9"}},
		Body:    map[string]any{"input": []any{}},
	}
	fromRealIP := CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{"x-real-ip": {"203.0.113.9"}},
		Body:    map[string]any{"input": []any{}},
	}
	if codexFingerprintHash(fromForwarded) != codexFingerprintHash(fromRealIP) {
		t.Fatalf("两个 IP 头指向同一地址时应同指纹")
	}
}

func TestCompleteCodexSessionIdentifiersEmptyForwardedForFallsBackToRealIP(t *testing.T) {
	blankForwarded := CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{"x-forwarded-for": {" , "}, "x-real-ip": {"203.0.113.9"}},
		Body:    map[string]any{"input": []any{}},
	}
	fromRealIP := CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{"x-real-ip": {"203.0.113.9"}},
		Body:    map[string]any{"input": []any{}},
	}
	if codexFingerprintHash(blankForwarded) != codexFingerprintHash(fromRealIP) {
		t.Fatalf("空的首段 xff 应退到 x-real-ip")
	}
}

func TestCompleteCodexSessionIdentifiersNXRaceReusesWinner(t *testing.T) {
	// 第一次 Get 空（缓存未命中）→ set NX 失败（别人写进去了）→ 回读拿到赢家。
	store := &fakeCodexStore{values: map[string]string{}, nxFails: true, getSequence: []string{"", validSessionID}}
	completer := testCompleter(store)

	result := completer.Complete(context.Background(), CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{},
		Body:    map[string]any{"input": []any{}},
	})

	if result.SessionID != validSessionID || result.Source != SourceFingerprintCache {
		t.Fatalf("竞争失败后应复用赢家: %+v", result)
	}
}

func TestCompleteCodexSessionIdentifiersNXRaceOverwritesWhenWinnerMissing(t *testing.T) {
	// 竞争失败且回读仍为空：Node 直接覆写并返回自己生成的候选值。
	store := &fakeCodexStore{values: map[string]string{}, nxFails: true, getSequence: []string{"", ""}}
	completer := testCompleter(store)

	result := completer.Complete(context.Background(), CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{},
		Body:    map[string]any{"input": []any{}},
	})

	if result.Source != SourceGeneratedUUIDV7 || result.Action != ActionGeneratedUUIDV7 {
		t.Fatalf("source/action = %s/%s", result.Source, result.Action)
	}
	if len(store.setKeys) != 1 {
		t.Fatalf("应发生一次覆写写入，实际 %d 次", len(store.setKeys))
	}
	if store.values[store.setKeys[0]] != result.SessionID {
		t.Fatalf("覆写值应与返回值一致")
	}
}

func TestCompleteCodexSessionIdentifiersRedisUnavailableFallsBackToFreshUUID(t *testing.T) {
	completer := testCompleter(nil)

	result := completer.Complete(context.Background(), CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{},
		Body:    map[string]any{"input": []any{}},
	})

	if result.Action != ActionGeneratedUUIDV7 || result.SessionID == "" {
		t.Fatalf("Redis 未接线时应降级生成: %+v", result)
	}
}

func TestCompleteCodexSessionIdentifiersRedisErrorFallsBackToFreshUUID(t *testing.T) {
	completer := testCompleter(&fakeCodexStore{values: map[string]string{}, getErr: errors.New("connection refused")})

	result := completer.Complete(context.Background(), CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{},
		Body:    map[string]any{"input": []any{}},
	})

	if result.Action != ActionGeneratedUUIDV7 || result.SessionID == "" {
		t.Fatalf("Redis 报错时应降级生成: %+v", result)
	}
}

func TestCompleteCodexSessionIdentifiersIgnoresInvalidIdentifiers(t *testing.T) {
	// 太短的标识按归一规则作废，等价于「缺失」。
	completer := testCompleter(newFakeCodexStore())

	result := completer.Complete(context.Background(), CodexCompleteArgs{
		KeyID:   7,
		Headers: map[string][]string{"session_id": {"short"}},
		Body:    map[string]any{"input": []any{}, "prompt_cache_key": 42},
	})

	if result.SessionID == "short" || result.Source != SourceGeneratedUUIDV7 {
		t.Fatalf("非法标识应被忽略: %+v", result)
	}
	if !result.SetBodyPromptCacheKey || !result.SetHeaderSessionID {
		t.Fatalf("非法标识视同缺失，两侧都该补: %+v", result)
	}
}

func TestGenerateUUIDV7ShapeAndDeterminism(t *testing.T) {
	now := func() time.Time { return time.UnixMilli(1_700_000_000_000) }
	random := func(buffer []byte) error {
		for index := range buffer {
			buffer[index] = 0xAB
		}
		return nil
	}

	first := GenerateUUIDV7(now, random)
	second := GenerateUUIDV7(now, random)

	if first != second {
		t.Fatalf("同时钟同随机源应产出同值: %s vs %s", first, second)
	}
	if len(first) != 36 || first[8] != '-' || first[13] != '-' || first[18] != '-' || first[23] != '-' {
		t.Fatalf("形状不对: %q", first)
	}
	if first[14] != '7' {
		t.Fatalf("版本位应为 7: %q", first)
	}
	if !strings.Contains("89ab", string(first[19])) {
		t.Fatalf("变体位应为 8/9/a/b: %q", first)
	}
	// 时间戳：0x0000018bcfe56800 → 前 8 位十六进制 = 018bcfe5，随后 4 位 = 6800。
	if first[:8] != "018bcfe5" || first[9:13] != "6800" {
		t.Fatalf("时间戳编码不符: %q", first)
	}
}
