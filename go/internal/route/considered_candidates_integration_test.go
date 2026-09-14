package route

// 本文件是**真实库**上的端到端：分组优先级覆盖的两条新语义（须在组内 / 记分层值）与候选留痕，
// 都必须从真实 jsonb 列经**真实读取路径**一路走到选路结果与落链上下文上。
//
// 为什么必须有：这两条语义的输入全是库里那两列的**实际形态**——`group_priorities` 可为
// jsonb null、键是用户组名、值与 `group_tag` 的一致性决定覆盖是否生效。只靠单元测试构造
// `Provider` 字面量，证不出「列读进来之后语义仍成立」。生产者形状照抄生产库实测读到的 7 家
// 越界覆盖。
// 库门控沿用 integration_test.go 的 integrationPools（未设 CCH_TEST_DSN 时整组跳过）。

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

const consideredFixturePrefix = "cch-route-considered-"

// consideredFixture 是一条供应商夹具：只覆盖本文件要证的三个维度（标签、覆盖表、配置优先级）。
type consideredFixture struct {
	label string
	// priority 是 `providers.priority` 列的配置值（覆盖不生效时的回退目标）。
	priority int
	// groupTag 为 nil 即写入 NULL（落回 default 组）。
	groupTag *string
	// groupPriorities 是 `group_priorities` 列的原文；"" 即写入 NULL（生产 17 家非 null 之外
	// 的绝大多数就是这个形态）。
	groupPriorities string
}

