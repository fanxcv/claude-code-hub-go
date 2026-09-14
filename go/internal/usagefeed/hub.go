// Package usagefeed 是「使用记录有新行落库」这一事实的跨实例广播面。
//
// 存在理由：管理面的使用记录页原来只靠前端轮询（非全屏 5s / 全屏 3s），而每次轮询会把
// 已加载的每一页整份重取（每页 284 KB）。推送模式改走**信号式**：只广播「有新行」这一个
// 事实（`{"maxId":N,"count":M}`），前端收到后再用增量接口取那几行。
//
// 三条设计约束：
//
//  1. **信号里不含行内容**。本包只搬 `maxId` 与 `count` 两个整数，不碰用户、密钥、正文。
//     这样即使通道被误订阅，泄漏面也只有「某时刻有 N 行新增」这一元信息。
//
//  2. **发布是 fire-and-forget**。`NotifyNewRow` 不返回错误、不阻塞调用方，发布失败只记
//     warn：它挂在终态结算与开行的关键路径上，绝不能让一次 Redis 抖动把结算打坏。反证见
//     `hub_test.go` 的 `TestNotifyWithoutRedisDoesNotBlockOrPanic`。
//
//  3. **投递只经 Redis 一条路径**。发布方把信号交给 Redis，订阅方（**包括发布方自己所在
//     的进程**）都从 Redis 收到后再扇出给本进程的 SSE 连接。刻意不做「本地直投 + Redis 广播」
//     的双路径：那会在同一进程里重复投递同一条信号，把 `count` 的语义搞错（前端拿它显示增量）。
//     代价是「Redis 未装配/不可用 ⇒ 收不到信号」，此时 SSE 连接照常建立与心跳，前端按
//     可用性回退轮询（见 residual risk 一节）。
package usagefeed

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/redis/go-redis/v9"
)

// ChannelNewRows 是「有新行落库」的广播通道名。
//
// 通道名是跨进程契约：一旦上线就不能改名（改名即静默失去推送，前端会一直等信号）。
const ChannelNewRows = "usage-logs:new-rows"

// Signal 是广播载荷。字段名是前端契约的一部分（`new-rows` 事件的 `data`）。
type Signal struct {
	// MaxID 是本次窗口内出现的最大行 id。前端拿它作为下一次增量拉取的 `sinceId`。
	MaxID int64 `json:"maxId"`
	// MinID 是本次窗口内被通知行的**最小** id（加性字段，2026-09 补）。
	//
	// 为何需要它：行 id 是**开行顺序**，不是结算顺序。一条流式请求开行 5 分钟后才结算，
	// 此时它的 id 低于前端已记的高水位——纯 `id > sinceId` 的增量拉取**取不到它**，
	// 那一行的用量/计费更新会一直看不到（直到手动刷新）。带上窗口下界后，前端可以
	// 用 `sinceId = minId - 1` 把这一段重拉一遍（靠 id 去重，幂等）。
	//
	// 语义上 MinID <= MaxID；Count == 0 时两者皆为 0（不发事件）。老前端忽略未知键，
	// 故这是向后兼容的加性扩展。
	MinID int64 `json:"minId"`
	// Count 是本次窗口内新增的行数（合并后）。它是**提示**，不是权威计数：
	// 前端仍以增量接口返回的行数为准（两者在丢信号/重连后可能不同）。
	Count int64 `json:"count"`
}

// 默认合并窗口。用户裁决的契约要求「≤200ms 窗口合并/节流」：一次宽表写入风暴（一次请求
// 可能连写开行与终态两笔）不该变成两条广播。
const DefaultMergeWindow = 200 * time.Millisecond

// 订阅失败后的有界退避：1s 起、每次翻倍、上限 60s。
const (
	subscriberBackoffBase = time.Second
	subscriberBackoffMax  = time.Minute
)

// Logger 是本包需要的日志面（`logx.Logger` 满足）；nil 即静默。
type Logger interface {
	Debug(event string, fields map[string]any)
	Warn(event string, fields map[string]any)
}

