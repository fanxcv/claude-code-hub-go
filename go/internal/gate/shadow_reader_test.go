package gate

import (
	"bytes"
	"io"
	"testing"
)

// closerReader 记录 Close 调用次数，用于断言包装层不改动上游正文的归属。
type closerReader struct {
	reader *bytes.Reader
	closed int
}

func (c *closerReader) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *closerReader) Close() error {
	c.closed++
	return nil
}

// TestShadowReaderPassesBytesThrough 钉住 shadow 旁路的三条纪律：字节不变、报告只出一次、
// Close 透传。
//
// 为什么要有它：shadow 模式不门控（Node forwarder.ts:2034），但必须留下「首非空字节 vs
// 首有效内容」的分歧度量。若包装层改了字节或吞了关闭，就会把一条正常响应变成故障。
func TestShadowReaderPassesBytesThrough(t *testing.T) {
	// payload 沿用 classify_test.go 的夹具形状：中性帧在前，内容帧在后。
	const payload = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"

	var reports []ShadowReport
	reader := &closerReader{reader: bytes.NewReader([]byte(payload))}
	wrapped := NewShadowReader(reader, ShadowConfig{
		Family:       FamilyAnthropic,
		ProviderID:   9,
		ProviderName: "供应商甲",
		OnReport:     func(report ShadowReport) { reports = append(reports, report) },
	})

	// 分两次读：验证跨块解析（首块只有中性帧，决定性帧在第二块）。
	first := make([]byte, 48)
	n, err := wrapped.Read(first)
	if err != nil || n != 48 {
		t.Fatalf("首次读 n=%d err=%v，期望读满 48 字节", n, err)
	}
	if got := string(first[:n]); got != payload[:48] {
		t.Fatalf("包装改变了字节：%q", got)
	}
	if len(reports) != 0 {
		t.Fatalf("首块只有中性帧，不应出报告：%+v", reports)
	}

	rest, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("后续读失败：%v", err)
	}
	if string(rest) != payload[48:] {
		t.Fatalf("包装改变了字节：%q", string(rest))
	}
	if len(reports) != 1 {
		t.Fatalf("报告数 = %d，期望 1（首个决定性帧即终结观测）", len(reports))
	}
	if reports[0].DecisiveVerdict != VerdictContent {
		t.Errorf("决定性判定 = %q，期望 content", reports[0].DecisiveVerdict)
	}
	if reports[0].ProviderID != 9 || reports[0].ProviderName != "供应商甲" {
		t.Errorf("报告未带上游身份：%+v", reports[0])
	}

	if err := wrapped.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}
	if reader.closed != 1 {
		t.Errorf("源被关闭 %d 次，期望 1", reader.closed)
	}
}
