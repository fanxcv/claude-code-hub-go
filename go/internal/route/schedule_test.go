package route

import (
	"testing"
	"time"
)

// TestProviderActiveNow 是活动时段判定的表驱动钉子（对齐 Node provider-schedule.ts:34-60）。
//
// 本钉子随唯一实现从 internal/dataplane 迁到本包：此前两处各一份实现（dataplane 与
// route/simulate），合并后钉子也必须跟着唯一实现走，否则会出现「实现只有一个、覆盖却挂在
// 被删的那份上」的假绿。
func TestProviderActiveNow(t *testing.T) {
	ptr := func(value string) *string { return &value }
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 9, 13, hour, minute, 0, 0, time.UTC)
	}
	cases := []struct {
		name  string
		start *string
		end   *string
		now   time.Time
		want  bool
	}{
		{"两端为空即恒活跃", nil, nil, at(3, 0), true},
		{"仅一端有值也恒活跃", ptr("09:00"), nil, at(3, 0), true},
		{"start 等于 end 恒不活跃", ptr("09:00"), ptr("09:00"), at(9, 0), false},
		{"同日窗口内", ptr("09:00"), ptr("17:00"), at(12, 0), true},
		{"同日窗口左闭", ptr("09:00"), ptr("17:00"), at(9, 0), true},
		{"同日窗口右开", ptr("09:00"), ptr("17:00"), at(17, 0), false},
		{"同日窗口之前", ptr("09:00"), ptr("17:00"), at(8, 59), false},
		{"跨日窗口前段", ptr("22:00"), ptr("02:00"), at(23, 30), true},
		{"跨日窗口后段", ptr("22:00"), ptr("02:00"), at(1, 0), true},
		{"跨日窗口右开", ptr("22:00"), ptr("02:00"), at(2, 0), false},
		{"跨日窗口之外", ptr("22:00"), ptr("02:00"), at(12, 0), false},
		{"格式非法 fail-open", ptr("9:00"), ptr("17:00"), at(3, 0), true},
		{"不可解析 fail-open", ptr("garbage"), ptr("17:00"), at(3, 0), true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := ProviderActiveNow(item.start, item.end, item.now); got != item.want {
				t.Fatalf("want %v got %v", item.want, got)
			}
		})
	}
}
