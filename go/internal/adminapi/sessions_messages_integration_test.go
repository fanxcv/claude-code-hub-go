package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
)

// 本文件是两条**消息面**端点（GET /sessions/{id}/messages 与 /messages/exists）的真依赖验收，
// 按「**写侧 → 读侧 → 端点**」的闭环断言，而不是拿手写键去喂读侧。
//
// 为什么闭环必须穿过真实的写侧函数：这两条端点的全部语义都在「工件在不在、是不是你的」上。
// 手写键只能证明读侧会读某个键名；让写入走 `session.Binder`（数据面调的就是这一组函数）
// 才能同时证明键名、值形制、所有者围栏与 TTL 口径在两侧一致——键名写错时端点会恒 404，
// 而手写键的测试会一路绿灯。

// seedMessagesArtifact 用**生产写侧**落一份 messages 工件与它的所有者键。
//
// 顺序照数据面：先写所有者键（围栏），再写工件；两者用同一个 (sessionID, sequence, keyID)。
func seedMessagesArtifact(
	t *testing.T,
	binder *session.Binder,
	sessionID string,
	sequence int,
	keyID int64,
	messages any,
) {
	t.Helper()
	ctx := context.Background()
	if err := binder.StoreSessionRequestOwner(ctx, sessionID, sequence, keyID); err != nil {
		t.Fatalf("写所有者键失败: %v", err)
	}
	if err := binder.StoreSessionMessages(ctx, sessionID, messages, sequence, session.SessionArtifactOptions{StoreMessages: false, MaxBytes: 1024}); err != nil {
		t.Fatalf("写 messages 工件失败: %v", err)
	}
}

// TestSessionMessagesEndpointsClosedLoop 钉住消息面两条端点的闭环与围栏。
func TestSessionMessagesEndpointsClosedLoop(t *testing.T) {
	deps := openSessionsDeps(t)
	pools := deps.Pools
	ctx := context.Background()

	providerID := fixtureProviderID(t, pools)
	ownerID := fixtureUser(t, pools, "user", true)
	otherID := fixtureUser(t, pools, "user", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: ownerID, canLoginWebUI: true, isEnabled: true,
	})

	identity := fmt.Sprintf("sess-messages-%d", time.Now().UnixNano())
	seedOriginChainRows(t, pools, ownerID, keyValue, identity, providerID)

	// 两份工件分落在**两个序号**上：序号 3 是夹具里最新的一行（不带选择器时定位器会选中它），
	// 序号 1 用来验证「显式指定序号」这条路径。序号 2 刻意不写（用来验「未写过的序号」）。
	latest := []any{
		map[string]any{"role": "user", "content": "[REDACTED]"},
		map[string]any{"role": "assistant", "content": "[REDACTED]"},
	}
	seeded := []any{map[string]any{"role": "user", "content": "[REDACTED]"}}
	seedMessagesArtifact(t, deps.Binder, identity, 3, keyID, latest)
	seedMessagesArtifact(t, deps.Binder, identity, 1, keyID, seeded)
	t.Cleanup(func() {
		raw := deps.Redis
		for _, sequence := range []int{1, 3} {
			_ = raw.Del(ctx, session.SessionRequestOwnerKey(identity, sequence)).Err()
			_ = raw.Del(ctx, session.MessagesSequenceKey(identity, sequence)).Err()
		}
	})

	ownerRouter := sessionsRouterWithDeps(t, pools,
		Principal{UserID: ownerID, IsAdmin: false}, deps)

	// 1. 闭环：数据面写侧落下的工件被端点读出来，且**结构保留、内容脱敏**。
	status, _, raw := sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/messages", "")
	if status != http.StatusOK {
		t.Fatalf("应 200，实得 %d（正文 %s）", status, raw)
	}
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("messages 应是数组，实得 %s（%v）", raw, err)
	}
	if len(decoded) != 2 || decoded[0]["role"] != "user" {
		t.Fatalf("messages 内容不符，实得 %s", raw)
	}

	// 2. 序号指定：命中同一条；未写过的序号答 404（工件不存在与已过 TTL 同判）。
	status, _, _ = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/messages?requestSequence=1", "")
	if status != http.StatusOK {
		t.Fatalf("指定序号 1 应 200，实得 %d", status)
	}
	status, problem, _ := sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/messages?requestSequence=2", "")
	if status != http.StatusNotFound || problem["errorCode"] != "session.not_found" {
		t.Fatalf("未写过的序号应 404 session.not_found，实得 %d %v", status, problem)
	}

	// 3. 存在性：有工件 → true；换其它身份 → 会话都看不见（404 级隔离由 owner 条件给）。
	status, exists, raw := sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/messages/exists", "")
	if status != http.StatusOK {
		t.Fatalf("存在性检查应 200，实得 %d（%s）", status, raw)
	}
	if exists["exists"] != true {
		t.Fatalf("有工件时 exists 应为 true，实得 %v", exists)
	}
	status, exists, _ = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/messages/exists?requestSequence=2", "")
	if status != http.StatusOK || exists["exists"] != false {
		t.Fatalf("未写过的序号应 exists=false，实得 %d %v", status, exists)
	}

	// 4. 会话不存在：messages 答 404，exists 答 200 false（Node 的分工：后者只是「按钮显不显示」）。
	missing := fmt.Sprintf("sess-messages-missing-%d", time.Now().UnixNano())
	status, _, _ = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+missing+"/messages", "")
	if status != http.StatusNotFound {
		t.Fatalf("不存在的会话 messages 应 404，实得 %d", status)
	}
	status, exists, _ = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+missing+"/messages/exists", "")
	if status != http.StatusOK || exists["exists"] != false {
		t.Fatalf("不存在的会话 exists 应 200 false，实得 %d %v", status, exists)
	}

	// 5. 围栏：把**最新序号**的所有者键改成别的 keyId，工件立刻不可读（并答 404，不是 403）。
	if err := deps.Binder.StoreSessionRequestOwner(ctx, identity, 3, keyID+999); err != nil {
		t.Fatalf("改所有者键失败: %v", err)
	}
	status, problem, _ = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/messages?requestSequence=3", "")
	if status != http.StatusNotFound {
		t.Fatalf("所有者不匹配应 404，实得 %d %v", status, problem)
	}
	status, exists, _ = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/messages/exists?requestSequence=3", "")
	if status != http.StatusOK || exists["exists"] != false {
		t.Fatalf("所有者不匹配时 exists 应 200 false，实得 %d %v", status, exists)
	}

	// 6. 他人（非管理员）：整个会话都定位不到 → exists 答 200 false、messages 答 404。
	foreignRouter := sessionsRouterWithDeps(t, pools,
		Principal{UserID: otherID, IsAdmin: false}, deps)
	status, exists, _ = sessionsRequest(t, foreignRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/messages/exists", "")
	if status != http.StatusOK || exists["exists"] != false {
		t.Fatalf("他人 exists 应 200 false，实得 %d %v", status, exists)
	}
	status, _, _ = sessionsRequest(t, foreignRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/messages", "")
	if status != http.StatusNotFound {
		t.Fatalf("他人 messages 应 404，实得 %d", status)
	}

	// 7. 管理员可读别人的会话（owner 条件不带），但围栏仍按 locator 的 keyId 判。
	adminRouter := sessionsRouterWithDeps(t, pools,
		Principal{UserID: ownerID, IsAdmin: true}, deps)
	status, _, _ = sessionsRequest(t, adminRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/messages", "")
	if status != http.StatusNotFound {
		t.Fatalf("围栏仍应拦住（所有者键已被改），实得 %d", status)
	}
}
