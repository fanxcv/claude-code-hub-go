package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// NonBillingEndpoints 复刻 src/lib/utils/performance-formatter.ts 的
// NON_BILLING_ENDPOINTS：这两类端点会写 message_request，但不得进入可计费口径。
var NonBillingEndpoints = []string{
	"/v1/messages/count_tokens",
	"/v1/responses/compact",
}

// BillingCondition 复刻 src/repository/_shared/ledger-conditions.ts 的
// LEDGER_BILLING_CONDITION：排除被拦截行、replay 行与不计费端点。
// 端点比较先去掉尾部斜杠再小写，与 TS 的 REGEXP_REPLACE(endpoint, '/+$', ”) 一致。
const BillingCondition = `(blocked_by IS NULL AND is_replay = false AND (
	endpoint IS NULL
	OR LOWER(REGEXP_REPLACE(endpoint, '/+$', '')) NOT IN ('/v1/messages/count_tokens', '/v1/responses/compact')
))`

// AuditCondition 复刻 LEDGER_AUDIT_CONDITION：允许 replay 行可见，但仍排除拦截行与
// 不计费端点。
const AuditCondition = `(blocked_by IS NULL AND (
	endpoint IS NULL
	OR LOWER(REGEXP_REPLACE(endpoint, '/+$', '')) NOT IN ('/v1/messages/count_tokens', '/v1/responses/compact')
))`

// LedgerEntityType 是账务聚合的主体维度。
type LedgerEntityType string

const (
	LedgerEntityUser     LedgerEntityType = "user"
	LedgerEntityKey      LedgerEntityType = "key"
	LedgerEntityProvider LedgerEntityType = "provider"
)

func (t LedgerEntityType) column() (string, error) {
	switch t {
	case LedgerEntityUser:
		return "user_id", nil
	case LedgerEntityKey:
		return "key", nil
	case LedgerEntityProvider:
		return "final_provider_id", nil
	default:
		return "", fmt.Errorf("store: 未知的账务主体类型 %q", t)
	}
}

// TimeRange 是配额统计的时间窗。
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// SumLedgerCostInTimeRange 复刻 sumLedgerCostInTimeRange：时间窗内的可计费成本合计。
// 返回 numeric 的文本形式，避免浮点误差。
func (p *Pools) SumLedgerCostInTimeRange(
	ctx context.Context,
	entityType LedgerEntityType,
	entityID any,
	start time.Time,
	end time.Time,
) (string, error) {
	column, err := entityType.column()
	if err != nil {
		return "", err
	}
	query := fmt.Sprintf(
		`SELECT COALESCE(SUM(cost_usd), 0)::text FROM usage_ledger
		 WHERE %s = $1 AND created_at >= $2 AND created_at < $3 AND %s`,
		quoteColumn(column), BillingCondition,
	)
	return p.queryCostText(ctx, query, entityID, start, end)
}

// SumLedgerTotalCost 复刻 sumLedgerTotalCost：resetAt 为 nil 表示全时段。
func (p *Pools) SumLedgerTotalCost(
	ctx context.Context,
	entityType LedgerEntityType,
	entityID any,
	resetAt *time.Time,
) (string, error) {
	column, err := entityType.column()
	if err != nil {
		return "", err
	}
	query := fmt.Sprintf(
		`SELECT COALESCE(SUM(cost_usd), 0)::text FROM usage_ledger
		 WHERE %s = $1 AND %s`,
		quoteColumn(column), BillingCondition,
	)
	args := []any{entityID}
	if resetAt != nil {
		args = append(args, *resetAt)
		query += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	return p.queryCostText(ctx, query, args...)
}

// CountLedgerRequestsInTimeRange 复刻 countLedgerRequestsInTimeRange：可计费请求计数。
func (p *Pools) CountLedgerRequestsInTimeRange(
	ctx context.Context,
	entityType LedgerEntityType,
	entityID any,
	start time.Time,
	end time.Time,
) (int64, error) {
	column, err := entityType.column()
	if err != nil {
		return 0, err
	}
	query := fmt.Sprintf(
		`SELECT COUNT(*) FROM usage_ledger
		 WHERE %s = $1 AND created_at >= $2 AND created_at < $3 AND %s`,
		quoteColumn(column), BillingCondition,
	)
	pool, err := p.Data()
	if err != nil {
		return 0, err
	}
	var count int64
	if err := pool.QueryRow(ctx, query, entityID, start, end).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: 统计账本请求数失败: %w", err)
	}
	return count, nil
}

