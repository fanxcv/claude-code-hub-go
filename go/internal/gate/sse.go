package gate

import (
	"fmt"
	"strings"
)

// Frame 是一个完整 SSE 帧。Event 为空串表示该帧没有 event 行
// （openai-chat / gemini 的 chunk 以及裸 JSON 行都是这种形态）。
type Frame struct {
	Event string
	Data  string
}

const (
	// dataHeadBytes 与 TS 的 DATA_HEAD_MAX_CHARACTERS 对齐：只保留 data 头部供回显豁免判定，
	// 避免为了判定豁免而复制整个大帧。
	dataHeadBytes = 64
	// lineHeadBytes 覆盖 "data: " 前缀加 dataHeadBytes。
	lineHeadBytes = dataHeadBytes + len("data: ")
	// 单帧数据超过该容量后释放 backing array，避免长流为一次偶发大帧长期持有内存。
	dataBufferRetainBytes = 64 << 10
)

// BufferLimitExemption 给指定帧形态放宽 parser 缓冲上限（TS 的 bufferLimitExemption）：
// openai-responses 的请求回显帧会带上完整请求体，单帧即可达数百 KB，
// 不应把「请求大」误判成「流异常」。豁免仍受独立硬上限约束。
type BufferLimitExemption struct {
	// MaxBufferedBytes 是豁免帧的硬上限，必须不小于 MaxBufferedBytes。
	MaxBufferedBytes int
	// Matches 只收到 event 名与固定长度 data 头部（不超过 dataHeadBytes 字节）。
	Matches func(event string, dataHead string) bool
}

// ParserOptions 是分帧器配置。MaxBufferedBytes 为 0 表示不设上限
// （仅解析完整已知 body 的调用方可以省略；门控热路径必须设上限）。
type ParserOptions struct {
	MaxBufferedBytes int
	Exemption        *BufferLimitExemption
}

// BufferLimitError 表示 parser 保留状态超过 MaxBufferedBytes。抛出前 parser 已重置，
// 但调用方不应复用该 parser 实例（残留语义不确定）。
type BufferLimitError struct {
	MaxBufferedBytes int
}

func (e *BufferLimitError) Error() string {
	return fmt.Sprintf("SSE parser buffered data exceeded %d bytes", e.MaxBufferedBytes)
}

// Parser 是增量 SSE 分帧器。零值不可用，须经 NewParser 构造。
//
// 帧边界语义（与 TS 侧一致）：
//   - 空行触发 dispatch；无 data 行的事件不产出帧（但会重置 event 名）
//   - event: 值 trim；data: 仅剥一个前导空白；多行 data 以 \n 连接
//   - 注释行（: 开头）与 id:/retry:/未知字段忽略
//   - 无 event/data 行、且以 { 或 [ 开头的裸 JSON 行直接作为一帧产出
type Parser struct {
	opts ParserOptions

	line          []byte
	lineHead      string
	skipLeadingLF bool
	currentEvent  string
	data          []byte
	dataLines     int
	dataHead      string
}

// NewParser 构造分帧器。
func NewParser(opts ParserOptions) *Parser {
	return &Parser{opts: opts}
}

// Push 喂入一个网络 chunk，返回其中完成的帧。
func (p *Parser) Push(chunk []byte) ([]Frame, error) {
	frames := make([]Frame, 0, 4)
	if _, err := p.Visit(chunk, func(frame Frame) bool {
		frames = append(frames, frame)
		return true
	}); err != nil {
		return nil, err
	}
	return frames, nil
}

// PushText 直接喂入已解码文本（供对完整 body 做一次性解析的调用方使用）。
func (p *Parser) PushText(text string) ([]Frame, error) {
	return p.Push([]byte(text))
}

// Visit 边解析边消费帧。返回的 drained 为 false 表示 visitor 提前终止了解析
// （visitor 返回 false），此后该 parser 不可复用。
func (p *Parser) Visit(chunk []byte, visitor func(Frame) bool) (drained bool, err error) {
	return p.consume(chunk, visitor)
}

// Finish 冲刷尾部未换行的行与未 dispatch 的帧。
func (p *Parser) Finish() ([]Frame, error) {
	frames := make([]Frame, 0, 1)
	if _, err := p.FinishVisit(func(frame Frame) bool {
		frames = append(frames, frame)
		return true
	}); err != nil {
		return nil, err
	}
	return frames, nil
}

