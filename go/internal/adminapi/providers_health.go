package adminapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// 本文件是 providers 资源的**运行态子面**：熔断健康总览与熔断复位（3 条端点）。
//
// 唯一真源：
//   - 路由：src/app/api/v1/resources/providers/router.ts:278(GET /providers/health)、
//     :325(POST /providers/{id}/circuit:reset)、:369(POST /providers/circuits:batchReset)
//   - handler：handlers.ts:223 getProvidersHealth / :238 resetProviderCircuit /
//     :270 resetProviderCircuitsBatch
//   - action：src/actions/providers.ts:1241 getProvidersHealthStatus / :1289 resetProviderCircuit /
//     :3080 batchResetProviderCircuits
//   - Redis 布局：src/lib/redis/circuit-breaker-state.ts:11-56 ——
//     hash `circuit_breaker:state:<providerId>`，字段 failureCount / lastFailureTime /
//     circuitState / circuitOpenUntil / halfOpenSuccessCount，TTL 86400s（:43）；
//     null 一律序列化成空串（serializeState:56-68），读回按 falsy 判空（:70-82）。
//
// 命名约定：本文件标识符带 providerCircuit 前缀，避免与同包其他 lane 撞符号。
// 哈希字段的解析复用 provider_circuit_admin.go 的 circuit* 辅助函数（同一套 Node 语义）。

// providerCircuitStateTTL 与 Node 的 STATE_TTL_SECONDS 一致（circuit-breaker-state.ts:43）。
const providerCircuitStateTTL = 86400 * time.Second

// providerCircuitSnapshot 是供应商级熔断状态的投影（键缺失即出厂闭态）。
type providerCircuitSnapshot struct {
	FailureCount         int64
	LastFailureTimeMS    *int64
	CircuitState         string
	CircuitOpenUntilMS   *int64
	HalfOpenSuccessCount int64
	// ConsecutiveOpenCount 是**等待阶梯**的级数 n（Go 侧增强字段，Node 无此键）。
	// 缺失（老实例写的哈希、或键不存在）读作 0——那正是「不开阶梯」的取值，
	// 故向前兼容是惰性的：不需要迁移，也不会把旧状态误报成爬过阶梯。
	ConsecutiveOpenCount int64
	// ConsecutiveOpenCountChangedAtMS 是级数**最近一次变化**的时刻；nil 表示从未变化
	// （写侧在首次开闸时把它写成空串，与闭态同形）。
	ConsecutiveOpenCountChangedAtMS *int64
}

// providerCircuitHealth 是 /providers/health 的响应值（actions/providers.ts:1249-1276）。
//
// 逐字段对齐：circuitState / failureCount / lastFailureTime / circuitOpenUntil / recoveryMinutes。
// recoveryMinutes 是「距恢复还有几分钟」（ceil），仅当 circuitOpenUntil 非空且非 0 时给出。
type providerCircuitHealth struct {
	CircuitState     string `json:"circuitState"`
	FailureCount     int64  `json:"failureCount"`
	LastFailureTime  *int64 `json:"lastFailureTime"`
	CircuitOpenUntil *int64 `json:"circuitOpenUntil"`
	RecoveryMinutes  *int64 `json:"recoveryMinutes"`
	// ConsecutiveOpenCount 是等待阶梯的级数 n：0 = 从未爬过阶梯（含阶梯未启用）。
	//
	// 它是**加性字段**：Node 与本服务此前的响应都没有它，前端按 0 处理即「不显示阶梯」。
	ConsecutiveOpenCount int64 `json:"consecutiveOpenCount"`
	// ConsecutiveOpenCountChangedAt 是级数最近一次变化的时刻；从未变化即 null。
	ConsecutiveOpenCountChangedAt *int64 `json:"consecutiveOpenCountChangedAt"`
	// OpenWindowMinutes 是**本次**开闸窗口的时长（分钟，向上取整）；求不出时 null。
	//
	// 它只在级数 n > 0 时给出（见 providerCircuitOpenWindowMinutes 的取值链与 nil 口径）：
	// 级数为 0 时窗口就是基础时长，与加阶梯之前一致，多报一个数字只会是噪声。
	OpenWindowMinutes *int64 `json:"openWindowMinutes"`
	// SlowRate 是本渠道的**低速降权**运行态投影（Go 侧增强字段，Node 无此键）。
	//
	// 加性字段：缺失即 null，前端按 null 处理即「不显示」。三种取值的区分是刻意的：
	//   - null：未装配低速读面（无 Redis），本维整段不显示；
	//   - available=false：读面在，但本次读不到；
	//   - available=true：读到了，Penalty 为 null 即「无降权」。
	SlowRate *providerSlowRateHealth `json:"slowRate"`
	// Concurrency 是本渠道的**实时并发数**投影（Go 侧增强字段，Node 无此键）。
	//
	// 加性字段，nil 即前端整段不显示。为何必须有 TrackingEnabled 而不能只给一个数字：
	// 统计开关关闭时并发数恒为 0，与「此刻真没有在飞请求」**同形**——只看数字无法区分，
	// 界面会把「没开统计」画成「空闲」（与 slowRate 的 available 同一条纪律）。
	Concurrency *providerConcurrencyHealth `json:"concurrency"`
}

