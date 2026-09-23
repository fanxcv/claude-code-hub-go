package forward

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
)

// TestHedgeRunGateKeepsInFlightReadBytes 钉住**竞速路径**（hedgeRace.runGate）的续读接线：
// 门控在速率闸检查点到期提交时，若仍有一次读在飞（已从上游取走字节但未交付），runGate
// 必须把那批字节接成泵的源（result.Continuation），否则在竞速路径上被丢弃。
//
// 与串行路径的 TestGateStreamAttemptKeepsInFlightReadBytes 是同一缺陷语义，但这里走
// hedgeRace.runGate——竞速唯一的门控入口，确保钉的是竞速那条接线而非串行那条。
// 复用 delayedBody 夹具（见 gate_continuation_test.go）：首块即时、第二块延迟，令提交发生在
// 第二次读仍在飞时。
func TestHedgeRunGateKeepsInFlightReadBytes(t *testing.T) {
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
	// θ=1 B/s：首档检查点 1s 需 2 字节，首块 300 字节即达标，故在首个检查点提交；
	// 此刻第二次读仍在飞（延迟 1200ms），其已从上游取走的字节不得丢。
	race := &hedgeRace{options: StreamOptions{
		Format:           convert.FormatClaude,
		PrecommitRateFor: func(int64) int { return 1 },
		Now:              time.Now,
	}}
	outcome := &AttemptOutcome{ProviderID: 1, ProviderName: "供应商甲"}

	var firstByteAt time.Time
	content, err := race.runGate(context.Background(), response, &Plan{}, outcome, gate.FamilyAnthropic, &firstByteAt)
	if err != nil {
		t.Fatalf("检查点到期应提交，得错误 %v", err)
	}
	if content.ReaderDone {
		t.Fatal("提交发生在上游 EOF 之前，ReaderDone 应为假")
	}
	// 不变式：前缀 + 从源读到的全部字节，逐字节等于上游完整正文。
	// 若没接续读句柄（直接拿 response.Body），在飞读已取走的那段字节会丢，此断言即红。
	tail, err := io.ReadAll(content.Source)
	if err != nil {
		t.Fatalf("续读失败: %v", err)
	}
	got := append(append([]byte{}, gate.ConcatPrefix(content.Prefix)...), tail...)
	if string(got) != first+second {
		t.Fatalf("前缀 + 续读必须逐字节等于上游完整正文：\n got=%q\nwant=%q", got, first+second)
	}
	// 换源必须真的发生：源是续读句柄，而不是裸的 response.Body。
	if _, ok := content.Source.(continuationSource); !ok {
		t.Fatalf("提交时仍有在飞读，Source 必须接上续读句柄，实得 %T", content.Source)
	}
}
