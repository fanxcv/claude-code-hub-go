package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Mode 是门控模式（TS 的 StreamGateMode）。模式的来源（系统设置快照优先、环境变量兜底）
// 由接线层解析，本包只认字面量。
type Mode string

const (
	// ModeOff 关闭门控：直接透传。
	ModeOff Mode = "off"
	// ModeShadow 只做旁路诊断，不阻断（见 NewShadowObserver）。
	ModeShadow Mode = "shadow"
	// ModeEnforce 强制执行门控（Run 的严格模式）。
	ModeEnforce Mode = "enforce"
)

// ParseMode 解析门控模式字面量。
func ParseMode(value string) (Mode, bool) {
	switch Mode(value) {
	case ModeOff:
		return ModeOff, true
	case ModeShadow:
		return ModeShadow, true
	case ModeEnforce:
		return ModeEnforce, true
	default:
		return "", false
	}
}

// readChunkBytes 是单次读取的缓冲区大小。
const readChunkBytes = 32 << 10

// prebufferReservationMultiplier 与 TS 的 PREBUFFER_MEMORY_RESERVATION_MULTIPLIER 一致：
// 读取期间需覆盖 parser 保留状态、输入副本与前缀的最坏峰值。
const prebufferReservationMultiplier = 4

// ErrInvalidOptions 表示门控选项缺了必要上限（前缀字节上限与事件上限必须为正）。
var ErrInvalidOptions = errors.New("stream gate requires positive prebuffer caps")

// errIdleTimeout 是内部信号：转化为 PrecommitError(FailIdleTimeout) 后即不再外泄。
var errIdleTimeout = errors.New("stream gate read idle timeout")

// FailReason 是门控 precommit 失败原因（TS 的 StreamGateFailureReason）。
type FailReason string

const (
	// FailGateError 表示上游在提交前发出错误帧（fake-200 或流中 error）。
	FailGateError FailReason = "gate_error"
	// FailDecodeError 表示 precommit 帧载荷损坏（非 JSON / 非对象）。
	FailDecodeError FailReason = "decode_error"
	// FailEmptyStream 表示没有有效内容就结束：终止帧先于内容，或无终止帧的 EOF（上游断流）。
	FailEmptyStream FailReason = "empty_stream"
	// FailPrebufferOverflow 表示中性帧洪泛越过事件或字节上限。
	FailPrebufferOverflow FailReason = "prebuffer_overflow"
	// FailIdleTimeout 表示门控等待期读间隔超过静默上限。
	FailIdleTimeout FailReason = "idle_timeout"
	// FailSlowProbe 表示首字节已到，但自首字节起的中途探测阈值内仍未提交内容。
	//
	// 与 FailIdleTimeout 的区别是分母：那条看**读间隔**（上游一个字节都不发就会命中），
	// 本条看**自首个非空 chunk 起**的时长（上游即使一直在发中性帧也会命中）。两者都可
	// 同时成立，谁先到期即归因给谁。
	FailSlowProbe FailReason = "slow_probe"
	// FailSlowRate 表示首字已到、也有语义内容，但自首个语义内容帧起的产出速率达不到分级
	// 速率闸的阈值（见 progress.go 的三档检查点）。
	//
	// 与 FailSlowProbe 的区别是判据：那条只判「T 秒内零内容」（停滞），本条目判「有内容但
	// 慢」（速率）。上游每隔几秒吐一两个 token 时不触发停滞探测，却正是本条目要抓的形态。
	FailSlowRate FailReason = "slow_rate"
)

// PrecommitError 是门控 precommit 失败。
//
// 与 TS 的差异：不携带 HTTP 状态码——TS 侧会用错误规则把上游错误文本推断成 4xx/5xx
// （ProxyError + inferUpstreamErrorStatusCodeFromText），那是错误规则层的职责。
type PrecommitError struct {
	Reason                FailReason
	Family                Family
	ProviderID            int
	ProviderName          string
	FrameData             string
	FramesSeen            int
	BufferedBytes         int
	EchoExcludedBytes     int
	TerminalBeforeContent bool
	// SlowPayloadBytes / SlowElapsedMS 只在 Reason == FailSlowRate 时有意义：
	// 判慢当时的累计语义 payload 字节数与自首个语义内容帧起的时长。两者相除即实测速率，
	// 是标定阈值与事后复盘的唯一直接证据（不靠推算）。
	SlowPayloadBytes int
	SlowElapsedMS    int
}

