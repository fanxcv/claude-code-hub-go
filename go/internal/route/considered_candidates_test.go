package route

// 本文件钉住两件事，它们都是**用户报上来的**（2026-09-14，生产 session 01a09b85）：
//
//  1. `selectedPriority` 必须是**分组覆盖后**的分层值，不能是 `priority` 配置列的原值。
//     生产链上写着 `selectedPriority=4`，而同一条 `decisionContext.priorityLevels=[0,2,3,5]`
//     里根本没有 4——因为分层用的是覆盖值 0，记账却用了配置值 4。使用者（与排查者）据此把一次
//     正确的选择读成「低优先级被选中」，我自己就先误判了一次。
//
//  2. 「通过了硬校验却落选」的候选必须留痕。原先把选中的那一家记进 `candidatesAtPriority`，
//     其余四家凭空消失，于是用户问「我看这个会话的决策链，还是看不到 opencode 那几个渠道参与呢？」
//     ——它们其实都在候选池里，只是档位更低。
//
// 另有一处**有意偏离 Node** 的语义修正钉在同一份文件的规则测试里：分组覆盖键必须让该供应商
// 自己也在那个组里才生效（见 TestResolveEffectivePriorityRequiresGroupMembership）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// weightedRequest 是一次**不咨询亲和**的首次选择输入（生产那一行走的就是这条路径：
// reason=initial_selection、selectionMethod=weighted_random）。
func weightedRequest() Request {
	return Request{Model: replayModel, Format: convert.FormatClaude, Group: "codex,fan"}
}

// productionReplayWithFanMembership 在 productionReplay 之上只改一处：让 163 真的**属于** fan 组
// （标签含 fan）并保留它的覆盖值 `{"fan":0}`。
//
// 这一处差别决定了本文件两条主线的分工：163 在组内时覆盖生效（分层 0、配置 4），两者不同，
// 正是缺陷 1 的形状；而生产那一行的标签是 `chat,CC-Paid,codex`（不含 fan），覆盖**不该**生效
// （见 TestOutOfGroupOverrideDoesNotOverrideTier）。
func productionReplayWithFanMembership(t *testing.T) []Provider {
	t.Helper()
	providers := productionReplay(t)
	for index := range providers {
		if providers[index].ID != 163 {
			continue
		}
		providers[index].GroupTag = strPtr("chat,CC-Paid,codex,fan")
		providers[index].GroupPriorities = map[string]int{"fan": 0}
	}
	return providers
}

// productionShapedConsideredJSON 是生产形状候选池（5 家通过、配置档位 2/2/3/5，外加 163 因分组覆盖
// 升到 0 档）的 consideredCandidates **落链形态**：键名、顺序、两个优先级数字、selected 标志都钉死。
//
// 亲和短路与加权随机两条路径**共用同一个期望值**：同一份池子就该产出同一份留痕，路径不该改变形状
// （这正是本次补做的诉求：两条路径的留痕此前不对称）。
const productionShapedConsideredJSON = `[{"id":138,"name":"OpenCode X Chat","priority":2,"effectivePriority":2,"weight":1,"costMultiplier":1,"selected":false},` +
	`{"id":162,"name":"OpenCode Grl Chat","priority":2,"effectivePriority":2,"weight":1,"costMultiplier":1,"selected":false},` +
	`{"id":145,"name":"Ollama Codex","priority":3,"effectivePriority":3,"weight":1,"costMultiplier":1,"selected":false},` +
	`{"id":163,"name":"CommandCode Chat","priority":4,"effectivePriority":0,"weight":1,"costMultiplier":1,"selected":true},` +
	`{"id":148,"name":"Ollama2 Codex","priority":5,"effectivePriority":5,"weight":1,"costMultiplier":1,"selected":false}]`

