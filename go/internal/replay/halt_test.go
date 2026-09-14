package replay

import (
	"context"
	"testing"
	"time"
)

// TestSpoolLeaseTakeoverHaltsWithoutDeletingSuccessor：owner 租约被后来的请求接管时，
// 旧 spool 必须停写并让位（halt），绝不能删掉新 owner 正在写的条目。
func TestSpoolLeaseTakeoverHaltsWithoutDeletingSuccessor(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	storeInstance, err := NewStore(StoreOptions{Redis: client, TTL: time.Minute})
	if err != nil {
		t.Fatalf("构造存储失败: %v", err)
	}
	identity := fixtureIdentity()
	replayID := identity.ReplayID
	t.Cleanup(func() {
		_ = client.Del(context.Background(), ownerKey(replayID), metaKey(replayID), chunksKey(replayID)).Err()
	})

	baselineSpools := ActiveSpoolCount()
	if !storeInstance.TryClaimOwner(ctx, replayID, "token-a") {
		t.Fatal("claim 失败")
	}
	spool := NewSpool(storeInstance, identity, "token-a", 200, map[string]string{},
		DeliveryStream, SpoolOptions{MaxConcurrentSpools: 8, HeartbeatInterval: 20 * time.Millisecond})
	if spool == nil {
		t.Fatal("spool 创建失败")
	}
	if ActiveSpoolCount() != baselineSpools+1 {
		t.Fatal("spool 名额未计入")
	}
	spool.Observe([]byte("data: old\n\n"))

	// 模拟接管：删掉旧租约，新 owner 用 token-b 抢占并写入自己的正文。
	if err := client.Del(ctx, ownerKey(replayID)).Err(); err != nil {
		t.Fatalf("清理旧租约失败: %v", err)
	}
	if !storeInstance.TryClaimOwner(ctx, replayID, "token-b") {
		t.Fatal("接管的 claim 失败")
	}
	meta := &Meta{Status: MetaOwning, Verifier: identity.Verifier, ScopeTag: identity.ScopeTag,
		Delivery: DeliveryStream}
	if _, outcome := storeInstance.WriteOwned(
		ctx, replayID, "token-b", meta, []string{"data: new\n\n"},
	); outcome != WriteOK {
		t.Fatalf("新 owner 写入失败: %v", outcome)
	}

	waitForRelease(t, spool)
	if ActiveSpoolCount() != baselineSpools {
		t.Fatalf("halt 后应归还 spool 名额，当前 %d", ActiveSpoolCount())
	}
	// 新 owner 的条目必须完好（halt 绝不删条目）。
	chunks, outcome := storeInstance.ReadOwnedChunks(ctx, replayID, "token-b", 0, 8)
	if outcome != ReadOK {
		t.Fatalf("新 owner 条目应仍可读: outcome=%v", outcome)
	}
	if len(chunks) != 1 || chunks[0] != "data: new\n\n" {
		t.Fatalf("新 owner 正文被破坏: %v", chunks)
	}
}

// TestSpoolWriteLeaseLossHalts：写入时发现租约易主（WriteLeaseLost）必须停写而非删条目。
func TestSpoolWriteLeaseLossHalts(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	storeInstance, err := NewStore(StoreOptions{Redis: client, TTL: time.Minute})
	if err != nil {
		t.Fatalf("构造存储失败: %v", err)
	}
	identity := fixtureIdentity()
	replayID := identity.ReplayID
	t.Cleanup(func() {
		_ = client.Del(context.Background(), ownerKey(replayID), metaKey(replayID), chunksKey(replayID)).Err()
	})

	baselineSpools := ActiveSpoolCount()
	if !storeInstance.TryClaimOwner(ctx, replayID, "token-a") {
		t.Fatal("claim 失败")
	}
	// 心跳间隔取默认（15s），确保本用例只由写入路径发现租约易主。
	spool := NewSpool(storeInstance, identity, "token-a", 200, map[string]string{},
		DeliveryStream, SpoolOptions{MaxConcurrentSpools: 8})
	if spool == nil {
		t.Fatal("spool 创建失败")
	}
	// 等 bootstrap 先落地，否则接管可能早于首次写入，租约易主就无人察觉
	// （心跳 15s 一轮，本用例只等几秒）。
	waitForMeta(t, storeInstance, replayID)

	if err := client.Del(ctx, ownerKey(replayID)).Err(); err != nil {
		t.Fatalf("清理旧租约失败: %v", err)
	}
	if !storeInstance.TryClaimOwner(ctx, replayID, "token-b") {
		t.Fatal("接管的 claim 失败")
	}

	// 触发一次冲刷：WriteOwned 会因 token 不符返回 WriteLeaseLost -> halt。
	spool.Observe(makeStreamPayload(flushBytesThreshold))
	waitForRelease(t, spool)
	if ActiveSpoolCount() != baselineSpools {
		t.Fatalf("halt 后应归还 spool 名额，当前 %d", ActiveSpoolCount())
	}
	// halt 不得动新 owner 的租约，也不得写 aborted meta 遮蔽在写条目。
	owner, err := client.Get(ctx, ownerKey(replayID)).Result()
	if err != nil || owner != "token-b" {
		t.Fatalf("halt 动了新 owner 的租约: %q, %v", owner, err)
	}
	meta, err := storeInstance.GetMeta(ctx, replayID)
	if err != nil {
		t.Fatalf("读 meta 失败: %v", err)
	}
	if meta != nil && meta.Status == MetaAborted {
		t.Fatalf("halt 写下了 aborted meta 遮蔽在写条目: %+v", meta)
	}
}

