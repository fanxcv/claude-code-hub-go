package cfgsync

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// 失效通道名。与 src/lib/redis/pubsub.ts 的常量逐字对应，不得改名：
// Node 与 Go 在同一套通道上广播，改名即静默失去跨进程失效。
const (
	ChannelErrorRulesUpdated     = "cch:cache:error_rules:updated"
	ChannelRequestFiltersUpdated = "cch:cache:request_filters:updated"
	ChannelSensitiveWordsUpdated = "cch:cache:sensitive_words:updated"
	ChannelProviderGroupsUpdated = "cch:cache:provider_groups:updated"
	ChannelProvidersUpdated      = "cch:cache:providers:updated"
	ChannelSystemSettingsUpdated = "cch:cache:system_settings:updated"
	ChannelAPIKeysUpdated        = "cch:cache:api_keys:updated"
)

// ResyncMessage 是订阅恢复后派发的合成消息。
//
// 存在的理由：Redis Pub/Sub 不会补发断线期间的消息。首次订阅成功与每次断线恢复后
// 都必须派发一次，让订阅方从权威存储重新加载，而不是靠 TTL 兜底。
const ResyncMessage = "cch:cache:resync"

// 订阅连接的有界退避：1s 起、每次失败翻倍、上限 60s。
const (
	subscriberConnectBackoffBase = time.Second
	subscriberConnectBackoffMax  = time.Minute
)

// subscriberReadDeadline 是订阅连接单轮读取的等待上限（生产 30s）。
//
// 声明为 var 而非 const，是为了让「读截止到点 = 安静、不拆连」这条判据能被毫秒级窗口的
// 用例钉住——否则每个断言都得跑满 30s。生产代码不得改写它。
var subscriberReadDeadline = 30 * time.Second

// Logger 是日志接口，由 go/internal/logx 的 Logger 满足；传 nil 即静默。
type Logger interface {
	Debug(event string, fields map[string]any)
	Info(event string, fields map[string]any)
	Warn(event string, fields map[string]any)
	Error(event string, fields map[string]any)
}

type registration struct {
	callback    func(message string)
	needsResync bool
}

// Bus 是失效广播的订阅与发布端。零值不可用，请用 NewBus。
type Bus struct {
	client redis.UniversalClient
	logger Logger

	mu          sync.Mutex
	registered  map[string]map[*registration]struct{}
	subscribed  map[string]struct{}
	connectErr  int
	nextAttempt time.Time
	pubsub      *redis.PubSub
	wakeup      chan struct{}
	closed      bool
	started     bool
	done        chan struct{}
}

// NewBus 建一个订阅总线。client 必须非 nil。
func NewBus(client redis.UniversalClient, logger Logger) *Bus {
	return &Bus{
		client:     client,
		logger:     logger,
		registered: make(map[string]map[*registration]struct{}),
		subscribed: make(map[string]struct{}),
		wakeup:     make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
}

// Subscribe 登记一个失效回调，返回注销函数。
//
// 与 pubsub.ts 一致：首次订阅成功时会立刻派发一次 ResyncMessage。理由是登记前缓存
// 可能已经加载过，必须从权威存储对齐一次，否则会把登记前的陈旧快照一直用到 TTL 到期。
// Redis 不可达不阻塞登记：后台按有界退避重试，恢复后自动订阅并强制 resync。
func (b *Bus) Subscribe(channel string, callback func(message string)) (func(), error) {
	if channel == "" {
		return nil, errors.New("cfgsync: 通道名不能为空")
	}
	if callback == nil {
		return nil, errors.New("cfgsync: 回调不能为空")
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("cfgsync: 总线已关闭")
	}
	target := &registration{callback: callback, needsResync: true}
	regs, ok := b.registered[channel]
	if !ok {
		regs = make(map[*registration]struct{})
		b.registered[channel] = regs
	}
	regs[target] = struct{}{}
	b.ensureLoopLocked()
	b.mu.Unlock()

	b.signal()

	disposed := false
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if disposed {
			return
		}
		disposed = true
		current, ok := b.registered[channel]
		if !ok {
			return
		}
		delete(current, target)
		if len(current) > 0 {
			return
		}
		delete(b.registered, channel)
		delete(b.subscribed, channel)
		if b.pubsub != nil {
			_ = b.pubsub.Unsubscribe(context.Background(), channel)
		}
	}, nil
}

// Publish 广播一条失效消息；message 为空时用当前毫秒时间戳（与 Node 一致）。
// 发布失败只记录日志，不上抛：失效广播是 fail-open，最终一致性由 TTL 兜底。
func (b *Bus) Publish(ctx context.Context, channel, message string) {
	if message == "" {
		message = strconv.FormatInt(time.Now().UnixMilli(), 10)
	}
	if err := b.client.Publish(ctx, channel, message).Err(); err != nil && b.logger != nil {
		b.logger.Warn("cfgsync_publish_failed", map[string]any{"channel": channel, "error": err.Error()})
	}
}

