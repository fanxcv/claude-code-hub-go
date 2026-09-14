package limit

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/redis/go-redis/v9"
)

// 集成测试统一的门控变量与库号纪律：未设置时整组跳过；URL 未带库号时固定落在 13，
// 避免碰其它键空间（仓库约定）。
const testRedisEnv = "CCH_TEST_REDIS_URL"

const integrationDB = 13

// integrationClient 建一个真实 Redis 客户端；未设置门控变量时跳过。
func integrationClient(t *testing.T) *ratelimit.Client {
	t.Helper()
	rawURL := os.Getenv(testRedisEnv)
	if rawURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = integrationDB
	}
	rdb := redis.NewClient(options)
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("装载脚本清单失败: %v", err)
	}
	client, err := ratelimit.New(rdb, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client
}

// cleanupKeys 在测试结束后删掉本次用到的键（幂等，缺键不算错）。
func cleanupKeys(t *testing.T, client *ratelimit.Client, keys ...string) {
	t.Helper()
	raw := client.Raw()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, key := range keys {
			if err := raw.Del(ctx, key).Err(); err != nil {
				t.Logf("清理键 %s 失败: %v", key, err)
			}
		}
	})
}

// cleanupSessionMembers 只摘掉本次用到的会话成员，不删全局键。
//
// 全局活跃会话键是共享键（它不该被某个测试整键删掉，否则会打断并行测试的观测）。
func cleanupSessionMembers(t *testing.T, client *ratelimit.Client, sessionIDs ...string) {
	t.Helper()
	raw := client.Raw()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, sessionID := range sessionIDs {
			if err := raw.ZRem(ctx, ActiveSessionsGlobalKey(), sessionID).Err(); err != nil {
				t.Logf("清理全局会话成员 %s 失败: %v", sessionID, err)
			}
		}
	})
}

// trackedCostKeys 列出一次 TrackCost 可能写出的全部成本窗口键。
//
// TrackCost 一次写多个维度，逐个手写容易漏（漏掉的就是测试污染）。
func trackedCostKeys(keyID, providerID, userID int64, resetTime string) []string {
	entities := []struct {
		entity Entity
		id     int64
	}{
		{EntityKey, keyID},
		{EntityProvider, providerID},
		{EntityUser, userID},
	}
	keys := make([]string, 0, len(entities)*6)
	for _, item := range entities {
		keys = append(keys,
			Cost5hKey(item.entity, item.id, ResetRolling),
			Cost5hKey(item.entity, item.id, ResetFixed),
			CostDailyRollingKey(item.entity, item.id),
			CostDailyFixedKey(item.entity, item.id, resetTime),
			CostPeriodFixedKey(item.entity, item.id, PeriodWeekly),
			CostPeriodFixedKey(item.entity, item.id, PeriodMonthly),
		)
	}
	return keys
}

// uniqueID 给每次运行一组互不冲突的实体 id。
func uniqueID(t *testing.T) int64 {
	t.Helper()
	return time.Now().UnixNano() % 1_000_000_000
}

