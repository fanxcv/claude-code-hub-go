package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 provider-endpoints 资源里**两族熔断状态端点**的 Go 落点
// （Node 侧 src/app/api/v1/resources/provider-endpoints/{router,handlers}.ts 的六条 + actions/provider-endpoints.ts）：
//
//	GET  /provider-endpoints/{endpointId}/circuit              admin
//	POST /provider-endpoints/circuits:batch                    admin
//	POST /provider-endpoints/{endpointId}/circuit:reset        admin（204）
//	GET  /provider-vendors/{vendorId}/circuit                  admin
//	POST /provider-vendors/{vendorId}/circuit:setManualOpen    admin（204）
//	POST /provider-vendors/{vendorId}/circuit:reset            admin（204）
//
// **为什么单独一个文件与一个注册函数**：这六条要的是 **Redis 熔断状态**，而 provider_endpoints.go
// 的读面只要 PG。注册条件不同（无 Redis 时只该退这六条，不该把读面一起退掉），所以分开注册——
// 与 system/settings 之于 system 的分法同一条理由。
//
// **为什么读 Redis 而不读内存态**：Node 侧两族熔断器都是「内存态为主、Redis 为跨实例同步」
// （endpoint-circuit-breaker.ts:84-143、vendor-type-circuit-breaker.ts:52-99），管理面读到的
// 可能是本进程内存里那份。Go 数据面的熔断读**每次都直接读 Redis**（route.HealthReader 无内存态），
// 因此这里也直接读 Redis：两边读同一份真源，才不会出现「管理页说 closed、闸门却按 open 拒」。
//
// 三处与 Node 的登记差异：
//
//  1. **读失败不作静默降级**。Node 的内存态兜底在 Go 不成立（Go 没有那份内存态），把 Redis 故障
//     报成 "closed" 会让管理页显示「一切正常」而闸门实际在拒——按 500 如实作答。
//  2. **批量端点做可见性检查，逐 id 读库**（照 Node 的 ensureVisibleEndpointIds 顺序），
//     读状态用流水线分块（endpoint-circuit-breaker-state.ts:71 的 200/块）。
//  3. **手工开闸的落库失败如实作答**。Node 的 saveVendorTypeCircuitState 把异常吞成 warn，
//     于是 UI 收到 204 而状态没变；这里返回 500——「点了没反应」比「点了报错」难查。
//
// 另有一处刻意的**不**对齐：这六条不查 ENABLE_ENDPOINT_CIRCUIT_BREAKER。Node 的
// getEndpointHealthInfo / resetEndpointCircuit / 厂级三函数同样不查该开关（只有选路侧
// isEndpointCircuitOpen 查），若在写路径上加这道闸，开关关闭时管理员的重置会变成空操作。

const (
	// endpointCircuitFailureThreshold / endpointCircuitOpenDurationMS /
	// endpointCircuitHalfOpenSuccessThreshold 照 DEFAULT_ENDPOINT_CIRCUIT_BREAKER_CONFIG
	// （src/lib/endpoint-circuit-breaker.ts:19-23）。端点级熔断没有可配置项，读取即返回这三个数。
	endpointCircuitFailureThreshold         = 3
	endpointCircuitOpenDurationMS           = 300000
	endpointCircuitHalfOpenSuccessThreshold = 1
	// vendorTypeCircuitStateTTLSeconds 照 vendor-type-circuit-breaker-state.ts:33 的 STATE_TTL_SECONDS。
	vendorTypeCircuitStateTTLSeconds = 2592000
	// vendorTypeCircuitRetentionShrinkSeconds 照 proxy-runtime.ts:102 的 24 小时收缩上限。
	vendorTypeCircuitRetentionShrinkSeconds = 86400
	// circuitBatchReadChunk 是批量读流水线的分块大小（endpoint-circuit-breaker-state.ts:71）。
	circuitBatchReadChunk = 200
	// circuitBatchMaxEndpointIDs 照 BatchEndpointCircuitSchema 的 `.max(500)`。
	circuitBatchMaxEndpointIDs = 500
)

