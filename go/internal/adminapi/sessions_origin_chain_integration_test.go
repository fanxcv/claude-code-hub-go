package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 GET /sessions/{sessionId}/origin-chain 的**真库**验收：路由契约、错误定档、
// 活体形状，以及「定位器 + 来源链」两步串起来之后的端到端结果。
//
// 为什么非真库不可：这条端点的三段判据（身份归并、选择器完整性、时间边界内的初始选链）全在
// SQL 里，桩件只能证明「调了哪条查询」。错误定档也必须与 Node 的 actionError 逐档对齐——
// 「不存在」是 404、「选择器残缺」是 400，把两者混成同一档会让 UI 的提示走错分支。

// seedOriginChainRows 造一条会话的三行请求：seq1 带初始选链、seq2 换供应商、seq3 选中行。
//
// 返回 (选中行 id, 物理 session_id, keyName)。物理 session_id 与规范 identity 同值（普通会话）。
func seedOriginChainRows(
	t *testing.T,
	pools *store.Pools,
	userID int64,
	keyValue string,
	identity string,
	providerID int64,
) (int64, string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	model := fmt.Sprintf("origin-chain-%d-%s", time.Now().UnixNano(), identity)
	chain := fmt.Sprintf(`[{"reason":"initial_selection","id":%d,"name":"origin"}]`, providerID)

	var selectedID int64
	for index, sequence := range []int{1, 2, 3} {
		var chainValue any
		if sequence == 1 {
			chainValue = chain
		}
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO message_request (
				provider_id, user_id, key, model, original_model, endpoint,
				status_code, input_tokens, output_tokens, cost_usd, duration_ms, ttfb_ms,
				session_id, session_identity, session_identity_kind, request_sequence,
				provider_chain, is_replay, created_at
			) VALUES (
				$1, $2, $3, $4, $4, '/v1/messages',
				200, 100, 20, '0.5'::numeric, 10, 5,
				$5, $5, 'session_id', $6,
				$7::jsonb, false, $8
			) RETURNING id`,
			providerID, userID, keyValue, model, identity, sequence, chainValue,
			base.Add(time.Duration(index)*time.Minute),
		).Scan(&id); err != nil {
			t.Fatalf("插入来源链夹具行失败: %v", err)
		}
		if sequence == 3 {
			selectedID = id
		}
	}
	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(ctx,
			`DELETE FROM usage_ledger WHERE request_id IN
				(SELECT id FROM message_request WHERE key = $1 AND session_id = $2)`,
			keyValue, identity)
		_, _ = cleanupPool.Exec(ctx,
			`DELETE FROM message_request WHERE key = $1 AND session_id = $2`, keyValue, identity)
	})
	return selectedID, identity
}

// seedAffinityOriginRow 造一行前缀亲和会话（canonical=pfs:..., 物理 session_id 另取）。
func seedAffinityOriginRow(
	t *testing.T,
	pools *store.Pools,
	userID int64,
	keyValue string,
	canonical string,
	physical string,
	providerID int64,
) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	model := fmt.Sprintf("origin-affinity-%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_request (
			provider_id, user_id, key, model, original_model, endpoint,
			status_code, input_tokens, output_tokens, cost_usd, duration_ms, ttfb_ms,
			session_id, session_identity, session_identity_kind, request_sequence,
			affinity_scope_tag, affinity_fingerprint, is_replay, created_at
		) VALUES (
			$1, $2, $3, $4, $4, '/v1/messages',
			200, 100, 20, '0.5'::numeric, 10, 5,
			$5, $6, 'prefix_affinity', 1,
			'scope-origin', 'fp-origin', false, $7
		)`, providerID, userID, keyValue, model, physical, canonical,
		time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("插入前缀亲和夹具行失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(ctx,
			`DELETE FROM usage_ledger WHERE request_id IN
				(SELECT id FROM message_request WHERE key = $1 AND session_id = $2)`,
			keyValue, physical)
		_, _ = cleanupPool.Exec(ctx,
			`DELETE FROM message_request WHERE key = $1 AND session_id = $2`, keyValue, physical)
	})
}

