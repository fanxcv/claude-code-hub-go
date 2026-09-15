package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/notify"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件把通知调度器的两个反向外设接到管理面：**投递**与**数据生成**。
//
// 为什么放在这里：投递要复用 webhook_deliver.go 的信封、签名与响应判定（那一层是
// package adminapi 的内部实现），而 internal/jobs 又不能反向依赖 adminapi（adminapi 已依赖
// jobs，会成环）。故由 jobs 定义接口、本包实现，装配处（cmd/cchd）把实现注进去。
//
// 对应 Node：
//   - 处理作业时的目标解析与开关复检：src/lib/notification/notification-queue.ts:557-566（目标缺失/停用）、
//     571-581（取 binding.templateOverride）、585（缺目的地）；
//   - legacy 单 URL 的渠道推断：src/lib/webhook/notifier.ts:62 的 detectProvider、69 的 getEndpointUrl；
//   - 任务类型的 webhook 映射：notification-queue.ts:45-51；
//   - 三个定时类型的数据现算：internal/notify（逐条对照见该包 doc.go）。
//
// 登记差异：
//  1. 正文仍是 webhook_deliver.go 的简化文案（见该文件头「登记进差异白名单的一项」）；
//     binding 的 templateOverride 同样未参与拼装（Node 会覆盖模板）。渠道、字段名、
//     可解析性与 Node 一致。
//  2. 一次投递只发一个 HTTP 请求（MaxAttempts=1）：Bull 的 attempts/backoff 由
//     jobs.NotifyScheduler 在任务层实现，避免两层重试把一次失败放大成九次。
//  3. 成本预警一次算出的多条告警**都会发出**（每份一个 HTTP），而 Node 的
//     notification-queue.ts:465 只发 `alerts[0]` 并注明「后续可扩展为批量发送」。
//     这是有意偏离：只发第一条会把其余超限对象静默丢掉。

// NotificationDelivery 实现 jobs.NotifyDeliverer。
type NotificationDelivery struct {
	pools  *store.Pools
	logger *logx.Logger
}

// NewNotificationDelivery 建一个投递器；pools 为 nil 时目标模式无法解析，只会如实报错。
func NewNotificationDelivery(pools *store.Pools, logger *logx.Logger) *NotificationDelivery {
	if logger == nil {
		logger = logx.New(nil)
	}
	return &NotificationDelivery{pools: pools, logger: logger}
}

// Deliver 投递一条通知任务。
func (d *NotificationDelivery) Deliver(
	ctx context.Context,
	request jobs.NotifyDeliveryRequest,
) (jobs.NotifyDeliveryResult, error) {
	notificationType, ok := notificationTypeFromJobType(request.Type)
	if !ok {
		return jobs.NotifyDeliveryResult{}, fmt.Errorf("未知通知类型: %s", request.Type)
	}
	timezone := request.Timezone
	if timezone == "" {
		timezone = "UTC"
	}

	// legacy 模式：单 URL 直发，渠道由主机名推断（Node 的 notifier 同判）。
	if request.WebhookURL != "" {
		providerType, err := notificationDetectProvider(request.WebhookURL)
		if err != nil {
			// Node 在此也是抛错 → Bull 重试；Go 侧交给任务层退避重试。
			return jobs.NotifyDeliveryResult{}, err
		}
		url := request.WebhookURL
		result := adminSendWebhook(ctx, store.AdminWebhookTarget{
			ProviderType: providerType,
			WebhookURL:   &url,
		}, webhookSendOptions{
			NotificationType: notificationType,
			Timezone:         timezone,
			MaxAttempts:      1,
			Data:             request.Data,
		})
		return notifyDeliveryResultOf(result), nil
	}

	// targets 模式：按目标 id 取目标并复检启用状态（Node 的 getWebhookTargetById + isEnabled）。
	if request.TargetID == 0 {
		return jobs.NotifyDeliveryResult{}, errors.New("通知任务既没有 webhookUrl 也没有 targetId")
	}
	if d.pools == nil {
		return jobs.NotifyDeliveryResult{}, errors.New("未装配连接池，无法解析推送目标")
	}
	target, err := d.pools.AdminGetWebhookTarget(ctx, request.TargetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 目标已被删除：Node 记 warn 并跳过（不重试）。
			return jobs.NotifyDeliveryResult{Skipped: true}, nil
		}
		return jobs.NotifyDeliveryResult{}, err
	}
	if !target.IsEnabled {
		return jobs.NotifyDeliveryResult{Skipped: true}, nil
	}

	result := adminSendWebhook(ctx, target, webhookSendOptions{
		NotificationType: notificationType,
		Timezone:         timezone,
		MaxAttempts:      1,
		Data:             request.Data,
	})
	return notifyDeliveryResultOf(result), nil
}

// notifyDeliveryResultOf 把投递结果投影成调度器要的形状。
func notifyDeliveryResultOf(result webhookSendResult) jobs.NotifyDeliveryResult {
	return jobs.NotifyDeliveryResult{
		Success:   result.Success,
		Error:     result.Error,
		LatencyMS: result.LatencyMS,
	}
}

// notificationTypeFromJobType 复刻 toWebhookNotificationType（notification-queue.ts:45-51）。
func notificationTypeFromJobType(jobType string) (string, bool) {
	switch jobType {
	case jobs.NotifyTypeCircuitBreaker:
		return "circuit_breaker", true
	case jobs.NotifyTypeDailyLeaderboard:
		return "daily_leaderboard", true
	case jobs.NotifyTypeCostAlert:
		return "cost_alert", true
	case jobs.NotifyTypeCacheHitRateAlert:
		return "cache_hit_rate_alert", true
	default:
		return "", false
	}
}