// EndpointCircuitSnapshot 是端点级熔断状态的 Redis 投影
// （endpoint-circuit-breaker-state.ts:8-14 的五个字段）。
type EndpointCircuitSnapshot struct {
	FailureCount         int64
	LastFailureTimeMS    *int64
	CircuitState         string
	CircuitOpenUntilMS   *int64
	HalfOpenSuccessCount int64
}

// VendorTypeCircuitSnapshot 是厂级（供应商类型）熔断状态的 Redis 投影
// （vendor-type-circuit-breaker-state.ts:11-16 的四个字段）。
type VendorTypeCircuitSnapshot struct {
	CircuitState       string
	CircuitOpenUntilMS *int64
	LastFailureTimeMS  *int64
	ManualOpen         bool
}

// CircuitStateStore 是管理面读写两族熔断状态的窄接口。
//
// 比 redis.UniversalClient 窄：本模块只用得上「读一个哈希、写一个哈希、删一个键」，
// 测试也就不必造一个完整客户端替身。
type CircuitStateStore interface {
	// EndpointCircuit 读单个端点级状态；键缺失时返回**出厂闭态**（不返回错误）。
	EndpointCircuit(ctx context.Context, endpointID int64) (EndpointCircuitSnapshot, error)
	// EndpointCircuits 批量读端点级状态（内部去重并按 200 分块流水线，照
	// endpoint-circuit-breaker-state.ts:71）。返回的 map 只含去重后的 id。
	EndpointCircuits(ctx context.Context, endpointIDs []int64) (map[int64]EndpointCircuitSnapshot, error)
	// ResetEndpointCircuit 删除端点状态键（Node 的 resetEndpointCircuit 就是 DEL）。
	ResetEndpointCircuit(ctx context.Context, endpointID int64) error
	// VendorTypeCircuit 读厂级状态；键缺失时返回出厂闭态。
	VendorTypeCircuit(ctx context.Context, vendorID int64, providerType string) (VendorTypeCircuitSnapshot, error)
	// SaveVendorTypeCircuit 覆盖厂级状态并续 TTL（高并发模式下收缩到 24 小时）。
	SaveVendorTypeCircuit(
		ctx context.Context,
		vendorID int64,
		providerType string,
		state VendorTypeCircuitSnapshot,
	) error
	// DeleteVendorTypeCircuit 删除厂级状态键。
	DeleteVendorTypeCircuit(ctx context.Context, vendorID int64, providerType string) error
}

// CircuitSettingsSource 提供系统设置快照，仅用于解析高并发模式下的 TTL 收缩。
//
// 与 health.SettingsSource 同形，另行声明以免管理面依赖数据面的熔断写入器。
type CircuitSettingsSource interface {
	FindSystemSettings(ctx context.Context) (*store.SystemSettings, error)
}

// redisCircuitStateStore 是 CircuitStateStore 的 Redis 实现。
type redisCircuitStateStore struct {
	client   redis.UniversalClient
	settings CircuitSettingsSource
	logger   *logx.Logger
}

// NewRedisCircuitStates 用命令连接装配熔断状态读写面；client 为 nil 时返回 nil
// （调用方据此不注册这六条路由）。
func NewRedisCircuitStates(
	client redis.UniversalClient,
	settings CircuitSettingsSource,
	logger *logx.Logger,
) CircuitStateStore {
	if client == nil {
		return nil
	}
	if logger == nil {
		logger = logx.New(nil)
	}
	return &redisCircuitStateStore{client: client, settings: settings, logger: logger}
}

// EndpointCircuit 读端点级状态。
func (s *redisCircuitStateStore) EndpointCircuit(
	ctx context.Context,
	endpointID int64,
) (EndpointCircuitSnapshot, error) {
	raw, err := s.client.HGetAll(ctx, endpointCircuitKey(endpointID)).Result()
	if err != nil {
		return EndpointCircuitSnapshot{}, err
	}
	return endpointCircuitSnapshotFromRaw(raw), nil
}

