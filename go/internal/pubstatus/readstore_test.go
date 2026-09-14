package pubstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 本文件钉住 read-store.ts（读路径）与 rebuild-hints.ts（重建提示）的分支。
//
// 为什么用内存假 Redis 而不是真 Redis：读路径的价值几乎全在**降级分支**上（manifest 缺、
// 快照缺、legacy 回退、rollup 未覆盖、TTL 保留……），真 Redis 只能覆盖「一切正常」那一条。
// 窄接口 `PublicStatusStore` 就是为这批分支测试准备的。

type fakeStatusStore struct {
	values map[string]string
	ttls   map[string]time.Duration
	// setCalls 记录写调用（键 + 是否带 TTL），用于断言「保留 TTL」这类语义。
	setCalls []fakeSetCall
	getErr   bool
	setErr   bool
	notReady bool
}

type fakeSetCall struct {
	key     string
	value   string
	ttl     time.Duration
	hasTTL  bool
	written string
}

func newFakeStatusStore() *fakeStatusStore {
	return &fakeStatusStore{values: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (f *fakeStatusStore) Ready(_ context.Context) bool { return !f.notReady }

func (f *fakeStatusStore) Get(_ context.Context, key string) (string, bool) {
	if f.getErr {
		return "", false
	}
	value, ok := f.values[key]
	return value, ok
}

func (f *fakeStatusStore) PTTL(_ context.Context, key string) (time.Duration, error) {
	if _, ok := f.values[key]; !ok {
		return -2 * time.Nanosecond, nil
	}
	if ttl, ok := f.ttls[key]; ok {
		return ttl, nil
	}
	return -1 * time.Nanosecond, nil
}

func (f *fakeStatusStore) SetEX(_ context.Context, key, value string, ttl time.Duration) error {
	if f.setErr {
		return fmt.Errorf("fake redis: set failed")
	}
	f.values[key] = value
	f.ttls[key] = ttl
	f.setCalls = append(f.setCalls, fakeSetCall{key: key, value: value, ttl: ttl, hasTTL: true, written: "EX"})
	return nil
}

func (f *fakeStatusStore) SetPX(_ context.Context, key, value string, ttl time.Duration) error {
	if f.setErr {
		return fmt.Errorf("fake redis: set failed")
	}
	f.values[key] = value
	f.ttls[key] = ttl
	f.setCalls = append(f.setCalls, fakeSetCall{key: key, value: value, ttl: ttl, hasTTL: true, written: "PX"})
	return nil
}

func (f *fakeStatusStore) Set(_ context.Context, key, value string) error {
	if f.setErr {
		return fmt.Errorf("fake redis: set failed")
	}
	f.values[key] = value
	delete(f.ttls, key)
	f.setCalls = append(f.setCalls, fakeSetCall{key: key, value: value, written: "SET"})
	return nil
}

const (
	fixtureNowISO     = "2026-09-13T00:10:00.000Z"
	fixtureFreshUntil = "2026-09-13T00:20:00.000Z"
	fixtureStaleUntil = "2026-09-13T00:05:00.000Z"
	fixtureVersion    = "cfg-1"
	fixtureGeneration = "gen-1"
)

// seedConfigSnapshot 写一份 v2 公开配置快照（版本指针 + 版本化正文）。
func seedConfigSnapshot(store *fakeStatusStore, groups int, interval, rangeHours int) {
	snapshot := map[string]any{
		"configVersion":          fixtureVersion,
		"generatedAt":            fixtureNowISO,
		"siteTitle":              "  CC Hub  ",
		"siteDescription":        "  desc  ",
		"timeZone":               "Asia/Shanghai",
		"defaultIntervalMinutes": interval,
		"defaultRangeHours":      rangeHours,
		"groups":                 make([]any, 0, groups),
	}
	for index := 0; index < groups; index++ {
		snapshot["groups"] = append(snapshot["groups"].([]any), map[string]any{
			"slug": fmt.Sprintf("group-%d", index), "displayName": fmt.Sprintf("Group %d", index),
		})
	}
	raw, _ := json.Marshal(snapshot)
	store.values[BuildConfigVersionPointerKey()] = fixtureVersion
	store.values[BuildConfigSnapshotKey(fixtureVersion)] = string(raw)
}

// seedManifest 写 manifest。
//
// keyVersion 是**键里的版本段**，innerVersion 是**正文里的 configVersion**：两者不同正是
// 「版本指针滞后」那种真实情形（键按请求的版本走，正文还是旧版本写的），
// 读路径的 config-version-mismatch 分支靠它触发。
func seedManifest(store *fakeStatusStore, prefix string, lastComplete *string, freshUntil, rebuildState string, coverageComplete *bool, keyVersion, innerVersion string) {
	if keyVersion == "" {
		keyVersion = fixtureVersion
	}
	if innerVersion == "" {
		innerVersion = keyVersion
	}
	manifest := map[string]any{
		"configVersion":          innerVersion,
		"generation":             "gen-0",
		"lastCompleteGeneration": lastComplete,
		"generatedAt":            fixtureNowISO,
		"freshUntil":             freshUntil,
		"rebuildState":           rebuildState,
	}
	if coverageComplete != nil {
		manifest["rollupCoverageComplete"] = *coverageComplete
	}
	raw, _ := json.Marshal(manifest)
	key, err := BuildManifestKey(keyVersion, 5, 24, prefix)
	if err != nil {
		panic(err)
	}
	store.values[key] = string(raw)
}

// seedSnapshot 写一份快照正文；groups 由调用方给定（用于注入私有字段与旧字段名）。
func seedSnapshot(store *fakeStatusStore, prefix, generation string, groups []any) {
	record := map[string]any{
		"sourceGeneration": generation,
		"generatedAt":      fixtureNowISO,
		"freshUntil":       fixtureFreshUntil,
		"groups":           groups,
	}
	raw, _ := json.Marshal(record)
	key, err := BuildCurrentSnapshotKey(5, 24, generation, prefix)
	if err != nil {
		panic(err)
	}
	store.values[key] = string(raw)
}

// publicGroupFixture 是一份**含私有字段与旧字段名**的快照分组：读路径必须按白名单重建。
func publicGroupFixture() []any {
	return []any{
		map[string]any{
			"publicGroupSlug": "group-a",
			"displayName":     "Group A",
			"explanatoryCopy": "copy",
			// 以下都是内部字段：绝不该出现在公开响应里（防泄露钉子会断言）。
			"sourceGroupId":   float64(42),
			"sourceGroupName": "internal-group-a",
			"price":           "0.003",
			"providerUrl":     "https://upstream.example",
			"secret":          "sk-do-not-leak",
			"models": []any{
				map[string]any{
					"publicModelKey":   "deepseek-v4-flash",
					"label":            "DeepSeek V4 Flash",
					"vendorIconKey":    "deepseek",
					"requestTypeBadge": "openaiCompatible",
					"latestState":      "operational",
					"availabilityPct":  float64(99.5),
					"latestTtfbMs":     float64(120), // 旧字段名 → 必须落到 latestTtftMs
					"latestTps":        float64(30.5),
					"providerId":       float64(7), // 私有
					"timeline": []any{
						map[string]any{
							"bucketStart":     "2026-09-13T00:00:00.000Z",
							"bucketEnd":       "2026-09-13T00:05:00.000Z",
							"state":           "operational",
							"availabilityPct": float64(100),
							"ttfbMs":          float64(110), // 旧字段名 → 必须落到 ttftMs
							"tps":             float64(28),
							"sampleCount":     float64(12),
							"errorMessages":   []any{"boom"}, // 私有
						},
					},
				},
			},
		},
		// 非法条目（缺字段 / 类型不对）必须被丢弃，而不是拼成半截对象。
		map[string]any{"displayName": "no slug", "models": []any{}},
		map[string]any{"publicGroupSlug": "group-b", "displayName": "Group B", "models": "not-an-array"},
	}
}

func readPayload(t *testing.T, store PublicStatusStore, hasGroups *bool, configVersion *string) (PublicStatusPayload, []string) {
	t.Helper()
	reasons := make([]string, 0)
	payload := ReadPublicStatusPayload(context.Background(), store, ReadPublicStatusPayloadInput{
		IntervalMinutes:     5,
		RangeHours:          24,
		NowISO:              fixtureNowISO,
		ConfigVersion:       configVersion,
		HasConfiguredGroups: hasGroups,
		TriggerRebuildHint:  func(reason string) { reasons = append(reasons, reason) },
	})
	return payload, reasons
}

func TestReadPayloadNoConfiguredGroupsYieldsNoData(t *testing.T) {
	store := newFakeStatusStore()
	seventy := false
	payload, reasons := readPayload(t, store, &seventy, nil)

	if payload.RebuildState != ServeStateNoData {
		t.Fatalf("没配分组应报 no-data，收到 %q", payload.RebuildState)
	}
	if len(reasons) != 0 {
		t.Fatalf("no-data 不该触发重建提示，收到 %v", reasons)
	}
	if payload.Groups == nil {
		t.Fatal("groups 必须是空数组而不是 nil（前端按恒有字段解析）")
	}
}

func TestReadPayloadWithoutStoreReportsRedisUnavailable(t *testing.T) {
	payload, reasons := readPayload(t, nil, nil, nil)

	if payload.RebuildState != ServeStateRebuilding {
		t.Fatalf("无 Redis 应报 rebuilding，收到 %q", payload.RebuildState)
	}
	if len(reasons) != 1 || reasons[0] != "redis-unavailable" {
		t.Fatalf("提示原因应为 redis-unavailable，收到 %v", reasons)
	}
}

func TestReadPayloadFreshPathSanitizesGroups(t *testing.T) {
	store := newFakeStatusStore()
	seedConfigSnapshot(store, 1, 5, 24)
	lastComplete := fixtureGeneration
	seedManifest(store, "", &lastComplete, fixtureFreshUntil, "idle", pointerTo(true), fixtureVersion, "")
	seedSnapshot(store, "", fixtureGeneration, publicGroupFixture())

	version := fixtureVersion
	payload, reasons := readPayload(t, store, pointerTo(true), &version)

	if payload.RebuildState != ServeStateFresh {
		t.Fatalf("新鲜快照应报 fresh，收到 %q（提示：%v）", payload.RebuildState, reasons)
	}
	if payload.SourceGeneration != fixtureGeneration {
		t.Fatalf("sourceGeneration 不符：%q", payload.SourceGeneration)
	}
	if payload.GeneratedAt == nil || *payload.GeneratedAt != fixtureNowISO {
		t.Fatalf("generatedAt 不符：%v", payload.GeneratedAt)
	}
	// 载荷层只做**字段白名单**：缺 slug 的那条被丢弃，而「slug/displayName 齐全但 models 不是数组」
	// 那条会被保留（models 归一为空数组）——与 Node 的 sanitizeGroupSnapshots 同判。
	// 「models 为空的组不进响应」是**装配层**（filterPublicStatusGroups）的活，不在这里。
	if len(payload.Groups) != 2 {
		t.Fatalf("载荷层应保留 2 组（缺 slug 的那条被丢），还剩 %d 组", len(payload.Groups))
	}
	group := payload.Groups[0]
	if group.PublicGroupSlug != "group-a" || len(group.Models) != 1 {
		t.Fatalf("分组净化不符：%+v", group)
	}
	if payload.Groups[1].PublicGroupSlug != "group-b" || len(payload.Groups[1].Models) != 0 {
		t.Fatalf("models 非数组时应归一为空数组：%+v", payload.Groups[1])
	}
	model := group.Models[0]
	if model.LatestTTFTMs == nil || *model.LatestTTFTMs != 120 {
		t.Fatalf("latestTtfbMs 应回落到 latestTtftMs：%v", model.LatestTTFTMs)
	}
	if len(model.Timeline) != 1 || model.Timeline[0].TTFTMs == nil || *model.Timeline[0].TTFTMs != 110 {
		t.Fatalf("桶的 ttfbMs 应回落到 ttftMs：%+v", model.Timeline)
	}
	if model.Timeline[0].SampleCount != 12 {
		t.Fatalf("sampleCount 不符：%v", model.Timeline[0].SampleCount)
	}
}

// TestReadPayloadNeverLeaksPrivateFields 是防泄露钉子：快照在 Redis 里跨版本持久化，可能是
// 旧版本写的、带内部字段的记录。响应正文里出现任一内部键即视为泄露。
func TestReadPayloadNeverLeaksPrivateFields(t *testing.T) {
	store := newFakeStatusStore()
	seedConfigSnapshot(store, 1, 5, 24)
	lastComplete := fixtureGeneration
	seedManifest(store, "", &lastComplete, fixtureFreshUntil, "idle", pointerTo(true), fixtureVersion, "")
	seedSnapshot(store, "", fixtureGeneration, publicGroupFixture())

	version := fixtureVersion
	payload, _ := readPayload(t, store, pointerTo(true), &version)

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	forbidden := []string{
		"sourceGroupId", "sourceGroupName", "price", "providerUrl", "secret",
		"providerId", "errorMessages", "sk-do-not-leak",
	}
	for _, key := range forbidden {
		if strings.Contains(string(encoded), key) {
			t.Fatalf("公开载荷里出现了内部字段 %q：%s", key, string(encoded))
		}
	}
}

func TestReadPayloadStaleWhenFreshUntilPassed(t *testing.T) {
	store := newFakeStatusStore()
	seedConfigSnapshot(store, 1, 5, 24)
	lastComplete := fixtureGeneration
	// freshUntil 在**过去**（fixtureStaleUntil < now）→ 判 stale。
	seedManifest(store, "", &lastComplete, fixtureStaleUntil, "idle", pointerTo(true), fixtureVersion, "")
	seedSnapshot(store, "", fixtureGeneration, publicGroupFixture())

	version := fixtureVersion
	payload, reasons := readPayload(t, store, pointerTo(true), &version)

	if payload.RebuildState != ServeStateStale {
		t.Fatalf("过期快照应报 stale，收到 %q", payload.RebuildState)
	}
	if len(reasons) != 1 || reasons[0] != "stale-generation" {
		t.Fatalf("应提示 stale-generation，收到 %v", reasons)
	}
}

func TestReadPayloadFallsBackToLegacyPrefixAndReportsReasons(t *testing.T) {
	store := newFakeStatusStore()
	seedConfigSnapshot(store, 1, 5, 24)
	lastComplete := fixtureGeneration
	// 只有 legacy v1 前缀下有可服务的代。
	seedManifest(store, LegacyPublicStatusRedisPrefix, &lastComplete, fixtureFreshUntil, "idle", pointerTo(true), fixtureVersion, "")
	seedSnapshot(store, LegacyPublicStatusRedisPrefix, fixtureGeneration, publicGroupFixture())

	version := fixtureVersion
	payload, reasons := readPayload(t, store, pointerTo(true), &version)

	if payload.RebuildState != ServeStateStale {
		t.Fatalf("legacy 回退必须降级为 stale，收到 %q", payload.RebuildState)
	}
	if len(reasons) == 0 {
		t.Fatal("legacy 回退应留下提示")
	}
	joined := strings.Join(reasons, ",")
	for _, want := range []string{"stale-generation", "legacy-generation"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("提示缺 %s：%v", want, reasons)
		}
	}
}

func TestReadPayloadIncompleteRollupCoveragePrefersLegacySnapshot(t *testing.T) {
	store := newFakeStatusStore()
	seedConfigSnapshot(store, 1, 5, 24)
	lastComplete := fixtureGeneration
	seedManifest(store, "", &lastComplete, fixtureFreshUntil, "idle", pointerTo(false), fixtureVersion, "")
	seedSnapshot(store, "", fixtureGeneration, publicGroupFixture())
	seedManifest(store, LegacyPublicStatusRedisPrefix, &lastComplete, fixtureFreshUntil, "idle", pointerTo(true), fixtureVersion, "")
	seedSnapshot(store, LegacyPublicStatusRedisPrefix, fixtureGeneration, publicGroupFixture())

	version := fixtureVersion
	payload, reasons := readPayload(t, store, pointerTo(true), &version)

	if payload.RebuildState != ServeStateStale {
		t.Fatalf("rollup 未覆盖应服务 legacy 并降级 stale，收到 %q", payload.RebuildState)
	}
	joined := strings.Join(reasons, ",")
	if !strings.Contains(joined, "rollup-coverage-incomplete") {
		t.Fatalf("应提示 rollup-coverage-incomplete：%v", reasons)
	}
}

func TestReadPayloadConfigVersionMismatchDowngradesToStale(t *testing.T) {
	store := newFakeStatusStore()
	seedConfigSnapshot(store, 1, 5, 24)
	lastComplete := fixtureGeneration
	// manifest 建在 cfg-1 的**键**上，但正文里写的是 cfg-0——这正是「版本指针滞后」的真实形态。
	seedManifest(store, "", &lastComplete, fixtureFreshUntil, "idle", pointerTo(true), fixtureVersion, "cfg-0")
	seedSnapshot(store, "", fixtureGeneration, publicGroupFixture())

	version := fixtureVersion
	payload, reasons := readPayload(t, store, pointerTo(true), &version)

	if payload.RebuildState != ServeStateStale {
		t.Fatalf("版本不匹配必须降级 stale，收到 %q", payload.RebuildState)
	}
	if !strings.Contains(strings.Join(reasons, ","), "config-version-mismatch") {
		t.Fatalf("应提示 config-version-mismatch：%v", reasons)
	}
}

func TestReadPayloadSnapshotMissingReportsSnapshotMissing(t *testing.T) {
	store := newFakeStatusStore()
	lastComplete := fixtureGeneration
	seedManifest(store, "", &lastComplete, fixtureFreshUntil, "idle", pointerTo(true), fixtureVersion, "")
	// 故意不写快照。
	version := fixtureVersion
	_, reasons := readPayload(t, store, pointerTo(true), &version)

	if len(reasons) != 1 || reasons[0] != "snapshot-missing" {
		t.Fatalf("应提示 snapshot-missing，收到 %v", reasons)
	}
}

func TestReadPayloadManifestMissingReportsManifestMissing(t *testing.T) {
	store := newFakeStatusStore()
	_, reasons := readPayload(t, store, pointerTo(true), nil)

	if len(reasons) != 1 || reasons[0] != "manifest-missing" {
		t.Fatalf("应提示 manifest-missing，收到 %v", reasons)
	}
}

func TestReadPayloadRebuildGoingOnIsStaleNotFresh(t *testing.T) {
	store := newFakeStatusStore()
	seedConfigSnapshot(store, 1, 5, 24)
	lastComplete := fixtureGeneration
	seedManifest(store, "", &lastComplete, fixtureFreshUntil, "rebuilding", pointerTo(true), fixtureVersion, "")
	seedSnapshot(store, "", fixtureGeneration, publicGroupFixture())

	version := fixtureVersion
	payload, _ := readPayload(t, store, pointerTo(true), &version)

	if payload.RebuildState != ServeStateStale {
		t.Fatalf("新鲜期内后台在重建应报 stale（不因后台干活而变灰），收到 %q", payload.RebuildState)
	}
}

func TestScheduleRebuildWritesHintAndMarksManifest(t *testing.T) {
	store := newFakeStatusStore()
	lastComplete := fixtureGeneration
	seedManifest(store, "", &lastComplete, fixtureFreshUntil, "idle", pointerTo(true), fixtureVersion, "")
	manifestKey, _ := BuildManifestKey(fixtureVersion, 5, 24, "")
	store.ttls[manifestKey] = 30 * time.Minute
	store.values[BuildInternalConfigSnapshotKey(fixtureVersion)] = `{"configVersion":"cfg-1","groups":[]}`
	store.values[BuildConfigVersionPointerKey()] = fixtureVersion

	result := SchedulePublicStatusRebuild(context.Background(), store, ScheduleRebuildInput{
		IntervalMinutes: 5, RangeHours: 24, Reason: "manifest-missing",
	})

	if !result.Accepted {
		t.Fatal("应接受重建请求")
	}
	hintKey, _ := BuildRebuildHintKey(5, 24, "")
	if result.Key != hintKey {
		t.Fatalf("提示键不符：%q", result.Key)
	}
	if ttl, ok := store.ttls[hintKey]; !ok || ttl != rebuildHintTTL {
		t.Fatalf("提示键 TTL 应为 %v，收到 %v（存在=%v）", rebuildHintTTL, ttl, ok)
	}

	var hint struct {
		Reason          string `json:"reason"`
		RequestedAt     string `json:"requestedAt"`
		IntervalMinutes int    `json:"intervalMinutes"`
		RangeHours      int    `json:"rangeHours"`
	}
	if err := json.Unmarshal([]byte(store.values[hintKey]), &hint); err != nil {
		t.Fatalf("提示正文不是 JSON: %v", err)
	}
	if hint.Reason != "manifest-missing" || hint.IntervalMinutes != 5 || hint.RangeHours != 24 || hint.RequestedAt == "" {
		t.Fatalf("提示正文不符：%+v", hint)
	}

	var manifest map[string]any
	if err := json.Unmarshal([]byte(store.values[manifestKey]), &manifest); err != nil {
		t.Fatalf("manifest 被写坏: %v", err)
	}
	if manifest["rebuildState"] != "rebuilding" {
		t.Fatalf("manifest 的 rebuildState 应改为 rebuilding：%+v", manifest)
	}
	// TTL 必须保留：写成永不过期会让过期快照永远不再被判 stale。
	if ttl := store.ttls[manifestKey]; ttl != 30*time.Minute {
		t.Fatalf("manifest 重写必须保留原 TTL，收到 %v", ttl)
	}
}

func TestScheduleRebuildIsIdempotentWhileHintAlive(t *testing.T) {
	store := newFakeStatusStore()
	hintKey, _ := BuildRebuildHintKey(5, 24, "")
	store.values[hintKey] = `{"reason":"earlier"}`
	store.ttls[hintKey] = 4 * time.Minute

	result := SchedulePublicStatusRebuild(context.Background(), store, ScheduleRebuildInput{
		IntervalMinutes: 5, RangeHours: 24, Reason: "manifest-missing",
	})

	if !result.Accepted || result.RebuildState != ServeStateRebuilding {
		t.Fatalf("已有提示时应直接接受并报 rebuilding：%+v", result)
	}
	if store.values[hintKey] != `{"reason":"earlier"}` {
		t.Fatal("已有提示不该被重写（重写会把 TTL 续成永久，且让「已排队」判定失效）")
	}
	for _, call := range store.setCalls {
		if call.key == hintKey {
			t.Fatal("已有提示时不该再写提示键")
		}
	}
}

func TestScheduleRebuildSkipsWhenStoreNotReady(t *testing.T) {
	store := newFakeStatusStore()
	store.notReady = true
	result := SchedulePublicStatusRebuild(context.Background(), store, ScheduleRebuildInput{
		IntervalMinutes: 5, RangeHours: 24, Reason: "manifest-missing",
	})
	if result.Accepted {
		t.Fatal("Redis 不可用时不该报 accepted（Node 的 getReadyRedisClient 返回 null 就 return）")
	}
	if len(store.setCalls) != 0 {
		t.Fatalf("Redis 不可用时不该写任何键，收到 %+v", store.setCalls)
	}
}

func TestScheduleRebuildWithoutStoreIsNotAccepted(t *testing.T) {
	result := SchedulePublicStatusRebuild(context.Background(), nil, ScheduleRebuildInput{
		IntervalMinutes: 5, RangeHours: 24, Reason: "manifest-missing",
	})
	if result.Accepted {
		t.Fatal("没有 Redis 时不该报 accepted")
	}
	if result.RebuildState != ServeStateRebuilding {
		t.Fatalf("没有 Redis 时服务态应为 rebuilding，收到 %q", result.RebuildState)
	}
}

func TestReadCurrentConfigSnapshotResolvesVersionPointerAndLegacy(t *testing.T) {
	store := newFakeStatusStore()
	seedConfigSnapshot(store, 2, 15, 72)

	snapshot := ReadCurrentConfigSnapshot(context.Background(), store)
	if snapshot == nil {
		t.Fatal("应能按版本指针读到配置快照")
	}
	if snapshot.ConfigVersion != fixtureVersion || snapshot.DefaultIntervalMinutes != 15 || snapshot.DefaultRangeHours != 72 {
		t.Fatalf("快照字段不符：%+v", snapshot)
	}
	if len(snapshot.Groups) != 2 {
		t.Fatalf("分组数不符：%d", len(snapshot.Groups))
	}

	// 只有 legacy 前缀有快照时也要能读到（与 pubmeta 的读取器同一份查找顺序）。
	legacy := newFakeStatusStore()
	raw := `{"configVersion":"cfg-legacy","siteTitle":"Old","siteDescription":"d","timeZone":null,
		"defaultIntervalMinutes":5,"defaultRangeHours":24,"groups":[]}`
	legacy.values[LegacyPublicStatusRedisPrefix+":config-version:current"] = "cfg-legacy"
	legacy.values[LegacyPublicStatusRedisPrefix+":config:cfg-legacy"] = strings.ReplaceAll(raw, "\n", "")
	if got := ReadCurrentConfigSnapshot(context.Background(), legacy); got == nil || got.ConfigVersion != "cfg-legacy" {
		t.Fatalf("legacy 前缀应能读到，收到 %+v", got)
	}
}

func TestReadCurrentConfigSnapshotToleratesBrokenJSON(t *testing.T) {
	store := newFakeStatusStore()
	store.values[BuildConfigVersionPointerKey()] = fixtureVersion
	store.values[BuildConfigSnapshotKey(fixtureVersion)] = "{not json"
	if got := ReadCurrentConfigSnapshot(context.Background(), store); got != nil {
		t.Fatalf("坏 JSON 应归一为「无快照」，收到 %+v", got)
	}
	if got := ReadCurrentConfigSnapshot(context.Background(), nil); got != nil {
		t.Fatal("store 为 nil 时应返回 nil")
	}
}

// TestReadPayloadHandlesLargeSnapshot 覆盖「大库」场景：本读路径**完全不碰 Postgres**
// （它只读 Redis 里预聚合好的快照），所以所谓「大」指的是**快照体积**——一个站点可能公开
// 几十个分组、上千个模型。这条用例钉住两件事：
//
//  1. 大快照下不丢条目、不退化（按白名单逐条重建，不因规模而跳过）；
//  2. 读路径没有对条目数的二次方开销（1000 个模型在毫秒级读完）。
func TestReadPayloadHandlesLargeSnapshot(t *testing.T) {
	const groupCount = 40
	const modelsPerGroup = 25

	store := newFakeStatusStore()
	seedConfigSnapshot(store, 1, 5, 24)
	lastComplete := fixtureGeneration
	seedManifest(store, "", &lastComplete, fixtureFreshUntil, "idle", pointerTo(true), fixtureVersion, "")

	groups := make([]any, 0, groupCount)
	for groupIndex := 0; groupIndex < groupCount; groupIndex++ {
		models := make([]any, 0, modelsPerGroup)
		for modelIndex := 0; modelIndex < modelsPerGroup; modelIndex++ {
			models = append(models, map[string]any{
				"publicModelKey":   fmt.Sprintf("model-%d-%d", groupIndex, modelIndex),
				"label":            fmt.Sprintf("Model %d-%d", groupIndex, modelIndex),
				"vendorIconKey":    "vendor",
				"requestTypeBadge": "openaiCompatible",
				"latestState":      "operational",
				"latestTps":        float64(10),
				"timeline":         []any{},
			})
		}
		groups = append(groups, map[string]any{
			"publicGroupSlug": fmt.Sprintf("group-%d", groupIndex),
			"displayName":     fmt.Sprintf("Group %d", groupIndex),
			"models":          models,
		})
	}
	seedSnapshot(store, "", fixtureGeneration, groups)

	version := fixtureVersion
	started := time.Now()
	payload, reasons := readPayload(t, store, pointerTo(true), &version)
	elapsed := time.Since(started)

	if payload.RebuildState != ServeStateFresh || len(reasons) != 0 {
		t.Fatalf("大快照应正常服务：state=%q reasons=%v", payload.RebuildState, reasons)
	}
	if len(payload.Groups) != groupCount {
		t.Fatalf("分组数不符：want %d got %d", groupCount, len(payload.Groups))
	}
	totalModels := 0
	for _, group := range payload.Groups {
		totalModels += len(group.Models)
	}
	if totalModels != groupCount*modelsPerGroup {
		t.Fatalf("模型总数不符：want %d got %d", groupCount*modelsPerGroup, totalModels)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("大快照读取过慢（疑似二次方开销）：%v", elapsed)
	}
}