// TestResolveEffectivePriorityRequiresGroupMembership 是那条有意偏离 Node 的语义修正的主用例：
// 覆盖键只在「用户在该组」**且**「供应商自己也在该组」时生效。
//
// Node 只看前者，于是把供应商移出分组时，遗留的覆盖值继续生效——生产实证有 7 家这种越界覆盖，
// 全是 fan 键（99/115 覆盖 2；154/155/156/161/163 覆盖 0）。
func TestResolveEffectivePriorityRequiresGroupMembership(t *testing.T) {
	cases := []struct {
		name      string
		tag       *string
		overrides map[string]int
		priority  int
		userGroup string
		want      int
		why       string
	}{
		{
			name: "组内覆盖生效", tag: strPtr("chat,fan"), overrides: map[string]int{"fan": 0},
			priority: 4, userGroup: "codex,fan", want: 0,
			why: "供应商确实在 fan 组（标签含 fan）",
		},
		{
			name: "越界覆盖不生效（回退 priority 列）", tag: strPtr("chat,CC-Paid,codex"),
			overrides: map[string]int{"fan": 0}, priority: 4, userGroup: "codex,fan", want: 4,
			why: "生产 163 的形状：标签不含 fan，摘除分组后覆盖不得继续把它抬到最高档",
		},
		{
			name: "越界覆盖不生效（生产 99/115 形状）", tag: strPtr("CC-Paid,CC-Fallback"),
			overrides: map[string]int{"fan": 2}, priority: 0, userGroup: "codex,fan", want: 0,
			why: "标签不含 fan，覆盖 2 不生效，回退配置值 0",
		},
		{
			name: "匹配组取最小（组内多键）", tag: strPtr("fan,codex"),
			overrides: map[string]int{"fan": 5, "codex": 3}, priority: 9, userGroup: "codex,fan",
			want: 3, why: "两个键都在组内，取最小 3",
		},
		{
			name: "组内多键，越界的那个被忽略", tag: strPtr("fan"),
			overrides: map[string]int{"fan": 5, "codex": 1}, priority: 9, userGroup: "codex,fan",
			want: 5, why: "codex 键越界（标签不含 codex）⇒ 忽略，只剩 fan 的 5",
		},
		{
			name: "用户在别的组", tag: strPtr("fan"), overrides: map[string]int{"fan": 0},
			priority: 4, userGroup: "codex", want: 4,
			why: "用户组里没有 fan，覆盖键根本匹配不上",
		},
		{
			name: "无覆盖（jsonb null 解出 nil map）", tag: strPtr("fan"), overrides: nil,
			priority: 4, userGroup: "codex,fan", want: 4,
			why: "17 家非 null 之外的行都是这个形态，不得 panic",
		},
		{
			name: "覆盖表为空 map", tag: strPtr("fan"), overrides: map[string]int{},
			priority: 4, userGroup: "codex,fan", want: 4,
			why: "同上",
		},
		{
			name: "用户组为空（不做分组判定）", tag: strPtr("fan"), overrides: map[string]int{"fan": 0},
			priority: 4, userGroup: "", want: 4,
			why: "无分组语义时覆盖无从谈起",
		},
		{
			name: "标签为 null（落回 default）而覆盖键恰是 default", tag: nil,
			overrides: map[string]int{"default": 1}, priority: 7, userGroup: "default",
			want: 1, why: "标签缺省即 default，与分组过滤同一口径",
		},
		{
			name: "标签为 null 而覆盖键是别的组", tag: nil,
			overrides: map[string]int{"fan": 0}, priority: 7, userGroup: "fan",
			want: 7, why: "标签缺省是 default，不含 fan ⇒ 越界",
		},
		{
			name: "异常取值不 panic（负数与极大值照常取最小）", tag: strPtr("fan"),
			overrides: map[string]int{"fan": -3}, priority: 4, userGroup: "fan", want: -3,
			why: "脏数据不该让选路崩掉，取值如实透出",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := baseProvider(1, convert.ProviderClaude)
			provider.GroupTag = tc.tag
			provider.GroupPriorities = tc.overrides
			provider.Priority = intPtr(tc.priority)

			if got := resolveEffectivePriority(provider, tc.userGroup, nil); got != tc.want {
				t.Fatalf("resolveEffectivePriority = %d，期望 %d（%s）", got, tc.want, tc.why)
			}
		})
	}
}

