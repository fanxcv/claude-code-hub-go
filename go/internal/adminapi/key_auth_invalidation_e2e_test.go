package adminapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是「密钥失效链」的端到端用例（审计 C2）：管理面禁用/删除密钥之后，**下一次**带该密钥的
// 鉴权必须立即按新状态作答，而不是等密钥缓存的 TTL 到期。
//
// 为什么必须端到端：这条链有四段，任何一段单独正确都不代表密钥真的失效——
//  1. 管理面写路径调 Invalidator（keys_handlers.go 的四处 + keys.go 的一处；handler → Invalidator
//     这一段由既有集成用例断言，如 TestAdminKeyLimitPatchIntegration 的 invalidator.keyAuth 断言）；
//  2. Invalidator 广播 cfgsync.DomainAPIKeys（本用例调用的正是该方法）；
//  3. guard 的进程内密钥缓存收到广播后清表（cfgsync.Registry 的订阅，绑在 guard/adapters.go）；
//  4. 下一次解析读回新状态并拒答。
//
// 「只删 Redis 键」与「只断言缓存会清」各自都能变绿，而密钥照样能用满一个 TTL——那正是本用例要
// 排除的假绿：起点取管理面真正调用的方法，终点取 guard 真正的解析结果，中间不许有断点。
//
// 依赖替代说明（替换点只有两处边界，缓存/订阅/广播/解析全是真的）：
//   - 真 Redis 换成进程内的最小 RESP 应答器（与 cfgsync 的 bus_read_timeout_test 同一手法）；
//   - 真 PG 换成 AdapterOptions.AuthKeyRows 注入的可控行源。
func TestKeyAuthInvalidationRejectsKeyOnNextRequest(t *testing.T) {
	const (
		keyValue = "sk-e2e-invalidation"
		keyID    = int64(7)
		// TTL 取 1 分钟：用例必须在这之前就拒答，才叫「立即失效」而不是「TTL 到期」。
		cacheTTL  = time.Minute
		waitLimit = 2 * time.Second
	)

	server := newRelayRedisServer(t)
	client := redis.NewClient(server.options())
	t.Cleanup(func() { _ = client.Close() })
	bus := cfgsync.NewBus(client, nil)
	t.Cleanup(func() { _ = bus.Close() })

	rows := &scriptedKeyRows{}
	registry := cfgsync.NewRegistry(bus)
	adapters, err := guard.NewAdapters(guard.AdapterOptions{
		Pools:          &store.Pools{},
		Bus:            bus,
		Registry:       registry,
		APIKeyCacheTTL: cacheTTL,
		UserCacheTTL:   cacheTTL,
		AuthKeyRows:    rows.load,
	})
	if err != nil {
		t.Fatalf("装配 guard 适配器失败：%v", err)
	}
	t.Cleanup(adapters.Close)

	// 订阅就绪：首次订阅会补发一次 resync（cfgsync.Registry 计数即证据），等它到齐再往下走，
	// 否则第一次广播可能落在订阅建立之前，用例会以「偶发假绿」的形态失败。
	waitForAPIKeysInvalidations(t, registry, 1, waitLimit, "api_keys 订阅就绪")

	invalidator := NewCacheInvalidator(Deps{Store: &store.Pools{}}, InvalidatorOptions{
		Redis: client,
		Bus:   bus,
	})

	cases := []struct {
		name    string
		rows    []string
		wantErr error
	}{
		{"禁用密钥", []string{keyAuthRowJSON(keyID, false)}, guard.ErrKeyDisabled},
		{"删除密钥", nil, guard.ErrKeyNotFound},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			rows.set([]string{keyAuthRowJSON(keyID, true)})

			resolution, err := adapters.Auth.ResolveAPIKey(ctx, keyValue)
			if err != nil {
				t.Fatalf("预热解析应成功：%v", err)
			}
			if resolution.Key.ID != keyID {
				t.Fatalf("预热解析拿到的 keyId 应为 %d，实际 %d", keyID, resolution.Key.ID)
			}

			// 再解析一次：必须命中缓存（行源装载数不增），否则下面的「立即拒答」可能只是没缓存过。
			loads := adapters.Auth.KeyLoads()
			if _, err := adapters.Auth.ResolveAPIKey(ctx, keyValue); err != nil {
				t.Fatalf("第二次解析应命中缓存：%v", err)
			}
			if got := adapters.Auth.KeyLoads(); got != loads {
				t.Fatalf("第二次解析不该再打行源（缓存未命中）：装载数 %d → %d", loads, got)
			}

			// 管理面动作：改库（此处改行源）后调失效广播——写路径调的就是这个方法。
			rows.set(testCase.rows)
			before := registry.Invalidations(cfgsync.DomainAPIKeys)
			invalidator.InvalidateKeyAuth(ctx, keyValue)

			// 广播送达（订阅回调已跑）：这是「内存缓存真被清」的可观测前兆，不代表拒答已发生。
			waitForAPIKeysInvalidations(t, registry, before+1, waitLimit, "api_keys 广播送达")

			// 判据：TTL（1 分钟）远未到期，而解析必须已经按新状态拒答。
			deadline := time.Now().Add(waitLimit)
			var lastErr error
			for {
				_, lastErr = adapters.Auth.ResolveAPIKey(ctx, keyValue)
				if errors.Is(lastErr, testCase.wantErr) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("失效广播后 %v 内未按新状态拒答：want %v, got %v",
						waitLimit, testCase.wantErr, lastErr)
				}
				time.Sleep(2 * time.Millisecond)
			}
		})
	}
}

