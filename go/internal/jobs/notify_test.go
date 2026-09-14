package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件覆盖通知调度器的可注入时钟路径：触发时刻、重排、单实例、重试退避。
// 真库部分（advisory 锁的跨会话互斥、设置的读路径）见 notify_integration_test.go。
//
// 为什么全部用假时钟：规则的粒度是分钟、重试退避是分钟级，真等一次用例要跑三分钟，
// 在 CI 上必然变成 flake 源或被跳过。假时钟让「触发」变成一次显式推时刻。

func notifyTestPtr[T any](value T) *T { return &value }

// fakeNotifyClock 是可推进的时钟：Wait 立即返回并把「现在」推进 d，同时记录退避序列。
type fakeNotifyClock struct {
	now   time.Time
	waits []time.Duration
}

func (c *fakeNotifyClock) Now() time.Time { return c.now }

func (c *fakeNotifyClock) Wait(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	return true
}

func (c *fakeNotifyClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// fakeNotifyDeliverer 按脚本返回结果，并记录收到的请求。
type fakeNotifyDeliverer struct {
	results  []NotifyDeliveryResult
	errs     []error
	requests []NotifyDeliveryRequest
	index    int
}

func (d *fakeNotifyDeliverer) Deliver(
	_ context.Context,
	request NotifyDeliveryRequest,
) (NotifyDeliveryResult, error) {
	d.requests = append(d.requests, request)
	if d.index >= len(d.results) {
		return NotifyDeliveryResult{Success: true}, nil
	}
	result, err := d.results[d.index], error(nil)
	if d.index < len(d.errs) {
		err = d.errs[d.index]
	}
	d.index++
	return result, err
}

// fakeNotifyPayloads 返回固定数据或「无数据」。
type fakeNotifyPayloads struct {
	data   json.RawMessage
	ok     bool
	err    error
	called int
}

func (p *fakeNotifyPayloads) Payload(
	context.Context,
	NotifyPayloadRequest,
) (json.RawMessage, bool, error) {
	p.called++
	return p.data, p.ok, p.err
}

// fakeNotifyLeader 模拟独占权：held 为 true 表示别的实例持锁。
type fakeNotifyLeader struct {
	held     bool
	acquires int
}

func (l *fakeNotifyLeader) Acquire(
	context.Context,
) (func(context.Context), bool, error) {
	l.acquires++
	if l.held {
		return nil, false, nil
	}
	return func(context.Context) {}, true, nil
}

// notifyTestScheduler 装配一个只依赖假件的调度器。
func notifyTestScheduler(
	t *testing.T,
	settings store.AdminNotificationSettings,
	options notifyTestOptions,
) *NotifyScheduler {
	t.Helper()
	clock := options.clock
	if clock == nil {
		clock = &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	}
	leader := options.leader
	if leader == nil {
		leader = &fakeNotifyLeader{}
	}
	scheduler := NewNotifyScheduler(NotifySchedulerOptions{
		Clock:     clock,
		Leader:    leader,
		Payloads:  options.payloads,
		Deliverer: options.deliverer,
		SettingsSource: func(context.Context) (store.AdminNotificationSettings, string, error) {
			if options.settingsErr != nil {
				return store.AdminNotificationSettings{}, "", options.settingsErr
			}
			// settingsPtr 优先：测试用它在「排程后、触发前」改设置，验证执行期复检。
			current := settings
			if options.settingsAfter != nil {
				current = *options.settingsAfter
			}
			if options.settingsPtr != nil {
				current = *options.settingsPtr
			}
			return current, options.systemTimezone, nil
		},
		Bindings: func(notificationType string) ([]store.AdminNotificationBinding, error) {
			if options.bindingsErr != nil {
				return nil, options.bindingsErr
			}
			return options.bindings[notificationType], nil
		},
		RetryBase:   options.retryBase,
		MaxAttempts: options.maxAttempts,
	})
	if options.removeAll != nil {
		scheduler.removeAll = options.removeAll
	}
	return scheduler
}

type notifyTestOptions struct {
	clock          *fakeNotifyClock
	leader         *fakeNotifyLeader
	payloads       NotifyPayloadSource
	deliverer      NotifyDeliverer
	bindings       map[string][]store.AdminNotificationBinding
	bindingsErr    error
	settingsErr    error
	settingsAfter  *store.AdminNotificationSettings
	settingsPtr    *store.AdminNotificationSettings
	systemTimezone string
	retryBase      time.Duration
	maxAttempts    int
	removeAll      func(context.Context, []NotifySchedule) (bool, error)
}

func TestPlanNotifySchedulesLegacy(t *testing.T) {
	settings := store.AdminNotificationSettings{
		Enabled:                        true,
		UseLegacyMode:                  true,
		DailyLeaderboardEnabled:        true,
		DailyLeaderboardWebhook:        notifyTestPtr("https://example.test/leaderboard"),
		DailyLeaderboardTime:           notifyTestPtr("09:30"),
		CostAlertEnabled:               true,
		CostAlertWebhook:               notifyTestPtr("https://example.test/cost"),
		CostAlertCheckInterval:         notifyTestPtr(45),
		CacheHitRateAlertEnabled:       true,
		CacheHitRateAlertWebhook:       notifyTestPtr("https://example.test/cache"),
		CacheHitRateAlertCheckInterval: notifyTestPtr(5),
	}
	plan, err := PlanNotifySchedules(NotifyPlanInput{
		Settings:       settings,
		SystemTimezone: "Asia/Shanghai",
	})
	if err != nil {
		t.Fatalf("规划失败: %v", err)
	}
	if len(plan) != 3 {
		t.Fatalf("legacy 模式应排 3 条任务，实际 %d", len(plan))
	}

	// 每日排行：cron 时刻来自 dailyLeaderboardTime。
	if plan[0].Type != NotifyTypeDailyLeaderboard ||
		plan[0].Rule.Daily == nil || plan[0].Rule.Daily.Hour != 9 || plan[0].Rule.Daily.Minute != 30 {
		t.Fatalf("每日排行的规则不对: %+v", plan[0])
	}
	if plan[0].JobID != "daily-leaderboard-scheduled" || plan[0].WebhookURL == "" {
		t.Fatalf("每日排行的 jobId/webhook 不对: %+v", plan[0])
	}

	// 成本预警：45 分钟不整除 60 → Bull 的 every（非对齐）。
	costInterval := plan[1].Rule.Interval
	if costInterval == nil || costInterval.Interval != 45*time.Minute || costInterval.Aligned {
		t.Fatalf("45 分钟应退化为非对齐间隔: %+v", plan[1])
	}

	// 缓存命中率：5 分钟整除 60 且 <=59 → cron（对齐）。
	cacheInterval := plan[2].Rule.Interval
	if cacheInterval == nil || cacheInterval.Interval != 5*time.Minute || !cacheInterval.Aligned {
		t.Fatalf("5 分钟应对齐整分: %+v", plan[2])
	}

	// target 模式的字段在 legacy 下必须为空：否则执行期会去查不存在的目标。
	for _, schedule := range plan {
		if schedule.TargetID != 0 || schedule.BindingID != 0 {
			t.Fatalf("legacy 任务不该带目标: %+v", schedule)
		}
		if !schedule.NeedsPayload {
			t.Fatalf("定时任务应标记为执行期现算数据: %+v", schedule)
		}
	}
}

func TestPlanNotifySchedulesTargets(t *testing.T) {
	settings := store.AdminNotificationSettings{
		Enabled:                        true,
		UseLegacyMode:                  false,
		DailyLeaderboardEnabled:        true,
		DailyLeaderboardTime:           notifyTestPtr("09:00"),
		CostAlertEnabled:               true,
		CostAlertCheckInterval:         notifyTestPtr(60),
		CacheHitRateAlertEnabled:       true,
		CacheHitRateAlertCheckInterval: notifyTestPtr(5),
	}
	bindings := map[string][]store.AdminNotificationBinding{
		"daily_leaderboard": {
			{ID: 11, TargetID: 101, ScheduleCron: notifyTestPtr("30 7 * * *")},
			{ID: 12, TargetID: 102, ScheduleTimezone: notifyTestPtr("UTC")},
		},
		"cost_alert": {
			{ID: 21, TargetID: 201},
		},
		"cache_hit_rate_alert": {
			{ID: 31, TargetID: 301},
			{ID: 32, TargetID: 302},
		},
	}
	plan, err := PlanNotifySchedules(NotifyPlanInput{
		Settings:       settings,
		SystemTimezone: "Asia/Shanghai",
		Bindings: func(notificationType string) ([]store.AdminNotificationBinding, error) {
			return bindings[notificationType], nil
		},
	})
	if err != nil {
		t.Fatalf("规划失败: %v", err)
	}

	// 每日排行：每个绑定一条，绑定 cron 覆盖默认时刻；未给时区用系统时区。
	if len(plan) != 4 {
		t.Fatalf("targets 模式应排 2+1+1 条任务，实际 %d", len(plan))
	}
	if plan[0].JobID != "daily-leaderboard:11" || plan[0].Rule.Daily.Hour != 7 ||
		plan[0].Rule.Daily.Minute != 30 {
		t.Fatalf("绑定 11 的 cron 覆盖未生效: %+v", plan[0])
	}
	if plan[0].Timezone != "Asia/Shanghai" {
		t.Fatalf("绑定 11 应回退系统时区，实际 %q", plan[0].Timezone)
	}
	if plan[1].JobID != "daily-leaderboard:12" || plan[1].Timezone != "UTC" ||
		plan[1].Rule.Daily.Hour != 9 {
		t.Fatalf("绑定 12 应按时区/默认时刻排程: %+v", plan[1])
	}
	if plan[2].JobID != "cost-alert:21" || plan[2].TargetID != 201 {
		t.Fatalf("成本预警绑定排程不对: %+v", plan[2])
	}
	// 缓存命中率：两个绑定只排**一个共享**作业（避免同一份 payload 重复计算）。
	if plan[3].JobID != "cache-hit-rate-alert-targets-scheduled" || plan[3].TargetID != 0 {
		t.Fatalf("缓存命中率应为单个共享作业: %+v", plan[3])
	}
}

func TestPlanNotifySchedulesSkipsWhenDisabledOrUnconfigured(t *testing.T) {
	// 总开关关闭：一条都不排（Node 在这个分支只做清空）。
	plan, err := PlanNotifySchedules(NotifyPlanInput{Settings: store.AdminNotificationSettings{}})
	if err != nil || len(plan) != 0 {
		t.Fatalf("总开关关闭应零任务: plan=%d err=%v", len(plan), err)
	}

	// 子开关打开但缺少 webhook（legacy）：该类型的任务不排。
	plan, err = PlanNotifySchedules(NotifyPlanInput{
		Settings: store.AdminNotificationSettings{
			Enabled:                 true,
			UseLegacyMode:           true,
			DailyLeaderboardEnabled: true,
		},
	})
	if err != nil || len(plan) != 0 {
		t.Fatalf("缺 webhook 的 legacy 任务不该排: plan=%d err=%v", len(plan), err)
	}

	// targets 模式但没有任何绑定：缓存命中率不排（Node 记 skipped），其余两条也不排。
	plan, err = PlanNotifySchedules(NotifyPlanInput{
		Settings: store.AdminNotificationSettings{
			Enabled:                  true,
			CacheHitRateAlertEnabled: true,
		},
	})
	if err != nil || len(plan) != 0 {
		t.Fatalf("无绑定时不该排共享作业: plan=%d err=%v", len(plan), err)
	}
}

func TestPlanNotifySchedulesRejectsInvalidInput(t *testing.T) {
	// 非法时区：规划期报错（而不是静默退化到 UTC）。
	_, err := PlanNotifySchedules(NotifyPlanInput{
		Settings: store.AdminNotificationSettings{
			Enabled:                  true,
			CacheHitRateAlertEnabled: true,
		},
		SystemTimezone: "Not/AZone",
	})
	if err == nil {
		t.Fatal("非法系统时区应报错")
	}

	// 绑定的非法时区同样要报错，并带上绑定 id 便于定位。
	_, err = PlanNotifySchedules(NotifyPlanInput{
		Settings: store.AdminNotificationSettings{
			Enabled:                true,
			CostAlertEnabled:       true,
			CostAlertCheckInterval: notifyTestPtr(60),
		},
		Bindings: func(string) ([]store.AdminNotificationBinding, error) {
			return []store.AdminNotificationBinding{{
				ID:               7,
				TargetID:         70,
				ScheduleTimezone: notifyTestPtr("Mars/Olympus"),
			}}, nil
		},
	})
	if err == nil {
		t.Fatal("绑定的非法时区应报错")
	}
	if want := "binding 7"; !contains(err.Error(), want) {
		t.Fatalf("错误信息应含绑定 id，实际 %q", err.Error())
	}

	// 时刻越界：09:99 应被拒。
	_, err = PlanNotifySchedules(NotifyPlanInput{
		Settings: store.AdminNotificationSettings{
			Enabled:                 true,
			UseLegacyMode:           true,
			DailyLeaderboardEnabled: true,
			DailyLeaderboardWebhook: notifyTestPtr("https://example.test/x"),
			DailyLeaderboardTime:    notifyTestPtr("09:99"),
		},
	})
	if err == nil {
		t.Fatal("越界时刻应报错")
	}
}

func TestNotifyRuleNext(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("载入时区失败: %v", err)
	}

	// 每日：今天未到 → 今天；今天已过 → 明天（按日历日推进，不受夏令时影响）。
	daily := NotifyRule{Daily: &NotifyDailyRule{Hour: 9, Minute: 30, Location: shanghai}}
	at := daily.Next(time.Date(2026, 3, 1, 8, 0, 0, 0, shanghai))
	if at.Hour() != 9 || at.Minute() != 30 || at.Day() != 1 {
		t.Fatalf("当天未到应取当天: %s", at)
	}
	at = daily.Next(time.Date(2026, 3, 1, 9, 30, 0, 0, shanghai))
	if at.Day() != 2 {
		t.Fatalf("正好到点应取次日（不重复触发）: %s", at)
	}

	// 对齐间隔：下一个分钟数是 5 的倍数的整分。
	aligned := NotifyRule{Interval: &NotifyIntervalRule{
		Interval: 5 * time.Minute,
		Aligned:  true,
		Location: time.UTC,
	}}
	at = aligned.Next(time.Date(2026, 3, 1, 8, 3, 20, 0, time.UTC))
	if at.Minute() != 5 || at.Second() != 0 {
		t.Fatalf("5 分钟对齐的下一格应是 08:05:00，实际 %s", at)
	}

	// 非对齐间隔：以入参为锚加一个间隔（Bull 的 every 语义）。
	every := NotifyRule{Interval: &NotifyIntervalRule{
		Interval: 45 * time.Minute,
		Aligned:  false,
		Location: time.UTC,
	}}
	at = every.Next(time.Date(2026, 3, 1, 8, 3, 20, 0, time.UTC))
	if !at.Equal(time.Date(2026, 3, 1, 8, 48, 20, 0, time.UTC)) {
		t.Fatalf("非对齐间隔应按锚点推进，实际 %s", at)
	}
}

