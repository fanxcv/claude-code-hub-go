package limit

import (
	"testing"
	"time"
)

func TestEntityLabel(t *testing.T) {
	cases := map[Entity]string{
		EntityKey:      "Key",
		EntityProvider: "供应商",
		EntityUser:     "User",
	}
	for entity, want := range cases {
		if got := entity.Label(); got != want {
			t.Errorf("Label(%q) = %q, 期望 %q", entity, got, want)
		}
	}
}

func TestCostKeys(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"5h rolling", Cost5hKey(EntityKey, 7, ResetRolling), "key:7:cost_5h_rolling"},
		{"5h fixed", Cost5hKey(EntityUser, 9, ResetFixed), "user:9:cost_5h_fixed"},
		{"daily rolling", CostDailyRollingKey(EntityProvider, 3), "provider:3:cost_daily_rolling"},
		{"daily fixed 默认 00:00", CostDailyFixedKey(EntityKey, 7, ""), "key:7:cost_daily_0000"},
		{"daily fixed 18:00", CostDailyFixedKey(EntityKey, 7, "18:00"), "key:7:cost_daily_1800"},
		{"daily fixed 非零分钟", CostDailyFixedKey(EntityUser, 4, "9:05"), "user:4:cost_daily_0905"},
		{"weekly", CostPeriodFixedKey(EntityKey, 7, PeriodWeekly), "key:7:cost_weekly"},
		{"monthly", CostPeriodFixedKey(EntityProvider, 3, PeriodMonthly), "provider:3:cost_monthly"},
		{"rpm", RPMWindowKey(42), "user:42:rpm_window"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: 得到 %q，期望 %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestActiveSessionKeysShareHashTag(t *testing.T) {
	global := ActiveSessionsGlobalKey()
	keyScope := KeyActiveSessionsKey(11)
	userScope := UserActiveSessionsKey(22)

	if global != "{active_sessions}:global:active_sessions" {
		t.Fatalf("全局键形制不符: %q", global)
	}
	// Redis Cluster 下 Lua 一次操作这三个键，必须同 slot，否则直接 CROSSSLOT。
	for _, item := range []string{global, keyScope, userScope} {
		if tag, err := hashTagOf(item); err != nil || tag != "{active_sessions}" {
			t.Fatalf("%q 的 hash tag 为 %q（err=%v），期望 {active_sessions}", item, tag, err)
		}
	}
	// provider 维度的脚本只动同前缀两键，Node 侧也未加 tag。
	if got := ProviderActiveSessionsKey(3); got != "provider:3:active_sessions" {
		t.Errorf("供应商并发键形制不符: %q", got)
	}
	if got := ProviderSessionRefsKey(3); got != "provider:3:active_session_refs" {
		t.Errorf("供应商引用键形制不符: %q", got)
	}
}

func TestTotalCostCacheKey(t *testing.T) {
	resetAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	if got := TotalCostCacheKey(EntityKey, 1, "sk-abc", nil); got != "total_cost:key:sk-abc" {
		t.Errorf("Key 无重置时刻: %q", got)
	}
	want := "total_cost:key:sk-abc:" + itoa(resetAt.UnixMilli())
	if got := TotalCostCacheKey(EntityKey, 1, "sk-abc", &resetAt); got != want {
		t.Errorf("Key 带重置时刻: 得到 %q，期望 %q", got, want)
	}
	if got := TotalCostCacheKey(EntityUser, 5, "", nil); got != "total_cost:user:5" {
		t.Errorf("User 无重置时刻: %q", got)
	}
	// provider 维度无重置时刻时补 :none，与 Node 侧一致。
	if got := TotalCostCacheKey(EntityProvider, 6, "", nil); got != "total_cost:provider:6:none" {
		t.Errorf("Provider 无重置时刻: %q", got)
	}
}

// hashTagOf 取出 Redis 键的 hash tag（{...} 之间的内容）。
func hashTagOf(key string) (string, error) {
	start := -1
	for index := 0; index < len(key); index++ {
		if key[index] == '{' {
			start = index
			continue
		}
		if key[index] == '}' && start >= 0 {
			if index == start+1 {
				return "", errEmptyHashTag
			}
			return key[start : index+1], nil
		}
	}
	return "", errNoHashTag
}

var (
	errEmptyHashTag = &testError{"空 hash tag"}
	errNoHashTag    = &testError{"无 hash tag"}
)

type testError struct{ message string }

func (e *testError) Error() string { return e.message }

func itoa(value int64) string { return FormatCostText(float64(value)) }
