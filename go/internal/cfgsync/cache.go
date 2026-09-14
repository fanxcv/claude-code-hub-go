package cfgsync

import (
	"context"
	"sync"
	"time"
)

// ValueCache 是「单个值 + TTL + 版本守卫 + in-flight 合并」的进程内缓存，
// 对应 src/lib/config/system-settings-cache.ts 与 src/lib/cache/provider-cache.ts。
//
// 三条关键语义（三者都必须保留，否则会退回已知缺陷）：
//  1. in-flight 合并：TTL 到期或冷缓存下，N 个并发未命中只发一次加载，否则同一条
//     查询会被 N 个并发请求各打一次（实测惊群：一次 TTL 到期打出约 30 条相同查询）。
//  2. 版本守卫：加载期间发生的失效不得让旧结果重新写回缓存。
//  3. 失败降级：加载失败时优先复用旧值；没有旧值时才交给 fallback。
type ValueCache[T any] struct {
	mu         sync.Mutex
	ttl        time.Duration
	value      *T
	expiresAt  time.Time
	hasExpiry  bool
	version    uint64
	inFlight   *valueFlight[T]
	clearOnBad bool
	now        func() time.Time
}

type valueFlight[T any] struct {
	done  chan struct{}
	value T
	err   error
}

// ValueCacheOption 调整 ValueCache 行为。
type ValueCacheOption[T any] func(*ValueCache[T])

// WithValueCacheClock 注入时钟（测试用）。
func WithValueCacheClock[T any](now func() time.Time) ValueCacheOption[T] {
	return func(cache *ValueCache[T]) { cache.now = now }
}

// WithValueCacheNoExpiry 让缓存永不按时间过期（对应事件驱动重载的域，如敏感词、
// 请求过滤器、错误规则：它们只靠失效消息重载，没有 TTL）。
func WithValueCacheNoExpiry[T any]() ValueCacheOption[T] {
	return func(cache *ValueCache[T]) { cache.hasExpiry = false }
}

// WithValueCacheDropInFlightOnInvalidate 在失效时同时丢弃在途加载。
//
// 对应 provider-endpoint-cache.ts 的 `state.inFlight.clear()`：失效后新请求另起一次
// 查询拿到失效后的数据，而不是继续等一个失效前发起的旧快照。默认（settings 语义）
// 保留在途加载，等待者仍会拿到该次结果，只是不再写回缓存。
func WithValueCacheDropInFlightOnInvalidate[T any]() ValueCacheOption[T] {
	return func(cache *ValueCache[T]) { cache.clearOnBad = true }
}

// NewValueCache 建一个空缓存。
func NewValueCache[T any](ttl time.Duration, options ...ValueCacheOption[T]) *ValueCache[T] {
	cache := &ValueCache[T]{hasExpiry: ttl > 0, now: time.Now}
	if cache.hasExpiry {
		cache.expiresAt = time.Time{}
	}
	for _, option := range options {
		option(cache)
	}
	cache.ttl = ttl
	return cache
}

// Get 返回未过期的缓存值；未命中时调用 load 加载。
//
// fallback 可为 nil：此时加载失败且无旧值时返回错误。fallback 存在时（对应
// system-settings-cache 的保守默认值），加载失败被降级为返回 fallback 且不报错。
func (c *ValueCache[T]) Get(
	ctx context.Context,
	load func(context.Context) (T, error),
	fallback func() T,
) (T, error) {
	c.mu.Lock()
	if c.value != nil && c.freshLocked() {
		value := *c.value
		c.mu.Unlock()
		return value, nil
	}
	if c.inFlight != nil {
		flight := c.inFlight
		c.mu.Unlock()
		return c.await(ctx, flight)
	}

	flight := &valueFlight[T]{done: make(chan struct{})}
	c.inFlight = flight
	expectedVersion := c.version
	c.mu.Unlock()

	// 加载在锁外进行：加载可能很慢（DB/Redis），持锁会把整个进程的读路径串行化。
	loaded, loadErr := load(ctx)

	c.mu.Lock()
	// 降级判定在发起者一侧完成，并写进 flight：等待者必须与发起者拿到同一个结果。
	// 否则等待者会收到错误，而发起者降级走了 fallback（Node 侧两者共享同一个不会
	// reject 的 promise，语义是「都拿到降级值」）。
	result := loaded
	resultErr := loadErr
	if loadErr != nil {
		if c.value != nil {
			result = *c.value
			resultErr = nil
		} else if fallback != nil {
			result = fallback()
			resultErr = nil
		}
	}
	flight.value = result
	flight.err = resultErr
	if c.inFlight == flight {
		c.inFlight = nil
	}
	if loadErr == nil && c.version == expectedVersion {
		copied := loaded
		c.value = &copied
		if c.hasExpiry {
			c.expiresAt = c.now().Add(c.ttl)
		}
	}
	c.mu.Unlock()
	close(flight.done)

	if resultErr != nil {
		var zero T
		return zero, resultErr
	}
	return result, nil
}

