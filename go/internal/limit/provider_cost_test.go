package limit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

const providerCostTestID int64 = 7

var errProviderCostLedger = errors.New("provider_cost_test: 账本读失败")

// windowLedger 是按窗口跨度（end-start）区分的账本桩：供应商各周期限额读的是同一个账本接口，
// 只有窗口起点不同，故用跨度把维度区分开，才能验「命中顺序」。
//
// 未登记的跨度返回错误而不是 0：维度读错窗口会立刻变成可复现的失败，而不是静默放行。
type windowLedger struct {
	amounts  map[time.Duration]float64
	errs     map[time.Duration]error
	spans    []time.Duration
	total    float64
	totalErr error
}

func (l *windowLedger) SumLedgerCostInTimeRange(
	_ context.Context, _ store.LedgerEntityType, _ any, start, end time.Time,
) (string, error) {
	span := end.Sub(start)
	l.spans = append(l.spans, span)
	if err := l.errs[span]; err != nil {
		return "", err
	}
	amount, ok := l.amounts[span]
	if !ok {
		return "", fmt.Errorf("provider_cost_test: 未登记的窗口跨度 %v", span)
	}
	return FormatCostText(amount), nil
}

func (l *windowLedger) SumLedgerTotalCost(
	_ context.Context, _ store.LedgerEntityType, _ any, _ *time.Time,
) (string, error) {
	if l.totalErr != nil {
		return "", l.totalErr
	}
	return FormatCostText(l.total), nil
}

// providerCostWindowSpans 给出四个周期维度在 fixedNow 下的账本查询跨度（日窗口用 20:00 重置，
// 以与 5h 滚动窗口和「本月起点」的跨度区分开）。
func providerCostWindowSpans(t *testing.T) (span5h, spanDaily, spanWeekly, spanMonthly time.Duration) {
	t.Helper()
	const dailyResetTime = "20:00"
	span5h = 5 * time.Hour
	spanDaily = fixedNow.Sub(DailyWindowStart(fixedNow, dailyResetTime, time.UTC))
	spanWeekly = fixedNow.Sub(WeekStart(fixedNow, time.UTC))
	spanMonthly = fixedNow.Sub(MonthStart(fixedNow, time.UTC))
	spans := map[string]time.Duration{
		"5h": span5h, "daily": spanDaily, "weekly": spanWeekly, "monthly": spanMonthly,
	}
	seen := map[time.Duration]string{}
	for name, span := range spans {
		if other, dup := seen[span]; dup {
			t.Fatalf("窗口跨度撞车：%s 与 %s 都是 %v，本用例无法区分维度", name, other, span)
		}
		seen[span] = name
	}
	return span5h, spanDaily, spanWeekly, spanMonthly
}

// providerCostLimits 是四个周期限额都生效的入参（金额即上限，断言里对得上）。
func providerCostLimits() ProviderCostLimits {
	dailyResetTime := "20:00"
	return ProviderCostLimits{
		Limit5hUSD:      floatPtr(10),
		LimitDailyUSD:   floatPtr(20),
		DailyResetTime:  &dailyResetTime,
		LimitWeeklyUSD:  floatPtr(30),
		LimitMonthlyUSD: floatPtr(40),
	}
}

// 维度顺序是契约：5 小时 → 每日 → 周 → 月，同时触顶时先报靠前的维度（Node 的同序判定）。
func TestCheckProviderCostLimitsReportsEarliestExceededDimension(t *testing.T) {
	span5h, spanDaily, spanWeekly, spanMonthly := providerCostWindowSpans(t)
	cases := []struct {
		name       string
		amounts    map[time.Duration]float64
		wantReason string
	}{
		{
			name:       "四维均未触顶时放行",
			amounts:    map[time.Duration]float64{span5h: 9, spanDaily: 19, spanWeekly: 29, spanMonthly: 39},
			wantReason: "",
		},
		{
			name:       "5 小时与每日同时触顶时先报 5 小时",
			amounts:    map[time.Duration]float64{span5h: 11, spanDaily: 21, spanWeekly: 29, spanMonthly: 39},
			wantReason: "供应商 5小时消费上限已达到（11.0000/10）",
		},
		{
			name:       "每日与周同时触顶时先报每日",
			amounts:    map[time.Duration]float64{span5h: 9, spanDaily: 25, spanWeekly: 35, spanMonthly: 39},
			wantReason: "供应商 每日消费上限已达到（25.0000/20）",
		},
		{
			name:       "周与月同时触顶时先报周",
			amounts:    map[time.Duration]float64{span5h: 9, spanDaily: 19, spanWeekly: 35, spanMonthly: 45},
			wantReason: "供应商 周消费上限已达到（35.0000/30）",
		},
		{
			name:       "仅月触顶时报月",
			amounts:    map[time.Duration]float64{span5h: 9, spanDaily: 19, spanWeekly: 29, spanMonthly: 45},
			wantReason: "供应商 月消费上限已达到（45.0000/40）",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := newTestService(t, &fakeQuotaSource{}, &windowLedger{amounts: tc.amounts})
			if service.windows.Ready() {
				t.Fatal("本用例以「Redis 未接线、走账本回退」为前提")
			}

			allowed, reason := service.CheckProviderCostLimits(context.Background(), providerCostTestID, providerCostLimits())
			if tc.wantReason == "" {
				if !allowed || reason != "" {
					t.Fatalf("未触顶应放行且无理由，得 allowed=%v reason=%q", allowed, reason)
				}
				return
			}
			if allowed {
				t.Fatalf("触顶应拦截，得 reason=%q", reason)
			}
			if reason != tc.wantReason {
				t.Fatalf("理由 = %q，期望 %q", reason, tc.wantReason)
			}
		})
	}
}