// providerSlowRateHealth 是「低速降权」的展示投影。
//
// 纪律与 circuitLogsState 一致（provider_circuit_logs.go:43-46）：`Available=false` 时
// Penalty 一律为 nil，**不用 0 冒充**——0 与「无降权」同义，必须与「读不到」可区分。
//
// 聚合口径（本端点是 per-provider，低速状态却按「渠道 × 模型」存，一个渠道可有多个组合）：
//   - Penalty 取该渠道**所有**组合里已生效降权的**最大值**。
//     「生效」= penalty > 0 且基线 source 不是 extended_stale，与选路读侧
//     `route.SlowRateReader.Penalties` 逐条同口径（那边也只收这类组合）。
//     为何取最大而不是求和或平均：降权是**分层排序的位移量**，同一渠道的多个模型组合
//     在选路里各自竞争、互不叠加，取最大才等于「该渠道在最坏情况下被压下去多少」。
//   - ModelKey 是取到该最大值的那一个组合的模型键。
//   - Combinations 是有生效降权的组合数；> 1 时界面必须如实说「还有 N 个」，
//     否则管理员会以为只有一个模型慢。
//
// 为何不逐组合全给：本端点是总览，逐组合明细会把响应体积按「渠道 × 模型」放大，
// 而排障要的是「这家有没有被压、压了多少」。
type providerSlowRateHealth struct {
	Available bool `json:"available"`
	// Penalty 是降权量（生效最大值）；无降权或读不到即 null。
	Penalty *int `json:"penalty"`
	// ModelKey 是降权最重的那一个「渠道 × 模型」组合的模型键；无降权即 null。
	ModelKey *string `json:"modelKey"`
	// Combinations 是有生效降权的组合数（含 ModelKey 那一个）。
	Combinations int `json:"combinations"`
	// UnavailableReason 只在 Available=false 时给出（例如 redis_unavailable）。
	UnavailableReason *string `json:"unavailableReason"`
}

// providerConcurrencyHealth 是「实时并发数」的展示投影。
//
// 四态，每一态在前端都对应不同的渲染分支：
//   - TrackingEnabled=false：全局统计开关关着。**此时不给出 ActiveSessions**——统计根本没在跑。
//   - TrackingEnabled=true 且 Available=false：开着但本次读不到（Redis 不可用）；
//   - TrackingEnabled=true 且 Available=true：ActiveSessions 是真实读数，**0 也是有意义的读数**；
//   - 字段整体为 nil：未装配并发读面，本维整段不存在。
//
// 为什么 ActiveSessions 用指针：它在 Available=true 时可能合法地为 0，「0」必须与「不给」区分。
type providerConcurrencyHealth struct {
	// TrackingEnabled 是全局统计开关的**本次请求读数**（逐请求读，故管理面改完立即生效）。
	TrackingEnabled bool `json:"trackingEnabled"`
	// Available 为假表示开着统计但本次读不到；此时 ActiveSessions 为 null。
	Available bool `json:"available"`
	// ActiveSessions 是在飞请求数（每尝试计）；仅在 Available=true 时给出。
	ActiveSessions *int `json:"activeSessions"`
	// UnavailableReason 只在 Available=false 时给出。
	UnavailableReason *string `json:"unavailableReason"`
}

// providerSlowRateSnapshot 是单渠道的降权聚合读数（读侧内部形状，不是响应形状）。
type providerSlowRateSnapshot struct {
	Penalty      int
	ModelKey     string
	Combinations int
}

// ProviderSlowRateReader 读**渠道级**低速降权运行态（per-provider 聚合读数）。
//
// 为什么另立接口而不塞进 ProviderCircuitStore：那是熔断的语义与键族，两者只共用「同一次
// /providers/health 请求」这一件事，合在一起会让「谁在读哪族键」变模糊（同 provider_circuit_admin.go
// 把三族熔断分开声明的那条理由）。
//
// 候选取 `route.Provider` 而不是 `[]int64`：读侧据渠道行上的四个降权参数算滑窗下界与档位，
// 传完整候选才能让「参数改完立即生效」（见 route.SlowRateReader.Penalties 的参数优先级）。
// 只传 id 会让读侧回退到状态里记录的旧参数，管理面因此滞后到下一次慢样本。
//
// 返回的 map 只含**有生效降权的** id；无降权的 id 不在表内——「不在表内」与「读不到」
// 由调用方分开处理（后者是 error）。
type ProviderSlowRateReader interface {
	ProviderSlowRates(ctx context.Context, candidates []route.Provider) (map[int64]providerSlowRateSnapshot, error)
}

