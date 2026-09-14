package guard

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 会话空闲闸门的端到端用例（守卫层）。
//
// 分工：`route` 包钉 store 的两个方法与键的选型；本文件钉**选路行为与留痕**——
// 命中被撤销、撤销后确实重新选举、以及两种 fail-open 情形（客户端不带会话身份）可辨。
//
// 为什么判定必须在守卫层：会话身份是每请求事实（走既有会话缝合道 SessionLookup），
// 而 route 的 Lookup 只拿得到指纹链（session 包 import guard，反向引用成环）。

// idleFakeSource 是两家候选的固定源：7 是亲和提名的目标，8 是重新选举时按权重落位的另一家。
type idleFakeSource struct {
	providers []route.Provider
}

func (s idleFakeSource) Providers(context.Context) ([]route.Provider, error) {
	return s.providers, nil
}

func (s idleFakeSource) Provider(_ context.Context, id int64) (*route.Provider, error) {
	for _, provider := range s.providers {
		if provider.ID == id {
			copied := provider
			return &copied, nil
		}
	}
	return nil, nil
}

func (s idleFakeSource) Endpoints(context.Context, int64, convert.ProviderType) ([]route.Endpoint, error) {
	return nil, nil
}

// idleProvider 造一家只差 ID 的候选。
func idleProvider(id int64) route.Provider {
	return route.Provider{
		ID:           id,
		Name:         "idle-gate-" + string(rune('a'+id%26)),
		ProviderType: convert.ProviderClaude,
		URL:          "http://127.0.0.1:1",
		IsEnabled:    true,
		Weight:       100,
	}
}

// idleGateRouter 造一个带日志缓冲与固定随机源的选路器。
//
// 固定 Rand 是让「亲和提名」与「重新选举」可分辨的手段：亲和路径只有一家候选（不经随机），
// 重新选举走加权随机 ⇒ 两者落到不同供应商。
func idleGateRouter(source route.Source, store *route.AffinityStore, logs *bytes.Buffer, sessionID string) *ProviderRouter {
	options := route.Options{
		Source:   source,
		Affinity: store,
		Rand:     func() float64 { return 0.99 },
		Logger:   quietLogger(),
	}
	return &ProviderRouter{
		selector:     route.NewSelector(options),
		routeOptions: options,
		sessionID:    sessionID,
		logger:       logx.New(logs),
	}
}

// 会话步骤必须把已解析的会话身份盖上本次请求的选路器：这是空闲闸门唯一的判定输入。
// 不钉这一条的话，「步骤未接线 / 步骤顺序变了」会静默退化成「永不撤销」，
// 而那道余道恰好就是本闸门存在的理由。
func TestSessionStepStampsConversationIdentityOnRouter(t *testing.T) {
	binder := &fakeBinder{result: SessionResult{SessionID: "sess-idle-gate", Sequence: 2}}
	router := &ProviderRouter{}
	body := map[string]any{"messages": []any{}}
	factory, _ := bodyFactory(t, body)
	deps := Deps{Sessions: binder, Provider: router, Body: factory}

	ctx := newContext(t, map[string]string{"user-agent": "claude-cli/1.0.0"}, body)
	withAuth(ctx, 3, 7, "sk-x")

	response, err := deps.sessionStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatal("会话步骤不应产生响应")
	}
	got, ok := router.conversationID()
	if !ok || got != "sess-idle-gate" {
		t.Fatalf("会话身份未盖上选路器: got=%q ok=%v", got, ok)
	}
}

