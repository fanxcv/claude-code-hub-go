// Package cfgsync 维护「设置与规则」的进程内快照，并在 Redis 失效广播到达或订阅
// 恢复时强制从权威存储重新对齐。
//
// 对应 Node 侧三处实现：
//   - src/lib/config/system-settings-cache.ts（60s TTL + 版本守卫 + in-flight 合并 + 保守默认值）
//   - src/lib/cache/provider-cache.ts 与 provider-endpoint-cache.ts（30s TTL + 逐键/整表失效）
//   - src/lib/redis/pubsub.ts（8 条失效通道 + 订阅恢复后的强制 resync）
//
// 为什么不能只靠 TTL：Node 与 Go 会在过渡期同时写库，Redis 广播是唯一的即时失效通道；
// 丢一条广播就意味着某个进程最多用陈旧配置一整个 TTL。resync 是断线窗口的唯一补偿手段。
package cfgsync

import (
	"errors"
	"sync"
	"time"
)

// Domain 是参与缓存与失效的配置域。
type Domain string

const (
	DomainSystemSettings    Domain = "system_settings"
	DomainProviders         Domain = "providers"
	DomainProviderEndpoints Domain = "provider_endpoints"
	DomainProviderGroups    Domain = "provider_groups"
	DomainAPIKeys           Domain = "api_keys"
	DomainRequestFilters    Domain = "request_filters"
	DomainSensitiveWords    Domain = "sensitive_words"
	DomainErrorRules        Domain = "error_rules"
)

// DomainSpec 描述一个域的失效通道与时间上界。
type DomainSpec struct {
	// Channel 是该域失效时广播的通道。
	Channel string
	// TTL 是时间自愈上界；为 0 表示只靠失效消息重载（事件驱动域）。
	TTL time.Duration
	// EventDriven 为 true 时表示该域没有 TTL，只能在失效消息（含 resync）到达时重载。
	EventDriven bool
}

// SystemSettingsTTL 与 providerCacheTTL 逐字取自 Node 侧常量。
const (
	SystemSettingsTTL = 60 * time.Second
	ProviderCacheTTL  = 30 * time.Second
)

// Spec 返回一个域的失效通道与 TTL。
//
// provider_endpoints 刻意复用 providers 通道：端点的增删改在 Node 侧随 provider 一起
// 广播，不引入新通道。端点行携带 last_probe_ok / last_probe_latency_ms，而探活结果由
// 后台探针持续更新且**不广播失效**——否则每次探活都会打掉缓存，等于没有缓存。代价是
// 探活排序最多陈旧一个 TTL；端点级熔断走 Redis，不受这份陈旧影响。
func Spec(domain Domain) DomainSpec {
	switch domain {
	case DomainSystemSettings:
		return DomainSpec{Channel: ChannelSystemSettingsUpdated, TTL: SystemSettingsTTL}
	case DomainProviders:
		return DomainSpec{Channel: ChannelProvidersUpdated, TTL: ProviderCacheTTL}
	case DomainProviderEndpoints:
		return DomainSpec{Channel: ChannelProvidersUpdated, TTL: ProviderCacheTTL}
	case DomainProviderGroups:
		return DomainSpec{Channel: ChannelProviderGroupsUpdated, TTL: SystemSettingsTTL}
	case DomainAPIKeys:
		return DomainSpec{Channel: ChannelAPIKeysUpdated, EventDriven: true}
	case DomainRequestFilters:
		return DomainSpec{Channel: ChannelRequestFiltersUpdated, EventDriven: true}
	case DomainSensitiveWords:
		return DomainSpec{Channel: ChannelSensitiveWordsUpdated, EventDriven: true}
	case DomainErrorRules:
		return DomainSpec{Channel: ChannelErrorRulesUpdated, EventDriven: true}
	default:
		return DomainSpec{}
	}
}

// AllDomains 列出全部参与失效的域，供订阅与自检使用。
func AllDomains() []Domain {
	return []Domain{
		DomainSystemSettings,
		DomainProviders,
		DomainProviderEndpoints,
		DomainProviderGroups,
		DomainAPIKeys,
		DomainRequestFilters,
		DomainSensitiveWords,
		DomainErrorRules,
	}
}

// Snapshot 是整体装载状态的只读观测面，供 /readyz 如实报告。
//
// M1 骨架与 /readyz 依赖这一组方法名，保持不变。
type Snapshot struct {
	mu       sync.RWMutex
	loaded   bool
	loadedAt time.Time
	version  string
}