// 总额度排在四个周期维度之后：两者同时超时仍报周期维度，只有周期维度都放行时才报总额度。
func TestCheckProviderCostLimitsTotalIsCheckedAfterPeriods(t *testing.T) {
	span5h, spanDaily, spanWeekly, spanMonthly := providerCostWindowSpans(t)
	limits := providerCostLimits()
	limits.LimitTotalUSD = floatPtr(100)

	t.Run("仅总额度超限时以其理由拦截", func(t *testing.T) {
		ledger := &windowLedger{
			amounts: map[time.Duration]float64{span5h: 9, spanDaily: 19, spanWeekly: 29, spanMonthly: 39},
			total:   105,
		}
		allowed, reason := newTestService(t, &fakeQuotaSource{}, ledger).
			CheckProviderCostLimits(context.Background(), providerCostTestID, limits)
		if allowed {
			t.Fatal("总额度超限应拦截")
		}
		if want := "供应商 total spending limit reached (105/100)"; reason != want {
			t.Fatalf("理由 = %q，期望 %q", reason, want)
		}
	})

	t.Run("月与总额度同时超限时先报月", func(t *testing.T) {
		ledger := &windowLedger{
			amounts: map[time.Duration]float64{span5h: 9, spanDaily: 19, spanWeekly: 29, spanMonthly: 45},
			total:   105,
		}
		allowed, reason := newTestService(t, &fakeQuotaSource{}, ledger).
			CheckProviderCostLimits(context.Background(), providerCostTestID, limits)
		if allowed {
			t.Fatal("月触顶应拦截")
		}
		if want := "供应商 月消费上限已达到（45.0000/40）"; reason != want {
			t.Fatalf("理由 = %q，期望 %q", reason, want)
		}
	})
}

// Fail Open：Redis 未就绪且账本不可用时不得因读取失败而谎报「该供应商被排除」。
func TestCheckProviderCostLimitsFailOpenWithoutReadableWindow(t *testing.T) {
	span5h, spanDaily, spanWeekly, spanMonthly := providerCostWindowSpans(t)

	t.Run("账本未接线时全维放行", func(t *testing.T) {
		service := newTestService(t, &fakeQuotaSource{}, nil)
		if service.windows.Ready() {
			t.Fatal("本用例以「Redis 未接线」为前提")
		}
		allowed, reason := service.CheckProviderCostLimits(context.Background(), providerCostTestID, providerCostLimits())
		if !allowed || reason != "" {
			t.Fatalf("读取不可用应放行且无理由，得 allowed=%v reason=%q", allowed, reason)
		}
	})

	t.Run("账本读失败时全维放行", func(t *testing.T) {
		ledger := &windowLedger{errs: map[time.Duration]error{
			span5h: errProviderCostLedger, spanDaily: errProviderCostLedger,
			spanWeekly: errProviderCostLedger, spanMonthly: errProviderCostLedger,
		}}
		limits := providerCostLimits()
		limits.LimitTotalUSD = floatPtr(100)
		ledger.totalErr = errProviderCostLedger

		allowed, reason := newTestService(t, &fakeQuotaSource{}, ledger).
			CheckProviderCostLimits(context.Background(), providerCostTestID, limits)
		if !allowed || reason != "" {
			t.Fatalf("读失败应放行且无理由，得 allowed=%v reason=%q", allowed, reason)
		}
	})
}

// 降级只作用于读失败的那一维：其余维度照常判定，不因一维失败而整条链放行。
func TestCheckProviderCostLimitsSkipsOnlyFailedDimension(t *testing.T) {
	span5h, spanDaily, spanWeekly, spanMonthly := providerCostWindowSpans(t)
	ledger := &windowLedger{
		amounts: map[time.Duration]float64{spanDaily: 25, spanWeekly: 29, spanMonthly: 39},
		errs:    map[time.Duration]error{span5h: errProviderCostLedger},
	}
	service := newTestService(t, &fakeQuotaSource{}, ledger)

	allowed, reason := service.CheckProviderCostLimits(context.Background(), providerCostTestID, providerCostLimits())
	if allowed {
		t.Fatal("5h 读失败不应影响每日维度触顶的拦截")
	}
	if want := "供应商 每日消费上限已达到（25.0000/20）"; reason != want {
		t.Fatalf("理由 = %q，期望 %q", reason, want)
	}
}
