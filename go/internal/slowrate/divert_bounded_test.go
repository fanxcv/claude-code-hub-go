package slowrate

import (
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 本文件钉住改道计数的**存储有界性**（一桶一键 + 每键自带 TTL）。
//
// 为什么必须单独钉：旧形制（一个渠道一个 Hash + 字段）的 TTL 是**整键一条**，每次写都刷新，
// 于是「滑出窗口的桶」只能靠写时剪除清掉——而删除目标随当前整点单向递增，间断会让漏删的桶
// **永不再被触及**。实测三轮「活跃 24h + 间断 24h」后攒到 140 项（上界应为 50），且读数仍然
// 正确——也就是说这种无界增长**完全静默**，只能靠用例钉。
//
// 注意一个测试能力的边界：**真实时间的流逝在测试里不成立**（`at` 是模拟的，Redis 的 TTL 走的
// 是墙上时钟）。故「稳态下键数 ≤ 50」不能直接断言；本文件改钉那个使它成立的**机制**——
//  1. 条目与 (小时, 成因) 严格一一对应（无共享槽位，故单位时间条目数有上界）；
//  2. 每个键的寿命都绑在自己那一小时上、**不被后续写延长**；
//  3. 键真到期时，读数随之下降（自清理，无派生状态）。
// 三者合起来才是「有界」，缺一条上界就不成立。

// TestDivertBucketEntriesStayOnePerHourCause 钉住条目与 (小时, 成因) 一一对应、且每键自带 TTL。
//
// 这一条对应「连续逐小时有流量」的常态：写 30 小时 ⇒ 60 个键（不是 1 个键装 60 个字段），
// 每个键都能独立到期，故稳态条目数上界是 25h × 2 = 50。
func TestDivertBucketEntriesStayOnePerHourCause(t *testing.T) {
	store, ctx := divertTestStore(t)
	const providerID = 200
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	const hours = 30
	for hour := 0; hour < hours; hour++ {
		at := base.Add(time.Duration(hour) * time.Hour)
		store.Record(ctx, providerID, route.DivertCauseCooldown, at)
		store.Record(ctx, providerID, route.DivertCausePenalty, at)
	}

	keys, err := divertBucketKeys(t, store.client, providerID)
	if err != nil {
		t.Fatalf("列桶键失败: %v", err)
	}
	if len(keys) != hours*2 {
		t.Fatalf("条目应与 (小时, 成因) 一一对应（期望 %d 个），实得 %d 个——多出来即共享槽位或残留",
			hours*2, len(keys))
	}
	for _, key := range keys {
		ttl, err := store.client.TTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("取 %s 的 TTL 失败: %v", key, err)
		}
		if ttl <= 0 {
			t.Fatalf("%s 应自带 TTL（否则永不过期、条目无界），实得 %s", key, ttl)
		}
		if ttl > divertTTL {
			t.Fatalf("%s 的 TTL 应不超过 %s，实得 %s", key, divertTTL, ttl)
		}
	}

	snapshot, err := store.ReadDivert(ctx, providerID, base.Add((hours-1)*time.Hour))
	if err != nil {
		t.Fatalf("读改道计数失败: %v", err)
	}
	if snapshot.Cooldown != divertBuckets || snapshot.Penalty != divertBuckets {
		t.Fatalf("窗口（%d 桶）内应各读到 %d，实得 cooldown=%d penalty=%d",
			divertBuckets, divertBuckets, snapshot.Cooldown, snapshot.Penalty)
	}
}

// TestDivertSingleHourKeepsTwoBuckets 钉住单小时只产生两个桶键、且累加在同一键上。
func TestDivertSingleHourKeepsTwoBuckets(t *testing.T) {
	store, ctx := divertTestStore(t)
	const providerID = 201
	now := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		store.Record(ctx, providerID, route.DivertCauseCooldown, now)
	}
	store.Record(ctx, providerID, route.DivertCausePenalty, now)

	if count := divertBucketCount(t, store.client, providerID); count != 2 {
		t.Fatalf("同一小时内两个成因应只产生 2 个键，实得 %d", count)
	}
	snapshot, _ := store.ReadDivert(ctx, providerID, now)
	if snapshot.Cooldown != 5 || snapshot.Penalty != 1 {
		t.Fatalf("应读到 cooldown=5 penalty=1，实得 cooldown=%d penalty=%d",
			snapshot.Cooldown, snapshot.Penalty)
	}
}