// EndpointCircuits 批量读端点级状态（去重 + 200/块流水线）。
func (s *redisCircuitStateStore) EndpointCircuits(
	ctx context.Context,
	endpointIDs []int64,
) (map[int64]EndpointCircuitSnapshot, error) {
	unique := dedupEndpointIDs(endpointIDs)
	snapshots := make(map[int64]EndpointCircuitSnapshot, len(unique))
	for start := 0; start < len(unique); start += circuitBatchReadChunk {
		end := min(start+circuitBatchReadChunk, len(unique))
		chunk := unique[start:end]
		pipeline := s.client.Pipeline()
		commands := make([]*redis.MapStringStringCmd, len(chunk))
		for index, endpointID := range chunk {
			commands[index] = pipeline.HGetAll(ctx, endpointCircuitKey(endpointID))
		}
		if _, err := pipeline.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return nil, err
		}
		for index, command := range commands {
			raw, err := command.Result()
			// 键不存在时 HGetAll 返回空 map 而非错误，所以这里只可能是真错（连接断、类型不对）。
			if err != nil && !errors.Is(err, redis.Nil) {
				return nil, err
			}
			snapshots[chunk[index]] = endpointCircuitSnapshotFromRaw(raw)
		}
	}
	return snapshots, nil
}

// endpointCircuitSnapshotFromRaw 把哈希投影成快照；键缺失（空 map）即出厂闭态。
func endpointCircuitSnapshotFromRaw(raw map[string]string) EndpointCircuitSnapshot {
	if len(raw) == 0 {
		return defaultEndpointCircuitSnapshot()
	}
	return EndpointCircuitSnapshot{
		FailureCount:         circuitIntOrZero(raw["failureCount"]),
		LastFailureTimeMS:    circuitIntOrNil(raw["lastFailureTime"]),
		CircuitState:         circuitStateOrDefault(raw["circuitState"]),
		CircuitOpenUntilMS:   circuitIntOrNil(raw["circuitOpenUntil"]),
		HalfOpenSuccessCount: circuitIntOrZero(raw["halfOpenSuccessCount"]),
	}
}

// dedupEndpointIDs 保序去重。
func dedupEndpointIDs(endpointIDs []int64) []int64 {
	unique := make([]int64, 0, len(endpointIDs))
	seen := make(map[int64]struct{}, len(endpointIDs))
	for _, endpointID := range endpointIDs {
		if _, exists := seen[endpointID]; exists {
			continue
		}
		seen[endpointID] = struct{}{}
		unique = append(unique, endpointID)
	}
	return unique
}

// ResetEndpointCircuit 删键（不查熔断开关，见文件头）。
func (s *redisCircuitStateStore) ResetEndpointCircuit(ctx context.Context, endpointID int64) error {
	return s.client.Del(ctx, endpointCircuitKey(endpointID)).Err()
}

// VendorTypeCircuit 读厂级状态。
func (s *redisCircuitStateStore) VendorTypeCircuit(
	ctx context.Context,
	vendorID int64,
	providerType string,
) (VendorTypeCircuitSnapshot, error) {
	raw, err := s.client.HGetAll(ctx, vendorTypeCircuitKey(vendorID, providerType)).Result()
	if err != nil {
		return VendorTypeCircuitSnapshot{}, err
	}
	if len(raw) == 0 {
		return VendorTypeCircuitSnapshot{CircuitState: string(route.StateClosed)}, nil
	}
	return VendorTypeCircuitSnapshot{
		CircuitState:       circuitStateOrDefault(raw["circuitState"]),
		CircuitOpenUntilMS: circuitIntOrNil(raw["circuitOpenUntil"]),
		LastFailureTimeMS:  circuitIntOrNil(raw["lastFailureTime"]),
		ManualOpen:         raw["manualOpen"] == "1",
	}, nil
}

