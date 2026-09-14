package pubstatus

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是 Node `src/lib/public-status/read-store.ts`（399 行）与 `rebuild-hints.ts`（122 行）的
// 转写：**公开状态 GET 的读路径**，以及它在服务降级时投下的重建提示。
//
// 一个必须先纠正的既有判断：
// 那份建议说「先移植 rollup 读面，它是 GET 的唯一外部依赖」。**实测不成立**：读路径只读
// `<prefix>:manifest:<version>:<interval>m:<range>h` 与
// `<prefix>:snapshot:<generation>:<interval>m:<range>h` 两个键，而 `snapshot` **已经是聚合完成
// 的成品**（内含每个模型的 timeline）。rollup/series/aggregation 属于**写侧**（Node 的
// rebuild-worker 用它把时序压成 snapshot），GET 一行都不碰——证据：`read-store.ts` 全文只
// import 了 redis-contract 的 manifest/snapshot 两个键构造器与状态判定；`rollup-store.ts` /
// `aggregation.ts` 的引用者只有 `rebuild-worker.ts` 与 `src/repository/message.ts`。
//
// **因此本文件不含 rollup / aggregation**。但那带来一处**必须写明的退役前置**：
// `<prefix>:manifest:*` 与 `<prefix>:snapshot:*` 今天**只有 Node 的 rebuild-worker 在写**
// （全仓 `grep` 证据见报告）。Node 下线后没人再刷新这两组键 → 公开状态页会在 `freshUntil`
// 到期后停在 stale，并因没有新快照而逐步降级到 rebuilding。也就是说：**读端点可以先上，
// 但「公开状态页在 Node 下线后仍会自更新」这件事需要把写侧（rebuild-worker + rollup +
// aggregation ≈ 2050 行）port 过来**——那是独立一路，本文件不越界替它决定。

// PublicStatusTimelineState 是时间线的单桶状态（payload.ts:3）。
type PublicStatusTimelineState string

const (
	TimelineStateOperational PublicStatusTimelineState = "operational"
	TimelineStateDegraded    PublicStatusTimelineState = "degraded"
	TimelineStateFailed      PublicStatusTimelineState = "failed"
	TimelineStateNoData      PublicStatusTimelineState = "no_data"
)

// PublicStatusTimelineBucket 对应 payload.ts:5-14。字段顺序即响应里的键序（对拍逐字节比对）。
//
// TTFTMs 读 JSON 的 `ttftMs`，缺失时回落旧字段名 `ttfbMs`（read-store.ts:141-143）：
// 快照跨版本持久化，旧快照里是 ttfbMs。
type PublicStatusTimelineBucket struct {
	BucketStart     string                    `json:"bucketStart"`
	BucketEnd       string                    `json:"bucketEnd"`
	State           PublicStatusTimelineState `json:"state"`
	AvailabilityPct *float64                  `json:"availabilityPct"`
	TTFTMs          *float64                  `json:"ttftMs"`
	TPS             *float64                  `json:"tps"`
	SampleCount     float64                   `json:"sampleCount"`
}

// PublicStatusPayloadModel 对应 payload.ts:16-26（含 latestTtfbMs 的旧字段名回落）。
type PublicStatusPayloadModel struct {
	PublicModelKey   string                       `json:"publicModelKey"`
	Label            string                       `json:"label"`
	VendorIconKey    string                       `json:"vendorIconKey"`
	RequestTypeBadge string                       `json:"requestTypeBadge"`
	LatestState      PublicStatusTimelineState    `json:"latestState"`
	AvailabilityPct  *float64                     `json:"availabilityPct"`
	LatestTTFTMs     *float64                     `json:"latestTtftMs"`
	LatestTPS        *float64                     `json:"latestTps"`
	Timeline         []PublicStatusTimelineBucket `json:"timeline"`
}

// PublicStatusPayloadGroup 对应 payload.ts:28-33。
type PublicStatusPayloadGroup struct {
	PublicGroupSlug string                     `json:"publicGroupSlug"`
	DisplayName     string                     `json:"displayName"`
	ExplanatoryCopy *string                    `json:"explanatoryCopy"`
	Models          []PublicStatusPayloadModel `json:"models"`
}

