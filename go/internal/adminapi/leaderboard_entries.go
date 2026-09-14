package adminapi

import (
	"context"
	"sort"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是排行榜五个 scope 的**作答条目**与组装逻辑（对应 route.ts 的格式化收尾）。
//
// 键序与存在性逐条对齐 Node 的对象字面量与条件展开：
//   - `modelStats` 只在被请求时出现（provider 面的 includeModelStats、user 面的 includeUserModelStats）；
//     providerCacheHitRate 面**恒有** modelStats。用两组结构体区分「有/无该键」，避免空切片被 omitempty 吞掉。
//   - `successRateUnavailableReason` 只在 successRate 为 null 时出现。
//   - `*Formatted` 字段是 route.ts 追加的，可空值对应 null。

// leaderboardUserModelStat 是 user 面的按模型拆分（Node 的 UserModelStat + totalCostFormatted）。
type leaderboardUserModelStat struct {
	Model              *string `json:"model"`
	TotalRequests      float64 `json:"totalRequests"`
	TotalCost          float64 `json:"totalCost"`
	TotalTokens        float64 `json:"totalTokens"`
	TotalCostFormatted string  `json:"totalCostFormatted"`
}

// leaderboardUserModelCacheStat 是 userCacheHitRate 面的按模型拆分。
//
// 注意它**没有** totalCost，故 Node 的 `"totalCost" in stat` 为假、不带格式化字段。
type leaderboardUserModelCacheStat struct {
	Model            *string `json:"model"`
	TotalRequests    float64 `json:"totalRequests"`
	CacheReadTokens  float64 `json:"cacheReadTokens"`
	TotalInputTokens float64 `json:"totalInputTokens"`
	CacheHitRate     float64 `json:"cacheHitRate"`
}

// leaderboardUserEntry 是 user scope 的一行（不带模型拆分）。
type leaderboardUserEntry struct {
	UserID             int64   `json:"userId"`
	UserName           string  `json:"userName"`
	TotalRequests      float64 `json:"totalRequests"`
	TotalCost          float64 `json:"totalCost"`
	TotalTokens        float64 `json:"totalTokens"`
	TotalCostFormatted string  `json:"totalCostFormatted"`
}

// leaderboardUserEntryWithModels 是 user scope 的一行（带模型拆分）。
type leaderboardUserEntryWithModels struct {
	UserID             int64                      `json:"userId"`
	UserName           string                     `json:"userName"`
	TotalRequests      float64                    `json:"totalRequests"`
	TotalCost          float64                    `json:"totalCost"`
	TotalTokens        float64                    `json:"totalTokens"`
	ModelStats         []leaderboardUserModelStat `json:"modelStats"`
	TotalCostFormatted string                     `json:"totalCostFormatted"`
}

// leaderboardUserCacheEntry 是 userCacheHitRate scope 的一行（不带模型拆分）。
type leaderboardUserCacheEntry struct {
	UserID                     int64   `json:"userId"`
	UserName                   string  `json:"userName"`
	TotalRequests              float64 `json:"totalRequests"`
	TotalCost                  float64 `json:"totalCost"`
	CacheReadTokens            float64 `json:"cacheReadTokens"`
	CacheCreationCost          float64 `json:"cacheCreationCost"`
	TotalInputTokens           float64 `json:"totalInputTokens"`
	TotalTokens                float64 `json:"totalTokens"`
	CacheHitRate               float64 `json:"cacheHitRate"`
	TotalCostFormatted         string  `json:"totalCostFormatted"`
	CacheCreationCostFormatted string  `json:"cacheCreationCostFormatted"`
}

// leaderboardUserCacheEntryWithModels 是 userCacheHitRate scope 的一行（带模型拆分）。
type leaderboardUserCacheEntryWithModels struct {
	UserID                     int64                           `json:"userId"`
	UserName                   string                          `json:"userName"`
	TotalRequests              float64                         `json:"totalRequests"`
	TotalCost                  float64                         `json:"totalCost"`
	CacheReadTokens            float64                         `json:"cacheReadTokens"`
	CacheCreationCost          float64                         `json:"cacheCreationCost"`
	TotalInputTokens           float64                         `json:"totalInputTokens"`
	TotalTokens                float64                         `json:"totalTokens"`
	CacheHitRate               float64                         `json:"cacheHitRate"`
	ModelStats                 []leaderboardUserModelCacheStat `json:"modelStats"`
	TotalCostFormatted         string                          `json:"totalCostFormatted"`
	CacheCreationCostFormatted string                          `json:"cacheCreationCostFormatted"`
}

// leaderboardProviderModelStat 是 provider 面的按模型拆分（Node 的 ModelProviderStat + 格式化字段）。
type leaderboardProviderModelStat struct {
	Model                            string   `json:"model"`
	TotalRequests                    float64  `json:"totalRequests"`
	TotalCost                        float64  `json:"totalCost"`
	TotalTokens                      float64  `json:"totalTokens"`
	SuccessRate                      *float64 `json:"successRate"`
	AvgTtftMS                        float64  `json:"avgTtftMs"`
	AvgTokensPerSecond               float64  `json:"avgTokensPerSecond"`
	RowIdentityBasis                 string   `json:"rowIdentityBasis"`
	SuccessRateBasis                 string   `json:"successRateBasis"`
	CostTokensBasis                  string   `json:"costTokensBasis"`
	BasisDisclosureRequired          bool     `json:"basisDisclosureRequired"`
	SuccessRateUnavailableReason     *string  `json:"successRateUnavailableReason,omitempty"`
	CacheCoefficientBP               *int64   `json:"cacheCoefficientBp"`
	AvgCostPerRequest                *float64 `json:"avgCostPerRequest"`
	AvgCostPerMillionTokens          *float64 `json:"avgCostPerMillionTokens"`
	TotalCostFormatted               string   `json:"totalCostFormatted"`
	AvgCostPerRequestFormatted       *string  `json:"avgCostPerRequestFormatted"`
	AvgCostPerMillionTokensFormatted *string  `json:"avgCostPerMillionTokensFormatted"`
}

// leaderboardProviderEntry 是 provider scope 的一行（不带模型拆分）。
type leaderboardProviderEntry struct {
	ProviderID                       int64    `json:"providerId"`
	ProviderName                     string   `json:"providerName"`
	TotalRequests                    float64  `json:"totalRequests"`
	TotalCost                        float64  `json:"totalCost"`
	TotalTokens                      float64  `json:"totalTokens"`
	SuccessRate                      *float64 `json:"successRate"`
	AvgTtftMS                        float64  `json:"avgTtftMs"`
	AvgTokensPerSecond               float64  `json:"avgTokensPerSecond"`
	CacheCoefficientBP               *int64   `json:"cacheCoefficientBp"`
	AvgCostPerRequest                *float64 `json:"avgCostPerRequest"`
	AvgCostPerMillionTokens          *float64 `json:"avgCostPerMillionTokens"`
	TotalCostFormatted               string   `json:"totalCostFormatted"`
	AvgCostPerRequestFormatted       *string  `json:"avgCostPerRequestFormatted"`
	AvgCostPerMillionTokensFormatted *string  `json:"avgCostPerMillionTokensFormatted"`
}

// leaderboardProviderEntryWithModels 是 provider scope 的一行（带模型拆分）。
type leaderboardProviderEntryWithModels struct {
	ProviderID                       int64                          `json:"providerId"`
	ProviderName                     string                         `json:"providerName"`
	TotalRequests                    float64                        `json:"totalRequests"`
	TotalCost                        float64                        `json:"totalCost"`
	TotalTokens                      float64                        `json:"totalTokens"`
	SuccessRate                      *float64                       `json:"successRate"`
	AvgTtftMS                        float64                        `json:"avgTtftMs"`
	AvgTokensPerSecond               float64                        `json:"avgTokensPerSecond"`
	CacheCoefficientBP               *int64                         `json:"cacheCoefficientBp"`
	AvgCostPerRequest                *float64                       `json:"avgCostPerRequest"`
	AvgCostPerMillionTokens          *float64                       `json:"avgCostPerMillionTokens"`
	ModelStats                       []leaderboardProviderModelStat `json:"modelStats"`
	TotalCostFormatted               string                         `json:"totalCostFormatted"`
	AvgCostPerRequestFormatted       *string                        `json:"avgCostPerRequestFormatted"`
	AvgCostPerMillionTokensFormatted *string                        `json:"avgCostPerMillionTokensFormatted"`
}

// leaderboardProviderCacheModelStat 是 providerCacheHitRate 面的按模型拆分。
type leaderboardProviderCacheModelStat struct {
	Model              string  `json:"model"`
	TotalRequests      float64 `json:"totalRequests"`
	CacheReadTokens    float64 `json:"cacheReadTokens"`
	TotalInputTokens   float64 `json:"totalInputTokens"`
	CacheHitRate       float64 `json:"cacheHitRate"`
	CacheCoefficientBP *int64  `json:"cacheCoefficientBp"`
}

// leaderboardProviderCacheEntry 是 providerCacheHitRate scope 的一行（modelStats 恒在）。
type leaderboardProviderCacheEntry struct {
	ProviderID                 int64                               `json:"providerId"`
	ProviderName               string                              `json:"providerName"`
	TotalRequests              float64                             `json:"totalRequests"`
	TotalCost                  float64                             `json:"totalCost"`
	CacheReadTokens            float64                             `json:"cacheReadTokens"`
	CacheCreationCost          float64                             `json:"cacheCreationCost"`
	TotalInputTokens           float64                             `json:"totalInputTokens"`
	TotalTokens                float64                             `json:"totalTokens"`
	CacheHitRate               float64                             `json:"cacheHitRate"`
	CacheCoefficientBP         *int64                              `json:"cacheCoefficientBp"`
	ModelStats                 []leaderboardProviderCacheModelStat `json:"modelStats"`
	TotalCostFormatted         string                              `json:"totalCostFormatted"`
	CacheCreationCostFormatted string                              `json:"cacheCreationCostFormatted"`
}

// leaderboardModelEntry 是 model scope 的一行。
type leaderboardModelEntry struct {
	Model                        string   `json:"model"`
	TotalRequests                float64  `json:"totalRequests"`
	TotalCost                    float64  `json:"totalCost"`
	TotalTokens                  float64  `json:"totalTokens"`
	SuccessRate                  *float64 `json:"successRate"`
	RowIdentityBasis             string   `json:"rowIdentityBasis"`
	SuccessRateBasis             string   `json:"successRateBasis"`
	CostTokensBasis              string   `json:"costTokensBasis"`
	BasisDisclosureRequired      bool     `json:"basisDisclosureRequired"`
	SuccessRateUnavailableReason *string  `json:"successRateUnavailableReason,omitempty"`
	TotalCostFormatted           string   `json:"totalCostFormatted"`
}

// userLeaderboard 组装 user scope 的作答。
func (api *leaderboardAPI) userLeaderboard(
	ctx context.Context,
	query store.AdminLeaderboardQuery,
	currency string,
	includeModelStats bool,
) (any, error) {
	rows, err := api.pools.AdminUserLeaderboard(ctx, query)
	if err != nil {
		return nil, err
	}
	if !includeModelStats {
		entries := make([]leaderboardUserEntry, 0, len(rows))
		for _, row := range rows {
			totalCost := strconvFloatText(row.TotalCostText)
			entries = append(entries, leaderboardUserEntry{
				UserID:             row.UserID,
				UserName:           row.UserName,
				TotalRequests:      row.TotalRequests,
				TotalCost:          totalCost,
				TotalTokens:        row.TotalTokens,
				TotalCostFormatted: leaderboardFormatCurrency(totalCost, currency),
			})
		}
		return entries, nil
	}

	modelRows, err := api.pools.AdminUserModelLeaderboard(ctx, query)
	if err != nil {
		return nil, err
	}
	modelsByUser := make(map[int64][]leaderboardUserModelStat, len(modelRows))
	for _, row := range modelRows {
		totalCost := strconvFloatText(row.TotalCostText)
		modelsByUser[row.UserID] = append(modelsByUser[row.UserID], leaderboardUserModelStat{
			Model:              row.Model,
			TotalRequests:      row.TotalRequests,
			TotalCost:          totalCost,
			TotalTokens:        row.TotalTokens,
			TotalCostFormatted: leaderboardFormatCurrency(totalCost, currency),
		})
	}
	entries := make([]leaderboardUserEntryWithModels, 0, len(rows))
	for _, row := range rows {
		totalCost := strconvFloatText(row.TotalCostText)
		models := modelsByUser[row.UserID]
		if models == nil {
			models = []leaderboardUserModelStat{}
		}
		entries = append(entries, leaderboardUserEntryWithModels{
			UserID:             row.UserID,
			UserName:           row.UserName,
			TotalRequests:      row.TotalRequests,
			TotalCost:          totalCost,
			TotalTokens:        row.TotalTokens,
			ModelStats:         models,
			TotalCostFormatted: leaderboardFormatCurrency(totalCost, currency),
		})
	}
	return entries, nil
}

// userCacheLeaderboard 组装 userCacheHitRate scope 的作答。
func (api *leaderboardAPI) userCacheLeaderboard(
	ctx context.Context,
	query store.AdminLeaderboardQuery,
	currency string,
	includeModelStats bool,
) (any, error) {
	rows, err := api.pools.AdminUserCacheLeaderboard(ctx, query)
	if err != nil {
		return nil, err
	}
	if !includeModelStats {
		entries := make([]leaderboardUserCacheEntry, 0, len(rows))
		for _, row := range rows {
			totalCost := strconvFloatText(row.TotalCostText)
			cacheCreationCost := strconvFloatText(row.CacheCreationCost)
			entries = append(entries, leaderboardUserCacheEntry{
				UserID:                     row.UserID,
				UserName:                   row.UserName,
				TotalRequests:              row.TotalRequests,
				TotalCost:                  totalCost,
				CacheReadTokens:            row.CacheReadTokens,
				CacheCreationCost:          cacheCreationCost,
				TotalInputTokens:           row.TotalInputTokens,
				TotalTokens:                row.TotalInputTokens,
				CacheHitRate:               clampRatio01(row.CacheHitRate),
				TotalCostFormatted:         leaderboardFormatCurrency(totalCost, currency),
				CacheCreationCostFormatted: leaderboardFormatCurrency(cacheCreationCost, currency),
			})
		}
		return entries, nil
	}

	modelRows, err := api.pools.AdminUserCacheModelLeaderboard(ctx, query)
	if err != nil {
		return nil, err
	}
	modelsByUser := make(map[int64][]leaderboardUserModelCacheStat, len(modelRows))
	for _, row := range modelRows {
		modelsByUser[row.UserID] = append(modelsByUser[row.UserID], leaderboardUserModelCacheStat{
			Model:            row.Model,
			TotalRequests:    row.TotalRequests,
			CacheReadTokens:  row.CacheReadTokens,
			TotalInputTokens: row.TotalInputTokens,
			CacheHitRate:     clampRatio01(row.CacheHitRate),
		})
	}
	entries := make([]leaderboardUserCacheEntryWithModels, 0, len(rows))
	for _, row := range rows {
		totalCost := strconvFloatText(row.TotalCostText)
		cacheCreationCost := strconvFloatText(row.CacheCreationCost)
		models := modelsByUser[row.UserID]
		if models == nil {
			models = []leaderboardUserModelCacheStat{}
		}
		entries = append(entries, leaderboardUserCacheEntryWithModels{
			UserID:                     row.UserID,
			UserName:                   row.UserName,
			TotalRequests:              row.TotalRequests,
			TotalCost:                  totalCost,
			CacheReadTokens:            row.CacheReadTokens,
			CacheCreationCost:          cacheCreationCost,
			TotalInputTokens:           row.TotalInputTokens,
			TotalTokens:                row.TotalInputTokens,
			CacheHitRate:               clampRatio01(row.CacheHitRate),
			ModelStats:                 models,
			TotalCostFormatted:         leaderboardFormatCurrency(totalCost, currency),
			CacheCreationCostFormatted: leaderboardFormatCurrency(cacheCreationCost, currency),
		})
	}
	return entries, nil
}

// leaderboardAverageCosts 复刻 computeAvgCosts：分母为 0 时两个均值都是 null。
func leaderboardAverageCosts(totalCost float64, totalRequests, totalTokens float64) (*float64, *float64) {
	var perRequest, perMillionTokens *float64
	if totalRequests > 0 {
		value := totalCost / totalRequests
		perRequest = &value
	}
	if totalTokens > 0 {
		value := (totalCost * 1_000_000) / totalTokens
		perMillionTokens = &value
	}
	return perRequest, perMillionTokens
}

// providerLeaderboard 组装 provider scope 的作答（含缓存系数合并与可选的模型拆分）。
func (api *leaderboardAPI) providerLeaderboard(
	ctx context.Context,
	query store.AdminLeaderboardQuery,
	currency string,
	includeModelStats bool,
) (any, error) {
	rows, err := api.pools.AdminProviderLeaderboard(ctx, query)
	if err != nil {
		return nil, err
	}
	coefficients, err := api.pools.AdminProviderCacheCoefficients(ctx, query)
	if err != nil {
		return nil, err
	}
	coefficientBP := func(providerID int64) *int64 {
		coefficient, ok := coefficients[providerID]
		if !ok {
			return nil
		}
		value := coefficient.CoefficientBP
		return &value
	}

	if !includeModelStats {
		entries := make([]leaderboardProviderEntry, 0, len(rows))
		for _, row := range rows {
			totalCost := strconvFloatText(row.TotalCostText)
			perRequest, perMillionTokens := leaderboardAverageCosts(
				totalCost, row.TotalRequests, row.TotalTokens)
			entries = append(entries, leaderboardProviderEntry{
				ProviderID:                       row.ProviderID,
				ProviderName:                     row.ProviderName,
				TotalRequests:                    row.TotalRequests,
				TotalCost:                        totalCost,
				TotalTokens:                      row.TotalTokens,
				SuccessRate:                      clampRatio01Ptr(row.SuccessRate),
				AvgTtftMS:                        row.AvgTtftMS,
				AvgTokensPerSecond:               row.AvgTokensPerSec,
				CacheCoefficientBP:               coefficientBP(row.ProviderID),
				AvgCostPerRequest:                perRequest,
				AvgCostPerMillionTokens:          perMillionTokens,
				TotalCostFormatted:               leaderboardFormatCurrency(totalCost, currency),
				AvgCostPerRequestFormatted:       leaderboardFormatCurrencyPtr(perRequest, currency),
				AvgCostPerMillionTokensFormatted: leaderboardFormatCurrencyPtr(perMillionTokens, currency),
			})
		}
		return entries, nil
	}

	modelRows, err := api.pools.AdminProviderModelLeaderboard(ctx, query)
	if err != nil {
		return nil, err
	}
	// 模型维度的缓存系数只在 billingModelSource=redirected 时才有意义（Node 同判）。
	var modelCoefficients map[int64]map[string]store.CacheCoefficient
	if query.BillingModelSource == "redirected" {
		modelCoefficients, err = api.pools.AdminProviderModelCacheCoefficients(ctx, query)
		if err != nil {
			return nil, err
		}
	}
	basisDisclosureRequired := query.BillingModelSource != "original"

	modelsByProvider := make(map[int64][]leaderboardProviderModelStat, len(modelRows))
	for _, row := range modelRows {
		if row.Model == nil || *row.Model == "" {
			continue
		}
		totalCost := strconvFloatText(row.TotalCostText)
		perRequest, perMillionTokens := leaderboardAverageCosts(
			totalCost, row.TotalRequests, row.TotalTokens)
		successRate := clampRatio01Ptr(row.SuccessRate)
		stat := leaderboardProviderModelStat{
			Model:                            *row.Model,
			TotalRequests:                    row.TotalRequests,
			TotalCost:                        totalCost,
			TotalTokens:                      row.TotalTokens,
			SuccessRate:                      successRate,
			AvgTtftMS:                        row.AvgTtftMS,
			AvgTokensPerSecond:               row.AvgTokensPerSec,
			RowIdentityBasis:                 query.BillingModelSource,
			SuccessRateBasis:                 query.BillingModelSource,
			CostTokensBasis:                  query.BillingModelSource,
			BasisDisclosureRequired:          basisDisclosureRequired,
			AvgCostPerRequest:                perRequest,
			AvgCostPerMillionTokens:          perMillionTokens,
			TotalCostFormatted:               leaderboardFormatCurrency(totalCost, currency),
			AvgCostPerRequestFormatted:       leaderboardFormatCurrencyPtr(perRequest, currency),
			AvgCostPerMillionTokensFormatted: leaderboardFormatCurrencyPtr(perMillionTokens, currency),
		}
		if successRate == nil {
			reason := "no_countable_outcomes"
			stat.SuccessRateUnavailableReason = &reason
		}
		if query.BillingModelSource == "redirected" {
			if byModel, ok := modelCoefficients[row.ProviderID]; ok {
				if coefficient, ok := byModel[*row.Model]; ok {
					value := coefficient.CoefficientBP
					stat.CacheCoefficientBP = &value
				}
			}
		}
		modelsByProvider[row.ProviderID] = append(modelsByProvider[row.ProviderID], stat)
	}

	entries := make([]leaderboardProviderEntryWithModels, 0, len(rows))
	for _, row := range rows {
		totalCost := strconvFloatText(row.TotalCostText)
		perRequest, perMillionTokens := leaderboardAverageCosts(
			totalCost, row.TotalRequests, row.TotalTokens)
		models := modelsByProvider[row.ProviderID]
		if models == nil {
			models = []leaderboardProviderModelStat{}
		}
		entries = append(entries, leaderboardProviderEntryWithModels{
			ProviderID:                       row.ProviderID,
			ProviderName:                     row.ProviderName,
			TotalRequests:                    row.TotalRequests,
			TotalCost:                        totalCost,
			TotalTokens:                      row.TotalTokens,
			SuccessRate:                      clampRatio01Ptr(row.SuccessRate),
			AvgTtftMS:                        row.AvgTtftMS,
			AvgTokensPerSecond:               row.AvgTokensPerSec,
			CacheCoefficientBP:               coefficientBP(row.ProviderID),
			AvgCostPerRequest:                perRequest,
			AvgCostPerMillionTokens:          perMillionTokens,
			ModelStats:                       models,
			TotalCostFormatted:               leaderboardFormatCurrency(totalCost, currency),
			AvgCostPerRequestFormatted:       leaderboardFormatCurrencyPtr(perRequest, currency),
			AvgCostPerMillionTokensFormatted: leaderboardFormatCurrencyPtr(perMillionTokens, currency),
		})
	}
	return entries, nil
}

// providerCacheLeaderboard 组装 providerCacheHitRate scope 的作答。
//
// 排序是**合并系数之后**重排的：系数 DESC（无数据排最后），并列再按命中率 DESC（Node 同序）。
func (api *leaderboardAPI) providerCacheLeaderboard(
	ctx context.Context,
	query store.AdminLeaderboardQuery,
	currency string,
) (any, error) {
	rows, err := api.pools.AdminProviderCacheLeaderboard(ctx, query)
	if err != nil {
		return nil, err
	}
	coefficients, err := api.pools.AdminProviderCacheCoefficients(ctx, query)
	if err != nil {
		return nil, err
	}
	modelRows, err := api.pools.AdminProviderCacheModelLeaderboard(ctx, query)
	if err != nil {
		return nil, err
	}
	var modelCoefficients map[int64]map[string]store.CacheCoefficient
	if query.BillingModelSource == "redirected" {
		modelCoefficients, err = api.pools.AdminProviderModelCacheCoefficients(ctx, query)
		if err != nil {
			return nil, err
		}
	}

	modelsByProvider := make(map[int64][]leaderboardProviderCacheModelStat, len(modelRows))
	for _, row := range modelRows {
		if row.Model == nil || *row.Model == "" {
			continue
		}
		stat := leaderboardProviderCacheModelStat{
			Model:            *row.Model,
			TotalRequests:    row.TotalRequests,
			CacheReadTokens:  row.CacheReadTokens,
			TotalInputTokens: row.TotalInputTokens,
			CacheHitRate:     clampRatio01(row.CacheHitRate),
		}
		if query.BillingModelSource == "redirected" {
			if byModel, ok := modelCoefficients[row.ProviderID]; ok {
				if coefficient, ok := byModel[*row.Model]; ok {
					value := coefficient.CoefficientBP
					stat.CacheCoefficientBP = &value
				}
			}
		}
		modelsByProvider[row.ProviderID] = append(modelsByProvider[row.ProviderID], stat)
	}

	entries := make([]leaderboardProviderCacheEntry, 0, len(rows))
	for _, row := range rows {
		totalCost := strconvFloatText(row.TotalCostText)
		cacheCreationCost := strconvFloatText(row.CacheCreationCost)
		entry := leaderboardProviderCacheEntry{
			ProviderID:                 row.ProviderID,
			ProviderName:               row.ProviderName,
			TotalRequests:              row.TotalRequests,
			TotalCost:                  totalCost,
			CacheReadTokens:            row.CacheReadTokens,
			CacheCreationCost:          cacheCreationCost,
			TotalInputTokens:           row.TotalInputTokens,
			TotalTokens:                row.TotalInputTokens,
			CacheHitRate:               clampRatio01(row.CacheHitRate),
			ModelStats:                 []leaderboardProviderCacheModelStat{},
			TotalCostFormatted:         leaderboardFormatCurrency(totalCost, currency),
			CacheCreationCostFormatted: leaderboardFormatCurrency(cacheCreationCost, currency),
		}
		if models := modelsByProvider[row.ProviderID]; models != nil {
			entry.ModelStats = models
		}
		if coefficient, ok := coefficients[row.ProviderID]; ok {
			value := coefficient.CoefficientBP
			entry.CacheCoefficientBP = &value
		}
		entries = append(entries, entry)
	}

	sort.SliceStable(entries, func(first, second int) bool {
		left, right := entries[first].CacheCoefficientBP, entries[second].CacheCoefficientBP
		if (left == nil) != (right == nil) {
			return right == nil
		}
		if left != nil && right != nil && *left != *right {
			return *left > *right
		}
		return entries[first].CacheHitRate > entries[second].CacheHitRate
	})
	return entries, nil
}

// modelLeaderboard 组装 model scope 的作答（空模型行在 Go 侧过滤）。
func (api *leaderboardAPI) modelLeaderboard(
	ctx context.Context,
	query store.AdminLeaderboardQuery,
	currency string,
) (any, error) {
	rows, err := api.pools.AdminModelLeaderboard(ctx, query)
	if err != nil {
		return nil, err
	}
	basisDisclosureRequired := query.BillingModelSource != "original"
	entries := make([]leaderboardModelEntry, 0, len(rows))
	for _, row := range rows {
		if row.Model == nil || *row.Model == "" {
			continue
		}
		totalCost := strconvFloatText(row.TotalCostText)
		successRate := clampRatio01Ptr(row.SuccessRate)
		entry := leaderboardModelEntry{
			Model:                   *row.Model,
			TotalRequests:           row.TotalRequests,
			TotalCost:               totalCost,
			TotalTokens:             row.TotalTokens,
			SuccessRate:             successRate,
			RowIdentityBasis:        query.BillingModelSource,
			SuccessRateBasis:        query.BillingModelSource,
			CostTokensBasis:         query.BillingModelSource,
			BasisDisclosureRequired: basisDisclosureRequired,
			TotalCostFormatted:      leaderboardFormatCurrency(totalCost, currency),
		}
		if successRate == nil {
			reason := "no_countable_outcomes"
			entry.SuccessRateUnavailableReason = &reason
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