// waitForMeta 等 spool 的 bootstrap 落地（写入链首个作业完成）。
func waitForMeta(t *testing.T, storeInstance *Store, replayID string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if meta, err := storeInstance.GetMeta(ctx, replayID); err == nil && meta != nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("bootstrap 未在期限内落地 owning meta")
}

// TestSpoolDurableConflictDiscardsOwnEntry：已存在内容不一致的 durable winner 时，
// 本 spool 让位（discard）：不落库、不写 aborted 遮蔽 winner、热层候选清干净。
func TestSpoolDurableConflictDiscardsOwnEntry(t *testing.T) {
	client := integrationRedis(t)
	pools := openTestPools(t)
	ctx := context.Background()
	storeInstance, err := NewStore(StoreOptions{Redis: client, Pools: pools, TTL: time.Minute})
	if err != nil {
		t.Fatalf("构造存储失败: %v", err)
	}
	identity := fixtureIdentity()
	replayID := identity.ReplayID
	t.Cleanup(func() {
		_ = client.Del(context.Background(), ownerKey(replayID), metaKey(replayID), chunksKey(replayID)).Err()
		if pool, err := pools.Control(); err == nil {
			_, _ = pool.Exec(ctx, `DELETE FROM replay_payloads WHERE replay_id = $1`, replayID)
		}
	})

	winner := PersistedRow{
		ReplayID: replayID, Verifier: identity.Verifier, ScopeTag: identity.ScopeTag,
		KeyID: identity.KeyID, UserID: identity.UserID, Format: identity.Format,
		StatusCode: 200, Headers: map[string]string{"content-type": "text/event-stream"},
		Payload: "data: winner\n\n", ByteSize: 14, ExpiresAt: time.Now().Add(time.Hour),
	}
	if result, err := storeInstance.PersistCompleted(ctx, winner); err != nil || result != PersistWritten {
		t.Fatalf("预置 winner 失败: %s, %v", result, err)
	}

	baselineSpools := ActiveSpoolCount()
	if !storeInstance.TryClaimOwner(ctx, replayID, "token-a") {
		t.Fatal("claim 失败")
	}
	spool := NewSpool(storeInstance, identity, "token-a", 200,
		map[string]string{"content-type": "text/event-stream"}, DeliveryStream,
		SpoolOptions{MaxConcurrentSpools: 8})
	if spool == nil {
		t.Fatal("spool 创建失败")
	}
	spool.Observe([]byte("data: loser\n\n"))
	if err := spool.CompleteAndPersist(ctx, 77); err != nil {
		t.Fatalf("冲突应让位而非报错: %v", err)
	}
	if ActiveSpoolCount() != baselineSpools {
		t.Fatalf("discard 后应归还 spool 名额，当前 %d", ActiveSpoolCount())
	}
	persisted, err := storeInstance.FindCompleted(ctx, replayID)
	if err != nil || persisted == nil {
		t.Fatalf("winner 应仍在: %v, %v", persisted, err)
	}
	if persisted.Payload != winner.Payload {
		t.Fatalf("winner 正文被覆盖: %q", persisted.Payload)
	}
	// 热层候选不得留下遮蔽 winner 的 aborted 条目。
	if meta, err := storeInstance.GetMeta(ctx, replayID); err != nil {
		t.Fatalf("读 meta 失败: %v", err)
	} else if meta != nil {
		t.Fatalf("discard 后热层不应留条目，实际 %+v", meta)
	}
}
