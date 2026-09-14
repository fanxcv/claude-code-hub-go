package cfgsync

import (
	"sort"
	"sync"
	"time"
)

// TTLMap 是带 TTL 与 LRU 淘汰的进程内键值表，逐条对应
// src/lib/cache/ttl-map.ts：命中会把条目移到迭代序末尾（LRU bump），
// 容量满时先清过期项，仍满则淘汰最旧的约 10%。
//
// 并发纪律：所有读写都持写锁。该表只服务热路径上「几乎不变的管理配置」，
// 命中率远高于写入频率，故不为读路径引入 RWMutex 的双结构复杂度。
type TTLMap[K comparable, V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxSize int
	// order 是插入序（近似访问序）：每次 set 都先删后插，故尾部即最近使用。
	order map[K]time.Time
	items map[K]V
	now   func() time.Time
}

// NewTTLMap 建一个 TTL 表。ttlMs 必须为正，maxSize 必须为正。
func NewTTLMap[K comparable, V any](ttl time.Duration, maxSize int) *TTLMap[K, V] {
	if ttl <= 0 {
		panic("cfgsync: TTLMap ttl 必须为正")
	}
	if maxSize <= 0 {
		panic("cfgsync: TTLMap maxSize 必须为正")
	}
	return &TTLMap[K, V]{
		ttl:     ttl,
		maxSize: maxSize,
		order:   make(map[K]time.Time, maxSize),
		items:   make(map[K]V, maxSize),
		now:     time.Now,
	}
}

// Get 读一条未过期记录；过期即删除并返回 false。
func (m *TTLMap[K, V]) Get(key K) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	expiresAt, ok := m.order[key]
	if !ok {
		var zero V
		return zero, false
	}
	if !m.now().Before(expiresAt) {
		delete(m.order, key)
		delete(m.items, key)
		var zero V
		return zero, false
	}

	// LRU bump：删除后重新插入，使其落到迭代序末尾。
	value := m.items[key]
	delete(m.order, key)
	delete(m.items, key)
	m.order[key] = expiresAt
	m.items[key] = value
	return value, true
}

// Set 写入一条记录，必要时先淘汰。
func (m *TTLMap[K, V]) Set(key K, value V) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.order, key)
	delete(m.items, key)

	if len(m.items) >= m.maxSize {
		m.evictLocked()
	}

	m.order[key] = m.now().Add(m.ttl)
	m.items[key] = value
}

// Delete 删除一条记录；返回它此前是否存在。
func (m *TTLMap[K, V]) Delete(key K) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.order[key]; !ok {
		return false
	}
	delete(m.order, key)
	delete(m.items, key)
	return true
}

// Has 报告键是否存在且未过期。
func (m *TTLMap[K, V]) Has(key K) bool {
	_, ok := m.Get(key)
	return ok
}

// Clear 清空全表。
func (m *TTLMap[K, V]) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.order = make(map[K]time.Time, m.maxSize)
	m.items = make(map[K]V, m.maxSize)
}

// PurgeExpired 清掉全部过期项。
func (m *TTLMap[K, V]) PurgeExpired() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for key, expiresAt := range m.order {
		if !now.Before(expiresAt) {
			delete(m.order, key)
			delete(m.items, key)
		}
	}
}

// Size 返回当前条目数（含未过期与未清理的过期项）。
func (m *TTLMap[K, V]) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

// evictLocked 先清过期项，仍满则按插入序淘汰最旧的约 10%（至少一条）。
//
// 插入序用 existsAt 升序表达：本表所有条目的 TTL 相同，且 Get 的 LRU bump 不刷新
// 到期时间（与 ttl-map.ts 一致，TTL 从 set 时刻算），故 existsAt 的顺序就是 TS 里
// Map 迭代序的顺序。不用额外维护序列号，也不用依赖 Go map 的随机迭代序。
func (m *TTLMap[K, V]) evictLocked() {
	now := m.now()
	for key, expiresAt := range m.order {
		if !now.Before(expiresAt) {
			delete(m.order, key)
			delete(m.items, key)
		}
	}
	if len(m.items) < m.maxSize {
		return
	}

	remainder := (m.maxSize + 9) / 10
	if remainder < 1 {
		remainder = 1
	}
	type entry struct {
		key       K
		expiresAt time.Time
	}
	entries := make([]entry, 0, len(m.order))
	for key, expiresAt := range m.order {
		entries = append(entries, entry{key: key, expiresAt: expiresAt})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].expiresAt.Equal(entries[j].expiresAt) {
			return false
		}
		return entries[i].expiresAt.Before(entries[j].expiresAt)
	})
	for index := 0; index < len(entries) && index < remainder; index++ {
		delete(m.order, entries[index].key)
		delete(m.items, entries[index].key)
	}
}