// redisProviderSlowRates 是 ProviderSlowRateReader 的 Redis 实现。
//
// 判定**不复刻**：生效与否（penalty > 0 且基线 source 非 extended_stale）一律问
// `route.SlowRateReader.Penalties`——那就是数据面选路调用的同一个方法。复刻一份判定
// 迟早在两处分叉（表现是「界面说降权了、选路没降」或反之），而这里的职责只是
// 「把按渠道×模型的读数聚合成 per-渠道」。
//
// 本实现只额外做一件事：扫出「哪些渠道 × 模型组合存在状态键」，因为 `Penalties` 要求
// 调用方已知模型名，而 health 端点是 per-provider 的。
type redisProviderSlowRates struct {
	client    redis.UniversalClient
	penalties SlowRatePenaltyReader
	logger    *logx.Logger
}

// NewRedisProviderSlowRates 装配读面；client 为 nil 时返回 nil（调用方据此让该维整段不显示）。
//
// penalties 取与数据面**同一个** `route.NewSlowRateReader(redisClient, logger)`。
func NewRedisProviderSlowRates(
	client redis.UniversalClient,
	penalties SlowRatePenaltyReader,
	logger *logx.Logger,
) ProviderSlowRateReader {
	if client == nil {
		return nil
	}
	if logger == nil {
		logger = logx.New(nil)
	}
	return &redisProviderSlowRates{client: client, penalties: penalties, logger: logger}
}

// slowRateStateKeyShape 反解状态键的固定前后缀（`cch:slow:` 与 `:state`）。
//
// 为何是反解而不是写字面量：键形制的唯一真源是 `route.SlowRateStateKey`（同包另有一条
// 镜像测试钉它与写侧逐字节一致）。在这里再写一遍字面量，等于把那份一致性又破一个口。
// 探针的形制与真实键同构，故截取结果稳定。
func slowRateStateKeyShape() (prefix, suffix string) {
	probe := route.SlowRateStateKey(0, "m")
	return probe[:strings.Index(probe, "{")], probe[strings.LastIndex(probe, "}")+1:]
}

// parseSlowRateStateKey 从状态键反解出（渠道 id、模型键）。
//
// 模型键里**可能含冒号**（如 `global:deepseek-v4.1-flash`），故只按**第一个**冒号切分。
func parseSlowRateStateKey(key, prefix, suffix string) (int64, string, bool) {
	inner := strings.TrimSuffix(strings.TrimPrefix(key, prefix), suffix)
	inner = strings.TrimSuffix(strings.TrimPrefix(inner, "{"), "}")
	rawID, modelKey, found := strings.Cut(inner, ":")
	if !found || modelKey == "" {
		return 0, "", false
	}
	providerID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || providerID <= 0 {
		return 0, "", false
	}
	return providerID, modelKey, true
}

