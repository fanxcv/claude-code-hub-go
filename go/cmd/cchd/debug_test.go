// 剖析面的装配测试：单测覆盖开关语义，集成覆盖真实进程的「起得来、关得掉、不泄漏」。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// debugConfig 按给定环境变量装载配置；地址固定为回环上的空闲端口，避免与生产/其它用例撞端口。
func debugConfig(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	if _, ok := env["CCH_PPROF_ADDR"]; !ok {
		env["CCH_PPROF_ADDR"] = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	}
	cfg, err := config.Load(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("装载配置失败: %v", err)
	}
	return cfg
}

func debugGet(t *testing.T, url string) (int, string) {
	t.Helper()
	response, err := (&http.Client{Timeout: 10 * time.Second}).Get(url)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	return response.StatusCode, string(body)
}

// TestOpenDebugPlaneDisabledServes404 钉住默认档的对外行为：
// 关闭时端点不注册，所有路径 404（而不是 401——不泄漏存在性）。
func TestOpenDebugPlaneDisabledServes404(t *testing.T) {
	cfg := debugConfig(t, map[string]string{"PORT": "23110"})
	if cfg.Pprof.Enabled {
		t.Fatal("默认应为关闭")
	}
	plane, err := openDebugPlane(debugOptions{Cfg: cfg, Logger: logx.New(io.Discard)})
	if err != nil {
		t.Fatalf("关闭态也应起监听（404 语义要求它存在）: %v", err)
	}
	defer func() { _ = plane.Close(context.Background()) }()

	base := "http://" + plane.Addr()
	for _, path := range []string{"/debug/pprof/index", "/debug/pprof/heap", "/debug/metrics"} {
		status, _ := debugGet(t, base+path)
		if status != http.StatusNotFound {
			t.Fatalf("关闭态 %s 应为 404，收到 %d", path, status)
		}
	}
}

// TestOpenDebugPlaneEnabledServesMetrics 钉住开启档：指标可解析，且回环绑定。
func TestOpenDebugPlaneEnabledServesMetrics(t *testing.T) {
	cfg := debugConfig(t, map[string]string{
		"PORT":                 "23111",
		"CCH_PPROF_ENABLED":    "true",
		"CCH_PPROF_BLOCK_RATE": "0",
	})
	var logs bytes.Buffer
	plane, err := openDebugPlane(debugOptions{Cfg: cfg, Logger: logx.New(&logs)})
	if err != nil {
		t.Fatalf("起剖析面失败: %v", err)
	}
	defer func() { _ = plane.Close(context.Background()) }()

	base := "http://" + plane.Addr()
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/goroutine"} {
		if status, _ := debugGet(t, base+path); status != http.StatusOK {
			t.Fatalf("开启态 %s 应为 200，收到 %d", path, status)
		}
	}
	status, body := debugGet(t, base+"/debug/metrics")
	if status != http.StatusOK {
		t.Fatalf("指标端点应为 200，收到 %d", status)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("指标不是可解析 JSON: %v", err)
	}
	if _, ok := payload["process"]; !ok {
		t.Fatalf("指标缺少 process 段: %s", body)
	}

	// 监听地址必须落在回环：这是「谁会看到堆内容」的第一道闸。
	host, _, err := net.SplitHostPort(plane.Addr())
	if err != nil {
		t.Fatalf("解析绑定地址失败: %v", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("剖析面应只绑回环，实际绑在 %s", plane.Addr())
	}
	if !strings.Contains(logs.String(), "debug_plane_listening") {
		t.Fatalf("启动日志缺少 debug_plane_listening: %s", logs.String())
	}
	if strings.Contains(logs.String(), "debug_plane_non_loopback_bind") {
		t.Fatalf("回环绑定不应告警: %s", logs.String())
	}
}

// TestNewDebugCollectorsWithoutPools 钉住未装配连接池时不给指标面造一个空段。
func TestNewDebugCollectorsWithoutPools(t *testing.T) {
	if got := newDebugCollectors(nil); got != nil {
		t.Fatalf("无连接池时应返回 nil，收到 %v", got)
	}
}

// TestCloseDebugPlaneHandlesMissingPlane 钉住收口对未装配场景是 no-op。
func TestCloseDebugPlaneHandlesMissingPlane(t *testing.T) {
	var logs bytes.Buffer
	closeDebugPlane(nil, logx.New(&logs))
	if logs.Len() != 0 {
		t.Fatalf("未装配时不该记日志，收到 %s", logs.String())
	}
}

// TestCloseDebugPlaneReleasesListener 钉住收口真释放监听（不是只停路由）。
func TestCloseDebugPlaneReleasesListener(t *testing.T) {
	cfg := debugConfig(t, map[string]string{"PORT": "23112"})
	plane, err := openDebugPlane(debugOptions{Cfg: cfg, Logger: logx.New(io.Discard)})
	if err != nil {
		t.Fatalf("起剖析面失败: %v", err)
	}
	addr := plane.Addr()
	var logs bytes.Buffer
	closeDebugPlane(plane, logx.New(&logs))
	if _, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		t.Fatal("收口后监听应已释放")
	}
}

