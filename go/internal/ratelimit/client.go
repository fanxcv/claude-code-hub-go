package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

// 错误分类。调用方据此决定「退化为进程内降级」还是「向客户端报错」，
// 因此分类必须可判别，不能只靠字符串比对日志。
const (
	// ErrorScriptMissing 表示 Redis 侧没有这段脚本（NOSCRIPT / 脚本被 SCRIPT FLUSH 清掉）。
	ErrorScriptMissing ErrorKind = "script_missing"
	// ErrorArity 表示 KEYS/ARGV 数量或形态不符合脚本预期。
	ErrorArity ErrorKind = "arity"
	// ErrorType 表示键类型与脚本预期不符（WRONGTYPE）。
	ErrorType ErrorKind = "type"
	// ErrorUnavailable 表示 Redis 不可达、超时、连接池耗尽或处于不可服务状态。
	ErrorUnavailable ErrorKind = "unavailable"
	// ErrorReply 表示脚本用 {err=...} 主动返回的业务错误（例如参数非法、状态冲突）。
	ErrorReply ErrorKind = "error_reply"
	// ErrorNilReply 表示脚本返回 nil/false。
	ErrorNilReply ErrorKind = "nil_reply"
	// ErrorUnknown 表示未归类的错误。
	ErrorUnknown ErrorKind = "unknown"
)

// ErrorKind 是 Redis 调用错误的可判别分类。
type ErrorKind string

// EvalError 包装一次脚本调用的失败，并附带分类与脚本名。
type EvalError struct {
	Script string
	Kind   ErrorKind
	Err    error
}

func (e *EvalError) Error() string {
	return fmt.Sprintf("执行 Lua 脚本 %s 失败[%s]: %v", e.Script, e.Kind, e.Err)
}

// Unwrap 保留原始错误，使 errors.Is(err, redis.Nil) 一类判断继续成立。
func (e *EvalError) Unwrap() error {
	return e.Err
}

// arityHints / typeHints / unavailableHints 是分类用的子串表。
// 顺序敏感：先判脚本缺失，再判参数形态，再判类型，最后才落到业务错误。
var (
	missingHints     = []string{"NOSCRIPT", "script not found", "No matching script"}
	arityHints       = []string{"wrong number of args", "wrong number of arguments", "number of keys", "wrong number of parameters"}
	typeHints        = []string{"WRONGTYPE", "Operation against a key holding the wrong kind of value"}
	unavailableHints = []string{
		"connection refused", "connection reset", "broken pipe", "i/o timeout", "unexpected EOF",
		"pool timeout", "max number of clients reached", "LOADING", "CLUSTERDOWN", "MASTERDOWN",
		"READONLY", "NOAUTH", "OOM command not allowed",
	}
	replyHints = []string{"Error running script", "user_script:"}
)

// ClassifyError 把 Redis 调用错误归类。nil 返回 ErrorUnknown，调用方应先判 nil。
func ClassifyError(err error) ErrorKind {
	if err == nil {
		return ErrorUnknown
	}
	if errors.Is(err, redis.Nil) {
		return ErrorNilReply
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrorUnavailable
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ErrorUnavailable
	}

	message := err.Error()
	if containsAny(message, missingHints) {
		return ErrorScriptMissing
	}
	if containsAny(message, arityHints) {
		return ErrorArity
	}
	if containsAny(message, typeHints) {
		return ErrorType
	}
	if containsAny(message, unavailableHints) {
		return ErrorUnavailable
	}
	var redisErr redis.Error
	if errors.As(err, &redisErr) || containsAny(message, replyHints) {
		return ErrorReply
	}
	return ErrorUnknown
}

func containsAny(message string, hints []string) bool {
	for _, hint := range hints {
		if strings.Contains(message, hint) {
			return true
		}
	}
	return false
}

// Client 是脚本调用层：持有连接与脚本注册表。
type Client struct {
	rdb     redis.UniversalClient
	scripts *Registry
	// cache 按清单摘要缓存 go-redis 的 Script，避免每次调用重算。
	// 注意：EVALSHA 用的 sha1 由 go-redis 依据脚本正文自行计算，与本包的 sha256 清单校验无关。
	cache sync.Map
}

// New 组装调用层。两个依赖都必须非 nil：缺脚本表就没有可执行语义，缺连接就无法调用。
func New(rdb redis.UniversalClient, scripts *Registry) (*Client, error) {
	if rdb == nil {
		return nil, errors.New("ratelimit: 连接不能为空")
	}
	if scripts == nil {
		return nil, errors.New("ratelimit: 脚本注册表不能为空")
	}
	return &Client{rdb: rdb, scripts: scripts}, nil
}

// Dial 按 URL 建连并确认可达，再组装调用层。
func Dial(ctx context.Context, rawURL string, scripts *Registry) (*Client, error) {
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("解析 Redis URL 失败: %w", err)
	}
	rdb := redis.NewClient(options)
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("连接 Redis 失败: %w", err)
	}
	return New(rdb, scripts)
}

// Raw 暴露底层连接，供不涉及脚本的命令使用。
func (c *Client) Raw() redis.UniversalClient {
	return c.rdb
}

// Scripts 暴露脚本注册表。
func (c *Client) Scripts() *Registry {
	return c.scripts
}

// Close 释放连接。
func (c *Client) Close() error {
	return c.rdb.Close()
}

// Preload 在启动时把全部脚本 SCRIPT LOAD 进 Redis，让稳态调用直接走 EVALSHA。
//
// 不预热也能工作（第一次调用会回退 EVAL 并顺带缓存），预热只是省掉每个 worker 的首次往返。
func (c *Client) Preload(ctx context.Context) (int, error) {
	loaded := 0
	for _, script := range c.scripts.All() {
		if err := c.scriptFor(script).Load(ctx, c.rdb).Err(); err != nil {
			return loaded, &EvalError{Script: script.File, Kind: ClassifyError(err), Err: err}
		}
		loaded++
	}
	return loaded, nil
}

// Eval 执行脚本：先 EVALSHA，Redis 报 NOSCRIPT 时自动回退 EVAL（go-redis 的 Script 语义）。
func (c *Client) Eval(ctx context.Context, script *Script, keys []string, argv []any) (any, error) {
	if script == nil {
		return nil, errors.New("ratelimit: 脚本不能为空")
	}
	value, err := c.scriptFor(script).Run(ctx, c.rdb, keys, argv...).Result()
	if err != nil {
		return nil, &EvalError{Script: script.File, Kind: ClassifyError(err), Err: err}
	}
	return value, nil
}

// EvalConst 按 Node 侧常量名执行脚本，未登记的常量名视为调用方缺陷。
func (c *Client) EvalConst(ctx context.Context, constName string, keys []string, argv []any) (any, error) {
	script, ok := c.scripts.LookupConst(constName)
	if !ok {
		return nil, fmt.Errorf("ratelimit: 未登记的脚本常量名 %s", constName)
	}
	return c.Eval(ctx, script, keys, argv)
}

func (c *Client) scriptFor(script *Script) *redis.Script {
	if cached, ok := c.cache.Load(script.SHA256); ok {
		return cached.(*redis.Script)
	}
	created := redis.NewScript(script.Source)
	actual, _ := c.cache.LoadOrStore(script.SHA256, created)
	return actual.(*redis.Script)
}
