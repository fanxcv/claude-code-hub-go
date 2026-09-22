package forward

import (
	"sync"
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
// 并发：标定点**必须在期限当刻取快照**，否则慢流下样本记的是「下一帧到达时」的累计，
// 而慢流恰是这批数据最要紧的那一端（见 Observe 上三条路径的说明）。采样器因此同时被
// 读取 goroutine（Observe）与各档定时器的回调访问，可变状态一律经 mu 保护。

// DefaultPostCommitRateWindow 是二级闸的滑动判定窗：自提交起越过裁决期限后，
// 每满一个窗口按该窗内的实测速率对照 θ。
const DefaultPostCommitRateWindow = 10 * time.Second

// MaxRateSamplerBufferBytes 与 shadow 观察者同量级：采样只切帧、不保留正文。
const MaxRateSamplerBufferBytes = 1024 * 1024

// 日志事件名是标定脚本的检索锚，改名即等于改标定口径。
const (
	rateSampleEvent      = "StreamGate[rate]: post-commit rate sample"
	rateDegradationEvent = "StreamGate[rate]: post-commit degradation"
	// rateStreamEndEvent 是**流结束终值**的检索锚。
	//
	// 为何需要它：标定 bytesPerToken 要拿「整条流的语义字节数」除 message_request.output_tokens，
	// 而三档检查点只给出 1s/3s/10s 的**中间值**，短流根本没有检查点样本。终值把这一半补齐。
	rateStreamEndEvent = "StreamGate[rate]: stream end payload"
)

// StageStreamEnd 是「流结束终值」样本的档位号（不是检查点，与 0/1/2 与二级闸的 -1 都不同）。
const StageStreamEnd = -2

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
	// ElapsedMS 是**该标定点的名义期限**（自首个语义内容帧起算，故 1s 档恒为 1000）。
	//
	// 为何记名义期限而不是实际流逝：样本要回答的是「在检查点 t 当刻，闸会怎么判」，
	// 判据是 θ×倍数×t。记名义值让 PayloadBytes / ElapsedMS / Verdict 三者自洽，
	// 标定脚本不必再猜分母；发射相对期限的偏差另由 EmitElapsedMS 带出。
	ElapsedMS int
	// EmitElapsedMS 是**实际发射**该样本时自首个语义内容帧起的流逝，仅供诊断定时抖动。
	//
	// 与之差得越远，说明该样本的快照越晚于期限（GC 停顿、调度延迟）。常态下两者相等或
	// 相差数毫秒；若长期显著偏大，说明标定数据的时间轴本身不可信，应先修这里。
	EmitElapsedMS int
	// PayloadBytes 是**该标定点当刻**累计的语义 payload 字节数（口径与门控一致：
	// 只算 text/thinking/tool-args）。期限之后才到的字节不计入本档。
	PayloadBytes int
	// BytesPerSecond 是本标定点实测速率。
	BytesPerSecond int
	// Threshold 是本次对照的 θ（字节/秒）；未配置时为 0（只标定不判定）。
	Threshold int
	// Verdict 是按判据得出的结论：commit / continue / slow / no_threshold。
	Verdict string
	// Degraded 为真表示这是二级闸的掉速判定。
	Degraded bool
	// Ended 为真表示这是**流结束终值**（Stage 为 StageStreamEnd）：序号字段无意义，
	// PayloadBytes 是整条流累计的语义字节数，供与 output_tokens 相除标定 bytesPerToken。
	Ended bool
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
	// Now 可注入时钟；nil 用 time.Now。
	Now func() time.Time
	// AfterFunc 安排一次到期回调并返回可撤销句柄；nil 用 time.AfterFunc。
	// **仅供包内单测**注入与注入时钟同步推进的假定时器：真 time.AfterFunc 按真实时间到点，
	// 与注入的假时钟错配，会让标定点在错误的时刻发射。
	AfterFunc func(time.Duration, func()) rateTimer
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

// rateTimer 是一次到期回调的撤销句柄。*time.Timer 满足它，包内单测的假定时器同样满足。
type rateTimer interface{ Stop() bool }

type rateSampler struct {
	config RateSamplerConfig
	parser *gate.Parser

	// mu 保护下面全部可变状态：读取 goroutine（Observe）与各档定时器的回调并发访问。
	mu sync.Mutex
	// startedAt 是时钟起点（首个语义内容帧到达的时刻）。
	startedAt time.Time
	started   bool
	payload   int
	// sent 标记某档已发射；timers 是各档在途的定时器（Close 时逐个撤销）。
	sent   []bool
	timers []rateTimer
	// due 是启动时就已到期（期限 <= 0）的档位样本，由 Observe 在锁外发射。
	//
	// 为何不交给定时器：零延时的真定时器在另一个 goroutine 上「尽快」触发，而流可能在那之前
	// 就到终态并被 Close 撤销——发射与否成了调度竞争。期限已经在过去，语义上就是「当刻已到」。
	due []RateSample
	// closed 让 Close 幂等：终态可能从多条路径到达（调用方放弃消费与泵终态）。
	closed     bool
	degraded   bool
	windowFrom time.Time
	windowBase int
	parseLost  bool
}

// Observe 喂入一段提交后的上游字节。采样绝不影响热路径：分类或解析异常只丢本次样本。
//
// 三条路径各司其职，共同保证「样本记的是期限当刻的快照」：
//   - **到达驱动**（本函数）：只累计字节与推进二级闸，**不再**发射任何标定点。
//     曾经在这里用「当前累计 + 当前流逝」回填已过期档位，于是首帧 1B、次帧 4s 后才到时，
//     1s 档被记成 501B/4s（而不是 1B/1s）——快速流下误差仅毫秒，但慢流下正好是最大误差，
//     而慢流就是要标定的那一端。
//   - **期限定时器**（armTimers 安排的回调）：到点当刻取快照并发射，与字节何时再来无关。
//   - **二级闸**（checkDegraded）：仍走到达驱动。它判的是「一段窗口内的产出速率」，
//     没有新字节就没有新窗口事实可判，由定时器空转只会重复算同一个滑动窗。
func (s *rateSampler) Observe(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	if _, err := s.parser.Visit(chunk, func(frame gate.Frame) bool {
		s.observeFrame(frame)
		return true
	}); err != nil {
		s.mu.Lock()
		s.parseLost = true
		s.mu.Unlock()
	}
	for _, sample := range s.takeDue() {
		s.emit(sample)
	}
	s.checkDegraded()
}

// takeDue 取出启动时就已到期的档位样本；无待发射时返回 nil。
func (s *rateSampler) takeDue() []RateSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.due) == 0 {
		return nil
	}
	due := s.due
	s.due = nil
	return due
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if !s.started {
		s.started = true
		s.startedAt = s.now()
		s.armTimers()
	}
	s.payload += payloadBytes
}

