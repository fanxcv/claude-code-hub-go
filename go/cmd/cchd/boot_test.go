package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/httpapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// stubDeps 用桩替代真实 PG/Redis：就绪只看规则快照，便于验证冷启动到就绪的跃迁。
type stubDeps struct {
	snapshot *cfgsync.Snapshot
	closed   bool
}

func (s *stubDeps) PingPG(context.Context) (httpapi.DependencyStatus, string) {
	return httpapi.StatusOK, ""
}

func (s *stubDeps) PingRedis(context.Context) (httpapi.DependencyStatus, string) {
	return httpapi.StatusOK, ""
}

func (s *stubDeps) RulesStatus() (httpapi.DependencyStatus, string) {
	if s.snapshot != nil && s.snapshot.Loaded() {
		return httpapi.StatusOK, ""
	}
	return httpapi.StatusNotLoaded, "rules snapshot not loaded yet"
}

func (s *stubDeps) Close() error {
	s.closed = true
	return nil
}

// readyReport 取一次 /readyz。
func readyReport(t *testing.T, baseURL string) (int, httpapi.ReadyReport) {
	t.Helper()
	response, err := http.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("请求 /readyz 失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	var report httpapi.ReadyReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("响应体必须是 JSON: %v（原文 %s）", err, raw)
	}
	return response.StatusCode, report
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", what)
}

// 冷启动期 /readyz 必须 503，规则装载完成后转 200；关闭时先排空再退出。
func TestRunWithColdStartToReadyThenShutdown(t *testing.T) {
	var (
		mu       sync.Mutex
		order    []string
		logs     bytes.Buffer
		depsStub = &stubDeps{}
	)
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}
	addresses := make(chan string, 1)
	releaseLoad := make(chan struct{})
	signals := make(chan os.Signal, 1)
	loaded := make(chan struct{})

	options := startup{
		Logger: logx.New(&logs),
		LookupEnv: func(name string) (string, bool) {
			switch name {
			case "PORT":
				return "23099", true
			}
			return "", false
		},
		OpenDeps: func(_ context.Context, _ config.Config, rules *cfgsync.Snapshot) (dependencies, error) {
			record("deps")
			depsStub.snapshot = rules
			return depsStub, nil
		},
		OpenSubscriber: func(config.Config) (redis.UniversalClient, error) {
			record("subscriber")
			return nil, nil
		},
		RegisterDomains: func(_ context.Context, rules *rulesSync) error {
			record("register")
			// 装载被挡住，用来稳定观察冷启动期的 503。
			return rules.Register(cfgsync.DomainAPIKeys, func(context.Context) error {
				<-releaseLoad
				close(loaded)
				return nil
			})
		},
		Listen: func(int) (net.Listener, error) {
			record("listen")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			addresses <- listener.Addr().String()
			return listener, nil
		},
		Signals: func() (<-chan os.Signal, func()) {
			return signals, func() {}
		},
		DrainTimeout:    time.Second,
		ShutdownTimeout: time.Second,
		ProbeTimeout:    time.Second,
	}

	finished := make(chan error, 1)
	go func() { finished <- runWith(context.Background(), options) }()

	var address string
	select {
	case address = <-addresses:
	case <-time.After(5 * time.Second):
		t.Fatal("监听未在限期内建立")
	}
	baseURL := "http://" + address

	// 冷启动：规则域装载未完成 → rules 未就绪 → 503，且必须点名是哪一项。
	status, report := readyReport(t, baseURL)
	if status != http.StatusServiceUnavailable || report.Ready {
		t.Fatalf("冷启动期应 503 且未就绪，收到 %d %+v", status, report)
	}
	if strings.Join(report.Failing, ",") != "rules" {
		t.Fatalf("未就绪项应只有 rules，收到 %v", report.Failing)
	}
	if report.PG != httpapi.StatusOK || report.Redis != httpapi.StatusOK {
		t.Fatalf("PG/Redis 桩应是 ok，收到 %+v", report)
	}

	// 探测期间 /v1/_ping 一直可用。
	pingResponse, err := http.Get(baseURL + "/v1/_ping")
	if err != nil {
		t.Fatalf("请求 /v1/_ping 失败: %v", err)
	}
	_ = pingResponse.Body.Close()
	if pingResponse.StatusCode != http.StatusOK {
		t.Fatalf("冷启动期 /v1/_ping 应 200，收到 %d", pingResponse.StatusCode)
	}

	close(releaseLoad)
	<-loaded
	waitFor(t, "规则装载后转就绪", func() bool {
		status, report := readyReport(t, baseURL)
		return status == http.StatusOK && report.Ready
	})
	_, readyState := readyReport(t, baseURL)
	if readyState.Rules != httpapi.StatusOK {
		t.Fatalf("规则装载完成后 rules 应为 ok，收到 %+v", readyState)
	}

	signals <- syscall.SIGTERM
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("正常关闭不得报错: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("收到 SIGTERM 后未在限期内退出")
	}

	if !depsStub.closed {
		t.Fatal("关闭流程必须关闭依赖句柄")
	}
	if _, err := net.DialTimeout("tcp", address, 200*time.Millisecond); err == nil {
		t.Fatal("关闭后监听端口必须已释放")
	}

	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	if got != "deps,subscriber,register,listen" {
		t.Fatalf("启动顺序应为 deps,subscriber,register,listen，收到 %s", got)
	}
	if !strings.Contains(logs.String(), "shutdown_complete") {
		t.Fatalf("关闭必须留下 shutdown_complete 日志：%s", logs.String())
	}
	for _, forbidden := range []string{"postgres://", "redis://"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("日志不得出现凭据原文: %s", forbidden)
		}
	}
}

