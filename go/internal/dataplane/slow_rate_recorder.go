package dataplane

import (
	"context"
	"errors"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/slowrate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件是低速监控配置面的生产实现：把 providers 表的六个 slow_rate_* 列投影成
// slowrate.ProviderConfig。
//
// 为何放在 dataplane 而不是 guard：guard 的 cachedSource（providers 快照）在 guard 包内，
// 而 slowrate 需要经 session 包取冷却键构造器，session 又依赖 guard——把 ProviderSource
// 实现在 guard 里会让 guard 反向依赖 slowrate、形成环。本包已在依赖链顶端，是天然的落点。

// providerSlowRateSource 从 store 读单个供应商的低速监控配置，带进程内缓存。
//
// 为何必须有缓存：Recorder.Record 在**每条终态**上问一次本方法，而本方法原先每次直查
// `SELECT * FROM providers WHERE id=$1`。生产上绝大多数渠道没开低速监控，于是「未开启的渠道
// 逐请求零额外开销」这条设计约束不成立——关闭的渠道每次终态仍付一次 DB 往返，且该延迟由
// terminal 的异步写 worker 承担。缓存后热路径只剩一次 map 查（见 slowrate/config.go 的口径）。
//
// 为何不复用 guard 的 providers 快照：那是 guard 包内的私有实现，本包拿不到；而 slowrate 经
// session 包取冷却键构造器、session 又依赖 guard，把 ProviderSource 实现在 guard 里会让 guard
// 反向依赖 slowrate、形成环。本包已在依赖链顶端，是天然的落点。
//
// TTL 与容量照 idleTimeoutCache 的手法：TTL 取 providers 域的失效周期，容量等于启用态供应商数。
type providerSlowRateSource struct {
	providers providerRowReader
	logger    *logx.Logger
	cache     *cfgsync.TTLMap[int64, slowRateConfigEntry]
}

// providerRowReader 是 providerSlowRateSource 唯一的存储读面（*store.Pools 天然满足）。
//
// 为何要接口：本类型存在的全部理由就是「未开启监控的渠道每请求不多查一次库」，而这条只能靠
// 「查了几次」来钉；store.Pools 的字段私有，测试里没法替身计数。
type providerRowReader interface {
	FindProviderByID(ctx context.Context, id int64) (*store.Provider, error)
}

// asProviderRowReader 把可能为 nil 的 *store.Pools 折成接口。
//
// 为何要这一步：nil 的 *store.Pools 装进接口会得到**非 nil 接口**，SlowRateProvider 的 nil
// 闸门随之失效，调用会打到 nil 接收者上。
func asProviderRowReader(pools *store.Pools) providerRowReader {
	if pools == nil {
		return nil
	}
	return pools
}

// slowRateConfigEntry 是缓存的一条值：一次查库同时记住该行是否存在。
//
// 为何连「行不存在」也缓存：只缓存正结果等于没修——已删除的 providerID 每次终态仍会回查。
// 查库**报错**不进缓存（瞬时故障不该被钉住一个 TTL），故本结构不表示错误态。
//
// 「行不存在」在 store 侧**不是无错返回**：readSingleRowAs 命中 sql.ErrNoRows 时返回
// ErrNotFound（store/read.go 的 isNoRows 分支），FindProviderByID 原样上抛。故判定必须走
// errors.Is(err, store.ErrNotFound)——只看 err != nil 会把「稳定的负结果」当成瞬时故障，
// 于是负缓存永不命中，本类型存在的全部理由落空。
type slowRateConfigEntry struct {
	config slowrate.ProviderConfig
	exists bool
}

// slowRateConfigCacheSize 是低速配置缓存的容量（条目数等于启用态供应商数，同 idleTimeoutCacheSize）。
const slowRateConfigCacheSize = 4096

// newProviderSlowRateSource 建带缓存的配置源，并把缓存挂到 providers 域的失效广播上。
//
// 为何必须挂失效：改了某渠道的 slow_rate_* 列后管理面会广播 DomainProviders；不挂的话新参数
// 要等一个 TTL（ProviderCacheTTL = 30s）才生效，运维改了却看不到效果会被当成「没生效」。
// registry 为 nil（无订阅通道的部署与单测）时退化为只靠 TTL 自愈，与 idleTimeoutCache 同。
//
// 返回的 dispose 不保留：缓存与 Registry 同寿命（均为进程级），没有比进程退出更早的解绑时机。
func newProviderSlowRateSource(providers providerRowReader, registry *cfgsync.Registry, logger *logx.Logger) *providerSlowRateSource {
	source := &providerSlowRateSource{
		providers: providers,
		logger:    logger,
		cache: cfgsync.NewTTLMap[int64, slowRateConfigEntry](
			cfgsync.Spec(cfgsync.DomainProviders).TTL, slowRateConfigCacheSize),
	}
	if registry != nil {
		if _, err := registry.Bind(cfgsync.DomainProviders, source.cache.Clear); err != nil {
			logger.Error("dataplane.slow_rate_cache_bind_failed", map[string]any{
				"domain": string(cfgsync.DomainProviders),
				"error":  err.Error(),
			})
		}
	}
	return source
}

// SlowRateProvider 实现 slowrate.ProviderSource。
//
// 读不到行（已删除/禁用）或查询失败一律返回 false：调用方按未开启处理（fail-open）。
// 但两者的**缓存语义相反**：行不存在是稳定的负结果（进缓存、不打日志），查询失败是瞬时
// 故障（不进缓存、打 warn）——混为一谈会让不存在的 providerID 每条终态都回查一次库。
func (s *providerSlowRateSource) SlowRateProvider(ctx context.Context, providerID int64) (slowrate.ProviderConfig, bool) {
	if s == nil || s.providers == nil || providerID <= 0 {
		return slowrate.ProviderConfig{}, false
	}
	if entry, ok := s.cache.Get(providerID); ok {
		return entry.config, entry.exists
	}
	row, err := s.providers.FindProviderByID(ctx, providerID)
	if err != nil { // ErrNotFound 是**稳定的负结果**（行确实不存在），不是故障：与「查到了但没开监控」
		// 同档，必须进缓存。漏了这一档的后果是双重的——不存在的 providerID 每条终态都回查
		// 一次库，且每次刷一条 slow_rate_config_lookup_failed，把真故障淹在噪音里。
		if errors.Is(err, store.ErrNotFound) {
			s.cache.Set(providerID, slowRateConfigEntry{})
			return slowrate.ProviderConfig{}, false
		}
		if s.logger != nil {
			s.logger.Warn("dataplane.slow_rate_config_lookup_failed", map[string]any{
				"providerId": providerID,
				"error":      err.Error(),
			})
		}
		return slowrate.ProviderConfig{}, false
	}
	if row == nil {
		// 防御分支：*store.Pools 在无行时走上面的 ErrNotFound，到不了这里。保留是因为
		// providerRowReader 是接口，别的实现可能以 (nil, nil) 表达同一事实。
		s.cache.Set(providerID, slowRateConfigEntry{})
		return slowrate.ProviderConfig{}, false
	}
	entry := slowRateConfigEntry{
		exists: true,
		config: slowrate.ProviderConfig{
			Enabled:       row.SlowRateMonitorEnabled,
			WindowMinutes: row.SlowRateWindowMinutes,
			TriggerCount:  row.SlowRateTriggerCount,
			Ratio:         row.SlowRateRatio,
			PenaltyStep:   row.SlowRatePenaltyStep,
			PenaltyMax:    row.SlowRatePenaltyMax,
			// 恢复阈值列：本包只透传（同 ProbeAfterFirstByteSeconds），判定在 slowrate 写入器。
			RecoveryRequests: row.SlowRateRecoveryRequests,
			// 探测阈值列：本包只透传，接线与判定在 forward/gate 侧。
			ProbeAfterFirstByteSeconds: row.SlowRateProbeAfterFirstByteSeconds,
		},
	}
	s.cache.Set(providerID, entry)
	return entry.config, true
}

// slowRateRecorder 装配低速样本旁路；Redis 或配置源缺失时返回 nil（旁路整段跳过）。
// 返回具体类型而不是 terminal.SlowRateRecorder：本适配器**同时**实现两个旁路接口
// （样本与改道），而它们是两个独立的配置位；声明成其中之一会逼调用方做类型断言。
func slowRateRecorder(options StoreOptions, logger *logx.Logger) *slowRateAdapter {
	if options.Redis == nil {
		logger.Info("dataplane.slow_rate_recorder_skipped", map[string]any{
			"reason": "redis_unconfigured",
			"effect": "slow_rate_samples_not_recorded",
		})
		return nil
	}
	config := slowrate.NewSnapshotConfig(newProviderSlowRateSource(asProviderRowReader(options.Pools), options.Registry, logger))
	if config == nil {
		return nil
	}
	recorder := slowrate.New(slowrate.Options{
		Redis:  options.Redis,
		Config: config,
		Logger: logger,
		Now:    options.Now,
	})
	if recorder == nil {
		return nil
	}
	return &slowRateAdapter{
		recorder: recorder,
		diverts:  slowrate.NewDivertStore(options.Redis, logger),
		clock:    options.Now,
	}
}

// slowPostCommitSink 把二级闸的处置调用折成「写一条与提交前判废同形的事实」。
//
// 复用 RecordSlowPrecommit 而不是新开一个写入面：两者的事实形状与语义完全一致——
// 「这个渠道×模型在窗内又出了一次慢」，共用同一把 samplesKey、同一个幂等成员（请求行 id）。
// 另开一条写入面只会多出一份可能分叉的分档与封顶逻辑。
func slowPostCommitSink(adapter *slowRateAdapter) PostCommitSlowSink {
	if adapter == nil {
		return nil
	}
	return func(ctx context.Context, fact terminal.SlowPrecommit) {
		adapter.RecordSlowPrecommit(ctx, []terminal.SlowPrecommit{fact})
	}
}

// slowRateAdapter 把 terminal 的中性样本视图译成 slowrate 的事实结构。
//
// 为何由本包做这层适配：terminal 不得 import slowrate（会与 guard/session 成环），
// 而 slowrate 也不必知道 terminal 的类型。本包在依赖链顶端，两侧都看得到，是唯一合适的译点。
type slowRateAdapter struct {
	recorder *slowrate.Recorder
	// diverts 是改道计数的读写面；与 recorder 共用同一个 Redis，但构造条件不同
	// （读面不需要 ConfigSource）。
	diverts *slowrate.DivertStore
	// clock 可注入（nil 即 time.Now），取时刻走本包的 nowOr。
	clock func() time.Time
}

// RecordSlowRate 实现 terminal.SlowRateRecorder。
func (a *slowRateAdapter) RecordSlowRate(ctx context.Context, sample terminal.SlowRateSample) {
	if a == nil || a.recorder == nil {
		return
	}
	a.recorder.Record(ctx, slowrate.Facts{
		ProviderID:   sample.ProviderID,
		SessionID:    sample.SessionID,
		KeyID:        sample.KeyID,
		ModelKey:     sample.ModelKey,
		RequestID:    sample.RequestID,
		StatusCode:   sample.StatusCode,
		OutputTokens: sample.OutputTokens,
		DurationMS:   sample.DurationMS,
		FirstByteMS:  sample.FirstByteMS,
	})
}

// RecordSlowPrecommit 实现 terminal.SlowRateRecorder 的「提交前判慢」侧。
//
// 为何不走 RecordSlowRate：样本的每个判据都建立在**能算出的生成速率**上（状态码、输出 token
// 下限、首字节比例），而判废时流还没走完、算不出速率；且样本受终态提交闸门约束，而判废家
// 恰恰**不是**作答家（见 terminal 侧 recordSlowPrecommit 的说明）。
func (a *slowRateAdapter) RecordSlowPrecommit(ctx context.Context, facts []terminal.SlowPrecommit) {
	if a == nil || a.recorder == nil {
		return
	}
	for _, fact := range facts {
		a.recorder.RecordPrecommit(ctx, slowrate.PrecommitFacts{
			ProviderID: fact.ProviderID,
			SessionID:  fact.SessionID,
			KeyID:      fact.KeyID,
			ModelKey:   fact.ModelKey,
			RequestID:  fact.RequestID,
		})
	}
}

// RecordSlowDiverts 实现 terminal.SlowDivertRecorder。
//
// 与样本分开走另一条路（而不是塞进 RecordSlowRate）：改道的键按**被挤掉的那家**，
// 而那家恰恰不是作答的 ProviderID；且 503 路径没有作答者，却最该被计。
func (a *slowRateAdapter) RecordSlowDiverts(ctx context.Context, diverts []terminal.SlowDivert) {
	if a == nil || a.recorder == nil {
		return
	}
	now := nowOr(a.clock)
	for _, divert := range diverts {
		a.diverts.Record(ctx, divert.ProviderID, route.DivertCause(divert.Cause), now)
	}
}
