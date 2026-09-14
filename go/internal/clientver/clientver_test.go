package clientver

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// fakeRedis 是最小 Redis 替身：只实现本包用到的 Get / Set。
type fakeRedis struct {
	redis.UniversalClient
	values map[string]string
	ttl    map[string]time.Duration
	getErr error
	setErr error
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{values: map[string]string{}, ttl: map[string]time.Duration{}}
}

func (f *fakeRedis) Get(_ context.Context, key string) *redis.StringCmd {
	cmd := redis.NewStringCmd(context.Background())
	if f.getErr != nil {
		cmd.SetErr(f.getErr)
		return cmd
	}
	if value, ok := f.values[key]; ok {
		cmd.SetVal(value)
		return cmd
	}
	cmd.SetErr(redis.Nil)
	return cmd
}

func (f *fakeRedis) Set(_ context.Context, key string, value interface{}, ttl time.Duration) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(context.Background())
	if f.setErr != nil {
		cmd.SetErr(f.setErr)
		return cmd
	}
	if text, ok := value.(string); ok {
		f.values[key] = text
	} else if payload, ok := value.([]byte); ok {
		f.values[key] = string(payload)
	}
	f.ttl[key] = ttl
	cmd.SetVal("OK")
	return cmd
}

// fakeUsers 是活跃用户来源的替身。
type fakeUsers struct {
	rows []store.ActiveUserAgent
	err  error
}

func (f fakeUsers) ActiveUserAgents(context.Context, int) ([]store.ActiveUserAgent, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func TestParseUserAgent(t *testing.T) {
	cases := []struct {
		name       string
		userAgent  string
		clientType string
		version    string
		ok         bool
	}{
		{
			name:       "vscode 插件优先识别",
			userAgent:  "claude-cli/2.0.31 (external, claude-vscode, agent-sdk/0.1.30)",
			clientType: "claude-vscode",
			version:    "2.0.31",
			ok:         true,
		},
		{
			name:       "纯 CLI",
			userAgent:  "claude-cli/2.0.32 (external, cli)",
			clientType: "claude-cli",
			version:    "2.0.32",
			ok:         true,
		},
		{
			// Node 的注释说这种情况是 claude-cli-unknown，但它的代码用 includes("cli") 判定，
			// 而 "claude-cli/..." 必然含 "cli"，故实际返回 claude-cli。已用 node 实测对齐代码行为。
			// 于是 claude-cli-unknown 在 Node 里不可达，本包照代码而不照注释。
			name:       "无标记的 CLI 仍判为 claude-cli（照 Node 代码非注释）",
			userAgent:  "claude-cli/2.0.20",
			clientType: "claude-cli",
			version:    "2.0.20",
			ok:         true,
		},
		{
			name:       "SDK 原样返回类型",
			userAgent:  "anthropic-sdk-typescript/1.0.0",
			clientType: "anthropic-sdk-typescript",
			version:    "1.0.0",
			ok:         true,
		},
		{name: "空 UA 不解析", userAgent: "", ok: false},
		{
			// Node 的正则是通用的 name/semver，任何此类 UA 都能解析；守卫只对自己认识的类型起作用。
			name:       "任意 name/semver 也解析（与 Node 同）",
			userAgent:  "curl/8.4.0",
			clientType: "curl",
			version:    "8.4.0",
			ok:         true,
		},
		{name: "缺版本号不解析", userAgent: "claude-cli/2.0", ok: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, ok := ParseUserAgent(testCase.userAgent)
			if ok != testCase.ok {
				t.Fatalf("解析结果 = %v，期望 %v", ok, testCase.ok)
			}
			if !ok {
				return
			}
			if client.ClientType != testCase.clientType || client.Version != testCase.version {
				t.Fatalf("得到 %+v，期望 %s/%s", client, testCase.clientType, testCase.version)
			}
		})
	}
}

func TestDisplayName(t *testing.T) {
	cases := map[string]string{
		"claude-vscode":            "Claude VSCode Extension",
		"claude-cli":               "Claude CLI",
		"claude-cli-unknown":       "Claude CLI (Unknown Version)",
		"anthropic-sdk-typescript": "Anthropic SDK (TypeScript)",
		"other-client":             "other-client",
	}
	for clientType, want := range cases {
		if got := DisplayName(clientType); got != want {
			t.Fatalf("DisplayName(%q) = %q，期望 %q", clientType, got, want)
		}
	}
}