// ProviderSlowRates 聚合出每个渠道的降权读数。
//
// 一次 SCAN（状态键族）+ 每个**不同模型键**一次 `Penalties`（各自内部把该模型的全部渠道
// 压进一次 pipeline）。
//
// SCAN 的成本边界（如实登记）：`cch:slow:*:state` 是全库扫描，耗时随**全库键数**而非
// `slow` 族键数增长——本仓生产实测全库 23668 键、slow 族个位数，一次 SCAN 约一次往返
// 加全库键的匹配开销；本端点由界面按 staleTime 30s 触发且**无** refetchInterval
// （provider-manager-loader.tsx:39-45），故量级可接受。
// 若将来 slow 族规模显著增长或本端点被高频轮询，正确的做法是写侧维护一个按渠道的汇总键
// （需改 `internal/slowrate`，不在本次改动范围内），而不是在这里加缓存。
func (r *redisProviderSlowRates) ProviderSlowRates(
	ctx context.Context,
	candidates []route.Provider,
) (map[int64]providerSlowRateSnapshot, error) {
	byID := make(map[int64]route.Provider, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID > 0 {
			byID[candidate.ID] = candidate
		}
	}
	wanted := make(map[int64]struct{}, len(byID))
	for id := range byID {
		wanted[id] = struct{}{}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	if r == nil || r.penalties == nil {
		return nil, errors.New("adminapi: 低速降权读面未装配")
	}

	// 模型键 -> 出现了该组合的（可见）渠道集合。
	byModel := make(map[string]map[int64]struct{})
	prefix, suffix := slowRateStateKeyShape()
	cursor := uint64(0)
	for {
		keys, next, err := r.client.Scan(ctx, cursor, prefix+"*"+suffix, 256).Result()
		if err != nil {
			return nil, fmt.Errorf("adminapi: 扫描低速状态键失败: %w", err)
		}
		for _, key := range keys {
			providerID, modelKey, ok := parseSlowRateStateKey(key, prefix, suffix)
			if !ok {
				continue
			}
			if _, visible := wanted[providerID]; !visible {
				continue
			}
			if byModel[modelKey] == nil {
				byModel[modelKey] = make(map[int64]struct{})
			}
			byModel[modelKey][providerID] = struct{}{}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	out := make(map[int64]providerSlowRateSnapshot, len(byModel))
	for modelKey, ids := range byModel {
		perModel := make([]route.Provider, 0, len(ids))
		for id := range ids {
			// 带上渠道行上的实时四参数：读侧据此算滑窗下界与档位，管理面因此与数据面
			// 同步热生效（只传 id 会让读侧回退状态里的旧参数）。
			//
			// 状态键只由已开启监控的渠道写出（写侧门），故候选一律按已开启报：
			// 否则 `Penalties` 的开关过滤会把真实存在降权的渠道滤掉。
			provider := byID[id]
			provider.SlowRateMonitorEnabled = true
			perModel = append(perModel, provider)
		}
		for id, penalty := range r.penalties.Penalties(ctx, perModel, modelKey) {
			current, exists := out[id]
			if !exists {
				out[id] = providerSlowRateSnapshot{Penalty: penalty, ModelKey: modelKey, Combinations: 1}
				continue
			}
			// 组合数无条件累加（本组合已生效降权）；penalty 与 ModelKey 只在更重时改写。
			current.Combinations++
			if penalty > current.Penalty {
				current.Penalty = penalty
				current.ModelKey = modelKey
			}
			out[id] = current
		}
	}
	return out, nil
}

// ProviderCircuitStore 读写**供应商级**熔断状态。
//
// 与 CircuitStateStore（端点级/厂级两族，provider_circuit_admin.go）分开声明：三族的键前缀、
// 字段与复位语义各不相同，合成一个接口只会让「谁在读哪族键」变模糊，也无从保证复位语义
// 不被张冠李戴。
type ProviderCircuitStore interface {
	// ProviderCircuit 读单个供应商级状态；键缺失时返回出厂闭态（不返回错误）。
	ProviderCircuit(ctx context.Context, providerID int64) (providerCircuitSnapshot, error)
	// ProviderCircuits 批量读供应商级状态：返回的 map **只含入参里的 id**（缺失即闭态）。
	ProviderCircuits(ctx context.Context, providerIDs []int64) (map[int64]providerCircuitSnapshot, error)
	// ResetProviderCircuit 把状态覆盖写成出厂闭态并续 TTL（Node 的 resetCircuit → persistStateToRedis）。
	ResetProviderCircuit(ctx context.Context, providerID int64) error
}

// providerCircuitKey 复刻 getStateKey（circuit-breaker-state.ts:47-49）。
func providerCircuitKey(providerID int64) string {
	return "circuit_breaker:state:" + strconv.FormatInt(providerID, 10)
}

// providerCircuitSnapshotFromRaw 复刻 deserializeState（circuit-breaker-state.ts:70-82）。
func providerCircuitSnapshotFromRaw(raw map[string]string) providerCircuitSnapshot {
	if len(raw) == 0 {
		return providerCircuitSnapshot{CircuitState: defaultProviderCircuitState()}
	}
	return providerCircuitSnapshot{
		FailureCount:         circuitIntOrZero(raw["failureCount"]),
		LastFailureTimeMS:    circuitIntOrNil(raw["lastFailureTime"]),
		CircuitState:         circuitStateOrDefault(raw["circuitState"]),
		CircuitOpenUntilMS:   circuitIntOrNil(raw["circuitOpenUntil"]),
		HalfOpenSuccessCount: circuitIntOrZero(raw["halfOpenSuccessCount"]),
		// 两个阶梯字段都是加性读取：键不在（老实例写的哈希）即 0/nil。
		ConsecutiveOpenCount:            circuitIntOrZero(raw["consecutiveOpenCount"]),
		ConsecutiveOpenCountChangedAtMS: circuitIntOrNil(raw["consecutiveOpenCountChangedAt"]),
	}
}

// defaultProviderCircuitState 是键缺失时的闭态取值（与端点级同字面）。
func defaultProviderCircuitState() string {
	return "closed"
}

// providerCircuitClosedRaw 是出厂闭态的 hash 序列化（serializeState:56-68）：
// null 写成空串——写方与读方必须同一套约定，否则复位后 UI 会读到 `null` 或缺字段。
//
// 两个**等待阶梯**字段也一并归零（它们不在 Node 的 serializeState 里，是 Go 侧增强）：
// 复位是 HSet 覆盖写而非删键，不写它们就会把上一轮的级数留在哈希里，于是复位后的
// 供应商会被读成「第 3 阶」而状态是 closed——一处自相矛盾的展示（写侧的 closeState
// 会归零这两项，但管理面的复位走的是本函数，两条路必须一致）。
func providerCircuitClosedRaw() map[string]string {
	return map[string]string{
		"failureCount":                  "0",
		"lastFailureTime":               "",
		"circuitState":                  defaultProviderCircuitState(),
		"circuitOpenUntil":              "",
		"halfOpenSuccessCount":          "0",
		"consecutiveOpenCount":          "0",
		"consecutiveOpenCountChangedAt": "",
	}
}

// ProviderCircuit 读单个供应商级状态。
func (s *redisCircuitStateStore) ProviderCircuit(
	ctx context.Context,
	providerID int64,
) (providerCircuitSnapshot, error) {
	raw, err := s.client.HGetAll(ctx, providerCircuitKey(providerID)).Result()
	if err != nil {
		return providerCircuitSnapshot{}, err
	}
	return providerCircuitSnapshotFromRaw(raw), nil
}

// ProviderCircuits 批量读供应商级状态。
//
// 去重后按 circuitBatchReadChunk 一块流水线发出（与端点级 EndpointCircuits 同口径）。
// 键缺失的 id 也放进结果、值为出厂闭态——调用方因此不必区分「没有键」与「闭态」，
// 与 Node 的 loadAllCircuitStates 缺省语义一致。
func (s *redisCircuitStateStore) ProviderCircuits(
	ctx context.Context,
	providerIDs []int64,
) (map[int64]providerCircuitSnapshot, error) {
	unique := providerCircuitDedupIDs(providerIDs)
	snapshots := make(map[int64]providerCircuitSnapshot, len(unique))
	for start := 0; start < len(unique); start += circuitBatchReadChunk {
		end := min(start+circuitBatchReadChunk, len(unique))
		chunk := unique[start:end]
		pipeline := s.client.Pipeline()
		commands := make([]*redis.MapStringStringCmd, len(chunk))
		for index, providerID := range chunk {
			commands[index] = pipeline.HGetAll(ctx, providerCircuitKey(providerID))
		}
		if _, err := pipeline.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return nil, err
		}
		for index, command := range commands {
			raw, err := command.Result()
			// 键不存在时 HGetAll 返回空 map 而非错误，所以这里只可能是真错。
			if err != nil && !errors.Is(err, redis.Nil) {
				return nil, err
			}
			snapshots[chunk[index]] = providerCircuitSnapshotFromRaw(raw)
		}
	}
	return snapshots, nil
}

// ResetProviderCircuit 把状态覆盖写成出厂闭态并续 TTL。
//
// 与端点级 ResetEndpointCircuit（Node 就是 DEL）**不同**：供应商级复位是覆盖写闭态
// （circuit-breaker.ts:889-908 的 resetCircuit 重置内存态后 persistStateToRedis），
// 因为 health 读的是这批字段，直接删键虽等价于闭态但会让 Node 侧的 halfOpenSuccessCount
// 等状态与键的存在性脱钩——照 Node 写，不自行发明。
func (s *redisCircuitStateStore) ResetProviderCircuit(ctx context.Context, providerID int64) error {
	key := providerCircuitKey(providerID)
	pipeline := s.client.Pipeline()
	pipeline.HSet(ctx, key, providerCircuitClosedRaw())
	pipeline.Expire(ctx, key, providerCircuitStateTTL)
	_, err := pipeline.Exec(ctx)
	return err
}

// providerCircuitDedupIDs 保序去重（非正数一律丢弃：providerId 的 schema 约束是正数）。
func providerCircuitDedupIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}

// providerCircuitStore 取供应商级熔断读写面。
//
// 返回 false 表示未接线（Deps 里没有熔断状态存储，或它不是供应商级实现）——调用方据此
// **不注册**这三条路由，让它们原样回退 Node：注册一个必然失败的路由，比不注册坏得多
// （本仓既定的降级语义）。
func providerCircuitStore(deps Deps) (ProviderCircuitStore, bool) {
	if deps.CircuitStates == nil {
		return nil, false
	}
	circuits, ok := deps.CircuitStates.(ProviderCircuitStore)
	return circuits, ok
}

// handleGetProvidersHealth 复刻 getProvidersHealth（handlers.ts:223-237 + actions:1241-1285）。
//
// 两段：①按可见性取 id ②逐 id 读熔断状态并算 recoveryMinutes。
// Node 侧 action 失败返回 `{}`（200）：本实现同样不把读失败翻成 5xx，只记日志——
// 健康面板读不到状态时应当显示「无数据」，而不是把整页打成错误。
func handleGetProvidersHealth(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		circuits, ok := providerCircuitStore(deps)
		if !ok {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("provider", "provider.unavailable", http.StatusServiceUnavailable,
					errors.New("熔断状态存储未接线")))
			return
		}
		providers, err := providerVisibleProviders(request, deps)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		ids := make([]int64, 0, len(providers))
		for _, provider := range providers {
			ids = append(ids, provider.ID)
		}

		snapshots, readErr := circuits.ProviderCircuits(request.Context(), ids)
		if readErr != nil {
			adminLoggerOf(deps).Warn("admin_providers_health_read_failed", map[string]any{
				"error": readErr.Error(),
			})
			snapshots = map[int64]providerCircuitSnapshot{}
		}

		nowMS := time.Now().UnixMilli()
		// 实时并发统计：开关是**逐请求读**的（不是构造期快照）。
		//
		// 为何不能快照：仓内已有教训——affinityIgnoreClientSessionId 曾是启动期快照，
		// 而管理面 PUT 返回 200 且广播失效，运维会以为已生效。这里直读 system_settings
		// 单行（与同包的 dashboard.go 的读法同例），代价是每请求一次单行查，
		// 换来「改完立即生效」这条确定性——本端点是被 5s 轮询的总览面，不是热路径。
		//
		// 读设置失败时按**关闭**处理（fail-closed 到「不统计」）：统计是可选增值面，
		// 读不到开关时宁可不报数，也不去 Redis 发一批可能无人要的计数命令。
		liveStatsEnabled := false
		if deps.Store != nil {
			if settings, settingsErr := deps.Store.FindSystemSettings(request.Context()); settingsErr == nil && settings != nil {
				liveStatsEnabled = settings.ProviderLiveStatsEnabled
			} else if settingsErr != nil {
				adminLoggerOf(deps).Warn("admin_providers_live_stats_switch_read_failed", map[string]any{
					"error": settingsErr.Error(),
				})
			}
		}
		// 关闭时**不查计数键**——这是「关上完全不占用资源」在服务侧的那一半
		var concurrencyCounts map[int64]int
		concurrencyReadFailed := false
		if liveStatsEnabled && deps.ObservedSessions != nil {
			concurrencyCounts, readErr = deps.ObservedSessions.ProviderSessionCounts(request.Context(), ids)
			if readErr != nil {
				adminLoggerOf(deps).Warn("admin_providers_concurrency_read_failed", map[string]any{
					"error": readErr.Error(),
				})
				concurrencyCounts = nil
				concurrencyReadFailed = true
			}
		}
		// 低速降权读数：未装配即 nil（整段不显示）；读失败也不打整页——降级为「读不到」，
		// 与熔断状态同一条纪律（读不到显示「无数据」，不是把页面打成错误）。
		var slowRates map[int64]providerSlowRateSnapshot
		slowRateReadFailed := false
		if deps.ProviderSlowRates != nil {
			slowRates, readErr = deps.ProviderSlowRates.ProviderSlowRates(request.Context(), slowRateCandidates(providers))
			if readErr != nil {
				adminLoggerOf(deps).Warn("admin_providers_slow_rate_read_failed", map[string]any{
					"error": readErr.Error(),
				})
				slowRates = nil
				slowRateReadFailed = true
			}
		}
		out := make(map[string]providerCircuitHealth, len(ids))
		for _, id := range ids {
			snapshot, exists := snapshots[id]
			if !exists {
				snapshot = providerCircuitSnapshotFromRaw(nil)
			}
			// 「已熔断」只在**窗口内**成立；窗口过期按 half-open（探测中）返回。
			//
			// 为何回有效态而不是 Redis 原值：数据面 ProviderOpen 就是按有效态放行的
			// （route.EffectiveProviderState），若这里回原值，页面会显示「已熔断」而请求照过。
			// 两侧共route.EffectiveProviderState 这一个判据，分叉不可能再发生。
			effective := route.EffectiveProviderState(
				route.CircuitState(snapshot.CircuitState),
				int64OrZero(snapshot.CircuitOpenUntilMS),
				nowMS,
			)
			out[strconv.FormatInt(id, 10)] = providerCircuitHealth{
				CircuitState:     string(effective),
				FailureCount:     snapshot.FailureCount,
				LastFailureTime:  snapshot.LastFailureTimeMS,
				CircuitOpenUntil: snapshot.CircuitOpenUntilMS,
				RecoveryMinutes:  providerCircuitRecoveryMinutes(snapshot.CircuitOpenUntilMS, nowMS, effective),
				// 阶梯读数走**原始**读数而不是有效态：它是「历史上爬了几级」的事实，
				// 不随窗口过期（open → 有效 half-open）而变。
				ConsecutiveOpenCount:          snapshot.ConsecutiveOpenCount,
				ConsecutiveOpenCountChangedAt: snapshot.ConsecutiveOpenCountChangedAtMS,
				OpenWindowMinutes: providerCircuitOpenWindowMinutes(
					snapshot.CircuitOpenUntilMS,
					snapshot.ConsecutiveOpenCountChangedAtMS,
					snapshot.ConsecutiveOpenCount,
				),
				SlowRate: providerSlowRateProjection(deps.ProviderSlowRates != nil, slowRateReadFailed, slowRates[id]),
				Concurrency: providerConcurrencyProjection(
					liveStatsEnabled,
					deps.ObservedSessions != nil,
					concurrencyReadFailed,
					concurrencyCounts[id],
				),
			}
		}
		adminWriteJSON(writer, http.StatusOK, out)
	}
}