func (e *PrecommitError) Error() string {
	return fmt.Sprintf("Stream content gate rejected upstream before first valid content (%s)", e.Reason)
}

// GateErrorBody 构造失败响应体：gate_error 直接返回上游错误帧原文（截断到 2000 字节），
// 让错误规则 / 人工排查看到真实上游错误；其余原因返回结构化诊断体。
func GateErrorBody(e *PrecommitError) string {
	if e.Reason == FailGateError && e.FrameData != "" {
		if len(e.FrameData) > 2000 {
			return e.FrameData[:2000]
		}
		return e.FrameData
	}
	var body struct {
		Error struct {
			Type                  string     `json:"type"`
			Reason                FailReason `json:"reason"`
			Family                Family     `json:"family"`
			FramesSeen            int        `json:"frames_seen"`
			BufferedBytes         int        `json:"buffered_bytes"`
			EchoExcludedBytes     int        `json:"echo_excluded_bytes,omitempty"`
			TerminalBeforeContent *bool      `json:"terminal_before_content,omitempty"`
			FramePreview          string     `json:"frame_preview,omitempty"`
			SlowPayloadBytes      int        `json:"slow_payload_bytes,omitempty"`
			SlowElapsedMS         int        `json:"slow_elapsed_ms,omitempty"`
		} `json:"error"`
	}
	body.Error.Type = "stream_gate_precommit"
	body.Error.Reason = e.Reason
	body.Error.Family = e.Family
	body.Error.FramesSeen = e.FramesSeen
	body.Error.BufferedBytes = e.BufferedBytes
	body.Error.EchoExcludedBytes = e.EchoExcludedBytes
	body.Error.SlowPayloadBytes = e.SlowPayloadBytes
	body.Error.SlowElapsedMS = e.SlowElapsedMS
	if e.Reason == FailEmptyStream {
		terminalBeforeContent := e.TerminalBeforeContent
		body.Error.TerminalBeforeContent = &terminalBeforeContent
	}
	if e.FrameData != "" {
		preview := e.FrameData
		if len(preview) > 500 {
			preview = preview[:500]
		}
		body.Error.FramePreview = preview
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		// 结构体字段全部可直接编码，这里不可达。
		return ""
	}
	return string(encoded)
}

// IsRequestScopedGateFailure 判定该失败是否属于**请求作用域**：不是供应商健康信号，
// 不应计入熔断器。
//
// 仅限 openai-responses 家族的 empty_stream。该家族下上游会返回语法完整、语义为空的响应：
// response.output_text.done 带 text: ""、response.output_item.done 带 content[0].text: ""、
// response.completed 带 output: []。所有帧按 IsNonEmptyValue 判定均非内容，terminal 先于
// content 到达即空流。这种空是请求内容决定的（同一 body 在任何供应商、任何账号上都复现），
// 记成供应商失败会让一个「毒性请求」在客户端重试放大下打开健康供应商的熔断器。
// 仍然 failover（客户端确实拿不到可见内容），只是不计健康度。
//
// 必须同时满足 TerminalBeforeContent：empty_stream 也覆盖「上游断流 / 空 body」的 EOF 分支，
// 那是真实的供应商侧异常，必须继续计入熔断。
//
// 其余家族的 empty_stream 保持计入：anthropic / openai-chat / gemini 在正常空回复下仍会发出
// 内容帧（如 text_delta 的空串所在的 content_block 系列），只吐终止帧属于畸形流。
func IsRequestScopedGateFailure(err error) bool {
	var precommit *PrecommitError
	if !errors.As(err, &precommit) {
		return false
	}
	return precommit.Reason == FailEmptyStream &&
		precommit.Family == FamilyOpenAIResponses &&
		precommit.TerminalBeforeContent
}

