package pubstatus

import (
	"context"
	"sync"
	"time"
)

// 本文件是投影写侧的**装配面**：把「事件 → 桶」这条链路包成一个对象，供结算路径调用。
//
// Node 对应物是 `rollup-store.ts:248-287` 的 `getConfiguredPublicStatusGroupsForRollupResolution`
// 加 `:471-478` 的 `queuePublicStatusRollupWrite`：
//   - 分组来自**内部配置快照**，并带一层短 TTL 缓存（30 秒；读空/读失败时 5 秒），
//     因为每个请求都读一次 Redis 不划算；
//   - 写入失败只记日志（`queuePublicStatusRollupWrite` 的 catch），调用方拿不到错误。

const (
	// configuredGroupsCacheTTL 复刻 rollup-store.ts:28 的 CONFIGURED_GROUPS_CACHE_TTL_MS。
	configuredGroupsCacheTTL = 30 * time.Second
	// emptyConfiguredGroupsCacheTTL 复刻 rollup-store.ts:29 的 EMPTY_CONFIGURED_GROUPS_CACHE_TTL_MS。
	emptyConfiguredGroupsCacheTTL = 5 * time.Second
)

// DebugLogger 与 InfoLogger 是**可选**的日志级面：pubstatus.Logger 只要求 Warn（装配方与既有
// 替身都只实现它），而快照不可用的上报需要「持续期间」与「恢复」两个更低/更高一级的级别。
//
// 与 terminal.DebugLogger 同一手法：可选断言，缺失即该级不发——绝不为此把 Logger 的必需集
// 加宽（那会让所有既有替身与装配点编译失败）。
type DebugLogger interface {
	Debug(event string, fields map[string]any)
}

// InfoLogger 见 DebugLogger 的说明。
type InfoLogger interface {
	Info(event string, fields map[string]any)
}

// RollupGroupSource 给出「当前已配置的公开分组」。
//
// 名字带 Rollup 前缀：本包另有一个 `GroupSource`（配置发布侧的分组来源，字段形态不同），
// 两者不可混用。
type RollupGroupSource interface {
	// Groups 返回当前分组；第二个返回值表示本次读取是否成功
	// （失败时返回空切片，调用方据此跳过写入并记日志）。
	Groups(ctx context.Context) ([]ConfiguredGroup, bool)
}

// SnapshotGroupSource 从内部配置快照读分组，并带短 TTL 缓存。
type SnapshotGroupSource struct {
	store  PublicStatusStore
	prefix string
	now    func() time.Time
	logger Logger

	// mu 保护下面三个缓存字段。本实例在生产里是单例（dataplane 装配一次），
	// 而终态结算路径可以并发调用它（见 terminal.RollupRecorder 的约定），
	// 不加锁时 -race 会同时报读（loaded/expiresAt）与写（cached/loaded/expiresAt）。
	mu        sync.Mutex
	cached    []ConfiguredGroup
	expiresAt time.Time
	loaded    bool
	// unavailable 记录上一次加载是否落在「快照读不到」分支，供状态转移上报用（受 mu 保护）。
	unavailable bool
}

// NewSnapshotGroupSource 建分组来源；store 为 nil 时永远返回「未就绪」。
//
// prefix 必须与写入侧一致（空串＝当前前缀 v2）；读路径对 legacy v1 复用同一批构造器，
// 写侧只写当前前缀，故这里默认走当前前缀。
func NewSnapshotGroupSource(store PublicStatusStore, prefix string, now func() time.Time, logger Logger) *SnapshotGroupSource {
	if now == nil {
		now = time.Now
	}
	return &SnapshotGroupSource{store: store, prefix: prefix, now: now, logger: logger}
}

// Groups 实现 RollupGroupSource。
//
// 临界区只护缓存字段的读写：加载（会打 Redis）留在锁外，否则并发结算会被整个串行化。
// 代价是 TTL 到期后可能有一次以上的并发加载，最后一次完成的结果胜出——加锁前后一致。
func (s *SnapshotGroupSource) Groups(ctx context.Context) ([]ConfiguredGroup, bool) {
	if s == nil || s.store == nil {
		return nil, false
	}
	s.mu.Lock()
	if s.loaded && s.now().Before(s.expiresAt) {
		cached := s.cached
		s.mu.Unlock()
		return cached, true
	}
	s.mu.Unlock()

	snapshot := ReadInternalConfigSnapshot(ctx, s.store, s.prefix)
	if snapshot == nil {
		// 读不到（未发布/Redis 抖动）：短 TTL 后再试，避免每请求都打 Redis，又不会长期卡住。
		//
		// 上报按**状态转移**而非按次：读不到是稳定态与真抖动共用的返回值，按次 warn 在稳定态下
		// 会以 emptyConfiguredGroupsCacheTTL 的节奏刷屏（部署实测 11 条/分钟），把真信号淹没。
		s.mu.Lock()
		s.cached = nil
		s.loaded = true
		s.expiresAt = s.now().Add(emptyConfiguredGroupsCacheTTL)
		entered := !s.unavailable
		s.unavailable = true
		s.mu.Unlock()
		s.reportUnavailable(entered)
		return nil, false
	}

	groups := ConfiguredGroupsFromSnapshot(snapshot)
	// 站点没配公开状态：正常情形，用短 TTL 以便配置生效后尽快跟上。
	ttl := configuredGroupsCacheTTL
	if len(groups) == 0 {
		ttl = emptyConfiguredGroupsCacheTTL
	}
	s.mu.Lock()
	s.cached = groups
	s.loaded = true
	s.expiresAt = s.now().Add(ttl)
	recovered := s.unavailable
	s.unavailable = false
	s.mu.Unlock()
	if recovered {
		s.reportRecovered()
	}
	return groups, true
}

