package limit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/redis/go-redis/v9"
)

// 本文件钉住租约族的语义（Node `src/lib/rate-limit/lease.ts` + `lease-service.ts`）。
// 纯单测覆盖键形制、切片数学、序列化与判定分支；Redis 行为（两段 Lua）单列集成用例，
// 未注入 CCH_TEST_REDIS_URL 时跳过。

// leaseSettingsStub 是固定返回一份设置的桩。
type leaseSettingsStub struct {
	settings QuotaLeaseSettings
	err      error
	calls    int
}

func (s *leaseSettingsStub) QuotaLeaseSettings(_ context.Context) (QuotaLeaseSettings, error) {
	s.calls++
	if s.err != nil {
		return QuotaLeaseSettings{}, s.err
	}
	return s.settings, nil
}

// deadRedisClient 是「地址不可达」的调用层：Ready() 为真（Raw 非空），但命令一律失败，
// 用来覆盖「Redis 在、但读写失败」的分支。
func deadRedisClient(t *testing.T) *ratelimit.Client {
	t.Helper()
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("装载脚本清单失败: %v", err)
	}
	raw := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:6399",
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = raw.Close() })
	client, err := ratelimit.New(raw, registry)
	if err != nil {
		t.Fatalf("组装调用层失败: %v", err)
	}
	return client
}

func TestBuildLeaseKeyShapes(t *testing.T) {
	cases := []struct {
		name      string
		entity    LeaseEntity
		id        int64
		window    LeaseWindow
		resetMode ResetMode
		wantKey   string
	}{
		{"user 5h rolling 带模式后缀", EntityUser, 5, LeaseWindow5h, ResetRolling, "lease:user:5:5h:rolling"},
		{"key daily fixed 带模式后缀", EntityKey, 7, LeaseWindowDaily, ResetFixed, "lease:key:7:daily:fixed"},
		{"weekly 不带模式后缀", EntityKey, 7, LeaseWindowWeekly, ResetFixed, "lease:key:7:weekly"},
		{"monthly 不带模式后缀", EntityProvider, 9, LeaseWindowMonthly, ResetFixed, "lease:provider:9:monthly"},
		{"5h 模式缺省为 rolling", EntityUser, 5, LeaseWindow5h, "", "lease:user:5:5h:rolling"},
		{"daily 模式缺省为 fixed", EntityUser, 5, LeaseWindowDaily, "", "lease:user:5:daily:fixed"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := BuildLeaseKey(testCase.entity, testCase.id, testCase.window, testCase.resetMode)
			if got != testCase.wantKey {
				t.Errorf("BuildLeaseKey = %q，期望 %q", got, testCase.wantKey)
			}
		})
	}
}

func TestCalculateLeaseSlice(t *testing.T) {
	capSmall := 0.1
	cases := []struct {
		name      string
		limit     float64
		usage     float64
		percent   float64
		capUSD    *float64
		wantSlice float64
	}{
		{"按比例切片", 10, 0, 0.05, nil, 0.5},
		{"剩余额度小于切片", 10, 9.8, 0.05, nil, 0.2},
		{"上限截断", 10, 0, 0.05, &capSmall, 0.1},
		{"额度用尽", 10, 10, 0.05, nil, 0},
		{"超用为 0 不取负", 10, 12, 0.05, nil, 0},
		{"比例归一到 4 位小数", 3, 0, 0.33333, nil, 1.0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := CalculateLeaseSlice(testCase.limit, testCase.usage, testCase.percent, testCase.capUSD)
			if got != testCase.wantSlice {
				t.Errorf("CalculateLeaseSlice = %v，期望 %v", got, testCase.wantSlice)
			}
		})
	}
}

