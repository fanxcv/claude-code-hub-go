package slowlog

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/redis/go-redis/v9"
)

// 本文件是真 Redis 集成用例：钉住「容量有界」这条**只有真 Redis 才能证**的语义。
//
// 为什么单元用例不够：单元用例的替身自己也实现了裁剪，故它只能证明「本包把 MaxLen 传对了」，
// 证不了「Redis 真的按精确 MAXLEN 裁、且裁掉的是最旧的」。这两件事的判据都在服务端。
//
// 夹具纪律：键含合成渠道 id（994xxx 段是 slowlog 的保留段），用完即删。
//
// 门控：未设 CCH_TEST_REDIS_URL 即跳过（与 internal/jobs、internal/slowrate 的既有约定一致）。

// slowLogITProviderID 是合成渠道 id（本文件专用段）。
const slowLogITProviderID int64 = 994001

func slowLogITRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	// 键按渠道 id 唯一，用完即删（不依赖 TTL，避免留下跨用例的残留）。
	t.Cleanup(func() { _ = client.Del(context.Background(), Key(slowLogITProviderID)).Err() })
	return client
}

// TestIntegrationStreamTrimsToMaxEntries 钉住真 Redis 上的容量有界：
// 写入超过上限后，条目数恰为上限，且**最旧的被裁掉、最新的留着**。
func TestIntegrationStreamTrimsToMaxEntries(t *testing.T) {
	client := slowLogITRedis(t)
	ctx := context.Background()
	if err := client.Del(ctx, Key(slowLogITProviderID)).Err(); err != nil {
		t.Fatalf("清理旧键失败: %v", err)
	}

	total := MaxEntries + 25
	for index := 0; index < total; index++ {
		Record(ctx, client, &recordingLogger{}, Event{
			Kind:       KindPenaltyUp,
			ProviderID: slowLogITProviderID,
			ModelKey:   "m" + strconv.Itoa(index),
		})
	}

	length, err := client.XLen(ctx, Key(slowLogITProviderID)).Result()
	if err != nil {
		t.Fatalf("读流长度失败: %v", err)
	}
	if length != MaxEntries {
		t.Fatalf("真 Redis 上条目数应恰为 %d（精确 MAXLEN），实际 %d", MaxEntries, length)
	}

	events, err := NewReader(client, &recordingLogger{}).
		Recent(ctx, slowLogITProviderID, MaxLimit)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	// 读面的条数上限是 MaxLimit（界面一次看不完 200 条），而**容量**上限是 MaxEntries——
	// 两者不是同一个数，故分开断言。
	if len(events) != MaxLimit {
		t.Fatalf("读回条数应为读面上限 %d，实际 %d", MaxLimit, len(events))
	}
	// 最新在前：第一条应是最后写入的。
	if events[0].ModelKey != "m"+strconv.Itoa(total-1) {
		t.Errorf("最新的应排最前，收到 %q", events[0].ModelKey)
	}
	// 最旧的已被裁掉：读回的最后一条应是第 total-MaxLimit 条（不是 m0）。
	wantOldest := "m" + strconv.Itoa(total-MaxLimit)
	if got := events[len(events)-1].ModelKey; got != wantOldest {
		t.Errorf("最旧的应被裁到 %q，实际 %q", wantOldest, got)
	}
	// TTL 必须被续上（键随写入续期到 24h 量级），否则事件会在无人写入时提前消失。
	ttl, err := client.TTL(ctx, Key(slowLogITProviderID)).Result()
	if err != nil {
		t.Fatalf("读 TTL 失败: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("键应有正 TTL，实际 %v", ttl)
	}
}

// TestIntegrationReaderKeepsNewestFirstAcrossWrites 钉住真 Redis 上的倒序读：
// 逐条写入后读回的顺序必须是「最新在前」（XREVRANGE 语义）。
func TestIntegrationReaderKeepsNewestFirstAcrossWrites(t *testing.T) {
	client := slowLogITRedis(t)
	ctx := context.Background()
	if err := client.Del(ctx, Key(slowLogITProviderID)).Err(); err != nil {
		t.Fatalf("清理旧键失败: %v", err)
	}
	from, to := 10, 30
	for index := 0; index < 3; index++ {
		Record(ctx, client, &recordingLogger{}, Event{
			Kind:        KindPenaltyUp,
			ProviderID:  slowLogITProviderID,
			ModelKey:    "ordered-" + strconv.Itoa(index),
			PenaltyFrom: &from,
			PenaltyTo:   &to,
		})
	}
	events, err := NewReader(client, &recordingLogger{}).Recent(ctx, slowLogITProviderID, DefaultLimit)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("应有 3 条，实际 %d", len(events))
	}
	for index, want := range []string{"ordered-2", "ordered-1", "ordered-0"} {
		if events[index].ModelKey != want {
			t.Errorf("第 %d 条应为 %q（最新在前），收到 %q", index, want, events[index].ModelKey)
		}
	}
	// 往返一致：指针字段也要活着回来（JSON 存的是 Redis Stream 的一个字段）。
	if events[0].PenaltyFrom == nil || *events[0].PenaltyFrom != 10 ||
		events[0].PenaltyTo == nil || *events[0].PenaltyTo != 30 {
		t.Errorf("前后值应往返一致，收到 %+v", events[0])
	}
}
