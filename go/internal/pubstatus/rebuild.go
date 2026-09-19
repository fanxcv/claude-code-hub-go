package pubstatus

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是投影写侧的第三段：**重建 worker**——把 rollup 桶折算成一代投影并发布。
//
// Node 出处：`src/lib/public-status/rebuild-worker.ts`（全文件）。调用入口是它的调度器
// （`scheduler.ts:207` 的 `startPublicStatusRebuildScheduler`，由 `instrumentation.ts:749` 启动）。
//
// 「代」（generation）是这个模块的核心概念：一代 = (配置版本, 展示粒度, 窗口) 的组合指纹。
// 同一代重复重建会得到同一个键，故重建是**幂等**的；跨代并存时旧代仍可服务
// （manifest 里有 `lastCompleteGeneration`，读侧据此优先服务历史快照）。

const (
	// RebuildLockTTLMs 是分布式重建锁的存活时长（rebuild-worker.ts:44）。
	RebuildLockTTLMs = 60_000
	// TempProjectionTTLSeconds 是临时投影键的存活时长（rebuild-worker.ts:45）。
	TempProjectionTTLSeconds = 300
	// GenerationProjectionTTLSeconds 是正式投影的基准存活时长（rebuild-worker.ts:46，30 天）。
	GenerationProjectionTTLSeconds = 60 * 60 * 24 * 30
)

// RetentionTTLResolver 复刻 resolveRedisRetentionTtlSeconds（proxy-runtime.ts:101-103）。
//
// 高并发模式开启时把投影保留期压到最多 24 小时（该模式下 Redis 内存优先）。
// 取值来自系统设置缓存，故由装配方注入而不是本包直接读设置。
type RetentionTTLResolver func(defaultTTLSeconds int) int

// UnlimitedRetentionTTL 是「高并发模式关闭」时的口径：原样返回。
// 生产装配必须注入设置驱动的实现（默认值会让高并发模式下的保留期被放长，属有意可见的降级）。
func UnlimitedRetentionTTL(defaultTTLSeconds int) int { return defaultTTLSeconds }

// HighConcurrencyRetentionTTL 复刻 proxy-runtime.ts:101-103 的行为本身，供装配方在知道
// 高并发模式状态时使用（enabled 为真则压到 24 小时）。
func HighConcurrencyRetentionTTL(defaultTTLSeconds int, highConcurrencyMode bool) int {
	if highConcurrencyMode && defaultTTLSeconds > 60*60*24 {
		return 60 * 60 * 24
	}
	return defaultTTLSeconds
}

// ProjectionRedis 是投影重建需要的 Redis 子集。
//
// 内嵌读侧的 PublicStatusStore：worker 与读路径读同一批键、用同一套 encodeKeyPart/键构造器，
// 嵌进来可以让装配方只造一个适配器（两个接口分别适配会在「同一键两处构造」上埋漂移）。
type ProjectionRedis interface {
	PublicStatusStore
	// Del 删除键（临时投影键、重建提示、锁）。
	Del(ctx context.Context, keys ...string) error
	// AcquireLock 以 SET NX PX 获取分布式锁；返回 false 表示已被他人持有。
	AcquireLock(ctx context.Context, key string, value string, ttlMs int) (bool, error)
	// ReleaseLock 以「值相等才删」的方式释放锁（复刻 Node 的 Lua 脚本，避免误删他人的锁）。
	ReleaseLock(ctx context.Context, key string, value string) error
	// HGetAll 读一个 rollup 桶的全部字段。
	HGetAll(ctx context.Context, key string) (map[string]string, error)
	// Scan 按模式扫描键（重建提示用）。limit 为上限。
	Scan(ctx context.Context, pattern string, limit int) ([]string, error)
}

// NewRedisProjectionRedis 把 go-redis 客户端包成 ProjectionRedis；client 为 nil 时返回 nil。
func NewRedisProjectionRedis(client redis.UniversalClient) ProjectionRedis {
	if client == nil {
		return nil
	}
	return &redisProjectionRedis{client: client}
}

type redisProjectionRedis struct {
	client redis.UniversalClient
}

func (r *redisProjectionRedis) Ready(ctx context.Context) bool {
	pingCtx, cancel := context.WithTimeout(ctx, statusStorePingTimeout)
	defer cancel()
	return r.client.Ping(pingCtx).Err() == nil
}

