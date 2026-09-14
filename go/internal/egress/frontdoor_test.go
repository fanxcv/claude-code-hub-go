package egress

import (
	"context"
	"net/http"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"/v1/messages":       "/v1/messages",
		"/v1/messages/":      "/v1/messages",
		"/v1//messages":      "/v1/messages",
		"/v1///messages//":   "/v1/messages",
		"/v1/messages?x=1":   "/v1/messages",
		"":                   "/",
		"/":                  "/",
		"//":                 "/",
		"/v1/messages?a=//b": "/v1/messages",
	}
	for input, expected := range cases {
		if got := NormalizePath(input); got != expected {
			t.Fatalf("NormalizePath(%q) = %q, 期望 %q", input, got, expected)
		}
	}
}

func TestMiddlewarePassesRequestsThrough(t *testing.T) {
	frontDoor := newTestFrontDoor(t)
	called := false
	handler := frontDoor.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}))

	recorder := newRecorder()
	handler.ServeHTTP(recorder, newRequest(http.MethodPost, "/v1/responses", nil))

	if !called {
		t.Fatal("中间件必须把请求交给 next：Node 退役后本进程承载全部路径，不再有归属判定")
	}
	if recorder.Code != http.StatusTeapot {
		t.Fatalf("next 的响应应原样透出，得到 %d", recorder.Code)
	}
	if frontDoor.InFlight() != 0 {
		t.Fatalf("请求结束后在途计数应归零，得到 %d", frontDoor.InFlight())
	}
}

func TestUnimplementedReturnsNotFoundJSON(t *testing.T) {
	frontDoor := newTestFrontDoor(t)
	recorder := newRecorder()
	frontDoor.Unimplemented().ServeHTTP(recorder, newRequest(http.MethodPost, "/api/admin/database/export", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未实现路径应回 404，得到 %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type 应为 JSON，得到 %q", got)
	}
	if body := recorder.Body.String(); body != `{"error":"not_found"}` {
		t.Fatalf("错误体应为 not_found，得到 %s", body)
	}
}

func TestFrontDoorDrainWaitsForInFlight(t *testing.T) {
	frontDoor := newTestFrontDoor(t)
	release, entered, done := enterFrontDoor(t, frontDoor)

	<-entered
	if frontDoor.InFlight() != 1 {
		t.Fatalf("在途计数应为 1，得到 %d", frontDoor.InFlight())
	}

	drained := make(chan error, 1)
	go func() { drained <- frontDoor.Drain(context.Background()) }()

	select {
	case err := <-drained:
		t.Fatalf("在途未结束前 Drain 不应返回，却返回 %v", err)
	default:
	}

	release()
	<-done
	if err := <-drained; err != nil {
		t.Fatalf("在途结束后 Drain 应成功: %v", err)
	}
	if !frontDoor.Draining() {
		t.Fatal("排空后 Draining 应为 true")
	}
}

func TestFrontDoorRejectsNewRequestsWhileDraining(t *testing.T) {
	frontDoor := newTestFrontDoor(t)
	if err := frontDoor.Drain(context.Background()); err != nil {
		t.Fatalf("无在途请求时 Drain 应立刻成功: %v", err)
	}

	recorder := newRecorder()
	frontDoor.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("排空窗口内不应把请求交给处理器")
	})).ServeHTTP(recorder, newRequest(http.MethodPost, "/v1/responses", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("排空窗口内应返回 503，得到 %d", recorder.Code)
	}
	// 错误码与旧版不同：旧版叫 backend_switching（node/go 切换期），现在排空只发生在
	// 本进程退出前，故如实报 shutting_down。
	if body := recorder.Body.String(); body != `{"error":"shutting_down"}` {
		t.Fatalf("错误体应为 shutting_down，得到 %s", body)
	}
}
