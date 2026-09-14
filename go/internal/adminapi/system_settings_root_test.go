package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住根级 `GET /api/system-settings` 的契约（Node：src/app/api/system-settings/route.ts）：
//
//  1. 正文与 `/api/v1/system/settings` **同源**（同一个 buildSystemSettingsBody），含三处归一化；
//  2. 只要「已认证」即可（AccessRead，无角色门槛——Node 该路由只看有没有会话）；
//  3. 不在 /api/v1 应用壳下，故不发管理面 X-API-Version 信封头（与 /api/version 同判）；
//  4. Store 未装配时不注册（回退 Node，与本包同一纪律）；
//  5. 库故障时是裸 JSON 500 `{"error":"获取系统设置失败"}`（Node 的形状）。

type stubRootSettingsPools struct {
	row  *store.AdminSystemSettings
	err  error
	call int
}

func (s *stubRootSettingsPools) EnsureAdminSystemSettings(context.Context) (*store.AdminSystemSettings, error) {
	s.call++
	if s.err != nil {
		return nil, s.err
	}
	return s.row, nil
}

func newRootSettingsRouter(t *testing.T, pools rootSystemSettingsPools) *Router {
	t.Helper()
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	if pools == nil {
		RegisterRootSystemSettingsRoute(router, Deps{})
		return router
	}
	api := &rootSystemSettingsAPI{pools: pools, now: func() time.Time {
		return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	}, logger: logx.New(nil)}
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/system-settings",
		Access:               AccessRead,
		Module:               "system",
		OperationID:          "getRootSystemSettings",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleGet),
	})
	return router
}

func TestRootSystemSettingsReturnsSameBodyAsV1(t *testing.T) {
	pools := &stubRootSettingsPools{row: &store.AdminSystemSettings{
		ID:              7,
		SiteTitle:       "CC Hub",
		CurrencyDisplay: "USD",
		// 故意越界：验证归一化仍然生效（与 /api/v1/system/settings 同一条代码路径）。
		LegacyHedgeMaxInFlight: 9,
	}}
	router := newRootSettingsRouter(t, pools)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/system-settings", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("应 200，收到 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-API-Version"); got != "" {
		t.Fatalf("/api/system-settings 在 Node 侧不在管理面应用壳内，不该发 X-API-Version，收到 %q", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("正文类型应为 application/json; charset=utf-8，收到 %q", got)
	}

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文不是合法 JSON: %v", err)
	}
	if body["siteTitle"] != "CC Hub" || body["currencyDisplay"] != "USD" {
		t.Fatalf("逐字段正文未透传: %v", body)
	}
	// 归一化一：legacyHedgeMaxInFlight 夹到 1..4（transformer 的边界语义）。
	if got, ok := body["legacyHedgeMaxInFlight"].(float64); !ok || got != 2 {
		t.Fatalf("legacyHedgeMaxInFlight 越界应归一为 2，收到 %v", body["legacyHedgeMaxInFlight"])
	}
	// 归一化二：responseFixerConfig 缺列时补默认对象（半做会静默丢字段，正是本钉子的意义）。
	fixer, ok := body["responseFixerConfig"].(map[string]any)
	if !ok {
		t.Fatalf("responseFixerConfig 应是对象（缺列补默认），收到 %T", body["responseFixerConfig"])
	}
	if fixer["fixTruncatedJson"] != true || fixer["maxJsonDepth"].(float64) != 200 {
		t.Fatalf("responseFixerConfig 默认对象不符: %v", fixer)
	}
	if pools.call != 1 {
		t.Fatalf("应只查一次库，实际 %d", pools.call)
	}
}

func TestRootSystemSettingsFailureIsRawJSON(t *testing.T) {
	router := newRootSettingsRouter(t, &stubRootSettingsPools{err: errors.New("boom")})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/system-settings", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("库故障应 500，收到 %d", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文不是合法 JSON: %v", err)
	}
	if body["error"] != "获取系统设置失败" {
		t.Fatalf("故障文案应与 Node 同形，收到 %v", body["error"])
	}
}

func TestRootSystemSettingsNotRegisteredWithoutStore(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterRootSystemSettingsRoute(router, Deps{})
	if count := router.RouteCount(); count != 0 {
		t.Fatalf("Store 未装配时不该注册任何路由（回退 Node），收到 %d 条", count)
	}
}

func TestRootSystemSettingsRouteMetadataAndUnwired(t *testing.T) {
	// 走真实注册路径：只校验路由元数据（Access/路径/信封标记），不触发库调用。
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterRootSystemSettingsRoute(router, Deps{Store: &store.Pools{}})

	var found *Route
	for _, route := range router.RouteList() {
		if route.Path == "/api/system-settings" {
			route := route
			found = &route
			break
		}
	}
	if found == nil {
		t.Fatal("Store 装配时该路由应注册")
	}
	if found.Method != http.MethodGet {
		t.Fatalf("方法应为 GET，收到 %s", found.Method)
	}
	if found.Access != AccessRead {
		t.Fatalf("Node 该路由只要会话即可（无角色门槛），Access 应为 read，收到 %q", found.Access)
	}
	if !found.NoManagementEnvelope {
		t.Fatal("该路由在 /api/v1 应用壳外，应不发管理面信封头")
	}
}
