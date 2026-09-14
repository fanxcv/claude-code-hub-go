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
		} `json:"error"`
	}
	body.Error.Type = "stream_gate_precommit"
	body.Error.Reason = e.Reason
	body.Error.Family = e.Family
	body.Error.FramesSeen = e.FramesSeen
	body.Error.BufferedBytes = e.BufferedBytes
	body.Error.EchoExcludedBytes = e.EchoExcludedBytes
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
	// CaptureCommitMarker 决定是否记录触发提交的帧信息（高并发模式下可关闭以省开销）。
	CaptureCommitMarker bool
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
	var lease *Lease
	leaseTransferred := false

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
		if opts.CaptureCommitMarker {
			result.Marker = &CommitMarker{
				FrameIndex:        framesSeen,
				ChunkIndex:        chunkIndex,
				EventName:         event,
				BufferedBytes:     bufferedBytes,
				EchoExcludedBytes: echoExcludedBytes,
			}
		}
		return result, nil
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
		n, readErr := readWithIdleTimeout(ctx, source, buffer, opts.IdleTimeout)
		if errors.Is(readErr, errIdleTimeout) {
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

// readWithIdleTimeout 让单次读取与静默计时器竞速（TS 的 readWithIdleTimeout）。
//
// 计时器或 ctx 胜出时挂起的读由调用方随后的关闭/取消收尾：本函数不关闭 source。
func readWithIdleTimeout(
	ctx context.Context,
	source io.Reader,
	buffer []byte,
	idleTimeout time.Duration,
) (int, error) {
	if idleTimeout <= 0 {
		return source.Read(buffer)
	}
	type readResult struct {
		n   int
		err error
	}
	results := make(chan readResult, 1)
	go func() {
		n, err := source.Read(buffer)
		results <- readResult{n: n, err: err}
	}()
	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()
	select {
	case result := <-results:
		return result.n, result.err
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
