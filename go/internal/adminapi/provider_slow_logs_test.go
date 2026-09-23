package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/slowlog"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是低速日志端点（provider_slow_logs.go）的用例。
//
// 为什么读写两侧分开测：写侧（internal/slowlog）钉住命令形态与容量；本文件钉住**端点契约**
// ——空数据返回 [] 而不是 500、条数上限生效、降级给出原因、可见性门与同族端点一致。
//
// 为什么慢日志面用替身而 Store 用真库：可见性门（providerFindVisible）要真查 providers 表，
// 那是本端点与熔断日志端点的**共同前提**，替身会把「门是否真的装上了」测掉；
// 而慢日志面只是一次 Redis 读，替身能精确造「读不到」这一态（真 Redis 要造故障更麻烦）。

// slowLogsFakeReader 是 SlowLogsReader 的替身，同时钉住「读面签名与生产实现一致」。
//
// 编译期断言（而非注释）：签名一改就编译不过——接缝静默腐烂是本仓的既有教训。
var _ SlowLogsReader = (*slowlog.Reader)(nil)

type slowLogsFakeReader struct {
	events []slowlog.Event
	err    error
	// calls 记录每次调用的 (providerID, limit)，供「条数上限真的传下去了」断言。
	calls []struct {
		providerID int64
		limit      int
	}
}

func (f *slowLogsFakeReader) Recent(_ context.Context, providerID int64, limit int) ([]slowlog.Event, error) {
	f.calls = append(f.calls, struct {
		providerID int64
		limit      int
	}{providerID, limit})
	if f.err != nil {
		return nil, f.err
	}
	return f.events, nil
}

// slowLogsPayload 承接响应体。
type slowLogsPayload struct {
	ProviderID int64 `json:"providerId"`
	Window     struct {
		Limit          int    `json:"limit"`
		RetentionHours int    `json:"retentionHours"`
		Since          string `json:"since"`
	} `json:"window"`
	Events []struct {
		Kind        string   `json:"kind"`
		At          int64    `json:"at"`
		ModelKey    *string  `json:"modelKey"`
		PenaltyFrom *int     `json:"penaltyFrom"`
		PenaltyTo   *int     `json:"penaltyTo"`
		Median      *float64 `json:"median"`
		Samples     *int64   `json:"samples"`
		Reason      *string  `json:"reason"`
	} `json:"events"`
	// Quarantine 是本次新增块（只增不改）：nil 表示未装配读面。
	Quarantine *struct {
		Combinations []struct {
			ModelKey            string `json:"modelKey"`
			StateExists         bool   `json:"stateExists"`
			Quarantined         bool   `json:"quarantined"`
			Penalty             int    `json:"penalty"`
			CleanStreak         int    `json:"cleanStreak"`
			AdmissionPermille   int    `json:"admissionPermille"`
			SampleLiveCount     int    `json:"sampleLiveCount"`
			BaselineUsable      bool   `json:"baselineUsable"`
			ProbeLeaseHeld      bool   `json:"probeLeaseHeld"`
			ProbeLeaseTTLMillis int64  `json:"probeLeaseTtlMillis"`
		} `json:"combinations"`
		UnavailableReason *string `json:"unavailableReason"`
	} `json:"quarantine"`
	UnavailableReason *string `json:"unavailableReason"`
}

// slowLogsFakeStates 是 SlowRateStateReader 的替身。它同时记录收到的 provider，
// 供「传的是渠道行而不是光一个 id」断言。
//
// 为什么隔离态面用替身而不是真 Redis：本文件要造的是**边界形态**（state 在 / 不在、读失败），
// 那三类在真 Redis 上要凑夹具与故障注入，替身能逐字控制。
// 「这个读面在真 Redis 下能不能读对」由 route 侧的 slowrate_observe_test.go 覆盖。
type slowLogsFakeStates struct {
	observations []route.SlowRateStateObservation
	err          error
	providers    []route.Provider
}

func (f *slowLogsFakeStates) ObserveStates(
	_ context.Context,
	provider route.Provider,
) ([]route.SlowRateStateObservation, error) {
	f.providers = append(f.providers, provider)
	if f.err != nil {
		return nil, f.err
	}
	return f.observations, nil
}