// NotificationAlerts 实现 jobs.NotifyPayloadSource：四种通知类型的数据生成入口。
//
// 对应 Node：
//   - daily-leaderboard：tasks/daily-leaderboard.ts:11（近 24h 用户榜前 N + 全量合计）
//   - cost-alert：tasks/cost-alert.ts:14（密钥 5h/周/月 + 供应商周/月的阈值比较）
//   - cache-hit-rate-alert：tasks/cache-hit-rate-alert.ts:209（窗口切分、基线判定、冷却去重）
//   - circuit-breaker：**事件型**，数据由转发路径在开闸那一刻随任务带上（generate* 不参与）
//
// 一次可返回多份：成本预警在多个对象超限时就是多条告警，Node 的队列逐条发送，Go 侧同样逐条投递。
type NotificationAlerts struct {
	generators *notify.Generators
	logger     *logx.Logger
}

// NewNotificationAlerts 建一个数据生成器；generators 为 nil 时一律回「本刻无数据」。
func NewNotificationAlerts(generators *notify.Generators, logger *logx.Logger) *NotificationAlerts {
	if logger == nil {
		logger = logx.New(nil)
	}
	if generators != nil && generators.Logger == nil {
		generators.Logger = logger
	}
	return &NotificationAlerts{generators: generators, logger: logger}
}

// Payloads 现算本轮要投递的数据。
func (a *NotificationAlerts) Payloads(
	ctx context.Context,
	request jobs.NotifyPayloadRequest,
) ([]jobs.NotifyPayload, error) {
	if a.generators == nil {
		a.logger.Warn("notification_payload_generators_unwired", map[string]any{
			"type": request.Type,
		})
		return nil, nil
	}
	switch request.Type {
	case jobs.NotifyTypeDailyLeaderboard:
		return a.leaderboardPayload(ctx, request)
	case jobs.NotifyTypeCostAlert:
		return a.costAlertPayloads(ctx, request)
	case jobs.NotifyTypeCacheHitRateAlert:
		return a.cacheHitRateAlertPayload(ctx, request)
	default:
		// 事件型任务自带数据（NeedsPayload=false），走到这里说明装配错了。
		return nil, fmt.Errorf("该类型不需要现算数据: %s", request.Type)
	}
}

func (a *NotificationAlerts) leaderboardPayload(
	ctx context.Context,
	request jobs.NotifyPayloadRequest,
) ([]jobs.NotifyPayload, error) {
	data, err := a.generators.DailyLeaderboard(
		ctx,
		notify.LeaderboardTopN(request.Settings),
		request.SystemTimezone,
		request.Now,
	)
	if err != nil {
		return nil, err
	}
	if data == nil {
		a.logger.Info("notification_payload_no_data", map[string]any{"type": request.Type})
		return nil, nil
	}
	return marshalNotifyPayloads([]any{data})
}

func (a *NotificationAlerts) costAlertPayloads(
	ctx context.Context,
	request jobs.NotifyPayloadRequest,
) ([]jobs.NotifyPayload, error) {
	alerts, err := a.generators.CostAlerts(
		ctx,
		notify.CostAlertThreshold(request.Settings),
		request.SystemTimezone,
		request.Now,
	)
	if err != nil {
		return nil, err
	}
	if len(alerts) == 0 {
		a.logger.Info("notification_payload_no_data", map[string]any{"type": request.Type})
		return nil, nil
	}
	items := make([]any, 0, len(alerts))
	for _, alert := range alerts {
		items = append(items, alert)
	}
	return marshalNotifyPayloads(items)
}

func (a *NotificationAlerts) cacheHitRateAlertPayload(
	ctx context.Context,
	request jobs.NotifyPayloadRequest,
) ([]jobs.NotifyPayload, error) {
	result, err := a.generators.CacheHitRateAlert(
		ctx,
		request.Settings,
		request.SystemTimezone,
		request.Now,
		request.BindingID,
	)
	if err != nil {
		return nil, err
	}
	if result == nil {
		a.logger.Info("notification_payload_no_data", map[string]any{"type": request.Type})
		return nil, nil
	}
	payloads, err := marshalNotifyPayloads([]any{result.Payload})
	if err != nil {
		return nil, err
	}
	// 冷却键只在**投递成功后**由调度器写下（Node：commitCacheHitRateAlertCooldown）。
	payloads[0].CooldownKeys = result.CooldownKeys
	payloads[0].CooldownTTL = time.Duration(result.CooldownMinutes) * time.Minute
	return payloads, nil
}

// marshalNotifyPayloads 把结构体逐份编码成正文。
func marshalNotifyPayloads(items []any) ([]jobs.NotifyPayload, error) {
	payloads := make([]jobs.NotifyPayload, 0, len(items))
	for _, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("编码通知数据失败: %w", err)
		}
		payloads = append(payloads, jobs.NotifyPayload{Data: encoded})
	}
	return payloads, nil
}

// notificationPayloadSourceRef 给出该类型生成器的 TS 位置（供日志与报告引用）。
func notificationPayloadSourceRef(jobType string) string {
	switch jobType {
	case jobs.NotifyTypeDailyLeaderboard:
		return "src/lib/notification/tasks/daily-leaderboard.ts:11"
	case jobs.NotifyTypeCostAlert:
		return "src/lib/notification/tasks/cost-alert.ts:14"
	case jobs.NotifyTypeCacheHitRateAlert:
		return "src/lib/notification/tasks/cache-hit-rate-alert.ts:209"
	default:
		return ""
	}
}

// 守住接口实现（装配处改签名时这里先报错）。
var _ jobs.NotifyDeliverer = (*NotificationDelivery)(nil)
var _ jobs.NotifyPayloadSource = (*NotificationAlerts)(nil)