// SumLedgerQuotaCosts 复刻 sumLedgerQuotaCosts：一次查询取多个时间窗的合计，返回顺序与
// 入参一致。
func (p *Pools) SumLedgerQuotaCosts(
	ctx context.Context,
	entityType LedgerEntityType,
	entityID any,
	ranges []TimeRange,
) ([]string, error) {
	if len(ranges) == 0 {
		return []string{}, nil
	}
	column, err := entityType.column()
	if err != nil {
		return nil, err
	}

	args := []any{entityID}
	selectParts := make([]string, 0, len(ranges))
	for index, window := range ranges {
		args = append(args, window.Start, window.End)
		selectParts = append(selectParts, fmt.Sprintf(
			`COALESCE(SUM(CASE WHEN created_at >= $%d AND created_at < $%d THEN cost_usd ELSE 0 END), 0)::text AS r%d`,
			len(args)-1, len(args), index,
		))
	}

	query := fmt.Sprintf(
		`SELECT %s FROM usage_ledger WHERE %s = $1 AND %s`,
		strings.Join(selectParts, ", "), quoteColumn(column), BillingCondition,
	)

	pool, err := p.Data()
	if err != nil {
		return nil, err
	}
	destinations := make([]any, 0, len(ranges))
	values := make([]string, len(ranges))
	for index := range values {
		destinations = append(destinations, &values[index])
	}
	if err := pool.QueryRow(ctx, query, args...).Scan(destinations...); err != nil {
		return nil, fmt.Errorf("store: 批量配额成本查询失败: %w", err)
	}
	return values, nil
}

// LedgerTotalCostBatch 复刻 sumLedgerTotalCostBatch：按主体分组返回总额；
// 入参里的每个 id 都会有结果（缺省为 "0"），maxAgeDays 非正数表示不限时段。
func (p *Pools) LedgerTotalCostBatch(
	ctx context.Context,
	entityType LedgerEntityType,
	entityIDs []any,
	maxAgeDays float64,
) (map[string]string, error) {
	result := make(map[string]string, len(entityIDs))
	if len(entityIDs) == 0 {
		return result, nil
	}
	if entityType != LedgerEntityUser && entityType != LedgerEntityKey {
		return nil, fmt.Errorf("store: 批量成本只支持 user 与 key，收到 %q", entityType)
	}
	column, err := entityType.column()
	if err != nil {
		return nil, err
	}
	for _, id := range entityIDs {
		result[fmt.Sprint(id)] = "0"
	}

	query := fmt.Sprintf(
		`SELECT %s::text AS entity_id, COALESCE(SUM(cost_usd), 0)::text AS total
		 FROM usage_ledger WHERE %s = ANY($1) AND %s`,
		quoteColumn(column), quoteColumn(column), BillingCondition,
	)
	args := []any{entityIDs}
	if maxAgeDays > 0 {
		cutoff := time.Now().Add(-time.Duration(maxAgeDays) * 24 * time.Hour)
		args = append(args, cutoff)
		query += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	query += fmt.Sprintf(" GROUP BY %s", quoteColumn(column))

	pool, err := p.Data()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 批量成本查询失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entityID string
		var total string
		if err := rows.Scan(&entityID, &total); err != nil {
			return nil, fmt.Errorf("store: 读取批量成本行失败: %w", err)
		}
		result[entityID] = total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 批量成本遍历失败: %w", err)
	}
	return result, nil
}

func (p *Pools) queryCostText(ctx context.Context, query string, args ...any) (string, error) {
	pool, err := p.Data()
	if err != nil {
		return "", err
	}
	var total string
	if err := pool.QueryRow(ctx, query, args...).Scan(&total); err != nil {
		return "", fmt.Errorf("store: 账本成本查询失败: %w", err)
	}
	return total, nil
}
