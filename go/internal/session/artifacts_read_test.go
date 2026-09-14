package session

import (
	"context"
	"testing"
)

// 本文件钉住会话工件的**读侧与所有者围栏**（写侧在 artifacts.go / artifacts_read.go）。
//
// 为什么必须用真 Redis：围栏的判据是「键里存的那个字符串等于 locator 给的 keyId」，
// 而键名、TTL 与旧格式回退都只在真键空间里能验。桩件只能证明「调了 GET」。

// TestIntegrationSessionRequestOwnerFence 断言所有者围栏的三态：匹配、不匹配、缺失。
func TestIntegrationSessionRequestOwnerFence(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	ctx := context.Background()

	// 未写过：任何 keyId 都不算拥有（这正是「工件不存在」的判据）。
	if binder.IsSessionRequestOwnedByKey(ctx, sessionID, 1, testKeyID) {
		t.Fatal("未写所有者键时不应通过围栏")
	}

	if err := binder.StoreSessionRequestOwner(ctx, sessionID, 1, testKeyID); err != nil {
		t.Fatalf("写所有者键失败: %v", err)
	}
	// 键里存的必须是**裸的十进制 keyId**：读侧按字符串比对。
	stored, err := rdb.Get(ctx, SessionRequestOwnerKey(sessionID, 1)).Result()
	if err != nil {
		t.Fatalf("读所有者键失败: %v", err)
	}
	if stored != "42" {
		t.Fatalf("所有者键应为 \"42\"，收到 %q", stored)
	}
	if !binder.IsSessionRequestOwnedByKey(ctx, sessionID, 1, testKeyID) {
		t.Fatal("同一 keyId 应通过围栏")
	}
	if binder.IsSessionRequestOwnedByKey(ctx, sessionID, 1, testKeyID+1) {
		t.Fatal("别的 keyId 不应通过围栏")
	}
	// 另一个序号是另一份工件：围栏按 (session, sequence) 隔离。
	if binder.IsSessionRequestOwnedByKey(ctx, sessionID, 2, testKeyID) {
		t.Fatal("序号 2 未写过，不应通过围栏")
	}
	// keyID<=0 不写（Node 的 keyId === undefined）——写 0 会让所有比对失效。
	if err := binder.StoreSessionRequestOwner(ctx, sessionID, 3, 0); err != nil {
		t.Fatalf("keyId 为 0 时应静默跳过，收到错误 %v", err)
	}
	if rdb.Exists(ctx, SessionRequestOwnerKey(sessionID, 3)).Val() != 0 {
		t.Fatal("keyId 为 0 不应写出所有者键")
	}
}

// TestIntegrationSessionMessagesReadAndExistence 断言 messages 读数与存在性检查。
func TestIntegrationSessionMessagesReadAndExistence(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	ctx := context.Background()

	// 无工件：不存在，且不是错误。
	if _, found, err := binder.SessionMessages(ctx, sessionID, 1); err != nil || found {
		t.Fatalf("无工件时应 (false, nil)，收到 (%v, %v)", found, err)
	}
	if binder.HasAnySessionMessages(ctx, sessionID) {
		t.Fatal("无工件时 HasAnySessionMessages 应为 false")
	}
	// 序号非正：按 Node 的 normalizeRequestSequence 判 null，直接无工件（不回退旧格式键）。
	if _, found, _ := binder.SessionMessages(ctx, sessionID, 0); found {
		t.Fatal("序号 0 不应命中任何工件")
	}

	messages := []any{map[string]any{"role": "user", "content": "[REDACTED]"}}
	if err := binder.StoreSessionMessages(ctx, sessionID, messages, 1, SessionArtifactOptions{
		StoreMessages: false, MaxBytes: 1024,
	}); err != nil {
		t.Fatalf("写 messages 工件失败: %v", err)
	}

	value, found, err := binder.SessionMessages(ctx, sessionID, 1)
	if err != nil || !found {
		t.Fatalf("应读到 messages，收到 (found=%v, err=%v)", found, err)
	}
	items, ok := value.([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("messages 应解成数组，收到 %#v", value)
	}
	// 结构保留、内容脱敏（StoreMessages=false）。
	first, _ := items[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "[REDACTED]" {
		t.Fatalf("脱敏后应保留角色与结构，收到 %#v", first)
	}
	if !binder.HasAnySessionMessages(ctx, sessionID) {
		t.Fatal("写入后 HasAnySessionMessages 应为 true")
	}

	// 旧格式键也算「有 messages」（升级前的数据不能被答成没有）。
	if err := rdb.Set(ctx, LegacyMessagesKey(sessionID), `[{"role":"user"}]`, 60).Err(); err != nil {
		t.Fatalf("写旧格式键失败: %v", err)
	}
	if !binder.HasAnySessionMessages(ctx, sessionID) {
		t.Fatal("旧格式键应被存在性检查认到")
	}

	// 上限超限即删键（StoreSessionMessages 的既有语义）——读侧随之落空。
	if err := binder.StoreSessionMessages(ctx, sessionID, messages, 1, SessionArtifactOptions{
		StoreMessages: true, MaxBytes: 4,
	}); err != nil {
		t.Fatalf("超限写入应静默删键，收到错误 %v", err)
	}
	if _, found, _ := binder.SessionMessages(ctx, sessionID, 1); found {
		t.Fatal("超限后应按不存在作答（键已删）")
	}
}
