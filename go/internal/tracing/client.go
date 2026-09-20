package tracing

import (
	"context"
	"encoding/base64"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// Default* 是 Options 里零值项的兜底。数值取自设计稿：队列有界 4096、批 100 条 / 1 秒
// 先到者发、单次出站 5 秒。
const (
	DefaultBaseURL       = "https://cloud.langfuse.com"
	DefaultQueueSize     = 4096
	DefaultBatchSize     = 100
	DefaultFlushInterval = time.Second
	DefaultTimeout       = 5 * time.Second
)

// ingestionPath 是 Langfuse 的批量上报端点（相对 base URL）。
const ingestionPath = "/api/public/ingestion"

// 丢弃告警的限频：第一次丢记一条，之后每 1000 条再记一条（字段里带累计数）。
// 逐条记会把一次下游抖动放大成日志风暴——上游限流时恰恰是最容易丢的时候。
const dropWarnEvery = 1000

// Logger 是本包需要的日志面（`*logx.Logger` 满足）。
//
// Debug 只在 LANGFUSE_DEBUG 打开时被调用，用于逐事件结果与请求级失败。
type Logger interface {
	Warn(event string, fields map[string]any)
	Debug(event string, fields map[string]any)
}

// Options 是本包的全部入参。字段逐条对应 LANGFUSE_* 契约，换算发生在装配层
// （`cmd/cchd/dataplane.go`）——本包不读环境变量，故可用纯单测覆盖。
type Options struct {
	// BaseURL 是 LANGFUSE_BASE_URL；空串取 DefaultBaseURL。
	BaseURL string
	// PublicKey / SecretKey 是 LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY。
	// 任一为空即整体关闭（见 New）。
	PublicKey string
	SecretKey string
	// SampleRate 是 LANGFUSE_SAMPLE_RATE，取值域 [0,1]（契约已钳制）。
	// <=0 全丢，>=1 全留。
	SampleRate float64
	// Debug 是 LANGFUSE_DEBUG：打开后逐事件落 debug 日志。
	Debug bool
	// Logger 为 nil 时静默（丢弃与失败计数仍可用 Counters 读）。
	Logger Logger
	// HTTPClient 为零值时按 Timeout 新建，仅供测试注入假收集器的 transport。
	// 注入时 Timeout 不再作用于它（超时由该 client 自己决定）。
	HTTPClient *http.Client
	// QueueSize / BatchSize / FlushInterval / Timeout 为零值时取 Default*。
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	Timeout       time.Duration
	// Sample 是采样掷点（返回 [0,1)），零值用 rand.Float64。**只为测试注入**：
	// 采样在旁路上，真环境不需要可注入时钟。
	Sample func() float64
}

// Counters 是上报的自述计数（丢弃/发出/失败/采样命中）。
//
// 用原子计数而不是只靠日志：运维要看的是「这段时间丢了多少」，不是翻日志数行。
type Counters struct {
	// Enqueued 是通过采样、真正入队的条数。
	Enqueued uint64
	// Sampled 是被采样挡下的条数（未入队）。
	Sampled uint64
	// Dropped 是队列满而丢掉的条数。
	Dropped uint64
	// Sent 是成功发出的条数（按条计，不按批）。
	Sent uint64
	// Failed 是发出失败的条数（含非 2xx/207 与网络错误）。
	Failed uint64
}

// Tracer 是终态上报器。它满足 `terminal.Tracer`。
//
// 全部可变状态要么是原子计数，要么只被后台 goroutine 触碰（队列本身是 channel），
// 因此可以被任意多个请求 goroutine 并发调用。
type Tracer struct {
	endpoint      string
	authorization string
	client        *http.Client
	sampleRate    float64
	debug         bool
	logger        Logger
	sample        func() float64
	queue         chan terminal.TraceRecord
	batchSize     int
	flushInterval time.Duration

	enqueued atomic.Uint64
	sampled  atomic.Uint64
	dropped  atomic.Uint64
	sent     atomic.Uint64
	failed   atomic.Uint64

	closed   atomic.Bool
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// New 构造上报器。PUBLIC_KEY 或 SECRET_KEY 任一为空（或只有空白）即**整体关闭**：返回 nil。
//
// 返回具体类型（可为 nil 指针）而不是接口，是为了让装配层还能调 Close；
// 交给接缝时**必须**经 AsTracer —— 直接把 nil 指针塞进接口会得到「非 nil 接口包着 nil 指针」，
// 接缝的 nil 判断拦不住，调用即 panic。这类坑在旁路里最难发现，故从签名上隔开两步。
func New(options Options) *Tracer {
	publicKey := strings.TrimSpace(options.PublicKey)
	secretKey := strings.TrimSpace(options.SecretKey)
	if publicKey == "" || secretKey == "" {
		return nil
	}

	baseURL := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	queueSize := options.QueueSize
	if queueSize <= 0 {
		queueSize = DefaultQueueSize
	}
	batchSize := options.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	if batchSize > queueSize {
		batchSize = queueSize
	}
	flushInterval := options.FlushInterval
	if flushInterval <= 0 {
		flushInterval = DefaultFlushInterval
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	sample := options.Sample
	if sample == nil {
		sample = rand.Float64
	}
	sampleRate := options.SampleRate
	if sampleRate < 0 {
		sampleRate = 0
	}
	if sampleRate > 1 {
		sampleRate = 1
	}

	tracer := &Tracer{
		endpoint:      baseURL + ingestionPath,
		authorization: basicAuthorization(publicKey, secretKey),
		client:        client,
		sampleRate:    sampleRate,
		debug:         options.Debug,
		logger:        options.Logger,
		sample:        sample,
		queue:         make(chan terminal.TraceRecord, queueSize),
		batchSize:     batchSize,
		flushInterval: flushInterval,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	go tracer.run()
	return tracer
}

// AsTracer 把上报器交给接缝（`terminal.Tracer`）。nil 时返回**真 nil 接口**，
// 让接缝只需一次 nil 判断（理由见 New 的说明）。
func AsTracer(tracer *Tracer) terminal.Tracer {
	if tracer == nil {
		return nil
	}
	return tracer
}

// basicAuthorization 折出 Basic 头。明文只在构造期存在一次，之后只留 base64 串。
func basicAuthorization(publicKey, secretKey string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(publicKey+":"+secretKey))
}

// Counters 返回当前计数快照。
func (t *Tracer) Counters() Counters {
	if t == nil {
		return Counters{}
	}
	return Counters{
		Enqueued: t.enqueued.Load(),
		Sampled:  t.sampled.Load(),
		Dropped:  t.dropped.Load(),
		Sent:     t.sent.Load(),
		Failed:   t.failed.Load(),
	}
}

// Close 停止接收并尽力发出残余。
//
// 语义：
//   - 先置关闭位（之后的 RecordTerminal 直接返回，不再入队）；
//   - 再通知后台收尾：把队列里已入队的事实一次发完，然后返回；
//   - 调用方给的 ctx 只限制**等待**，超时就返回（残余丢失，属旁路可接受损失）。
//
// 不关闭 channel 是刻意的：并发调用方与关闭方之间「检查关闭位 → 发送」之间存在竞态，
// 关 channel 会有向已关闭 channel 发送的 panic 面；关位 + 不关 channel 没有这个面。
func (t *Tracer) Close(ctx context.Context) {
	if t == nil {
		return
	}
	t.closed.Store(true)
	t.stopOnce.Do(func() { close(t.stop) })
	if ctx == nil {
		<-t.done
		return
	}
	select {
	case <-t.done:
	case <-ctx.Done():
	}
}
