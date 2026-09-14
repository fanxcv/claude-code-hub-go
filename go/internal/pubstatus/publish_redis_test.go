package pubstatus

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件对拍**键契约**（键名、TTL、指针推进语义）——这些是跨进程硬约束，错一处公开状态页
// 就会静默读不到数据。门控与仓库其余 Redis 测试一致：未设 CCH_TEST_REDIS_URL 则跳过。
func testRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client
}

// publishFixture 是发布用例的输入：一个带两个模型的分组 + 两条价格 + 站点设置。
func publishFixture() Source {
	description := `{"version":2,"publicStatus":{"displayName":"主力","publicGroupSlug":"main",` +
		`"sortOrder":1,"explanatoryCopy":"说明","publicModels":["claude-opus-4","gpt-5.5"]}}`
	timeZone := "Asia/Shanghai"
	return Source{
		Groups: []GroupSource{{ID: 7, Name: "团队", Description: &description}},
		Prices: []ModelPriceSource{
			{ModelName: "claude-opus-4", DisplayName: "  Opus 4  ", Vendor: "not-a-known-icon-key", HasVendor: true},
			{ModelName: "gpt-5.5", LitellmProvider: "openai", HasLitellm: true},
		},
		Settings: SettingsSource{
			SiteTitle:                     "CC Hub",
			TimeZone:                      &timeZone,
			PublicStatusWindowHours:       24,
			PublicStatusAggregationMinute: 5,
		},
	}
}

func TestPublishConfigProjectionWritesContractKeys(t *testing.T) {
	client := testRedis(t)
	ctx := context.Background()
	version := "cfg-" + strconv.FormatInt(time.Now().UnixMilli(), 10)

	publicKey := "public-status:v2:config:" + version
	internalKey := "public-status:v2:config-internal:" + version
	pointerKey := "public-status:v2:config-version:current"
	t.Cleanup(func() {
		_ = client.Del(ctx, publicKey, internalKey, pointerKey).Err()
	})

	result, err := PublishCurrentProjection(ctx, client, publishFixture(), version, time.Now())
	if err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if !result.Written {
		t.Fatal("首发应写成功")
	}
	if result.ConfigVersion != version || result.Key != publicKey {
		t.Fatalf("结果字段不符: %+v", result)
	}
	if result.GroupCount != 1 {
		t.Fatalf("groupCount = %d, want 1", result.GroupCount)
	}

	for _, key := range []string{publicKey, internalKey} {
		ttl, err := client.TTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("读 %s 的 TTL 失败: %v", key, err)
		}
		wantSeconds := 30 * 24 * 60 * 60
		if ttl > time.Duration(wantSeconds)*time.Second || ttl < time.Duration(wantSeconds-60)*time.Second {
			t.Fatalf("%s 的 TTL = %v，应约为 %d 秒", key, ttl, wantSeconds)
		}
	}

	// 指针键必须**没有 TTL**（长期不发布时靠它兜住公开状态页）。
	pointer, err := client.Get(ctx, pointerKey).Result()
	if err != nil {
		t.Fatalf("读指针失败: %v", err)
	}
	if pointer != version {
		t.Fatalf("指针 = %q, want %q", pointer, version)
	}
	pointerTTL, err := client.TTL(ctx, pointerKey).Result()
	if err != nil {
		t.Fatalf("读指针 TTL 失败: %v", err)
	}
	if pointerTTL != -1 {
		t.Fatalf("指针键不该有 TTL，实际 %v", pointerTTL)
	}

	raw, err := client.Get(ctx, publicKey).Result()
	if err != nil {
		t.Fatalf("读公开快照失败: %v", err)
	}
	var snapshot PublicStatusConfigSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		t.Fatalf("解析公开快照失败: %v", err)
	}
	if snapshot.SiteTitle != "CC Hub" || snapshot.SiteDescription != "CC Hub public status" {
		t.Fatalf("站点字段不符: %+v", snapshot)
	}
	if snapshot.TimeZone == nil || *snapshot.TimeZone != "Asia/Shanghai" {
		t.Fatalf("时区不符: %v", snapshot.TimeZone)
	}
	if len(snapshot.Groups) != 1 || len(snapshot.Groups[0].Models) != 2 {
		t.Fatalf("分组形状不符: %+v", snapshot.Groups)
	}
	if snapshot.Groups[0].Slug != "main" || snapshot.Groups[0].DisplayName != "主力" {
		t.Fatalf("分组标识不符: %+v", snapshot.Groups[0])
	}
	// 价格里的 display_name 去掉首尾空白后作为标签；显式 vendor 值不是已知图标键时回退推断。
	opus := snapshot.Groups[0].Models[0]
	if opus.PublicModelKey != "claude-opus-4" || opus.Label != "Opus 4" {
		t.Fatalf("模型标签不符: %+v", opus)
	}
	if opus.VendorIconKey != "anthropic" || opus.RequestTypeBadge != "anthropic" {
		t.Fatalf("模型图标/徽标不符: %+v", opus)
	}
	if snapshot.Groups[0].Models[1].VendorIconKey != "openai" {
		t.Fatalf("litellm_provider 应回退成图标键: %+v", snapshot.Groups[0].Models[1])
	}
}

