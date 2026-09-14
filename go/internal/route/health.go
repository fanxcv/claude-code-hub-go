package route

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/redis/go-redis/v9"
)

// ExitReason 是状态读取失败时的降级原因，用于把「读不到」与「确实关闭」区分开。
type ExitReason string

const (
	// ExitNoRedis 未注入 Redis 客户端。
	ExitNoRedis ExitReason = "no_redis"
	// ExitReadError Redis 读取出错。
	ExitReadError ExitReason = "redis_read_error"
)

// Redis 键形制，逐字对齐 Node（改动即破坏与 Node 的状态共享）：
//   - circuit_breaker:state:{providerId}                （Hash，src/lib/redis/circuit-breaker-state.ts:54）
//   - circuit_breaker:config:{providerId}               （Hash，src/lib/redis/circuit-breaker-config.ts:37）
//   - vendor_type_circuit_breaker:state:{vendorId}:{type}（Hash，src/lib/redis/vendor-type-circuit-breaker-state.ts:29）
//   - endpoint_circuit_breaker:state:{endpointId}       （Hash，src/lib/redis/endpoint-circuit-breaker-state.ts:25）
const (
	ProviderStateKeyPrefix   = "circuit_breaker:state:"
	ProviderConfigKeyPrefix  = "circuit_breaker:config:"
	VendorTypeStateKeyPrefix = "vendor_type_circuit_breaker:state:"
	EndpointStateKeyPrefix   = "endpoint_circuit_breaker:state:"
)

// CircuitState 是熔断状态取值（与 Node 的 "closed" | "open" | "half-open" 逐字一致）。
type CircuitState string

const (
	// StateClosed 正常。
	StateClosed CircuitState = "closed"
	// StateOpen 打开（拒绝）。
	StateOpen CircuitState = "open"
	// StateHalfOpen 半开（放行试探）。
	StateHalfOpen CircuitState = "half-open"
)

const (
	// DefaultFailureThreshold 与 Node 的 DEFAULT_CIRCUIT_BREAKER_CONFIG.failureThreshold 一致。
	DefaultFailureThreshold = 5
	// DefaultOpenDurationMS 与 Node 的默认开启时长一致（30 分钟）。
	DefaultOpenDurationMS = 1800000
	// DefaultHalfOpenSuccessThreshold 与 Node 的 halfOpenSuccessThreshold 一致。
	DefaultHalfOpenSuccessThreshold = 2
	// DefaultOpenDurationIncrementMS 是等待阶梯的出厂默认递增时长：**0 = 不启用阶梯**。
	//
	// 协调者裁决（用户明示）：出厂默认不改变现行为——窗口恒为基础值 openDuration，
	// 与加阶梯之前逐字段一致；想用时由管理员在供应商阈值里自行填写（如 10m/3）。
	DefaultOpenDurationIncrementMS = 0
	// DefaultMaxOpenCount 是等待阶梯的出厂默认最大次数：**0 = 不启用阶梯**。
	//
	// 与递增时长是**与**关系：任一项为 0，阶梯即退化为基础值。
	DefaultMaxOpenCount = 0
)

// ProviderCircuitConfig 是供应商熔断配置。
//
// 读写两侧共用同一份解析，避免「读闸用的阈值」与「写闸用的阈值」各自漂移。
type ProviderCircuitConfig struct {
	// FailureThreshold 非正即熔断器关闭（Node 的 isCircuitBreakerDisabled）。
	FailureThreshold int64
	// OpenDurationMS 开闸时长（等待阶梯的基数：第 0 级窗口）。
	OpenDurationMS int64
	// HalfOpenSuccessThreshold 半开转闭所需的连续成功数。
	HalfOpenSuccessThreshold int64
	// OpenDurationIncrementMS 是**等待阶梯**的递增时长（哈希字段 releaseIncrement）。
	// 非正即不递增（退化为恒为基数，与加阶梯之前逐字段一致）。
	//
	// 与 Node 的关系：Node 的 DEFAULT_CIRCUIT_BREAKER_CONFIG 里**没有**这一项，也没有阶梯；
	// 这是 Go 侧的**有意增强**（用户明示需求），故无对拍基准，用例是唯一防线。
	// 出厂默认 0 = 不启用，故默认下与 Node 行为逐项一致。
	OpenDurationIncrementMS int64
	// MaxOpenCount 是等待阶梯的最大次数（哈希字段 maxOpenCount）。
	// 非正即不递增（同上）；超过它的级数一律取该值。
	MaxOpenCount int64
}

