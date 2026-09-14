package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 本文件钉住会话观测**写侧**的键形制与 TTL 口径（读侧在 observed_read.go 有各自的钉子）。
//
// 这四个断言对应数据面接线前的三条缺口：不写 info ⇒ 会话列表把会话判成「已终止」；
// 不写观测 ZSET ⇒ 列表没有 id 可读；不写并发计数 ⇒ 并发数恒 0。

// TestIntegrationStoreSessionInfoWritesHashAndTTL 断言 info Hash 的字段集与 TTL。
func TestIntegrationStoreSessionInfoWritesHashAndTTL(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	ctx := context.Background()

	identity := PublicSessionIdentity(sessionID, testKeyID)
	if err := binder.StoreSessionInfo(ctx, identity, SessionInfo{
		UserName: "测试用户",
		UserID:   7,
		KeyID:    testKeyID,
		KeyName:  "测试密钥",
		Model:    "claude-sonnet-4-5",
		APIType:  "chat",
	}); err != nil {
		t.Fatalf("写会话 info 失败: %v", err)
	}

	values, err := rdb.HGetAll(ctx, InfoKey(identity)).Result()
	if err != nil {
		t.Fatalf("读会话 info 失败: %v", err)
	}
	// 逐字段对齐 Node 的 storeSessionInfo：少一个字段都会让管理面显示成 "unknown"。
	want := map[string]string{
		"userName": "测试用户",
		"userId":   "7",
		"keyId":    "42",
		"keyName":  "测试密钥",
		"model":    "claude-sonnet-4-5",
		"apiType":  "chat",
		"status":   "in_progress",
	}
	for field, expected := range want {
		if values[field] != expected {
			t.Errorf("info.%s 应为 %q，收到 %q", field, expected, values[field])
		}
	}
	if values["startTime"] == "" || values["startTime"] == "0" {
		t.Errorf("startTime 应为毫秒时间戳，收到 %q", values["startTime"])
	}
	assertTTLWithin(t, rdb, InfoKey(identity), DefaultBindingTTLSeconds)

	// provider 字段在选中供应商之后才补写：写前不存在，写后必须能读到。
	if _, ok := values["providerId"]; ok {
		t.Error("providerId 不应在 storeSessionInfo 阶段出现")
	}
	if err := binder.UpdateSessionProvider(ctx, identity, 99, "测试供应商"); err != nil {
		t.Fatalf("写会话供应商失败: %v", err)
	}
	updated, err := rdb.HGetAll(ctx, InfoKey(identity)).Result()
	if err != nil {
		t.Fatalf("读会话 info 失败: %v", err)
	}
	if updated["providerId"] != "99" || updated["providerName"] != "测试供应商" {
		t.Errorf("provider 字段应写成 99/测试供应商，收到 %q/%q", updated["providerId"], updated["providerName"])
	}
	if updated["status"] != "in_progress" {
		t.Errorf("写供应商不得改动 status，收到 %q", updated["status"])
	}

	// 终态状态覆盖 info.status 并刷新 TTL。
	if err := binder.SetSessionStatus(ctx, identity, "completed"); err != nil {
		t.Fatalf("写会话终态失败: %v", err)
	}
	status, err := rdb.HGet(ctx, InfoKey(identity), "status").Result()
	if err != nil {
		t.Fatalf("读 status 失败: %v", err)
	}
	if status != "completed" {
		t.Errorf("status 应为 completed，收到 %q", status)
	}
}

// TestIntegrationTrackSessionAndObservedFillsBothZSets 断言两组活跃 ZSET 都被写上。
//
// 全局/密钥/用户三条是「物理会话」维度（限流与展示共用），观测那条是「会话身份」维度
// （列表页的 id 来源）。少写任一组，对应的读面就会静默变空。
func TestIntegrationTrackSessionAndObservedFillsBothZSets(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	ctx := context.Background()

	identity := PublicSessionIdentity(sessionID, testKeyID)
	if err := binder.TrackSessionAndObserved(ctx, sessionID, testKeyID, 7, identity); err != nil {
		t.Fatalf("跟踪会话失败: %v", err)
	}
	// 读面第 3 步会逐个 EXISTS session:{id}:info，只留仍在的成员——故「列表可见」
	// 这件事故意需要 info 一起存在（这也是写侧两条必须同时接的原因）。
	if err := binder.StoreSessionInfo(ctx, identity, SessionInfo{
		UserName: "u", UserID: 7, KeyID: testKeyID, APIType: "chat",
	}); err != nil {
		t.Fatalf("写会话 info 失败: %v", err)
	}

	for _, key := range []string{
		ActiveSessionsGlobalKey(),
		KeyActiveSessionsKey(testKeyID),
		UserActiveSessionsKey(7),
		ObservedGlobalActiveSessionsKey(),
	} {
		score, err := rdb.ZScore(ctx, key, sessionID).Result()
		if err != nil {
			t.Fatalf("ZSET %s 应含成员 %s: %v", key, sessionID, err)
		}
		if score <= 0 {
			t.Errorf("ZSET %s 的 score 应为毫秒时间戳，收到 %v", key, score)
		}
	}
	// 观测 ZSET 的成员是**身份**，不是物理 id；两者在当前用例里同名，故单独再断言一次。
	if _, err := rdb.ZScore(ctx, ObservedGlobalActiveSessionsKey(), identity).Result(); err != nil {
		t.Fatalf("观测 ZSET 应含身份 %s: %v", identity, err)
	}

	// 观测读面必须能看见这个会话（这正是 GET /sessions 的 id 来源）。
	active, err := binder.ObservedActiveSessions(ctx)
	if err != nil {
		t.Fatalf("读观测集合失败: %v", err)
	}
	if !containsValue(active, identity) {
		t.Fatalf("观测集合应含 %s，收到 %v", identity, active)
	}
}

