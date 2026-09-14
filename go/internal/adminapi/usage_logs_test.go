package adminapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 usage-logs 的单元测试：查询参数解析的 zod 语义、行渲染的键序与派生字段、
// 缓存指标与统一 specialSettings 的移植、游标编码，以及路由注册的条数与权限档位。
//
// 集成（真实 PG）用例在 usage_logs_integration_test.go。

func TestParseUsageLogsQueryZodSemantics(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		wantLimit  int
		wantIssues int
		issuePath  string
		issueCode  string
	}{
		{name: "缺省 limit 取 20", target: "/api/v1/usage-logs", wantLimit: 20},
		{name: "空串按 coerce 为 0 而越界", target: "/api/v1/usage-logs?limit=", wantIssues: 1,
			issuePath: "limit", issueCode: "too_small"},
		{name: "非数值", target: "/api/v1/usage-logs?limit=abc", wantIssues: 1,
			issuePath: "limit", issueCode: "invalid_type"},
		{name: "上界", target: "/api/v1/usage-logs?limit=101", wantIssues: 1,
			issuePath: "limit", issueCode: "too_big"},
		{name: "合法值", target: "/api/v1/usage-logs?limit=100", wantLimit: 100},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, testCase.target, nil)
			query, issues := parseUsageLogsQuery(request)
			if len(issues) != testCase.wantIssues {
				t.Fatalf("issues 数应为 %d，实际 %d: %+v", testCase.wantIssues, len(issues), issues)
			}
			if testCase.wantIssues > 0 {
				if issues[0].Path[0] != testCase.issuePath || issues[0].Code != testCase.issueCode {
					t.Fatalf("issue 不符: %+v", issues[0])
				}
				return
			}
			if query.Limit != testCase.wantLimit {
				t.Fatalf("limit 应为 %d，实际 %d", testCase.wantLimit, query.Limit)
			}
		})
	}
}

func TestParseUsageLogsQueryCursorAndEnums(t *testing.T) {
	// 游标只在两个分量都给出时装配。
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/usage-logs?cursorCreatedAt=2026-01-02T03:04:05.000Z", nil)
	query, issues := parseUsageLogsQuery(request)
	if len(issues) != 0 || query.Cursor != nil {
		t.Fatalf("只给 cursorCreatedAt 不应构成游标: %+v issues=%+v", query.Cursor, issues)
	}

	request = httptest.NewRequest(http.MethodGet,
		"/api/v1/usage-logs?cursorCreatedAt=2026-01-02T03:04:05.000Z&cursorId=42", nil)
	query, issues = parseUsageLogsQuery(request)
	if len(issues) != 0 {
		t.Fatalf("游标参数应合法: %+v", issues)
	}
	if query.Cursor == nil || query.Cursor.ID != 42 ||
		query.Cursor.CreatedAt != "2026-01-02T03:04:05.000Z" {
		t.Fatalf("游标装配不符: %+v", query.Cursor)
	}

	// replayFilter 非法值。
	request = httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs?replayFilter=bogus", nil)
	_, issues = parseUsageLogsQuery(request)
	if len(issues) != 1 || issues[0].Code != "invalid_enum_value" {
		t.Fatalf("非法 replayFilter 应报 invalid_enum_value: %+v", issues)
	}

	// 布尔档位只认 true/false。
	request = httptest.NewRequest(http.MethodGet,
		"/api/v1/usage-logs?excludeStatusCode200=yes", nil)
	_, issues = parseUsageLogsQuery(request)
	if len(issues) != 1 || issues[0].Code != "invalid_union" {
		t.Fatalf("非法布尔应报 invalid_union: %+v", issues)
	}

	// 合法布尔。
	request = httptest.NewRequest(http.MethodGet,
		"/api/v1/usage-logs?excludeStatusCode200=true&actualResponseModelMismatch=false", nil)
	query, issues = parseUsageLogsQuery(request)
	if len(issues) != 0 {
		t.Fatalf("合法布尔不应报错: %+v", issues)
	}
	if query.StatusExcl == nil || !*query.StatusExcl || query.Mismatch {
		t.Fatalf("布尔解析不符: exclude=%v mismatch=%v", query.StatusExcl, query.Mismatch)
	}
}