// 配置非法必须 fail fast：连依赖都不该建。
func TestRunWithInvalidConfigFailsFast(t *testing.T) {
	var logs bytes.Buffer
	opened := false
	options := startup{
		Logger: logx.New(&logs),
		LookupEnv: func(name string) (string, bool) {
			if name == "PORT" {
				return "not-a-port", true
			}
			return "", false
		},
		OpenDeps: func(context.Context, config.Config, *cfgsync.Snapshot) (dependencies, error) {
			opened = true
			return &stubDeps{}, nil
		},
		OpenSubscriber:  func(config.Config) (redis.UniversalClient, error) { return nil, nil },
		RegisterDomains: func(context.Context, *rulesSync) error { return nil },
		Listen:          func(int) (net.Listener, error) { return nil, errors.New("不应被调用") },
		Signals: func() (<-chan os.Signal, func()) {
			return make(chan os.Signal), func() {}
		},
	}

	err := runWith(context.Background(), options)
	if err == nil {
		t.Fatal("非法配置必须返回错误")
	}
	if opened {
		t.Fatal("配置非法时不得建立依赖")
	}
	if !strings.Contains(logs.String(), "config_invalid") {
		t.Fatalf("必须留下 config_invalid 日志：%s", logs.String())
	}
}

// 排空超时：在途请求未归零时必须报错退出，且窗口内新请求拿到 503。
func TestDrainAndShutdownReportsDrainTimeout(t *testing.T) {
	blockUpstream := make(chan struct{})
	frontDoor := egress.New(logx.New(nil))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	server := &http.Server{Handler: frontDoor.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		<-blockUpstream
		writer.WriteHeader(http.StatusOK)
	}))}
	go func() { _ = server.Serve(listener) }()

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := http.Get("http://" + listener.Addr().String() + "/v1/messages")
		if err == nil {
			_ = response.Body.Close()
		}
	}()

	waitFor(t, "在途请求被前门计入", func() bool { return frontDoor.InFlight() == 1 })

	drained := make(chan error, 1)
	go func() {
		drained <- drainAndShutdown(server, frontDoor, nil, nil, logx.New(nil), 100*time.Millisecond, 100*time.Millisecond)
	}()

	waitFor(t, "排空窗口拒绝新请求", func() bool {
		response, err := http.Get("http://" + listener.Addr().String() + "/v1/models")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusServiceUnavailable
	})

	select {
	case err := <-drained:
		if err == nil {
			t.Fatal("在途未归零时排空必须报错")
		}
		if !strings.Contains(err.Error(), "排空超时") {
			t.Fatalf("错误应说明排空超时，收到 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("排空未在限期内返回")
	}

	close(blockUpstream)
	<-requestDone
}

// 在途请求能自行结束时，排空必须干净返回。
func TestDrainAndShutdownCompletesCleanly(t *testing.T) {
	frontDoor := egress.New(logx.New(nil))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	server := &http.Server{Handler: frontDoor.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
	}))}
	go func() { _ = server.Serve(listener) }()

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := http.Get("http://" + listener.Addr().String() + "/v1/messages")
		if err == nil {
			_ = response.Body.Close()
		}
	}()

	waitFor(t, "在途请求被前门计入", func() bool { return frontDoor.InFlight() == 1 })

	if err := drainAndShutdown(server, frontDoor, nil, nil, logx.New(nil), 2*time.Second, time.Second); err != nil {
		t.Fatalf("在途请求能自行结束时排空应干净返回: %v", err)
	}
	<-requestDone
}

