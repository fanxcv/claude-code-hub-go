package forward

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// TestObserverResponseFailedIsUpstreamError 钉住 response.failed 的归因。
//
// 错误文案嵌在 response.error 里、顶层没有 error 字段，故旧实现读不到文案，归因落到
// TerminalUpstreamTruncated——渠道会因此被当成「正文没传完」而非上游失败。
func TestObserverResponseFailedIsUpstreamError(t *testing.T) {
	const frames = "event: response.failed\n" +
		`data: {"type":"response.failed","response":{"status":"failed","error":{"message":"boom"}}}` + "\n\n"

	observer := newTestObserver(convert.FormatResponse, 4096, 4096)
	observer.Push([]byte(frames))
	snapshot := observer.Snapshot()

	if snapshot.ErrorText != "boom" {
		t.Fatalf("ErrorText = %q，期望 boom", snapshot.ErrorText)
	}
	if snapshot.CompletionMarker {
		t.Fatal("response.failed 不得算作完成标记")
	}
	if got := terminalKindFor(PumpCompletion{}, snapshot); got != TerminalUpstreamError {
		t.Fatalf("终态 = %s，期望 %s", got, TerminalUpstreamError)
	}
}

// TestObserverResponseCompletedIsCompleted 反向钉住：正常 completed 流仍是完成。
func TestObserverResponseCompletedIsCompleted(t *testing.T) {
	const frames = "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"status":"completed","output":[]}}` + "\n\n"

	observer := newTestObserver(convert.FormatResponse, 4096, 4096)
	observer.Push([]byte(frames))
	snapshot := observer.Snapshot()

	if snapshot.ErrorText != "" {
		t.Fatalf("正常完成流不得带错误文案：%q", snapshot.ErrorText)
	}
	if !snapshot.CompletionMarker {
		t.Fatal("response.completed 应为完成标记")
	}
	if got := terminalKindFor(PumpCompletion{}, snapshot); got != TerminalCompleted {
		t.Fatalf("终态 = %s，期望 %s", got, TerminalCompleted)
	}
}