func TestDeriveRequestCacheMetrics(t *testing.T) {
	ulIntPtr := func(value int64) *int64 { return &value }
	boolPtr := func(value bool) *bool { return &value }
	stringPtr := func(value string) *string { return &value }

	cases := []struct {
		name             string
		input            ulCacheMetricsInput
		wantAvailability string
		wantCoefficient  any
	}{
		{
			name:             "无 F3b 字段即 not_recorded",
			input:            ulCacheMetricsInput{input: ulIntPtr(100)},
			wantAvailability: "not_recorded",
		},
		{
			name:             "有 F3b 但无输入即 no_input",
			input:            ulCacheMetricsInput{theoretical: ulIntPtr(50)},
			wantAvailability: "no_input",
		},
		{
			name: "无理论值即 no_affinity_key",
			input: ulCacheMetricsInput{
				input: ulIntPtr(100), cacheRead: ulIntPtr(40), eligible: boolPtr(true),
			},
			wantAvailability: "no_affinity_key",
		},
		{
			name: "可算即 available 且系数按 bp 截断",
			input: ulCacheMetricsInput{
				input: ulIntPtr(100), cacheRead: ulIntPtr(40), theoretical: ulIntPtr(80),
				eligible: boolPtr(true),
			},
			wantAvailability: "available",
			wantCoefficient:  int64(5000),
		},
		{
			name: "排除理由优先且不给系数",
			input: ulCacheMetricsInput{
				input: ulIntPtr(100), cacheRead: ulIntPtr(40), theoretical: ulIntPtr(80),
				reason: stringPtr("stream_truncated"),
			},
			wantAvailability: "stream_truncated",
			wantCoefficient:  nil,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			metrics := deriveRequestCacheMetrics(
				testCase.input.input, testCase.input.cacheCreation, testCase.input.cacheRead,
				testCase.input.theoretical, testCase.input.eligible, testCase.input.reason,
			)
			if string(metrics.RequestCacheMetricAvailability) != testCase.wantAvailability {
				t.Fatalf("availability 应为 %q，实际 %q", testCase.wantAvailability,
					metrics.RequestCacheMetricAvailability)
			}
			if metrics.RequestCacheCoefficientBP != testCase.wantCoefficient {
				t.Fatalf("系数应为 %v，实际 %v", testCase.wantCoefficient,
					metrics.RequestCacheCoefficientBP)
			}
		})
	}
}

// ulCacheMetricsInput 是缓存指标用例的输入集合（避免长参数列表）。
type ulCacheMetricsInput struct {
	input         *int64
	cacheCreation *int64
	cacheRead     *int64
	theoretical   *int64
	eligible      *bool
	reason        *string
}

func TestUnionSettingDerivesAndDedupes(t *testing.T) {
	blocked := "sensitive_word"
	reason := `{"rule":"x"}`
	status := 403
	ttl := "5m"

	settings := unionSetting(nil, &blocked, &reason, &status, &ttl)
	if len(settings) != 2 {
		t.Fatalf("应派出两条统一设置，实际 %d: %+v", len(settings), settings)
	}
	encoded, err := encodeJSONObject(settings[0])
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	want := `{"type":"guard_intercept","scope":"guard","hit":true,"guard":"sensitive_word",` +
		`"action":"block_request","statusCode":403,"reason":"{\"rule\":\"x\"}"}`
	if string(encoded) != want {
		t.Fatalf("guard_intercept 形状不符:\n实际 %s\n期望 %s", encoded, want)
	}
	encoded, err = encodeJSONObject(settings[1])
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	want = `{"type":"anthropic_cache_ttl_header_override","scope":"request_header","hit":true,` +
		`"ttl":"5m"}`
	if string(encoded) != want {
		t.Fatalf("ttl 覆写形状不符:\n实际 %s\n期望 %s", encoded, want)
	}

	// 去重：库里已有一条等价的 guard_intercept 时不再追加。
	existing := []map[string]any{{
		"type": "guard_intercept", "guard": "sensitive_word", "action": "block_request",
		"statusCode": json.Number("403"), "hit": true, "scope": "guard",
	}}
	settings = unionSetting(existing, &blocked, nil, &status, nil)
	if len(settings) != 1 {
		t.Fatalf("重复项应按类型+字段去重，实际 %d 条", len(settings))
	}

	// 没有任何来源时返回 nil（Node 的 null）。
	if unionSetting(nil, nil, nil, nil, nil) != nil {
		t.Fatal("无来源应返回 nil")
	}
}

