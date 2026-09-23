package route_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
	"github.com/redis/go-redis/v9"
)

// 本文件用真 Binder（真 Redis + 真 Lua）、真选路器与真终态结算器钉住
// 「读绑定行失败」仍留痕 transient、备用成功后绑定跟随 winner。
//
// 为何是**外部测试包**（route_test）而不是包内：本文件要 import session 与 terminal，而
// session → guard → terminal（guard/adapters.go），包内测试会撞 import cycle。
// 外部测试包不受该限制。这也解释了为何同型用例在 dataplane 包里（它在本包之上）。
//
// 真库门控：未设 CCH_TEST_REDIS_URL 时跳过（CI 即跳过）。故 CI 门禁的半边依据在包内
// （含哨兵翻译的纯单测），本文件是补强而非唯一依据。

const lookupFailureBindingTTLSeconds = 3600

// lookupFailureWriter 是最小终态写入面：只回答「赢下该行」，不落库、不查价。
type lookupFailureWriter struct{}

func (lookupFailureWriter) CreateMessageRequest(
	context.Context,
	store.CreateMessageRequestData,
) (store.MessageRequest, error) {
	return store.MessageRequest{}, nil
}

func (lookupFailureWriter) UpdateDetailsIfUnfinalized(context.Context, int64, store.DetailsPatch) (bool, error) {
	return true, nil
}

func (lookupFailureWriter) UpdateWinnerCost(context.Context, int64, string, []byte) error { return nil }

func (lookupFailureWriter) FindModelPrice(context.Context, string) (*store.ModelPrice, error) {
	return nil, errors.New("lookupfailure: 本用例不查价")
}

// lookupFailureSource 是「按 id 直读会失败」的选路数据源：列表照常给，直读一律返回注入的错误。
type lookupFailureSource struct {
	providers []route.Provider
	byID      map[int64]route.Provider
	err       error
}

func (s lookupFailureSource) Providers(context.Context) ([]route.Provider, error) {
	return s.providers, nil
}

func (s lookupFailureSource) Provider(_ context.Context, id int64) (*route.Provider, error) {
	if s.err != nil {
		return nil, s.err
	}
	provider, ok := s.byID[id]
	if !ok {
		return nil, nil
	}
	return &provider, nil
}

func (s lookupFailureSource) Endpoints(context.Context, int64, convert.ProviderType) ([]route.Endpoint, error) {
	return nil, nil
}

// lookupFailureProvider 造一家最小可用候选。
func lookupFailureProvider(id int64) route.Provider {
	priority := 0
	return route.Provider{
		ID:             id,
		Name:           "p" + strconv.FormatInt(id, 10),
		ProviderType:   convert.ProviderClaude,
		IsEnabled:      true,
		Weight:         1,
		Priority:       &priority,
		CostMultiplier: json.Number("1"),
	}
}

// lookupFailureRedis 读门控变量建 Redis 客户端（库号 >= 13，与 session 包同一纪律）。
func lookupFailureRedis(t *testing.T) *redis.Client {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过会话绑定跨层集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败（值已隐去）: %v", err)
	}
	if options.DB < 13 {
		t.Fatalf("CCH_TEST_REDIS_URL 必须使用 DB index >= 13，收到 %d", options.DB)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("连接 Redis 失败: %v", err)
	}
	return client
}

