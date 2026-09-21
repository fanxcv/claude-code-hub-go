package session

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
)

// 本文件用**真 Redis + 真 Lua** 钉住 ClearBinding 的语义。
//
// 为什么不能用计数替身：ClearBinding 原实现传 expectedProviderID=0，而 Lua 里 0 表示
// 「期望空绑定」，于是**已有绑定**时必得 provider_mismatch、清不掉——恰好与用途相反
// （`clear-session-binding.lua` 的 `(current_provider_id or '') ~= expected_provider_id`）。
// 计数替身只看「有没有被调用」，看不见「调用有没有生效」，这正是该缺陷能长期漏网的原因：
// 接缝测试全绿，而生产上「资源类失效清绑定」实际是空操作。
//
// 四种情形各一条：已有绑定清得掉 / 传错 provider 不动键 / 传错 generation 不动键 /
// 本来没绑定不算故障（记 skipped 而非 conflict）。

const clearTestProviderID int64 = 9

// clearFixture 组装「真 Redis + 真脚本」的写回能力与日志缓冲。
func clearFixture(t *testing.T) (*Binder, *redis.Client, *SessionBinderAdapter, *bytes.Buffer) {
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
	logs := &bytes.Buffer{}
	adapter := NewSessionBinderAdapter(BinderOptions{
		Client: NewBinder(client),
		TTL:    time.Duration(testTTLSeconds) * time.Second,
		Logger: logx.New(logs),
	})
	return NewBinder(client), rdb, adapter, logs
}

// seedBinding 建会话；providerID > 0 时把绑定指向它，并返回**下一次请求选路会读到的**
// generation（CAS 会旋转代际，故必须重读一次——写回实现固化的正是这个值）。
func seedBinding(t *testing.T, binder *Binder, sessionID string, providerID int64) string {
	t.Helper()
	ctx := context.Background()
	created, err := binder.ReadOrReconcile(ctx, sessionID, testKeyID, testTTLSeconds)
	if err != nil || !created.OK {
		t.Fatalf("建会话失败: %+v err=%v", created, err)
	}
	if providerID > 0 {
		set, err := binder.CompareAndSet(ctx, sessionID, testKeyID, created.Snapshot.Generation, providerID, testTTLSeconds)
		if err != nil || !set.OK {
			t.Fatalf("写绑定失败: %+v err=%v", set, err)
		}
	}
	current, err := binder.ReadOrReconcile(ctx, sessionID, testKeyID, testTTLSeconds)
	if err != nil || !current.OK {
		t.Fatalf("重读绑定失败: %+v err=%v", current, err)
	}
	return current.Snapshot.Generation
}

// canonicalProvider 读 canonical 的 provider_id 字段；空串表示无 provider。
func canonicalProvider(t *testing.T, rdb *redis.Client, sessionID string) string {
	t.Helper()
	fields, err := rdb.HGetAll(context.Background(), BuildBindingKeys(sessionID, testKeyID).Canonical).Result()
	if err != nil {
		t.Fatalf("读 canonical 失败: %v", err)
	}
	return fields["provider_id"]
}

func cooldownExists(t *testing.T, rdb *redis.Client, sessionID string, providerID int64) bool {
	t.Helper()
	exists, err := rdb.Exists(context.Background(), ProviderCooldownKey(sessionID, testKeyID, providerID)).Result()
	if err != nil {
		t.Fatalf("查冷却键失败: %v", err)
	}
	return exists > 0
}

