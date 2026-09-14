package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

const (
	// ruleLoadTimeout 限制单次域装载的时长：装载是启动就绪的闸门，不能无限等。
	ruleLoadTimeout = 10 * time.Second
	// ruleLoadAttempts 是单次触发后的装载尝试次数上限，ruleLoadBackoff 为首次退避。
	// 有界重试的理由：首次 resync 落空（例如 PG 抖动）后，Redis 不会自动再发一次失效，
	// 没有重试就会永久停在「规则未装载」。
	ruleLoadAttempts   = 4
	ruleLoadBackoff    = time.Second
	ruleLoadBackoffMax = 8 * time.Second
)

// rulesSync 管理 cfgsync 的订阅生命周期与规则就绪门。
//
// 就绪语义：**已登记的域全部装载完成**，才把规则快照标记为已装载（即 /readyz 的 rules 项）。
// 刻意不用 cfgsync.Registry.Ready()：它要求全部事件驱动域都已登记且已装载，而本进程当前
// 还没有消费这些域的数据面缓存（路由、限流、请求过滤器的缓存在后续波次接入），用 Ready()
// 会让 /readyz 永远 503。用「已登记域全部装载」既如实反映本进程已完成的准备工作，又会在
// 后续波次登记域之后自动变严。
//
// 登记为零时（当前状态）就绪判定为空真，此时唯一的真事实是订阅连接可用——所以 Start 会
// 做一次真实的 Redis 往返，而不是直接宣称就绪。
type rulesSync struct {
	logger   *logx.Logger
	snapshot *cfgsync.Snapshot
	registry *cfgsync.Registry
	bus      *cfgsync.Bus
	client   redis.UniversalClient

	mu         sync.Mutex
	registered []cfgsync.Domain

	// retryBase / retryAttempts 可被测试覆盖；默认取上面的常量。
	retryBase     time.Duration
	retryAttempts int
}

// newRulesSync 建一个尚未接入订阅通道的规则同步器。
func newRulesSync(logger *logx.Logger, snapshot *cfgsync.Snapshot) *rulesSync {
	if logger == nil {
		logger = logx.New(nil)
	}
	return &rulesSync{
		logger:        logger,
		snapshot:      snapshot,
		registry:      cfgsync.NewRegistry(nil),
		retryBase:     ruleLoadBackoff,
		retryAttempts: ruleLoadAttempts,
	}
}

// attach 接入订阅总线。client 为 nil（未配置 Redis）时保持「只装载不订阅」的运行方式。
func (r *rulesSync) attach(client redis.UniversalClient) {
	if client == nil {
		return
	}
	r.client = client
	r.bus = cfgsync.NewBus(client, r.logger)
	r.registry = cfgsync.NewRegistry(r.bus)
}

// Register 登记一个本进程消费的配置域。
//
// 有订阅通道时：经 registry 订阅该域通道（首次订阅成功即派发 ResyncMessage，与 Node 一致），
// 回调里执行 load 从权威存储重载，成功后标记该域已装载。无订阅通道时：启动时装载一次，
// 此后不再因外部变更失效——调用方须自行确认这种退化可接受。
//
// 本进程当前不登记任何域（见 registerDomains）：数据面缓存在后续波次接入后才登记。
func (r *rulesSync) Register(domain cfgsync.Domain, load func(ctx context.Context) error) error {
	if load == nil {
		return errors.New("rules: load 不能为空")
	}
	r.mu.Lock()
	r.registered = append(r.registered, domain)
	bus := r.bus
	r.mu.Unlock()

	if bus == nil {
		// 无订阅通道时只能装载一次；放到后台跑，不让规则装载阻塞监听——
		// 冷启动期探针必须有人应答，否则编排层分不清「在预热」与「没起来」。
		go r.load(domain, load)
		return nil
	}
	if _, err := r.registry.Bind(domain, func() { r.load(domain, load) }); err != nil {
		return fmt.Errorf("rules: 登记配置域 %s 失败: %w", domain, err)
	}
	return nil
}

// load 执行一次装载（含退避重试），成功后标记装载完成并推进就绪门。
func (r *rulesSync) load(domain cfgsync.Domain, load func(ctx context.Context) error) {
	attempts := r.retryAttempts
	if attempts < 1 {
		attempts = 1
	}
	backoff := r.retryBase
	if backoff <= 0 {
		backoff = ruleLoadBackoff
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), ruleLoadTimeout)
		err := load(ctx)
		cancel()
		if err == nil {
			r.registry.MarkLoaded(domain)
			r.logger.Info("rules_domain_loaded", map[string]any{
				"domain":   string(domain),
				"attempt":  attempt,
				"attempts": attempts,
			})
			r.markSnapshotIfReady()
			return
		}
		lastErr = err
		if attempt == attempts {
			break
		}
		r.logger.Warn("rules_load_retry", map[string]any{
			"domain":  string(domain),
			"attempt": attempt,
			"error":   err.Error(),
			"backoff": backoff.Milliseconds(),
		})
		time.Sleep(backoff)
		backoff *= 2
		if backoff > ruleLoadBackoffMax {
			backoff = ruleLoadBackoffMax
		}
	}

	// 装载失败就保持「未装载」：宁可 /readyz 503，也不要带着空规则表对外服务。
	r.logger.Error("rules_load_failed", map[string]any{
		"domain":   string(domain),
		"attempts": attempts,
		"error":    lastErr.Error(),
	})
}

// Start 在登记完成后调用：确认真实依赖可达并把就绪门判一次。
func (r *rulesSync) Start(ctx context.Context) error {
	if r.client != nil {
		if err := r.client.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("rules: 订阅连接不可用: %w", err)
		}
	}
	r.markSnapshotIfReady()
	return nil
}

// markSnapshotIfReady 在已登记域全部装载后把规则快照标记为已装载。
func (r *rulesSync) markSnapshotIfReady() {
	r.mu.Lock()
	domains := make([]cfgsync.Domain, len(r.registered))
	copy(domains, r.registered)
	r.mu.Unlock()

	for _, domain := range domains {
		if !r.registry.Loaded(domain) {
			return
		}
	}
	r.snapshot.MarkLoaded(fmt.Sprintf("cchd-domains-%d", len(domains)))
}

// RegisteredDomains 返回已登记的域，供启动日志说明就绪门当前由什么驱动。
func (r *rulesSync) RegisteredDomains() []cfgsync.Domain {
	r.mu.Lock()
	defer r.mu.Unlock()
	domains := make([]cfgsync.Domain, len(r.registered))
	copy(domains, r.registered)
	return domains
}

// Close 解绑全部订阅并停掉总线；可重复调用。
func (r *rulesSync) Close() error {
	r.registry.Close()
	if r.bus == nil {
		return nil
	}
	return r.bus.Close()
}

// registerDomains 登记本进程消费的配置域。
//
// 当前为空：Go 侧还没有任何读取这些域的消费者（数据面的路由缓存、限流配置、请求过滤器
// 缓存都在后续波次落地）。接入方式是在拿到缓存句柄后登记，例如
//
//	rules.Register(cfgsync.DomainProviders, func(ctx context.Context) error {
//	    return providersCache.Reload(ctx)
//	})
//
// 登记后 /readyz 的 rules 项会一直等到该域装载成功才变 ok。
//
// 参数中的 ctx 目前未使用，保留是为了让后续的装载函数能接受启动上下文。
func registerDomains(_ context.Context, rules *rulesSync) error {
	rules.logger.Info("rules_domains_registered", map[string]any{
		"count": 0,
		"note":  "数据面缓存尚未接入，订阅已就绪但不参与就绪判定",
	})
	return nil
}