// CommitMarker 是触发门控提交的帧/chunk 标记（用于请求详情可观测性）。
type CommitMarker struct {
	// FrameIndex 是触发提交的帧序号（1-based，含中性前缀帧）。
	FrameIndex int
	// ChunkIndex 是触发提交的帧所在网络 chunk 序号（1-based）。
	ChunkIndex int
	// EventName 是触发提交的 SSE event 名；无事件行时为空串。
	EventName string
	// BufferedBytes 是提交时已缓冲的前缀字节数。
	BufferedBytes int
	// EchoExcludedBytes 是被排除出字节计数的请求回显帧字节数。
	EchoExcludedBytes int
	// LadderStage 是分级速率闸放开本次提交的档位（0/1/2）；未启用时为 -1。
	LadderStage int
	// LadderPayloadBytes 是提交时累计的语义 payload 字节数（未启用时为 0）。
	LadderPayloadBytes int
}

// Options 是门控参数（TS 的 StreamGateOptions）。
type Options struct {
	// Family 必须是四家族之一。调用方在 MapProviderTypeToFamily 返回 ok=false 时必须跳过
	// 门控（fail-open）：未知家族会让所有帧判为中性并最终 prebuffer_overflow。
	Family       Family
	ProviderID   int
	ProviderName string
	// PrebufferEventCap 是 precommit 期间允许的中性帧数上限。
	PrebufferEventCap int
	// PrebufferByteCap 是 precommit 前缀的字节上限（请求回显帧可放宽到 2 倍）。
	PrebufferByteCap int
	// OnFirstByte 在首个非空上游 chunk 到达时回调一次（调用方用于清除首字节计时器，
	// 保持「首字节」而非「首内容」语义）。
	OnFirstByte func()
	// IdleTimeout 是门控等待期的读间隔静默上限（<=0 表示不启用），
	// 对齐提交后响应处理阶段的静默超时。
	IdleTimeout time.Duration
	// ProbeAfterFirstByte 是自首个非空 chunk 起算的中途探测阈值（<=0 表示不启用）。
	//
	// 到期仍未提交内容即判 FailSlowProbe（首字后低速探测的换家判据）。它与 IdleTimeout
	// 是两条独立的分母，取**较小者**作为本次读的超时，故先到期的那个决定归因。
	ProbeAfterFirstByte time.Duration
	// CaptureCommitMarker 决定是否记录触发提交的帧信息（高并发模式下可关闭以省开销）。
	CaptureCommitMarker bool
	// PrecommitRate 是分级速率闸的阈值 θ（语义字节/秒）；<=0 表示不启用。
	//
	// 启用后提交点后移：首个语义内容帧不再立即提交，而是暂存内容按三档检查点裁决
	// （见 progress.go）。判慢即返回 FailSlowRate，此时客户端仍零字节。
	PrecommitRate int
	// ladderStages 覆盖速率闸的检查点序列，**仅供包内单测**缩短检查点用；
	// nil 时用出厂三档（1s/3s/10s）。生产路径不设。
	ladderStages []ladderStage
	// Budget 是进程级共享前缀预算；生产路径必须传入，单元测试可省略。
	Budget *Budget
	// OnBudgetWaitStart 在开始等待本地预算时回调（竞速路径用它暂停本地 hedge 阈值）。
	OnBudgetWaitStart func()
	// OnBudgetWaitEnd 在预算获得或等待失败后回调。
	OnBudgetWaitEnd func()
}

// Result 是门控提交结果。所有权约定与 TS 一致：提交后上游正文与租约一并交给调用方。
type Result struct {
	// Prefix 是待透传的前缀字节块（含触发提交的 content 帧所在块）；提交后由调用方消费。
	Prefix [][]byte
	// FramesSeen 是门控期间分类过的帧数（含中性前缀帧）。
	FramesSeen int
	// ReaderDone 表示门控期间已读到上游 EOF（前缀即全部响应正文）。
	ReaderDone bool
	// Marker 仅在 CaptureCommitMarker 为真时非空。
	Marker *CommitMarker
	// Lease 是提交后仅覆盖实际前缀占用的预算租约；调用方消费完前缀后必须 Release。
	Lease *Lease
	// Continuation 仅在提交时仍有一次「已从 source 取出但未交付」的在飞读时非 nil：
	// 调用方须改读它（先交付那批在飞字节，再透传 source），否则那批字节会被丢弃。
	Continuation io.Reader
}

