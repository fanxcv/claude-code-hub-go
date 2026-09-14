package route

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// TestScheduleGateUsesRequestGateWithNodeDetails 钉住：
//  1. 请求级 ScheduleGate 生效（数据面走这条）；
//  2. 排除理由与文案与 Node 逐字一致（`outside active window {start}-{end}`，
//     provider-selector.ts:1356）；
//  3. 调度窗口属**基础过滤**（Step 2a-2），因此它会把 enabledProviders 打掉。
func TestScheduleGateUsesRequestGateWithNodeDetails(t *testing.T) {
	inactive := baseProvider(1, convert.ProviderClaude)
	inactive.ActiveTimeStart = strPtr("01:00")
	inactive.ActiveTimeEnd = strPtr("05:00")

	active := baseProvider(2, convert.ProviderClaude)
	active.ActiveTimeStart = strPtr("00:00")
	active.ActiveTimeEnd = strPtr("23:59")

	selector := NewSelector(Options{Source: &stubSource{providers: []Provider{inactive, active}}})
	result, err := selector.Select(context.Background(), Request{
		ScheduleGate: func(p Provider) bool { return p.ID == active.ID },
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}

	if got := reasonOf(t, result.Context, inactive.ID); got != ReasonScheduleInactive {
		t.Errorf("非活动时段供应商的理由 = %q，期望 %q", got, ReasonScheduleInactive)
	}
	details := detailsOf(t, result.Context, inactive.ID)
	if details != "outside active window 01:00-05:00" {
		t.Errorf("理由文案 = %q，期望与 Node 逐字一致", details)
	}
	if result.Context.EnabledProviders != 1 {
		t.Errorf("enabledProviders = %d，期望 1（调度窗口属基础过滤）",
			result.Context.EnabledProviders)
	}
	if result.Context.AfterGroupFilter == nil || *result.Context.AfterGroupFilter != 1 {
		t.Errorf("afterGroupFilter = %v，期望 1（与 Node 的基础过滤计数同口径）",
			result.Context.AfterGroupFilter)
	}
}

// TestScheduleGateFallsBackToSelectorGates 钉住兼容面：无请求级钩子时用选择器级 Gates.Schedule。
func TestScheduleGateFallsBackToSelectorGates(t *testing.T) {
	inactive := baseProvider(1, convert.ProviderClaude)
	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{inactive}},
		Gates:  Gates{Schedule: func(Provider) bool { return false }},
	})
	result, err := selector.Select(context.Background(), Request{})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got := reasonOf(t, result.Context, inactive.ID); got != ReasonScheduleInactive {
		t.Errorf("理由 = %q，期望 %q", got, ReasonScheduleInactive)
	}
}

// TestProviderCostLimitsGateRunsAtHealthStep 钉住限额判定的**步骤归属**：
// Node 在 Step 4（filterByLimits）判金额限额，因此超限供应商应仍被计入
// enabledProviders / afterGroupFilter，只在 afterHealthCheck 上掉下来，理由是 rate_limited。
// 放到 Step 2 判定会让这几个计数与 Node 分叉（这是本次修复的起因）。
func TestProviderCostLimitsGateRunsAtHealthStep(t *testing.T) {
	overLimit := baseProvider(1, convert.ProviderClaude)
	healthy := baseProvider(2, convert.ProviderClaude)

	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{overLimit, healthy}},
		Gates: Gates{Limits: func(_ context.Context, p Provider) (bool, string) {
			return p.ID != overLimit.ID, "5h cost limit reached (usage: 1.0000/0.5000)"
		}},
	})
	result, err := selector.Select(context.Background(), Request{})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}

	if got := reasonOf(t, result.Context, overLimit.ID); got != ReasonRateLimited {
		t.Errorf("超限供应商的理由 = %q，期望 %q", got, ReasonRateLimited)
	}
	// Node 在此分支不记具体窗口文案，details 恒为 `rate_limited`（provider-selector.ts:1441）。
	if got := detailsOf(t, result.Context, overLimit.ID); got != "rate_limited" {
		t.Errorf("超限理由文案 = %q，期望 rate_limited", got)
	}
	if result.Context.EnabledProviders != 2 {
		t.Errorf("enabledProviders = %d，期望 2（限额在 Step 4 判，不该影响 Step 2 计数）",
			result.Context.EnabledProviders)
	}
	if result.Context.BeforeHealthCheck != 2 {
		t.Errorf("beforeHealthCheck = %d，期望 2", result.Context.BeforeHealthCheck)
	}
	if result.Context.AfterHealthCheck != 1 {
		t.Errorf("afterHealthCheck = %d，期望 1", result.Context.AfterHealthCheck)
	}
	if result.Provider == nil || result.Provider.ID != healthy.ID {
		t.Errorf("选中供应商 = %+v，期望健康的那一个", result.Provider)
	}
}

