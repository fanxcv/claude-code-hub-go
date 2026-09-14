package route

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

type scriptedRand struct {
	values []float64
	index  int
}

func (s *scriptedRand) next() float64 {
	if len(s.values) == 0 {
		return 0
	}
	value := s.values[s.index%len(s.values)]
	s.index++
	return value
}

func TestPriorityLayeringKeepsHighestPriorityOnly(t *testing.T) {
	high := baseProvider(1, convert.ProviderClaude)
	high.Priority = intPtr(10)
	low := baseProvider(2, convert.ProviderClaude)
	low.Priority = intPtr(1)

	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{high, low}},
		Rand:   (&scriptedRand{values: []float64{0}}).next,
	})
	result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 2 {
		t.Fatalf("应选中优先级数值最小的供应商 2")
	}
	if len(result.Context.PriorityLevels) != 2 || result.Context.PriorityLevels[0] != 1 {
		t.Errorf("priorityLevels = %v，期望升序去重的 [1 10]", result.Context.PriorityLevels)
	}
	if result.Context.SelectedPriority != 1 {
		t.Errorf("selectedPriority = %d，期望 1", result.Context.SelectedPriority)
	}
	if len(result.Context.CandidatesAtPriority) != 1 || result.Context.CandidatesAtPriority[0].ID != 2 {
		t.Errorf("candidatesAtPriority 应只含该优先级的一组: %+v", result.Context.CandidatesAtPriority)
	}
}

func TestGroupPriorityOverrideWins(t *testing.T) {
	overridden := baseProvider(1, convert.ProviderClaude)
	overridden.Priority = intPtr(5)
	overridden.GroupPriorities = map[string]int{"vip": 0}
	overridden.GroupTag = strPtr("vip")

	plain := baseProvider(2, convert.ProviderClaude)
	plain.Priority = intPtr(1)
	plain.GroupTag = strPtr("vip")

	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{overridden, plain}},
		Rand:   (&scriptedRand{values: []float64{0}}).next,
	})
	result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude, Group: "vip"})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 1 {
		t.Fatalf("分组覆盖优先级应让供应商 1 胜出")
	}
	if result.Method != MethodGroupFiltered {
		t.Errorf("selectionMethod = %q，期望 group_filtered", result.Method)
	}
}

func TestWeightedRandomIsReproducibleWithInjectedRand(t *testing.T) {
	a := baseProvider(1, convert.ProviderClaude)
	a.Weight = 10
	b := baseProvider(2, convert.ProviderClaude)
	b.Weight = 1

	pick := func(position float64) int64 {
		selector := NewSelector(Options{
			Source: &stubSource{providers: []Provider{a, b}},
			Rand:   (&scriptedRand{values: []float64{position}}).next,
		})
		result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
		if err != nil {
			t.Fatalf("选路失败: %v", err)
		}
		if result.Provider == nil {
			t.Fatalf("应选出供应商")
		}
		return result.Provider.ID
	}

	// 总权重 11：落在 [0, 10/11) 命中供应商 1，落在 [10/11, 1) 命中供应商 2。
	if got := pick(0.5); got != 1 {
		t.Errorf("随机值 0.5 应命中供应商 1，实际 %d", got)
	}
	if got := pick(0.95); got != 2 {
		t.Errorf("随机值 0.95 应命中供应商 2，实际 %d", got)
	}
}

func TestWeightedRandomFallsBackToUniformWhenAllWeightsZero(t *testing.T) {
	a := baseProvider(1, convert.ProviderClaude)
	a.Weight = 0
	b := baseProvider(2, convert.ProviderClaude)
	b.Weight = 0

	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{a, b}},
		Rand:   (&scriptedRand{values: []float64{0.9}}).next,
	})
	result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 2 {
		t.Fatalf("全零权重应退化为等概率随机：0.9 命中第二项")
	}
	for _, candidate := range result.Context.CandidatesAtPriority {
		if candidate.Probability != 0 {
			t.Errorf("总权重为 0 时概率应为 0，实际 %v", candidate.Probability)
		}
	}
}

func TestProbabilityAndCostOrderingDoNotChangeReportedOrder(t *testing.T) {
	cheap := baseProvider(1, convert.ProviderClaude)
	cheap.CostMultiplier = "0.5"
	cheap.Weight = 3
	expensive := baseProvider(2, convert.ProviderClaude)
	expensive.CostMultiplier = "2"
	expensive.Weight = 1

	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{cheap, expensive}},
		Rand:   (&scriptedRand{values: []float64{0}}).next,
	})
	result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if len(result.Context.CandidatesAtPriority) != 2 {
		t.Fatalf("候选数 = %d，期望 2", len(result.Context.CandidatesAtPriority))
	}
	if result.Context.CandidatesAtPriority[0].ID != 1 || result.Context.CandidatesAtPriority[1].ID != 2 {
		t.Errorf("candidatesAtPriority 应保持快照顺序，不按成本重排: %+v", result.Context.CandidatesAtPriority)
	}
	if result.Context.CandidatesAtPriority[0].Probability != 0.75 {
		t.Errorf("概率应 = 3/4，实际 %v", result.Context.CandidatesAtPriority[0].Probability)
	}
}

func TestCostOrderingBreaksTieForWeightedRandom(t *testing.T) {
	// 两项权重相同、成本不同：排序后低成本在前，同一随机值应命中低成本项。
	costly := baseProvider(1, convert.ProviderClaude)
	costly.CostMultiplier = "3"
	cheap := baseProvider(2, convert.ProviderClaude)
	cheap.CostMultiplier = "0.1"

	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{costly, cheap}},
		Rand:   (&scriptedRand{values: []float64{0}}).next,
	})
	result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 2 {
		t.Fatalf("随机值 0 应命中排序后的第一项（低成本）")
	}
}

func TestSelectWithoutCandidatesReportsContext(t *testing.T) {
	selector := NewSelector(Options{Source: &stubSource{providers: nil}})
	result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider != nil {
		t.Fatalf("无候选时不应产出供应商")
	}
	if result.Context.PriorityLevels == nil || result.Context.CandidatesAtPriority == nil {
		t.Errorf("留痕数组应初始化为空切片而不是 nil")
	}
}

func TestSelectRequiresSource(t *testing.T) {
	selector := NewSelector(Options{})
	if _, err := selector.Select(context.Background(), Request{}); err == nil {
		t.Fatalf("未提供 Source 应报错")
	}
}

func TestChainItemSnapshot(t *testing.T) {
	provider := baseProvider(1, convert.ProviderClaude)
	provider.Weight = 7
	provider.GroupTag = strPtr("cli")

	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{provider}},
		Rand:   (&scriptedRand{values: []float64{0}}).next,
	})
	result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	item := result.ChainItem()
	if item.ID != 1 || item.Weight == nil || *item.Weight != 7 {
		t.Errorf("落链项未记录当时的权重快照: %+v", item)
	}
	if item.Reason != string(ReasonSelectedInitial) || item.SelectionMethod != string(MethodWeightedRandom) {
		t.Errorf("落链项的原因与方法不符: %+v", item)
	}
	if item.DecisionContext == nil || item.DecisionContext.TargetType != TargetClaude {
		t.Errorf("落链项缺少决策上下文")
	}
	if item.Timestamp == 0 {
		t.Errorf("落链项应带时间戳")
	}
}