// New 建一个未装载的快照。
func New() *Snapshot {
	return &Snapshot{}
}

// MarkLoaded 记录一次成功装载。
func (s *Snapshot) MarkLoaded(version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = true
	s.loadedAt = time.Now().UTC()
	s.version = version
}

// Loaded 报告快照是否已装载。
func (s *Snapshot) Loaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loaded
}

// LoadedAt 返回最近一次装载时间；未装载时为零值。
func (s *Snapshot) LoadedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadedAt
}

// Version 返回最近一次装载的版本标记；未装载时为空串。
func (s *Snapshot) Version() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// Registry 把配置域的缓存绑到失效总线上，并记录每个域的装载与失效状态。
//
// 用法：域自己持有 *ValueCache / *KeyedCache，通过 Bind 注册它的 Invalidate；
// Registry 只负责「谁在什么时候被失效过」的记账与订阅生命周期。
type Registry struct {
	mu      sync.Mutex
	bus     *Bus
	domains map[Domain]*domainState
	closed  bool
}

type domainState struct {
	loadedAt      time.Time
	loaded        bool
	invalidations uint64
	disposers     []func()
}

// NewRegistry 建一个域注册表。bus 为 nil 时只记账、不订阅（单测与无 Redis 部署）。
func NewRegistry(bus *Bus) *Registry {
	return &Registry{bus: bus, domains: make(map[Domain]*domainState)}
}

// Bind 把一个域的失效回调绑到该域的通道，返回解绑函数。
//
// 收到的消息既可能是真实失效，也可能是订阅恢复后的 ResyncMessage：两者都必须触发
// 重载，不得区分对待（区分会让断线窗口失去补偿）。
func (r *Registry) Bind(domain Domain, invalidate func()) (func(), error) {
	if invalidate == nil {
		return nil, errors.New("cfgsync: invalidate 不能为空")
	}
	spec := Spec(domain)
	if spec.Channel == "" {
		return nil, errors.New("cfgsync: 未知的配置域 " + string(domain))
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("cfgsync: 注册表已关闭")
	}
	state := r.domainStateLocked(domain)
	r.mu.Unlock()

	callback := func(_ string) {
		r.mu.Lock()
		r.domainStateLocked(domain).invalidations++
		r.mu.Unlock()
		invalidate()
	}

	if r.bus == nil {
		dispose := func() {}
		r.mu.Lock()
		state.disposers = append(state.disposers, dispose)
		r.mu.Unlock()
		return dispose, nil
	}

	return r.bus.Subscribe(spec.Channel, callback)
}

// MarkLoaded 记录该域完成了一次成功装载。
func (r *Registry) MarkLoaded(domain Domain) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.domainStateLocked(domain)
	state.loaded = true
	state.loadedAt = time.Now().UTC()
}

// Loaded 报告该域是否装载过。
func (r *Registry) Loaded(domain Domain) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.domainStateLocked(domain).loaded
}

// LoadedAt 返回该域最近一次装载时间。
func (r *Registry) LoadedAt(domain Domain) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.domainStateLocked(domain).loadedAt
}

// Invalidations 返回该域累计收到的失效次数（含 resync）。
func (r *Registry) Invalidations(domain Domain) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.domainStateLocked(domain).invalidations
}

// LoadedDomains 返回已装载的域数量。
func (r *Registry) LoadedDomains() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, state := range r.domains {
		if state.loaded {
			count++
		}
	}
	return count
}

// Ready 报告全部事件驱动域是否都已装载。
//
// 事件驱动域没有 TTL，一旦没装载就没有任何自愈路径，所以它们决定就绪与否；
// TTL 域可以在运行期按需加载，不阻塞就绪。
func (r *Registry) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, domain := range AllDomains() {
		if !Spec(domain).EventDriven {
			continue
		}
		if state, ok := r.domains[domain]; !ok || !state.loaded {
			return false
		}
	}
	return true
}

// Close 解绑全部注册。
func (r *Registry) Close() {
	r.mu.Lock()
	states := make([]*domainState, 0, len(r.domains))
	for _, state := range r.domains {
		states = append(states, state)
	}
	r.closed = true
	r.mu.Unlock()

	for _, state := range states {
		for _, dispose := range state.disposers {
			dispose()
		}
	}
}

func (r *Registry) domainStateLocked(domain Domain) *domainState {
	state, ok := r.domains[domain]
	if !ok {
		state = &domainState{}
		r.domains[domain] = state
	}
	return state
}