// TestSelectedPriorityFollowsGroupOverride 是缺陷 1 的正面钉子：分层值与配置值不同时，
// `selectedPriority` 必须记**分层值**。
//
// 反证方式：把 `select.go` 的 `dc.SelectedPriority = resolveEffectivePriority(top[0], req.Group)`
// 换回 `top[0].EffectivePriority()`，本用例立刻红（得到 4 而非 0）——那正是生产链上的错误读数。
func TestSelectedPriorityFollowsGroupOverride(t *testing.T) {
	providers := productionReplayWithFanMembership(t)
	selector := replaySelector(t, providers)

	result, err := selector.Select(context.Background(), weightedRequest())
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	// 163 的分层值是 0（fan 覆盖、且在组内），其余四家是 2/2/3/5 ⇒ 它独占最高档。
	if result.Provider == nil || result.Provider.ID != 163 {
		t.Fatalf("应选中唯一最高档（163），实际 %+v", result.Provider)
	}
	if got := result.Context.SelectedPriority; got != 0 {
		t.Fatalf("selectedPriority = %d，期望 0（分组覆盖后的分层值；4 是配置列的原值）", got)
	}
	// 两个数字都要在场能被区分：配置值仍是 4，分层值是 0。
	if raw := result.Provider.EffectivePriority(); raw != 4 {
		t.Fatalf("前置条件不成立：163 的配置优先级应为 4，实际 %d", raw)
	}
	// 与生产读取对齐：priorityLevels 是**去重升序**的档位集合。
	want := []int{0, 2, 3, 5}
	if len(result.Context.PriorityLevels) != len(want) {
		t.Fatalf("priorityLevels = %v，期望 %v", result.Context.PriorityLevels, want)
	}
	for index := range want {
		if result.Context.PriorityLevels[index] != want[index] {
			t.Fatalf("priorityLevels = %v，期望 %v", result.Context.PriorityLevels, want)
		}
	}
}

// TestConsideredCandidatesExposeEveryCandidate 是缺陷 2 的主用例：候选池里的每一家都要留痕，
// 含各自的分层值与是否被选中——用户就是靠它看出「那两个渠道参与了、只是档位更低」。
func TestConsideredCandidatesExposeEveryCandidate(t *testing.T) {
	providers := productionReplayWithFanMembership(t)
	selector := replaySelector(t, providers)

	result, err := selector.Select(context.Background(), weightedRequest())
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	dc := result.Context
	if len(dc.ConsideredCandidates) != 5 {
		t.Fatalf("consideredCandidates 应记下全部 5 家通过者，实际 %d：%+v",
			len(dc.ConsideredCandidates), dc.ConsideredCandidates)
	}

	// 落链形态逐字节钉住：键名、顺序、两个优先级数字、selected 标志。
	encoded, err := json.Marshal(dc.ConsideredCandidates)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	want := productionShapedConsideredJSON
	if string(encoded) != want {
		t.Fatalf("consideredCandidates 落链形态不符\n实际 %s\n期望 %s", encoded, want)
	}

	// 用户的问题在这里被回答：OpenCode 两家**在链上**，且能读出「参与了、档位 2 > 选中档 0」。
	for _, candidate := range dc.ConsideredCandidates {
		if candidate.ID != 138 && candidate.ID != 162 {
			continue
		}
		if candidate.Selected {
			t.Fatalf("id=%d 不该被选中", candidate.ID)
		}
		if candidate.EffectivePriority <= dc.SelectedPriority {
			t.Fatalf("id=%d 的分层值 %d 应大于选中档 %d，才能解释它的落选",
				candidate.ID, candidate.EffectivePriority, dc.SelectedPriority)
		}
	}

	// 链项（落库的那一份）必须带上它，否则前端读不到。
	item := result.ChainItem()
	if item.DecisionContext == nil || len(item.DecisionContext.ConsideredCandidates) != 5 {
		t.Fatalf("链项应带上决策上下文及其 consideredCandidates：%+v", item.DecisionContext)
	}
	chainJSON, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("链项序列化失败: %v", err)
	}
	if !strings.Contains(string(chainJSON), "consideredCandidates") {
		t.Fatalf("链项 JSON 里应有 consideredCandidates：%s", chainJSON)
	}
}

