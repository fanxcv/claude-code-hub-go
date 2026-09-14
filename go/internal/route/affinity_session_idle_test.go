package route

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 「会话空闲闸门」的 store 侧用例集。
//
// 背景（用户口径）：前缀亲和存在的意义是打中上游 prompt cache，而 cache 有时效
// （一般不超过 1 小时）。绑定键是**前缀级**且命中即续期，于是共享前缀的另一个会话
// （典型：pi 的 fork / 续跑会话复用同一段历史）会把旧绑定一直续命，某次对话闲置再久
// 也可能永不过期。判定「本会话闲置了多久」只能靠本会话自己的记录 ⇒ 记录按**客户端会话身份**记账。
//
// 本文件只钉 store 的两个方法（读、写）与键的选型；「命中被撤销」的选路行为在守卫层
// （`internal/guard` 的会话空闲闸门用例），因为会话身份只在那里可见。

func TestConversationActivityKeyShape(t *testing.T) {
	store := NewAffinityStore(AffinityOptions{})
	scope := "k1:openai-responses:deepseek"
	sessionID := "01a09b85-dc47-7737-96f4-b61bc634ab6d"

	key := store.conversationActivityKey(scope, sessionID)
	if want := "cch:pfx:{" + scope + "}:seen:" + sessionID; key != want {
		t.Fatalf("键形制 = %q，期望 %q", key, want)
	}
	// 与 fp/gen/desc 系列同 hash tag：同槽；且既有集成夹具的 `cch:pfx:{<scope>:*` 清扫模式
	// 能覆盖到它（否则会残留键，污染同一 Redis 库里的兄弟用例）。
	if !strings.HasPrefix(key, affinityKeyPrefix+"{"+scope+"}:") {
		t.Fatalf("键 %q 未落在 {scope} hash tag 内", key)
	}
	// 不得与任何一种既有键重名（撞名会让查找读到别人的值，静默改变命中语义）。
	for _, other := range []string{
		store.bindingKey(scope, sessionID),
		store.generationKey(scope, sessionID),
		store.descendantsKey(scope, sessionID),
		store.descendantsV2Key(scope, sessionID),
		store.legacyGenerationKey(scope),
	} {
		if key == other {
			t.Fatalf("活跃时刻键与既有键重名: %q", key)
		}
	}
	// 键只随会话身份变，**不随指纹变**：这正是它区别于绑定键（前缀级）的地方。
	if store.conversationActivityKey(scope, sessionID+"x") == key {
		t.Fatal("不同会话身份必须落到不同键")
	}
	if store.conversationActivityKey(scope+"2", sessionID) == key {
		t.Fatal("不同 scope 必须落到不同键")
	}
}

// 闸门必须 fail-open：读不到可信历史时**不**给出撤销依据。
func TestConversationIdleFailsOpenWithoutRedis(t *testing.T) {
	ctx := context.Background()

	// 未配置 Redis：不可信。
	noRedis := NewAffinityStore(AffinityOptions{SlidingTTLSeconds: 60})
	if _, _, known := noRedis.ConversationIdle(ctx, "scope", "session", 1000); known {
		t.Fatal("未配置 Redis 时不得给出可信记录")
	}
	// 会话身份为空：客户端未带 id ⇒ 不可信（调用方据此 fail-open 并留痕）。
	store := NewAffinityStore(AffinityOptions{SlidingTTLSeconds: 60})
	if _, _, known := store.ConversationIdle(ctx, "scope", "", 1000); known {
		t.Fatal("会话身份为空时不得给出可信记录")
	}
	// 阈值未配置（0）：判定无从谈起。
	zeroTTL := NewAffinityStore(AffinityOptions{SlidingTTLSeconds: 0})
	if _, _, known := zeroTTL.ConversationIdle(ctx, "scope", "session", 1000); known {
		t.Fatal("阈值为 0 时不得给出可信记录")
	}
	// 写侧在这三种情形下都不得 panic，也不得写出键（无 Redis 时静默返回即为正确）。
	store.NoteConversationActivity(ctx, "scope", "", 1000)
	noRedis.NoteConversationActivity(ctx, "scope", "session", 1000)
	zeroTTL.NoteConversationActivity(ctx, "scope", "session", 1000)
}

