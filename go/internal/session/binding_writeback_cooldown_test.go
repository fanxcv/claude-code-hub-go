package session

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// 本文件用**真 Redis + 真脚本**钉住 CooldownOnFailure 的**范围**：写冷却键、绑定一个字段都不动。
//
// 为何必须用真 Redis（与 binding_writeback_clear_test.go 同一条理由）：本缺陷的全部危害都在
// 「写完键之后 canonical 变成了什么」。计数替身只看「有没有被调用」，看不见「绑定被清掉了」——
// 旧实现（Binder.Clear：HDEL provider_id + SETEX cooldown，同一次 Lua 里两件事）在替身下全程
// 绿灯，而生产上 60 秒的临时冷却因此变成永久迁移：冷却 10:07:42 过期后，序号 37–104 共 68 个
// 请求 100% 走备用渠道。
//
// 夹具沿用 binding_writeback_clear_test.go 的 clearFixture（真 Redis + 真脚本 + 日志缓冲）。

// TestCooldownOnFailureKeepsBindingForReturn 是主线：写冷却后绑定仍指向该家，冷却过期即回迁。
//
// 把写侧换回 Binder.Clear（或任何清了 provider_id 的写法），本用例第一条断言即红。
func TestCooldownOnFailureKeepsBindingForReturn(t *testing.T) {
	binder, rdb, adapter, logs := clearFixture(t)
	ctx := context.Background()
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)

	generation := seedBinding(t, binder, sessionID, clearTestProviderID)
	writeback := adapter.SessionBindingWriteback(sessionID, testKeyID, generation)
	if writeback == nil {
		t.Fatal("写回能力构造失败（前置不成立）")
	}

	if !writeback.CooldownOnFailure(ctx, clearTestProviderID) {
		t.Fatalf("绑定正指向该家时应写下冷却；日志：%s", logs.String())
	}
	want := strconv.FormatInt(clearTestProviderID, 10)
	if got := canonicalProvider(t, rdb, sessionID); got != want {
		t.Fatalf("写冷却不得动绑定：provider_id = %q，期望 %q（清了绑定，冷却期一过就再也回不来）", got, want)
	}
	if !cooldownExists(t, rdb, sessionID, clearTestProviderID) {
		t.Fatalf("冷却键未写下；日志：%s", logs.String())
	}
	assertTTLWithin(t, rdb, ProviderCooldownKey(sessionID, testKeyID, clearTestProviderID), int(sessionCooldownTTL.Seconds()))

	// 冷却过期（读侧再也看到不冷却键的那一刻）：本会话必须粘回原家。
	if err := rdb.Del(ctx, ProviderCooldownKey(sessionID, testKeyID, clearTestProviderID)).Err(); err != nil {
		t.Fatalf("删除冷却键失败: %v", err)
	}
	returned, err := binder.ReadOrReconcile(ctx, sessionID, testKeyID, testTTLSeconds)
	if err != nil || !returned.OK {
		t.Fatalf("冷却过期后读绑定失败: %+v err=%v", returned, err)
	}
	if returned.Snapshot.ProviderID != clearTestProviderID {
		t.Fatalf("冷却过期后应粘回 %d，实得 %d（临时冷却变成了永久迁移）",
			clearTestProviderID, returned.Snapshot.ProviderID)
	}
	if out := logs.String(); strings.Contains(out, "cooldown_failed") || strings.Contains(out, "cooldown_skipped") {
		t.Fatalf("绑定指向该家时不该报失败/跳过：%s", out)
	}
}

