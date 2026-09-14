package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是通知调度器的**纯计算面**：把「设置 + 绑定」算成一组待触发的任务，
// 并给出每条规则的下一次触发时刻。触发与投递见 notify_scheduler.go。
//
// 唯一真源：src/lib/notification/notification-queue.ts（995 行），逐段对应如下
// （行号以该文件当前内容为准）：
//   - 四个任务类型的 webhook 映射：45-51（toWebhookNotificationType）；
//   - legacy 分支（单 URL）：799-861（每日 817、成本 840、缓存命中率 852）；
//   - targets 分支（按绑定）：873-956（每日 873-885、成本 907-920、缓存命中率共享作业 940-956）；
//   - 间隔归一化 clampIntervalMinutes：741；
//   - 间隔到 cron / every 的映射 intervalToRepeat：751；
//   - 每日任务的 cron 拼装 `${minute} ${hour} * * *`：878-882、911-914。
//
// 两处**刻意的语义差异**（都写进报告，不做静默改写）：
//  1. 规则先全量校验再落地：Node 是「先删旧、再逐个 add」（771-980），中途一条非法（例如绑定的
//     scheduleTimezone 不是合法 IANA 名）会抛出，此前的 add 已经生效——留下半套调度。
//     Go 侧用 PlanNotifySchedules 一次算完，任何一条非法就一条不落地（fail-closed），
//     并记 error 日志。后果更可控：宁可这一轮不调度，也不要「一半新一半旧」。
//  2. Node 的 `every`（固定毫秒间隔）由 Bull 按「入队时刻 + N」排队（751 的分支）；Go 侧同样
//     以调度时刻锚定，见 NotifyIntervalRule.Next 与 notifyInitialRun。

// 通知任务类型（notification.constants.ts 的 NotificationJobType，四个取值）。
const (
	NotifyTypeCircuitBreaker    = "circuit-breaker"
	NotifyTypeDailyLeaderboard  = "daily-leaderboard"
	NotifyTypeCostAlert         = "cost-alert"
	NotifyTypeCacheHitRateAlert = "cache-hit-rate-alert"
)

// NotifyRule 是一条重复规则：要么「每天某时刻」，要么「固定间隔/整分对齐间隔」。
//
// 为什么用结构体而不是 cron 字符串：Node 用 cron 是因为 Bull 只吃 cron；
// Go 侧要自己算下一次，保留 cron 字符串只会多一层「拼出来再解析」的可错面。
// 非法时区必须在规划期就被拒绝，故 Location 由 NewNotifyRule 系列构造函数保证非 nil。
type NotifyRule struct {
	// Daily 非 nil 时表示日历规则（每天 hour:minute，按 Location 的墙上时间）。
	Daily *NotifyDailyRule
	// Interval 非 nil 时表示间隔规则。
	Interval *NotifyIntervalRule
}

// NotifyDailyRule 是「每天 HH:MM（Location 墙上时间）」。
type NotifyDailyRule struct {
	Hour     int
	Minute   int
	Location *time.Location
}

// NotifyIntervalRule 是「每 Interval 一次」。
//
// Aligned 为 true 表示 Node 用 cron 表达式 `*/N * * * *`（N<=59 且整除 60），
// 即按墙上时钟的整分网格触发、可携带时区；为 false 时是 Bull 的 `every`，
// 按固定节奏触发、不对齐整点、无时区语义（这是 Bull 的限制，见 notification-queue.ts:838-846）。
type NotifyIntervalRule struct {
	Interval time.Duration
	Aligned  bool
	Location *time.Location
}

// Next 返回规则在 after 之后的下一次触发时刻。
//
// 调用方保证规则合法（Daily 与 Interval 恰有一个非 nil）。
func (r NotifyRule) Next(after time.Time) time.Time {
	switch {
	case r.Daily != nil:
		return r.Daily.next(after)
	case r.Interval != nil:
		return r.Interval.next(after)
	default:
		// 规划期已挡住空规则；真到这一步说明内部有 bug，返回零值让上层按「无规则」处理。
		return time.Time{}
	}
}

