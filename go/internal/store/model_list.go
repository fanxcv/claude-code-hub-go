// 本文件是聚合式模型列表（`/v1/models` 一族）的**只读行视图**。
//
// 分层：本文件只给「启用态供应商行 + 有效分组」两件事，过滤（类型 / 活动时段 / 分组）留给
// 调用方 `dataplane` 完成——那里同时依赖 route 与 store，而 store 不能反向依赖 route
// （route→store 已存在，会成环）。分组语义因此只有一份：route.ProviderGroupMatches。
package store

import (
	"context"
	"encoding/json"
	"strings"
)

// ModelListProviderRow 是聚合式模型列表决策所需的供应商行。
//
// 刻意只带决策与取数用到的列。Key 是供应商凭据：调用方只可把它放进上游请求头，
// **不得**写进日志或审计（与本仓既有的脱敏纪律一致）。
type ModelListProviderRow struct {
	// ID 是 providers.id。
	ID int64 `json:"id"`
	// Name 是 providers.name（日志用）。
	Name string `json:"name"`
	// ProviderType 是 providers.provider_type。
	ProviderType string `json:"provider_type"`
	// URL 是供应商基址。
	URL string `json:"url"`
	// Key 是供应商凭据。
	Key string `json:"key"`
	// AllowedModels 是 providers.allowed_models 原始 jsonb；非空时不再访问上游
	// （Node available-models.ts:325-334）。
	AllowedModels json.RawMessage `json:"allowed_models"`
	// RequestTimeoutNonStreamingMS 是供应商级超时（毫秒，零值表示未设）。
	RequestTimeoutNonStreamingMS int `json:"request_timeout_non_streaming_ms"`
	// GroupTag 是 providers.group_tag（可空）。
	GroupTag *string `json:"group_tag"`
	// ActiveTimeStart / ActiveTimeEnd 是供应商的活动时段（`HH:MM`，可空）。
	ActiveTimeStart *string `json:"active_time_start"`
	ActiveTimeEnd   *string `json:"active_time_end"`
}

// ModelListProviderRows 读回全部**启用态**供应商行（含 deleted_at 过滤）。
//
// 与 Node 的 findAllProviders + 过滤的第一条（isEnabled）同源；其余过滤由调用方做。
// 顺序按 priority, id：保持与选路一致的稳定次序，使同一份数据每次成形的列表顺序一致。
func (p *Pools) ModelListProviderRows(ctx context.Context) ([]ModelListProviderRow, error) {
	return readRowsAs[ModelListProviderRow](
		ctx, p,
		`SELECT row_to_json(t)::text FROM (
			SELECT id, name, provider_type, url, key,
			       COALESCE(allowed_models, 'null'::jsonb) AS allowed_models,
			       COALESCE(request_timeout_non_streaming_ms, 0) AS request_timeout_non_streaming_ms,
			       group_tag, active_time_start, active_time_end
			FROM providers
			WHERE is_enabled = true AND deleted_at IS NULL
			ORDER BY priority ASC, id ASC
		) t`,
	)
}

// ModelListEffectiveGroup 取有效分组：密钥分组优先，其次用户分组；都空返回空串。
//
// 与 Node 的 `authState.key.providerGroup || authState.user.providerGroup || null` 同义。
// 读不到密钥或用户时按「无分组」处理而不报错：模型列表不该因为一次读数失败整体失败。
func (p *Pools) ModelListEffectiveGroup(ctx context.Context, keyID int64, userID int64) (string, error) {
	if keyID > 0 {
		group, err := p.modelListGroupOf(ctx, "keys", keyID)
		if err == nil && group != "" {
			return group, nil
		}
	}
	if userID > 0 {
		group, err := p.modelListGroupOf(ctx, "users", userID)
		if err == nil && group != "" {
			return group, nil
		}
	}
	return "", nil
}

// modelListGroupOf 读单表的 provider_group（table 只接受本文件内的两个字面量表名）。
func (p *Pools) modelListGroupOf(ctx context.Context, table string, id int64) (string, error) {
	query := `SELECT row_to_json(t)::text FROM (SELECT provider_group FROM keys WHERE id = $1) t`
	if table == "users" {
		query = `SELECT row_to_json(t)::text FROM (SELECT provider_group FROM users WHERE id = $1) t`
	}
	var row struct {
		ProviderGroup *string `json:"provider_group"`
	}
	if err := p.readSingleRowAs(ctx, query, &row, []any{id}); err != nil {
		return "", err
	}
	if row.ProviderGroup == nil {
		return "", nil
	}
	return strings.TrimSpace(*row.ProviderGroup), nil
}