func TestNotifySchedulerFiresDueJobsOnceAndAdvances(t *testing.T) {
	clock := &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	deliverer := &fakeNotifyDeliverer{}
	scheduler := notifyTestScheduler(t, store.AdminNotificationSettings{
		Enabled:                true,
		UseLegacyMode:          true,
		CostAlertEnabled:       true,
		CostAlertWebhook:       notifyTestPtr("https://example.test/cost"),
		CostAlertCheckInterval: notifyTestPtr(60),
	}, notifyTestOptions{
		clock:     clock,
		deliverer: deliverer,
		payloads:  &fakeNotifyPayloads{ok: true, data: json.RawMessage(`{"alerts":1}`)},
		retryBase: time.Second,
	})

	if err := scheduler.Reschedule(context.Background()); err != nil {
		t.Fatalf("重排失败: %v", err)
	}
	if scheduler.Len() != 1 {
		t.Fatalf("应排 1 条任务，实际 %d", scheduler.Len())
	}

	// 未到点：不触发。
	results, err := scheduler.Tick(context.Background(), clock.Now())
	if err != nil || len(results) != 0 {
		t.Fatalf("未到点不该触发: results=%d err=%v", len(results), err)
	}
	if len(deliverer.requests) != 0 {
		t.Fatalf("未到点不该投递")
	}

	// 到点：触发一次，且下一次触发被推进到下一个整点。
	clock.advance(60 * time.Minute)
	results, err = scheduler.Tick(context.Background(), clock.Now())
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if len(results) != 1 || !results[0].Fired || results[0].Skipped {
		t.Fatalf("应触发一条: %+v", results)
	}
	if len(deliverer.requests) != 1 {
		t.Fatalf("应投递一次，实际 %d", len(deliverer.requests))
	}
	requests := deliverer.requests[0]
	if requests.WebhookURL != "https://example.test/cost" || requests.Type != NotifyTypeCostAlert {
		t.Fatalf("投递请求不对: %+v", requests)
	}
	if requests.Timezone != "UTC" {
		t.Fatalf("legacy 任务的时区应为系统时区，实际 %q", requests.Timezone)
	}

	// 同一时刻再扫一次：不该重复触发。
	results, err = scheduler.Tick(context.Background(), clock.Now())
	if err != nil || len(results) != 0 {
		t.Fatalf("同一时刻不该重复触发: results=%d err=%v", len(results), err)
	}
	if len(deliverer.requests) != 1 {
		t.Fatalf("重复触发导致多投递: %d", len(deliverer.requests))
	}
	scheduled := scheduler.Scheduled()
	if !scheduled[0].NextRun.After(clock.Now()) {
		t.Fatalf("下一次触发应被推进到未来: %s", scheduled[0].NextRun)
	}
}