// SaveVendorTypeCircuit 覆盖厂级状态并续 TTL。
//
// 与 Node 的 serializeState 逐字一致：四个字段全写（manualOpen 写 "0"/"1"，时间戳空值写空串）。
// TTL 用 getProxyRuntimeSettings 的收缩规则；**设置读不到时如实返回错误**（见文件头差异 3）。
func (s *redisCircuitStateStore) SaveVendorTypeCircuit(
	ctx context.Context,
	vendorID int64,
	providerType string,
	state VendorTypeCircuitSnapshot,
) error {
	ttl, err := s.vendorTypeTTLSeconds(ctx)
	if err != nil {
		return err
	}
	manualOpen := "0"
	if state.ManualOpen {
		manualOpen = "1"
	}
	key := vendorTypeCircuitKey(vendorID, providerType)
	if err := s.client.HSet(ctx, key, map[string]any{
		"circuitState":     state.CircuitState,
		"circuitOpenUntil": circuitMillisField(state.CircuitOpenUntilMS),
		"lastFailureTime":  circuitMillisField(state.LastFailureTimeMS),
		"manualOpen":       manualOpen,
	}).Err(); err != nil {
		return err
	}
	return s.client.Expire(ctx, key, time.Duration(ttl)*time.Second).Err()
}

// DeleteVendorTypeCircuit 删键。
func (s *redisCircuitStateStore) DeleteVendorTypeCircuit(
	ctx context.Context,
	vendorID int64,
	providerType string,
) error {
	return s.client.Del(ctx, vendorTypeCircuitKey(vendorID, providerType)).Err()
}

// vendorTypeTTLSeconds 解析厂级 TTL：高并发模式下收缩到 24 小时（proxy-runtime.ts:101）。
func (s *redisCircuitStateStore) vendorTypeTTLSeconds(ctx context.Context) (int, error) {
	if s.settings == nil {
		return vendorTypeCircuitStateTTLSeconds, nil
	}
	settings, err := s.settings.FindSystemSettings(ctx)
	if err != nil {
		return 0, err
	}
	if settings == nil || !settings.EnableHighConcurrencyMode {
		return vendorTypeCircuitStateTTLSeconds, nil
	}
	// ponytail: 高并发模式恒收缩到 24 小时（Node 的 min(默认, 24h) 在默认 30 天时必取后者）。
	if vendorTypeCircuitStateTTLSeconds > vendorTypeCircuitRetentionShrinkSeconds {
		return vendorTypeCircuitRetentionShrinkSeconds, nil
	}
	return vendorTypeCircuitStateTTLSeconds, nil
}

// endpointCircuitKey 用 route 包的键前缀，避免同一键名在库里有两处字面量。
func endpointCircuitKey(endpointID int64) string {
	return route.EndpointStateKeyPrefix + strconv.FormatInt(endpointID, 10)
}

// vendorTypeCircuitKey 同上（厂级键：前缀 + vendorId + ":" + providerType）。
func vendorTypeCircuitKey(vendorID int64, providerType string) string {
	return route.VendorTypeStateKeyPrefix + strconv.FormatInt(vendorID, 10) + ":" + providerType
}

// defaultEndpointCircuitSnapshot 是键缺失时的出厂闭态（endpoint-circuit-breaker-state.ts:17-23）。
func defaultEndpointCircuitSnapshot() EndpointCircuitSnapshot {
	return EndpointCircuitSnapshot{CircuitState: string(route.StateClosed)}
}

// circuitStateOrDefault 照 Node 的 `(data.circuitState) || "closed"`。
func circuitStateOrDefault(raw string) string {
	if raw == "" {
		return string(route.StateClosed)
	}
	return raw
}

// circuitIntOrNil 把 Redis 里的十进制时间戳读成 *int64；空串或不可解析即 nil（照 Node 的三元）。
func circuitIntOrNil(raw string) *int64 {
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &value
}