// slowLogsRouter 造真路由表：真 Store（可见性门）+ 传入的慢日志读面。
func slowLogsRouter(t *testing.T, pools *store.Pools, reader SlowLogsReader) *Router {
	t.Helper()
	return slowLogsRouterWithStates(t, pools, reader, nil)
}

// slowLogsRouterWithStates 在同一条路由上再装隔离态读面（nil 即未装配：该块为 null）。
func slowLogsRouterWithStates(
	t *testing.T,
	pools *store.Pools,
	reader SlowLogsReader,
	states SlowRateStateReader,
) *Router {
	t.Helper()
	deps := Deps{
		Logger:         logx.New(nil),
		Guard:          principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Store:          pools,
		Problems:       NewProblems(nil),
		SlowLogs:       reader,
		SlowRateStates: states,
	}
	router := New(Options{Deps: deps})
	RegisterProviderSlowLogs(router, deps)
	return router
}

// slowLogsGet 打一发本端点并解出响应体。
func slowLogsGet(t *testing.T, router *Router, providerID int64, query string) (int, slowLogsPayload) {
	t.Helper()
	status, body := slowLogsGetRaw(t, router, providerID, query)
	if status != http.StatusOK {
		return status, slowLogsPayload{}
	}
	var payload slowLogsPayload
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析低速日志响应失败: %v（原文 %.300s）", err, body)
	}
	return status, payload
}

// slowLogsGetRaw 返回状态码与**原始响应体**：钉「字段名与 null/[] 形态」这类契约只能用原文。
func slowLogsGetRaw(t *testing.T, router *Router, providerID int64, query string) (int, string) {
	t.Helper()
	target := fmt.Sprintf("/api/v1/providers/%d/slow-logs", providerID)
	if query != "" {
		target += "?" + query
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder.Code, recorder.Body.String()
}

// TestProviderSlowLogsReturnsEmptyArrayOnNoData 钉住空数据契约：200 + `events: []`
// 而不是 500 或 `null`。
//
// 为什么这条最要紧：界面要能区分「24 小时内没有降权」与「读不到」——前者是正常结果，
// 后者必须给出原因。若空数据也返 500，运维会以为排障入口坏了。
func TestProviderSlowLogsReturnsEmptyArrayOnNoData(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)

	router := slowLogsRouter(t, pools, &slowLogsFakeReader{})
	status, payload := slowLogsGet(t, router, fixture.enabledID, "")
	if status != http.StatusOK {
		t.Fatalf("无数据应 200，收到 %d", status)
	}
	if payload.Events == nil {
		t.Fatal("events 应为 [] 而不是 null（前端不必为 null 与 [] 各写一条分支）")
	}
	if len(payload.Events) != 0 {
		t.Fatalf("应为空，实际 %d 条", len(payload.Events))
	}
	if payload.UnavailableReason != nil {
		t.Errorf("正常空结果不该给 unavailableReason，收到 %q", *payload.UnavailableReason)
	}
	// 时间范围必须回给界面：否则「窗内无记录」会被读成「从未发生」。
	if payload.Window.RetentionHours != slowLogsRetentionHours || payload.Window.Since == "" {
		t.Errorf("应给出时间范围，收到 %+v", payload.Window)
	}
}

