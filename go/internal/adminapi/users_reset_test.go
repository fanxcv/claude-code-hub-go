package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/usersreset"
)

// 本文件钉住两条统计重置路由的对外形状：注册面（两条、admin 档、module=users）、
// resetId 的 uuid 校验（400）、以及「队列未装配即整组不注册」（回退 Node）。
//
// 202/200/404 的端到端断言在 users_reset_integration_test.go（需要真库与真 Redis）。

// stubUsersResetQueue 是队列缝的替身。
type stubUsersResetQueue struct {
	record   usersreset.PublicRecord
	findErr  error
	enqErr   error
	findCall int
	lastUser int64
}

func (s *stubUsersResetQueue) Enqueue(_ context.Context, userID int64) (usersreset.PublicRecord, error) {
	s.lastUser = userID
	if s.enqErr != nil {
		return usersreset.PublicRecord{}, s.enqErr
	}
	return s.record, nil
}

func (s *stubUsersResetQueue) Find(
	_ context.Context,
	_ int64,
	_ string,
) (*usersreset.PublicRecord, error) {
	s.findCall++
	if s.findErr != nil {
		return nil, s.findErr
	}
	if s.record.ResetID == "" {
		return nil, nil
	}
	return &s.record, nil
}

// TestRegisterUsersResetRoutesShape 钉住注册面：两条路由、admin 档、module=users、操作 id 与 Node 一致。
func TestRegisterUsersResetRoutesShape(t *testing.T) {
	guard := &recordingGuard{}
	deps := Deps{Guard: guard, Store: &store.Pools{}, Problems: NewProblems(nil),
		UsersReset: &stubUsersResetQueue{}}
	router := New(Options{Deps: deps})
	RegisterUsersResetRoutes(router, deps)

	want := map[string]string{
		"POST /users/{id:[0-9]+}/statistics:reset":           "resetUserStatistics",
		"GET /users/{id:[0-9]+}/statistics-resets/{resetId}": "getUserStatisticsReset",
	}
	found := map[string]string{}
	for _, route := range router.routes {
		key := route.route.Method + " " + route.route.Path
		if _, ok := want[key]; !ok {
			t.Fatalf("注册了意料之外的路由：%s", key)
		}
		found[key] = route.route.OperationID
		if route.route.Access != AccessAdmin {
			t.Fatalf("%s 的权限档位应为 admin，实际 %s", key, route.route.Access)
		}
		if route.route.Module != "users" {
			t.Fatalf("%s 的模块应为 users，实际 %s", key, route.route.Module)
		}
	}
	for key, op := range want {
		if got, ok := found[key]; !ok {
			t.Fatalf("缺少路由 %s（已注册 %v）", key, found)
		} else if got != op {
			t.Fatalf("%s 的 operationId 应为 %s，实际 %s", key, op, got)
		}
	}
	// 守卫必须按 admin 档位包装两条路由（档位错了等于放宽或收紧权限）。
	if len(guard.levels) != 2 {
		t.Fatalf("两条路由都应过守卫，实际包装 %d 条", len(guard.levels))
	}
	for _, level := range guard.levels {
		if level != AccessAdmin {
			t.Fatalf("包装档位应为 admin，实际 %s", level)
		}
	}
}

// TestRegisterUsersResetRoutesUnwired 钉住「队列未装配即整组不注册」（回退 Node）。
func TestRegisterUsersResetRoutesUnwired(t *testing.T) {
	guard := &recordingGuard{}
	deps := Deps{Guard: guard, Store: &store.Pools{}}
	router := New(Options{Deps: deps})
	RegisterUsersResetRoutes(router, deps)
	if router.RouteCount() != 0 {
		t.Fatalf("队列未装配时不得注册任何路由，实际 %d 条", router.RouteCount())
	}
	// 缺连接池同理。
	deps2 := Deps{Guard: guard, UsersReset: &stubUsersResetQueue{}}
	router2 := New(Options{Deps: deps2})
	RegisterUsersResetRoutes(router2, deps2)
	if router2.RouteCount() != 0 {
		t.Fatalf("连接池未装配时不得注册任何路由，实际 %d 条", router2.RouteCount())
	}
}

