package pubstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 真 Redis 集成：`CCH_TEST_REDIS_URL` 未设时跳过。
//
// 两件在内存假实现里验不到的事：
//  1. **键名与真 Redis 的字节语义**：夹具里的键用**手写字面串**（不是本包的构造器），
//     读路径必须能找到它们——这才是「键布局与 Node 逐字一致」的独立证据（用自己的构造器
//     写、用自己的构造器读，两边同时写错也会通过）。
//  2. **TTL 语义**：提示键的 300s、manifest 重写时**保留原 TTL** —— 这两条只有在真 Redis 上
//     的 `PTTL` 才说得清。
func TestIntegrationPublicStatusReadAgainstRealRedis(t *testing.T) {
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

	// 用带 nonce 的版本段，避免与并行跑的其它测试/真实数据互踩。
	nonce := time.Now().UnixNano()
	version := fmt.Sprintf("cfg-it-%d", nonce)
	const generation = "gen-it-1"

	// **手写字面键**（复刻 redis-contract.ts 的形状），而不是调本包的构造器。
	configKey := "public-status:v2:config:" + version
	// 重建提示要按**版本化** manifest 重写，而版本号只存在于**内部**配置快照里
	// （Node 的 rebuild-hints.ts 也读 `config-internal`）：夹具漏写它的话，
	// 提示就会落到“current”版本上（本例无该键）→ manifest 不被改。
	configInternalKey := "public-status:v2:config-internal:" + version
	configPointerKey := "public-status:v2:config-version:current"
	manifestKey := fmt.Sprintf("public-status:v2:manifest:%s:5m:24h", version)
	snapshotKey := fmt.Sprintf("public-status:v2:snapshot:%s:5m:24h", generation)
	hintKey := "public-status:v2:rebuild-hint:5m:24h"
	keys := []string{configPointerKey, configKey, configInternalKey, manifestKey, snapshotKey, hintKey}
	for _, key := range keys {
		_ = client.Del(ctx, key).Err()
	}
	t.Cleanup(func() {
		for _, key := range keys {
			_ = client.Del(ctx, key).Err()
		}
	})

	configBody, _ := json.Marshal(map[string]any{
		"configVersion":          version,
		"generatedAt":            fixtureNowISO,
		"siteTitle":              "CC Hub",
		"siteDescription":        "desc",
		"timeZone":               "Asia/Shanghai",
		"defaultIntervalMinutes": 5,
		"defaultRangeHours":      24,
		"groups":                 []any{map[string]any{"slug": "group-a", "displayName": "Group A"}},
	})
	manifestBody, _ := json.Marshal(map[string]any{
		"configVersion":          version,
		"lastCompleteGeneration": generation,
		"generatedAt":            fixtureNowISO,
		"freshUntil":             goldenFreshUntil,
		"rebuildState":           "idle",
		"rollupCoverageComplete": true,
	})
	snapshotBodyRaw, _ := json.Marshal(goldenSnapshotBody())

	if err := client.Set(ctx, configPointerKey, version, 0).Err(); err != nil {
		t.Fatalf("写配置指针失败: %v", err)
	}
	if err := client.Set(ctx, configKey, string(configBody), 0).Err(); err != nil {
		t.Fatalf("写配置快照失败: %v", err)
	}
	if err := client.Set(ctx, configInternalKey, string(configBody), 0).Err(); err != nil {
		t.Fatalf("写内部配置快照失败: %v", err)
	}
	// manifest 给 30 分钟 TTL：下面要验「重写保留 TTL」。
	if err := client.Set(ctx, manifestKey, string(manifestBody), 30*time.Minute).Err(); err != nil {
		t.Fatalf("写 manifest 失败: %v", err)
	}
	if err := client.Set(ctx, snapshotKey, string(snapshotBodyRaw), 0).Err(); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}

	store := NewRedisStatusStore(client, logx.New(nil))
	if store == nil {
		t.Fatal("真客户端应能建出门面")
	}
	if !store.Ready(ctx) {
		t.Fatal("真 Redis 应报 ready")
	}

	// 一、按手写键名能读到配置快照（独立验证键布局）。
	configSnapshot := ReadCurrentConfigSnapshot(ctx, store)
	if configSnapshot == nil || configSnapshot.ConfigVersion != version {
		t.Fatalf("按字面键名应读到配置快照，收到 %+v", configSnapshot)
	}
	if len(configSnapshot.Groups) != 1 {
		t.Fatalf("分组数不符：%d", len(configSnapshot.Groups))
	}

	// 二、新鲜读路径：真 Redis 上拿到 fresh + 净化后的 groups。
	reasons := make([]string, 0)
	payload := ReadPublicStatusPayload(ctx, store, ReadPublicStatusPayloadInput{
		IntervalMinutes: 5, RangeHours: 24, NowISO: fixtureNowISO,
		ConfigVersion: &version, HasConfiguredGroups: pointerTo(true),
		TriggerRebuildHint: func(reason string) { reasons = append(reasons, reason) },
	})
	if payload.RebuildState != ServeStateFresh {
		t.Fatalf("新鲜快照应报 fresh，收到 %q（提示 %v）", payload.RebuildState, reasons)
	}
	if len(reasons) != 0 {
		t.Fatalf("新鲜路径不该触发重建提示，收到 %v", reasons)
	}
	if len(payload.Groups) != 2 {
		t.Fatalf("载荷层应保留 2 组（缺 slug 的被丢），收到 %d", len(payload.Groups))
	}

	// 三、降级读：把 manifest 的新鲜期改到过去 → stale，并在真 Redis 上留下 300s 提示键。
	staleManifest, _ := json.Marshal(map[string]any{
		"configVersion":          version,
		"lastCompleteGeneration": generation,
		"generatedAt":            fixtureNowISO,
		"freshUntil":             goldenStaleUntil,
		"rebuildState":           "idle",
		"rollupCoverageComplete": true,
	})
	if err := client.Set(ctx, manifestKey, string(staleManifest), 30*time.Minute).Err(); err != nil {
		t.Fatalf("改 manifest 失败: %v", err)
	}

	reasons = reasons[:0]
	payload = ReadPublicStatusPayload(ctx, store, ReadPublicStatusPayloadInput{
		IntervalMinutes: 5, RangeHours: 24, NowISO: fixtureNowISO,
		ConfigVersion: &version, HasConfiguredGroups: pointerTo(true),
		TriggerRebuildHint: func(reason string) {
			reasons = append(reasons, reason)
			SchedulePublicStatusRebuild(ctx, store, ScheduleRebuildInput{
				IntervalMinutes: 5, RangeHours: 24, Reason: reason, RequestedAt: fixtureNowISO,
			})
		},
	})
	if payload.RebuildState != ServeStateStale {
		t.Fatalf("过期快照应报 stale，收到 %q", payload.RebuildState)
	}
	if len(reasons) == 0 || reasons[0] != "stale-generation" {
		t.Fatalf("应提示 stale-generation，收到 %v", reasons)
	}

	ttl := client.PTTL(ctx, hintKey)
	if ttl.Err() != nil {
		t.Fatalf("读提示键 TTL 失败: %v", ttl.Err())
	}
	if ttl.Val() <= 0 || ttl.Val() > rebuildHintTTL {
		t.Fatalf("提示键 TTL 应在 (0, %v]，收到 %v", rebuildHintTTL, ttl.Val())
	}

	// 四、manifest 被改成 rebuilding，且**原 TTL 被保留**（不能变成永不过期）。
	manifestAfter := client.Get(ctx, manifestKey)
	if manifestAfter.Err() != nil {
		t.Fatalf("读 manifest 失败: %v", manifestAfter.Err())
	}
	var manifest struct {
		RebuildState string `json:"rebuildState"`
	}
	if err := json.Unmarshal([]byte(manifestAfter.Val()), &manifest); err != nil {
		t.Fatalf("manifest 不是 JSON: %v", err)
	}
	if manifest.RebuildState != "rebuilding" {
		t.Fatalf("manifest.rebuildState 应为 rebuilding，收到 %q", manifest.RebuildState)
	}
	manifestTTL := client.PTTL(ctx, manifestKey)
	if manifestTTL.Err() != nil {
		t.Fatalf("读 manifest TTL 失败: %v", manifestTTL.Err())
	}
	// 保留 TTL 的判据：仍在 (0, 30m] 内，而不是 -1（无 TTL）。
	if manifestTTL.Val() <= 0 || manifestTTL.Val() > 30*time.Minute {
		t.Fatalf("manifest 重写必须保留原 TTL（(0, 30m]），收到 %v", manifestTTL.Val())
	}

	// 五、幂等：提示键尚在时再触发一次，不该重写（TTL 不被续满）。
	before := client.PTTL(ctx, hintKey).Val()
	second := SchedulePublicStatusRebuild(ctx, store, ScheduleRebuildInput{
		IntervalMinutes: 5, RangeHours: 24, Reason: "stale-generation", RequestedAt: fixtureNowISO,
	})
	if !second.Accepted {
		t.Fatal("已有提示时应报 accepted")
	}
	after := client.PTTL(ctx, hintKey).Val()
	if after > before {
		t.Fatalf("已有提示时 TTL 不该被续长：%v → %v", before, after)
	}
}
