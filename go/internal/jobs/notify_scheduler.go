package jobs

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是通知调度器的**执行面**：重排任务（对应 Node 的 scheduleNotifications）、
// 到期触发、执行期复检开关、投递与重试退避。
//
// 唯一真源：src/lib/notification/notification-queue.ts，逐段对应如下（行号以该文件当前内容为准）：
//   - 重排流程（先清空、清不干净就中止、按模式新增）：771-980；
//   - 总开关关闭只清空不新增：776-784；
//   - 清空失败中止（避免新旧任务同时触发造成重复发送）：771-795 与 removeAllRepeatableJobs:712；
//   - 执行期复检开关：401-407（circuit-breaker）、414-420（daily-leaderboard）、441-447（cost-alert）、
//     469-475（cache-hit-rate-alert）；无数据即跳过：425-430、452-460；
//   - 目标缺失或被停用即跳过：557-566；
//   - 失败重试 3 次、指数退避首次 60 秒：189-193（Bull 的 defaultJobOptions）。
//
// Go 侧的两处结构性差异（登记，且都有对应测试）：
//  1. **单实例执行靠 advisory 锁**而不是 Bull 的 jobId 去重：Node 的重复作业存在 Redis 里，
//     多实例各自 scheduleNotifications 会因 jobId 相同而互相覆盖；Go 侧没有共享队列，
//     故每轮触发前申请 `pg_try_advisory_lock`，拿不到就跳过本轮（多实例只有一个真正发送）。
//  2. **没有队列**，故「进队列」与「触发」在一个进程里完成：到期即跑，不落 Redis 队列。
//     代价是进程崩溃会丢掉这一轮（下一轮按规则重算），收益是没有队列积压与重复投递。
//
// 已登记未实现（见报告「未实现清单」）：三个数据生成器（每日排行 / 成本预警 / 缓存命中率
// 异常的现算）、cache-hit-rate-alert 的 fan-out 子作业与 cooldown 去重、circuit-breaker 的
// 事件入队点（Node 由熔断器触发 addNotificationJob）。

// notifyLockName 是本调度器的 advisory 锁名。
//
// 与 Node 的差别：Node 没有这把锁（靠 jobId 去重）。名字带 cch 前缀以免与 Node 的锁名相撞
// （Node 侧不持有同名锁，故不存在「Go 抢了 Node 的锁」这种情况）。
const notifyLockName = "claude-code-hub:notifications-scheduler"

// notifyDefaultMaxAttempts 对齐 Bull 的 attempts: 3。
const notifyDefaultMaxAttempts = 3

// notifyDefaultRetryBase 对齐 Bull 的 backoff.delay = 60000ms（exponential）。
const notifyDefaultRetryBase = 60 * time.Second

// notifyDefaultTickInterval 是触发扫描间隔。
//
// 最小规则粒度是 1 分钟（clamp 下限），故 30 秒扫描足以让触发时刻落在分钟内；
// 更密的扫描只是白开一次 advisory 锁连接。
const notifyDefaultTickInterval = 30 * time.Second

// NotifyClock 抽象「现在」与「等待」，让触发时刻与重试退避都能在测试里确定性推进。
type NotifyClock interface {
	// Now 返回当前时刻。
	Now() time.Time
	// Wait 等待 d 或 ctx 结束；返回是否等满（ctx 结束返回 false）。
	Wait(ctx context.Context, d time.Duration) bool
}

// realNotifyClock 是生产时钟。
type realNotifyClock struct{}

func (realNotifyClock) Now() time.Time { return time.Now() }

func (realNotifyClock) Wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// NotifyLeaderAcquirer 申请「本轮由我触发」的独占权。
type NotifyLeaderAcquirer interface {
	// Acquire 尝试取得独占权；acquired 为 false 表示别的实例持有（跳过本轮）。
	// release 在 acquired 为 true 时必非 nil。
	Acquire(ctx context.Context) (release func(context.Context), acquired bool, err error)
}

// notifyScheduledJob 是运行态的任务：待触发任务 + 下一次触发时刻。
type notifyScheduledJob struct {
	Schedule NotifySchedule
	NextRun  time.Time
	Fires    int64
}

// NotifyScheduledJob 是给测试与日志看的只读视图。
type NotifyScheduledJob struct {
	Schedule NotifySchedule
	NextRun  time.Time
	Fires    int64
}

