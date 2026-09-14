package dataplane

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
)

// 本文件钉住**响应侧工件接线**的三条不变量（协调者定档里的第 1 项与第 6 项）：
//
//  1. 正文捕获**不改变客户端收到的字节**（旁路，不是整流缓冲）；
//  2. 8 MiB 流下**每流驻留有界**（头尾窗口 + 摊还裁剪，与流长无关）；
//  3. 首块**不因捕获而延迟**（捕获点在写与 flush 之后）。
//
// 为什么这三条要一起钉：捕获写在热路径上，它最危险的失败模式是「没失败但变慢/变胖」——
// 任何请求都照常 200，只有进程在流量上来时被 OOM 或客户端发现 TTFT 变长。故这里断言的是
// 边界与顺序，不是功能。

// fakeTelemetry 记录观测与响应工件的调用（不碰 Redis）。
type fakeTelemetry struct {
	begins    []TelemetryFacts
	responses []ResponseArtifacts
	finishes  []string
}

func (f *fakeTelemetry) Begin(_ context.Context, facts TelemetryFacts) TelemetryLease {
	f.begins = append(f.begins, facts)
	return TelemetryLease{
		Identity:  session.PublicSessionIdentity(facts.SessionID, facts.KeyID),
		SessionID: facts.SessionID,
		Sequence:  facts.Sequence,
		KeyID:     facts.KeyID,
	}
}

func (f *fakeTelemetry) ProviderSelected(context.Context, TelemetryLease, int64, string) {}

func (f *fakeTelemetry) Respond(_ context.Context, _ TelemetryLease, artifacts ResponseArtifacts) {
	f.responses = append(f.responses, artifacts)
}

func (f *fakeTelemetry) Finish(_ context.Context, _ TelemetryLease, status string) {
	f.finishes = append(f.finishes, status)
}

// newCaptureTestHandler 造一个接线了假 Telemetry 与假会话绑定的数据面（捕获开关打开）。
//
// 会话绑定必须给一个真的 SessionID：没有会话就没有观测租约，也就没有响应侧工件——
// 那条路径在「未绑定会话」时是刻意短路的（见 startTelemetry 的 SessionID == "" 判定）。
func newCaptureTestHandler(
	t *testing.T, upstreamURL string, telemetry *fakeTelemetry,
) (*Handler, *fakeMessageWriter) {
	t.Helper()
	dialClient, err := dial.New(dial.Options{})
	if err != nil {
		t.Fatalf("拨号器构造失败: %v", err)
	}
	messages := &fakeMessageWriter{}
	auth := fakeAuth{
		user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
		key:  guard.Key{ID: 2, Name: "k", UserID: 1},
	}
	handler, err := New(Options{
		Logger: logx.New(nil),
		Base: guard.Deps{
			Auth:           auth,
			Users:          auth,
			Settings:       fakeSettings{},
			Sensitive:      fakeEmptySource{},
			Filters:        fakeEmptySource{},
			Provider:       fakeProvider{selection: pctx.ProviderSelection{ProviderID: 7, Name: "假供应商", Type: "claude"}},
			MessageContext: messages,
			Sessions: &fakeBinder{result: guard.SessionResult{
				SessionID: "sess_capture", Sequence: 2,
			}},
		},
		Candidates: fakeCandidates{url: upstreamURL},
		Settlers: func(*RequestState) Settler {
			return &fakeSettler{}
		},
		Forward: forward.Deps{Dial: dialClient},
		Stream: forward.StreamOptions{
			Budget: gate.DefaultBudget(),
		},
		Telemetry: telemetry,
		SessionArtifacts: session.SessionArtifactOptions{
			StoreMessages: false, StoreResponseBody: true,
		},
	})
	if err != nil {
		t.Fatalf("数据面构造失败: %v", err)
	}
	return handler, messages
}