// PublicStatusPayload 对应 payload.ts:35-42。
type PublicStatusPayload struct {
	RebuildState     ServeState                 `json:"rebuildState"`
	SourceGeneration string                     `json:"sourceGeneration"`
	GeneratedAt      *string                    `json:"generatedAt"`
	FreshUntil       *string                    `json:"freshUntil"`
	Groups           []PublicStatusPayloadGroup `json:"groups"`
}

// PublicStatusSnapshotRecord 是 `<prefix>:snapshot:<generation>:…` 的正文（read-store.ts:22-27）。
type PublicStatusSnapshotRecord struct {
	SourceGeneration string `json:"sourceGeneration"`
	GeneratedAt      string `json:"generatedAt"`
	FreshUntil       string `json:"freshUntil"`
	Groups           any    `json:"groups"`
}

// PublicStatusStore 是读路径与重建提示需要的 Redis 子集。
//
// 用自定义窄接口而不是直接吃 `redis.UniversalClient`：读路径有十几条分支（快照缺失、manifest
// 缺字段、legacy 回退、TTL 保留……），真 Redis 只能覆盖其中一小部分；窄接口让每条分支都有
// 可重复的确定性测试。
//
// **Ready 是必需的**：Node 用 `redis.status !== "ready"` 区分两种截然不同的失败——
// 「Redis 不可用」（→ 503 rebuilding，客户端应重试）与「Redis 可用但没有快照」
// （→ 200 no_snapshot，前端显示「暂无数据」）。若不区分，所有读失败都退化成「没有快照」，
// 于是 Redis 挂掉时公开页会安静地显示「暂无数据」——监控看不到、客户端也不会重试。
type PublicStatusStore interface {
	// Ready 报告存储当前可用（Node 的 `redis.status === "ready"`）。
	Ready(ctx context.Context) bool
	// Get 返回键值；键不存在或读失败都返回 ("", false)（与 Node 的 safeGet 同义：Redis 抖动时
	// 公开端点退化成「无快照」，而不是把内部错误暴露给访客）。
	Get(ctx context.Context, key string) (string, bool)
	// PTTL 返回剩余 TTL；无 TTL 返回 -1，键不存在返回 -2。
	PTTL(ctx context.Context, key string) (time.Duration, error)
	SetEX(ctx context.Context, key, value string, ttl time.Duration) error
	SetPX(ctx context.Context, key, value string, ttl time.Duration) error
	Set(ctx context.Context, key, value string) error
}

// redisStatusStore 是 PublicStatusStore 的 go-redis 实现。
type redisStatusStore struct {
	client redis.UniversalClient
	logger Logger
}

// Logger 是读路径需要的最小日志面（logx.Logger 满足）。
type Logger interface {
	Warn(event string, fields map[string]any)
}

// NewRedisStatusStore 建 Redis 实现；client 为 nil 时返回 nil（装配方据此让路由不注册、回退 Node）。
func NewRedisStatusStore(client redis.UniversalClient, logger Logger) PublicStatusStore {
	if client == nil {
		return nil
	}
	return &redisStatusStore{client: client, logger: logger}
}

func (s *redisStatusStore) Ready(ctx context.Context) bool {
	// 一次 Ping（而不是缓存状态）：这条端点只有公开状态页在用，量级极低；而缓存就绪状态会把
	// 「Redis 刚恢复」也一并缓存住，那正是最需要立刻拿到 200 的时刻。用带超时的子上下文，
	// 避免 Redis 半死时把请求挂在默认超时上。
	pingCtx, cancel := context.WithTimeout(ctx, statusStorePingTimeout)
	defer cancel()
	return s.client.Ping(pingCtx).Err() == nil
}

// statusStorePingTimeout 是就绪探测的上限（与 Node 的「瞬时状态读」在体感上等价）。
const statusStorePingTimeout = 2 * time.Second

func (s *redisStatusStore) Get(ctx context.Context, key string) (string, bool) {
	value, err := s.client.Get(ctx, key).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) && s.logger != nil {
			s.logger.Warn("public_status_read_failed", map[string]any{"key": key, "error": err.Error()})
		}
		return "", false
	}
	return value, true
}

