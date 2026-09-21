package session

import (
	"context"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
)

// 本文件钉住「会话包如实报告身份的来源」。
//
// 为什么必须单独钉：前缀兜底层按来源判定是否参与（见 guard.SessionIdentitySource 的说明），
// 而三条分支都在 Ensure 里。某条分支忘了标，前缀兜底对该类客户端就静默失效——无报错、
// 无日志，只表现为「不带 session id 的客户端每次换渠道」。
//
// 真 Redis + 真脚本表（CCH_TEST_REDIS_URL，未设则跳过，与同目录其余集成用例一致）。

// TestEnsureReportsClientIdentityWhenClientSendsID 客户端显式携带 id ⇒ Client。
func TestEnsureReportsClientIdentityWhenClientSendsID(t *testing.T) {
	rdb := testRedis(t)
	adapter := NewSessionBinderAdapter(BinderOptions{
		Client: newTestBinder(t, rdb),
		TTL:    time.Duration(testTTLSeconds) * time.Second,
	})

	result, err := adapter.Ensure(context.Background(), guard.SessionRequest{
		KeyID: testKeyID,
		Body: map[string]any{
			"metadata": map[string]any{"session_id": "sess_client_explicit"},
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		},
	})
	if err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	if result.SessionID != "sess_client_explicit" {
		t.Fatalf("应复用客户端携带的 id，实际 %q", result.SessionID)
	}
	if result.IdentitySource != guard.SessionIdentityClient {
		t.Errorf("IdentitySource = %v，期望 Client", result.IdentitySource)
	}
}

// TestEnsureReportsGeneratedThenRecoveredIdentity 主线（curl 形态两轮）：
// 第一轮客户端不带 id ⇒ Generated；第二轮正文相同 ⇒ 按哈希找回 ⇒ Recovered。
//
// 两条分支都属「客户端身份缺失」，故前缀兜底层对它们一视同仁——这正是设计稿 §2 的兜底语义。
func TestEnsureReportsGeneratedThenRecoveredIdentity(t *testing.T) {
	rdb := testRedis(t)
	adapter := NewSessionBinderAdapter(BinderOptions{
		Client: newTestBinder(t, rdb),
		TTL:    time.Duration(testTTLSeconds) * time.Second,
	})
	body := func() map[string]any {
		return map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "curl 形态的请求"}},
		}
	}
	// 先清掉本用例可能残留的「正文哈希 → 会话」映射。
	//
	// 不清的话用例**不幂等**：上一次运行（或本包内另一个同 keyID、同正文的用例）留下的映射
	// 会被第二轮之前的调用命中，于是第一轮就判为 Recovered，本用例单跑绿、整包跑红。
	ctx := context.Background()
	hash := CalculateMessagesHash(body()["messages"])
	if hash != "" {
		mappingKey := TenantContentHashSessionKey(testKeyID, hash)
		_ = rdb.Del(ctx, mappingKey).Err()
		t.Cleanup(func() { _ = rdb.Del(ctx, mappingKey).Err() })
	}

	first, err := adapter.Ensure(context.Background(), guard.SessionRequest{KeyID: testKeyID, Body: body()})
	if err != nil {
		t.Fatalf("第一轮 Ensure 失败: %v", err)
	}
	if first.IdentitySource != guard.SessionIdentityGenerated {
		t.Fatalf("第一轮 IdentitySource = %v，期望 Generated", first.IdentitySource)
	}
	if first.SessionID == "" {
		t.Fatal("第一轮应生成会话 id（否则后续轮次无从复用）")
	}

	second, err := adapter.Ensure(context.Background(), guard.SessionRequest{KeyID: testKeyID, Body: body()})
	if err != nil {
		t.Fatalf("第二轮 Ensure 失败: %v", err)
	}
	if second.IdentitySource != guard.SessionIdentityRecovered {
		t.Fatalf("第二轮 IdentitySource = %v，期望 Recovered（正文哈希应找回第一轮的会话）", second.IdentitySource)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("第二轮应复用第一轮的会话 %q，实际 %q", first.SessionID, second.SessionID)
	}
}
