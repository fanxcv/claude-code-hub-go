package session

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件覆盖**供应商维度**的批量终止（Node 的 terminateProviderSessionsBatch /
// terminateSessionsBatch，session-manager.ts:3733-3857）。这是管理面写路径的粘性会话失效
// （SessionManager.terminateStickySessionsForProviders）唯一的落地路径，所以它要证明的是
// 两件事：**旧亲和确实不再命中**，以及**影响范围确实只有该供应商**。

// affinityFixture 是一次「带亲和」的会话现场。
type affinityFixture struct {
	sessionID  string
	keyID      int64
	userID     int64
	providerID int64
}

// seedAffinitySession 造一个绑定到 providerID 的会话（legacy 镜像 + info + 四组索引 + 亲和索引）。
//
// 与 seedTerminateFixture 的差别：那里每个用例只有一个会话且 provider/key 固定，这里要按用例给
// 不同 provider 造多个会话（「只动该供应商」的断言需要另一个供应商的会话做对照）。
func seedAffinitySession(
	t *testing.T,
	rdb *redis.Client,
	index int,
	providerID int64,
	keyID int64,
	userID int64,
) affinityFixture {
	t.Helper()
	ctx := context.Background()
	fixture := affinityFixture{
		sessionID:  fmt.Sprintf("%s_%d", uniqueSessionID(t), index),
		keyID:      keyID,
		userID:     userID,
		providerID: providerID,
	}
	bindingKeys := BuildBindingKeys(fixture.sessionID, fixture.keyID)

	pipe := rdb.Pipeline()
	// legacy 镜像：ReadOrReconcile 由它升级出规范绑定（没有它绑定路径根本走不到）。
	pipe.Set(ctx, LegacyProviderKey(fixture.sessionID), fmt.Sprintf("%d", providerID), 0)
	pipe.Set(ctx, LegacyOwnerKey(fixture.sessionID), fmt.Sprintf("%d", keyID), 0)
	pipe.HSet(ctx, InfoKey(fixture.sessionID), "userId", fmt.Sprintf("%d", userID))
	pipe.ZAdd(ctx, ActiveSessionsGlobalKey(), redis.Z{Score: 1, Member: fixture.sessionID})
	pipe.ZAdd(ctx, KeyActiveSessionsKey(fixture.keyID), redis.Z{Score: 1, Member: fixture.sessionID})
	pipe.ZAdd(ctx, UserActiveSessionsKey(fixture.userID), redis.Z{Score: 1, Member: fixture.sessionID})
	pipe.ZAdd(ctx, ProviderActiveSessionsKey(fixture.providerID), redis.Z{Score: 1, Member: fixture.sessionID})
	pipe.HSet(ctx, ProviderActiveSessionRefsKey(fixture.providerID), fixture.sessionID, "1")
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("造亲和会话失败: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rdb.Del(cleanupCtx,
			LegacyProviderKey(fixture.sessionID),
			LegacyOwnerKey(fixture.sessionID),
			InfoKey(fixture.sessionID),
			LastSeenKey(fixture.sessionID),
			MessagesKey(fixture.sessionID),
			ConcurrentCountKey(fixture.sessionID),
			bindingKeys.Canonical,
		).Err()
		// 共享 ZSET 只摘自己的成员，绝不整键删除（其他用例与真实数据都在里面）。
		_ = rdb.ZRem(cleanupCtx,
			ActiveSessionsGlobalKey(),
			KeyActiveSessionsKey(fixture.keyID),
			UserActiveSessionsKey(fixture.userID),
			ProviderActiveSessionsKey(fixture.providerID),
			fixture.sessionID,
		).Err()
		_ = rdb.HDel(cleanupCtx, ProviderActiveSessionRefsKey(fixture.providerID), fixture.sessionID).Err()
	})
	return fixture
}

