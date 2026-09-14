package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 语义出处（Node）：
//   - 调度：`src/instrumentation.ts:272-290`（`setInterval`，间隔 5 分钟硬编码；
//     每 tick 读一次运行态设置，关闭时整轮跳过）；
//   - 聚合：`src/lib/cache-effectiveness/service.ts`（`aggregateCacheEffectiveness`）。
const (
	// CacheEffectivenessDefaultEvery 是 Node 的聚合间隔（`instrumentation.ts:276` 硬编码 5 分钟）。
	CacheEffectivenessDefaultEvery = 5 * time.Minute

	// cacheEffectivenessSafetyLag 是窗口终点前留的余量（`service.ts:28` WINDOW_SAFETY_LAG_MS）。
	//
	// 为什么需要：`message_request.updated_at` 没有自动更新语义，只能按 `created_at` 过滤，
	// 而终态结算可能晚于写入。留 15 分钟避免把未结算的行算进样本；超过 15 分钟才终态的流
	// 仍会漏计——Node 的注释明确接受这一上限（展示级指标）。
	cacheEffectivenessSafetyLag = 15 * time.Minute

	// cacheEffectivenessInitialLookback 是首次运行（表内无历史）时的回看窗口（`service.ts:30`）。
	cacheEffectivenessInitialLookback = time.Hour

	// CacheEffectivenessTaskName 是任务名（与既有任务同风格：小写短横线）。
	CacheEffectivenessTaskName = "cache-effectiveness-aggregate"

	// cacheEffectivenessTaskTimeout 是单轮上限。
	//
	// Node 侧没有超时（`setInterval` 回调无界）。这里必须有界：聚合是一条 `INSERT ... SELECT`
	// 扫窗口内的 message_request，卡死会永久占住调度循环。窗口最宽为「上次聚合并发滞后 + 间隔」，
	// 正常秒级完成；给 5 分钟以覆盖大库上的长尾，超出即整轮取消并计一次失败。
	cacheEffectivenessTaskTimeout = 5 * time.Minute
)

// CacheEffectivenessOptions 是缓存效果窗口聚合任务的构造参数。
type CacheEffectivenessOptions struct {
	// Pools 是共享连接池；必填（nil 时构造返回错误，装配缺陷应显式可见）。
	Pools *store.Pools
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Now 可注入时钟；nil 时用 time.Now。
	Now func() time.Time
	// EnabledByEnv 是 `ENABLE_CACHE_EFFECTIVENESS` 的读数（Node 的 env 兜底默认 true）。
	//
	// 与系统设置的关系（`src/lib/system-settings/proxy-runtime.ts:77-78`）：
	// `systemSettings.cacheEffectivenessEnabled ?? env`——设置里显式为 false 时关闭，
	// 为 null/缺行时回落本字段。
	EnabledByEnv bool
}

// CacheEffectiveness 按窗口聚合缓存模拟指标（仅展示，不参与路由）。
type CacheEffectiveness struct {
	pools        *store.Pools
	logger       *logx.Logger
	now          func() time.Time
	enabledByEnv bool
}

// NewCacheEffectiveness 构造任务；Pools 为 nil 时报错（与同包其它任务同判）。
func NewCacheEffectiveness(options CacheEffectivenessOptions) (*CacheEffectiveness, error) {
	if options.Pools == nil {
		return nil, errors.New("jobs: 缓存效果聚合需要数据库连接池")
	}
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &CacheEffectiveness{
		pools:        options.Pools,
		logger:       logger,
		now:          now,
		enabledByEnv: options.EnabledByEnv,
	}, nil
}

// Task 返回可登记进 Scheduler 的任务定义。
//
// 与 Node 的差别：Node 的 `setInterval` 首次触发要等满一个间隔，这里立刻跑一轮
// （`Scheduler` 的既有语义：注册后按错峰延迟首跑）。首次跑会按 INITIAL_LOOKBACK 回看 1 小时，
// 与 Node 首次跑同口径，只是提前了几分钟——不会改变结果（窗口推进由 MAX(window_end) 决定）。
func (c *CacheEffectiveness) Task(interval time.Duration) Task {
	if interval <= 0 {
		interval = CacheEffectivenessDefaultEvery
	}
	return Task{
		Name:     CacheEffectivenessTaskName,
		Interval: interval,
		Timeout:  cacheEffectivenessTaskTimeout,
		Run: func(ctx context.Context) error {
			_, err := c.RunOnce(ctx)
			return err
		},
	}
}

// CacheEffectivenessResult 是单轮结果（供日志与巡检断言）。
type CacheEffectivenessResult struct {
	// WindowStart/WindowEnd 是本轮聚合的窗口；跳过且未算窗口时为 nil。
	WindowStart *time.Time
	WindowEnd   *time.Time
	// GroupsWritten 是写入的分组数（Node 的 groupsWritten）。
	GroupsWritten int
	// Skipped 表示本轮没写任何行。
	Skipped bool
	// Reason 说明跳过原因：disabled / locked / window_not_advanced。
	Reason string
}

