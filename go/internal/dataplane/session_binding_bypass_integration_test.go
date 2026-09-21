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
// 为什么必须跨层：这条缺陷的判定在选路层、动作在终态层，两者隔着守卫链与 pctx。分层钉子
// 各自全绿仍可能整体失效（判定做对了但没人读，或读到了但写回照样发）。故本用例走完整链：
// 预置绑定 → 熔断跳过绑定 → 选路给出 winner 与 bypass → 盖保留事实 → 终态结算 → 读回 Redis。
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

// TestSessionBindingKeptWhenBoundProviderInCircuitAcrossLayers 是本 lane 的主线钉子。
//
// 判据（缺一即红）：
//  1. 绑定 provider 熔断开闸时，本次能选别家，且 bypass 判为「须保留绑定」；
//  2. 备用**成功**终态跑完之后，Redis 里的绑定**仍指向原 provider**（这是缺陷本体）；
//  3. 熔断恢复后，下一次选路重新命中原 provider（设计稿 §4：待恢复后仍粘回去）。
func TestSessionBindingKeptWhenBoundProviderInCircuitAcrossLayers(t *testing.T) {
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
	stateKey := route.ProviderStateKeyPrefix + strconv.FormatInt(boundID, 10)
	purge := func() {
		_ = rdb.Del(ctx, keys.Canonical, keys.LegacyProvider, keys.LegacyOwner,
			session.ProviderCooldownKey(sessionID, keyID, boundID),
			session.ProviderCooldownKey(sessionID, keyID, backupID),
			stateKey).Err()
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

	// 熔断状态：真 HealthReader 读真 Redis 的键（键形制取自 route.ProviderStateKeyPrefix）。
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err := rdb.HSet(ctx, stateKey, map[string]any{
		"circuitState":     string(route.StateOpen),
		"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
	}).Err(); err != nil {
		t.Fatalf("写入熔断状态失败: %v", err)
	}

	bound := crossLayerProvider(boundID)
	backup := crossLayerProvider(backupID)
	selector := route.NewSelector(route.Options{
		Source: crossLayerSource{
			providers: []route.Provider{bound, backup},
			byID:      map[int64]route.Provider{boundID: bound, backupID: backup},
		},
		Health:   route.NewHealthReader(route.HealthOptions{Redis: rdb, Now: func() time.Time { return now }}),
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
		t.Fatalf("熔断的绑定 provider 不该被选中，实际 %+v", result.Provider)
	}
	if !result.SessionBindingBypass.KeepsBinding() {
		t.Fatalf("熔断开闸应判为临时原因，实际 %v", result.SessionBindingBypass)
	}

	// 终态：备用成功。守卫链在这一步盖「不得改绑」事实（与 guard/adapters_route.go 同源）。
	writeback := adapter.SessionBindingWriteback(sessionID, keyID, generation)
	if writeback == nil {
		t.Fatal("应拿到会话绑定写回能力（真 Binder 已就绪）")
	}
	pc := newCrossLayerContext(t, writeback)
	if result.SessionBindingBypass.KeepsBinding() {
		pc.SetSessionBindingKeep(result.SessionBindingBypass.String())
	}
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
	if after.Snapshot.ProviderID != boundID {
		t.Fatalf("临时原因下备用成功不得改绑：绑定应仍为 %d，实际 %d"+
			"（改绑后「熔断恢复仍粘回去」即为假，且每次熔断都会把会话永久搬走一次）",
			boundID, after.Snapshot.ProviderID)
	}

	// 熔断恢复：删掉熔断状态，下一次选路必须重新命中原 provider。
	if err := rdb.Del(ctx, stateKey).Err(); err != nil {
		t.Fatalf("清除熔断状态失败: %v", err)
	}
	recovered, err := selector.Select(ctx, request)
	if err != nil {
		t.Fatalf("恢复后选路失败: %v", err)
	}
	if recovered.Provider == nil || recovered.Provider.ID != boundID {
		t.Fatalf("熔断恢复后应重新粘回 %d，实际 %+v", boundID, recovered.Provider)
	}
	if recovered.Method != route.MethodSessionReuse {
		t.Errorf("熔断恢复后应走会话复用，实际 %q", recovered.Method)
	}
}
