package forward

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
)

// 本文件钉住 TTFT 口径：`ttfb_ms` 恒为「首个有效内容」。
//
// 病根（改造前）：门控行的 TTFT 取提交时刻（用户可见首字），非门控行取首个 chunk
// （往往是 message_start / response.created 这类中性帧，此时屏幕还是空的）。同一列混两种语义，
// 非门控行因而系统性偏乐观——模拟里差 2400ms。

// arrivalClock 是脚本时钟：正文源在每次交付 chunk 前把它拨到该 chunk 的到达时刻，
// 于是「到达时刻」由测试决定，与调度抖动无关。
type arrivalClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *arrivalClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *arrivalClock) set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// arrivalBody 在每次读交付前把时钟拨到该块的到达时刻（模拟真实的网络到达）。
type arrivalBody struct {
	clock    *arrivalClock
	chunks   []string
	arrivals []time.Time
	index    int
	closed   bool
}

func (b *arrivalBody) Read(p []byte) (int, error) {
	if b.closed || b.index >= len(b.chunks) {
		return 0, io.EOF
	}
	b.clock.set(b.arrivals[b.index])
	chunk := b.chunks[b.index]
	b.index++
	return copy(p, chunk), nil
}

func (b *arrivalBody) Close() error {
	b.closed = true
	return nil
}

// ttftFixtureChunks 是一段「100ms 中性帧 + 2500ms 内容帧」的 anthropic 流。
//
// 中性帧刻意选用真实上游最常先发的那一个（message_start）：它在改造前就是被误当成 TTFT 的帧。
func ttftFixtureChunks() []string {
	return []string{
		`event: message_start` + "\n" +
			`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-5","usage":{"input_tokens":11}}}` + "\n\n",
		`event: content_block_delta` + "\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}` + "\n\n",
		`event: message_stop` + "\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	}
}

// newTTFTFixture 造一份到达时刻固定为 100ms / 2500ms / 2510ms 的正文与时钟。
func newTTFTFixture(startedAt time.Time) (*arrivalClock, *arrivalBody) {
	clock := &arrivalClock{now: startedAt}
	body := &arrivalBody{
		clock:  clock,
		chunks: ttftFixtureChunks(),
		arrivals: []time.Time{
			startedAt.Add(100 * time.Millisecond),
			startedAt.Add(2500 * time.Millisecond),
			startedAt.Add(2510 * time.Millisecond),
		},
	}
	return clock, body
}

func ttftSSEHeader() http.Header {
	return http.Header{"Content-Type": []string{"text/event-stream"}}
}

// TestTTFTIsFirstContentOnBothGatePaths 是方案 B 的核心断言：门控与非门控
// 都必须得到 ttfb_ms = 首个有效内容的时刻，而 first_byte_ms 都仍是上游首字节。
//
// 改造前：非门控行会得到 100（首个 chunk = message_start），差 2400ms。
func TestTTFTIsFirstContentOnBothGatePaths(t *testing.T) {
	startedAt := time.Unix(0, 0).UTC()

	t.Run("门控", func(t *testing.T) {
		clock, body := newTTFTFixture(startedAt)
		response := &dial.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     ttftSSEHeader(),
			Body:       body,
		}
		options := StreamOptions{StartedAt: startedAt, Now: clock.Now, Format: convert.FormatClaude}
		outcome := &AttemptOutcome{ProviderID: 1, ProviderName: "供应商甲"}

		resp, failure := Deps{}.gateStreamAttempt(
			context.Background(), response, &Plan{}, outcome, options, gate.FamilyAnthropic)
		if failure != nil {
			t.Fatalf("门控应提交，得失败 %v", failure)
		}
		// 提交之后再过 500ms 才把前缀喂进观测器：TTFT 必须取**提交时刻**而不是喂入时刻。
		// 少了显式的 CommitAt，这条断言就会读到 3000ms（改造前是靠「首次 Push 恰好是提交」蒙对的）。
		clock.set(startedAt.Add(3000 * time.Millisecond))

		stream := newStream(context.Background(), resp.Stream,
			Provider{ID: 1, Name: "供应商甲"}, Endpoint{}, &Plan{}, nil, options, nil)
		consumeStream(t, stream)
		assertTTFT(t, stream.Observation(), 2500, 100)
	})

	t.Run("非门控", func(t *testing.T) {
		clock, body := newTTFTFixture(startedAt)
		attempt := &streamAttempt{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     ttftSSEHeader(),
			Source:     body,
			Gated:      false,
			Family:     gate.FamilyAnthropic,
		}
		options := StreamOptions{StartedAt: startedAt, Now: clock.Now, Format: convert.FormatClaude}
		stream := newStream(context.Background(), attempt,
			Provider{ID: 1, Name: "供应商甲"}, Endpoint{}, &Plan{}, nil, options, nil)
		consumeStream(t, stream)
		assertTTFT(t, stream.Observation(), 2500, 100)
	})
}

