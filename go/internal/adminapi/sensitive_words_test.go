package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 sensitive-words 六条端点的真实 PG 集成测试（lane A1-5）。
//
// 夹具纪律（共享库上踩过的坑）：本测试的所有行都由**本测试自己创建**，词名带唯一的
// `go-a15-sw-<纳秒>` 前缀，清理时按词名精确删除，且清理用的连接池不会被本测试关闭
// （testPools 注册的 Cleanup 是 LIFO，后注册的本清理先跑）。
// 列表类断言必须**按自己的前缀过滤**：库里还有别的测试与生产数据。

// semanticWordsFixture 建一个走守卫的路由表。
func sensitiveWordsRouter(t *testing.T, pools *store.Pools, deps *Deps) *Router {
	t.Helper()
	if deps.Guard == nil {
		deps.Guard = newTestGuard(t, GuardOptions{})
	}
	if deps.Problems == nil {
		deps.Problems = NewProblems(nil)
	}
	deps.Store = pools
	router := New(Options{Deps: *deps})
	RegisterSensitiveWords(router, *deps)
	return router
}

// sensitiveWordRequest 发一次带管理员令牌的请求。
func sensitiveWordRequest(
	t *testing.T,
	router *Router,
	method, target, body, contentType string,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// sensitiveWordPrefix 生成本次测试唯一的前缀。
func sensitiveWordPrefix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("go-a15-sw-%d", time.Now().UnixNano())
}

// cleanupSensitiveWords 按前缀清掉本测试留下的行。
func cleanupSensitiveWords(t *testing.T, pools *store.Pools, prefix string) {
	t.Helper()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"DELETE FROM sensitive_words WHERE word LIKE $1", prefix+"%"); err != nil {
		t.Fatalf("清理敏感词失败: %v", err)
	}
}

// TestSensitiveWordsCRUDRoundTrip 覆盖 create → list → patch → delete → 404 的完整回路。
func TestSensitiveWordsCRUDRoundTrip(t *testing.T) {
	pools := testPools(t)
	router := sensitiveWordsRouter(t, pools, &Deps{})
	prefix := sensitiveWordPrefix(t)
	t.Cleanup(func() { cleanupSensitiveWords(t, pools, prefix) })

	created := sensitiveWordRequest(t, router, http.MethodPost, "/api/v1/sensitive-words",
		fmt.Sprintf(`{"word":"%s-alpha","matchType":"contains","description":"来自 A1-5 集成测试"}`, prefix),
		"application/json")
	if created.Code != http.StatusCreated {
		t.Fatalf("创建应 201，实际 %d body=%s", created.Code, created.Body.String())
	}
	word := decodeSensitiveWord(t, created.Body.Bytes())
	if word["word"] != prefix+"-alpha" || word["matchType"] != "contains" ||
		word["isEnabled"] != true || word["description"] != "来自 A1-5 集成测试" {
		t.Fatalf("创建响应字段不符: %v", word)
	}
	if createdAt, _ := word["createdAt"].(string); !strings.HasSuffix(createdAt, "Z") || len(createdAt) != 24 {
		t.Fatalf("createdAt 应为 JS toISOString 形状（24 字符、Z 结尾），实际 %v", word["createdAt"])
	}
	id := int64(word["id"].(float64))
	if location := created.Header().Get("Location"); location != fmt.Sprintf("/api/v1/sensitive-words/%d", id) {
		t.Fatalf("Location 头不符: %q", location)
	}

	listed := sensitiveWordRequest(t, router, http.MethodGet, "/api/v1/sensitive-words", "", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d", listed.Code)
	}
	if !sensitiveWordListContains(t, listed.Body.Bytes(), prefix) {
		t.Fatalf("列表未包含新建的词: %s", listed.Body.String())
	}

	patched := sensitiveWordRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/sensitive-words/%d", id),
		`{"isEnabled":false,"matchType":"exact"}`, "application/json")
	if patched.Code != http.StatusOK {
		t.Fatalf("更新应 200，实际 %d body=%s", patched.Code, patched.Body.String())
	}
	updated := decodeSensitiveWord(t, patched.Body.Bytes())
	if updated["isEnabled"] != false || updated["matchType"] != "exact" {
		t.Fatalf("部分更新未生效: %v", updated)
	}

	deleted := sensitiveWordRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/api/v1/sensitive-words/%d", id), "", "")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("删除应 204，实际 %d", deleted.Code)
	}
	if deleted.Body.Len() != 0 {
		t.Fatalf("204 不应有正文: %q", deleted.Body.String())
	}

	missing := sensitiveWordRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/api/v1/sensitive-words/%d", id), "", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404，实际 %d body=%s", missing.Code, missing.Body.String())
	}
	if code := a15DecodeProblemCode(t, missing.Body.Bytes()); code != "sensitive_word.not_found" {
		t.Fatalf("404 错误码应为 sensitive_word.not_found，实际 %q", code)
	}
	if missing.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("问题响应 Content-Type 不符: %q", missing.Header().Get("Content-Type"))
	}
}