// next 返回 Location 时区里下一个 hour:minute。
//
// 实现说明：按「该时区的日历日」推进（AddDate），而不是加 24 小时——夏令时切换当天
// 会少/多一小时，加固定 24h 会让触发时刻漂移一小时。
func (r *NotifyDailyRule) next(after time.Time) time.Time {
	loc := r.Location
	local := after.In(loc)
	candidate := time.Date(local.Year(), local.Month(), local.Day(), r.Hour, r.Minute, 0, 0, loc)
	if candidate.After(after) {
		return candidate
	}
	// 今天这一时刻已过（或正好是现在）→ 取明天的同一墙上时刻。
	tomorrow := local.AddDate(0, 0, 1)
	return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), r.Hour, r.Minute, 0, 0, loc)
}

// next 返回 Interval 规则的下一次触发时刻。
//
// Aligned（cron `*/N * * * *`）：取 Location 墙上时钟的下一个「分钟数是 N 的倍数」的整分。
// 非 Aligned（Bull `every`）：以 after 为锚点加一个间隔——Node 的 every 不按整点对齐，
// 这里同样不刻意对齐到整分。
func (r *NotifyIntervalRule) next(after time.Time) time.Time {
	if !r.Aligned || r.Interval <= 0 {
		return after.Add(r.Interval)
	}
	minutes := int(r.Interval / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	loc := r.Location
	// 先在绝对时间上截到整分：偏移都是整分钟的时区里，这与「按墙上时间截整分」等价。
	candidate := after.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 60; i++ {
		if candidate.In(loc).Minute()%minutes == 0 {
			return candidate
		}
		candidate = candidate.Add(time.Minute)
	}
	// 走不到这里：N 整除 60 时 60 分钟内必有解。兜底加一个间隔，避免返回零值导致的忙循环。
	return after.Add(r.Interval)
}

// String 给出可读的规则描述（对齐 Node 的 describeRepeat 日志字段）。
func (r NotifyRule) String() string {
	switch {
	case r.Daily != nil:
		return fmt.Sprintf("%02d:%02d@%s", r.Daily.Hour, r.Daily.Minute, r.Daily.Location.String())
	case r.Interval != nil && r.Interval.Aligned:
		return fmt.Sprintf("*/%d * * * *@%s", int(r.Interval.Interval/time.Minute), r.Interval.Location.String())
	case r.Interval != nil:
		return fmt.Sprintf("every:%dm", int(r.Interval.Interval/time.Minute))
	default:
		return "invalid"
	}
}

// NotifySchedule 是一条待触发的通知任务（对应 Bull 的一个 repeatable job）。
type NotifySchedule struct {
	// JobID 对应 Node 的 jobId（`daily-leaderboard-scheduled` / `cost-alert:<bindingId>` 等）。
	JobID string
	// Type 是四个任务类型之一。
	Type string
	// WebhookURL 仅在 legacy 模式非空（单 URL 直发）。
	WebhookURL string
	// TargetID 与 BindingID 仅在 targets 模式使用；BindingID 为 0 表示无绑定。
	TargetID  int64
	BindingID int64
	// Rule 是重复规则。
	Rule NotifyRule
	// Timezone 是执行期的格式化时区：绑定的 scheduleTimezone > 系统时区。
	//
	// 与 Node 的差别：Node 在处理作业时**临时**再读一次绑定取时区；Go 侧在规划期就取好。
	// 等价性来自「改绑定会重排任务」——绑定时区变了，下一轮 Reschedule 就带上了新值。
	Timezone string
	// NeedsPayload 表示执行期要现算数据（三个定时类型为 true；事件型 circuit-breaker 由入队方带数据）。
	NeedsPayload bool
	// Data 是入队时就带上的数据（事件型任务用；定时型为空、执行期现算）。
	Data json.RawMessage
}

// NotifyPlanInput 是一次规划的输入。
type NotifyPlanInput struct {
	Settings store.AdminNotificationSettings
	// Bindings 按类型给出该类型的启用中绑定（targets 模式用；legacy 模式不会被读）。
	Bindings func(notificationType string) ([]store.AdminNotificationBinding, error)
	// SystemTimezone 是系统时区名（resolveSystemTimezone 的三级取值，缺失时为 "UTC"）。
	SystemTimezone string
}

// PlanNotifySchedules 把设置与绑定算成待触发的任务集。
//
// 返回的切片顺序与 Node 的 add 顺序一致（每日排行 → 成本预警 → 缓存命中率），
// 便于日志与测试逐条比对。
func PlanNotifySchedules(input NotifyPlanInput) ([]NotifySchedule, error) {
	settings := input.Settings
	if !settings.Enabled {
		// 总开关关闭：Node 在此只做「清空」，不新增任何任务。
		return nil, nil
	}

	systemLocation, err := loadNotifyLocation(input.SystemTimezone)
	if err != nil {
		return nil, err
	}
	if settings.UseLegacyMode {
		return planLegacySchedules(settings, systemLocation)
	}
	return planTargetSchedules(input, systemLocation)
}

// planLegacySchedules 复刻 useLegacyMode 分支（notification-queue.ts:785-843）。
func planLegacySchedules(
	settings store.AdminNotificationSettings,
	systemLocation *time.Location,
) ([]NotifySchedule, error) {
	schedules := make([]NotifySchedule, 0, 3)

	if settings.DailyLeaderboardEnabled &&
		notifyNonEmpty(settings.DailyLeaderboardWebhook) &&
		notifyNonEmpty(settings.DailyLeaderboardTime) {
		hour, minute, err := parseNotifyClock(*settings.DailyLeaderboardTime)
		if err != nil {
			return nil, fmt.Errorf("dailyLeaderboardTime: %w", err)
		}
		schedules = append(schedules, NotifySchedule{
			JobID:      "daily-leaderboard-scheduled",
			Type:       NotifyTypeDailyLeaderboard,
			WebhookURL: *settings.DailyLeaderboardWebhook,
			Rule: NotifyRule{Daily: &NotifyDailyRule{
				Hour:   hour,
				Minute: minute,
				// legacy 模式 Node 不传 tz：cron 在 Bull 的默认时区（进程时区）里解释。
				Location: systemLocation,
			}},
			Timezone:     systemLocation.String(),
			NeedsPayload: true,
		})
	}

	if settings.CostAlertEnabled && notifyNonEmpty(settings.CostAlertWebhook) {
		interval := clampNotifyInterval(notifyIntOr(settings.CostAlertCheckInterval, 60))
		schedules = append(schedules, NotifySchedule{
			JobID:      "cost-alert-scheduled",
			Type:       NotifyTypeCostAlert,
			WebhookURL: *settings.CostAlertWebhook,
			// legacy 模式不带 tz（Node 调用 intervalToRepeat(interval) 单参）。
			Rule: NotifyRule{Interval: &NotifyIntervalRule{
				Interval: interval,
				Aligned:  notifyIntervalAligned(interval),
				Location: systemLocation,
			}},
			Timezone:     systemLocation.String(),
			NeedsPayload: true,
		})
	}

	if settings.CacheHitRateAlertEnabled && notifyNonEmpty(settings.CacheHitRateAlertWebhook) {
		interval := clampNotifyInterval(notifyIntOr(settings.CacheHitRateAlertCheckInterval, 5))
		schedules = append(schedules, NotifySchedule{
			JobID:      "cache-hit-rate-alert-scheduled",
			Type:       NotifyTypeCacheHitRateAlert,
			WebhookURL: *settings.CacheHitRateAlertWebhook,
			Rule: NotifyRule{Interval: &NotifyIntervalRule{
				Interval: interval,
				Aligned:  notifyIntervalAligned(interval),
				Location: systemLocation,
			}},
			Timezone:     systemLocation.String(),
			NeedsPayload: true,
		})
	}

	return schedules, nil
}

// planTargetSchedules 复刻 targets 分支（notification-queue.ts:844-960）。
func planTargetSchedules(
	input NotifyPlanInput,
	systemLocation *time.Location,
) ([]NotifySchedule, error) {
	settings := input.Settings
	loadBindings := input.Bindings
	if loadBindings == nil {
		loadBindings = func(string) ([]store.AdminNotificationBinding, error) { return nil, nil }
	}
	schedules := make([]NotifySchedule, 0, 3)

	if settings.DailyLeaderboardEnabled {
		bindings, err := loadBindings("daily_leaderboard")
		if err != nil {
			return nil, err
		}
		defaultTime := "09:00"
		if notifyNonEmpty(settings.DailyLeaderboardTime) {
			defaultTime = *settings.DailyLeaderboardTime
		}
		hour, minute, err := parseNotifyClock(defaultTime)
		if err != nil {
			return nil, fmt.Errorf("dailyLeaderboardTime: %w", err)
		}
		for _, binding := range bindings {
			rule, timezone, err := notifyDailyRuleForBinding(binding, hour, minute, systemLocation)
			if err != nil {
				return nil, err
			}
			schedules = append(schedules, NotifySchedule{
				JobID:        notifyBindingJobID(NotifyTypeDailyLeaderboard, binding.ID),
				Type:         NotifyTypeDailyLeaderboard,
				TargetID:     binding.TargetID,
				BindingID:    binding.ID,
				Rule:         rule,
				Timezone:     timezone,
				NeedsPayload: true,
			})
		}
	}

	if settings.CostAlertEnabled {
		bindings, err := loadBindings("cost_alert")
		if err != nil {
			return nil, err
		}
		interval := clampNotifyInterval(notifyIntOr(settings.CostAlertCheckInterval, 60))
		for _, binding := range bindings {
			rule, timezone, err := notifyIntervalRuleForBinding(binding, interval, systemLocation)
			if err != nil {
				return nil, err
			}
			schedules = append(schedules, NotifySchedule{
				JobID:        notifyBindingJobID(NotifyTypeCostAlert, binding.ID),
				Type:         NotifyTypeCostAlert,
				TargetID:     binding.TargetID,
				BindingID:    binding.ID,
				Rule:         rule,
				Timezone:     timezone,
				NeedsPayload: true,
			})
		}
	}

	if settings.CacheHitRateAlertEnabled {
		bindings, err := loadBindings("cache_hit_rate_alert")
		if err != nil {
			return nil, err
		}
		interval := clampNotifyInterval(notifyIntOr(settings.CacheHitRateAlertCheckInterval, 5))
		// 只有存在绑定才排一个**共享**作业，处理时再 fan-out 到全部绑定：
		// 每个绑定各排一个会让同一份 payload 被重复计算（notification-queue.ts:944-947 的注释）。
		// 代价：绑定的 scheduleCron / scheduleTimezone 在这个类型上被忽略（照抄 Node）。
		if len(bindings) > 0 {
			schedules = append(schedules, NotifySchedule{
				JobID: "cache-hit-rate-alert-targets-scheduled",
				Type:  NotifyTypeCacheHitRateAlert,
				Rule: NotifyRule{Interval: &NotifyIntervalRule{
					Interval: interval,
					Aligned:  notifyIntervalAligned(interval),
					Location: systemLocation,
				}},
				Timezone:     systemLocation.String(),
				NeedsPayload: true,
			})
		}
	}

	return schedules, nil
}

// notifyDailyRuleForBinding 复刻「binding.scheduleCron ?? 默认每日 cron」与「binding tz ?? 系统 tz」。
//
// 绑定自带 cron 时 Node 直接把 cron 串交给 Bull；Go 侧只支持 Node 会产生的两种形状
// （`M H * * *` 每日、`*/N * * * *` 整分对齐），其余形状如实报错而不是静默退化成默认值。
func notifyDailyRuleForBinding(
	binding store.AdminNotificationBinding,
	defaultHour int,
	defaultMinute int,
	systemLocation *time.Location,
) (NotifyRule, string, error) {
	timezone, location, err := notifyBindingLocation(binding, systemLocation)
	if err != nil {
		return NotifyRule{}, "", err
	}
	hour, minute := defaultHour, defaultMinute
	if notifyNonEmpty(binding.ScheduleCron) {
		parsedHour, parsedMinute, err := parseNotifyCron(*binding.ScheduleCron)
		if err != nil {
			return NotifyRule{}, "", fmt.Errorf("binding %d scheduleCron: %w", binding.ID, err)
		}
		hour, minute = parsedHour, parsedMinute
	}
	return NotifyRule{Daily: &NotifyDailyRule{Hour: hour, Minute: minute, Location: location}}, timezone, nil
}

// notifyIntervalRuleForBinding 复刻「binding.scheduleCron ?? 默认间隔（按 N 是否整除 60 选 cron 或 every）」。
func notifyIntervalRuleForBinding(
	binding store.AdminNotificationBinding,
	interval time.Duration,
	systemLocation *time.Location,
) (NotifyRule, string, error) {
	timezone, location, err := notifyBindingLocation(binding, systemLocation)
	if err != nil {
		return NotifyRule{}, "", err
	}
	if notifyNonEmpty(binding.ScheduleCron) {
		// 成本预警允许绑定给「每 N 分钟」形状的 cron（`*/N * * * *`）。
		minutes, err := parseNotifyIntervalCron(*binding.ScheduleCron)
		if err != nil {
			return NotifyRule{}, "", fmt.Errorf("binding %d scheduleCron: %w", binding.ID, err)
		}
		return NotifyRule{Interval: &NotifyIntervalRule{
			Interval: minutes,
			Aligned:  true,
			Location: location,
		}}, timezone, nil
	}
	return NotifyRule{Interval: &NotifyIntervalRule{
		Interval: interval,
		Aligned:  notifyIntervalAligned(interval),
		Location: location,
	}}, timezone, nil
}

// notifyBindingLocation 取绑定时区（缺省用系统时区），并返回它与名字。
func notifyBindingLocation(
	binding store.AdminNotificationBinding,
	systemLocation *time.Location,
) (string, *time.Location, error) {
	if !notifyNonEmpty(binding.ScheduleTimezone) {
		return systemLocation.String(), systemLocation, nil
	}
	location, err := loadNotifyLocation(*binding.ScheduleTimezone)
	if err != nil {
		return "", nil, fmt.Errorf("binding %d scheduleTimezone: %w", binding.ID, err)
	}
	return *binding.ScheduleTimezone, location, nil
}

// notifyBindingJobID 复刻 targets 模式的 jobId 形状（`<type>:<bindingId>`）。
func notifyBindingJobID(notificationType string, bindingID int64) string {
	return fmt.Sprintf("%s:%d", notificationType, bindingID)
}

// clampNotifyInterval 复刻 clampIntervalMinutes：取整后夹到 [1, 1440] 分钟。
func clampNotifyInterval(rawMinutes int) time.Duration {
	if rawMinutes < 1 {
		rawMinutes = 1
	}
	if rawMinutes > 24*60 {
		rawMinutes = 24 * 60
	}
	return time.Duration(rawMinutes) * time.Minute
}

// notifyIntervalAligned 复刻 intervalToRepeat 的选择：N<=59 且整除 60 才用 cron（可带时区）。
func notifyIntervalAligned(interval time.Duration) bool {
	minutes := int(interval / time.Minute)
	return minutes <= 59 && minutes > 0 && 60%minutes == 0
}

// loadNotifyLocation 解析 IANA 时区名；空名回退 UTC（与 resolveSystemTimezone 的兜底一致）。
func loadNotifyLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("非法时区 %q: %w", name, err)
	}
	return location, nil
}

