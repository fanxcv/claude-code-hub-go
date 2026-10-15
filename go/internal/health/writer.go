// Package health 是熔断状态的写入面（route.HealthSink 的实体实现）。
//
// 选路包只读、本包只写：把两侧分开是为了让「谁在改熔断状态」可归因（见 route.HealthSink 注释）。
// 键名、字段名、默认值与 TTL 逐字对齐 Node，切换期两侧读写同一份 Redis 状态。
//
// 语义出处（Node）：
//   - 供应商失败记数/开闸：src/lib/circuit-breaker.ts:512
//   - 供应商成功/半开转闭：src/lib/circuit-breaker.ts:640
//   - 熔断被配置禁用时强制归闭：src/lib/circuit-breaker.ts:261
//   - 端点失败/成功：src/lib/endpoint-circuit-breaker.ts:334 / :382（默认 3 次、5 分钟、半开 1 次成功）
//   - 厂级全端点超时开闸：src/lib/vendor-type-circuit-breaker.ts:145
//   - 状态 Hash 形状与 TTL：src/lib/redis/circuit-breaker-state.ts、endpoint-circuit-breaker-state.ts、
//     vendor-type-circuit-breaker-state.ts
//
// 键形制与 Node 一致（同一份 Redis 状态），但**落库方式不同**：Node 是内存态 + 覆盖式 HSET，
// 本包一律走 WATCH + MULTI/EXEC 的读改写（见 changeState），因为覆盖式写会在并发失败风暴里丢计数。
package health

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

const (
	providerStateTTLSeconds   = 86400   // circuit-breaker-state.ts:43
	endpointStateTTLSeconds   = 86400   // endpoint-circuit-breaker-state.ts:24
	vendorTypeStateTTLSeconds = 2592000 // vendor-type-circuit-breaker-state.ts:27
	// retentionShrinkSeconds 是高并发模式下长 TTL 的收缩上限（proxy-runtime.ts:101）。
	retentionShrinkSeconds = 86400

	// 端点级默认值（endpoint-circuit-breaker.ts:19）。
	endpointFailureThreshold         = 3
	endpointOpenDurationMS           = 300000
	endpointHalfOpenSuccessThreshold = 1

	// vendorTypeOpenDurationMinMS 是厂级开闸时长下限（vendor-type-circuit-breaker.ts:159 的 max(1000, x)）。
	vendorTypeOpenDurationMinMS = 1000

	// maxStateRetries 是一次状态读改写的最大尝试次数：WATCH 冲突即用新状态重算重试。
	//
	// 为何有上限：失败风暴下同一供应商会被多个请求/多个实例同时记账，冲突是常态不是异常；
	// 上限只为不让请求线程被无限钉住。到顶仍冲突即放弃本次记账并记 warn——代价是一次计数丢失，
	// 比改造前「后写者必然覆盖先写者」只少不多。
	maxStateRetries = 5
)

// SettingsSource 提供系统设置快照，仅用于解析高并发模式下的 TTL 收缩。
//
// 与 guard.SettingsSource 同形，另行声明以免本包依赖守卫层。
type SettingsSource interface {
	FindSystemSettings(ctx context.Context) (*store.SystemSettings, error)
}

// Options 是 Writer 的构造参数。
type Options struct {
	// Redis 为 nil 时所有写入一律跳过（Node 在无 Redis 时也只改内存态；本包无内存态）。
	Redis redis.UniversalClient
	// EndpointCircuitBreakerEnabled 对应 ENABLE_ENDPOINT_CIRCUIT_BREAKER：关闭时端点级与厂级都不写。
	EndpointCircuitBreakerEnabled bool
	// Settings 为 nil 时按非高并发处理（长 TTL 不收缩）。
	Settings SettingsSource
	// Now 可注入时钟。
	Now func() time.Time
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// OnProviderOpened 在供应商熔断**初次**开闸时回调（Node 在此发告警 webhook）。nil 时不告警。
	OnProviderOpened func(providerID int64, failureCount int64, openUntilMS int64, cause error)
	// OnEndpointOpened 在端点熔断初次开闸时回调。nil 时不告警。
	OnEndpointOpened func(endpointID int64, failureCount int64, openUntilMS int64, cause error)
}

