package session

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// terminateFixture 是终止用例的现场：一个带绑定、活跃索引与响应体 bundle 的会话。
type terminateFixture struct {
	sessionID  string
	keyID      int64
	userID     int64
	providerID int64
}

// seedTerminateFixture 造出「终止前」的完整现场：legacy 镜像、info、活跃四索引、
// 响应体 bundle 索引与正文、元数据键。全部写真实 Redis。
func seedTerminateFixture(t *testing.T, rdb *redis.Client) terminateFixture {
	t.Helper()
	ctx := context.Background()
	fixture := terminateFixture{
		sessionID:  uniqueSessionID(t),
		keyID:      testKeyID,
		userID:     7,
		providerID: 9,
	}
	bundleKey := ResponseBodyBundleKey(fixture.sessionID, 1)

	if err := rdb.Set(ctx, LegacyProviderKey(fixture.sessionID), "9", 0).Err(); err != nil {
		t.Fatalf("写 legacy provider 失败: %v", err)
	}
	if err := rdb.Set(ctx, LegacyOwnerKey(fixture.sessionID), "42", 0).Err(); err != nil {
		t.Fatalf("写 legacy owner 失败: %v", err)
	}
	if err := rdb.HSet(ctx, InfoKey(fixture.sessionID), "userId", "7").Err(); err != nil {
		t.Fatalf("写 info 失败: %v", err)
	}

	// 四组活跃索引 + 观测集合都要有成员，才能断言「终止后不再含该会话」。
	pipe := rdb.Pipeline()
	pipe.ZAdd(ctx, ActiveSessionsGlobalKey(), redis.Z{Score: 1, Member: fixture.sessionID})
	pipe.ZAdd(ctx, KeyActiveSessionsKey(fixture.keyID), redis.Z{Score: 1, Member: fixture.sessionID})
	pipe.ZAdd(ctx, UserActiveSessionsKey(fixture.userID), redis.Z{Score: 1, Member: fixture.sessionID})
	pipe.ZAdd(ctx, ProviderActiveSessionsKey(fixture.providerID), redis.Z{Score: 1, Member: fixture.sessionID})
	pipe.HSet(ctx, ProviderActiveSessionRefsKey(fixture.providerID), fixture.sessionID, "1")
	pipe.ZAdd(ctx, ObservedGlobalActiveSessionsKey(), redis.Z{Score: 1, Member: fixture.sessionID})
	// 元数据与响应体 bundle。
	pipe.Set(ctx, LastSeenKey(fixture.sessionID), "1", 0)
	pipe.Set(ctx, ConcurrentCountKey(fixture.sessionID), "1", 0)
	pipe.Set(ctx, MessagesKey(fixture.sessionID), "[]", 0)
	pipe.Set(ctx, LegacyResponseBodyKey(fixture.sessionID), "legacy-body", 0)
	pipe.Set(ctx, LegacyResponseBodyRequestKey(fixture.sessionID, 1), "view-body", 0)
	pipe.Set(ctx, ResponseSnapshotBodyKey(fixture.sessionID, 1, "before"), "before-body", 0)
	pipe.Set(ctx, ResponseSnapshotBodyKey(fixture.sessionID, 1, "after"), "after-body", 0)
	pipe.ZAdd(ctx, ResponseBodyBundleIndexKey(fixture.sessionID), redis.Z{Score: 1, Member: bundleKey})
	pipe.HSet(ctx, bundleKey, "schema", "1", "body:0", "chunk")
	pipe.Set(ctx, ownerRequestGenerationKey(fixture.sessionID, 1), "gen-1", 0)
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("造终止现场失败: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rdb.Del(cleanupCtx,
			LegacyProviderKey(fixture.sessionID),
			LegacyOwnerKey(fixture.sessionID),
			InfoKey(fixture.sessionID),
			LastSeenKey(fixture.sessionID),
			ConcurrentCountKey(fixture.sessionID),
			MessagesKey(fixture.sessionID),
			ResponseBodyBundleIndexKey(fixture.sessionID),
			ResponseBodyGenerationKey(fixture.sessionID),
			LegacyResponseBodyKey(fixture.sessionID),
			LegacyResponseBodyRequestKey(fixture.sessionID, 1),
			ResponseSnapshotBodyKey(fixture.sessionID, 1, "before"),
			ResponseSnapshotBodyKey(fixture.sessionID, 1, "after"),
			bundleKey,
			BuildBindingKeys(fixture.sessionID, fixture.keyID).Canonical,
		).Err()
		// 共享 ZSET 只摘自己的成员，绝不整键删除（其他用例与真实数据都在里面）。
		_ = rdb.ZRem(cleanupCtx,
			ActiveSessionsGlobalKey(),
			KeyActiveSessionsKey(fixture.keyID),
			UserActiveSessionsKey(fixture.userID),
			ProviderActiveSessionsKey(fixture.providerID),
			ObservedGlobalActiveSessionsKey(),
			fixture.sessionID,
		).Err()
		_ = rdb.HDel(cleanupCtx, ProviderActiveSessionRefsKey(fixture.providerID), fixture.sessionID).Err()
	})
	return fixture
}

