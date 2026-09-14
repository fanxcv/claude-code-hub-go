package dataplane

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// 本文件覆盖「供应商级金额限额」门槛的两种正交性质：
//   1. **装配**：buildGates 在什么情况下把限额门槛接上、什么时候必须报缺口（单元，无 IO）；
//   2. **生效与步骤归属**：真库 + 真 Redis + 真 limit.Service 下，超限供应商应在 Step 4
//      被排除（enabledProviders 仍计入它、afterHealthCheck 才掉），理由是 rate_limited。
//
// 为什么必须做真库真 Redis：限额判定的输入（providers 的限额列、usage_ledger 的累计值）都
// 在库里，构造字面量只能证明「函数会调用判定器」，证明不了「列读到了、口径对得上」。

// stubQuotas 是 limit.QuotaSource 的最小实现：供应商级判定不读 key/user 配额，
// 所以这层只用来满足装配前置条件（真实现见 cmd/cchd/ratelimit.go 的 storeQuotas）。
type stubQuotas struct{}

func (stubQuotas) KeyQuota(context.Context, int64) (limit.KeyQuota, error) {
	return limit.KeyQuota{}, nil
}

func (stubQuotas) UserQuota(context.Context, int64) (limit.UserQuota, error) {
	return limit.UserQuota{}, nil
}

// costGateLimiterStub 是「非 *limit.Service」的限流器替身（如测试桩或旧实现）。重名会与
// limit_session_test.go 的 recordingLimiter 冲突，故用带前缀的名字。
type costGateLimiterStub struct{}

func (costGateLimiterStub) Throttle(context.Context, string, string) (guard.ThrottleDecision, error) {
	return guard.ThrottleDecision{Allowed: true}, nil
}
func (costGateLimiterStub) RecordAuthSuccess(context.Context, string, string) {}
func (costGateLimiterStub) RecordAuthFailure(context.Context, string, string) {}
func (costGateLimiterStub) Check(context.Context, *pctx.Context) (*guard.RateLimitBlock, error) {
	return nil, nil
}

// TestBuildGatesWiresProviderCostFromLimitService 钉住：传入 *limit.Service 时限额门槛必须接上，
// 且不再报缺口（缺口一旦漏报，生产上「不判供应商额度」就没有任何可见痕迹）。
func TestBuildGatesWiresProviderCostFromLimitService(t *testing.T) {
	service, err := limit.New(limit.Config{Quotas: stubQuotas{}, Logger: logx.New(nil)})
	if err != nil {
		t.Fatalf("构造限额服务失败: %v", err)
	}

	gates, unwired := buildGates(StoreOptions{RateLimit: service})
	if gates.Limits == nil {
		t.Fatal("传入 *limit.Service 时 Gates.Limits 必须被接上")
	}
	if unwired {
		t.Fatal("接上了就不该报缺口")
	}
}

// TestBuildGatesReportsGapWithoutLimitService 钉住：拿不到限额服务时**报缺口**而不是静默跳过。
func TestBuildGatesReportsGapWithoutLimitService(t *testing.T) {
	gates, unwired := buildGates(StoreOptions{RateLimit: costGateLimiterStub{}})
	if gates.Limits != nil {
		t.Fatal("非 *limit.Service 的实现不应被当成限额判定器")
	}
	if !unwired {
		t.Fatal("拿不到限额服务必须报缺口（否则行为差异不可见）")
	}
}

// TestBuildGatesKeepsCallerInjection 钉住：调用方显式注入的 Gates.Limits 优先，不被覆盖。
func TestBuildGatesKeepsCallerInjection(t *testing.T) {
	caller := func(context.Context, route.Provider) (bool, string) { return true, "" }
	gates, unwired := buildGates(StoreOptions{
		RouteOptions: route.Options{Gates: route.Gates{Limits: caller}},
	})
	if gates.Limits == nil || !unwired == false {
		t.Fatalf("调用方注入应被保留且不报缺口（unwired=%v）", unwired)
	}
	if allowed, _ := gates.Limits(context.Background(), route.Provider{}); !allowed {
		t.Fatal("保留的应是调用方那份实现")
	}
}