func TestUsageLogRowFieldsKeyOrderAndDerived(t *testing.T) {
	created := time.Date(2026, 2, 3, 4, 5, 6, 789000000, time.UTC)
	input := int64(100)
	output := int64(20)
	cacheRead := int64(40)
	theoretical := int64(80)
	eligible := true
	status := 200
	seq := int64(7)
	sessionID := "sid:abc"
	model := "claude-x"
	cost := "0.001234"
	payload := `[{"type":"anthropic_effort","hit":true,"effort":" high "}]`

	row := store.UsageLogRow{
		ID:                     9,
		CreatedAt:              &created,
		SessionID:              &sessionID,
		RequestSequence:        &seq,
		Model:                  &model,
		StatusCode:             &status,
		InputTokens:            &input,
		OutputTokens:           &output,
		CacheReadInputTokens:   &cacheRead,
		TheoreticalCacheTokens: &theoretical,
		CacheScoreEligible:     &eligible,
		CostUSD:                &cost,
		SpecialSettings:        []byte(payload),
		CostBreakdown:          []byte(`{"total":0.001234}`),
	}

	fields := usageLogRowFields(row, true)
	encoded, err := fields.marshalJSON()
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}

	// 键序：select 顺序（createdAtRaw 紧随 createdAt）+ 末尾派生字段。
	order := []string{
		`"id"`, `"createdAt"`, `"createdAtRaw"`, `"sessionId"`, `"sourceSessionId"`,
		`"sessionIdentityKind"`, `"requestSequence"`, `"userName"`, `"keyName"`, `"providerName"`,
		`"model"`, `"originalModel"`, `"actualResponseModel"`, `"endpoint"`, `"statusCode"`,
		`"inputTokens"`, `"outputTokens"`, `"cacheCreationInputTokens"`, `"cacheReadInputTokens"`,
		`"cacheCreation5mInputTokens"`, `"cacheCreation1hInputTokens"`, `"cacheTtlApplied"`,
		`"theoreticalCacheTokens"`, `"cacheScoreEligible"`, `"cacheScoreExcludedReason"`,
		`"costUsd"`, `"costMultiplier"`, `"groupCostMultiplier"`, `"costBreakdown"`,
		`"hedgeLosers"`, `"durationMs"`, `"ttftMs"`, `"firstByteMs"`, `"errorMessage"`,
		`"providerChain"`, `"routingTrace"`, `"blockedBy"`, `"blockedReason"`, `"isReplay"`,
		`"replaySourceRequestId"`, `"userAgent"`, `"clientIp"`, `"messagesCount"`,
		`"context1mApplied"`, `"swapCacheTtlApplied"`, `"specialSettings"`, `"totalTokens"`,
		`"cacheInputTotal"`, `"actualCacheRate"`, `"theoreticalCacheRate"`,
		`"requestCacheCoefficientBp"`, `"requestCacheMetricAvailability"`, `"anthropicEffort"`,
	}
	text := string(encoded)
	position := -1
	for _, key := range order {
		index := strings.Index(text, key+":")
		if index < 0 {
			t.Fatalf("缺少键 %s：%s", key, text)
		}
		if index < position {
			t.Fatalf("键 %s 的顺序不符：%s", key, text)
		}
		position = index
	}

	// 派生值：totalTokens = 120+40（input+output+cacheRead+creation）、db 系数 5000、
	// anthropicEffort 去空白。
	ulAssertJSONContains(t, text, `"createdAt":"2026-02-03T04:05:06.789Z"`)
	ulAssertJSONContains(t, text, `"totalTokens":160`)
	ulAssertJSONContains(t, text, `"cacheInputTotal":140`)
	ulAssertJSONContains(t, text, `"cacheInputTotal":140`)
	ulAssertJSONContains(t, text, `"requestCacheCoefficientBp":5000`)
	ulAssertJSONContains(t, text, `"requestCacheMetricAvailability":"available"`)
	ulAssertJSONContains(t, text, `"anthropicEffort":"high"`)
	ulAssertJSONContains(t, text, `"costUsd":"0.001234"`)
	// 未取 createdAtRaw 的偏移路径不含该键。
	offsetEncoding, err := usageLogRowFields(row, false).marshalJSON()
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if strings.Contains(string(offsetEncoding), "createdAtRaw") {
		t.Fatalf("偏移路径不应含 createdAtRaw: %s", offsetEncoding)
	}
}