// TestSensitiveWordsCacheEndpoints 覆盖 cache:refresh 与 cache/stats 的统计口径。
func TestSensitiveWordsCacheEndpoints(t *testing.T) {
	pools := testPools(t)
	router := sensitiveWordsRouter(t, pools, &Deps{})
	prefix := sensitiveWordPrefix(t)
	t.Cleanup(func() { cleanupSensitiveWords(t, pools, prefix) })

	for index, matchType := range []string{"contains", "exact", "regex"} {
		response := sensitiveWordRequest(t, router, http.MethodPost, "/api/v1/sensitive-words",
			fmt.Sprintf(`{"word":"%s-cache-%d","matchType":"%s"}`, prefix, index, matchType),
			"application/json")
		if response.Code != http.StatusCreated {
			t.Fatalf("创建 %s 词失败: %d %s", matchType, response.Code, response.Body.String())
		}
	}
	// 一个禁用行不得计入统计（Node 侧只装载启用行）。
	disabled := sensitiveWordRequest(t, router, http.MethodPost, "/api/v1/sensitive-words",
		fmt.Sprintf(`{"word":"%s-cache-disabled","matchType":"contains","description":null}`, prefix),
		"application/json")
	if disabled.Code != http.StatusBadRequest {
		t.Fatalf("description 为 null 应被 strict schema 拒绝（zod 的 .optional() 不接受 null），实际 %d",
			disabled.Code)
	}
	off := sensitiveWordRequest(t, router, http.MethodPost, "/api/v1/sensitive-words",
		fmt.Sprintf(`{"word":"%s-cache-off","matchType":"contains"}`, prefix), "application/json")
	if off.Code != http.StatusCreated {
		t.Fatalf("创建待禁用行失败: %d %s", off.Code, off.Body.String())
	}
	offID := int64(decodeSensitiveWord(t, off.Body.Bytes())["id"].(float64))
	if response := sensitiveWordRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/sensitive-words/%d", offID), `{"isEnabled":false}`,
		"application/json"); response.Code != http.StatusOK {
		t.Fatalf("禁用失败: %d %s", response.Code, response.Body.String())
	}

	stats := sensitiveWordRequest(t, router, http.MethodGet, "/api/v1/sensitive-words/cache/stats", "", "")
	if stats.Code != http.StatusOK {
		t.Fatalf("cache/stats 应 200，实际 %d body=%s", stats.Code, stats.Body.String())
	}
	counts := a15DecodeJSON(t, stats.Body.Bytes())
	for _, key := range []string{"containsCount", "exactCount", "regexCount", "totalCount", "lastReloadTime", "isLoading"} {
		if _, ok := counts[key]; !ok {
			t.Fatalf("cache/stats 缺少字段 %q: %v", key, counts)
		}
	}
	if counts["containsCount"].(float64) < 1 || counts["exactCount"].(float64) < 1 ||
		counts["regexCount"].(float64) < 1 {
		t.Fatalf("三类计数都应 ≥1（本测试各建了一条）: %v", counts)
	}
	if counts["isLoading"] != false {
		t.Fatalf("Go 侧无异步装载状态，isLoading 应恒 false: %v", counts["isLoading"])
	}
	total := counts["containsCount"].(float64) + counts["exactCount"].(float64) + counts["regexCount"].(float64)
	if counts["totalCount"].(float64) != total {
		t.Fatalf("totalCount 应等于三类之和: %v", counts)
	}

	refreshed := sensitiveWordRequest(t, router, http.MethodPost,
		"/api/v1/sensitive-words/cache:refresh", "", "")
	if refreshed.Code != http.StatusOK {
		t.Fatalf("cache:refresh 应 200，实际 %d body=%s", refreshed.Code, refreshed.Body.String())
	}
	payload := a15DecodeJSON(t, refreshed.Body.Bytes())
	statsNode, ok := payload["stats"].(map[string]any)
	if !ok {
		t.Fatalf("refresh 响应应为 {stats:{...}}，实际 %v", payload)
	}
	if statsNode["lastReloadTime"].(float64) <= 0 {
		t.Fatalf("refresh 之后 lastReloadTime 应为正数（毫秒时间戳）: %v", statsNode)
	}
}

