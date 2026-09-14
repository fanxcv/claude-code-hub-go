package route

// 本文件钉住「因前缀亲和短路而未参与竞争的候选必须留痕」这条可观测性契约。
//
// 为什么必须钉：亲和短路是**唯一**会让一批已通过全部硬校验的候选凭空消失的路径。用户就是据此
// 报的「优先级 1/2/3 的渠道都没参与决策」（生产 session 01a09b85：11 家通过分组、6 家因模型被
// 滤、5 家成为候选，链里只看得见被亲和选中的那 1 家），而它与 filteredProviders（真被滤、带
// reason）在界面上呈现成**同一种观感**——两类完全不同的事实被压成了一个。
//
// 反向同样要钉：**无可报之事时这个键不得出现**。decisionContext 是落链契约，黄金样本对键集有
// 精确相等断言（chain_test.go 的 TestChainItemMatchesGoldenKeySet），白写一个空数组就会打破它。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// affinityRequestTargeting 构造一次「亲和命中指定供应商」的选路输入。
//
// 与 affinity_circuit_rejection_test.go 的同名夹具同法（正文指纹 + 注入的 lookup，不访问 Redis），
// 只是把命中的目标参数化：本文件的用例要复现「命中的那家优先级最低」这一生产形状。
func affinityRequestTargeting(t *testing.T, providerID int64, model string) Request {
	t.Helper()
	body := claudeBody(t, `{"messages": [{"role": "user", "content": "hi"}]}`)
	chain, ok := Fingerprint(body, convert.FormatClaude, 8)
	if !ok {
		t.Fatal("夹具正文应能指纹化")
	}
	return Request{
		Model: model, Format: convert.FormatClaude, KeyID: 42, Group: "codex,fan",
		AffinityBody: body,
		AffinityLookup: &AffinityLookup{
			Hint: &AffinityHint{
				ProviderID:   providerID,
				MatchedFP:    chain.Tip().FP,
				MatchedIndex: 0,
			},
			IdentityFP: "idfp",
			Generation: "v3:gen",
		},
	}
}

// replayModel 是被请求的模型（生产那次就是它）。
const replayModel = "deepseek-v4.1-flash"

// supportsModel 是「声明支持该模型」的规则集。
//
// 形态取自生产：`allowed_models` 的头一条是 `{"pattern": "deepseek-v[\d.]+-flash", "matchType": "exact"}`，
// 而 matchType=exact 在两侧都是**字面量相等**（rules.go 的 matchesPattern），那条本身并不匹配
// `deepseek-v4.1-flash`——真正命中的规则在采样被截断的尾部。夹具只保留语义：这家声明支持它。
func supportsModel(t *testing.T) json.RawMessage {
	t.Helper()
	return mustRaw(t, []any{
		map[string]any{"pattern": `deepseek-v[\d.]+-flash`, "matchType": "exact"},
		map[string]any{"pattern": `deepseek-v[\d.]+-flash`, "matchType": "regex"},
	})
}

// rejectsModel 是「不声明支持该模型」的规则集（生产上那 6 家 codex 类供应商即此形态）。
func rejectsModel(t *testing.T) json.RawMessage {
	t.Helper()
	return mustRaw(t, []any{map[string]any{"pattern": "gpt-5.6-codex", "matchType": "exact"}})
}

// productionReplay 还原生产 session 01a09b85 的供应商池：11 家通过分组，
// 其中 6 家（id 113/156/149/159/78/100）不支持本次模型，5 家成为候选。
//
// 顺序按优先级升序排，与生产库里的观测顺序一致；id 与优先级都照抄生产读数。
func productionReplay(t *testing.T) []Provider {
	t.Helper()
	build := func(id int64, name string, priority int, supports bool) Provider {
		p := baseProvider(id, convert.ProviderClaude)
		p.Name = name
		p.Priority = intPtr(priority)
		p.GroupTag = strPtr("chat,CC-Paid,codex")
		if supports {
			p.AllowedModels = supportsModel(t)
		} else {
			p.AllowedModels = rejectsModel(t)
		}
		return p
	}
	return []Provider{
		build(113, "Any Router_Codex", 0, false),
		build(156, "HC Chat", 0, false),
		build(149, "ARK Codex", 1, false),
		build(138, "OpenCode X Chat", 2, true),
		build(162, "OpenCode Grl Chat", 2, true),
		build(145, "Ollama Codex", 3, true),
		build(163, "CommandCode Chat", 4, true),
		build(148, "Ollama2 Codex", 5, true),
		build(159, "MM_Codex", 20, false),
		build(78, "MyNav_Auto_Codex(Paid)", 21, false),
		build(100, "MyNav_Pro_Codex(Paid)", 29, false),
	}
}

