package gate

import (
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func TestParserFraming(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []Frame
	}{
		{
			name: "event 与 data",
			body: "event: message_start\ndata: {\"a\":1}\n\n",
			want: []Frame{{Event: "message_start", Data: `{"a":1}`}},
		},
		{
			name: "无 event 行的 data",
			body: "data: {\"b\":2}\n\n",
			want: []Frame{{Event: "", Data: `{"b":2}`}},
		},
		{
			name: "多行 data 以换行连接并只剥一个前导空白",
			body: "data:  x\ndata:y\n\n",
			want: []Frame{{Event: "", Data: " x\ny"}},
		},
		{
			name: "event 值 trim",
			body: "event:   response.created  \ndata: {}\n\n",
			want: []Frame{{Event: "response.created", Data: "{}"}},
		},
		{
			name: "注释行与未知字段忽略",
			body: ": keep-alive\nid: 7\nretry: 100\ndata: {\"c\":3}\n\n",
			want: []Frame{{Event: "", Data: `{"c":3}`}},
		},
		{
			name: "无 data 行的事件不产出帧",
			body: "event: ping\n\ndata: {\"d\":4}\n\n",
			want: []Frame{{Event: "", Data: `{"d":4}`}},
		},
		{
			name: "CRLF 行尾",
			body: "event: e\r\ndata: {\"e\":5}\r\n\r\n",
			want: []Frame{{Event: "e", Data: `{"e":5}`}},
		},
		{
			name: "裸 JSON 行直接成帧",
			body: "{\"f\":6}\n\n",
			want: []Frame{{Event: "", Data: `{"f":6}`}},
		},
		{
			name: "裸 JSON 行在已有 event 名时不产出帧",
			body: "event: e\n{\"g\":7}\n\n",
			want: nil,
		},
		{
			name: "尾部无空行的残帧由 Finish 冲刷",
			body: "data: {\"h\":8}",
			want: []Frame{{Event: "", Data: `{"h":8}`}},
		},
		{
			name: "尾部残行无 data 则不产出",
			body: "event: tail",
			want: nil,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := ParseBody(testCase.body)
			if err != nil {
				t.Fatalf("ParseBody 失败: %v", err)
			}
			assertFrames(t, got, testCase.want)
		})
	}
}

func TestParserSplitEquivalence(t *testing.T) {
	bodies := []string{
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
			"event: content_block_delta\ndata: {\"delta\":{\"text\":\"你好\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n",
		"\ufeff: comment\r\nevent: x\r\ndata: {\"k\":\"v\"}\r\n\r\n",
		"data: a\ndata: b\ndata: c\n\n",
	}
	slices := [][]int{{1}, {2, 3}, {5, 1, 7}, {17}}
	for _, body := range bodies {
		want, err := ParseBody(body)
		if err != nil {
			t.Fatalf("ParseBody 失败: %v", err)
		}
		for _, sizes := range slices {
			parser := NewParser(ParserOptions{})
			var got []Frame
			offset := 0
			for offset < len(body) {
				for _, size := range sizes {
					if offset >= len(body) {
						break
					}
					end := offset + size
					if end > len(body) {
						end = len(body)
					}
					frames, err := parser.Push([]byte(body[offset:end]))
					if err != nil {
						t.Fatalf("Push(%q @%d) 失败: %v", body, offset, err)
					}
					got = append(got, frames...)
					offset = end
				}
			}
			tail, err := parser.Finish()
			if err != nil {
				t.Fatalf("Finish 失败: %v", err)
			}
			got = append(got, tail...)
			if len(got) != len(want) {
				t.Fatalf("切片 %v 帧数不同: got %d want %d", sizes, len(got), len(want))
			}
			for index := range want {
				if got[index] != want[index] {
					t.Fatalf("切片 %v 第 %d 帧不同: got %+v want %+v", sizes, index, got[index], want[index])
				}
			}
		}
	}
}

// TestParserRandomSplitEquivalence 是属性式测试：随机切片喂入与整块喂入必须逐帧一致（固定种子）。
func TestParserRandomSplitEquivalence(t *testing.T) {
	body := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"m1","content":[]}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello 世界"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")
	want, err := ParseBody(body)
	if err != nil {
		t.Fatalf("ParseBody 失败: %v", err)
	}
	if len(want) != 3 {
		t.Fatalf("基准帧数应为 3，得到 %d", len(want))
	}

	random := rand.New(rand.NewSource(20260912))
	for round := 0; round < 200; round++ {
		parser := NewParser(ParserOptions{})
		var got []Frame
		offset := 0
		for offset < len(body) {
			size := 1 + random.Intn(11)
			end := offset + size
			if end > len(body) {
				end = len(body)
			}
			frames, err := parser.Push([]byte(body[offset:end]))
			if err != nil {
				t.Fatalf("第 %d 轮 Push 失败: %v", round, err)
			}
			got = append(got, frames...)
			offset = end
		}
		tail, err := parser.Finish()
		if err != nil {
			t.Fatalf("第 %d 轮 Finish 失败: %v", round, err)
		}
		got = append(got, tail...)
		if len(got) != len(want) {
			t.Fatalf("第 %d 轮帧数不同: got %d want %d", round, len(got), len(want))
		}
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("第 %d 轮第 %d 帧不同: got %+v want %+v", round, index, got[index], want[index])
			}
		}
	}
}