// assertAffinityCleared 断言该会话的**亲和**已被摘除：legacy provider 镜像没了、规范绑定不再带
// provider_id、供应商的活跃索引与引用哈希都不再含它。
//
// 这就是「旧亲和不再命中」的可观测定义：数据面选路读到的是「无绑定」，下次请求会重新选供应商。
func assertAffinityCleared(t *testing.T, rdb *redis.Client, fixture affinityFixture) {
	t.Helper()
	ctx := context.Background()
	if err := rdb.Get(ctx, LegacyProviderKey(fixture.sessionID)).Err(); err != redis.Nil {
		t.Fatalf("legacy provider 镜像仍在（err=%v）", err)
	}
	providerID, err := rdb.HGet(ctx, BuildBindingKeys(fixture.sessionID, fixture.keyID).Canonical,
		"provider_id").Result()
	if err != redis.Nil {
		t.Fatalf("规范绑定仍带 provider_id=%q（err=%v）", providerID, err)
	}
	if score, err := rdb.ZScore(ctx, ProviderActiveSessionsKey(fixture.providerID),
		fixture.sessionID).Result(); err != redis.Nil {
		t.Fatalf("供应商活跃索引仍含会话（score=%v err=%v）", score, err)
	}
	if _, err := rdb.HGet(ctx, ProviderActiveSessionRefsKey(fixture.providerID),
		fixture.sessionID).Result(); err != redis.Nil {
		t.Fatalf("供应商引用哈希仍含会话（err=%v）", err)
	}
}

// assertAffinityIntact 断言该会话完全没被动过（对照组的反向断言）。
func assertAffinityIntact(t *testing.T, rdb *redis.Client, fixture affinityFixture) {
	t.Helper()
	ctx := context.Background()
	if err := rdb.Get(ctx, LegacyProviderKey(fixture.sessionID)).Err(); err != nil {
		t.Fatalf("对照会话的 legacy provider 镜像不该消失: %v", err)
	}
	if score, err := rdb.ZScore(ctx, ProviderActiveSessionsKey(fixture.providerID),
		fixture.sessionID).Result(); err != nil || score == 0 {
		t.Fatalf("对照会话仍应在自己的供应商索引里（score=%v err=%v）", score, err)
	}
	if exists, _ := rdb.Exists(ctx, InfoKey(fixture.sessionID)).Result(); exists != 1 {
		t.Fatal("对照会话的 info 不该被删")
	}
}

// TestTerminateProviderSessionsBatchOnlyTouchesThatProvider 是范围断言的核心：
// 终止 provider 9 只影响绑在 9 上的会话，绑在 77 上的会话一个字段都不能少。
func TestTerminateProviderSessionsBatchOnlyTouchesThatProvider(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()

	target := seedAffinitySession(t, rdb, 0, 9, testKeyID, 7)
	other := seedAffinitySession(t, rdb, 1, 77, testKeyID+1, 8)

	terminated, err := binder.TerminateProviderSessionsBatch(ctx, []int64{9})
	if err != nil {
		t.Fatalf("按供应商批量终止失败: %v", err)
	}
	if terminated != 1 {
		t.Fatalf("应终止 1 个会话，实得 %d", terminated)
	}
	assertAffinityCleared(t, rdb, target)
	assertAffinityIntact(t, rdb, other)
}

// TestTerminateProviderSessionsBatchScopeIsDrivenByActiveIndex 钉住「候选来自供应商索引」：
// 一个绑在 9 上、但已经不在 provider:9:active_sessions 里的会话不该被这条路径碰到
// （Node 同样是先 ZRANGE 索引再逐个终止）。
func TestTerminateProviderSessionsBatchScopeIsDrivenByActiveIndex(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()

	listed := seedAffinitySession(t, rdb, 0, 9, testKeyID, 7)
	unlisted := seedAffinitySession(t, rdb, 1, 9, testKeyID+1, 8)
	if err := rdb.ZRem(ctx, ProviderActiveSessionsKey(9), unlisted.sessionID).Err(); err != nil {
		t.Fatalf("摘除索引成员失败: %v", err)
	}

	terminated, err := binder.TerminateProviderSessionsBatch(ctx, []int64{9})
	if err != nil {
		t.Fatalf("按供应商批量终止失败: %v", err)
	}
	if terminated != 1 {
		t.Fatalf("只有索引里的那一个应被终止，实得 %d", terminated)
	}
	assertAffinityCleared(t, rdb, listed)
	if err := rdb.Get(ctx, LegacyProviderKey(unlisted.sessionID)).Err(); err != nil {
		t.Fatalf("不在索引里的会话不该被终止: %v", err)
	}
}

