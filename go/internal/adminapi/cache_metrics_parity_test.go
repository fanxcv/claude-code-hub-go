package adminapi

import "testing"

// 读侧派生（requestCacheMetricAvailability / theoreticalCacheRate /
// requestCacheCoefficientBp）的对拍用例：期望值**逐条对着 Node 的
// src/lib/cache-effectiveness/request-metrics.ts 写**，不是照 Go 实现反推——
// 否则实现错了用例也会跟着错，对拍就成了自证。
//
// 为什么这一层要专门测：F3b 五个存储列现在会被写入（见 usage-logs-cost-cache-fields.md），
// 于是这三个派生列从「恒 not_recorded」变成「随存储列变化」，任何分支走错都会让
// provider 缓存回测的分子分母取错值——那是看不见的错数字，只能靠用例钉。
func TestDeriveRequestCacheMetricsMatchesNodeSpec(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	b := func(v bool) *bool { return &v }
	s := func(v string) *string { return &v }

	cases := []struct {
		name                 string
		inputTokens          *int64
		cacheCreation        *int64
		cacheRead            *int64
		theoretical          *int64
		eligible             *bool
		reason               *string
		wantInputTotal       int64
		wantAvailability     cacheMetricAvailability
		wantTheoreticalRate  any
		wantCoefficientBP    any
		wantActualRateIsNull bool
	}{
		{
			name:             "无 F3b 列 → not_recorded（历史行与关闭开关的行都走这里）",
			inputTokens:      i64(100),
			cacheRead:        i64(20),
			wantInputTotal:   120,
			wantAvailability: cacheMetricNotRecorded,
		},
		{
			name:                 "有排除原因且无输入 → availability 取原因、实际命中率留 NULL",
			inputTokens:          i64(0),
			reason:               s("no_affinity_key"),
			wantInputTotal:       0,
			wantAvailability:     cacheMetricNoAffinityKey,
			wantActualRateIsNull: true,
		},
		{
			name:                "有排除原因且有输入 → 原因 + 理论率，系数留 NULL",
			inputTokens:         i64(100),
			cacheRead:           i64(25),
			theoretical:         i64(50),
			reason:              s("stream_truncated"),
			wantInputTotal:      125,
			wantAvailability:    cacheMetricStreamTruncated,
			wantTheoreticalRate: 0.4,
		},
		{
			name:             "有 F3b 列但无输入 → no_input",
			eligible:         b(true),
			theoretical:      i64(0),
			wantInputTotal:   0,
			wantAvailability: cacheMetricNoInput,
		},
		{
			name:             "理论量为 nil 而归因链为空 → no_affinity_key（不是 available）",
			inputTokens:      i64(100),
			eligible:         b(false),
			theoretical:      nil,
			reason:           nil,
			wantInputTotal:   100,
			wantAvailability: cacheMetricNoAffinityKey,
		},
		{
			name:                "合格且理论量 > 0 → available + 系数按 bp 取整封顶",
			inputTokens:         i64(1000),
			cacheRead:           i64(250),
			eligible:            b(true),
			theoretical:         i64(500),
			wantInputTotal:      1250,
			wantAvailability:    cacheMetricAvailable,
			wantTheoreticalRate: 0.4,
			wantCoefficientBP:   5000, // 250 * 10000 / 500 = 5000
		},
		{
			name:                "系数封顶 10000（读量大于理论量时不越界）",
			inputTokens:         i64(1000),
			cacheRead:           i64(900),
			eligible:            b(true),
			theoretical:         i64(100),
			wantInputTotal:      1900,
			wantAvailability:    cacheMetricAvailable,
			wantTheoreticalRate: 0.05263157894736842,
			wantCoefficientBP:   10000,
		},
		{
			name:                "理论量为 0 → 系数留 NULL（分母为 0 不可算）",
			inputTokens:         i64(100),
			eligible:            b(true),
			theoretical:         i64(0),
			wantInputTotal:      100,
			wantAvailability:    cacheMetricAvailable,
			wantTheoreticalRate: 0.0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveRequestCacheMetrics(
				tc.inputTokens, tc.cacheCreation, tc.cacheRead, tc.theoretical, tc.eligible, tc.reason,
			)
			if got.CacheInputTotal != tc.wantInputTotal {
				t.Fatalf("cacheInputTotal = %d, want %d", got.CacheInputTotal, tc.wantInputTotal)
			}
			if got.RequestCacheMetricAvailability != tc.wantAvailability {
				t.Fatalf("availability = %q, want %q", got.RequestCacheMetricAvailability, tc.wantAvailability)
			}
			assertRate(t, "theoreticalCacheRate", got.TheoreticalCacheRate, tc.wantTheoreticalRate)
			assertRate(t, "requestCacheCoefficientBp", got.RequestCacheCoefficientBP, tc.wantCoefficientBP)
			if tc.wantActualRateIsNull && got.ActualCacheRate != nil {
				t.Fatalf("actualCacheRate = %v, want NULL", got.ActualCacheRate)
			}
		})
	}
}

func assertRate(t *testing.T, name string, got, want any) {
	t.Helper()
	switch expected := want.(type) {
	case nil:
		if got != nil {
			t.Fatalf("%s = %v, want NULL", name, got)
		}
	case float64:
		value, ok := got.(float64)
		if !ok {
			t.Fatalf("%s = %v（类型 %T）, want float64", name, got, got)
		}
		if diff := value - expected; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("%s = %v, want %v", name, value, expected)
		}
	case int:
		// 派生实现用 int64 承载 bp 系数（PG 的 bigint 语义），用例按十进制写期望值。
		switch value := got.(type) {
		case int:
			if value != expected {
				t.Fatalf("%s = %v, want %v", name, value, expected)
			}
		case int64:
			if value != int64(expected) {
				t.Fatalf("%s = %v, want %v", name, value, expected)
			}
		default:
			t.Fatalf("%s = %v（类型 %T）, want 整数", name, got, got)
		}
	default:
		t.Fatalf("用例写错了期望类型 %T", want)
	}
}
