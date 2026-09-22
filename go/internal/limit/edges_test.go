package limit

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/redis/go-redis/v9"
)

func TestServiceThrottleAdapter(t *testing.T) {
	now := fixedNow
	service, err := New(Config{
		Quotas: &fakeQuotaSource{},
		Abuse:  AuthAbuseConfig{MaxAttemptsPerIP: 2, MaxAttemptsPerKey: 2, WindowSeconds: 300, LockoutSeconds: 600},
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}
	ctx := context.Background()

	decision, err := service.Throttle(ctx, "9.9.9.9", "sk-x")
	if err != nil || !decision.Allowed {
		t.Fatalf("首次节流检查应放行: %+v err=%v", decision, err)
	}

	service.RecordAuthFailure(ctx, "9.9.9.9", "sk-x")
	service.RecordAuthFailure(ctx, "9.9.9.9", "sk-x")
	decision, err = service.Throttle(ctx, "9.9.9.9", "sk-x")
	if err != nil {
		t.Fatalf("节流检查出错: %v", err)
	}
	if decision.Allowed || decision.RetryAfterSeconds == nil {
		t.Fatalf("触顶后应拒绝并给出 Retry-After: %+v", decision)
	}

	service.RecordAuthSuccess(ctx, "9.9.9.9", "sk-x")
	if decision, _ := service.Throttle(ctx, "9.9.9.9", "sk-x"); !decision.Allowed {
		t.Errorf("成功后应清零: %+v", decision)
	}
}

func TestServiceTrackCostAndSessionsAccessor(t *testing.T) {
	service, err := New(Config{Quotas: &fakeQuotaSource{}})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}
	if service.Sessions() == nil {
		t.Fatal("Sessions 访问器不应返回 nil")
	}
	// 未接 Redis 时记账是静默空操作，不得 panic 也不得报错。
	service.TrackCost(context.Background(), TrackCostInput{KeyID: 1, ProviderID: 1, Cost: 1})
}

func TestSessionIDFallsBackToGeneratedNodeShape(t *testing.T) {
	service, err := New(Config{Quotas: &fakeQuotaSource{}, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}
	req, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatal(err)
	}

	// SessionID 缝隙缺失时现场生成，形制与 SessionManager.generateSessionId 一致。
	generated, err := service.sessionID(req)
	if err != nil {
		t.Fatalf("生成会话 id 失败: %v", err)
	}
	if !regexp.MustCompile(`^sess_[0-9a-z]+_[0-9a-f]{12}$`).MatchString(generated) {
		t.Errorf("会话 id 形制不符: %q", generated)
	}

	// 返回空串的缝隙同样视为缺失。
	service.cfg.SessionID = func(*pctx.Context) (string, error) { return "", nil }
	if got, _ := service.sessionID(req); got == "" {
		t.Error("缝隙返回空串时应现场生成")
	}

	// 缝隙报错时把错误交给调用方（并发维度据此退化为不判定）。
	wantErr := &testError{"绑定未接线"}
	service.cfg.SessionID = func(*pctx.Context) (string, error) { return "", wantErr }
	if _, err := service.sessionID(req); err == nil {
		t.Error("缝隙报错时应向上传递")
	}
}

func TestCostWindowsWithoutRedisAreNoops(t *testing.T) {
	windows := NewCostWindows(nil, nil)
	ctx := context.Background()

	if windows.Ready() {
		t.Fatal("未接 Redis 时不应报告 Ready")
	}
	if value, exists, err := windows.RollingCost(ctx, EntityKey, 1, Period5h, fixedNow); value != 0 || exists || err != nil {
		t.Errorf("无 Redis 读滚动窗口应为零值: %v %v %v", value, exists, err)
	}
	if value, exists, err := windows.FixedCost(ctx, "key:1:cost_weekly"); value != 0 || exists || err != nil {
		t.Errorf("无 Redis 读固定窗口应为零值: %v %v %v", value, exists, err)
	}
	if state, err := windows.Fixed5hWindowState(ctx, EntityKey, 1, fixedNow); state.Exists || state.Current != 0 || err != nil {
		t.Errorf("无 Redis 读 5h 固定窗口应为零值: %+v %v", state, err)
	}
	if err := windows.WarmFixed(ctx, "key:1:cost_weekly", 1, 60); err != nil {
		t.Errorf("无 Redis 预热应是空操作: %v", err)
	}
	if err := windows.TrackCost(ctx, TrackCostInput{KeyID: 1, ProviderID: 1, Cost: 1}, time.UTC, fixedNow); err != nil {
		t.Errorf("无 Redis 记账应是空操作: %v", err)
	}
	// 周期非法时读滚动窗口要给出可判别错误，而不是静默返回 0。
	if _, _, err := windows.RollingCost(ctx, EntityKey, 1, PeriodWeekly, fixedNow); err == nil {
		t.Error("非滚动周期应报错")
	}
}

