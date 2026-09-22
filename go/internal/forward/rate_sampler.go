package forward

import (
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件是提交后的速率采样器：**影子标定**与**二级闸**共用同一个实现。
//
// 为什么采样在提交后：门控一旦提交，客户端已收到字节，HTTP 200 与 headers 不可改。
// 已提交后中断对客户端无效——Claude Code 官方明确 mid-stream 出错不重跑（避免重复执行
// tool call），Codex 的 server_is_overloaded 也在不可重试分支。故二级闸只能「标记慢速、
// 让后续请求避开」，救不了当前这个流。
//
// 影子标定为什么也在这里：影子期**不得改变提交时机**，否则拿到的样本就不是「若启用速率闸
// 会怎样」（客户端时延也变了）。故影子期门控照旧在首个语义内容帧提交，由本采样器在提交后
// 按同一口径量 1s/3s/10s 三档。这是标定阈值 θ 的**唯一数据来源**——生产现有数据只有整段
// 生成窗口的平均速率，推不出逐窗速率。
//
// 一次读一个实例，与上游正文的读侧同 goroutine，非并发安全。

// DefaultPostCommitRateWindow 是二级闸的滑动判定窗：自提交起越过裁决期限后，
// 每满一个窗口按该窗内的实测速率对照 θ。
const DefaultPostCommitRateWindow = 10 * time.Second

// MaxRateSamplerBufferBytes 与 shadow 观察者同量级：采样只切帧、不保留正文。
const MaxRateSamplerBufferBytes = 1024 * 1024

// 日志事件名是标定脚本的检索锚，改名即等于改标定口径。
const (
	rateSampleEvent      = "StreamGate[rate]: post-commit rate sample"
	rateDegradationEvent = "StreamGate[rate]: post-commit degradation"
)

// rateMark 是一个标定点：自提交起 elapsed 时刻，按 multiplier×θ 对照。
type rateMark struct {
	index      int
	elapsed    time.Duration
	multiplier int
}

// rateSamplerMarks 与门控的出厂检查点同源（gate 包导出），避免两处各写一遍时刻与倍数。
var rateSamplerMarks = [3]rateMark{
	{0, gate.PrecommitFirstCheckpoint, gate.PrecommitFastMultiplier},
	{1, gate.PrecommitSecondCheckpoint, 1},
	{2, gate.PrecommitVerdictDeadline, 1},
}

// RateSample 是一次提交后速率观测。字段集是标定契约：改字段名等于改标定脚本。
type RateSample struct {
	ProviderID   int64
	ProviderName string
	// Stage 是标定点序号：0=1s、1=3s、2=10s；Degraded 为真时为 -1（滑动窗判定）。
	Stage int
	// ElapsedMS 是自**首个语义内容帧**起的流逝（提交发生在该帧，故也即自提交起）。
	ElapsedMS int
	// PayloadBytes 是累计语义 payload 字节数（口径与门控一致：只算 text/thinking/tool-args）。
	PayloadBytes int
	// BytesPerSecond 是本标定点实测速率。
	BytesPerSecond int
	// Threshold 是本次对照的 θ（字节/秒）；未配置时为 0（只标定不判定）。
	Threshold int
	// Verdict 是按判据得出的结论：commit / continue / slow / no_threshold。
	Verdict string
	// Degraded 为真表示这是二级闸的掉速判定。
	Degraded bool
	// RequestID 是 message_request 行标识（取到时非 0），用于与落库行对齐做标定。
	RequestID int64
	// Model 是观测到的实际模型名（从流内帧提取），便于按模型标定。
	Model string
}

// RateSamplerConfig 是采样器配置。
type RateSamplerConfig struct {
	Family       gate.Family
	ProviderID   int64
	ProviderName string
	// Rate 是 θ（字节/秒）；<=0 表示只标定不判定。
	Rate int
	// Shadow 为真表示只落标定样本，不触发二级闸。
	Shadow bool
	// Window 是二级闸滑动窗；<=0 取 DefaultPostCommitRateWindow。
	Window time.Duration
	// OnSample 收到每个标定点各一次（1s/3s/10s）与掉速判定各一次。
	OnSample func(RateSample)
	// OnDegraded 在二级闸判定「提交后掉速」时回调一次（渠道级，供后续请求避开）。
	OnDegraded func(RateSample)
	// RequestID / Model 是可选上下文；nil 时留空。两者都只在日志与标定里带出。
	RequestID func() (int64, bool)
	Model     func() string
	Logger    *logx.Logger
	// Now 可注入时钟；nil 用 time.Now。采样器只在「帧到达」时推进，故不需要自行定时。
	Now func() time.Time
	// marks 覆盖标定点序列，**仅供包内单测**缩到毫秒级；nil 时用 rateSamplerMarks。
	marks []rateMark
}

// newRateSampler 构造采样器；family 未知时返回 nil（无分类器即无法计量）。
func newRateSampler(config RateSamplerConfig) *rateSampler {
	if config.Family == "" {
		return nil
	}
	return &rateSampler{
		config: config,
		parser: gate.NewParser(gate.ParserOptions{MaxBufferedBytes: MaxRateSamplerBufferBytes}),
	}
}

type rateSampler struct {
	config RateSamplerConfig
	parser *gate.Parser

	startedAt  time.Time
	started    bool
	payload    int
	sampled    [3]bool
	degraded   bool
	windowFrom time.Time
	windowBase int
	parseLost  bool
}

// Observe 喂入一段提交后的上游字节。采样绝不影响热路径：分类或解析异常只丢本次样本。
func (s *rateSampler) Observe(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	if _, err := s.parser.Visit(chunk, func(frame gate.Frame) bool {
		s.observeFrame(frame)
		return true
	}); err != nil {
		s.parseLost = true
	}
	s.checkMarks()
}

// observeFrame 只对「语义内容帧」计量：心跳、头帧、usage、终止包装一律不计入。
// 时钟起点是首个**非零**计量帧，与门控内的速率闸同一口径，两处样本才可比。
func (s *rateSampler) observeFrame(frame gate.Frame) {
	if gate.Classify(s.config.Family, frame.Event, frame.Data) != gate.VerdictContent {
		return
	}
	payloadBytes := gate.SemanticPayloadBytes(s.config.Family, frame.Event, frame.Data)
	if payloadBytes <= 0 {
		return
	}
	if !s.started {
		s.started = true
		s.startedAt = s.now()
	}
	s.payload += payloadBytes
}

// allMarks 返回本次生效的标定点序列。
func (s *rateSampler) allMarks() []rateMark {
	if len(s.config.marks) > 0 {
		return s.config.marks
	}
	return rateSamplerMarks[:]
}

// checkMarks 在每次喂入后推进标定点与二级闸判定。
func (s *rateSampler) checkMarks() {
	if !s.started {
		return
	}
	elapsed := s.now().Sub(s.startedAt)
	marks := s.allMarks()
	for index, mark := range marks {
		if index < len(s.sampled) && s.sampled[index] {
			continue
		}
		if elapsed < mark.elapsed {
			continue
		}
		s.markSampled(index)
		s.emit(s.markSample(mark, index == len(marks)-1, elapsed))
	}
	s.checkDegraded(elapsed)
}

// markSampled 标记某档已采样。sampled 是定长数组，档位超出时退化为不记录（只用来自定义档位的测试）。
func (s *rateSampler) markSampled(index int) {
	if index >= 0 && index < len(s.sampled) {
		s.sampled[index] = true
	}
}

// markSample 生成一个标定点样本。
func (s *rateSampler) markSample(mark rateMark, last bool, elapsed time.Duration) RateSample {
	bytesPerSecond := rateOf(s.payload, elapsed)
	sample := s.baseSample()
	sample.Stage = mark.index
	sample.ElapsedMS = int(elapsed.Milliseconds())
	sample.PayloadBytes = s.payload
	sample.BytesPerSecond = bytesPerSecond
	sample.Threshold = s.config.Rate
	sample.Verdict = rateVerdict(s.config.Rate, mark, last, s.payload, elapsed)
	return sample
}

// checkDegraded 是二级闸：越过裁决期限后按滑动窗对照 θ，掉速即回调一次。
//
// 只报一次：本采样的目的是「让后续请求避开这家」，重复报同一个流没有增量信息，
// 且上游持续慢时每个流都回报会让日志与标记放大。
func (s *rateSampler) checkDegraded(elapsed time.Duration) {
	if s.config.Shadow || s.config.Rate <= 0 || s.degraded {
		return
	}
	// 二级闸自**末档**之后才开始判：那三档本身就是判据，不应与它重叠。
	if elapsed < s.verdictDeadline() {
		return
	}
	window := s.config.Window
	if window <= 0 {
		window = DefaultPostCommitRateWindow
	}
	now := s.now()
	if s.windowFrom.IsZero() {
		s.windowFrom = now
		s.windowBase = s.payload
		return
	}
	windowElapsed := now.Sub(s.windowFrom)
	if windowElapsed < window {
		return
	}
	windowBytes := s.payload - s.windowBase
	bytesPerSecond := rateOf(windowBytes, windowElapsed)
	// 重置窗口：无论是否判掉速都从此刻重新计，避免用陈旧累计值反复判定。
	s.windowFrom = now
	s.windowBase = s.payload
	if bytesPerSecond >= s.config.Rate {
		return
	}
	s.degraded = true
	sample := s.baseSample()
	sample.Stage = -1
	sample.ElapsedMS = int(elapsed.Milliseconds())
	sample.PayloadBytes = windowBytes
	sample.BytesPerSecond = bytesPerSecond
	sample.Threshold = s.config.Rate
	sample.Verdict = "slow"
	sample.Degraded = true
	s.emit(sample)
	if s.config.OnDegraded != nil {
		s.config.OnDegraded(sample)
	}
}

// verdictDeadline 是二级闸开始判定的时刻。取出厂裁决期限（10s），但取实际生效档位的
// 末档——否则注入自定义档位（单测缩到毫秒）时二级闸永不触发，测试与实现语义脱钩。
func (s *rateSampler) verdictDeadline() time.Duration {
	marks := s.allMarks()
	if len(marks) == 0 {
		return gate.PrecommitVerdictDeadline
	}
	return marks[len(marks)-1].elapsed
}

func (s *rateSampler) baseSample() RateSample {
	sample := RateSample{
		ProviderID:   s.config.ProviderID,
		ProviderName: s.config.ProviderName,
	}
	if s.config.RequestID != nil {
		if id, ok := s.config.RequestID(); ok {
			sample.RequestID = id
		}
	}
	if s.config.Model != nil {
		sample.Model = s.config.Model()
	}
	return sample
}

// emit 落一条结构化日志并回调 OnSample。日志字段集即标定契约。
func (s *rateSampler) emit(sample RateSample) {
	if s.config.Logger != nil {
		event := rateSampleEvent
		if sample.Degraded {
			event = rateDegradationEvent
		}
		s.config.Logger.Info(event, map[string]any{
			"providerId":     sample.ProviderID,
			"providerName":   sample.ProviderName,
			"stage":          sample.Stage,
			"elapsedMs":      sample.ElapsedMS,
			"payloadBytes":   sample.PayloadBytes,
			"bytesPerSecond": sample.BytesPerSecond,
			"threshold":      sample.Threshold,
			"verdict":        sample.Verdict,
			"shadow":         s.config.Shadow,
			"requestId":      sample.RequestID,
			"model":          sample.Model,
			"parseLost":      s.parseLost,
		})
	}
	if s.config.OnSample != nil {
		s.config.OnSample(sample)
	}
}

func (s *rateSampler) now() time.Time {
	if s.config.Now != nil {
		return s.config.Now()
	}
	return time.Now()
}

// rateOf 由字节数与时长折算速率（字节/秒）。时长非正时返回 0。
func rateOf(payloadBytes int, elapsed time.Duration) int {
	millis := elapsed.Milliseconds()
	if millis <= 0 {
		return 0
	}
	return int(int64(payloadBytes) * 1000 / millis)
}

// rateVerdict 复算「若启用速率闸，本档会怎么判」。
//
// 与门控内 Ladder 用同一算式（θ × 倍数 × 实逝秒数），只差一个严格大于：两处不一致
// 会让影子标定出的阈值与实判行为对不上。θ<=0 时无阈值可对照，返回 no_threshold。
func rateVerdict(rate int, mark rateMark, last bool, payloadBytes int, elapsed time.Duration) string {
	if rate <= 0 {
		return "no_threshold"
	}
	need := int64(rate) * int64(mark.multiplier) * elapsed.Milliseconds() / 1000
	if int64(payloadBytes) > need {
		return "commit"
	}
	if last {
		return "slow"
	}
	return "continue"
}
