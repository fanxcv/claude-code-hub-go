// Package jobs 承载 cchd 的后台任务：端点探活、清理类、路由轨迹 outbox 回收。
//
// 命名契约：本包凡由本文件族（ops/probe/cleanup/outbox）定义的导出符号一律以 Ops 前缀打头，
// 以免与同包内其它波次新增的符号（如价格同步的调度器）相撞。
//
// 职责边界：本文件只给「任务是什么」与「共享依赖」，不含调度循环。
// 调度由外部驱动（cmd/cchd 的 opsRuntime；将来并入统一调度器时它只需消费 OpsTasks）。
// 这样任务体可在测试里被单轮驱动，不必依赖真实时钟。
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/health"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// OpsDeps 是后台任务的共享依赖面。
type OpsDeps struct {
	// Pools 为 nil 时所有需要数据库的任务直接报错（装配缺陷应显式可见）。
	Pools *store.Pools
	// Redis 为 nil 时分布式互斥退化为进程内互斥（与 Node 的 memory 降级同形）。
	Redis redis.UniversalClient
	// Health 为 nil 时跳过端点熔断记账（Node 在无 Redis 时同样只改内存态）。
	Health *health.Writer
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Now 可注入时钟；nil 时用 time.Now。
	Now func() time.Time
	// HTTP 是探活拨测客户端；nil 时用 OpsDefaultHTTPClient。
	HTTP *http.Client
	// ProbeTargets 是探活目标来源；nil 时用 Pools.FindProbeEndpoints（全库启用端点）。
	//
	// 这个缝是为**测试隔离**而留：探活任务默认扫全库，用例若无法把自己钉成唯一目标，
	// 就会连带拨测库里的真实上游（慢、不稳、还改写别人的快照列）。
	ProbeTargets func(ctx context.Context) ([]store.ProbeEndpoint, error)
}

// OpsOutcome 是单轮执行的结果（供日志与巡检断言）。
type OpsOutcome struct {
	// Processed 是本轮实际处理的对象数：探活条数 / 删除行数 / 重放条数。
	Processed int
	// Fields 是要一并落日志的附加字段，键名与 Node 的日志字段同名。
	Fields map[string]any
}

// OpsTask 是一个可被统一调度器驱动的后台任务。
type OpsTask struct {
	// Name 是任务名，用于日志与去重（同时是互斥键的后缀来源）。
	Name string
	// Every 是本任务的触发间隔。
	Every time.Duration
	// Run 执行一轮；返回 nil 错误表示本轮正常（含「本轮无工作」）。
	Run func(ctx context.Context) (OpsOutcome, error)
}

// now 返回本轮使用的时钟读数。
func (d OpsDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// logger 返回可用的日志器。
func (d OpsDeps) logger() *logx.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return logx.New(nil)
}

// client 返回探活拨测客户端。
func (d OpsDeps) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return OpsDefaultHTTPClient()
}

// probeTargets 返回本轮要拨测的端点集。
func (d OpsDeps) probeTargets(ctx context.Context) ([]store.ProbeEndpoint, error) {
	if d.ProbeTargets != nil {
		return d.ProbeTargets(ctx)
	}
	return d.Pools.FindProbeEndpoints(ctx)
}

// OpsDefaultHTTPClient 建探活用的 HTTP 客户端。
//
// 不跟随重定向（Node probe.ts 的 fetch 用 redirect: "manual"：3xx 本身就是「端点活着」的证据，
// 跟到别处会把无关站点的状态算到该端点上）；超时由每轮 ctx 之外的单次拨测超时控制。
func OpsDefaultHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        4,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

// OpsLeaderLock 是分布式互斥（Node leader-lock.ts 的同形实现）。
//
// 键名与 Node 逐字一致：切换期两侧读写同一把锁，避免 Node 与 Go 同时拨测同一批端点。
type OpsLeaderLock struct {
	key   string
	id    string
	redis redis.UniversalClient
}

// opsRenewCompareAndPexpire 与 Node renewLeaderLock 的 Lua 逐字一致。
const opsRenewCompareAndPexpire = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
else
  return 0
end`

// opsReleaseCompareAndDel 与 Node releaseLeaderLock 的 Lua 逐字一致。
const opsReleaseCompareAndDel = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
else
  return 0
end`

// AcquireOpsLeaderLock 尝试取锁。第三个返回值区分「没取到」与「取锁出错」。
//
// 无 Redis 时按 Node 的 memory 降级语义直接持锁：单进程部署仍应拨测，
// 代价是拿不到跨进程互斥（与 Node 相同）。
func (d OpsDeps) AcquireOpsLeaderLock(
	ctx context.Context,
	key string,
	ttl time.Duration,
) (*OpsLeaderLock, bool, error) {
	if d.Redis == nil {
		return &OpsLeaderLock{key: key, id: opsLockID()}, true, nil
	}
	id := opsLockID()
	ok, err := d.Redis.SetNX(ctx, key, id, ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("jobs: 取后台任务锁失败: %w", err)
	}
	if !ok {
		return nil, false, nil
	}
	return &OpsLeaderLock{key: key, id: id, redis: d.Redis}, true, nil
}

// Key 返回锁键名（日志用）。
func (l *OpsLeaderLock) Key() string {
	if l == nil {
		return ""
	}
	return l.key
}

// Renew 续约；返回 false 表示锁已被他人持有（或 Redis 不可用）。
func (l *OpsLeaderLock) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	if l == nil || l.redis == nil {
		return l != nil, nil
	}
	result, err := l.redis.Eval(ctx, opsRenewCompareAndPexpire, []string{l.key}, l.id, ttl.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("jobs: 续约后台任务锁失败: %w", err)
	}
	return result == 1, nil
}

// Release 释放锁（只在仍由本持有者持有时删除）。
func (l *OpsLeaderLock) Release(ctx context.Context) error {
	if l == nil || l.redis == nil {
		return nil
	}
	if err := l.redis.Eval(ctx, opsReleaseCompareAndDel, []string{l.key}, l.id).Err(); err != nil {
		return fmt.Errorf("jobs: 释放后台任务锁失败: %w", err)
	}
	return nil
}

// opsLockID 生成锁持有者标识（Node：时间戳 + 随机后缀）。
func opsLockID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d-rand-failed", time.Now().UnixNano())
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(buf))
}

// OpsEnv 是本包消费的环境变量读取面（boot 注入，测试可替换）。
type OpsEnv = config.LookupEnvFunc