// Disabled 复刻 isCircuitBreakerDisabled：阈值非法或非正即关闭熔断。
func (c ProviderCircuitConfig) Disabled() bool { return c.FailureThreshold <= 0 }

// ReadProviderCircuitConfig 读供应商熔断配置；缺失或读取失败时取默认值（Node 同样降级到默认）。
func ReadProviderCircuitConfig(
	ctx context.Context,
	redisClient redis.UniversalClient,
	providerID int64,
) ProviderCircuitConfig {
	fallback := ProviderCircuitConfig{
		FailureThreshold:         DefaultFailureThreshold,
		OpenDurationMS:           DefaultOpenDurationMS,
		HalfOpenSuccessThreshold: DefaultHalfOpenSuccessThreshold,
		OpenDurationIncrementMS:  DefaultOpenDurationIncrementMS,
		MaxOpenCount:             DefaultMaxOpenCount,
	}
	if redisClient == nil {
		return fallback
	}
	raw, err := redisClient.HGetAll(ctx, ProviderConfigKeyPrefix+strconv.FormatInt(providerID, 10)).Result()
	if err != nil || len(raw) == 0 {
		return fallback
	}
	return ProviderCircuitConfig{
		FailureThreshold:         parseConfigInt(raw["failureThreshold"], fallback.FailureThreshold),
		OpenDurationMS:           parseConfigInt(raw["openDuration"], fallback.OpenDurationMS),
		HalfOpenSuccessThreshold: parseConfigInt(raw["halfOpenSuccessThreshold"], fallback.HalfOpenSuccessThreshold),
		OpenDurationIncrementMS:  nonNegativeConfigInt(raw["releaseIncrement"], fallback.OpenDurationIncrementMS),
		MaxOpenCount:             nonNegativeConfigInt(raw["maxOpenCount"], fallback.MaxOpenCount),
	}
}

// nonNegativeConfigInt 读一个非负配置项：空串取默认，负数按 0 处理。
//
// 负的递增时长会让「阶梯」反而缩短窗口（比不开阶梯更激进），负的次数上限则无意义；
// 两者都夹到 0——0 在语义上正是「不递增」，是安全的退化值。
func nonNegativeConfigInt(raw string, fallback int64) int64 {
	value := parseConfigInt(raw, fallback)
	if value < 0 {
		return 0
	}
	return value
}

// parseConfigInt 复刻 Node 的 parseInt(cached.x || "默认", 10)：空串取默认。
// 非数字也取默认：Node 的 parseInt 会给 NaN，随即被 isCircuitBreakerDisabled 当作关闭，
// 而「配置写坏」在 Go 侧落到默认阈值同样安全（默认阈值非正才会关闸，见 Disabled）。
func parseConfigInt(raw string, fallback int64) int64 {
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return value
}

// HealthReader 只读熔断状态。写入属终态链（W2c/W3），本包不实现。
type HealthReader struct {
	redis redis.UniversalClient
	// endpointCircuitBreakerEnabled 对应 ENABLE_ENDPOINT_CIRCUIT_BREAKER：
	// 关闭时端点级与 vendor-type 级一律视为未打开（Node 在同一开关上短路）。
	endpointCircuitBreakerEnabled bool
	// now 可注入时钟，测试用。
	now func() time.Time
}

// HealthOptions 是 HealthReader 的构造参数。
type HealthOptions struct {
	// Redis 为 nil 时所有判定一律「未打开」，并把原因记为 ExitNoRedis（fail-open，与 Node 一致）。
	Redis redis.UniversalClient
	// EndpointCircuitBreakerEnabled 取自 config.EnvConfig.EnableEndpointCircuitBreaker。
	EndpointCircuitBreakerEnabled bool
	// Now 可注入时钟。
	Now func() time.Time
}