func TestUsageLogRowFieldsHandlesNulls(t *testing.T) {
	// 全空行：所有可空列都应是 null，且不出现 undefined 语义（Node 的列是 null 而非缺席）。
	encoded, err := usageLogRowFields(store.UsageLogRow{IsReplay: false}, false).marshalJSON()
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	text := string(encoded)
	ulAssertJSONContains(t, text, `"sessionId":null`)
	ulAssertJSONContains(t, text, `"costUsd":null`)
	ulAssertJSONContains(t, text, `"specialSettings":null`)
	ulAssertJSONContains(t, text, `"isReplay":false`)
	ulAssertJSONContains(t, text, `"totalTokens":0`)
	ulAssertJSONContains(t, text, `"requestCacheMetricAvailability":"not_recorded"`)
	ulAssertJSONContains(t, text, `"anthropicEffort":null`)
}

func TestLedgerFallbackRowFieldsFillsUserNameAndKeyName(t *testing.T) {
	userID := int64(12)
	key := "sk-raw"
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	fields := ledgerFallbackRowFields(store.LedgerUsageLogRow{
		ID:        3,
		CreatedAt: &created,
		UserID:    &userID,
		Key:       &key,
	})
	encoded, err := fields.marshalJSON()
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	text := string(encoded)
	// Node: userName = row.userName ?? `User #${row.userId}`，keyName = row.keyName ?? row.key。
	ulAssertJSONContains(t, text, `"userName":"User #12"`)
	ulAssertJSONContains(t, text, `"keyName":"sk-raw"`)
	ulAssertJSONContains(t, text, `"requestSequence":null`)
	ulAssertJSONContains(t, text, `"specialSettings":null`)
}

func TestNormalizeUsageLogsCursor(t *testing.T) {
	cursor := &store.UsageLogCursor{CreatedAt: "2026-01-02T03:04:05.000Z", ID: 42}
	encoded, _ := normalizeUsageLogsCursor(cursor).(string)
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("游标应是 base64url: %v", err)
	}
	want := `{"createdAt":"2026-01-02T03:04:05.000Z","id":42}`
	if string(decoded) != want {
		t.Fatalf("游标内层 JSON 不符:\n实际 %s\n期望 %s", decoded, want)
	}
	if normalizeUsageLogsCursor(nil) != nil {
		t.Fatal("无游标应为 null")
	}
}

func TestWriteValidationProblemShape(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs?limit=abc", nil)
	writeUsageLogsValidationProblem(recorder, request, []usageLogsValidationIssue{{
		Path: []any{"limit"}, Code: "invalid_type", Message: "Expected number, received nan",
	}})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态码应为 400，实际 %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != problemContentType {
		t.Fatalf("Content-Type 应为 %s，实际 %s", problemContentType, got)
	}
	ulAssertJSONContains(t, recorder.Body.String(), `"errorCode":"request.validation_failed"`)
	ulAssertJSONContains(t, recorder.Body.String(), `"title":"Validation failed"`)
	ulAssertJSONContains(t, recorder.Body.String(), `"invalidParams":[{"path":["limit"]`)
}