func TestNotifySchedulerRechecksSwitchesAtExecution(t *testing.T) {
	clock := &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	deliverer := &fakeNotifyDeliverer{}
	payloads := &fakeNotifyPayloads{ok: true, data: json.RawMessage(`{}`)}
	current := store.AdminNotificationSettings{
		Enabled:                true,
		UseLegacyMode:          true,
		CostAlertEnabled:       true,
		CostAlertWebhook:       notifyTestPtr("https://example.test/cost"),
		CostAlertCheckInterval: notifyTestPtr(60),
	}
	scheduler := notifyTestScheduler(t, current, notifyTestOptions{
		clock:       clock,
		deliverer:   deliverer,
		payloads:    payloads,
		settingsPtr: &current,
	})

	if err := scheduler.Reschedule(context.Background()); err != nil {
		t.Fatalf("重排失败: %v", err)
	}
	if scheduler.Len() != 1 {
		t.Fatalf("启用状态下应排 1 条，实际 %d", scheduler.Len())
	}

	// 排程之后、触发之前把子开关关掉：遗留任务不该继续发送（Node 的执行期复检）。
	current.CostAlertEnabled = false
	clock.advance(60 * time.Minute)
	results, err := scheduler.Tick(context.Background(), clock.Now())
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if len(results) != 1 || !results[0].Skipped || results[0].Reason != "disabled" {
		t.Fatalf("执行期开关关闭应跳过: %+v", results)
	}
	if len(deliverer.requests) != 0 || payloads.called != 0 {
		t.Fatalf("跳过的任务不该取数据/投递: deliver=%d payload=%d",
			len(deliverer.requests), payloads.called)
	}
}

