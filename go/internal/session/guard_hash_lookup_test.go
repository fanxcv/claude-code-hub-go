package session

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
)

// 本文件钉住「正文哈希降级查找」的日志语义：**未命中不是故障**。
//
// 为什么值得单独一个文件：`session.ensure.hash_lookup_failed` 的 error 值在生产上全部是
// `redis: nil`——即每次首见会话都报一条 warn，真实 Redis 故障反而被淹没在这堆噪音里
// （2026-09-21 生产取证：90 条 warn 全部为此）。
//
// 判据取「日志里有没有那条 warn」，因此需要一个可控的假 Redis：CI 不注入
// `CCH_TEST_REDIS_URL`，真库用例整组跳过，那种测法等于没测。假服务端按 RESP2 应答
// `GET`：命中给 bulk string，未命中给 nil bulk（`$-1`），故障给错误行——三种语义都在
// 协议层可辨，不需要任何生产代码为测试让路。

// fakeRedisReply 是假服务端对 `GET` 的应答形态。
type fakeRedisReply int

const (
	// fakeRedisMiss 回答 nil bulk（`$-1\r\n`）：键不存在，go-redis 归一为 redis.Nil。
	fakeRedisMiss fakeRedisReply = iota
	// fakeRedisHit 回答 bulk string：键存在。
	fakeRedisHit
	// fakeRedisError 回答一个真实错误（非 Nil）：模拟 Redis 侧故障。
	fakeRedisError
)

const fakeRedisHitValue = "sess_fake_hash_hit"

// fakeRedisServer 是按 RESP2 应答的最小假 Redis：只实现 go-redis 建连所需的三条命令
// （HELLO 拒绝、CLIENT SETINFO 接受、GET 按 reply 应答）。
//
// HELLO 一律回错误：go-redis 据此判定对端不支持 HELLO 并退回 RESP2（见 redis.go 的
// `isRedisError(initErr)` 分支），于是不必实现握手内容——仓库既有先例同此手法
// （cfgsync/bus_read_timeout_test.go 的 silentRedisServer）。
type fakeRedisServer struct {
	listener net.Listener
	reply    fakeRedisReply
	// getCommands 记录收到的 GET 键，供断言「确实走到了 Redis」。
	mu          sync.Mutex
	getCommands []string
}

func newFakeRedisServer(t *testing.T, reply fakeRedisReply) *fakeRedisServer {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听本地端口失败: %v", err)
	}
	server := &fakeRedisServer{listener: listener, reply: reply}
	t.Cleanup(func() { _ = listener.Close() })
	go server.acceptLoop()
	return server
}

func (s *fakeRedisServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

func (s *fakeRedisServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	reader := bufio.NewReader(conn)
	for {
		args, err := readRESPArray(reader)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}
		switch strings.ToUpper(args[0]) {
		case "HELLO":
			if _, err := conn.Write([]byte("-ERR unknown command 'hello'\r\n")); err != nil {
				return
			}
		case "CLIENT":
			if _, err := conn.Write([]byte("+OK\r\n")); err != nil {
				return
			}
		case "GET":
			if len(args) > 1 {
				s.mu.Lock()
				s.getCommands = append(s.getCommands, args[1])
				s.mu.Unlock()
			}
			if err := s.writeGetReply(conn); err != nil {
				return
			}
		default:
			if _, err := conn.Write([]byte("+OK\r\n")); err != nil {
				return
			}
		}
	}
}

func (s *fakeRedisServer) writeGetReply(conn net.Conn) error {
	switch s.reply {
	case fakeRedisMiss:
		_, err := conn.Write([]byte("$-1\r\n"))
		return err
	case fakeRedisHit:
		_, err := fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(fakeRedisHitValue), fakeRedisHitValue)
		return err
	default:
		_, err := conn.Write([]byte("-ERR fake redis failure\r\n"))
		return err
	}
}

func (s *fakeRedisServer) getKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.getCommands...)
}

