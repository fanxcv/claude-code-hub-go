package forward

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
)

// delayedBody 模拟「首块即时、其后各块延迟」的上游正文：门控在速率闸检查点到期时提交，
// 此刻第二次读仍在飞。
type delayedBody struct {
	chunks []string
	delay  time.Duration

	mu     sync.Mutex
	index  int
	closed bool
}

func (b *delayedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed || b.index >= len(b.chunks) {
		b.mu.Unlock()
		return 0, io.EOF
	}
	index := b.index
	b.index++
	b.mu.Unlock()
	if index > 0 && b.delay > 0 {
		time.Sleep(b.delay)
	}
	return copy(p, b.chunks[index]), nil
}

func (b *delayedBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return nil
}

// TestGateStreamAttemptKeepsInFlightReadBytes 断言门控检查点提交时，门控把续读句柄接成
// 泵的源：前缀 + 续读逐字节等于上游完整正文，在飞读已取走的那一段不丢。
func TestGateStreamAttemptKeepsInFlightReadBytes(t *testing.T) {
	first := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" +
		strings.Repeat("a", 300) + "\"}}\n\n"
	second := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"tail\"}}\n\n"
	body := &delayedBody{chunks: []string{first, second}, delay: 1200 * time.Millisecond}
	response := &dial.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	// θ=1 B/s：首档 1s 需 2 字节，首块 300 字节即达标，故在首个检查点提交。
	options := StreamOptions{
		Format:           convert.FormatClaude,
		PrecommitRateFor: func(int64) int { return 1 },
		Settle:           newCountingSettler(),
	}
	outcome := &AttemptOutcome{ProviderID: 1, ProviderName: "供应商甲"}

	resp, fail := Deps{}.gateStreamAttempt(context.Background(), response, &Plan{}, outcome, options, gate.FamilyAnthropic)
	if fail != nil {
		t.Fatalf("门控应提交，得失败 %v", fail)
	}
	if _, ok := resp.Stream.Source.(continuationSource); !ok {
		t.Fatalf("提交时仍有在飞读，Source 必须接上续读句柄，实得 %T", resp.Stream.Source)
	}

	stream := newStream(context.Background(), resp.Stream, Provider{ID: 1, Name: "供应商甲"}, Endpoint{}, &Plan{}, newTestPctx(t), options, nil)
	received, _ := consumeStream(t, stream)
	if string(received) != first+second {
		t.Fatalf("前缀 + 续读必须逐字节等于上游完整正文：\n got=%q\nwant=%q", received, first+second)
	}
}