// TestCooldownOnFailureLeavesSuccessPathStickiness 是反向护栏：写冷却不得以任何方式锁住或搞乱绑定。
//
// 两条断言各自拦一种错法：
//   - 同一代际仍能 CAS 到别家 —— 证明写冷却没有旋转代际（旧实现会转，此后迟到写入全被 fence 拒）；
//   - 成功改绑后 legacy 镜像与 canonical 逐字一致 —— 若写冷却改了一边而不改另一边，
//     此后每条 Lua 命令都会撞 mirror_conflict，会话粘性从此再也改不动（静默、只记 warn）。
//
// 正常粘性本身（无保留事实时成功侧照旧 CAS）由 terminal/session_binding_keep_test.go 钉住，
// 两侧合起来才是完整的「正常粘性不变」。
func TestCooldownOnFailureLeavesSuccessPathStickiness(t *testing.T) {
	binder, rdb, adapter, logs := clearFixture(t)
	ctx := context.Background()
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)

	const winnerProviderID int64 = clearTestProviderID + 1
	generation := seedBinding(t, binder, sessionID, clearTestProviderID)
	writeback := adapter.SessionBindingWriteback(sessionID, testKeyID, generation)
	if writeback == nil {
		t.Fatal("写回能力构造失败（前置不成立）")
	}
	if !writeback.CooldownOnFailure(ctx, clearTestProviderID) {
		t.Fatalf("绑定正指向该家时应写下冷却；日志：%s", logs.String())
	}

	// 冷却期内由备用渠道作答成功：改绑的抑制在选路层（SessionBindingBypass），不在本层——
	// 本层的 CAS 必须照旧可用，否则「选路层判定 + 终态层执行」这条链失去下半截。
	moved, err := binder.CompareAndSet(ctx, sessionID, testKeyID, generation, winnerProviderID, testTTLSeconds)
	if err != nil || !moved.OK {
		t.Fatalf("写冷却不得旋转代际：同一代际的成功 CAS 仍须成立，实得 %+v err=%v（日志：%s）",
			moved, err, logs.String())
	}
	if got, want := canonicalProvider(t, rdb, sessionID), strconv.FormatInt(winnerProviderID, 10); got != want {
		t.Fatalf("成功 CAS 后 canonical.provider_id = %q，期望 %q", got, want)
	}
	if got, want := legacyProviderMirror(t, rdb, sessionID), strconv.FormatInt(winnerProviderID, 10); got != want {
		t.Fatalf("legacy 镜像与 canonical 必须一致：镜像 = %q，canonical = %q", got, want)
	}
}

// TestCooldownOnFailureSkipsWhenBindingPointsElsewhere 钉住闸门仍在：只对「绑定恰好指向失败的那家」
// 写冷却（防羊群，与清绑定、亲和墓碑同一道闸）。绑定指向别家或本会话无绑定时，冷却键一个都不许写。
func TestCooldownOnFailureSkipsWhenBindingPointsElsewhere(t *testing.T) {
	cases := []struct {
		name       string
		seededWith int64
		failedOn   int64
	}{
		{name: "绑定指向别家", seededWith: clearTestProviderID + 5, failedOn: clearTestProviderID},
		{name: "本会话无绑定", seededWith: 0, failedOn: clearTestProviderID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binder, rdb, adapter, logs := clearFixture(t)
			ctx := context.Background()
			sessionID := uniqueSessionID(t)
			cleanupSessionKeys(t, rdb, sessionID, testKeyID)

			generation := seedBinding(t, binder, sessionID, tc.seededWith)
			writeback := adapter.SessionBindingWriteback(sessionID, testKeyID, generation)
			if writeback == nil {
				t.Fatal("写回能力构造失败（前置不成立）")
			}
			if writeback.CooldownOnFailure(ctx, tc.failedOn) {
				t.Fatalf("绑定不指向失败的那家，不得报写成功；日志：%s", logs.String())
			}
			if cooldownExists(t, rdb, sessionID, tc.failedOn) {
				t.Fatal("绑定不指向失败的那家时不得写冷却键")
			}
			if got, want := canonicalProvider(t, rdb, sessionID), boundProviderString(tc.seededWith); got != want {
				t.Fatalf("被跳过时不得改动键：provider_id = %q，期望 %q", got, want)
			}
			if !strings.Contains(logs.String(), "session.binding.cooldown_skipped") {
				t.Fatalf("跳过要留痕（旧实现只记 conflict，无从分辨「没该写」与「写失败」）：%s", logs.String())
			}
		})
	}
}

// legacyProviderMirror 读 legacy 供应商镜像键；空串表示无镜像。
func legacyProviderMirror(t *testing.T, rdb *redis.Client, sessionID string) string {
	t.Helper()
	raw, err := rdb.Get(context.Background(), LegacyProviderKey(sessionID)).Result()
	if errors.Is(err, redis.Nil) {
		return ""
	}
	if err != nil {
		t.Fatalf("读 legacy 镜像失败: %v", err)
	}
	return raw
}

// boundProviderString 把「播种的供应商 id」折成 canonical 字段的期望值（0 = 空绑定）。
func boundProviderString(providerID int64) string {
	if providerID <= 0 {
		return ""
	}
	return strconv.FormatInt(providerID, 10)
}