// slowRateCandidates 把可见渠道行折成低速读面的候选。
//
// 只带 id 与四个降权参数：读侧据这四个参数算滑窗下界与档位（`Penalties`）。带实时值，
// 参数改完就立即生效；若只给 id，读侧会回退到状态里记录的旧参数，管理面就滞后到下一次慢样本
// （数据面不会，因为它传的是完整行）。
//
// 开关一律按已开启报：状态键只由已开启监控的渠道写出，按原值过滤会把真实存在降权的渠道滤掉
// （理由同 redisProviderSlowRates.ProviderSlowRates 的候选构造）。
func slowRateCandidates(providers []store.AdminProvider) []route.Provider {
	out := make([]route.Provider, 0, len(providers))
	for _, provider := range providers {
		out = append(out, route.Provider{
			ID:                     provider.ID,
			SlowRateMonitorEnabled: true,
			SlowRateWindowMinutes:  provider.SlowRateWindowMinutes,
			SlowRateTriggerCount:   provider.SlowRateTriggerCount,
			SlowRatePenaltyStep:    provider.SlowRatePenaltyStep,
			SlowRatePenaltyMax:     provider.SlowRatePenaltyMax,
		})
	}
	return out
}

// providerSlowRateProjection 把内部聚合读数转成响应投影。
//
// 三态（与 providerSlowRateHealth 的注释同一条纪律）：
//   - wired=false：未装配读面，返回 nil（键不存在，前端整段不显示）；
//   - readFailed=true：装配了但读不到，available=false + penalty=null（**不用 0 冒充**）；
//   - 否则：available=true，有降权则带读数，无降权则 penalty/modelKey 为 null。
func providerSlowRateProjection(
	wired bool,
	readFailed bool,
	snapshot providerSlowRateSnapshot,
) *providerSlowRateHealth {
	if !wired {
		return nil
	}
	if readFailed {
		reason := "redis_unavailable"
		return &providerSlowRateHealth{Available: false, UnavailableReason: &reason}
	}
	projection := &providerSlowRateHealth{Available: true, Combinations: snapshot.Combinations}
	if snapshot.Penalty > 0 {
		penalty := snapshot.Penalty
		modelKey := snapshot.ModelKey
		projection.Penalty = &penalty
		projection.ModelKey = &modelKey
	}
	return projection
}

