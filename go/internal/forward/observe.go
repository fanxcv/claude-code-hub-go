package forward

import (
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
)

// 本文件实现流式响应的增量终态观测（对应 Node response-handler.ts 的流式观测部分）。
//
// 核心不变量：每流的驻留量与正文长度无关。除有界头尾窗口外，本文件不保留任何正文——
// 正文一旦分类完就丢弃，只留下 O(1) 的计数与终态事实。usage 与模型名按帧解析，
// 每帧解析完即释放。
//
// 头尾窗口刻意小于 Node 的 10 MiB 上限（见 NodeStream* 常量）：Node 的上限服务于
// 「整条流的字节级快照」，而本进程要在 700 MiB 内跑 50 条并发流，按流保留 10 MiB
// 会吃掉全部预算。默认窗口只够调试工件用；需要与 Node 同口径的完整快照时，
// 由接线层把 HeadBytes/TailBytes 抬到 NodeStreamMaxBytes 一档。

const (
	// DefaultStreamHeadBytes 是默认头部窗口：正文开头（含首帧元信息）。
	DefaultStreamHeadBytes = 4 << 10
	// DefaultStreamTailBytes 是默认尾部窗口：终态帧与 usage 通常在这里。
	DefaultStreamTailBytes = 4 << 10
	// DefaultObserverParserBytes 是观测器分帧器的**常规**缓冲上限。
	//
	// 为何是 256 KiB 而不是 64 KiB（2026-09-13 真机实测）：对 `https://ollama.com/v1/responses`
	// 用真实客户端规模的请求体（213,713 B：instructions ~180 KB + 24 个带描述的 tools）抓取，
	// 上游响应的**终态帧** `response.completed` 实测 **213,635 B**——64 KiB 上限下它必然被丢，
	// 于是整条流的 usage 只能靠 4 KiB 尾窗去猜（那正是生产上「模型有值、用量为空」的形态）。
	DefaultObserverParserBytes = 256 << 10
	// DefaultObserverEchoFrameBytes 是**请求回显帧**的放宽上限（与门控同一判据）。
	//
	// 为何需要它：openai-responses 上游会在 `response.created` / `response.in_progress` 里
	// **回显完整请求体**（实测同一次抓取：213,327 B 与 213,331 B）。门控侧早就为此配了豁免
	// （`gate.go` 的 BufferLimitExemption + IsRequestEchoFrame），观测侧当时漏了，于是真实
	// 客户端（大系统提示 + 工具表）的**第一帧就超限** → parserLost 置位 → 整条流退化成窗口回退。
	//
	// 为何是 1 MiB 而不是照搬门控的 8 MiB：这是每流**可能**的额外驻留量（回显帧在 dispatch 后
	// 即释放）。1 MiB 对实测的 213 KB 有 5 倍余量，50 路并发最坏 50 MiB，在 700 MiB 预算内；
	// 照搬 8 MiB 则最坏 400 MiB，不可取。超过本上限的回显帧仍按溢出处理（退回窗口回退，
	// 事实依旧可恢复，只是不再是帧级）；若将来出现更大的客户端提示，抬这个常量即可。
	DefaultObserverEchoFrameBytes = 1 << 20
	// StreamTruncatedMarker 与 Node 的 STREAM_STATS_TRUNCATED_MARKER 一致。
	StreamTruncatedMarker = "\n\n: [cch_truncated]\n\n"
	// NodeStreamMaxBytes / NodeStreamHeadBytes 是 Node 侧快照窗口默认值，供接线层按 env 抬升。
	NodeStreamMaxBytes  = 10 << 20
	NodeStreamHeadBytes = 1 << 20
	// NodeStreamTailBytes 是 Node 侧尾部窗口（MAX - HEAD）。
	NodeStreamTailBytes = NodeStreamMaxBytes - NodeStreamHeadBytes
)

// TerminalKind 是流的终态类型。
type TerminalKind string

