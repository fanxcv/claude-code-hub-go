package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件的金样由 Node 真实代码产出，不是手抄：
//
//	bun /tmp/gen-problem3.ts    # 见文末「生成命令」注释里的脚本要点
//
// 脚本直接 import src/lib/api/v1/_shared/error-envelope.ts 调 createProblemJson /
// publicActionErrorDetail，并把两张状态码表（keys/handlers.ts:309-334、users/handlers.ts:
// 365-386）原样抄进脚本参与计算。改动信封形状时，先重跑脚本、再改本文件。
//
// 生成命令（2026-09-12 执行，bun 1.3）：
//
//	cat > /tmp/gen-problem3.ts <<'TS'
//	import { createProblemJson, publicActionErrorDetail } from "<repo>/src/lib/api/v1/_shared/error-envelope";
//	...（两张表 + keysAction/usersAction，见提交信息）
//	TS
//	bun /tmp/gen-problem3.ts
const nodeEnvelopeGoldens = `{
  "authMissing": {"type":"urn:claude-code-hub:problem:auth.missing","title":"Unauthorized","status":401,"detail":"Authentication is required.","instance":"/api/v1/keys","errorCode":"auth.missing"},
  "authInvalid": {"type":"urn:claude-code-hub:problem:auth.invalid","title":"Unauthorized","status":401,"detail":"Authentication is invalid or expired.","instance":"/api/v1/keys","errorCode":"auth.invalid"},
  "authForbidden": {"type":"urn:claude-code-hub:problem:auth.forbidden","title":"Forbidden","status":403,"detail":"Admin access is required.","instance":"/api/v1/users","errorCode":"auth.forbidden"},
  "apiKeyAdminDisabled": {"type":"urn:claude-code-hub:problem:auth.api_key_admin_disabled","title":"Forbidden","status":403,"detail":"API key admin access is disabled.","instance":"/api/v1/keys","errorCode":"auth.api_key_admin_disabled"},
  "csrfInvalid": {"type":"urn:claude-code-hub:problem:auth.csrf_invalid","title":"Forbidden","status":403,"detail":"CSRF token is missing or invalid.","instance":"/api/v1/keys","errorCode":"auth.csrf_invalid"},
  "internalDefault": {"type":"urn:claude-code-hub:problem:internal.error","title":"Internal server error","status":500,"detail":"Internal server error","instance":"/api/v1","errorCode":"internal.error"},
  "unsupportedMediaType": {"type":"urn:claude-code-hub:problem:request.unsupported_media_type","title":"Unsupported media type","status":415,"detail":"Unsupported media type","instance":"/api/v1","errorCode":"request.unsupported_media_type"},
  "dependencyUnavailable": {"type":"urn:claude-code-hub:problem:dependency.unavailable","title":"Service unavailable","status":503,"detail":"Service unavailable","instance":"/api/v1/usage-logs","errorCode":"dependency.unavailable"}
}`

// nodeActionGoldens 由同一个脚本的 keysAction/usersAction 产出。
const nodeActionGoldens = `{
  "keyNotFound": {"type":"urn:claude-code-hub:problem:NOT_FOUND","title":"Not found","status":404,"detail":"Not found","instance":"/api/v1/keys/7","errorCode":"NOT_FOUND"},
  "keyNotFoundNoCode": {"type":"urn:claude-code-hub:problem:key.action_failed","title":"Bad request","status":400,"detail":"Bad request","instance":"/api/v1/keys/7","errorCode":"key.action_failed"},
  "keyPermission": {"type":"urn:claude-code-hub:problem:PERMISSION_DENIED","title":"Forbidden","status":403,"detail":"Forbidden","instance":"/api/v1/keys/7","errorCode":"PERMISSION_DENIED"},
  "keyForbiddenSuffix": {"type":"urn:claude-code-hub:problem:TENANT_FORBIDDEN","title":"Forbidden","status":403,"detail":"Forbidden","instance":"/api/v1/keys/7","errorCode":"TENANT_FORBIDDEN"},
  "keySaveFailed": {"type":"urn:claude-code-hub:problem:CREATE_FAILED","title":"Bad request","status":400,"detail":"Bad request","instance":"/api/v1/keys/7","errorCode":"CREATE_FAILED"},
  "keyInternal": {"type":"urn:claude-code-hub:problem:INTERNAL_ERROR","title":"Bad request","status":400,"detail":"Bad request","instance":"/api/v1/keys/7","errorCode":"INTERNAL_ERROR"},
  "userInternal": {"type":"urn:claude-code-hub:problem:INTERNAL_ERROR","title":"Internal server error","status":500,"detail":"Internal server error","instance":"/api/v1/users/3","errorCode":"INTERNAL_ERROR"},
  "userDbError": {"type":"urn:claude-code-hub:problem:DATABASE_ERROR","title":"Service unavailable","status":503,"detail":"Service unavailable","instance":"/api/v1/users/3","errorCode":"DATABASE_ERROR"},
  "userUnauthorized": {"type":"urn:claude-code-hub:problem:UNAUTHORIZED","title":"Unauthorized","status":401,"detail":"Unauthorized","instance":"/api/v1/users/3","errorCode":"UNAUTHORIZED"},
  "userGroupNotFound": {"type":"urn:claude-code-hub:problem:GROUP_NOT_FOUND","title":"Bad request","status":400,"detail":"Bad request","instance":"/api/v1/users/3","errorCode":"GROUP_NOT_FOUND"},
  "userNoCode": {"type":"urn:claude-code-hub:problem:user.action_failed","title":"Bad request","status":400,"detail":"Bad request","instance":"/api/v1/users/3","errorCode":"user.action_failed"}
}`

