package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// 本文件是「供应商熔断日志」的读面：某供应商**近期失败**的请求清单。
//
// 与既有读面的分道一致：走 control（不占数据面连接）。
//
// 为什么是**两条来源合并**（而非只看行级 provider_id）：
// W67 已把行级 provider_id 的优先级改为「作答者 > 失败归属 > 入口选中」
// 于是「A 失败后故障转移到 B 并成功」的行，
// 其 provider_id 是 **B**——只查行级字段会**漏掉 A 的失败**，而 A 恰恰可能是已经熔断的那家。
// 实测（近 24h，生产）链内含失败条目的行有 106 条，非空集，故这一类必须查。
//
// 时间窗是**必填**（不是可选优化）：链项的 jsonb 展开没有索引，靠时间窗把扫描按在万行量级
// （实测 24h ≈ 9.7k 行，全表 3.5 万行）。

// AdminProviderCircuitFailure 是熔断日志里的一条失败记录。
//
// 字段名用 camelCase：本类型同时充当响应体的形状来源（与 admin_audit_logs.go 同法）。
type AdminProviderCircuitFailure struct {
	// RequestID 是 message_request.id——排查时用它去使用记录页定位整条请求。
	RequestID int64 `json:"requestId"`
	// CreatedAt 是 ISO 8601 串（Node toISOString() 同形：三位毫秒 + Z）。
	CreatedAt string  `json:"createdAt"`
	Model     *string `json:"model"`
	// StatusCode 为空表示「没有 HTTP 状态码」（本地拒绝、客户端中断、在途未结算）。
	StatusCode   *int    `json:"statusCode"`
	ErrorMessage *string `json:"errorMessage"`
	DurationMS   *int    `json:"durationMs"`
	Endpoint     *string `json:"endpoint"`
	// Source 是这条记录从哪来：direct（行级事实）或 chain（该行链内的失败条目）。
	Source string `json:"source"`
	// ChainReason 只在 Source=chain 时可能非空：链上的结局词（如 retry_failed）。
	ChainReason *string `json:"chainReason"`
}

// circuitFailureSourceDirect 是行级来源。命名带 circuitFailure 前缀以免与并行 lane 撞符号。
const circuitFailureSourceDirect = "direct"

// circuitFailureSourceChain 是链内来源。
const circuitFailureSourceChain = "chain"