func (s *redisStatusStore) PTTL(ctx context.Context, key string) (time.Duration, error) {
	return s.client.PTTL(ctx, key).Result()
}

func (s *redisStatusStore) SetEX(ctx context.Context, key, value string, ttl time.Duration) error {
	return s.client.Set(ctx, key, value, ttl).Err()
}

func (s *redisStatusStore) SetPX(ctx context.Context, key, value string, ttl time.Duration) error {
	return s.client.Set(ctx, key, value, ttl).Err()
}

func (s *redisStatusStore) Set(ctx context.Context, key, value string) error {
	return s.client.Set(ctx, key, value, 0).Err()
}

// ReadPublicStatusPayloadInput 是读路径的输入（read-store.ts:293-301）。
type ReadPublicStatusPayloadInput struct {
	IntervalMinutes int
	RangeHours      int
	NowISO          string
	// ConfigVersion 为 nil 表示调用方没有配置快照（Node 的 `configSnapshot?.configVersion`）。
	ConfigVersion       *string
	HasConfiguredGroups *bool
	// TriggerRebuildHint 在服务降级处被调用；nil 时视为空实现。
	TriggerRebuildHint func(reason string)
}

// ReadPublicStatusPayload 复刻 readPublicStatusPayload（read-store.ts:293-399）。
func ReadPublicStatusPayload(
	ctx context.Context,
	store PublicStatusStore,
	input ReadPublicStatusPayloadInput,
) PublicStatusPayload {
	trigger := input.TriggerRebuildHint
	if trigger == nil {
		trigger = func(string) {}
	}

	// 没有配置分组 = 站点根本没配公开状态：诚实报 no-data，而不是 rebuilding
	// （rebuilding 会让前端显示「正在重建」，而实际是「没配置」）。
	if input.HasConfiguredGroups != nil && !*input.HasConfiguredGroups {
		return noDataPayload()
	}

	// Redis 不可用与「Redis 可用但没数据」必须是两种答复：前者 503（可重试），
	// 后者 200 + no_snapshot（前端显示暂无数据）。
	if store == nil || !store.Ready(ctx) {
		trigger("redis-unavailable")
		return rebuildingPayload()
	}

	configVersion := "current"
	if input.ConfigVersion != nil {
		configVersion = *input.ConfigVersion
	}

	primary, ok := readProjection(ctx, store, projectionReadInput{
		intervalMinutes: input.IntervalMinutes,
		rangeHours:      input.RangeHours,
		nowISO:          input.NowISO,
		configVersion:   configVersion,
	})
	if !ok {
		legacy, legacyOK := readProjection(ctx, store, projectionReadInput{
			intervalMinutes: input.IntervalMinutes,
			rangeHours:      input.RangeHours,
			nowISO:          input.NowISO,
			configVersion:   configVersion,
			prefix:          LegacyPublicStatusRedisPrefix,
		})
		if legacyOK {
			primary, ok = legacy, true
		} else if primary.miss != missSnapshotMissing {
			// Node 的取舍：只有「versioned 快照缺失」不被 legacy 的 miss 覆盖——
			// 否则会把「有 manifest 但快照丢了」误报成「manifest 都没有」。
			primary.miss = legacy.miss
		}
	}

	if !ok {
		reason := primary.miss
		if reason == "" {
			reason = missManifestMissing
		}
		trigger(string(reason))
		return rebuildingPayload()
	}

	projection := primary.projection

	if projection.prefix != LegacyPublicStatusRedisPrefix &&
		projection.manifest.RollupCoverageComplete != nil &&
		!*projection.manifest.RollupCoverageComplete {
		legacy, legacyOK := readProjection(ctx, store, projectionReadInput{
			intervalMinutes: input.IntervalMinutes,
			rangeHours:      input.RangeHours,
			nowISO:          input.NowISO,
			configVersion:   configVersion,
			prefix:          LegacyPublicStatusRedisPrefix,
		})

		if legacyOK {
			trigger("rollup-coverage-incomplete")
			trigger("legacy-generation")
			if input.ConfigVersion != nil && legacy.projection.manifest.ConfigVersion != configVersion {
				trigger("config-version-mismatch")
			}
			return projectionToPayload(legacy.projection, ServeStateStale)
		}

		trigger("rollup-coverage-incomplete")
		if input.ConfigVersion != nil && projection.manifest.ConfigVersion != configVersion {
			trigger("config-version-mismatch")
		}
		return projectionToPayload(projection, ServeStateStale)
	}

	if projection.resolution.RebuildState != ServeStateFresh ||
		projection.prefix == LegacyPublicStatusRedisPrefix {
		trigger("stale-generation")
	}
	if projection.prefix == LegacyPublicStatusRedisPrefix {
		trigger("legacy-generation")
	}
	if input.ConfigVersion != nil && projection.manifest.ConfigVersion != configVersion {
		trigger("config-version-mismatch")
		return projectionToPayload(projection, ServeStateStale)
	}

	state := projection.resolution.RebuildState
	if projection.prefix == LegacyPublicStatusRedisPrefix {
		state = ServeStateStale
	}
	return projectionToPayload(projection, state)
}