func TestNotifySchedulerSkipsWhenNoData(t *testing.T) {
	clock := &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	deliverer := &fakeNotifyDeliverer{}
	scheduler := notifyTestScheduler(t, store.AdminNotificationSettings{
		Enabled:                 true,
		UseLegacyMode:           true,
		DailyLeaderboardEnabled: true,
		DailyLeaderboardWebhook: notifyTestPtr("https://example.test/leaderboard"),
		DailyLeaderboardTime:    notifyTestPtr("09:00"),
	}, notifyTestOptions{
		clock:     clock,
		deliverer: deliverer,
		// 生成器给出「本刻无数据」：不发、不重试。
		payloads: &fakeNotifyPayloads{ok: false},
	})

	if err := scheduler.Reschedule(context.Background()); err != nil {
		t.Fatalf("重排失败: %v", err)
	}
	clock.advance(2 * time.Hour)
	results, err := scheduler.Tick(context.Background(), clock.Now())
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if len(results) != 1 || !results[0].Skipped || results[0].Reason != "no_data" {
		t.Fatalf("无数据应跳过: %+v", results)
	}
	if len(deliverer.requests) != 0 {
		t.Fatalf("无数据不该投递")
	}
}

func TestNotifySchedulerRetriesWithExponentialBackoff(t *testing.T) {
	clock := &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	// 前两次失败、第三次成功（对齐 Bull 的 attempts=3）。
	deliverer := &fakeNotifyDeliverer{
		results: []NotifyDeliveryResult{
			{Success: false, Error: "boom"},
			{Success: false, Error: "boom"},
			{Success: true},
		},
	}
	scheduler := notifyTestScheduler(t, store.AdminNotificationSettings{
		Enabled:                true,
		UseLegacyMode:          true,
		CostAlertEnabled:       true,
		CostAlertWebhook:       notifyTestPtr("https://example.test/cost"),
		CostAlertCheckInterval: notifyTestPtr(60),
	}, notifyTestOptions{
		clock:       clock,
		deliverer:   deliverer,
		payloads:    &fakeNotifyPayloads{ok: true, data: json.RawMessage(`{}`)},
		retryBase:   60 * time.Second,
		maxAttempts: 3,
	})

	if err := scheduler.Reschedule(context.Background()); err != nil {
		t.Fatalf("重排失败: %v", err)
	}
	clock.advance(60 * time.Minute)
	results, err := scheduler.Tick(context.Background(), clock.Now())
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if len(results) != 1 || results[0].Attempts != 3 || results[0].Error != "" {
		t.Fatalf("第三次应成功: %+v", results)
	}
	if len(deliverer.requests) != 3 {
		t.Fatalf("应投递 3 次，实际 %d", len(deliverer.requests))
	}
	wantWaits := []time.Duration{60 * time.Second, 120 * time.Second}
	if len(clock.waits) != len(wantWaits) {
		t.Fatalf("退避次数不对: %v", clock.waits)
	}
	for i, want := range wantWaits {
		if clock.waits[i] != want {
			t.Fatalf("第 %d 次退避应为 %s，实际 %s", i+1, want, clock.waits[i])
		}
	}
}

