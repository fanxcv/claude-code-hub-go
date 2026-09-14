package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件覆盖两条批量成本端点的**校验**与**信封形状**。
//
// 校验部分不依赖数据库：直接构造 `costBatchAPI` 调处理器（注册器在缺 Store 时整组不注册，
// 走注册器反而测不到校验分支）。取数正确性在 `internal/store` 的集成测试里对账——
// 那边用「手写 SQL 复刻 Node 的分支语义」与 Go 的单查询实现互证。

func costBatchTestAPI() *costBatchAPI {
	return &costBatchAPI{deps: Deps{}, problems: adminProblemWriter(Deps{})}
}

func costBatchPost(t *testing.T, api *costBatchAPI, body string, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/users:costBatch", strings.NewReader(body))
	if contentType == "" {
		contentType = "application/json"
	}
	request.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	api.handleUserCostBatch(recorder, request)
	return recorder
}

// TestCostBatchValidation 逐条钉住入参校验：空/超限/类型错/未知键/坏 JSON/错 Content-Type。
func TestCostBatchValidation(t *testing.T) {
	api := costBatchTestAPI()

	cases := []struct {
		name        string
		body        string
		contentType string
		status      int
		wantCode    string
		wantMessage string
	}{
		{name: "缺 ids", body: `{}`, status: http.StatusBadRequest, wantCode: "invalid_type", wantMessage: "Required"},
		{name: "空数组", body: `{"ids":[]}`, status: http.StatusBadRequest, wantCode: "too_small",
			wantMessage: "Array must contain at least 1 element(s)"},
		{name: "ids 非数组", body: `{"ids":1}`, status: http.StatusBadRequest, wantCode: "invalid_type"},
		{name: "ids 含小数", body: `{"ids":[1.5]}`, status: http.StatusBadRequest, wantCode: "invalid_type"},
		{name: "ids 含 0", body: `{"ids":[0]}`, status: http.StatusBadRequest, wantCode: "invalid_type"},
		{name: "ids 含负数", body: `{"ids":[-3]}`, status: http.StatusBadRequest, wantCode: "invalid_type"},
		{name: "未知键", body: `{"ids":[1],"foo":1}`, status: http.StatusBadRequest, wantCode: "unrecognized_keys"},
		{name: "坏 JSON", body: `{"ids":[1`, status: http.StatusBadRequest, wantCode: "request.malformed_json"},
		{name: "顶层非对象", body: `null`, status: http.StatusBadRequest, wantCode: "invalid_type"},
		{name: "错 Content-Type", body: `{"ids":[1]}`, contentType: "text/plain",
			status: http.StatusUnsupportedMediaType},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := costBatchPost(t, api, testCase.body, testCase.contentType)
			if recorder.Code != testCase.status {
				t.Fatalf("状态码 = %d, want %d（响应 %s）", recorder.Code, testCase.status, recorder.Body.String())
			}
			if testCase.wantCode != "" && !strings.Contains(recorder.Body.String(), testCase.wantCode) {
				t.Fatalf("响应应含错误码 %q，实际 %s", testCase.wantCode, recorder.Body.String())
			}
			if testCase.wantMessage != "" && !strings.Contains(recorder.Body.String(), testCase.wantMessage) {
				t.Fatalf("响应应含文案 %q，实际 %s", testCase.wantMessage, recorder.Body.String())
			}
		})
	}
}

// TestCostBatchTooBigIsExplicit 单独钉上限：**超限必须 400 且明说上限**，不得静默截断。
//
// 静默截断是本次要消灭的那类缺陷：界面会把「没查到的实体」显示成 0，也就是一个确定的错数。
func TestCostBatchTooBigIsExplicit(t *testing.T) {
	api := costBatchTestAPI()
	ids := make([]string, 0, costBatchMaxIDs+1)
	for index := 0; index <= costBatchMaxIDs; index++ {
		ids = append(ids, "1")
	}
	recorder := costBatchPost(t, api, `{"ids":[`+strings.Join(ids, ",")+`]}`, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("超限应为 400，实际 %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "too_big") ||
		!strings.Contains(recorder.Body.String(), "at most 200 element(s)") {
		t.Fatalf("超限响应应含 too_big 与上限文案，实际 %s", recorder.Body.String())
	}
}

// TestCostBatchDuplicateIDsAreDeduplicated 钉去重与升序：前端分片时可能重复投递同一 id。
//
// 不去重的不良后果是「响应里出现重复 id」——前端按 id 建 Map 时后者覆盖前者，不报错却难查。
// 这里用真库跑（顺带覆盖信封形状），未设 DSN 即跳过。
func TestCostBatchDuplicateIDsAreDeduplicated(t *testing.T) {
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:      dsn,
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	deps := Deps{Guard: &recordingGuard{}, Store: pools}
	router := New(Options{Deps: deps})
	RegisterCostBatchRoutes(router, deps)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/users:costBatch",
		strings.NewReader(`{"ids":[7,3,7,3]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200（响应 %s）", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Items []struct {
			ID        int64  `json:"id"`
			TotalCost string `json:"totalCost"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是预期信封: %v（%s）", err, recorder.Body.String())
	}
	if len(body.Items) != 2 {
		t.Fatalf("去重后应有 2 条，实际 %d: %s", len(body.Items), recorder.Body.String())
	}
	if body.Items[0].ID != 3 || body.Items[1].ID != 7 {
		t.Fatalf("应按 id 升序输出，实际 %v", body.Items)
	}
	// totalCost 是 numeric 文本（不在取数层转 float64）——两个不存在的 id 都应为 "0"。
	for _, item := range body.Items {
		if item.TotalCost != "0" {
			t.Fatalf("不存在的用户 id %d 应为 \"0\"，实际 %q", item.ID, item.TotalCost)
		}
	}
}

// TestRegisterCostBatchRoutesRegistersBothDimensions 钉注册器：两条路由都要在，且缺 Store 时整组不注册。
func TestRegisterCostBatchRoutesRegistersBothDimensions(t *testing.T) {
	deps := Deps{Guard: &recordingGuard{}, Store: &store.Pools{}}
	router := New(Options{Deps: deps})
	RegisterCostBatchRoutes(router, deps)

	paths := map[string]bool{}
	for _, route := range router.RouteList() {
		paths[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{"POST /users:costBatch", "POST /keys:costBatch"} {
		if !paths[want] {
			t.Fatalf("缺路由 %s，实际 %v", want, router.RouteList())
		}
	}

	bare := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterCostBatchRoutes(bare, Deps{Guard: &recordingGuard{}})
	if count := bare.RouteCount(); count != 0 {
		t.Fatalf("缺 Store 时不应注册任何路由（回退 Node），实际 %d 条", count)
	}
}