// TestProviderSlowLogsProjectsEvents 钉住事件投影：种类、前后值、时间，以及「不含的那一维是 null」。
func TestProviderSlowLogsProjectsEvents(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)

	from, to := 10, 30
	median, samples := 239.68, int64(7215)
	reader := &slowLogsFakeReader{events: []slowlog.Event{
		{
			Kind: slowlog.KindPenaltyUp, At: 1789993274606, ProviderID: fixture.enabledID,
			ModelKey: "deepseek-v4.1-flash", PenaltyFrom: &from, PenaltyTo: &to,
		},
		{
			Kind: slowlog.KindBaselinePublished, At: 1789993274706, ProviderID: fixture.enabledID,
			ModelKey: "deepseek-v4.1-flash", Median: &median, Samples: &samples, Reason: "primary",
		},
	}}
	router := slowLogsRouter(t, pools, reader)
	status, payload := slowLogsGet(t, router, fixture.enabledID, "")
	if status != http.StatusOK {
		t.Fatalf("应 200，收到 %d", status)
	}
	if len(payload.Events) != 2 {
		t.Fatalf("应有 2 条，实际 %d 条", len(payload.Events))
	}
	penalty := payload.Events[0]
	if penalty.Kind != "penalty_up" || penalty.PenaltyFrom == nil || *penalty.PenaltyFrom != 10 ||
		penalty.PenaltyTo == nil || *penalty.PenaltyTo != 30 {
		t.Errorf("升档事件投影有误：%+v", penalty)
	}
	if penalty.ModelKey == nil || *penalty.ModelKey != "deepseek-v4.1-flash" {
		t.Errorf("模型键应透传，收到 %v", penalty.ModelKey)
	}
	// 惩罚事件不带基线读数 ⇒ 必须是 null（0 会被渲染成「中位数 0」）。
	if penalty.Median != nil || penalty.Samples != nil || penalty.Reason != nil {
		t.Errorf("惩罚事件的基线维应为 null，收到 median=%v samples=%v reason=%v",
			penalty.Median, penalty.Samples, penalty.Reason)
	}

	baseline := payload.Events[1]
	if baseline.Kind != "baseline_published" || baseline.Median == nil || *baseline.Median != 239.68 {
		t.Errorf("基线事件投影有误：%+v", baseline)
	}
	if baseline.Reason == nil || *baseline.Reason != "primary" {
		t.Errorf("基线来源应透传，收到 %v", baseline.Reason)
	}
	if baseline.PenaltyFrom != nil || baseline.PenaltyTo != nil {
		t.Errorf("基线事件的惩罚维应为 null，收到 %v / %v", baseline.PenaltyFrom, baseline.PenaltyTo)
	}
}

// TestProviderSlowLogsClampsLimit 钉住条数上限：默认 20、越界报 too_big（与熔断日志同信箱语义）。
func TestProviderSlowLogsClampsLimit(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)

	reader := &slowLogsFakeReader{}
	router := slowLogsRouter(t, pools, reader)

	// 默认值：未传 limit 时用 slowLogsDefaultLimit。
	if _, payload := slowLogsGet(t, router, fixture.enabledID, ""); payload.Window.Limit != slowLogsDefaultLimit {
		t.Errorf("默认条数应为 %d，收到 %d", slowLogsDefaultLimit, payload.Window.Limit)
	}
	if len(reader.calls) != 1 || reader.calls[0].limit != slowLogsDefaultLimit {
		t.Errorf("默认条数应真的传到读面，收到 %+v", reader.calls)
	}

	// 越上限：拒绝（400）而不是静默夹取——静默夹取会让调用方以为拿到了 1000 条。
	status, _ := slowLogsGet(t, router, fixture.enabledID, "limit=101")
	if status != http.StatusBadRequest {
		t.Errorf("limit 越上限应 400，收到 %d", status)
	}
	// 非法类型：同样 400。
	if status, _ := slowLogsGet(t, router, fixture.enabledID, "limit=abc"); status != http.StatusBadRequest {
		t.Errorf("limit 非数字应 400，收到 %d", status)
	}
	// 合法值（上限本身）应通过。
	if status, _ := slowLogsGet(t, router, fixture.enabledID, "limit=100"); status != http.StatusOK {
		t.Errorf("limit=100（恰在上限）应 200，收到 %d", status)
	}
}

// TestProviderSlowLogsDegradesOnReadFailure 钉住降级：读不到给 200 + 原因，且 events 仍是 []。
//
// 为什么不能打成 5xx：排障入口在最需要它的时候（Redis 抖动）必须还能打开，
// 至少让运维看到「读不到」而不是一个空白错误页。
func TestProviderSlowLogsDegradesOnReadFailure(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)

	router := slowLogsRouter(t, pools, &slowLogsFakeReader{err: errors.New("redis: connection refused")})
	status, payload := slowLogsGet(t, router, fixture.enabledID, "")
	if status != http.StatusOK {
		t.Fatalf("读不到应 200（附原因），收到 %d", status)
	}
	if payload.UnavailableReason == nil || *payload.UnavailableReason == "" {
		t.Fatal("读不到必须给出原因")
	}
	if payload.Events == nil || len(payload.Events) != 0 {
		t.Errorf("读不到时 events 应为 []（不是 null），收到 %v", payload.Events)
	}
}