func (r *redisProjectionRedis) Get(ctx context.Context, key string) (string, bool) {
	value, err := r.client.Get(ctx, key).Result()
	if err != nil {
		return "", false
	}
	return value, true
}

func (r *redisProjectionRedis) PTTL(ctx context.Context, key string) (time.Duration, error) {
	return r.client.PTTL(ctx, key).Result()
}

func (r *redisProjectionRedis) SetEX(ctx context.Context, key, value string, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r *redisProjectionRedis) SetPX(ctx context.Context, key, value string, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r *redisProjectionRedis) Set(ctx context.Context, key, value string) error {
	return r.client.Set(ctx, key, value, 0).Err()
}

func (r *redisProjectionRedis) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return r.client.Del(ctx, keys...).Err()
}

func (r *redisProjectionRedis) AcquireLock(ctx context.Context, key, value string, ttlMs int) (bool, error) {
	return r.client.SetNX(ctx, key, value, time.Duration(ttlMs)*time.Millisecond).Result()
}

// releaseLockScript 与 Node 的 Lua 逐字同义（rebuild-worker.ts:283-289）：
// 只有锁值等于自己的 nonce 才删，避免删掉别人的锁。
const releaseLockScript = `
      if redis.call('GET', KEYS[1]) == ARGV[1] then
        return redis.call('DEL', KEYS[1])
      else
        return 0
      end
    `

func (r *redisProjectionRedis) ReleaseLock(ctx context.Context, key, value string) error {
	return r.client.Eval(ctx, releaseLockScript, []string{key}, value).Err()
}

func (r *redisProjectionRedis) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return r.client.HGetAll(ctx, key).Result()
}

func (r *redisProjectionRedis) Scan(ctx context.Context, pattern string, limit int) ([]string, error) {
	var cursor uint64
	keys := make([]string, 0, limit)
	for {
		batch, next, err := r.client.Scan(ctx, cursor, pattern, 64).Result()
		if err != nil {
			return keys, err
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 || (limit > 0 && len(keys) >= limit) {
			break
		}
	}
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}

// BuildGenerationFingerprint 复刻 buildGenerationFingerprint（redis-contract.ts:55-69）：
// `sha1(configVersion|interval|alignedFrom|alignedTo)` 取前 16 位十六进制。
//
// 两端点都先按 interval 对齐再入指纹：同一窗口内的重建得到同一个代，重建因此幂等。
func BuildGenerationFingerprint(
	configVersion string,
	intervalMinutes int,
	coveredFromISO string,
	coveredToISO string,
) (string, error) {
	if err := assertPositiveInt(intervalMinutes, "intervalMinutes"); err != nil {
		return "", err
	}
	alignedFrom, err := AlignBucketStartUTC(coveredFromISO, intervalMinutes)
	if err != nil {
		return "", err
	}
	alignedTo, err := AlignBucketStartUTC(coveredToISO, intervalMinutes)
	if err != nil {
		return "", err
	}
	fingerprint := strings.Join([]string{
		configVersion,
		fmt.Sprintf("%d", intervalMinutes),
		alignedFrom,
		alignedTo,
	}, "|")
	sum := sha1.Sum([]byte(fingerprint))
	return hex.EncodeToString(sum[:])[:16], nil
}

// ReadInternalConfigSnapshot 复刻 readCurrentInternalPublicStatusConfigSnapshot
// （config-snapshot.ts:382-426）在 `allowLegacyFallback: false` 下的路径：只看当前前缀。
//
// 与读侧既有 `ReadCurrentInternalConfigSnapshot` 的区别：那一个把内部快照解成了**公开**类型
// （只有 slug/displayName/models，没有 sourceGroupId/sourceGroupName），而 rollup 字段键
// 恰恰需要源分组名（`getPublicStatusGroupId`）。故这里解成内部类型。
func ReadInternalConfigSnapshot(
	ctx context.Context,
	store PublicStatusStore,
	prefix string,
) *InternalPublicStatusConfigSnapshot {
	return readInternalConfigSnapshotAt(ctx, store, prefix)
}

// ReadInternalConfigSnapshotLegacyAware 复刻同一函数在**允许 legacy 回退**下的路径
// （config-snapshot.ts:390-393：`[undefined, LEGACY]`）：先当前前缀，再 v1。
//
// 两条路径分开列而不是合成一个 bool 参数：调用点要能一眼看出「我这一处允不允许回退」。
// 调度器用这一条（Node 调用时未传 `allowLegacyFallback:false`），worker 用上一条。
func ReadInternalConfigSnapshotLegacyAware(
	ctx context.Context,
	store PublicStatusStore,
	prefix string,
) *InternalPublicStatusConfigSnapshot {
	if snapshot := readInternalConfigSnapshotAt(ctx, store, prefix); snapshot != nil {
		return snapshot
	}
	if prefix == "" {
		return readInternalConfigSnapshotAt(ctx, store, LegacyPublicStatusRedisPrefix)
	}
	return nil
}

// readInternalConfigSnapshotAt 是单个前缀下的「版本指针 → 内部快照」查找（含 current 指针回退）。
func readInternalConfigSnapshotAt(
	ctx context.Context,
	store PublicStatusStore,
	prefix string,
) *InternalPublicStatusConfigSnapshot {
	if store == nil {
		return nil
	}
	versionKey := BuildConfigVersionPointerKey()
	if prefix != "" {
		versionKey = prefix + ":config-version:current"
	}
	if version := extractConfigVersion(safeGet(ctx, store, versionKey)); version != "" {
		key := BuildInternalConfigSnapshotKey(version)
		if prefix != "" {
			key = prefix + ":config-internal:" + encodeKeyPart(version)
		}
		if snapshot := parseInternalConfigSnapshot(safeGet(ctx, store, key)); snapshot != nil {
			return snapshot
		}
	}

	pointerKey := BuildInternalConfigSnapshotKey("current")
	if prefix != "" {
		pointerKey = prefix + ":config-internal:current"
	}
	pointerRaw := safeGet(ctx, store, pointerKey)
	var pointer struct {
		Key string `json:"key"`
	}
	if len(pointerRaw) == 0 || json.Unmarshal([]byte(pointerRaw), &pointer) != nil || pointer.Key == "" {
		return nil
	}
	return parseInternalConfigSnapshot(safeGet(ctx, store, pointer.Key))
}

func parseInternalConfigSnapshot(raw []byte) *InternalPublicStatusConfigSnapshot {
	if len(raw) == 0 {
		return nil
	}
	var snapshot InternalPublicStatusConfigSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil
	}
	if snapshot.ConfigVersion == "" {
		return nil
	}
	return &snapshot
}