// parseNotifyClock 解析 "HH:MM"。
func parseNotifyClock(value string) (int, int, error) {
	var hour, minute int
	if _, err := fmt.Sscanf(value, "%d:%d", &hour, &minute); err != nil {
		return 0, 0, fmt.Errorf("无法解析时刻 %q: %w", value, err)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("时刻越界: %q", value)
	}
	return hour, minute, nil
}

// parseNotifyCron 接受 Node 会生成的每日 cron：`M H * * *`。
func parseNotifyCron(value string) (int, int, error) {
	var minute, hour int
	var dom, month, dow string
	if _, err := fmt.Sscanf(value, "%d %d %s %s %s", &minute, &hour, &dom, &month, &dow); err != nil {
		return 0, 0, fmt.Errorf("只支持 5 段 cron（%q）: %w", value, err)
	}
	if dom != "*" || month != "*" || dow != "*" {
		return 0, 0, fmt.Errorf("只支持「每天某时刻」的 cron（%q）", value)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("cron 越界: %q", value)
	}
	return hour, minute, nil
}

// parseNotifyIntervalCron 接受 `*/N * * * *`（每 N 分钟）。
func parseNotifyIntervalCron(value string) (time.Duration, error) {
	var minutes int
	var rest1, rest2, rest3, rest4 string
	if _, err := fmt.Sscanf(value, "*/%d %s %s %s %s", &minutes, &rest1, &rest2, &rest3, &rest4); err != nil {
		return 0, fmt.Errorf("只支持「每 N 分钟」的 cron（%q）: %w", value, err)
	}
	if minutes < 1 || minutes > 59 {
		return 0, fmt.Errorf("cron 分钟步进越界: %q", value)
	}
	return clampNotifyInterval(minutes), nil
}