// Writer 实现 route.HealthSink：供应商级、端点级、厂级三档熔断状态的写入。
type Writer struct {
	redis redis.UniversalClient
	// watch 是状态读改写的事务执行面（redWatch 为生产实现）。
	//
	// 做成字段只为一件事：*redis.Tx 无法在用例里构造，而「并发写者插在本次读改写中间」
	// 这条缺陷必须能在单测里确定复现（见 writer_test.go 的 fakeWatch）。
	// 不导出也不进阶到 Options：除了本包用例，没有任何调用方需要换掉它。
	watch           stateWatch
	endpointEnabled bool
	settings        SettingsSource
	now             func() time.Time
	logger          *logx.Logger
	onProviderOpen  func(int64, int64, int64, error)
	onEndpointOpen  func(int64, int64, int64, error)
}

// stateChange 是一次状态读改写的**纯计算**部分：给出读到的当前状态对应的新状态。
//
// write=false 表示本次不改动状态（原逻辑的「无变化即不写」）。它必须无副作用：
// 事务冲突会重跑它，通知与日志一律在提交成功之后发（见 RecordProviderFailure）。
type stateChange func(current state) (next state, write bool, err error)

// stateTx 是一次 WATCH 事务里可见的最小面：读一个哈希 + 在 MULTI/EXEC 里提交。
type stateTx interface {
	HGetAll(ctx context.Context, key string) *redis.MapStringStringCmd
	TxPipelined(ctx context.Context, fn func(redis.Pipeliner) error) ([]redis.Cmder, error)
}

// stateWatch 是「WATCH 一段键并在事务里执行」的能力。
type stateWatch interface {
	Watch(ctx context.Context, fn func(stateTx) error, keys ...string) error
}

// redisWatch 把 UniversalClient 的 Watch 适配成 stateWatch（只差回调参数 *redis.Tx → stateTx）。
type redisWatch struct{ client redis.UniversalClient }

func (r redisWatch) Watch(ctx context.Context, fn func(stateTx) error, keys ...string) error {
	return r.client.Watch(ctx, func(tx *redis.Tx) error { return fn(tx) }, keys...)
}

// newStateWatch 装配事务执行面；无 Redis 时返回 nil（此时 enabled() 为假，写入全路径早退）。
func newStateWatch(client redis.UniversalClient) stateWatch {
	if client == nil {
		return nil
	}
	return redisWatch{client: client}
}

// 编译期钉住接口实现。
var _ route.HealthSink = (*Writer)(nil)

// NewWriter 构造熔断状态写入器。
func NewWriter(options Options) *Writer {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	return &Writer{
		redis:           options.Redis,
		watch:           newStateWatch(options.Redis),
		endpointEnabled: options.EndpointCircuitBreakerEnabled,
		settings:        options.Settings,
		now:             now,
		logger:          logger,
		onProviderOpen:  options.OnProviderOpened,
		onEndpointOpen:  options.OnEndpointOpened,
	}
}

// state 是与 Node 共用的熔断状态（同一 Hash 的两个形状子集）。
//
// 空字符串表示 null：Node 的 serializeState 把 null 写成 ""，读回时按 falsy 还原为 null。
type state struct {
	failureCount         int64
	lastFailureTimeMS    int64
	circuitState         route.CircuitState
	circuitOpenUntilMS   int64
	halfOpenSuccessCount int64
	manualOpen           bool
	// consecutiveOpenCount 是**等待阶梯**的级数 n（Go 侧增强字段，Node 无此字段）：
	// 连续「熔断 → 半开试探 → 未能恢复」的轮数，首次开闸为 0。缺失（老实例写的哈希）
	// 视为 0——那正是「不开阶梯」的取值，故向前兼容是惰性的、不需要迁移。
	consecutiveOpenCount int64
	// consecutiveOpenCountChangedAtMS 是级数最近一次变化的时间（0 表示从未变化）。
	//
	// 为何多存一个时间戳（超出「只加一个字段」的授权）：协调者裁决要求熔断详情里展示
	// 「最近一次变化时间」——历史级数无从回溯（Redis 里只有当前值），这是唯一可得的口径。
	consecutiveOpenCountChangedAtMS int64
}