// ---------- 发布 ----------

// ProjectionSnapshotRecord 是 `<prefix>:snapshot:<generation>:…` 的正文。
//
// 字段顺序即 Node `JSON.stringify` 的键序（rebuild-worker.ts:180-187）：写出去的记录
// 会被**两侧**读，键序不影响语义，但逐字节对拍时会看，故照抄。
type ProjectionSnapshotRecord struct {
	RebuildState     string                     `json:"rebuildState"`
	SourceGeneration string                     `json:"sourceGeneration"`
	GeneratedAt      string                     `json:"generatedAt"`
	FreshUntil       string                     `json:"freshUntil"`
	Groups           []PublicStatusPayloadGroup `json:"groups"`
}

// ProjectionSeriesRecord 是 `<prefix>:series:<generation>:…` 的正文（rebuild-worker.ts:188-194）。
type ProjectionSeriesRecord struct {
	SourceGeneration string                     `json:"sourceGeneration"`
	GeneratedAt      string                     `json:"generatedAt"`
	CoveredFrom      string                     `json:"coveredFrom"`
	CoveredTo        string                     `json:"coveredTo"`
	Groups           []PublicStatusPayloadGroup `json:"groups"`
}

// ProjectionManifestRecord 是 manifest 的正文（rebuild-worker.ts:195-212）。
//
// 读侧只读其中一部分（见 readkeys.go 的 PublicStatusManifest）；这里写全 Node 的字段，
// 因为**双跑期 Node 也读同一个键**，缺字段会让 Node 的读侧判成「从未有成代」。
type ProjectionManifestRecord struct {
	ConfigVersion          string  `json:"configVersion"`
	IntervalMinutes        int     `json:"intervalMinutes"`
	RangeHours             int     `json:"rangeHours"`
	Generation             string  `json:"generation"`
	SourceGeneration       string  `json:"sourceGeneration"`
	CoveredFrom            string  `json:"coveredFrom"`
	CoveredTo              string  `json:"coveredTo"`
	GeneratedAt            string  `json:"generatedAt"`
	FreshUntil             string  `json:"freshUntil"`
	RebuildState           string  `json:"rebuildState"`
	LastCompleteGeneration *string `json:"lastCompleteGeneration"`
	RollupCoverageStartAt  *string `json:"rollupCoverageStartedAt"`
	RollupCoverageComplete bool    `json:"rollupCoverageComplete"`
	RollupSampleCount      float64 `json:"rollupSampleCount"`
}

