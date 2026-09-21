package dataplane

import (
	"context"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
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

// providerSlowRateSource 从 store 直读单个供应商的低速监控配置。
//
// 为何每次直读而不复用 guard 的 providers 快照：那是 guard 包内的私有实现，本包拿不到；
// 而本实现只在**写入侧**（终态旁路）调用，且已被「该渠道是否开启监控」之外没有别的闸门
// 可省——即每条终态至多一次按主键的窄查询，与 idleTimeoutCache 同量级。
//
// ponytail: 每次终态一次按主键只读查询，未加缓存；若慢样本写入量级导致可测的 DB 压力，
// 再套 cfgsync.KeyedCache（照 idleTimeoutCache 的手法），此处不预先加。
type providerSlowRateSource struct {
	pools  *store.Pools
	logger *logx.Logger
}

// SlowRateProvider 实现 slowrate.ProviderSource。
//
// 读不到行（已删除/禁用）或查询失败一律返回 false：调用方按未开启处理（fail-open）。
func (s *providerSlowRateSource) SlowRateProvider(ctx context.Context, providerID int64) (slowrate.ProviderConfig, bool) {
	if s == nil || s.pools == nil || providerID <= 0 {
		return slowrate.ProviderConfig{}, false
	}
	row, err := s.pools.FindProviderByID(ctx, providerID)
	if err != nil || row == nil {
		if err != nil && s.logger != nil {
			s.logger.Warn("dataplane.slow_rate_config_lookup_failed", map[string]any{
				"providerId": providerID,
				"error":      err.Error(),
			})
		}
		return slowrate.ProviderConfig{}, false
	}
	return slowrate.ProviderConfig{
		Enabled:       row.SlowRateMonitorEnabled,
		WindowSeconds: row.SlowRateWindowSeconds,
		MinSamples:    row.SlowRateMinSamples,
		RatioPerMille: row.SlowRateRatioPerMille,
		PenaltyStep:   row.SlowRatePenaltyStep,
		PenaltyMax:    row.SlowRatePenaltyMax,
	}, true
}

// slowRateRecorder 装配低速样本旁路；Redis 或配置源缺失时返回 nil（旁路整段跳过）。
func slowRateRecorder(options StoreOptions, logger *logx.Logger) terminal.SlowRateRecorder {
	if options.Redis == nil {
		logger.Info("dataplane.slow_rate_recorder_skipped", map[string]any{
			"reason": "redis_unconfigured",
			"effect": "slow_rate_samples_not_recorded",
		})
		return nil
	}
	config := slowrate.NewSnapshotConfig(&providerSlowRateSource{pools: options.Pools, logger: logger})
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
	return &slowRateAdapter{recorder: recorder}
}

// slowRateAdapter 把 terminal 的中性样本视图译成 slowrate 的事实结构。
//
// 为何由本包做这层适配：terminal 不得 import slowrate（会与 guard/session 成环），
// 而 slowrate 也不必知道 terminal 的类型。本包在依赖链顶端，两侧都看得到，是唯一合适的译点。
type slowRateAdapter struct {
	recorder *slowrate.Recorder
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