func decodeGoldens(t *testing.T, raw string) map[string]map[string]any {
	t.Helper()
	var goldens map[string]map[string]any
	if err := json.Unmarshal([]byte(raw), &goldens); err != nil {
		t.Fatalf("金样解析失败: %v", err)
	}
	return goldens
}

// serveProblem 跑一次 WriteProblem 并返回状态码、Content-Type 与解析后的正文。
func serveProblem(t *testing.T, target string, write func(writer http.ResponseWriter, request *http.Request)) (int, string, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	recorder := httptest.NewRecorder()
	write(recorder, request)
	result := recorder.Result()
	defer func() { _ = result.Body.Close() }()

	var body map[string]any
	if err := json.NewDecoder(result.Body).Decode(&body); err != nil {
		t.Fatalf("响应正文不是 JSON: %v", err)
	}
	return result.StatusCode, result.Header.Get("Content-Type"), body
}

// assertEqualJSON 逐字段比对两份 JSON 对象（缺字段与多字段都算差异）。
func assertEqualJSON(t *testing.T, name string, want, got map[string]any) {
	t.Helper()
	for key, wantValue := range want {
		gotValue, ok := got[key]
		if !ok {
			t.Errorf("%s: 缺少字段 %q（Node: %v）", name, key, wantValue)
			continue
		}
		wantJSON, _ := json.Marshal(wantValue)
		gotJSON, _ := json.Marshal(gotValue)
		if string(wantJSON) != string(gotJSON) {
			t.Errorf("%s: 字段 %q 不一致：Node=%s Go=%s", name, key, wantJSON, gotJSON)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("%s: 多出字段 %q（Node 无此字段）", name, key)
		}
	}
}

// TestProblemEnvelopeMatchesNode 逐字段对齐 Node 的 createProblemResponse。
func TestProblemEnvelopeMatchesNode(t *testing.T) {
	goldens := decodeGoldens(t, nodeEnvelopeGoldens)
	problems := NewProblems(nil)

	cases := []struct {
		name      string
		target    string
		status    int
		errorCode string
		detail    string
	}{
		{"authMissing", "/api/v1/keys", 401, "auth.missing", "Authentication is required."},
		{"authInvalid", "/api/v1/keys", 401, "auth.invalid", "Authentication is invalid or expired."},
		{"authForbidden", "/api/v1/users", 403, "auth.forbidden", "Admin access is required."},
		{"apiKeyAdminDisabled", "/api/v1/keys", 403, "auth.api_key_admin_disabled", "API key admin access is disabled."},
		{"csrfInvalid", "/api/v1/keys", 403, "auth.csrf_invalid", "CSRF token is missing or invalid."},
		{"internalDefault", "/api/v1", 500, "internal.error", ""},
		{"unsupportedMediaType", "/api/v1", 415, "", ""},
		{"dependencyUnavailable", "/api/v1/usage-logs", 503, "", ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			want := goldens[testCase.name]
			if want == nil {
				t.Fatalf("缺金样 %q", testCase.name)
			}
			status, contentType, body := serveProblem(t, testCase.target, func(writer http.ResponseWriter, request *http.Request) {
				problems.WriteProblem(writer, request, testCase.status, testCase.errorCode, testCase.detail)
			})
			if float64(status) != want["status"] {
				t.Errorf("状态码不一致：Node=%v Go=%d", want["status"], status)
			}
			if contentType != problemContentType {
				t.Errorf("Content-Type 不一致：Node=%q Go=%q", problemContentType, contentType)
			}
			assertEqualJSON(t, testCase.name, want, body)
		})
	}
}