const (
	// TerminalEmpty 表示还没有任何终态事实。
	TerminalEmpty TerminalKind = ""
	// TerminalCompleted 表示上游发出协议终止标记，流完整结束。
	TerminalCompleted TerminalKind = "completed"
	// TerminalUpstreamError 表示流内出现错误帧（上游已在流中宣告失败）。
	TerminalUpstreamError TerminalKind = "upstream_error"
	// TerminalUpstreamTruncated 表示上游在协议终止标记之前断流。
	TerminalUpstreamTruncated TerminalKind = "upstream_truncated"
	// TerminalIdleTimeout 表示上游静默超过配置上限。
	TerminalIdleTimeout TerminalKind = "idle_timeout"
	// TerminalClientAborted 表示客户端先于上游终态断开。
	TerminalClientAborted TerminalKind = "client_aborted"
	// TerminalLocalError 表示本地读取/写入失败。
	TerminalLocalError TerminalKind = "local_error"
)

// Observation 是一次流式转发的终态观测结果。
type Observation struct {
	// Bytes / Chunks 是上游正文的字节数与 chunk 数（含门控前缀与引流期间读到的部分）。
	Bytes  int64
	Chunks int64
	// FirstByteAt 是首个非空 chunk 的到达时刻；零值表示未见到任何字节。
	FirstByteAt time.Time
	// TTFT 是首字节相对请求开始（ObservationOptions.StartedAt）的延迟。
	TTFT time.Duration
	// FirstByte 是**真 TTFB**：上游首个非空 chunk 相对请求开始的延迟
	// （ObservationOptions.UpstreamFirstByteAt 可得时用它；不可得时退化为 TTFT，
	// 与 Node 的 `markFirstByte` 兼作 TTFB 兑底同义）。
	//
	// 它是公开页 TPS 的分母起点，也是排行榜 tok/s 榜的必备列
	//（Node 的 `firstByteMs`）。
	FirstByte time.Duration
	// Model 是流内最后一次声明的模型名（协议转换生效时是供应商线口径）。
	Model string
	// Usage 是流内最后一次可解析的用量，枢纽口径（见 convert.Usage 的注释）。
	Usage convert.Usage
	// UsageSeen 为真表示至少解析到一次用量（含由头尾窗口回退补回的那一次，见 recoverFromWindows）。
	//
	// 结算层只认这个标志（dataplane 的 `if observation.UsageSeen` 才写用量列），故任何
	// 「填了 Usage 却没置位」的路径都等于把用量丢掉。
	UsageSeen bool
	// Frames 是分类过的帧数（SSE 帧或 NDJSON 行）。
	Frames int64
	// CompletionMarker 为真表示见到了与协议族匹配的终止标记。
	CompletionMarker bool
	// SawIncomplete 为真表示见到 responses 家族的 response.incomplete（语义未完成）。
	SawIncomplete bool
	// StopReason 是流内最后一次声明的停止原因（原样保留各线取值）。
	StopReason string
	// ErrorText 是流内错误帧的错误文案（截断）。
	ErrorText string
	// Truncated 为真表示正文超出有界窗口，窗口只覆盖首尾两段。
	Truncated bool
	// BufferOverflow 为真表示分帧器曾因缓冲上限丢弃帧（超长单行）。
	//
	// 为何要暴露它：这种情况下的 model/usage 只能靠头尾窗口回退补回，而回退是有条件的
	// （见 recoverFromWindows），一旦失效就会静默少记用量与计费。生产上「同一供应商成批行
	// 没用量、而模型名有值」就是这个形状（见 usage-capture-ollama-codex.md），
	// 故结算层需要能区分「上游真的没报」与「我们没接住」。
	BufferOverflow bool
	// Snapshot 是有界头尾窗口的文本；Truncated 为真时中间带截断标记。
	//
	// Node 语义：截断快照不足以重建完整终态，调试工件落盘前必须跳过（由调用方判断）。
	Snapshot string
	// RetainedBytes 是本观测实际驻留的字节数（窗口 + 解析缓冲），用于证明驻留量有界。
	RetainedBytes int

	// Kind / Err 由 Stream 在终态时补上（观测本身不知道客户端与超时事实）。
	Kind TerminalKind
	Err  error
}

