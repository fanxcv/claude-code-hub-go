package forward

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errPumpTestCleanup 是测试收尾时的取消归因（已定终态时它是空操作）。
var errPumpTestCleanup = errors.New("测试收尾")

// fakeSource 是可控的上游正文：按脚本逐次返回数据块，并统计读次数与关闭次数。
type fakeSource struct {
	chunks [][]byte
	index  int
	reads  atomic.Int64
	closed atomic.Int64
	// terminalErr 是脚本读尽后返回的错误；nil 表示 EOF。
	terminalErr error
	mu          sync.Mutex
}

func newFakeSource(chunks ...[]byte) *fakeSource {
	return &fakeSource{chunks: chunks}
}

func (f *fakeSource) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads.Add(1)
	if f.index >= len(f.chunks) {
		if f.terminalErr != nil {
			return 0, f.terminalErr
		}
		return 0, io.EOF
	}
	chunk := f.chunks[f.index]
	f.index++
	return copy(p, chunk), nil
}

func (f *fakeSource) Close() error {
	f.closed.Add(1)
	return nil
}

func (f *fakeSource) readCount() int64  { return f.reads.Load() }
func (f *fakeSource) closeCount() int64 { return f.closed.Load() }

// waitForCondition 在超时内等待条件成立，避免测试依赖固定睡眠。
func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待超时: %s", message)
}

// TestPumpReadsUpstreamOnDemand 断言泵只在被拉取时才读上游，且不会预读下一个 chunk。
func TestPumpReadsUpstreamOnDemand(t *testing.T) {
	source := newFakeSource([]byte("aaaa"), []byte("bbbb"), []byte("cccc"))
	pump := NewPump(PumpOptions{Source: source, ChunkBytes: 16})
	defer func() { pump.CancelSource(errPumpTestCleanup) }()

	if got := source.readCount(); got != 0 {
		t.Fatalf("构造后不应发生上游读，实际 %d 次", got)
	}

	buffer := make([]byte, 32)
	n, err := pump.Read(buffer)
	if err != nil {
		t.Fatalf("初次 Read 失败: %v", err)
	}
	if n != 4 || string(buffer[:n]) != "aaaa" {
		t.Fatalf("初次 Read = %q", buffer[:n])
	}
	// 关键不变量：交付一个 chunk 不等于可以预读下一个。
	if got := source.readCount(); got != 1 {
		t.Fatalf("交付首个 chunk 后上游读次数 = %d，期望 1", got)
	}

	n, err = pump.Read(buffer)
	if err != nil {
		t.Fatalf("再次 Read 失败: %v", err)
	}
	if n != 4 || string(buffer[:n]) != "bbbb" {
		t.Fatalf("再次 Read = %q", buffer[:n])
	}
	if got := source.readCount(); got != 2 {
		t.Fatalf("两次按需读取后上游读次数 = %d，期望 2", got)
	}
}

// TestPumpKeepsSinglePendingChunk 断言下游缓冲小时不会触发额外上游读。
func TestPumpKeepsSinglePendingChunk(t *testing.T) {
	source := newFakeSource([]byte("0123456789"))
	pump := NewPump(PumpOptions{Source: source, ChunkBytes: 16})
	defer func() { pump.CancelSource(errPumpTestCleanup) }()

	buffer := make([]byte, 4)
	if _, err := pump.Read(buffer); err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	if got := source.readCount(); got != 1 {
		t.Fatalf("上游读次数 = %d，期望 1", got)
	}
	// pending 仍有 6 字节：继续交付不应再读上游。
	if _, err := pump.Read(buffer); err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	if got := source.readCount(); got != 1 {
		t.Fatalf("pending 未取尽就发生了额外上游读（%d 次）", got)
	}
}

// TestPumpPendingDeadlineCancelsStalledConsumer 断言下游不取走 pending 时按超时取消。
func TestPumpPendingDeadlineCancelsStalledConsumer(t *testing.T) {
	source := newFakeSource([]byte("0123456789"), []byte("unreached"))
	pump := NewPump(PumpOptions{
		Source:               source,
		ChunkBytes:           16,
		PendingChunkDeadline: 20 * time.Millisecond,
	})

	if _, err := pump.Read(make([]byte, 4)); err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	completion := pump.Completion()
	if completion.StreamEndedNormally {
		t.Fatal("pending 超时不应算正常结束")
	}
	if completion.Err == nil {
		t.Fatal("pending 超时应带错误")
	}
	if completion.ClientAborted {
		t.Fatal("下游不消费不等于客户端中断，归因必须区分")
	}
	waitForCondition(t, time.Second, func() bool { return source.closeCount() > 0 }, "上游未被关闭")
}