// NewHealthReader 构造只读熔断状态读取器。
func NewHealthReader(opts HealthOptions) *HealthReader {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &HealthReader{
		redis:                         opts.Redis,
		endpointCircuitBreakerEnabled: opts.EndpointCircuitBreakerEnabled,
		now:                           now,
	}
}

// EffectiveProviderState 求「有效」熔断状态：raw 为 open 但已过 circuitOpenUntil 时判为 half-open。
//
// 存在的理由（防止判定分叉）：状态的**读取侧**（管理面页面/接口显示的熔断徽标）与**决策侧**
// （ProviderOpen 是否拦下候选）若各写一份过期判定，就会出现「页面说已熔断、请求却照常通过」。
// 生产实测到过这个分叉：id=156/145 的 Redis 里 circuitState=open 且 circuitOpenUntil 已过期，
// 选路按 half-open 放行，而管理面直接回原值 open。两侧一律调本函数，判定不可能再分叉。
//
// 语义（与 Node 的状态机一致）：
//   - closed / 其它未知值 / openUntil 缺失或非正 -> closed；
//   - open 且未过期 -> open；
//   - open 且已过期 -> half-open（Node 的 isCircuitOpen 在此处迁移，见其 persistStateToRedis 调用）；
//   - half-open -> half-open。
func EffectiveProviderState(current CircuitState, openUntilMS, nowMS int64) CircuitState {
	switch current {
	case StateOpen:
		if openUntilMS > 0 && nowMS > openUntilMS {
			return StateHalfOpen
		}
		return StateOpen
	case StateHalfOpen:
		return StateHalfOpen
	default:
		return StateClosed
	}
}

// ProviderOpen 复刻 isCircuitOpen：
// 1. 状态缺失或无 Redis -> false（fail-open）；
// 2. 熔断器被配置禁用（failureThreshold <= 0 或非法）-> false；
// 3. open 且已过 circuitOpenUntil -> 视为 half-open，放行（false）；
// 4. 仍在 open 窗口内 -> true；half-open -> false（放行试探）。
//
// 过期判定经 EffectiveProviderState（与读取侧同源），不在此另写一份。
func (h *HealthReader) ProviderOpen(ctx context.Context, providerID int64) (bool, ExitReason) {
	if h == nil || h.redis == nil {
		return false, ExitNoRedis
	}
	state, err := h.redis.HGetAll(ctx, ProviderStateKeyPrefix+strconv.FormatInt(providerID, 10)).Result()
	if err != nil {
		return false, ExitReadError
	}
	if len(state) == 0 {
		return false, ""
	}
	if !h.providerCircuitEnabled(ctx, providerID) {
		return false, ""
	}

	switch CircuitState(state["circuitState"]) {
	case StateOpen:
		effective := EffectiveProviderState(
			StateOpen, parseInt64(state["circuitOpenUntil"]), h.now().UnixMilli())
		if effective != StateOpen {
			// 已过窗口：按 half-open 放行试探。
			return false, ""
		}
		return true, ""
	default:
		return false, ""
	}
}

// ProviderState 返回用于落链记录的熔断状态快照（Node 的 getCircuitState 语义）。
//
// 三态如实回，**不得把 raw=half-open 折成 closed**：Node 的六处落链（provider-selector.ts:287/
// :406/:483/:709/:984/:1503）都调 getCircuitState，而它读的是**内存态**——选路前的 isCircuitOpen
// 已把该供应商从 Redis 水合进内存、并在窗口过期时迁成 half-open，故 Node 落链记的就是 half-open。
// Go 若折成 closed，同一家供应商的链记录会与 Node 相反，而 `circuitState` 正是「这家是不是正在
// 试探恢复（half-open，放行）」与「真被拦下（open，窗口内）」的唯一区分（见 types/message.ts:71）。
//
// 缺失/未知值与被禁用一律 closed（Node 的 getOrCreateHealth 默认值与 handleDisabledCircuitBreaker
// 的强制归闭）；窗口过期的 open 按 half-open（与 ProviderOpen 的放行判定同源，不另写一份）。
func (h *HealthReader) ProviderState(ctx context.Context, providerID int64) CircuitState {
	if h == nil || h.redis == nil {
		return StateClosed
	}
	state, err := h.redis.HGetAll(ctx, ProviderStateKeyPrefix+strconv.FormatInt(providerID, 10)).Result()
	if err != nil || len(state) == 0 {
		return StateClosed
	}
	if !h.providerCircuitEnabled(ctx, providerID) {
		return StateClosed
	}
	return EffectiveProviderState(
		CircuitState(state["circuitState"]),
		parseInt64(state["circuitOpenUntil"]),
		h.now().UnixMilli(),
	)
}