// providerConcurrencyProjection 把实时并发读数转成响应投影。
//
// 四态见 providerConcurrencyHealth 的注释。这里的判据顺序很重要：
//  1. 开关关闭优先于一切——此时连读面都没跑（handler 里那段 if），故 activeSessions 必须是 nil。
//     若漏掉这一条，关闭状态下会输出 activeSessions=0，前端会把「没开统计」画成「空闲」。
//  2. 未装配读面（wired=false）：整段 nil。
//  3. 读失败：available=false + activeSessions=nil（**不用 0 冒充**，与 slowRate 同纪律）。
//  4. 正常：available=true，0 也是真实读数。
func providerConcurrencyProjection(
	trackingEnabled bool,
	wired bool,
	readFailed bool,
	activeSessions int,
) *providerConcurrencyHealth {
	if !trackingEnabled {
		return nil
	}
	if !wired {
		return nil
	}
	if readFailed {
		reason := "redis_unavailable"
		return &providerConcurrencyHealth{
			TrackingEnabled:   true,
			Available:         false,
			UnavailableReason: &reason,
		}
	}
	count := activeSessions
	return &providerConcurrencyHealth{
		TrackingEnabled: true,
		Available:       true,
		ActiveSessions:  &count,
	}
}

// providerCircuitRecoveryMinutes 复刻 actions/providers.ts:1268-1271：
// ceil((circuitOpenUntil - now) / 60000)；circuitOpenUntil 为 nil 即 null（Node 的三元判的是 truthy，
// 故 0 也算 null）。
//
// 有意收窄（与 Node 的原始三元不同）：仅当**有效态是 open**（即窗口真的还在走）时才给倒计时。
// 窗口已过期时返回 null：那一刻供应商已按 half-open 放行试探，再报「还有 N 分钟恢复」是假的
// （原始公式此时会算出**负数**，UI 会把它当“已过期但仍在倒计时”渲染）。
func providerCircuitRecoveryMinutes(openUntilMS *int64, nowMS int64, effective route.CircuitState) *int64 {
	if openUntilMS == nil || *openUntilMS == 0 || effective != route.StateOpen {
		return nil
	}
	minutes := (*openUntilMS - nowMS + 59999) / 60000
	return &minutes
}