func replaySelector(t *testing.T, providers []Provider) *Selector {
	t.Helper()
	byID := map[int64]Provider{}
	for _, p := range providers {
		byID[p.ID] = p
	}
	return NewSelector(Options{
		Source:   &stubSource{providers: providers, byID: byID},
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
	})
}

// TestAffinityShortCircuitRecordsSkippedCandidates 是主线：生产形状的整场回放。
//
// 四件事一次钉住：① 三类事实（被滤 / 通过却未竞争 / 最终选中）在链上互相可分；
// ② 通过者无一旁落（这是用户投诉的那一点）；③ 落链键名与 JSON 形态（前端按它渲染）；
// ④ 亲和目标即使优先级最低也照样被选中（本次不去动那条既有语义）。
func TestAffinityShortCircuitRecordsSkippedCandidates(t *testing.T) {
	providers := productionReplay(t)
	selector := replaySelector(t, providers)

	result, err := selector.Select(context.Background(), affinityRequestTargeting(t, 163, replayModel))
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 163 {
		t.Fatalf("亲和目标应被选中（优先级最低也照旧），实际 %+v", result.Provider)
	}
	if result.Method != MethodPrefixAffinity {
		t.Fatalf("selectionMethod = %q，期望 %q", result.Method, MethodPrefixAffinity)
	}

	dc := result.Context
	// ① 被滤的那 6 家仍然照旧记 reason，一个不少。
	if dc.TotalProviders != 11 {
		t.Errorf("totalProviders = %d，期望 11（分组后）", dc.TotalProviders)
	}
	if dc.EnabledProviders != 5 || dc.AfterHealthCheck != 5 {
		t.Errorf("通过过滤者应为 5 家，实际 enabled=%d afterHealthCheck=%d",
			dc.EnabledProviders, dc.AfterHealthCheck)
	}
	reasonCounts := map[Reason]int{}
	for _, record := range dc.FilteredProviders {
		reasonCounts[record.Reason]++
	}
	if reasonCounts[ReasonModelNotAllowed] != 6 || len(dc.FilteredProviders) != 6 {
		t.Fatalf("应有 6 家因模型被滤，实际 %d 条：%+v", len(dc.FilteredProviders), dc.FilteredProviders)
	}

	// ② 通过者无一旁落：5 家全在。
	survivors := dc.SurvivingCandidates
	if len(survivors) != 5 {
		t.Fatalf("survivingCandidates 应记下全部 5 家通过者，实际 %d：%+v", len(survivors), survivors)
	}

	// ③ 落链形态逐字节钉住：键名、顺序、数字不加引号、两个优先级数字与标志位都不得缺。
	// 两个优先级并列出现是有意的：effectivePriority 决定分层，priority 是配置值，两者不同时
	// 正说明分组覆盖改写了档位（见 ConsideredCandidate 的注释）。
	encoded, err := json.Marshal(survivors)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	want := `[{"id":138,"name":"OpenCode X Chat","priority":2,"effectivePriority":2,"weight":1,"costMultiplier":1,"selected":false,"affinitySkipped":true},` +
		`{"id":162,"name":"OpenCode Grl Chat","priority":2,"effectivePriority":2,"weight":1,"costMultiplier":1,"selected":false,"affinitySkipped":true},` +
		`{"id":145,"name":"Ollama Codex","priority":3,"effectivePriority":3,"weight":1,"costMultiplier":1,"selected":false,"affinitySkipped":true},` +
		`{"id":163,"name":"CommandCode Chat","priority":4,"effectivePriority":4,"weight":1,"costMultiplier":1,"selected":true,"affinitySkipped":false},` +
		`{"id":148,"name":"Ollama2 Codex","priority":5,"effectivePriority":5,"weight":1,"costMultiplier":1,"selected":false,"affinitySkipped":true}]`
	if string(encoded) != want {
		t.Fatalf("survivingCandidates 落链形态不符\n实际 %s\n期望 %s", encoded, want)
	}

	// ④ 链项里仍缺不得决策上下文（前端读的就是它）。
	item := result.ChainItem()
	if item.DecisionContext == nil || len(item.DecisionContext.SurvivingCandidates) != 5 {
		t.Fatalf("链项应带上决策上下文及其 survivingCandidates：%+v", item.DecisionContext)
	}
	// candidatesAtPriority 保持原样（只记被选中那家）——既有键不改语义。
	if len(dc.CandidatesAtPriority) != 1 || dc.CandidatesAtPriority[0].ID != 163 {
		t.Errorf("candidatesAtPriority 应仍只记被选中那家：%+v", dc.CandidatesAtPriority)
	}
	if len(dc.PriorityLevels) != 1 || dc.PriorityLevels[0] != 4 {
		t.Errorf("priorityLevels 应仍记被选中那家的优先级：%+v", dc.PriorityLevels)
	}
}