// insertConsideredFixture 建一条专用供应商并登记清理（唯一名 + 按 id 删）。
func insertConsideredFixture(t *testing.T, pools *store.Pools, fixture consideredFixture) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}

	var overrides any
	if fixture.groupPriorities != "" {
		overrides = fixture.groupPriorities
	}

	var id int64
	err = pool.QueryRow(ctx, `
		INSERT INTO providers (
			name, url, key, provider_type, is_enabled, weight, priority, cost_multiplier,
			allowed_models, group_tag, group_priorities
		) VALUES (
			$1, 'https://route-considered-fixture.invalid', 'fixture-key', 'claude', true, 1, $2, 1,
			'[]', $3, $4::jsonb
		) RETURNING id`,
		consideredFixturePrefix+fixture.label+"-"+strconv.FormatInt(time.Now().UnixNano(), 10),
		fixture.priority, fixture.groupTag, overrides,
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

// TestIntegrationGroupOverrideNeedsProviderMembership 是那条有意偏离 Node 的语义修正在**真库**上的
// 端到端：覆盖键必须让该供应商自己也在那个组里才生效，否则回退 `priority` 列。
//
// 四种形状分别对应：真在组内的对照、生产 163（`{"fan":0}` 但标签不含 fan）、生产 99/115
// （`{"fan":2}` 且标签与 codex/fan 都不相交）、以及覆盖列写 NULL 的绝大多数。
func TestIntegrationGroupOverrideNeedsProviderMembership(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()

	inGroup := insertConsideredFixture(t, pools, consideredFixture{
		label: "in-group", priority: 4, groupTag: strPtr("chat,fan"), groupPriorities: `{"fan":0}`,
	})
	// 生产 163/161 的形状：标签 chat,CC-Paid,codex，覆盖 {"fan":0}。
	outOfGroupChat := insertConsideredFixture(t, pools, consideredFixture{
		label: "out-of-group-163", priority: 4, groupTag: strPtr("chat,CC-Paid,codex"),
		groupPriorities: `{"fan":0}`,
	})
	// 生产 99/115 的形状：标签 CC-Paid,CC-Fallback，覆盖 {"fan":2}——与 codex/fan 两组都不相交。
	outOfGroupPaid := insertConsideredFixture(t, pools, consideredFixture{
		label: "out-of-group-115", priority: 0, groupTag: strPtr("CC-Paid,CC-Fallback"),
		groupPriorities: `{"fan":2}`,
	})
	nullOverrides := insertConsideredFixture(t, pools, consideredFixture{
		label: "null-overrides", priority: 5, groupTag: strPtr("codex,fan"), groupPriorities: "",
	})

	providers, err := NewStoreSource(pools).Providers(ctx)
	if err != nil {
		t.Fatalf("读取供应商失败: %v", err)
	}

	// ① 真库 jsonb 列经真实读取路径后的分层值。
	want := []struct {
		id   int64
		want int
		why  string
	}{
		{inGroup, 0, "标签含 fan，覆盖生效"},
		{outOfGroupChat, 4, "标签不含 fan ⇒ 越界，回退配置值 4"},
		{outOfGroupPaid, 0, "标签与 fan 不相交 ⇒ 越界，回退配置值 0"},
		{nullOverrides, 5, "覆盖列为 NULL ⇒ nil map，回退配置值 5"},
	}
	for _, item := range want {
		provider := findProvider(t, providers, item.id)
		if got := resolveEffectivePriority(provider, "codex,fan"); got != item.want {
			t.Fatalf("id=%d 的分层优先级 = %d，期望 %d（%s）", item.id, got, item.want, item.why)
		}
	}
	if findProvider(t, providers, nullOverrides).GroupPriorities != nil {
		t.Fatalf("NULL 的覆盖列应解出 nil map（生产 17 家非 null 之外的行都是这个形态）")
	}

	// ② 用这几行**真实数据**跑一次完整选路：越界者不得再占据最高档。
	// 注意 outOfGroupPaid 不会进候选——它的标签与 codex/fan 不相交，分组过滤先把它挡住；
	// 这正是它那份覆盖值在生产里早已无从生效的原因。
	inGroupRow := findProvider(t, providers, inGroup)
	outOfGroupRow := findProvider(t, providers, outOfGroupChat)
	nullRow := findProvider(t, providers, nullOverrides)
	byID := map[int64]Provider{inGroupRow.ID: inGroupRow, outOfGroupRow.ID: outOfGroupRow, nullRow.ID: nullRow}
	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{inGroupRow, outOfGroupRow, nullRow}, byID: byID},
		Rand:   (&scriptedRand{values: []float64{0}}).next,
	})

	result, err := selector.Select(ctx, Request{
		Model: "claude-3-5-sonnet", Format: convert.FormatClaude, Group: "codex,fan",
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != inGroup {
		t.Fatalf("应选中分层值最小的组内那家（id=%d），实际 %+v", inGroup, result.Provider)
	}
	if result.Context.SelectedPriority != 0 {
		t.Fatalf("selectedPriority = %d，期望 0（分组覆盖后的分层值）", result.Context.SelectedPriority)
	}

	// ③ 落链上下文必须把「参与了、但档位更低」的那家说清（用户报的就是看不见它）。
	chainJSON, err := json.Marshal(result.ChainItem())
	if err != nil {
		t.Fatalf("链项序列化失败: %v", err)
	}
	var decoded struct {
		DecisionContext struct {
			ConsideredCandidates []ConsideredCandidate `json:"consideredCandidates"`
		} `json:"decisionContext"`
	}
	if err := json.Unmarshal(chainJSON, &decoded); err != nil {
		t.Fatalf("链项反序列化失败: %v", err)
	}
	considered := decoded.DecisionContext.ConsideredCandidates
	if len(considered) != 3 {
		t.Fatalf("consideredCandidates 应记下 3 家候选，实际 %d：%s", len(considered), chainJSON)
	}
	found := false
	for _, candidate := range considered {
		if candidate.ID != outOfGroupChat {
			continue
		}
		found = true
		if candidate.Selected {
			t.Fatalf("越界的 163 形状不该被选中：%+v", candidate)
		}
		if candidate.EffectivePriority != 4 || candidate.Priority != 4 {
			t.Fatalf("越界者的两个优先级都应回退为 4，实际 priority=%d effective=%d",
				candidate.Priority, candidate.EffectivePriority)
		}
	}
	if !found {
		t.Fatalf("越界的那家应在候选留痕里（它通过了硬校验，只是档位更低）：%s", chainJSON)
	}
}
