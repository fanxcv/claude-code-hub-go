package gate

import (
	"strings"
	"testing"
)

// 语料取典型上游块：一帧一个 chunk（分帧器最热的形态是「整块里恰有一整帧」）。

var benchFrameChunk = []byte(
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
)

// BenchmarkParserPushOneFrame 钉住 B/C：每个完整帧的分帧开销（allocs/帧）。
func BenchmarkParserPushOneFrame(b *testing.B) {
	parser := NewParser(ParserOptions{MaxBufferedBytes: 1 << 20})
	frames := make([]Frame, 0, 1)
	b.ReportAllocs()
	for b.Loop() {
		// 借用契约：帧只在本块生命周期内有效，故就地判完即丢引用。
		frames = frames[:0]
		got, err := parser.Visit(benchFrameChunk, func(frame Frame) bool {
			frames = append(frames, frame)
			return true
		})
		if err != nil || !got {
			b.Fatalf("解析失败: %v", err)
		}
		if len(frames) != 1 {
			b.Fatalf("应产出 1 帧，得到 %d", len(frames))
		}
	}
}

// TestParserFrameDataOwned 钉住 C 的契约：完整行借用输入切片，但 Frame.Data 必须是拷出的
// owned string——改动输入切片不得改到已交给调用方的帧。
func TestParserFrameDataOwned(t *testing.T) {
	chunk := []byte("data: {\"a\":1}\n\n")
	frames, err := ParseBody(string(chunk))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(frames) != 1 || frames[0].Data != `{"a":1}` {
		t.Fatalf("帧不符: %+v", frames)
	}
	for i := range chunk {
		chunk[i] = 'x'
	}
	if frames[0].Data != `{"a":1}` {
		t.Fatalf("Frame.Data 借用了输入切片: %q", frames[0].Data)
	}
}

// TestParserCompleteLineBorrowMatchesResidual 钉住 C：同一帧以「整块喂入（全程借用输入）」
// 与「逐字节喂入（每行都跨块，走残行拼接）」两种方式解析，结果必须一致。
func TestParserCompleteLineBorrowMatchesResidual(t *testing.T) {
	bodies := []string{
		"event: message_start\ndata: {\"a\":1}\n\n",
		"data: {\"b\":2}\r\n\r\n",
		"{\"bare\":3}\n\n",
		"data: x\ndata: y\n\n",
		"event: e\ndata: [DONE]\n\n",
		"data: {\"c\":4}\r",
	}
	for _, body := range bodies {
		want, err := ParseBody(body)
		if err != nil {
			t.Fatalf("整块解析失败: %v", err)
		}
		parser := NewParser(ParserOptions{})
		var got []Frame
		for i := 0; i < len(body); i++ {
			frames, err := parser.Push([]byte(body[i : i+1]))
			if err != nil {
				t.Fatalf("逐字节解析失败: %v", err)
			}
			got = append(got, frames...)
		}
		tail, err := parser.Finish()
		if err != nil {
			t.Fatalf("Finish 失败: %v", err)
		}
		got = append(got, tail...)
		if len(got) != len(want) {
			t.Fatalf("body=%q 帧数不同: got %+v want %+v", body, got, want)
		}
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("body=%q 第 %d 帧不同: got %+v want %+v", body, index, got[index], want[index])
			}
		}
	}
}

// TestParserLimitUsesCompletedLine 钉住 B 的定长头与上限检查：完整行的 event 名与 data 头
// 都要按行内容算，不能因为「行未入 p.line」而漏判。
func TestParserLimitUsesCompletedLine(t *testing.T) {
	exemption := &BufferLimitExemption{
		MaxBufferedBytes: 256,
		Matches: func(event string, dataHead string) bool {
			return event == "response.created"
		},
	}
	parser := NewParser(ParserOptions{MaxBufferedBytes: 64, Exemption: exemption})
	frames, err := parser.Push([]byte("event: response.created\ndata: " + strings.Repeat("e", 100) + "\n\n"))
	if err != nil {
		t.Fatalf("豁免帧应放行: %v", err)
	}
	if len(frames) != 1 || frames[0].Event != "response.created" || len(frames[0].Data) != 100 {
		t.Fatalf("豁免帧应完整产出: %+v", frames)
	}
}
