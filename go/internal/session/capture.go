package session

import (
	"bytes"
	"strconv"
)

// 本文件是**响应正文的有界捕获**：流式直通路径上唯一允许触碰正文的地方。
//
// 为什么需要它：数据面把上游响应逐块直传客户端（见 dataplane/stream.go），全程不保留副本，
// 于是详情页的 `response` 字段在纯 Go 世界里恒空。要在热路径上留下正文，就必须给「捕获」
// 划一条**与正文总大小无关**的内存上界。
//
// 定档（协调者）：头尾各 64 KiB 的有界窗口，超限即封顶并在正文里写内联截断标记。
// 故单流驻留 = 头窗口 + 尾缓冲，与流长无关：
//
//	头窗口   ≤ 64 KiB（只进不出）
//	尾缓冲   ≤ 128 KiB（摊还裁剪，见 Write）
//	单流合计 ≤ 192 KiB
//
// 与 Node 的**登记差异**：Node 的 storeSessionResponse 用
// SESSION_RESPONSE_BODY_MAX_BYTES（默认 5 MiB）判**整份**落或 `del` 键，超限的响应在详情页
// 是「没有正文」；本实现改为「头尾窗口 + 内联标记」，超限的响应在详情页是「能看到开头与结尾，
// 中间有一段明确的省略标记」。故 SESSION_RESPONSE_BODY_MAX_BYTES 不参与响应正文的落盘判定
// （保留在配置里只为与 Node 的配置面同构）。

// ResponseCaptureWindowBytes 是响应正文捕获的**单侧**窗口（头与尾各一份）。
const ResponseCaptureWindowBytes = 64 * 1024

// truncationMarkerPrefix/Suffix 是内联截断标记的两端。
//
// 为什么是内联文本而不是结构化字段：Node 的 `response` 在 Redis 里就是**裸字符串**（不是 JSON
// 对象），详情页把它原样展示。加一层结构会改变字段类型，让读侧与 Node 不再同形——比标记本身
// 更坏。标记进正文，类型保持字符串。
const (
	truncationMarkerPrefix = "\n\n[TRUNCATED: "
	truncationMarkerSuffix = " bytes omitted]\n\n"
)

// ResponseCapture 是响应正文的有界头尾捕获。
//
// 零值不可用，必须经 NewResponseCapture 构造。非并发安全：一次请求一个实例，只由交付出
// 客户端字节的那条 goroutine 写。
type ResponseCapture struct {
	head []byte
	tail []byte
	// total 是**看到过的**总字节数（不是保留的字节数），截断标记里的数字来自它。
	total int64
	// window 是单侧窗口；<=0 时取 ResponseCaptureWindowBytes。
	window int
}

// NewResponseCapture 构造一个捕获器。window <= 0 时取 ResponseCaptureWindowBytes。
func NewResponseCapture(window int) *ResponseCapture {
	if window <= 0 {
		window = ResponseCaptureWindowBytes
	}
	return &ResponseCapture{window: window}
}

// Write 旁路累加一段**已交付客户端**的字节。
//
// 为什么参数是「已交付客户端的字节」：Node 存的响应正文就是客户端可见文本（格式回译、
// 门控前缀都已包含）。捕获点放在写回之后，与 Node 同一口径。
//
// 内存：头窗口只进不出；尾缓冲允许涨到 2×window 再摊还裁掉前一半，故每块写入的搬运量摊还
// 到 O(len(p))——不能每块都从头部裁剪（8 MiB 流按 32 KiB 分块会有 256 次整体搬运）。
func (c *ResponseCapture) Write(p []byte) {
	if c == nil || len(p) == 0 {
		return
	}
	c.total += int64(len(p))
	if len(c.head) < c.window {
		take := c.window - len(c.head)
		if take > len(p) {
			take = len(p)
		}
		c.head = append(c.head, p[:take]...)
		p = p[take:]
		if len(p) == 0 {
			return
		}
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) > 2*c.window {
		trimmed := make([]byte, c.window)
		copy(trimmed, c.tail[len(c.tail)-c.window:])
		c.tail = trimmed
	}
}

// Total 返回看到过的总字节数。
func (c *ResponseCapture) Total() int64 {
	if c == nil {
		return 0
	}
	return c.total
}

// Empty 表示没有捕获到任何字节。
func (c *ResponseCapture) Empty() bool { return c == nil || c.total == 0 }

// Truncated 表示正文被窗口裁过（省略了多少字节由 Total 与保留量之差给出）。
func (c *ResponseCapture) Truncated() bool {
	if c == nil {
		return false
	}
	return c.total > int64(len(c.head)+len(c.tailKept()))
}

// tailKept 给出尾窗口里**真正保留**的那一段（可能比缓冲短——缓冲允许涨到 2×window）。
func (c *ResponseCapture) tailKept() []byte {
	if c == nil {
		return nil
	}
	if len(c.tail) <= c.window {
		return c.tail
	}
	return c.tail[len(c.tail)-c.window:]
}

// Bytes 给出要落盘的正文：头窗口 + 内联截断标记 + 尾窗口。
//
// 未截断时就是原正文（一次拼接），已截断时标记里的数字是**省略的字节数**（total 减去保留量），
// 让看详情的人能判断「还差多少」。
func (c *ResponseCapture) Bytes() []byte {
	if c.Empty() {
		return nil
	}
	tail := c.tailKept()
	if !c.Truncated() {
		out := make([]byte, 0, len(c.head)+len(tail))
		out = append(out, c.head...)
		out = append(out, tail...)
		return out
	}
	omitted := c.total - int64(len(c.head)+len(tail))
	var buffer bytes.Buffer
	buffer.Grow(len(c.head) + len(tail) + 48)
	buffer.Write(c.head)
	buffer.WriteString(truncationMarkerPrefix)
	buffer.WriteString(strconv.FormatInt(omitted, 10))
	buffer.WriteString(truncationMarkerSuffix)
	buffer.Write(tail)
	return buffer.Bytes()
}