// notifyNonEmpty 判断可空字符串非空。
func notifyNonEmpty(value *string) bool {
	return value != nil && *value != ""
}

// notifyIntOr 取可空整数的值，缺省用 fallback。
func notifyIntOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

// notifySwitchEnabled 判断某任务类型在设置里是否同时满足总开关与子开关。
//
// 对应 Node 在处理作业时对每个类型的复检（notification-queue.ts:452-457 等四处）：
// 入队后开关被关掉时，遗留作业不应继续发送。
func notifySwitchEnabled(settings store.AdminNotificationSettings, notificationType string) bool {
	if !settings.Enabled {
		return false
	}
	switch notificationType {
	case NotifyTypeCircuitBreaker:
		return settings.CircuitBreakerEnabled
	case NotifyTypeDailyLeaderboard:
		return settings.DailyLeaderboardEnabled
	case NotifyTypeCostAlert:
		return settings.CostAlertEnabled
	case NotifyTypeCacheHitRateAlert:
		return settings.CacheHitRateAlertEnabled
	default:
		return false
	}
}

// NotifyPayloadRequest 是「现算一份通知数据」的输入。
type NotifyPayloadRequest struct {
	// Type 是任务类型。
	Type string
	// Timezone 是执行期时区名（窗口切分与文案时间戳都用它）。
	Timezone string
	// Now 是执行时刻。
	Now time.Time
}