// TestDivertOldBucketLifetimeNotExtended 是**判别性**用例：旧桶的寿命不得被后续写延长。
//
// 这是新旧形制的分水岭。旧形制一个渠道一个 Hash、TTL 是整键一条且每次写都刷新，故任何一条
// 记录的寿命都被「最近一次写」续命——那正是无界增长的根。一桶一键则写入只碰自己那一桶的键。
//
// 手法：写一个 10 小时前的桶，把它的人工寿命压到 90 秒（模拟它已经老去），再写当前桶，
// 断言旧键的寿命仍在分钟级（未被续成 25 小时）。旧形制下这个键根本不存在（桶是字段），
// 故第一条断言即红。
func TestDivertOldBucketLifetimeNotExtended(t *testing.T) {
	store, ctx := divertTestStore(t)
	const providerID = 202
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	oldAt := base.Add(-10 * time.Hour)

	store.Record(ctx, providerID, route.DivertCauseCooldown, oldAt)
	oldKey := divertBucketKey(providerID, hourStartUnix(oldAt), route.DivertCauseCooldown)
	renewed, err := store.client.Expire(ctx, oldKey, 90*time.Second).Result()
	if err != nil || !renewed {
		t.Fatalf("旧桶应有自己的键（Expire 返回 %v, err %v）——桶不是独立键时，它的寿命归整键所有，正因如此才会被后续写续命",
			renewed, err)
	}

	store.Record(ctx, providerID, route.DivertCauseCooldown, base)

	ttl, err := store.client.TTL(ctx, oldKey).Result()
	if err != nil {
		t.Fatalf("取旧桶 TTL 失败: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("旧桶键不该消失（还没到期），实得 TTL=%s", ttl)
	}
	if ttl > 5*time.Minute {
		t.Fatalf("旧桶寿命被后续写延长了（应为分钟级、实得 %s）——寿命绑到别人身上即无界增长的根", ttl)
	}
}

// TestDivertWindowIncludesBoundaryExcludesOlder 钉住窗口边界：最老那一桶计入、再老一小时不计入。
//
// 边界差一小时是「窗口桶数」这一口径的全部内容；错一小时不会报错，只会让「最近 24 小时」
// 悄悄变成 23 或 25 小时。
func TestDivertWindowIncludesBoundaryExcludesOlder(t *testing.T) {
	store, ctx := divertTestStore(t)
	const providerID = 203
	now := time.Date(2026, 9, 1, 18, 20, 0, 0, time.UTC)

	store.Record(ctx, providerID, route.DivertCauseCooldown, now.Add(-time.Duration(divertBuckets-1)*time.Hour))
	store.Record(ctx, providerID, route.DivertCausePenalty, now.Add(-time.Duration(divertBuckets)*time.Hour))

	// 两笔都必须真的落成键（否则本用例可能因「写侧丢写」而假绿）。
	if count := divertBucketCount(t, store.client, providerID); count != 2 {
		t.Fatalf("两笔跨小时写入应产生 2 个键，实得 %d", count)
	}
	snapshot, err := store.ReadDivert(ctx, providerID, now)
	if err != nil {
		t.Fatalf("读改道计数失败: %v", err)
	}
	if snapshot.Cooldown != 1 {
		t.Fatalf("窗口最老那一桶（-%dh）应计入，实得 cooldown=%d", divertBuckets-1, snapshot.Cooldown)
	}
	if snapshot.Penalty != 0 {
		t.Fatalf("窗口外那一桶（-%dh）不该计入，实得 penalty=%d", divertBuckets, snapshot.Penalty)
	}
}

// TestDivertExpiredBucketLeavesWindow 钉住「键到期即自动离开读数」，即自清理无派生状态。
//
// 这条是 (B) 有界性的直接证据：不靠任何剪除逻辑，靠键自己的 TTL。手法是把刚写的桶键寿命压到
// 1 秒、真等它过期，再断言读数归零、键也没了。
func TestDivertExpiredBucketLeavesWindow(t *testing.T) {
	store, ctx := divertTestStore(t)
	const providerID = 204
	now := time.Date(2026, 9, 1, 9, 5, 0, 0, time.UTC)

	store.Record(ctx, providerID, route.DivertCauseCooldown, now)
	bucket := divertBucketKey(providerID, hourStartUnix(now), route.DivertCauseCooldown)
	if _, err := store.client.Expire(ctx, bucket, time.Second).Result(); err != nil {
		t.Fatalf("压缩桶寿命失败: %v", err)
	}
	time.Sleep(1300 * time.Millisecond)

	snapshot, err := store.ReadDivert(ctx, providerID, now)
	if err != nil {
		t.Fatalf("读改道计数失败: %v", err)
	}
	if snapshot.Cooldown != 0 {
		t.Fatalf("已到期的桶不该再被计入，实得 cooldown=%d", snapshot.Cooldown)
	}
	if count := divertBucketCount(t, store.client, providerID); count != 0 {
		t.Fatalf("已到期的桶键应被 Redis 清掉，实得仍剩 %d 个", count)
	}
}