// FinishVisit 是 Finish 的 visitor 版本。
func (p *Parser) FinishVisit(visitor func(Frame) bool) (drained bool, err error) {
	if _, err := p.consume(nil, visitor); err != nil {
		return false, err
	}
	p.skipLeadingLF = false
	if len(p.line) > 0 {
		// 尾部残行按一行处理（与 TS 对无终止空行的流的行为一致）
		keepGoing, err := p.handleLine(p.takeLine(), visitor)
		if err != nil {
			return false, err
		}
		if !keepGoing {
			return false, nil
		}
	}
	return p.flush(visitor)
}

func (p *Parser) consume(text []byte, visitor func(Frame) bool) (bool, error) {
	start := 0
	if p.skipLeadingLF {
		if len(text) == 0 {
			return true, nil
		}
		if text[0] == '\n' {
			start = 1
		}
		p.skipLeadingLF = false
	}

	for index := start; index < len(text); index++ {
		code := text[index]
		if code != '\n' && code != '\r' {
			continue
		}

		p.appendLinePart(text[start:index])
		// 完整行也必须在归并前执行上限检查；否则带换行的超长未知字段会绕过行尾的
		// 保留状态检查，并在取行时制造一次大分配。
		if err := p.assertBufferLimitWith(
			p.eventForCompletedLine(),
			p.currentDataHead(),
			p.completedLineSyntaxBytes(),
			strings.HasPrefix(p.lineHead, "event:"),
		); err != nil {
			return false, err
		}
		keepGoing, err := p.handleLine(p.takeLine(), visitor)
		if err != nil {
			return false, err
		}
		if !keepGoing {
			return false, nil
		}

		if code == '\r' {
			if index+1 < len(text) && text[index+1] == '\n' {
				index++
			} else if index == len(text)-1 {
				p.skipLeadingLF = true
			}
		}
		start = index + 1
	}

	p.appendLinePart(text[start:])
	if err := p.assertBufferLimit(); err != nil {
		return false, err
	}
	return true, nil
}

func (p *Parser) appendLinePart(part []byte) {
	if len(part) == 0 {
		return
	}
	p.line = append(p.line, part...)
	if len(p.lineHead) < lineHeadBytes {
		head := part
		if len(head) > lineHeadBytes-len(p.lineHead) {
			head = head[:lineHeadBytes-len(p.lineHead)]
		}
		p.lineHead += string(head)
	}
}

func (p *Parser) takeLine() []byte {
	line := p.line
	p.line = nil
	p.lineHead = ""
	return line
}

// handleLine 返回 false 表示 visitor 终止了解析。
func (p *Parser) handleLine(line []byte, visitor func(Frame) bool) (bool, error) {
	if len(line) == 0 {
		return p.flush(visitor)
	}
	if line[0] == ':' {
		return true, nil // SSE 注释
	}
	if hasFieldPrefix(line, "event:") {
		p.currentEvent = strings.TrimSpace(string(line[len("event:"):]))
		return true, p.assertBufferLimit()
	}
	if hasFieldPrefix(line, "data:") {
		data := stripOneLeadingSpace(line[len("data:"):])
		if p.dataLines > 0 {
			p.data = append(p.data, '\n')
		}
		p.data = append(p.data, data...)
		p.dataLines++
		p.appendDataHead(data)
		return true, p.assertBufferLimit()
	}
	candidate := strings.TrimSpace(string(line))
	if p.currentEvent == "" && p.dataLines == 0 && len(candidate) > 0 &&
		(candidate[0] == '{' || candidate[0] == '[') {
		return visitor(Frame{Event: "", Data: candidate}), nil
	}
	// id: / retry: / 未知字段：忽略
	return true, nil
}

func (p *Parser) flush(visitor func(Frame) bool) (bool, error) {
	event := p.currentEvent
	p.currentEvent = ""
	if p.dataLines == 0 {
		p.dataHead = ""
		return true, nil
	}
	data := string(p.data)
	p.releaseDataBuffer()
	p.dataLines = 0
	p.dataHead = ""
	return visitor(Frame{Event: event, Data: data}), nil
}

