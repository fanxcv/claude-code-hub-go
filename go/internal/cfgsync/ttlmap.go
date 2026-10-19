package cfgsync

import (
	"sort"
	"sync"
	"time"
)

// TTLMap 是带 TTL 与容量淘汰的进程内键值表，逐条对应
// src/lib/cache/ttl-map.ts：容量满时先清过期项，仍满则淘汰最接近到期的一批（约 10%）。
//
// 并发纪律：读路径持读锁，写入与清理持写锁。命中是热路径（每个请求都要查几次管理配置），
// 读锁让并发请求互不排队；唯一需要写锁的读是「读到过期项并顺手删除」，它本身就是写。
type TTLMap[K comparable, V any] struct {
	mu      sync.RWMutex
	ttl     time.Duration
	maxSize int
	// order 是插入序：每次 set 都先删后插，故尾部即最近写入。
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
	m.mu.RLock()
	expiresAt, ok := m.order[key]
	if ok && m.now().Before(expiresAt) {
		// 命中只读：不重排插入序。淘汰按 expiresAt 升序（见 evictLocked），而本表所有条目 TTL 相同、
		// 到期时间只由 set 时刻决定，故重排迭代序对淘汰结果没有任何影响——那四次 map 操作是空转。
		value := m.items[key]
		m.mu.RUnlock()
		return value, true
	}
	m.mu.RUnlock()

	var zero V
	if !ok {
		return zero, false
	}

	// 过期：删除是写操作，须升为写锁。重查一遍再删，避免删掉期间被 Set 刷新过的条目。
	m.mu.Lock()
	defer m.mu.Unlock()
	if latest, stillThere := m.order[key]; stillThere && !m.now().Before(latest) {
		delete(m.order, key)
		delete(m.items, key)
	}
	return zero, false
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
//
// 原地删除而不是重建两张 map：配置失效是常态（每次失效广播都会走到这里），重建会
// 每次给 GC 交一批新堆块（4096 条的容量提示对应近百 KB），而 Go 的 delete 不缩容，
// 复用底层数组正是这里要的。
func (m *TTLMap[K, V]) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.order {
		delete(m.order, key)
		delete(m.items, key)
	}
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.items)
}

// evictLocked 先清过期项，仍满则按到期时间升序淘汰最近到期的一批（至少一条）。
//
// 淘汰顺序用 expiresAt 升序表达：本表所有条目的 TTL 相同，而 TTL 从 set 时刻算、读不刷新，
// 故 expiresAt 的顺序就是 TS 里 Map 迭代序的顺序。不用额外维护序列号，也不用依赖 Go map
// 的随机迭代序。
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