func parseState(raw map[string]string) state {
	result := state{
		failureCount:                    parseInt(raw["failureCount"]),
		lastFailureTimeMS:               parseInt(raw["lastFailureTime"]),
		circuitState:                    route.CircuitState(raw["circuitState"]),
		circuitOpenUntilMS:              parseInt(raw["circuitOpenUntil"]),
		halfOpenSuccessCount:            parseInt(raw["halfOpenSuccessCount"]),
		manualOpen:                      raw["manualOpen"] == "1",
		consecutiveOpenCount:            parseInt(raw["consecutiveOpenCount"]),
		consecutiveOpenCountChangedAtMS: parseInt(raw["consecutiveOpenCountChangedAt"]),
	}
	if result.circuitState == "" {
		result.circuitState = route.StateClosed
	}
	return result
}

// RecordProviderFailure 记一次供应商失败，达到阈值即开闸（Node circuit-breaker.ts:512）。
//
// 在 Node 语义之上多了一层**等待阶梯**：窗口到期后放行试探、试探又失败时，窗口按
// base + increment × n 递增（n 封顶 maxCount，恢复即归零）。出厂默认 increment=0 或
// maxCount=0 时恒为 base，与加阶梯之前逐字段一致（见 ladder.go 的文件头）。
func (w *Writer) RecordProviderFailure(ctx context.Context, providerID int64, cause error) error {
	if !w.enabled() || providerID <= 0 {
		return nil
	}
	key := route.ProviderStateKeyPrefix + strconv.FormatInt(providerID, 10)
	config := route.ReadProviderCircuitConfig(ctx, w.redis, providerID)
	nowMS := w.now().UnixMilli()

	// 阶梯日志与开闸告警在**提交成功之后**发：事务冲突会重跑 change，事务体内触发即重复告警
	// （见 changeState）。三个变量在每次尝试的开头重清，而提交成功必定是最后一次尝试。
	var (
		didOpen     bool
		opened      state
		trialFailed bool
	)
	err := w.changeState(ctx, key, providerStateTTLSeconds, func(current state) (state, bool, error) {
		didOpen, opened, trialFailed = false, state{}, false
		if config.Disabled() {
			return forceClosed(current)
		}

		current.failureCount++
		current.lastFailureTimeMS = nowMS

		if current.circuitState == route.StateOpen && !openWindowExpiredAt(current, nowMS) {
			// 已开闸（**窗口内**）：不重复开闸、不重置 openUntil，避免失败风暴把开闸时刻无限推后。
			return current, true, nil
		}

		// trialFailed：本次失败是否属于「一轮已开始的试探未能恢复」？
		//
		// 判据必须在这里**先取走**：下面的 markHalfOpen 会把 raw open 改写成 half-open，
		// 之后再判就只能看见「已是 half-open」这半个条件，首次开闸（closed → open）也会被误计成
		// 一次阶梯推进。raw=open 且窗口已过期、或已是 raw=half-open，两者都表示本轮试探已开始。
		trialFailed = openWindowExpiredAt(current, nowMS) || current.circuitState == route.StateHalfOpen

		if openWindowExpiredAt(current, nowMS) {
			// 窗口已过期：Node 此刻已迁到 half-open，半开下的失败走阈值分支
			// ⇒ 以**新的** openUntil 重新开闸。不迁就会落进上面那条「已开闸」分支，
			// openUntil 永远不更新，也就永远回不到试探与恢复。
			markHalfOpen(&current)
		}

		if current.failureCount >= config.FailureThreshold {
			// Node 在阈值命中时强制重读一次配置（它那份配置有缓存）；本包每次直读 Redis，等价。
			if trialFailed {
				current.consecutiveOpenCount = nextLadderLevel(current.consecutiveOpenCount, config.MaxOpenCount)
				current.consecutiveOpenCountChangedAtMS = nowMS
			} else {
				// 首次开闸（closed → open）：恒为第 0 级。显式归零是为了不让历史残值
				// （老版本写的、或手工改过 Redis 的）把首个窗口抬高。
				current.consecutiveOpenCount = 0
				current.consecutiveOpenCountChangedAtMS = 0
			}
			window := ladderWindowMS(config, current.consecutiveOpenCount)
			current.circuitState = route.StateOpen
			current.circuitOpenUntilMS = nowMS + window
			current.halfOpenSuccessCount = 0
			didOpen, opened = true, current
		}
		return current, true, nil
	})
	if err != nil {
		return err
	}
	if !didOpen {
		return nil
	}
	if trialFailed {
		// 聚合口径：一次「试探未能恢复」= 一条；level 是本次新窗口的级数（封顶后不再增长，
		// 故它同时也是「是否还在爬阶梯」的读数）。
		w.logger.Info("provider_circuit_ladder_advanced", map[string]any{
			"providerId":  providerID,
			"level":       opened.consecutiveOpenCount,
			"windowMs":    opened.circuitOpenUntilMS - nowMS,
			"baseMs":      config.OpenDurationMS,
			"incrementMs": config.OpenDurationIncrementMS,
			"maxCount":    config.MaxOpenCount,
		})
	}
	if w.onProviderOpen != nil {
		w.onProviderOpen(providerID, opened.failureCount, opened.circuitOpenUntilMS, cause)
	}
	return nil
}