func TestRulesSyncKeepsReadinessClosedOnLoadFailure(t *testing.T) {
	snapshot := cfgsync.New()
	state := newRulesSync(logx.New(nil), snapshot)
	state.retryBase = time.Millisecond
	state.retryAttempts = 2

	var attempts atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	if err := state.Register(cfgsync.DomainProviders, func(context.Context) error {
		attempts.Add(1)
		if failing.Load() {
			return errors.New("装载失败（桩）")
		}
		return nil
	}); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	waitFor(t, "装载尝试结束", func() bool { return attempts.Load() >= 2 })
	if snapshot.Loaded() {
		t.Fatal("装载失败时快照不得标记为已装载")
	}

	failing.Store(false)
	if err := state.Register(cfgsync.DomainSystemSettings, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	waitFor(t, "第二个域装载完成", func() bool { return state.registry.Loaded(cfgsync.DomainSystemSettings) })
	if snapshot.Loaded() {
		t.Fatal("仍有域未装载（DomainProviders）时快照不得标记为已装载")
	}

	if err := state.Start(context.Background()); err != nil {
		t.Fatalf("Start 不得报错: %v", err)
	}
	if snapshot.Loaded() {
		t.Fatal("Start 不得绕过未装载的域")
	}

	// 第三个域装载成功后，因为 DomainProviders 仍失败，就绪门必须仍然关闭。
	if err := state.Register(cfgsync.DomainErrorRules, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	waitFor(t, "第三个域装载完成", func() bool { return state.registry.Loaded(cfgsync.DomainErrorRules) })
	if snapshot.Loaded() {
		t.Fatal("有域未装载时快照不得标记为已装载")
	}
	if err := state.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
}

// 无域登记时（当前进程状态）就绪门在依赖可达后放行，并如实说明「没有域可管」。
func TestReadyProberExplainsEmptyDomainSet(t *testing.T) {
	snapshot := cfgsync.New()
	state := newRulesSync(logx.New(nil), snapshot)
	prober := readyProber{dependencies: &stubDeps{snapshot: snapshot}, rules: state}

	// 第一次调用只关心 status；note 要到重新装载后才有意义，故此处丢弃（避免死赋值）。
	status, _ := prober.RulesStatus()
	if status != httpapi.StatusNotLoaded {
		t.Fatalf("快照未装载时应报 not_loaded，收到 %q", status)
	}
	snapshot.MarkLoaded("test")
	status, note := prober.RulesStatus()
	if status != httpapi.StatusOK || !strings.Contains(note, "未登记任何配置域") {
		t.Fatalf("应就绪并说明未登记域，收到 %q / %q", status, note)
	}

	if err := state.Register(cfgsync.DomainSensitiveWords, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	waitFor(t, "域装载完成", func() bool { return state.registry.Loaded(cfgsync.DomainSensitiveWords) })
	if _, note := prober.RulesStatus(); note != "" {
		t.Fatalf("有域登记时不应再补说明，收到 %q", note)
	}
}

// 未配置 Redis 时不得建订阅连接；配置了则必须能解析。
func TestOpenSubscriberHonoursRedisURL(t *testing.T) {
	client, err := openSubscriber(config.Config{})
	if err != nil {
		t.Fatalf("未配置 Redis 不应报错: %v", err)
	}
	if client != nil {
		t.Fatal("未配置 Redis 时不应建订阅连接")
	}

	client, err = openSubscriber(config.Config{RedisURL: "redis://127.0.0.1:6379/13"})
	if err != nil {
		t.Fatalf("合法的 REDIS_URL 不应报错: %v", err)
	}
	if client == nil {
		t.Fatal("配置了 Redis 时必须建订阅连接")
	}
	_ = client.Close()
}

// 集成：真实进程 + 真实 PG/Redis，走冷启动 → 就绪 → SIGTERM 关闭。
//
// 需要 CCH_TEST_DSN / CCH_TEST_REDIS_URL；Node 不启动，用来证明就绪不依赖 Node。
func TestIntegrationRealProcessLifecycle(t *testing.T) {
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
	var logs bytes.Buffer
	command := exec.Command(binary)
	command.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", publicPort),
		"DSN="+dsn,
		"REDIS_URL="+redisURL,
		"NODE_ENV=production",
		// 迁移会真连库建 schema；共享测试库对测试账号不给 CREATE（本地实测 42501），
		// 而本用例验的是进程生命周期，故按真实开关关掉迁移——迁移本身由
		// internal/migrate 的集成用例在自建库上覆盖。
		"AUTO_MIGRATE=false",
	)
	command.Stderr = &logs
	command.Stdout = &logs
	if err := command.Start(); err != nil {
		t.Fatalf("启动进程失败: %v", err)
	}
	// 无论如何都要回收进程：测试自己负责不留后台进程。
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", publicPort)

	// 冷启动观测：先等到端口可答（/v1/_ping），再等就绪。
	waitFor(t, "进程开始应答", func() bool {
		response, err := http.Get(baseURL + "/v1/_ping")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusOK
	})

	coldObserved := false
	waitFor(t, "进程转为就绪", func() bool {
		status, report := probeReady(baseURL)
		if status == http.StatusServiceUnavailable && !report.Ready {
			coldObserved = true
		}
		return status == http.StatusOK && report.Ready
	})
	if !coldObserved {
		t.Log("冷启动窗口未被采样到（就绪跃迁快于首次探测），非缺陷；冷启动语义由单测覆盖")
	}

	_, report := probeReady(baseURL)
	if report.PG != httpapi.StatusOK || report.Redis != httpapi.StatusOK || report.Rules != httpapi.StatusOK {
		t.Fatalf("真实依赖应全部就绪，收到 %+v（日志 %s）", report, logs.String())
	}

	// 数据面由本进程作答：无凭据的 /v1/messages 得到真实的认证错误，而不是「协议不支持」（501）
	// 或「后端不可达」（502）——Node 已退役，不再有第三方承载者。
	response, err := http.Post(baseURL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("请求 /v1/messages 失败: %v", err)
	}
	raw, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭据的 /v1/messages 应 401，收到 %d（%s）", response.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "authentication_error") {
		t.Fatalf("401 响应体应是本进程的认证错误信封，收到 %s", raw)
	}

	// 管理面路径与数据面同构：挂载后由本进程作答，/api/v1/providers 已注册，故同样是 401。
	adminResponse, err := http.Get(baseURL + "/api/v1/providers")
	if err != nil {
		t.Fatalf("请求管理面路径失败: %v", err)
	}
	adminRaw, _ := io.ReadAll(adminResponse.Body)
	_ = adminResponse.Body.Close()
	if adminResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭据的 /api/v1/providers 应 401，收到 %d（%s）",
			adminResponse.StatusCode, adminRaw)
	}
	if !strings.Contains(string(adminRaw), "auth.missing") {
		t.Fatalf("401 响应体应是本进程的问题信封，收到 %s", adminRaw)
	}

	// 未实现的路径由前门的终端如实回 404 not_found：原语义是原样交回 Node，而此刻没有承载者。
	missingResponse, err := http.Get(baseURL + "/api/v1/no-such-endpoint")
	if err != nil {
		t.Fatalf("请求未实现路径失败: %v", err)
	}
	missingRaw, _ := io.ReadAll(missingResponse.Body)
	_ = missingResponse.Body.Close()
	if missingResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("未实现路径应 404，收到 %d（%s）", missingResponse.StatusCode, missingRaw)
	}
	if !strings.Contains(string(missingRaw), "not_found") {
		t.Fatalf("404 响应体应为 not_found，收到 %s", missingRaw)
	}

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
	case <-time.After(15 * time.Second):
		t.Fatalf("SIGTERM 后未在限期内退出（日志 %s）", logs.String())
	}

	if !strings.Contains(logs.String(), "shutdown_complete") {
		t.Fatalf("关闭日志缺少 shutdown_complete：%s", logs.String())
	}
	if !strings.Contains(logs.String(), "admin_plane_ready") {
		t.Fatalf("启动日志缺少 admin_plane_ready：%s", logs.String())
	}
	for _, forbidden := range []string{dsn, redisURL} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("日志泄漏了凭据原文")
		}
	}
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 200*time.Millisecond); err == nil {
		t.Fatal("退出后监听端口必须已释放")
	}
}

