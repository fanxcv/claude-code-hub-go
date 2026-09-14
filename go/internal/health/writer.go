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
package health

import (
	"context"
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
	redis           redis.UniversalClient
	endpointEnabled bool
	settings        SettingsSource
	now             func() time.Time
	logger          *logx.Logger
	onProviderOpen  func(int64, int64, int64, error)
	onEndpointOpen  func(int64, int64, int64, error)
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
	current, err := w.readState(ctx, key)
	if err != nil {
		return err
	}
	config := route.ReadProviderCircuitConfig(ctx, w.redis, providerID)
	if config.Disabled() {
		return w.forceClosed(ctx, key, current)
	}

	nowMS := w.now().UnixMilli()
	current.failureCount++
	current.lastFailureTimeMS = nowMS

	if current.circuitState == route.StateOpen && !w.openWindowExpired(current) {
		// 已开闸（**窗口内**）：不重复开闸、不重置 openUntil，避免失败风暴把开闸时刻无限推后。
		return w.writeState(ctx, key, current, providerStateTTLSeconds)
	}

	// trialFailed：本次失败是否属于「一轮已开始的试探未能恢复」？
	//
	// 判据必须在这里**先取走**：下面的 markHalfOpen 会把 raw open 改写成 half-open，
	// 之后再判就只能看见「已是 half-open」这半个条件，首次开闸（closed → open）也会被误计成
	// 一次阶梯推进。raw=open 且窗口已过期、或已是 raw=half-open，两者都表示本轮试探已开始。
	trialFailed := w.openWindowExpired(current) || current.circuitState == route.StateHalfOpen

	if w.openWindowExpired(current) {
		// 窗口已过期：Node 此刻已迁到 half-open，半开下的失败走阈值分支
		// ⇒ 以**新的** openUntil 重新开闸。不迁就会落进上面那条「已开闸」分支，
		// openUntil 永远不更新，也就永远回不到试探与恢复。
		markHalfOpen(&current)
	}

	if current.failureCount >= config.FailureThreshold && !config.Disabled() {
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
		if trialFailed {
			// 聚合口径：一次「试探未能恢复」= 一条；level 是本次新窗口的级数（封顶后不再增长，
			// 故它同时也是「是否还在爬阶梯」的读数）。
			w.logger.Info("provider_circuit_ladder_advanced", map[string]any{
				"providerId":  providerID,
				"level":       current.consecutiveOpenCount,
				"windowMs":    window,
				"baseMs":      config.OpenDurationMS,
				"incrementMs": config.OpenDurationIncrementMS,
				"maxCount":    config.MaxOpenCount,
			})
		}
		if w.onProviderOpen != nil {
			w.onProviderOpen(providerID, current.failureCount, current.circuitOpenUntilMS, cause)
		}
	}
	return w.writeState(ctx, key, current, providerStateTTLSeconds)
}

// RecordProviderSuccess 记一次供应商成功（Node circuit-breaker.ts:640）。
func (w *Writer) RecordProviderSuccess(ctx context.Context, providerID int64) error {
	if !w.enabled() || providerID <= 0 {
		return nil
	}
	key := route.ProviderStateKeyPrefix + strconv.FormatInt(providerID, 10)
	current, err := w.readState(ctx, key)
	if err != nil {
		return err
	}
	config := route.ReadProviderCircuitConfig(ctx, w.redis, providerID)
	if config.Disabled() {
		return w.forceClosed(ctx, key, current)
	}

	changed := false

	// 窗口已过期的 open 先落成 half-open。
	//
	// 为何必须在这里补这一步：Node 的 isCircuitOpen 在读到过期窗口时就把状态迁到 half-open
	// 并 persistStateToRedis（circuit-breaker.ts:492-496）；Go 的选路侧是**只读方**
	// （route.HealthSink 注释里的不变量），不能在读路径写状态，于是那一步在 Go 里缺失。
	// 后果不是「显示不准」那么简单：成功路径只认 half-open（下面的 switch），过期后永远是 open，
	// 成功一次次被忽略 → **熔断再也回不到 closed**（生产现象：一批供应商永远显示已熔断）。
	if w.openWindowExpired(current) {
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
		return nil
	}
	return w.writeState(ctx, key, current, providerStateTTLSeconds)
}

