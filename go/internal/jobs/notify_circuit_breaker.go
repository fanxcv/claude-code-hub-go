package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/notify"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是**熔断告警的产生点**：熔断状态机开闸时把一条事件型任务投出去。
//
// 对应 Node：
//   - 触发点：src/lib/circuit-breaker.ts:571-573（供应商级开闸后 triggerCircuitBreakerAlert）、
//     src/lib/endpoint-circuit-breaker.ts:478（端点级）；
//   - 告警前的三重闸门与去重：src/lib/notification/notifier.ts:11-110
//     （① 总开关与 circuitBreakerEnabled 复检；② Redis 键
//     `circuit-breaker-alert:{providerId}:{source|endpoint:id}` 挡 5 分钟内的重复；
//     ③ legacy 模式看 circuitBreakerWebhook、targets 模式逐绑定入队）；
//   - 告警数据里 providerName 的来源：circuit-breaker.ts:606-618（查库取名字，
//     查不到就放弃告警并记 warn）、endpoint-circuit-breaker.ts:461-472（端点 url/label）。
//
// 为什么放在 jobs 包：通知的设置、绑定与入队都在这条链上（NotifyScheduler），
// 而 health 包只认识熔断状态。故由 health 的回调（Options.OnProviderOpened /
// OnEndpointOpened）把事件交出来，这里再决定「发不发、发给谁」。

// CircuitBreakerAlertSource 是熔断事件的来源（对齐 Node 的 incidentSource）。
type CircuitBreakerAlertSource string

// 两种来源。
const (
	CircuitBreakerSourceProvider CircuitBreakerAlertSource = "provider"
	CircuitBreakerSourceEndpoint CircuitBreakerAlertSource = "endpoint"
)

// CircuitBreakerAlertEvent 是一次开闸事件的原始输入（health 的回调给出的四个值）。
type CircuitBreakerAlertEvent struct {
	Source CircuitBreakerAlertSource
	// ProviderID 在供应商级是供应商 id；端点级是端点所属的 vendor id（Node 同判）。
	ProviderID   int64
	EndpointID   int64
	FailureCount int64
	// OpenUntilMS 是开闸窗口的结束时刻（毫秒时间戳）。
	OpenUntilMS int64
	// Cause 是最后一次失败的原因（正文的 lastError）。
	Cause error
}

// CircuitBreakerAlertStore 是发布器要读的那几个面（*store.Pools 满足它）。
//
// 用窄接口而不是直接收 *store.Pools：发布器的四条分支（开关关、读不到实体、无绑定、
// legacy 缺 webhook）都只能造数据才能跑到，收具体类型就只剩真库集成测试一条路。
type CircuitBreakerAlertStore interface {
	AdminGetNotificationSettings(ctx context.Context) (store.AdminNotificationSettings, error)
	AdminGetProviderByID(ctx context.Context, id int64) (*store.AdminProvider, error)
	AdminGetProviderEndpointByID(ctx context.Context, id int64) (*store.AdminProviderEndpoint, error)
	AdminListNotificationBindings(ctx context.Context, notificationType string) ([]store.AdminNotificationBinding, error)
	AdminSystemTimezoneOrUTC(ctx context.Context) string
}

// CircuitBreakerAlertPublisher 处理一次开闸事件：查名字、复检闸门、逐绑出入队。
type CircuitBreakerAlertPublisher struct {
	// Pools 读设置、绑定、供应商/端点名字。
	Pools CircuitBreakerAlertStore
	// Deliverer 投递一条通知（复用通知调度器的同一套投递与重试语义）。
	Deliverer NotifyDeliverer
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Dedup 挡 5 分钟内的重复（Node 用 Redis；nil 时不去重，与 Node 无 Redis 分支一致）。
	Dedup NotifyDeduper
	// Now 可注入时钟。
	Now func() time.Time
	// Clock 可注入等待（重试退避用）；nil 时用真实时钟。
	Clock NotifyClock
	// SystemTimezone 读系统时区；缺省从 Pools 读。
	SystemTimezone func(ctx context.Context) string

	// mu 串行化「查重 → 入队 → 占坑」这三步。
	//
	// Node 侧这三步之间有 Redis 的往返，理论上也会并发重复；但它靠作业队列去重兜底。
	// Go 侧没有队列，故用进程内锁把窗口内的一致性做足（同一进程内不会双发）。
	// ponytail: 进程内锁；多实例并发时仍可能各发一条，要彻底就改成 SET NX 的原子占坑。
	mu sync.Mutex
}