// TestPumpClientCancelDrainsToEOF 断言取消后引流会读到上游 EOF，并如实区分「客户端先走」与「上游结束」。
//
// 两者的组合就是 Node 侧判定「上游其实完整结束、只是客户端先断开」的依据，
// 因此引流读到 EOF 时终态是 streamEndedNormally=true + clientAborted=true，错误为空。
func TestPumpClientCancelDrainsToEOF(t *testing.T) {
	source := newFakeSource([]byte("aaaa"), []byte("bbbb"), []byte("cccc"))
	var observed atomic.Int64
	pump := NewPump(PumpOptions{
		Source:     source,
		ChunkBytes: 16,
		OnChunk:    func([]byte) { observed.Add(1) },
	})
	defer func() { pump.CancelSource(errPumpTestCleanup) }()

	if _, err := pump.Read(make([]byte, 4)); err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	pump.ClientCancel(errors.New("客户端断开"))

	waitForCondition(t, time.Second, func() bool { return source.closeCount() > 0 }, "上游未被关闭")
	completion := pump.Completion()
	if !completion.ClientAborted {
		t.Fatal("终态应标记客户端中断")
	}
	if !completion.StreamEndedNormally || completion.Err != nil {
		t.Fatalf("引流读到 EOF 的终态 = %+v，期望正常结束且无错误", completion)
	}
	// 引流读到剩余两个 chunk，计量不缺尾部现场。
	if got := observed.Load(); got < 3 {
		t.Fatalf("引流只观测到 %d 个 chunk，期望读完全部 3 个", got)
	}
	// 下游已不再是消费方：继续 Read 不能拿到数据（引流中报 ErrPumpDraining，
	// 引流已结束则报 EOF，两者都不允许是正文）。
	n, err := pump.Read(make([]byte, 4))
	if n != 0 || (!errors.Is(err, ErrPumpDraining) && !errors.Is(err, io.EOF)) {
		t.Fatalf("取消后的 Read = %d, %v", n, err)
	}
}

// TestPumpClientCancelWithCancelSourceKeepsReason 断言取消并显式取消上游时保留归因。
func TestPumpClientCancelWithCancelSourceKeepsReason(t *testing.T) {
	source := &hangingCloseSource{release: make(chan struct{})}
	pump := NewPump(PumpOptions{Source: source, ChunkBytes: 16})
	defer close(source.release)

	reason := errors.New("客户端断开")
	pump.ClientCancel(reason)
	if !pump.CancelSource(reason) {
		t.Fatal("取消上游应赢得终态")
	}
	completion := pump.Completion()
	if !completion.ClientAborted {
		t.Fatal("终态应标记客户端中断")
	}
	if completion.StreamEndedNormally {
		t.Fatal("显式取消不应算正常结束")
	}
	if !errors.Is(completion.Err, reason) {
		t.Fatalf("终态错误 = %v，期望 %v", completion.Err, reason)
	}
}

// TestPumpSettleIsIdempotent 断言唯一终态：后到的触发源不会改写归因。
func TestPumpSettleIsIdempotent(t *testing.T) {
	source := newFakeSource([]byte("aaaa"), []byte("bbbb"))
	pump := NewPump(PumpOptions{Source: source, ChunkBytes: 16})
	defer func() { pump.CancelSource(errPumpTestCleanup) }()

	if _, err := pump.Read(make([]byte, 4)); err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	first := pump.CancelSource(errors.New("首个归因"))
	second := pump.CancelSource(errors.New("迟到的归因"))
	if !first {
		t.Fatal("首个触发源应赢得终态")
	}
	if second {
		t.Fatal("迟到触发源不应再赢得终态")
	}
	if err := pump.Completion().Err; err == nil || err.Error() != "首个归因" {
		t.Fatalf("终态归因被改写: %v", err)
	}
}

// TestPumpEOFIsNormalCompletion 断言上游 EOF 是正常结束。
func TestPumpEOFIsNormalCompletion(t *testing.T) {
	source := newFakeSource([]byte("aaaa"))
	pump := NewPump(PumpOptions{Source: source, ChunkBytes: 16})
	defer func() { pump.CancelSource(errPumpTestCleanup) }()

	buffer := make([]byte, 8)
	if n, err := pump.Read(buffer); err != nil || n != 4 {
		t.Fatalf("Read = %d, %v", n, err)
	}
	if n, err := pump.Read(buffer); !errors.Is(err, io.EOF) || n != 0 {
		t.Fatalf("结束读取 = %d, %v", n, err)
	}
	completion := pump.Completion()
	if !completion.StreamEndedNormally || completion.Err != nil {
		t.Fatalf("终态 = %+v", completion)
	}
}