// 读 miss 的两种原因（read-store.ts:35-37 的字面量）。
type projectionMiss string

const (
	missManifestMissing projectionMiss = "manifest-missing"
	missSnapshotMissing projectionMiss = "snapshot-missing"
)

type projectionReadInput struct {
	intervalMinutes int
	rangeHours      int
	nowISO          string
	configVersion   string
	prefix          string
}

type projectionReadResult struct {
	prefix     string
	manifest   *PublicStatusManifest
	resolution PublicStatusManifestResolution
	snapshot   *PublicStatusSnapshotRecord
}

type projectionReadOutcome struct {
	projection *projectionReadResult
	miss       projectionMiss
}

// readProjection 复刻 readProjection（read-store.ts:203-277）。
//
// 一处容易漏的语义：当版本化 manifest 指不出可服务代时，会再看一眼 `current` manifest，
// 并在用它时把服务态**强制降为 stale**——「current 顶上」本身就是降级路径，不能报 fresh，
// 否则前端会把一个可能过期很久的快照当成新鲜数据。
func readProjection(
	ctx context.Context,
	store PublicStatusStore,
	input projectionReadInput,
) (projectionReadOutcome, bool) {
	manifestKey, err := BuildManifestKey(input.configVersion, input.intervalMinutes, input.rangeHours, input.prefix)
	if err != nil {
		return projectionReadOutcome{miss: missManifestMissing}, false
	}
	manifest := parseManifest(safeGet(ctx, store, manifestKey))

	var currentManifest *PublicStatusManifest
	if input.configVersion != "current" {
		currentKey, keyErr := BuildManifestKey("current", input.intervalMinutes, input.rangeHours, input.prefix)
		if keyErr == nil {
			currentManifest = parseManifest(safeGet(ctx, store, currentKey))
		}
	} else {
		currentManifest = manifest
	}

	selected := manifest
	resolution := ResolveManifestState(selected, input.nowISO)

	if resolution.SourceGeneration == "" && currentManifest != nil {
		selected = currentManifest
		resolution = ResolveManifestState(currentManifest, input.nowISO)
		resolution.RebuildState = ServeStateStale
	}

	if selected == nil || resolution.SourceGeneration == "" {
		return projectionReadOutcome{miss: missManifestMissing}, false
	}

	snapshotKey, snapshotKeyErr := BuildCurrentSnapshotKey(
		input.intervalMinutes, input.rangeHours, resolution.SourceGeneration, input.prefix,
	)
	if snapshotKeyErr != nil {
		return projectionReadOutcome{miss: missSnapshotMissing}, false
	}
	snapshot := parseSnapshot(safeGet(ctx, store, snapshotKey))
	if snapshot == nil {
		return projectionReadOutcome{miss: missSnapshotMissing}, false
	}

	return projectionReadOutcome{projection: &projectionReadResult{
		prefix:     input.prefix,
		manifest:   selected,
		resolution: resolution,
		snapshot:   snapshot,
	}}, true
}