// armTimers 启动时钟时一次性安排好全部标定点的到期定时器。
//
// 为何用定时器而不是靠「下一帧到达时补判」：补判只能拿到下一帧到达那刻的累计与流逝，
// 两者都属于**那一刻**而不属于期限当刻。定时器让快照与上游何时再发字节解耦。
// 调用方必须持有 mu。
func (s *rateSampler) armTimers() {
	marks := s.allMarks()
	s.sent = make([]bool, len(marks))
	s.timers = make([]rateTimer, 0, len(marks))
	for index, mark := range marks {
		index, mark := index, mark
		if mark.elapsed <= 0 {
			// 期限已在过去（仅包内单测用零延时档位）⇒ 当刻即到期。
			s.sent[index] = true
			s.due = append(s.due, s.markSample(mark, index == len(marks)-1))
			continue
		}
		s.timers = append(s.timers, s.afterFunc(mark.elapsed, func() {
			s.emitMark(index, mark)
		}))
	}
}

// emitMark 是某档期限到点时的回调：在锁内取快照并发射，每档只一次。
func (s *rateSampler) emitMark(index int, mark rateMark) {
	s.mu.Lock()
	if s.closed || !s.started || index < 0 || index >= len(s.sent) || s.sent[index] {
		s.mu.Unlock()
		return
	}
	s.sent[index] = true
	sample := s.markSample(mark, index == len(s.sent)-1)
	s.mu.Unlock()
	s.emit(sample)
}

// Close 撤销尚未到期的标定定时器，并落一条**流结束终值**日志。流在末档之前结束
// （短响应、客户端断开、上游失败）时必须调，否则定时器会拖着一个已死的流到最后一档才回收。幂等。
//
// 为何终值记在这里：本函数是终态三条路径（正常结束 / 上游失败 / 中断）的**唯一汇合点**，
// 记在这里就天然覆盖全部终态，不必在每个调用点各补一次。
//
// 零行为影响：本函数在泵终态之后跑，此刻提交早已发生，多一条日志不可能影响提交时机。
func (s *rateSampler) Close(endReason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	timers := s.timers
	s.timers = nil
	// 未启动（整条流没出现过语义内容帧）时没有字节可标定，不记：保持零开销。
	// 采样器本身也只有「有 θ 或影子期」时才会被构造，故未启用时这里根本不会跑。
	var end *RateSample
	if s.started {
		elapsed := s.now().Sub(s.startedAt)
		sample := s.baseSample()
		sample.Stage = StageStreamEnd
		sample.Ended = true
		sample.ElapsedMS = int(elapsed.Milliseconds())
		sample.EmitElapsedMS = sample.ElapsedMS
		sample.PayloadBytes = s.payload
		sample.BytesPerSecond = rateOf(s.payload, elapsed)
		sample.Threshold = s.config.Rate
		sample.Verdict = endReason
		end = &sample
	}
	s.mu.Unlock()
	for _, timer := range timers {
		if timer != nil {
			timer.Stop()
		}
	}
	if end != nil {
		s.emit(*end)
	}
}

// allMarks 返回本次生效的标定点序列。
func (s *rateSampler) allMarks() []rateMark {
	if len(s.config.marks) > 0 {
		return s.config.marks
	}
	return rateSamplerMarks[:]
}