// Options 是 Hub 的建造参数。
type Options struct {
	// Redis 是命令与订阅连接。nil 表示未装配：广播空转（只记一次 warn），SSE 仍可建立。
	Redis redis.UniversalClient
	// Logger 为 nil 时静默。
	Logger *logx.Logger
	// MergeWindow 是发布侧的合并窗口；<=0 时取 DefaultMergeWindow。
	MergeWindow time.Duration
	// OnSubscribed 在 Redis 订阅握手成功后调用一次（可观测与测试用）。
	// nil 即不调用。它不携带信息：订阅是「本实例能不能收到广播」这件事本身。
	OnSubscribed func()
}

// Hub 既是发布端也是订阅端。
//
// 同一个 Hub 可同时发布与订阅（生产装配即如此）；也可以只发布（数据面）与只订阅（管理面）
// 各持一个实例——两条路径经 Redis 会合，不依赖共享内存，故多实例天然成立。
//
// Hub 可并发使用。零值不可用，请用 NewHub。
type Hub struct {
	client redis.UniversalClient
	logger *logx.Logger
	window time.Duration
	// onSubscribed 见 Options.OnSubscribed。
	onSubscribed func()

	// publisher 侧
	pubMu         sync.Mutex
	pendingMaxID  int64
	pendingMinID  int64
	pendingCount  int64
	flushTimer    *time.Timer
	warnedNoRedis bool

	// subscriber 侧
	subMu    sync.Mutex
	subs     map[*subscriber]struct{}
	started  bool
	closed   bool
	stop     context.CancelFunc
	loopDone chan struct{}
}

// NewHub 建造 Hub。Redis 可为 nil（未装配：只发布空转、不订阅）。
func NewHub(options Options) *Hub {
	window := options.MergeWindow
	if window <= 0 {
		window = DefaultMergeWindow
	}
	return &Hub{
		client:       options.Redis,
		logger:       options.Logger,
		window:       window,
		onSubscribed: options.OnSubscribed,
		subs:         make(map[*subscriber]struct{}),
	}
}

// NotifyNewRow 报告「id 这一行刚落库」。
//
// 刻意不返回错误、不接收 ctx：它挂在结算与开行的关键路径上，调用方**没有**补救手段
// （信号丢了，前端下一轮增量拉取仍会取到那几行）。窗口内的多次调用合并成一条广播。
func (h *Hub) NotifyNewRow(id int64) {
	if h == nil || id <= 0 {
		return
	}

	h.pubMu.Lock()
	if id > h.pendingMaxID {
		h.pendingMaxID = id
	}
	// 窗口下界：第一次通知开窗，此后取更小者。晚结算的低 id 行就靠它被带回。
	if h.pendingCount == 0 || id < h.pendingMinID {
		h.pendingMinID = id
	}
	h.pendingCount++
	// 窗口开启即安排 flush。用 AfterFunc 而非「阻塞等待」：调用方立即返回，
	// 发布发生在计时器 goroutine 里，天然不与请求生命周期耦合。
	if h.flushTimer == nil {
		h.flushTimer = time.AfterFunc(h.window, h.flush)
	}
	h.pubMu.Unlock()
}

// flush 把窗口内累积的信号发布出去。由计时器调用。
func (h *Hub) flush() {
	h.pubMu.Lock()
	signal := Signal{MaxID: h.pendingMaxID, MinID: h.pendingMinID, Count: h.pendingCount}
	h.pendingMaxID = 0
	h.pendingMinID = 0
	h.pendingCount = 0
	h.flushTimer = nil
	h.pubMu.Unlock()

	if signal.Count == 0 {
		return
	}

	if h.client == nil {
		// 未装配 Redis：只记一次，避免每个请求刷一条日志。
		h.pubMu.Lock()
		first := !h.warnedNoRedis
		h.warnedNoRedis = true
		h.pubMu.Unlock()
		if first {
			h.warn("usage_feed_publish_skipped", map[string]any{"reason": "redis_not_wired"})
		}
		return
	}

	payload, err := json.Marshal(signal)
	if err != nil {
		// 载荷是自己构造的两个整数，序列化失败只可能是编程错误；仍不 panic。
		h.warn("usage_feed_payload_encode_failed", map[string]any{"error": err.Error()})
		return
	}

	// 用脱离请求的上下文：请求可能在终态之后立刻被取消或进入排空，沿用原 ctx 会让
	// 「刚落库的那一行」在广播里消失。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.client.Publish(ctx, ChannelNewRows, payload).Err(); err != nil {
		h.warn("usage_feed_publish_failed", map[string]any{
			"channel": ChannelNewRows,
			"max_id":  signal.MaxID,
			"error":   err.Error(),
		})
	}
}