// NotifyRunResult 是一轮触发的单条结果。
type NotifyRunResult struct {
	JobID   string
	Type    string
	Fired   bool
	Skipped bool
	Reason  string
	// Payloads 是本轮要投递的数据份数（成本预警可能一次多条）。
	Payloads int
	// Delivered 是成功送达的份数。
	Delivered int
	// Attempts 是全部份数累计的投递尝试次数。
	Attempts int
	Error    string
}

// NotifySchedulerOptions 是构造参数。
type NotifySchedulerOptions struct {
	// Pools 是连接池；SettingsSource / Bindings 都注入时才可为 nil（测试）。
	Pools *store.Pools
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Deliverer 投递一条通知；nil 时任务照排但执行期记 error（装配缺失要说出来）。
	Deliverer NotifyDeliverer
	// Payloads 现算通知数据；nil 时 NeedsPayload 的任务记 error。
	Payloads NotifyPayloadSource
	// Cooldown 写下「投递成功后」的去重键（Node：commitCacheHitRateAlertCooldown）；
	// nil 时不去重，与 Node 在无 Redis 时的分支一致。
	Cooldown NotifyCooldownWriter
	// Clock 为 nil 时用真实时钟。
	Clock NotifyClock
	// LockName 覆盖默认锁名（测试用）。
	LockName string
	// MaxAttempts 是单条任务的总投递次数（对齐 Bull attempts，默认 3）。
	MaxAttempts int
	// RetryBase 是指数退避基数（默认 60s）。
	RetryBase time.Duration
	// SettingsSource 覆盖「读设置 + 系统时区」；缺省从 Pools 读。
	SettingsSource func(ctx context.Context) (store.AdminNotificationSettings, string, error)
	// Bindings 覆盖绑定读取（targets 模式用）；缺省从 Pools 读。
	Bindings func(notificationType string) ([]store.AdminNotificationBinding, error)
	// Leader 覆盖独占权申请；缺省用 advisory 锁。
	Leader NotifyLeaderAcquirer
}

// NotifyScheduler 是通知调度器。
type NotifyScheduler struct {
	pools       *store.Pools
	logger      *logx.Logger
	clock       NotifyClock
	deliverer   NotifyDeliverer
	payloads    NotifyPayloadSource
	cooldown    NotifyCooldownWriter
	maxAttempts int
	retryBase   time.Duration
	settingsFor func(ctx context.Context) (store.AdminNotificationSettings, string, error)
	bindingsFor func(notificationType string) ([]store.AdminNotificationBinding, error)
	leader      NotifyLeaderAcquirer
	// removeAll 可注入：默认清空即成功；测试用它走「旧任务未清干净 → 中止重排」分支。
	removeAll func(ctx context.Context, previous []NotifySchedule) (bool, error)

	mu        sync.Mutex
	schedules []*notifyScheduledJob
}

// NewNotifyScheduler 建一个通知调度器。
func NewNotifyScheduler(options NotifySchedulerOptions) *NotifyScheduler {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	clock := options.Clock
	if clock == nil {
		clock = realNotifyClock{}
	}
	lockName := options.LockName
	if lockName == "" {
		lockName = notifyLockName
	}
	maxAttempts := options.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = notifyDefaultMaxAttempts
	}
	retryBase := options.RetryBase
	if retryBase <= 0 {
		retryBase = notifyDefaultRetryBase
	}

	scheduler := &NotifyScheduler{
		pools:       options.Pools,
		logger:      logger,
		clock:       clock,
		deliverer:   options.Deliverer,
		payloads:    options.Payloads,
		cooldown:    options.Cooldown,
		maxAttempts: maxAttempts,
		retryBase:   retryBase,
		settingsFor: options.SettingsSource,
		bindingsFor: options.Bindings,
		leader:      options.Leader,
		removeAll:   func(context.Context, []NotifySchedule) (bool, error) { return true, nil },
	}
	if scheduler.settingsFor == nil {
		scheduler.settingsFor = scheduler.settingsFromPools
	}
	if scheduler.bindingsFor == nil {
		scheduler.bindingsFor = scheduler.bindingsFromPools
	}
	if scheduler.leader == nil {
		scheduler.leader = &advisoryNotifyLeader{pools: options.Pools, lockName: lockName}
	}
	return scheduler
}