func TestProviderRouterSuppressesIdleConversationAffinity(t *testing.T) {
	client := guardIntegrationRedis(t)
	ctx := context.Background()
	// 只登记清理，作用域本身由选路结果给出（避免用例自行重建口径）。
	guardAffinityScope(t, client)

	const thresholdSeconds = 120
	store := route.NewAffinityStore(route.AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: thresholdSeconds,
	})

	nominee := idleProvider(7)
	other := idleProvider(8)
	source := idleFakeSource{providers: []route.Provider{nominee, other}}

	sessionID := "guard-idle-" + time.Now().Format("150405.000000000")
	logs := &bytes.Buffer{}
	router := idleGateRouter(source, store, logs, sessionID)
	body := newAffinityBody()
	router.Body = body.factory()

	tree, err := body.JSON()
	if err != nil {
		t.Fatalf("读取正文失败: %v", err)
	}
	// 基准事实（scope/tip/identity/generation）来自选路本身，避免用例自行重建这套口径。
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
		t.Fatal("亲和参与时应有写回事实")
	}
	facts := baseline.AffinityWriteback
	// 在 tip 上写下指向 7 的绑定：本题要的是「本可命中 7」。
	binding := "1|7|" + facts.IdentityFP + "|" + facts.Generation
	bindingKey := "cch:pfx:{" + facts.ScopeTag + "}:fp:" + facts.TipFP
	if err := client.Set(ctx, bindingKey, binding, 5*time.Minute).Err(); err != nil {
		t.Fatalf("写入绑定失败: %v", err)
	}
	pc := newAffinityContext(t)

	// ① 本会话刚活跃：照常采信亲和（粘性不得被闸门损害）。
	store.NoteConversationActivity(ctx, facts.ScopeTag, sessionID, time.Now().Unix()-1)
	logs.Reset()
	selection, err := router.Select(ctx, pc)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if selection.ProviderID != nominee.ID {
		t.Fatalf("刚活跃的会话应继续粘在 %d，实际 %d", nominee.ID, selection.ProviderID)
	}
	if strings.Contains(logs.String(), "affinity_expired") {
		t.Fatalf("刚活跃不得撤销亲和: %s", logs.String())
	}

	// ② 本会话空闲超阈：撤销亲和，从全部候选重新选举。
	now := time.Now().Unix()
	store.NoteConversationActivity(ctx, facts.ScopeTag, sessionID, now-thresholdSeconds-5)
	logs.Reset()
	selection, err = router.Select(ctx, pc)
	if err != nil {
		t.Fatalf("撤销后选路失败: %v", err)
	}
	if selection.ProviderID == nominee.ID {
		t.Fatalf("空闲超阈后不得再粘在 %d（应从全部候选重新选举）", nominee.ID)
	}
	if selection.ProviderID != other.ID {
		t.Fatalf("重新选举应落到另一家 %d，实际 %d", other.ID, selection.ProviderID)
	}
	event := logs.String()
	if !strings.Contains(event, "guard.adapters.affinity_expired") {
		t.Fatalf("撤销必须留痕（否则事后分不清闸门生效还是亲和没装配）: %s", event)
	}
	for _, field := range []string{"idleSeconds", "ttlSeconds", "sessionId", "\"providerId\":7"} {
		if !strings.Contains(event, field) {
			t.Fatalf("留痕缺少字段 %s: %s", field, event)
		}
	}
	// 撤销的当次请求仍要刷新活跃时刻：下一次请求就该恢复正常粘性（否则对话永久失粘）。
	_, _, known := store.ConversationIdle(ctx, facts.ScopeTag, sessionID, time.Now().Unix())
	if !known {
		t.Fatal("撤销当次请求本身是活跃，必须刷新活跃时刻")
	}

	// ③ 客户端不带会话身份：fail-open（仍走亲和），但必须留下可辨的痕迹。
	silent := &bytes.Buffer{}
	router.SetConversationSession("")
	router.logger = logx.New(silent)
	store.NoteConversationActivity(ctx, facts.ScopeTag, sessionID, time.Now().Unix()-thresholdSeconds-5)
	selection, err = router.Select(ctx, pc)
	if err != nil {
		t.Fatalf("无会话身份时选路失败: %v", err)
	}
	if selection.ProviderID != nominee.ID {
		t.Fatalf("无会话身份时必须 fail-open（不得无故打断粘性），实际 %d", selection.ProviderID)
	}
	if !strings.Contains(silent.String(), "guard.adapters.affinity_idle_unknown") {
		t.Fatalf("无会话身份的 fail-open 必须可辨: %s", silent.String())
	}
}
