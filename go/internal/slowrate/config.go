package slowrate

import "context"

// 本文件是 ConfigSource 的生产实现：把 providers 表的 slow_rate_* 列折算成 Params。
//
// 零开销保证（设计稿 §8 的设计约束，不是优化）：ProviderSnapshot 返回的开关状态
// 走**内存快照**，每请求只做一次 map 查；未开启的渠道在 Recorder.Record 的首段即返回，
// 一次 Redis 读写都不会发生。

// ProviderConfig 是一个渠道的低速监控配置（从 providers 表的七个 slow_rate_* 列投影）。
type ProviderConfig struct {
	Enabled bool
	// 六个参数用指针：列可空，NULL 表示取出厂默认（不是取 0）。
	WindowSeconds *int
	// TriggerCount 是触发阈值（列 slow_rate_trigger_count）：窗内低速数达此值才标记，
	// 且是 penalty 档位分母。**不是**基线样本下限——后者是 slow_rate_min_samples，
	// 只由 B3 基线任务读取，本包不读它。
	TriggerCount  *int
	RatioPerMille *int
	PenaltyStep   *int
	PenaltyMax    *int
}

// ProviderSource 给出某渠道的低速监控配置。
//
// 第二个返回值为 false 表示该渠道不在快照里（已删除/禁用）——此时按未开启处理。
type ProviderSource interface {
	SlowRateProvider(ctx context.Context, providerID int64) (ProviderConfig, bool)
}

// SnapshotConfig 是 ProviderSource 的 ConfigSource 实现。
type SnapshotConfig struct {
	source ProviderSource
}

// NewSnapshotConfig 用 providers 快照读取面构造 ConfigSource；source 为 nil 时返回 nil
// （调用方据此不装配旁路，行为与接线前一致）。
func NewSnapshotConfig(source ProviderSource) *SnapshotConfig {
	if source == nil {
		return nil
	}
	return &SnapshotConfig{source: source}
}

// SlowRateConfig 实现 ConfigSource：未开启的渠道返回 false，调用方据此零开销跳过。
func (c *SnapshotConfig) SlowRateConfig(ctx context.Context, providerID int64) (Params, bool) {
	if c == nil || c.source == nil {
		return Params{}, false
	}
	config, ok := c.source.SlowRateProvider(ctx, providerID)
	if !ok || !config.Enabled {
		return Params{}, false
	}
	return Params{
		WindowSeconds: deref(config.WindowSeconds),
		TriggerCount:  deref(config.TriggerCount),
		RatioPerMille: deref(config.RatioPerMille),
		PenaltyStep:   deref(config.PenaltyStep),
		PenaltyMax:    deref(config.PenaltyMax),
	}, true
}

// deref 把可空列折成 0，交给 Params.normalize 收敛到出厂默认。
//
// 不在本函数取默认值：默认只应有一处定义（DefaultParams），两处会随维护分叉。
func deref(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}
