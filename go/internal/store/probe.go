package store

import (
	"context"
	"fmt"
	"time"
)

// ProbeEndpoint 是端点探活调度的读取视图。
//
// 语义出处（Node）：src/repository/provider-endpoints.ts:330 findEnabledProviderEndpointsForProbing。
// 与 FindEnabledProviderEndpointsByVendorAndType 的差别：那条按 (vendor, type) 取某厂端点池
// 给数据面选路用；这条是**全量**扫描，且只保留「仍存在启用供应商」的 (vendor, type)，
// 避免全禁用或孤儿厂仍被持续拨测（#779/#781）。
type ProbeEndpoint struct {
	ID                 int64      `json:"id"`
	URL                string     `json:"url"`
	VendorID           int64      `json:"vendor_id"`
	ProviderType       string     `json:"provider_type"`
	LastProbedAt       *time.Time `json:"last_probed_at"`
	LastProbeOK        *bool      `json:"last_probe_ok"`
	LastProbeErrorType *string    `json:"last_probe_error_type"`
}

// FindProbeEndpoints 复刻 findEnabledProviderEndpointsForProbing：按 vendor/type 维度
// 门控的启用态端点，按 id 升序。
func (p *Pools) FindProbeEndpoints(ctx context.Context) ([]ProbeEndpoint, error) {
	return readRowsAs[ProbeEndpoint](
		ctx, p,
		`SELECT row_to_json(t)::text FROM (
			WITH enabled_vendor_types AS (
				SELECT DISTINCT pr.provider_vendor_id AS vendor_id, pr.provider_type
				FROM providers pr
				WHERE pr.is_enabled = true
					AND pr.deleted_at IS NULL
					AND pr.provider_vendor_id IS NOT NULL
					AND pr.provider_vendor_id > 0
			)
			SELECT
				e.id AS id,
				e.url AS url,
				e.vendor_id AS vendor_id,
				e.provider_type AS provider_type,
				e.last_probed_at AS last_probed_at,
				e.last_probe_ok AS last_probe_ok,
				e.last_probe_error_type AS last_probe_error_type
			FROM provider_endpoints e
			INNER JOIN enabled_vendor_types vt
				ON vt.vendor_id = e.vendor_id
			 AND vt.provider_type = e.provider_type
			WHERE e.is_enabled = true
				AND e.deleted_at IS NULL
			ORDER BY e.id ASC
		) t`,
	)
}

// ProbeResultInput 是一次探活的结果（Node recordProviderEndpointProbeResult 的入参）。
type ProbeResultInput struct {
	EndpointID   int64
	Source       string
	OK           bool
	StatusCode   *int
	LatencyMS    *int
	ErrorType    *string
	ErrorMessage *string
	ProbedAt     time.Time
}

// RecordProbeResult 复刻 recordProviderEndpointProbeResult：同一事务内先更新端点快照列，
// 再追加一条探活历史。
//
// 端点可能在探测过程中被删除（厂级 cascade / 管理面操作）：此时更新命中 0 行，
// 直接返回而不写历史——否则会撞 FK（Node provider-endpoints.ts:2162 的同一条注释）。
// 失败态的错误描述列只在 ok=false 时写入（成功即清空），与 Node 一致。
func (p *Pools) RecordProbeResult(ctx context.Context, in ProbeResultInput) error {
	pool, err := p.Control()
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 探活写事务开启失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(
		ctx,
		`UPDATE provider_endpoints SET
			last_probed_at = $2,
			last_probe_ok = $3,
			last_probe_status_code = $4,
			last_probe_latency_ms = $5,
			last_probe_error_type = $6,
			last_probe_error_message = $7,
			updated_at = $8
		WHERE id = $1 AND deleted_at IS NULL`,
		in.EndpointID,
		in.ProbedAt,
		in.OK,
		nullableInt(in.StatusCode),
		nullableInt(in.LatencyMS),
		nullableText(in.ErrorType, in.OK),
		nullableText(in.ErrorMessage, in.OK),
		in.ProbedAt,
	)
	if err != nil {
		return fmt.Errorf("store: 探活结果更新失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}

	if _, err := tx.Exec(
		ctx,
		`INSERT INTO provider_endpoint_probe_logs
			(endpoint_id, source, ok, status_code, latency_ms, error_type, error_message, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		in.EndpointID,
		in.Source,
		in.OK,
		nullableInt(in.StatusCode),
		nullableInt(in.LatencyMS),
		in.ErrorType,
		in.ErrorMessage,
		in.ProbedAt,
	); err != nil {
		return fmt.Errorf("store: 探活历史写入失败: %w", err)
	}
	return tx.Commit(ctx)
}

// DeleteProbeLogsBeforeDateBatch 复刻 deleteProviderEndpointProbeLogsBeforeDateBatch：
// 单批删除早于 cutoff 的探活历史（FOR UPDATE SKIP LOCKED），返回删除行数。
//
// 用 Exec 读 CommandTag 而不是 QueryRow：批量 DELETE 的产物是受影响行数，
// 不是结果集（Node 的 db.execute 也是取 count）。
func (p *Pools) DeleteProbeLogsBeforeDateBatch(
	ctx context.Context,
	before time.Time,
	batchSize int,
) (int, error) {
	pool, err := p.Control()
	if err != nil {
		return 0, err
	}
	tag, err := pool.Exec(
		ctx,
		`WITH ids_to_delete AS (
			SELECT id FROM provider_endpoint_probe_logs
			WHERE created_at < $1
			ORDER BY created_at ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM provider_endpoint_probe_logs
		WHERE id IN (SELECT id FROM ids_to_delete)`,
		before, batchSize,
	)
	if err != nil {
		return 0, fmt.Errorf("store: 探活历史清理失败: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

// nullableText 在成功探活时把错误描述列清空（Node: ok ? null : value）。
func nullableText(value *string, success bool) any {
	if success || value == nil {
		return nil
	}
	return *value
}
