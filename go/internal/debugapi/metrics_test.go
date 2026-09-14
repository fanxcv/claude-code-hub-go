package debugapi

import (
	"context"
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// TestMetricsTopLevelKeysAreStable 钉住响应形状：多一段少一段都算契约变更。
//
// 这条同时是**防泄漏**的结构性保证：要往指标里塞新东西（例如整份配置），
// 必须先改这张清单，而不是悄悄多出一个字段。
func TestMetricsTopLevelKeysAreStable(t *testing.T) {
	_, base := openTestPlane(t, Options{Enabled: true})
	status, body, _ := get(t, base+"/debug/metrics")
	if status != 200 {
		t.Fatalf("指标端点应为 200，收到 %d", status)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析指标失败: %v", err)
	}
	want := []string{"gc", "memory", "process", "timestamp"}
	if len(payload) != len(want) {
		t.Fatalf("顶层字段应为 %v，收到 %v", want, keysOf(payload))
	}
	for _, name := range want {
		if _, ok := payload[name]; !ok {
			t.Fatalf("缺少顶层字段 %s（实际 %v）", name, keysOf(payload))
		}
	}
}

func keysOf(payload map[string]json.RawMessage) []string {
	names := make([]string, 0, len(payload))
	for name := range payload {
		names = append(names, name)
	}
	return names
}

// TestCollectorsLandUnderCollectorsKey 钉住外部计数器（连接池等）的落点。
func TestCollectorsLandUnderCollectorsKey(t *testing.T) {
	_, base := openTestPlane(t, Options{
		Enabled: true,
		Collectors: []Collector{
			{Name: "dbPool", Collect: func() map[string]any {
				return map[string]any{"outstanding": int64(3), "maxOutstanding": 16}
			}},
			{Name: "empty", Collect: func() map[string]any { return nil }},
			{Name: "noFunc"},
		},
	})

	_, body, _ := get(t, base+"/debug/metrics")
	var payload struct {
		Collectors map[string]map[string]any `json:"collectors"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析指标失败: %v", err)
	}
	if len(payload.Collectors) != 1 {
		t.Fatalf("只应有 dbPool 一段（空读数与无函数的收集器都省略），收到 %v", payload.Collectors)
	}
	pool, ok := payload.Collectors["dbPool"]
	if !ok {
		t.Fatalf("缺少 dbPool 段: %v", payload.Collectors)
	}
	if pool["outstanding"] != float64(3) {
		t.Fatalf("outstanding 应为 3，收到 %v", pool["outstanding"])
	}
}

// TestMetricsLeakNoEnvironmentSecrets 是防泄漏的反证：
// 把三份凭据放进环境，再断言指标正文里连片段都不出现。
//
// 指标面只读运行时数字，故这条在当前实现下必然通过；它的价值在**未来**——
// 谁若把配置或环境摘要塞进这个端点，这条会红。
func TestMetricsLeakNoEnvironmentSecrets(t *testing.T) {
	const adminToken = "sentinel-admin-token-7f3a9c"
	const dsn = "postgres://sentinel_user:sentinel_pass@sentinel-host:5432/sentinel_db"
	const redisURL = "redis://sentinel-redis:sentinel_redis_pass@sentinel-redis-host:6379"
	t.Setenv("ADMIN_TOKEN", adminToken)
	t.Setenv("DSN", dsn)
	t.Setenv("REDIS_URL", redisURL)

	_, base := openTestPlane(t, Options{Enabled: true})
	_, body, _ := get(t, base+"/debug/metrics")

	for name, secret := range map[string]string{"ADMIN_TOKEN": adminToken, "DSN": dsn, "REDIS_URL": redisURL} {
		if strings.Contains(body, secret) {
			t.Fatalf("指标正文泄漏了 %s 的取值", name)
		}
	}
	for _, fragment := range []string{"sentinel_pass", "sentinel_redis_pass", "postgres://", "redis://", "ADMIN_TOKEN", "REDIS_URL", "DSN"} {
		if strings.Contains(body, fragment) {
			t.Fatalf("指标正文出现了敏感片段 %q", fragment)
		}
	}
}

// TestReadGCSettingsFollowsRuntime 钉住 GOGC 读数是真读运行时，而不是常量。
//
// 用 debug.SetGCPercent 改到 37 再读回来：若实现改成了硬编码或读环境变量，这条会红。
// 结束时还原，避免影响同进程其它用例。
func TestReadGCSettingsFollowsRuntime(t *testing.T) {
	previous := debug.SetGCPercent(37)
	t.Cleanup(func() { debug.SetGCPercent(previous) })

	gogc, memLimit := readGCSettings()
	if gogc == nil {
		t.Fatal("本运行时应当提供 GOGC 读数")
	}
	if *gogc != 37 {
		t.Fatalf("GOGC 应为 37（刚设置的值），收到 %d", *gogc)
	}
	if memLimit == nil {
		t.Fatal("本运行时应当提供 GOMEMLIMIT 读数")
	}
	// 未设上限时运行时报 math.MaxInt64；设了就应是个正数。
	if *memLimit <= 0 {
		t.Fatalf("GOMEMLIMIT 应为正数（未设上限时是 MaxInt64），收到 %d", *memLimit)
	}
}

// TestLoggerMayBeNil 钉住日志器可选（测试与嵌入场景不该被强制注入）。
func TestLoggerMayBeNil(t *testing.T) {
	plane, err := Open(Options{Enabled: true, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("日志器为 nil 时应照常起面: %v", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	defer func() { _ = plane.Close(closeCtx) }()
	if plane.Addr() == "" {
		t.Fatal("应报告绑定地址")
	}
}
