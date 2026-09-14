package dataplane

import (
	"context"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// StoreModelCatalog 用存储层的只读行视图实现 ModelCatalog。
//
// 过滤四条与 Node 的 getAvailableModelsByProviderTypes（available-models.ts:437-470）逐条对齐：
// 启用态（SQL 侧已滤）→ 类型命中 → 活动时段（按**系统时区**）→ 分组命中
// （密钥分组 > 用户分组；分组为空则不过滤）。
//
// 装配点：`assemble.go` 构造 dataplane.Options 处加一行
// `ModelCatalog: dataplane.StoreModelCatalog{Pools: pools}`。未装配时这五条聚合端点
// **不注册**（回退 Node），见 dataplane.go 的 Options.ModelCatalog 注释。
type StoreModelCatalog struct {
	// Pools 是只读分道；必填。
	Pools *store.Pools
}

// ProvidersForModelList 实现 ModelCatalog。
func (c StoreModelCatalog) ProvidersForModelList(
	ctx context.Context,
	providerTypes []string,
	keyID int64,
	userID int64,
) ([]ModelProvider, error) {
	rows, err := c.Pools.ModelListProviderRows(ctx)
	if err != nil {
		return nil, err
	}
	group, err := c.Pools.ModelListEffectiveGroup(ctx, keyID, userID)
	if err != nil {
		return nil, err
	}
	location := modelsLocation(c.Pools.AdminSystemTimezoneOrUTC(ctx))
	now := time.Now().In(location)

	wanted := make(map[string]bool, len(providerTypes))
	for _, providerType := range providerTypes {
		wanted[providerType] = true
	}

	out := make([]ModelProvider, 0, len(rows))
	for _, row := range rows {
		if len(wanted) > 0 && !wanted[row.ProviderType] {
			continue
		}
		if !route.ProviderActiveNow(row.ActiveTimeStart, row.ActiveTimeEnd, now) {
			continue
		}
		if group != "" && !route.ProviderGroupMatches(row.GroupTag, group) {
			continue
		}
		out = append(out, ModelProvider{
			ID:                           row.ID,
			Name:                         row.Name,
			Type:                         row.ProviderType,
			URL:                          row.URL,
			Key:                          row.Key,
			AllowedModels:                row.AllowedModels,
			RequestTimeoutNonStreamingMS: row.RequestTimeoutNonStreamingMS,
		})
	}
	return out, nil
}

// modelsLocation 把时区名解析成 *time.Location；非法名按 UTC（活动时段判定不该被一个设置值拖垮）。
func modelsLocation(name string) *time.Location {
	if strings.TrimSpace(name) == "" {
		return time.UTC
	}
	location, err := time.LoadLocation(strings.TrimSpace(name))
	if err != nil {
		return time.UTC
	}
	return location
}
