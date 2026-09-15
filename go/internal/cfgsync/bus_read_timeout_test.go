package cfgsync

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件钉住订阅读取错误的分类：**读截止到点（连接仍活、只是安静）不得被当成断连**。
//
// 为什么必须有独立的单测：生产实测 `cfgsync_subscription_lost` 每 31s 一次（30s 读截止 +
// 1s 重连退避），根因是判据只认 `context.DeadlineExceeded`，而 go-redis 在空闲连接上返回的
// 是裸 `net.OpError`。仓内既有的 bus 用例都要真 Redis（未注入 CCH_TEST_REDIS_URL 即整组
// 跳过），正是这条路径此前无人盯住的原因。故这里自带一个最小 RESP 应答器，不依赖外部 Redis。

func TestIsSubscriberReadTimeoutClassifiesQuietVersusLost(t *testing.T) {
	wrappedTimeout := fmt.Errorf("redis: %w", &net.OpError{
		Op:  "read",
		Net: "tcp",
		Err: &timeoutError{},
	})

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ctx 截止到点", context.DeadlineExceeded, true},
		{"裸 net 读超时（生产形态）", &net.OpError{Op: "read", Net: "tcp", Err: &timeoutError{}}, true},
		{"被包装的 net 读超时", wrappedTimeout, true},
		{"os.ErrDeadlineExceeded", os.ErrDeadlineExceeded, true},
		{"ctx 取消（总线关闭）", context.Canceled, false},
		{"io.EOF", io.EOF, false},
		{"连接已被关闭", net.ErrClosed, false},
		{"连接被对端重置", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, false},
		{"协议错误", errors.New("redis: can't parse reply"), false},
	}
	for _, testCase := range cases {
		if got := isSubscriberReadTimeout(testCase.err); got != testCase.want {
			t.Errorf("%s：isSubscriberReadTimeout = %v，期望 %v（err=%v）", testCase.name, got, testCase.want, testCase.err)
		}
	}
}

// timeoutError 是一个 Timeout() 为真的最小 net.Error，用来构造「读超时」形态而不真等 socket。
type timeoutError struct{}