// Subscription 是本进程的一个订阅句柄。它只需要「等信号」与「关闭」两件事，
// 故对使用方（管理面的 SSE 处理器）暴露成接口，便于注入测试替身。
type Subscription interface {
	// Wait 阻塞直到有新信号或被关闭。同一窗口内的多行只会产生一条信号。
	Wait(ctx context.Context) (Signal, error)
	// Close 关闭订阅；Wait 会立刻返回 ErrSubscriptionClosed。
	Close()
}

// Feed 是「订阅本进程的新行信号」这一能力面（实现见 Hub）。
type Feed interface {
	// Subscribe 登记一个订阅者，返回句柄与注销函数。
	Subscribe() (Subscription, func())
}

// subscriber 是 Subscription 的实现：一个「一槽位最新值 + 唤醒」的邮箱。
type subscriber struct {
	hub    *Hub
	wake   chan struct{}
	mu     sync.Mutex
	latest *Signal
	closed bool
}

// Subscribe 登记一个本进程订阅者，返回订阅句柄与注销函数（实现 Feed）。
//
// 订阅会在后台按有界退避连上 Redis；Redis 不可用不阻塞登记，恢复后自动续订。
// 同一 Hub 上多个订阅者各自收到同一批信号（各自独立，互不影响）。
func (h *Hub) Subscribe() (Subscription, func()) {
	if h == nil {
		// 未装配的 Hub：给出一个永远等不到信号的订阅，而不是 nil 解引用。
		return &subscriber{wake: make(chan struct{}), closed: true}, func() {}
	}

	sub := &subscriber{hub: h, wake: make(chan struct{}, 1)}

	h.subMu.Lock()
	h.subs[sub] = struct{}{}
	h.startLoopLocked()
	h.subMu.Unlock()

	disposed := false
	return sub, func() {
		h.subMu.Lock()
		if disposed {
			h.subMu.Unlock()
			return
		}
		disposed = true
		delete(h.subs, sub)
		h.subMu.Unlock()
		sub.Close()
	}
}

// Close 关闭订阅。已在 Wait 上的调用会立刻返回 ErrSubscriptionClosed。
func (s *subscriber) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	// 唤醒等待者，让它们看到 closed。
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Wait 阻塞直到有新信号或被关闭。返回的信号**可能是合并后的**：同一窗口内的多行
// 只产生一条信号（见 Hub 的文件头第 3 条）。
//
// 采用「一槽位最新值 + 唤醒」而不是队列：队列在高频写入下会无界增长，而信号本身
// 是可合并的（前端只关心「现在有新行了」）。阻塞期间若连续来了多条，只有最后一条会被取走。
func (s *subscriber) Wait(ctx context.Context) (Signal, error) {
	if s == nil {
		return Signal{}, ErrSubscriptionClosed
	}
	for {
		s.mu.Lock()
		if s.latest != nil {
			signal := *s.latest
			s.latest = nil
			s.mu.Unlock()
			return signal, nil
		}
		if s.closed {
			s.mu.Unlock()
			return Signal{}, ErrSubscriptionClosed
		}
		s.mu.Unlock()

		select {
		case <-s.wake:
		case <-ctx.Done():
			return Signal{}, ctx.Err()
		}
	}
}

// deliver 把一条信号放进订阅者的槽位并唤醒它。
func (s *subscriber) deliver(signal Signal) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	// 合并：槽位里已有未取走的信号时，按 id 取大/取小、计数相加。
	merged := signal
	if s.latest != nil {
		if s.latest.MaxID > merged.MaxID {
			merged.MaxID = s.latest.MaxID
		}
		// 下界同样要合并：两段窗口的并集下界是两者的较小者，否则早先那条低 id 会被丢掉。
		if s.latest.MinID > 0 && (merged.MinID == 0 || s.latest.MinID < merged.MinID) {
			merged.MinID = s.latest.MinID
		}
		merged.Count += s.latest.Count
	}
	s.latest = &merged
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
		// 已有待处理的唤醒，够了。
	}
}

