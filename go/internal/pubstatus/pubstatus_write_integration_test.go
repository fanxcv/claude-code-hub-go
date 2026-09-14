package pubstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 真 Redis 集成：`CCH_TEST_REDIS_URL` 未设时跳过。
//
// 内存假实现验不到的三件事，只能在这里说清：
//  1. **键名与真 Redis 的字节语义**：桶键里嵌着 URL 编码的时刻，字段名里嵌着 `|` 分隔的三段——
//     编码错一个字节，读路径（已发货的那份）就读不到；
//  2. **TTL 语义**：桶与覆盖起点的 32 天、投影的保留期（30 天，受高并发模式压制）、
//     以及 coverage-start 的 **SET NX 只写一次**（每次续 TTL）；
//  3. **读写两侧真的接得上**：本包写出的快照/manifest 由**已发货的读侧类型与状态解析器**
//     读回（`PublicStatusSnapshotRecord` + `ResolveManifestState`），而不是我自己再解析一遍。
func TestIntegrationPublicStatusWriteAgainstRealRedis(t *testing.T) {
	redisURL := os.Getenv("CCH_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过集成测试")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败: %v", err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("连不上测试 Redis: %v", err)
	}

	// 用带 nonce 的**前缀**隔离（rollup 桶键里不含版本段，用非默认前缀才不会踩到真实数据）。
	nonce := time.Now().UnixNano()
	prefix := fmt.Sprintf("public-status:it-write-%d", nonce)
	version := fmt.Sprintf("cfg-it-%d", nonce)
	logger := logx.New(os.Stderr)

	// 夹具：内部配置快照（版本指针 + 快照本体）。用**手写字面键**复刻 redis-contract.ts 形状。
	configPointerKey := prefix + ":config-version:current"
	configInternalKey := prefix + ":config-internal:" + encodeKeyPart(version)
	internalSnapshot := InternalPublicStatusConfigSnapshot{
		ConfigVersion:          version,
		SiteTitle:              "CC Hub",
		SiteDescription:        "",
		TimeZone:               ptrString("Asia/Shanghai"),
		DefaultIntervalMinutes: 15,
		DefaultRangeHours:      24,
		Groups: []InternalPublicStatusGroupSnapshot{{
			SourceGroupID:   ptrInt64(1),
			SourceGroupName: "default",
			Slug:            "default",
			DisplayName:     "Default",
			SortOrder:       0,
			Description:     ptrString("默认分组"),
			Models: []PublicStatusModelSnapshot{{
				PublicModelKey: "claude-sonnet-4", Label: "Sonnet 4", RequestTypeBadge: "chat",
			}},
		}},
		GeneratedAt: "2026-09-13T00:00:00.000Z",
	}
	snapshotJSON, err := json.Marshal(internalSnapshot)
	if err != nil {
		t.Fatalf("序列化内部快照失败: %v", err)
	}
	if err := client.Set(ctx, configPointerKey, version, 0).Err(); err != nil {
		t.Fatalf("写版本指针失败: %v", err)
	}
	if err := client.Set(ctx, configInternalKey, string(snapshotJSON), 0).Err(); err != nil {
		t.Fatalf("写内部快照失败: %v", err)
	}

	// 本轮用到的所有键都由前缀限定，故清场只需按前缀扫删。
	t.Cleanup(func() {
		keys, _ := client.Keys(ctx, prefix+"*").Result()
		if len(keys) > 0 {
			_ = client.Del(ctx, keys...).Err()
		}
	})

	// ---- 1) 事件写入：真 Redis 上的哈希累加与 TTL ----
	writer := NewRedisRollupWriter(client)
	groups := ConfiguredGroupsFromSnapshot(&internalSnapshot)
	if len(groups) != 1 {
		t.Fatalf("配置快照应解析出 1 个分组，实际 %d", len(groups))
	}
	// 必须落在窗口 [前一日00:00, 今日00:00) 内：now=00:10 且 interval=15 时末端下取整到 00:00。
	createdAt := time.Date(2026, 9, 12, 23, 56, 0, 0, time.UTC)
	event := RollupEvent{
		CreatedAt:    createdAt,
		Model:        ptrString("claude-sonnet-4"),
		DurationMs:   ptrFloat(4000),
		TTFTMs:       ptrFloat(700.5),
		FirstByteMs:  ptrFloat(400),
		OutputTokens: ptrInt64(800),
		ProviderChain: []ProviderChainItem{{
			StatusCode: ptrInt(200), Reason: ptrString("request_success"), GroupTag: ptrString("default"),
		}},
	}
	result, err := WriteRollupEvent(ctx, writer, event, groups, prefix)
	if err != nil {
		t.Fatalf("写 rollup 事件失败: %v", err)
	}
	if !result.Written || result.IncrementCount != 5 {
		t.Fatalf("事件应写入 5 条增量（success + ttfb_sum/count + tps_sum/count），实际 %+v", result)
	}

	bucketKey := result.Key
	fields, err := client.HGetAll(ctx, bucketKey).Result()
	if err != nil {
		t.Fatalf("读桶失败: %v", err)
	}
	// 计数类字段是精确整数，可以逐字节比。
	wantExact := map[string]string{
		BuildRollupField("1", "claude-sonnet-4", RollupMetricSuccess):   "1",
		BuildRollupField("1", "claude-sonnet-4", RollupMetricTTFbCount): "1",
		BuildRollupField("1", "claude-sonnet-4", RollupMetricTPSCount):  "1",
	}
	for field, want := range wantExact {
		if got := fields[field]; got != want {
			t.Fatalf("桶字段 %s 的值不一致：Go 写入后为 %q，应为 %q（全部字段：%v）", field, got, want, fields)
		}
	}
	// **浮点类字段比的是「值」，不是「字符串」**——真 Redis 的 HINCRBYFLOAT 按完整精度回写
	// （实测 `222.22219999999999999`），不保留 4 位小数。跨语言契约是「送进 Redis 的 float64
	// 相同」：两侧都由 `Number(x.toFixed(4))` / `toFixed4` 得到同一个 float64，
	// 而字符串是 Redis 自己的十进制展开。故这里解析后比 float64。
	wantFloats := map[string]float64{
		BuildRollupField("1", "claude-sonnet-4", RollupMetricTTFbSum): 700.5,
		// tps = 800 / ((4000-400)/1000) = 222.2222…（4 位舍入）
		BuildRollupField("1", "claude-sonnet-4", RollupMetricTPSSum): 222.2222,
	}
	for field, want := range wantFloats {
		raw, ok := fields[field]
		if !ok {
			t.Fatalf("桶里缺字段 %s（全部字段：%v）", field, fields)
		}
		got, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			t.Fatalf("字段 %s 的值不是数：%q", field, raw)
		}
		if got != want {
			t.Fatalf("桶字段 %s 的浮点值不一致：Go 写入后解析为 %v，应为 %v（原始串 %q）", field, got, want, raw)
		}
	}

	bucketTTL, err := client.TTL(ctx, bucketKey).Result()
	if err != nil {
		t.Fatalf("读桶 TTL 失败: %v", err)
	}
	if bucketTTL < time.Duration(RollupTTLSeconds-5)*time.Second || bucketTTL > time.Duration(RollupTTLSeconds)*time.Second {
		t.Fatalf("桶 TTL 应约为 %ds，实际 %s", RollupTTLSeconds, bucketTTL)
	}

	// coverage-start：第一次写入落桶起点，之后的写入**不得覆盖**（NX），但每次续 TTL。
	coverageKey, err := BuildRollupCoverageStartKey(PublicStatusRollupBucketMinutes, prefix)
	if err != nil {
		t.Fatalf("构造 coverage-start 键失败: %v", err)
	}
	firstCoverage, err := client.Get(ctx, coverageKey).Result()
	if err != nil {
		t.Fatalf("读 coverage-start 失败: %v", err)
	}
	firstBucketStart, err := AlignBucketStartUTC(createdAt.Format(isoMilliLayout), PublicStatusRollupBucketMinutes)
	if err != nil {
		t.Fatalf("对齐失败: %v", err)
	}
	if firstCoverage != firstBucketStart {
		t.Fatalf("coverage-start 应为首个桶起点 %s，实际 %s", firstBucketStart, firstCoverage)
	}
	laterEvent := event
	laterEvent.CreatedAt = createdAt.Add(2 * time.Minute)
	if _, err := WriteRollupEvent(ctx, writer, laterEvent, groups, prefix); err != nil {
		t.Fatalf("第二次写事件失败: %v", err)
	}
	secondCoverage, err := client.Get(ctx, coverageKey).Result()
	if err != nil {
		t.Fatalf("第二次读 coverage-start 失败: %v", err)
	}
	if secondCoverage != firstCoverage {
		t.Fatalf("coverage-start 被覆盖了（NX 语义失效）：首次 %s → 现在 %s", firstCoverage, secondCoverage)
	}
	coverageTTL, err := client.TTL(ctx, coverageKey).Result()
	if err != nil {
		t.Fatalf("读 coverage-start TTL 失败: %v", err)
	}
	if coverageTTL <= 0 || coverageTTL > time.Duration(RollupTTLSeconds)*time.Second {
		t.Fatalf("coverage-start TTL 应被续到约 %ds，实际 %s", RollupTTLSeconds, coverageTTL)
	}

	// ---- 2) worker：真 Redis 上跑一次重建 ----
	projectionRedis := NewRedisProjectionRedis(client)
	now := time.Date(2026, 9, 13, 0, 10, 0, 0, time.UTC)
	rebuild, err := RebuildProjection(ctx, RebuildOptions{
		Redis:           projectionRedis,
		IntervalMinutes: 15,
		RangeHours:      24,
		Prefix:          prefix,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	if rebuild.Status != RebuildStatusUpdated {
		t.Fatalf("重建应产出 updated，实际 %+v", rebuild)
	}
	if rebuild.Outcome.PromotedCurrent != true {
		t.Fatalf("首次重建应提升 current manifest，实际 %+v", rebuild.Outcome)
	}

	snapshotKey := rebuild.Outcome.SnapshotKey
	rawSnapshot, err := client.Get(ctx, snapshotKey).Result()
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}

	// **读侧已发货的类型**解析我写出的正文：这才是「两侧接得上」的证据。
	var snapshotRecord PublicStatusSnapshotRecord
	if err := json.Unmarshal([]byte(rawSnapshot), &snapshotRecord); err != nil {
		t.Fatalf("快照无法被读侧类型解析: %v（正文前 200 字：%.200s）", err, rawSnapshot)
	}
	if snapshotRecord.SourceGeneration != rebuild.SourceGeneration {
		t.Fatalf("快照里的代与重建结果不一致：%s vs %s", snapshotRecord.SourceGeneration, rebuild.SourceGeneration)
	}
	groupsJSON, err := json.Marshal(snapshotRecord.Groups)
	if err != nil {
		t.Fatalf("重新序列化快照分组失败: %v", err)
	}
	var payloadGroups []PublicStatusPayloadGroup
	if err := json.Unmarshal(groupsJSON, &payloadGroups); err != nil {
		t.Fatalf("快照分组无法被读侧 payload 类型解析: %v", err)
	}
	if len(payloadGroups) != 1 || len(payloadGroups[0].Models) != 1 {
		t.Fatalf("快照分组形状不符：%+v", payloadGroups)
	}
	model := payloadGroups[0].Models[0]
	if model.AvailabilityPct == nil || *model.AvailabilityPct != 100 {
		t.Fatalf("唯一一次成功请求应给出 100%% 可用率，实际 %+v", model.AvailabilityPct)
	}
	if model.LatestTTFTMs == nil || *model.LatestTTFTMs != 700.5 {
		t.Fatalf("latestTtftMs 应为 700.5，实际 %+v", model.LatestTTFTMs)
	}
	if model.LatestTPS == nil || *model.LatestTPS != 222.2222 {
		t.Fatalf("latestTps 应为 222.2222，实际 %+v", model.LatestTPS)
	}

	// manifest：读侧的状态解析器必须把它判成 fresh（且是同一代）。
	manifestRaw, err := client.Get(ctx, rebuild.Outcome.CurrentManifestKey).Result()
	if err != nil {
		t.Fatalf("读 current manifest 失败: %v", err)
	}
	var manifest PublicStatusManifest
	if err := json.Unmarshal([]byte(manifestRaw), &manifest); err != nil {
		t.Fatalf("manifest 无法被读侧类型解析: %v", err)
	}
	resolution := ResolveManifestState(&manifest, now.UTC().Format(isoMilliLayout))
	if resolution.RebuildState != ServeStateFresh {
		t.Fatalf("读侧应把刚写的 manifest 判成 fresh，实际 %s（manifest=%s）", resolution.RebuildState, manifestRaw)
	}
	if resolution.SourceGeneration != rebuild.SourceGeneration {
		t.Fatalf("读侧解析出的代与写入不一致：读侧=%q 写入=%q", resolution.SourceGeneration, rebuild.SourceGeneration)
	}

	// 投影 TTL：保留期 30 天（未注入保留期策略时为默认值）。
	snapshotTTL, err := client.TTL(ctx, snapshotKey).Result()
	if err != nil {
		t.Fatalf("读快照 TTL 失败: %v", err)
	}
	if snapshotTTL < time.Duration(GenerationProjectionTTLSeconds-60)*time.Second ||
		snapshotTTL > time.Duration(GenerationProjectionTTLSeconds)*time.Second {
		t.Fatalf("快照 TTL 应约为 %ds，实际 %s", GenerationProjectionTTLSeconds, snapshotTTL)
	}
	// 临时键必须已被删除（写完即删）。
	tempSnapshotKey := BuildTempKey(snapshotKey, "nonce")
	if tempSnapshotKey == snapshotKey {
		t.Fatal("临时键构造异常：应与正式键不同")
	}
	if exists, _ := client.Exists(ctx, snapshotKey+":tmp:"+"*").Result(); exists != 0 {
		t.Fatalf("临时键未被清理：%d 个", exists)
	}

	// ---- 3) 幂等：同一窗口再跑一次，代不变、正文不变 ----
	rebuildAgain, err := RebuildProjection(ctx, RebuildOptions{
		Redis:           projectionRedis,
		IntervalMinutes: 15,
		RangeHours:      24,
		Prefix:          prefix,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("二次重建失败: %v", err)
	}
	if rebuildAgain.SourceGeneration != rebuild.SourceGeneration {
		t.Fatalf("同一窗口的二次重建应得到同一代：%s vs %s",
			rebuildAgain.SourceGeneration, rebuild.SourceGeneration)
	}
	rawSnapshotAgain, err := client.Get(ctx, snapshotKey).Result()
	if err != nil {
		t.Fatalf("二次读快照失败: %v", err)
	}
	if rawSnapshotAgain != rawSnapshot {
		t.Fatalf("幂等性不成立：二次重建后快照正文变了\n前：%.300s\n后：%.300s", rawSnapshot, rawSnapshotAgain)
	}
	logger.Warn("pubstatus_write_integration_done", map[string]any{
		"prefix": prefix, "generation": rebuild.SourceGeneration,
	})
}

func ptrString(value string) *string  { return &value }
func ptrInt(value int) *int           { return &value }
func ptrInt64(value int64) *int64     { return &value }
func ptrFloat(value float64) *float64 { return &value }