func projectionToPayload(projection *projectionReadResult, state ServeState) PublicStatusPayload {
	generatedAt := projection.snapshot.GeneratedAt
	freshUntil := projection.snapshot.FreshUntil
	payload := PublicStatusPayload{
		RebuildState:     state,
		SourceGeneration: projection.snapshot.SourceGeneration,
		Groups:           sanitizeGroupSnapshots(projection.snapshot.Groups),
	}
	if generatedAt != "" {
		payload.GeneratedAt = &generatedAt
	}
	if freshUntil != "" {
		payload.FreshUntil = &freshUntil
	}
	return payload
}

// safeGet 归一化「键不存在」与「读失败」。
func safeGet(ctx context.Context, store PublicStatusStore, key string) []byte {
	value, ok := store.Get(ctx, key)
	if !ok {
		return nil
	}
	return []byte(value)
}

func parseManifest(raw []byte) *PublicStatusManifest {
	if len(raw) == 0 {
		return nil
	}
	var manifest PublicStatusManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil
	}
	return &manifest
}

func parseSnapshot(raw []byte) *PublicStatusSnapshotRecord {
	if len(raw) == 0 {
		return nil
	}
	var snapshot PublicStatusSnapshotRecord
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil
	}
	return &snapshot
}

func rebuildingPayload() PublicStatusPayload {
	return PublicStatusPayload{RebuildState: ServeStateRebuilding, Groups: []PublicStatusPayloadGroup{}}
}

func noDataPayload() PublicStatusPayload {
	return PublicStatusPayload{RebuildState: ServeStateNoData, Groups: []PublicStatusPayloadGroup{}}
}

// sanitizeGroupSnapshots 复刻 sanitizeGroupSnapshots（read-store.ts:189-217）。
//
// 为什么逐个字段白名单重建而不是直接透传快照正文：快照在 Redis 里跨版本持久化，可能是**旧版本
// 写的、含内部字段**的记录。直接透传就把内部字段（源分组 id/名、价格、供应商）泄露到无需认证的
// /status 上。这是防泄露的主闸门，不是格式美化。
func sanitizeGroupSnapshots(input any) []PublicStatusPayloadGroup {
	groups := make([]PublicStatusPayloadGroup, 0)
	items, ok := input.([]any)
	if !ok {
		return groups
	}

	for _, item := range items {
		value, ok := item.(map[string]any)
		if !ok {
			continue
		}
		slug, slugOK := value["publicGroupSlug"].(string)
		displayName, nameOK := value["displayName"].(string)
		if !slugOK || !nameOK {
			continue
		}
		var explanatoryCopy *string
		if copy_, ok := value["explanatoryCopy"].(string); ok {
			explanatoryCopy = &copy_
		}
		groups = append(groups, PublicStatusPayloadGroup{
			PublicGroupSlug: slug,
			DisplayName:     displayName,
			ExplanatoryCopy: explanatoryCopy,
			Models:          sanitizeModelSnapshots(value["models"]),
		})
	}
	return groups
}

// sanitizeModelSnapshots 复刻 sanitizeModelSnapshots（read-store.ts:146-186）。
func sanitizeModelSnapshots(input any) []PublicStatusPayloadModel {
	models := make([]PublicStatusPayloadModel, 0)
	items, ok := input.([]any)
	if !ok {
		return models
	}

	for _, item := range items {
		value, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, keyOK := value["publicModelKey"].(string)
		label, labelOK := value["label"].(string)
		iconKey, iconOK := value["vendorIconKey"].(string)
		badge, badgeOK := value["requestTypeBadge"].(string)
		if !keyOK || !labelOK || !iconOK || !badgeOK {
			continue
		}
		models = append(models, PublicStatusPayloadModel{
			PublicModelKey:   key,
			Label:            label,
			VendorIconKey:    iconKey,
			RequestTypeBadge: badge,
			LatestState:      normalizeTimelineState(value["latestState"]),
			AvailabilityPct:  normalizeNullableNumber(value["availabilityPct"]),
			LatestTTFTMs:     normalizeNullableNumber(firstPresent(value, "latestTtftMs", "latestTtfbMs")),
			LatestTPS:        normalizeNullableNumber(value["latestTps"]),
			Timeline:         sanitizeTimelineBuckets(value["timeline"]),
		})
	}
	return models
}