// ObservationOptions 描述一次观测。
type ObservationOptions struct {
	// StartedAt 是请求开始时刻，用于计算 TTFT 与 FirstByte。
	StartedAt time.Time
	// UpstreamFirstByteAt 是**上游首个非空 chunk 的到达时刻**（门控已在上游侧替我们
	// 抓到这个时刻：`gate.Options.OnFirstByte`，其语义就是「首字节而非首内容」）。
	//
	// 为何要单独一个字段：门控前缀是在**提交后**才喂进观测器的（见 forward/stream.go），
	// 若拿喂入时刻当首字节，非流式以外所有门控流的 FirstByte 都会退化成提交时刻，
	// 从而把 TPS 的生成窗口算小（Node 的 firstByteMs 取的就是上游侧时刻）。
	// 零值表示门控未见到字节（如未走门控的路径），此时 FirstByte 退化成 TTFT。
	UpstreamFirstByteAt time.Time
	// Format 是客户端入站格式，决定终止标记与用量字段的方言。
	Format convert.ClientFormat
	// HeadBytes / TailBytes 是有界窗口容量；0 取默认（默认刻意小，见本文件头注释）。
	HeadBytes int
	TailBytes int
	// ParserBytes / EchoFrameBytes 是分帧器的常规上限与请求回显豁免上限；
	// 0 分别取 DefaultObserverParserBytes / DefaultObserverEchoFrameBytes。
	//
	// 为何要可注入：上限直接决定「帧路径 vs 窗口回退」这条分水岭（见两个常量的注释），
	// 而各部署的内存预算不同——装配层需要能按自己的预算抬升，而不必改本包。
	ParserBytes    int
	EchoFrameBytes int
	// Now 可注入时钟；nil 用 time.Now。
	Now func() time.Time
}

// Observer 是流式正文的增量观测器。除窗口外不保留正文，状态为 O(1)。
type Observer struct {
	opts ObservationOptions

	// head 与 tail 都是定容缓冲：容量在构造时分配，之后不再增长。
	head     []byte
	tail     []byte
	headFull bool
	tailPos  int
	tailFull bool
	total    int64

	parser *gate.Parser
	// parserBound 是分帧器保留缓冲的上限：驻留量统计把它按上限计入，
	// 宁可多报也不少报（真实占用在 0 与该上限之间）。
	parserBound int

	// parserLost 记录「分帧器曾因缓冲上限丢弃帧」。置位后由 Snapshot 走窗口回退：
	// 单行流被整行丢弃时，其 model 在行首、usage 在行尾，恰好落在本观测器已经保留的头尾窗口里。
	parserLost bool

	observation Observation
}