func TestSessionTrackerWithoutRedisFailsOpen(t *testing.T) {
	tracker := NewSessionTracker(nil, 0, nil)
	ctx := context.Background()

	if tracker.Ready() {
		t.Fatal("未接 Redis 时不应报告 Ready")
	}
	keyUser, err := tracker.CheckAndTrackKeyUserSession(ctx, 1, 2, "sess", 1, 1)
	if err != nil || !keyUser.Allowed {
		t.Errorf("无 Redis 时 Key/User 并发应 Fail Open: %+v err=%v", keyUser, err)
	}
	provider, err := tracker.CheckAndTrackProviderAttempt(ctx, 1, "sess\x1fatk", 1)
	if err != nil || !provider.Allowed {
		t.Errorf("无 Redis 时供应商并发应 Fail Open: %+v err=%v", provider, err)
	}
	// 无上限时不追踪也不访问 Redis。
	if unlimited, err := tracker.CheckAndTrackKeyUserSession(ctx, 1, 2, "sess", 0, 0); err != nil || !unlimited.Allowed || unlimited.TrackedKey {
		t.Errorf("无上限时应直接放行: %+v err=%v", unlimited, err)
	}
	if _, _, err := tracker.ReleaseProviderAttempt(ctx, 0, ""); err != nil {
		t.Errorf("非法参数释放应是空操作: %v", err)
	}
	if _, _, err := tracker.ForceTerminateProviderAttempts(ctx, 1, ""); err != nil {
		t.Errorf("空会话 id 终止应是空操作: %v", err)
	}
	if _, err := tracker.ForceTerminateKeyUserSession(ctx, 0, 2, "sess"); err != nil {
		t.Errorf("非法 id 终止应是空操作: %v", err)
	}
	// 有上限但无 Redis：Fail Open 且不记账。
	if allowed, err := tracker.CheckAndTrackKeyUserSession(ctx, 1, 2, "sess", 1, 0); err != nil || !allowed.Allowed || allowed.TrackedKey {
		t.Errorf("有上限无 Redis 时应放行且不记账: %+v err=%v", allowed, err)
	}
}

func TestScriptReplyParsing(t *testing.T) {
	values, err := toInt64Slice([]any{int64(1), "0", float64(3), []byte("2")})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := []int64{1, 0, 3, 2}
	for index := range want {
		if values[index] != want[index] {
			t.Errorf("第 %d 项 = %d，期望 %d", index, values[index], want[index])
		}
	}
	if _, err := toInt64Slice(nil); err == nil {
		t.Error("nil 回复应报错")
	}
	if _, err := toInt64Slice("不是数组"); err == nil {
		t.Error("非数组回复应报错")
	}
	if _, err := toInt64Slice([]any{struct{}{}}); err == nil {
		t.Error("元素类型不支持时应报错")
	}
	if _, err := toInt64("不是数字"); err == nil {
		t.Error("非数字字符串应报错")
	}
}

func TestParseCostTextShapes(t *testing.T) {
	cases := []struct {
		input any
		want  float64
	}{
		{nil, 0},
		{float64(1.5), 1.5},
		{int64(2), 2},
		{"3.25", 3.25},
		{[]byte("4.5"), 4.5},
		{"不是数字", 0},
		{true, 0},
	}
	for _, tc := range cases {
		if got := ParseCostText(tc.input); got != tc.want {
			t.Errorf("ParseCostText(%v) = %v，期望 %v", tc.input, got, tc.want)
		}
	}
}

