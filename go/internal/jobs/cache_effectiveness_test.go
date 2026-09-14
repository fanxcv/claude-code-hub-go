package jobs

import (
	"testing"
	"time"
)

// 本文件是无库单测：钉住本任务唯一的运行期开关（系统设置 vs env 的优先级）与窗口推进算法。
//
// 为什么这两块要单独测：窗口推进与开关判定是「写错了也不报错、只是静静不干活或算错区间」的
// 两处——它们不出现在任何请求路径上，只有日志里一行 skipped。SQL 语义由真库集成用例覆盖
// （见 cache_effectiveness_integration_test.go）。

// TestCacheEffectivenessEnabledPrefersSettingOverEnv 钉住 Node 的开关优先级：
// 设置里显式为 false 时关闭；设置为 null（缺行/未设置）时回落 env。
//
// 依据 `src/lib/system-settings/proxy-runtime.ts:77-78`：
//
//	cacheEffectivenessEnabled: settings.cacheEffectivenessEnabled ?? envCacheEffectivenessDefault()
func TestCacheEffectivenessEnabledPrefersSettingOverEnv(t *testing.T) {
	enabled, disabled := true, false
	cases := []struct {
		name        string
		setting     *bool
		envDefault  bool
		wantEnabled bool
	}{
		{"设置未设置时回落 env（env 开）", nil, true, true},
		{"设置未设置时回落 env（env 关）", nil, false, false},
		{"设置显式开（env 关也开）", &enabled, false, true},
		{"设置显式关（env 开也关）", &disabled, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheEffectivenessEnabled(tc.setting, tc.envDefault); got != tc.wantEnabled {
				t.Fatalf("cacheEffectivenessEnabled(%v, %v) = %v，期望 %v",
					tc.setting, tc.envDefault, got, tc.wantEnabled)
			}
		})
	}
}

// TestCacheEffectivenessWindow 钉住窗口推进（Node `service.ts:62-80`）：
//   - 无历史 → 回看 1 小时；
//   - 有历史 → 从上次 window_end 续上（不重不漏靠这一点）；
//   - windowStart >= windowEnd → 不推进（含**恰好相等**与时钟回拨两种）；
//   - 终点恒为 now - 15 分钟（终态迟到缓冲）。
func TestCacheEffectivenessWindow(t *testing.T) {
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)

	t.Run("首次运行回看一小时", func(t *testing.T) {
		start, end, advanced := cacheEffectivenessWindow(nil, now)
		if !advanced {
			t.Fatal("首次运行应当推进窗口")
		}
		if want := now.Add(-cacheEffectivenessInitialLookback); !start.Equal(want) {
			t.Fatalf("windowStart = %s，期望 %s", start, want)
		}
		if want := now.Add(-cacheEffectivenessSafetyLag); !end.Equal(want) {
			t.Fatalf("windowEnd = %s，期望 %s", end, want)
		}
	})

	t.Run("从上次终点续上", func(t *testing.T) {
		last := now.Add(-40 * time.Minute)
		start, end, advanced := cacheEffectivenessWindow(&last, now)
		if !advanced {
			t.Fatal("水位早于终点时应推进")
		}
		if !start.Equal(last) {
			// 起点必须逐位等于上次终点：否则要么漏算（起点偏后）要么重复计（起点偏前）。
			t.Fatalf("windowStart = %s，期望等于上次 window_end %s", start, last)
		}
		if want := now.Add(-cacheEffectivenessSafetyLag); !end.Equal(want) {
			t.Fatalf("windowEnd = %s，期望 %s", end, want)
		}
	})

	t.Run("水位恰等于终点时不推进", func(t *testing.T) {
		last := now.Add(-cacheEffectivenessSafetyLag)
		_, _, advanced := cacheEffectivenessWindow(&last, now)
		if advanced {
			t.Fatal("windowStart == windowEnd 时必须跳过（空窗口不写行）")
		}
	})

	t.Run("水位晚于终点（时钟回拨）时不推进", func(t *testing.T) {
		last := now.Add(-time.Minute)
		_, _, advanced := cacheEffectivenessWindow(&last, now)
		if advanced {
			t.Fatal("windowStart > windowEnd 时必须跳过，不得写出倒挂窗口")
		}
	})
}

// TestNewCacheEffectivenessRequiresPools 钉住装配缺陷必须显式可见（与同包其它任务同判）。
func TestNewCacheEffectivenessRequiresPools(t *testing.T) {
	if _, err := NewCacheEffectiveness(CacheEffectivenessOptions{}); err == nil {
		t.Fatal("Pools 为 nil 时应报错，而不是构造出一个每轮都 panic 的任务")
	}
}

// TestCacheEffectivenessTaskContract 钉住任务契约：名称、默认间隔（Node 硬编码 5 分钟）与有界超时。
func TestCacheEffectivenessTaskContract(t *testing.T) {
	job, err := NewCacheEffectiveness(CacheEffectivenessOptions{Pools: nil})
	if err == nil {
		t.Fatal("Pools 为 nil 时应报错")
	}
	if job != nil {
		t.Fatal("构造失败时不应返回任务")
	}

	// 用一个不触库的替身验证 Task 契约：Task() 本身不访问数据库。
	fake := &CacheEffectiveness{}
	task := fake.Task(0)
	if task.Name != CacheEffectivenessTaskName {
		t.Fatalf("任务名 = %q，期望 %q", task.Name, CacheEffectivenessTaskName)
	}
	if task.Interval != CacheEffectivenessDefaultEvery {
		t.Fatalf("默认间隔 = %s，期望 %s（Node instrumentation.ts:276 硬编码 5 分钟）",
			task.Interval, CacheEffectivenessDefaultEvery)
	}
	if task.Interval != 5*time.Minute {
		t.Fatalf("默认间隔必须与 Node 的 5 分钟一致，实际 %s", task.Interval)
	}
	if task.Timeout <= 0 {
		t.Fatal("必须有界超时：Node 的 setInterval 回调无界，这里必须给上限")
	}
	if task.Run == nil {
		t.Fatal("Task.Run 不得为空")
	}

	// 显式间隔应被尊重（装配处可按 env 覆盖）。
	if got := fake.Task(90 * time.Second).Interval; got != 90*time.Second {
		t.Fatalf("显式间隔被改写：%s", got)
	}
}