// PublishProjectionInput 是一次发布的输入（rebuild-worker.ts:113-130）。
type PublishProjectionInput struct {
	ConfigVersion          string
	IntervalMinutes        int
	RangeHours             int
	SourceGeneration       string
	GeneratedAt            string
	CoveredFrom            string
	CoveredTo              string
	RollupCoverageStartAt  *string
	RollupCoverageComplete bool
	RollupSampleCount      float64
	Groups                 []PublicStatusPayloadGroup
}

// PublishOutcome 报告一次发布的落点（用于测试与日志）。
type PublishOutcome struct {
	SnapshotKey          string
	SeriesKey            string
	CurrentManifestKey   string
	VersionedManifestKey string
	PromotedCurrent      bool
}

// PublishProjection 复刻 publishPublicStatusProjection（rebuild-worker.ts:113-247）。
//
// 写入顺序（照 Node）：
//  1. 先写**临时键**（TTL 300s）：崩溃时留下的是可辨认的半成品，而不是污染正式键；
//  2. 再写正式快照/序列键与**版本化** manifest（TTL 30 天，受保留期策略约束）；
//  3. 最后按**提升规则**决定是否覆盖 `manifest:current`——只有「版本更新或同版本但更新鲜」
//     才覆盖，避免旧的慢任务把新代顶掉（这才是 current 键不写坏的关键）；
//  4. 删除临时键。
func PublishProjection(
	ctx context.Context,
	writer ProjectionRedis,
	input PublishProjectionInput,
	retention RetentionTTLResolver,
	prefix string,
) (PublishOutcome, error) {
	var outcome PublishOutcome
	if retention == nil {
		retention = UnlimitedRetentionTTL
	}
	generatedAtMs, err := ParseISOMilli(input.GeneratedAt)
	if err != nil {
		return outcome, err
	}
	freshUntil := time.UnixMilli(generatedAtMs.UnixMilli() + int64(input.IntervalMinutes)*60*1000).
		UTC().Format(isoMilliLayout)

	snapshotKey, err := BuildCurrentSnapshotKey(input.IntervalMinutes, input.RangeHours, input.SourceGeneration, prefix)
	if err != nil {
		return outcome, err
	}
	seriesKey, err := BuildSeriesChunkKey(
		input.IntervalMinutes, input.SourceGeneration, input.CoveredFrom, input.CoveredTo, prefix,
	)
	if err != nil {
		return outcome, err
	}
	currentManifestKey, err := BuildManifestKey("current", input.IntervalMinutes, input.RangeHours, prefix)
	if err != nil {
		return outcome, err
	}
	versionedManifestKey, err := BuildManifestKey(input.ConfigVersion, input.IntervalMinutes, input.RangeHours, prefix)
	if err != nil {
		return outcome, err
	}
	outcome.SnapshotKey = snapshotKey
	outcome.SeriesKey = seriesKey
	outcome.CurrentManifestKey = currentManifestKey
	outcome.VersionedManifestKey = versionedManifestKey

	nonce := fmt.Sprintf("%s-%d", input.SourceGeneration, time.Now().UnixMilli())
	snapshotTempKey := BuildTempKey(snapshotKey, nonce)
	seriesTempKey := BuildTempKey(seriesKey, nonce)

	snapshotRecord := ProjectionSnapshotRecord{
		RebuildState:     "fresh",
		SourceGeneration: input.SourceGeneration,
		GeneratedAt:      input.GeneratedAt,
		FreshUntil:       freshUntil,
		Groups:           input.Groups,
	}
	seriesRecord := ProjectionSeriesRecord{
		SourceGeneration: input.SourceGeneration,
		GeneratedAt:      input.GeneratedAt,
		CoveredFrom:      input.CoveredFrom,
		CoveredTo:        input.CoveredTo,
		Groups:           input.Groups,
	}
	lastComplete := input.SourceGeneration
	manifestRecord := ProjectionManifestRecord{
		ConfigVersion:          input.ConfigVersion,
		IntervalMinutes:        input.IntervalMinutes,
		RangeHours:             input.RangeHours,
		Generation:             input.SourceGeneration,
		SourceGeneration:       input.SourceGeneration,
		CoveredFrom:            input.CoveredFrom,
		CoveredTo:              input.CoveredTo,
		GeneratedAt:            input.GeneratedAt,
		FreshUntil:             freshUntil,
		RebuildState:           "idle",
		LastCompleteGeneration: &lastComplete,
		RollupCoverageStartAt:  input.RollupCoverageStartAt,
		RollupCoverageComplete: input.RollupCoverageComplete,
		RollupSampleCount:      input.RollupSampleCount,
	}

	snapshotJSON, err := json.Marshal(snapshotRecord)
	if err != nil {
		return outcome, err
	}
	seriesJSON, err := json.Marshal(seriesRecord)
	if err != nil {
		return outcome, err
	}
	manifestJSON, err := json.Marshal(manifestRecord)
	if err != nil {
		return outcome, err
	}

	tempTTL := time.Duration(TempProjectionTTLSeconds) * time.Second
	if err := writer.SetEX(ctx, snapshotTempKey, string(snapshotJSON), tempTTL); err != nil {
		return outcome, err
	}
	if err := writer.SetEX(ctx, seriesTempKey, string(seriesJSON), tempTTL); err != nil {
		return outcome, err
	}

	generationTTL := time.Duration(retention(GenerationProjectionTTLSeconds)) * time.Second
	if err := writer.SetEX(ctx, snapshotKey, string(snapshotJSON), generationTTL); err != nil {
		return outcome, err
	}
	if err := writer.SetEX(ctx, seriesKey, string(seriesJSON), generationTTL); err != nil {
		return outcome, err
	}
	if err := writer.SetEX(ctx, versionedManifestKey, string(manifestJSON), generationTTL); err != nil {
		return outcome, err
	}

	existingRaw, existingOK := writer.Get(ctx, currentManifestKey)
	promote := true
	if existingOK {
		promote = shouldPromoteCurrentManifest(existingRaw, input.ConfigVersion, input.CoveredTo)
	}
	if promote {
		if err := writer.SetEX(ctx, currentManifestKey, string(manifestJSON), generationTTL); err != nil {
			return outcome, err
		}
		outcome.PromotedCurrent = true
	}

	if err := writer.Del(ctx, snapshotTempKey, seriesTempKey); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// shouldPromoteCurrentManifest 复刻 shouldPromoteCurrentManifest（rebuild-worker.ts:69-94）。
//
// 判据（顺序即语义）：候选版本号更大 → 提升；版本号相同但候选的 coveredTo 更新 → 提升；
// 其余一律不提升。解析失败按 0/零值处理（与 Node 的 `Number.parseInt(digits || "0")` 同判）。
func shouldPromoteCurrentManifest(existingRaw string, candidateVersion string, candidateCoveredTo string) bool {
	var existing struct {
		ConfigVersion *string `json:"configVersion"`
		CoveredTo     *string `json:"coveredTo"`
	}
	if err := json.Unmarshal([]byte(existingRaw), &existing); err != nil {
		return true
	}
	existingVersion := ""
	if existing.ConfigVersion != nil {
		existingVersion = *existing.ConfigVersion
	}
	candidateOrder := parseConfigVersionOrder(candidateVersion)
	existingOrder := parseConfigVersionOrder(existingVersion)
	if existingOrder > candidateOrder {
		return false
	}
	if existingOrder == candidateOrder && existing.CoveredTo != nil {
		existingCoveredTo, err := ParseISOMilli(*existing.CoveredTo)
		if err == nil {
			candidateTime, candidateErr := ParseISOMilli(candidateCoveredTo)
			if candidateErr == nil && existingCoveredTo.After(candidateTime) {
				return false
			}
		}
	}
	return true
}

// parseConfigVersionOrder 复刻 parseConfigVersionOrder（rebuild-worker.ts:64-67）：
// 取出全部数字拼起来当序（`cfg-1789227546000` → 1789227546000）。
func parseConfigVersionOrder(configVersion string) int64 {
	digits := make([]byte, 0, len(configVersion))
	for index := 0; index < len(configVersion); index++ {
		char := configVersion[index]
		if char >= '0' && char <= '9' {
			digits = append(digits, char)
		}
	}
	if len(digits) == 0 {
		return 0
	}
	var value int64
	for _, char := range digits {
		value = value*10 + int64(char-'0')
	}
	return value
}

// ---------- worker 主入口 ----------

// RebuildStatus 是重建结果的状态（rebuild-worker.ts:316-326）。
type RebuildStatus string

const (
	RebuildStatusDisabled RebuildStatus = "disabled"
	RebuildStatusSkipped  RebuildStatus = "skipped"
	RebuildStatusUpdated  RebuildStatus = "updated"
)

// RebuildDisabledReason 是 disabled 的原因（rebuild-worker.ts:318）。
type RebuildDisabledReason string

const (
	RebuildRedisUnavailable RebuildDisabledReason = "redis-unavailable"
	RebuildMissingConfig    RebuildDisabledReason = "missing-config"
	RebuildNoGroups         RebuildDisabledReason = "no-configured-groups"
)

// RebuildResult 是重建结果。
type RebuildResult struct {
	Status           RebuildStatus
	Reason           RebuildDisabledReason
	SkipReason       string
	SourceGeneration string
	Outcome          PublishOutcome
}

// RebuildOptions 是 worker 的依赖与开关。
type RebuildOptions struct {
	Redis ProjectionRedis
	// IntervalMinutes / RangeHours 是本轮要重建的展示粒度与窗口（Node 的调度器从配置快照与
	// 重建提示里得出这两个值，再逐个调用 worker）。
	IntervalMinutes int
	RangeHours      int
	// Prefix 为空即当前前缀（v2）。读路径对 legacy v1 复用同一批构造器，写侧只写当前前缀。
	Prefix string
	// Retention 由装配方注入设置驱动的实现；nil 即「不限（30 天）」。
	Retention RetentionTTLResolver
	// Now 供测试注入固定时刻；nil 即 time.Now。
	Now func() time.Time
	// BootstrapConfig 是「内部配置快照缺失时自举发布一次」的钩子（Node 的
	// publishCurrentPublicStatusConfigProjection）。nil 表示不自举（按 missing-config 处理）。
	BootstrapConfig func(ctx context.Context) error
	Logger          Logger
}

// inFlightRebuilds 复刻 runPublicStatusRebuild 的单飞登记（rebuild-worker.ts:291-315）：
// 同一 flightKey 的并发重建合并成一次，避免同代被重复计算并同时抢锁。
var (
	inFlightMu       sync.Mutex
	inFlightRebuilds = map[string]*inFlightEntry{}
)

type inFlightEntry struct {
	done   chan struct{}
	result RebuildResult
	err    error
}

// RebuildProjection 复刻 rebuildPublicStatusProjection（rebuild-worker.ts:316-...）。
func RebuildProjection(ctx context.Context, options RebuildOptions) (RebuildResult, error) {
	if err := assertSupportedRollupInterval(options.IntervalMinutes); err != nil {
		return RebuildResult{}, err
	}
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	if options.Redis == nil {
		return RebuildResult{Status: RebuildStatusDisabled, Reason: RebuildRedisUnavailable}, nil
	}
	if !options.Redis.Ready(ctx) {
		return RebuildResult{Status: RebuildStatusDisabled, Reason: RebuildRedisUnavailable}, nil
	}

	snapshot := ReadInternalConfigSnapshot(ctx, options.Redis, options.Prefix)
	if snapshot == nil && options.BootstrapConfig != nil {
		if err := options.BootstrapConfig(ctx); err == nil {
			snapshot = ReadInternalConfigSnapshot(ctx, options.Redis, options.Prefix)
		}
	}
	if snapshot == nil {
		return RebuildResult{Status: RebuildStatusDisabled, Reason: RebuildMissingConfig}, nil
	}
	groups := ConfiguredGroupsFromSnapshot(snapshot)
	if len(groups) == 0 {
		return RebuildResult{Status: RebuildStatusDisabled, Reason: RebuildNoGroups}, nil
	}

	intervalMinutes := options.IntervalMinutes
	rangeHours := options.RangeHours
	coveredTo, err := AlignBucketStartUTC(now().UTC().Format(isoMilliLayout), intervalMinutes)
	if err != nil {
		return RebuildResult{}, err
	}
	coveredToMs, err := ParseISOMilli(coveredTo)
	if err != nil {
		return RebuildResult{}, err
	}
	coveredFrom := time.UnixMilli(coveredToMs.UnixMilli() - int64(rangeHours)*60*60*1000).
		UTC().Format(isoMilliLayout)
	sourceGeneration, err := BuildGenerationFingerprint(
		snapshot.ConfigVersion, intervalMinutes, coveredFrom, coveredTo,
	)
	if err != nil {
		return RebuildResult{}, err
	}

	flightKey := strings.Join([]string{
		snapshot.ConfigVersion,
		fmt.Sprintf("%dm", intervalMinutes),
		fmt.Sprintf("%dh", rangeHours),
		sourceGeneration,
	}, ":")

	result, err := runSingleFlight(flightKey, func() (RebuildResult, error) {
		return computeGeneration(ctx, options, computeInput{
			configVersion:    snapshot.ConfigVersion,
			intervalMinutes:  intervalMinutes,
			rangeHours:       rangeHours,
			coveredFrom:      coveredFrom,
			coveredTo:        coveredTo,
			sourceGeneration: sourceGeneration,
			flightKey:        flightKey,
			groups:           groups,
		})
	})
	return result, err
}

// computeInput 是单代计算的输入。
type computeInput struct {
	configVersion    string
	intervalMinutes  int
	rangeHours       int
	coveredFrom      string
	coveredTo        string
	sourceGeneration string
	flightKey        string
	groups           []ConfiguredGroup
}

// runSingleFlight 复刻 runPublicStatusRebuild 的单飞语义：同键并发只算一次。
//
// 收尾一律走 defer：compute 一旦 panic，原本「算完再 close(done) + 删条目」的写法会让
// done 永不关闭、条目永不删除，同键的等待者全部永久阻塞在 <-entry.done 上——
// 一个后台重建的 panic 会因此变成整条投影链的静默挂死。恢复后的 panic 转成错误返回，
// 由调用方按普通失败处理（不向调用者重新抛出，免得一个脏代数带走整个进程）。
func runSingleFlight(flightKey string, compute func() (RebuildResult, error)) (result RebuildResult, err error) {
	inFlightMu.Lock()
	if entry, ok := inFlightRebuilds[flightKey]; ok {
		inFlightMu.Unlock()
		<-entry.done
		return entry.result, entry.err
	}
	entry := &inFlightEntry{done: make(chan struct{})}
	inFlightRebuilds[flightKey] = entry
	inFlightMu.Unlock()

	// 具名返回值不是风格选择：panic 被 recover 后，函数返回的是返回值变量的当前值，
	// 写 entry.err 只能让等待者看到错误，调用方仍会拿到零值 nil——两边必须同时可见。
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("pubstatus: rebuild panicked: %v", recovered)
		}
		entry.result, entry.err = result, err
		close(entry.done)
		inFlightMu.Lock()
		delete(inFlightRebuilds, flightKey)
		inFlightMu.Unlock()
	}()

	result, err = compute()
	return result, err
}