func TestRetryAfterAndFormatting(t *testing.T) {
	now := fixedNow
	if got := retryAfterSeconds(nil, now); got != nil {
		t.Errorf("无重置时刻不应有 Retry-After: %v", got)
	}
	later := now.Add(90*time.Second + 500*time.Millisecond)
	if got := retryAfterSeconds(&later, now); got == nil || *got != 91 {
		t.Errorf("Retry-After = %v，期望 91（向上取整）", got)
	}
	past := now.Add(-time.Hour)
	if got := retryAfterSeconds(&past, now); got == nil || *got != 0 {
		t.Errorf("已过重置时刻应钳到 0: %v", got)
	}
	if got := formatUSD(1.5); got != "1.5000" {
		t.Errorf("toFixed(4) 形制不符: %q", got)
	}
	if got := isoMillis(now); got != "2026-03-01T02:00:00.000Z" {
		t.Errorf("ISO 形制不符: %q", got)
	}
}

func TestIsRollingWindowAndFixedWindowKey(t *testing.T) {
	cases := []struct {
		dimension costDimension
		want      bool
	}{
		{costDimension{period: Period5h, resetMode: ResetRolling}, true},
		{costDimension{period: Period5h, resetMode: ResetFixed}, false},
		{costDimension{period: PeriodDaily, resetMode: ResetRolling}, true},
		{costDimension{period: PeriodDaily, resetMode: ResetFixed}, false},
		{costDimension{period: PeriodWeekly}, false},
		{costDimension{period: PeriodMonthly}, false},
	}
	for _, tc := range cases {
		if got := isRollingWindow(tc.dimension); got != tc.want {
			t.Errorf("%s/%s: 得到 %v，期望 %v", tc.dimension.period, tc.dimension.resetMode, got, tc.want)
		}
	}

	if got := FixedWindowKey(EntityKey, 1, PeriodDaily, "18:00"); got != "key:1:cost_daily_1800" {
		t.Errorf("daily 固定窗口键不符: %q", got)
	}
	if got := FixedWindowKey(EntityUser, 2, PeriodMonthly, "18:00"); got != "user:2:cost_monthly" {
		t.Errorf("月窗口键不应带重置时刻后缀: %q", got)
	}
}

// TestIntegrationServiceTrackCostWritesWindows 断言服务层记账写入的窗口可被读回。
func TestIntegrationServiceTrackCostWritesWindows(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	keyID := uniqueID(t) + 31
	cleanupKeys(t, client, trackedCostKeys(keyID, keyID+1, 0, "00:00")...)

	service, err := New(Config{
		Quotas:   &fakeQuotaSource{},
		Redis:    client,
		Location: time.UTC,
	})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}
	now := time.Now().UTC()
	service.TrackCost(ctx, TrackCostInput{
		KeyID:          keyID,
		ProviderID:     keyID + 1,
		Cost:           1.5,
		CreatedAt:      now,
		Key5hResetMode: ResetRolling,
		KeyResetMode:   ResetRolling,
	})

	current, exists, err := NewCostWindows(client, nil).RollingCost(ctx, EntityKey, keyID, Period5h, time.Now().UTC())
	if err != nil || !exists || current != 1.5 {
		t.Fatalf("服务层记账未落库: value=%v exists=%v err=%v", current, exists, err)
	}
}