// runLookupFailureScenario 跑完整链：预置绑定 → 选路（直读按 sourceErr 的形态返回）→
// 备用成功终态 → 读回 Redis 的绑定。返回终态后的绑定 providerID 与本次 bypass。
func runLookupFailureScenario(t *testing.T, sourceErr error) (int64, route.SessionBindingBypass) {
	t.Helper()
	rdb := lookupFailureRedis(t)
	registry, err := ratelimit.Load()
	if err != nil {
		t.Fatalf("加载脚本注册表失败: %v", err)
	}
	client, err := ratelimit.New(rdb, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	binder := session.NewBinder(client)
	if !binder.Ready() {
		t.Fatal("绑定门面未就绪")
	}
	adapter := session.NewSessionBinderAdapter(session.BinderOptions{
		Client: binder,
		TTL:    time.Duration(lookupFailureBindingTTLSeconds) * time.Second,
	})

	const (
		sessionID       = "sess_gotest_r3a_lookupfail"
		keyID     int64 = 4343
		boundID   int64 = 7
		backupID  int64 = 8
	)
	ctx := context.Background()
	keys := session.BuildBindingKeys(sessionID, keyID)
	purge := func() {
		_ = rdb.Del(ctx, keys.Canonical, keys.LegacyProvider, keys.LegacyOwner,
			session.ProviderCooldownKey(sessionID, keyID, boundID),
			session.ProviderCooldownKey(sessionID, keyID, backupID)).Err()
	}
	purge()
	t.Cleanup(purge)

	// 预置绑定：CAS 到 boundID，再读回拿当前 generation（CAS 会旋转 generation）。
	seeded, err := binder.ReadOrReconcile(ctx, sessionID, keyID, lookupFailureBindingTTLSeconds)
	if err != nil || !seeded.OK {
		t.Fatalf("读取空绑定失败: %+v / %v", seeded, err)
	}
	if _, err := binder.CompareAndSet(
		ctx, sessionID, keyID, seeded.Snapshot.Generation, boundID, lookupFailureBindingTTLSeconds,
	); err != nil {
		t.Fatalf("预置绑定失败: %v", err)
	}
	current, err := binder.ReadOrReconcile(ctx, sessionID, keyID, lookupFailureBindingTTLSeconds)
	if err != nil || !current.OK {
		t.Fatalf("读回绑定失败: %+v / %v", current, err)
	}
	if current.Snapshot.ProviderID != boundID {
		t.Fatalf("预置后绑定应指向 %d，实际 %d", boundID, current.Snapshot.ProviderID)
	}
	generation := current.Snapshot.Generation

	bound := lookupFailureProvider(boundID)
	backup := lookupFailureProvider(backupID)
	selector := route.NewSelector(route.Options{
		// 备用排首位：加权随机的脚本取首家，便于断言「回落到备用」而非恰好抽中绑定家。
		Source: lookupFailureSource{
			providers: []route.Provider{backup, bound},
			byID:      map[int64]route.Provider{boundID: bound, backupID: backup},
			err:       sourceErr,
		},
		Affinity: route.NewAffinityStore(route.AffinityOptions{Window: 8}),
		Rand:     func() float64 { return 0 },
	})
	request := route.Request{
		Model: "m", Format: convert.FormatClaude, KeyID: keyID,
		SessionID: sessionID,
		SessionBinding: &route.SessionBindingSnapshot{
			SessionID: sessionID, KeyID: keyID, Generation: generation, ProviderID: boundID,
		},
	}

	result, err := selector.Select(ctx, request)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method == route.MethodSessionReuse {
		t.Fatal("前置条件不成立：读不到绑定行时不得短路为会话复用")
	}

	// 终态：备用成功，绑定跟随 winner。
	writeback := adapter.SessionBindingWriteback(sessionID, keyID, generation)
	if writeback == nil {
		t.Fatal("应拿到会话绑定写回能力（真 Binder 已就绪）")
	}
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}
	if err := pc.SetMessageRequestID(1); err != nil {
		t.Fatalf("写入行标识失败: %v", err)
	}
	pc.SetSessionBindingWriteback(writeback)
	durationMS := 62
	settlement := terminal.Settlement{
		StatusCode:    200,
		DurationMS:    &durationMS,
		ProviderChain: []byte(`[{"id":8,"reason":"request_success"}]`),
		Affinity:      terminal.AffinityDirective{WinnerProviderID: backupID},
	}
	if _, err := terminal.New(lookupFailureWriter{}, terminal.Options{}).
		SettleContext(ctx, pc, settlement, nil); err != nil && !errors.Is(err, terminal.ErrNotSettled) {
		t.Fatalf("终态结算失败: %v", err)
	}

	after, err := binder.ReadOrReconcile(ctx, sessionID, keyID, lookupFailureBindingTTLSeconds)
	if err != nil || !after.OK {
		t.Fatalf("读回绑定失败: %+v / %v", after, err)
	}
	return after.Snapshot.ProviderID, result.SessionBindingBypass
}

// TestSessionBindingFollowsWinnerWhenLookupFailsAcrossLayers 钉住读绑定行失败仍留痕临时原因，
// 但备用成功后 Redis 绑定跟随 winner。
func TestSessionBindingFollowsWinnerWhenLookupFailsAcrossLayers(t *testing.T) {
	providerID, bypass := runLookupFailureScenario(t, errors.New("lookup: 连接池暂时不可用"))
	if bypass != route.SessionBindingBypassTransient {
		t.Fatalf("读绑定行失败应留痕为临时原因，实际 %v", bypass)
	}
	if providerID != 8 {
		t.Fatalf("读绑定行失败后备用成功应改绑至 8，实际 %d", providerID)
	}
}

// TestSessionBindingReboundWhenBindingRowGoneAcrossLayers 钉住行已不存在时留痕 none，
// 备用成功后 Redis 绑定同样跟随 winner。
func TestSessionBindingReboundWhenBindingRowGoneAcrossLayers(t *testing.T) {
	providerID, bypass := runLookupFailureScenario(t, route.ErrProviderNotFound)
	if bypass != route.SessionBindingBypassNone {
		t.Fatalf("行已不存在应留痕为结构性原因，实际 %v", bypass)
	}
	if providerID != 8 {
		t.Fatalf("行已不存在时应允许改绑到备用 8，实际绑定为 %d", providerID)
	}
}