// Task 把调度器交给既有的 jobs.Scheduler 驱动：每个 Interval 扫一次「到期任务」。
//
// 复用既有 Scheduler 的理由：单例（上一轮没跑完则跳过本轮）、超时、错峰、停止等待
// 都在那里实现过一次；通知调度不需要第二套循环。
func (s *NotifyScheduler) Task(interval time.Duration) Task {
	if interval <= 0 {
		interval = notifyDefaultTickInterval
	}
	return Task{
		Name:     "notifications-scheduler",
		Interval: interval,
		// 超时必须覆盖「重试两次」的最坏情形（60s + 120s）+ 投递本身，故取 10 分钟
		// （与 defaultTaskTimeout 相同量级；一次投递的 HTTP 超时远小于此）。
		Timeout: 10 * time.Minute,
		Run: func(ctx context.Context) error {
			_, err := s.Tick(ctx, s.clock.Now())
			return err
		},
	}
}

// Reschedule 复刻 scheduleNotifications：读设置 → 清旧 → 按模式排新。
//
// 返回错误的三种情形（都不影响调用方响应，Node 侧是 catch-并-日志的 fail-open）：
//   - 读设置或读绑定失败；
//   - 规划失败（例如绑定的时区名非法）：此时**保留上一套调度**继续跑，不做半途替换；
//   - 旧任务未能清干净：中止本次重排，不新增任何任务（避免新旧同时触发造成重复发送）。
func (s *NotifyScheduler) Reschedule(ctx context.Context) error {
	settings, systemTimezone, err := s.settingsFor(ctx)
	if err != nil {
		s.logger.Error("notify_schedule_failed", map[string]any{
			"reason": "settings_unreadable",
			"error":  err.Error(),
		})
		return err
	}

	if !settings.Enabled {
		s.logger.Info("notifications_disabled", nil)
		// 总开关关闭：清空已存在的定时任务（此处无需新增，清不干净不阻断）。
		previous := s.currentSchedules()
		if ok, err := s.removeAll(ctx, previous); !ok || err != nil {
			s.logger.Warn("notify_repeatable_remove_failed", map[string]any{
				"reason": "disabled_path_not_blocking",
				"error":  notifyErrorText(err),
			})
		}
		s.setSchedules(nil)
		return nil
	}

	plan, err := PlanNotifySchedules(NotifyPlanInput{
		Settings:       settings,
		Bindings:       s.bindingsFor,
		SystemTimezone: systemTimezone,
	})
	if err != nil {
		// 规划失败不改动现有调度：Node 在此会因半途抛错留下「一半新一半旧」，
		// Go 侧宁可沿用上一套可用调度，等管理员修正配置后下一轮重排。
		s.logger.Error("notify_schedule_failed", map[string]any{
			"reason": "plan_invalid",
			"error":  err.Error(),
		})
		return err
	}

	previous := s.currentSchedules()
	removedAll, err := s.removeAll(ctx, previous)
	if err != nil || !removedAll {
		// 见文件头：清不干净就中止。否则旧任务与新任务会同时触发（重复发送）。
		s.logger.Error("schedule_notifications_aborted", map[string]any{
			"reason": "stale_repeatable_remove_failed",
			"error":  notifyErrorText(err),
		})
		return errors.New("旧定时任务未能全部移除，已中止重排")
	}

	now := s.clock.Now()
	jobs := make([]*notifyScheduledJob, 0, len(plan))
	for _, schedule := range plan {
		jobs = append(jobs, &notifyScheduledJob{
			Schedule: schedule,
			NextRun:  notifyInitialRun(schedule.Rule, now),
		})
		s.logNotifyScheduled(schedule)
	}
	s.setSchedules(jobs)
	s.logger.Info("notifications_scheduled", map[string]any{"jobs": len(jobs)})
	return nil
}

// Tick 触发所有到期任务；返回逐条结果（测试与日志共用）。
//
// now 由调用方给出：真实路径用时钟，测试直接推时刻，不需要 sleep。
func (s *NotifyScheduler) Tick(ctx context.Context, now time.Time) ([]NotifyRunResult, error) {
	due := s.takeDue(now)
	if len(due) == 0 {
		return nil, nil
	}

	// 独占权：多实例只有一个真正发送（Node 的等价物是 Bull 的 jobId 去重）。
	release, acquired, err := s.leader.Acquire(ctx)
	if err != nil {
		s.logger.Error("notify_tick_leader_failed", map[string]any{"error": err.Error()})
		return nil, err
	}
	if !acquired {
		s.logger.Info("notify_tick_skipped", map[string]any{
			"reason": "leader_held_by_other_instance",
			"jobs":   len(due),
		})
		return nil, nil
	}
	if release != nil {
		defer release(ctx)
	}

	results := make([]NotifyRunResult, 0, len(due))
	for _, job := range due {
		results = append(results, s.runJob(ctx, job, now))
	}
	sort.Slice(results, func(i, j int) bool { return results[i].JobID < results[j].JobID })
	return results, nil
}

