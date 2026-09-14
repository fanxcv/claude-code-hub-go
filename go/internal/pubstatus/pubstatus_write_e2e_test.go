package pubstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 端到端：**写侧产出 → 真 Redis → 已发货的读侧入口**，并且走的是**默认前缀**
// （即生产真实键空间，不是测试用的隔离前缀）。
//
// 为什么单列一条：隔离前缀的集成测试证明「两侧接得上」，但接的是**测试前缀**；
// 而读端点的 HTTP 层读的是默认前缀。若默认前缀下有任何键名分叉，隔离测试看不见。
//
// 两个开关（都只在本地 e2e 用，CI 默认不设）：
//   - `CCH_TEST_REDIS_URL`：未设则跳过（与其余 Redis 集成测试一致）；
//   - `CCH_E2E_KEEP_KEYS=1`：**保留**写入的键，供随即启动的真实二进制用 HTTP 读一遍
//     （见报告里的 e2e 序列）；默认会清理。
func TestE2EPublicStatusWriteThenReadDefaultPrefix(t *testing.T) {
	redisURL := os.Getenv("CCH_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过端到端测试")
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

	// 默认前缀下的键是**共享**的，故先记录写前的存在性，收尾时按需还原：
	// 这条测试绝不能把本地/生产 Redis 上已有的公开状态投影清掉。
	prefix := "" // 空串即默认前缀（public-status:v2）
	version := fmt.Sprintf("cfg-e2e-%d", time.Now().UnixNano())
	internalSnapshot := InternalPublicStatusConfigSnapshot{
		ConfigVersion:          version,
		SiteTitle:              "CC Hub",
		SiteDescription:        "",
		TimeZone:               ptrString("Asia/Shanghai"),
		DefaultIntervalMinutes: 15,
		DefaultRangeHours:      24,
		Groups: []InternalPublicStatusGroupSnapshot{{
			SourceGroupID:   ptrInt64(7),
			SourceGroupName: "e2e-group",
			Slug:            "e2e-group",
			DisplayName:     "E2E Group",
			SortOrder:       0,
			Models: []PublicStatusModelSnapshot{{
				PublicModelKey: "e2e-model", Label: "E2E Model", RequestTypeBadge: "chat",
			}},
		}},
		GeneratedAt: "2026-09-13T00:00:00.000Z",
	}
	snapshotJSON, err := json.Marshal(internalSnapshot)
	if err != nil {
		t.Fatalf("序列化内部快照失败: %v", err)
	}

	configPointerKey := prefixOr(prefix) + ":config-version:current"
	configInternalKey := prefixOr(prefix) + ":config-internal:" + encodeKeyPart(version)
	// **公开**配置快照：读端点的 HTTP 层用它取 configVersion / hasConfiguredGroups / defaults / meta
	// （adminapi/public_status_read.go:174-195）。缺它时端点会诚实报 no-data，与「投影写没写」无关。
	configPublicKey := prefixOr(prefix) + ":config:" + encodeKeyPart(version)
	publicSnapshot := map[string]any{
		"configVersion":          version,
		"generatedAt":            "2026-09-13T00:00:00.000Z",
		"siteTitle":              "CC Hub",
		"siteDescription":        "",
		"timeZone":               "Asia/Shanghai",
		"defaultIntervalMinutes": 15,
		"defaultRangeHours":      24,
		"groups": []map[string]any{{
			"slug": "e2e-group", "displayName": "E2E Group", "sortOrder": 0, "description": "E2E",
			"models": []map[string]any{{
				"publicModelKey": "e2e-model", "label": "E2E Model", "vendorIconKey": "", "requestTypeBadge": "chat",
			}},
		}},
	}
	publicSnapshotJSON, err := json.Marshal(publicSnapshot)
	if err != nil {
		t.Fatalf("序列化公开快照失败: %v", err)
	}
	previousPointer, pointerExisted := client.Get(ctx, configPointerKey).Result()
	previousPointerErr := client.Get(ctx, configPointerKey).Err()
	if previousPointerErr != nil && previousPointerErr != redis.Nil {
		t.Fatalf("读既有版本指针失败: %v", previousPointerErr)
	}
	_ = pointerExisted

	keep := os.Getenv("CCH_E2E_KEEP_KEYS") == "1"
	written := []string{configPointerKey, configInternalKey, configPublicKey}
	t.Cleanup(func() {
		if keep {
			t.Logf("CCH_E2E_KEEP_KEYS=1：保留本轮写入的键，供 HTTP e2e 使用：%v", written)
			return
		}
		if len(written) > 0 {
			_ = client.Del(ctx, written...).Err()
		}
		if previousPointerErr == nil && previousPointer != "" {
			_ = client.Set(ctx, configPointerKey, previousPointer, 0).Err()
		}
	})

	if err := client.Set(ctx, configPointerKey, version, 0).Err(); err != nil {
		t.Fatalf("写版本指针失败: %v", err)
	}
	if err := client.Set(ctx, configInternalKey, string(snapshotJSON), 0).Err(); err != nil {
		t.Fatalf("写内部快照失败: %v", err)
	}
	if err := client.Set(ctx, configPublicKey, string(publicSnapshotJSON), 0).Err(); err != nil {
		t.Fatalf("写公开快照失败: %v", err)
	}

	// ---- 写侧：真事件进真 Redis ----
	groups := ConfiguredGroupsFromSnapshot(&internalSnapshot)
	writer := NewRedisRollupWriter(client)
	now := time.Date(2026, 9, 13, 0, 10, 0, 0, time.UTC)
	// 事件落在窗口 [前一日00:00, 今日00:00) 内（展示窗口末端是 now 下取整到 15 分钟）。
	event := RollupEvent{
		CreatedAt:     time.Date(2026, 9, 12, 23, 56, 0, 0, time.UTC),
		Model:         ptrString("e2e-model"),
		DurationMs:    ptrFloat(3000),
		TTFTMs:        ptrFloat(500),
		FirstByteMs:   ptrFloat(300),
		OutputTokens:  ptrInt64(600),
		ProviderChain: []ProviderChainItem{{StatusCode: ptrInt(200), Reason: ptrString("request_success"), GroupTag: ptrString("e2e-group")}},
	}
	writeResult, err := WriteRollupEvent(ctx, writer, event, groups, prefix)
	if err != nil {
		t.Fatalf("写事件失败: %v", err)
	}
	if !writeResult.Written {
		t.Fatalf("事件应写入，实际 %+v", writeResult)
	}
	written = append(written, writeResult.Key)
	coverageKey, _ := BuildRollupCoverageStartKey(PublicStatusRollupBucketMinutes, prefix)
	written = append(written, coverageKey)

	// ---- worker：真 Redis 上重建（默认前缀）----
	rebuild, err := RebuildProjection(ctx, RebuildOptions{
		Redis:           NewRedisProjectionRedis(client),
		IntervalMinutes: 15,
		RangeHours:      24,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	if rebuild.Status != RebuildStatusUpdated {
		t.Fatalf("重建应 updated，实际 %+v", rebuild)
	}
	written = append(written,
		rebuild.Outcome.SnapshotKey, rebuild.Outcome.SeriesKey,
		rebuild.Outcome.VersionedManifestKey, rebuild.Outcome.CurrentManifestKey,
	)

	// ---- 读侧：用**已发货的读入口**读回（默认前缀，与 HTTP 层同一路径）----
	store := NewRedisStatusStore(client, nil)
	hasGroups := true
	configVersion := version
	payload := ReadPublicStatusPayload(ctx, store, ReadPublicStatusPayloadInput{
		IntervalMinutes:     15,
		RangeHours:          24,
		NowISO:              now.UTC().Format(isoMilliLayout),
		ConfigVersion:       &configVersion,
		HasConfiguredGroups: &hasGroups,
	})
	// 第一层：**覆盖不完整时必须降级**。本轮只有一条事件，coverage-start 落在窗口末端那个桶，
	// 于是 manifest 的 `rollupCoverageComplete=false`，读侧据此刻意报 stale 而不是 fresh——
	// 「只覆盖了窗口一角就宣称新鲜」是 Node 与 Go 共同禁止的（这条正是读侧已发货的判据）。
	if payload.RebuildState != ServeStateStale {
		t.Fatalf("覆盖不完整时读侧应报 stale，实际 %s（generation=%q）",
			payload.RebuildState, payload.SourceGeneration)
	}
	if payload.SourceGeneration != rebuild.SourceGeneration {
		t.Fatalf("读侧的代与写侧不一致：读=%s 写=%s", payload.SourceGeneration, rebuild.SourceGeneration)
	}
	if len(payload.Groups) != 1 || len(payload.Groups[0].Models) != 1 {
		t.Fatalf("读回的 payload 形状不符：%+v", payload.Groups)
	}
	model := payload.Groups[0].Models[0]
	if model.PublicModelKey != "e2e-model" {
		t.Fatalf("读回模型键不符：%s", model.PublicModelKey)
	}
	if model.AvailabilityPct == nil || *model.AvailabilityPct != 100 {
		t.Fatalf("读回可用率应为 100，实际 %v", model.AvailabilityPct)
	}
	if model.LatestTTFTMs == nil || *model.LatestTTFTMs != 500 {
		t.Fatalf("读回 latestTtftMs 应为 500，实际 %v", model.LatestTTFTMs)
	}
	// 第二层：把覆盖起点补到窗口起点（等价于「桶已从此前就在积累」的环境）后重跑一次，
	// 读侧必须报 fresh——这才证明确实是**覆盖门**在降级，而不是写侧的记录有问题。
	coveredFrom, _, _, err := BuildRollupBucketStarts(now, 24, 15)
	if err != nil {
		t.Fatalf("推导窗口起点失败: %v", err)
	}
	if err := client.Set(ctx, coverageKey, coveredFrom, 0).Err(); err != nil {
		t.Fatalf("写覆盖起点失败: %v", err)
	}
	rebuildFull, err := RebuildProjection(ctx, RebuildOptions{
		Redis:           NewRedisProjectionRedis(client),
		IntervalMinutes: 15,
		RangeHours:      24,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("覆盖补全后的重建失败: %v", err)
	}
	if rebuildFull.Status != RebuildStatusUpdated {
		t.Fatalf("覆盖补全后应 updated，实际 %+v", rebuildFull)
	}
	written = append(written, rebuildFull.Outcome.SnapshotKey, rebuildFull.Outcome.SeriesKey,
		rebuildFull.Outcome.VersionedManifestKey, rebuildFull.Outcome.CurrentManifestKey)
	payloadFull := ReadPublicStatusPayload(ctx, store, ReadPublicStatusPayloadInput{
		IntervalMinutes:     15,
		RangeHours:          24,
		NowISO:              now.UTC().Format(isoMilliLayout),
		ConfigVersion:       &configVersion,
		HasConfiguredGroups: &hasGroups,
	})
	if payloadFull.RebuildState != ServeStateFresh {
		t.Fatalf("覆盖完整时读侧应报 fresh，实际 %s（manifest=%s）",
			payloadFull.RebuildState, readManifestForTest(ctx, client, rebuildFull.Outcome.CurrentManifestKey))
	}
	if payloadFull.SourceGeneration != rebuildFull.SourceGeneration {
		t.Fatalf("fresh 后读侧的代应为写侧当代：读=%s 写=%s",
			payloadFull.SourceGeneration, rebuildFull.SourceGeneration)
	}
	t.Logf("E2E OK：覆盖不完整→stale、补全→fresh；generation=%s 模型 %s 可用率 %.0f%% 样本合计 %.0f",
		rebuildFull.SourceGeneration, model.PublicModelKey, *model.AvailabilityPct,
		sumSamples(model.Timeline))
}

// readManifestForTest 只为失败信息服务：把 manifest 原文带进断言输出。
func readManifestForTest(ctx context.Context, client *redis.Client, key string) string {
	raw, _ := client.Get(ctx, key).Result()
	return raw
}

func sumSamples(timeline []PublicStatusTimelineBucket) float64 {
	total := 0.0
	for _, bucket := range timeline {
		total += bucket.SampleCount
	}
	return total
}
