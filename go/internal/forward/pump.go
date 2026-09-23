package forward

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件实现按需拉取的响应泵，对应 Node 的 demand-driven-response-pump.ts。
//
// 三条不变量（都是内存与生命周期结论，不是性能微调）：
//
//  1. 按需拉取：只有下游发起 Read 才向上游读一次，且同时至多驻留一个上游 chunk。
//     上游 chunk 的切分由网络决定；允许预读就会让「下游很慢」变成「进程持有整条流」。
//  2. 唯一终态：settle 幂等并返回本次调用是否赢得终态。客户端中断、静默超时、
//     上游错误可能同时发生，只有第一个赢家的归因能进入结算。
//  3. 本地回收不等待上游取消：teardown 的条件是「本泵不再读上游」，而不是上游
//     Close 返回。挂起的 Close 会永久保留 session、listener 与上游连接配额。

// PumpState 是泵的状态，取值与 TS 的 DemandDrivenResponsePumpState 一致。
type PumpState string

const (
	// PumpClientActive 表示下游正在消费。
	PumpClientActive PumpState = "client-active"
	// PumpDraining 表示下游已不消费，泵继续读上游只为 O(1) 计量。
	PumpDraining PumpState = "draining"
	// PumpFinalizing 表示终态已定、正在释放资源。
	PumpFinalizing PumpState = "finalizing"
	// PumpClosed 表示终态已定且本地资源已释放。
	PumpClosed PumpState = "closed"
)

const (
	// DefaultPendingChunkDeadline 与 TS 的 PENDING_CHUNK_DEADLINE_MS 一致：下游拿到
	// pending 却不取走，超过此时限即按「客户端不再消费」处理。
	DefaultPendingChunkDeadline = 60 * time.Second
	// DefaultPumpChunkBytes 是单次上游读的缓冲区大小。
	DefaultPumpChunkBytes = 32 << 10
)

// ErrPumpDraining 表示泵已进入引流：下游不再是本次响应的消费方。
var ErrPumpDraining = errors.New("forward: 响应泵已进入引流，下游不再消费")

// PumpCompletion 是泵的终态。
type PumpCompletion struct {
	// StreamEndedNormally 为真表示上游读到 EOF 结束。
	StreamEndedNormally bool
	// ClientAborted 为真表示终态由下游取消触发。
	ClientAborted bool
	// Err 是终态错误；正常结束为 nil。
	Err error
}

// PumpOptions 描述一个响应泵。
type PumpOptions struct {
	// Source 是上游正文。泵接管其所有权：终态时由泵负责关闭。
	Source io.ReadCloser
	// OnReadStart 在每次真正向上游发起读之前回调一次。调用方用它武装静默计时器：
	// pending 未被取走不算上游静默，只有真正发起读之后的等待才算。
	OnReadStart func()
	// OnChunk 在每个非空 chunk 到达时回调一次（含引流期间读到的 chunk），供 O(1) 观测。
	OnChunk func(chunk []byte)
	// OnClientCancel 在下游取消且终态尚未定时回调一次。
	OnClientCancel func(reason error)
	// ChunkBytes 是单次上游读缓冲区大小；<=0 取 DefaultPumpChunkBytes。
	ChunkBytes int
	// PendingChunkDeadline 是 pending chunk 未被下游取走的时限；0 取默认 60s，负值关闭。
	PendingChunkDeadline time.Duration
	// ClientCtx 是下游请求的上下文；nil 表示调用方不提供归因依据。
	//
	// 它只用于一处归因修正，见 settle：客户端在上游读阻塞中断开时，错误对象只会是
	// context.Canceled（不是写回失败），单靠 ClientCancel 置位会漏掉这条路径。
	ClientCtx context.Context
	// Logger 用于记录 pending 超时；nil 时静默。
	Logger *logx.Logger
}

// Pump 是按需拉取的响应泵，可被下游 goroutine 与引流 goroutine 同时使用。
type Pump struct {
	opts PumpOptions

	// readMu 串行化上游读：Read 与引流 goroutine 共享同一个 reader。
	readMu sync.Mutex

	mu            sync.Mutex
	state         PumpState
	settled       bool
	clientAborted bool
	pending       []byte
	buf           []byte
	completion    PumpCompletion
	sourceClosed  bool
	deadline      *time.Timer

	done     chan struct{}
	teardown chan struct{}
}

