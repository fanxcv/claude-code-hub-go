package usagefeed

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件放两个测试替身：
//
//  1. `recordingClient` 拦截 Publish 的载荷，让「窗口合并」「非法 id 丢弃」这类用例不必依赖
//     真 Redis（也就能在无 Redis 的环境里跑），同时真的走一遍载荷编码。
//  2. `subscribedChan` 等订阅握手真正完成——Redis Pub/Sub **不补发**连接建立前的消息，
//     不等就发会得到随机的假红。

// recordingClient 只实现 Publish；其余方法走嵌入接口的零值（一旦被调用即 panic，
// 正好提示用例越界访问了未替身的能力）。
type recordingClient struct {
	redis.UniversalClient

	mu      sync.Mutex
	payload []string
	wake    chan struct{}
}

func newRecordingClient() *recordingClient {
	return &recordingClient{wake: make(chan struct{}, 64)}
}

// Publish 记录载荷。**必须同时接受 string 与 []byte**：生产路径传的是 []byte
// （json.Marshal 的结果），只认 string 会让替身静默记下空串，用例就以看不懂的
// JSON 解析错误收场。
func (c *recordingClient) Publish(_ context.Context, _ string, message any) *redis.IntCmd {
	var text string
	switch value := message.(type) {
	case string:
		text = value
	case []byte:
		text = string(value)
	}
	c.mu.Lock()
	c.payload = append(c.payload, text)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return redis.NewIntCmd(context.Background())
}

func (c *recordingClient) published() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.payload...)
}

// waitPublished 等到至少 count 条发布（或超时），返回已收到的载荷。
func (c *recordingClient) waitPublished(t *testing.T, timeout time.Duration, count int) []string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if got := c.published(); len(got) >= count {
			return got
		}
		select {
		case <-c.wake:
		case <-deadline:
			return c.published()
		}
	}
}

func decodeSignal(t *testing.T, payload string) Signal {
	t.Helper()
	var signal Signal
	if err := json.Unmarshal([]byte(payload), &signal); err != nil {
		t.Fatalf("载荷不是合法信号 JSON（%q）: %v", payload, err)
	}
	return signal
}

// subscribedChan 返回「订阅握手完成」的通道与 Options 侧的钩子。
func subscribedChan() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	return ch, func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// waitSubscribed 等订阅握手完成。等不到就失败并说明原因（而不是让后续断言以假红收场）。
func waitSubscribed(t *testing.T, ready <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(timeout):
		t.Fatal("订阅循环未在预期时间内与 Redis 完成握手")
	}
}

// waitSignal 在超时内等一条信号；等不到即返回 false。
//
// 用有界超时而不是真实长等待：门禁要求测试不得等真实长超时（编排纪律 §9）。
func waitSignal(t *testing.T, sub Subscription, timeout time.Duration) (Signal, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	signal, err := sub.Wait(ctx)
	if err != nil {
		return Signal{}, false
	}
	return signal, true
}