// assertIndexesExcludeSession 断言终止路径负责的四组活跃索引都不再含该会话。
//
// 观测集合（{observed_sessions}:global）**不在此列**：Node 侧它由动作层的
// SessionTracker.terminateObservedSession 另行摘除，见 TestTerminateObservedSessionClearsIndex。
func assertIndexesExcludeSession(t *testing.T, rdb *redis.Client, fixture terminateFixture) {
	t.Helper()
	ctx := context.Background()
	checks := map[string]string{
		"global":   ActiveSessionsGlobalKey(),
		"key":      KeyActiveSessionsKey(fixture.keyID),
		"user":     UserActiveSessionsKey(fixture.userID),
		"provider": ProviderActiveSessionsKey(fixture.providerID),
	}
	for label, key := range checks {
		score, err := rdb.ZScore(ctx, key, fixture.sessionID).Result()
		if err != redis.Nil {
			t.Fatalf("%s 索引仍含会话（score=%v err=%v）", label, score, err)
		}
	}
	if _, err := rdb.HGet(ctx, ProviderActiveSessionRefsKey(fixture.providerID), fixture.sessionID).Result(); err != redis.Nil {
		t.Fatalf("provider refs 仍含会话（err=%v）", err)
	}
}

// TestTerminateSessionClearsBindingIndexesAndBundles 是终止面的核心断言：
// 调一次 TerminateSession 之后，活跃索引、观测集合、元数据与响应体 bundle 都不再存在。
func TestTerminateSessionClearsBindingIndexesAndBundles(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	fixture := seedTerminateFixture(t, rdb)

	terminated, err := binder.TerminateSession(ctx, fixture.sessionID, nil, nil)
	if err != nil {
		t.Fatalf("终止会话失败: %v", err)
	}
	if !terminated {
		t.Fatal("终止应返回 true")
	}

	assertIndexesExcludeSession(t, rdb, fixture)

	for label, key := range map[string]string{
		"info":      InfoKey(fixture.sessionID),
		"last_seen": LastSeenKey(fixture.sessionID),
		"messages":  MessagesKey(fixture.sessionID),
		"bundle":    ResponseBodyBundleKey(fixture.sessionID, 1),
		"legacy":    LegacyResponseBodyKey(fixture.sessionID),
		"view":      LegacyResponseBodyRequestKey(fixture.sessionID, 1),
		"snapshot":  ResponseSnapshotBodyKey(fixture.sessionID, 1, "before"),
		"after":     ResponseSnapshotBodyKey(fixture.sessionID, 1, "after"),
		"reqgen":    ownerRequestGenerationKey(fixture.sessionID, 1),
	} {
		exists, err := rdb.Exists(ctx, key).Result()
		if err != nil {
			t.Fatalf("检查 %s 存在性失败: %v", label, err)
		}
		if exists != 0 {
			t.Fatalf("%s 键应已删除: %s", label, key)
		}
	}

	// 会话级代际键按 Node 语义是**重写**（SETEX 新 generation），不是删除。
	generation, err := rdb.Get(ctx, ResponseBodyGenerationKey(fixture.sessionID)).Result()
	if err != nil {
		t.Fatalf("代际键应被重写而非删除: %v", err)
	}
	if generation == "" || generation == "0" {
		t.Fatalf("代际键应换成新 generation，实际=%q", generation)
	}
}

