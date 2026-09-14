package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{name: "空错误", err: nil, want: ErrorUnknown},
		{name: "nil 回包", err: redis.Nil, want: ErrorNilReply},
		{name: "上下文取消", err: context.Canceled, want: ErrorUnavailable},
		{name: "上下文超时", err: context.DeadlineExceeded, want: ErrorUnavailable},
		{name: "网络错误", err: &net.DNSError{Err: "no such host", Name: "redis"}, want: ErrorUnavailable},
		{name: "脚本缺失", err: errors.New("NOSCRIPT No matching script. Please use EVAL."), want: ErrorScriptMissing},
		{name: "参数数量不符", err: errors.New("ERR Error running script (call to f_1): wrong number of args"), want: ErrorArity},
		{name: "键类型不符", err: errors.New("WRONGTYPE Operation against a key holding the wrong kind of value"), want: ErrorType},
		{name: "不可达", err: errors.New("dial tcp 127.0.0.1:6379: connect: connection refused"), want: ErrorUnavailable},
		{name: "连接池耗尽", err: errors.New("redis: connection pool timeout"), want: ErrorUnavailable},
		{name: "只读副本", err: errors.New("READONLY You can't write against a read only replica."), want: ErrorUnavailable},
		{name: "脚本主动报错", err: fakeRedisError("invalid rolling cost arguments"), want: ErrorReply},
		{name: "脚本运行时错误", err: errors.New("ERR Error running script (call to f_2): user_script:12: attempt to compare nil with number"), want: ErrorReply},
		{name: "未归类", err: errors.New("something else entirely"), want: ErrorUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.want {
				t.Fatalf("分类不符: 期望 %s, 实际 %s", tc.want, got)
			}
		})
	}
}

// fakeRedisError 复刻 Redis 协议层错误（v9 的 redis.Error 是接口，具体类型在 internal/proto 下不可导入）。
type fakeRedisError string

func (e fakeRedisError) Error() string { return string(e) }

func (e fakeRedisError) RedisError() {}

func TestEvalErrorKeepsCauseAndKind(t *testing.T) {
	cause := fmt.Errorf("外层包装: %w", redis.Nil)
	err := error(&EvalError{Script: "touch-session-binding.lua", Kind: ClassifyError(cause), Err: cause})

	var evalErr *EvalError
	if !errors.As(err, &evalErr) {
		t.Fatal("EvalError 必须可被 errors.As 取出")
	}
	if evalErr.Kind != ErrorNilReply {
		t.Fatalf("分类不符: %s", evalErr.Kind)
	}
	if !errors.Is(err, redis.Nil) {
		t.Fatal("包装后必须仍可用 errors.Is 命中 redis.Nil")
	}
	if got := err.Error(); got == "" || !containsAny(got, []string{"touch-session-binding.lua", string(ErrorNilReply)}) {
		t.Fatalf("错误文本缺少脚本名或分类: %q", got)
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	registry, err := Load()
	if err != nil {
		t.Fatalf("加载脚本失败: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	defer func() { _ = rdb.Close() }()

	if _, err := New(nil, registry); err == nil {
		t.Fatal("缺连接必须报错")
	}
	if _, err := New(rdb, nil); err == nil {
		t.Fatal("缺脚本表必须报错")
	}
	client, err := New(rdb, registry)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if client.Scripts() != registry {
		t.Fatal("客户端必须持有传入的脚本表")
	}
	if _, err := client.Eval(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("脚本为空必须报错")
	}
	if _, err := client.EvalConst(context.Background(), "NO_SUCH_CONST", nil, nil); err == nil {
		t.Fatal("未登记的常量名必须报错")
	}
}

// TestScriptForIsMemoized 保证同一脚本只构造一次 go-redis 包装（内含 sha1 计算）。
func TestScriptForIsMemoized(t *testing.T) {
	registry, err := Load()
	if err != nil {
		t.Fatalf("加载脚本失败: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	defer func() { _ = rdb.Close() }()
	client, err := New(rdb, registry)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}

	first, ok := registry.LookupConst("CAS_SESSION_BINDING")
	if !ok {
		t.Fatal("缺少 CAS_SESSION_BINDING")
	}
	second, ok := registry.LookupFile("cas-session-binding.lua")
	if !ok {
		t.Fatal("缺少 cas-session-binding.lua")
	}
	if client.scriptFor(first) != client.scriptFor(second) {
		t.Fatal("同一脚本必须复用同一个 redis.Script")
	}
}

// TestDialRejectsBadURL 不依赖 Redis 可用性。
func TestDialRejectsBadURL(t *testing.T) {
	registry, err := Load()
	if err != nil {
		t.Fatalf("加载脚本失败: %v", err)
	}
	if _, err := Dial(context.Background(), "not a url", registry); err == nil {
		t.Fatal("非法 URL 必须报错")
	}
}

// TestDialFailsWhenUnreachable 用本地必然无人监听的端口验证 Dial 的可达性检查。
func TestDialFailsWhenUnreachable(t *testing.T) {
	if os.Getenv("CCH_TEST_REDIS_URL") != "" {
		t.Skip("已配置真实 Redis，跳过不可达分支")
	}
	registry, err := Load()
	if err != nil {
		t.Fatalf("加载脚本失败: %v", err)
	}
	// 端口 1 属于保留段，无人监听且连接会被立刻拒绝，不需要等待超时。
	if _, err := Dial(context.Background(), "redis://127.0.0.1:1/13", registry); err == nil {
		t.Fatal("不可达的 Redis 必须报错")
	}
}