// PrefixBytes 返回拼接后的前缀（无前缀时为 nil）。
func (r Result) PrefixBytes() []byte {
	return ConcatPrefix(r.Prefix)
}

// Run 对上游正文执行首个有效内容门控（TS 的 runStreamContentGate）。
//
// 提交时返回缓冲前缀与帧计数，上游正文所有权归还调用方（ReaderDone 为 false 时后续字节
// 仍在 source 上）。失败返回 *PrecommitError；source 的读错误原样上抛，由调用方按来源归类。
//
// 失败或取消时本函数不关闭也不排空 source（TS 语义：正文所有权始终在调用方）。
// 启用 IdleTimeout 时，超时后必须由调用方关闭或取消 source，以释放挂起的读 goroutine；
// ctx 取消亦会释放它。
func Run(ctx context.Context, source io.Reader, opts Options) (Result, error) {
	if opts.PrebufferByteCap <= 0 || opts.PrebufferEventCap <= 0 {
		return Result{}, ErrInvalidOptions
	}

	parser := NewParser(ParserOptions{
		MaxBufferedBytes: opts.PrebufferByteCap,
		Exemption: &BufferLimitExemption{
			MaxBufferedBytes: opts.PrebufferByteCap * 2,
			Matches: func(event string, dataHead string) bool {
				return IsRequestEchoFrame(opts.Family, event, dataHead)
			},
		},
	})

	var chunks [][]byte
	bufferedBytes := 0
	echoExcludedBytes := 0
	framesSeen := 0
	chunkIndex := 0
	firstByteSeen := false
	// firstByteAt 是首个非空 chunk 的到达时刻（零值表示尚未到达），探测阈值的起点。
	var firstByteAt time.Time
	var lease *Lease
	leaseTransferred := false
	// pending 跨循环迭代保留挂起的那次读（见 pendingRead 的注释：检查点唤醒不是终态）。
	pending := newPendingRead(source)

	// 分级速率闸：θ<=0 时为 nil，全部分支退回「首个内容帧即提交」的既有语义。
	ladder := newLadderWithStages(opts.PrecommitRate, opts.ladderStages)
	// ladderReleasedStage 记录是第几档放开提交的（-1 表示不是速率闸放开的），仅供提交标记。
	ladderReleasedStage := -1

	defer func() {
		if leaseTransferred {
			return
		}
		chunks = nil
		if lease != nil {
			lease.Release()
		}
	}()

	fail := func(reason FailReason, frameData string, terminalBeforeContent bool) (Result, error) {
		return Result{}, &PrecommitError{
			Reason:                reason,
			Family:                opts.Family,
			ProviderID:            opts.ProviderID,
			ProviderName:          opts.ProviderName,
			FrameData:             frameData,
			FramesSeen:            framesSeen,
			BufferedBytes:         bufferedBytes,
			EchoExcludedBytes:     echoExcludedBytes,
			TerminalBeforeContent: terminalBeforeContent,
		}
	}

	commit := func(event string, readerDone bool) (Result, error) {
		retainedBytes := 0
		for _, chunk := range chunks {
			retainedBytes += len(chunk)
		}
		// 读取期间需要覆盖 parser、输入副本和前缀的最坏峰值；提交后 parser 已停止，
		// 租约只需覆盖仍挂在下游 pending slot 中的实际前缀字节。
		if lease != nil {
			_ = lease.ShrinkTo(retainedBytes)
		}
		leaseTransferred = true
		result := Result{
			Prefix:     chunks,
			FramesSeen: framesSeen,
			ReaderDone: readerDone,
			Lease:      lease,
		}
		if !readerDone && pending.inFlight() {
			// 检查点裁决路径上可能有一次读已从 source 取走字节但尚未交付；随结果交给
			// 调用方续读，否则那批字节会凭空消失。
			result.Continuation = pending
		}
		if opts.CaptureCommitMarker {
			result.Marker = &CommitMarker{
				FrameIndex:         framesSeen,
				ChunkIndex:         chunkIndex,
				EventName:          event,
				BufferedBytes:      bufferedBytes,
				EchoExcludedBytes:  echoExcludedBytes,
				LadderStage:        ladderReleasedStage,
				LadderPayloadBytes: ladder.PayloadBytes(),
			}
		}
		return result, nil
	}

	// failRate 把速率闸的「判慢」落成 PrecommitError，并把实测速率写进报文。
	failRate := func() (Result, error) {
		return Result{}, &PrecommitError{
			Reason:            FailSlowRate,
			Family:            opts.Family,
			ProviderID:        opts.ProviderID,
			ProviderName:      opts.ProviderName,
			FramesSeen:        framesSeen,
			BufferedBytes:     bufferedBytes,
			EchoExcludedBytes: echoExcludedBytes,
			SlowPayloadBytes:  ladder.PayloadBytes(),
			SlowElapsedMS:     int(time.Since(ladder.StartedAt()).Milliseconds()),
		}
	}

	// decideLadder 在检查点到期时裁决；未启用、未启动或未到期返回 nil（继续缓冲）。
	//
	// 「未启动」的判据是首个**语义内容帧**而非首个非空字节：只有中性头帧 / 心跳时
	// 不启动时钟，否则「先发 3 秒心跳再正常吐字」会被算成低速。
	decideLadder := func(now time.Time) *gateOutcome {
		if !ladder.Enabled() || !ladder.Started() {
			return nil
		}
		switch ladder.Evaluate(now) {
		case LadderCommit:
			ladderReleasedStage = ladder.Stage()
			return newOutcome(commit("", false))
		case LadderSlow:
			return newOutcome(failRate())
		default:
			return nil
		}
	}

	// 豁免额度以 cap 为自身上限：伪装成回显的中性帧最多把缓冲总量抬到 2×cap。
	exceedsByteCap := func() bool {
		excluded := echoExcludedBytes
		if excluded > opts.PrebufferByteCap {
			excluded = opts.PrebufferByteCap
		}
		return bufferedBytes-excluded > opts.PrebufferByteCap
	}

	if opts.Budget != nil {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		reservationBytes := opts.PrebufferByteCap * prebufferReservationMultiplier
		snapshot := opts.Budget.Snapshot()
		// snapshot 与 Reserve 之间没有异步边界；只在本次调用确实会进入 FIFO 队列时
		// 暂停供应商计时，避免正常热路径反复清除并重建 timer。
		waitsForBudget := reservationBytes <= snapshot.Limit &&
			(snapshot.Waiting > 0 || snapshot.ReservedBytes+reservationBytes > snapshot.Limit)
		if waitsForBudget && opts.OnBudgetWaitStart != nil {
			opts.OnBudgetWaitStart()
		}
		acquired, err := opts.Budget.Reserve(ctx, reservationBytes)
		if waitsForBudget && opts.OnBudgetWaitEnd != nil {
			opts.OnBudgetWaitEnd()
		}
		if err != nil {
			return Result{}, err
		}
		lease = acquired
	}

	buffer := make([]byte, readChunkBytes)
	for {
		// 速率闸的检查点早于任何读超时：先裁决，不依赖「上游恰好又发了字节」才推进。
		// 否则一条彻底停住的上游会既不提交也不判慢，直到探测/静默超时兜底。
		if decided := decideLadder(time.Now()); decided != nil {
			return decided.result, decided.err
		}
		// 三条超时取较小者：探测阈值（自首字节起）、速率闸下一档检查点、读间隔静默上限。
		// 哪个到期即归因给哪个，故这里要记住「本次用的超时是否由探测阈值决定」。
		readTimeout, probeDecidesTimeout := probeReadTimeout(opts, firstByteAt, time.Now())
		ladderWakes := false
		if deadline, ok := ladder.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				// 正常路径已在循环顶部裁决；走到这里只可能是时钟跳变，用极小超时兜底。
				remaining = time.Nanosecond
			}
			if readTimeout <= 0 || remaining < readTimeout {
				readTimeout = remaining
				ladderWakes = true
				probeDecidesTimeout = false
			}
		}
		n, readErr := pending.take(ctx, buffer, readTimeout)
		if errors.Is(readErr, errIdleTimeout) {
			if ladderWakes {
				// 检查点唤醒不是失败：回循环顶部裁决（提交、判慢或进下一档）。
				continue
			}
			if probeDecidesTimeout {
				return fail(FailSlowProbe, "", false)
			}
			return fail(FailIdleTimeout, "", false)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			// 首字节超时 abort / 客户端断开 / 传输错误：原样上抛，调用方按来源归类
			return Result{}, readErr
		}

		if n > 0 {
			chunk := buffer[:n]
			if !firstByteSeen {
				firstByteSeen = true
				// 首字节时刻是探测阈值的起点（与 OnFirstByte 的「首字节而非首内容」语义同源）。
				firstByteAt = time.Now()
				if opts.OnFirstByte != nil {
					opts.OnFirstByte()
				}
			}
			chunkIndex++
			// 回显豁免最多把门禁前缀抬到 2×cap。先检查再持有引用，避免一个超大网络
			// chunk 在帧分类和事后检查之前直接越过宣称的内存边界。
			if len(chunk) > opts.PrebufferByteCap*2-bufferedBytes {
				return fail(FailPrebufferOverflow, "", false)
			}
			owned := make([]byte, len(chunk))
			copy(owned, chunk)
			chunks = append(chunks, owned)
			bufferedBytes += len(owned)

			var decided *gateOutcome
			if _, err := parser.Visit(owned, func(frame Frame) bool {
				framesSeen++
				switch Classify(opts.Family, frame.Event, frame.Data) {
				case VerdictContent:
					if ladder.Enabled() {
						// 速率闸开启：先计量再裁决，本帧已在 chunks 里，故不提交也不丢字节。
						ladder.Observe(SemanticPayloadBytes(opts.Family, frame.Event, frame.Data), time.Now())
						if exceedsByteCap() {
							// 内容把前缀上限撞满⇒直接提交，而**不是**报 prebuffer_overflow。
							// 前缀上限的用途是挡中性帧洪泛（那仍然在上面的 neutral 分支报错），
							// 而能撞满上限的内容本身就证明这家不慢；在旧的「首内容帧即提交」
							// 语义下根本不会走到这里（那时早已提交），所以这里必须提交才不引入回归。
							decided = newOutcome(commit(frame.Event, false))
							return false
						}
						if decision := decideLadder(time.Now()); decision != nil {
							decided = decision
							return false
						}
						return true
					}
					if exceedsByteCap() {
						decided = newOutcome(fail(FailPrebufferOverflow, "", false))
					} else {
						decided = newOutcome(commit(frame.Event, false))
					}
					return false
				case VerdictError:
					decided = newOutcome(fail(FailGateError, frame.Data, false))
					return false
				case VerdictMalformed:
					decided = newOutcome(fail(FailDecodeError, frame.Data, false))
					return false
				case VerdictTerminal:
					if ladder.Started() {
						// 短响应豁免：已见语义内容却先到终止帧（总长不够阈值）⇒ 直接提交。
						if exceedsByteCap() {
							decided = newOutcome(fail(FailPrebufferOverflow, "", false))
						} else {
							decided = newOutcome(commit(frame.Event, false))
						}
						return false
					}
					if opts.Family == FamilyOpenAIResponses &&
						(IsCleanResponsesCompletion(frame.Event, frame.Data) ||
							IsResponsesIncompleteCompletion(frame.Event, frame.Data)) {
						if exceedsByteCap() {
							decided = newOutcome(fail(FailPrebufferOverflow, "", false))
						} else {
							decided = newOutcome(commit(frame.Event, false))
						}
					} else {
						decided = newOutcome(fail(FailEmptyStream, frame.Data, true))
					}
					return false
				default:
					// neutral：继续缓冲；请求回显帧的载荷不计入字节上限。
					if IsRequestEchoFrame(opts.Family, frame.Event, frame.Data) {
						echoExcludedBytes += len(frame.Data)
					}
					if framesSeen > opts.PrebufferEventCap {
						decided = newOutcome(fail(FailPrebufferOverflow, "", false))
						return false
					}
					return true
				}
			}); err != nil {
				var limitErr *BufferLimitError
				if errors.As(err, &limitErr) {
					return fail(FailPrebufferOverflow, "", false)
				}
				return Result{}, err
			}
			if decided != nil {
				return decided.result, decided.err
			}
			if exceedsByteCap() {
				return fail(FailPrebufferOverflow, "", false)
			}
		}

		if readErr == nil {
			continue
		}

		// EOF：冲刷尾部未终止帧（无结尾空行的流），并判定空流。
		sawTerminal := false
		var trailing *gateOutcome
		if _, err := parser.FinishVisit(func(frame Frame) bool {
			framesSeen++
			switch Classify(opts.Family, frame.Event, frame.Data) {
			case VerdictContent:
				if ladder.Enabled() {
					// 上游在检查点前就结束：计量后直接提交（响应已完整，不再等档位）。
					ladder.Observe(SemanticPayloadBytes(opts.Family, frame.Event, frame.Data), time.Now())
				}
				trailing = newOutcome(commit(frame.Event, true))
				return false
			case VerdictError:
				trailing = newOutcome(fail(FailGateError, frame.Data, false))
				return false
			case VerdictMalformed:
				trailing = newOutcome(fail(FailDecodeError, frame.Data, false))
				return false
			case VerdictTerminal:
				if opts.Family == FamilyOpenAIResponses &&
					(IsCleanResponsesCompletion(frame.Event, frame.Data) ||
						IsResponsesIncompleteCompletion(frame.Event, frame.Data)) {
					trailing = newOutcome(commit(frame.Event, true))
					return false
				}
				sawTerminal = true
				return true
			default:
				if framesSeen > opts.PrebufferEventCap {
					trailing = newOutcome(fail(FailPrebufferOverflow, "", false))
					return false
				}
				return true
			}
		}); err != nil {
			var limitErr *BufferLimitError
			if errors.As(err, &limitErr) {
				return fail(FailPrebufferOverflow, "", false)
			}
			return Result{}, err
		}
		if trailing != nil {
			return trailing.result, trailing.err
		}
		if ladder.Started() {
			// 已见语义内容且上游结束（含断流）：与「提交点在首个内容帧」的既有行为等价——
			// 不再因为「终止帧没到」而把一段已有内容的响应判成空流。
			return commit("", true)
		}
		// 无终止帧的 EOF 是供应商断流；终止帧先于内容则是请求作用域空结果。
		return fail(FailEmptyStream, "", sawTerminal)
	}
}