func TestLeaseSerializeRoundTrip(t *testing.T) {
	resetAt := int64(1789000000000)
	windowReset := int64(1789000001000)
	lease := BudgetLease{
		EntityType: "user", EntityID: 5, Window: "5h", ResetMode: "rolling", ResetTime: "04:00",
		SnapshotAtMS: 1788999999000, CurrentUsage: 1.25, LimitAmount: 10, RemainingBudget: 0.5, TTLSeconds: 10,
		CostResetAtMS: &resetAt, WindowResetAtMS: &windowReset,
	}
	payload, err := SerializeLease(lease)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	// 字段名是跨语言契约（Node 按 camelCase 读同一份 Redis 正文），改名即破坏回退兼容。
	for _, field := range []string{`"remainingBudget"`, `"snapshotAtMs"`, `"costResetAtMs"`, `"windowResetAtMs"`} {
		if !strings.Contains(payload, field) {
			t.Errorf("序列化结果缺少 %s: %s", field, payload)
		}
	}
	decoded, ok := DeserializeLease(payload)
	if !ok {
		t.Fatalf("反序列化失败: %s", payload)
	}
	if decoded.EntityID != 5 || decoded.RemainingBudget != 0.5 || decoded.CurrentUsage != 1.25 {
		t.Errorf("往返后字段不符: %+v", decoded)
	}
	if decoded.CostResetAtMS == nil || *decoded.CostResetAtMS != resetAt || decoded.WindowResetAtMS == nil || *decoded.WindowResetAtMS != windowReset {
		t.Errorf("往返后重置时刻丢失: %+v", decoded)
	}
}

func TestDeserializeLeaseRejectsIncomplete(t *testing.T) {
	// 必填字段缺失时必须判为非法：宁可当缓存未命中重算，也不拿半份租约做判定。
	missing := []string{
		`{"entityType":"user","entityId":1,"window":"5h","resetMode":"rolling","resetTime":"04:00","snapshotAtMs":1,"currentUsage":0,"limitAmount":1,"ttlSeconds":10}`,
		`not json`,
		`{"entityType":"","entityId":1,"window":"5h","resetMode":"rolling","resetTime":"04:00","snapshotAtMs":1,"currentUsage":0,"limitAmount":1,"remainingBudget":0,"ttlSeconds":10}`,
	}
	for _, payload := range missing {
		if _, ok := DeserializeLease(payload); ok {
			t.Errorf("非法租约应被拒绝: %s", payload)
		}
	}
}

func TestIsLeaseExpired(t *testing.T) {
	lease := BudgetLease{SnapshotAtMS: 1000, TTLSeconds: 10}
	if IsLeaseExpired(lease, 10999) {
		t.Error("TTL 未到不应判过期")
	}
	if !IsLeaseExpired(lease, 11000) {
		t.Error("TTL 到期应判过期")
	}
}

// TestLeaseCheckBlocksWhenLimitConsumed 覆盖「DB 权威用量已达限额 → 切片为 0 → 拒绝」。
//
// 这里 Redis 地址不可达：读缓存失败后回表刷新，写缓存失败不影响本次判定（Node 同款）。
func TestLeaseCheckBlocksWhenLimitConsumed(t *testing.T) {
	settings := &leaseSettingsStub{settings: QuotaLeaseSettings{RefreshIntervalSeconds: 10, Percent5h: 0.05}}
	ledger := &fakeLedger{ranges: map[string]float64{"key/sk-lease": 10}}
	service := NewLeaseService(deadRedisClient(t), settings, ledger, time.UTC, func() time.Time { return fixedNow }, nil)
	if service == nil {
		t.Fatal("租约服务应可组装")
	}

	params := GetCostLeaseParams{
		Entity: EntityKey, EntityID: 7, KeyHash: "sk-lease", Window: LeaseWindow5h,
		LimitAmount: 10, ResetTime: "04:00", ResetMode: ResetRolling,
	}
	allowed, current, ok := service.CheckCostLimitWithLease(context.Background(), params)
	if !ok {
		t.Fatal("设置可读、账本可读时应给出判定")
	}
	if allowed {
		t.Errorf("用量已达限额应拒绝，当前用量 %v", current)
	}
	if current != 10 {
		t.Errorf("currentUsage = %v，期望 10", current)
	}
	// key 维度必须按密钥字符串聚合（与账本回退同口径）。
	if len(ledger.calls) != 1 || ledger.calls[0].entityID != "sk-lease" {
		t.Errorf("账本聚合口径不符: %+v", ledger.calls)
	}
}