// RecordProviderSuccess 记一次供应商成功（Node circuit-breaker.ts:640）。
func (w *Writer) RecordProviderSuccess(ctx context.Context, providerID int64) error {
	if !w.enabled() || providerID <= 0 {
		return nil
	}
	key := route.ProviderStateKeyPrefix + strconv.FormatInt(providerID, 10)
	config := route.ReadProviderCircuitConfig(ctx, w.redis, providerID)
	nowMS := w.now().UnixMilli()
	return w.changeState(ctx, key, providerStateTTLSeconds, func(current state) (state, bool, error) {
		if config.Disabled() {
			return forceClosed(current)
		}

		changed := false

		// 窗口已过期的 open 先落成 half-open。
		//
		// 为何必须在这里补这一步：Node 的 isCircuitOpen 在读到过期窗口时就把状态迁到 half-open
		// 并 persistStateToRedis（circuit-breaker.ts:492-496）；Go 的选路侧是**只读方**
		// （route.HealthSink 注释里的不变量），不能在读路径写状态，于是那一步在 Go 里缺失。
		// 后果不是「显示不准」那么简单：成功路径只认 half-open（下面的 switch），过期后永远是 open，
		// 成功一次次被忽略 → **熔断再也回不到 closed**（生产现象：一批供应商永远显示已熔断）。
		if openWindowExpiredAt(current, nowMS) {
			markHalfOpen(&current)
			changed = true
		}

		switch current.circuitState {
		case route.StateHalfOpen:
			current.halfOpenSuccessCount++
			changed = true
			threshold := config.HalfOpenSuccessThreshold
			if threshold <= 0 {
				threshold = route.DefaultHalfOpenSuccessThreshold
			}
			if current.halfOpenSuccessCount >= threshold {
				closeState(&current)
			}
		case route.StateClosed:
			if current.failureCount > 0 || current.consecutiveOpenCount > 0 {
				current.failureCount = 0
				current.lastFailureTimeMS = 0
				// 阶梯级数：closed 态一律归零（恢复的回零就发生在转入 closed 的那一步；
				// 这里兑的是「已是 closed 却还带着级数」的历史残值，避免下个窗口从高位起）。
				current.consecutiveOpenCount = 0
				current.consecutiveOpenCountChangedAtMS = 0
				changed = true
			}
		}
		if !changed {
			// Node 仅在状态变化时持久化（recordSuccess 的 stateChanged 判定）。
			return current, false, nil
		}
		return current, true, nil
	})
}

// RecordEndpointFailure 记一次端点失败（Node endpoint-circuit-breaker.ts:334）。
func (w *Writer) RecordEndpointFailure(ctx context.Context, endpointID int64, cause error) error {
	if !w.endpointEnabled || endpointID <= 0 {
		return nil
	}
	key := route.EndpointStateKeyPrefix + strconv.FormatInt(endpointID, 10)
	// 与供应商级同：开闸告警在提交成功之后发（重试会重跑 change）。
	var (
		didOpen bool
		opened  state
	)
	err := w.changeState(ctx, key, endpointStateTTLSeconds, func(current state) (state, bool, error) {
		didOpen, opened = false, state{}
		nowMS := w.now().UnixMilli()
		current.failureCount++
		current.lastFailureTimeMS = nowMS

		if endpointFailureThreshold > 0 &&
			current.failureCount >= endpointFailureThreshold &&
			current.circuitState != route.StateOpen {
			current.circuitState = route.StateOpen
			current.circuitOpenUntilMS = nowMS + endpointOpenDurationMS
			current.halfOpenSuccessCount = 0
			didOpen, opened = true, current
		}
		return current, true, nil
	})
	if err != nil || !didOpen || w.onEndpointOpen == nil {
		return err
	}
	w.onEndpointOpen(endpointID, opened.failureCount, opened.circuitOpenUntilMS, cause)
	return nil
}

