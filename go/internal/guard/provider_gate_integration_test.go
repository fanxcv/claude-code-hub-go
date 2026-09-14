package guard

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是**数据面门槛的接线集成测试**：真库 + 真适配器集合，从 `ProviderRouter.Select`
// 一路走到 route 的门槛判定。它证的是「接线对」——列读到了、判定器注入了、请求级取值
// （UA/头部/正文/metadata）确实进了判定，而不是「函数单元正确」（那是 route 包的钉子）。
//
// 隔离手法：把夹具供应商放进**唯一分组**（Node 的分组过滤是严格隔离：该分组下没有候选就
// 直接判无可用）。于是「被门槛排除」与「选出别的供应商」不再混在一起，断言只需看
// `ErrNoProviderAvailable`。每个用例都配一个正控，证明隔离本身成立。

// gateFixture 描述一条门槛夹具。
type gateFixture struct {
	name        string
	group       string
	activeStart *string
	activeEnd   *string
	blocked     string
}

// insertProviderGateFixture 建夹具供应商并登记清理，返回 id。
func insertProviderGateFixture(t *testing.T, pools *store.Pools, fixture gateFixture) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	blocked := fixture.blocked
	if blocked == "" {
		blocked = "[]"
	}
	var id int64
	err = pool.QueryRow(ctx, `
		INSERT INTO providers (
			name, url, key, provider_type, is_enabled, weight, priority, cost_multiplier,
			allowed_models, group_tag, active_time_start, active_time_end, blocked_clients
		) VALUES (
			$1, 'https://guard-gate-fixture.invalid', 'fixture-key', 'claude', true, 1, 0, 1,
			'[]', $2, $3, $4, $5::jsonb
		) RETURNING id`,
		testPrefix+"gate-"+fixture.name, fixture.group, fixture.activeStart, fixture.activeEnd, blocked,
	).Scan(&id)
	if err != nil {
		t.Fatalf("插入供应商夹具失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(context.Background(), `DELETE FROM providers WHERE id = $1`, id)
	})
	return id
}

