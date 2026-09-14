package debugapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// openTestPlane 起一面剖析面并返回它的基址。
//
// 绑 127.0.0.1:0（内核分配端口）：测试不争固定端口，也不会碰到生产端口。
func openTestPlane(t *testing.T, options Options) (*Plane, string) {
	t.Helper()
	if options.Addr == "" {
		options.Addr = "127.0.0.1:0"
	}
	if options.Logger == nil {
		options.Logger = logx.New(io.Discard)
	}
	plane, err := Open(options)
	if err != nil {
		t.Fatalf("起剖析面失败: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = plane.Close(closeCtx)
	})
	return plane, "http://" + plane.Addr()
}

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	return response.StatusCode, string(body), response.Header
}

// TestDisabledRegistersNoRoutes 钉住关闭态的契约：不是 401，而是 404——
// 401 会告诉探测者「这里有个受了保护的面」，404 与「路径不存在」不可区分。
func TestDisabledRegistersNoRoutes(t *testing.T) {
	plane, base := openTestPlane(t, Options{Enabled: false})
	if plane.Enabled() {
		t.Fatal("关闭态不应报告已启用")
	}
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/index", "/debug/pprof/heap", "/debug/metrics"} {
		status, _, _ := get(t, base+path)
		if status != http.StatusNotFound {
			t.Fatalf("关闭态 %s 应为 404，收到 %d", path, status)
		}
	}
}

// TestEnabledServesPprofAndMetrics 钉住开启态：pprof 与指标都可达，且指标是可解析的 JSON。
func TestEnabledServesPprofAndMetrics(t *testing.T) {
	plane, base := openTestPlane(t, Options{Enabled: true})
	if !plane.Enabled() {
		t.Fatal("开启态应报告已启用")
	}

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/heap", "/debug/pprof/goroutine"} {
		status, _, _ := get(t, base+path)
		if status != http.StatusOK {
			t.Fatalf("开启态 %s 应为 200，收到 %d", path, status)
		}
	}

	status, body, header := get(t, base+"/debug/metrics")
	if status != http.StatusOK {
		t.Fatalf("指标端点应为 200，收到 %d", status)
	}
	if contentType := header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("指标端点 Content-Type 应为 JSON，收到 %q", contentType)
	}
	if cacheControl := header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("指标端点应禁缓存，收到 %q", cacheControl)
	}

	var payload Snapshot
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("指标不是可解析 JSON: %v", err)
	}
	if payload.Process.PID != os.Getpid() {
		t.Fatalf("pid 应为 %d，收到 %d", os.Getpid(), payload.Process.PID)
	}
	if payload.Process.Goroutines <= 0 {
		t.Fatalf("goroutines 应为正数，收到 %d", payload.Process.Goroutines)
	}
	if payload.Process.GOMAXPROCS <= 0 {
		t.Fatalf("gomaxprocs 应为正数，收到 %d", payload.Process.GOMAXPROCS)
	}
	if payload.Process.UptimeSeconds <= 0 {
		t.Fatalf("uptime 应为正数，收到 %v", payload.Process.UptimeSeconds)
	}
	if payload.GC.GOGCPercent == nil {
		t.Fatal("本运行时应当提供 GOGC 读数")
	}
	if payload.GC.GOMemLimitBytes == nil {
		t.Fatal("本运行时应当提供 GOMEMLIMIT 读数")
	}
	if payload.Memory.HeapAllocBytes == 0 && payload.Memory.SysBytes == 0 {
		t.Fatal("内存读数不应全为 0")
	}
	if payload.Timestamp == "" {
		t.Fatal("时间戳不得为空")
	}
}

// TestUnknownPathIs404 确认开启态也只为已注册路径作答。
func TestUnknownPathIs404(t *testing.T) {
	_, base := openTestPlane(t, Options{Enabled: true})
	status, _, _ := get(t, base+"/debug/nope")
	if status != http.StatusNotFound {
		t.Fatalf("未注册路径应为 404，收到 %d", status)
	}
}