// RecordEndpointSuccess 记一次端点成功（Node endpoint-circuit-breaker.ts:382）。
func (w *Writer) RecordEndpointSuccess(ctx context.Context, endpointID int64) error {
	if !w.endpointEnabled || endpointID <= 0 {
		return nil
	}
	key := route.EndpointStateKeyPrefix + strconv.FormatInt(endpointID, 10)
	return w.changeState(ctx, key, endpointStateTTLSeconds, func(current state) (state, bool, error) {
		if current.circuitState == route.StateHalfOpen {
			current.halfOpenSuccessCount++
			if current.halfOpenSuccessCount >= endpointHalfOpenSuccessThreshold {
				closeState(&current)
			}
			return current, true, nil
		}
		if current.failureCount > 0 {
			current.failureCount = 0
			current.lastFailureTimeMS = 0
			current.circuitOpenUntilMS = 0
			return current, true, nil
		}
		return current, false, nil
	})
}

// ResetEndpointCircuit 强制归闭端点熔断并删除状态键，返回删除前的状态。
//
// 与 RecordEndpointSuccess 的分工：后者是**请求路径**上的成功记账（半开计数递增、闭合态清零计数），
// 而探活是外部拨测，成功即证明端点可用，Node 因此走 resetEndpointCircuit
// （src/lib/endpoint-circuit-breaker.ts:418）：无论处于 open 还是 half-open 一律归闭，
// 且直接 DEL 键（deleteEndpointCircuitState，src/lib/redis/endpoint-circuit-breaker-state.ts:192），
// 不留残值。若探活成功误用 RecordEndpointSuccess，处于 open 的端点会被永久卡在开闸态
// ——拨测就失去了探活归闸的意义。
//
// 读与删刻意不包在同一个事务里：DEL 本身原子，且这里的键上没有任何需要累加/比较的字段
// （不是读改写）；不原子只影响返回值——它可能是一个并发写之前的快照，而该值只用于接口回显。
func (w *Writer) ResetEndpointCircuit(ctx context.Context, endpointID int64) (route.CircuitState, error) {
	if !w.endpointEnabled || endpointID <= 0 {
		return route.StateClosed, nil
	}
	key := route.EndpointStateKeyPrefix + strconv.FormatInt(endpointID, 10)
	current, err := w.readState(ctx, key)
	if err != nil {
		return route.StateClosed, err
	}
	if err := w.redis.Del(ctx, key).Err(); err != nil {
		return current.circuitState, err
	}
	return current.circuitState, nil
}

// RecordVendorTypeAllEndpointsTimeout 记一次厂级全端点超时并开闸（Node vendor-type-circuit-breaker.ts:145）。
func (w *Writer) RecordVendorTypeAllEndpointsTimeout(
	ctx context.Context,
	vendorID int64,
	providerType convert.ProviderType,
	openDuration time.Duration,
) error {
	if !w.endpointEnabled || vendorID <= 0 {
		return nil
	}
	key := vendorTypeKey(vendorID, providerType)
	openMS := openDuration.Milliseconds()
	if openMS < vendorTypeOpenDurationMinMS {
		openMS = vendorTypeOpenDurationMinMS
	}
	nowMS := w.now().UnixMilli()
	return w.changeState(ctx, key, w.vendorTypeTTLSeconds(ctx), func(current state) (state, bool, error) {
		if current.manualOpen {
			// 手工开闸优先：自动写入不改手工状态（Node 在同一分支上 return）。
			return current, false, nil
		}
		current.circuitState = route.StateOpen
		current.lastFailureTimeMS = nowMS
		current.circuitOpenUntilMS = nowMS + openMS
		return current, true, nil
	})
}

func vendorTypeKey(vendorID int64, providerType convert.ProviderType) string {
	return route.VendorTypeStateKeyPrefix + strconv.FormatInt(vendorID, 10) + ":" + string(providerType)
}

// closeState 把状态归零为 closed（Node resetHealthToClosed 的等价物）。
//
// 阶梯级数一并归零：这就是「如果恢复了，则又从 base 开始」的落点（用户明示口径）。
func closeState(current *state) {
	current.circuitState = route.StateClosed
	current.failureCount = 0
	current.lastFailureTimeMS = 0
	current.circuitOpenUntilMS = 0
	current.halfOpenSuccessCount = 0
	current.consecutiveOpenCount = 0
	current.consecutiveOpenCountChangedAtMS = 0
}

