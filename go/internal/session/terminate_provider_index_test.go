package session

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
)

// 本文件钉住 clearProviderIndex 的**按新形制摘除**语义。
//
// 为什么必须有这条：活跃索引的成员从「裸会话身份」改成了「会话身份 + 尝试 token」的组合串
// （见 ProviderAttemptMember）。此后若仍拿裸会话身份去 ZREM，**命令会成功返回 0** 而一个成员
// 也不删——是静默失效：调用方看到无错、索引却一直残留到 TTL 过期（页面上的并发数因此偏大）。
// 这类「按旧形制操作新数据」的缺口不会被「有没有被调用」的替身抓到，只能用真 Redis 打真键验。

const indexTestProviderID int64 = 4242

func indexFixture(t *testing.T) (*Binder, *redis.Client) {
	t.Helper()
	rdb := testRedis(t)
	registry, err := ratelimit.Load()
	if err != nil {
		t.Fatalf("加载脚本注册表失败: %v", err)
	}
	client, err := ratelimit.New(rdb, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	return NewBinder(client), rdb
}

// TestClearProviderIndexRemovesAttemptMembers 钉住：按会话身份摘除时，该会话的**全部尝试成员**
// 与它们的应用计数一起被摘掉，而**别家会话的成员一个不动**。
func TestClearProviderIndexRemovesAttemptMembers(t *testing.T) {
	binder, rdb := indexFixture(t)
	ctx := context.Background()
	zsetKey := ProviderActiveSessionsKey(indexTestProviderID)
	refsKey := ProviderActiveSessionRefsKey(indexTestProviderID)
	rdb.Del(ctx, zsetKey, refsKey)

	mine := []string{
		ProviderAttemptMember("sess-index-a", "tk-1"),
		ProviderAttemptMember("sess-index-a", "tk-2"),
	}
	other := ProviderAttemptMember("sess-index-b", "tk-9")
	for _, member := range append(append([]string{}, mine...), other) {
		if err := rdb.ZAdd(ctx, zsetKey, redis.Z{Score: 1, Member: member}).Err(); err != nil {
			t.Fatalf("造成员失败: %v", err)
		}
		if err := rdb.HSet(ctx, refsKey, member, "1").Err(); err != nil {
			t.Fatalf("造引用计数失败: %v", err)
		}
	}

	binder.clearProviderIndex(ctx, indexTestProviderID, "sess-index-a")

	remaining, err := rdb.ZRange(ctx, zsetKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("读回成员失败: %v", err)
	}
	if len(remaining) != 1 || remaining[0] != other {
		t.Fatalf("被清会话的成员应全部摘除、他家成员应原样保留，实际成员为 %v", remaining)
	}
	refs, err := rdb.HGetAll(ctx, refsKey).Result()
	if err != nil {
		t.Fatalf("读回引用计数失败: %v", err)
	}
	if _, stillThere := refs[other]; !stillThere {
		t.Fatalf("他家会话的引用计数不该被删，实际为 %v", refs)
	}
	for _, member := range mine {
		if _, stillThere := refs[member]; stillThere {
			t.Fatalf("被清会话的引用计数 %q 应被删，实际为 %v", member, refs)
		}
	}
}

// TestClearProviderIndexOnAbsentSessionIsNoop 钉住空真边界：会话在该渠道上没有成员时，
// 不得误删别家成员（枚举后无一命中即整段跳过）。
func TestClearProviderIndexOnAbsentSessionIsNoop(t *testing.T) {
	binder, rdb := indexFixture(t)
	ctx := context.Background()
	zsetKey := ProviderActiveSessionsKey(indexTestProviderID)
	refsKey := ProviderActiveSessionRefsKey(indexTestProviderID)
	rdb.Del(ctx, zsetKey, refsKey)

	other := ProviderAttemptMember("sess-index-c", "tk-1")
	if err := rdb.ZAdd(ctx, zsetKey, redis.Z{Score: 1, Member: other}).Err(); err != nil {
		t.Fatalf("造成员失败: %v", err)
	}
	if err := rdb.HSet(ctx, refsKey, other, "1").Err(); err != nil {
		t.Fatalf("造引用计数失败: %v", err)
	}

	binder.clearProviderIndex(ctx, indexTestProviderID, "sess-index-not-there")

	remaining, err := rdb.ZRange(ctx, zsetKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("读回成员失败: %v", err)
	}
	if len(remaining) != 1 || remaining[0] != other {
		t.Fatalf("未命中的会话身份不该改动任何成员，实际成员为 %v", remaining)
	}
}