// sanitizeTimelineBuckets 复刻 sanitizeTimelineBuckets（read-store.ts:99-144）。
func sanitizeTimelineBuckets(input any) []PublicStatusTimelineBucket {
	buckets := make([]PublicStatusTimelineBucket, 0)
	items, ok := input.([]any)
	if !ok {
		return buckets
	}

	for _, item := range items {
		value, ok := item.(map[string]any)
		if !ok {
			continue
		}
		bucketStart, startOK := value["bucketStart"].(string)
		bucketEnd, endOK := value["bucketEnd"].(string)
		sampleCount, countOK := value["sampleCount"].(float64)
		if !startOK || !endOK || !countOK || math.IsNaN(sampleCount) || math.IsInf(sampleCount, 0) {
			continue
		}
		buckets = append(buckets, PublicStatusTimelineBucket{
			BucketStart:     bucketStart,
			BucketEnd:       bucketEnd,
			State:           normalizeTimelineState(value["state"]),
			AvailabilityPct: normalizeNullableNumber(value["availabilityPct"]),
			TTFTMs:          normalizeNullableNumber(firstPresent(value, "ttftMs", "ttfbMs")),
			TPS:             normalizeNullableNumber(value["tps"]),
			SampleCount:     sampleCount,
		})
	}
	return buckets
}

// firstPresent 取首个**存在**的键值（不看真假值，只看到没到）。
//
// 与 Node 的 `value.ttftMs !== undefined ? value.ttftMs : value.ttfbMs` 同义：显式的 null
// 会被 normalizeNullableNumber 归一成 nil，而不是串到旧字段上去。
func firstPresent(value map[string]any, keys ...string) any {
	for _, key := range keys {
		if found, ok := value[key]; ok {
			return found
		}
	}
	return nil
}

func normalizeTimelineState(value any) PublicStatusTimelineState {
	switch value {
	case "operational":
		return TimelineStateOperational
	case "degraded":
		return TimelineStateDegraded
	case "failed":
		return TimelineStateFailed
	case "no_data":
		return TimelineStateNoData
	}
	return TimelineStateNoData
}

func normalizeNullableNumber(value any) *float64 {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	return &number
}

// rebuildHintTTL 见 rebuild-hints.ts:6（300 秒）。
const rebuildHintTTL = 5 * time.Minute

// ReadCurrentConfigSnapshot 读「当前」public-status **配置**快照（config-snapshot.ts:337-380）。
//
// 查找顺序与 Node 逐条一致：两套前缀各试「版本指针 → `config:current` 指针里的 key」。
// 与 `adminapi` 的 pubmeta 读取器是**同一份键布局、不同的字段需求**：那边刻意只声明三个字段
// 以免公开路由泄露元数据，这里需要 defaultIntervalMinutes/defaultRangeHours/groups。
func ReadCurrentConfigSnapshot(ctx context.Context, store PublicStatusStore) *PublicStatusConfigSnapshot {
	if store == nil {
		return nil
	}
	for _, prefix := range []string{"", LegacyPublicStatusRedisPrefix} {
		versionKey := BuildConfigVersionPointerKey()
		prefixVersionKey := versionKey
		if prefix != "" {
			prefixVersionKey = prefix + ":config-version:current"
		}
		if version := extractConfigVersion(safeGet(ctx, store, prefixVersionKey)); version != "" {
			key := BuildConfigSnapshotKey(version)
			if prefix != "" {
				key = prefix + ":config:" + encodeKeyPart(version)
			}
			if snapshot := parseConfigSnapshot(safeGet(ctx, store, key)); snapshot != nil {
				return snapshot
			}
		}

		pointerKey := BuildConfigSnapshotKey("current")
		if prefix != "" {
			pointerKey = prefix + ":config:current"
		}
		pointerRaw := safeGet(ctx, store, pointerKey)
		var pointer struct {
			Key string `json:"key"`
		}
		if len(pointerRaw) == 0 || json.Unmarshal(pointerRaw, &pointer) != nil || pointer.Key == "" {
			continue
		}
		if snapshot := parseConfigSnapshot(safeGet(ctx, store, pointer.Key)); snapshot != nil {
			return snapshot
		}
	}
	return nil
}

