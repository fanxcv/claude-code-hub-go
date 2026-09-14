package adminapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 audit-logs 资源（/api/v1/audit-logs）的**路由契约与纯函数**测试。
// 真库集成在 audit_logs_integration_test.go。
//
// 钉住四件事：
//  1. 两条端点的档位都是 admin（审计行含操作人 IP 与前后快照，普通用户不可见）。
//  2. Store 未装配时**一条都不注册**（回退 Node），且不 panic。
//  3. 游标编解码与 Node 的 encodeCursor 逐字节同形（这是会回给前端并被拿回来的串）。
//  4. success / limit / cursor 三处的校验失败出口各不相同（前者 400 校验，后者 400 invalid_cursor）。

// auditLogRouteKeys 取出路由表里的 "METHOD PATH" 列表。
func auditLogRouteKeys(router *Router) []string {
	keys := make([]string, 0, router.RouteCount())
	for _, route := range router.RouteList() {
		keys = append(keys, route.Method+" "+route.Path)
	}
	return keys
}

func TestRegisterAuditLogsRoutes(t *testing.T) {
	guard := &recordingGuard{}
	deps := Deps{Guard: guard, Problems: NewProblems(nil), Store: auditLogStubPools()}
	router := New(Options{Deps: Deps{Guard: guard}})
	RegisterAuditLogs(router, deps)

	want := []string{
		http.MethodGet + " /audit-logs",
		http.MethodGet + " /audit-logs/{id}",
	}
	if got := auditLogRouteKeys(router); len(got) != len(want) {
		t.Fatalf("应注册 %d 条，实际 %d：%v", len(want), len(got), got)
	}
	for index, route := range router.RouteList() {
		if route.Method+" "+route.Path != want[index] {
			t.Errorf("第 %d 条应为 %s，实际 %s %s", index, want[index], route.Method, route.Path)
		}
		if route.Access != AccessAdmin {
			t.Errorf("%s 的档位应为 admin，实际 %s", route.Path, route.Access)
		}
		if route.Module != "audit_logs" {
			t.Errorf("%s 的模块应为 audit_logs，实际 %s", route.Path, route.Module)
		}
	}
	if ids := []string{"listAuditLogs", "getAuditLog"}; len(router.RouteList()) == len(ids) {
		for index, id := range ids {
			if got := router.RouteList()[index].OperationID; got != id {
				t.Errorf("第 %d 条的 operationId 应为 %s，实际 %s", index, id, got)
			}
		}
	}
}

// TestRegisterAuditLogsWithoutStore 钉住 fail-closed：没有连接池就一条都不注册。
func TestRegisterAuditLogsWithoutStore(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterAuditLogs(router, Deps{Guard: &recordingGuard{}})
	if got := router.RouteCount(); got != 0 {
		t.Fatalf("Store 未装配时应注册 0 条，实际 %d：%v", got, auditLogRouteKeys(router))
	}
}

// TestAuditLogCursorRoundTrip 钉住游标与 Node 的 encodeCursor 逐字节同形。
//
// 期望串由 Node 侧
// `Buffer.from(JSON.stringify({createdAt:"2026-01-02T03:04:05.678Z",id:42}),"utf8").toString("base64url")`
// 产出；键序必须是 createdAt 在前（Go 结构体字段序即 JSON 键序）。
func TestAuditLogCursorRoundTrip(t *testing.T) {
	const wantCursor = "eyJjcmVhdGVkQXQiOiIyMDI2LTAxLTAyVDAzOjA0OjA1LjY3OFoiLCJpZCI6NDJ9"
	encoded := encodeAuditLogCursor(store.AdminAuditLogCursor{CreatedAt: "2026-01-02T03:04:05.678Z", ID: 42})
	if encoded != wantCursor {
		t.Fatalf("游标编码应为 %s，实际 %s", wantCursor, encoded)
	}

	decoded, ok := decodeAuditLogCursor(wantCursor)
	if !ok {
		t.Fatal("合法游标应解码成功")
	}
	if decoded.CreatedAt != "2026-01-02T03:04:05.678Z" || decoded.ID != 42 {
		t.Fatalf("解码结果不对：%+v", decoded)
	}
}

// TestDecodeAuditLogCursorRejects 钉住 handlers.ts:101-121 的三条拒绝条件。
func TestDecodeAuditLogCursorRejects(t *testing.T) {
	cases := map[string]string{
		"非 base64":    "!!!!",
		"不是 JSON":     encodeBase64URL("not-json"),
		"不是对象":        encodeBase64URL("42"),
		"缺 createdAt": encodeBase64URL(`{"id":1}`),
		"id 不是整数":     encodeBase64URL(`{"createdAt":"2026-01-02T03:04:05.678Z","id":1.5}`),
		"id 类型不对":     encodeBase64URL(`{"createdAt":"2026-01-02T03:04:05.678Z","id":"1"}`),
	}
	for name, cursor := range cases {
		if _, ok := decodeAuditLogCursor(cursor); ok {
			t.Errorf("%s 的游标应被拒绝：%s", name, cursor)
		}
	}
}

