package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
)

// 本文件是 `GET /api/v1/sessions/{sessionId}/response` 的真依赖验收，形式与同资源别的集成测试
// 一致：**走生产写侧落工件**（`session.Binder.StoreSessionResponse`），再让端点读出来。
//
// 为什么必须穿写侧：这条端点的全部语义在「工件在不在、是不是你的」，手写 Redis 键只能证明
// 「读侧会读某个键名」，而键名/所有者围栏/TTL 口径写错时端点会恒 404 —— 手写键的测试会一路绿灯。
func TestSessionResponseClosedLoop(t *testing.T) {
	deps := openSessionsDeps(t)
	pools := deps.Pools
	ctx := context.Background()

	providerID := fixtureProviderID(t, pools)
	ownerID := fixtureUser(t, pools, "user", true)
	otherID := fixtureUser(t, pools, "user", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: ownerID, canLoginWebUI: true, isEnabled: true,
	})

	// 会话的账本行（定位器的身份来源）。夹具落在序号 1..3，序号 3 最新。
	identity := fmt.Sprintf("sess-response-%d", time.Now().UnixNano())
	seedOriginChainRows(t, pools, ownerID, keyValue, identity, providerID)

	// 响应正文由生产写侧落盘：序号 3 是「不带选择器」时定位器会选中的那条。
	// StoreMessages=true 表示「原样落盘」——端点契约要验的是**逐字节透传**（正文可能是 JSON、
	// 也可能是 SSE 文本或带截断标记的窗口，端点一律不解码）；脱敏分支是写侧的职责，另有用例覆盖。
	const storedBody = `{"id":"msg_1","content":"[REDACTED]"}`
	if err := deps.Binder.StoreSessionResponse(ctx, identity, 3, keyID, []byte(storedBody),
		session.SessionArtifactOptions{StoreResponseBody: true, StoreMessages: true}); err != nil {
		t.Fatalf("写响应正文工件失败: %v", err)
	}
	// 另一把 key（同属主）写的正文：用来验所有权围栏——工件不是这把 key 写的就不算数。
	foreignKeyID, _ := fixtureKey(t, pools, fixtureKeyOptions{
		userID: ownerID, canLoginWebUI: true, isEnabled: true,
	})
	if err := deps.Binder.StoreSessionResponse(ctx, identity, 1, foreignKeyID, []byte(`{"id":"foreign"}`),
		session.SessionArtifactOptions{StoreResponseBody: true, StoreMessages: false}); err != nil {
		t.Fatalf("写第二份响应正文工件失败: %v", err)
	}
	t.Cleanup(func() {
		for _, sequence := range []int{1, 3} {
			_ = deps.Redis.Del(ctx, session.SessionResponseKey(identity, sequence)).Err()
			_ = deps.Redis.Del(ctx, session.SessionRequestOwnerKey(identity, sequence)).Err()
		}
	})

	newRouter := func(principal Principal) *Router {
		router := sessionsRouterWithDeps(t, pools, principal, deps)
		RegisterSessionResponseRoute(router, Deps{
			Guard:            principalGuard{principal: principal},
			Problems:         NewProblems(nil),
			Store:            pools,
			SessionArtifacts: deps.Artifacts,
		})
		return router
	}

	// 1. 闭环：不带选择器时定位到最新序号（3），正文逐字节返回，包在 `{"response": …}` 里。
	ownerRouter := newRouter(Principal{UserID: ownerID, IsAdmin: false})
	status, body, raw := sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/response", "")
	if status != http.StatusOK {
		t.Fatalf("应 200，实得 %d（正文 %s）", status, raw)
	}
	if got := body["response"]; got != storedBody {
		t.Fatalf("正文应逐字节返回，实得 %v", got)
	}

	// 2. 显式指定序号 1：那份工件是**另一把 key**写的，所有权围栏应判不属于本次调用者 → 404。
	status, _, raw = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/response?requestSequence=1", "")
	if status != http.StatusNotFound {
		t.Fatalf("非本 key 写的工件应 404（围栏），实得 %d（正文 %s）", status, raw)
	}

	// 3. 序号 2 没写过工件 → 404（「响应体已过期或尚未记录」）。
	status, _, raw = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/response?requestSequence=2", "")
	if status != http.StatusNotFound {
		t.Fatalf("未记录的序号应 404，实得 %d（正文 %s）", status, raw)
	}

	// 4. 非属主（普通用户）访问别人的会话：**404**，不是 403。
	//
	// 这一条与直觉相反，但两侧同判：Node 的聚合 `aggregateMultipleSessionStats(ids, currentUserId)`
	// 在 SQL 层就按属主过滤（message.ts:1731），非属主拿不到任何行 ⇒ 走「Session 不存在」⇒
	// actionError 定档 404 session.not_found。Node 源码里那条 403「无权访问该 Session」分支
	// 因此在同一条路径上不可达；Go 的 sessionOwnerOrError 保留了同样的二次判定
	// （sessions_detail.go：「SQL 侧已按 owner 过滤，这里保留 Node 的显式二次判定」），
	// 所以这里钉的是 404 —— 钉 403 会假装与 Node 对齐而实际分叉。
	otherRouter := newRouter(Principal{UserID: otherID, IsAdmin: false})
	status, body, raw = sessionsRequest(t, otherRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/response", "")
	if status != http.StatusNotFound {
		t.Fatalf("非属主应 404（SQL 侧按属主过滤，Node 同判），实得 %d（正文 %s）", status, raw)
	}
	if code, _ := body["errorCode"].(string); code != "session.not_found" {
		t.Fatalf("非属主错误码应为 session.not_found，实得 %v", body["errorCode"])
	}

	// 5. 管理员可跨用户读（Node 的 isAdmin 分支）。
	adminRouter := newRouter(Principal{UserID: otherID, IsAdmin: true})
	status, body, raw = sessionsRequest(t, adminRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/response", "")
	if status != http.StatusOK {
		t.Fatalf("管理员应可读，实得 %d（正文 %s）", status, raw)
	}
	if body["response"] != storedBody {
		t.Fatalf("管理员读到的正文不符：%v", body["response"])
	}

	// 6. 不存在的会话 → 404 session.not_found。
	status, body, raw = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/sess-does-not-exist-"+identity+"/response", "")
	if status != http.StatusNotFound {
		t.Fatalf("未知会话应 404，实得 %d（正文 %s）", status, raw)
	}
	if code, _ := body["errorCode"].(string); code != "session.not_found" {
		t.Fatalf("错误码应为 session.not_found，实得 %v（正文 %s）", body["errorCode"], raw)
	}

	// 7. 参数校验：requestSequence=0 非正整数 → 400（zod 的 positive() 语义）。
	status, _, raw = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/response?requestSequence=0", "")
	if status != http.StatusBadRequest {
		t.Fatalf("requestSequence=0 应 400，实得 %d（正文 %s）", status, raw)
	}
	// 8. 空的 sourceSessionId → 400（min(1)）。
	status, _, raw = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/response?sourceSessionId=", "")
	if status != http.StatusBadRequest {
		t.Fatalf("空 sourceSessionId 应 400，实得 %d（正文 %s）", status, raw)
	}
}

// TestRegisterSessionResponseRouteConditions 钉住注册条件与路由元数据。
func TestRegisterSessionResponseRouteConditions(t *testing.T) {
	// 缺 Store（或工件读面）→ 不注册，整条回退 Node。
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterSessionResponseRoute(router, Deps{})
	if count := router.RouteCount(); count != 0 {
		t.Fatalf("缺依赖时不该注册任何路由，收到 %d 条", count)
	}
}