// TestProblemActionErrorMatchesNode 逐字段对齐 Node 的 action 错误映射。
func TestProblemActionErrorMatchesNode(t *testing.T) {
	goldens := decodeGoldens(t, nodeActionGoldens)
	problems := NewProblems(nil)

	cases := []struct {
		name     string
		target   string
		resource string
		code     string
	}{
		{"keyNotFound", "/api/v1/keys/7", "key", "NOT_FOUND"},
		{"keyNotFoundNoCode", "/api/v1/keys/7", "key", ""},
		{"keyPermission", "/api/v1/keys/7", "key", "PERMISSION_DENIED"},
		{"keyForbiddenSuffix", "/api/v1/keys/7", "key", "TENANT_FORBIDDEN"},
		{"keySaveFailed", "/api/v1/keys/7", "key", "CREATE_FAILED"},
		{"keyInternal", "/api/v1/keys/7", "key", "INTERNAL_ERROR"},
		{"userInternal", "/api/v1/users/3", "user", "INTERNAL_ERROR"},
		{"userDbError", "/api/v1/users/3", "user", "DATABASE_ERROR"},
		{"userUnauthorized", "/api/v1/users/3", "user", "UNAUTHORIZED"},
		{"userGroupNotFound", "/api/v1/users/3", "user", "GROUP_NOT_FOUND"},
		{"userNoCode", "/api/v1/users/3", "user", ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			want := goldens[testCase.name]
			if want == nil {
				t.Fatalf("缺金样 %q", testCase.name)
			}
			actionErr := &ActionError{Resource: testCase.resource, Code: testCase.code, Err: errors.New("内部原因")}
			status, _, body := serveProblem(t, testCase.target, func(writer http.ResponseWriter, request *http.Request) {
				problems.WriteActionError(writer, request, actionErr)
			})
			if float64(status) != want["status"] {
				t.Errorf("状态码不一致：Node=%v Go=%d", want["status"], status)
			}
			assertEqualJSON(t, testCase.name, want, body)
		})
	}
}

// TestProblemActionErrorExplicitStatusWins 验证 A1 可以逐模块覆盖状态码。
func TestProblemActionErrorExplicitStatusWins(t *testing.T) {
	problems := NewProblems(nil)
	actionErr := NewActionError("key", "CREATE_FAILED", http.StatusInternalServerError, errors.New("boom"))
	_, _, body := serveProblem(t, "/api/v1/keys/7", func(writer http.ResponseWriter, request *http.Request) {
		problems.WriteActionError(writer, request, actionErr)
	})
	if body["errorCode"] != "CREATE_FAILED" || body["title"] != "Internal server error" {
		t.Fatalf("显式状态码未生效: %v", body)
	}
}

// TestProblemActionErrorUntypedIs500 验证非 ActionError 一律 500 且不泄漏原始消息。
func TestProblemActionErrorUntypedIs500(t *testing.T) {
	problems := NewProblems(nil)
	status, _, body := serveProblem(t, "/api/v1/keys/7", func(writer http.ResponseWriter, request *http.Request) {
		problems.WriteActionError(writer, request, errors.New("pg: duplicate key value 密钥原文"))
	})
	if status != http.StatusInternalServerError || body["errorCode"] != "internal.error" {
		t.Fatalf("非 ActionError 应 500 internal.error: status=%d body=%v", status, body)
	}
	if detail, _ := body["detail"].(string); detail == "" || detail == "pg: duplicate key value 密钥原文" {
		t.Fatalf("detail 必须是公开文案，实际 %q", detail)
	}
}