// RunOnce 执行一轮聚合。
//
// 与 Node 的逐条对应（`service.ts:40-162`）：
//  1. 事务级 advisory 锁（同键 `20260722`）：取不到就直接返回（Node 同样不排队）；
//  2. `windowEnd = now - 15min`；`windowStart = MAX(window_end)`，无历史则 `now - 1h`；
//  3. `windowStart >= windowEnd` 时跳过（窗口尚未推进）；
//  4. 单语句聚合并写入，返回写入分组数；
//  5. 写日志只在 groupsWritten > 0 时（与 Node 同判，避免每 5 分钟一条空轮日志）。
//
// 开关在**每轮**读系统设置（Node 每 tick 读一次运行态快照），故运行期切换即时生效。
func (c *CacheEffectiveness) RunOnce(ctx context.Context) (CacheEffectivenessResult, error) {
	setting, err := c.pools.CacheEffectivenessEnabledSetting(ctx)
	if err != nil {
		return CacheEffectivenessResult{}, fmt.Errorf("jobs: 读缓存效果开关失败: %w", err)
	}
	if !cacheEffectivenessEnabled(setting, c.enabledByEnv) {
		c.logger.Info("cache_effectiveness_aggregate_skipped", map[string]any{
			"reason":            "disabled",
			"settingExplicitly": setting != nil,
		})
		return CacheEffectivenessResult{Skipped: true, Reason: "disabled"}, nil
	}

	pool, err := c.pools.Control()
	if err != nil {
		return CacheEffectivenessResult{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return CacheEffectivenessResult{}, fmt.Errorf("jobs: 缓存效果聚合开启事务失败: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// 用 WithoutCancel：ctx 已取消（超时）时回滚仍必须发出，否则连接带着未结束的事务
			// 回池，下一次借到它的调用会看到脏状态。
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	acquired, err := c.pools.TryCacheEffectivenessXactLock(ctx, tx)
	if err != nil {
		return CacheEffectivenessResult{}, fmt.Errorf("jobs: 缓存效果聚合取锁失败: %w", err)
	}
	if !acquired {
		c.logger.Info("cache_effectiveness_aggregate_skipped", map[string]any{"reason": "locked"})
		return CacheEffectivenessResult{Skipped: true, Reason: "locked"}, nil
	}

	lastEnd, err := c.pools.LastCacheEffectivenessWindowEnd(ctx, tx)
	if err != nil {
		return CacheEffectivenessResult{}, fmt.Errorf("jobs: 读缓存效果窗口水位失败: %w", err)
	}
	windowStart, windowEnd, advanced := cacheEffectivenessWindow(lastEnd, c.now())
	if !advanced {
		c.logger.Info("cache_effectiveness_aggregate_skipped", map[string]any{
			"reason":      "window_not_advanced",
			"windowStart": windowStart.UTC().Format(time.RFC3339Nano),
			"windowEnd":   windowEnd.UTC().Format(time.RFC3339Nano),
		})
		return CacheEffectivenessResult{
			WindowStart: &windowStart,
			WindowEnd:   &windowEnd,
			Skipped:     true,
			Reason:      "window_not_advanced",
		}, nil
	}

	written, err := c.pools.InsertCacheEffectivenessWindow(ctx, tx, windowStart, windowEnd)
	if err != nil {
		return CacheEffectivenessResult{}, fmt.Errorf("jobs: 缓存效果聚合写入失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CacheEffectivenessResult{}, fmt.Errorf("jobs: 缓存效果聚合提交失败: %w", err)
	}
	committed = true

	if written > 0 {
		// 字段名与 Node 的日志逐字同名（service.ts:149-153），便于双跑期按同一口径对账。
		c.logger.Info("cache_effectiveness_window_aggregated", map[string]any{
			"windowStart":   windowStart.UTC().Format(time.RFC3339Nano),
			"windowEnd":     windowEnd.UTC().Format(time.RFC3339Nano),
			"groupsWritten": written,
		})
	}
	return CacheEffectivenessResult{
		WindowStart:   &windowStart,
		WindowEnd:     &windowEnd,
		GroupsWritten: written,
	}, nil
}

// cacheEffectivenessEnabled 复刻 Node 的开关判定：系统设置优先、env 兜底。
//
// 抽成纯函数是为了让「null 回落 env」与「显式 false 关闭」这两个分支能被单测直接钉住——
// 它们是本任务唯一的运行期开关，判错会让聚合整轮不跑且只在日志里留一行。
func cacheEffectivenessEnabled(setting *bool, envDefault bool) bool {
	if setting != nil {
		return *setting
	}
	return envDefault
}

// cacheEffectivenessWindow 复刻 Node 的窗口计算（`service.ts:62-80`），返回 advanced=false
// 表示窗口尚未推进（本轮无事可做）。
//
// 抽成纯函数的原因：窗口推进是本任务最容易写错、也最难在集成测试里构造边界的一环
// （首次回看、恰好相等、时钟回拨），纯函数可以逐边界断言。
func cacheEffectivenessWindow(
	lastEnd *time.Time,
	now time.Time,
) (windowStart time.Time, windowEnd time.Time, advanced bool) {
	windowEnd = now.Add(-cacheEffectivenessSafetyLag)
	if lastEnd != nil {
		windowStart = *lastEnd
	} else {
		windowStart = now.Add(-cacheEffectivenessInitialLookback)
	}
	// Node 用 `windowStart >= windowEnd` 判停：相等同样跳过（空窗口不写行）。
	return windowStart, windowEnd, windowStart.Before(windowEnd)
}