// TestMetricsRejectsNonGet 钉住写方法被拒（诊断面不该有写语义）。
func TestMetricsRejectsNonGet(t *testing.T) {
	_, base := openTestPlane(t, Options{Enabled: true})
	request, err := http.NewRequest(http.MethodPost, base+"/debug/metrics", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应为 405，收到 %d", response.StatusCode)
	}
	if allow := response.Header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("405 应带 Allow: GET，收到 %q", allow)
	}
}

// TestInvalidAddr 钉住地址校验：空地址与非法端口都要明确报错（而不是绑到随机端口）。
func TestInvalidAddr(t *testing.T) {
	cases := map[string]string{
		"空地址":  "",
		"缺端口":  "127.0.0.1",
		"端口非数": "127.0.0.1:abc",
		"端口越界": "127.0.0.1:70000",
	}
	for name, addr := range cases {
		if _, err := Open(Options{Enabled: true, Addr: addr, Logger: logx.New(io.Discard)}); err == nil {
			t.Fatalf("%s（%q）应当报错", name, addr)
		}
	}
}

// TestNonLoopbackBindWarns 钉住非回环绑定必须留下告警：这是「谁会看到堆内容」的唯一提示。
func TestNonLoopbackBindWarns(t *testing.T) {
	var logs bytes.Buffer
	plane, err := Open(Options{
		Enabled: true,
		Addr:    "0.0.0.0:0",
		Logger:  logx.New(&logs),
	})
	if err != nil {
		// 某些环境不允许绑通配地址；那属于环境限制，不是本包的行为，跳过。
		t.Skipf("本环境不允许绑通配地址: %v", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	defer func() { _ = plane.Close(closeCtx) }()

	if !strings.Contains(logs.String(), "debug_plane_non_loopback_bind") {
		t.Fatalf("非回环绑定应留下告警，日志为: %s", logs.String())
	}
}

// TestLoopbackBindDoesNotWarn 反向对照：回环绑定不该刷告警（否则告警会被无视）。
func TestLoopbackBindDoesNotWarn(t *testing.T) {
	var logs bytes.Buffer
	plane, err := Open(Options{Enabled: false, Addr: "127.0.0.1:0", Logger: logx.New(&logs)})
	if err != nil {
		t.Fatalf("起剖析面失败: %v", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	defer func() { _ = plane.Close(closeCtx) }()

	if strings.Contains(logs.String(), "debug_plane_non_loopback_bind") {
		t.Fatalf("回环绑定不应告警，日志为: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "debug_plane_listening") {
		t.Fatalf("应留下监听日志，日志为: %s", logs.String())
	}
}

// TestCloseStopsServing 钉住关闭语义：关掉之后连不上（真关监听，不只是停路由）。
func TestCloseStopsServing(t *testing.T) {
	plane, base := openTestPlane(t, Options{Enabled: true})
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := plane.Close(closeCtx); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if _, err := (&http.Client{Timeout: 2 * time.Second}).Get(base + "/debug/metrics"); err == nil {
		t.Fatal("关闭后不应再能连上")
	}
}

// TestBindFailureIsReported 钉住绑定失败要报错（由调用方记降级），而不是静默起不来。
func TestBindFailureIsReported(t *testing.T) {
	first, _ := openTestPlane(t, Options{Enabled: true})
	_, err := Open(Options{Enabled: true, Addr: first.Addr(), Logger: logx.New(io.Discard)})
	if err == nil {
		t.Fatal("端口已被占用时应报错")
	}
	if !strings.Contains(err.Error(), "绑定") {
		t.Fatalf("错误应点明绑定失败，收到 %q", err.Error())
	}
}

// TestLoopbackHostDetection 覆盖地址判定的边界：空主机（绑全网卡）不是回环。
func TestLoopbackHostDetection(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true,
		"::1":       true,
		"localhost": true,
		"127.0.0.5": true,
		"":          false,
		"0.0.0.0":   false,
		"10.0.0.2":  false,
	}
	for host, want := range cases {
		if got := isLoopbackHost(host); got != want {
			t.Fatalf("isLoopbackHost(%q) 应为 %v，收到 %v", host, want, got)
		}
	}
}