func TestNotifySchedulerGivesUpAfterMaxAttempts(t *testing.T) {
	clock := &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	deliverer := &fakeNotifyDeliverer{results: []NotifyDeliveryResult{
		{Success: false, Error: "boom"},
		{Success: false, Error: "boom"},
		{Success: false, Error: "boom"},
	}}
	scheduler := notifyTestScheduler(t, store.AdminNotificationSettings{
		Enabled:                true,
		UseLegacyMode:          true,
		CostAlertEnabled:       true,
		CostAlertWebhook:       notifyTestPtr("https://example.test/cost"),
		CostAlertCheckInterval: notifyTestPtr(60),
	}, notifyTestOptions{
		clock:       clock,
		deliverer:   deliverer,
		payloads:    &fakeNotifyPayloads{ok: true, data: json.RawMessage(`{}`)},
		retryBase:   time.Second,
		maxAttempts: 3,
	})

	if err := scheduler.Reschedule(context.Background()); err != nil {
		t.Fatalf("重排失败: %v", err)
	}
	clock.advance(60 * time.Minute)
	results, _ := scheduler.Tick(context.Background(), clock.Now())
	if len(results) != 1 || results[0].Attempts != 3 || results[0].Error != "boom" {
		t.Fatalf("三次都失败应如实上报: %+v", results)
	}
	if len(deliverer.requests) != 3 {
		t.Fatalf("应恰好投递 3 次，实际 %d", len(deliverer.requests))
	}
}