type gateOutcome struct {
	result Result
	err    error
}

func newOutcome(result Result, err error) *gateOutcome {
	return &gateOutcome{result: result, err: err}
}

// probeReadTimeout 给出本次读应使用的超时，以及它是否由中途探测阈值决定。
//
// 挂起的那次读如何收尾归 pendingRead：本函数只决定等多久。
//
// 三个分支的理由：
//   - 探测未启用（阈值 <=0）或首字节未到：探测还没开始计时，只能用读间隔上限；
//   - 探测阈值剩余 <=0：已过期，用一个极小正超时让读立即以 errIdleTimeout 返回，
//     调用方据 probeDecidesTimeout 归因为 FailSlowProbe（不能传 0，那是「不启用超时」）；
//   - 两者都有：取较小者，并如实报告是谁决定了它。
func probeReadTimeout(opts Options, firstByteAt time.Time, now time.Time) (time.Duration, bool) {
	if opts.ProbeAfterFirstByte <= 0 || firstByteAt.IsZero() {
		return opts.IdleTimeout, false
	}
	remaining := opts.ProbeAfterFirstByte - now.Sub(firstByteAt)
	if remaining <= 0 {
		return time.Nanosecond, true
	}
	if opts.IdleTimeout > 0 && opts.IdleTimeout < remaining {
		return opts.IdleTimeout, false
	}
	return remaining, true
}