// NewObserver 构造观测器。
func NewObserver(opts ObservationOptions) *Observer {
	if opts.HeadBytes <= 0 {
		opts.HeadBytes = DefaultStreamHeadBytes
	}
	if opts.TailBytes <= 0 {
		opts.TailBytes = DefaultStreamTailBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	// 常规上限与回显豁免上限：两者都可由调用方注入（零值取默认），便于装配层按预算抬升。
	parserBound := opts.ParserBytes
	if parserBound <= 0 {
		parserBound = DefaultObserverParserBytes
	}
	echoFrameBound := opts.EchoFrameBytes
	if echoFrameBound <= 0 {
		echoFrameBound = DefaultObserverEchoFrameBytes
	}
	if echoFrameBound < parserBound {
		// 豁免上限不得低于常规上限（否则豁免帧反而比普通帧更容易报错）。
		echoFrameBound = parserBound
	}
	return &Observer{
		opts:        opts,
		head:        make([]byte, 0, opts.HeadBytes),
		tail:        make([]byte, opts.TailBytes),
		parserBound: parserBound,
		parser: gate.NewParser(gate.ParserOptions{
			MaxBufferedBytes: parserBound,
			// 与门控同判据的请求回显豁免：否则真实客户端的第一帧（回显完整请求体，
			// 实测 213 KB）就会被当成缓冲溢出，整条流退化成 4 KiB 窗口猜测。
			Exemption: &gate.BufferLimitExemption{
				MaxBufferedBytes: echoFrameBound,
				Matches: func(event string, dataHead string) bool {
					return gate.IsRequestEchoFrame(echoFamilyOf(opts.Format), event, dataHead)
				},
			},
		}),
	}
}

// echoFamilyOf 把观测器的方言映射为门控家族，用于请求回显帧判定。
//
// 已知家族才返回可命中的家族；未知方言返回空串（IsRequestEchoFrame 对未知家族恒为 false，
// 于是回退到「没有豁免」的保守行为）。
func echoFamilyOf(format convert.ClientFormat) gate.Family {
	switch format {
	case convert.FormatResponse:
		return gate.FamilyOpenAIResponses
	case convert.FormatClaude:
		return gate.FamilyAnthropic
	case convert.FormatOpenAI:
		return gate.FamilyOpenAIChat
	case convert.FormatGemini, convert.FormatGeminiCLI:
		return gate.FamilyGemini
	default:
		return ""
	}
}

// parserBound 是分帧器缓冲上限。
//
// **不得为「长行」抬高此值**：包内不变量是「每流驻留量与正文长度无关」，由
// TestObserverResidencyStaysBoundedForLargeStream 守着；抬高它会让该值进入
// `RetainedBytes` 并随窗口一起超限。超长行的补救走窗口回退（见 recoverFromWindows）。
func parserBoundOf(opts ObservationOptions) int {
	return maxInt(opts.HeadBytes, 64<<10)
}

// Push 吃入一个上游 chunk：更新计数、窗口与帧级事实。
func (o *Observer) Push(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	if o.observation.FirstByteAt.IsZero() {
		o.observation.FirstByteAt = o.opts.Now()
	}
	o.observation.Bytes += int64(len(chunk))
	o.observation.Chunks++
	o.appendWindow(chunk)
	o.consumeFrames(chunk)
}

// Snapshot 返回当前观测快照（不改状态，可多次调用）。
func (o *Observer) Snapshot() Observation {
	snapshot := o.observation
	snapshot.Truncated = o.truncated()
	snapshot.BufferOverflow = o.parserLost
	snapshot.Snapshot = o.windowText()
	snapshot.RetainedBytes = len(o.head) + len(o.tail) + o.parserBound
	if o.parserLost {
		o.recoverFromWindows(&snapshot)
	}
	if !snapshot.FirstByteAt.IsZero() {
		snapshot.TTFT = snapshot.FirstByteAt.Sub(o.opts.StartedAt)
	}
	// FirstByte 优先取上游侧时刻（门控的 OnFirstByte）；拿不到时退化为 TTFT，
	// 与 Node 的 `markFirstByte` 兼作 TTFB 兑底一致。
	snapshot.FirstByte = snapshot.TTFT
	if !o.opts.UpstreamFirstByteAt.IsZero() {
		snapshot.FirstByte = o.opts.UpstreamFirstByteAt.Sub(o.opts.StartedAt)
	}
	return snapshot
}

// appendWindow 把 chunk 装入定容头尾窗口：头部先填满，其后只滚动保留最后 TailBytes 字节。
func (o *Observer) appendWindow(chunk []byte) {
	o.total += int64(len(chunk))
	if !o.headFull {
		remaining := o.opts.HeadBytes - len(o.head)
		if remaining >= len(chunk) {
			o.head = append(o.head, chunk...)
			return
		}
		o.head = append(o.head, chunk[:remaining]...)
		o.headFull = true
		chunk = chunk[remaining:]
	}
	o.rollTail(chunk)
}

// rollTail 把字节写入环形尾窗，覆盖最旧字节。
func (o *Observer) rollTail(chunk []byte) {
	if len(o.tail) == 0 {
		return
	}
	if len(chunk) >= len(o.tail) {
		// 单个 chunk 就超过尾窗容量：只保留最后 len(tail) 字节。
		chunk = chunk[len(chunk)-len(o.tail):]
		copy(o.tail, chunk)
		o.tailPos = 0
		o.tailFull = true
		return
	}
	for len(chunk) > 0 {
		n := copy(o.tail[o.tailPos:], chunk)
		o.tailPos = (o.tailPos + n) % len(o.tail)
		chunk = chunk[n:]
		if o.tailPos == 0 {
			o.tailFull = true
		}
	}
}

// truncated 报告是否有正文字节落在窗口之外。
func (o *Observer) truncated() bool {
	return o.total > int64(len(o.head))+int64(o.tailLen())
}

// tailLen 返回尾窗当前有效字节数。
func (o *Observer) tailLen() int {
	if o.tailFull {
		return len(o.tail)
	}
	return o.tailPos
}

// windowText 拼接头尾窗口；中间被丢弃时插入截断标记。
func (o *Observer) windowText() string {
	if o.total == 0 {
		return ""
	}
	headText := string(o.head)
	if !o.truncated() {
		// 未截断：尾窗里的内容紧接头部（此时它还没绕回）。
		return headText + string(o.tail[:o.tailLen()])
	}
	var builder strings.Builder
	builder.Grow(len(headText) + len(StreamTruncatedMarker) + len(o.tail))
	builder.WriteString(headText)
	builder.WriteString(StreamTruncatedMarker)
	builder.Write(o.tailOrdered())
	return builder.String()
}

// tailOrdered 按时间顺序返回尾窗内容。
func (o *Observer) tailOrdered() []byte {
	length := o.tailLen()
	if length == 0 {
		return nil
	}
	if !o.tailFull {
		return o.tail[:o.tailPos]
	}
	ordered := make([]byte, 0, length)
	ordered = append(ordered, o.tail[o.tailPos:]...)
	ordered = append(ordered, o.tail[:o.tailPos]...)
	return ordered
}

// consumeFrames 用门控的 SSE 解析器增量切帧并提取终态事实。
//
// 解析失败（损坏帧、超缓冲）不改判终态：观测只做加法，判定交给门控与终态路径。
// 但超缓冲会**丢帧**，必须置位 parserLost —— 否则「上游把整段回答作为一条超长行」的上游
// （实测 ollama.com）会静默地一条事实都抽不到：生产实测同一供应商短请求正常、大上下文请求
// 97% 的落库行没有 usage/actual_response_model，而同批 claude 线（无长行）零缺失。
func (o *Observer) consumeFrames(chunk []byte) {
	// 用 Visit 而不是 Push：**Push 在报错时会把同一 chunk 里已经解析出来的帧一并丢弃**
	// （帧收在局部切片里，`return nil, err` 即全部消失），而 `consumeFrames` 出错又直接 return，
	// 于是「超限帧之后/之前的正常帧」——包括带 usage 的终态帧——会随错误一起不见。
	// 生产实测过一次真实损失：同一 chunk 里的终态帧因后续出现超限行而彻底丢失
	// （窗口回退也取不回来，因为 usage 已落在窗口之外）。Visit 边解析边交付，
	// 错误之前的帧仍然进入观测。
	if _, err := o.parser.Visit(chunk, func(frame gate.Frame) bool {
		o.observeFrame(frame)
		return true
	}); err != nil {
		o.parserLost = true
	}
}

func (o *Observer) observeFrame(frame gate.Frame) {
	o.observation.Frames++
	data := strings.TrimSpace(frame.Data)
	if data == "" {
		return
	}
	if data == "[DONE]" {
		o.observation.CompletionMarker = true
		return
	}
	payload, err := convert.ParseJSON([]byte(data))
	if err != nil || !payload.IsObject() {
		return
	}
	o.observePayload(payload, frame.Event)
}

// observePayload 按客户端方言提取模型名、用量、终态标记与错误。
func (o *Observer) observePayload(payload *convert.Value, event string) {
	if model, ok := modelFromPayload(o.opts.Format, payload); ok {
		o.observation.Model = model
	}
	if usage := usageFromPayload(o.opts.Format, payload); !usage.IsEmpty() {
		o.mergeUsage(usage)
	}
	if reason, ok := stopReasonFromPayload(o.opts.Format, payload); ok {
		o.observation.StopReason = reason
	}
	if text, ok := errorTextFromPayload(payload); ok {
		o.observation.ErrorText = truncateText(text, 500)
	}
	if hasCompletionMarker(o.opts.Format, payload, event) {
		o.observation.CompletionMarker = true
	}
	if kind, ok := payload.StringField("type"); ok && kind == "response.incomplete" {
		o.observation.SawIncomplete = true
	}
}

// mergeUsage 按字段合并跨帧用量：后到的帧覆盖同名字段，未声明的字段保留旧值。
//
// 流式用量天然分散在多个帧里（Anthropic 的输入计数在 message_start、最终输出计数在
// message_delta；OpenAI 的 usage 只在收尾帧）。整帧覆盖会让输出计数回来时把输入清零，
// 计费随之失真。
func (o *Observer) mergeUsage(usage *convert.Usage) {
	merge := func(target **float64, value *float64) {
		if value != nil {
			*target = value
		}
	}
	merge(&o.observation.Usage.InputTokens, usage.InputTokens)
	merge(&o.observation.Usage.OutputTokens, usage.OutputTokens)
	merge(&o.observation.Usage.CacheReadTokens, usage.CacheReadTokens)
	merge(&o.observation.Usage.CacheWriteTokens, usage.CacheWriteTokens)
	merge(&o.observation.Usage.ReasoningTokens, usage.ReasoningTokens)
	o.observation.UsageSeen = true
}

// modelFromPayload 按方言取模型名。
//
// Anthropic 把模型名放在 message.model（首帧），Gemini 用 modelVersion，
// 其余线用顶层 model；只认顶层会让下游看到空模型名。
func modelFromPayload(format convert.ClientFormat, payload *convert.Value) (string, bool) {
	candidates := []*convert.Value{payload}
	switch format {
	case convert.FormatClaude:
		candidates = append(candidates, payload.ObjectField("message"))
	case convert.FormatResponse, convert.FormatGemini, convert.FormatGeminiCLI:
		candidates = append(candidates, payload.ObjectField("response"))
	}
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		for _, key := range []string{"model", "modelVersion"} {
			if value, ok := candidate.StringField(key); ok && value != "" {
				return value, true
			}
		}
	}
	return "", false
}