// TestTerminateObservedSessionClearsIndex 覆盖动作层那一步：终止观测 identity 之后，
// 观测集合与展示用并发计数都不再含该会话。
func TestTerminateObservedSessionClearsIndex(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	fixture := seedTerminateFixture(t, rdb)

	cleared, err := binder.TerminateObservedSessionForIdentity(ctx, fixture.sessionID)
	if err != nil {
		t.Fatalf("终止观测 identity 失败: %v", err)
	}
	if !cleared {
		t.Fatal("观测集合里有成员，应返回 true")
	}
	if score, err := rdb.ZScore(ctx, ObservedGlobalActiveSessionsKey(), fixture.sessionID).Result(); err != redis.Nil {
		t.Fatalf("观测集合仍含会话（score=%v err=%v）", score, err)
	}
	// 展示用并发计数是 observed_session:{identity}:concurrent_count，与终止路径的
	// session:{id}:concurrent_count 不是同一个键。
	if exists, _ := rdb.Exists(ctx, ObservedConcurrentCountKey(fixture.sessionID)).Result(); exists != 0 {
		t.Fatal("展示用并发计数键应已删除")
	}
}

// TestTerminateSessionRejectsChangedOwner 复刻 Node 的 owner 预检：期望 key 与镜像不符时
// 一个键都不许动（这是「终止前 owner 已变」的防误删闸门）。
func TestTerminateSessionRejectsChangedOwner(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	fixture := seedTerminateFixture(t, rdb)

	otherKey := fixture.keyID + 1
	terminated, err := binder.TerminateSession(ctx, fixture.sessionID, nil, &otherKey)
	if err != nil {
		t.Fatalf("终止会话失败: %v", err)
	}
	if terminated {
		t.Fatal("owner 不符时应返回 false")
	}

	// 现场必须原样保留（只检查最容易被误删的两处）。
	if exists, _ := rdb.Exists(ctx, InfoKey(fixture.sessionID)).Result(); exists != 1 {
		t.Fatal("owner 不符时不应删除 info")
	}
	if _, err := rdb.ZScore(ctx, ActiveSessionsGlobalKey(), fixture.sessionID).Result(); err != nil {
		t.Fatalf("owner 不符时不应摘除活跃索引: %v", err)
	}
}

// TestTerminateSessionScopedToProvider 是「按供应商范围终止」：期望集合含当前绑定才动，
// 只摘该家的 provider 索引；期望集合不符时一个键都不许动。
func TestTerminateSessionScopedToProvider(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	fixture := seedTerminateFixture(t, rdb)

	// 现场只有 legacy 镜像，ReadOrReconcile 会以 legacy 升级成 provider=9 的规范绑定，
	// 因此期望集合含 9 才允许终止。
	terminated, err := binder.TerminateSession(ctx, fixture.sessionID, []int64{fixture.providerID}, nil)
	if err != nil {
		t.Fatalf("范围终止失败: %v", err)
	}
	if !terminated {
		t.Fatal("范围终止应返回 true")
	}
	if score, err := rdb.ZScore(ctx, ProviderActiveSessionsKey(fixture.providerID), fixture.sessionID).Result(); err != redis.Nil {
		t.Fatalf("provider 索引仍含会话（score=%v err=%v）", score, err)
	}

	// 第二批：期望集合与当前绑定不符时必须放弃，且不删任何东西。
	second := seedTerminateFixture(t, rdb)
	terminated, err = binder.TerminateSession(ctx, second.sessionID, []int64{4242}, nil)
	if err != nil {
		t.Fatalf("范围不匹配时终止失败: %v", err)
	}
	if terminated {
		t.Fatal("期望供应商不匹配时应返回 false")
	}
	if exists, _ := rdb.Exists(ctx, InfoKey(second.sessionID)).Result(); exists != 1 {
		t.Fatal("期望供应商不匹配时不应删除 info")
	}
}