// computeGeneration 复刻 worker 里 `computeGeneration` 那段（rebuild-worker.ts:366-...）：
// 抢锁 → 读桶 → 覆盖率与样本数 → 聚合 → 发布 → 放锁。
func computeGeneration(ctx context.Context, options RebuildOptions, input computeInput) (RebuildResult, error) {
	lockKey := BuildRebuildLockKey(input.flightKey, options.Prefix)
	lockID := fmt.Sprintf("%d-%d", time.Now().UnixMilli(), time.Now().UnixNano()%1_000_000)
	acquired, err := options.Redis.AcquireLock(ctx, lockKey, lockID, RebuildLockTTLMs)
	if err != nil {
		return RebuildResult{}, err
	}
	if !acquired {
		return RebuildResult{
			Status: RebuildStatusSkipped, SkipReason: "distributed-lock-held",
			SourceGeneration: input.sourceGeneration,
		}, nil
	}
	defer func() {
		if releaseErr := options.Redis.ReleaseLock(ctx, lockKey, lockID); releaseErr != nil && options.Logger != nil {
			options.Logger.Warn("public_status_rebuild_lock_release_failed", map[string]any{
				"lockKey": lockKey, "error": releaseErr.Error(),
			})
		}
	}()

	coveredToTime, err := ParseISOMilli(input.coveredTo)
	if err != nil {
		return RebuildResult{}, err
	}
	bucketStarts, err := BuildRollupBucketStartsOnly(coveredToTime, input.rangeHours, input.intervalMinutes)
	if err != nil {
		return RebuildResult{}, err
	}
	buckets := ReadRollupBucketsWithProjectionRedis(ctx, options.Redis, bucketStarts, options.Prefix)

	coverageStartKey, err := BuildRollupCoverageStartKey(PublicStatusRollupBucketMinutes, options.Prefix)
	if err != nil {
		return RebuildResult{}, err
	}
	coverageStartRaw, coverageStartOK := options.Redis.Get(ctx, coverageStartKey)
	var coverageStart *string
	if coverageStartOK && strings.TrimSpace(coverageStartRaw) != "" {
		trimmed := strings.TrimSpace(coverageStartRaw)
		coverageStart = &trimmed
	}

	sampleCount := 0.0
	for _, bucket := range buckets {
		for field, value := range bucket.Values {
			_, _, metric, ok := ParseRollupField(field)
			if !ok {
				continue
			}
			if metric == RollupMetricSuccess || metric == RollupMetricFailure {
				sampleCount += value
			}
		}
	}
	coverageComplete := isRollupCoverageComplete(coverageStart, input.coveredFrom)

	aggregation, err := BuildPayloadFromRollups(
		coveredToTime, input.rangeHours, input.intervalMinutes, input.groups, buckets,
	)
	if err != nil {
		return RebuildResult{}, err
	}

	outcome, err := PublishProjection(ctx, options.Redis, PublishProjectionInput{
		ConfigVersion:          input.configVersion,
		IntervalMinutes:        input.intervalMinutes,
		RangeHours:             input.rangeHours,
		SourceGeneration:       input.sourceGeneration,
		GeneratedAt:            aggregation.GeneratedAt,
		CoveredFrom:            aggregation.CoveredFrom,
		CoveredTo:              aggregation.CoveredTo,
		RollupCoverageStartAt:  coverageStart,
		RollupCoverageComplete: coverageComplete,
		RollupSampleCount:      sampleCount,
		Groups:                 aggregation.Groups,
	}, options.Retention, options.Prefix)
	if err != nil {
		return RebuildResult{}, err
	}

	return RebuildResult{
		Status:           RebuildStatusUpdated,
		SourceGeneration: input.sourceGeneration,
		Outcome:          outcome,
	}, nil
}