// ReadCurrentInternalConfigSnapshot 读「当前」**内部**配置快照（config-snapshot.ts 的 internal 变体）。
//
// 重建提示需要它：提示里要写版本化的 manifest 键，而版本号只在内部快照里有。
func ReadCurrentInternalConfigSnapshot(ctx context.Context, store PublicStatusStore) *PublicStatusConfigSnapshot {
	if store == nil {
		return nil
	}
	for _, prefix := range []string{"", LegacyPublicStatusRedisPrefix} {
		versionKey := BuildConfigVersionPointerKey()
		if prefix != "" {
			versionKey = prefix + ":config-version:current"
		}
		if version := extractConfigVersion(safeGet(ctx, store, versionKey)); version != "" {
			key := BuildInternalConfigSnapshotKey(version)
			if prefix != "" {
				key = prefix + ":config-internal:" + encodeKeyPart(version)
			}
			if snapshot := parseConfigSnapshot(safeGet(ctx, store, key)); snapshot != nil {
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
		if len(pointerRaw) == 0 || json.Unmarshal(pointerRaw, &pointer) != nil || pointer.Key == "" {
			continue
		}
		if snapshot := parseConfigSnapshot(safeGet(ctx, store, pointer.Key)); snapshot != nil {
			return snapshot
		}
	}
	return nil
}

func parseConfigSnapshot(raw []byte) *PublicStatusConfigSnapshot {
	if len(raw) == 0 {
		return nil
	}
	var snapshot PublicStatusConfigSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil
	}
	return &snapshot
}

// ScheduleRebuildInput 见 rebuild-hints.ts:52-57。
type ScheduleRebuildInput struct {
	IntervalMinutes int
	RangeHours      int
	Reason          string
	// RequestedAt 为空时取当前时刻（Node 用 `new Date().toISOString()`）。
	RequestedAt string
}

// ScheduleRebuildResult 见 rebuild-hints.ts:58-62。
type ScheduleRebuildResult struct {
	Accepted     bool
	RebuildState ServeState
	Key          string
}

// SchedulePublicStatusRebuild 复刻 schedulePublicStatusRebuild（rebuild-hints.ts:52-118）。
//
// 三步语义，每一步都有存在理由：
//  1. **已有提示不重写**：提示键带 300s TTL，重建需要时间；重写会把 TTL 续成永久，
//     且会让「提示存在即已排队」这一判定失效。故只回 accepted:true。
//  2. **写提示**（EX 300）：真正的排队信号，rebuild-worker 轮询它。
//  3. **把 manifest 的 rebuildState 改成 rebuilding**（**保留原 TTL**）：让读侧在新鲜期内
//     把服务态降为 stale。TTL 必须保留——用普通 set 会把 manifest 变成永不过期，
//     于是过期快照永远不会被判定为 stale。
//
// 与 Node 一致：**Redis 不可用时直接回 accepted:false**（Node 的 `getReadyRedisClient` 返回 null
// 就 return，既不写提示也不改 manifest）。不先判这一条的话，Redis 故障期间会往一个连不上的
// 客户端上白扔写命令（且在假实现下会「假装成功」，让分支测试看上去与 Node 不一致）。
func SchedulePublicStatusRebuild(
	ctx context.Context,
	store PublicStatusStore,
	input ScheduleRebuildInput,
) ScheduleRebuildResult {
	if store == nil || !store.Ready(ctx) {
		return ScheduleRebuildResult{Accepted: false, RebuildState: ServeStateRebuilding}
	}

	key, err := BuildRebuildHintKey(input.IntervalMinutes, input.RangeHours, "")
	if err != nil {
		return ScheduleRebuildResult{Accepted: false, RebuildState: ServeStateRebuilding}
	}

	if ttl, ttlErr := store.PTTL(ctx, key); ttlErr == nil && ttl > 0 {
		return ScheduleRebuildResult{Accepted: true, RebuildState: ServeStateRebuilding, Key: key}
	}

	requestedAt := input.RequestedAt
	if requestedAt == "" {
		requestedAt = time.Now().UTC().Format(isoMilliLayout)
	}
	hintBody, marshalErr := json.Marshal(map[string]any{
		"reason":          input.Reason,
		"requestedAt":     requestedAt,
		"intervalMinutes": input.IntervalMinutes,
		"rangeHours":      input.RangeHours,
	})
	if marshalErr != nil {
		return ScheduleRebuildResult{Accepted: false, RebuildState: ServeStateRebuilding, Key: key}
	}
	if setErr := store.SetEX(ctx, key, string(hintBody), rebuildHintTTL); setErr != nil {
		return ScheduleRebuildResult{Accepted: false, RebuildState: ServeStateRebuilding, Key: key}
	}

	configSnapshot := ReadCurrentInternalConfigSnapshot(ctx, store)
	configVersion := "current"
	if configSnapshot != nil && configSnapshot.ConfigVersion != "" {
		configVersion = configSnapshot.ConfigVersion
	}

	for _, version := range []string{configVersion, "current"} {
		manifestKey, keyErr := BuildManifestKey(version, input.IntervalMinutes, input.RangeHours, "")
		if keyErr != nil {
			continue
		}
		raw := safeGet(ctx, store, manifestKey)
		if len(raw) == 0 {
			continue
		}
		var manifest map[string]any
		if json.Unmarshal(raw, &manifest) != nil {
			// 坏 manifest：忽略。读侧自己会走安全降级，这里替它「修好」反而掩盖写入侧的毛病。
			continue
		}
		manifest["rebuildState"] = "rebuilding"
		updated, marshalErr := json.Marshal(manifest)
		if marshalErr != nil {
			continue
		}
		writePreservingTTL(ctx, store, manifestKey, string(updated))
	}

	return ScheduleRebuildResult{Accepted: true, RebuildState: ServeStateRebuilding, Key: key}
}

// writePreservingTTL 复刻 writeManifestPreservingTtl（rebuild-hints.ts:16-29）。
//
// 只有「键确实还有剩余 TTL」时才带上 TTL 重写；`PTTL` 返回 -1（无 TTL）/ -2（键不存在）/
// 出错时都退化为不带 TTL 的普通 set——那是 Node 的 `Number.isFinite(ttlMs) && ttlMs > 0`
// 判定为假时的分支。
func writePreservingTTL(ctx context.Context, store PublicStatusStore, key, value string) {
	ttl, err := store.PTTL(ctx, key)
	if err == nil && ttl > 0 {
		_ = store.SetPX(ctx, key, value, ttl)
		return
	}
	_ = store.Set(ctx, key, value)
}

// extractConfigVersion 复刻 extractCurrentConfigVersion（config-snapshot.ts:158-177）：
// 裸 `cfg-` 串即版本；否则取 JSON 的 configVersion；再否则从 key 的末段反解。
func extractConfigVersion(pointerRaw []byte) string {
	if len(pointerRaw) == 0 {
		return ""
	}
	text := string(pointerRaw)
	if len(text) >= 4 && text[:4] == "cfg-" {
		return text
	}
	var pointer struct {
		Key           string `json:"key"`
		ConfigVersion string `json:"configVersion"`
	}
	if json.Unmarshal(pointerRaw, &pointer) != nil {
		return ""
	}
	if pointer.ConfigVersion != "" {
		return pointer.ConfigVersion
	}
	if pointer.Key == "" {
		return ""
	}
	segments := splitOnColon(pointer.Key)
	last := segments[len(segments)-1]
	trimmed := trimPrefixCI(last, "config-internal:")
	trimmed = trimPrefixCI(trimmed, "config:")
	return trimmed
}

func splitOnColon(value string) []string {
	parts := make([]string, 0, 4)
	start := 0
	for index := 0; index < len(value); index++ {
		if value[index] == ':' {
			parts = append(parts, value[start:index])
			start = index + 1
		}
	}
	return append(parts, value[start:])
}

// trimPrefixCI 只处理这里可能出现的两种字面前缀（大小写无关）。
func trimPrefixCI(value, prefix string) string {
	if len(value) < len(prefix) {
		return value
	}
	if !equalFoldASCII(value[:len(prefix)], prefix) {
		return value
	}
	return value[len(prefix):]
}

func equalFoldASCII(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := 0; index < len(left); index++ {
		a, b := left[index], right[index]
		if 'A' <= a && a <= 'Z' {
			a += 'a' - 'A'
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}
