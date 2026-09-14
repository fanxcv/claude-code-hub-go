package dataplane

import (
	"context"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件把 Node 的「取价 → 计费」入口接到终态结算上
// （session.getResolvedPricingByBillingSource + response-handler 的 calculateRequestCost）。
//
// 计费基准是**模型名**，而模型名的来源有两个：客户端请求的原始名，与供应商重定向规则
// 改写的目标名。用哪一个由 system_settings.billing_model_source 决定；主模型查不到价格
// 时回落另一个。重定向发生在 forward.BuildPlan 内部，故终态结算必须拿到重定向事实
// （非流式走 Result.Plan，流式走 StreamOutcome.Plan）。

// 出厂常量。
const (
	// billingSourceKey 是设置缓存的键：system_settings 是单行表，键恒为 0。
	billingSourceKey = 0
	// priceCacheSize / groupCacheSize 是缓存容量；取值与热路径的可能基数同阶（模型名、
	// 用户分组组合），超出后按 LRU 淘汰最旧的一批。
	priceCacheSize = 4096
	groupCacheSize = 1024
	// priceCacheTTL 是价格的进程内自愈上界。
	//
	// model_prices 没有 cfgsync 失效域（管理面改价不会广播），故只能靠 TTL：代价是最多
	// 陈旧一个 TTL 的价格，收益是热路径不出现「每个请求一次价格查询」。
	priceCacheTTL = 60 * time.Second
	// groupAll 是「全部分组」通配符，对应 Node 的 PROVIDER_GROUP.ALL。
	groupAll = "all"
	// billingSourceOriginal / billingSourceRedirected 是 billing_model_source 的两个取值。
	billingSourceOriginal = "original"
)

// priceSlot 是价格缓存的值。
//
// 用结构体而不是裸 []byte 是刻意的：nil 切片与「空价格」在缓存里必须可区分，
// 否则「这个模型没有价格」会被反复查库（那正是缓存要避免的）。
type priceSlot struct {
	data []byte
}

// priceReader 与 groupReader 是计费所需的两条只读取数缝。
//
// 用接口而不是直接持有 *store.Pools：计费的口径（取价基准、倍率求交、回落）全是纯判定，
// 必须能不开数据库就断言；store 的两个读恰好各自满足一个接口，生产装配零额外代码。
type priceReader interface {
	FindModelPrice(ctx context.Context, modelName string) (*store.ModelPrice, error)
}

type groupReader interface {
	FindGroupMultipliers(ctx context.Context, names []string) (map[string]float64, error)
}

// costResolver 取价并计算一次请求的成本。
type costResolver struct {
	settings guard.SettingsSource
	prices   priceReader
	groups   groupReader
	logger   *logx.Logger
	source   *cfgsync.TTLMap[int, string]
	priceTTL *cfgsync.TTLMap[string, priceSlot]
	groupTTL *cfgsync.TTLMap[string, float64]
}

// newCostResolver 建计费解析器。pools 或 settings 为空时返回 nil，调用方据此跳过计费。
func newCostResolver(settings guard.SettingsSource, pools *store.Pools, logger *logx.Logger) *costResolver {
	if settings == nil || pools == nil {
		return nil
	}
	return newCostResolverWith(settings, pools, pools, logger)
}

// newCostResolverWith 供测试注入取数缝。
func newCostResolverWith(
	settings guard.SettingsSource,
	prices priceReader,
	groups groupReader,
	logger *logx.Logger,
) *costResolver {
	if logger == nil {
		logger = logx.New(nil)
	}
	return &costResolver{
		settings: settings,
		prices:   prices,
		groups:   groups,
		logger:   logger,
		source:   cfgsync.NewTTLMap[int, string](cfgsync.Spec(cfgsync.DomainSystemSettings).TTL, 1),
		priceTTL: cfgsync.NewTTLMap[string, priceSlot](priceCacheTTL, priceCacheSize),
		groupTTL: cfgsync.NewTTLMap[string, float64](cfgsync.Spec(cfgsync.DomainProviderGroups).TTL, groupCacheSize),
	}
}

// costInput 是一次计费所需的请求级事实。
type costInput struct {
	// RequestedModel 是客户端请求里的模型名。
	RequestedModel string
	// RedirectedModel 是供应商重定向后的模型名；未重定向时与 RequestedModel 相同。
	RedirectedModel string
	Usage           terminal.Usage
	// ProviderMultiplier 是供应商级的成本倍率；nil 表示按 1 计。
	ProviderMultiplier *float64
	// ProviderGroupTag 是选中供应商的 group_tag（providers.group_tag）。
	ProviderGroupTag string
	// UserGroup 是本次请求的有效分组（密钥级 > 用户级 > 默认）。
	UserGroup string
}

// resolve 取价并计费。
//
// 返回 nil 成本表示**本次不计费**：用量为空（上游没报）、价格表里查不到主备两个模型、
// 或价格数据不可用。三种情况都与 Node 一致——静默跳过，不写 cost_usd 也不报错。
func (r *costResolver) resolve(ctx context.Context, in costInput) *terminal.Cost {
	if r == nil || usageEmpty(in.Usage) {
		return nil
	}
	model := r.billingModel(ctx, in)
	if model == "" {
		return nil
	}
	price, ok := r.priceFor(ctx, model)
	if !ok {
		// 主模型无价格：回落另一个模型名（Node 的 fallback 分支）。
		fallback := in.RedirectedModel
		if model == in.RedirectedModel {
			fallback = in.RequestedModel
		}
		if fallback == "" || fallback == model {
			return nil
		}
		price, ok = r.priceFor(ctx, fallback)
		if !ok {
			return nil
		}
	}
	cost, err := terminal.ComputeCost(terminal.CostInput{
		Usage:              in.Usage,
		PriceData:          price,
		ProviderMultiplier: in.ProviderMultiplier,
		GroupMultiplier:    r.groupMultiplier(ctx, in.ProviderGroupTag, in.UserGroup),
	})
	if err != nil {
		// 价格数据坏了按「无价格」处理：计费不能把一条正常响应变成错误。
		r.logger.Debug("dataplane.cost_compute_failed", map[string]any{"model": model, "error": err.Error()})
		return nil
	}
	return &cost
}

// billingModel 按 billing_model_source 选出主计费模型名。
//
// 设置读不到时按「重定向后」处理：Node 的判定是 `cachedBillingModelSource === "original"`，
// 即非 original 一律走重定向后模型，故读失败与默认值同路，不会分叉。
func (r *costResolver) billingModel(ctx context.Context, in costInput) string {
	if r.billingSource(ctx) == billingSourceOriginal {
		if in.RequestedModel != "" {
			return in.RequestedModel
		}
		return in.RedirectedModel
	}
	if in.RedirectedModel != "" {
		return in.RedirectedModel
	}
	return in.RequestedModel
}

// billingSource 读 system_settings.billing_model_source（TTL 缓存）。
//
// 读失败也写缓存：否则一次数据库抖动会把热路径变成「每请求一次设置查询」。代价是最多
// 陈旧一个 TTL 的取价基准，且失败有日志可查。
func (r *costResolver) billingSource(ctx context.Context) string {
	if value, ok := r.source.Get(billingSourceKey); ok {
		return value
	}
	source := ""
	settings, err := r.settings.FindSystemSettings(ctx)
	if err != nil {
		r.logger.Warn("dataplane.billing_source_lookup_failed", map[string]any{"error": err.Error()})
	} else if settings != nil {
		source = settings.BillingModelSource
	}
	r.source.Set(billingSourceKey, source)
	return source
}

// priceFor 取某模型的价格数据（TTL 缓存）。返回 false 表示价格表里没有该模型。
func (r *costResolver) priceFor(ctx context.Context, model string) ([]byte, bool) {
	if model == "" {
		return nil, false
	}
	if slot, ok := r.priceTTL.Get(model); ok {
		return slot.data, slot.data != nil
	}
	price, err := r.prices.FindModelPrice(ctx, model)
	if err != nil {
		r.logger.Warn("dataplane.model_price_lookup_failed", map[string]any{"model": model, "error": err.Error()})
		return nil, false
	}
	if price == nil || len(price.PriceData) == 0 {
		// 缓存「查不到」：否则不存在的模型会每请求打一次库。
		r.priceTTL.Set(model, priceSlot{})
		return nil, false
	}
	r.priceTTL.Set(model, priceSlot{data: price.PriceData})
	return price.PriceData, true
}

// groupMultiplier 解析用户分组的成本倍率（复刻 getGroupCostMultiplier 的取数口径）。
//
// 取数只在「供应商分组 ∩ 用户分组」上做：没有交集就没有计费分组，倍率按 1 计
// （Node 同样回落 1.0 并留一条 warn）。命中顺序按用户声明的顺序取首个**存在**的分组，
// 因为倍率可能不是 1，不能把「没这个分组」和「倍率是 0」混为一谈。
func (r *costResolver) groupMultiplier(ctx context.Context, providerTag, userGroup string) *float64 {
	names := billingProviderGroups(providerTag, userGroup)
	if len(names) == 0 {
		return nil
	}
	key := strings.Join(names, ",")
	if value, ok := r.groupTTL.Get(key); ok {
		multiplier := value
		return &multiplier
	}
	multipliers, err := r.groups.FindGroupMultipliers(ctx, names)
	if err != nil {
		r.logger.Warn("dataplane.group_multiplier_lookup_failed", map[string]any{"groups": key, "error": err.Error()})
		return nil
	}
	for _, name := range names {
		if value, ok := multipliers[name]; ok {
			// 只缓存真命中：缓存未命中会把新建分组的可见性推迟一个 TTL。
			r.groupTTL.Set(key, value)
			multiplier := value
			return &multiplier
		}
	}
	return nil
}

// billingProviderGroups 复刻 resolveBillingProviderGroups：用户分组 ∩ 供应商分组标签，
// 保留用户的声明顺序；用户声明了通配符 all 时，退化为供应商自己的标签集合。
func billingProviderGroups(providerTag, userGroup string) []string {
	providerGroups := splitGroupList(providerTag)
	if len(providerGroups) == 0 {
		providerGroups = []string{route.GroupDefault}
	}
	providerSet := make(map[string]struct{}, len(providerGroups))
	for _, group := range providerGroups {
		providerSet[group] = struct{}{}
	}

	userGroups := splitGroupList(userGroup)
	explicit := make([]string, 0, len(userGroups))
	for _, group := range userGroups {
		if group == groupAll {
			continue
		}
		if _, ok := providerSet[group]; ok {
			explicit = append(explicit, group)
		}
	}
	if len(explicit) > 0 {
		return explicit
	}
	for _, group := range userGroups {
		if group == groupAll {
			return providerGroups
		}
	}
	return nil
}

// splitGroupList 按 Node 的分隔符集合拆分分组串（英文逗号、中文逗号、换行）。
func splitGroupList(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// usageEmpty 报告上游是否压根没报用量。
//
// 全 nil 与「全 0」不同：前者是「上游没给」，后者是「真的零用量」——前者不计费（也无法
// 计费），后者照常按 0 算出一笔 0 成本并落库，这与 Node 的 `usage == null` 判定一致。
func usageEmpty(usage terminal.Usage) bool {
	return usage.InputTokens == nil &&
		usage.OutputTokens == nil &&
		usage.CacheCreationInputTokens == nil &&
		usage.CacheCreation5mInputTokens == nil &&
		usage.CacheCreation1hInputTokens == nil &&
		usage.CacheReadInputTokens == nil
}

// costMultiplierOf 取一次选路结果的供应商成本倍率。
//
// 未选中供应商（Result.Provider 为 nil）时不写倍率：nil 在计费里表示「按 1 计」，
// 而 0 是一个合法的免费倍率，两者绝不能混。
func costMultiplierOf(result route.Result) *float64 {
	if result.Provider == nil {
		return nil
	}
	multiplier := result.Provider.CostMultiplierFloat()
	return &multiplier
}