// TestSensitiveWordsRejectsBadRequests 覆盖三条与 Node 同形的失败路径。
func TestSensitiveWordsRejectsBadRequests(t *testing.T) {
	pools := testPools(t)
	router := sensitiveWordsRouter(t, pools, &Deps{})
	prefix := sensitiveWordPrefix(t)
	t.Cleanup(func() { cleanupSensitiveWords(t, pools, prefix) })

	unknownKey := sensitiveWordRequest(t, router, http.MethodPost, "/api/v1/sensitive-words",
		fmt.Sprintf(`{"word":"%s-x","matchType":"contains","nope":1}`, prefix), "application/json")
	if unknownKey.Code != http.StatusBadRequest {
		t.Fatalf("strict schema 应拒未知键，实际 %d", unknownKey.Code)
	}
	problem := a15DecodeJSON(t, unknownKey.Body.Bytes())
	if problem["errorCode"] != "request.validation_failed" || problem["title"] != "Validation failed" {
		t.Fatalf("校验失败信封不符: %v", problem)
	}
	if params, ok := problem["invalidParams"].([]any); !ok || len(params) == 0 {
		t.Fatalf("校验失败应带 invalidParams: %v", problem)
	}

	wrongType := sensitiveWordRequest(t, router, http.MethodPost, "/api/v1/sensitive-words",
		fmt.Sprintf(`{"word":"%s-y","matchType":"contains"}`, prefix), "text/plain")
	if wrongType.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("非 JSON 正文应 415，实际 %d body=%s", wrongType.Code, wrongType.Body.String())
	}

	malformed := sensitiveWordRequest(t, router, http.MethodPost, "/api/v1/sensitive-words",
		`{"word":`, "application/json")
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400，实际 %d", malformed.Code)
	}
	if code := a15DecodeProblemCode(t, malformed.Body.Bytes()); code != "request.malformed_json" {
		t.Fatalf("坏 JSON 错误码应为 request.malformed_json，实际 %q", code)
	}

	badID := sensitiveWordRequest(t, router, http.MethodPatch, "/api/v1/sensitive-words/abc",
		`{}`, "application/json")
	if badID.Code != http.StatusBadRequest {
		t.Fatalf("非数字 id 应 400，实际 %d body=%s", badID.Code, badID.Body.String())
	}
}

// TestSensitiveWordsWritesAudit 验证写路径真的落了审计行（真库 + 真审计写入器）。
//
// 审计是 fire-and-forget（Node 同语义：不阻塞响应、失败只告警），故这里**有界轮询**最多 3 秒，
// 不做无限等待。断言的列是 A0-3 扩出来的那几列：target_name / target_id / success / category。
func TestSensitiveWordsWritesAudit(t *testing.T) {
	pools := testPools(t)
	deps := Deps{Problems: NewProblems(nil)}
	audit, err := NewAuditLog(deps, AuditLogOptions{Pools: pools, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("建立审计写入器失败: %v", err)
	}
	deps.Audit = audit
	router := sensitiveWordsRouter(t, pools, &deps)
	prefix := sensitiveWordPrefix(t)
	t.Cleanup(func() { cleanupSensitiveWords(t, pools, prefix) })

	created := sensitiveWordRequest(t, router, http.MethodPost, "/api/v1/sensitive-words",
		fmt.Sprintf(`{"word":"%s-audit","matchType":"contains"}`, prefix), "application/json")
	if created.Code != http.StatusCreated {
		t.Fatalf("创建失败: %d %s", created.Code, created.Body.String())
	}
	id := int64(decodeSensitiveWord(t, created.Body.Bytes())["id"].(float64))

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var (
			category   string
			targetName string
			success    bool
		)
		scanErr := pool.QueryRow(context.Background(),
			`SELECT action_category, COALESCE(target_name, ''), success FROM audit_log
			 WHERE action_type = 'sensitive_word.create' AND target_id = $1
			 ORDER BY id DESC LIMIT 1`, fmt.Sprintf("%d", id),
		).Scan(&category, &targetName, &success)
		if scanErr == nil {
			if category != "sensitive_word" || success != true || targetName != prefix+"-audit" {
				t.Fatalf("审计行内容不符: category=%q targetName=%q success=%v",
					category, targetName, success)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("3 秒内未看到 sensitive_word.create 的审计行: %v", scanErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---- 解码小工具（本文件内公用） ----

func a15DecodeJSON(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v body=%s", err, payload)
	}
	return object
}

func a15DecodeProblemCode(t *testing.T, payload []byte) string {
	t.Helper()
	object := a15DecodeJSON(t, payload)
	code, _ := object["errorCode"].(string)
	return code
}

func decodeSensitiveWord(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	return a15DecodeJSON(t, payload)
}

// sensitiveWordListContains 判断列表响应里是否有以 prefix 开头的词。
func sensitiveWordListContains(t *testing.T, payload []byte, prefix string) bool {
	t.Helper()
	object := a15DecodeJSON(t, payload)
	items, _ := object["items"].([]any)
	for _, item := range items {
		node, _ := item.(map[string]any)
		if word, _ := node["word"].(string); strings.HasPrefix(word, prefix) {
			return true
		}
	}
	return false
}
