package slowrate

import (
	"context"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 本文件钉住「因低速被改道请求数」的存储面（用户 2026-09-22 需求）。
//
// 为什么这些断言必须在：计数有三条各自独立、都能静默出错的规则——
//  1. 两个成因必须**分开**记（合并后运维无法区分「会话冷却」与「渠道降权」，动作不同）；
//  2. 窗口必须是**真窗口**（按小时分桶求和），不是「TTL 每次刷新的 lifetime 计数」
//     ——后者在稳定流量下永不归零，读出的数对不上任何时间范围；
//  3. 未被改道不得计数（把正常成功请求也计进去，这个数就成了「请求数」而不是「改道数」）。
//
// 三条都不会报错、不会 panic，只会让界面上的数字变成另一件事，故只能靠用例钉。

// divertTestStore 建一个指向测试 Redis 的 DivertStore；未注入 CCH_TEST_REDIS_URL 时跳过。
//
// 复用本包既有的 recoveryRedis（它与 recovery_test.go 同一门控与库号约定）。
func divertTestStore(t *testing.T) (*DivertStore, context.Context) {
	t.Helper()
	client := recoveryRedis(t)
	return NewDivertStore(client, nil), context.Background()
}

// TestDivertCountsTwoCausesSeparately 钉住两个成因分开记（冷却 1、降权 1）。
func TestDivertCountsTwoCausesSeparately(t *testing.T) {
	store, ctx := divertTestStore(t)
	now := time.Now()
	store.Record(ctx, 167, route.DivertCauseCooldown, now)
	store.Record(ctx, 167, route.DivertCausePenalty, now)

	snapshot, err := store.ReadDivert(ctx, 167, now)
	if err != nil {
		t.Fatalf("读改道计数失败: %v", err)
	}
	if snapshot.Cooldown != 1 || snapshot.Penalty != 1 {
		t.Fatalf("两个成因应各计 1，实得 cooldown=%d penalty=%d（合并成一类即本条红）",
			snapshot.Cooldown, snapshot.Penalty)
	}
	if snapshot.Total() != 2 {
		t.Fatalf("总数应为 2，实得 %d", snapshot.Total())
	}
}

// TestDivertIgnoresUncountedRequests 钉住「没被改道不计」。
//
// 这条只能靠「读出来是 0」来钉：把正常成功请求也计进去，这个数就退化成请求数。
func TestDivertIgnoresUncountedRequests(t *testing.T) {
	store, ctx := divertTestStore(t)
	now := time.Now()
	// 只录两笔，然后断言总数恰为 2——若实现把「每次终态都计」当成改道，本条会读到更大值。
	store.Record(ctx, 168, route.DivertCauseCooldown, now)
	store.Record(ctx, 168, route.DivertCauseCooldown, now)

	snapshot, err := store.ReadDivert(ctx, 169, now)
	if err != nil {
		t.Fatalf("读改道计数失败: %v", err)
	}
	if snapshot.Total() != 0 {
		t.Fatalf("未被改道的渠道应读到 0，实得 %d", snapshot.Total())
	}
	recorded, _ := store.ReadDivert(ctx, 168, now)
	if recorded.Total() != 2 {
		t.Fatalf("被改道两次应读到 2，实得 %d", recorded.Total())
	}
}

// TestDivertWindowExcludesExpiredBuckets 钉住窗口求和**不含过期桶、且含窗内多个桶**。
//
// 两半必须同时存在，否则本条对「取单桶」这种实现无分辨力：
//   - 只测「过期桶不计」——取单桶的实现也满足它（过期桶本就不在当前桶里）；
//   - 只测「窗内多桶相加」——不裁剪窗口的实现也满足它。
//
// 故本用例造三个桶：当前桶（1）、窗内另一桶（2）、25 小时前的过期桶（4）。
// 正确实现读到 3；取单桶读到 1；不裁剪读到 7。
func TestDivertWindowExcludesExpiredBuckets(t *testing.T) {
	store, ctx := divertTestStore(t)
	now := time.Now()
	// 窗内另一桶：当前整点往前 10 小时（仍在 24 桶内）。
	store.Record(ctx, 170, route.DivertCauseCooldown, now)
	store.Record(ctx, 170, route.DivertCausePenalty, now.Add(-10*time.Hour))
	// 过期桶：25 小时前（已出窗）。
	store.Record(ctx, 170, route.DivertCausePenalty, now.Add(-25*time.Hour))

	snapshot, err := store.ReadDivert(ctx, 170, now)
	if err != nil {
		t.Fatalf("读改道计数失败: %v", err)
	}
	if snapshot.Cooldown != 1 || snapshot.Penalty != 1 {
		t.Fatalf("窗口应含当前桶与 10 小时前那桶（cooldown=1 penalty=1）、排掉 25 小时前那桶；"+
			"实得 cooldown=%d penalty=%d（取单桶会少计、不裁剪会多计）",
			snapshot.Cooldown, snapshot.Penalty)
	}
	if snapshot.WindowHours != divertBuckets {
		t.Fatalf("窗口口径应随读数给出（%d 小时），实得 %d", divertBuckets, snapshot.WindowHours)
	}
}

// TestDivertBucketsAreHourAligned 钉住桶按整点对齐（同一小时内累加、跨小时分桶）。
//
// 为什么必须：求和依赖「桶起点」这一数值。若实现用「写入时刻」当字段名，
// 每个请求都自成桶、24 小时窗口会装下成千上万个字段，且求和边界无法判定。
func TestDivertBucketsAreHourAligned(t *testing.T) {
	store, ctx := divertTestStore(t)
	base := time.Date(2026, 9, 22, 10, 5, 0, 0, time.UTC)
	store.Record(ctx, 171, route.DivertCauseCooldown, base)
	store.Record(ctx, 171, route.DivertCauseCooldown, base.Add(50*time.Minute))
	// 同一整点（10:05 与 10:55）应落同一个桶；11:05 属下一桶。
	store.Record(ctx, 171, route.DivertCauseCooldown, time.Date(2026, 9, 22, 11, 5, 0, 0, time.UTC))

	fields, err := store.client.HKeys(ctx, DivertKey(171)).Result()
	if err != nil {
		t.Fatalf("列字段失败: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("三笔写入（两个整点）应只产生 2 个字段，实得 %d: %v", len(fields), fields)
	}
	snapshot, _ := store.ReadDivert(ctx, 171, time.Date(2026, 9, 22, 11, 30, 0, 0, time.UTC))
	if snapshot.Cooldown != 3 {
		t.Fatalf("两个桶都在窗口内，应读到 3，实得 %d", snapshot.Cooldown)
	}
}

// TestDivertExpireLivesLongerThanWindow 钉住 TTL 略长于窗口桶数。
//
// 若 TTL 等于窗口（24 小时），每个整点刚过时最老那一桶会被删掉，「最近 24 小时」恒少一桶。
func TestDivertExpireLivesLongerThanWindow(t *testing.T) {
	store, ctx := divertTestStore(t)
	now := time.Now()
	store.Record(ctx, 172, route.DivertCauseCooldown, now)

	ttl, err := store.client.TTL(ctx, DivertKey(172)).Result()
	if err != nil {
		t.Fatalf("取 TTL 失败: %v", err)
	}
	minTTL := time.Duration(divertBuckets) * time.Hour
	if ttl < minTTL {
		t.Fatalf("TTL 应长于窗口（>=%s），实得 %s——等长会在整点边界少掉最老那一桶", minTTL, ttl)
	}
	if ttl > divertTTL+time.Minute {
		t.Fatalf("TTL 应为 %s，实得 %s", divertTTL, ttl)
	}
}