// startLoopLocked 在首个订阅者登记时启动 Redis 订阅循环。调用方须持有 subMu。
func (h *Hub) startLoopLocked() {
	if h.started || h.closed || h.client == nil {
		return
	}
	h.started = true
	ctx, cancel := context.WithCancel(context.Background())
	h.stop = cancel
	h.loopDone = make(chan struct{})
	go h.loop(ctx, h.loopDone)
}

// loop 订阅 Redis 通道并把消息扇出给本进程订阅者；断线按有界退避重连。
func (h *Hub) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	backoff := subscriberBackoffBase
	for {
		if ctx.Err() != nil {
			return
		}
		if err := h.consume(ctx); err != nil && ctx.Err() == nil {
			h.warn("usage_feed_subscribe_failed", map[string]any{
				"channel":     ChannelNewRows,
				"retry_in_ms": backoff.Milliseconds(),
				"error":       err.Error(),
			})
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff *= 2
			if backoff > subscriberBackoffMax {
				backoff = subscriberBackoffMax
			}
			continue
		}
		// 正常结束（ctx 取消）或一次成功的连接会话：重置退避。
		backoff = subscriberBackoffBase
		if ctx.Err() != nil {
			return
		}
	}
}

// consume 建立一次订阅并读消息，直到出错或 ctx 结束。返回 nil 表示 ctx 结束。
//
// 用 go-redis 的托管通道（`pubsub.Channel()`）而不是自己 ReceiveMessage 加读超时：
// 后者在 Close 时要等一次读超时才返回（实测每个用例因此多花 30s），而托管通道由
// go-redis 负责底层重连，我们的 ctx 取消会立刻让本函数据返回。
func (h *Hub) consume(ctx context.Context) error {
	pubsub := h.client.Subscribe(ctx, ChannelNewRows)
	defer func() { _ = pubsub.Close() }()

	// 先阻塞确认订阅成功，避免把「尚未连上」当成「没有消息」。
	if _, err := pubsub.Receive(ctx); err != nil {
		return err
	}
	h.debug("usage_feed_subscribed", map[string]any{"channel": ChannelNewRows})
	if h.onSubscribed != nil {
		h.onSubscribed()
	}

	messages := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case message, ok := <-messages:
			if !ok {
				return errors.New("usagefeed: 订阅通道已关闭")
			}
			var signal Signal
			if err := json.Unmarshal([]byte(message.Payload), &signal); err != nil {
				h.warn("usage_feed_payload_decode_failed", map[string]any{"error": err.Error()})
				continue
			}
			h.fanout(signal)
		}
	}
}

// fanout 把信号投给本进程的所有订阅者。
func (h *Hub) fanout(signal Signal) {
	h.subMu.Lock()
	targets := make([]*subscriber, 0, len(h.subs))
	for sub := range h.subs {
		targets = append(targets, sub)
	}
	h.subMu.Unlock()

	for _, sub := range targets {
		sub.deliver(signal)
	}
}

// Subscribers 返回当前订阅者数量（观测与测试用）。
func (h *Hub) Subscribers() int {
	if h == nil {
		return 0
	}
	h.subMu.Lock()
	defer h.subMu.Unlock()
	return len(h.subs)
}

// Close 停掉订阅循环并关闭全部订阅者。
func (h *Hub) Close() error {
	if h == nil {
		return nil
	}
	h.subMu.Lock()
	if h.closed {
		h.subMu.Unlock()
		return nil
	}
	h.closed = true
	stop := h.stop
	done := h.loopDone
	subs := make([]*subscriber, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.subs = make(map[*subscriber]struct{})
	h.subMu.Unlock()

	for _, sub := range subs {
		sub.Close()
	}
	if stop != nil {
		stop()
	}
	if done != nil {
		<-done
	}
	return nil
}

func (h *Hub) warn(event string, fields map[string]any) {
	if h.logger == nil {
		return
	}
	h.logger.Warn(event, fields)
}

func (h *Hub) debug(event string, fields map[string]any) {
	if h.logger == nil {
		return
	}
	h.logger.Debug(event, fields)
}