// TestProviderSlowLogsKeepsVisibilityGate 钉住可见性门与同族端点一致：隐藏类型的供应商
// 对非 compat 请求 404——否则这条新端点会成为一个绕过门的信息泄漏面。
func TestProviderSlowLogsKeepsVisibilityGate(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)

	router := slowLogsRouter(t, pools, &slowLogsFakeReader{})
	if status, _ := slowLogsGet(t, router, fixture.otherID, ""); status != http.StatusNotFound {
		t.Errorf("隐藏类型的供应商应 404，收到 %d", status)
	}
	// 不存在的 id 同样 404。
	if status, _ := slowLogsGet(t, router, 999999999, ""); status != http.StatusNotFound {
		t.Errorf("不存在的供应商应 404，收到 %d", status)
	}
}

// TestProviderSlowLogsNotRegisteredWithoutSeam 钉住 fail-closed 装配：无慢日志读面时不注册路由。
//
// 为什么这条要有：注册一个必然失败的路由比不注册坏得多（会 500 而不是回退 Node）。
// 断 RouteCount 而不是打请求：未注册时请求落给谁由 Router 之外的层决定，
// 「没注册」这个事实只有路由表知道（与 TestCircuitRoutesNotRegisteredWithoutRedis 同一手法）。
func TestProviderSlowLogsNotRegisteredWithoutSeam(t *testing.T) {
	deps := Deps{
		Logger:   logx.New(nil),
		Guard:    principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Problems: NewProblems(nil),
	}
	router := New(Options{Deps: deps})
	RegisterProviderSlowLogs(router, deps)

	if count := router.RouteCount(); count != 0 {
		t.Fatalf("未装配读面时应零注册，实际 %d 条", count)
	}
}

// TestSlowLogsRetentionMatchesStorage 钉住两处寿命一致：端点展示的时间范围必须与写入侧的
// 事件 TTL 同值，否则界面会把「已过期的窗」说成「还有数据」。
func TestSlowLogsRetentionMatchesStorage(t *testing.T) {
	want := int(slowlog.EventTTL / time.Hour)
	if slowLogsRetentionHours != want {
		t.Fatalf("端点声明的保留 %d 小时与存储 TTL %v 不一致", slowLogsRetentionHours, slowlog.EventTTL)
	}
}

// TestProviderSlowLogsQuarantineSurvivesEventReadFailure 钉住两条读面互相独立。
//
// 事件流读失败时端点提前作答——若隔离态在那之后才读，就永远不会被读。而「事件流读不到」
// 恰恰是运维最需要看隔离态的时候（两者是独立的键族，一条挂不等于另一条也挂）。
func TestProviderSlowLogsQuarantineSurvivesEventReadFailure(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	states := &slowLogsFakeStates{observations: []route.SlowRateStateObservation{{
		ModelKey: "m1", StateExists: true, Quarantined: true, Penalty: 10,
		CleanStreak: 3, AdmissionPermille: 100, SampleLiveCount: 3, BaselineUsable: true,
	}}}
	router := slowLogsRouterWithStates(t, pools,
		&slowLogsFakeReader{err: errors.New("redis: connection refused")}, states)

	status, payload := slowLogsGet(t, router, fixture.enabledID, "")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}
	if payload.UnavailableReason == nil || *payload.UnavailableReason != "redis_unavailable" {
		t.Errorf("事件流的原因 = %v，期望 redis_unavailable", payload.UnavailableReason)
	}
	if payload.Quarantine == nil || len(payload.Quarantine.Combinations) != 1 {
		t.Fatalf("事件流读失败时隔离态没被读到（quarantine = %+v）", payload.Quarantine)
	}
	if payload.Quarantine.Combinations[0].AdmissionPermille != 100 {
		t.Errorf("admissionPermille = %d，期望 100", payload.Quarantine.Combinations[0].AdmissionPermille)
	}
}