// TestHealthStepCircuitReasonWinsOverLimits 钉住 Step 4 内的顺序：熔断先于限额
// （Node filterByLimits：vendor-type 熔断 → 供应商熔断 → 金额限额），故熔断开启时
// 记 circuit_open 而不是 rate_limited。
func TestHealthStepCircuitReasonWinsOverLimits(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	open := baseProvider(1, convert.ProviderClaude)
	client := &fakeRedis{hashes: map[string]map[string]string{
		providerStateKey(open.ID): {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
		},
	}}
	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{open}},
		Health: newTestHealth(t, client, true),
		Gates: Gates{Limits: func(context.Context, Provider) (bool, string) {
			return false, "rate_limited"
		}},
	})
	result, err := selector.Select(context.Background(), Request{})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got := reasonOf(t, result.Context, open.ID); got != ReasonCircuitOpen {
		t.Errorf("理由 = %q，期望 %q", got, ReasonCircuitOpen)
	}
}

// TestClientGateRecordsNodeRestrictionContext 钉住供应商级客户端限制：
// 理由 client_restriction、details 取 matchType（blocklist_hit / allowlist_miss），
// 并带上与 Node 同字段的 clientRestrictionContext 留痕；两侧名单都空时不产生条目。
func TestClientGateRecordsNodeRestrictionContext(t *testing.T) {
	blocked := baseProvider(1, convert.ProviderClaude)
	unrestricted := baseProvider(2, convert.ProviderClaude)

	selector := NewSelector(Options{Source: &stubSource{providers: []Provider{blocked, unrestricted}}})
	result, err := selector.Select(context.Background(), Request{
		ClientGate: func(p Provider) *ClientRestriction {
			if p.ID != blocked.ID {
				// 两侧名单都空 → Node 直接放行且不判定（返回 nil 表达同一语义）。
				return nil
			}
			return &ClientRestriction{
				Allowed:          false,
				MatchType:        MatchTypeBlocklistHit,
				MatchedPattern:   "claude-code-cli",
				DetectedClient:   "claude-code-cli",
				CheckedAllowlist: []string{},
				CheckedBlocklist: []string{"claude-code-cli"},
			}
		},
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}

	if got := reasonOf(t, result.Context, blocked.ID); got != ReasonClientRestriction {
		t.Errorf("理由 = %q，期望 %q", got, ReasonClientRestriction)
	}
	record := recordOf(t, result.Context, blocked.ID)
	if record.Details != MatchTypeBlocklistHit {
		t.Errorf("details = %q，期望 %q", record.Details, MatchTypeBlocklistHit)
	}
	if record.ClientRestrictionContext == nil {
		t.Fatalf("缺少 clientRestrictionContext 留痕: %+v", record)
	}
	got := record.ClientRestrictionContext
	if got.MatchType != MatchTypeBlocklistHit || got.MatchedPattern != "claude-code-cli" ||
		got.DetectedClient != "claude-code-cli" || len(got.ProviderBlocklist) != 1 ||
		got.ProviderBlocklist[0] != "claude-code-cli" || got.ProviderAllowlist == nil {
		t.Errorf("clientRestrictionContext = %+v，与 Node 字段不符", got)
	}
	// 无名单的供应商不得出现任何过滤条目。
	for _, item := range result.Context.FilteredProviders {
		if item.ID == unrestricted.ID {
			t.Errorf("无名单供应商不该被记录为过滤项: %+v", item)
		}
	}
	if result.Context.EnabledProviders != 1 {
		t.Errorf("enabledProviders = %d，期望 1", result.Context.EnabledProviders)
	}
}

// TestClientGateAllowlistMissDetails 钉住白名单未命中的 details 取值。
func TestClientGateAllowlistMissDetails(t *testing.T) {
	provider := baseProvider(1, convert.ProviderClaude)
	selector := NewSelector(Options{Source: &stubSource{providers: []Provider{provider}}})
	result, err := selector.Select(context.Background(), Request{
		ClientGate: func(Provider) *ClientRestriction {
			return &ClientRestriction{
				Allowed:          false,
				MatchType:        MatchTypeAllowlistMiss,
				DetectedClient:   "curl/8.0",
				CheckedAllowlist: []string{"claude-code*"},
				CheckedBlocklist: []string{},
			}
		},
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	record := recordOf(t, result.Context, provider.ID)
	if record.Details != MatchTypeAllowlistMiss {
		t.Errorf("details = %q，期望 %q", record.Details, MatchTypeAllowlistMiss)
	}
}

func recordOf(t *testing.T, dc DecisionContext, id int64) Filtered {
	t.Helper()
	for _, record := range dc.FilteredProviders {
		if record.ID == id {
			return record
		}
	}
	t.Fatalf("决策上下文里没有供应商 %d 的过滤记录: %+v", id, dc.FilteredProviders)
	return Filtered{}
}

func detailsOf(t *testing.T, dc DecisionContext, id int64) string {
	t.Helper()
	return recordOf(t, dc, id).Details
}