func TestRegisterUsageLogsRouteTable(t *testing.T) {
	router := New(Options{Deps: Deps{
		Guard:    ulStubGuard{},
		Problems: NewProblems(nil),
		Store:    &store.Pools{},
	}})
	RegisterUsageLogs(router, Deps{Guard: ulStubGuard{}, Store: &store.Pools{}})

	expected := map[string]AccessLevel{
		"GET /usage-logs":                        AccessRead,
		"GET /usage-logs/stats":                  AccessRead,
		"GET /usage-logs/filter-options":         AccessAdmin,
		"GET /usage-logs/models":                 AccessAdmin,
		"GET /usage-logs/status-codes":           AccessAdmin,
		"GET /usage-logs/endpoints":              AccessAdmin,
		"GET /usage-logs/session-id-suggestions": AccessRead,
	}
	routes := router.RouteList()
	if len(routes) != len(expected) {
		t.Fatalf("应注册 %d 条路由，实际 %d: %+v", len(expected), len(routes), routes)
	}
	for _, route := range routes {
		level, ok := expected[route.Method+" "+route.Path]
		if !ok {
			t.Fatalf("注册了未预期的路由: %s %s", route.Method, route.Path)
		}
		if route.Access != level {
			t.Fatalf("%s 的权限档位应为 %s，实际 %s", route.Path, level, route.Access)
		}
		if route.Module != "usage-logs" {
			t.Fatalf("模块名应为 usage-logs，实际 %s", route.Module)
		}
		if route.OperationID == "" {
			t.Fatalf("%s 缺少 OperationID", route.Path)
		}
	}
}

func TestRegisterUsageLogsWithoutStoreRegistersNothing(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: ulStubGuard{}}})
	RegisterUsageLogs(router, Deps{Guard: ulStubGuard{}})
	if router.RouteCount() != 0 {
		t.Fatalf("未装配连接池时不应注册路由，实际 %d 条", router.RouteCount())
	}
}

func TestLedgerOnlyCacheTTLAndFailureRetention(t *testing.T) {
	cache := &ledgerOnlyCache{}
	current := time.Unix(0, 0)
	cache.now = func() time.Time { return current }
	// 无池时恒 false（且不 panic）：与 Node「读设置失败沿用旧值」同向。
	if cache.ledgerOnly(httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs", nil)) {
		t.Fatal("无池时应为 false")
	}

	value := true
	cache.value = &value
	cache.expires = current.Add(ledgerOnlyCacheTTL)
	if !cache.ledgerOnly(httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs", nil)) {
		t.Fatal("缓存有效期内应沿用旧值")
	}
	current = current.Add(2 * ledgerOnlyCacheTTL)
	cache.pools = nil
	if cache.ledgerOnly(httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs", nil)) != true {
		t.Fatal("探测失败时应沿用上次结果而不是翻转")
	}
}

func TestWriteActionErrorStatusMapping(t *testing.T) {
	module := &usageLogsModule{deps: Deps{Logger: nil}}
	cases := []struct {
		message    string
		wantStatus int
	}{
		{message: "Export job not found or expired", wantStatus: http.StatusNotFound},
		{message: "记录不存在", wantStatus: http.StatusNotFound},
		{message: "没有权限", wantStatus: http.StatusForbidden},
		{message: "获取使用日志失败", wantStatus: http.StatusBadRequest},
	}
	for _, testCase := range cases {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs", nil)
		module.writeActionError(recorder, request, ulErrString(testCase.message))
		if recorder.Code != testCase.wantStatus {
			t.Fatalf("%q 应映射为 %d，实际 %d", testCase.message, testCase.wantStatus, recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != problemContentType {
			t.Fatalf("Content-Type 应为 %s，实际 %s", problemContentType, got)
		}
		ulAssertJSONContains(t, recorder.Body.String(), `"errorCode":"usage_logs.action_failed"`)
	}
}

// ulErrString 是只带消息的错误。
type ulErrString string

func (e ulErrString) Error() string { return string(e) }

// ulStubGuard 是注册用守卫：只把身份放进上下文，不做任何认证判定。
type ulStubGuard struct{}

func (ulStubGuard) Wrap(_ AccessLevel, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		next.ServeHTTP(writer, request.WithContext(WithPrincipal(request.Context(), Principal{
			UserID: 1, Username: "tester", IsAdmin: true,
		})))
	})
}

func ulAssertJSONContains(t *testing.T, payload, fragment string) {
	t.Helper()
	if !strings.Contains(payload, fragment) {
		t.Fatalf("响应缺少片段 %s\n实际: %s", fragment, payload)
	}
}

// 保证 context 包被使用（集成测试在本包内共用同一测试二进制）。
var _ = context.Background