// takeDue 取出到期任务并推进它们的下一次触发时刻。
//
// 推进语义：错过的轮次不补跑（Node 的 Bull 也只会在到期时各跑一次），
// 下一次触发一律以「当前时刻」为锚重算，避免进程停机一小时后台账式连发。
func (s *NotifyScheduler) takeDue(now time.Time) []*notifyScheduledJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	due := make([]*notifyScheduledJob, 0, len(s.schedules))
	for _, job := range s.schedules {
		if job.NextRun.IsZero() || job.NextRun.After(now) {
			continue
		}
		job.Fires++
		job.NextRun = notifyInitialRun(job.Schedule.Rule, now)
		due = append(due, job)
	}
	return due
}

// runJob 执行一条任务：复检开关 → 取数据 → 投递（带重试退避）。
func (s *NotifyScheduler) runJob(ctx context.Context, job *notifyScheduledJob, now time.Time) NotifyRunResult {
	schedule := job.Schedule
	result := NotifyRunResult{JobID: schedule.JobID, Type: schedule.Type, Fired: true}

	s.logger.Info("notification_job_start", map[string]any{
		"jobId": schedule.JobID,
		"type":  schedule.Type,
	})

	// 1) 执行期复检开关：入队后开关被关掉的遗留任务不应继续发送。
	settings, systemTimezone, err := s.settingsFor(ctx)
	if err != nil {
		return s.failJob(result, "settings_unreadable", err, 0)
	}
	if !notifySwitchEnabled(settings, schedule.Type) {
		result.Skipped = true
		result.Reason = "disabled"
		s.logger.Info("notification_job_skipped", map[string]any{
			"jobId":  schedule.JobID,
			"type":   schedule.Type,
			"reason": "disabled",
		})
		return result
	}

	// 2) 数据：事件型任务带上数据；定时型任务现算，算不出就跳过（不是失败，不重试）。
	//
	// 生成器可以一次给出多份（成本预警有多个超限对象时就是多条告警）：多份各自独立投递、
	// 各自独立重试，一份失败不牵连其余份。
	payloads := []NotifyPayload{{Data: schedule.Data}}
	if schedule.NeedsPayload && len(schedule.Data) == 0 {
		if s.payloads == nil {
			return s.failJob(result, "payload_source_unwired", errors.New("未装配数据生成器"), 0)
		}
		computed, err := s.payloads.Payloads(ctx, NotifyPayloadRequest{
			Type:           schedule.Type,
			Timezone:       schedule.Timezone,
			SystemTimezone: systemTimezone,
			Now:            now,
			Settings:       settings,
			TargetID:       schedule.TargetID,
			BindingID:      schedule.BindingID,
		})
		if err != nil {
			return s.failJob(result, "payload_failed", err, 0)
		}
		if len(computed) == 0 {
			result.Skipped = true
			result.Reason = "no_data"
			s.logger.Info("notification_job_skipped", map[string]any{
				"jobId":  schedule.JobID,
				"type":   schedule.Type,
				"reason": "no_data",
			})
			return result
		}
		payloads = computed
	}

	// 3) 逐份投递 + 重试退避（Node 的 Bull attempts/backoff）。
	if s.deliverer == nil {
		return s.failJob(result, "deliverer_unwired", errors.New("未装配投递器"), 0)
	}
	result.Payloads = len(payloads)
	failures := 0
	for index, payload := range payloads {
		outcome, lastError := s.deliverPayload(ctx, schedule, payload, index, &result)
		switch outcome {
		case notifyDeliveryDelivered:
			result.Delivered++
			// Node 只在**发送成功后**写冷却键（notification-queue.ts:592-597）。
			s.commitCooldown(ctx, payload)
		case notifyTargetMissing:
			// 目标缺失或被停用：Node 同样不重试，且同一目标下其余份也不会成功。
			result.Skipped = true
			result.Reason = "target_missing_or_disabled"
			return result
		case notifyDeliveryFailed:
			failures++
			result.Error = lastError
		}
	}
	if failures == 0 {
		return result
	}
	if failures < len(payloads) {
		s.logger.Warn("notification_job_partial_failure", map[string]any{
			"jobId":     schedule.JobID,
			"type":      schedule.Type,
			"payloads":  len(payloads),
			"delivered": result.Delivered,
			"failed":    failures,
			"error":     result.Error,
		})
		return result
	}
	lastError := result.Error
	result.Error = lastError
	s.logger.Error("notification_job_error", map[string]any{
		"jobId":     schedule.JobID,
		"type":      schedule.Type,
		"payloads":  len(payloads),
		"delivered": result.Delivered,
		"attempts":  result.Attempts,
		"error":     lastError,
	})
	return result
}