func (*timeoutError) Error() string   { return "i/o timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

func TestBusIdleSubscriberStaysConnectedPastReadDeadline(t *testing.T) {
	server := newSilentRedisServer(t)
	recorder := &recordingLogger{}
	bus := newTestBus(t, server, recorder)

	resyncs := new(int32)
	messages := make(chan string, 16)
	if _, err := bus.Subscribe(ChannelSystemSettingsUpdated, func(message string) {
		if message == ResyncMessage {
			atomic.AddInt32(resyncs, 1)
		}
		messages <- message
	}); err != nil {
		t.Fatalf("订阅失败：%v", err)
	}
	waitForCondition(t, 5*time.Second, func() bool { return atomic.LoadInt32(resyncs) >= 1 }, "首次订阅的 resync")

	// 跑过至少 6 个读窗口：修复前每个窗口都会自毁重连一次（lost + 重新订阅 + 强制 resync）。
	idleFor := 6 * testReadDeadline
	time.Sleep(idleFor)

	if lost := recorder.count("cfgsync_subscription_lost"); lost != 0 {
		t.Fatalf("%v 内出现 %d 次 cfgsync_subscription_lost：空闲订阅被误判为断连", idleFor, lost)
	}
	if subscribed := recorder.count("cfgsync_subscribed"); subscribed != 1 {
		t.Fatalf("连接数 = %d 次订阅，期望 1：读超时不得触发重连", subscribed)
	}
	if got := atomic.LoadInt32(resyncs); got != 1 {
		t.Fatalf("resync 次数 = %d，期望 1（仅首次订阅）：重连风暴会带来重复 resync", got)
	}
	if server.acceptedConns() < 2 {
		// 读截止一次都没落到 socket 上：用例会退化成空断言（读超时路径根本没被跑到）。
		// go-redis 在读到超时后会自己重拨，故「>1 条连接」正是超时确实发生过的痕迹。
		t.Fatalf("上游只看到 %d 条连接，读截止未触发", server.acceptedConns())
	}
}

func TestBusReconnectsAndResyncsOnRealReadFailure(t *testing.T) {
	server := newSilentRedisServer(t)
	recorder := &recordingLogger{}
	bus := newTestBus(t, server, recorder)

	resyncs := new(int32)
	messages := make(chan string, 32)
	if _, err := bus.Subscribe(ChannelProvidersUpdated, func(message string) {
		if message == ResyncMessage {
			atomic.AddInt32(resyncs, 1)
		}
		messages <- message
	}); err != nil {
		t.Fatalf("订阅失败：%v", err)
	}
	waitForCondition(t, 5*time.Second, func() bool { return atomic.LoadInt32(resyncs) >= 1 }, "首次订阅的 resync")

	// 真实断连：对端关闭连接（socket 层失败，非读超时）。
	server.closeClientConns()

	waitForCondition(t, 5*time.Second, func() bool { return recorder.count("cfgsync_subscription_lost") >= 1 },
		"真实断连必须记 cfgsync_subscription_lost")
	waitForCondition(t, 5*time.Second, func() bool { return recorder.count("cfgsync_subscribed") >= 2 },
		"真实断连必须触发重连")
	waitForCondition(t, 5*time.Second, func() bool { return atomic.LoadInt32(resyncs) >= 2 },
		"重连后必须强制 resync（Pub/Sub 不补发断线窗口）")

	bus.Publish(context.Background(), ChannelProvidersUpdated, "after-reconnect")
	waitForExactMessage(t, messages, "after-reconnect", "重连后仍能收到真实失效消息")
}

// testReadDeadline 是测试用的读窗口：短到 6 个窗口能在毫秒级跑完，长到足够让
// 「连接仍活」与「连接已断」两种形态被真实 socket 区分开。
const testReadDeadline = 120 * time.Millisecond

func newTestBus(t *testing.T, server *silentRedisServer, logger Logger) *Bus {
	t.Helper()

	previous := subscriberReadDeadline
	subscriberReadDeadline = testReadDeadline
	t.Cleanup(func() { subscriberReadDeadline = previous })

	client := redis.NewClient(server.options())
	bus := NewBus(client, logger)
	t.Cleanup(func() {
		_ = bus.Close()
		_ = client.Close()
	})
	return bus
}

// silentRedisServer 是最小的 RESP 应答器：只确认 SUBSCRIBE，其余命令一律不回应。
//
// 它不是 Redis 的替身，只负责把 go-redis 的订阅连接卡在「已连上、随后一直安静」这一形态——
// 生产里那条每 31s 一次的读超时正是从这个形态长出来的。真 Redis 也能测（既有的
// CCH_TEST_REDIS_URL 用例就是这么做的），但那样这些用例在 CI 里整组跳过。
type silentRedisServer struct {
	listener      net.Listener
	mu            sync.Mutex
	conns         []net.Conn
	accepted      int
	subscriptions []serverSubscription
}

// serverSubscription 记下「哪条连接订阅了哪个通道」，供假服务端转发 PUBLISH 用。
type serverSubscription struct {
	connIndex int
	channel   string
}

func newSilentRedisServer(t *testing.T) *silentRedisServer {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听本地端口失败：%v", err)
	}
	server := &silentRedisServer{listener: listener}
	t.Cleanup(func() { _ = listener.Close() })
	go server.acceptLoop()
	return server
}

func (s *silentRedisServer) options() *redis.Options {
	return &redis.Options{
		Addr:            s.listener.Addr().String(),
		Protocol:        2,
		DisableIdentity: true,
	}
}

func (s *silentRedisServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.accepted++
		connIndex := len(s.conns) - 1
		s.mu.Unlock()
		go s.serve(conn, connIndex)
	}
}

