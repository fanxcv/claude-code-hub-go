package dataplane

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

// 本文件是**跨层端到端钉子**：真 Binder（真 Redis + 真 Lua）+ 真选路器 + 真终态结算器。
//
// 为什么必须跨层：选路会因冷却跳过绑定，成功终态则须改绑到 winner。故本用例走完整链：
// 预置绑定 → 故障冷却跳过绑定 → 选路给出 winner 与 bypass → 终态结算 → 读回 Redis。
//
// 真库门控：未设 CCH_TEST_REDIS_URL 时跳过（与同目录其余集成用例一致，CI 即跳过）。

const crossLayerTTLSeconds = 3600

// crossLayerRedis 建真 Redis 连接（DB >= 13，与 session 包同一纪律）。
func crossLayerRedis(t *testing.T) *redis.Client {
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

// crossLayerSource 是最小选路数据源：两家固定候选。
type crossLayerSource struct {
	providers []route.Provider
	byID      map[int64]route.Provider
}

func (s crossLayerSource) Providers(context.Context) ([]route.Provider, error) {
	return s.providers, nil
}

func (s crossLayerSource) Provider(_ context.Context, id int64) (*route.Provider, error) {
	provider, ok := s.byID[id]
	if !ok {
		return nil, errors.New("crosslayer: 供应商不存在")
	}
	return &provider, nil
}

func (s crossLayerSource) Endpoints(context.Context, int64, convert.ProviderType) ([]route.Endpoint, error) {
	return nil, nil
}

// crossLayerWriter 是最小终态写入面：只回答「赢下该行」，不落库、不查价。
type crossLayerWriter struct{}

func (crossLayerWriter) CreateMessageRequest(context.Context, store.CreateMessageRequestData) (store.MessageRequest, error) {
	return store.MessageRequest{}, nil
}

func (crossLayerWriter) UpdateDetailsIfUnfinalized(context.Context, int64, store.DetailsPatch) (bool, error) {
	return true, nil
}

func (crossLayerWriter) UpdateWinnerCost(context.Context, int64, string, []byte) error { return nil }

func (crossLayerWriter) FindModelPrice(context.Context, string) (*store.ModelPrice, error) {
	return nil, errors.New("crosslayer: 本用例不查价")
}

// crossLayerProvider 造一家最小可用候选。
func crossLayerProvider(id int64) route.Provider {
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

// newCrossLayerContext 造一个带行标识与绑定写回能力的请求上下文。
func newCrossLayerContext(t *testing.T, writeback pctx.SessionBindingWriteback) *pctx.Context {
	t.Helper()
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}
	if err := pc.SetMessageRequestID(1); err != nil {
		t.Fatalf("写入行标识失败: %v", err)
	}
	pc.SetSessionBindingWriteback(writeback)
	return pc
}

// TestSessionBindingFollowsWinnerAfterProviderErrorCooldownAcrossLayers 钉住冷却跳过绑定、
// 备用成功后绑定改写为 winner；冷却到期也不粘回原家。
func TestSessionBindingFollowsWinnerAfterProviderErrorCooldownAcrossLayers(t *testing.T) {
	rdb := crossLayerRedis(t)
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
		TTL:    time.Duration(crossLayerTTLSeconds) * time.Second,
	})

	const (
		sessionID       = "sess_gotest_p1a_crosslayer"
		keyID     int64 = 4242
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
	seeded, err := binder.ReadOrReconcile(ctx, sessionID, keyID, crossLayerTTLSeconds)
	if err != nil || !seeded.OK {
		t.Fatalf("读取空绑定失败: %+v / %v", seeded, err)
	}
	if _, err := binder.CompareAndSet(ctx, sessionID, keyID, seeded.Snapshot.Generation, boundID, crossLayerTTLSeconds); err != nil {
		t.Fatalf("预置绑定失败: %v", err)
	}
	current, err := binder.ReadOrReconcile(ctx, sessionID, keyID, crossLayerTTLSeconds)
	if err != nil || !current.OK {
		t.Fatalf("读回绑定失败: %+v / %v", current, err)
	}
	if current.Snapshot.ProviderID != boundID {
		t.Fatalf("预置后绑定应指向 %d，实际 %d", boundID, current.Snapshot.ProviderID)
	}
	generation := current.Snapshot.Generation

	writeback := adapter.SessionBindingWriteback(sessionID, keyID, generation)
	if writeback == nil || !writeback.CooldownOnFailure(ctx, boundID) {
		t.Fatal("写入 provider_error_cooldown 失败")
	}

	bound := crossLayerProvider(boundID)
	backup := crossLayerProvider(backupID)
	selector := route.NewSelector(route.Options{
		Source: crossLayerSource{
			providers: []route.Provider{bound, backup},
			byID:      map[int64]route.Provider{boundID: bound, backupID: backup},
		},
		SlowRate: route.NewSlowRateReader(route.SlowRateOptions{Redis: rdb}),
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
	if result.Provider == nil || result.Provider.ID != backupID {
		t.Fatalf("故障冷却中的绑定 provider 不该被选中，实际 %+v", result.Provider)
	}
	if result.SessionBindingBypass != route.SessionBindingBypassTransient {
		t.Fatalf("故障冷却应留痕为临时原因，实际 %v", result.SessionBindingBypass)
	}
	if got := result.Context.FilteredProviders; len(got) == 0 || got[0].Reason != route.ReasonProviderErrorCooldown {
		t.Fatalf("故障冷却应留痕 provider_error_cooldown，实际 %+v", got)
	}

	// 备用成功后，终态应改绑到备用。
	pc := newCrossLayerContext(t, writeback)
	durationMS := 62
	settlement := terminal.Settlement{
		StatusCode:    200,
		DurationMS:    &durationMS,
		ProviderChain: []byte(`[{"id":8,"reason":"request_success"}]`),
		Affinity:      terminal.AffinityDirective{WinnerProviderID: backupID},
	}
	if _, err := terminal.New(crossLayerWriter{}, terminal.Options{}).SettleContext(ctx, pc, settlement, nil); err != nil && !errors.Is(err, terminal.ErrNotSettled) {
		t.Fatalf("终态结算失败: %v", err)
	}

	after, err := binder.ReadOrReconcile(ctx, sessionID, keyID, crossLayerTTLSeconds)
	if err != nil || !after.OK {
		t.Fatalf("读回绑定失败: %+v / %v", after, err)
	}
	if after.Snapshot.ProviderID != backupID {
		t.Fatalf("provider_error_cooldown 跳过绑定家、备用成功后应改绑至 %d，实际 %d", backupID, after.Snapshot.ProviderID)
	}

	// 冷却到期后，用最新绑定快照选路，仍须命中备用。
	if err := rdb.Del(ctx, session.ProviderCooldownKey(sessionID, keyID, boundID)).Err(); err != nil {
		t.Fatalf("清除冷却键失败: %v", err)
	}
	request.SessionBinding.Generation = after.Snapshot.Generation
	request.SessionBinding.ProviderID = backupID
	recovered, err := selector.Select(ctx, request)
	if err != nil {
		t.Fatalf("恢复后选路失败: %v", err)
	}
	if recovered.Provider == nil || recovered.Provider.ID != backupID {
		t.Fatalf("冷却到期后应仍粘于 winner %d，实际 %+v", backupID, recovered.Provider)
	}
	if recovered.Method != route.MethodSessionReuse {
		t.Errorf("冷却到期后应走会话复用，实际 %q", recovered.Method)
	}
}
