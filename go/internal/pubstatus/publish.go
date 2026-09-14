package pubstatus

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是 Node src/lib/public-status/config-publisher.ts 的转写：把「分组 + 最新价格 + 设置」
// 压成快照并发布。它不碰库——数据由调用方（adminapi 的 PublicStatusPublisher）从 store 读好送进来，
// 这样本包不依赖 store，测试可以直接喂数据。
//
// 发布被触发的地方（照 Node）：
//   - system/settings PUT 且动了 siteTitle/timezone/publicStatusWindowHours/
//     publicStatusAggregationIntervalMinutes（actions/system-config.ts:243-277）；
//   - 公开状态设置保存（actions/public-status.ts，Go 侧未移植该动作）；
//   - Node 的 rebuild-worker 每次聚合完成后。

// GroupSource 是供应商分组的只读视图（Node 的 findAllProviderGroups 用到的三列）。
type GroupSource struct {
	ID          int64
	Name        string
	Description *string
}

// ModelPriceSource 是某个模型的最新价格里发布用到的三字段。
//
// 对应 Node 的 price.priceData.display_name / vendor / litellm_provider：后两者是
// 「同一个意思的两个历史字段名」，发布时优先 vendor，缺失才回落 litellm_provider。
type ModelPriceSource struct {
	ModelName       string
	DisplayName     string
	Vendor          string
	HasVendor       bool
	LitellmProvider string
	HasLitellm      bool
}

// SettingsSource 是发布用到的 system_settings 四字段。
type SettingsSource struct {
	SiteTitle                     string
	TimeZone                      *string
	PublicStatusWindowHours       int
	PublicStatusAggregationMinute int
}

// Source 是一次发布所需的全部输入。
type Source struct {
	Groups   []GroupSource
	Prices   []ModelPriceSource
	Settings SettingsSource
}

// BuildProjection 按 Node 的顺序构造两份快照，并返回启用的分组数。
func BuildProjection(source Source, configVersion string, generatedAt time.Time) (
	PublicStatusConfigSnapshot,
	InternalPublicStatusConfigSnapshot,
	int,
) {
	configured := make([]PublicStatusConfiguredGroupInput, 0, len(source.Groups))
	for _, group := range source.Groups {
		configured = append(configured, PublicStatusConfiguredGroupInput{
			GroupName:                     group.Name,
			ParsedPublicStatusDescription: ParsePublicStatusDescription(group.Description),
		})
	}
	enabled := CollectEnabledPublicStatusGroups(configured)

	pricesByName := make(map[string]ModelPriceSource, len(source.Prices))
	for _, price := range source.Prices {
		if _, exists := pricesByName[price.ModelName]; exists {
			continue
		}
		pricesByName[price.ModelName] = price
	}

	groupIDByName := make(map[string]int64, len(source.Groups))
	for _, group := range source.Groups {
		groupIDByName[group.Name] = group.ID
	}

	groups := make([]ConfigSnapshotGroup, 0, len(enabled))
	for _, group := range enabled {
		models := make([]PublicStatusModelSnapshot, 0, len(group.PublicModels))
		for _, model := range group.PublicModels {
			modelName := model.ModelKey
			label := modelName
			vendorIconKey := ""
			hasVendorIconKey := false
			if price, found := pricesByName[modelName]; found {
				if trimmed := strings.TrimSpace(price.DisplayName); trimmed != "" {
					label = trimmed
				}
				switch {
				case price.HasVendor:
					vendorIconKey, hasVendorIconKey = price.Vendor, true
				case price.HasLitellm:
					vendorIconKey, hasVendorIconKey = price.LitellmProvider, true
				}
			}
			if !hasVendorIconKey {
				vendorIconKey = ""
			}

			models = append(models, PublicStatusModelSnapshot{
				PublicModelKey: modelName,
				Label:          label,
				VendorIconKey: resolvePublicStatusVendorIconKey(
					modelName, vendorIconKey, model.ProviderTypeOverride),
				RequestTypeBadge: ResolveRequestTypeBadge(modelName, model.ProviderTypeOverride),
			})
		}

		sourceGroupID, hasGroupID := groupIDByName[group.GroupName]
		groupSnapshot := ConfigSnapshotGroup{
			SourceGroupName: group.GroupName,
			Slug:            group.PublicGroupSlug,
			DisplayName:     group.DisplayName,
			SortOrder:       group.SortOrder,
			Description:     group.ExplanatoryCopy,
			Models:          models,
		}
		if hasGroupID {
			groupID := sourceGroupID
			groupSnapshot.SourceGroupID = &groupID
		}
		groups = append(groups, groupSnapshot)
	}

	input := ConfigSnapshotInput{
		ConfigVersion:          configVersion,
		SiteTitle:              source.Settings.SiteTitle,
		TimeZone:               source.Settings.TimeZone,
		DefaultIntervalMinutes: NormalizePublicInterval(source.Settings.PublicStatusAggregationMinute),
		DefaultRangeHours:      NormalizePublicRange(source.Settings.PublicStatusWindowHours),
		Groups:                 groups,
	}
	return BuildConfigSnapshot(input, generatedAt), BuildInternalConfigSnapshot(input, generatedAt), len(enabled)
}

// PublishCurrentProjection 是「读好数据 -> 构造 -> 写入」的一站式入口。
//
// configVersion 传空时铸一个新版本（`cfg-<毫秒>`，与 Node 的 `cfg-${Date.now()}` 同形）。
func PublishCurrentProjection(
	ctx context.Context,
	client redis.UniversalClient,
	source Source,
	configVersion string,
	now time.Time,
) (PublishResult, error) {
	if configVersion == "" {
		configVersion = "cfg-" + strconv.FormatInt(now.UnixMilli(), 10)
	}
	public, internal, groupCount := BuildProjection(source, configVersion, now)
	result, err := PublishConfigProjection(ctx, client, public, internal)
	if err != nil {
		return result, err
	}
	result.GroupCount = groupCount
	return result, nil
}