// TestLeaseCheckAllowsWhenBudgetSliced 覆盖「用量远低于限额 → 切片 > 0 → 放行」。
func TestLeaseCheckAllowsWhenBudgetSliced(t *testing.T) {
	settings := &leaseSettingsStub{settings: QuotaLeaseSettings{RefreshIntervalSeconds: 10, Percent5h: 0.05}}
	ledger := &fakeLedger{ranges: map[string]float64{"user/5": 1}}
	service := NewLeaseService(deadRedisClient(t), settings, ledger, time.UTC, func() time.Time { return fixedNow }, nil)

	allowed, current, ok := service.CheckCostLimitWithLease(context.Background(), GetCostLeaseParams{
		Entity: EntityUser, EntityID: 5, Window: LeaseWindow5h, LimitAmount: 10,
		ResetTime: "04:00", ResetMode: ResetRolling,
	})
	if !ok || !allowed {
		t.Fatalf("应有切片并放行：ok=%v allowed=%v current=%v", ok, allowed, current)
	}
}

// TestLeaseCheckFailsOpenWhenSettingsUnreadable 覆盖「设置读不到 → ok=false → 交调用方回退」。
func TestLeaseCheckFailsOpenWhenSettingsUnreadable(t *testing.T) {
	settings := &leaseSettingsStub{err: errors.New("db down")}
	service := NewLeaseService(deadRedisClient(t), settings, &fakeLedger{}, time.UTC, nil, nil)
	if _, _, ok := service.CheckCostLimitWithLease(context.Background(), GetCostLeaseParams{
		Entity: EntityUser, EntityID: 5, Window: LeaseWindow5h, LimitAmount: 10,
	}); ok {
		t.Error("设置不可读时不应给出判定")
	}
}

// TestLeaseServiceDisabledWithoutRedisOrSettings 钉住「缺一即未接线」：没有 Redis 就没有租约，
// 没有设置源就不知道该切多少。
func TestLeaseServiceDisabledWithoutRedisOrSettings(t *testing.T) {
	if NewLeaseService(nil, &leaseSettingsStub{}, &fakeLedger{}, time.UTC, nil, nil) != nil {
		t.Error("无 Redis 时不应组装出租约服务")
	}
	if NewLeaseService(deadRedisClient(t), nil, &fakeLedger{}, time.UTC, nil, nil) != nil {
		t.Error("无设置源时不应组装出租约服务")
	}
}

// TestLeaseCostLimitFallsBackToLedgerPath 覆盖判定路径的回退：租约不可用时不得放行，
// 必须退回窗口/账本路径继续判（比 Node 的 fail-open 更严，是本次移植的有意差异）。
func TestLeaseCostLimitFallsBackToLedgerPath(t *testing.T) {
	ledger := &fakeLedger{
		totals: map[string]float64{"key/sk-fb": 0},
		ranges: map[string]float64{"key/sk-fb": 9},
	}
	service, err := New(Config{
		Quotas: &fakeQuotaSource{key: KeyQuota{
			KeyID:            1,
			KeyHash:          "sk-fb",
			Limit5hUSD:       floatPtr(1),
			Limit5hResetMode: ResetRolling,
		}},
		Ledger: ledger,
		Redis:  deadRedisClient(t),
		// 设置源报错：租约不可用，判定必须落回账本口径。
		LeaseSettings: &leaseSettingsStub{err: errors.New("settings down")},
		Location:      time.UTC,
	})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}
	block, err := service.Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil {
		t.Fatal("租约不可用时应回退账本路径并拦截")
	}
	if len(ledger.calls) == 0 {
		t.Error("回退路径应查账本")
	}
}