func TestParserBufferLimit(t *testing.T) {
	t.Run("超出上限即 BufferLimitError", func(t *testing.T) {
		parser := NewParser(ParserOptions{MaxBufferedBytes: 32})
		_, err := parser.Push([]byte("data: " + strings.Repeat("x", 64) + "\n\n"))
		var limitErr *BufferLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("应返回 BufferLimitError，得到 %v", err)
		}
		if limitErr.MaxBufferedBytes != 32 {
			t.Fatalf("上限应为 32，得到 %d", limitErr.MaxBufferedBytes)
		}
	})

	t.Run("完整行也检查上限", func(t *testing.T) {
		parser := NewParser(ParserOptions{MaxBufferedBytes: 16})
		_, err := parser.Push([]byte("x-unknown-field: " + strings.Repeat("y", 64) + "\n"))
		var limitErr *BufferLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("带换行的未知字段必须触发上限，得到 %v", err)
		}
	})

	t.Run("未终止帧的保留状态累计超限", func(t *testing.T) {
		// 已 dispatch 的帧会释放保留状态（与 TS 的 take() 一致），故累计只发生在单个未终止帧内部：
		// 连续 data 行而没有空行，是中性帧洪泛的 parser 侧兵底。
		parser := NewParser(ParserOptions{MaxBufferedBytes: 64})
		line := []byte("data: " + strings.Repeat("p", 16) + "\n")
		var err error
		for range 8 {
			if _, err = parser.Push(line); err != nil {
				break
			}
		}
		var limitErr *BufferLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("累计超限应报错，得到 %v", err)
		}
	})

	t.Run("豁免帧在豁免上限内放行、越界报错", func(t *testing.T) {
		const capBytes = 128
		exemption := &BufferLimitExemption{
			MaxBufferedBytes: capBytes * 2,
			Matches: func(event string, dataHead string) bool {
				return event == "response.created"
			},
		}
		frame := func(event string, dataBytes int) []byte {
			return []byte("event: " + event + "\ndata: " + strings.Repeat("e", dataBytes) + "\n\n")
		}

		allowed := NewParser(ParserOptions{MaxBufferedBytes: capBytes, Exemption: exemption})
		frames, err := allowed.Push(frame("response.created", 200))
		if err != nil {
			t.Fatalf("豁免帧应放行，得到 %v", err)
		}
		if len(frames) != 1 || len(frames[0].Data) != 200 {
			t.Fatalf("豁免帧应完整产出，得到 %+v", frames)
		}

		overExemption := NewParser(ParserOptions{MaxBufferedBytes: capBytes, Exemption: exemption})
		if _, err := overExemption.Push(frame("response.created", 300)); err == nil {
			t.Fatal("越过豁免硬上限应报错")
		}

		notExempt := NewParser(ParserOptions{MaxBufferedBytes: capBytes, Exemption: exemption})
		if _, err := notExempt.Push(frame("response.in_progress", 200)); err == nil {
			t.Fatal("非豁免事件应报错")
		}
	})

	t.Run("不设上限则不检查", func(t *testing.T) {
		parser := NewParser(ParserOptions{})
		if _, err := parser.Push([]byte("data: " + strings.Repeat("z", 4096) + "\n\n")); err != nil {
			t.Fatalf("无上限时不应报错，得到 %v", err)
		}
	})
}

func TestParserVisitorStops(t *testing.T) {
	parser := NewParser(ParserOptions{})
	drained, err := parser.Visit([]byte("data: one\n\ndata: two\n\n"), func(frame Frame) bool {
		return false
	})
	if err != nil {
		t.Fatalf("Visit 失败: %v", err)
	}
	if drained {
		t.Fatal("visitor 提前终止时 drained 应为 false")
	}
}

func TestResolvePath(t *testing.T) {
	parsed := mustParseJSON(t, `{
		"choices": [{"delta": {"content": "a"}}, {"delta": {}}],
		"nested": [{"parts": [{"text": "x"}, {"text": ""}]}],
		"deep": [{"a": [{"b": 1}]}],
		"flag": false
	}`)

	cases := []struct {
		path string
		want any
	}{
		{"choices.#.delta.content", []any{"a"}},
		{"nested.#.parts.#.text", []any{"x", ""}},
		{"deep.#.a.#.b", []any{float64(1)}},
		{"missing.path", nil},
		{"flag", false},
	}
	for _, testCase := range cases {
		got := ResolvePath(parsed, testCase.path)
		if !deepEqualAny(got, testCase.want) {
			t.Fatalf("ResolvePath(%q) = %#v，期望 %#v", testCase.path, got, testCase.want)
		}
	}

	if got := ResolvePath("not-an-object", "a.b"); got != nil {
		t.Fatalf("非对象应为 nil，得到 %#v", got)
	}
	if got := ResolvePath(parsed, "#"); got != nil {
		t.Fatalf("非数组上的 # 应为 nil，得到 %#v", got)
	}
}

func TestIsNonEmptyValue(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  bool
	}{
		{"空串", "", false},
		{"非空串", "x", true},
		{"零也算内容", float64(0), true},
		{"true 算内容", true, true},
		{"false 不算", false, false},
		{"nil 不算", nil, false},
		{"空数组", []any{}, false},
		{"数组任一非空即算", []any{"", "y"}, true},
		{"空对象不算", map[string]any{}, false},
		{"非空对象算", map[string]any{"k": nil}, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := IsNonEmptyValue(testCase.value); got != testCase.want {
				t.Fatalf("IsNonEmptyValue(%#v) = %v，期望 %v", testCase.value, got, testCase.want)
			}
		})
	}
}

func assertFrames(t *testing.T, got []Frame, want []Frame) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("帧数不同: got %d (%+v) want %d (%+v)", len(got), got, len(want), want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("第 %d 帧不同: got %+v want %+v", index, got[index], want[index])
		}
	}
}

func mustParseJSON(t *testing.T, raw string) any {
	t.Helper()
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("测试语料不是合法 JSON: %v", err)
	}
	return parsed
}

func deepEqualAny(left any, right any) bool {
	return reflect.DeepEqual(left, right)
}