func TestIntegrationRollingCostWindowExcludesExpiredEntries(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	windows := NewCostWindows(client, nil)
	id := uniqueID(t)

	cleanupKeys(t, client, trackedCostKeys(id, id+1, id+2, "00:00")...)

	now := time.Now().UTC()
	if err := windows.TrackCost(ctx, TrackCostInput{
		KeyID:               id,
		ProviderID:          id + 1,
		Cost:                2.5,
		CreatedAt:           now,
		RequestID:           "req-1",
		Key5hResetMode:      ResetRolling,
		KeyResetMode:        ResetRolling,
		Provider5hResetMode: ResetRolling,
		ProviderResetMode:   ResetRolling,
	}, time.UTC, now); err != nil {
		t.Fatalf("记账失败: %v", err)
	}

	current, exists, err := windows.RollingCost(ctx, EntityKey, id, Period5h, now)
	if err != nil {
		t.Fatalf("读 5h 滚动窗口失败: %v", err)
	}
	if !exists || current != 2.5 {
		t.Fatalf("5h 滚动窗口 = %v（exists=%v），期望 2.5", current, exists)
	}

	daily, exists, err := windows.RollingCost(ctx, EntityKey, id, PeriodDaily, now)
	if err != nil || !exists || daily != 2.5 {
		t.Fatalf("daily 滚动窗口 = %v（exists=%v，err=%v），期望 2.5", daily, exists, err)
	}

	// 窗口外的记账：成员会被 GET 脚本按 cutoff 清掉，故不计入。
	if err := windows.TrackCost(ctx, TrackCostInput{
		KeyID:               id,
		ProviderID:          id + 1,
		Cost:                9,
		CreatedAt:           now.Add(-6 * time.Hour),
		Key5hResetMode:      ResetRolling,
		KeyResetMode:        ResetRolling,
		Provider5hResetMode: ResetRolling,
		ProviderResetMode:   ResetRolling,
	}, time.UTC, now); err != nil {
		t.Fatalf("记账失败: %v", err)
	}
	current, _, err = windows.RollingCost(ctx, EntityKey, id, Period5h, now)
	if err != nil {
		t.Fatalf("读 5h 滚动窗口失败: %v", err)
	}
	if current != 2.5 {
		t.Errorf("5h 窗口外的记账被计入: %v，期望 2.5", current)
	}
	// daily 窗口包含 6 小时前的消费（24h 窗口）。
	daily, _, err = windows.RollingCost(ctx, EntityKey, id, PeriodDaily, now)
	if err != nil {
		t.Fatalf("读 daily 滚动窗口失败: %v", err)
	}
	if daily != 11.5 {
		t.Errorf("daily 滚动窗口 = %v，期望 11.5", daily)
	}
}

func TestIntegrationFixedWindowsCarryResetMetadata(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	windows := NewCostWindows(client, nil)
	id := uniqueID(t)

	dailyKey := CostDailyFixedKey(EntityUser, id, "18:00")
	// 周/月固定窗口只记 Key 与 Provider：User 的长周期额度由账本（PostgreSQL）负责，
	// 这与 Node 侧 trackCost 的分工一致。
	weeklyKey := CostPeriodFixedKey(EntityKey, id+1, PeriodWeekly)
	monthlyKey := CostPeriodFixedKey(EntityKey, id+1, PeriodMonthly)
	userWeeklyKey := CostPeriodFixedKey(EntityUser, id, PeriodWeekly)
	cleanupKeys(t, client, append(trackedCostKeys(id+1, id+2, id, "18:00"), userWeeklyKey)...)

	now := time.Now().UTC()
	input := TrackCostInput{
		KeyID:             id + 1,
		ProviderID:        id + 2,
		UserID:            id,
		UserIDSet:         true,
		Cost:              1.25,
		CreatedAt:         now,
		User5hResetMode:   ResetFixed,
		UserResetMode:     ResetFixed,
		UserResetTime:     "18:00",
		KeyResetMode:      ResetFixed,
		KeyResetTime:      "18:00",
		Key5hResetMode:    ResetFixed,
		ProviderResetTime: "18:00",
	}
	if err := windows.TrackCost(ctx, input, time.UTC, now); err != nil {
		t.Fatalf("记账失败: %v", err)
	}
	input.Cost = 0.75
	if err := windows.TrackCost(ctx, input, time.UTC, now); err != nil {
		t.Fatalf("第二次记账失败: %v", err)
	}

	state, err := windows.Fixed5hWindowState(ctx, EntityUser, id, now)
	if err != nil {
		t.Fatalf("读 5h 固定窗口失败: %v", err)
	}
	if !state.Exists || state.Current != 2 {
		t.Fatalf("5h 固定窗口 = %v（exists=%v），期望 2", state.Current, state.Exists)
	}
	if state.ResetAt == nil {
		t.Fatal("5h 固定窗口应由 TTL 反推出重置时刻")
	}
	if remaining := state.ResetAt.Sub(now); remaining <= 0 || remaining > 5*time.Hour+time.Minute {
		t.Errorf("5h 固定窗口重置时刻 = %s，剩余 %s，期望落在 5 小时内", state.ResetAt, remaining)
	}

	daily, exists, err := windows.FixedCost(ctx, dailyKey)
	if err != nil || !exists || daily != 2 {
		t.Fatalf("daily 固定窗口 = %v（exists=%v，err=%v），期望 2", daily, exists, err)
	}
	weekly, exists, err := windows.FixedCost(ctx, weeklyKey)
	if err != nil || !exists || weekly != 2 {
		t.Fatalf("周窗口 = %v（exists=%v，err=%v），期望 2", weekly, exists, err)
	}
	monthly, exists, err := windows.FixedCost(ctx, monthlyKey)
	if err != nil || !exists || monthly != 2 {
		t.Fatalf("月窗口 = %v（exists=%v，err=%v），期望 2", monthly, exists, err)
	}
	if _, exists, err := windows.FixedCost(ctx, userWeeklyKey); err != nil || exists {
		t.Errorf("User 周窗口不应由数据面记账（exists=%v，err=%v）", exists, err)
	}

	// TTL 落位检查：daily 的 TTL 必须指向 18:00，落在 (0, 24h]。
	ttl := client.Raw().TTL(ctx, dailyKey).Val()
	if ttl <= 0 || ttl > 24*time.Hour {
		t.Errorf("daily 固定窗口 TTL = %s，期望落在 24 小时内", ttl)
	}
}