// TestIntegrationConcurrentCountsIncrementAndRelease 断言两个并发计数的自增/自减与归零删键。
func TestIntegrationConcurrentCountsIncrementAndRelease(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	ctx := context.Background()

	identity := PublicSessionIdentity(sessionID, testKeyID)
	if err := binder.IncrementObservedConcurrentCount(ctx, identity); err != nil {
		t.Fatalf("观测计数自增失败: %v", err)
	}
	if err := binder.IncrementConcurrentCount(ctx, sessionID); err != nil {
		t.Fatalf("物理计数自增失败: %v", err)
	}

	counts, err := binder.ObservedConcurrentCounts(ctx, []string{identity})
	if err != nil {
		t.Fatalf("读观测计数失败: %v", err)
	}
	if counts[identity] != 1 {
		t.Fatalf("观测并发数应为 1，收到 %v", counts[identity])
	}
	assertTTLWithin(t, rdb, ObservedConcurrentCountKey(identity), int(observedConcurrentCountTTL/time.Second))

	// 降到 0 必须删键：留着 0 会让「已收尾」与「并发 0」在键空间上无法区分。
	if err := binder.DecrementObservedConcurrentCount(ctx, identity); err != nil {
		t.Fatalf("观测计数自减失败: %v", err)
	}
	if err := binder.DecrementConcurrentCount(ctx, sessionID); err != nil {
		t.Fatalf("物理计数自减失败: %v", err)
	}
	for _, key := range []string{ObservedConcurrentCountKey(identity), ConcurrentCountKey(sessionID)} {
		exists, err := rdb.Exists(ctx, key).Result()
		if err != nil {
			t.Fatalf("查键失败: %v", err)
		}
		if exists != 0 {
			t.Errorf("计数归零后 %s 应被删除", key)
		}
	}
}