// NewPump 构造响应泵。Source 为空时返回一个已正常结束的泵。
func NewPump(opts PumpOptions) *Pump {
	if opts.ChunkBytes <= 0 {
		opts.ChunkBytes = DefaultPumpChunkBytes
	}
	if opts.PendingChunkDeadline == 0 {
		opts.PendingChunkDeadline = DefaultPendingChunkDeadline
	}
	pump := &Pump{
		opts:     opts,
		state:    PumpClientActive,
		buf:      make([]byte, opts.ChunkBytes),
		done:     make(chan struct{}),
		teardown: make(chan struct{}),
	}
	if opts.Source == nil {
		pump.settle(true, nil, nil)
	}
	return pump
}

// Read 交付下游可见的字节：先交付 pending chunk，用尽后才向上游读一次。
//
// 返回值：
//   - 读到数据：n > 0，err 为 nil。
//   - 上游正常结束：0, io.EOF。
//   - 上游错误或下游取消：0, 对应错误（与 Completion().Err 同值）。
func (p *Pump) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for {
		p.mu.Lock()
		if len(p.pending) > 0 {
			n := copy(b, p.pending)
			p.pending = p.pending[n:]
			if len(p.pending) == 0 {
				p.pending = nil
				p.clearDeadlineLocked()
			}
			p.mu.Unlock()
			return n, nil
		}
		state, completion := p.state, p.completion
		p.mu.Unlock()

		switch state {
		case PumpClientActive:
			if err := p.readSourceOnce(); err != nil {
				// 终态已定（EOF 或其他错误）：下一轮循环按终态返回。
				continue
			}
			if !p.hasPending() {
				// 上游返回 (0, nil)：合法但无数据。让出时间片，避免病态 reader 把这里变成忙等。
				runtime.Gosched()
			}
		case PumpDraining:
			return 0, ErrPumpDraining
		default:
			if completion.Err != nil {
				return 0, completion.Err
			}
			return 0, io.EOF
		}
	}
}

// hasPending 报告是否仍有未交付的上游数据。
func (p *Pump) hasPending() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending) > 0
}

// State 返回当前状态。
func (p *Pump) State() PumpState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// WasClientAborted 报告终态是否由下游取消触发。
func (p *Pump) WasClientAborted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clientAborted
}

// Done 在终态确定时关闭。
func (p *Pump) Done() <-chan struct{} { return p.done }

// Teardown 在本地资源释放完成时关闭，不等待上游 Close 返回。
func (p *Pump) Teardown() <-chan struct{} { return p.teardown }

// Completion 阻塞到终态确定并返回终态。
func (p *Pump) Completion() PumpCompletion {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.completion
}

// ClientCancel 处理下游取消：回调一次并以「客户端中断」为归因开始引流。
//
// 引流会继续按需读上游（OnChunk 仍被调用），供断线计量拿到终态 usage；
// 归零由 FinishDrain / CancelSource 决定（见 Stream 的计量预算策略）。
func (p *Pump) ClientCancel(reason error) {
	p.mu.Lock()
	if p.settled || p.state == PumpFinalizing || p.state == PumpClosed {
		p.mu.Unlock()
		return
	}
	first := !p.clientAborted
	p.clientAborted = true
	p.mu.Unlock()

	if first && p.opts.OnClientCancel != nil {
		p.opts.OnClientCancel(reason)
	}
	p.startDrain(reason, true)
}