// notifyDeliveryOutcome 是一次投递的结局。
type notifyDeliveryOutcome int

const (
	// notifyDeliveryDelivered 投递成功（该份 payload 的冷却键可以落下了）。
	notifyDeliveryDelivered notifyDeliveryOutcome = iota
	// notifyTargetMissing 目标缺失/停用：不重试。
	notifyTargetMissing
	// notifyDeliveryFailed 重试耗尽仍失败。
	notifyDeliveryFailed
)

// deliverPayload 投递一份 payload（含重试退避），把尝试次数累加进 result。
func (s *NotifyScheduler) deliverPayload(
	ctx context.Context,
	schedule NotifySchedule,
	payload NotifyPayload,
	index int,
	result *NotifyRunResult,
) (notifyDeliveryOutcome, string) {
	var lastError string
	for attempt := 1; attempt <= s.maxAttempts; attempt++ {
		result.Attempts++
		delivery, err := s.deliverer.Deliver(ctx, NotifyDeliveryRequest{
			Type:       schedule.Type,
			WebhookURL: schedule.WebhookURL,
			TargetID:   schedule.TargetID,
			BindingID:  schedule.BindingID,
			Data:       payload.Data,
			Timezone:   schedule.Timezone,
		})
		if err == nil && delivery.Success {
			s.logger.Info("notification_job_complete", map[string]any{
				"jobId":     schedule.JobID,
				"type":      schedule.Type,
				"index":     index,
				"attempts":  attempt,
				"latencyMs": delivery.LatencyMS,
			})
			return notifyDeliveryDelivered, ""
		}
		if err == nil && delivery.Skipped {
			s.logger.Warn("notification_target_missing_or_disabled", map[string]any{
				"jobId":    schedule.JobID,
				"type":     schedule.Type,
				"targetId": schedule.TargetID,
			})
			return notifyTargetMissing, ""
		}
		if err != nil {
			lastError = err.Error()
		} else {
			lastError = delivery.Error
		}
		if attempt < s.maxAttempts {
			delay := s.retryBase * time.Duration(int64(1)<<(attempt-1))
			s.logger.Warn("notification_job_retry", map[string]any{
				"jobId":      schedule.JobID,
				"type":       schedule.Type,
				"index":      index,
				"attempt":    attempt,
				"retryDelay": delay.Milliseconds(),
				"error":      lastError,
			})
			if !s.clock.Wait(ctx, delay) {
				break
			}
		}
	}
	s.logger.Error("notification_job_error", map[string]any{
		"jobId":    schedule.JobID,
		"type":     schedule.Type,
		"index":    index,
		"attempts": s.maxAttempts,
		"error":    lastError,
	})
	return notifyDeliveryFailed, lastError
}

// commitCooldown 复刻 commitCacheHitRateAlertCooldown：投递成功后把去重键占坑。
//
// 写失败只记 warn，不让任务失败：键没落下最多让下一轮重发一次（漏发比重发坏）。
func (s *NotifyScheduler) commitCooldown(ctx context.Context, payload NotifyPayload) {
	if s.cooldown == nil || len(payload.CooldownKeys) == 0 || payload.CooldownTTL <= 0 {
		return
	}
	if err := s.cooldown.Set(ctx, payload.CooldownKeys, payload.CooldownTTL); err != nil {
		s.logger.Warn("notification_cooldown_commit_failed", map[string]any{
			"keysCount": len(payload.CooldownKeys),
			"cooldown":  payload.CooldownTTL.String(),
			"error":     err.Error(),
		})
	}
}

// failJob 记一条不可重试的失败（装配缺失 / 读设置失败）。
func (s *NotifyScheduler) failJob(
	result NotifyRunResult,
	reason string,
	err error,
	attempts int,
) NotifyRunResult {
	result.Error = err.Error()
	result.Attempts = attempts
	s.logger.Error("notification_job_error", map[string]any{
		"jobId":  result.JobID,
		"type":   result.Type,
		"reason": reason,
		"error":  err.Error(),
	})
	return result
}