// TestTerminateProviderSessionsBatchFiltersAndDedupesProviderIDs 钉住入参归一：
// Node 用 `new Set(ids.filter(Number.isInteger && > 0))`，故非正整数被丢、重复只算一次。
func TestTerminateProviderSessionsBatchFiltersAndDedupesProviderIDs(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()

	target := seedAffinitySession(t, rdb, 0, 9, testKeyID, 7)

	terminated, err := binder.TerminateProviderSessionsBatch(ctx, []int64{0, -3, 9, 9})
	if err != nil {
		t.Fatalf("按供应商批量终止失败: %v", err)
	}
	if terminated != 1 {
		t.Fatalf("去重与过滤后应终止 1 个会话，实得 %d", terminated)
	}
	assertAffinityCleared(t, rdb, target)

	// 全部非法（或空）：一条都不动，也不报错。
	for _, input := range [][]int64{nil, {}, {0}, {-1, 0}} {
		count, err := binder.TerminateProviderSessionsBatch(ctx, input)
		if err != nil || count != 0 {
			t.Fatalf("非法入参 %v 应得 (0, nil)，实得 (%d, %v)", input, count, err)
		}
	}
}

// TestTerminateProviderSessionsBatchWithNoActiveSessions 钉住空索引分支：供应商一个活跃会话都没有。
func TestTerminateProviderSessionsBatchWithNoActiveSessions(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)

	count, err := binder.TerminateProviderSessionsBatch(context.Background(), []int64{987654321})
	if err != nil {
		t.Fatalf("空索引不该报错: %v", err)
	}
	if count != 0 {
		t.Fatalf("空索引应得 0，实得 %d", count)
	}
}

// TestTerminateSessionsBatchCountsOnlyTerminated 覆盖分块路径（25 > CHUNK_SIZE=20）：
// 返回的是**成功终止的条数**，混进来的无效 id 不计数、也不影响其余条目。
func TestTerminateSessionsBatchCountsOnlyTerminated(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()

	seeded := make([]affinityFixture, 0, 25)
	sessionIDs := make([]string, 0, 26)
	for index := range 25 {
		fixture := seedAffinitySession(t, rdb, index, 9, testKeyID, 7)
		seeded = append(seeded, fixture)
		sessionIDs = append(sessionIDs, fixture.sessionID)
	}
	sessionIDs = append(sessionIDs, "sess_gotest_absent_"+t.Name())

	terminated, err := binder.TerminateSessionsBatch(ctx, sessionIDs, nil)
	if err != nil {
		t.Fatalf("批量终止失败: %v", err)
	}
	if terminated != 25 {
		t.Fatalf("应终止 25 个会话（无效 id 不计），实得 %d", terminated)
	}
	for _, fixture := range seeded {
		if exists, _ := rdb.Exists(ctx, InfoKey(fixture.sessionID)).Result(); exists != 0 {
			t.Fatalf("无范围批量终止应删掉会话元数据：%s", fixture.sessionID)
		}
	}

	// 空入参：直接返回 0，不碰 Redis。
	if count, err := binder.TerminateSessionsBatch(ctx, nil, nil); err != nil || count != 0 {
		t.Fatalf("空入参应得 (0, nil)，实得 (%d, %v)", count, err)
	}
}

// positiveUniqueProviderIDs 是入参归一的纯函数，单独钉住顺序与去重（Node 的 Set 保序）。
func TestPositiveUniqueProviderIDs(t *testing.T) {
	got := positiveUniqueProviderIDs([]int64{3, 0, 3, -1, 5, 3})
	want := []int64{3, 5}
	if len(got) != len(want) {
		t.Fatalf("应得 %v，实得 %v", want, got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("应得 %v，实得 %v", want, got)
		}
	}
}