// reportUnavailable 上报「快照读不到」：首次进入记 warn 一次，持续期间只留 debug 足迹。
//
// 为什么首次必须仍是 warn：读不到有两种成因——实现上无周期性重发（稳定态），以及 Redis 抖动或
// 快照键被清（真故障）。前者不该刷 warn，后者不能静默；合并成「进入时一条 warn + 持续期 debug +
// 恢复时一条 info」的转移序列后，只看 warn 的告警面读到的仍是「发生过几次不可用」这个真信号，
// 而不再是「每 5 秒一次」。
func (s *SnapshotGroupSource) reportUnavailable(entered bool) {
	if s == nil || s.logger == nil {
		return
	}
	if entered {
		s.logger.Warn("public_status_rollup_groups_unavailable", map[string]any{"prefix": s.prefix})
		return
	}
	if debugger, ok := s.logger.(DebugLogger); ok {
		debugger.Debug("public_status_rollup_groups_unavailable", map[string]any{
			"prefix": s.prefix,
			"state":  "still_unavailable",
		})
	}
}

// reportRecovered 上报「从读不到回到读得到」，每个不可用期只报一次。
//
// 缺 Info 面的 logger 直接跳过（与 Debug 同一手法）：恢复是好消息，不该为了让它可见而走 warn。
func (s *SnapshotGroupSource) reportRecovered() {
	if s == nil || s.logger == nil {
		return
	}
	if infoLogger, ok := s.logger.(InfoLogger); ok {
		infoLogger.Info("public_status_rollup_groups_recovered", map[string]any{"prefix": s.prefix})
	}
}

// RollupRecorder 把终态事件折算成桶增量并写入。
//
// 它满足 `terminal.RollupRecorder`（同一方法签名），由装配方注入结算器。
type RollupRecorder struct {
	writer  RollupWriter
	groups  RollupGroupSource
	prefix  string
	logger  Logger
	metrics *RecorderMetrics
	// metricsMu 保护 metrics 的读写。接收器是单例、被并发结算调用，而计数面是普通结构体，
	// 所有读写都必须经由本接收器（count / Metrics）走这把锁。
	metricsMu sync.Mutex
}

// RecorderMetrics 是该旁路的可观测计数（零值可用）。
//
// 字段是裸计数：并发场景下经 `RollupRecorder.Metrics()` 读取（持锁取快照），
// 不要在结算进行中直接读字段。
//
// 为什么要计数器：旁路的失败**不会**冒泡（见 terminal/rollup.go 的约束 1），若不落计数，
// 「桶一直没长」在生产上无从发现——只会表现为公开页停在旧代。
type RecorderMetrics struct {
	Written   int64
	Skipped   int64
	Failed    int64
	LastError string
}

// NewRollupRecorder 建接收器；writer 为 nil 时 RecordTerminal 静默跳过
// （装配方据此表达「未配置 Redis」）。metrics 可为 nil。
func NewRollupRecorder(writer RollupWriter, groups RollupGroupSource, prefix string, logger Logger, metrics *RecorderMetrics) *RollupRecorder {
	return &RollupRecorder{writer: writer, groups: groups, prefix: prefix, logger: logger, metrics: metrics}
}

// RecordTerminal 实现终态事件的接收面（**永不返回错误**）。
func (r *RollupRecorder) RecordTerminal(ctx context.Context, event RollupEvent) {
	if r == nil || r.writer == nil {
		return
	}
	groups, ok := r.groups.Groups(ctx)
	if !ok {
		// 分组读不到：按 Node 的 `retryable` 分支语义——本次不写，等下一轮（缓存 5 秒后重试）。
		r.count(func(m *RecorderMetrics) { m.Skipped++ })
		return
	}
	if len(groups) == 0 {
		// 站点未配置公开状态：任何请求都不会有公开统计，静默跳过（不记 warn，否则每请求一条）。
		r.count(func(m *RecorderMetrics) { m.Skipped++ })
		return
	}

	result, err := WriteRollupEvent(ctx, r.writer, event, groups, r.prefix)
	if err != nil {
		r.fail(err)
		return
	}
	if !result.Written {
		// `ignored` 是正常路径：模型不属于任何公开分组、链为空、或本就无成功/失败结局。
		r.count(func(m *RecorderMetrics) { m.Skipped++ })
		return
	}
	r.count(func(m *RecorderMetrics) { m.Written++ })
}

func (r *RollupRecorder) fail(err error) {
	r.count(func(m *RecorderMetrics) {
		m.Failed++
		m.LastError = err.Error()
	})
	if r.logger != nil {
		r.logger.Warn("public_status_rollup_write_failed", map[string]any{"error": err.Error()})
	}
}

func (r *RollupRecorder) count(apply func(*RecorderMetrics)) {
	if r.metrics == nil {
		return
	}
	r.metricsMu.Lock()
	apply(r.metrics)
	r.metricsMu.Unlock()
}

// Metrics 返回计数快照（无计数面时返回零值）。
//
// 与 count() 共用 metricsMu：计数面是普通结构体，读写必须走同一把锁，
// 否则并发结算下 -race 会报竞争（写 Written/Skipped/Failed 与读交错）。
func (r *RollupRecorder) Metrics() RecorderMetrics {
	if r == nil || r.metrics == nil {
		return RecorderMetrics{}
	}
	r.metricsMu.Lock()
	defer r.metricsMu.Unlock()
	return RecorderMetrics{
		Written:   r.metrics.Written,
		Skipped:   r.metrics.Skipped,
		Failed:    r.metrics.Failed,
		LastError: r.metrics.LastError,
	}
}
