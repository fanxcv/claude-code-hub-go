package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// fakeCostSettings 是计费设置源的桩：只回答 billing_model_source。
type fakeCostSettings struct {
	source string
	err    error
}

func (f fakeCostSettings) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &store.SystemSettings{BillingModelSource: f.source}, nil
}

// fakeCostPrices 是价格表的桩：按模型名给价格数据，缺失的模型返回 nil 行。
type fakeCostPrices struct {
	byModel map[string]string
	calls   int
}

func (f *fakeCostPrices) FindModelPrice(_ context.Context, model string) (*store.ModelPrice, error) {
	f.calls++
	raw, ok := f.byModel[model]
	if !ok {
		return nil, nil
	}
	return &store.ModelPrice{ModelName: model, PriceData: json.RawMessage(raw)}, nil
}

// fakeCostGroups 是分组倍率表的桩。
type fakeCostGroups struct {
	byName map[string]float64
	calls  int
}

func (f *fakeCostGroups) FindGroupMultipliers(_ context.Context, names []string) (map[string]float64, error) {
	f.calls++
	out := map[string]float64{}
	for _, name := range names {
		if value, ok := f.byName[name]; ok {
			out[name] = value
		}
	}
	return out, nil
}

// priceJSON 是一份最小可用价格：输入 1 美元/百万 token，输出 2 美元/百万 token。
const priceJSON = `{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}`

// usageAt 造一份用量（输入 1000 / 输出 500），两个字段都显式给值。
func usageAt(input, output int64) terminal.Usage {
	return terminal.Usage{InputTokens: &input, OutputTokens: &output}
}

func newTestResolver(t *testing.T, source string, prices map[string]string, groups map[string]float64) (*costResolver, *fakeCostPrices, *fakeCostGroups) {
	t.Helper()
	priceReader := &fakeCostPrices{byModel: prices}
	groupReader := &fakeCostGroups{byName: groups}
	resolver := newCostResolverWith(fakeCostSettings{source: source}, priceReader, groupReader, nil)
	return resolver, priceReader, groupReader
}

// TestCostResolverBillingSourcePicksModel 覆盖取价基准的两个取值与回落。
//
// 这是计费口径的第一道判定：用错模型名会静默算错金额（不报错），必须逐条钉住。
func TestCostResolverBillingSourcePicksModel(t *testing.T) {
	prices := map[string]string{"requested-model": priceJSON, "redirected-model": priceJSON}
	ctx := context.Background()

	t.Run("redirected 基准取重定向后的模型", func(t *testing.T) {
		resolver, reader, _ := newTestResolver(t, "redirected", prices, nil)
		if cost := resolver.resolve(ctx, costInput{
			RequestedModel: "requested-model", RedirectedModel: "redirected-model", Usage: usageAt(1000, 500),
		}); cost == nil {
			t.Fatal("应计费，得到 nil")
		}
		if reader.calls != 1 {
			t.Fatalf("取价次数 = %d，期望 1（只该查重定向后的模型）", reader.calls)
		}
	})

	t.Run("original 基准取请求模型", func(t *testing.T) {
		resolver, reader, _ := newTestResolver(t, "original", prices, nil)
		if cost := resolver.resolve(ctx, costInput{
			RequestedModel: "requested-model", RedirectedModel: "redirected-model", Usage: usageAt(1000, 500),
		}); cost == nil {
			t.Fatal("应计费，得到 nil")
		}
		if reader.calls != 1 {
			t.Fatalf("取价次数 = %d，期望 1", reader.calls)
		}
	})

	t.Run("主模型无价格时回落备选", func(t *testing.T) {
		resolver, reader, _ := newTestResolver(t, "redirected",
			map[string]string{"requested-model": priceJSON}, nil)
		input := costInput{
			RequestedModel: "requested-model", RedirectedModel: "redirected-model", Usage: usageAt(1000, 500),
		}
		if cost := resolver.resolve(ctx, input); cost == nil {
			t.Fatal("主模型无价格时应回落备选模型并计费")
		}
		if reader.calls != 2 {
			t.Fatalf("取价次数 = %d，期望 2（主模型未命中 + 备选命中）", reader.calls)
		}
		// 第二次 resolve 必须命中缓存（包括「主模型无价格」这条结论），否则不存在的
		// 模型会变成每请求一次查询。
		if cost := resolver.resolve(ctx, input); cost == nil {
			t.Fatal("第二次 resolve 应同样计费")
		}
		if reader.calls != 2 {
			t.Fatalf("第二次 resolve 后取价次数 = %d，期望仍为 2（缓存生效）", reader.calls)
		}
	})
}

// TestCostResolverSkipsWhenNoUsageOrNoPrice 覆盖两种「不计费」分支。
//
// 它们都不能报错：计费失败把一条正常响应变成错误，是比少收钱更坏的故障。
func TestCostResolverSkipsWhenNoUsageOrNoPrice(t *testing.T) {
	ctx := context.Background()

	resolver, _, _ := newTestResolver(t, "redirected", map[string]string{"m": priceJSON}, nil)
	if cost := resolver.resolve(ctx, costInput{RequestedModel: "m", RedirectedModel: "m", Usage: terminal.Usage{}}); cost != nil {
		t.Fatalf("上游未报用量时不该计费，得到 %+v", cost)
	}

	resolver, _, _ = newTestResolver(t, "redirected", map[string]string{}, nil)
	if cost := resolver.resolve(ctx, costInput{RequestedModel: "m", RedirectedModel: "m", Usage: usageAt(1000, 500)}); cost != nil {
		t.Fatalf("价格表无该模型时不该计费，得到 %+v", cost)
	}

	resolver, _, _ = newTestResolver(t, "redirected", map[string]string{"m": `{"input_cost_per_token":`}, nil)
	if cost := resolver.resolve(ctx, costInput{RequestedModel: "m", RedirectedModel: "m", Usage: usageAt(1000, 500)}); cost != nil {
		t.Fatalf("价格数据损坏时不该计费，得到 %+v", cost)
	}
}