// TestPumpUpstreamErrorIsTerminal 断言上游读错误直接成终态并交给下游。
func TestPumpUpstreamErrorIsTerminal(t *testing.T) {
	upstreamErr := errors.New("上游连接被重置")
	source := newFakeSource([]byte("aaaa"))
	source.terminalErr = upstreamErr
	pump := NewPump(PumpOptions{Source: source, ChunkBytes: 16})
	defer func() { pump.CancelSource(errPumpTestCleanup) }()

	buffer := make([]byte, 8)
	if _, err := pump.Read(buffer); err != nil {
		t.Fatalf("首个 Read 失败: %v", err)
	}
	if _, err := pump.Read(buffer); !errors.Is(err, upstreamErr) {
		t.Fatalf("错误未被交给下游: %v", err)
	}
	if !errors.Is(pump.Completion().Err, upstreamErr) {
		t.Fatalf("终态错误 = %v", pump.Completion().Err)
	}
	if pump.Completion().StreamEndedNormally {
		t.Fatal("上游错误不应算正常结束")
	}
}

// TestPumpMarksClientAbortWhenSourceReadFailsWithClientCancel 断言「上游读阻塞中客户端撤 ctx」
// 归因为**客户端中断**，而不是本地/上游错误。
//
// 生产实证（2026-09-23，session 01a0b3af…）：这条路径不经过 ClientCancel，只靠它置位的
// 归因会漏，终态落 TerminalLocalError，亲和侧随即按「供应商故障」写 60 秒会话冷却，
// 把健康渠道上的会话赶到优先级最高的另一家。
func TestPumpMarksClientAbortWhenSourceReadFailsWithClientCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := newFakeSource()
	source.terminalErr = context.Canceled
	pump := NewPump(PumpOptions{Source: source, ChunkBytes: 16, ClientCtx: ctx})
	defer func() { pump.CancelSource(errPumpTestCleanup) }()

	cancel()
	if _, err := pump.Read(make([]byte, 8)); !errors.Is(err, context.Canceled) {
		t.Fatalf("错误未被交给下游: %v", err)
	}
	completion := pump.Completion()
	if !completion.ClientAborted {
		t.Fatal("客户端撤 ctx 后终态应记为客户端中断")
	}
	if completion.StreamEndedNormally {
		t.Fatal("客户端中断不应算正常结束")
	}
}

// TestPumpKeepsNormalCompletionWhenClientCancelsLate 断言终态标记已到之后的客户端断开
// 不改归因：那是「协议终态之后断开仍算成功」的一半，反过来会让真成功被记成中断。
func TestPumpKeepsNormalCompletionWhenClientCancelsLate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := newFakeSource([]byte("aaaa"))
	pump := NewPump(PumpOptions{Source: source, ChunkBytes: 16, ClientCtx: ctx})
	defer func() { pump.CancelSource(errPumpTestCleanup) }()

	buffer := make([]byte, 8)
	if n, err := pump.Read(buffer); err != nil || n != 4 {
		t.Fatalf("Read = %d, %v", n, err)
	}
	cancel()
	if n, err := pump.Read(buffer); !errors.Is(err, io.EOF) || n != 0 {
		t.Fatalf("结束读取 = %d, %v", n, err)
	}
	completion := pump.Completion()
	if !completion.StreamEndedNormally || completion.ClientAborted {
		t.Fatalf("终态 = %+v", completion)
	}
}

// TestPumpTeardownDoesNotWaitForHangingClose 断言本地回收不被挂起的上游关闭阻塞。
func TestPumpTeardownDoesNotWaitForHangingClose(t *testing.T) {
	source := &hangingCloseSource{release: make(chan struct{})}
	pump := NewPump(PumpOptions{Source: source, ChunkBytes: 16})

	pump.CancelSource(errors.New("取消"))
	select {
	case <-pump.Teardown():
	case <-time.After(time.Second):
		t.Fatal("teardown 不应等待上游 Close 返回")
	}
	close(source.release)
}

// hangingCloseSource 的 Close 会一直阻塞，直到测试放行。
type hangingCloseSource struct {
	release chan struct{}
}

func (h *hangingCloseSource) Read([]byte) (int, error) {
	<-h.release
	return 0, io.EOF
}

func (h *hangingCloseSource) Close() error {
	<-h.release
	return nil
}