// openWindowExpiredAt 报告「**原始态是 open** 且已过 circuitOpenUntil」（Node 的 isCircuitOpen 里那段迁移条件）。
//
// 必须限定 raw == open：有效态为 half-open 有两类来因（raw=open+已过期、raw=half-open），
// 只有前者需要迁移。若不分，已在半开计数的供应商每来一次成功都会被重新清零，
// 半开计数永远凑不够阈值 ⇒ 熔断同样回不到 closed。
//
// 时刻由调用方传入：一次状态读改写里的多处判定必须用同一个时刻（本次记账的 nowMS），
// 否则「读到的窗口」与「判定用的现在」各取一次时钟，重试时还会各自漂移。
// 到期判定复用 route.EffectiveProviderState，与选路侧（ProviderOpen）同源。
func openWindowExpiredAt(current state, nowMS int64) bool {
	if current.circuitState != route.StateOpen {
		return false
	}
	return route.EffectiveProviderState(current.circuitState, current.circuitOpenUntilMS, nowMS) == route.StateHalfOpen
}

// markHalfOpen 复刻 Node 的 isCircuitOpen 在窗口过期时的迁移：open -> half-open、halfOpenSuccessCount 归零。
//
// 只改这两项：Node 的迁移也就只改这两项（failureCount 与 circuitOpenUntil 保持原值），
// 所以半开下的首次失败仍会因 failureCount 已达阈值而立即以**新窗口**重新开闸（见 recordFailure）。
func markHalfOpen(current *state) {
	current.circuitState = route.StateHalfOpen
	current.halfOpenSuccessCount = 0
}

// forceClosed 复刻 handleDisabledCircuitBreaker：熔断被配置禁用时，把非闭状态强制归闭并落库。
// write=false 表示已是干净闭态、无需落库（原 forceClosed 的早退）。
func forceClosed(current state) (state, bool, error) {
	if current.circuitState == route.StateClosed &&
		current.failureCount == 0 &&
		current.lastFailureTimeMS == 0 &&
		current.circuitOpenUntilMS == 0 &&
		current.halfOpenSuccessCount == 0 &&
		current.consecutiveOpenCount == 0 {
		return current, false, nil
	}
	closeState(&current)
	return current, true, nil
}

func (w *Writer) enabled() bool { return w != nil && w.redis != nil }

// readState 读现有状态（**非事务**读，只给 ResetEndpointCircuit 这种「读一个快照就删键」的路径用）。
// 读改写一律走 changeState：那里的读必须在 WATCH 之内。
func (w *Writer) readState(ctx context.Context, key string) (state, error) {
	raw, err := w.redis.HGetAll(ctx, key).Result()
	if err != nil {
		return state{}, err
	}
	return stateFromHash(raw), nil
}

// stateFromHash 把哈希原文折成状态；键不存在（空哈希）时按 closed，与 Node 的 getOrCreateHealth 等价。
func stateFromHash(raw map[string]string) state {
	current := parseState(raw)
	if len(raw) == 0 {
		current.circuitState = route.StateClosed
	}
	return current
}

// changeState 在事务保护下执行一次「读 → 改 → 写」：键在事务期间被并发改动即整段重来。
//
// 存在的理由（缺陷 P-2）：改造前是 readState → 内存改 → HSET 覆盖式落库，三步之间没有原子性。
// 失败风暴里同一供应商会被多个请求/多个实例同时记账，后写者用自己那份旧计数覆盖先写者的结果：
// 丢计数 ⇒ 已该开闸的供应商继续被放行、半开计数永远凑不够 ⇒ 熔断在风暴里形同虚设。
// WATCH 让「读到的值仍是提交时的值」成为提交的前提：键被动过，EXEC 就返回 nil，整段重算。
// （Node 当年也是内存态 + 覆盖式落库，但那个理由已不再成立：本包已是唯一写入方。）
func (w *Writer) changeState(ctx context.Context, key string, ttlSeconds int, change stateChange) error {
	for attempt := 1; ; attempt++ {
		err := w.watch.Watch(ctx, func(tx stateTx) error {
			raw, err := tx.HGetAll(ctx, key).Result()
			if err != nil {
				return err
			}
			next, write, err := change(stateFromHash(raw))
			if err != nil || !write {
				return err
			}
			cmds, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				queueStateWrite(ctx, pipe, key, next, ttlSeconds)
				return nil
			})
			if err != nil && !errors.Is(err, redis.TxFailedErr) {
				w.logStateWriteFailure(key, cmds, err)
			}
			return err
		}, key)
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, redis.TxFailedErr):
			// 真故障（连接断、命令错）：已在内层记 warn，重试也不会变好。
			return err
		case attempt >= maxStateRetries:
			w.logger.Warn("circuit_breaker_state_write_failed", map[string]any{
				"key":   key,
				"error": "并发写冲突 " + strconv.Itoa(maxStateRetries) + " 次仍未提交: " + err.Error(),
			})
			return err
		}
	}
}