// 读侧语义：无记录⇒不可信、刚活跃⇒空闲 0、久未活跃⇒空闲可测且阈值与绑定 TTL 同源；
// 写侧 TTL 必须是阈值的若干倍（倍数=1 时记录与判定同时到期，闸门不可判定）。
func TestIntegrationConversationIdleSemantics(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	scope := affinityScope(t, client)

	const thresholdSeconds = 120
	store := NewAffinityStore(AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: thresholdSeconds,
	})
	sessionID := "01a09b85-" + strconv.Itoa(int(time.Now().UnixNano()))

	// ① 无记录：不可信（首条请求与「空闲超过记录 TTL」不可区分，只能放行）。
	if idle, threshold, known := store.ConversationIdle(ctx, scope, sessionID, time.Now().Unix()); known || idle != 0 {
		t.Fatalf("无记录时不得给出可信空闲量: idle=%d threshold=%d known=%v", idle, threshold, known)
	} else if threshold != thresholdSeconds {
		t.Fatalf("阈值应与绑定同源 = %d，实际 %d", thresholdSeconds, threshold)
	}

	// ② 刚活跃：空闲 0，且阈值仍照给（调用方留痕要用它）。
	now := time.Now().Unix()
	store.NoteConversationActivity(ctx, scope, sessionID, now)
	if idle, _, known := store.ConversationIdle(ctx, scope, sessionID, now); !known || idle != 0 {
		t.Fatalf("刚活跃应得 idle=0 且可信: idle=%d known=%v", idle, known)
	}

	// ③ 久未活跃：空闲超出阈值 —— 这是闸门唯一据以撤销的状态。
	recorded := now - thresholdSeconds - 5
	store.NoteConversationActivity(ctx, scope, sessionID, recorded)
	idle, threshold, known := store.ConversationIdle(ctx, scope, sessionID, now)
	if !known {
		t.Fatal("记录在场时必须可信")
	}
	if idle != int64(thresholdSeconds+5) {
		t.Fatalf("空闲量 = %d，期望 %d", idle, thresholdSeconds+5)
	}
	if idle <= threshold {
		t.Fatalf("空闲 %d 应大于阈值 %d", idle, threshold)
	}

	// ④ 记录 TTL 必须严格大于阈值（否则「无记录」与「久未活跃」不可区分，闸门不可判定）。
	key := store.conversationActivityKey(scope, sessionID)
	assertTTLBetween(t, client, key,
		time.Duration(thresholdSeconds*conversationActivityTTLFactor-30)*time.Second,
		time.Duration(thresholdSeconds*conversationActivityTTLFactor)*time.Second)
}

// 决定性一条：活跃记录按**会话身份**记账，与绑定的刷新（命中续期）彼此独立。
//
// 这一条正是前一版设计（按绑定指纹或它的 identity root 记账）做不到的地方：
// 那种记法下「刷新绑定的请求」必然同时刷新记录 ⇒ 共享前缀的别的会话会把本会话的
// 空闲状态一起抹掉 ⇒ 闸门永不触发（详见 conversationActivityKey 的说明）。
func TestIntegrationConversationActivityIsPerSessionNotPerBinding(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	scope := affinityScope(t, client)

	const thresholdSeconds = 120
	store := NewAffinityStore(AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: thresholdSeconds,
	})

	idleSession := "session-idle-" + strconv.Itoa(int(time.Now().UnixNano()))
	busySession := "session-busy-" + strconv.Itoa(int(time.Now().UnixNano()))
	tipFP := hash32(scope + "-shared-tip")

	// 两个会话共享同一个绑定（fork / 续跑会话复用同一段历史前缀：命中同一 tip 指纹）。
	binding := "1|7|" + tipFP + "|v3:shared"
	if err := client.Set(ctx, store.bindingKey(scope, tipFP), binding, 5*time.Minute).Err(); err != nil {
		t.Fatalf("写入共享绑定失败: %v", err)
	}
	// 共享绑定仍可命中 —— 即「绑定层面看不出谁闲置了」。
	lookup, ok := store.Lookup(ctx, scope, []string{tipFP})
	if !ok || lookup.Hint == nil || lookup.Hint.ProviderID != 7 {
		t.Fatalf("共享绑定应可命中: ok=%v lookup=%+v", ok, lookup)
	}

	now := time.Now().Unix()
	// 闲置会话的活跃时刻停在阈值之外；忙碌会话刚刚活跃。
	store.NoteConversationActivity(ctx, scope, idleSession, now-thresholdSeconds-1)
	store.NoteConversationActivity(ctx, scope, busySession, now)

	// 忙碌会话的活跃**不得**影响闲置会话的判定（键不同即独立）。
	idle, threshold, known := store.ConversationIdle(ctx, scope, idleSession, now)
	if !known || idle <= threshold {
		t.Fatalf("空闲会话应仍判定为空闲超阈: idle=%d threshold=%d known=%v", idle, threshold, known)
	}
	busyIdle, _, busyKnown := store.ConversationIdle(ctx, scope, busySession, now)
	if !busyKnown || busyIdle != 0 {
		t.Fatalf("忙碌会话应为空闲 0: idle=%d known=%v", busyIdle, busyKnown)
	}
	// 而共享绑定的 TTL 被命中续到**滑动阈值**（不是记录 TTL 的 4 倍，也不是我写入时的 5 分钟）：
	// 绑定与活跃记录是两套独立生命周期 —— 这正是闸门能看见「绑定仍活但本会话已闲置」的前提。
	assertTTLBetween(t, client, store.bindingKey(scope, tipFP),
		time.Duration(thresholdSeconds-20)*time.Second, time.Duration(thresholdSeconds)*time.Second)
}