// TestAffinityShortCircuitOmitsKeyWhenNothingSkipped 是反向（契约钉子）：通过集里除被选中者外
// 没有别人时**不得**写这个键——decisionContext 的键集是落链契约，白写空数组即打破黄金样本断言。
func TestAffinityShortCircuitOmitsKeyWhenNothingSkipped(t *testing.T) {
	only := baseProvider(163, convert.ProviderClaude)
	only.Priority = intPtr(4)
	// 分组标签要与请求的 userGroup 有交集，否则它连候选都进不了（本用例要的是「唯一通过者
	// 就是被选中者」这一前提，不是分组判定）。
	only.GroupTag = strPtr("chat,CC-Paid,codex")
	selector := NewSelector(Options{
		Source:   &stubSource{providers: []Provider{only}, byID: map[int64]Provider{163: only}},
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
	})

	result, err := selector.Select(context.Background(), affinityRequestTargeting(t, 163, replayModel))
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method != MethodPrefixAffinity {
		t.Fatalf("前置条件不成立：应走亲和短路，实际 %q", result.Method)
	}
	if result.Context.SurvivingCandidates != nil {
		t.Fatalf("唯一通过者就是被选中者，没有「未参与竞争」可说，不该记：%+v",
			result.Context.SurvivingCandidates)
	}
	encoded, err := json.Marshal(result.ChainItem())
	if err != nil {
		t.Fatalf("链项序列化失败: %v", err)
	}
	if strings.Contains(string(encoded), "survivingCandidates") {
		t.Fatalf("无可报之事时该键不得出现（会破坏黄金样本键集断言）：%s", encoded)
	}
}

// TestWeightedSelectionNeverRecordsSurvivors 是第二条反向：非亲和路径本来就把候选写全了
// （candidatesAtPriority + priorityLevels），不该多写这个键——否则既有落链形态会凭空长出一块。
func TestWeightedSelectionNeverRecordsSurvivors(t *testing.T) {
	providers := productionReplay(t)
	selector := replaySelector(t, providers)

	result, err := selector.Select(context.Background(), Request{
		Model: replayModel, Format: convert.FormatClaude, Group: "codex,fan",
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method != MethodGroupFiltered && result.Method != MethodWeightedRandom {
		t.Fatalf("前置条件不成立：应走加权随机，实际 %q", result.Method)
	}
	if result.Provider == nil || result.Provider.EffectivePriority() != 2 {
		t.Fatalf("应选中最高优先（P2）那家，实际 %+v", result.Provider)
	}
	if result.Context.SurvivingCandidates != nil {
		t.Fatalf("加权随机路径不得写 survivingCandidates：%+v", result.Context.SurvivingCandidates)
	}
	if len(result.Context.CandidatesAtPriority) != 2 {
		t.Errorf("竞争发生在同一优先级内，候选应为 P2 的两家，实际 %+v",
			result.Context.CandidatesAtPriority)
	}
}
