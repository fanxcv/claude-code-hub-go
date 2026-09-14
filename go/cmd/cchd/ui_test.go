package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/appversion"
	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/uiapp"
	"github.com/redis/go-redis/v9"
)

// embed 档的接线：页面路径必须由 UI 处理器作答。
//
// 它钉的是接线（boot 把 UI 处理器换成页面面的终端），不是择路语义本身——
// 后者由 internal/uiapp 与 internal/egress 的用例覆盖。
func TestRunWithEmbedPagesServesUIHandler(t *testing.T) {
	var logs strings.Builder
	addresses := make(chan string, 1)
	signals := make(chan os.Signal, 1)
	depsStub := &stubDeps{}
	var uiOpened bool

	options := startup{
		Logger: logx.New(&logs),
		LookupEnv: func(name string) (string, bool) {
			switch name {
			case "PORT":
				return "23197", true
			case "CCH_EGRESS_MODE":
				return "go", true
			case "CCH_EGRESS_PAGES":
				return "embed", true
			}
			return "", false
		},
		OpenDeps: func(_ context.Context, _ config.Config, rules *cfgsync.Snapshot) (dependencies, error) {
			depsStub.snapshot = rules
			return depsStub, nil
		},
		OpenSubscriber: func(config.Config) (redis.UniversalClient, error) { return nil, nil },
		RegisterDomains: func(context.Context, *rulesSync) error {
			return nil
		},
		OpenUIPlane: func(uiOptions) (http.Handler, uiStatus, error) {
			uiOpened = true
			handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = writer.Write([]byte("<html>from-embed:" + request.URL.Path + "</html>"))
			})
			return handler, uiStatus{
				Mode: config.EgressPagesEmbed,
				Build: uiapp.Status{
					Enabled: true, Files: 42, Bytes: 1024, Locales: []string{"en", "zh-CN"}, BuildID: "build-x",
				},
			}, nil
		},
		Listen: func(int) (net.Listener, error) {
			listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
			if listenErr != nil {
				return nil, listenErr
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

	if !uiOpened {
		t.Fatal("embed 档必须装配 UI 处理器")
	}

	page, err := http.Get(baseURL + "/zh-CN/dashboard")
	if err != nil {
		t.Fatalf("请求页面失败: %v", err)
	}
	body, _ := io.ReadAll(page.Body)
	_ = page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(body), "from-embed:/zh-CN/dashboard") {
		t.Fatalf("页面应由 UI 处理器作答，实际 %d %s", page.StatusCode, body)
	}

	// /readyz 必须说出「页面面谁在答、跑的哪份产物」：三档都不报错，只有这里能看出差别。
	_, report := readyReport(t, baseURL)
	if note := report.Notes["pages"]; !strings.Contains(note, "embed") || !strings.Contains(note, "build-x") {
		t.Fatalf("/readyz 的 pages 结论应说明 embed 与构建号，收到 %q", note)
	}

	signals <- syscall.SIGTERM
	select {
	case runErr := <-finished:
		if runErr != nil {
			t.Fatalf("正常关闭应无错误: %v", runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("未在限期内退出")
	}
}

// embed 档产物缺失必须 fail fast：监听都不该建立，更不能静默服务一个空站点。
func TestRunWithEmbedPagesFailsFastWhenUIPlaneFails(t *testing.T) {
	listened := make(chan struct{}, 1)
	options := startup{
		Logger: logx.New(io.Discard),
		LookupEnv: func(name string) (string, bool) {
			switch name {
			case "PORT":
				return "23198", true
			case "CCH_EGRESS_PAGES":
				return "embed", true
			}
			return "", false
		},
		OpenDeps: func(_ context.Context, _ config.Config, rules *cfgsync.Snapshot) (dependencies, error) {
			return &stubDeps{snapshot: rules}, nil
		},
		OpenSubscriber:  func(config.Config) (redis.UniversalClient, error) { return nil, nil },
		RegisterDomains: func(context.Context, *rulesSync) error { return nil },
		OpenUIPlane: func(uiOptions) (http.Handler, uiStatus, error) {
			return nil, uiStatus{}, uiapp.ErrAssetsMissing
		},
		Listen: func(int) (net.Listener, error) {
			listened <- struct{}{}
			return net.Listen("tcp", "127.0.0.1:0")
		},
		Signals:         func() (<-chan os.Signal, func()) { return make(chan os.Signal, 1), func() {} },
		DrainTimeout:    time.Second,
		ShutdownTimeout: time.Second,
		ProbeTimeout:    time.Second,
	}

	runErr := runWith(context.Background(), options)
	if runErr == nil {
		t.Fatal("UI 产物缺失必须以错误结束启动")
	}
	// 错误必须原样带上原因（ErrAssetsMissing 的文案会告诉运维跑哪个脚本），
	// 而不是被包成一句「启动失败」。
	if !errors.Is(runErr, uiapp.ErrAssetsMissing) {
		t.Fatalf("错误应保留 ErrAssetsMissing，收到 %v", runErr)
	}
	select {
	case <-listened:
		t.Fatal("产物缺失时不应建立监听")
	default:
	}
}

// 会话解析器是**适配器**：解析口径在 internal/adminapi（真库真 Redis 用例在那里），
// 本层只做「快照 → 壳契约」的翻译与 nil/错误的分流。
func TestUISessionResolverAdaptsSnapshot(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/zh-CN/dashboard", nil)

	t.Run("来源为空按未登录", func(t *testing.T) {
		resolver := newUISessionResolver(nil)
		session, ok, err := resolver.ResolveSession(context.Background(), request)
		if err != nil || ok || session.User.ID != 0 {
			t.Fatalf("管理面未装配时应按未登录，实际 %+v / %v / %v", session, ok, err)
		}
	})

	t.Run("未登录不报错", func(t *testing.T) {
		resolver := newUISessionResolver(stubSessionSource{})
		if _, ok, err := resolver.ResolveSession(context.Background(), request); err != nil || ok {
			t.Fatalf("未登录应为 (false, nil)，实际 %v / %v", ok, err)
		}
	})

	t.Run("已登录带上角色与密钥权限", func(t *testing.T) {
		resolver := newUISessionResolver(stubSessionSource{snapshot: &adminapi.SessionSnapshot{
			UserID: 42, UserName: "u42", Role: "admin", CanLoginWebUI: true,
		}})
		session, ok, err := resolver.ResolveSession(context.Background(), request)
		if err != nil || !ok {
			t.Fatalf("已登录应为 (true, nil)，实际 %v / %v", ok, err)
		}
		if session.User.ID != 42 || session.User.Name != "u42" || session.User.Role != "admin" {
			t.Errorf("用户快照翻译不符: %+v", session.User)
		}
		if session.Key == nil || !session.Key.CanLoginWebUI {
			t.Errorf("key.canLoginWebUi 应带出，实际 %+v", session.Key)
		}
	})

	t.Run("依赖故障原样交出", func(t *testing.T) {
		// uiapp 收到 err 时会记一次 warn 并按未登录处理：故障不能被当成「没登录」静默吞掉。
		resolver := newUISessionResolver(stubSessionSource{err: errors.New("boom")})
		if _, ok, err := resolver.ResolveSession(context.Background(), request); err == nil || ok {
			t.Fatalf("依赖故障应原样交出，实际 %v / %v", ok, err)
		}
	})
}

// 接线（真实进程装配路径 + 真 PG/Redis）：boot 必须把管理面守卫的只读解析入口交给 UI 面。
//
// 这里**不桩管理面**：走真实的 openAdminPlane，于是验到的是「壳注入拿到的是 AuthGuard 的结论」
// 这条完整链路——不接的话壳永远只看到未登录，而那正是上一版的上限。
func TestRunWithEmbedPagesWiresSessionResolver(t *testing.T) {
	dsn := os.Getenv("CCH_TEST_DSN")
	redisURL := os.Getenv("CCH_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("未设置 CCH_TEST_DSN / CCH_TEST_REDIS_URL，跳过集成测试")
	}
	const adminToken = "ui-wire-admin-token-0123456789"

	var logs strings.Builder
	addresses := make(chan string, 1)
	signals := make(chan os.Signal, 1)
	var resolverSeen uiapp.SessionResolver

	options := startup{
		Logger: logx.New(&logs),
		LookupEnv: func(name string) (string, bool) {
			switch name {
			case "PORT":
				return "23199", true
			case "CCH_EGRESS_PAGES":
				return "embed", true
			case "DSN":
				return dsn, true
			case "AUTO_MIGRATE":
				// 本测试验的是 UI 面的会话解析入口接线，不涉迁移；关掉以免真连库跑迁移。
				return "false", true
			case "REDIS_URL":
				return redisURL, true
			case "ADMIN_TOKEN":
				return adminToken, true
			}
			return "", false
		},
		OpenDeps: func(context.Context, config.Config, *cfgsync.Snapshot) (dependencies, error) {
			return &stubDeps{}, nil
		},
		OpenSubscriber: func(config.Config) (redis.UniversalClient, error) { return nil, nil },
		RegisterDomains: func(context.Context, *rulesSync) error {
			return nil
		},
		// 用真实管理面装配（runWith 不填默认值：生产路径由 newStartup 给）。
		OpenAdminPlane: openAdminPlane,
		OpenUIPlane: func(options uiOptions) (http.Handler, uiStatus, error) {
			// 走真实适配器：这一步同时验「入口接到了」与「翻译对了」。
			resolverSeen = newUISessionResolver(options.Sessions)
			return http.NotFoundHandler(), uiStatus{Mode: config.EgressPagesEmbed}, nil
		},
		Listen: func(int) (net.Listener, error) {
			listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
			if listenErr != nil {
				return nil, listenErr
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

	select {
	case <-addresses:
	case <-time.After(10 * time.Second):
		t.Fatal("监听未在限期内建立")
	}
	if resolverSeen == nil {
		t.Fatal("UI 面必须拿到管理面的会话解析入口")
	}

	anonymous, err := http.NewRequest(http.MethodGet, "http://ui.invalid/zh-CN/dashboard", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if _, ok, resolveErr := resolverSeen.ResolveSession(context.Background(), anonymous); resolveErr != nil || ok {
		t.Fatalf("无凭据应为未登录，实际 %v / %v", ok, resolveErr)
	}

	admin, err := http.NewRequest(http.MethodGet, "http://ui.invalid/zh-CN/dashboard", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	admin.Header.Set("Cookie", "auth-token="+adminToken)
	session, ok, resolveErr := resolverSeen.ResolveSession(context.Background(), admin)
	if resolveErr != nil || !ok {
		t.Fatalf("裸 ADMIN_TOKEN 应解析出会话，实际 %v / %v", ok, resolveErr)
	}
	if session.User.ID != -1 || session.User.Role != "admin" || session.Key == nil ||
		!session.Key.CanLoginWebUI {
		t.Fatalf("管理员快照不符: %+v", session)
	}

	signals <- syscall.SIGTERM
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("未在限期内退出")
	}
}

// stubSessionSource 是只读解析入口的桩。
type stubSessionSource struct {
	snapshot *adminapi.SessionSnapshot
	err      error
}

func (s stubSessionSource) ResolveSession(context.Context, *http.Request) (*adminapi.SessionSnapshot, error) {
	return s.snapshot, s.err
}

// 壳注入的版本号：取值唯一实现在 internal/appversion（注入的 APP_VERSION 为真源）。
// 这里验的是装配缝确实走它，而不是自建一条链。
func TestUIAppVersion(t *testing.T) {
	if got := uiAppVersion(func(string) string { return "1.2.3" }); got != "v1.2.3" {
		t.Errorf("注入版本应规整为 v1.2.3，实际 %q", got)
	}
	if got := uiAppVersion(func(string) string { return "V1.2.3" }); got != "v1.2.3" {
		t.Errorf("大写 V 应统一成小写 v，实际 %q", got)
	}
	// 无注入时用 appversion 的兜底常量——不再读工作区的 VERSION 文件（那份文件已删）。
	if got := uiAppVersion(func(string) string { return "" }); got != appversion.Fallback {
		t.Errorf("缺注入时应回落到 %q，实际 %q", appversion.Fallback, got)
	}
}

// 两档页面面都必须能被 /readyz 说清（都不报错，只有这里能看出差别）。
func TestUIStatusDescribe(t *testing.T) {
	if got := (uiStatus{Mode: config.EgressPagesOff}).describe(); !strings.Contains(got, "off") {
		t.Errorf("off 档应说明页面面未装配，实际 %q", got)
	}
	// 零值即 off：缺省不再把页面面交给已退役的后端。
	if got := (uiStatus{}).describe(); !strings.Contains(got, "off") {
		t.Errorf("零值应描述为 off，实际 %q", got)
	}
	embed := uiStatus{Mode: config.EgressPagesEmbed, Build: uiapp.Status{
		Enabled: true, Files: 3, Bytes: 2048, Locales: []string{"zh-CN"}, BuildID: "b1",
	}}
	if got := embed.describe(); !strings.Contains(got, "embed") || !strings.Contains(got, "b1") {
		t.Errorf("embed 档应带产物摘要，实际 %q", got)
	}
}