// TestClearBindingClearsExistingBindingWithoutCooldown 是本缺陷的正面钉子：
// 已有绑定（provider_id 非空）时 ClearBinding 必须**真的清掉**它，且不写冷却。
// 把 expectedProviderID 改回 0，本用例即红（那正是修复前的现场）。
func TestClearBindingClearsExistingBindingWithoutCooldown(t *testing.T) {
	binder, rdb, adapter, logs := clearFixture(t)
	ctx := context.Background()
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)

	generation := seedBinding(t, binder, sessionID, clearTestProviderID)
	if got := canonicalProvider(t, rdb, sessionID); got != strconv.FormatInt(clearTestProviderID, 10) {
		t.Fatalf("前置失败：绑定未指向 %d，实得 %q", clearTestProviderID, got)
	}

	writeback := adapter.SessionBindingWriteback(sessionID, testKeyID, generation)
	if writeback == nil {
		t.Fatal("写回能力构造失败（前置不成立）")
	}
	if !writeback.ClearBinding(ctx, clearTestProviderID) {
		t.Fatalf("已有绑定时 ClearBinding 应成功；日志：%s", logs.String())
	}
	if got := canonicalProvider(t, rdb, sessionID); got != "" {
		t.Fatalf("绑定应被清空，实得 provider_id=%q", got)
	}
	if cooldownExists(t, rdb, sessionID, clearTestProviderID) {
		t.Fatal("清绑定不得写冷却（设计稿 §4：资源类失效不是故障）")
	}
	if out := logs.String(); strings.Contains(out, "clear_conflict") || strings.Contains(out, "clear_failed") {
		t.Fatalf("清成功不该有冲突/失败日志：%s", out)
	}
}

// TestClearBindingKeepsKeyOnWrongProviderOrGeneration 钉住 fence 仍严：传错 provider
// 或错 generation 时**一个字段都不许改**。
func TestClearBindingKeepsKeyOnWrongProviderOrGeneration(t *testing.T) {
	cases := []struct {
		name       string
		providerID int64
		generation string
	}{
		{name: "传错 provider", providerID: clearTestProviderID + 1},
		{name: "传错 generation", providerID: clearTestProviderID, generation: "stale-generation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binder, rdb, adapter, logs := clearFixture(t)
			ctx := context.Background()
			sessionID := uniqueSessionID(t)
			cleanupSessionKeys(t, rdb, sessionID, testKeyID)

			generation := seedBinding(t, binder, sessionID, clearTestProviderID)
			if tc.generation != "" {
				generation = tc.generation
			}
			writeback := adapter.SessionBindingWriteback(sessionID, testKeyID, generation)
			if writeback == nil {
				t.Fatal("写回能力构造失败（前置不成立）")
			}
			if writeback.ClearBinding(ctx, tc.providerID) {
				t.Fatalf("fence 应拒绝本次清除；日志：%s", logs.String())
			}
			if got := canonicalProvider(t, rdb, sessionID); got != strconv.FormatInt(clearTestProviderID, 10) {
				t.Fatalf("键不得被改动，实得 provider_id=%q", got)
			}
			if cooldownExists(t, rdb, sessionID, clearTestProviderID) {
				t.Fatal("被拒绝的清除更不该写冷却")
			}
		})
	}
}

// TestClearBindingWithoutBindingIsNotAFault 钉住「本来就没绑定」不算故障：
// 记 clear_skipped（fence 的正常语义），不得记成 clear_conflict / clear_failed。
func TestClearBindingWithoutBindingIsNotAFault(t *testing.T) {
	binder, rdb, adapter, logs := clearFixture(t)
	ctx := context.Background()
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)

	generation := seedBinding(t, binder, sessionID, 0)
	if got := canonicalProvider(t, rdb, sessionID); got != "" {
		t.Fatalf("前置失败：本会话不该有 provider，实得 %q", got)
	}

	writeback := adapter.SessionBindingWriteback(sessionID, testKeyID, generation)
	if writeback == nil {
		t.Fatal("写回能力构造失败（前置不成立）")
	}
	if writeback.ClearBinding(ctx, clearTestProviderID) {
		t.Fatal("无可清时不应报成功")
	}
	out := logs.String()
	if !strings.Contains(out, "session.binding.clear_skipped") {
		t.Fatalf("应记 clear_skipped（带冲突原因）：%s", out)
	}
	if strings.Contains(out, "clear_conflict") || strings.Contains(out, "clear_failed") {
		t.Fatalf("无可清不是故障，不得记 conflict/failed：%s", out)
	}
}