// TestOutOfGroupOverrideDoesNotOverrideTier 是修正后的**行为对照**：生产那 7 家越界覆盖里，
// 163 的标签不含 fan，于是它的 `{"fan":0}` 不再生效——它在 fan 组请求里回到配置档位 4，
// 分层让给档位 2 的 OpenCode 两家。这正是用户期待的「新会话应该走优先级更高的渠道」。
func TestOutOfGroupOverrideDoesNotOverrideTier(t *testing.T) {
	// productionReplay 原样：163 带 {"fan":0} 但标签是 chat,CC-Paid,codex（不含 fan）。
	providers := productionReplay(t)
	for index := range providers {
		if providers[index].ID == 163 {
			providers[index].GroupPriorities = map[string]int{"fan": 0}
		}
	}
	byID := map[int64]Provider{}
	for _, p := range providers {
		byID[p.ID] = p
	}
	selector := NewSelector(Options{
		Source:   &stubSource{providers: providers, byID: byID},
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		// 最高档是两家权重相同的 OpenCode（138/162，各自 weight 1）：0 落在第一家上。
		Rand: (&scriptedRand{values: []float64{0}}).next,
	})

	result, err := selector.Select(context.Background(), weightedRequest())
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil {
		t.Fatal("应有可用供应商")
	}
	switch result.Provider.ID {
	case 138, 162:
	default:
		t.Fatalf("选中 id=%d，期望越界的 163 不再被覆盖抬档、落到档位 2 的 OpenCode 两家之一",
			result.Provider.ID)
	}
	if got := result.Context.SelectedPriority; got != 2 {
		t.Fatalf("selectedPriority = %d，期望 2（163 的越界覆盖已失效，最高档是 2）", got)
	}
	// 163 仍在候选池里（它通过了全部硬校验），只是档位更低——正是要在链上说清的那件事。
	found := false
	for _, candidate := range result.Context.ConsideredCandidates {
		if candidate.ID != 163 {
			continue
		}
		found = true
		if candidate.EffectivePriority != 4 || candidate.Priority != 4 {
			t.Fatalf("163 应回退配置档位 4，实际 effective=%d priority=%d",
				candidate.EffectivePriority, candidate.Priority)
		}
	}
	if !found {
		t.Fatalf("163 应仍在候选留痕里：%+v", result.Context.ConsideredCandidates)
	}
}

