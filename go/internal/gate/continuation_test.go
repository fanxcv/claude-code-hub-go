package gate

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

// TestCommitAtCheckpointKeepsPendingReadBytes 钉住「检查点到期提交时不得丢掉在飞读
// 已从上游取走的字节」：提交后把前缀与续读句柄拼起来，必须逐字节等于上游完整正文。
//
// 构造：首块内容帧立即到达启动速率闸（首档检查点 20ms），第二块延迟 200ms——门控在
// 检查点到期时提交，此刻第二次读仍在飞。修复前那次读的字节被丢弃。
func TestCommitAtCheckpointKeepsPendingReadBytes(t *testing.T) {
	opts := ladderOptions()
	first := chatContentChunk(100) // 首档 20ms 需 40 字节，100 已达标
	second := chatContentChunk(7)
	full := first + second
	source := newScriptedReader(
		scriptedStep{text: first},
		scriptedStep{delay: 200 * time.Millisecond, text: second},
		scriptedStep{text: ""}, // EOF
	)
	defer func() { _ = source.Close() }()

	result, err := Run(context.Background(), source, opts)
	if err != nil {
		t.Fatalf("检查点到期应提交，得错误 %v", err)
	}
	if result.ReaderDone {
		t.Fatal("提交发生在上游 EOF 之前，ReaderDone 应为假")
	}

	// 模拟 forward 接线：修复后泵读续读句柄，修复前泵读原始 source（会丢掉在飞读的字节）。
	var upstream io.Reader = source
	if result.Continuation != nil {
		upstream = result.Continuation
	}
	tail, err := io.ReadAll(upstream)
	if err != nil {
		t.Fatalf("续读失败: %v", err)
	}
	got := append(append([]byte{}, ConcatPrefix(result.Prefix)...), tail...)
	if !bytes.Equal(got, []byte(full)) {
		t.Fatalf("前缀 + 续读必须逐字节等于上游完整正文：\n got=%q\nwant=%q", got, full)
	}
	if result.Continuation == nil {
		t.Fatal("提交时仍有在飞读，必须交回续读句柄")
	}
}

// TestCommitOnContentFrameHasNoContinuation 钉住「无在飞读时不得交回续读句柄」：
// 首个内容帧提交发生在一次读已交付之后，没有未交付字节，Continuation 必须为 nil，
// 调用方继续读原始 source。
func TestCommitOnContentFrameHasNoContinuation(t *testing.T) {
	first := chatContentChunk(3)
	second := chatContentChunk(4)
	source := newScriptedReader(
		scriptedStep{text: first},
		scriptedStep{text: second},
		scriptedStep{text: ""},
	)
	defer func() { _ = source.Close() }()

	result, err := Run(context.Background(), source, baseOptions())
	if err != nil {
		t.Fatalf("首个内容帧应提交，得错误 %v", err)
	}
	if result.Continuation != nil {
		t.Fatal("无在飞读时不得交回续读句柄")
	}
	tail, err := io.ReadAll(source)
	if err != nil {
		t.Fatalf("续读失败: %v", err)
	}
	got := append(append([]byte{}, ConcatPrefix(result.Prefix)...), tail...)
	if !bytes.Equal(got, []byte(first+second)) {
		t.Fatalf("前缀 + 原始 source 续读必须等于上游完整正文：\n got=%q\nwant=%q", got, first+second)
	}
}
