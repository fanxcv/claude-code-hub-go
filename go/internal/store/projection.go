package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// InsertProjAppliedRequest 复刻 src/lib/availability/projection-worker.ts 的投影去重写入：
//
//	INSERT INTO proj_applied_requests (request_id, event_id) VALUES ($1, $2::uuid)
//	ON CONFLICT (request_id) DO NOTHING
//	RETURNING request_id
//
// 幂等键是 request_id（表上的唯一约束）。返回 true 表示本次是新应用（投影应继续累加），
// false 表示该 request 已被应用过（重复投递或重试）。event_id 必须是合法 uuid 文本，
// 非法值会让 PostgreSQL 直接报错——与 TS 侧的 ::uuid 强制转换同语义。
func (p *Pools) InsertProjAppliedRequest(
	ctx context.Context,
	requestID int64,
	eventID string,
) (bool, error) {
	pool, err := p.Control()
	if err != nil {
		return false, err
	}
	return InsertProjAppliedRequestWith(ctx, pool, requestID, eventID)
}

// InsertProjAppliedRequestWith 在指定分道（或事务内）执行投影去重写入。
func InsertProjAppliedRequestWith(
	ctx context.Context,
	pool *Pool,
	requestID int64,
	eventID string,
) (bool, error) {
	var returned int64
	err := pool.QueryRow(
		ctx,
		`INSERT INTO proj_applied_requests (request_id, event_id)
		 VALUES ($1, $2::uuid)
		 ON CONFLICT (request_id) DO NOTHING
		 RETURNING request_id`,
		requestID, eventID,
	).Scan(&returned)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING 命中：该 request_id 已应用过。
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: 投影去重写入失败: %w", err)
	}
	return true, nil
}