// TestProblemErrorParams 验证 errorParams 透传与省略。
func TestProblemErrorParams(t *testing.T) {
	problems := NewProblems(nil)
	actionErr := &ActionError{
		Resource: "key",
		Code:     "key.action_failed",
		Status:   http.StatusBadRequest,
		Params:   map[string]any{"name": "demo", "count": float64(3)},
	}
	_, _, withParams := serveProblem(t, "/api/v1/keys", func(writer http.ResponseWriter, request *http.Request) {
		problems.WriteActionError(writer, request, actionErr)
	})
	if _, ok := withParams["errorParams"]; !ok {
		t.Fatalf("errorParams 应存在: %v", withParams)
	}

	_, _, withoutParams := serveProblem(t, "/api/v1/keys", func(writer http.ResponseWriter, request *http.Request) {
		problems.WriteActionError(writer, request, &ActionError{Resource: "key", Status: http.StatusBadRequest})
	})
	if _, ok := withoutParams["errorParams"]; ok {
		t.Fatalf("无参数时不该出现 errorParams: %v", withoutParams)
	}
}

// TestEncodeURIComponentMatchesJS 校验百分号转义与 JS 一致（含空格、斜杠、非 ASCII）。
func TestEncodeURIComponentMatchesJS(t *testing.T) {
	cases := map[string]string{
		"auth.missing": "auth.missing",
		"A-Z_a-z0-9":   "A-Z_a-z0-9",
		"a b":          "a%20b",
		"x/y":          "x%2Fy",
		"a~!*'()":      "a~!*'()",
		"密钥":           "%E5%AF%86%E9%92%A5",
		"100%":         "100%25",
	}
	for input, want := range cases {
		if got := encodeURIComponent(input); got != want {
			t.Errorf("encodeURIComponent(%q)=%q, 期望 %q", input, got, want)
		}
	}
}

// TestActionStatusTables 逐条覆盖两张状态码表（含 Node 无子串规则这一点）。
func TestActionStatusTables(t *testing.T) {
	cases := []struct {
		resource string
		code     string
		want     int
	}{
		{"key", "NOT_FOUND", 404},
		{"key", "GROUP_NOT_FOUND", 404},
		{"key", "PERMISSION_DENIED", 403},
		{"key", "UNAUTHORIZED", 403},
		{"key", "TENANT_FORBIDDEN", 403},
		{"key", "INTERNAL_ERROR", 400},
		{"user", "UNAUTHORIZED", 401},
		{"user", "PERMISSION_DENIED", 403},
		{"user", "NOT_FOUND", 404},
		{"user", "GROUP_NOT_FOUND", 400},
		{"user", "DATABASE_ERROR", 503},
		{"user", "CONNECTION_FAILED", 503},
		{"user", "TIMEOUT", 503},
		{"user", "NETWORK_ERROR", 503},
		{"user", "INTERNAL_ERROR", 500},
		{"user", "OPERATION_FAILED", 500},
		{"user", "UPDATE_FAILED", 500},
		{"user", "DELETE_FAILED", 500},
		{"user", "", 400},
		{"model_price", "INTERNAL_ERROR", 500},
	}
	for _, testCase := range cases {
		if got := actionStatus(testCase.resource, testCase.code); got != testCase.want {
			t.Errorf("actionStatus(%q,%q)=%d, 期望 %d", testCase.resource, testCase.code, got, testCase.want)
		}
	}
}

// recordingProblemsLogger 收 Problems 的日志（Warn / Error 两条通道）。
type recordingProblemsLogger struct {
	events []string
	warns  []map[string]any
	errors []map[string]any
}

func (l *recordingProblemsLogger) Warn(event string, fields map[string]any) {
	l.events = append(l.events, event)
	l.warns = append(l.warns, fields)
}

func (l *recordingProblemsLogger) Error(event string, fields map[string]any) {
	l.events = append(l.events, event)
	l.errors = append(l.errors, fields)
}