// CircuitBreakerAlertOptions 是构造参数。
type CircuitBreakerAlertOptions struct {
	// Pools 是读面（生产传 *store.Pools）。
	Pools CircuitBreakerAlertStore
	// Deliverer 投递一条通知（生产传 adminapi 的 NotificationDelivery）。
	Deliverer NotifyDeliverer
	// Dedup 挡 5 分钟内的重复（生产传 NewRedisNotifyDeduper(redisClient)）；nil 时不去重。
	Dedup NotifyDeduper
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Now 可注入时钟；nil 时用真实时钟。
	Now func() time.Time
	// Clock 可注入等待（重试退避用）；nil 时用真实时钟。
	Clock NotifyClock
}

// NewCircuitBreakerAlertPublisher 建一个产生点。
//
// 只有这一个入口（不给字段级默认值留第二处）：装配处漏传 Dedup 或 Clock 都会在构造期被
// 补成安全默认，而不是在开闸那一刻才发现空指针。
func NewCircuitBreakerAlertPublisher(options CircuitBreakerAlertOptions) *CircuitBreakerAlertPublisher {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &CircuitBreakerAlertPublisher{
		Pools:     options.Pools,
		Deliverer: options.Deliverer,
		Dedup:     options.Dedup,
		Logger:    logger,
		Now:       now,
		Clock:     options.Clock,
	}
}

// NotifyDeduper 是「窗口内是否已告警过」的读写面（Node 的 get/set cacheKey 两步）。
type NotifyDeduper interface {
	// Seen 报告这些键里是否有任一已被占（Node 逐个 get；这里一次给全）。
	Seen(ctx context.Context, keys []string) (bool, error)
	// Mark 占坑并设过期。
	Mark(ctx context.Context, keys []string, ttl time.Duration) error
}

// redisNotifyDeduper 是 NotifyDeduper 的 Redis 实现（对齐 Node 的 get / set EX）。
type redisNotifyDeduper struct {
	client redis.UniversalClient
}

// NewRedisNotifyDeduper 包一个 Redis 客户端；client 为 nil 时返回 nil
// （调用方据此视为「不去重」，与 Node 的 `getRedisClient()` 回 null 的分支一致）。
func NewRedisNotifyDeduper(client redis.UniversalClient) NotifyDeduper {
	if client == nil {
		return nil
	}
	return &redisNotifyDeduper{client: client}
}

func (d *redisNotifyDeduper) Seen(ctx context.Context, keys []string) (bool, error) {
	if len(keys) == 0 {
		return false, nil
	}
	values, err := d.client.MGet(ctx, keys...).Result()
	if err != nil {
		return false, err
	}
	for _, value := range values {
		if value != nil {
			return true, nil
		}
	}
	return false, nil
}