// TestInvalidateKeyAuthPublishesWithoutCommandConnection 钉住 InvalidateKeyAuth 的「先广播后删
// 键」这个顺序：Redis 命令连接为 nil（未配 Redis）时，广播仍必须出站。若把广播挪到
// `client == nil` 的早退之后，本用例立刻变红——而那种部署下密钥禁用后进程内缓存要等 TTL 到期
// 才失效，正是 C2 要关掉的窗口。
func TestInvalidateKeyAuthPublishesWithoutCommandConnection(t *testing.T) {
	client := redis.NewClient(newRelayRedisServer(t).options())
	t.Cleanup(func() { _ = client.Close() })
	bus := cfgsync.NewBus(client, nil)
	t.Cleanup(func() { _ = bus.Close() })

	registry := cfgsync.NewRegistry(bus)
	unbind, err := registry.Bind(cfgsync.DomainAPIKeys, func() {})
	if err != nil {
		t.Fatalf("绑定 api_keys 域失败：%v", err)
	}
	defer unbind()
	// 首次绑定会补发一次 resync：计数到位即订阅已建立，否则下面的广播可能落在订阅之前。
	waitForAPIKeysInvalidations(t, registry, 1, 2*time.Second, "api_keys 订阅就绪")

	before := registry.Invalidations(cfgsync.DomainAPIKeys)
	invalidator := NewCacheInvalidator(Deps{}, InvalidatorOptions{Bus: bus})
	invalidator.InvalidateKeyAuth(context.Background(), "sk-no-command-conn")
	waitForAPIKeysInvalidations(t, registry, before+1, 2*time.Second, "无命令连接时的失效广播")
}

// waitForAPIKeysInvalidations 等该域累计失效次数到达 want（订阅回调已执行）。
func waitForAPIKeysInvalidations(
	t *testing.T,
	registry *cfgsync.Registry,
	want uint64,
	timeout time.Duration,
	description string,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if registry.Invalidations(cfgsync.DomainAPIKeys) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时（want %d, got %d）",
		description, want, registry.Invalidations(cfgsync.DomainAPIKeys))
}

// scriptedKeyRows 是可切换的密钥归并行源（AuthKeyRows 的测试实现）。
type scriptedKeyRows struct {
	mu    sync.Mutex
	rows  []string
	loads int
}