// TestCostResolverZeroUsageStillBills 钉住「零用量 ≠ 无用量」：零用量照常算出一笔 0 成本。
func TestCostResolverZeroUsageStillBills(t *testing.T) {
	resolver, _, _ := newTestResolver(t, "redirected", map[string]string{"m": priceJSON}, nil)
	cost := resolver.resolve(context.Background(), costInput{
		RequestedModel: "m", RedirectedModel: "m", Usage: usageAt(0, 0),
	})
	if cost == nil {
		t.Fatal("显式零用量应计费（金额 0），不能与「上游没报用量」混为一谈")
	}
	if cost.Total != "0" {
		t.Fatalf("零用量成本 = %q，期望 \"0\"", cost.Total)
	}
}

// TestCostResolverAppliesMultipliers 覆盖倍率：供应商倍率直接进总额，分组倍率由分组求交得到。
func TestCostResolverAppliesMultipliers(t *testing.T) {
	groups := map[string]float64{"vip": 2}
	resolver, _, groupReader := newTestResolver(t, "redirected", map[string]string{"m": priceJSON}, groups)
	providerMultiplier := 3.0

	cost := resolver.resolve(context.Background(), costInput{
		RequestedModel:     "m",
		RedirectedModel:    "m",
		Usage:              usageAt(1000, 500),
		ProviderMultiplier: &providerMultiplier,
		ProviderGroupTag:   "vip",
		UserGroup:          "vip",
	})
	if cost == nil {
		t.Fatal("应计费，得到 nil")
	}
	// 基础成本 = 1000×0.000001 + 500×0.000002 = 0.002，×3（供应商）×2（分组）= 0.012。
	if cost.Total != "0.012" {
		t.Fatalf("总额 = %q，期望 0.012（供应商倍率 3 × 分组倍率 2）", cost.Total)
	}
	var breakdown terminal.StoredCostBreakdown
	if err := json.Unmarshal(cost.Breakdown, &breakdown); err != nil {
		t.Fatalf("成本明细不是合法 JSON: %v", err)
	}
	if breakdown.ProviderMultiplier != 3 || breakdown.GroupMultiplier != 2 {
		t.Fatalf("明细倍率 = %v/%v，期望 3/2", breakdown.ProviderMultiplier, breakdown.GroupMultiplier)
	}
	if groupReader.calls == 0 {
		t.Fatal("分组倍率应查库一次")
	}
}

// TestBillingProviderGroups 钉住分组求交的三条口径（显式交集 / 通配符 / 无交集）。
func TestBillingProviderGroups(t *testing.T) {
	cases := []struct {
		name        string
		providerTag string
		userGroup   string
		want        []string
	}{
		{"显式交集保留用户声明顺序", "vip,prod", "prod,vip", []string{"prod", "vip"}},
		{"无交集且无通配符时为空", "vip", "prod", nil},
		{"用户通配符退化为供应商标签", "vip,prod", "all", []string{"vip", "prod"}},
		{"供应商无标签时按 default 组", "", "default", []string{"default"}},
		{"中文逗号与换行都是分隔符", "vip，prod\nbeta", "beta", []string{"beta"}},
		{"通配符不参与显式交集", "all,vip", "all,vip", []string{"vip"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := billingProviderGroups(testCase.providerTag, testCase.userGroup)
			if len(got) != len(testCase.want) {
				t.Fatalf("分组 = %v，期望 %v", got, testCase.want)
			}
			for index := range testCase.want {
				if got[index] != testCase.want[index] {
					t.Fatalf("分组 = %v，期望 %v", got, testCase.want)
				}
			}
		})
	}
}

// TestCostResolverGroupLookupFailureFallsBack 钉住取数失败按 1 计（不阻断计费、不报错）。
func TestCostResolverGroupLookupFailureFallsBack(t *testing.T) {
	resolver := newCostResolverWith(
		fakeCostSettings{source: "redirected"},
		&fakeCostPrices{byModel: map[string]string{"m": priceJSON}},
		&failingCostGroups{},
		nil,
	)
	cost := resolver.resolve(context.Background(), costInput{
		RequestedModel: "m", RedirectedModel: "m", Usage: usageAt(1000, 500),
		ProviderGroupTag: "vip", UserGroup: "vip",
	})
	if cost == nil {
		t.Fatal("分组取数失败不该阻断计费")
	}
	if cost.Total != "0.002" {
		t.Fatalf("总额 = %q，期望 0.002（分组倍率回落 1）", cost.Total)
	}
}

// failingCostGroups 让分组取数失败。
type failingCostGroups struct{}

func (f *failingCostGroups) FindGroupMultipliers(context.Context, []string) (map[string]float64, error) {
	return nil, errors.New("库不可用")
}