// circuitIntOrZero 照 Node 的 parseInt(raw || "0", 10)。
func circuitIntOrZero(raw string) int64 {
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// circuitMillisField 把可空时间戳写成哈希字段值（空串表示 null，与 Node 的序列化一致）。
func circuitMillisField(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}

// providerCircuitAPI 是两族熔断端点的处理器依赖。
type providerCircuitAPI struct {
	pools  *store.Pools
	states CircuitStateStore
	depts  ProblemWriter
	logger *logx.Logger
	now    func() time.Time
}

// 端点级熔断的响应体（actions/provider-endpoints.ts:879-895 的 ActionResult 载荷）。
type endpointCircuitResponse struct {
	EndpointID int64                    `json:"endpointId"`
	Health     endpointCircuitHealth    `json:"health"`
	Config     endpointCircuitConfigOut `json:"config"`
}

type endpointCircuitHealth struct {
	FailureCount         int64  `json:"failureCount"`
	LastFailureTime      *int64 `json:"lastFailureTime"`
	CircuitState         string `json:"circuitState"`
	CircuitOpenUntil     *int64 `json:"circuitOpenUntil"`
	HalfOpenSuccessCount int64  `json:"halfOpenSuccessCount"`
}

type endpointCircuitConfigOut struct {
	FailureThreshold         int64 `json:"failureThreshold"`
	OpenDuration             int64 `json:"openDuration"`
	HalfOpenSuccessThreshold int64 `json:"halfOpenSuccessThreshold"`
}

// endpointCircuitBatchItem 是批量读的单项（actions/provider-endpoints.ts:939-944）。
type endpointCircuitBatchItem struct {
	EndpointID       int64  `json:"endpointId"`
	CircuitState     string `json:"circuitState"`
	FailureCount     int64  `json:"failureCount"`
	CircuitOpenUntil *int64 `json:"circuitOpenUntil"`
}

// vendorCircuitResponse 是厂级读的响应体（actions/provider-endpoints.ts:1016-1023）。
type vendorCircuitResponse struct {
	VendorID         int64  `json:"vendorId"`
	ProviderType     string `json:"providerType"`
	CircuitState     string `json:"circuitState"`
	CircuitOpenUntil *int64 `json:"circuitOpenUntil"`
	LastFailureTime  *int64 `json:"lastFailureTime"`
	ManualOpen       bool   `json:"manualOpen"`
}

// RegisterProviderCircuitRoutes 注册两族熔断端点。
//
// Store 或 CircuitStates 任一未装配即整组不注册（回退 Node）：可见性检查要 PG，读写要 Redis，
// 缺任一侧都只能答半张表。
func RegisterProviderCircuitRoutes(router *Router, deps Deps) {
	if deps.Store == nil || deps.CircuitStates == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_provider_circuit_unwired", map[string]any{
				"module": "provider_endpoint",
				"action": "routes_not_registered",
				"store":  deps.Store != nil,
				"redis":  deps.CircuitStates != nil,
			})
		}
		return
	}
	api := &providerCircuitAPI{
		pools:  deps.Store,
		states: deps.CircuitStates,
		depts:  adminProblemWriter(deps),
		logger: adminLoggerOf(deps),
		now:    time.Now,
	}

	entries := []struct {
		method      string
		path        string
		operationID string
		handler     http.HandlerFunc
	}{
		{http.MethodGet, "/provider-endpoints/{endpointId}/circuit", "getEndpointCircuit",
			api.handleEndpointCircuit},
		{http.MethodPost, "/provider-endpoints/circuits:batch", "batchGetEndpointCircuits",
			api.handleBatchEndpointCircuits},
		{http.MethodPost, "/provider-endpoints/{endpointId}/circuit:reset", "resetEndpointCircuit",
			api.handleResetEndpointCircuit},
		{http.MethodGet, "/provider-vendors/{vendorId}/circuit", "getVendorCircuit",
			api.handleVendorCircuit},
		{http.MethodPost, "/provider-vendors/{vendorId}/circuit:setManualOpen",
			"setVendorCircuitManualOpen", api.handleSetVendorCircuitManualOpen},
		{http.MethodPost, "/provider-vendors/{vendorId}/circuit:reset", "resetVendorCircuit",
			api.handleResetVendorCircuit},
	}
	for _, entry := range entries {
		router.Add(Route{
			Method:      entry.method,
			Path:        entry.path,
			Access:      AccessAdmin,
			Module:      "provider_endpoint",
			OperationID: entry.operationID,
			Handler:     entry.handler,
		})
	}
}