// RecordEndpointFailure 记一次端点失败（Node endpoint-circuit-breaker.ts:334）。
func (w *Writer) RecordEndpointFailure(ctx context.Context, endpointID int64, cause error) error {
	if !w.endpointEnabled || endpointID <= 0 {
		return nil
	}
	key := route.EndpointStateKeyPrefix + strconv.FormatInt(endpointID, 10)
	current, err := w.readState(ctx, key)
	if err != nil {
		return err
	}
	current.failureCount++
	current.lastFailureTimeMS = w.now().UnixMilli()

	if endpointFailureThreshold > 0 &&
		current.failureCount >= endpointFailureThreshold &&
		current.circuitState != route.StateOpen {
		current.circuitState = route.StateOpen
		current.circuitOpenUntilMS = w.now().UnixMilli() + endpointOpenDurationMS
		current.halfOpenSuccessCount = 0
		if w.onEndpointOpen != nil {
			w.onEndpointOpen(endpointID, current.failureCount, current.circuitOpenUntilMS, cause)
		}
	}
	return w.writeState(ctx, key, current, endpointStateTTLSeconds)
}

// RecordEndpointSuccess 记一次端点成功（Node endpoint-circuit-breaker.ts:382）。
func (w *Writer) RecordEndpointSuccess(ctx context.Context, endpointID int64) error {
	if !w.endpointEnabled || endpointID <= 0 {
		return nil
	}
	key := route.EndpointStateKeyPrefix + strconv.FormatInt(endpointID, 10)
	current, err := w.readState(ctx, key)
	if err != nil {
		return err
	}
	if current.circuitState == route.StateHalfOpen {
		current.halfOpenSuccessCount++
		if current.halfOpenSuccessCount >= endpointHalfOpenSuccessThreshold {
			closeState(&current)
		}
		return w.writeState(ctx, key, current, endpointStateTTLSeconds)
	}
	if current.failureCount > 0 {
		current.failureCount = 0
		current.lastFailureTimeMS = 0
		current.circuitOpenUntilMS = 0
		return w.writeState(ctx, key, current, endpointStateTTLSeconds)
	}
	return nil
}

// ResetEndpointCircuit 强制归闭端点熔断并删除状态键，返回删除前的状态。
//
// 与 RecordEndpointSuccess 的分工：后者是**请求路径**上的成功记账（半开计数递增、闭合态清零计数），
// 而探活是外部拨测，成功即证明端点可用，Node 因此走 resetEndpointCircuit
// （src/lib/endpoint-circuit-breaker.ts:418）：无论处于 open 还是 half-open 一律归闭，
// 且直接 DEL 键（deleteEndpointCircuitState，src/lib/redis/endpoint-circuit-breaker-state.ts:192），
// 不留残值。若探活成功误用 RecordEndpointSuccess，处于 open 的端点会被永久卡在开闸态
// ——拨测就失去了探活归闸的意义。
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
	current, err := w.readState(ctx, key)
	if err != nil {
		return err
	}
	if current.manualOpen {
		// 手工开闸优先：自动写入不改手工状态（Node 在同一分支上 return）。
		return nil
	}
	openMS := openDuration.Milliseconds()
	if openMS < vendorTypeOpenDurationMinMS {
		openMS = vendorTypeOpenDurationMinMS
	}
	nowMS := w.now().UnixMilli()
	current.circuitState = route.StateOpen
	current.lastFailureTimeMS = nowMS
	current.circuitOpenUntilMS = nowMS + openMS
	return w.writeState(ctx, key, current, w.vendorTypeTTLSeconds(ctx))
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