// queueStateWrite 把一次状态落库排进管道：HSET 给定字段 + EXPIRE。
//
// HSET 只覆盖给定字段、不删其余字段，所以本次写入天然满足「不清掉既有键的其它字段」：
// 老实例（不认识阶梯那两个字段）写状态时不会抹掉它们，级数不会因为回滚而丢。
func queueStateWrite(ctx context.Context, pipe redis.Pipeliner, key string, current state, ttlSeconds int) {
	pipe.HSet(ctx, key, stateFields(key, current))
	pipe.Expire(ctx, key, time.Duration(ttlSeconds)*time.Second)
}

// stateFields 把一个状态折成要落库的哈希字段。
func stateFields(key string, current state) map[string]interface{} {
	fields := map[string]interface{}{
		"failureCount":         strconv.FormatInt(current.failureCount, 10),
		"lastFailureTime":      nullOrInt(current.lastFailureTimeMS),
		"circuitState":         string(current.circuitState),
		"circuitOpenUntil":     nullOrInt(current.circuitOpenUntilMS),
		"halfOpenSuccessCount": strconv.FormatInt(current.halfOpenSuccessCount, 10),
	}
	// 等待阶梯的两个字段只属于**供应商级**状态：端点级与厂级没有阶梯，把字段写进去只会
	// 给另两族熔断键添噪音（Node 的端点/厂级序列化里也没有它们）。
	if strings.HasPrefix(key, route.ProviderStateKeyPrefix) {
		fields["consecutiveOpenCount"] = strconv.FormatInt(current.consecutiveOpenCount, 10)
		fields["consecutiveOpenCountChangedAt"] = nullOrInt(current.consecutiveOpenCountChangedAtMS)
	}
	// manualOpen 只在厂级状态里存在；写空串会覆盖成 false，与 Node 的 deserialize 判定一致。
	if current.manualOpen {
		fields["manualOpen"] = "1"
	}
	return fields
}

// logStateWriteFailure 分因记录落库失败；事件名与改造前一致（HSET 失败 = 状态没落库，
// EXPIRE 失败 = TTL 没设上），运维告警规则不必改。
func (w *Writer) logStateWriteFailure(key string, cmds []redis.Cmder, err error) {
	fields := map[string]any{"key": key, "error": err.Error()}
	// 队列里的第二条命令就是 EXPIRE（见 queueStateWrite）——只为把两个事件名分开。
	if len(cmds) == 2 && cmds[0].Err() == nil && cmds[1].Err() != nil {
		w.logger.Warn("circuit_breaker_state_ttl_failed", fields)
		return
	}
	w.logger.Warn("circuit_breaker_state_write_failed", fields)
}

// vendorTypeTTLSeconds 解析厂级 TTL：高并发模式下收缩到 24 小时（proxy-runtime.ts:101）。
func (w *Writer) vendorTypeTTLSeconds(ctx context.Context) int {
	if w.settings == nil {
		return vendorTypeStateTTLSeconds
	}
	settings, err := w.settings.FindSystemSettings(ctx)
	if err != nil || settings == nil || !settings.EnableHighConcurrencyMode {
		return vendorTypeStateTTLSeconds
	}
	if vendorTypeStateTTLSeconds > retentionShrinkSeconds {
		return retentionShrinkSeconds
	}
	return vendorTypeStateTTLSeconds
}

// nullOrInt 把 null 写成空串、数值写成十进制（与 Node serializeState 逐字一致）。
func nullOrInt(value int64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func parseInt(raw string) int64 {
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value
}
