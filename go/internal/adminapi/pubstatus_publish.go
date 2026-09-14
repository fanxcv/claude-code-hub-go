package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件把 pubstatus（Node 的 public-status 配置投影发布面）接到管理面：读库 → 建快照 → 写 Redis。
//
// 装配纪律与其它可选依赖一致：Redis 命令连接拿不到时 Deps.PublicStatusPublisher 为 nil，
// 调用方（system/settings PUT）**如实回答警告码**，而不是假装发布成功。
type PublicStatusPublisher interface {
	// PublishCurrentProjection 重建并发布当前配置投影，返回发布结果（written=false 表示
	// 版本化键写失败或指针被更新的版本占住）。
	PublishCurrentProjection(ctx context.Context, reason string) (pubstatus.PublishResult, error)
}

// redisPublicStatusPublisher 是 PublicStatusPublisher 的默认实现。
type redisPublicStatusPublisher struct {
	pools  *store.Pools
	client redis.UniversalClient
	logger *logx.Logger
}

// NewPublicStatusPublisher 建发布器；client 为 nil 时返回 nil（调用方据此走降级分支）。
func NewPublicStatusPublisher(
	pools *store.Pools,
	client redis.UniversalClient,
	logger *logx.Logger,
) PublicStatusPublisher {
	if pools == nil || client == nil {
		return nil
	}
	if logger == nil {
		logger = logx.New(nil)
	}
	return &redisPublicStatusPublisher{pools: pools, client: client, logger: logger}
}

// PublishCurrentProjection 复刻 config-publisher.ts:58-178 的数据读取顺序。
func (p *redisPublicStatusPublisher) PublishCurrentProjection(
	ctx context.Context,
	reason string,
) (pubstatus.PublishResult, error) {
	groups, err := p.pools.AdminListProviderGroups(ctx)
	if err != nil {
		return pubstatus.PublishResult{}, fmt.Errorf("管理面: 读供应商分组失败: %w", err)
	}
	prices, err := p.pools.AdminListLatestModelPrices(ctx)
	if err != nil {
		return pubstatus.PublishResult{}, fmt.Errorf("管理面: 读最新模型价格失败: %w", err)
	}
	settingsRow, err := p.pools.EnsureAdminSystemSettings(ctx)
	if err != nil {
		return pubstatus.PublishResult{}, fmt.Errorf("管理面: 读系统设置失败: %w", err)
	}

	settings := buildSystemSettingsBody(settingsRow, time.Now())
	source := pubstatus.Source{
		Groups: make([]pubstatus.GroupSource, 0, len(groups)),
		Prices: make([]pubstatus.ModelPriceSource, 0, len(prices)),
		Settings: pubstatus.SettingsSource{
			SiteTitle:                     settings.SiteTitle,
			TimeZone:                      settings.Timezone,
			PublicStatusWindowHours:       settings.PublicStatusWindowHours,
			PublicStatusAggregationMinute: settings.PublicStatusAggregationMins,
		},
	}
	for _, group := range groups {
		source.Groups = append(source.Groups, pubstatus.GroupSource{
			ID: group.ID, Name: group.Name, Description: group.Description,
		})
	}
	for _, price := range prices {
		source.Prices = append(source.Prices, pubStatusPriceSource(price))
	}

	result, err := pubstatus.PublishCurrentProjection(ctx, p.client, source, "", time.Now())
	if err != nil {
		return result, err
	}
	p.logger.Info("admin_public_status_projection_published", map[string]any{
		"reason":        reason,
		"configVersion": result.ConfigVersion,
		"groupCount":    result.GroupCount,
		"written":       result.Written,
	})
	return result, nil
}

// pubStatusPriceSource 把价格行的 priceData 投影成发布要用的三字段。
//
// vendor 与 litellm_provider 是同一含义的两个历史字段名；Node 用 `typeof === "string"` 判
// 「存在」（空串也算存在），故这里也带 Has 标志而不是用空串判无。
func pubStatusPriceSource(price store.AdminModelPrice) pubstatus.ModelPriceSource {
	source := pubstatus.ModelPriceSource{ModelName: price.ModelName}
	if len(price.PriceData) == 0 {
		return source
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(price.PriceData, &data); err != nil {
		return source
	}
	if raw, exists := data["display_name"]; exists {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			source.DisplayName = text
		}
	}
	if raw, exists := data["vendor"]; exists {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			source.Vendor, source.HasVendor = text, true
		}
	}
	if raw, exists := data["litellm_provider"]; exists {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			source.LitellmProvider, source.HasLitellm = text, true
		}
	}
	return source
}