func TestNotifySchedulerAbortsRescheduleWhenRemovalFails(t *testing.T) {
	clock := &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	deliverer := &fakeNotifyDeliverer{}
	scheduler := notifyTestScheduler(t, store.AdminNotificationSettings{
		Enabled:                true,
		UseLegacyMode:          true,
		CostAlertEnabled:       true,
		CostAlertWebhook:       notifyTestPtr("https://example.test/cost"),
		CostAlertCheckInterval: notifyTestPtr(60),
	}, notifyTestOptions{
		clock:     clock,
		deliverer: deliverer,
		payloads:  &fakeNotifyPayloads{ok: true, data: json.RawMessage(`{}`)},
		// 旧任务清不干净：必须中止，不新增（否则新旧同时触发=重复发送）。
		removeAll: func(context.Context, []NotifySchedule) (bool, error) { return false, nil },
	})

	if err := scheduler.Reschedule(context.Background()); err == nil {
		t.Fatal("清不干净时应报错中止")
	}
	if scheduler.Len() != 0 {
		t.Fatalf("中止时不该新增任务，实际 %d", scheduler.Len())
	}
}

func TestNotifySchedulerKeepsPreviousScheduleOnPlanError(t *testing.T) {
	clock := &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	scheduler := notifyTestScheduler(t, store.AdminNotificationSettings{
		Enabled:                true,
		UseLegacyMode:          true,
		CostAlertEnabled:       true,
		CostAlertWebhook:       notifyTestPtr("https://example.test/cost"),
		CostAlertCheckInterval: notifyTestPtr(60),
	}, notifyTestOptions{
		clock:     clock,
		deliverer: &fakeNotifyDeliverer{},
		payloads:  &fakeNotifyPayloads{ok: true, data: json.RawMessage(`{}`)},
	})

	if err := scheduler.Reschedule(context.Background()); err != nil {
		t.Fatalf("首次重排应成功: %v", err)
	}
	before := scheduler.Scheduled()
	if len(before) != 1 {
		t.Fatalf("首次应排 1 条，实际 %d", len(before))
	}

	// 第二轮：规划期错误（读绑定失败）→ 沿用上一套调度，不清空也不替换。
	scheduler.bindingsFor = func(string) ([]store.AdminNotificationBinding, error) {
		return nil, errors.New("db down")
	}
	targets := store.AdminNotificationSettings{
		Enabled:                true,
		CostAlertEnabled:       true,
		CostAlertCheckInterval: notifyTestPtr(60),
	}
	scheduler.settingsFor = func(context.Context) (store.AdminNotificationSettings, string, error) {
		return targets, "UTC", nil
	}
	if err := scheduler.Reschedule(context.Background()); err == nil {
		t.Fatal("规划失败应报错")
	}
	after := scheduler.Scheduled()
	if len(after) != len(before) || after[0].Schedule.JobID != before[0].Schedule.JobID {
		t.Fatalf("上一套调度应保持不变: before=%+v after=%+v", before, after)
	}
	if !after[0].NextRun.Equal(before[0].NextRun) {
		t.Fatalf("未重排时下一次触发也不该变: before=%s after=%s",
			before[0].NextRun, after[0].NextRun)
	}
}