// TestUsersResetStatusRejectsNonUUID 钉住 resetId 的 uuid 校验（Node 的 z.string().uuid() → 400）。
func TestUsersResetStatusRejectsNonUUID(t *testing.T) {
	guard := &recordingGuard{}
	stub := &stubUsersResetQueue{}
	deps := Deps{Guard: guard, Store: &store.Pools{}, Problems: NewProblems(nil), UsersReset: stub}
	router := New(Options{Deps: deps})
	RegisterUsersResetRoutes(router, deps)

	request := httptest.NewRequest(http.MethodGet, MountPrefix+"/users/7/statistics-resets/not-a-uuid", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("非法 uuid 应得 400，实际 %d", recorder.Code)
	}
	var body struct {
		ErrorCode     string `json:"errorCode"`
		InvalidParams []struct {
			Path []any  `json:"path"`
			Code string `json:"code"`
		} `json:"invalidParams"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 400 正文失败: %v（%s）", err, recorder.Body.String())
	}
	if body.ErrorCode != "request.validation_failed" {
		t.Fatalf("错误码应为 request.validation_failed，实际 %s", body.ErrorCode)
	}
	if len(body.InvalidParams) != 1 || len(body.InvalidParams[0].Path) != 1 ||
		body.InvalidParams[0].Path[0] != "resetId" {
		t.Fatalf("invalidParams 应指出 resetId：%s", recorder.Body.String())
	}
	if stub.findCall != 0 {
		t.Fatal("校验失败时不得触碰队列")
	}
}

// TestUsersResetStatusMapsQueueErrors 钉住查询的失败映射：依赖不可用 → 503 dependency.unavailable。
func TestUsersResetStatusMapsQueueErrors(t *testing.T) {
	guard := &recordingGuard{}
	stub := &stubUsersResetQueue{findErr: errors.New("redis down")}
	deps := Deps{Guard: guard, Store: &store.Pools{}, Problems: NewProblems(nil), UsersReset: stub}
	router := New(Options{Deps: deps})
	RegisterUsersResetRoutes(router, deps)

	request := httptest.NewRequest(
		http.MethodGet,
		MountPrefix+"/users/7/statistics-resets/0b9f1c2e-3d4a-4b5c-8d6e-7f8091a2b3c4",
		nil,
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("依赖不可用应得 503，实际 %d（%s）", recorder.Code, recorder.Body.String())
	}
	var body struct {
		ErrorCode string `json:"errorCode"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析正文失败: %v", err)
	}
	if body.ErrorCode != "dependency.unavailable" {
		t.Fatalf("错误码应为 dependency.unavailable，实际 %s", body.ErrorCode)
	}
}

// TestUsersResetStatusNotFound 钉住 404 的形状与错误码（Node 的 user.statistics_reset_not_found）。
func TestUsersResetStatusNotFound(t *testing.T) {
	guard := &recordingGuard{}
	stub := &stubUsersResetQueue{}
	deps := Deps{Guard: guard, Store: &store.Pools{}, Problems: NewProblems(nil), UsersReset: stub}
	router := New(Options{Deps: deps})
	RegisterUsersResetRoutes(router, deps)

	request := httptest.NewRequest(
		http.MethodGet,
		MountPrefix+"/users/7/statistics-resets/0b9f1c2e-3d4a-4b5c-8d6e-7f8091a2b3c4",
		nil,
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("查不到应得 404，实际 %d", recorder.Code)
	}
	var body struct {
		ErrorCode string `json:"errorCode"`
		Detail    string `json:"detail"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析正文失败: %v", err)
	}
	if body.ErrorCode != "user.statistics_reset_not_found" {
		t.Fatalf("错误码应为 user.statistics_reset_not_found，实际 %s", body.ErrorCode)
	}
	if body.Detail != "Not found" {
		t.Fatalf("detail 应为 problemTitles[404]，实际 %q", body.Detail)
	}
}