func (p *Parser) releaseDataBuffer() {
	if cap(p.data) > dataBufferRetainBytes {
		p.data = nil
		return
	}
	p.data = p.data[:0]
}

func (p *Parser) appendDataHead(data []byte) {
	if len(p.dataHead) >= dataHeadBytes {
		return
	}
	if p.dataLines > 1 {
		p.dataHead += "\n"
	}
	room := dataHeadBytes - len(p.dataHead)
	if len(data) > room {
		data = data[:room]
	}
	p.dataHead += string(data)
}

// currentDataHead 返回「已积累 data 头部 + 当前未完成行的 data 头部」，供豁免判定用。
func (p *Parser) currentDataHead() string {
	if len(p.dataHead) >= dataHeadBytes {
		return p.dataHead
	}
	if !strings.HasPrefix(p.lineHead, "data:") {
		return p.dataHead
	}
	tailData := stripOneLeadingSpace([]byte(p.lineHead[len("data:"):]))
	separator := ""
	if p.dataLines > 0 && len(p.dataHead) > 0 {
		separator = "\n"
	}
	head := p.dataHead + separator + string(tailData)
	if len(head) > dataHeadBytes {
		head = head[:dataHeadBytes]
	}
	return head
}

func (p *Parser) eventForCompletedLine() string {
	if !strings.HasPrefix(p.lineHead, "event:") {
		return p.currentEvent
	}
	return strings.TrimSpace(p.lineHead[len("event:"):])
}

func (p *Parser) completedLineSyntaxBytes() int {
	field := 0
	switch {
	case strings.HasPrefix(p.lineHead, "data:"):
		field = len("data:")
	case strings.HasPrefix(p.lineHead, "event:"):
		field = len("event:")
	default:
		return 0
	}
	if len(p.lineHead) > field && p.lineHead[field] == ' ' {
		field++
	}
	return field
}

func (p *Parser) resetRetainedState() {
	p.line = nil
	p.lineHead = ""
	p.currentEvent = ""
	p.data = nil
	p.dataLines = 0
	p.dataHead = ""
	p.skipLeadingLF = false
}

func (p *Parser) assertBufferLimit() error {
	return p.assertBufferLimitWith(p.currentEvent, p.currentDataHead(), 0, false)
}

func (p *Parser) assertBufferLimitWith(
	candidateEvent string,
	candidateHead string,
	ignoredLineSyntaxBytes int,
	replacesCurrentEvent bool,
) error {
	maxBufferedBytes := p.opts.MaxBufferedBytes
	if maxBufferedBytes <= 0 {
		return nil
	}

	bufferedBytes := len(p.line) - ignoredLineSyntaxBytes
	if bufferedBytes < 0 {
		bufferedBytes = 0
	}
	if !replacesCurrentEvent {
		bufferedBytes += len(p.currentEvent)
	}
	bufferedBytes += len(p.data)
	if bufferedBytes <= maxBufferedBytes {
		return nil
	}

	if exemption := p.opts.Exemption; exemption != nil &&
		bufferedBytes <= exemption.MaxBufferedBytes &&
		exemption.Matches != nil &&
		exemption.Matches(candidateEvent, candidateHead) {
		return nil
	}
	p.resetRetainedState()
	return &BufferLimitError{MaxBufferedBytes: maxBufferedBytes}
}

func hasFieldPrefix(line []byte, prefix string) bool {
	if len(line) < len(prefix) {
		return false
	}
	return string(line[:len(prefix)]) == prefix
}

// stripOneLeadingSpace 剥掉一个前导 ASCII 空白（TS 用 /\s/，见 doc.go 的差异说明）。
func stripOneLeadingSpace(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	switch data[0] {
	case ' ', '\t', '\v', '\f', '\r':
		return data[1:]
	default:
		return data
	}
}

// ParseBody 对完整 SSE body 一次性解析出全部帧。
func ParseBody(body string) ([]Frame, error) {
	parser := NewParser(ParserOptions{})
	frames, err := parser.PushText(body)
	if err != nil {
		return nil, err
	}
	// 与 TS 的 parseSseBody 一致：先冲刷输入，再冲刷尾部残行与未 dispatch 的帧。
	tail, err := parser.Finish()
	if err != nil {
		return nil, err
	}
	return append(frames, tail...), nil
}