// markSample 在锁内生成一个标定点样本：ElapsedMS 记**名义期限**，EmitElapsedMS 记实际发射时刻。
// 调用方必须持有 mu，且必须已确认该档未发射过。
func (s *rateSampler) markSample(mark rateMark, last bool) RateSample {
	// 分母用名义期限而非实际流逝：判据是 θ×倍数×t，两者必须同轴，否则标定脚本
	// 算出的「若启用会怎样」与门控实判对不上（门控在期限内裁决时用的就是名义时刻）。
	elapsed := mark.elapsed
	sample := s.baseSample()
	sample.Stage = mark.index
	sample.ElapsedMS = int(elapsed.Milliseconds())
	sample.EmitElapsedMS = int(s.now().Sub(s.startedAt).Milliseconds())
	sample.PayloadBytes = s.payload
	sample.BytesPerSecond = rateOf(s.payload, elapsed)
	sample.Threshold = s.config.Rate
	sample.Verdict = rateVerdict(s.config.Rate, mark, last, s.payload, elapsed)
	return sample
}

// checkDegraded 是二级闸：越过裁决期限后按滑动窗对照 θ，掉速即回调一次。
//
// 为何仍走到达驱动：它判的是「一段窗口内的产出速率」，而窗口两端都是「字节到了」这件事。
// 没有新字节就没有新的窗口事实；由定时器空转只会把同一个未满的窗反复算一遍。
//
// 只报一次：本采样的目的是「让后续请求避开这家」，重复报同一个流没有增量信息，
// 且上游持续慢时每个流都回报会让日志与标记放大。
//
// 判定在锁内、发射与回调在锁外：OnDegraded 接线后要写 Redis（见 F2），
// 持采样器锁做网络 IO 会顶住读取 goroutine 的 Observe，即阻塞向客户端推字节。
func (s *rateSampler) checkDegraded() {
	sample, ok := s.degradedSample()
	if !ok {
		return
	}
	s.emit(sample)
	if s.config.OnDegraded != nil {
		s.config.OnDegraded(sample)
	}
}

// degradedSample 在锁内判定是否掉速；成立时置位并返回一个待发射的样本。
func (s *rateSampler) degradedSample() (RateSample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.started || s.config.Shadow || s.config.Rate <= 0 || s.degraded {
		return RateSample{}, false
	}
	elapsed := s.now().Sub(s.startedAt)
	// 二级闸自**末档**之后才开始判：那三档本身就是判据，不应与它重叠。
	if elapsed < s.verdictDeadline() {
		return RateSample{}, false
	}
	window := s.config.Window
	if window <= 0 {
		window = DefaultPostCommitRateWindow
	}
	now := s.now()
	if s.windowFrom.IsZero() {
		s.windowFrom = now
		s.windowBase = s.payload
		return RateSample{}, false
	}
	windowElapsed := now.Sub(s.windowFrom)
	if windowElapsed < window {
		return RateSample{}, false
	}
	windowBytes := s.payload - s.windowBase
	bytesPerSecond := rateOf(windowBytes, windowElapsed)
	// 重置窗口：无论是否判掉速都从此刻重新计，避免用陈旧累计值反复判定。
	s.windowFrom = now
	s.windowBase = s.payload
	if bytesPerSecond >= s.config.Rate {
		return RateSample{}, false
	}
	s.degraded = true
	sample := s.baseSample()
	sample.Stage = -1
	sample.ElapsedMS = int(elapsed.Milliseconds())
	sample.EmitElapsedMS = sample.ElapsedMS
	sample.PayloadBytes = windowBytes
	sample.BytesPerSecond = bytesPerSecond
	sample.Threshold = s.config.Rate
	sample.Verdict = "slow"
	sample.Degraded = true
	return sample, true
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
//
// 关于 `verdict` 字段：终值样本（event = rateStreamEndEvent，Stage = StageStreamEnd）用它报
// **结束原因**。标定脚本按同一字段名取值即可，而 Stage 已足以区分样本种类
// （0/1/2 = 三档、-1 = 二级闸降速、-2 = 流结束终值），故不另开 `ended` 字段——
// 多一个只在一种事件上出现的字段，只会多一处可缺省。
func (s *rateSampler) emit(sample RateSample) {
	if s.config.Logger != nil {
		event := rateSampleEvent
		switch {
		case sample.Degraded:
			event = rateDegradationEvent
		case sample.Ended:
			event = rateStreamEndEvent
		}
		s.config.Logger.Info(event, map[string]any{
			"providerId":     sample.ProviderID,
			"providerName":   sample.ProviderName,
			"stage":          sample.Stage,
			"elapsedMs":      sample.ElapsedMS,
			"emitElapsedMs":  sample.EmitElapsedMS,
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

// afterFunc 安排一次到期回调；未注入时用真定时器。
func (s *rateSampler) afterFunc(delay time.Duration, fire func()) rateTimer {
	if s.config.AfterFunc != nil {
		return s.config.AfterFunc(delay, fire)
	}
	return time.AfterFunc(delay, fire)
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