// FinishDrain 在引流计量拿到终态后立即结束本次流：不算上游错误，也不算正常结束。
//
// 它让引流不必等到上游真的关闭连接：终态帧已经到手，继续等只是在占着配额。
// 单次引流的兜底上限仍由调用方的引流超时决定。
func (p *Pump) FinishDrain(reason error) {
	p.mu.Lock()
	if p.settled || p.state != PumpDraining {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.settle(false, nil, reason)
}

// CancelSource 取消上游并以给定原因定下终态；返回本次调用是否赢得唯一终态。
func (p *Pump) CancelSource(reason error) bool {
	return p.settle(false, reason, reason)
}

func (p *Pump) startDrain(reason error, markClientAborted bool) {
	p.mu.Lock()
	if p.settled || p.state == PumpFinalizing || p.state == PumpClosed {
		p.mu.Unlock()
		return
	}
	if markClientAborted {
		p.clientAborted = true
	}
	if p.state == PumpDraining {
		p.mu.Unlock()
		return
	}
	p.state = PumpDraining
	p.pending = nil
	p.clearDeadlineLocked()
	p.mu.Unlock()

	go p.drain()
}

// drain 在引流期间持续读上游：只保留 O(1) 状态，正文一律丢弃。
func (p *Pump) drain() {
	for {
		p.mu.Lock()
		if p.settled || p.state != PumpDraining {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		if err := p.readSourceOnce(); err != nil {
			return
		}
	}
}

// readSourceOnce 向上游读一次。
//
// 交付路径把读到的 chunk 记为 pending（至多一个）；引流路径只交给 OnChunk 观测后丢弃。
// 返回非 nil 表示本次读没有产生可交付数据（EOF 或错误，终态已定）。
func (p *Pump) readSourceOnce() error {
	p.readMu.Lock()
	defer p.readMu.Unlock()

	p.mu.Lock()
	if p.settled || (p.state != PumpClientActive && p.state != PumpDraining) {
		completion := p.completion
		p.mu.Unlock()
		if completion.Err != nil {
			return completion.Err
		}
		return io.EOF
	}
	source := p.opts.Source
	buffer := p.buf
	onReadStart := p.opts.OnReadStart
	p.mu.Unlock()

	if onReadStart != nil {
		onReadStart()
	}
	n, err := source.Read(buffer)
	if n > 0 {
		chunk := buffer[:n]
		if p.opts.OnChunk != nil {
			p.opts.OnChunk(chunk)
		}

		p.mu.Lock()
		if p.settled || p.state != PumpClientActive {
			// 引流路径（或已定终态）：计量已做完，正文不保留。
			p.mu.Unlock()
			return nil
		}
		p.pending = chunk
		p.armDeadlineLocked()
		p.mu.Unlock()
		return nil
	}
	if err == nil {
		// n == 0 且无错误：上游暂时无数据，下一次 Read 继续拉。
		return nil
	}

	if errors.Is(err, io.EOF) {
		p.settle(true, nil, nil)
	} else {
		p.settle(false, err, err)
	}
	return err
}

// armDeadlineLocked 武装 pending 超时；调用方必须持有 p.mu。
func (p *Pump) armDeadlineLocked() {
	if p.opts.PendingChunkDeadline < 0 {
		return
	}
	p.clearDeadlineLocked()
	p.deadline = time.AfterFunc(p.opts.PendingChunkDeadline, func() {
		reason := errors.New("forward: 下游未在时限内取走已缓冲的上游数据块")
		if p.opts.Logger != nil {
			p.opts.Logger.Warn("forward: 响应泵 pending 超时", map[string]any{
				"deadline_ms": p.opts.PendingChunkDeadline.Milliseconds(),
			})
		}
		// 先转引流再取消：与 TS 的 startDrain(error, false) + cancelSource(error) 同序，
		// 且不把归因写成「客户端中断」——真正的原因是下游不消费。
		p.startDrain(reason, false)
		p.CancelSource(reason)
	})
}

func (p *Pump) clearDeadlineLocked() {
	if p.deadline == nil {
		return
	}
	p.deadline.Stop()
	p.deadline = nil
}

// settle 定下唯一终态；返回本次调用是否赢得终态。
func (p *Pump) settle(normal bool, err error, cancelReason error) bool {
	p.mu.Lock()
	if p.settled {
		p.mu.Unlock()
		return false
	}
	// 归因修正：上游读阻塞中客户端断开时，net/http 撤掉请求 ctx，上游读即以
	// context.Canceled 出错，而这条路径**不经过** ClientCancel（置 clientAborted 的唯一入口），
	// 故终态会落 TerminalLocalError —— 亲和侧据此按「供应商故障」写 60 秒会话冷却，
	// 把健康渠道上的会话无故赶走（生产实证 2026-09-23：session 01a0b3af…）。
	//
	// 只认 context.Canceled（不是 DeadlineExceeded）：上游静默由 IdleTimeout 这条独立通道
	// 归因，本仓不给请求 ctx 加时限，故 Canceled 只能是下游断开。
	// 正常结束分支不修正：终态标记已到，客户端事后断开仍算成功（见 affinity 的墓碑判定）。
	// 客户端与上游同时出事的极端情形按「客户端中断」记——错误方向取少记一次失败。
	if !normal && p.opts.ClientCtx != nil && errors.Is(p.opts.ClientCtx.Err(), context.Canceled) {
		p.clientAborted = true
	}
	p.settled = true
	p.state = PumpFinalizing
	p.pending = nil
	p.clearDeadlineLocked()
	completion := PumpCompletion{
		StreamEndedNormally: normal,
		ClientAborted:       p.clientAborted,
		Err:                 err,
	}
	p.completion = completion
	source := p.opts.Source
	alreadyClosed := p.sourceClosed
	p.sourceClosed = true
	p.mu.Unlock()

	if !alreadyClosed && source != nil {
		if cancelReason != nil {
			// 取消路径：关闭上游只是尽力而为，绝不让它阻塞本地回收。
			go func() { _ = source.Close() }()
		} else {
			_ = source.Close()
		}
	}

	p.mu.Lock()
	p.state = PumpClosed
	p.mu.Unlock()
	close(p.done)
	close(p.teardown)
	return true
}
