package limit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// fixedNow 是测试统一的时间基准（UTC）。
var fixedNow = time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

// fakeQuotaSource 是可注入的限额快照。
type fakeQuotaSource struct {
	key       KeyQuota
	user      UserQuota
	keyErr    error
	userErr   error
	keyCalls  int
	userCalls int
}

func (f *fakeQuotaSource) KeyQuota(_ context.Context, _ int64) (KeyQuota, error) {
	f.keyCalls++
	return f.key, f.keyErr
}

func (f *fakeQuotaSource) UserQuota(_ context.Context, _ int64) (UserQuota, error) {
	f.userCalls++
	return f.user, f.userErr
}

// ledgerCall 记录一次账本查询，用于断言「Key 维度按密钥字符串聚合」等口径。
type ledgerCall struct {
	entityType store.LedgerEntityType
	entityID   any
	start      time.Time
	end        time.Time
}

// fakeLedger 是账本回退的桩：按实体标识给出合计值。
type fakeLedger struct {
	totals   map[string]float64
	ranges   map[string]float64
	calls    []ledgerCall
	totalErr error
	rangeErr error
}

func (f *fakeLedger) SumLedgerTotalCost(_ context.Context, entityType store.LedgerEntityType, entityID any, _ *time.Time) (string, error) {
	if f.totalErr != nil {
		return "", f.totalErr
	}
	return FormatCostText(f.totals[ledgerKey(entityType, entityID)]), nil
}

func (f *fakeLedger) SumLedgerCostInTimeRange(_ context.Context, entityType store.LedgerEntityType, entityID any, start time.Time, end time.Time) (string, error) {
	f.calls = append(f.calls, ledgerCall{entityType: entityType, entityID: entityID, start: start, end: end})
	if f.rangeErr != nil {
		return "", f.rangeErr
	}
	return FormatCostText(f.ranges[ledgerKey(entityType, entityID)]), nil
}

func ledgerKey(entityType store.LedgerEntityType, entityID any) string {
	return fmt.Sprintf("%s/%v", entityType, entityID)
}

// newTestService 组装一个不接 Redis 的服务（全部走账本回退），便于纯单测。
func newTestService(t *testing.T, quotas QuotaSource, ledger LedgerReader) *Service {
	t.Helper()
	service, err := New(Config{
		Quotas:     quotas,
		Ledger:     ledger,
		Location:   time.UTC,
		Now:        func() time.Time { return fixedNow },
		SessionID:  func(*pctx.Context) (string, error) { return "sess_test", nil },
		SessionTTL: 300 * time.Second,
	})
	if err != nil {
		t.Fatalf("组装限流服务失败: %v", err)
	}
	return service
}

func requestWithAuth(t *testing.T) *pctx.Context {
	t.Helper()
	req, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	req.SetAuth(pctx.AuthState{KeyID: 1, UserID: 2, APIKey: "sk-test"})
	return req
}

func floatPtr(value float64) *float64 { return &value }

func TestCheckSkipsWithoutAuthIdentity(t *testing.T) {
	service := newTestService(t, &fakeQuotaSource{}, &fakeLedger{})
	req, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatal(err)
	}
	block, err := service.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block != nil {
		t.Fatalf("无身份时不应拦截: %+v", block)
	}
	if service.cfg.Quotas.(*fakeQuotaSource).keyCalls != 0 {
		t.Error("无身份时不应读取限额快照")
	}
}

func TestCheckKeyTotalLimitBlocksWith402(t *testing.T) {
	ledger := &fakeLedger{totals: map[string]float64{"key/sk-total": 10.5}}
	quotas := &fakeQuotaSource{key: KeyQuota{KeyID: 1, KeyHash: "sk-total", LimitTotalUSD: floatPtr(5)}}
	service := newTestService(t, quotas, ledger)

	block, err := service.Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil {
		t.Fatal("总额度触发时应拦截")
	}
	if block.Status != 402 {
		t.Errorf("状态码 = %d，期望 402（消费类限额）", block.Status)
	}
	if block.ErrorType != "rate_limit_error" {
		t.Errorf("错误类型 = %q", block.ErrorType)
	}
	// 默认语种 zh-CN，且 toFixed(4) 形制（模板里的 ${current} 渲染为 $ + 数值）。
	if !strings.Contains(block.Message, "$10.5000 / $5.0000 USD") {
		t.Errorf("文案未按 toFixed(4) 渲染: %q", block.Message)
	}
	if block.BlockedBy != "" {
		t.Errorf("Node 侧限流不写 blocked_by，得到 %q", block.BlockedBy)
	}
	if !strings.Contains(block.BlockedReason, `"limit_type":"usd_total"`) {
		t.Errorf("拦截原因元数据不符: %q", block.BlockedReason)
	}
	if block.RetryAfterSeconds == nil {
		t.Error("总额度有「不会到来」的重置时刻，应当带 Retry-After")
	}
}