func TestIntegrationKeyUserConcurrentSessions(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	tracker := NewSessionTracker(client, 300*time.Second, nil)
	keyID, userID := uniqueID(t), uniqueID(t)+1
	cleanupKeys(t, client, KeyActiveSessionsKey(keyID), UserActiveSessionsKey(userID))
	cleanupSessionMembers(t, client, "sess-a", "sess-b")

	first, err := tracker.CheckAndTrackKeyUserSession(ctx, keyID, userID, "sess-a", 1, 0)
	if err != nil {
		t.Fatalf("首次检查失败: %v", err)
	}
	if !first.Allowed || !first.TrackedKey || first.KeyCount != 1 {
		t.Fatalf("首次检查应放行并记账: %+v", first)
	}

	second, err := tracker.CheckAndTrackKeyUserSession(ctx, keyID, userID, "sess-b", 1, 0)
	if err != nil {
		t.Fatalf("第二次检查失败: %v", err)
	}
	if second.Allowed || second.RejectedBy != string(EntityKey) {
		t.Fatalf("第二个会话应被 Key 维度拒绝: %+v", second)
	}
	if second.Current != 1 || second.Limit != 1 {
		t.Errorf("拒绝信息不符: current=%d limit=%d", second.Current, second.Limit)
	}

	// 已追踪的会话即使在满额后继续请求也必须放行，否则会把自己锁死。
	repeat, err := tracker.CheckAndTrackKeyUserSession(ctx, keyID, userID, "sess-a", 1, 0)
	if err != nil {
		t.Fatalf("已追踪会话复查失败: %v", err)
	}
	if !repeat.Allowed || repeat.TrackedKey {
		t.Fatalf("已存在的会话应放行且不重复计数: %+v", repeat)
	}

	// 强制终止后额度应释放。
	if _, err := tracker.ForceTerminateKeyUserSession(ctx, keyID, userID, "sess-a"); err != nil {
		t.Fatalf("强制终止失败: %v", err)
	}
	afterRelease, err := tracker.CheckAndTrackKeyUserSession(ctx, keyID, userID, "sess-b", 1, 0)
	if err != nil {
		t.Fatalf("释放后检查失败: %v", err)
	}
	if !afterRelease.Allowed {
		t.Fatalf("强制终止后新会话应放行: %+v", afterRelease)
	}
}