// providerCircuitEnabled 复刻 isCircuitBreakerDisabled 的取反：
// 配置缺失取默认值；failureThreshold 非正即视为熔断器关闭（Node 会强制归零为 closed）。
func (h *HealthReader) providerCircuitEnabled(ctx context.Context, providerID int64) bool {
	return !ReadProviderCircuitConfig(ctx, h.redis, providerID).Disabled()
}

// VendorTypeOpen 复刻 isVendorTypeCircuitOpen：
// 开关关闭 -> false；manualOpen -> true；open 且已过窗口 -> false（并视为归 closed）；否则 true。
func (h *HealthReader) VendorTypeOpen(
	ctx context.Context,
	vendorID int64,
	providerType convert.ProviderType,
) bool {
	if h == nil || h.redis == nil || !h.endpointCircuitBreakerEnabled {
		return false
	}
	key := VendorTypeStateKeyPrefix + strconv.FormatInt(vendorID, 10) + ":" + string(providerType)
	state, err := h.redis.HGetAll(ctx, key).Result()
	if err != nil || len(state) == 0 {
		return false
	}
	if state["manualOpen"] == "1" {
		return true
	}
	if CircuitState(state["circuitState"]) != StateOpen {
		return false
	}
	until := parseInt64(state["circuitOpenUntil"])
	if until > 0 && h.now().UnixMilli() > until {
		return false
	}
	return true
}

// EndpointOpen 复刻 isEndpointCircuitOpen：
// 开关关闭 -> false；closed -> false；open 且已过窗口 -> false；其余 open -> true；half-open -> false。
func (h *HealthReader) EndpointOpen(ctx context.Context, endpointID int64) bool {
	if h == nil || h.redis == nil || !h.endpointCircuitBreakerEnabled {
		return false
	}
	state, err := h.redis.HGetAll(ctx, EndpointStateKeyPrefix+strconv.FormatInt(endpointID, 10)).Result()
	if err != nil || len(state) == 0 {
		return false
	}
	if CircuitState(state["circuitState"]) != StateOpen {
		return false
	}
	until := parseInt64(state["circuitOpenUntil"])
	if until > 0 && h.now().UnixMilli() > until {
		return false
	}
	return true
}

// HealthSink 是熔断状态的写入面，供终态链实现（属 W2c/W3，不在本包）。
//
// 本包只定义接口：选路是纯读方，任何写入都会让「谁在改状态」变得不可归因。
type HealthSink interface {
	// RecordProviderSuccess 记录一次成功（可能触发 half-open 计数与归 closed）。
	RecordProviderSuccess(ctx context.Context, providerID int64) error
	// RecordProviderFailure 记录一次失败（可能触发开闸）。
	RecordProviderFailure(ctx context.Context, providerID int64, cause error) error
	// RecordEndpointFailure 记录端点失败。
	RecordEndpointFailure(ctx context.Context, endpointID int64, cause error) error
	// RecordVendorTypeAllEndpointsTimeout 记录厂级全端点超时。
	RecordVendorTypeAllEndpointsTimeout(
		ctx context.Context,
		vendorID int64,
		providerType convert.ProviderType,
		openDuration time.Duration,
	) error
}

// ErrHealthSinkUnavailable 由接线层在状态写面尚未就绪时返回。
var ErrHealthSinkUnavailable = errors.New("route: 熔断状态写入面未就绪")

func parseInt64(raw string) int64 {
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value
}