// pendingRead 持有「一次挂起的源读取」。
//
// 为什么不能每次超时都新起一个 goroutine 去读：分级速率闸把「超时」从**终态**变成
// 「回循环裁决」（检查点到期不提交也不失败）。若超时后丢弃那次读、下次另起一个，
// 被丢下的 goroutine 仍会继续从 source 取字节并注入它的私有缓冲——那些字节就凭空消失，
// 表现为上游恰好跨过检查点的那一帧内容丢失。故在这里只允许**一次**读在飞，结果经容量 1
// 的 channel 交付一次，超时保留待取，下一次取到的是同一次读的结果。
//
// channel 带缓冲是必需的：即使调用方已放弃（提前返回），投递也不会阻塞读 goroutine。
type pendingRead struct {
	source  io.Reader
	result  chan readOutcome
	started bool

	// buf 是在飞那次读写入的缓冲区；take 超时后仍要能从中取回已读出的字节。
	buf []byte
	// 未交付的在飞结果（take 超时、或结果已入 channel 但未取走），供 Read 续读。
	pendingN   int
	pendingErr error
	offset     int
}

type readOutcome struct {
	n   int
	err error
}

func newPendingRead(source io.Reader) *pendingRead {
	return &pendingRead{source: source, result: make(chan readOutcome, 1)}
}

