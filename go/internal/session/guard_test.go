package session

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// TestAdapterSatisfiesGuardSeam 是接线的编译期契约：会话包提供的适配器必须原样满足
// internal/guard 定义的 SessionBinder 缝隙，签名由编译器而非文档保证。
var _ guard.SessionBinder = (*SessionBinderAdapter)(nil)

// guardSessionRequest 造一份守卫链会传给会话缝隙的入口事实。
func guardSessionRequest(body map[string]any) guard.SessionRequest {
	return guard.SessionRequest{
		KeyID:     testKeyID,
		Body:      body,
		Headers:   map[string][]string{"user-agent": {"claude-cli/1.0.0"}},
		UserAgent: "claude-cli/1.0.0",
	}
}

// fakeBodyAccess 是正文缝隙的最小实现：持有同一棵树，Store 就地替换。
type fakeBodyAccess struct {
	body map[string]any
}

func (a *fakeBodyAccess) JSON() (map[string]any, error) { return a.body, nil }
func (a *fakeBodyAccess) Store(body map[string]any) error {
	a.body = body
	return nil
}

// 以下桩件只为满足 Assemble 的必需缝隙校验：会话缝隙不在必需集合里（灰度期允许缺失），
// 因此要证明「装配成功」就必须把其余缝隙补齐，否则测到的是校验本身。
type (
	stubAuth      struct{}
	stubUser      struct{}
	stubProvider  struct{}
	stubMessages  struct{}
	stubSettings  struct{}
	stubSensitive struct{}
	stubFilters   struct{}
)

func (stubAuth) ResolveAPIKey(context.Context, string) (guard.AuthResolution, error) {
	return guard.AuthResolution{
		User: guard.User{ID: 7, IsEnabled: true, AllowedClients: []string{"claude"}, AllowedModels: []string{"claude-sonnet-4"}},
		Key:  guard.Key{ID: testKeyID, UserID: 7},
	}, nil
}

func (stubUser) User(context.Context, int64) (guard.User, error) {
	return guard.User{ID: 7, IsEnabled: true, AllowedClients: []string{"claude"}, AllowedModels: []string{"claude-sonnet-4"}}, nil
}

func (stubProvider) Select(context.Context, *pctx.Context) (pctx.ProviderSelection, error) {
	return pctx.ProviderSelection{ProviderID: 1, Name: "stub", Endpoint: "https://upstream.invalid"}, nil
}

func (stubMessages) EnsureContext(context.Context, *pctx.Context) error { return nil }

func (stubSettings) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return &store.SystemSettings{}, nil
}

func (stubSensitive) SensitiveWords(context.Context) ([]guard.SensitiveWord, error) { return nil, nil }

func (stubFilters) RequestFilters(context.Context) ([]guard.RequestFilter, error) { return nil, nil }

// TestAdapterIsInjectableIntoGuardChain 证明适配器可注入真实守卫链：
// 装配成功、链条里确实有 session 步、且该步能取到适配器并跑完。
func TestAdapterIsInjectableIntoGuardChain(t *testing.T) {
	body := map[string]any{
		"model":    "claude-sonnet-4",
		"messages": []any{map[string]any{"role": "user", "content": "接线用例"}},
		"metadata": map[string]any{"user_id": `{"session_id":"sess_gotest_wiring"}`},
	}

	// 不接 Redis：本用例只证明「可注入」，真实 Redis 的行为由 TestStepSessionBindsAgainstRedis 覆盖。
	deps := guard.Deps{
		Auth:           stubAuth{},
		Users:          stubUser{},
		Provider:       stubProvider{},
		MessageContext: stubMessages{},
		Settings:       stubSettings{},
		Sensitive:      stubSensitive{},
		Filters:        stubFilters{},
		Body: func(*pctx.Context) (guard.BodyAccess, error) {
			return &fakeBodyAccess{body: body}, nil
		},
		Sessions: NewSessionBinderAdapter(BinderOptions{TTL: time.Duration(testTTLSeconds) * time.Second}),
	}

	chain, err := guard.Assemble(deps, guard.ChatPolicy())
	if err != nil {
		t.Fatalf("装配守卫链失败（会话缝隙应当可注入）: %v", err)
	}
	hasSession := false
	for _, key := range guard.ChainSteps(chain) {
		if key == guard.StepSession {
			hasSession = true
		}
	}
	if !hasSession {
		t.Fatalf("对答链里应含 session 步: %v", guard.ChainSteps(chain))
	}

	reqCtx, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages", Headers: http.Header{}})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}
	reqCtx.SetAuth(pctx.AuthState{KeyID: testKeyID, UserID: 7})

	step, ok := deps.Steps()[guard.StepSession]
	if !ok {
		t.Fatal("步骤索引缺少 session 步")
	}
	response, err := step(reqCtx)
	if err != nil {
		t.Fatalf("session 步失败: %v", err)
	}
	if response != nil {
		t.Fatalf("session 步不应早退: %+v", response)
	}
}

