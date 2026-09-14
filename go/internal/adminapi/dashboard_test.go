package adminapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 dashboard 资源（/api/v1/dashboard/*）的**路由契约与纯函数**测试。
// 真库集成在 dashboard_integration_test.go。

// fakeObservedSessions 是会话观测读数的替身。
type fakeObservedSessions struct {
	count        int
	providerIDs  []int64
	providerWise map[int64]int
	identities   []string
	err          error
}

func (f *fakeObservedSessions) ObservedSessionCount(_ context.Context) (int, error) {
	return f.count, f.err
}

func (f *fakeObservedSessions) ObservedSessionIdentities(_ context.Context) ([]string, error) {
	return f.identities, f.err
}

func (f *fakeObservedSessions) ProviderSessionCounts(
	_ context.Context,
	providerIDs []int64,
) (map[int64]int, error) {
	f.providerIDs = providerIDs
	counts := make(map[int64]int, len(providerIDs))
	for _, id := range providerIDs {
		if value, ok := f.providerWise[id]; ok {
			counts[id] = value
		}
	}
	return counts, f.err
}

// TestRegisterDashboardRoutes 钉住已就绪端点的档位与 operationId。
func TestRegisterDashboardRoutes(t *testing.T) {
	guard := &recordingGuard{}
	router := New(Options{Deps: Deps{Guard: guard}})
	RegisterDashboardRoutes(router, Deps{
		Guard:            guard,
		Problems:         NewProblems(nil),
		Store:            &store.Pools{},
		ObservedSessions: &fakeObservedSessions{},
	})

	want := []struct {
		key    string
		access AccessLevel
		op     string
	}{
		{http.MethodGet + " /dashboard/overview", AccessRead, "getDashboardOverview"},
		{http.MethodGet + " /dashboard/concurrent-sessions", AccessRead,
			"getDashboardConcurrentSessions"},
		{http.MethodGet + " /dashboard/provider-slots", AccessAdmin, "getDashboardProviderSlots"},
		{http.MethodGet + " /dashboard/rate-limit-stats", AccessAdmin, "getDashboardRateLimitStats"},
		{http.MethodGet + " /dashboard/client-versions", AccessAdmin, "getDashboardClientVersions"},
		{http.MethodGet + " /dashboard/statistics", AccessRead, "getDashboardStatistics"},
		{http.MethodGet + " /dashboard/proxy-status", AccessAdmin, "getDashboardProxyStatus"},
		{http.MethodGet + " /dashboard/realtime", AccessAdmin, "getDashboardRealtime"},
		{http.MethodPost + " /dashboard/dispatch-simulator:simulate", AccessAdmin,
			"simulateDispatchDecisionTree"},
	}
	routes := router.RouteList()
	if len(routes) != len(want) {
		t.Fatalf("应注册 %d 条，实际 %d：%v", len(want), len(routes), auditLogRouteKeys(router))
	}
	for index, route := range routes {
		if route.Method+" "+route.Path != want[index].key {
			t.Errorf("第 %d 条应为 %s，实际 %s %s", index, want[index].key, route.Method, route.Path)
		}
		if route.Access != want[index].access {
			t.Errorf("%s 的档位应为 %s，实际 %s", route.Path, want[index].access, route.Access)
		}
		if route.OperationID != want[index].op {
			t.Errorf("%s 的 operationId 应为 %s，实际 %s", route.Path, want[index].op, route.OperationID)
		}
		if route.Module != "dashboard" {
			t.Errorf("%s 的模块应为 dashboard，实际 %s", route.Path, route.Module)
		}
	}
}

// TestRegisterDashboardRoutesWithoutSessionRuntime 钉住「缺会话观测读数时不注册那四条」。
//
// overview / concurrent-sessions / provider-slots / realtime 都要读观测集合；没有它时并发数、
// 插槽数与活动流的活跃半边都拿不到——在大屏上是静默错数，故宁可不答（回退 Node）。另外四条
// 与本依赖无关，必须照常注册。
func TestRegisterDashboardRoutesWithoutSessionRuntime(t *testing.T) {
	guard := &recordingGuard{}
	router := New(Options{Deps: Deps{Guard: guard}})
	RegisterDashboardRoutes(router, Deps{
		Guard:    guard,
		Problems: NewProblems(nil),
		Store:    &store.Pools{},
	})

	want := []string{
		http.MethodGet + " /dashboard/rate-limit-stats",
		http.MethodGet + " /dashboard/client-versions",
		http.MethodGet + " /dashboard/statistics",
		http.MethodGet + " /dashboard/proxy-status",
		// 调度模拟是纯计算 + 只读读数，不依赖会话观测，故它在缺观测读数时仍注册。
		http.MethodPost + " /dashboard/dispatch-simulator:simulate",
	}
	got := auditLogRouteKeys(router)
	if len(got) != len(want) {
		t.Fatalf("缺观测读数时应只注册 %d 条，实际 %d：%v", len(want), len(got), got)
	}
	for index, key := range want {
		if got[index] != key {
			t.Errorf("第 %d 条应为 %s，实际 %s", index, key, got[index])
		}
	}
}

// TestRegisterDashboardRoutesWithoutStore 钉住 fail-closed：没有连接池就一条都不注册。
func TestRegisterDashboardRoutesWithoutStore(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterDashboardRoutes(router, Deps{
		Guard:            &recordingGuard{},
		ObservedSessions: &fakeObservedSessions{count: 3},
	})
	if got := router.RouteCount(); got != 0 {
		t.Fatalf("Store 未装配时应注册 0 条，实际 %d：%v", got, auditLogRouteKeys(router))
	}
}

// TestComputeClientVersionGA 钉住 GA 判定：阈值 2 个去重用户、取达标者里的最高版本。
func TestComputeClientVersionGA(t *testing.T) {
	type row struct {
		userID  int64
		version string
	}
	gaOf := func(rows []row) *string {
		return computeClientVersionGA(
			rows,
			func(item row) int64 { return item.userID },
			func(item row) string { return item.version },
		)
	}

	if got := gaOf(nil); got != nil {
		t.Errorf("空列表应无 GA，实际 %v", *got)
	}
	if got := gaOf([]row{{1, "2.0.35"}}); got != nil {
		t.Errorf("只有一个用户时不应有 GA（阈值 2），实际 %v", *got)
	}
	got := gaOf([]row{{1, "2.0.35"}, {2, "2.0.35"}, {3, "2.0.30"}, {3, "2.0.30"}})
	if got == nil || *got != "2.0.35" {
		t.Fatalf("GA 应为达标的最高版本 2.0.35，实际 %v", got)
	}
	// 同一用户在两个版本上出现时按「去重用户数」算：单用户两版本都不达标。
	if got := gaOf([]row{{1, "2.0.35"}, {1, "2.0.30"}}); got != nil {
		t.Errorf("同一用户的多个版本不构成 GA，实际 %v", *got)
	}
	// 低版本达标、高版本只有一人：GA 取达标者，故仍是低版本。
	got = gaOf([]row{{1, "2.0.30"}, {2, "2.0.30"}, {3, "2.0.35"}})
	if got == nil || *got != "2.0.30" {
		t.Fatalf("GA 只在达标版本里取最高，实际 %v", got)
	}
}

// TestDashboardRateLimitFiltersValidation 钉住 limitType 枚举与时间格式的校验出口。
func TestDashboardRateLimitFiltersValidation(t *testing.T) {
	cases := []struct {
		target string
		field  string
		code   string
	}{
		{"/dashboard/rate-limit-stats?limitType=nope", "limitType", "invalid_enum_value"},
		{"/dashboard/rate-limit-stats?userId=0", "userId", "too_small"},
		{"/dashboard/rate-limit-stats?userId=abc", "userId", "invalid_type"},
		{"/dashboard/rate-limit-stats?startTime=2026-01-02", "startTime", "invalid_string"},
	}
	for _, testCase := range cases {
		request := httptest.NewRequest(http.MethodGet, testCase.target, nil)
		_, issues := parseDashboardRateLimitFilters(request)
		if len(issues) != 1 {
			t.Fatalf("%s 应有 1 条校验错误，实际 %d：%+v", testCase.target, len(issues), issues)
		}
		if issues[0].Code != testCase.code {
			t.Errorf("%s 的错误码应为 %s，实际 %s", testCase.target, testCase.code, issues[0].Code)
		}
		if path, ok := issues[0].Path[0].(string); !ok || path != testCase.field {
			t.Errorf("%s 的错误路径应为 %s，实际 %v", testCase.target, testCase.field, issues[0].Path)
		}
	}
}

// TestDashboardMetricRounding 钉住三处展示精度（Node 的 toDecimalPlaces / toFixed / Math.round）。
func TestDashboardMetricRounding(t *testing.T) {
	if got := roundCost6(0.1234567891); got != 0.123457 {
		t.Errorf("成本应保留 6 位并四舍五入，实际 %v", got)
	}
	if got := roundCost6(2); got != 2 {
		t.Errorf("整数成本不应变形，实际 %v", got)
	}
	if got := errorRate(0, 0); got != 0 {
		t.Errorf("无请求时错误率应为 0，实际 %v", got)
	}
	if got := errorRate(1, 3); got != 33.33 {
		t.Errorf("错误率应保留 2 位，实际 %v", got)
	}
	if got := errorRate(1, 1); got != 100 {
		t.Errorf("全错时错误率应为 100，实际 %v", got)
	}
}