// TestCachedQuotaLeaseSettings 覆盖设置面缓存：TTL 内只读一次，失败不缓存，零值按缺省补齐。
func TestCachedQuotaLeaseSettings(t *testing.T) {
	inner := &leaseSettingsStub{settings: QuotaLeaseSettings{}}
	cached := NewCachedQuotaLeaseSettings(inner, 30*time.Second)
	now := fixedNow
	cached.now = func() time.Time { return now }

	first, err := cached.QuotaLeaseSettings(context.Background())
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	// 零值（含库里的 NULL）一律按 Node 缺省补齐：百分比为 0 会让每份切片恒为 0。
	if first.RefreshIntervalSeconds != DefaultQuotaLeaseRefreshSeconds || first.Percent5h != DefaultQuotaLeasePercent {
		t.Errorf("缺省值补齐不符: %+v", first)
	}
	if inner.calls != 1 {
		t.Fatalf("首次应读一次，实际 %d", inner.calls)
	}
	if _, err := cached.QuotaLeaseSettings(context.Background()); err != nil {
		t.Fatalf("二次读取失败: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("TTL 内应命中缓存，实际读 %d 次", inner.calls)
	}
	now = now.Add(31 * time.Second)
	if _, err := cached.QuotaLeaseSettings(context.Background()); err != nil {
		t.Fatalf("过期后读取失败: %v", err)
	}
	if inner.calls != 2 {
		t.Errorf("过期后应重读，实际 %d 次", inner.calls)
	}

	failing := &leaseSettingsStub{err: errors.New("db down")}
	cachedFailing := NewCachedQuotaLeaseSettings(failing, 30*time.Second)
	cachedFailing.now = func() time.Time { return now }
	for index := 0; index < 2; index++ {
		if _, err := cachedFailing.QuotaLeaseSettings(context.Background()); err == nil {
			t.Fatal("读取失败应向上报告")
		}
	}
	if failing.calls != 2 {
		t.Errorf("失败不应被缓存，实际调用 %d 次", failing.calls)
	}
}

func TestParseLeaseSettlements(t *testing.T) {
	targets := []leaseSettlementTarget{
		{entity: EntityKey, id: 1, window: LeaseWindow5h, resetMode: ResetRolling},
		{entity: EntityKey, id: 1, window: LeaseWindowDaily, resetMode: ResetFixed},
		{entity: EntityUser, id: 2, window: LeaseWindow5h, resetMode: ResetRolling},
	}
	settlements, err := parseLeaseSettlements(`[[1,0.4],[-1,0],[0,-1]]`, targets)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	wants := []LeaseBudgetSettlementStatus{LeaseSettlementDecremented, LeaseSettlementInsufficient, LeaseSettlementMissing}
	for index, want := range wants {
		if settlements[index].Status != want {
			t.Errorf("第 %d 项状态 = %s，期望 %s", index, settlements[index].Status, want)
		}
	}
	if settlements[0].NewRemaining != 0.4 || settlements[1].NewRemaining != 0 {
		t.Errorf("剩余额度解析不符: %+v", settlements)
	}
	// 条数不符即非法（Lua 回填与 KEYS 同位同长，错位会让结算写到别的窗口上）。
	if _, err := parseLeaseSettlements(`[[1,0.4]]`, targets); err == nil {
		t.Error("条数不符应报错")
	}
}

// TestIntegrationLeaseCacheReuse 用真 Redis 钉住「命中缓存就不回表」这条设计目的（租约存在的理由），
// 以及三道「事实已变」强制刷新判据。
func TestIntegrationLeaseCacheReuse(t *testing.T) {
	client := integrationClient(t)
	settings := &leaseSettingsStub{settings: QuotaLeaseSettings{RefreshIntervalSeconds: 10, Percent5h: 0.05}}
	ledger := &fakeLedger{ranges: map[string]float64{"user/5": 1}}
	service := NewLeaseService(client, settings, ledger, time.UTC, time.Now, nil)
	ctx := context.Background()
	key := BuildLeaseKey(EntityUser, 5, LeaseWindow5h, ResetRolling)
	cleanupKeys(t, client, key)

	params := GetCostLeaseParams{
		Entity: EntityUser, EntityID: 5, Window: LeaseWindow5h, LimitAmount: 10,
		ResetTime: "04:00", ResetMode: ResetRolling,
	}
	first, ok := service.GetCostLease(ctx, params)
	if !ok {
		t.Fatal("首次取租约应成功")
	}
	if first.RemainingBudget <= 0 {
		t.Fatalf("首次应切出预算: %+v", first)
	}
	if len(ledger.calls) != 1 {
		t.Fatalf("首次应回表一次，实际 %d", len(ledger.calls))
	}
	// 命中缓存：不再回表，且拿到同一份余额。
	second, ok := service.GetCostLease(ctx, params)
	if !ok || len(ledger.calls) != 1 {
		t.Fatalf("命中缓存不应再回表：ok=%v calls=%d", ok, len(ledger.calls))
	}
	if second.RemainingBudget != first.RemainingBudget {
		t.Errorf("复用余额不符: %+v vs %+v", second, first)
	}

	// 限额被改 → 强制刷新。
	params.LimitAmount = 20
	if _, ok := service.GetCostLease(ctx, params); !ok || len(ledger.calls) != 2 {
		t.Fatalf("限额变更应强制刷新：ok=%v calls=%d", ok, len(ledger.calls))
	}

	// costResetAt 变更 → 强制刷新。
	resetAt := time.Now()
	params.CostResetAt = &resetAt
	if _, ok := service.GetCostLease(ctx, params); !ok || len(ledger.calls) != 3 {
		t.Fatalf("costResetAt 变更应强制刷新：ok=%v calls=%d", ok, len(ledger.calls))
	}

	// 快照过期（把 snapshotAtMs 改到久远）→ 强制刷新。
	stored, valid := DeserializeLease(client.Raw().Get(ctx, key).Val())
	if !valid {
		t.Fatal("缓存的租约应可解析")
	}
	stored.SnapshotAtMS = time.Now().Add(-time.Hour).UnixMilli()
	payload, err := SerializeLease(stored)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if err := client.Raw().Set(ctx, key, payload, 0).Err(); err != nil {
		t.Fatalf("改写缓存失败: %v", err)
	}
	if _, ok := service.GetCostLease(ctx, params); !ok || len(ledger.calls) != 4 {
		t.Fatalf("过期租约应强制刷新：ok=%v calls=%d", ok, len(ledger.calls))
	}
}

// TestIntegrationLeaseLuaScripts 用真 Redis 钉住两段 Lua 的语义：扣减的三种返回值、
// 结算的幂等标记与「不足则清零」的行为。
func TestIntegrationLeaseLuaScripts(t *testing.T) {
	client := integrationClient(t)
	settings := &leaseSettingsStub{settings: QuotaLeaseSettings{RefreshIntervalSeconds: 10, Percent5h: 0.05}}
	service := NewLeaseService(client, settings, &fakeLedger{}, time.UTC, time.Now, nil)
	if service == nil {
		t.Fatal("租约服务应可组装")
	}
	ctx := context.Background()
	raw := client.Raw()

	// --- 扣减：足额、不足、缺键 ---
	key := "lease:test:1:5h:rolling"
	cleanupKeys(t, client, key)
	payload, err := SerializeLease(BudgetLease{
		EntityType: "test", EntityID: 1, Window: "5h", ResetMode: "rolling", ResetTime: "04:00",
		SnapshotAtMS: time.Now().UnixMilli(), CurrentUsage: 0, LimitAmount: 10, RemainingBudget: 0.5, TTLSeconds: 60,
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if err := raw.SetEx(ctx, key, payload, time.Minute).Err(); err != nil {
		t.Fatalf("写入租约失败: %v", err)
	}

	params := GetCostLeaseParams{Entity: "test", EntityID: 1, Window: LeaseWindow5h, ResetMode: ResetRolling}
	// 注：Lua 数值回包在 RESP2 下取整（脚本体里存的是 0.3，回包是 0），Node 侧同款，
	// 故断「存储里的余额」才是有意义的判据。
	result := service.DecrementLeaseBudget(ctx, params, 0.2)
	if !result.Success {
		t.Errorf("足额扣减应判成功: %+v", result)
	}
	stored, ok := DeserializeLease(raw.Get(ctx, key).Val())
	if !ok || stored.RemainingBudget < 0.29 || stored.RemainingBudget > 0.31 {
		t.Errorf("扣减后的余额不符: %+v ok=%v", stored, ok)
	}
	// 不足：判失败且**不动**已存余额（清零是结算脚本的职责，扣减脚本只报不足）。
	result = service.DecrementLeaseBudget(ctx, params, 0.9)
	if result.Success {
		t.Errorf("不足应判失败: %+v", result)
	}
	stored, ok = DeserializeLease(raw.Get(ctx, key).Val())
	if !ok || stored.RemainingBudget < 0.29 || stored.RemainingBudget > 0.31 {
		t.Errorf("不足时不应改写余额: %+v ok=%v", stored, ok)
	}
	result = service.DecrementLeaseBudget(ctx, GetCostLeaseParams{
		Entity: "test", EntityID: 1, Window: LeaseWindowDaily, ResetMode: ResetFixed,
	}, 0.1)
	if result.Success || result.NewRemaining != -1 {
		t.Errorf("缺键应返回 newRemaining=-1: %+v", result)
	}

	// --- 结算：12 窗口一次结算 + 幂等重放 ---
	settleKeys := make([]string, 0, 12)
	for _, entity := range leaseSettlementEntityOrder {
		for _, window := range LeaseWindows {
			settleKeys = append(settleKeys, BuildLeaseKey(entity, 1, window, ResetRolling))
		}
	}
	cleanupKeys(t, client, append(settleKeys, leaseSettlementMarkerPrefix+"req-1")...)
	// 只给两份租约：一份够扣减、一份不足；其余应为 missing。
	for index, value := range map[int]float64{0: 0.5, 1: 0.01} {
		body, err := SerializeLease(BudgetLease{
			EntityType: "key", EntityID: 1, Window: string(LeaseWindows[index]), ResetMode: "rolling",
			ResetTime: "04:00", SnapshotAtMS: time.Now().UnixMilli(), CurrentUsage: 0, LimitAmount: 10,
			RemainingBudget: value, TTLSeconds: 60,
		})
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		if err := raw.SetEx(ctx, settleKeys[index], body, time.Minute).Err(); err != nil {
			t.Fatalf("写入租约失败: %v", err)
		}
	}

	input := SettleLeaseInput{
		RequestID: "req-1", Cost: 0.1,
		Key:      LeaseSettlementEntity{ID: 1, Reset5h: ResetRolling, ResetDaily: ResetRolling},
		User:     LeaseSettlementEntity{ID: 1, Reset5h: ResetRolling, ResetDaily: ResetRolling},
		Provider: LeaseSettlementEntity{ID: 1, Reset5h: ResetRolling, ResetDaily: ResetRolling},
	}
	settled := service.SettleLeaseBudgets(ctx, input)
	if settled.Status != "settled" || settled.FailOpen {
		t.Fatalf("结算状态不符: %+v", settled)
	}
	if len(settled.Settlements) != 12 {
		t.Fatalf("应结算 12 个窗口，实际 %d", len(settled.Settlements))
	}
	if settled.Settlements[0].Status != LeaseSettlementDecremented || settled.Settlements[1].Status != LeaseSettlementInsufficient {
		t.Errorf("前两个窗口状态不符: %+v", settled.Settlements[:2])
	}
	if settled.Settlements[2].Status != LeaseSettlementMissing {
		t.Errorf("无租约的窗口应判 missing: %+v", settled.Settlements[2])
	}
	// 幂等：同一 requestId 重放直接返回上一次结果，不重复扣。
	replayed := service.SettleLeaseBudgets(ctx, input)
	if replayed.Status != "duplicate" {
		t.Errorf("重放应判 duplicate，实际 %s", replayed.Status)
	}
	if len(replayed.Settlements) != 12 || replayed.Settlements[0].NewRemaining != settled.Settlements[0].NewRemaining {
		t.Errorf("重放结果应与首次一致: %+v", replayed.Settlements[:1])
	}
	// 非法入参一律 fail-open（成本为 0 不该产生扣减）。
	if invalid := service.SettleLeaseBudgets(ctx, SettleLeaseInput{RequestID: "req-2", Cost: 0}); !invalid.FailOpen {
		t.Errorf("成本非正应 fail-open: %+v", invalid)
	}
}