// TestConsideredCandidatesOmittedWhenNothingToReport 是反向契约钉子：唯一候选必然被选中，
// 没有「参与了却没选中」可说，键不得出现——否则黄金样本的键集精确相等断言会红
// （go/testdata/golden/message_request_row.json 就是单供应商场景）。
func TestConsideredCandidatesOmittedWhenNothingToReport(t *testing.T) {
	provider := baseProvider(1, convert.ProviderClaude)
	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{provider}, byID: map[int64]Provider{1: provider}},
	})

	result, err := selector.Select(context.Background(), Request{
		Model: "claude-3-5-sonnet", Format: convert.FormatClaude, Group: GroupDefault,
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Context.ConsideredCandidates != nil {
		t.Fatalf("唯一候选无需留痕，不该记：%+v", result.Context.ConsideredCandidates)
	}
	encoded, err := json.Marshal(result.ChainItem())
	if err != nil {
		t.Fatalf("链项序列化失败: %v", err)
	}
	if strings.Contains(string(encoded), "consideredCandidates") {
		t.Fatalf("无可报之事时该键不得出现（会破坏黄金样本键集断言）：%s", encoded)
	}
}

// TestAffinityPathRecordsResolvedPriorityToo 钉住亲和短路那条路径的同一处口径：它的
// survivingCandidates 与 selectedPriority 也必须用分组覆盖后的值（否则同一条记录里
// 「Priority 0」的标题与「P4」的徽标会互相打架）。
func TestAffinityPathRecordsResolvedPriorityToo(t *testing.T) {
	providers := productionReplayWithFanMembership(t)
	selector := replaySelector(t, providers)

	result, err := selector.Select(context.Background(), affinityRequestTargeting(t, 163, replayModel))
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method != MethodPrefixAffinity {
		t.Fatalf("前置条件不成立：应走亲和短路，实际 %q", result.Method)
	}
	dc := result.Context
	if dc.SelectedPriority != 0 {
		t.Fatalf("selectedPriority = %d，期望 0（fan 覆盖、且在组内）", dc.SelectedPriority)
	}
	if len(dc.PriorityLevels) != 1 || dc.PriorityLevels[0] != 0 {
		t.Fatalf("priorityLevels = %v，期望 [0]", dc.PriorityLevels)
	}
	var selected *SurvivingCandidate
	for index := range dc.SurvivingCandidates {
		if dc.SurvivingCandidates[index].ID == 163 {
			selected = &dc.SurvivingCandidates[index]
		}
	}
	if selected == nil {
		t.Fatalf("survivingCandidates 应含 163：%+v", dc.SurvivingCandidates)
	}
	if selected.Priority != 4 || selected.EffectivePriority != 0 {
		t.Fatalf("163 应同时记配置值 4 与分层值 0，实际 priority=%d effective=%d",
			selected.Priority, selected.EffectivePriority)
	}
	if !selected.Selected || selected.AffinitySkipped {
		t.Fatalf("163 应标为已选中且未被跳过：%+v", selected)
	}
	// 亲和路径**也**要写统一的参与池键，且与 survivingCandidates **同源同序**：生产 3 小时窗口里
	// 1549 行 affinity_hit 的 considered 计数为 0、surviving 合计 5108——统一读前者的界面在
	// 这几行上什么也看不到，而用户报的恰好就是这几行。
	if len(dc.ConsideredCandidates) != len(dc.SurvivingCandidates) {
		t.Fatalf("两个键成员数应一致：considered=%d surviving=%d",
			len(dc.ConsideredCandidates), len(dc.SurvivingCandidates))
	}
	for index := range dc.SurvivingCandidates {
		if dc.ConsideredCandidates[index] != dc.SurvivingCandidates[index].ConsideredCandidate {
			t.Fatalf("第 %d 项两个键不一致：considered=%+v surviving=%+v", index,
				dc.ConsideredCandidates[index], dc.SurvivingCandidates[index])
		}
	}
}

// TestAffinityPathRecordsConsideredCandidates 是本次补做的主用例：亲和短路**也要**写「参与池」。
//
// 生产实证（2026-09-14，cchd-1.4.0，3 小时窗口；行号由协调者给定，我在本机重取复核）：
//
//	960131/960132/960133（reason=affinity_hit、prefix_affinity）:
//	  candidatesAtPriority=1、consideredCandidates=0、survivingCandidates=5
//	affinity_hit 1549 行：considered 合计 0、surviving 合计 5108
//	initial_selection 272 行：considered 合计 35、surviving 合计 0
//
// 即候选池数据一直在，但只挂在 survivingCandidates 上；界面统一读 consideredCandidates 时，那 1549
// 行是全空的——用户抱怨的正是这几行。本用例钉住修复后两条路径的落链形状一致。
func TestAffinityPathRecordsConsideredCandidates(t *testing.T) {
	providers := productionReplayWithFanMembership(t)
	selector := replaySelector(t, providers)

	result, err := selector.Select(context.Background(), affinityRequestTargeting(t, 163, replayModel))
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method != MethodPrefixAffinity {
		t.Fatalf("前置条件不成立：应走亲和短路，实际 %q", result.Method)
	}
	dc := result.Context
	if len(dc.ConsideredCandidates) != 5 {
		t.Fatalf("亲和路径也应记下全部 5 家通过者，实际 %d：%+v",
			len(dc.ConsideredCandidates), dc.ConsideredCandidates)
	}

	encoded, err := json.Marshal(dc.ConsideredCandidates)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if string(encoded) != productionShapedConsideredJSON {
		t.Fatalf("亲和路径的 consideredCandidates 应与加权随机路径同形\n实际 %s\n期望 %s",
			encoded, productionShapedConsideredJSON)
	}

	// 「为何未被选中」要能从链上读出来：本次系亲和短路（surviving 侧的 AffinitySkipped 为真），
	// 而不是档位更低或同档未被抽中。
	if len(dc.SurvivingCandidates) != 5 {
		t.Fatalf("survivingCandidates 应同时在场（它才带得出「为何未参与」），实际 %d",
			len(dc.SurvivingCandidates))
	}
	for index, survivor := range dc.SurvivingCandidates {
		if survival := dc.ConsideredCandidates[index]; survival != survivor.ConsideredCandidate {
			t.Fatalf("第 %d 项两个键不一致：%+v / %+v", index, survival, survivor)
		}
		if survivor.AffinitySkipped == survivor.Selected {
			t.Fatalf("AffinitySkipped 与 Selected 应互为反相：%+v", survivor)
		}
	}

	// Node parity 不被顺手放宽：Node 在亲和路径上 candidatesAtPriority 也只记一家。
	if len(dc.CandidatesAtPriority) != 1 {
		t.Fatalf("candidatesAtPriority 应仍只记被选中那一家（Node 口径），实际 %d",
			len(dc.CandidatesAtPriority))
	}
}

// TestAffinityPathExposesHigherTierCandidates 是用户原话所在的场景：亲和命中的档位**不是最高**时，
// 「更优档位也在池里」这件事必须能从链上读出来（「看不到 opencode 那几个渠道参与呢」）。
//
// 语义前提（用户已裁决维持现状）：亲和无视优先级，命中即短路整场竞争。故本用例**不**要求亲和改
// 选高分档位，只要求链上能看出「本可更优」——否则使用者只能看到链首那一家，无从判断选路是否合理。
func TestAffinityPathExposesHigherTierCandidates(t *testing.T) {
	// productionReplay 原样：163 分层 4，而 138/162 分层 2。
	providers := productionReplay(t)
	selector := replaySelector(t, providers)

	result, err := selector.Select(context.Background(), affinityRequestTargeting(t, 163, replayModel))
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method != MethodPrefixAffinity || result.Provider.ID != 163 {
		t.Fatalf("前置条件不成立：应亲和命中 163，实际 method=%q provider=%+v",
			result.Method, result.Provider)
	}
	dc := result.Context
	if dc.SelectedPriority != 4 {
		t.Fatalf("selectedPriority = %d，期望 4（163 的分层值；亲和路径只记被选中者的档位）",
			dc.SelectedPriority)
	}

	better := 0
	for _, candidate := range dc.ConsideredCandidates {
		if candidate.Selected {
			continue
		}
		if candidate.EffectivePriority < dc.SelectedPriority {
			better++
		}
	}
	if better == 0 {
		t.Fatalf("链上应能读出「有更高档位的候选在池里」（138/162 分层 2 < 选中档 4），实际：%+v",
			dc.ConsideredCandidates)
	}

	// 那两家还得说清为何没被选中：亲和短路，而不是它们不满足硬校验。
	skippedByAffinity := 0
	for _, survivor := range dc.SurvivingCandidates {
		if survivor.EffectivePriority < dc.SelectedPriority {
			if !survivor.AffinitySkipped {
				t.Fatalf("更高档位的候选应标为因亲和短路而跳过：%+v", survivor)
			}
			skippedByAffinity++
		}
	}
	if skippedByAffinity != better {
		t.Fatalf("两个键对「更高档位候选」的家数不一致：considered=%d surviving=%d",
			better, skippedByAffinity)
	}
}

// TestSurvivorProjectionsAgreeOnEdgeCases 是「一个计算、两个投影」这条不变量在边界上的钉子：
// 两个键必须**一起在场或一起缺席**，成员永远同一批。
//
// 为何不能各自算：consideredCandidates 的闸门是 `len(通过集) < 2`，affinitySurvivors 的闸门是
// `被跳过者 == 0`，两者在「通过集只有一家、而提名者不在其中」时分叉——两次独立计算正是分叉的入口。
func TestSurvivorProjectionsAgreeOnEdgeCases(t *testing.T) {
	passers := productionReplay(t)[3:7] // 138/162/145/163，提名 999 不在其中
	survivors := affinitySurvivors(passers, 999, "codex,fan", nil)
	considered := consideredFromSurvivors(survivors)
	if len(survivors) != len(passers) || len(considered) != len(passers) {
		t.Fatalf("提名者不在通过集内时，两个键都应记全：surviving=%d considered=%d",
			len(survivors), len(considered))
	}
	for index := range survivors {
		if considered[index] != survivors[index].ConsideredCandidate {
			t.Fatalf("第 %d 项不一致：%+v / %+v", index, considered[index], survivors[index])
		}
		if considered[index].Selected {
			t.Fatalf("提名者不在通过集内时不该有 Selected 项：%+v", considered[index])
		}
		if !survivors[index].AffinitySkipped {
			t.Fatalf("提名者不在通过集内时整表都应标 AffinitySkipped：%+v", survivors[index])
		}
	}

	// 通过集只有一家且就是提名者：没有可说之事，两个键一起缺席（黄金样本键集不受影响）。
	alone := productionReplay(t)[3:4]
	if got := affinitySurvivors(alone, 138, "codex,fan", nil); got != nil {
		t.Fatalf("唯一候选人必然被选中，survivingCandidates 不该出现：%+v", got)
	} else if mirror := consideredFromSurvivors(got); mirror != nil {
		t.Fatalf("consideredCandidates 应随之一起缺席：%+v", mirror)
	}
}
