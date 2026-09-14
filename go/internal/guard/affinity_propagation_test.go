package guard

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/redis/go-redis/v9"
)

// 本文件钉住「选路阶段产生的亲和写回事实不得在守卫边界被丢弃」。
//
// 历史缺陷形态：ProviderRouter.Select 只把 ProviderID/Name/Type/Endpoint 搬进
// pctx.ProviderSelection，route.Result 上的亲和事实被静默丢掉，终态层再也拿不到它们——
// 表现是切换后亲和静默失效（粘性退化，且没有任何告警）。
//
// 断言尽量走真实 Redis 行为：写回键落在 route 算出的 scope 与 tip 指纹上；
// 字段一旦被丢，键就写不出来。

// affinityFakeSource 是只服务本测试的固定候选源。
//
// 本测试不测选路（选的是哪一个供应商），只测事实搬运，故 Source 是固定候选而不是真实快照：
// 与 route 里那句「不要用假快照测真选路」的分工一致——这里测的不是选路结论。
type affinityFakeSource struct {
	provider route.Provider
}

func (s affinityFakeSource) Providers(context.Context) ([]route.Provider, error) {
	return []route.Provider{s.provider}, nil
}

func (s affinityFakeSource) Provider(_ context.Context, id int64) (*route.Provider, error) {
	if id != s.provider.ID {
		return nil, nil
	}
	copied := s.provider
	return &copied, nil
}

func (s affinityFakeSource) Endpoints(context.Context, int64, convert.ProviderType) ([]route.Endpoint, error) {
	return nil, nil
}

// affinityTestProvider 是本测试唯一的候选。
func affinityTestProvider() route.Provider {
	return route.Provider{
		ID:           7,
		Name:         "affinity-propagation",
		ProviderType: convert.ProviderClaude,
		URL:          "http://127.0.0.1:1",
		IsEnabled:    true,
		Weight:       100,
	}
}

// newAffinityRouter 造一个把亲和存储接到真实 Redis 的选路适配器。
func newAffinityRouter(store *route.AffinityStore) *ProviderRouter {
	source := affinityFakeSource{provider: affinityTestProvider()}
	return &ProviderRouter{
		selector: route.NewSelector(route.Options{Source: source, Affinity: store}),
		source:   source,
		logger:   quietLogger(),
	}
}

// newAffinityBody 造可指纹化的 claude 正文：系统段 + 一条会话消息，
// 于是 tip 深度不为 0（tip 落在系统段时按 Node 语义不写绑定，本测试需要能写）。
func newAffinityBody() *fakeBody {
	return newFakeBody(map[string]any{
		"model":  "claude-sonnet-4-5",
		"system": "你是网关测试助手",
		"messages": []any{
			map[string]any{"role": "user", "content": "第一条会话消息"},
		},
	})
}

// newAffinityContext 造一个带密钥身份的请求上下文。
func newAffinityContext(t *testing.T) *pctx.Context {
	t.Helper()
	pc, err := pctx.New(pctx.Init{
		Method:       "POST",
		Path:         "/v1/messages",
		ProtocolFrom: egress.FamilyAnthropicMessages,
		Logger:       quietLogger(),
	})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	pc.SetAuth(pctx.AuthState{KeyID: 1, APIKey: "sk-guard-affinity"})
	return pc
}

// guardAffinityScope 造本次运行独有的 scope 并登记清理。
func guardAffinityScope(t *testing.T, client redis.UniversalClient) string {
	t.Helper()
	scope := "guard" + time.Now().Format("150405.000000000")
	t.Cleanup(func() {
		ctx := context.Background()
		keys, _, err := client.Scan(ctx, 0, "cch:pfx:{"+scope+":*", 200).Result()
		if err != nil {
			return
		}
		if len(keys) > 0 {
			_ = client.Del(ctx, keys...).Err()
		}
	})
	return scope
}