// handleEndpointCircuit 复刻 getEndpointCircuit：端点不可见（不存在或隐藏类型）即 404。
func (api *providerCircuitAPI) handleEndpointCircuit(writer http.ResponseWriter, request *http.Request) {
	endpointID, ok := providerPathID(writer, request, "endpointId")
	if !ok {
		return
	}
	if !api.ensureVisibleEndpoint(writer, request, endpointID) {
		return
	}
	snapshot, err := api.states.EndpointCircuit(request.Context(), endpointID)
	if err != nil {
		api.writeCircuitFailure(writer, request, err)
		return
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, endpointCircuitResponse{
		EndpointID: endpointID,
		Health: endpointCircuitHealth{
			FailureCount:         snapshot.FailureCount,
			LastFailureTime:      snapshot.LastFailureTimeMS,
			CircuitState:         snapshot.CircuitState,
			CircuitOpenUntil:     snapshot.CircuitOpenUntilMS,
			HalfOpenSuccessCount: snapshot.HalfOpenSuccessCount,
		},
		Config: endpointCircuitConfigOut{
			FailureThreshold:         endpointCircuitFailureThreshold,
			OpenDuration:             endpointCircuitOpenDurationMS,
			HalfOpenSuccessThreshold: endpointCircuitHalfOpenSuccessThreshold,
		},
	})
}

// handleBatchEndpointCircuits 复刻 batchGetEndpointCircuits。
//
// 顺序纪律：先按**原始列表**逐个做可见性检查（Node 的 ensureVisibleEndpointIds 会在第一个不可见
// 端点处 404），再对**去重后**的 id 读状态，最后按原始列表（含重复项）逐个铺结果。
func (api *providerCircuitAPI) handleBatchEndpointCircuits(
	writer http.ResponseWriter,
	request *http.Request,
) {
	endpointIDs, ok := api.decodeEndpointIDs(writer, request)
	if !ok {
		return
	}
	for _, endpointID := range endpointIDs {
		if !api.ensureVisibleEndpoint(writer, request, endpointID) {
			return
		}
	}
	result := make([]endpointCircuitBatchItem, 0, len(endpointIDs))
	if len(endpointIDs) == 0 {
		writeShellJSONNoEnvelope(writer, http.StatusOK, result)
		return
	}

	snapshots, err := api.states.EndpointCircuits(request.Context(), endpointIDs)
	if err != nil {
		api.writeCircuitFailure(writer, request, err)
		return
	}
	for _, endpointID := range endpointIDs {
		snapshot, found := snapshots[endpointID]
		if !found {
			snapshot = defaultEndpointCircuitSnapshot()
		}
		result = append(result, endpointCircuitBatchItem{
			EndpointID:       endpointID,
			CircuitState:     snapshot.CircuitState,
			FailureCount:     snapshot.FailureCount,
			CircuitOpenUntil: snapshot.CircuitOpenUntilMS,
		})
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, result)
}