func TestNotifySchedulerSkipsTickWithoutLeadership(t *testing.T) {
	clock := &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	leader := &fakeNotifyLeader{held: true}
	deliverer := &fakeNotifyDeliverer{}
	scheduler := notifyTestScheduler(t, store.AdminNotificationSettings{
		Enabled:                true,
		UseLegacyMode:          true,
		CostAlertEnabled:       true,
		CostAlertWebhook:       notifyTestPtr("https://example.test/cost"),
		CostAlertCheckInterval: notifyTestPtr(60),
	}, notifyTestOptions{
		clock:     clock,
		leader:    leader,
		deliverer: deliverer,
		payloads:  &fakeNotifyPayloads{ok: true, data: json.RawMessage(`{}`)},
	})

	if err := scheduler.Reschedule(context.Background()); err != nil {
		t.Fatalf("重排失败: %v", err)
	}
	clock.advance(60 * time.Minute)
	results, err := scheduler.Tick(context.Background(), clock.Now())
	if err != nil {
		t.Fatalf("无独占权不该报错: %v", err)
	}
	if len(results) != 0 || len(deliverer.requests) != 0 {
		t.Fatalf("别的实例持锁时不该发送: results=%d deliver=%d",
			len(results), len(deliverer.requests))
	}
	if leader.acquires != 1 {
		t.Fatalf("应申请一次独占权，实际 %d", leader.acquires)
	}
}