func (s *silentRedisServer) serve(conn net.Conn, connIndex int) {
	reader := bufio.NewReader(conn)
	for {
		args, err := readRESPCommand(reader)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}
		switch strings.ToUpper(args[0]) {
		case "HELLO":
			// 回一条普通错误：go-redis 会据此判定「服务端不支持 HELLO」并退回 RESP2，
			// 于是不必实现 HELLO 的握手内容。
			if _, err := conn.Write([]byte("-ERR unknown command 'hello'\r\n")); err != nil {
				return
			}
		case "SUBSCRIBE":
			for _, channel := range args[1:] {
				s.mu.Lock()
				s.subscriptions = append(s.subscriptions, serverSubscription{connIndex: connIndex, channel: channel})
				s.mu.Unlock()
				// 订阅确认；此后不再主动写任何字节（安静连接）。
				if _, err := fmt.Fprintf(conn, "*3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:1\r\n", len(channel), channel); err != nil {
					return
				}
			}
		case "PUBLISH":
			// 把消息转给所有已订阅该通道的连接——真 Redis 由服务端做这件事。
			// 没有它，「断连重连后仍能收到失效消息」在假服务端上永远不可能成立。
			delivered := s.relayPublish(args[1], args[2])
			if _, err := fmt.Fprintf(conn, ":%d\r\n", delivered); err != nil {
				return
			}
		case "PING":
			if _, err := conn.Write([]byte("+PONG\r\n")); err != nil {
				return
			}
		}
	}
}

func (s *silentRedisServer) closeClientConns() {
	s.mu.Lock()
	conns := append([]net.Conn(nil), s.conns...)
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// relayPublish 把一条发布转给所有订阅了该通道的连接，返回送达数。
func (s *silentRedisServer) relayPublish(channel, payload string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	delivered := 0
	for index, conn := range s.conns {
		if !s.subscribedBy(index, channel) {
			continue
		}
		if _, err := fmt.Fprintf(conn, "*3\r\n$7\r\nmessage\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n", len(channel), channel, len(payload), payload); err != nil {
			continue
		}
		delivered++
	}
	return delivered
}

func (s *silentRedisServer) subscribedBy(connIndex int, channel string) bool {
	for _, entry := range s.subscriptions {
		if entry.connIndex == connIndex && entry.channel == channel {
			return true
		}
	}
	return false
}

func (s *silentRedisServer) acceptedConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

// readRESPCommand 读一条 RESP 数组命令（go-redis 一律以 *N 形式发送）。
func readRESPCommand(reader *bufio.Reader) ([]string, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	header = strings.TrimRight(header, "\r\n")
	if !strings.HasPrefix(header, "*") {
		return nil, fmt.Errorf("非数组命令：%q", header)
	}
	count, err := strconv.Atoi(header[1:])
	if err != nil {
		return nil, err
	}

	args := make([]string, 0, count)
	for index := 0; index < count; index++ {
		lengthLine, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		lengthLine = strings.TrimRight(lengthLine, "\r\n")
		if !strings.HasPrefix(lengthLine, "$") {
			return nil, fmt.Errorf("非批量字符串：%q", lengthLine)
		}
		length, err := strconv.Atoi(lengthLine[1:])
		if err != nil {
			return nil, err
		}
		payload := make([]byte, length+2)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, err
		}
		args = append(args, string(payload[:length]))
	}
	return args, nil
}

type recordingLogger struct {
	mu     sync.Mutex
	events []string
}

func (l *recordingLogger) record(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *recordingLogger) count(event string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := 0
	for _, recorded := range l.events {
		if recorded == event {
			total++
		}
	}
	return total
}

func (l *recordingLogger) Debug(event string, _ map[string]any) { l.record(event) }
func (l *recordingLogger) Info(event string, _ map[string]any)  { l.record(event) }
func (l *recordingLogger) Warn(event string, _ map[string]any)  { l.record(event) }
func (l *recordingLogger) Error(event string, _ map[string]any) { l.record(event) }

func waitForExactMessage(t *testing.T, channel <-chan string, want string, description string) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case message := <-channel:
			if message == want {
				return
			}
		case <-deadline:
			t.Fatalf("等待 %s 超时", description)
		}
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, predicate func() bool, description string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", description)
}

func drainResyncCount(channel <-chan string) int {
	total := 0
	for {
		select {
		case message := <-channel:
			if message == ResyncMessage {
				total++
			}
		default:
			return total
		}
	}
}