func TestCheckUserTotalLimitBlocksAfterKeyPasses(t *testing.T) {
	ledger := &fakeLedger{totals: map[string]float64{"key/sk-ok": 1, "user/2": 9}}
	quotas := &fakeQuotaSource{
		key:  KeyQuota{KeyID: 1, KeyHash: "sk-ok", LimitTotalUSD: floatPtr(5)},
		user: UserQuota{UserID: 2, LimitTotalUSD: floatPtr(5)},
	}
	block, err := newTestService(t, quotas, ledger).Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil || block.Status != 402 {
		t.Fatalf("用户总额度应拦截: %+v", block)
	}
	if !strings.Contains(block.Message, "$9.0000 / $5.0000 USD") {
		t.Errorf("用户维度文案不符: %q", block.Message)
	}
}

func TestCheckKeyTotalSkipsWithoutKeyHash(t *testing.T) {
	// 没有密钥字符串就无法在账本里定位：Node 侧记 warn 并跳过执行，而不是误判为 0。
	ledger := &fakeLedger{totals: map[string]float64{"key/": 999}}
	quotas := &fakeQuotaSource{key: KeyQuota{KeyID: 1, LimitTotalUSD: floatPtr(5)}}
	block, err := newTestService(t, quotas, ledger).Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block != nil {
		t.Fatalf("缺密钥字符串时不应按总额度拦截: %+v", block)
	}
}

func TestCheckKeyCostLimitUsesKeyHashAndClipsReset(t *testing.T) {
	resetAt := fixedNow.Add(-time.Hour)
	ledger := &fakeLedger{ranges: map[string]float64{"key/sk-5h": 7}}
	quotas := &fakeQuotaSource{key: KeyQuota{
		KeyID:            1,
		KeyHash:          "sk-5h",
		Limit5hUSD:       floatPtr(5),
		CostResetAt:      &resetAt,
		Limit5hResetMode: ResetRolling,
	}}
	block, err := newTestService(t, quotas, ledger).Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil {
		t.Fatal("5h 超限应拦截")
	}
	if len(ledger.calls) != 1 {
		t.Fatalf("账本查询次数 = %d，期望 1", len(ledger.calls))
	}
	call := ledger.calls[0]
	if call.entityType != store.LedgerEntityKey {
		t.Errorf("账本维度 = %q，期望 key", call.entityType)
	}
	// 账本按密钥字符串聚合，不按 id。
	if call.entityID != "sk-5h" {
		t.Errorf("账本实体标识 = %v，期望密钥字符串 sk-5h", call.entityID)
	}
	// 滚动窗口起点不能早于重置时刻。
	if !call.start.Equal(resetAt) {
		t.Errorf("窗口起点 = %s，期望被重置时刻裁剪为 %s", call.start, resetAt)
	}
	if !call.end.Equal(fixedNow) {
		t.Errorf("窗口终点 = %s，期望 %s", call.end, fixedNow)
	}
	// 滚动窗口没有固定重置时刻，故不带 Retry-After（Node 侧 resetTime 为 null）。
	if block.RetryAfterSeconds != nil {
		t.Errorf("滚动窗口不应带 Retry-After，得到 %v", *block.RetryAfterSeconds)
	}
	if !strings.Contains(block.Message, "5小时滚动窗口消费超限") {
		t.Errorf("滚动窗口文案不符: %q", block.Message)
	}
}

func TestCheckUser5hRollingUsesLaterOfTwoResets(t *testing.T) {
	costResetAt := fixedNow.Add(-4 * time.Hour)
	limit5hResetAt := fixedNow.Add(-30 * time.Minute) // 更晚，应生效
	ledger := &fakeLedger{ranges: map[string]float64{"user/2": 6}}
	quotas := &fakeQuotaSource{user: UserQuota{
		UserID:             2,
		Limit5hUSD:         floatPtr(1),
		CostResetAt:        &costResetAt,
		Limit5hCostResetAt: &limit5hResetAt,
		Limit5hResetMode:   ResetRolling,
	}}
	block, err := newTestService(t, quotas, ledger).Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil {
		t.Fatal("用户 5h 超限应拦截")
	}
	if len(ledger.calls) != 1 {
		t.Fatalf("账本查询次数 = %d", len(ledger.calls))
	}
	if !ledger.calls[0].start.Equal(limit5hResetAt) {
		t.Errorf("窗口起点 = %s，期望 later(costResetAt, limit5hCostResetAt) = %s", ledger.calls[0].start, limit5hResetAt)
	}
	// 5h 滚动窗口的文案与 limit_type 与 Key 维度一致。
	if !strings.Contains(block.Message, "5小时滚动窗口消费超限") {
		t.Errorf("文案不符: %q", block.Message)
	}
	if !strings.Contains(block.BlockedReason, `"limit_type":"usd_5h"`) {
		t.Errorf("limit_type 不符: %q", block.BlockedReason)
	}
}