func TestIntegrationProviderSessionRefsGateConcurrency(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	tracker := NewSessionTracker(client, 300*time.Second, nil)
	providerID := uniqueID(t) + 7
	cleanupKeys(t, client, ProviderActiveSessionsKey(providerID), ProviderSessionRefsKey(providerID))

	first, err := tracker.CheckAndTrackProviderSession(ctx, providerID, "sess-1", 1)
	if err != nil {
		t.Fatalf("供应商并发检查失败: %v", err)
	}
	if !first.Allowed || !first.Tracked || !first.Referenced || first.Count != 1 {
		t.Fatalf("首次应放行并取得引用: %+v", first)
	}

	second, err := tracker.CheckAndTrackProviderSession(ctx, providerID, "sess-2", 1)
	if err != nil {
		t.Fatalf("供应商并发检查失败: %v", err)
	}
	if second.Allowed {
		t.Fatalf("第二个会话应被供应商并发上限拒绝: %+v", second)
	}

	// 同一会话再次占用：已是追踪态，计数不变，但会再拿一个引用（重试/hedge 场景）。
	again, err := tracker.CheckAndTrackProviderSession(ctx, providerID, "sess-1", 1)
	if err != nil {
		t.Fatalf("同会话再次占用失败: %v", err)
	}
	if !again.Allowed || again.Tracked || again.Count != 1 {
		t.Fatalf("同会话再次占用不应增加并发计数: %+v", again)
	}

	removed, remaining, err := tracker.ReleaseProviderSession(ctx, providerID, "sess-1")
	if err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if removed != 0 || remaining != 1 {
		t.Fatalf("释放一次后仍有引用: removed=%d remaining=%d", removed, remaining)
	}

	removed, remaining, err = tracker.ReleaseProviderSession(ctx, providerID, "sess-1")
	if err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if removed != 1 || remaining != 0 {
		t.Fatalf("引用归零时应移出并发集合: removed=%d remaining=%d", removed, remaining)
	}

	// 释放后并发额度重新可用。
	third, err := tracker.CheckAndTrackProviderSession(ctx, providerID, "sess-3", 1)
	if err != nil {
		t.Fatalf("释放后检查失败: %v", err)
	}
	if !third.Allowed {
		t.Fatalf("释放后应放行: %+v", third)
	}
	removedSession, removedRefs, err := tracker.ForceTerminateProviderSession(ctx, providerID, "sess-3")
	if err != nil {
		t.Fatalf("强制终止失败: %v", err)
	}
	if removedSession != 1 || removedRefs != 1 {
		t.Errorf("强制终止结果不符: session=%d refs=%d", removedSession, removedRefs)
	}
}

func TestIntegrationCheckBlocksConcurrentAndRPMLimits(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	keyID, userID := uniqueID(t), uniqueID(t)+3
	cleanupKeys(t, client, KeyActiveSessionsKey(keyID), UserActiveSessionsKey(userID), RPMWindowKey(userID))
	cleanupSessionMembers(t, client, "sess-fixed", "sess-other")

	session := "sess-fixed"
	service, err := New(Config{
		Quotas: &fakeQuotaSource{
			key:  KeyQuota{KeyID: keyID, KeyHash: "sk-int", LimitConcurrentSessions: 1},
			user: UserQuota{UserID: userID},
		},
		Redis:      client,
		Location:   time.UTC,
		SessionTTL: 300 * time.Second,
		SessionID:  func(*pctx.Context) (string, error) { return session, nil },
	})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}

	req, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatal(err)
	}
	req.SetAuth(pctx.AuthState{KeyID: keyID, UserID: userID})

	block, err := service.Check(ctx, req)
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block != nil {
		t.Fatalf("首个会话应放行: %+v", block)
	}

	// 换一个会话 id：Key 并发上限为 1，应被拒。
	session = "sess-other"
	block, err = service.Check(ctx, req)
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil || block.Status != 429 {
		t.Fatalf("并发超限应返回 429: %+v", block)
	}
	if !strings.Contains(block.BlockedReason, `"limit_type":"concurrent_sessions"`) {
		t.Errorf("limit_type 不符: %q", block.BlockedReason)
	}
	if !strings.Contains(block.Message, "并发 Session 超限") {
		t.Errorf("文案不符: %q", block.Message)
	}

	// RPM 维度：把会话额度放开、用户 RPM 设为 1，第二次请求应被 429 拦下。
	service.cfg.Quotas = &fakeQuotaSource{
		key:  KeyQuota{KeyID: keyID, KeyHash: "sk-int"},
		user: UserQuota{UserID: userID, RPM: 1},
	}
	service.sessions.ForceTerminateKeyUserSession(ctx, keyID, userID, "sess-fixed")
	service.sessions.ForceTerminateKeyUserSession(ctx, keyID, userID, "sess-other")

	if block, err = service.Check(ctx, req); err != nil || block != nil {
		t.Fatalf("RPM 首请求应放行: block=%+v err=%v", block, err)
	}
	block, err = service.Check(ctx, req)
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil || block.Status != 429 {
		t.Fatalf("RPM 超限应返回 429: %+v", block)
	}
	if !strings.Contains(block.BlockedReason, `"limit_type":"rpm"`) {
		t.Errorf("limit_type 不符: %q", block.BlockedReason)
	}
	if block.RetryAfterSeconds == nil || *block.RetryAfterSeconds <= 0 || *block.RetryAfterSeconds > 60 {
		t.Errorf("RPM 的 Retry-After 应落在 60 秒内: %v", block.RetryAfterSeconds)
	}
}