// usageFromPayload 从一帧里取用量，映射到枢纽口径。
//
// 枢纽口径的 InputTokens 是「未命中缓存的新鲜输入」：OpenAI 与 Gemini 的 prompt/input
// 计数含缓存命中，必须先扣掉缓存部分，否则缓存读会被重复计费（缓存读有自己的费率）。
func usageFromPayload(format convert.ClientFormat, payload *convert.Value) *convert.Usage {
	switch format {
	case convert.FormatClaude:
		// message_start 带 message.usage；message_delta 带顶层 usage。
		source := payload.ObjectField("message")
		if source == nil {
			source = payload
		}
		usage := source.ObjectField("usage")
		if usage == nil {
			return nil
		}
		return &convert.Usage{
			InputTokens:      numberField(usage, "input_tokens"),
			OutputTokens:     numberField(usage, "output_tokens"),
			CacheReadTokens:  numberField(usage, "cache_read_input_tokens"),
			CacheWriteTokens: numberField(usage, "cache_creation_input_tokens"),
		}
	case convert.FormatOpenAI:
		usage := payload.ObjectField("usage")
		if usage == nil {
			return nil
		}
		return fromPromptStyleUsage(usage, "prompt_tokens", "completion_tokens")
	case convert.FormatResponse:
		// 终态帧是 {type: response.completed, response: {...}}，也可能直接是 response 对象。
		source := payload.ObjectField("response")
		if source == nil {
			source = payload
		}
		usage := source.ObjectField("usage")
		if usage == nil {
			return nil
		}
		return fromPromptStyleUsage(usage, "input_tokens", "output_tokens")
	case convert.FormatGemini, convert.FormatGeminiCLI:
		source := payload.ObjectField("response")
		if source == nil {
			source = payload
		}
		usage := source.ObjectField("usageMetadata")
		if usage == nil {
			return nil
		}
		prompt := numberField(usage, "promptTokenCount")
		cached := numberField(usage, "cachedContentTokenCount")
		return &convert.Usage{
			InputTokens:     subtractTokens(prompt, cached),
			OutputTokens:    numberField(usage, "candidatesTokenCount"),
			CacheReadTokens: cached,
			ReasoningTokens: numberField(usage, "thoughtsTokenCount"),
		}
	default:
		return nil
	}
}