// providerCircuitOpenWindowMinutes 求「本次开闸窗口」的时长（分钟，向上取整）；求不出即 nil。
//
// 取值链（**不重算阶梯公式**）：状态哈希里 `circuitOpenUntil` 是窗口结束时刻，
// `consecutiveOpenCountChangedAt` 是级数推进的**同一时刻**——写侧在同一次迁移里用同一个 nowMS
// 同时写这两项（health/writer.go 的开闸分支），故两者之差就是本次窗口时长。
// 在这里再算一遍 `base + increment × n` 会长出第二份实现（还得把供应商配置读进来），
// 与写侧迟早分叉——而写侧那份才是真正生效的值。
//
// 为何级数 > 0 才给：首次开闸的 changedAt 被写侧显式归零（与闭态同形），窗口此时就是基础时长、
// 与加阶梯之前逐字节一致，多显示一个数字只会是噪声；且这一条正好让「阶梯未启用」的供应商
// 自然安静（它们的级数恒为 0）。
//
// 何时返回 nil（都是「求不出」，不是 0，也不是负数）：
//   - 级数 <= 0：从未爬过阶梯（含阶梯未启用、已恢复）；
//   - changedAt 缺失（空串/老实例写的哈希）或 <= 0；
//   - 差值非正：手工改过 Redis、或时钟回拨——报 0 会让界面说「本次窗口 0 分钟」。
func providerCircuitOpenWindowMinutes(
	openUntilMS *int64,
	changedAtMS *int64,
	level int64,
) *int64 {
	if level <= 0 || openUntilMS == nil || changedAtMS == nil || *changedAtMS <= 0 {
		return nil
	}
	windowMS := *openUntilMS - *changedAtMS
	if windowMS <= 0 {
		return nil
	}
	minutes := (windowMS + int64(time.Minute/time.Millisecond) - 1) / int64(time.Minute/time.Millisecond)
	return &minutes
}