func TestVersionComparison(t *testing.T) {
	cases := []struct {
		name    string
		current string
		target  string
		less    bool
		greater bool
	}{
		{name: "补丁落后", current: "2.0.31", target: "2.0.32", less: true},
		{name: "相等", current: "2.0.32", target: "2.0.32"},
		{name: "次版本领先", current: "2.1.0", target: "2.0.32", greater: true},
		{name: "带 v 前缀", current: "v2.0.31", target: "2.0.32", less: true},
		{name: "忽略 build 元数据", current: "2.0.32+build.7", target: "2.0.32"},
		{name: "稳定版高于预发布", current: "2.0.32", target: "2.0.32-beta.1", greater: true},
		{name: "预发布低于稳定版", current: "2.0.32-beta.1", target: "2.0.32", less: true},
		{name: "预发布数字段比较", current: "2.0.32-beta.1", target: "2.0.32-beta.2", less: true},
		{name: "段数不齐按 0 补", current: "2.0", target: "2.0.1", less: true},
		{name: "不可解析视为相等", current: "whatever", target: "2.0.32"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := IsVersionLess(testCase.current, testCase.target); got != testCase.less {
				t.Fatalf("IsVersionLess(%q,%q) = %v，期望 %v", testCase.current, testCase.target, got, testCase.less)
			}
			if got := IsVersionGreater(testCase.current, testCase.target); got != testCase.greater {
				t.Fatalf("IsVersionGreater(%q,%q) = %v，期望 %v", testCase.current, testCase.target, got, testCase.greater)
			}
		})
	}
}

func TestUpdateUserVersionWritesNodeKeyWithTTL(t *testing.T) {
	client := newFakeRedis()
	checker := NewChecker(Options{Redis: client})
	clientValue := guard.ClientVersion{ClientType: "claude-cli", Version: "2.0.31"}
	if err := checker.UpdateUserVersion(context.Background(), 42, clientValue); err != nil {
		t.Fatalf("记录用户版本失败: %v", err)
	}
	key := "client_version:claude-cli:42"
	if client.values[key] != "2.0.31" {
		t.Fatalf("键值不符: %v", client.values)
	}
	if client.ttl[key] != 7*24*time.Hour {
		t.Fatalf("TTL 应为 7 天，得到 %v", client.ttl[key])
	}
}

func TestShouldUpgradeThreeStates(t *testing.T) {
	activeUsers := []store.ActiveUserAgent{
		{UserID: 1, UserAgent: "claude-cli/2.0.32 (external, cli)"},
		{UserID: 2, UserAgent: "claude-cli/2.0.32 (external, cli)"},
		{UserID: 3, UserAgent: "claude-cli/2.0.20"},
	}
	client := newFakeRedis()
	checker := NewChecker(Options{Redis: client, Users: fakeUsers{rows: activeUsers}})

	// 1) 用户版本落后于 GA -> 需要升级，且 GA 版本来自「达阈值的最高版本」。
	needsUpgrade, gaVersion, err := checker.ShouldUpgrade(
		context.Background(),
		guard.ClientVersion{ClientType: "claude-cli", Version: "2.0.20"},
	)
	if err != nil {
		t.Fatalf("ShouldUpgrade 返回错误: %v", err)
	}
	if !needsUpgrade || gaVersion != "2.0.32" {
		t.Fatalf("落后版本应提示升级到 2.0.32，得到 needsUpgrade=%v ga=%q", needsUpgrade, gaVersion)
	}
	// 计算出的 GA 版本要写回缓存（Node 同样缓存 5 分钟），形状是 {version,userCount}。
	if cached := client.values["ga_version:claude-cli"]; cached != `{"version":"2.0.32","userCount":2}` {
		t.Fatalf("GA 缓存形状不符: %q", cached)
	}
	if client.ttl["ga_version:claude-cli"] != 5*time.Minute {
		t.Fatalf("GA 缓存 TTL 应为 5 分钟，得到 %v", client.ttl["ga_version:claude-cli"])
	}

	// 2) 用户版本即 GA -> 不提示。
	needsUpgrade, gaVersion, err = checker.ShouldUpgrade(
		context.Background(),
		guard.ClientVersion{ClientType: "claude-cli", Version: "2.0.32"},
	)
	if err != nil || needsUpgrade || gaVersion != "2.0.32" {
		t.Fatalf("同版本不应提示升级，得到 needsUpgrade=%v ga=%q err=%v", needsUpgrade, gaVersion, err)
	}

	// 3) 缓存命中优先：不再查活跃用户。
	checkerCached := NewChecker(Options{
		Redis: client,
		Users: fakeUsers{err: errors.New("不应被调用")},
	})
	needsUpgrade, gaVersion, err = checkerCached.ShouldUpgrade(
		context.Background(),
		guard.ClientVersion{ClientType: "claude-cli", Version: "2.0.20"},
	)
	if err != nil || !needsUpgrade || gaVersion != "2.0.32" {
		t.Fatalf("缓存命中应直接判定，得到 needsUpgrade=%v ga=%q err=%v", needsUpgrade, gaVersion, err)
	}
}