// fromPromptStyleUsage 处理「输入计数含缓存命中」的两条线。
func fromPromptStyleUsage(usage *convert.Value, inputKey, outputKey string) *convert.Usage {
	input := numberField(usage, inputKey)
	cached := numberField(
		nestedOrSelf(usage, "input_tokens_details", "prompt_tokens_details"),
		"cached_tokens",
	)
	return &convert.Usage{
		InputTokens:     subtractTokens(input, cached),
		OutputTokens:    numberField(usage, outputKey),
		CacheReadTokens: cached,
		ReasoningTokens: numberField(nestedOrSelf(usage, "output_tokens_details", "completion_tokens_details"), "reasoning_tokens"),
	}
}

// nestedOrSelf 依次尝试两个细节字段；都缺时返回原对象（让字段查询自然落空）。
func nestedOrSelf(value *convert.Value, keys ...string) *convert.Value {
	for _, key := range keys {
		if nested := value.ObjectField(key); nested != nil {
			return nested
		}
	}
	return value
}

// numberField 读取数值字段；缺失或非数值返回 nil（枢纽口径用 nil 表示「未声明」）。
func numberField(value *convert.Value, key string) *float64 {
	if value == nil {
		return nil
	}
	raw, ok := value.Get(key)
	if !ok || raw == nil {
		return nil
	}
	number, ok := raw.Float64()
	if !ok {
		return nil
	}
	return &number
}

