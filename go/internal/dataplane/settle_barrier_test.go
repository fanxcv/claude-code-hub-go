package dataplane

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
)

// 本文件钉住「结算屏障」这条不变量：**handler 返回之前，本流的终态必须已经落库**。
//
// 修复前的真实缺口：结算跑在 forward 的游离协程里，
// handler 返回即请求生命周期结束、前门在途计数归零；进程若在扇出完成前退出（滚动重启正是
// 与断线风暴重叠的窗口），未发出的终态 UPDATE 随进程消失，账本行永久留在 status_code IS NULL。
//
// 做法：把「结算慢下来」变成可注入的确定性事实（假结算器睡 800ms），于是
// 「handler 等没等」不再靠时序碰运气——不等就一定在睡醒前返回。

// settleTestDelay 是假结算器的睡眠时长：必须显著大于「客户端收到首帧」与「上游结束」的时间，
// 否则屏障是否生效会被时序掩盖。
const settleTestDelay = 800 * time.Millisecond

// newSettleTestHandler 造一个数据面：假上游立刻发完三段 SSE 并结束，假结算器故意慢。
func newSettleTestHandler(t *testing.T, hold <-chan struct{}) (*Handler, *fakeSettler) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// 首帧必须是决定性内容帧：门控只在看到真实内容或错误时才提交。
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		_, _ = io.WriteString(w,
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"甲\"}}\n\n")
		flusher.Flush()
		if hold != nil {
			// 上游挂住：让测试有机会在流中途断开客户端。
			<-hold
		}
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	handler, settler, _ := newTestHandler(t, upstream.URL, fakeAuth{
		user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
		key:  guard.Key{ID: 2, Name: "k", UserID: 1},
	})
	settler.delay = settleTestDelay
	return handler, settler
}

// settleTestServer 起一个能观测「handler 何时返回」的服务器。
func settleTestServer(t *testing.T, handler *Handler) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		close(handlerDone)
	}))
	t.Cleanup(server.Close)
	return server, handlerDone
}

// settleTestStreamRequest 发一条流式请求。
func settleTestStreamRequest(t *testing.T, server *httptest.Server) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d", response.StatusCode)
	}
	return response
}

// waitHandlerDone 等 handler 返回；超时即失败。
func waitHandlerDone(t *testing.T, done <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("handler 未在 %v 内返回", timeout)
	}
}

// 正常读完：客户端读到 EOF、handler 返回时，终态必须已经落库。
//
// 这条断言在修复前会红：handler 在流结束（EOF）时就返回，而结算此时还在后者后面排队。
func TestHandlerWaitsForSettlementBeforeReturning(t *testing.T) {
	handler, settler := newSettleTestHandler(t, nil)
	server, handlerDone := settleTestServer(t, handler)

	response := settleTestStreamRequest(t, server)
	defer func() { _ = response.Body.Close() }()

	// 首帧不能被屏障拖住：屏障只在写回函数的出口处生效，响应头与首帧照常先行。
	buffer := make([]byte, 256)
	started := time.Now()
	count, err := response.Body.Read(buffer)
	if count == 0 || err != nil {
		t.Fatalf("首帧读取失败：count=%d err=%v", count, err)
	}
	if elapsed := time.Since(started); elapsed >= settleTestDelay {
		t.Fatalf("首帧被结算屏障拖住：%v（结算耗时 %v）", elapsed, settleTestDelay)
	}

	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("读取流失败: %v", err)
	}
	waitHandlerDone(t, handlerDone, 5*time.Second)

	if got := settler.settleCount(); got != 1 {
		t.Fatalf("handler 返回时本流终态应已落库（1 次），实际 %d 次——结算跑到了请求生命周期之外", got)
	}
	if pending := handler.PendingSettlements(); pending != 0 {
		t.Fatalf("handler 返回后待落库终态应为 0，实际 %d", pending)
	}
	assertSettlementsDrained(t, handler)
}

// 客户端中途断开：同样不允许结算落到请求生命周期之外（这正是 F2 的触发路径）。
func TestClientDisconnectKeepsSettlementInsideRequestLifetime(t *testing.T) {
	hold := make(chan struct{})
	handler, settler := newSettleTestHandler(t, hold)
	server, handlerDone := settleTestServer(t, handler)

	response := settleTestStreamRequest(t, server)
	buffer := make([]byte, 256)
	if count, err := response.Body.Read(buffer); count == 0 || err != nil {
		t.Fatalf("首帧读取失败：count=%d err=%v", count, err)
	}

	// 断线：客户端不再消费，上游紧接着写完并结束（真实场景里上游会自己收尾）。
	_ = response.Body.Close()
	close(hold)

	waitHandlerDone(t, handlerDone, 5*time.Second)
	if got := settler.settleCount(); got != 1 {
		t.Fatalf("断线后 handler 返回时终态应已落库（1 次），实际 %d 次", got)
	}
	assertSettlementsDrained(t, handler)
}

// 排空前的「待落库」计数必须如实反映在途结算：退出序列据此决定能不能关依赖。
//
// ctx 先结束时必须返回 false，否则「关连接池」会静默带走尚未发出的终态 UPDATE。
func TestWaitSettlementsReportsPendingWork(t *testing.T) {
	hold := make(chan struct{})
	handler, _ := newSettleTestHandler(t, hold)
	server, handlerDone := settleTestServer(t, handler)

	response := settleTestStreamRequest(t, server)
	buffer := make([]byte, 256)
	if count, err := response.Body.Read(buffer); count == 0 || err != nil {
		t.Fatalf("首帧读取失败：count=%d err=%v", count, err)
	}

	if pending := handler.PendingSettlements(); pending < 1 {
		t.Fatalf("流在途时待落库终态应至少为 1，实际 %d", pending)
	}
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	if handler.WaitSettlements(shortCtx) {
		t.Fatal("待落库终态未归零时应返回 false，实际 true（会导致排空提前放行）")
	}

	close(hold)
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("读取流失败: %v", err)
	}
	_ = response.Body.Close()
	waitHandlerDone(t, handlerDone, 5*time.Second)
	if pending := handler.PendingSettlements(); pending != 0 {
		t.Fatalf("流结束后待落库终态应为 0，实际 %d", pending)
	}
	assertSettlementsDrained(t, handler)
}

func assertSettlementsDrained(t *testing.T, handler *Handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !handler.WaitSettlements(ctx) {
		t.Fatalf("终态全部落库后 WaitSettlements 仍报未归零（pending=%d）", handler.PendingSettlements())
	}
}