// Close 注销全部订阅并停止后台循环。
func (b *Bus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	pubsub := b.pubsub
	b.pubsub = nil
	b.registered = make(map[string]map[*registration]struct{})
	b.subscribed = make(map[string]struct{})
	b.mu.Unlock()

	close(b.done)
	if pubsub == nil {
		return nil
	}
	return pubsub.Close()
}

// ForceReconnect 主动丢弃当前订阅连接并重建，重建成功后对每个通道强制 resync。
//
// 用途：Redis 主从切换或网络分区之后，订阅套接字可能看起来仍然存活但已收不到消息；
// 该入口让运维与测试能显式要求「重新对齐一次权威存储」，而不必重启进程。
func (b *Bus) ForceReconnect() {
	b.mu.Lock()
	pubsub := b.pubsub
	b.pubsub = nil
	for channel := range b.subscribed {
		delete(b.subscribed, channel)
	}
	for _, regs := range b.registered {
		for reg := range regs {
			reg.needsResync = true
		}
	}
	b.mu.Unlock()

	if pubsub != nil {
		_ = pubsub.Close()
	}
	b.signal()
}

// SubscribedChannels 返回当前已确认订阅的通道数（供 /readyz 与测试观测）。
func (b *Bus) SubscribedChannels() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribed)
}

// DesiredChannels 返回已登记且有回调的通道数。
func (b *Bus) DesiredChannels() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	for _, regs := range b.registered {
		if len(regs) > 0 {
			count++
		}
	}
	return count
}

func (b *Bus) signal() {
	select {
	case b.wakeup <- struct{}{}:
	default:
	}
}

func (b *Bus) ensureLoopLocked() {
	if b.started {
		return
	}
	b.started = true
	go b.loop()
}

func (b *Bus) loop() {
	for {
		select {
		case <-b.done:
			return
		default:
		}

		delay, ready := b.connectIfNeeded()
		if !ready {
			select {
			case <-b.done:
				return
			case <-time.After(delay):
			case <-b.wakeup:
			}
			continue
		}

		b.pump()
		// pump 返回意味着连接已断开或订阅集合发生变化：标记全部待 resync 并重连。
		b.markAllForResync()
	}
}

// connectIfNeeded 确保所有已登记通道都已订阅；返回（重试等待时长, 是否已就绪）。
func (b *Bus) connectIfNeeded() (time.Duration, bool) {
	b.mu.Lock()
	desired := b.desiredChannelsLocked()
	if len(desired) == 0 {
		b.mu.Unlock()
		return time.Second, false
	}
	if b.pubsub != nil {
		pending := b.unsubscribedLocked(desired)
		if len(pending) == 0 {
			b.mu.Unlock()
			return 0, true
		}
	}
	if wait := time.Until(b.nextAttempt); wait > 0 {
		b.mu.Unlock()
		return wait, false
	}
	client := b.client
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), subscriberReadDeadline)
	pubsub := client.Subscribe(ctx)
	err := pubsub.Subscribe(ctx, desired...)
	cancel()
	if err != nil {
		_ = pubsub.Close()

		b.mu.Lock()
		b.connectErr++
		delay := subscriberBackoff(b.connectErr)
		b.nextAttempt = time.Now().Add(delay)
		logger := b.logger
		b.mu.Unlock()

		if logger != nil {
			logger.Warn("cfgsync_subscribe_failed", map[string]any{
				"error":    err.Error(),
				"delayMs":  delay.Milliseconds(),
				"channels": desired,
			})
		}
		return delay, false
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = pubsub.Close()
		return time.Second, false
	}
	b.pubsub = pubsub
	b.connectErr = 0
	b.nextAttempt = time.Time{}
	for _, channel := range desired {
		b.subscribed[channel] = struct{}{}
	}
	logger := b.logger
	b.mu.Unlock()

	if logger != nil {
		logger.Info("cfgsync_subscribed", map[string]any{"channels": desired})
	}

	// 首次订阅成功同样要 resync：登记前缓存可能已经加载过。
	b.dispatchPendingResync()
	return 0, true
}