// AdminProviderCircuitFailures 取某供应商在 since 之后的近期失败，按时间倒序，最多 limit 条。
//
// 失败判据（两条来源同一口径）：
//   - 行级：`status_code >= 400` 或 `error_message` 非空。**刻意不含**「status_code 为空且无错误」
//     的行——那是刚建行、尚未结算的在途请求，不属于「错误日志」，混进来会让首屏都是噪声。
//   - 链内：该供应商的链项 `statusCode >= 400` 或 `errorMessage` 非空。
//
// limit 的钳制在仓库层也做一遍（调用方已校验，但内部调用方不该能要求一个十万行的页）。
func (p *Pools) AdminProviderCircuitFailures(
	ctx context.Context,
	providerID int64,
	since time.Time,
	limit int,
) ([]AdminProviderCircuitFailure, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	// 三处防爆保护，都是为了让**脏数据**不至于把端点打成 500：
	//
	//   1. `jsonb_array_elements` 只允许数组入参——历史行的 provider_chain 可能是 NULL 或早期
	//      的非数组格式。用 CASE 包住函数入参，而不是把 `jsonb_typeof(...) = 'array'` 写在 WHERE：
	//      FROM 里的函数求值与 WHERE 的先后由计划决定，写在 WHERE 并不能保证拦住报错。
	//   2. `id` 与 `statusCode` 的转型同样用 CASE 包住：WHERE 里「先正则再转型」没有求值顺序保证。
	//   3. 去重用 DISTINCT ON (id)：同一行可能同时命中两条来源（该行 provider_id = 目标、且链里也
	//      有它的失败条目）。行级事实更强，故 direct 优先；链上的结局词仍保留在 chain_reason 里。
	//
	// 最外层按 createdAt 排序：ISO-8601 定宽 UTC 串的字典序等于时间序，可直接用作排序键。
	const query = `
SELECT row_to_json(t)::text FROM (
	SELECT DISTINCT ON (id)
		id,
		created_at_text AS "createdAt",
		model,
		status_code AS "statusCode",
		error_message AS "errorMessage",
		duration_ms AS "durationMs",
		endpoint,
		is_direct,
		chain_reason
	FROM (
		SELECT
			m.id,
			COALESCE(to_char(m.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
				'1970-01-01T00:00:00.000Z') AS created_at_text,
			m.model,
			m.status_code,
			m.error_message,
			m.duration_ms,
			m.endpoint,
			true AS is_direct,
			NULL::text AS chain_reason
		FROM message_request m
		WHERE m.created_at >= $2::timestamptz
			AND m.provider_id = $1
			AND (m.status_code >= 400 OR (m.error_message IS NOT NULL AND m.error_message <> ''))
		UNION ALL
		SELECT
			m.id,
			COALESCE(to_char(m.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
				'1970-01-01T00:00:00.000Z') AS created_at_text,
			m.model,
			m.status_code,
			NULLIF(e->>'errorMessage', '') AS error_message,
			m.duration_ms,
			m.endpoint,
			false AS is_direct,
			NULLIF(e->>'reason', '') AS chain_reason
		FROM message_request m
		CROSS JOIN LATERAL jsonb_array_elements(
			CASE WHEN jsonb_typeof(m.provider_chain) = 'array'
				THEN m.provider_chain ELSE '[]'::jsonb END
		) AS e
		WHERE m.created_at >= $2::timestamptz
			AND (CASE WHEN (e->>'id') ~ '^[0-9]+$' THEN (e->>'id')::bigint ELSE -1 END) = $1
			AND (
				(CASE WHEN COALESCE(e->>'statusCode', '') ~ '^[0-9]+$'
					THEN (e->>'statusCode')::int ELSE 0 END) >= 400
				OR COALESCE(e->>'errorMessage', '') <> ''
			)
	) AS unified
	ORDER BY id, is_direct DESC
) AS t
ORDER BY t."createdAt" DESC, t.id DESC
LIMIT $3`

	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, providerID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询供应商熔断日志失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminProviderCircuitFailure, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取供应商熔断日志行失败: %w", err)
		}
		var row struct {
			ID           int64   `json:"id"`
			CreatedAt    string  `json:"createdAt"`
			Model        *string `json:"model"`
			StatusCode   *int    `json:"statusCode"`
			ErrorMessage *string `json:"errorMessage"`
			DurationMS   *int    `json:"durationMs"`
			Endpoint     *string `json:"endpoint"`
			IsDirect     bool    `json:"is_direct"`
			ChainReason  *string `json:"chain_reason"`
		}
		if err := json.Unmarshal([]byte(payload), &row); err != nil {
			return nil, fmt.Errorf("store: 供应商熔断日志行反序列化失败: %w", err)
		}
		failure := AdminProviderCircuitFailure{
			RequestID:    row.ID,
			CreatedAt:    row.CreatedAt,
			Model:        row.Model,
			StatusCode:   row.StatusCode,
			ErrorMessage: row.ErrorMessage,
			DurationMS:   row.DurationMS,
			Endpoint:     row.Endpoint,
			Source:       circuitFailureSourceChain,
		}
		if row.IsDirect {
			failure.Source = circuitFailureSourceDirect
		}
		if failure.Source == circuitFailureSourceChain {
			failure.ChainReason = row.ChainReason
		}
		results = append(results, failure)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历供应商熔断日志行失败: %w", err)
	}
	return results, nil
}