// logNotifyScheduled 记一条与 Node 同名的排程日志。
func (s *NotifyScheduler) logNotifyScheduled(schedule NotifySchedule) {
	fields := map[string]any{
		"jobId":     schedule.JobID,
		"type":      schedule.Type,
		"schedule":  schedule.Rule.String(),
		"mode":      notifyScheduleMode(schedule),
		"timezone":  schedule.Timezone,
		"targetId":  schedule.TargetID,
		"bindingId": schedule.BindingID,
	}
	s.logger.Info("notification_task_scheduled", fields)
}

// notifyScheduleMode 判断任务落在哪个模式（日志字段与 Node 一致）。
func notifyScheduleMode(schedule NotifySchedule) string {
	if schedule.WebhookURL != "" {
		return "legacy"
	}
	return "targets"
}

// Scheduled 返回当前调度集的只读视图（按 NextRun 与 JobID 排序，便于断言）。
func (s *NotifyScheduler) Scheduled() []NotifyScheduledJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := make([]NotifyScheduledJob, 0, len(s.schedules))
	for _, job := range s.schedules {
		view = append(view, NotifyScheduledJob{
			Schedule: job.Schedule,
			NextRun:  job.NextRun,
			Fires:    job.Fires,
		})
	}
	sort.Slice(view, func(i, j int) bool {
		if view[i].NextRun.Equal(view[j].NextRun) {
			return view[i].Schedule.JobID < view[j].Schedule.JobID
		}
		return view[i].NextRun.Before(view[j].NextRun)
	})
	return view
}

// Len 返回当前调度条数。
func (s *NotifyScheduler) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.schedules)
}

// currentSchedules 取当前调度集（浅拷贝，重排路径用）。
func (s *NotifyScheduler) currentSchedules() []NotifySchedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]NotifySchedule, 0, len(s.schedules))
	for _, job := range s.schedules {
		out = append(out, job.Schedule)
	}
	return out
}

// setSchedules 整体替换调度集。
func (s *NotifyScheduler) setSchedules(jobs []*notifyScheduledJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schedules = jobs
}

// settingsFromPools 读设置与系统时区（默认实现）。
func (s *NotifyScheduler) settingsFromPools(
	ctx context.Context,
) (store.AdminNotificationSettings, string, error) {
	if s.pools == nil {
		return store.AdminNotificationSettings{}, "", errors.New("jobs: 未配置连接池，无法读通知设置")
	}
	settings, err := s.pools.AdminGetNotificationSettings(ctx)
	if err != nil {
		return store.AdminNotificationSettings{}, "", err
	}
	return settings, s.pools.AdminSystemTimezoneOrUTC(ctx), nil
}

// bindingsFromPools 读某类型的绑定（默认实现）。
func (s *NotifyScheduler) bindingsFromPools(
	notificationType string,
) ([]store.AdminNotificationBinding, error) {
	if s.pools == nil {
		return nil, errors.New("jobs: 未配置连接池，无法读通知绑定")
	}
	return s.pools.AdminListNotificationBindings(context.Background(), notificationType)
}

// notifyInitialRun 给出规则在当前时刻之后的下一次触发。
//
// 为什么非对齐间隔要显式锚到「现在 + 间隔」：Bull 的 `every` 是入队后过 N 才首次触发，
// 不按整分网格；若直接用 Next(now) 会立即触发一次，与 Node 不同。
func notifyInitialRun(rule NotifyRule, now time.Time) time.Time {
	if rule.Interval != nil && !rule.Interval.Aligned {
		return now.Add(rule.Interval.Interval)
	}
	return rule.Next(now)
}

// advisoryNotifyLeader 用 PG advisory 锁实现独占权（多实例只有一个触发）。
type advisoryNotifyLeader struct {
	pools    *store.Pools
	lockName string
}

func (l *advisoryNotifyLeader) Acquire(
	ctx context.Context,
) (func(context.Context), bool, error) {
	if l.pools == nil {
		// 没有池（骨架模式）：无别的实例可争，直接放行。
		return func(context.Context) {}, true, nil
	}
	lock, acquired, err := AcquireLeader(ctx, l.pools, l.lockName)
	if err != nil || !acquired {
		return nil, acquired, err
	}
	return func(releaseCtx context.Context) { _ = lock.Release(releaseCtx) }, true, nil
}

// notifyErrorText 把可空错误压成日志字段。
func notifyErrorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// realNotifyClock 满足 NotifyClock（编译期守住这个约定）。
var _ NotifyClock = realNotifyClock{}