func (d *redisNotifyDeduper) Mark(ctx context.Context, keys []string, ttl time.Duration) error {
	if len(keys) == 0 || ttl <= 0 {
		return nil
	}
	pipe := d.client.Pipeline()
	for _, key := range keys {
		pipe.Set(ctx, key, "1", ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// circuitBreakerAlertDedupTTL 对齐 Node 的 `set(cacheKey, "1", "EX", 300)`。
const circuitBreakerAlertDedupTTL = 5 * time.Minute

// NotifyCircuitBreakerAlert 复刻 sendCircuitBreakerAlert（notifier.ts:11）。
//
// 返回是否真的入了队（测试与日志用）。任何失败都不冒泡给调用方：Node 的
// triggerCircuitBreakerAlert 是 `.catch(记日志)`，告警失败绝不影响熔断本身。
func (p *CircuitBreakerAlertPublisher) NotifyCircuitBreakerAlert(
	ctx context.Context,
	event CircuitBreakerAlertEvent,
) bool {
	if p == nil || p.Pools == nil {
		return false
	}
	// 来源缺省按供应商级（Node 的 `data.incidentSource ?? "provider"`），
	// 在这里归一一处，后续的去重键与正文数据才不可能分叉。
	if event.Source == "" {
		event.Source = CircuitBreakerSourceProvider
	}
	logger := p.logger()
	now := p.now()

	settings, err := p.Pools.AdminGetNotificationSettings(ctx)
	if err != nil {
		logger.Warn("circuit_breaker_alert_settings_unreadable", map[string]any{
			"providerId": event.ProviderID,
			"error":      err.Error(),
		})
		return false
	}
	// 闸门①：总开关与熔断子开关（Node notifier.ts:17）。
	if !settings.Enabled || !settings.CircuitBreakerEnabled {
		logger.Info("circuit_breaker_alert_disabled", map[string]any{
			"providerId": event.ProviderID,
			"reason":     "switch_off",
		})
		return false
	}

	// 闸门②：正文要用的实体名（Node 在入队**之前**查；查不到就放弃，不发明名字）。
	data, ok := p.alertData(ctx, event)
	if !ok {
		return false
	}

	// 去重键用**已解析**的 id（Node 在 sendCircuitBreakerAlert 内部用 data.providerId 建键，
	// 而端点级事件传进去的 providerId 是查端点行得到的 vendor id）。
	endpointID := int64(0)
	if data.EndpointID != nil {
		endpointID = *data.EndpointID
	}
	keys := []string{circuitBreakerAlertKey(event.Source, data.ProviderID, endpointID)}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.Dedup != nil {
		seen, seenErr := p.Dedup.Seen(ctx, keys)
		if seenErr != nil {
			// 读失败按「没告警过」处理：漏发比重复发坏（Node 在无 Redis 时也照发）。
			logger.Warn("circuit_breaker_alert_dedup_read_failed", map[string]any{
				"providerId": event.ProviderID,
				"error":      seenErr.Error(),
			})
		} else if seen {
			logger.Info("circuit_breaker_alert_suppressed", map[string]any{
				"providerId":     event.ProviderID,
				"incidentSource": string(event.Source),
				"reason":         "duplicate_within_5min",
			})
			return false
		}
	}

	// 闸门③：目的地。legacy 走单 URL、targets 逐绑定（Node notifier.ts:49-75）。
	enqueued := p.enqueue(ctx, settings, event, data, now)
	if !enqueued {
		return false
	}

	if p.Dedup != nil {
		if markErr := p.Dedup.Mark(ctx, keys, circuitBreakerAlertDedupTTL); markErr != nil {
			logger.Warn("circuit_breaker_alert_dedup_write_failed", map[string]any{
				"providerId": event.ProviderID,
				"error":      markErr.Error(),
			})
		}
	}
	return true
}

// alertData 组装正文数据：查供应商名或端点信息（对齐 Node 的两处 enrich）。
func (p *CircuitBreakerAlertPublisher) alertData(
	ctx context.Context,
	event CircuitBreakerAlertEvent,
) (notify.CircuitBreakerAlertData, bool) {
	logger := p.logger()
	retryAt := time.UnixMilli(event.OpenUntilMS).UTC().Format("2006-01-02T15:04:05.000Z")
	lastError := ""
	if event.Cause != nil {
		lastError = event.Cause.Error()
	}

	if event.Source == CircuitBreakerSourceEndpoint {
		// 端点级：名字与 url 从端点行取（Node endpoint-circuit-breaker.ts:461-472）。
		endpointID := event.EndpointID
		data := notify.CircuitBreakerAlertData{
			ProviderID:     event.ProviderID,
			ProviderName:   "endpoint:" + strconv.FormatInt(endpointID, 10),
			FailureCount:   event.FailureCount,
			RetryAt:        retryAt,
			LastError:      lastError,
			IncidentSource: notify.IncidentSourceEndpoint,
			EndpointID:     &endpointID,
		}
		endpoint, err := p.Pools.AdminGetProviderEndpointByID(ctx, endpointID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			logger.Warn("circuit_breaker_alert_endpoint_lookup_failed", map[string]any{
				"endpointId": endpointID,
				"error":      err.Error(),
			})
		}
		// 查不到端点不是错误（Node 只是少了 url/label），故仍发。
		if endpoint != nil {
			data.EndpointURL = endpoint.URL
			if endpoint.Label != nil && *endpoint.Label != "" {
				data.ProviderName = *endpoint.Label
			}
			if endpoint.VendorID != 0 {
				data.ProviderID = endpoint.VendorID
			}
		}
		return data, true
	}

	// 供应商级：查名字，查不到就放弃（Node circuit-breaker.ts:610-617）。
	provider, err := p.Pools.AdminGetProviderByID(ctx, event.ProviderID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		logger.Warn("circuit_breaker_alert_provider_lookup_failed", map[string]any{
			"providerId": event.ProviderID,
			"error":      err.Error(),
		})
		return notify.CircuitBreakerAlertData{}, false
	}
	if provider == nil {
		logger.Warn("circuit_breaker_alert_provider_not_found", map[string]any{
			"providerId": event.ProviderID,
		})
		return notify.CircuitBreakerAlertData{}, false
	}
	return notify.CircuitBreakerAlertData{
		ProviderName:   provider.Name,
		ProviderID:     event.ProviderID,
		FailureCount:   event.FailureCount,
		RetryAt:        retryAt,
		LastError:      lastError,
		IncidentSource: notify.IncidentSourceProvider,
	}, true
}

// enqueue 按模式把任务放进调度器；返回是否至少入队一条。
func (p *CircuitBreakerAlertPublisher) enqueue(
	ctx context.Context,
	settings store.AdminNotificationSettings,
	event CircuitBreakerAlertEvent,
	data notify.CircuitBreakerAlertData,
	now time.Time,
) bool {
	logger := p.logger()
	payload, err := json.Marshal(data)
	if err != nil {
		logger.Warn("circuit_breaker_alert_encode_failed", map[string]any{
			"providerId": event.ProviderID,
			"error":      err.Error(),
		})
		return false
	}

	if settings.UseLegacyMode {
		webhook := ""
		if settings.CircuitBreakerWebhook != nil {
			webhook = *settings.CircuitBreakerWebhook
		}
		if webhook == "" {
			logger.Info("circuit_breaker_alert_disabled", map[string]any{
				"providerId": event.ProviderID,
				"reason":     "legacy_webhook_missing",
			})
			return false
		}
		return p.dispatch(ctx, NotifySchedule{
			JobID:        circuitBreakerJobID(event, now),
			Type:         NotifyTypeCircuitBreaker,
			WebhookURL:   webhook,
			Timezone:     p.systemTimezone(ctx),
			Data:         payload,
			NeedsPayload: false,
		})
	}

	bindings, err := p.Pools.AdminListNotificationBindings(ctx, circuitBreakerBindingType)
	if err != nil {
		logger.Warn("circuit_breaker_alert_bindings_unreadable", map[string]any{
			"providerId": event.ProviderID,
			"error":      err.Error(),
		})
		return false
	}
	enqueued := false
	for _, binding := range bindings {
		if !binding.IsEnabled {
			continue
		}
		if p.dispatch(ctx, NotifySchedule{
			JobID:        circuitBreakerJobID(event, now),
			Type:         NotifyTypeCircuitBreaker,
			TargetID:     binding.TargetID,
			BindingID:    binding.ID,
			Timezone:     p.systemTimezone(ctx),
			Data:         payload,
			NeedsPayload: false,
		}) {
			enqueued = true
		}
	}
	if !enqueued {
		logger.Info("circuit_breaker_alert_skipped", map[string]any{
			"providerId": event.ProviderID,
			"reason":     "no_bindings",
		})
	}
	return enqueued
}

// dispatch 把一条事件型任务直接投递出去。
//
// 为什么这里直接投而不是塞进调度队列：事件型任务没有重复规则（Node 的
// addNotificationJob 是 Bull 的一次性作业，入队即跑），而 Go 的 NotifyScheduler 管的是
// 「按规则重排的定时任务」。故这里复用同一套 Deliverer 与重试语义，但不经调度表——
// 否则要么给 NotifyScheduler 加一次性任务的概念，要么让熔断路径依赖「调度器恰好已重排」。
func (p *CircuitBreakerAlertPublisher) dispatch(ctx context.Context, schedule NotifySchedule) bool {
	if p.Deliverer == nil {
		p.logger().Warn("circuit_breaker_alert_deliverer_unwired", map[string]any{
			"jobId": schedule.JobID,
		})
		return false
	}
	clock := p.Clock
	if clock == nil {
		clock = realNotifyClock{}
	}
	task := &NotifyScheduler{
		logger:      p.logger(),
		clock:       clock,
		deliverer:   p.Deliverer,
		maxAttempts: notifyDefaultMaxAttempts,
		retryBase:   notifyDefaultRetryBase,
	}
	// 事件型任务自带数据，故不需要 Payloads 与 Cooldown。
	payload := NotifyPayload{Data: schedule.Data}
	outcome, _ := task.deliverPayload(ctx, schedule, payload, 0, &NotifyRunResult{
		JobID: schedule.JobID,
		Type:  schedule.Type,
	})
	return outcome == notifyDeliveryDelivered
}

// circuitBreakerJobID 给一次性任务一个可读 id（Node 是 Bull 生成的数字 id）。
func circuitBreakerJobID(event CircuitBreakerAlertEvent, now time.Time) string {
	suffix := string(event.Source)
	if event.Source == CircuitBreakerSourceEndpoint {
		suffix += ":" + strconv.FormatInt(event.EndpointID, 10)
	}
	return "circuit-breaker:" + strconv.FormatInt(event.ProviderID, 10) + ":" + suffix +
		":" + strconv.FormatInt(now.UnixMilli(), 10)
}

// circuitBreakerAlertKey 是去重键（Node notifier.ts:27-31）。
//
// 两种来源的键不同：供应商级只带 providerId，端点级带 endpoint:{id}——
// 同一供应商的多个端点因此各自告警，不会被彼此的冷却吃掉。
func circuitBreakerAlertKey(source CircuitBreakerAlertSource, providerID int64, endpointID int64) string {
	suffix := string(source)
	if suffix == "" {
		suffix = string(CircuitBreakerSourceProvider)
	}
	if source == CircuitBreakerSourceEndpoint && endpointID != 0 {
		suffix = "endpoint:" + strconv.FormatInt(endpointID, 10)
	}
	return "circuit-breaker-alert:" + strconv.FormatInt(providerID, 10) + ":" + suffix
}

// circuitBreakerBindingType 是绑定表里熔断告警的类型串（下划线，与 tasks 类型串不同）。
//
// 两套命名并存是 Node 的既有事实：作业类型是 "circuit-breaker"（连字符），
// 而绑定表与模板类型键是 "circuit_breaker"（下划线），notification-queue.ts:45-51 做映射。
const circuitBreakerBindingType = "circuit_breaker"

// OnProviderOpened 接供应商级开闸（health.Options.OnProviderOpened）。
//
// 两个适配方法与 health.Options 的回调**同签名**，装配处把 `publisher.OnProviderOpened`
// 直接当回调传进去即可，不必在两处各写一个闭包（签名一旦漂移，装配处会编译不过）。
//
// **必须异步**：回调是在请求路径上同步调用的（转发失败 → RecordProviderFailure → 开闸 →
// 回调），而告警投递带三次尝试与 60s/120s 退避——同步执行会把一次失败的告警变成三分钟的
// 请求卡顿。Node 侧同样 fire-and-forget（circuit-breaker.ts:572 的
// `triggerCircuitBreakerAlert(...).catch(记日志)`，没有 await）。
func (p *CircuitBreakerAlertPublisher) OnProviderOpened(
	providerID int64,
	failureCount int64,
	openUntilMS int64,
	cause error,
) {
	p.dispatchAsync(CircuitBreakerAlertEvent{
		Source:       CircuitBreakerSourceProvider,
		ProviderID:   providerID,
		FailureCount: failureCount,
		OpenUntilMS:  openUntilMS,
		Cause:        cause,
	})
}

// OnEndpointOpened 接端点级开闸（health.Options.OnEndpointOpened）。
func (p *CircuitBreakerAlertPublisher) OnEndpointOpened(
	endpointID int64,
	failureCount int64,
	openUntilMS int64,
	cause error,
) {
	p.dispatchAsync(CircuitBreakerAlertEvent{
		Source:       CircuitBreakerSourceEndpoint,
		EndpointID:   endpointID,
		FailureCount: failureCount,
		OpenUntilMS:  openUntilMS,
		Cause:        cause,
	})
}

// alertDispatchTimeout 是异步告警整条链的上限。
//
// 取值要盖住三次尝试的两段退避（60s + 120s）再加投递本身，故给 5 分钟；超时只影响本次告警，
// 不阻断熔断状态本身（状态早已写进 Redis）。
const alertDispatchTimeout = 5 * time.Minute

// dispatchAsync 把一次告警扔到后台并吞掉 panic。
//
// panic 必须吃：回调在熔断状态机的调用栈里，一个 nil 解引用会把熔断本身带崩。
// 不设并发闸门——开闸是稀有事件（每个供应商/端点在一个窗口内至多一次），
// 而丢掉一条告警比多一个 goroutine 代价大。
func (p *CircuitBreakerAlertPublisher) dispatchAsync(event CircuitBreakerAlertEvent) {
	if p == nil || p.Pools == nil {
		return
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				p.logger().Error("circuit_breaker_alert_panic", map[string]any{
					"providerId": event.ProviderID,
					"recovered":  recovered,
				})
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), alertDispatchTimeout)
		defer cancel()
		p.NotifyCircuitBreakerAlert(ctx, event)
	}()
}

// logger / now / systemTimezone 是三个取值助手。
func (p *CircuitBreakerAlertPublisher) logger() *logx.Logger {
	if p.Logger == nil {
		p.Logger = logx.New(nil)
	}
	return p.Logger
}

func (p *CircuitBreakerAlertPublisher) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

func (p *CircuitBreakerAlertPublisher) systemTimezone(ctx context.Context) string {
	if p.SystemTimezone != nil {
		return p.SystemTimezone(ctx)
	}
	if p.Pools == nil {
		return "UTC"
	}
	return p.Pools.AdminSystemTimezoneOrUTC(ctx)
}