// TestProviderRouterInstallsAffinityWriteback 钉住搬运：Select 之后 pctx 里有写回能力，
// 且它写出的键落在 route 计算的 scope 与 tip 上。
func TestProviderRouterInstallsAffinityWriteback(t *testing.T) {
	client := guardIntegrationRedis(t)
	ctx := context.Background()
	guardAffinityScope(t, client)
	store := route.NewAffinityStore(route.AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 300,
	})

	router := newAffinityRouter(store)
	body := newAffinityBody()
	router.Body = body.factory()
	tree, err := body.JSON()
	if err != nil {
		t.Fatalf("读取正文失败: %v", err)
	}

	// 先在选路层拿到基准事实，再走适配器安装一次，最后比对写出来的键——
	// 三者一致才说明 scope/tip/identity/generation 都真的穿过了守卫边界。
	baseline, err := router.selector.Select(ctx, route.Request{
		Model:        "claude-sonnet-4-5",
		Format:       convert.FormatClaude,
		Group:        route.GroupDefault,
		KeyID:        1,
		AffinityBody: tree,
	})
	if err != nil {
		t.Fatalf("基准选路失败: %v", err)
	}
	if baseline.AffinityWriteback == nil {
		t.Fatal("亲和参与时 route.Result 必须带写回事实")
	}

	pc := newAffinityContext(t)
	selection, err := router.Select(ctx, pc)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if selection.ProviderID != affinityTestProvider().ID {
		t.Fatalf("选中的供应商不符: %+v", selection)
	}

	writeback, ok := pc.AffinityWriteback()
	if !ok || writeback == nil {
		t.Fatal("Select 必须把亲和写回能力装进 pctx（否则终态层静默不写亲和）")
	}
	if !writeback.RecordWinner(ctx, selection.ProviderID) {
		t.Fatal("写回应成功：tip 深度不为 0，且 generation 已由查找 ensure")
	}

	facts := baseline.AffinityWriteback
	if facts.ScopeTag != route.ScopeTag(1, convert.FormatClaude, "claude-sonnet-4-5") {
		t.Fatalf("scope = %q，与选路结果不一致", facts.ScopeTag)
	}
	wantKey := "cch:pfx:{" + facts.ScopeTag + "}:fp:" + facts.TipFP
	value, err := client.Get(ctx, wantKey).Result()
	if err != nil {
		t.Fatalf("写回键必须落在选路给出的 scope/tip 上（%s）: %v", wantKey, err)
	}
	wantValue := "1|" + fmt.Sprint(selection.ProviderID) + "|" + facts.IdentityFP + "|" + facts.Generation
	if value != wantValue {
		t.Fatalf("写回值 = %q，期望 %q（identity/generation 必须来自选路时的查找）", value, wantValue)
	}
}

// TestProviderRouterSkipsAffinityWhenDisabled 是反向钉子：亲和未启用时不得装入槽位，
// 终态层据此跳过，而不是去推断「本次没有亲和」。
func TestProviderRouterSkipsAffinityWhenDisabled(t *testing.T) {
	router := newAffinityRouter(nil)
	router.Body = newAffinityBody().factory()
	pc := newAffinityContext(t)

	if _, err := router.Select(context.Background(), pc); err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if _, ok := pc.AffinityWriteback(); ok {
		t.Fatal("亲和未启用时不得装入写回能力")
	}
}

// TestProviderRouterAffinityWritebackMatchesRouteResult 直接比对 route.Result 与 pctx 槽位：
// 行为断言只看「有没有写」，看不出 scope 搬错，这里补上逐字段比对。
func TestProviderRouterAffinityWritebackMatchesRouteResult(t *testing.T) {
	client := guardIntegrationRedis(t)
	ctx := context.Background()
	guardAffinityScope(t, client)
	store := route.NewAffinityStore(route.AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 300,
	})
	router := newAffinityRouter(store)
	body := newAffinityBody()

	tree, err := body.JSON()
	if err != nil {
		t.Fatalf("读取正文失败: %v", err)
	}
	result, err := router.selector.Select(ctx, route.Request{
		Model:        "claude-sonnet-4-5",
		Format:       convert.FormatClaude,
		Group:        route.GroupDefault,
		KeyID:        1,
		AffinityBody: tree,
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.AffinityWriteback == nil {
		t.Fatal("亲和参与时 route.Result 必须带写回事实")
	}
	wantScope := route.ScopeTag(1, convert.FormatClaude, "claude-sonnet-4-5")
	if result.AffinityWriteback.ScopeTag != wantScope {
		t.Fatalf("scope = %q，期望 %q", result.AffinityWriteback.ScopeTag, wantScope)
	}
	if result.AffinityWriteback.TipFP == "" || result.AffinityWriteback.TipDepth == 0 {
		t.Fatalf("tip 事实不符: fp=%q depth=%d",
			result.AffinityWriteback.TipFP, result.AffinityWriteback.TipDepth)
	}
	if result.AffinityWriteback.IdentityFP == "" || result.AffinityWriteback.Generation == "" {
		t.Fatalf("identity 与 generation 必须由查找给出: %+v", result.AffinityWriteback)
	}
}
