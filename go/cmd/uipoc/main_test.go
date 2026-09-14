package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResolvePriority 钉住「静态文件 > 目录 index > locale 壳 > 根壳」的择路优先级。
func TestResolvePriority(t *testing.T) {
	s, err := newServer()
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	cases := []struct {
		path string
		want string
	}{
		{"/", "index.html"},
		{"/next.svg", "next.svg"},
		{"/_next/static/chunks/43k39urre6rl6.js", "_next/static/chunks/43k39urre6rl6.js"},
		{"/en", "en/index.html"},
		{"/zh-CN/", "zh-CN/index.html"},
		// 带 locale 前缀的未知路径 → 该 locale 的壳，而不是根壳。
		{"/zh-CN/dashboard/providers", "zh-CN/index.html"},
		{"/en/dashboard/not-a-real-page", "en/index.html"},
		// 未知 locale 前缀 → 根壳（不 404）。
		{"/ja/dashboard", "index.html"},
		{"/no-such-file.js", "index.html"},
	}
	for _, tc := range cases {
		got, ok := s.resolve(tc.path)
		if !ok {
			t.Fatalf("resolve(%q) 未命中", tc.path)
		}
		if got != tc.want {
			t.Errorf("resolve(%q) = %q，期望 %q", tc.path, got, tc.want)
		}
	}
}

// TestServeHeaders 钉住 MIME 与缓存头：静态哈希产物 immutable，HTML 走协商缓存。
func TestServeHeaders(t *testing.T) {
	s, err := newServer()
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_next/static/chunks/43k39urre6rl6.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("静态产物状态码 = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("静态产物 Cache-Control = %q，期望含 immutable", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Errorf("静态产物 Content-Type = %q", got)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/zh-CN/dashboard", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("壳状态码 = %d", rec.Code)
	}
	if !strings.Contains(body, "zh-CN 壳") {
		t.Errorf("带 locale 前缀的未知路径未回退到该 locale 的壳")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("壳 Cache-Control = %q，期望 no-cache", got)
	}

	// 协商缓存：带上 ETag 应得 304 且无正文。
	etag := rec.Header().Get("ETag")
	req := httptest.NewRequest(http.MethodGet, "/zh-CN/dashboard", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("带 ETag 的复访状态码 = %d，期望 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 不应有正文，实得 %d 字节", rec.Body.Len())
	}
}

// TestRejectPaths 钉住路径穿越与非 GET/HEAD 的拒绝。
func TestRejectPaths(t *testing.T) {
	s, err := newServer()
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/../../etc/passwd", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("路径穿越状态码 = %d，期望 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/zh-CN", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST 状态码 = %d，期望 405", rec.Code)
	}
}