// TestProblemActionErrorLogs4xxCause 钉住「4xx 的成因也进服务端日志」。
//
// 回归对象：2026-09-15 的 PATCH /api/v1/providers/149 连吃 400 provider.action_failed，
// 响应 detail 恒为常量，而进程日志里没有任何成因（当时只在 5xx 记），只能靠 Redis 时序反推。
// 本用例同时钉住另一半：观测增强**不得**改变客户端可见的响应体。
func TestProblemActionErrorLogs4xxCause(t *testing.T) {
	logger := &recordingProblemsLogger{}
	problems := NewProblems(logger)
	actionErr := NewActionError("provider", "provider.action_failed", http.StatusBadRequest,
		errors.New("redis: i/o timeout sk-abcdefghijklmnopqrstuvwxyz"))
	status, _, body := serveProblem(t, "/api/v1/providers/149",
		func(writer http.ResponseWriter, request *http.Request) {
			problems.WriteActionError(writer, request, actionErr)
		})

	if status != http.StatusBadRequest {
		t.Fatalf("应 400，实得 %d", status)
	}
	if len(logger.events) != 1 || logger.events[0] != "admin_action_error" {
		t.Fatalf("4xx 必须记一条 warn admin_action_error，实得 events=%v", logger.events)
	}
	fields := logger.warns[0]
	if fields["resource"] != "provider" || fields["status"] != http.StatusBadRequest {
		t.Fatalf("日志字段缺 resource/status：%v", fields)
	}
	text, _ := fields["error"].(string)
	if !strings.Contains(text, "i/o timeout") {
		t.Fatalf("日志必须含成因原文，实得 %q", text)
	}
	if strings.Contains(text, "sk-abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("日志里的密钥必须被遮蔽，实得 %q", text)
	}
	if fields["redacted"] != true {
		t.Fatalf("发生遮蔽时必须带 redacted 标记：%v", fields)
	}
	// 响应体：公开文案，且不含成因片段。
	if detail, _ := body["detail"].(string); detail != "Bad request" {
		t.Fatalf("detail 必须是公开常量，实得 %q", detail)
	}
	if strings.Contains(body["instance"].(string), "i/o timeout") {
		t.Fatalf("响应体不得夹带成因：%v", body)
	}
}

// TestProblemActionErrorSkipsLogWithoutCause 钉住「码表驱动的 4xx 不记日志」：
// 那类 400/404 是正常拒绝（不存在、校验失败），没有成因可报，记了只是噪声。
func TestProblemActionErrorSkipsLogWithoutCause(t *testing.T) {
	logger := &recordingProblemsLogger{}
	problems := NewProblems(logger)
	status, _, _ := serveProblem(t, "/api/v1/providers/404",
		func(writer http.ResponseWriter, request *http.Request) {
			problems.WriteActionError(writer, request,
				NewActionError("provider", "NOT_FOUND", http.StatusNotFound, nil))
		})
	if status != http.StatusNotFound {
		t.Fatalf("应 404，实得 %d", status)
	}
	if len(logger.events) != 0 {
		t.Fatalf("无底层 error 的 4xx 不应记日志，实得 %v", logger.events)
	}
}

// TestProblemActionErrorKeeps5xxEventName 钉住 5xx 的事件名不因这次改动而漂移
// （既有日志检索以 admin_action_error_500 为锚）。
func TestProblemActionErrorKeeps5xxEventName(t *testing.T) {
	logger := &recordingProblemsLogger{}
	problems := NewProblems(logger)
	status, _, _ := serveProblem(t, "/api/v1/providers/149",
		func(writer http.ResponseWriter, request *http.Request) {
			problems.WriteActionError(writer, request,
				NewActionError("provider", "provider.action_failed", http.StatusInternalServerError,
					errors.New("pg: connection refused")))
		})
	if status != http.StatusInternalServerError {
		t.Fatalf("应 500，实得 %d", status)
	}
	if len(logger.events) != 1 || logger.events[0] != "admin_action_error_500" {
		t.Fatalf("5xx 事件名必须保持 admin_action_error_500，实得 %v", logger.events)
	}
	if text, _ := logger.warns[0]["error"].(string); !strings.Contains(text, "pg: connection refused") {
		t.Fatalf("5xx 日志必须含成因原文，实得 %q", text)
	}
}

// TestProblemActionErrorToleratesNilConcreteLogger 钉住「Deps 没接日志器时 4xx 也不炸」。
//
// 现场：handler 用 Deps{} 装配时 `NewProblems(deps.Logger)` 会把 nil 的 *logx.Logger 装进
// Problems.Logger 接口——接口非 nil、调用即 SIGSEGV。4xx 开始记日志之前这条路径不会被走到。
func TestProblemActionErrorToleratesNilConcreteLogger(t *testing.T) {
	var nilLogger *logx.Logger
	problems := NewProblems(nilLogger)
	status, _, body := serveProblem(t, "/api/v1/providers/149",
		func(writer http.ResponseWriter, request *http.Request) {
			problems.WriteActionError(writer, request,
				adminActionFailure("provider", errors.New("redis: i/o timeout")))
		})
	if status != http.StatusBadRequest || body["errorCode"] != "provider.action_failed" {
		t.Fatalf("应 400 provider.action_failed，实得 %d %v", status, body)
	}
}