// isRollupCoverageComplete 复刻 isRollupCoverageComplete（rebuild-worker.ts:96-111）：
// 覆盖起点不晚于窗口起点才算完整。
func isRollupCoverageComplete(coverageStartedAt *string, coveredFrom string) bool {
	if coverageStartedAt == nil {
		return false
	}
	started, err := ParseISOMilli(*coverageStartedAt)
	if err != nil {
		return false
	}
	from, err := ParseISOMilli(coveredFrom)
	if err != nil {
		return false
	}
	return !started.After(from)
}

// ReadRollupBucketsWithProjectionRedis 把 ProjectionRedis 适配给 ReadRollupBuckets。
//
// 两个接口都要 HGetAll，但读侧为了「用窄接口就能测」而单列；这里只做一次投影适配，
// 不再复制读桶逻辑。
func ReadRollupBucketsWithProjectionRedis(
	ctx context.Context,
	redis ProjectionRedis,
	bucketStarts []string,
	prefix string,
) []RollupBucket {
	return ReadRollupBuckets(ctx, projectionBucketReader{redis: redis}, bucketStarts, prefix)
}

type projectionBucketReader struct {
	redis ProjectionRedis
}

func (r projectionBucketReader) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	if r.redis == nil {
		return nil, nil
	}
	return r.redis.HGetAll(ctx, key)
}