// adminRouteFloor 是「管理面注册点确实接过线」的日志下限（当前实测 41 条）。
//
// 只钉下限的理由：并列 lane 会持续把总数抬上去，钉等值会让每次合都红一次；而漏接一个模块
// 的下降幅度（≥ 6 条）一定把它压到下限以下。
const adminRouteFloor = 40

// TestIntegrationManagementPlaneServedByGo 是 A0-3 的验收：真实进程 + 真库，Node **不启动**，
// 管理面必须由本进程自己的处理器作答。
//
// 判定依据：管理路径不得出现 502（Node 已退役，「后端不可达」这种答案已不可能成立），
// 而是本进程的守卫与路由给出的真实回答——已注册路由 401（缺凭据）、未注册路由 404（未实现终端）。
func TestIntegrationManagementPlaneServedByGo(t *testing.T) {
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

	const adminToken = "cchd-integration-admin-token-0123456789"
	publicPort := freePort(t)
	var logs bytes.Buffer
	command := exec.Command(binary)
	command.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", publicPort),
		"DSN="+dsn,
		"REDIS_URL="+redisURL,
		"ADMIN_TOKEN="+adminToken,
		"NODE_ENV=production",
		// 同上：本用例验管理面装配与应答，不涉迁移。
		"AUTO_MIGRATE=false",
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

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", publicPort)
	waitFor(t, "进程开始应答", func() bool {
		response, err := http.Get(baseURL + "/v1/_ping")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusOK
	})
	waitFor(t, "进程转为就绪", func() bool {
		status, report := probeReady(baseURL)
		return status == http.StatusOK && report.Ready
	})

	// 断言 1：启动日志如实报出「路由非 0 且认证已接线」。
	startupLogs := logs.String()
	if !strings.Contains(startupLogs, "admin_plane_ready") {
		t.Fatalf("启动日志缺 admin_plane_ready：%s", startupLogs)
	}
	for _, want := range []string{`"authWired":true`, `"sessionMode":"opaque"`} {
		if !strings.Contains(startupLogs, want) {
			t.Fatalf("admin_plane_ready 缺字段 %s：%s", want, startupLogs)
		}
	}
	// 条数只钉下限：准确值由单测 TestRegisterAdminRoutesWiresEveryRegistrar 按「逐 registrar
	// 之和 == 唯一注册点」结构性断言（并列 lane 会把总数抬上去，钉等值只会制造无意义的红）。
	// 下限 40 的牙口：当前 41，任何**单个**模块掉线（最少 6 条）都会低于它。
	routeMatch := regexp.MustCompile(`"routes":([0-9]+)`).FindStringSubmatch(startupLogs)
	if routeMatch == nil {
		t.Fatalf("admin_plane_ready 未报出 routes：%s", startupLogs)
	}
	routes, convErr := strconv.Atoi(routeMatch[1])
	if convErr != nil || routes < adminRouteFloor {
		t.Fatalf("已注册管理路由应不少于 %d 条，收到 %q：%s", adminRouteFloor, routeMatch[1], startupLogs)
	}
	if strings.Contains(startupLogs, "admin_plane_degraded") {
		t.Fatalf("管理面不该降级：%s", startupLogs)
	}

	// 断言 2：public 档位的 /health 由 Go 作答，正文与响应头逐字对齐 Node。
	healthResponse, err := http.Get(baseURL + "/api/v1/health")
	if err != nil {
		t.Fatalf("请求 /api/v1/health 失败: %v", err)
	}
	healthRaw, _ := io.ReadAll(healthResponse.Body)
	_ = healthResponse.Body.Close()
	if healthResponse.StatusCode != http.StatusOK {
		t.Fatalf("/api/v1/health 应 200，收到 %d（%s）", healthResponse.StatusCode, healthRaw)
	}
	if string(healthRaw) != `{"status":"ok","apiVersion":"1.0.0"}` {
		t.Fatalf("/api/v1/health 正文不符：%s", healthRaw)
	}
	if got := healthResponse.Header.Get("X-API-Version"); got != "1.0.0" {
		t.Fatalf("缺版本头：%q", got)
	}

	// 断言 3：无凭据的 /auth/csrf 是 Go 的问题信封（401），而不是 Node 不可达的 502。
	csrfResponse, err := http.Get(baseURL + "/api/v1/auth/csrf")
	if err != nil {
		t.Fatalf("请求 /api/v1/auth/csrf 失败: %v", err)
	}
	csrfRaw, _ := io.ReadAll(csrfResponse.Body)
	_ = csrfResponse.Body.Close()
	if csrfResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/v1/auth/csrf 无凭据应 401，收到 %d（%s）", csrfResponse.StatusCode, csrfRaw)
	}
	if strings.Contains(string(csrfRaw), "node_backend_unreachable") {
		t.Fatalf("该路由应已由 Go 接管，不该回退 Node：%s", csrfRaw)
	}
	var problem map[string]any
	if err := json.Unmarshal(csrfRaw, &problem); err != nil {
		t.Fatalf("问题信封不是 JSON: %v（%s）", err, csrfRaw)
	}
	if problem["errorCode"] != "auth.missing" || problem["instance"] != "/api/v1/auth/csrf" {
		t.Fatalf("问题信封不符: %s", csrfRaw)
	}

	// 断言 4：资源模块的路由同样由 Go 作答。收口注册点之前 /api/v1/usage-logs 是一条死码——
	// 路由表里没有它，请求会静默回退 Node。无凭据时 Go 的答案是 401 问题信封，而不是 502。
	usageResponse, err := http.Get(baseURL + "/api/v1/usage-logs")
	if err != nil {
		t.Fatalf("请求 /api/v1/usage-logs 失败: %v", err)
	}
	usageRaw, _ := io.ReadAll(usageResponse.Body)
	_ = usageResponse.Body.Close()
	if usageResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/v1/usage-logs 无凭据应 401，收到 %d（%s）", usageResponse.StatusCode, usageRaw)
	}
	if strings.Contains(string(usageRaw), "node_backend_unreachable") {
		t.Fatalf("该路由应已由 Go 接管，不该回退 Node：%s", usageRaw)
	}

	// 断言 5：带 ADMIN_TOKEN 时签发 CSRF token（<bucket>.<base64url>）。
	authorizedRequest, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/auth/csrf", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	authorizedRequest.Header.Set("Authorization", "Bearer "+adminToken)
	authorizedResponse, err := http.DefaultClient.Do(authorizedRequest)
	if err != nil {
		t.Fatalf("请求 /api/v1/auth/csrf 失败: %v", err)
	}
	authorizedRaw, _ := io.ReadAll(authorizedResponse.Body)
	_ = authorizedResponse.Body.Close()
	if authorizedResponse.StatusCode != http.StatusOK {
		t.Fatalf("带 ADMIN_TOKEN 应 200，收到 %d（%s）", authorizedResponse.StatusCode, authorizedRaw)
	}
	var issued struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(authorizedRaw, &issued); err != nil {
		t.Fatalf("签发正文不是 JSON: %v（%s）", err, authorizedRaw)
	}
	bucketText, signature, found := strings.Cut(issued.CSRFToken, ".")
	if !found || bucketText == "" || signature == "" || strings.Trim(bucketText, "0123456789") != "" {
		t.Fatalf("CSRF token 形状不符: %q", issued.CSRFToken)
	}

	// 断言 6：无凭据的资源路径由本进程的守卫作答（401），不是「后端不可达」（502）。
	providerResponse, err := http.Get(baseURL + "/api/v1/providers")
	if err != nil {
		t.Fatalf("请求 /api/v1/providers 失败: %v", err)
	}
	providerRaw, _ := io.ReadAll(providerResponse.Body)
	_ = providerResponse.Body.Close()
	if providerResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/v1/providers 无凭据时应由本进程守卫回 401，收到 %d（%s）",
			providerResponse.StatusCode, providerRaw)
	}
	if strings.Contains(string(providerRaw), "node_backend_unreachable") {
		t.Fatalf("不得出现回退目标不可达：Node 已退役，那条路径已删除（%s）", providerRaw)
	}

	// 断言 7：未注册的管理路径由未实现终端回 404 not_found（不是 502，也不是 500）。
	missingResponse, err := http.Get(baseURL + "/api/v1/no-such-endpoint")
	if err != nil {
		t.Fatalf("请求未注册管理路径失败: %v", err)
	}
	missingRaw, _ := io.ReadAll(missingResponse.Body)
	_ = missingResponse.Body.Close()
	if missingResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("未注册管理路径应 404，收到 %d（%s）", missingResponse.StatusCode, missingRaw)
	}
	if !strings.Contains(string(missingRaw), "not_found") {
		t.Fatalf("404 响应体应为 not_found，收到 %s", missingRaw)
	}

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
	case <-time.After(15 * time.Second):
		t.Fatalf("SIGTERM 后未在限期内退出（日志 %s）", logs.String())
	}
	for _, forbidden := range []string{dsn, redisURL, adminToken} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("日志泄漏了凭据原文")
		}
	}
}

