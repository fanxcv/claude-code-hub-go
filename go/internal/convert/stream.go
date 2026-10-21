package convert

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// 本文件是流式转换的公共骨架，对应 TS 侧的 `shared/sse.ts` + `shared/stream.ts` + `types.ts` 的
// `CanonicalChunk` / `StreamTranslator` / `StreamEncoder` 三处契约。
//
// 为什么单独一套骨架：三线都用 `text/event-stream`，但帧边界、终止标记与事件名各不相同；
// 把「字节 ↔ 帧」的机械处理收在一处，三条协议线只写自己的语义映射，别处不重复解析 SSE。
//
// 内存纪律（与数据面的流式路径同一契约）：本层只在缓冲区里持有「尚未构成完整帧的残留」，
// 即单帧上限；正文总大小不参与本层驻留。任何「先把整流收齐再转换」的改动都是回归。

// DoneSentinel 是 Chat 线的流终止哨兵。
const DoneSentinel = "[DONE]"

// ---------------------------------------------------------------------------
// SSE 帧
// ---------------------------------------------------------------------------

// SSEFrame 是一帧 SSE：`event:` 字段可选，`data:` 为已剥离前缀的内容（多行以 \n 连接）。
type SSEFrame struct {
	Event    string
	HasEvent bool
	Data     string
}

// ParseSSEFrames 从缓冲文本中切出完整帧，返回帧序列与尚未成帧的残留。
//
// 判定「完整帧」的规则：出现空行分隔符（`\n\n`、`\r\n\r\n`，含跨块形式）；以 `\r` 结尾时
// 保留不解，等下一块（可能是被切开的 `\r\n`）。
func ParseSSEFrames(buffer string) ([]SSEFrame, string) {
	var frames sseFrameBuffer
	split := frames.push([]byte(buffer))
	return split, string(frames.rest)
}

// sseFrameBuffer 是流式解码器的字节累积器：追加一块、吐出其中新切出的完整帧。
//
// 为什么不用 `buffer += string(chunk)` + 整体重扫：未闭合的超长帧（如 256 KiB 的参数增量）
// 被小分块喂入时，每块都要把已累积的全部字节重拷并重扫一遍，整条流 O(帧长²)。
// 这里只追加、只扫新增部分（scanned 记已扫位置），残留以字节持有，不重复转字符串。
type sseFrameBuffer struct {
	rest []byte
	// scanned 是 rest 中「已确认不含分隔符」的长度，下次从这里继续扫。
	scanned int
}

// push 追加一块上游字节，返回其中新切出的完整帧；未成帧的残留留在 rest 里。
func (b *sseFrameBuffer) push(chunk []byte) []SSEFrame {
	b.rest = append(b.rest, chunk...)
	var frames []SSEFrame
	cursor := 0
	for {
		start, end, found := findFrameBoundary(b.rest, max(b.scanned, cursor))
		if !found {
			// 最长分隔符 4 字节（`\r\n\r\n`）：回退 3 字节重扫，跨块的分隔符不会漏。
			b.scanned = max(cursor, len(b.rest)-3)
			break
		}
		if frame, ok := parseFrameBody(b.rest[cursor:start]); ok {
			frames = append(frames, frame)
		}
		cursor, b.scanned = end, end
	}
	if cursor == 0 {
		return nil
	}
	// 残留搬到缓冲头部（同一切片，memmove）：只在真切出帧时发生，未闭合的长帧
	// 不会因逐块追加而被搬来搬去。
	b.rest = append(b.rest[:0], b.rest[cursor:]...)
	b.scanned = max(b.scanned-cursor, 0)
	return frames
}

// findFrameBoundary 从 from 起寻找分隔符，返回分隔符起止（start 之前的正文不含分隔符）。
func findFrameBoundary(buffer []byte, from int) (int, int, bool) {
	for i := from; i < len(buffer); i++ {
		switch buffer[i] {
		case '\n':
			if i+1 < len(buffer) && buffer[i+1] == '\n' {
				return i, i + 2, true
			}
			if i+2 < len(buffer) && buffer[i+1] == '\r' && buffer[i+2] == '\n' {
				return i, i + 3, true
			}
		case '\r':
			if i+1 >= len(buffer) || buffer[i+1] != '\n' {
				continue
			}
			if i+2 < len(buffer) && buffer[i+2] == '\n' {
				return i, i + 3, true
			}
			if i+3 < len(buffer) && buffer[i+2] == '\r' && buffer[i+3] == '\n' {
				return i, i + 4, true
			}
		}
	}
	return 0, 0, false
}

