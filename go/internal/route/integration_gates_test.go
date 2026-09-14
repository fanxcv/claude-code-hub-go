package route

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是**真实库**上的门槛集成测试：验证三组门槛列确实从 `providers` 读进了选路视图，
// 并按 Node 的步骤与理由口径生效。库门控沿用 integration_test.go 的 integrationPools。
//
// 为什么必须有：这三组列在接线前**根本没被读**（route.Provider 里没有字段），于是
// 「列读得到」这件事本身就是一个断言点——只靠单元测试构造 Provider 字面量是证不出来的。

const gateFixturePrefix = "cch-route-gate-"

// gateFixture 描述一条供应商夹具（只覆盖本文件要证的门槛列）。
type gateFixture struct {
	label       string
	activeStart *string
	activeEnd   *string
	allowed     string
	blocked     string
	limit5h     *float64
	limitTotal  *float64
}

// insertGateFixture 建一条专用供应商并登记清理。
func insertGateFixture(t *testing.T, pools *store.Pools, fixture gateFixture) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}

	allowed := fixture.allowed
	if allowed == "" {
		allowed = "[]"
	}
	blocked := fixture.blocked
	if blocked == "" {
		blocked = "[]"
	}

	var id int64
	err = pool.QueryRow(ctx, `
		INSERT INTO providers (
			name, url, key, provider_type, is_enabled, weight, priority, cost_multiplier,
			allowed_models, active_time_start, active_time_end, allowed_clients, blocked_clients,
			limit_5h_usd, limit_5h_reset_mode, limit_total_usd
		) VALUES (
			$1, 'https://route-gate-fixture.invalid', 'fixture-key', 'claude', true, 1, 0, 1,
			'[]', $2, $3, $4::jsonb, $5::jsonb,
			$6::numeric, 'rolling', $7::numeric
		) RETURNING id`,
		gateFixturePrefix+fixture.label+"-"+strconv.FormatInt(time.Now().UnixNano(), 10),
		fixture.activeStart, fixture.activeEnd, allowed, blocked, fixture.limit5h, fixture.limitTotal,
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

// findProvider 从选路视图里取指定 id 的供应商。
func findProvider(t *testing.T, providers []Provider, id int64) Provider {
	t.Helper()
	for _, provider := range providers {
		if provider.ID == id {
			return provider
		}
	}
	t.Fatalf("选路视图里没有供应商 %d", id)
	return Provider{}
}

// TestIntegrationStoreSourceProjectsGateColumns 断言三组门槛列真的读进了选路视图，
// 且与库中写入的原始值逐字段一致（列名写错、类型不匹配都会在这里红）。
func TestIntegrationStoreSourceProjectsGateColumns(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()

	start, end := "01:00", "05:00"
	limit5h := 12.5
	limitTotal := 300.0
	id := insertGateFixture(t, pools, gateFixture{
		label:       "projection",
		activeStart: &start,
		activeEnd:   &end,
		allowed:     `["claude-code-*","my-client"]`,
		blocked:     `["curl*"]`,
		limit5h:     &limit5h,
		limitTotal:  &limitTotal,
	})

	providers, err := NewStoreSource(pools).Providers(ctx)
	if err != nil {
		t.Fatalf("读取供应商失败: %v", err)
	}
	provider := findProvider(t, providers, id)

	if provider.ActiveTimeStart == nil || *provider.ActiveTimeStart != start {
		t.Errorf("active_time_start = %v，期望 %q", provider.ActiveTimeStart, start)
	}
	if provider.ActiveTimeEnd == nil || *provider.ActiveTimeEnd != end {
		t.Errorf("active_time_end = %v，期望 %q", provider.ActiveTimeEnd, end)
	}
	var allowed []string
	if err := json.Unmarshal(provider.AllowedClients, &allowed); err != nil || len(allowed) != 2 {
		t.Errorf("allowed_clients = %s（err=%v），期望两条模式", provider.AllowedClients, err)
	}
	var blocked []string
	if err := json.Unmarshal(provider.BlockedClients, &blocked); err != nil || len(blocked) != 1 {
		t.Errorf("blocked_clients = %s（err=%v），期望一条模式", provider.BlockedClients, err)
	}
	if provider.CostLimits.Limit5hUSD == nil || *provider.CostLimits.Limit5hUSD != limit5h {
		t.Errorf("limit_5h_usd = %v，期望 %v", provider.CostLimits.Limit5hUSD, limit5h)
	}
	if provider.CostLimits.Limit5hResetMode == nil || *provider.CostLimits.Limit5hResetMode != "rolling" {
		t.Errorf("limit_5h_reset_mode = %v，期望 rolling", provider.CostLimits.Limit5hResetMode)
	}
	if provider.CostLimits.LimitTotalUSD == nil || *provider.CostLimits.LimitTotalUSD != limitTotal {
		t.Errorf("limit_total_usd = %v，期望 %v", provider.CostLimits.LimitTotalUSD, limitTotal)
	}
	// 未设的限额列必须是 nil（nil = 该维度不判），不能被读成 0——0 会被判成「已达上限」。
	if provider.CostLimits.LimitDailyUSD != nil {
		t.Errorf("未设置的 limit_daily_usd 应为 nil，实际 %v", *provider.CostLimits.LimitDailyUSD)
	}
}

// excludingWindow 在系统时区下找一个**不含 now** 的合法 HH:mm 窗口。
//
// 需要搜索而不是固定值：窗口跨零点时 start>end 会变成跨日语义，可能反而把 now 包进来。
func excludingWindow(t *testing.T, now time.Time) (string, string) {
	t.Helper()
	for _, offset := range []int{1, 2, 3, 5, 7, 9, 11} {
		start := now.Add(time.Duration(offset) * time.Hour)
		end := start.Add(time.Hour)
		startText, endText := start.Format("15:04"), end.Format("15:04")
		if !ProviderActiveNow(&startText, &endText, now) {
			return startText, endText
		}
	}
	t.Fatalf("没能构造出排除当前时刻的活动时段（now=%s）", now.Format(time.RFC3339))
	return "", ""
}

// TestIntegrationScheduleGateExcludesProviderOutsideWindow 断言：库里的活动时段列 → 选路视图
// → 请求级 ScheduleGate 判定（与 guard 适配器同一构造方式：时区解析一次 + ProviderActiveNow），
// 最终记 `schedule_inactive` 且文案与 Node 逐字一致。
func TestIntegrationScheduleGateExcludesProviderOutsideWindow(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()

	location, err := time.LoadLocation(pools.AdminSystemTimezoneOrUTC(ctx))
	if err != nil {
		t.Fatalf("系统时区不可加载: %v", err)
	}
	now := time.Now().In(location)
	start, end := excludingWindow(t, now)

	id := insertGateFixture(t, pools, gateFixture{
		label:       "schedule",
		activeStart: &start,
		activeEnd:   &end,
	})

	selector := NewSelector(Options{Source: NewStoreSource(pools)})
	result, err := selector.Select(ctx, Request{
		Model:  "claude-3-5-sonnet",
		Format: "claude",
		// 与 guard 适配器同构：解析一次时区，再用同一个 now 逐候选判定。
		ScheduleGate: func(p Provider) bool {
			return ProviderActiveNow(p.ActiveTimeStart, p.ActiveTimeEnd, now)
		},
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}

	record := recordOf(t, result.Context, id)
	if record.Reason != ReasonScheduleInactive {
		t.Errorf("理由 = %q，期望 schedule_inactive（活动时段 %s-%s，now=%s）",
			record.Reason, start, end, now.Format("15:04"))
	}
	if want := "outside active window " + start + "-" + end; record.Details != want {
		t.Errorf("文案 = %q，期望 %q（Node provider-selector.ts:1356）", record.Details, want)
	}
}

// TestIntegrationClientGateRecordsBlockedProvider 断言：库里的 allowed/blocked_clients 列
// 能原样带到判定处（此处用与 guard 同形的判据构造结果，真实 UA 匹配由 guard 侧集成测试覆盖），
// 并留下与 Node 同字段的 clientRestrictionContext。
func TestIntegrationClientGateRecordsBlockedProvider(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()

	id := insertGateFixture(t, pools, gateFixture{
		label:   "client",
		blocked: `["claude-code-cli"]`,
	})

	// 判据与 guard 的 ProviderClientRestriction 同形：先黑名单命中，details 取 matchType。
	selector := NewSelector(Options{Source: NewStoreSource(pools)})
	result, err := selector.Select(ctx, Request{
		Model:  "claude-3-5-sonnet",
		Format: "claude",
		ClientGate: func(p Provider) *ClientRestriction {
			if p.ID != id {
				return nil
			}
			var blocked []string
			if err := json.Unmarshal(p.BlockedClients, &blocked); err != nil {
				t.Fatalf("解析 blocked_clients 失败: %v", err)
			}
			if len(blocked) != 1 || blocked[0] != "claude-code-cli" {
				t.Fatalf("blocked_clients 未按库中值读出: %v", blocked)
			}
			return &ClientRestriction{
				Allowed:          false,
				MatchType:        MatchTypeBlocklistHit,
				MatchedPattern:   blocked[0],
				DetectedClient:   "claude-code-cli",
				CheckedAllowlist: []string{},
				CheckedBlocklist: blocked,
			}
		},
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}

	record := recordOf(t, result.Context, id)
	if record.Reason != ReasonClientRestriction || record.Details != MatchTypeBlocklistHit {
		t.Errorf("理由/文案 = %q/%q，期望 client_restriction/blocklist_hit",
			record.Reason, record.Details)
	}
	if record.ClientRestrictionContext == nil {
		t.Fatalf("缺少 clientRestrictionContext: %+v", record)
	}
	if got := record.ClientRestrictionContext.ProviderBlocklist; len(got) != 1 ||
		got[0] != "claude-code-cli" {
		t.Errorf("providerBlocklist = %v，期望库中原值", got)
	}
}