// options 给出连到假服务端的客户端参数：短超时避免用例挂住。
func (s *fakeRedisServer) options() *redis.Options {
	return &redis.Options{
		Addr:            s.listener.Addr().String(),
		Protocol:        2,
		DisableIdentity: true,
		MaxRetries:      0,
		DialTimeout:     2 * time.Second,
		ReadTimeout:     2 * time.Second,
		WriteTimeout:    2 * time.Second,
	}
}

// readRESPArray 读一条 RESP 数组命令（go-redis 一律以 *N 形式发送）。
func readRESPArray(reader *bufio.Reader) ([]string, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	header = strings.TrimRight(header, "\r\n")
	if !strings.HasPrefix(header, "*") {
		return nil, fmt.Errorf("非数组命令: %q", header)
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
			return nil, fmt.Errorf("非批量字符串: %q", lengthLine)
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

// fakeRedisBinder 把适配器接到假服务端上，并捕获日志输出。
func fakeRedisBinder(t *testing.T, reply fakeRedisReply) (*SessionBinderAdapter, *fakeRedisServer, *bytes.Buffer) {
	t.Helper()

	server := newFakeRedisServer(t, reply)
	rdb := redis.NewClient(server.options())
	t.Cleanup(func() { _ = rdb.Close() })

	registry, err := ratelimit.Load()
	if err != nil {
		t.Fatalf("加载脚本注册表失败: %v", err)
	}
	client, err := ratelimit.New(rdb, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}

	logs := &bytes.Buffer{}
	adapter := NewSessionBinderAdapter(BinderOptions{
		Client: NewBinder(client),
		Logger: logx.New(logs),
	})
	return adapter, server, logs
}

// hashFallbackBody 造一份「无客户端 session id、但有可哈希 messages」的正文，
// 即唯一会走正文哈希降级查找的入口形状。
func hashFallbackBody() map[string]any {
	return map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "哈希降级路径的探针消息"},
		},
	}
}

// TestHashLookupMissIsNotReportedAsFailure 钉住本次修复：键不存在（redis.Nil）是正常未命中，
// 不得打 `session.ensure.hash_lookup_failed`。
func TestHashLookupMissIsNotReportedAsFailure(t *testing.T) {
	adapter, server, logs := fakeRedisBinder(t, fakeRedisMiss)

	result, err := adapter.Ensure(context.Background(), guardSessionRequest(hashFallbackBody()))
	if err != nil {
		t.Fatalf("未命中不应让 Ensure 报错: %v", err)
	}
	// 未命中的语义等价性：仍要分配一个可用会话（不是复用）。
	if result.SessionID == "" {
		t.Fatal("未命中时应新建会话")
	}
	if got := server.getKeys(); len(got) == 0 {
		t.Fatal("前提不成立：本次没有走到哈希查找（假服务端未收到 GET）")
	} else if got[0] != TenantContentHashSessionKey(testKeyID, CalculateMessagesHash(hashFallbackBody()["messages"])) {
		t.Fatalf("哈希键不符：%q", got[0])
	}
	if strings.Contains(logs.String(), "hash_lookup_failed") {
		t.Fatalf("redis.Nil 属正常未命中，不得报 hash_lookup_failed，实得日志:\n%s", logs.String())
	}
}

// TestHashLookupRealErrorIsReported 钉住反例：真实 Redis 故障仍必须留痕，
// 否则修复会把「故障可见性」一起删掉。
func TestHashLookupRealErrorIsReported(t *testing.T) {
	adapter, server, logs := fakeRedisBinder(t, fakeRedisError)

	if _, err := adapter.Ensure(context.Background(), guardSessionRequest(hashFallbackBody())); err != nil {
		t.Fatalf("哈希查找失败不应让 Ensure 报错: %v", err)
	}
	if got := server.getKeys(); len(got) == 0 {
		t.Fatal("前提不成立：本次没有走到哈希查找（假服务端未收到 GET）")
	}
	if !strings.Contains(logs.String(), "hash_lookup_failed") {
		t.Fatalf("真实 Redis 故障必须报 hash_lookup_failed，实得日志:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "fake redis failure") {
		t.Fatalf("日志须带故障原文以便排查，实得日志:\n%s", logs.String())
	}
}
