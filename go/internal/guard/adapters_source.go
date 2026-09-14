package guard

import (
	"context"
	"fmt"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// cachedSource 给 route.Source 套上快照缓存。
//
// 为什么必须有这一层：route.StoreSource 三个方法每次调用都打库（它只做读写分离与排序，
// 把缓存明确留给了调用方——见 route.Source 的接口注释）。而选路在**每请求热路径**上，
// 直接接 StoreSource 就是每请求 2~3 条查询：正是 Node 侧修过的性能回归
// （provider-cache.ts / provider-endpoint-cache.ts 的存在理由）。
//
// 缓存形制与两个 Node 缓存一一对应：
//   - providers 快照是「单值 + TTL」（provider-cache.ts）→ ValueCache。
//   - 厂级端点是「按 vendorId:providerType 分键 + TTL + 逐键 in-flight 合并」
//     （provider-endpoint-cache.ts）→ KeyedCache。
//
// 两者都绑同一条失效通道（providers 的增删改会同时影响两份缓存）。
type cachedSource struct {
	source    route.Source
	providers *cfgsync.ValueCache[[]route.Provider]
	endpoints *cfgsync.KeyedCache[route.Endpoint]
	registry  *cfgsync.Registry
	loads     counter
}

// newCachedSource 包一层快照缓存。
func newCachedSource(source route.Source, registry *cfgsync.Registry) *cachedSource {
	providersTTL := cfgsync.Spec(cfgsync.DomainProviders).TTL
	endpointsTTL := cfgsync.Spec(cfgsync.DomainProviderEndpoints).TTL
	return &cachedSource{
		source:    source,
		providers: cfgsync.NewValueCache[[]route.Provider](providersTTL),
		endpoints: cfgsync.NewKeyedCache[route.Endpoint](endpointsTTL, providerGroupTagCacheSize),
		registry:  registry,
	}
}

// Providers 返回启用态供应商快照。
func (c *cachedSource) Providers(ctx context.Context) ([]route.Provider, error) {
	return c.providers.Get(ctx, func(ctx context.Context) ([]route.Provider, error) {
		loaded, err := c.source.Providers(ctx)
		if err != nil {
			return nil, err
		}
		c.loads.add()
		markLoaded(c.registry, cfgsync.DomainProviders)
		return loaded, nil
	}, nil)
}

// Provider 取单个供应商。
//
// 先查快照（同一个请求里亲和提名会按 id 反复校验，不应变成 N 次查询）；快照里没有
// 说明它已被禁用或删除，此时退回一次直读，让「不存在」这一结论仍然真实。
func (c *cachedSource) Provider(ctx context.Context, id int64) (*route.Provider, error) {
	providers, err := c.Providers(ctx)
	if err != nil {
		return nil, err
	}
	for index := range providers {
		if providers[index].ID == id {
			provider := providers[index]
			return &provider, nil
		}
	}
	return c.source.Provider(ctx, id)
}

// Endpoints 返回某厂与类型下的启用态端点。
func (c *cachedSource) Endpoints(
	ctx context.Context,
	vendorID int64,
	providerType convert.ProviderType,
) ([]route.Endpoint, error) {
	key := fmt.Sprintf("%d:%s", vendorID, providerType)
	return c.endpoints.Get(ctx, key, func(ctx context.Context) ([]route.Endpoint, error) {
		loaded, err := c.source.Endpoints(ctx, vendorID, providerType)
		if err != nil {
			return nil, err
		}
		c.loads.add()
		markLoaded(c.registry, cfgsync.DomainProviderEndpoints)
		return loaded, nil
	})
}

// Invalidate 清空两份快照（providers 与 provider_endpoints 共用一条失效通道）。
func (c *cachedSource) Invalidate() {
	c.providers.Invalidate()
	c.endpoints.Invalidate()
}

// Loads 报告供应商与端点快照的装载总次数（观测与「不每请求查库」断言用）。
func (c *cachedSource) Loads() int64 { return c.loads.get() }