func TestCheckDailyFixedWindowsUseTheirOwnResetTime(t *testing.T) {
	ledger := &fakeLedger{ranges: map[string]float64{"key/sk-daily": 3}}
	quotas := &fakeQuotaSource{key: KeyQuota{
		KeyID:          1,
		KeyHash:        "sk-daily",
		LimitDailyUSD:  floatPtr(2),
		DailyResetTime: "18:00",
		DailyResetMode: ResetFixed,
	}}
	block, err := newTestService(t, quotas, ledger).Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil || block.Status != 402 {
		t.Fatalf("每日额度应拦截: %+v", block)
	}
	if !strings.Contains(block.BlockedReason, `"limit_type":"daily_quota"`) {
		t.Errorf("limit_type 不符: %q", block.BlockedReason)
	}
	// 02:00 UTC 时下一次 18:00 在 16 小时后。
	reset := ExtractResetTime(t, block.BlockedReason)
	if want := fixedNow.Add(16 * time.Hour); !reset.Equal(want) {
		t.Errorf("重置时刻 = %s，期望 %s", reset, want)
	}
	if block.RetryAfterSeconds == nil || *block.RetryAfterSeconds != int(16*3600) {
		t.Errorf("Retry-After = %v，期望 %d", block.RetryAfterSeconds, int(16*3600))
	}
	if !strings.Contains(block.Message, "每日额度超限") {
		t.Errorf("文案不符: %q", block.Message)
	}
}

func TestCheckKeyPrecedesUserInSameWindow(t *testing.T) {
	// 同一窗口内 Key 先于 User 判定：两者都触顶时应报 Key 的值。
	ledger := &fakeLedger{ranges: map[string]float64{"key/sk-order": 100, "user/2": 200}}
	quotas := &fakeQuotaSource{
		key:  KeyQuota{KeyID: 1, KeyHash: "sk-order", LimitWeeklyUSD: floatPtr(1)},
		user: UserQuota{UserID: 2, LimitWeeklyUSD: floatPtr(1)},
	}
	block, err := newTestService(t, quotas, ledger).Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil {
		t.Fatal("周额度应拦截")
	}
	if !strings.Contains(block.Message, "$100.0000") {
		t.Errorf("应先报 Key 维度用量: %q", block.Message)
	}
	if len(ledger.calls) != 1 {
		t.Errorf("Key 触顶后不应继续查 User 维度，账本查询 %d 次", len(ledger.calls))
	}
}

func TestCheckWeeklyAndMonthlyCodes(t *testing.T) {
	cases := []struct {
		name      string
		quota     KeyQuota
		wantCode  string
		wantRetry bool
	}{
		{
			name:      "周",
			quota:     KeyQuota{KeyID: 1, KeyHash: "sk-w", LimitWeeklyUSD: floatPtr(1)},
			wantCode:  "usd_weekly",
			wantRetry: true,
		},
		{
			name:      "月",
			quota:     KeyQuota{KeyID: 1, KeyHash: "sk-m", LimitMonthlyUSD: floatPtr(1)},
			wantCode:  "usd_monthly",
			wantRetry: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &fakeLedger{ranges: map[string]float64{"key/" + tc.quota.KeyHash: 2}}
			block, err := newTestService(t, &fakeQuotaSource{key: tc.quota}, ledger).Check(context.Background(), requestWithAuth(t))
			if err != nil {
				t.Fatalf("Check 出错: %v", err)
			}
			if block == nil || block.Status != 402 {
				t.Fatalf("应拦截: %+v", block)
			}
			if !strings.Contains(block.BlockedReason, `"limit_type":"`+tc.wantCode+`"`) {
				t.Errorf("limit_type 不符: %q", block.BlockedReason)
			}
			if (block.RetryAfterSeconds != nil) != tc.wantRetry {
				t.Errorf("Retry-After 存在性不符: %v", block.RetryAfterSeconds)
			}
		})
	}
}