// subtractTokens 返回 total - part，任一为空返回 nil，结果为负取 0。
func subtractTokens(total, part *float64) *float64 {
	if total == nil {
		return nil
	}
	if part == nil {
		return total
	}
	difference := *total - *part
	if difference < 0 {
		difference = 0
	}
	return &difference
}

// stopReasonFromPayload 取停止原因，保留各线原样取值。
func stopReasonFromPayload(format convert.ClientFormat, payload *convert.Value) (string, bool) {
	switch format {
	case convert.FormatClaude:
		if delta := payload.ObjectField("delta"); delta != nil {
			if reason, ok := delta.StringField("stop_reason"); ok && reason != "" {
				return reason, true
			}
		}
		return "", false
	case convert.FormatOpenAI:
		for _, choice := range payload.ArrayField("choices") {
			if reason, ok := choice.StringField("finish_reason"); ok && reason != "" {
				return reason, true
			}
		}
		return "", false
	case convert.FormatResponse:
		source := payload.ObjectField("response")
		if source == nil {
			source = payload
		}
		if reason, ok := source.StringField("status"); ok && reason != "" {
			return reason, true
		}
		return "", false
	case convert.FormatGemini, convert.FormatGeminiCLI:
		source := payload.ObjectField("response")
		if source == nil {
			source = payload
		}
		for _, candidate := range source.ArrayField("candidates") {
			if reason, ok := candidate.StringField("finishReason"); ok && reason != "" {
				return reason, true
			}
		}
		return "", false
	default:
		return "", false
	}
}