// setProviderActiveWindow 改夹具的活动时段（nil 即恒活跃），用于同一夹具上的正控。
func setProviderActiveWindow(t *testing.T, pools *store.Pools, id int64, start, end *string) {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE providers SET active_time_start = $2, active_time_end = $3 WHERE id = $1`,
		id, start, end,
	); err != nil {
		t.Fatalf("更新活动时段失败: %v", err)
	}
}

// excludingWindow 在系统时区下找一个不含 now 的合法窗口（跨零点会让 start>end 变成跨日语义，
// 因此要搜索而不是固定值）。
func excludingWindow(t *testing.T, now time.Time) (string, string) {
	t.Helper()
	for _, offset := range []int{1, 2, 3, 5, 7, 9, 11} {
		start := now.Add(time.Duration(offset) * time.Hour)
		end := start.Add(time.Hour)
		startText, endText := start.Format("15:04"), end.Format("15:04")
		if !route.ProviderActiveNow(&startText, &endText, now) {
			return startText, endText
		}
	}
	t.Fatalf("没能构造出排除当前时刻的窗口（now=%s）", now.Format(time.RFC3339))
	return "", ""
}

// claudeCLIHeaders 是能让 confirmClaudeCodeSignals 判 confirmed 的头部集合。
func claudeCLIHeaders() map[string]string {
	return map[string]string{
		"User-Agent":     "claude-cli/1.0.0 (external, cli)",
		"x-app":          "cli",
		"anthropic-beta": "prompt-caching-2024-07-31",
		"Content-Type":   "application/json",
	}
}

// claudeCLIBody 带 metadata.user_id（非 count_tokens 路径的必备信号）。
func claudeCLIBody() map[string]any {
	return map[string]any{
		"model":    "claude-3-5-sonnet",
		"metadata": map[string]any{"user_id": "user_fixture"},
	}
}

// TestIntegrationProviderClientGateExcludesBlockedClient 端到端断言供应商级客户端黑名单生效：
// 黑名单命中 → 该分组的唯一候选被排除 → 判无可用；换一个不匹配的 UA（正控）→ 能选出它。
func TestIntegrationProviderClientGateExcludesBlockedClient(t *testing.T) {
	pools := guardIntegrationPools(t)
	adapters := guardIntegrationAdapters(t, pools)
	group := testPrefix + "client-gate-" + randomSuffix(t)

	fixtureID := insertProviderGateFixture(t, pools, gateFixture{
		name:    "client",
		group:   group,
		blocked: `["claude-code-cli"]`,
	})

	// 正文工厂必须在 Apply 之前设：Apply 只在 deps.Body 为空时取适配器持有的工厂，
	// 否则 metadata.user_id 这一类「正文里的事实」取不到，客户端信号确认会静默退化。
	body, _ := bodyFactory(t, claudeCLIBody())
	adapters.SetBodyFactory(body)
	deps := Deps{Logger: quietLogger()}
	adapters.Apply(&deps)
	// 分组与格式由接线方决定（生产走 AuthStore.ProviderGroup 与入站协议）：这里直接钉住，
	// 使请求落在夹具的唯一分组上。
	adapters.Provider.Group = func(context.Context, *pctx.Context) string { return group }
	adapters.Provider.Format = func(*pctx.Context) convert.ClientFormat { return convert.FormatClaude }

	// 负控：UA 命中 claude-code-cli（内置关键字需要信号确认）→ 被黑名单排除。
	blockedCtx := newContext(t, claudeCLIHeaders(), claudeCLIBody())
	if _, err := deps.Provider.Select(context.Background(), blockedCtx); !errors.Is(err, ErrNoProviderAvailable) {
		t.Fatalf("黑名单命中时应判无可用供应商，实际 err=%v", err)
	}

	// 正控：同一分组、同一供应商，UA 换成 curl（内置关键字需要确认，未确认即不命中）
	// → 应该能选出它，证明上一条的失败确实来自客户端门槛而不是分组隔离本身。
	plainHeaders := map[string]string{"User-Agent": "curl/8.0.1", "Content-Type": "application/json"}
	allowedCtx := newContext(t, plainHeaders, map[string]any{"model": "claude-3-5-sonnet"})
	selection, err := deps.Provider.Select(context.Background(), allowedCtx)
	if err != nil {
		t.Fatalf("非命中 UA 应能选出供应商，实际 err=%v", err)
	}
	if selection.ProviderID != fixtureID {
		t.Fatalf("正控选出的供应商 = %d，期望夹具 %d（分组隔离未生效则会选到别的供应商）",
			selection.ProviderID, fixtureID)
	}
	// 直接调用判定器，区分「判定器说放行」与「门槛根本没被调用」两种可能。
	direct := deps.ProviderClientRestriction(blockedCtx, nil, []string{"claude-code-cli"})
	if direct == nil || direct.Allowed || direct.MatchType != route.MatchTypeBlocklistHit {
		t.Fatalf("判定器本身未命中黑名单: %+v", direct)
	}

	// 留痕与 Node 的逐字段对照用证据：把 route 层记录序列化出来（与对拍脚本的取法同源）。
	selector := route.NewSelector(route.Options{Source: adapters.Provider.source})
	result, err := selector.Select(context.Background(), route.Request{
		Model:      "claude-3-5-sonnet",
		Format:     convert.FormatClaude,
		Group:      group,
		ClientGate: adapters.Provider.clientGate(blockedCtx),
	})
	if err != nil {
		t.Fatalf("直接选路失败: %v", err)
	}
	for _, record := range result.Context.FilteredProviders {
		if record.ID != fixtureID {
			continue
		}
		encoded, encodeErr := json.Marshal(record)
		if encodeErr != nil {
			t.Fatalf("序列化记录失败: %v", encodeErr)
		}
		t.Logf("Go 留痕: %s", encoded)
	}
}

// TestIntegrationProviderScheduleGateExcludesProvider 端到端断言活动时段生效：
// 库里的窗口排除「现在」→ 判无可用；把窗口清空（恒活跃）→ 同一供应商可被选出。
func TestIntegrationProviderScheduleGateExcludesProvider(t *testing.T) {
	pools := guardIntegrationPools(t)
	adapters := guardIntegrationAdapters(t, pools)
	group := testPrefix + "schedule-gate-" + randomSuffix(t)

	ctx := context.Background()
	location, err := time.LoadLocation(pools.AdminSystemTimezoneOrUTC(ctx))
	if err != nil {
		t.Fatalf("系统时区不可加载: %v", err)
	}
	now := time.Now().In(location)
	start, end := excludingWindow(t, now)

	id := insertProviderGateFixture(t, pools, gateFixture{
		name:        "schedule",
		group:       group,
		activeStart: &start,
		activeEnd:   &end,
	})

	body, _ := bodyFactory(t, map[string]any{"model": "claude-3-5-sonnet"})
	adapters.SetBodyFactory(body)
	deps := Deps{Logger: quietLogger()}
	adapters.Apply(&deps)
	// 分组与格式由接线方决定（生产走 AuthStore.ProviderGroup 与入站协议）：这里直接钉住，
	// 使请求落在夹具的唯一分组上。
	adapters.Provider.Group = func(context.Context, *pctx.Context) string { return group }
	adapters.Provider.Format = func(*pctx.Context) convert.ClientFormat { return convert.FormatClaude }

	// 负控：窗口（按系统时区判定）不含现在 → 该分组唯一候选被排除。
	outsideCtx := newContext(t, map[string]string{"User-Agent": "curl/8.0.1"}, map[string]any{"model": "claude-3-5-sonnet"})
	if _, err := deps.Provider.Select(context.Background(), outsideCtx); !errors.Is(err, ErrNoProviderAvailable) {
		t.Fatalf("活动时段外应判无可用供应商（窗口 %s-%s，now=%s），实际 err=%v",
			start, end, now.Format("15:04"), err)
	}

	// 正控：把窗口清空（NULL 即恒活跃）→ 同一供应商应可被选出。
	// 注意：选路走的是带 TTL 的快照缓存，改库后必须显式失效，否则读到的仍是旧行
	// （这条也正是生产上「改配置后要广播失效」的同一机制）。
	setProviderActiveWindow(t, pools, id, nil, nil)
	adapters.Provider.Invalidate()
	selection, err := deps.Provider.Select(context.Background(), outsideCtx)
	if err != nil {
		t.Fatalf("窗口清空后应能选出供应商，实际 err=%v", err)
	}
	if selection.ProviderID != id {
		t.Fatalf("选出的供应商 = %d，期望夹具 %d", selection.ProviderID, id)
	}
}
