package dataplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// 流式写回：逐块读、逐块写、逐块 flush。
//
// 为什么必须逐块 flush：客户端的 SSE 解析器依赖「一帧一到」；若把整条流缓冲到 handler
// 返回（默认行为），TTFT 就退化成总时长，且前端会出现「一次性吐出全部内容」的表象。
//
// 内存纪律（本文件的全部存在理由）：缓冲区是每请求固定的 32 KiB，正文驻留量因此等于
// 在途 chunk，与正文总大小无关。**任何在本函数里累积正文的改动都是回归**——
// 8 MiB 流单流驻留小于 1 MiB 这条不变量（见 forward 的流式测试）也是本层的契约。
const pumpChunkBytes = forward.DefaultPumpChunkBytes

// settleBarrierTimeout 是「等本流终态落库」屏障的上限。
//
// 为什么是有限值：客户端中断计量会刻意继续引流上游（默认上限 60s）以拿真实用量，
// 那是设计上的游离行为；屏障若无限等待，断线风暴会把滚动重启拖满整个排空窗口，
// 而排空窗口之外的等待并不提高终态存活率（关依赖仍在窗口之后）。
// 超过上限的部分交给 settlementTracker：退出序列在关依赖之前会再等一次（有界）。
const settleBarrierTimeout = 5 * time.Second

// pumpStream 把已提交的流交付给客户端。
func (h *Handler) pumpStream(
	writer http.ResponseWriter,
	request *http.Request,
	result *forward.StreamResult,
	state *RequestState,
) {
	stream := result.Stream
	// 响应修复器（流式分支）：同样在协议转换**之前**动字节，且按「完整行」切分——
	// 一句帧可能被上游切在两个 chunk 之间，逐块修会把半个 JSON 当完整载荷补括号。
	fixer := h.newResponseFixStream(request.Context(), state, result.Headers)
	// 响应侧方言回译：上游说的可能是目标线方言，客户端按自己的方言解析。转换在建 spool 之前
	// 建好，因为喂给 spool 的必须是**客户端可见字节**（与 Node 的客户端可见文本同一口径）。
	var converter *convert.StreamPipe
	if state != nil && !hasOpaqueContentEncoding(result.Headers) {
		conversion := newResponseConversion(result.Plan, state.Format, state.Model, true)
		if pipe, ok := conversion.newStreamPipe(); ok {
			converter = pipe
		}
	}
	// 回放 owner：建 spool 必须在**第一字节写回客户端之前**，否则首帧之后的字节会永远
	// 不在缓存里（重放命中时客户端就看到截断的流）。建不成（并发上限/不匹配/开关关）
	// 由 startStream 自己释放 owner 租约，本层不再重试。
	var replayOwner *replaySession
	if state != nil {
		replayOwner = state.replay
	}
	if replayOwner != nil {
		replayOwner.startStream(request.Context(), result.StatusCode, result.Headers)
	}
	// 不登记、不等，进程一旦在扇出完成前退出（滚动重启正是断线风暴的窗口），
	// 未发出的终态 UPDATE 就随进程消失，账本行永久留在 status_code IS NULL。
	pending := h.settlements.begin(func() { stream.Completion() })
	defer func() {
		if err := stream.Close(); err != nil {
			h.logger.Debug("dataplane.stream_close_failed", map[string]any{"error": err.Error()})
		}
		if !pending.await(settleBarrierTimeout) {
			// 超时不是失败：终态仍归 settlementTracker 管，退出序列会在关依赖前再等一次。
			h.logger.Warn("dataplane.settle_barrier_timeout", map[string]any{
				"path":    request.URL.Path,
				"timeout": settleBarrierTimeout.Milliseconds(),
			})
		}
	}()

	copyUpstreamHeaders(writer.Header(), result.Headers)
	// 流式响应长度未知：必须抹掉上游可能带来的长度（门控前缀会改变实际字节数）。
	writer.Header().Del("Content-Length")

	status := result.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	// response.after 的两项事实（交付头与状态码）在写头之前采：写完就取不到 writer 上的头了。
	// 上游原始头（response.before）由 forward 在收到响应头时已记下，这里不覆盖它。
	h.recordDeliveredHeaders(state, writer.Header())
	h.recordStatus(state, status)
	writer.WriteHeader(status)

	flusher, canFlush := writer.(http.Flusher)
	if !canFlush {
		// 不 flush 的包装层会把流退化成「等全部读完才发」：如实记一条 warn，便于排障。
		h.logger.Warn("dataplane.stream_not_flushable", map[string]any{"path": request.URL.Path})
	} else {
		// 先 flush 一次响应头：客户端据此立刻拿到 200 与空正文，TTFB 不再被首帧拖住。
		flusher.Flush()
	}

	// emit 把一段客户端可见字节写回并喂 spool；返回 false 表示客户端已断。
	emit := func(payload []byte) bool {
		if len(payload) == 0 {
			return true
		}
		if _, writeErr := writer.Write(payload); writeErr != nil {
			// 写失败即客户端已断：交给流的引流预算决定「继续计量」还是「立刻取消上游」。
			stream.ClientCancel(writeErr)
			h.logger.Debug("dataplane.stream_write_failed", map[string]any{"error": writeErr.Error()})
			return false
		}
		if canFlush {
			flusher.Flush()
		}
		// 喂给 spool 的是「已写给客户端的字节」，与 Node 的客户端可见文本同一口径：
		// 门控前缀、转换与修正器都已包含在该字节流里。
		if replayOwner != nil {
			replayOwner.observe(payload)
		}
		// 会话详情的有界捕获（头尾窗口）旁路累加同一批「客户端可见字节」：与 Node 的
		// storeSessionResponseBodySet 同一口径。它**不缓冲整流**——只搬运进有界窗口，
		// 8 MiB 流下驻留恒 ≤ 192 KiB（见 session/capture.go 的不变量与驻留测试）。
		if state != nil && state.responseCapture != nil {
			state.responseCapture.Write(payload)
		}
		return true
	}

	buffer := make([]byte, pumpChunkBytes)
	for {
		count, err := stream.Read(buffer)
		if count > 0 {
			payload := buffer[:count]
			if fixer != nil {
				// 修复器按行边界吐出：本块未构成完整行时返回空，此时不往下走（不缓冲正文）。
				payload = fixer.Write(payload)
			}
			if converter != nil {
				// 逐帧回译：本块尚未构成完整帧时产出为空，不缓冲正文。
				payload = converter.Push(payload)
			}
			if !emit(payload) {
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// 上游正常结束：先吐净修复器的残留字节（未以换行结尾的最后一行），
				// 再让转换器吐净残留帧并补发客户端线的终止事件。
				if fixer != nil {
					if tail := fixer.Flush(); len(tail) > 0 {
						if converter != nil {
							tail = converter.Push(tail)
						}
						if !emit(tail) {
							return
						}
					}
					h.recordResponseFixAudit(request.Context(), state, fixer.Audit())
				}
				if converter != nil {
					if tail := converter.Flush(); !emit(tail) {
						return
					}
				}
			}
			if !errors.Is(err, io.EOF) {
				// 非 EOF 的读错误由流终态归因与结算，本层只留痕。截断的流不补终止事件：
				// 伪造一个 message_stop 会让客户端把半截内容当成完整回答。
				h.logger.Debug("dataplane.stream_read_failed", map[string]any{"error": err.Error()})
			}
			return
		}
		if request.Context().Err() != nil {
			stream.ClientCancel(request.Context().Err())
			return
		}
	}
}