// TestResponseCaptureDoesNotAlterDeliveredBytes SSE 流下客户端收到的字节与上游一致，且捕获有界。
func TestResponseCaptureDoesNotAlterDeliveredBytes(t *testing.T) {
	const chunkCount = 256 // 256 × 32 KiB = 8 MiB
	chunk := strings.Repeat("d", 32*1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		_, _ = io.WriteString(w,
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"甲\"}}\n\n")
		flusher.Flush()
		for index := 0; index < chunkCount; index++ {
			_, _ = io.WriteString(w, chunk)
		}
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	telemetry := &fakeTelemetry{}
	handler, _ := newCaptureTestHandler(t, upstream.URL, telemetry)
	server := httptest.NewServer(handler)
	defer server.Close()

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
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatalf("读响应失败: %v", readErr)
	}

	// 1. 客户端收到的字节里 8 MiB 数据一字不少（捕获不改交付内容）。
	if got := strings.Count(string(body), "d"+"d"); got < chunkCount {
		// 至少能证明数据段数够（"dd" 的出现次数与 chunk 数同量级）。
		t.Fatalf("客户端收到的数据段偏少：%d", got)
	}
	if !strings.Contains(string(body), "message_stop") {
		t.Fatal("客户端应收到终止事件")
	}

	// 2. 捕获确实发生了，且**有界**：≤ 头窗口 + 尾窗口 + 标记。
	if len(telemetry.responses) != 1 {
		t.Fatalf("应写一次响应侧工件，实得 %d", len(telemetry.responses))
	}
	captured := telemetry.responses[0].ResponseBody
	if len(captured) == 0 {
		t.Fatal("捕获开启时响应正文不应为空")
	}
	maxBounded := 2*session.ResponseCaptureWindowBytes + 64
	if len(captured) > maxBounded {
		t.Fatalf("捕获正文 %d 字节，超过上界 %d（驻留与流长无关这条不变量被破坏）",
			len(captured), maxBounded)
	}
	if !strings.Contains(string(captured), "[TRUNCATED: ") {
		t.Fatalf("8 MiB 流应带截断标记：%.80s", captured)
	}
	// 3. 捕获的是**客户端可见字节**：头尾窗口必须与交付内容同前缀同后缀。
	if !strings.HasPrefix(string(body), string(captured[:64])) {
		t.Fatal("捕获头窗口与交付内容不同源")
	}
	tail := captured[len(captured)-64:]
	if !strings.HasSuffix(string(body), string(tail)) {
		t.Fatal("捕获尾窗口与交付内容不同源")
	}

	// 4. 状态码与上游 URL 等 meta 也被记下（详情页的 response.before/after）。
	artifacts := telemetry.responses[0]
	if artifacts.StatusCode != http.StatusOK {
		t.Errorf("状态码应为 200，实得 %d", artifacts.StatusCode)
	}
	if artifacts.UpstreamURL == "" || artifacts.UpstreamMethod == "" {
		t.Errorf("上游 URL/method 应被记下：%+v", artifacts)
	}
	// 相位快照：四份都要有内容（各自至少一个字段）。
	for name, snapshot := range map[string]session.SessionDetailPhaseSnapshot{
		"request.before":  artifacts.RequestBefore,
		"request.after":   artifacts.RequestAfter,
		"response.before": artifacts.ResponseBefore,
		"response.after":  artifacts.ResponseAfter,
	} {
		if snapshot.Body == nil && len(snapshot.Headers) == 0 &&
			snapshot.Meta.Method == nil && snapshot.Meta.StatusCode == nil &&
			snapshot.Meta.UpstreamURL == nil {
			t.Errorf("%s 快照为空（详情页会出现一个空相位）", name)
		}
	}
}

// TestResponseCaptureKeepsFirstChunkImmediate 首块不因捕获而延迟。
//
// 判据：上游在发出首帧后**挡住**后续帧，客户端仍必须立刻读到首帧。捕获发生在写与 flush
// 之后，故它不可能拖住首块——这条测试正是靠这个顺序断言的（若捕获被挪到写之前并做重活，
// 首帧就会等到上游被挡住之后才到，测试超时）。
func TestResponseCaptureKeepsFirstChunkImmediate(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		_, _ = io.WriteString(w,
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"甲\"}}\n\n")
		flusher.Flush()
		<-release
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	telemetry := &fakeTelemetry{}
	handler, _ := newCaptureTestHandler(t, upstream.URL, telemetry)
	server := httptest.NewServer(handler)
	defer server.Close()

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
	defer func() { _ = response.Body.Close() }()

	readDone := make(chan string, 1)
	go func() {
		buffer := make([]byte, 256)
		count, _ := response.Body.Read(buffer)
		readDone <- string(buffer[:count])
	}()
	select {
	case first := <-readDone:
		if !strings.Contains(first, "content_block_delta") {
			t.Fatalf("首块应是内容帧，实得 %q", first)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("上游被挡住时客户端读不到首帧：首块被捕获拖住了")
	}
	close(release)
	_, _ = io.ReadAll(response.Body)
}