func TestPublishConfigProjectionPointerRejectsOlderVersion(t *testing.T) {
	client := testRedis(t)
	ctx := context.Background()
	newer := "cfg-9999999999999"
	older := "cfg-1111111111111"
	pointerKey := "public-status:v2:config-version:current"
	t.Cleanup(func() {
		_ = client.Del(ctx, pointerKey,
			"public-status:v2:config:"+newer, "public-status:v2:config-internal:"+newer,
			"public-status:v2:config:"+older, "public-status:v2:config-internal:"+older).Err()
	})

	first, err := PublishCurrentProjection(ctx, client, publishFixture(), newer, time.Now())
	if err != nil || !first.Written {
		t.Fatalf("新版本发布应成功: written=%v err=%v", first.Written, err)
	}
	second, err := PublishCurrentProjection(ctx, client, publishFixture(), older, time.Now())
	if err != nil {
		t.Fatalf("老版本发布不该报错（只是不推进指针）: %v", err)
	}
	if second.Written {
		t.Fatal("老版本不该推进指针，written 应为 false")
	}
	pointer, err := client.Get(ctx, pointerKey).Result()
	if err != nil {
		t.Fatalf("读指针失败: %v", err)
	}
	if pointer != newer {
		t.Fatalf("指针被老版本推回去了: %q", pointer)
	}
	if exists := client.Exists(ctx, "public-status:v2:config:"+older).Val(); exists != 1 {
		t.Fatal("版本化键应照写（Node 同样先写后比指针）")
	}
}

func TestPublishCurrentProjectionWithoutRedisIsNotWritten(t *testing.T) {
	result, err := PublishCurrentProjection(context.Background(), nil, publishFixture(), "cfg-1", time.Now())
	if err != nil {
		t.Fatalf("无 Redis 时不该报错: %v", err)
	}
	if result.Written {
		t.Fatal("无 Redis 时 written 应为 false（调用方据此回警告码）")
	}
	if result.ConfigVersion != "cfg-1" {
		t.Fatalf("configVersion = %q", result.ConfigVersion)
	}
}

// TestBuildProjectionPinsShapeAndKeyOrder 钉住「同名模型只取一条价格」、内部快照的源分组字段
// 与两套产物的键序（键序要逐字节对齐 Node）。
func TestBuildProjectionPinsShapeAndKeyOrder(t *testing.T) {
	source := publishFixture()
	source.Prices = append(source.Prices, ModelPriceSource{
		ModelName: "gpt-5.5", DisplayName: "重复价格行", Vendor: "openai", HasVendor: true,
	})

	public, internal, groupCount := BuildProjection(source, "cfg-1", time.UnixMilli(1700000000000))
	if groupCount != 1 {
		t.Fatalf("groupCount = %d, want 1", groupCount)
	}
	if got := public.Groups[0].Models[1].Label; got != "gpt-5.5" {
		t.Fatalf("重复价格行不该覆盖先到者，label = %q", got)
	}
	if len(internal.Groups) != 1 {
		t.Fatalf("内部快照分组数 = %d", len(internal.Groups))
	}
	if internal.Groups[0].SourceGroupID == nil || *internal.Groups[0].SourceGroupID != 7 {
		t.Fatalf("内部快照应带源分组 id: %+v", internal.Groups[0])
	}
	if internal.Groups[0].SourceGroupName != "团队" {
		t.Fatalf("内部快照应带源分组名: %+v", internal.Groups[0])
	}

	internalRaw, err := json.Marshal(internal)
	if err != nil {
		t.Fatalf("序列化内部快照失败: %v", err)
	}
	assertKeyOrder(t, "内部快照", string(internalRaw),
		`"configVersion"`, `"siteTitle"`, `"groups"`, `"generatedAt"`)

	publicRaw, err := json.Marshal(public)
	if err != nil {
		t.Fatalf("序列化公开快照失败: %v", err)
	}
	assertKeyOrder(t, "公开快照", string(publicRaw), `"configVersion"`, `"generatedAt"`, `"siteTitle"`)
}

// assertKeyOrder 断言给定键名在 JSON 文本里按顺序出现。
func assertKeyOrder(t *testing.T, label string, text string, keys ...string) {
	t.Helper()
	previous := -1
	for _, key := range keys {
		index := strings.Index(text, key)
		if index < 0 {
			t.Fatalf("%s 缺字段 %s", label, key)
		}
		if index < previous {
			t.Fatalf("%s 键序不符：%s 不在前一个键之后", label, key)
		}
		previous = index
	}
}