// settlementTracker 统计「已交付客户端、但终态尚未落库」的流数。
//
// 与 egress 在途计数的分工：在途计数跟请求走（handler 返回即归零），本计数跟结算走
// （可能比请求活得久）。两者必须都归零，退出序列才能关依赖——关连接池会把尚未发出的
// 终态 UPDATE 一并带走。
type settlementTracker struct {
	mu    sync.Mutex
	cond  *sync.Cond
	count int64
}

func newSettlementTracker() *settlementTracker {
	tracker := &settlementTracker{}
	tracker.cond = sync.NewCond(&tracker.mu)
	return tracker
}

// begin 登记一次待落库终态；await 返回（该流的终态已落库）时递减。
func (t *settlementTracker) begin(await func()) *pendingSettlement {
	pending := &pendingSettlement{tracker: t, done: make(chan struct{})}
	t.mu.Lock()
	t.count++
	t.mu.Unlock()
	go func() {
		await()
		pending.finish()
	}()
	return pending
}

// pending 返回尚未落库的终态数，供退出序列如实上报。
func (t *settlementTracker) pending() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count
}

// waitEmpty 等到没有待落库终态；ctx 先结束返回 false，调用方必须如实上报未完成数。
//
// 用一个 goroutine 把 ctx 结束转成 Broadcast：Cond 无法被 ctx 唤醒
// （与 egress 的排空等待同形）。
func (t *settlementTracker) waitEmpty(ctx context.Context) bool {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			t.mu.Lock()
			t.cond.Broadcast()
			t.mu.Unlock()
		case <-stop:
		}
	}()

	t.mu.Lock()
	defer t.mu.Unlock()
	for t.count > 0 {
		if ctx.Err() != nil {
			return false
		}
		t.cond.Wait()
	}
	return ctx.Err() == nil
}

// pendingSettlement 是一次尚未落库的流终态。
type pendingSettlement struct {
	tracker *settlementTracker
	done    chan struct{}
	once    sync.Once
}

// await 等终态落库；超时返回 false（屏障只影响 goroutine 生命周期，不阻塞客户端）。
func (p *pendingSettlement) await(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

func (p *pendingSettlement) finish() {
	p.once.Do(func() {
		close(p.done)
		p.tracker.mu.Lock()
		defer p.tracker.mu.Unlock()
		if p.tracker.count > 0 {
			p.tracker.count--
		}
		if p.tracker.count == 0 {
			p.tracker.cond.Broadcast()
		}
	})
}