// TestIntegrationDebugPlaneOverRealProcess 是验收的硬证据：真实进程 + 真库下，
// 剖析面起得来（指标含真实连接池读数、pprof 可取且 `go tool pprof` 能解析），
// SIGTERM 后关得掉，且日志里没有凭据。
func TestIntegrationDebugPlaneOverRealProcess(t *testing.T) {
	dsn := os.Getenv("CCH_TEST_DSN")
	redisURL := os.Getenv("CCH_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("未设置 CCH_TEST_DSN / CCH_TEST_REDIS_URL，跳过集成测试")
	}

	binary := filepath.Join(t.TempDir(), "cchd")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("编译失败: %v\n%s", err, output)
	}

	publicPort := freePort(t)
	debugPort := freePort(t)
	var logs bytes.Buffer
	command := exec.Command(binary)
	command.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", publicPort),
		"DSN="+dsn,
		"REDIS_URL="+redisURL,
		"NODE_ENV=production",
		"AUTO_MIGRATE=false",
		// 剖析面开在回环的空闲端口上（默认档是 127.0.0.1:3101）。
		"CCH_PPROF_ENABLED=true",
		fmt.Sprintf("CCH_PPROF_ADDR=127.0.0.1:%d", debugPort),
		"CCH_PPROF_BLOCK_RATE=0",
	)
	command.Stderr = &logs
	command.Stdout = &logs
	if err := command.Start(); err != nil {
		t.Fatalf("启动进程失败: %v", err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	}()

	debugBase := fmt.Sprintf("http://127.0.0.1:%d", debugPort)
	waitFor(t, "剖析面开始应答", func() bool {
		response, err := (&http.Client{Timeout: time.Second}).Get(debugBase + "/debug/metrics")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusOK
	})

	// 指标：必须是可解析 JSON，且带真实连接池读数（证明收集器真的接上了池）。
	status, body := debugGet(t, debugBase+"/debug/metrics")
	if status != http.StatusOK {
		t.Fatalf("指标端点应为 200，收到 %d", status)
	}
	var payload struct {
		Process    map[string]any            `json:"process"`
		Collectors map[string]map[string]any `json:"collectors"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("指标不是可解析 JSON: %v（%s）", err, body)
	}
	if payload.Process == nil {
		t.Fatalf("指标缺少 process 段: %s", body)
	}
	pool, ok := payload.Collectors["dbPool"]
	if !ok {
		t.Fatalf("指标缺少 dbPool 读数（收集器没接上池？）: %s", body)
	}
	lanes, ok := pool["lanes"].(map[string]any)
	if !ok || len(lanes) == 0 {
		t.Fatalf("dbPool 应含 lanes 读数，收到 %v", pool)
	}

	// heap 端点：可取的二进制 profile（pprof 解析见下）。
	heapStatus, heapBody := debugGet(t, debugBase+"/debug/pprof/heap")
	if heapStatus != http.StatusOK || len(heapBody) == 0 {
		t.Fatalf("heap 剖析应可取，收到 %d 长度 %d", heapStatus, len(heapBody))
	}

	// CPU profile：落盘后用 go tool pprof 解析——这是「真能用」的判据，
	// 只看 HTTP 200 不足以排除「返回一段乱码」。
	profilePath := filepath.Join(t.TempDir(), "cpu.pb.gz")
	response, err := (&http.Client{Timeout: 30 * time.Second}).Get(debugBase + "/debug/pprof/profile?seconds=2")
	if err != nil {
		t.Fatalf("取 CPU profile 失败: %v", err)
	}
	profileBytes, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatalf("读 CPU profile 失败: %v", readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CPU profile 应为 200，收到 %d", response.StatusCode)
	}
	if err := os.WriteFile(profilePath, profileBytes, 0o644); err != nil {
		t.Fatalf("写 profile 失败: %v", err)
	}
	pprof := exec.Command("go", "tool", "pprof", "-top", "-nodecount=5", profilePath)
	pprofOutput, pprofErr := pprof.CombinedOutput()
	if pprofErr != nil {
		t.Fatalf("go tool pprof 解析失败: %v\n%s", pprofErr, pprofOutput)
	}
	if !strings.Contains(string(pprofOutput), "flat") {
		t.Fatalf("pprof 输出不像一份 CPU profile:\n%s", pprofOutput)
	}
	t.Logf("profile %d 字节；pprof 前 5 行:\n%s", len(profileBytes), firstLines(string(pprofOutput), 5))

	// 关闭：SIGTERM 后剖析面必须一并释放（收口接线真的走到）。
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("发送 SIGTERM 失败: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("SIGTERM 后应正常退出，收到 %v（日志 %s）", err, logs.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("SIGTERM 后未在限期内退出（日志 %s）", logs.String())
	}
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", debugPort), 300*time.Millisecond); err == nil {
		t.Fatal("退出后剖析面监听必须已释放")
	}

	if !strings.Contains(logs.String(), "debug_plane_listening") {
		t.Fatalf("启动日志缺少 debug_plane_listening：%s", logs.String())
	}
	if !strings.Contains(logs.String(), "\"pprofEnabled\":true") {
		t.Fatalf("config_loaded 摘要应显示剖析面已开：%s", logs.String())
	}
	for _, forbidden := range []string{dsn, redisURL} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatal("日志泄漏了凭据原文")
		}
	}
}

func firstLines(text string, count int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.Join(lines, "\n")
}