// TestIntegrationRequestArtifactsHonorSwitchAndLimit 断言请求工件的开关、上限与脱敏。
func TestIntegrationRequestArtifactsHonorSwitchAndLimit(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	ctx := context.Background()

	body := map[string]any{
		"model":    "claude-sonnet-4-5",
		"system":   "你是助手",
		"messages": []any{map[string]any{"role": "user", "content": "机密问题"}},
	}
	raw := SessionArtifactOptions{StoreMessages: true, MaxBytes: 1 << 20}

	// 原样档：正文逐字落盘。
	if err := binder.StoreSessionRequestBody(ctx, sessionID, body, 3, raw); err != nil {
		t.Fatalf("写请求正文失败: %v", err)
	}
	stored, err := rdb.Get(ctx, RequestBodyKey(sessionID, 3)).Result()
	if err != nil {
		t.Fatalf("读请求正文失败: %v", err)
	}
	if !strings.Contains(stored, "机密问题") || !strings.Contains(stored, "你是助手") {
		t.Errorf("STORE_SESSION_MESSAGES=true 应原样落盘，收到 %q", stored)
	}
	assertTTLWithin(t, rdb, RequestBodyKey(sessionID, 3), DefaultBindingTTLSeconds)

	// 脱敏档：内容字段换 [REDACTED]，结构保留（模型名与角色仍可读）。
	if err := binder.StoreSessionRequestBody(ctx, sessionID, body, 4, SessionArtifactOptions{MaxBytes: 1 << 20}); err != nil {
		t.Fatalf("写脱敏正文失败: %v", err)
	}
	redacted, err := rdb.Get(ctx, RequestBodyKey(sessionID, 4)).Result()
	if err != nil {
		t.Fatalf("读脱敏正文失败: %v", err)
	}
	if strings.Contains(redacted, "机密问题") || strings.Contains(redacted, "你是助手") {
		t.Errorf("STORE_SESSION_MESSAGES=false 不得留下正文原文，收到 %q", redacted)
	}
	if !strings.Contains(redacted, redactedMarker) {
		t.Errorf("脱敏档应写入 %s，收到 %q", redactedMarker, redacted)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(redacted), &decoded); err != nil {
		t.Fatalf("脱敏后仍应是合法 JSON: %v", err)
	}
	if decoded["model"] != "claude-sonnet-4-5" {
		t.Errorf("脱敏不得改动非内容字段，model 收到 %v", decoded["model"])
	}
	messages, _ := decoded["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("脱敏不得改动数组长度，收到 %v", decoded["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["content"] != redactedMarker || first["role"] != "user" {
		t.Errorf("消息应保留 role 并脱敏 content，收到 %v", first)
	}

	// 上限档：超限删键（不是跳过）。先写一份合法工件，再用超限配置覆盖，
	// 断言旧工件被清掉——否则读侧会把两次请求的正文串起来。
	if err := binder.StoreSessionMessages(ctx, sessionID, body["messages"], 5, raw); err != nil {
		t.Fatalf("写消息工件失败: %v", err)
	}
	if exists, _ := rdb.Exists(ctx, MessagesSequenceKey(sessionID, 5)).Result(); exists != 1 {
		t.Fatal("合法大小的消息工件应落盘")
	}
	if err := binder.StoreSessionMessages(ctx, sessionID, body["messages"], 5,
		SessionArtifactOptions{MaxBytes: 8}); err != nil {
		t.Fatalf("超限写消息工件不应报错: %v", err)
	}
	if exists, _ := rdb.Exists(ctx, MessagesSequenceKey(sessionID, 5)).Result(); exists != 0 {
		t.Error("超限工件必须删除键，而不是留下上一次的旧工件")
	}
}

// TestIntegrationTerminateObservedSessionClearsObservation 断言终止动作清掉观测三件套。
//
// 这是「终止后全净」的写侧版本：只摘 ZSET 而不删计数与 info，会让列表页继续显示一个
// 已终止的会话（读面第 3 步的 EXISTS 才会把它滤掉，但并发计数会残留）。
func TestIntegrationTerminateObservedSessionClearsObservation(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	ctx := context.Background()

	identity := PublicSessionIdentity(sessionID, testKeyID)
	if err := binder.TrackSessionAndObserved(ctx, sessionID, testKeyID, 0, identity); err != nil {
		t.Fatalf("跟踪会话失败: %v", err)
	}
	if err := binder.StoreSessionInfo(ctx, identity, SessionInfo{UserName: "u", UserID: 1, KeyID: testKeyID, APIType: "chat"}); err != nil {
		t.Fatalf("写会话 info 失败: %v", err)
	}
	if err := binder.IncrementObservedConcurrentCount(ctx, identity); err != nil {
		t.Fatalf("观测计数自增失败: %v", err)
	}

	deleted, err := binder.TerminateObservedSession(ctx, identity)
	if err != nil {
		t.Fatalf("终止观测会话失败: %v", err)
	}
	if !deleted {
		t.Fatal("终止观测会话应报告「真的删掉了东西」")
	}
	for _, key := range []string{InfoKey(identity), ObservedConcurrentCountKey(identity)} {
		if exists, _ := rdb.Exists(ctx, key).Result(); exists != 0 {
			t.Errorf("终止后 %s 应被删除", key)
		}
	}
	active, err := binder.ObservedActiveSessions(ctx)
	if err != nil {
		t.Fatalf("读观测集合失败: %v", err)
	}
	if containsValue(active, identity) {
		t.Errorf("终止后观测集合不得再含 %s", identity)
	}
}

// TestPublicSessionIdentityEscapesReservedPrefixes 钉住保留前缀的 key-bound 编码。
//
// 普通 id 原样（展示与查询语义）；pfx:/sid: 前缀做 sha256 截断，避免客户端把自己
// 写进亲和命名空间或用超长 id 撑爆 identity 列。
func TestPublicSessionIdentityEscapesReservedPrefixes(t *testing.T) {
	plain := "sess_abcdef123456"
	if got := PublicSessionIdentity(plain, 42); got != plain {
		t.Errorf("普通会话 id 应原样返回，收到 %q", got)
	}
	if got := PublicSessionIdentity("", 42); got != "" {
		t.Errorf("空 id 应返回空，收到 %q", got)
	}

	escaped := PublicSessionIdentity("pfx:scope:deadbeef", 42)
	if !strings.HasPrefix(escaped, "sid:") {
		t.Fatalf("保留前缀应编码成 sid: 命名空间，收到 %q", escaped)
	}
	if len(escaped) != len("sid:")+32 {
		t.Errorf("编码后应为 sid: + 32 hex，收到 %q（长度 %d）", escaped, len(escaped))
	}
	if again := PublicSessionIdentity("pfx:scope:deadbeef", 42); again != escaped {
		t.Errorf("同一 (id, keyId) 的编码必须稳定，收到 %q / %q", escaped, again)
	}
	if other := PublicSessionIdentity("pfx:scope:deadbeef", 43); other == escaped {
		t.Error("不同 keyId 必须得到不同编码（跨租户不得 alias）")
	}
	if sid := PublicSessionIdentity("sid:whatever", 42); !strings.HasPrefix(sid, "sid:") || sid == "sid:whatever" {
		t.Errorf("sid: 前缀同样必须再编码，收到 %q", sid)
	}
}

// containsValue 是字符串切片包含判定（测试内联，避免为一个断言引入依赖）。
func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