// Peek 只读当前缓存值，不触发加载；第二返回值为是否存在未过期值。
func (c *ValueCache[T]) Peek() (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value == nil || !c.freshLocked() {
		var zero T
		return zero, false
	}
	return *c.value, true
}

// Invalidate 清空本地值并递增版本号，使在途加载的结果无法写回缓存。
func (c *ValueCache[T]) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = nil
	c.expiresAt = time.Time{}
	c.version++
	if c.clearOnBad {
		c.inFlight = nil
	}
}

// Version 返回当前版本号（供测试与观测使用）。
func (c *ValueCache[T]) Version() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// InFlight 报告当前是否有加载在途。
func (c *ValueCache[T]) InFlight() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight != nil
}

func (c *ValueCache[T]) freshLocked() bool {
	if !c.hasExpiry {
		return true
	}
	if c.expiresAt.IsZero() {
		return false
	}
	return c.now().Before(c.expiresAt)
}

func (c *ValueCache[T]) await(ctx context.Context, flight *valueFlight[T]) (T, error) {
	select {
	case <-flight.done:
		if flight.err != nil {
			// 等待者拿到与发起者一致的结果语义：错误由调用方决定是否降级。
			// 这里不再查缓存，因为发起者已在返回前完成缓存更新决策。
			var zero T
			return zero, flight.err
		}
		return flight.value, nil
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// KeyedCache 是「按键分区 + TTL + 版本守卫 + 逐键 in-flight 合并」的缓存，
// 对应 src/lib/cache/provider-endpoint-cache.ts（按 vendorId:providerType 分键）。
type KeyedCache[V any] struct {
	mu      sync.Mutex
	entries *TTLMap[string, []V]
	version uint64
	loading map[string]*keyedFlight[V]
}

type keyedFlight[V any] struct {
	done  chan struct{}
	value []V
	err   error
}

// NewKeyedCache 建一个按键缓存的表。
func NewKeyedCache[V any](ttl time.Duration, maxSize int) *KeyedCache[V] {
	return &KeyedCache[V]{
		entries: NewTTLMap[string, []V](ttl, maxSize),
		loading: make(map[string]*keyedFlight[V]),
	}
}

// Get 读缓存；未命中则调用 fetch，同键并发未命中共享一次查询。
func (c *KeyedCache[V]) Get(
	ctx context.Context,
	key string,
	fetch func(context.Context) ([]V, error),
) ([]V, error) {
	if cached, ok := c.entries.Get(key); ok {
		return cached, nil
	}

	c.mu.Lock()
	if flight, ok := c.loading[key]; ok {
		c.mu.Unlock()
		select {
		case <-flight.done:
			if flight.err != nil {
				return nil, flight.err
			}
			return flight.value, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &keyedFlight[V]{done: make(chan struct{})}
	c.loading[key] = flight
	expectedVersion := c.version
	c.mu.Unlock()

	value, err := fetch(ctx)

	c.mu.Lock()
	if c.loading[key] == flight {
		delete(c.loading, key)
	}
	if err == nil && c.version == expectedVersion {
		c.entries.Set(key, value)
	}
	c.mu.Unlock()

	flight.value = value
	flight.err = err
	close(flight.done)

	if err != nil {
		return nil, err
	}
	return value, nil
}

// Invalidate 清空全部键并递增版本号；同时丢弃在途查询。
//
// 丢弃在途查询与 provider-endpoint-cache.ts 一致：在途结果因版本守卫已不可能写回，
// 但它尚未返回，清空后新请求会另起一次查询，直接拿到失效后的数据。
func (c *KeyedCache[V]) Invalidate() {
	c.mu.Lock()
	c.version++
	c.loading = make(map[string]*keyedFlight[V])
	c.mu.Unlock()
	c.entries.Clear()
}

// Version 返回当前版本号。
func (c *KeyedCache[V]) Version() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// Size 返回缓存条目数。
func (c *KeyedCache[V]) Size() int {
	return c.entries.Size()
}