// int64OrZero 把可空指针解为值（nil 即 0，与 circuitIntOrNil 的空串约定一致）。
func int64OrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// handleResetProviderCircuit 复刻 resetProviderCircuit（handlers.ts:238-252 + actions:1289-1305）。
func handleResetProviderCircuit(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		circuits, ok := providerCircuitStore(deps)
		if !ok {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("provider", "provider.unavailable", http.StatusServiceUnavailable,
					errors.New("熔断状态存储未接线")))
			return
		}
		id, ok := providerIDParam(writer, request)
		if !ok {
			return
		}
		if _, err := providerFindVisible(request, deps, id); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, err)
			return
		}
		if err := circuits.ResetProviderCircuit(request.Context(), id); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		// Node 的 resetCircuit 还清了进程内配置缓存（circuit-breaker.ts:890 clearConfigCache）——
		// 那是 Node 的内存缓存；本进程的配置读缓存走 TTL 自过期，无对应物可清，故不广播失效。
		adminWriteJSON(writer, http.StatusOK, providerCircuitResetResponse{OK: true})
	}
}

// handleResetProviderCircuitsBatch 复刻 resetProviderCircuitsBatch
// （handlers.ts:270-285 + actions:3080-3121）。
//
// 前置：body `{providerIds}`（1..500），且**每个** id 都必须可见，否则整批 404
// （ensureVisibleProviderIds）。响应体是 action 的 data：`{resetCount}`。
func handleResetProviderCircuitsBatch(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		circuits, ok := providerCircuitStore(deps)
		if !ok {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("provider", "provider.unavailable", http.StatusServiceUnavailable,
					errors.New("熔断状态存储未接线")))
			return
		}
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, "providerIds")
		object.RejectUnknownKeys()
		ids, hasIDs := adminInt64Array(object, "providerIds", 1, 500)
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}
		if !hasIDs {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path: []any{"providerIds"}, Code: "invalid_type", Message: "Required",
			}})
			return
		}

		visible, err := providerVisibleSet(request, deps)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		for _, id := range ids {
			if _, exists := visible[id]; !exists {
				adminProblemWriter(deps).WriteActionError(writer, request, providerNotFoundError())
				return
			}
		}

		resetCount := 0
		for _, id := range providerCircuitDedupIDs(ids) {
			if err := circuits.ResetProviderCircuit(request.Context(), id); err != nil {
				adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
				return
			}
			resetCount++
		}
		adminWriteJSON(writer, http.StatusOK, providerCircuitBatchResetResponse{ResetCount: resetCount})
	}
}

// providerCircuitResetResponse 是 POST /providers/{id}/circuit:reset 的响应（handlers.ts:252）。
type providerCircuitResetResponse struct {
	OK bool `json:"ok"`
}

// providerCircuitBatchResetResponse 是 POST /providers/circuits:batchReset 的响应
// （actions/providers.ts:3111 的 data）。
type providerCircuitBatchResetResponse struct {
	ResetCount int `json:"resetCount"`
}