func TestIntegrationCacheMissFallsBackToLedgerAndWarmsFixedWindow(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	keyID := uniqueID(t) + 11
	keyHash := "sk-cache-miss-" + itoa(keyID)
	dailyKey := CostDailyFixedKey(EntityKey, keyID, "00:00")
	cleanupKeys(t, client, dailyKey, TotalCostCacheKey(EntityKey, keyID, keyHash, nil))

	ledger := &fakeLedger{
		totals: map[string]float64{"key/" + keyHash: 3},
		ranges: map[string]float64{"key/" + keyHash: 3},
	}
	service, err := New(Config{
		Quotas: &fakeQuotaSource{key: KeyQuota{
			KeyID:          keyID,
			KeyHash:        keyHash,
			LimitDailyUSD:  floatPtr(2),
			DailyResetTime: "00:00",
			DailyResetMode: ResetFixed,
		}},
		Ledger:   ledger,
		Redis:    client,
		Location: time.UTC,
	})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}

	req, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatal(err)
	}
	req.SetAuth(pctx.AuthState{KeyID: keyID, UserID: keyID + 1})

	block, err := service.Check(ctx, req)
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil {
		t.Fatal("账本显示已超限，应拦截")
	}
	if len(ledger.calls) != 1 {
		t.Fatalf("账本查询次数 = %d，期望 1（缓存未命中触发回退）", len(ledger.calls))
	}

	// 固定窗口的缓存回写：下一次判定应直接读 Redis，不再查账本。
	cached, exists, err := NewCostWindows(client, nil).FixedCost(ctx, dailyKey)
	if err != nil || !exists || cached != 3 {
		t.Fatalf("固定窗口缓存回写失败: value=%v exists=%v err=%v", cached, exists, err)
	}

	service.cfg.Quotas = &fakeQuotaSource{key: KeyQuota{
		KeyID:          keyID,
		KeyHash:        keyHash,
		LimitDailyUSD:  floatPtr(2),
		DailyResetTime: "00:00",
		DailyResetMode: ResetFixed,
	}}
	before := len(ledger.calls)
	if _, err := service.Check(ctx, req); err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if len(ledger.calls) != before {
		t.Errorf("缓存命中后不应再查账本，账本查询从 %d 增至 %d", before, len(ledger.calls))
	}
}

func TestIntegrationTotalCostCacheIsReusedAndResetAware(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	keyID := uniqueID(t) + 21
	keyHash := "sk-total-" + itoa(keyID)
	cacheKey := TotalCostCacheKey(EntityKey, keyID, keyHash, nil)
	cleanupKeys(t, client, cacheKey)

	ledger := &fakeLedger{totals: map[string]float64{"key/" + keyHash: 4}}
	service, err := New(Config{
		Quotas: &fakeQuotaSource{},
		Ledger: ledger,
		Redis:  client,
	})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}

	now := time.Now().UTC()
	current, err := service.totalCost(ctx, EntityKey, keyID, keyHash, nil, now)
	if err != nil || current != 4 {
		t.Fatalf("总额度读取 = %v（err=%v），期望 4", current, err)
	}
	// 缓存回写是异步的，等一下再断言。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if exists := client.Raw().Exists(ctx, cacheKey).Val(); exists == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cached, err := client.Raw().Get(ctx, cacheKey).Result()
	if err != nil {
		t.Fatalf("总额度缓存未回写: %v", err)
	}
	if ParseCostText(cached) != 4 {
		t.Errorf("缓存值 = %s，期望 4", cached)
	}

	if _, err := service.totalCost(ctx, EntityKey, keyID, keyHash, nil, now); err != nil {
		t.Fatalf("第二次读取失败: %v", err)
	}

	// 重置时刻参与键名：换一个重置时刻就是另一个键，不会读到旧值。
	resetAt := now.Add(-time.Hour)
	otherKey := TotalCostCacheKey(EntityKey, keyID, keyHash, &resetAt)
	cleanupKeys(t, client, otherKey)
	if otherKey == cacheKey {
		t.Fatal("带重置时刻的缓存键应与不带的不同")
	}
}