// openWindowExpired 报告「**原始态是 open** 且已过 circuitOpenUntil」（Node 的 isCircuitOpen 里那段迁移条件）。
//
// 必须限定 raw == open：有效态为 half-open 有两类来因（raw=open+已过期、raw=half-open），
// 只有前者需要迁移。若不分，已在半开计数的供应商每来一次成功都会被重新清零，
// 半开计数永远凑不够阈值 ⇒ 熔断同样回不到 closed。
//
// 到期判定复用 route.EffectiveProviderState，与选路侧（ProviderOpen）同源。
func (w *Writer) openWindowExpired(current state) bool {
	if current.circuitState != route.StateOpen {
		return false
	}
	return route.EffectiveProviderState(current.circuitState, current.circuitOpenUntilMS, w.now().UnixMilli()) == route.StateHalfOpen
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
func (w *Writer) forceClosed(ctx context.Context, key string, current state) error {
	if current.circuitState == route.StateClosed &&
		current.failureCount == 0 &&
		current.lastFailureTimeMS == 0 &&
		current.circuitOpenUntilMS == 0 &&
		current.halfOpenSuccessCount == 0 &&
		current.consecutiveOpenCount == 0 {
		return nil
	}
	closeState(&current)
	return w.writeState(ctx, key, current, providerStateTTLSeconds)
}

func (w *Writer) enabled() bool { return w != nil && w.redis != nil }

// readState 读现有状态；键不存在时返回全零（closed），与 Node 的 getOrCreateHealth 等价。
func (w *Writer) readState(ctx context.Context, key string) (state, error) {
	raw, err := w.redis.HGetAll(ctx, key).Result()
	if err != nil {
		return state{}, err
	}
	current := parseState(raw)
	if len(raw) == 0 {
		current.circuitState = route.StateClosed
	}
	return current, nil
}

// writeState 写状态并设置 TTL（Node：HSET + EXPIRE，见 circuit-breaker-state.ts:136）。
//
// 已知天花板：读改写非原子，多实例并发记账可能丢计数（Node 侧同样是内存态 + 覆盖式落库）。
// 若将来需要精确计数，改为 Lua 或 WATCH。
func (w *Writer) writeState(ctx context.Context, key string, current state, ttlSeconds int) error {
	fields := map[string]interface{}{
		"failureCount":         strconv.FormatInt(current.failureCount, 10),
		"lastFailureTime":      nullOrInt(current.lastFailureTimeMS),
		"circuitState":         string(current.circuitState),
		"circuitOpenUntil":     nullOrInt(current.circuitOpenUntilMS),
		"halfOpenSuccessCount": strconv.FormatInt(current.halfOpenSuccessCount, 10),
	}
	// 等待阶梯的两个字段只属于**供应商级**状态：端点级与厂级没有阶梯，把字段写进去只会
	// 给另两族熔断键添噪音（Node 的端点/厂级序列化里也没有它们）。
	//
	// HSET 只覆盖给定字段、不删其余字段，所以本次写入天然满足「不清掉既有键的其它字段」：
	// 老实例（不认识这两个字段）写状态时不会抹掉它们，级数不会因为回滚而丢。
	if strings.HasPrefix(key, route.ProviderStateKeyPrefix) {
		fields["consecutiveOpenCount"] = strconv.FormatInt(current.consecutiveOpenCount, 10)
		fields["consecutiveOpenCountChangedAt"] = nullOrInt(current.consecutiveOpenCountChangedAtMS)
	}
	// manualOpen 只在厂级状态里存在；写空串会覆盖成 false，与 Node 的 deserialize 判定一致。
	if current.manualOpen {
		fields["manualOpen"] = "1"
	}
	if err := w.redis.HSet(ctx, key, fields).Err(); err != nil {
		w.logger.Warn("circuit_breaker_state_write_failed", map[string]any{"key": key, "error": err.Error()})
		return err
	}
	if err := w.redis.Expire(ctx, key, time.Duration(ttlSeconds)*time.Second).Err(); err != nil {
		w.logger.Warn("circuit_breaker_state_ttl_failed", map[string]any{"key": key, "error": err.Error()})
		return err
	}
	return nil
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