// TestProviderCostLimitsMapping 钉住选路视图 → 判定器入参的字段映射（漏一个字段就等于
// 某个窗口的额度永远不判）。
func TestProviderCostLimitsMapping(t *testing.T) {
	limit5h := 1.5
	mode := "rolling"
	daily := 2.5
	dailyMode := "fixed"
	resetTime := "03:00"
	weekly := 3.5
	monthly := 4.5
	total := 5.5
	resetAt := time.Date(2026, 9, 13, 1, 2, 3, 0, time.UTC)

	out := providerCostLimits(route.ProviderCostLimits{
		Limit5hUSD:       &limit5h,
		Limit5hResetMode: &mode,
		LimitDailyUSD:    &daily,
		DailyResetMode:   &dailyMode,
		DailyResetTime:   &resetTime,
		LimitWeeklyUSD:   &weekly,
		LimitMonthlyUSD:  &monthly,
		LimitTotalUSD:    &total,
		CostResetAt:      &resetAt,
	})

	if out.Limit5hUSD == nil || *out.Limit5hUSD != limit5h ||
		out.Limit5hResetMode == nil || *out.Limit5hResetMode != mode ||
		out.LimitDailyUSD == nil || *out.LimitDailyUSD != daily ||
		out.DailyResetMode == nil || *out.DailyResetMode != dailyMode ||
		out.DailyResetTime == nil || *out.DailyResetTime != resetTime ||
		out.LimitWeeklyUSD == nil || *out.LimitWeeklyUSD != weekly ||
		out.LimitMonthlyUSD == nil || *out.LimitMonthlyUSD != monthly ||
		out.LimitTotalUSD == nil || *out.LimitTotalUSD != total ||
		out.CostResetAt == nil || !out.CostResetAt.Equal(resetAt) {
		t.Fatalf("字段映射不完整: %+v", out)
	}
}

// TestIntegrationProviderCostLimitExcludesAtHealthStep 是限额门槛的真库真 Redis 集成用例。
//
// 场景：给专用供应商设一个极小的 5h 限额，并在 usage_ledger 里留下超过该限额的成本
// （经由 message_request 的触发器写入，与生产同路径）；选路时该供应商应被排除，
// 且计数证明判定发生在 Step 4。
func TestIntegrationProviderCostLimitExcludesAtHealthStep(t *testing.T) {
	pools := integrationStore(t)
	redisClient := replayTestClient(t)
	ctx := context.Background()

	providerID := insertCostLimitFixture(t, pools, 0.01)
	seedProviderLedgerCost(t, pools, providerID, "1.0")

	service := newLimitService(t, pools, redisClient, ctx)
	gates, unwired := buildGates(StoreOptions{RateLimit: service})
	if unwired || gates.Limits == nil {
		t.Fatalf("限额门槛未接上（unwired=%v）", unwired)
	}

	selector := route.NewSelector(route.Options{Source: route.NewStoreSource(pools), Gates: gates})
	result, err := selector.Select(ctx, route.Request{Model: "claude-3-5-sonnet", Format: "claude"})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}

	var found *route.Filtered
	for index := range result.Context.FilteredProviders {
		if result.Context.FilteredProviders[index].ID == providerID {
			found = &result.Context.FilteredProviders[index]
			break
		}
	}
	if found == nil {
		t.Fatalf("超限供应商 %d 未被排除；filteredProviders=%+v",
			providerID, result.Context.FilteredProviders)
	}
	if found.Reason != route.ReasonRateLimited {
		t.Errorf("理由 = %q，期望 rate_limited", found.Reason)
	}
	// Node 在 Step 4 的 rate_limited 分支不记具体窗口文案（provider-selector.ts:1441）。
	if found.Details != "rate_limited" {
		t.Errorf("details = %q，期望 rate_limited", found.Details)
	}
	// 步骤归属：限额在 Step 4 判，故它仍应被计入 enabledProviders/afterGroupFilter，
	// 只在 afterHealthCheck 上掉下来。放到 Step 2 判会让这两个数字与 Node 分叉。
	if result.Context.AfterGroupFilter == nil ||
		result.Context.EnabledProviders != *result.Context.AfterGroupFilter {
		t.Errorf("enabledProviders=%d afterGroupFilter=%v，期望相等（Step 4 判定）",
			result.Context.EnabledProviders, result.Context.AfterGroupFilter)
	}
}