func TestCheck5hFixedWindowBlocksWithOwnMessage(t *testing.T) {
	// 5h 固定窗口只读固定窗口键；无 Redis 时计为 0，故用 limit 0 之外的极小值触发不到，
	// 这里直接断言「无 Redis 时不误判」。
	quotas := &fakeQuotaSource{key: KeyQuota{KeyID: 1, KeyHash: "sk-fixed", Limit5hUSD: floatPtr(1), Limit5hResetMode: ResetFixed}}
	block, err := newTestService(t, quotas, &fakeLedger{}).Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block != nil {
		t.Fatalf("无 Redis 时 5h 固定窗口计为 0，不应拦截: %+v", block)
	}
}

func TestCheckFailsOpenOnFailures(t *testing.T) {
	base := func() (*fakeQuotaSource, *fakeLedger) {
		return &fakeQuotaSource{
			key:  KeyQuota{KeyID: 1, KeyHash: "sk-f", LimitTotalUSD: floatPtr(1), Limit5hUSD: floatPtr(1)},
			user: UserQuota{UserID: 2},
		}, &fakeLedger{totals: map[string]float64{"key/sk-f": 99}}
	}

	t.Run("限额快照读取失败", func(t *testing.T) {
		quotas, ledger := base()
		quotas.keyErr = errors.New("快照不可用")
		block, err := newTestService(t, quotas, ledger).Check(context.Background(), requestWithAuth(t))
		if err != nil || block != nil {
			t.Fatalf("快照读取失败应按 Fail Open 放行: block=%+v err=%v", block, err)
		}
	})

	t.Run("账本回退未接线", func(t *testing.T) {
		quotas, _ := base()
		block, err := newTestService(t, quotas, nil).Check(context.Background(), requestWithAuth(t))
		if err != nil || block != nil {
			t.Fatalf("账本未接线时应放行并留痕: block=%+v err=%v", block, err)
		}
	})

	t.Run("账本查询失败", func(t *testing.T) {
		quotas, ledger := base()
		ledger.totalErr = errors.New("账本不可达")
		block, err := newTestService(t, quotas, ledger).Check(context.Background(), requestWithAuth(t))
		if err != nil || block != nil {
			t.Fatalf("账本失败时应放行: block=%+v err=%v", block, err)
		}
	})
}

func TestCheckRequiresQuotaSource(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("缺少 QuotaSource 应在装配期报错")
	}
}

func TestNormalizeConcurrentLimitsFallsBackToUser(t *testing.T) {
	cases := []struct {
		name             string
		keyLimit         int
		userLimit        int
		wantEffectiveKey int
		wantUser         int
	}{
		{"Key 优先", 3, 5, 3, 5},
		{"Key 未设置时继承 User", 0, 5, 5, 5},
		{"Key 为负视为未设置", -1, 5, 5, 5},
		{"都未设置即无限制", 0, 0, 0, 0},
		{"User 为负视为无限制", 2, -3, 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, user := resolveConcurrentLimits(tc.keyLimit, tc.userLimit)
			if key != tc.wantEffectiveKey || user != tc.wantUser {
				t.Errorf("得到 (%d,%d)，期望 (%d,%d)", key, user, tc.wantEffectiveKey, tc.wantUser)
			}
		})
	}
}

func TestDefaultModeFillsNodeDefaults(t *testing.T) {
	if got := defaultMode("", ResetRolling); got != ResetRolling {
		t.Errorf("5h 缺省应为 rolling，得到 %q", got)
	}
	if got := defaultMode("", ResetFixed); got != ResetFixed {
		t.Errorf("daily 缺省应为 fixed，得到 %q", got)
	}
	if got := defaultMode(ResetFixed, ResetRolling); got != ResetFixed {
		t.Errorf("显式模式不应被覆盖，得到 %q", got)
	}
}

func TestServiceImplementsGuardRateLimiter(t *testing.T) {
	var _ guard.RateLimiter = newTestService(t, &fakeQuotaSource{}, &fakeLedger{})
}

// ExtractResetTime 从拦截原因元数据里取出 reset_time。
func ExtractResetTime(t *testing.T, blockedReason string) time.Time {
	t.Helper()
	const marker = `"reset_time":"`
	index := strings.Index(blockedReason, marker)
	if index < 0 {
		t.Fatalf("拦截原因里没有 reset_time: %q", blockedReason)
	}
	rest := blockedReason[index+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("reset_time 未闭合: %q", blockedReason)
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", rest[:end])
	if err != nil {
		t.Fatalf("解析 reset_time %q 失败: %v", rest[:end], err)
	}
	return parsed
}