func (s *scriptedKeyRows) set(rows []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = rows
	s.loads++
}

func (s *scriptedKeyRows) load(context.Context, string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.rows...), nil
}

func (s *scriptedKeyRows) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

// keyAuthRowJSON 造一行 keys ⨝ users 的 row_to_json 结果（列名即 JSON 键名）。
func keyAuthRowJSON(keyID int64, keyEnabled bool) string {
	return fmt.Sprintf(`{
		"key_id": %d, "key_name": "e2e", "key_is_enabled": %t, "key_expires_at": null,
		"key_provider_group": null, "user_id": 3, "user_name": "e2e-user",
		"user_is_enabled": true, "user_expires_at": null, "user_provider_group": null,
		"allowed_clients": null, "blocked_clients": null, "allowed_models": null
	}`, keyID, keyEnabled)
}

// relayRedisServer 是进程内的最小 RESP 应答器：只实现本用例用得到的命令，其中 PUBLISH 把消息
// 转发给所有订阅了该通道的连接（真 Redis 由服务端做这件事，不转发则广播永远送不到订阅者）。
type relayRedisServer struct {
	listener net.Listener
	mu       sync.Mutex
	conns    []net.Conn
	subs     map[int]map[string]struct{}
}

func newRelayRedisServer(t *testing.T) *relayRedisServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听本地端口失败：%v", err)
	}
	server := &relayRedisServer{listener: listener, subs: map[int]map[string]struct{}{}}
	t.Cleanup(func() { _ = listener.Close() })
	go server.acceptLoop()
	return server
}

func (s *relayRedisServer) options() *redis.Options {
	// Protocol 2 + DisableIdentity：go-redis 不做 HELLO/CLIENT SETINFO 握手，假服务端不必实现它们。
	return &redis.Options{Addr: s.listener.Addr().String(), Protocol: 2, DisableIdentity: true}
}

func (s *relayRedisServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		connIndex := len(s.conns) - 1
		s.mu.Unlock()
		go s.serve(conn, connIndex)
	}
}

func (s *relayRedisServer) serve(conn net.Conn, connIndex int) {
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
			// 回一条普通错误：go-redis 据此判定「服务端只讲 RESP2」并继续（无密码时不是致命错误）。
			if _, err := conn.Write([]byte("-ERR unknown command 'hello'\r\n")); err != nil {
				return
			}
		case "SUBSCRIBE":
			for _, channel := range args[1:] {
				s.mu.Lock()
				if s.subs[connIndex] == nil {
					s.subs[connIndex] = map[string]struct{}{}
				}
				s.subs[connIndex][channel] = struct{}{}
				s.mu.Unlock()
				if _, err := fmt.Fprintf(conn,
					"*3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:1\r\n", len(channel), channel); err != nil {
					return
				}
			}
		case "PUBLISH":
			if _, err := fmt.Fprintf(conn, ":%d\r\n", s.relayPublish(args[1], args[2])); err != nil {
				return
			}
		case "PING":
			if _, err := conn.Write([]byte("+PONG\r\n")); err != nil {
				return
			}
		case "DEL":
			if _, err := conn.Write([]byte(":1\r\n")); err != nil {
				return
			}
		default:
			// 其余命令回 +OK：应答流与命令流保持对齐，go-redis 才不会把后续读串位。
			if _, err := conn.Write([]byte("+OK\r\n")); err != nil {
				return
			}
		}
	}
}

// relayPublish 把一条发布转给所有订阅了该通道的连接，返回送达数。
func (s *relayRedisServer) relayPublish(channel, payload string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	delivered := 0
	for index, conn := range s.conns {
		if _, ok := s.subs[index][channel]; !ok {
			continue
		}
		if _, err := fmt.Fprintf(conn,
			"*3\r\n$7\r\nmessage\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n",
			len(channel), channel, len(payload), payload); err != nil {
			continue
		}
		delivered++
	}
	return delivered
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