// isSubscriberReadTimeout 判定「读截止到点、连接仍活」这类错误。
//
// 为什么不能只判 context.DeadlineExceeded：go-redis 把 ctx 的截止时间落到连接的读截止上，
// 到点后返回的是裸 net.OpError（`read tcp …: i/o timeout`），那不是 ctx 错误。只判 ctx 会把
// 空闲订阅当成断连，于是每过一个读窗口就自毁重连一次——生产实测 145/148 次
// cfgsync_subscription_lost 的间隔恰为 31s（30s 读截止 + 1s 重连退避）。
//
// context.Canceled 不是 net.Error，故不落此判据：总线关闭时必须照旧结束 pump。
func isSubscriberReadTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func (b *Bus) pump() {
	for {
		b.mu.Lock()
		pubsub := b.pubsub
		desired := b.desiredChannelsLocked()
		b.mu.Unlock()
		if pubsub == nil {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), subscriberReadDeadline)
		message, err := pubsub.ReceiveMessage(ctx)
		cancel()
		if err != nil {
			// 读截止到点说明连接没断，只是安静；继续等。
			if isSubscriberReadTimeout(err) {
				continue
			}
			// 总线关闭（context.Canceled）不是订阅丢失：静默拆干净，让 pump 结束。
			if b.logger != nil && !errors.Is(err, context.Canceled) {
				b.logger.Warn("cfgsync_subscription_lost", map[string]any{"error": err.Error()})
			}
			b.mu.Lock()
			b.pubsub = nil
			b.subscribed = make(map[string]struct{})
			if b.nextAttempt.IsZero() {
				b.nextAttempt = time.Now().Add(subscriberConnectBackoffBase)
			}
			b.mu.Unlock()
			_ = pubsub.Close()
			return
		}

		if b.logger != nil {
			b.logger.Debug("cfgsync_invalidation_received", map[string]any{
				"channel": message.Channel,
				"message": message.Payload,
			})
		}
		b.dispatch(message.Channel, message.Payload)

		// 订阅集合变化（新通道登记或注销）时退出 pump，回到连接阶段补齐。
		b.mu.Lock()
		changed := len(b.unsubscribedLocked(b.desiredChannelsLocked())) > 0 || b.pubsub != pubsub
		b.mu.Unlock()
		if changed || len(desired) == 0 {
			return
		}
	}
}

func (b *Bus) dispatch(channel, message string) {
	b.mu.Lock()
	var callbacks []func(string)
	for reg := range b.registered[channel] {
		callbacks = append(callbacks, reg.callback)
	}
	b.mu.Unlock()

	for _, callback := range callbacks {
		invokeCallback(callback, message, b.logger, channel)
	}
}

// dispatchPendingResync 对带 resync 标记的登记派发一次合成消息，并在派发前清标记，
// 避免回调内再触发订阅检查时重复派发。
func (b *Bus) dispatchPendingResync() {
	b.mu.Lock()
	type pending struct {
		channel  string
		callback func(string)
	}
	var todo []pending
	for channel, regs := range b.registered {
		for reg := range regs {
			if !reg.needsResync {
				continue
			}
			reg.needsResync = false
			todo = append(todo, pending{channel: channel, callback: reg.callback})
		}
	}
	b.mu.Unlock()

	for _, item := range todo {
		invokeCallback(item.callback, ResyncMessage, b.logger, item.channel)
	}
	if len(todo) > 0 && b.logger != nil {
		b.logger.Info("cfgsync_forced_resync", map[string]any{"callbackCount": len(todo)})
	}
}

func (b *Bus) markAllForResync() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, regs := range b.registered {
		for reg := range regs {
			reg.needsResync = true
		}
	}
}

func (b *Bus) desiredChannelsLocked() []string {
	channels := make([]string, 0, len(b.registered))
	for channel, regs := range b.registered {
		if len(regs) > 0 {
			channels = append(channels, channel)
		}
	}
	return channels
}

func (b *Bus) unsubscribedLocked(desired []string) []string {
	pending := make([]string, 0, len(desired))
	for _, channel := range desired {
		if _, ok := b.subscribed[channel]; !ok {
			pending = append(pending, channel)
		}
	}
	return pending
}

func invokeCallback(callback func(string), message string, logger Logger, channel string) {
	defer func() {
		if recovered := recover(); recovered != nil && logger != nil {
			logger.Error("cfgsync_callback_panic", map[string]any{
				"channel": channel,
				"panic":   recovered,
			})
		}
	}()
	callback(message)
}

// subscriberBackoff 返回第 consecutiveFailures 次失败后的等待时长：1s 起、翻倍、上限 60s。
func subscriberBackoff(consecutiveFailures int) time.Duration {
	if consecutiveFailures <= 0 {
		return 0
	}
	exponent := consecutiveFailures - 1
	if exponent > 10 {
		exponent = 10
	}
	delay := subscriberConnectBackoffBase << exponent
	if delay > subscriberConnectBackoffMax {
		return subscriberConnectBackoffMax
	}
	return delay
}