// TestSessionOriginChainOnRealDependencies 钉住来源链端点的活体形状与错误定档。
func TestSessionOriginChainOnRealDependencies(t *testing.T) {
	pools := meOpenPools(t)
	providerID := fixtureProviderID(t, pools)
	ownerID := fixtureUser(t, pools, "user", true)
	otherID := fixtureUser(t, pools, "user", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: ownerID, canLoginWebUI: true, isEnabled: true,
	})
	_, otherKeyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: otherID, canLoginWebUI: true, isEnabled: true,
	})

	identity := fmt.Sprintf("sess-origin-%d", time.Now().UnixNano())
	seedOriginChainRows(t, pools, ownerID, keyValue, identity, providerID)
	affinityCanonical := fmt.Sprintf("pfx:origin-%d", time.Now().UnixNano())
	affinityPhysical := fmt.Sprintf("sess-affinity-%d", time.Now().UnixNano())
	seedAffinityOriginRow(t, pools, ownerID, keyValue, affinityCanonical, affinityPhysical, providerID)
	_ = otherKeyValue

	owner := Principal{UserID: ownerID, IsAdmin: false}
	router := sessionsRouterWithDeps(t, pools, owner, sessionsFullDeps{})

	// 1. 活体：返回裸数组，且是起点行的链。
	status, body, raw := sessionsRequest(t, router, http.MethodGet,
		"/api/v1/sessions/"+identity+"/origin-chain", "")
	if status != http.StatusOK {
		t.Fatalf("应 200，实得 %d（正文 %s）", status, raw)
	}
	var chain []map[string]any
	if err := json.Unmarshal([]byte(raw), &chain); err != nil || len(chain) == 0 {
		t.Fatalf("正文应是来源链数组，实得 %v / %s", err, raw)
	}
	if chain[0]["reason"] != "initial_selection" {
		t.Fatalf("来源链首项应是 initial_selection，实得 %s", raw)
	}
	if body != nil {
		t.Fatalf("来源链是裸数组，不应解出对象：%v", body)
	}

	// 2. 不存在的会话：404 session.not_found（Node 的「Session 不存在」）。
	status, problem, raw := sessionsRequest(t, router, http.MethodGet,
		"/api/v1/sessions/sess-origin-missing/origin-chain", "")
	if status != http.StatusNotFound {
		t.Fatalf("不存在应 404，实得 %d（正文 %s）", status, raw)
	}
	if problem["errorCode"] != "session.not_found" {
		t.Fatalf("错误码应为 session.not_found，实得 %v", problem)
	}

	// 3. 前缀亲和 + 只给序号：选择器残缺 → 400 SESSION_REQUEST_SELECTOR_INCOMPLETE。
	status, problem, raw = sessionsRequest(t, router, http.MethodGet,
		"/api/v1/sessions/"+affinityCanonical+"/origin-chain?requestSequence=1", "")
	if status != http.StatusBadRequest {
		t.Fatalf("残缺选择器应 400，实得 %d（正文 %s）", status, raw)
	}
	if problem["errorCode"] != sessionRequestSelectorIncomplete {
		t.Fatalf("错误码应为 %s，实得 %v", sessionRequestSelectorIncomplete, problem)
	}

	// 4. 前缀亲和 + 只给物理来源：同样残缺。
	status, problem, _ = sessionsRequest(t, router, http.MethodGet,
		"/api/v1/sessions/"+affinityCanonical+"/origin-chain?sourceSessionId="+affinityPhysical, "")
	if status != http.StatusBadRequest || problem["errorCode"] != sessionRequestSelectorIncomplete {
		t.Fatalf("只给物理来源应 400 %s，实得 %d %v",
			sessionRequestSelectorIncomplete, status, problem)
	}

	// 5. 前缀亲和 + 两者都给：可为空数组（该行没有初始选链）。
	status, _, raw = sessionsRequest(t, router, http.MethodGet,
		"/api/v1/sessions/"+affinityCanonical+"/origin-chain?requestSequence=1&sourceSessionId="+
			affinityPhysical, "")
	if status != http.StatusOK {
		t.Fatalf("完整选择器应 200，实得 %d（正文 %s）", status, raw)
	}
	if raw != "null" {
		t.Fatalf("无初始选链时应答 null，实得 %s", raw)
	}

	// 6. 序号非法：400 too_small（zod 的 positive）。
	status, problem, _ = sessionsRequest(t, router, http.MethodGet,
		"/api/v1/sessions/"+identity+"/origin-chain?requestSequence=0", "")
	if status != http.StatusBadRequest {
		t.Fatalf("序号 0 应 400，实得 %d", status)
	}
	if problem["errorCode"] == nil {
		t.Fatalf("校验失败应给出错误码，实得 %v", problem)
	}

	// 7. 他人（非管理员）：不存在的回答——与 Node 把「无权」与「不存在」合并成同一回答一致。
	foreignRouter := sessionsRouterWithDeps(t, pools,
		Principal{UserID: otherID, IsAdmin: false}, sessionsFullDeps{})
	status, problem, _ = sessionsRequest(t, foreignRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"/origin-chain", "")
	if status != http.StatusNotFound {
		t.Fatalf("他人访问应 404，实得 %d %v", status, problem)
	}

	// 8. 无条件路由守卫：无主体时 401。
	unauthenticated := New(Options{Deps: Deps{Guard: principalGuard{}}})
	RegisterSessionsRoutes(unauthenticated, Deps{
		Guard: principalGuard{}, Problems: NewProblems(nil), Store: pools,
	})
	status, _, _ = sessionsRequest(t, unauthenticated, http.MethodGet,
		"/api/v1/sessions/"+identity+"/origin-chain", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("无主体应 401，实得 %d", status)
	}

	_ = context.Background()
}
