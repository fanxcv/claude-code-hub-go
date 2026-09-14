package adminapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
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
			}
		}
		adminWriteJSON(writer, http.StatusOK, out)
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