// TestAuditLogListQueryValidation 钉住三处校验：success 只认字面量、limit 上下界、时间格式。
func TestAuditLogListQueryValidation(t *testing.T) {
	cases := []struct {
		name   string
		target string
		field  string
		code   string
	}{
		{"success 非字面量", "/audit-logs?success=1", "success", "invalid_enum_value"},
		{"success 大小写敏感", "/audit-logs?success=TRUE", "success", "invalid_enum_value"},
		{"limit 下界", "/audit-logs?limit=0", "limit", "too_small"},
		{"limit 上界", "/audit-logs?limit=101", "limit", "too_big"},
		{"limit 非数", "/audit-logs?limit=abc", "limit", "invalid_type"},
		{"category 非法", "/audit-logs?category=nope", "category", "invalid_enum_value"},
		{"from 非 ISO", "/audit-logs?from=2026-01-02", "from", "invalid_string"},
		{"to 缺时区", "/audit-logs?to=2026-01-02T03:04:05", "to", "invalid_string"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, testCase.target, nil)
			_, issues, invalidCursor := parseAuditLogListQuery(request)
			if invalidCursor {
				t.Fatalf("不应被判成游标错误")
			}
			if len(issues) != 1 {
				t.Fatalf("应有 1 条校验错误，实际 %d：%+v", len(issues), issues)
			}
			if got := issues[0].Code; got != testCase.code {
				t.Errorf("错误码应为 %s，实际 %s", testCase.code, got)
			}
			if path, ok := issues[0].Path[0].(string); !ok || path != testCase.field {
				t.Errorf("错误路径应为 %s，实际 %v", testCase.field, issues[0].Path)
			}
		})
	}
}

// TestAuditLogListQueryAccepts 钉住合法输入（含默认值与时区偏移写法）。
func TestAuditLogListQueryAccepts(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet,
		"/audit-logs?limit=5&category=key&success=false&from=2026-01-02T03:04:05Z"+
			"&to=2026-01-03T03:04:05%2B08:00&cursor="+encodeBase64URL(
			`{"createdAt":"2026-01-02T03:04:05.678Z","id":42}`), nil)
	query, issues, invalidCursor := parseAuditLogListQuery(request)
	if len(issues) > 0 || invalidCursor {
		t.Fatalf("合法查询不应报错：issues=%+v invalidCursor=%v", issues, invalidCursor)
	}
	if query.Limit != 5 || query.Category != "key" {
		t.Errorf("limit/category 解析不对：%+v", query)
	}
	if query.Success == nil || *query.Success {
		t.Errorf("success=false 应解析成 false，实际 %v", query.Success)
	}
	if query.From == nil || query.To == nil {
		t.Fatalf("from/to 应解析出来：%+v", query)
	}
	if query.Cursor == nil || query.Cursor.ID != 42 {
		t.Fatalf("游标应解析出来：%+v", query.Cursor)
	}

	// 无参数时：limit 取默认 20，其余全空。
	empty, issues, invalidCursor := parseAuditLogListQuery(
		httptest.NewRequest(http.MethodGet, "/audit-logs", nil))
	if len(issues) > 0 || invalidCursor {
		t.Fatalf("空查询不应报错：%+v", issues)
	}
	if empty.Limit != auditLogDefaultLimit || empty.Cursor != nil || empty.Category != "" {
		t.Errorf("空查询的默认值不对：%+v", empty)
	}
}

// TestAuditLogListInvalidCursorProblem 钉住游标非法时的作答形状（errorCode 与 detail 都不同）。
func TestAuditLogListInvalidCursorProblem(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterAuditLogs(router, Deps{
		Guard:    &recordingGuard{},
		Problems: NewProblems(nil),
		Store:    auditLogStubPools(),
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/audit-logs?cursor=!!!", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态应为 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON：%s", recorder.Body.String())
	}
	if body["errorCode"] != "audit_log.invalid_cursor" {
		t.Errorf("errorCode 应为 audit_log.invalid_cursor，实际 %v", body["errorCode"])
	}
	if body["detail"] != "Cursor is invalid." {
		t.Errorf("detail 应为 Node 的固定文案，实际 %v", body["detail"])
	}
	if _, present := body["invalidParams"]; present {
		t.Errorf("游标错误不带 invalidParams：%+v", body)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"urn:claude-code-hub:problem:audit_log.invalid_cursor"`) {
		t.Errorf("problem type 形状不对：%s", recorder.Body.String())
	}
}

// encodeBase64URL 是测试用的 base64url 编码（无填充，与 Node 的 Buffer.toString("base64url") 同形）。
func encodeBase64URL(text string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(text))
}

// auditLogStubPools 造一个非 nil 的空连接池：只用于注册路径，本文件的用例不碰数据库。
// 零值 Pools 的取道方法会报错，因此任何真正走到查询的用例都属于集成测试（另见
// audit_logs_integration_test.go）。
func auditLogStubPools() *store.Pools { return &store.Pools{} }