// inFlight 报告是否还有一次读在飞（结果可能已入 channel 但尚未取走）。
func (p *pendingRead) inFlight() bool { return p.started }

// Read 是续读句柄：先交付在飞那次读已从 source 取走的字节，再透传 source 的后续读。
// 仅在 pendingRead 被当作 Result.Continuation 交给调用方后由下游调用。
func (p *pendingRead) Read(b []byte) (int, error) {
	if p.started {
		outcome := <-p.result
		p.started = false
		p.pendingN = outcome.n
		p.pendingErr = outcome.err
		p.offset = 0
	}
	if p.offset < p.pendingN {
		n := copy(b, p.buf[p.offset:p.pendingN])
		p.offset += n
		if p.offset < p.pendingN {
			return n, nil
		}
		err := p.pendingErr
		p.pendingN, p.pendingErr, p.offset = 0, nil, 0
		return n, err
	}
	if p.pendingErr != nil {
		err := p.pendingErr
		p.pendingErr = nil
		return 0, err
	}
	return p.source.Read(b)
}

// take 取一次读结果，超时返回 errIdleTimeout 并**保留**挂起的那次读。
// buffer 必须在整个 Run 期间是同一块（挂起的 goroutine 已持有它）。
func (p *pendingRead) take(ctx context.Context, buffer []byte, timeout time.Duration) (int, error) {
	if !p.started {
		p.started = true
		p.buf = buffer
		go func() {
			n, err := p.source.Read(buffer)
			p.result <- readOutcome{n: n, err: err}
		}()
	}
	deliver := func() (int, error) {
		outcome := <-p.result
		p.started = false
		return outcome.n, outcome.err
	}
	if timeout <= 0 {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		return deliver()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case outcome := <-p.result:
		p.started = false
		return outcome.n, outcome.err
	case <-timer.C:
		return 0, errIdleTimeout
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// ConcatPrefix 拼接门控前缀字节（TS 的 concatChunks）：空切片返回 nil，单块零拷贝。
func ConcatPrefix(chunks [][]byte) []byte {
	if len(chunks) == 0 {
		return nil
	}
	if len(chunks) == 1 {
		return chunks[0]
	}
	total := 0
	for _, chunk := range chunks {
		total += len(chunk)
	}
	out := make([]byte, 0, total)
	for _, chunk := range chunks {
		out = append(out, chunk...)
	}
	return out
}