func TestNotifySchedulerDisabledMasterClearsSchedule(t *testing.T) {
	clock := &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	scheduler := notifyTestScheduler(t, store.AdminNotificationSettings{
		Enabled:                true,
		UseLegacyMode:          true,
		CostAlertEnabled:       true,
		CostAlertWebhook:       notifyTestPtr("https://example.test/cost"),
		CostAlertCheckInterval: notifyTestPtr(60),
	}, notifyTestOptions{
		clock:    clock,
		payloads: &fakeNotifyPayloads{ok: true, data: json.RawMessage(`{}`)},
	})
	if err := scheduler.Reschedule(context.Background()); err != nil {
		t.Fatalf("重排失败: %v", err)
	}
	if scheduler.Len() != 1 {
		t.Fatalf("应先排出 1 条，实际 %d", scheduler.Len())
	}

	// 总开关关闭 → 清空（Node 在这个分支只清不增）。
	off := store.AdminNotificationSettings{}
	scheduler.settingsFor = func(context.Context) (store.AdminNotificationSettings, string, error) {
		return off, "UTC", nil
	}
	if err := scheduler.Reschedule(context.Background()); err != nil {
		t.Fatalf("关闭总开关的重排不该报错: %v", err)
	}
	if scheduler.Len() != 0 {
		t.Fatalf("总开关关闭后应零任务，实际 %d", scheduler.Len())
	}
}

func TestNotifySchedulerUnwiredPayloadSourceFailsLoudly(t *testing.T) {
	clock := &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	deliverer := &fakeNotifyDeliverer{}
	// 未装配数据生成器：如实报错，不静默发送空正文。
	scheduler := notifyTestScheduler(t, store.AdminNotificationSettings{
		Enabled:                true,
		UseLegacyMode:          true,
		CostAlertEnabled:       true,
		CostAlertWebhook:       notifyTestPtr("https://example.test/cost"),
		CostAlertCheckInterval: notifyTestPtr(60),
	}, notifyTestOptions{clock: clock, deliverer: deliverer})

	if err := scheduler.Reschedule(context.Background()); err != nil {
		t.Fatalf("重排失败: %v", err)
	}
	clock.advance(60 * time.Minute)
	results, _ := scheduler.Tick(context.Background(), clock.Now())
	if len(results) != 1 || results[0].Error == "" {
		t.Fatalf("未装配生成器应报错: %+v", results)
	}
	if len(deliverer.requests) != 0 {
		t.Fatalf("未取到数据不该投递")
	}
}

// contains 是本文件的小工具：避免为一个字符串断言引入 strings 之外的依赖。
func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