// insertCostLimitFixture 建一个 5h 限额极小的专用供应商，返回 id 并登记清理。
func insertCostLimitFixture(t *testing.T, pools *store.Pools, limit5h float64) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}

	var id int64
	err = pool.QueryRow(ctx, `
		INSERT INTO providers (
			name, url, key, provider_type, is_enabled, weight, priority, cost_multiplier,
			allowed_models, limit_5h_usd, limit_5h_reset_mode
		) VALUES (
			$1, 'https://cost-limit-fixture.invalid', 'fixture-key', 'claude', true, 1, 0, 1,
			'[]', $2::numeric, 'rolling'
		) RETURNING id`,
		"cch-cost-limit-"+strconv.FormatInt(time.Now().UnixNano(), 10), limit5h,
	).Scan(&id)
	if err != nil {
		t.Fatalf("插入供应商夹具失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(context.Background(), `DELETE FROM providers WHERE id = $1`, id)
	})
	return id
}

// seedProviderLedgerCost 用 message_request 的触发器写账本（与生产同路径），
// 再登记清理（触发器只写不删，因此先删账本行再删请求行）。
func seedProviderLedgerCost(t *testing.T, pools *store.Pools, providerID int64, cost string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO message_request (
			provider_id, user_id, key, model, original_model, endpoint,
			status_code, input_tokens, output_tokens, cost_usd, duration_ms, ttfb_ms,
			error_message, user_agent, is_replay, created_at
		) VALUES (
			$1, 1, 'fixture-key', 'claude-3-5-sonnet', 'claude-3-5-sonnet', '/v1/messages',
			200, 100, 20, $2::numeric, 100, 50,
			NULL, 'fixture-ua', false, now()
		)`, providerID, cost)
	if err != nil {
		t.Fatalf("插入请求行（用于写账本）失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(context.Background(), `DELETE FROM usage_ledger WHERE provider_id = $1`, providerID)
		_, _ = cleanupPool.Exec(context.Background(), `DELETE FROM message_request WHERE provider_id = $1`, providerID)
	})
}

// newLimitService 用真实 PG + Redis 装配限额服务（与 cmd/cchd/ratelimit.go 同一配方，
// 只把 QuotaSource 换成不读 key/user 配额的最小实现）。
func newLimitService(
	t *testing.T,
	pools *store.Pools,
	redisClient redis.UniversalClient,
	ctx context.Context,
) *limit.Service {
	t.Helper()
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("加载内嵌 Lua 失败: %v", err)
	}
	scriptClient, err := ratelimit.New(redisClient, registry)
	if err != nil {
		t.Fatalf("构造脚本客户端失败: %v", err)
	}
	location, err := time.LoadLocation(pools.AdminSystemTimezoneOrUTC(ctx))
	if err != nil {
		t.Fatalf("系统时区不可加载: %v", err)
	}
	service, err := limit.New(limit.Config{
		Quotas:     stubQuotas{},
		Ledger:     pools,
		Redis:      scriptClient,
		Location:   location,
		SessionTTL: 300 * time.Second,
		Logger:     logx.New(nil),
	})
	if err != nil {
		t.Fatalf("构造限额服务失败: %v", err)
	}
	return service
}