// handleResetEndpointCircuit 复刻 resetEndpointCircuit：可见性检查后 DEL，204。
func (api *providerCircuitAPI) handleResetEndpointCircuit(
	writer http.ResponseWriter,
	request *http.Request,
) {
	endpointID, ok := providerPathID(writer, request, "endpointId")
	if !ok {
		return
	}
	if !api.ensureVisibleEndpoint(writer, request, endpointID) {
		return
	}
	if err := api.states.ResetEndpointCircuit(request.Context(), endpointID); err != nil {
		api.writeCircuitFailure(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// handleVendorCircuit 复刻 getVendorCircuit。
//
// 不查厂是否在库：Node 的 getVendorTypeCircuitInfo 也不查（键缺失即出厂闭态），
// 多一次查库会把这六条里唯一不碰 PG 的读变成碰库的读，换来一个 Node 没有的 404。
func (api *providerCircuitAPI) handleVendorCircuit(writer http.ResponseWriter, request *http.Request) {
	vendorID, ok := providerPathID(writer, request, "vendorId")
	if !ok {
		return
	}
	providerType, ok := api.providerTypeQuery(writer, request)
	if !ok {
		return
	}
	snapshot, err := api.states.VendorTypeCircuit(request.Context(), vendorID, providerType)
	if err != nil {
		api.writeCircuitFailure(writer, request, err)
		return
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, vendorCircuitResponse{
		VendorID:         vendorID,
		ProviderType:     providerType,
		CircuitState:     snapshot.CircuitState,
		CircuitOpenUntil: snapshot.CircuitOpenUntilMS,
		LastFailureTime:  snapshot.LastFailureTimeMS,
		ManualOpen:       snapshot.ManualOpen,
	})
}

// handleSetVendorCircuitManualOpen 复刻 setVendorCircuitManualOpen（204）。
//
// 语义照 Node vendor-type-circuit-breaker.ts:168-188：手工开闸时状态置 open、清开闸窗口、
// 记一次当前时间的 lastFailureTime；手工闭合时四字段全清。
func (api *providerCircuitAPI) handleSetVendorCircuitManualOpen(
	writer http.ResponseWriter,
	request *http.Request,
) {
	vendorID, ok := providerPathID(writer, request, "vendorId")
	if !ok {
		return
	}
	var body struct {
		ProviderType *string `json:"providerType"`
		ManualOpen   *bool   `json:"manualOpen"`
	}
	if !api.decodeStrictBody(writer, request, &body) {
		return
	}
	issues := make([]InvalidParam, 0, 2)
	if body.ProviderType == nil || !api.validProviderType(request, *body.ProviderType) {
		issues = append(issues, InvalidParam{
			Path:    []any{"providerType"},
			Code:    invalidEnumCode(body.ProviderType == nil),
			Message: "Invalid enum value",
		})
	}
	if body.ManualOpen == nil {
		issues = append(issues, InvalidParam{
			Path:    []any{"manualOpen"},
			Code:    "invalid_type",
			Message: "Invalid input: expected boolean",
		})
	}
	if len(issues) > 0 {
		api.depts.WriteValidationError(writer, request, issues)
		return
	}

	state := VendorTypeCircuitSnapshot{
		CircuitState: string(route.StateClosed),
		ManualOpen:   *body.ManualOpen,
	}
	if *body.ManualOpen {
		nowMS := api.now().UnixMilli()
		state.CircuitState = string(route.StateOpen)
		state.LastFailureTimeMS = &nowMS
	}
	if err := api.states.SaveVendorTypeCircuit(
		request.Context(), vendorID, *body.ProviderType, state,
	); err != nil {
		api.writeCircuitFailure(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// handleResetVendorCircuit 复刻 resetVendorCircuit（204）：DEL 状态键。
//
// 不先读状态：Node 的 reset 只清内存态与 Redis 键，与管理面读到的值无关。
func (api *providerCircuitAPI) handleResetVendorCircuit(
	writer http.ResponseWriter,
	request *http.Request,
) {
	vendorID, ok := providerPathID(writer, request, "vendorId")
	if !ok {
		return
	}
	var body struct {
		ProviderType *string `json:"providerType"`
	}
	if !api.decodeStrictBody(writer, request, &body) {
		return
	}
	if body.ProviderType == nil || !api.validProviderType(request, *body.ProviderType) {
		api.depts.WriteValidationError(writer, request, []InvalidParam{{
			Path:    []any{"providerType"},
			Code:    invalidEnumCode(body.ProviderType == nil),
			Message: "Invalid enum value",
		}})
		return
	}
	if err := api.states.DeleteVendorTypeCircuit(
		request.Context(), vendorID, *body.ProviderType,
	); err != nil {
		api.writeCircuitFailure(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// ensureVisibleEndpoint 复刻 ensureVisibleEndpoint：不存在或隐藏类型即 404。
//
// 已作答时返回 false（调用方直接返回）。
func (api *providerCircuitAPI) ensureVisibleEndpoint(
	writer http.ResponseWriter,
	request *http.Request,
	endpointID int64,
) bool {
	endpoint, err := api.pools.AdminGetProviderEndpointByID(request.Context(), endpointID)
	if errors.Is(err, store.ErrNotFound) {
		api.depts.WriteProblem(writer, request, http.StatusNotFound, "provider_endpoint.not_found",
			"Provider endpoint was not found.")
		return false
	}
	if err != nil {
		api.writeCircuitFailure(writer, request, err)
		return false
	}
	if !dashboardCompatRequest(request) && hiddenProviderType(endpoint.ProviderType) {
		api.depts.WriteProblem(writer, request, http.StatusNotFound, "provider_endpoint.not_found",
			"Provider endpoint was not found.")
		return false
	}
	return true
}

// decodeEndpointIDs 解并校验 `{"endpointIds": [...]}`（BatchEndpointCircuitSchema，strict）。
func (api *providerCircuitAPI) decodeEndpointIDs(
	writer http.ResponseWriter,
	request *http.Request,
) ([]int64, bool) {
	var body struct {
		EndpointIDs *[]json.Number `json:"endpointIds"`
	}
	if !api.decodeStrictBody(writer, request, &body) {
		return nil, false
	}
	if body.EndpointIDs == nil {
		api.depts.WriteValidationError(writer, request, []InvalidParam{{
			Path:    []any{"endpointIds"},
			Code:    "invalid_type",
			Message: "Invalid input: expected array",
		}})
		return nil, false
	}
	raw := *body.EndpointIDs
	if len(raw) > circuitBatchMaxEndpointIDs {
		api.depts.WriteValidationError(writer, request, []InvalidParam{{
			Path:    []any{"endpointIds"},
			Code:    "too_big",
			Message: "Array must contain at most 500 element(s)",
		}})
		return nil, false
	}
	ids := make([]int64, 0, len(raw))
	for index, item := range raw {
		value, err := strconv.ParseInt(item.String(), 10, 64)
		if err != nil || value <= 0 {
			api.depts.WriteValidationError(writer, request, []InvalidParam{{
				Path:    []any{"endpointIds", index},
				Code:    "invalid_type",
				Message: "Invalid input: expected positive integer",
			}})
			return nil, false
		}
		ids = append(ids, value)
	}
	return ids, true
}

// providerTypeQuery 解并校验厂级熔断的 `?providerType=`（公开四值，dashboard 口径另加两个隐藏值）。
func (api *providerCircuitAPI) providerTypeQuery(
	writer http.ResponseWriter,
	request *http.Request,
) (string, bool) {
	raw := strings.TrimSpace(request.URL.Query().Get("providerType"))
	if raw == "" || !api.validProviderType(request, raw) {
		api.depts.WriteValidationError(writer, request, []InvalidParam{{
			Path:    []any{"providerType"},
			Code:    invalidEnumCode(raw == ""),
			Message: "Invalid enum value",
		}})
		return "", false
	}
	return raw, true
}

// validProviderType 复刻两套枚举：公开面只认 PUBLIC_PROVIDER_TYPE_VALUES，
// dashboard 口径（兼容头 + 管理员）另认 claude-auth / gemini-cli。
func (api *providerCircuitAPI) validProviderType(request *http.Request, value string) bool {
	switch value {
	case "claude", "codex", "gemini", "openai-compatible":
		return true
	case providerTypeClaudeAuth, providerTypeGeminiCLI:
		return dashboardCompatRequest(request)
	default:
		return false
	}
}

// decodeStrictBody 解 JSON 正文并复刻 zod 的 .strict()：多传字段在 Node 是 400。
func (api *providerCircuitAPI) decodeStrictBody(
	writer http.ResponseWriter,
	request *http.Request,
	target any,
) bool {
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		code := "invalid_type"
		if strings.Contains(err.Error(), "unknown field") {
			code = "unrecognized_keys"
		}
		api.depts.WriteValidationError(writer, request, []InvalidParam{{
			Path:    []any{},
			Code:    code,
			Message: "Invalid input",
		}})
		return false
	}
	return true
}

// writeCircuitFailure 把 Redis 故障按 500 作答并记日志（见文件头差异 1）。
func (api *providerCircuitAPI) writeCircuitFailure(
	writer http.ResponseWriter,
	request *http.Request,
	err error,
) {
	api.logger.Error("admin_provider_circuit_failed", map[string]any{
		"path":  request.URL.Path,
		"error": err.Error(),
	})
	api.depts.WriteProblem(writer, request, http.StatusInternalServerError, "", "")
}

// invalidEnumCode 区分「字段缺失」与「取值不在枚举内」：zod 分别给 invalid_type 与
// invalid_enum_value（文案不逐字对齐 zod，理由同 users_schema.go 的文件头）。
func invalidEnumCode(missing bool) string {
	if missing {
		return "invalid_type"
	}
	return "invalid_enum_value"
}