// NotifyPayloadSource 现算一份通知数据（Node 的 generateDailyLeaderboard / generateCostAlerts /
// generateCacheHitRateAlertPayload 三个生成器）。
//
// 返回 (nil, false, nil) 表示「本刻无数据」，对应 Node 的 `return { success: true, skipped: true }`：
// 不发、不重试、只记日志。
type NotifyPayloadSource interface {
	Payload(ctx context.Context, request NotifyPayloadRequest) (json.RawMessage, bool, error)
}

// NotifyDeliveryRequest 一次投递的输入。
type NotifyDeliveryRequest struct {
	Type       string
	WebhookURL string
	TargetID   int64
	BindingID  int64
	Data       json.RawMessage
	Timezone   string
}

// NotifyDeliveryResult 一次投递的结果。
type NotifyDeliveryResult struct {
	// Success 为 true 表示这一条已送达（或按语义跳过，见 Skipped）。
	Success bool
	// Skipped 为 true 表示「不该发」：目标缺失/被停用。对应 Node 的 skipped 返回，不触发重试。
	Skipped bool
	// Error 是失败原因（Skipped 时可空）。
	Error string
	// LatencyMS 是投递耗时。
	LatencyMS int64
}

// NotifyDeliverer 投递一条通知（实现见 internal/adminapi 的 NotificationDelivery，
// 复用 webhook_deliver.go 的信封、签名与重试）。
type NotifyDeliverer interface {
	Deliver(ctx context.Context, request NotifyDeliveryRequest) (NotifyDeliveryResult, error)
}