// TestStepSessionBindsAgainstRedis 用真实 Redis 跑 session 步：证明适配器不只是「能编译进
// 链条」，而是真的在链条的调用点完成了会话身份分配（读 Redis 侧效应作为证据）。
func TestStepSessionBindsAgainstRedis(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()

	sessionID := "sess_gotest_wiring_redis"
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)

	body := map[string]any{
		"model":    "claude-sonnet-4",
		"messages": []any{map[string]any{"role": "user", "content": "接线用例"}},
		"metadata": map[string]any{"user_id": `{"session_id":"` + sessionID + `"}`},
	}

	deps := guard.Deps{
		Auth:     stubAuth{},
		Users:    stubUser{},
		Provider: stubProvider{},
		Body: func(*pctx.Context) (guard.BodyAccess, error) {
			return &fakeBodyAccess{body: body}, nil
		},
		Sessions: NewSessionBinderAdapter(BinderOptions{
			Client: binder,
			TTL:    time.Duration(testTTLSeconds) * time.Second,
		}),
	}

	reqCtx, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages", Headers: http.Header{}})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}
	reqCtx.SetAuth(pctx.AuthState{KeyID: testKeyID, UserID: 7})

	step := deps.Steps()[guard.StepSession]
	if _, err := step(reqCtx); err != nil {
		t.Fatalf("session 步失败: %v", err)
	}

	sequence, err := rdb.Get(ctx, SeqKey(sessionID)).Result()
	if err != nil {
		t.Fatalf("session 步未分配会话序号: %v", err)
	}
	if sequence != "1" {
		t.Fatalf("首次分配序号应为 1: got=%q", sequence)
	}
	if _, err := rdb.Get(ctx, LastSeenKey(sessionID)).Result(); err != nil {
		t.Fatalf("session 步未刷新最后活动时间: %v", err)
	}

	// 第二次调用必须推进序号（同一会话内序号单调递增）。
	if _, err := step(reqCtx); err != nil {
		t.Fatalf("session 步第二次调用失败: %v", err)
	}
	next, err := rdb.Get(ctx, SeqKey(sessionID)).Result()
	if err != nil {
		t.Fatalf("读会话序号失败: %v", err)
	}
	if next != "2" {
		t.Fatalf("第二次分配序号应为 2: got=%q", next)
	}
}

// TestStepSessionSkipsWithoutKey 钉住守卫链的前置条件：未鉴权（无密钥 id）时 session 步
// 必须静默跳过，不得因缺依赖而报错。
func TestStepSessionSkipsWithoutKey(t *testing.T) {
	deps := guard.Deps{
		Body: func(*pctx.Context) (guard.BodyAccess, error) {
			return &fakeBodyAccess{body: map[string]any{}}, nil
		},
		Sessions: NewSessionBinderAdapter(BinderOptions{Client: NewBinder(nil)}),
	}
	reqCtx, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages", Headers: http.Header{}})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}

	step := deps.Steps()[guard.StepSession]
	response, err := step(reqCtx)
	if err != nil {
		t.Fatalf("未鉴权时 session 步不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("未鉴权时 session 步不应早退: %+v", response)
	}
}

// TestAdapterWithoutBinderIsInert 钉住「缝隙缺失」的退化：未装配 Binder 时适配器自身
// 仍可用（生成一次性会话），不会把守卫链拖崩。
func TestAdapterWithoutBinderIsInert(t *testing.T) {
	adapter := NewSessionBinderAdapter(BinderOptions{})
	result, err := adapter.Ensure(context.Background(), guardSessionRequest(map[string]any{}))
	if err != nil {
		t.Fatalf("未装配 Binder 不应报错: %v", err)
	}
	if result.SessionID == "" || result.Sequence <= 0 {
		t.Fatalf("应给出降级会话与序号: %+v", result)
	}
	// 降级序号是时间戳加抖动，必须仍是正整数（负数会被后续写入当成合法序号用掉）。
	if result.Sequence < 0 {
		t.Fatalf("降级序号应为正: %d", result.Sequence)
	}
}