// TestIntegrationFixed5hWindowBlockCarriesResetMetadata 覆盖 5h 固定窗口的拦截分支
// （含由键 TTL 反推的重置时刻）。
func TestIntegrationFixed5hWindowBlockCarriesResetMetadata(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	keyID := uniqueID(t) + 41
	cleanupKeys(t, client, trackedCostKeys(keyID, keyID+1, 0, "00:00")...)

	now := time.Now().UTC()
	windows := NewCostWindows(client, nil)
	if err := windows.TrackCost(ctx, TrackCostInput{
		KeyID:               keyID,
		ProviderID:          keyID + 1,
		Cost:                3,
		CreatedAt:           now,
		Key5hResetMode:      ResetFixed,
		KeyResetMode:        ResetFixed,
		Provider5hResetMode: ResetFixed,
		ProviderResetMode:   ResetFixed,
	}, time.UTC, now); err != nil {
		t.Fatalf("记账失败: %v", err)
	}

	service, err := New(Config{
		Quotas: &fakeQuotaSource{key: KeyQuota{
			KeyID:            keyID,
			KeyHash:          "sk-fixed5h",
			Limit5hUSD:       floatPtr(1),
			Limit5hResetMode: ResetFixed,
		}},
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
	if block == nil || block.Status != 402 {
		t.Fatalf("5h 固定窗口超限应拦截: %+v", block)
	}
	if !strings.Contains(block.BlockedReason, `"limit_type":"usd_5h"`) {
		t.Errorf("limit_type 不符: %q", block.BlockedReason)
	}
	if !strings.Contains(block.Message, "5小时消费超限") {
		t.Errorf("固定窗口应使用 RATE_LIMIT_5H_EXCEEDED 文案: %q", block.Message)
	}
	if block.RetryAfterSeconds == nil || *block.RetryAfterSeconds <= 0 {
		t.Errorf("固定窗口应由 TTL 反推 Retry-After: %v", block.RetryAfterSeconds)
	}
}

// TestUnreachableRedisFallsBackToLedger 用不可达的 Redis 地址覆盖「Redis 故障回退账本」分支。
func TestUnreachableRedisFallsBackToLedger(t *testing.T) {
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("装载脚本清单失败: %v", err)
	}
	deadPort := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:6399",
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = deadPort.Close() })
	client, err := ratelimit.New(deadPort, registry)
	if err != nil {
		t.Fatalf("组装调用层失败: %v", err)
	}

	ledger := &fakeLedger{
		totals: map[string]float64{"key/sk-dead": 5},
		ranges: map[string]float64{"key/sk-dead": 9},
	}
	// 5h 放在限内、周放在限外：两个维度都会走到账本回退，最终拦截来自周维度。
	service, err := New(Config{
		Quotas: &fakeQuotaSource{key: KeyQuota{
			KeyID:            1,
			KeyHash:          "sk-dead",
			Limit5hUSD:       floatPtr(20),
			LimitWeeklyUSD:   floatPtr(1),
			Limit5hResetMode: ResetRolling,
		}},
		Ledger:   ledger,
		Redis:    client,
		Location: time.UTC,
	})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}

	block, err := service.Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil {
		t.Fatal("Redis 不可达时应回退账本并拦截")
	}
	if len(ledger.calls) != 2 {
		t.Errorf("5h 与周两个维度都应触发账本回退，实际 %d 次", len(ledger.calls))
	}
	if !strings.Contains(block.BlockedReason, `"limit_type":"usd_weekly"`) {
		t.Errorf("周维度应先行拦截: %q", block.BlockedReason)
	}
}

// TestCheckFailsOpenWhenLedgerMissingAndRedisDead 覆盖判定无法完成时的 Fail Open 出口。
func TestCheckFailsOpenWhenLedgerMissingAndRedisDead(t *testing.T) {
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("装载脚本清单失败: %v", err)
	}
	deadPort := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6399", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = deadPort.Close() })
	client, err := ratelimit.New(deadPort, registry)
	if err != nil {
		t.Fatalf("组装调用层失败: %v", err)
	}

	service, err := New(Config{
		Quotas: &fakeQuotaSource{key: KeyQuota{
			KeyID:      1,
			KeyHash:    "sk-fo",
			Limit5hUSD: floatPtr(1),
		}},
		Redis:    client,
		Location: time.UTC,
	})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}
	block, err := service.Check(context.Background(), requestWithAuth(t))
	if err != nil || block != nil {
		t.Fatalf("判定无法完成时应放行: block=%+v err=%v", block, err)
	}
}