// hasCompletionMarker 判定与方言匹配的终止标记。
//
// 仅「有 usage」不足以证明完成：Anthropic 在首个 message_start 就带 usage，
// Gemini 在中间事件也带 usageMetadata，被截断的流同样可能出现正向 token 数。
func hasCompletionMarker(format convert.ClientFormat, payload *convert.Value, event string) bool {
	switch format {
	case convert.FormatClaude:
		kind, ok := payload.StringField("type")
		return ok && kind == "message_stop" && (event == "" || event == "message" || event == kind)
	case convert.FormatOpenAI:
		// 只有 finish_reason 或 [DONE] 才算完成；[DONE] 在 observeFrame 里判定。
		for _, choice := range payload.ArrayField("choices") {
			if reason, ok := choice.StringField("finish_reason"); ok && strings.TrimSpace(reason) != "" {
				return true
			}
		}
		return false
	case convert.FormatResponse:
		kind, ok := payload.StringField("type")
		if !ok || (kind != "response.completed" && kind != "response.done") {
			return false
		}
		if event != "" && event != "message" && event != kind {
			return false
		}
		if kind == "response.done" {
			return true
		}
		return payload.ObjectField("response") != nil
	case convert.FormatGemini, convert.FormatGeminiCLI:
		source := payload.ObjectField("response")
		if source == nil {
			source = payload
		}
		for _, candidate := range source.ArrayField("candidates") {
			if reason, ok := candidate.StringField("finishReason"); ok && strings.TrimSpace(reason) != "" {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// errorTextFromPayload 提取流内错误帧的文案（多线形态：error 字段或顶层 type=error）。
func errorTextFromPayload(payload *convert.Value) (string, bool) {
	errorValue := payload.ObjectField("error")
	if errorValue == nil {
		if kind, ok := payload.StringField("type"); ok && kind == "error" {
			errorValue = payload
		} else {
			if message, ok := payload.StringField("message"); ok && message != "" {
				return message, true
			}
			return "", false
		}
	}
	if message, ok := errorValue.StringField("message"); ok && message != "" {
		return message, true
	}
	if text, ok := errorValue.String(); ok && text != "" {
		return text, true
	}
	return "", false
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// recoverFromWindows 在分帧器丢帧后，用已保留的头尾窗口补回 model 与 usage。
//
// 为何这样做而不是抬高分帧器上限：包内不变量是「每流驻留量与正文长度无关」
// （TestObserverResidencyStaysBoundedForLargeStream 守着），而抬上限会把整行缓存在内存里。
// 上游把整段回答作为一条超长行时（实测 ollama.com），该行里 `model` 在**行首**、`usage` 在
// **行尾**，正好落在观测器本来就保留的头尾窗口内（Node 也是靠 head 1 MiB + tail 9 MiB 双窗口
// 拿尾部 usage 的）。故这里只读窗口，不新增任何缓冲。
//
// 只补缺，不覆盖：帧级路径已经抽到的事实在任何时候都优先（它比窗口猜测更精确）。
func (o *Observer) recoverFromWindows(snapshot *Observation) {
	if snapshot.Model == "" {
		if model := modelFromWindowText(string(o.head)); model != "" {
			snapshot.Model = model
		}
	}
	if snapshot.Usage.IsEmpty() {
		if usage, ok := usageFromWindowText(string(o.tailOrdered()), o.opts.Format); ok {
			snapshot.Usage = usage
			// UsageSeen 必须一起置位：结算层的判据是它（dataplane 只在 UsageSeen 为真时写
			// 用量列），只填 Usage 而不置位，等于把回退拿回的用量整包丢弃——
			// 生产上表现为「模型有值、input/output_tokens 与计费全空」，即
			// 「上游把整段回答作为一条超长行」的供应商（实测 ollama.com）99% 的行丢用量。
			snapshot.UsageSeen = true
		}
	}
}

// modelFromWindowText 从窗口文本里取首个 `"model":"..."`。空串表示窗口里没有（或值被截断）。
func modelFromWindowText(text string) string {
	const key = `"model":"`
	index := strings.Index(text, key)
	if index < 0 {
		return ""
	}
	rest := text[index+len(key):]
	end := strings.IndexByte(rest, '"')
	if end <= 0 {
		return ""
	}
	return rest[:end]
}

// usageFromWindowText 从窗口文本里取**最后一个** usage 对象并按方言投影。
//
// 取最后一个：流尾才是终态用量（中途事件可能带占位 0）。括号配对失败（对象被窗口截断）
// 就如实返回 false——宁可没有，也不报半个数。
func usageFromWindowText(text string, format convert.ClientFormat) (convert.Usage, bool) {
	index := strings.LastIndex(text, `"usage":`)
	key := "usage"
	if index < 0 {
		index = strings.LastIndex(text, `"usageMetadata":`)
		key = "usageMetadata"
	}
	if index < 0 {
		return convert.Usage{}, false
	}
	open := strings.IndexByte(text[index:], '{')
	if open < 0 {
		return convert.Usage{}, false
	}
	open += index
	depth := 0
	for cursor := open; cursor < len(text); cursor++ {
		switch text[cursor] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				synthetic := `{"` + key + `":` + text[open:cursor+1] + `}`
				value, err := convert.ParseJSON([]byte(synthetic))
				if err != nil {
					return convert.Usage{}, false
				}
				usage := usageFromPayload(format, value)
				if usage == nil || usage.IsEmpty() {
					return convert.Usage{}, false
				}
				return *usage, true
			}
		}
	}
	return convert.Usage{}, false
}