func TestShouldUpgradeFailOpen(t *testing.T) {
	// 无活跃用户来源、无缓存：放行（Node 的「无 GA 版本」分支）。
	checker := NewChecker(Options{Redis: newFakeRedis()})
	needsUpgrade, gaVersion, err := checker.ShouldUpgrade(
		context.Background(),
		guard.ClientVersion{ClientType: "claude-cli", Version: "1.0.0"},
	)
	if err != nil || needsUpgrade || gaVersion != "" {
		t.Fatalf("无 GA 版本应放行，得到 needsUpgrade=%v ga=%q err=%v", needsUpgrade, gaVersion, err)
	}

	// 活跃用户查询失败：放行且不把错误抛给守卫（守卫只在日志里体现）。
	broken := NewChecker(Options{Redis: newFakeRedis(), Users: fakeUsers{err: errors.New("db down")}})
	needsUpgrade, _, err = broken.ShouldUpgrade(
		context.Background(),
		guard.ClientVersion{ClientType: "claude-cli", Version: "1.0.0"},
	)
	if err != nil || needsUpgrade {
		t.Fatalf("查询失败应放行，得到 needsUpgrade=%v err=%v", needsUpgrade, err)
	}
}

func TestUnparseableUserVersionDoesNotPromptUpgrade(t *testing.T) {
	client := newFakeRedis()
	client.values["ga_version:claude-cli"] = `{"version":"2.0.32","userCount":2}`
	checker := NewChecker(Options{Redis: client})
	needsUpgrade, gaVersion, err := checker.ShouldUpgrade(
		context.Background(),
		guard.ClientVersion{ClientType: "claude-cli", Version: "not-a-version"},
	)
	if err != nil || needsUpgrade || gaVersion != "2.0.32" {
		t.Fatalf("不可解析的版本应放行，得到 needsUpgrade=%v ga=%q err=%v", needsUpgrade, gaVersion, err)
	}
}

// TestIntegrationActiveUserAgentsAndGA 用真实 PG + Redis 走一遍「活跃用户 -> GA -> 缓存」链路。
func TestIntegrationActiveUserAgentsAndGA(t *testing.T) {
	dsn := os.Getenv("CCH_TEST_DSN")
	redisURL := os.Getenv("CCH_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("未设置 CCH_TEST_DSN / CCH_TEST_REDIS_URL，跳过集成测试")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败: %v", err)
	}
	redisClient := redis.NewClient(options)
	defer redisClient.Close()
	ctx := context.Background()

	pools, err := store.Open(ctx, store.Options{DSN: dsn})
	if err != nil {
		t.Fatalf("打开连接池失败: %v", err)
	}
	defer pools.Close()

	rows, err := pools.ActiveUserAgents(ctx, 7)
	if err != nil {
		t.Fatalf("查询活跃用户失败: %v", err)
	}
	checker := NewChecker(Options{Redis: redisClient, Users: pools})
	// 清掉缓存，走上「查库计算」这条路。
	const clientType = "claude-cli"
	redisClient.Del(ctx, gaVersionKeyPrefix+clientType)
	defer redisClient.Del(ctx, gaVersionKeyPrefix+clientType)

	// 库里未必有同版本两人以上的用户，故 GA 是否存在由同一套计算决定，而不是假定它存在。
	expectedGA, _ := computeGAVersion(rows, clientType)
	needsUpgrade, gaVersion, err := checker.ShouldUpgrade(
		ctx,
		guard.ClientVersion{ClientType: clientType, Version: "0.0.1"},
	)
	if err != nil {
		t.Fatalf("ShouldUpgrade 失败: %v", err)
	}
	if expectedGA == "" {
		// 无 GA 版本即放行（Node 的同一分支）。
		if needsUpgrade || gaVersion != "" {
			t.Fatalf("无 GA 版本应放行，得到 needsUpgrade=%v ga=%q（活跃用户 %d 行）", needsUpgrade, gaVersion, len(rows))
		}
		return
	}
	if !needsUpgrade || gaVersion != expectedGA {
		t.Fatalf("0.0.1 应提示升级到 %q，得到 needsUpgrade=%v ga=%q", expectedGA, needsUpgrade, gaVersion)
	}
}