// parseFrameBody 解析单帧正文：忽略注释/心跳行，收集 `event:` 与 `data:`。
//
// 帧正文以字节切片传入：切帧层持有的是字节缓冲，若每帧先转字符串则多一次全文拷贝。
// `Data` 仍必须拷成自有字符串——它可能别名解码器的追加缓冲，而那块缓冲会被后续 push 改写。
func parseFrameBody(raw []byte) (SSEFrame, bool) {
	event := ""
	hasEvent := false
	var data strings.Builder
	for _, line := range splitLines(raw) {
		if len(line) > 0 && line[0] == ':' {
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			event = string(bytes.TrimSpace(line[len("event:"):]))
			hasEvent = true
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			text := line[len("data:"):]
			if len(text) > 0 && text[0] == ' ' {
				text = text[1:]
			}
			data.Write(text)
		}
	}
	if !hasEvent && data.Len() == 0 {
		return SSEFrame{}, false
	}
	return SSEFrame{Event: event, HasEvent: hasEvent, Data: data.String()}, true
}

// splitLines 按 \n / \r\n / \r 切行（与 JS 的 /\r?\n/ 语义对齐，另容忍孤立 \r）。
func splitLines(raw []byte) [][]byte {
	if !bytes.ContainsAny(raw, "\r\n") {
		return [][]byte{raw}
	}
	lines := make([][]byte, 0, 4)
	start := 0
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '\n':
			lines = append(lines, raw[start:i])
			start = i + 1
		case '\r':
			lines = append(lines, raw[start:i])
			if i+1 < len(raw) && raw[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}

// AppendJSONSSEFrame 把「单行 JSON 载荷」一帧直接写成线上字节：`event: X\ndata: {…}\n\n`。
//
// 为何单独一条：流式编码的每帧都同形，而载荷来自 AppendCompact。串行化再转字节
// 会先把 JSON 造成 string、再拷进 Builder、再转 []byte——三份全量副本；这里只有一份。
func AppendJSONSSEFrame(dst []byte, event string, payload *Value) []byte {
	if event != "" {
		dst = append(dst, "event: "...)
		dst = append(dst, event...)
		dst = append(dst, '\n')
	}
	dst = append(dst, "data: "...)
	dst = payload.AppendCompact(dst)
	return append(dst, '\n', '\n')
}

// IsDoneFrame 判定是否为 Chat 的终止哨兵。
func IsDoneFrame(frame SSEFrame) bool {
	return strings.TrimSpace(frame.Data) == DoneSentinel
}

// parseFrameJSON 尽力解析帧内 JSON；失败返回 nil（调用方据此判定畸形帧）。
func parseFrameJSON(frame SSEFrame) *Value {
	text := strings.TrimSpace(frame.Data)
	if text == "" || text == DoneSentinel {
		return nil
	}
	parsed, err := ParseJSON([]byte(text))
	if err != nil || parsed == nil || !parsed.IsObject() {
		return nil
	}
	return parsed
}

// ---------------------------------------------------------------------------
// 枢纽块：流式中间态
// ---------------------------------------------------------------------------

// ChunkKind 是流式枢纽块的种类。
type ChunkKind string

const (
	ChunkStart      ChunkKind = "start"
	ChunkBlockStart ChunkKind = "block_start"
	ChunkBlockDelta ChunkKind = "block_delta"
	ChunkBlockStop  ChunkKind = "block_stop"
	ChunkDelta      ChunkKind = "delta"
	ChunkEnd        ChunkKind = "end"
)

// Chunk 是 decode 产出、encode 消费的流式枢纽块。
//
// 可选字段一律用指针：`blockIndex=0` 与「未给」在协议上语义不同（前者指第一个块，
// 后者要求编码器自行分配），用零值会静默混淆两者。
type Chunk struct {
	Kind           ChunkKind
	BlockIndex     *int
	Block          *Block
	TextDelta      *string
	ArgsDelta      *string
	ReasoningDelta *string
	StopReason     *StopReason
	Usage          *Usage
	// Failed 表示本次流以上游失败收尾。ChunkEnd 带上它时，编码器只关块、不补正常终止帧：
	// 补了就等于向客户端宣称「响应完整」，把上游的失败伪装成成功。
	Failed bool
}

func intPtr(value int) *int          { return &value }
func stringPtr(value string) *string { return &value }

// StreamDecoder 吃上游字节、吐枢纽块。每次响应新建实例，状态全部内聚。
type StreamDecoder interface {
	Push(chunk []byte) []Chunk
	Flush() []Chunk
}

// StreamEncoder 吃枢纽块、吐客户端字节。
type StreamEncoder interface {
	Push(chunk Chunk) [][]byte
	Flush() [][]byte
}

type streamFactory struct {
	newDecoder func(ConvertCtx) StreamDecoder
	newEncoder func(ConvertCtx) StreamEncoder
}

func streamFactoryFor(protocol WireProtocol) (streamFactory, bool) {
	switch protocol {
	case ProtocolAnthropicMessages:
		return streamFactory{newAnthropicStreamDecoder, newAnthropicStreamEncoder}, true
	case ProtocolOpenAIChat:
		return streamFactory{newChatStreamDecoder, newChatStreamEncoder}, true
	case ProtocolOpenAIResponses:
		return streamFactory{newResponsesStreamDecoder, newResponsesStreamEncoder}, true
	default:
		return streamFactory{}, false
	}
}

// NewStreamDecoder 按协议线建流式解码器。
func NewStreamDecoder(protocol WireProtocol, ctx ConvertCtx) (StreamDecoder, bool) {
	factory, ok := streamFactoryFor(protocol)
	if !ok || factory.newDecoder == nil {
		return nil, false
	}
	return factory.newDecoder(ctx), true
}

// NewStreamEncoder 按协议线建流式编码器。
func NewStreamEncoder(protocol WireProtocol, ctx ConvertCtx) (StreamEncoder, bool) {
	factory, ok := streamFactoryFor(protocol)
	if !ok || factory.newEncoder == nil {
		return nil, false
	}
	return factory.newEncoder(ctx), true
}

// ---------------------------------------------------------------------------
// 串联：上游字节 → 枢纽 → 客户端字节
// ---------------------------------------------------------------------------

// StreamPipe 把解码器与编码器串成面向字节的转换器。
//
// 收尾顺序：先 flush 解码器（吐净上游残留帧），再 flush 编码器（补发客户端线的终止事件）。
type StreamPipe struct {
	decoder StreamDecoder
	encoder StreamEncoder
	// pending 是跨块残字，恒为 pendingScratch 的切片（自有存储，不别名调用方的读缓冲）。
	pending []byte
	// pendingScratch 是 pending 的存储本体：splitCompleteUTF8 最多留下 4 字节残字。
	pendingScratch [4]byte
}

// NewStreamPipe 建一条「上游协议线 → 客户端协议线」的字节转换器。
func NewStreamPipe(upstream WireProtocol, client WireProtocol, ctx ConvertCtx) (*StreamPipe, bool) {
	decoder, ok := NewStreamDecoder(upstream, ctx)
	if !ok {
		return nil, false
	}
	encoder, ok := NewStreamEncoder(client, ctx)
	if !ok {
		return nil, false
	}
	return &StreamPipe{decoder: decoder, encoder: encoder}, true
}

// Push 喂入一块上游字节，产出一块客户端字节（可能为空，表示本块尚未构成完整帧）。
func (p *StreamPipe) Push(chunk []byte) []byte {
	text, rest := splitCompleteUTF8(p.joinPending(chunk))
	p.setPending(rest)
	if len(text) == 0 {
		return nil
	}
	return p.pipe(text)
}

// joinPending 把残字与本次的块拼成一段完整字节；无残字时直接返回本块（不拷贝）。
func (p *StreamPipe) joinPending(chunk []byte) []byte {
	if len(p.pending) == 0 {
		return chunk
	}
	joined := make([]byte, 0, len(p.pending)+len(chunk))
	joined = append(joined, p.pending...)
	return append(joined, chunk...)
}

// setPending 把残字拷进自有存储。
//
// 为什么必须拷：调用方复用同一个读缓冲（dataplane 每轮 Read 覆盖它）。残字若别名该缓冲，
// 跨块被切开的多字节字符就会在下次 Read 之后变成乱码。
func (p *StreamPipe) setPending(rest []byte) {
	n := copy(p.pendingScratch[:], rest)
	p.pending = p.pendingScratch[:n]
}

// Flush 结束上游：吐净残留并补发客户端线的终止事件。
func (p *StreamPipe) Flush() []byte {
	out := make([][]byte, 0, 4)
	if len(p.pending) > 0 {
		// 上游在半个字符中间断流：按替换字符处理，与 TextDecoder 的容错一致。
		out = append(out, p.pipe(p.pending))
		p.pending = nil
	}
	for _, chunk := range p.decoder.Flush() {
		out = append(out, p.encoder.Push(chunk)...)
	}
	out = append(out, p.encoder.Flush()...)
	return concatBytes(out)
}

func (p *StreamPipe) pipe(text []byte) []byte {
	out := make([][]byte, 0, 4)
	for _, chunk := range p.decoder.Push(text) {
		out = append(out, p.encoder.Push(chunk)...)
	}
	return concatBytes(out)
}

// splitCompleteUTF8 切掉结尾不完整的多字节序列，返回可直接解码的文本与残留字节。
//
// 为什么必须自己切：TS 侧用 TextDecoder 的 stream 模式跨块保留半个字符，Go 的 string
// 没有等价物。不切就会在块边界产出替换字符，客户端看到乱码。
//
// 返回的完整段是入参的子切片（只供本次转交解码器，解码器会自行持有副本），残留段最长 4 字节。
func splitCompleteUTF8(buffer []byte) ([]byte, []byte) {
	limit := len(buffer) - 4
	if limit < 0 {
		limit = 0
	}
	for i := len(buffer); i > limit; i-- {
		if !utf8.RuneStart(buffer[i-1]) {
			continue
		}
		if utf8.Valid(buffer[i-1:]) {
			return buffer, nil
		}
		return buffer[:i-1], buffer[i-1:]
	}
	return buffer, nil
}

func concatBytes(chunks [][]byte) []byte {
	total := 0
	for _, chunk := range chunks {
		total += len(chunk)
	}
	if total == 0 {
		return nil
	}
	out := make([]byte, 0, total)
	for _, chunk := range chunks {
		out = append(out, chunk...)
	}
	return out
}

// ---------------------------------------------------------------------------
// 流式共用的小工具
// ---------------------------------------------------------------------------

// mergeUsage 合并两段 usage：后到者覆盖同名字段，输入取两者较大值（与 TS 一致）。
//
// 为什么输入取 max：上游常在首帧给前缀用量、尾帧给最终用量；取后者会把首帧已计入的
// 缓存读取量抹掉，客户端看到的 usage 会自相矛盾。
func mergeUsage(base *Usage, delta *Usage) *Usage {
	if base == nil {
		if delta == nil {
			return &Usage{}
		}
		return cloneUsage(delta)
	}
	if delta == nil {
		return cloneUsage(base)
	}
	out := cloneUsage(base)
	if delta.InputTokens != nil {
		value := *delta.InputTokens
		if out.InputTokens != nil && *out.InputTokens > value {
			value = *out.InputTokens
		}
		if value < 0 {
			value = 0
		}
		out.InputTokens = &value
	}
	if delta.OutputTokens != nil {
		out.OutputTokens = delta.OutputTokens
	}
	if delta.CacheReadTokens != nil {
		out.CacheReadTokens = delta.CacheReadTokens
	}
	if delta.CacheWriteTokens != nil {
		out.CacheWriteTokens = delta.CacheWriteTokens
	}
	if delta.ReasoningTokens != nil {
		out.ReasoningTokens = delta.ReasoningTokens
	}
	return out
}

// declaredTextTail 计算「声明式全文里尚未发出的尾巴」。
//
// 用途：done / 终态 / item 载荷里的 text（含 arguments）是对「这一块应当有哪些字」的权威声明，
// 而上游可能只发了部分增量、甚至一个增量都没发；此时按声明补差额，否则客户端拿到的是残句。
//
// 硬约束是「绝不重复」：仅当已发内容恰好是声明全文的前缀时才补差额，否则返回空串——不构成
// 前缀关系说明上游顺序反常（重排或回退），补任何东西都会让客户端看到重复或乱序，宁可不补。
//
// 例外只有一种，且只在**块内首片歧义无法在悬置窗口内等来声明**时生效：content_part.added 的
// part.text 与首个增量逐字相同，本地无从消歧，此时按「宁可重复、不静默丢字」把两个来源各交付一次。
// 「等不来」含三种形态，取舍相同：① 上游完全不给声明——流结束仍无 done / item / 终态文本，见
// responsesStreamDecoder 的 closeBlock；② 声明晚于悬置上限 responsesPendingMaxBytes 才到达——
// releaseSuspended 已按兜底交付，随后到达的声明因 emitted 已长于声明全文、算不出差额而不回退已
// 交付内容；③ 首个增量本身超过上限，根本放不进窗口（走的是普通路径的 flushSeed，不是 releaseSuspended）。
// 三者的同一后果是「此刻已无可用声明」，故按兜底交付。见
// TestResponsesSuspendedSeedStreamsBeforeDeclaration 与 TestResponsesSuspendedSeedPendingHasHardCap。
// 声明在窗口内可得的路径一律按声明精确交付，不产生重复。
//
// 只许用于**声明式**载荷（done / 终态 / item / content_part.added 的初始文本），禁止用于
// 增量帧：增量之间没有前缀保证，合法的重复文本（如两个同字增量）会被误判成重复而吞掉。
func declaredTextTail(emitted string, declared string) string {
	if len(declared) == 0 || emitted == declared {
		return ""
	}
	if strings.HasPrefix(declared, emitted) {
		return declared[len(emitted):]
	}
	return ""
}

// messageReplay 判定一段即将交付的文本是否属「**跨块**整段回放」。
//
// 为什么需要消息级、而不只是块级：两条线的解码器都把「已发内容」按**块**记账，块边界一到就归零：
//
//   - chat 线：tool_calls / thinking 帧关闭文本块时就清 emitted，随后同形帧既无从比对、
//     又被当作新块的首帧原样下发；
//   - responses 线：声明式收尾（completed 的 output[] / item.done）按**数组下标**反推块键，
//     与流式期的 output_index 不一致时 startBlock 会建出一个 emitted 为空的新块，
//     声明全文于是被当成新内容补发（生产实测的 2x 重复即此）。
//
// 判据只取最保守的一条：本帧文本与消息级已发全文**逐字相等**，且该全文由**多个帧**拼成。
// 余下一律放行——「宁可重复、不静默丢字」。
//
// 不做「严格扩展」（本帧以全篇为前缀且更长）一档：消息级基线很长，合法续写里出现
// 「以全篇为前缀」的概率远高于块内，按前缀截断会静默吃掉合法首段。
func messageReplay(messageText string, messageParts int, text string) bool {
	return text != "" && messageParts > 1 && text == messageText
}

func cloneUsage(usage *Usage) *Usage {
	if usage == nil {
		return &Usage{}
	}
	out := &Usage{}
	out.InputTokens = copyFloat(usage.InputTokens)
	out.OutputTokens = copyFloat(usage.OutputTokens)
	out.CacheReadTokens = copyFloat(usage.CacheReadTokens)
	out.CacheWriteTokens = copyFloat(usage.CacheWriteTokens)
	out.ReasoningTokens = copyFloat(usage.ReasoningTokens)
	return out
}

func copyFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

// syntheticPrefix 是合成信封 id 的协议前缀（与真实上游同形，客户端按前缀识别协议）。
var syntheticPrefix = map[WireProtocol]string{
	ProtocolAnthropicMessages: "msg_cch_",
	ProtocolOpenAIChat:        "chatcmpl-cch-",
	ProtocolOpenAIResponses:   "resp_cch_",
}

// MakeSyntheticResponseID 合成一个唯一的响应信封 id。
//
// 调用方须保证「一次响应（或一条流）只合一个并复用」：固定常量会让所有被转换的响应在
// 客户端与日志里看起来是同一次响应。中缀 cch 把合成值与真实上游 id 区分开。
func MakeSyntheticResponseID(protocol WireProtocol) string {
	prefix, ok := syntheticPrefix[protocol]
	if !ok {
		prefix = "cch_"
	}
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		// 极端的熵源失败：不阻断响应，退化为确定性的零后缀（排障时一眼可辨）。
		return prefix + "0000000000000000"
	}
	return prefix + hex.EncodeToString(buffer)
}