func probeReady(baseURL string) (int, httpapi.ReadyReport) {
	response, err := http.Get(baseURL + "/readyz")
	if err != nil {
		return 0, httpapi.ReadyReport{}
	}
	defer func() { _ = response.Body.Close() }()
	var report httpapi.ReadyReport
	raw, _ := io.ReadAll(response.Body)
	_ = json.Unmarshal(raw, &report)
	return response.StatusCode, report
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

// 生产装配缝必须齐备：任一为 nil 都会让 runWith 在启动路径上 panic。
func TestNewStartupWiresProductionSeams(t *testing.T) {
	options := newStartup()
	if options.Logger == nil || options.LookupEnv == nil {
		t.Fatal("日志器与取环境变量的函数不得为空")
	}
	for name, seam := range map[string]any{
		"OpenDeps":        options.OpenDeps,
		"OpenSubscriber":  options.OpenSubscriber,
		"RegisterDomains": options.RegisterDomains,
		"OpenDataPlane":   options.OpenDataPlane,
		"OpenAdminPlane":  options.OpenAdminPlane,
		"Listen":          options.Listen,
		"Signals":         options.Signals,
	} {
		if seam == nil {
			t.Fatalf("装配缝 %s 不得为空", name)
		}
	}
	for name, timeout := range map[string]time.Duration{
		"DrainTimeout":    options.DrainTimeout,
		"ShutdownTimeout": options.ShutdownTimeout,
		"ProbeTimeout":    options.ProbeTimeout,
	} {
		if timeout <= 0 {
			t.Fatalf("%s 必须为正", name)
		}
	}
}

func TestListenOn(t *testing.T) {
	listener, err := listenOn(0)
	if err != nil {
		t.Fatalf("监听任意端口失败: %v", err)
	}
	if listener.Addr().String() == "" {
		t.Fatal("监听地址不得为空")
	}
	_ = listener.Close()
}

func TestNotifySignalsReturnsStoppableChannel(t *testing.T) {
	signals, stop := notifySignals()
	if signals == nil {
		t.Fatal("信号通道不得为空")
	}
	stop()
}

// 当前进程不登记任何配置域：登记阶段不阻塞，就绪在订阅检查后才放行。
func TestRegisterDomainsKeepsReadinessClosedUntilStart(t *testing.T) {
	snapshot := cfgsync.New()
	state := newRulesSync(logx.New(nil), snapshot)
	if err := registerDomains(context.Background(), state); err != nil {
		t.Fatalf("默认登记不得报错: %v", err)
	}
	if snapshot.Loaded() {
		t.Fatal("登记阶段不得提前宣称规则已装载")
	}
	if err := state.Start(context.Background()); err != nil {
		t.Fatalf("Start 不得报错: %v", err)
	}
	if !snapshot.Loaded() {
		t.Fatal("无域可管时 Start 后应放行就绪")
	}
	if snapshot.Version() != "cchd-domains-0" {
		t.Fatalf("版本标记应反映登记域数，收到 %q", snapshot.Version())
	}
}

// closeRecordingDeps 在关闭时记账，用来断言收口顺序里数据面排在最后。
type closeRecordingDeps struct {
	*stubDeps
	onClose func()
}

func (d closeRecordingDeps) Close() error {
	d.onClose()
	return d.stubDeps.Close()
}

// 亲和装配结论必须能从 /readyz 查到（静默关闭最难发现），且数据面（含亲和的 Redis 连接）
// 必须在关闭链里最后收口——它排在 cfgsync 与依赖之后是结构上的保证，不是调用方的记忆。
// TestRunWithReportsAffinityAndClosesDataPlaneLast 验证启动如实报出亲和结论，且收口顺序为
// 「管理面 → 数据面 → 依赖」。
//
// 为什么顺序是契约：两个面的 handler 仍可能在返回途中写库（数据面写终态、管理面写审计，后者是
// fire-and-forget），依赖与连接池先关就会把那些写一并带走。连接池还多一层：它排在两个面之后，
// 故这里用一个假 DSN 让共享池存在（store.Open 是惰性的，不会真连库）。
func TestRunWithReportsAffinityAndClosesDataPlaneLast(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
		logs  bytes.Buffer
	)
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}
	addresses := make(chan string, 1)
	signals := make(chan os.Signal, 1)

	options := startup{
		Logger: logx.New(&logs),
		LookupEnv: func(name string) (string, bool) {
			switch name {
			case "PORT":
				return "23098", true
			case "DSN":
				// 只为让共享连接池存在：store.Open 惰性，本测试不会真连库。
				return "postgres://stub/stub", true
			case "AUTO_MIGRATE":
				// 迁移会真连库（与惰性的 store.Open 不同），本测试不涉迁移，故关掉。
				return "false", true
			}
			return "", false
		},
		OpenDeps: func(_ context.Context, _ config.Config, rules *cfgsync.Snapshot) (dependencies, error) {
			return closeRecordingDeps{
				stubDeps: &stubDeps{snapshot: rules},
				onClose:  func() { record("deps_close") },
			}, nil
		},
		OpenSubscriber: func(config.Config) (redis.UniversalClient, error) { return nil, nil },
		RegisterDomains: func(_ context.Context, rules *rulesSync) error {
			return rules.Register(cfgsync.DomainAPIKeys, func(context.Context) error { return nil })
		},
		OpenDataPlane: func(_ context.Context, options dataPlaneOptions) (http.Handler, func(), error) {
			record("dataplane_open")
			// 生产路径由 openAffinity 回填这条结论；这里直接注入，断言的是「boot 会把它报出去」。
			options.AffinityReport(affinityStatus{
				Enabled: true, Source: "system_setting", Window: 8, TTLSeconds: 3600,
			})
			return http.NotFoundHandler(), func() { record("dataplane_close") }, nil
		},
		OpenAdminPlane: func(adminOptions) (http.Handler, func(), error) {
			record("admin_open")
			return http.NotFoundHandler(), func() { record("admin_close") }, nil
		},
		Listen: func(int) (net.Listener, error) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			addresses <- listener.Addr().String()
			return listener, nil
		},
		Signals:         func() (<-chan os.Signal, func()) { return signals, func() {} },
		DrainTimeout:    time.Second,
		ShutdownTimeout: time.Second,
		ProbeTimeout:    time.Second,
	}

	finished := make(chan error, 1)
	go func() { finished <- runWith(context.Background(), options) }()

	var address string
	select {
	case address = <-addresses:
	case <-time.After(5 * time.Second):
		t.Fatal("监听未在限期内建立")
	}
	baseURL := "http://" + address
	waitFor(t, "规则装载后转就绪", func() bool {
		status, report := readyReport(t, baseURL)
		return status == http.StatusOK && report.Ready
	})
	_, report := readyReport(t, baseURL)
	affinityNote := report.Notes["affinity"]
	if !strings.Contains(affinityNote, "enabled") || !strings.Contains(affinityNote, "system_setting") {
		t.Fatalf("/readyz 必须说明亲和是否启用及来源，收到 %q", affinityNote)
	}
	if !strings.Contains(affinityNote, "window 8") || !strings.Contains(affinityNote, "ttl 3600s") {
		t.Fatalf("/readyz 必须带出生效的窗口与 TTL，收到 %q", affinityNote)
	}

	signals <- syscall.SIGTERM
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("正常关闭不得报错: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("收到 SIGTERM 后未在限期内退出")
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if len(got) < 3 || got[0] != "dataplane_open" || got[1] != "admin_open" {
		t.Fatalf("两个面都应在启动时装配，收到顺序 %v", got)
	}
	// 收口：管理面 → 数据面 → 依赖。
	if indexOfStep(got, "admin_close") >= indexOfStep(got, "dataplane_close") {
		t.Fatalf("管理面应先于数据面收口，收到顺序 %v", got)
	}
	if indexOfStep(got, "dataplane_close") >= indexOfStep(got, "deps_close") {
		t.Fatalf("数据面应先于依赖收口（handler 可能还在用它写终态），收到顺序 %v", got)
	}
}

// indexOfStep 返回步骤在关闭顺序里的位置；不存在时返回 -1。
func indexOfStep(order []string, step string) int {
	for index, item := range order {
		if item == step {
			return index
		}
	}
	return -1
}