// assertTTFT 断言两列的口径：TTFT 是首个有效内容，FirstByte 是上游首字节。
// 容差 1ms 吸收时钟注入点的取整。
func assertTTFT(t *testing.T, observation Observation, wantTTFTMS, wantFirstByteMS int64) {
	t.Helper()
	gotTTFT := observation.TTFT.Milliseconds()
	if gotTTFT < wantTTFTMS-1 || gotTTFT > wantTTFTMS+1 {
		t.Fatalf("TTFT = %dms，期望 ≈%dms（首个有效内容，不是首个 chunk）", gotTTFT, wantTTFTMS)
	}
	gotFirstByte := observation.FirstByte.Milliseconds()
	if gotFirstByte < wantFirstByteMS-1 || gotFirstByte > wantFirstByteMS+1 {
		t.Fatalf("FirstByte = %dms，期望 ≈%dms（上游首个非空 chunk）", gotFirstByte, wantFirstByteMS)
	}
}

// TestTTFTStaysEmptyWhenStreamHasNoContent 钉住「只有中性帧」时的口径：TTFT 留零。
//
// 理由：整条流没有用户可见内容，任何数值都是编出来的。落库层只在 `TTFT > 0` 时写列
// （见 dataplane/upstream.go），故零值等于 ttfb_ms 留 NULL——缺数，而不是假数。
// first_byte_ms 照旧有值（上游确实发了字节）。
func TestTTFTStaysEmptyWhenStreamHasNoContent(t *testing.T) {
	startedAt := time.Unix(0, 0).UTC()
	clock := &arrivalClock{now: startedAt}
	body := &arrivalBody{
		clock: clock,
		chunks: []string{
			`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-5"}}` + "\n\n",
			`event: ping` + "\n" + `data: {"type":"ping"}` + "\n\n",
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
		},
		arrivals: []time.Time{
			startedAt.Add(100 * time.Millisecond),
			startedAt.Add(150 * time.Millisecond),
			startedAt.Add(200 * time.Millisecond),
		},
	}
	attempt := &streamAttempt{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     ttftSSEHeader(),
		Source:     body,
		Family:     gate.FamilyAnthropic,
	}
	options := StreamOptions{StartedAt: startedAt, Now: clock.Now, Format: convert.FormatClaude}
	stream := newStream(context.Background(), attempt,
		Provider{ID: 1, Name: "供应商甲"}, Endpoint{}, &Plan{}, nil, options, nil)
	consumeStream(t, stream)

	observation := stream.Observation()
	if !observation.FirstContentAt.IsZero() || observation.TTFT != 0 {
		t.Fatalf("无内容帧时 TTFT 必须留零，实际 FirstContentAt=%v TTFT=%v",
			observation.FirstContentAt, observation.TTFT)
	}
	if got := observation.FirstByte.Milliseconds(); got != 100 {
		t.Fatalf("FirstByte = %dms，期望 100ms（首字节不受 TTFT 口径影响）", got)
	}
}

// TestObserverSkipsClassificationAfterFirstContent 钉住热路径的上界：打戳之后不再分类。
//
// 它同时是性能断言的逻辑依据——分类开销只落在「首个内容帧之前」，与流长无关。
func TestObserverSkipsClassificationAfterFirstContent(t *testing.T) {
	clock := &arrivalClock{now: time.Unix(0, 0)}
	observer := NewObserver(ObservationOptions{
		StartedAt: time.Unix(0, 0),
		Format:    convert.FormatClaude,
		Family:    gate.FamilyAnthropic,
		Now:       clock.Now,
	})
	observer.Push([]byte(`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"a"}}` + "\n\n"))
	if observer.observation.FirstContentAt.IsZero() {
		t.Fatal("首个内容帧应打戳")
	}

	before := observer.observation.FirstContentAt
	clock.set(time.Unix(0, int64(900*time.Millisecond)))
	observer.Push([]byte(`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"b"}}` + "\n\n"))
	if !observer.observation.FirstContentAt.Equal(before) {
		t.Fatalf("首个内容帧之后不得再打戳：%v → %v", before, observer.observation.FirstContentAt)
	}
}
