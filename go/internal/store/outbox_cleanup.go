package store

import (
	"context"
	"fmt"
	"time"
)

// 本文件是 `outbox_events` 保留期清理的存储层（唯一语句）。
//
// 为什么需要它：`outbox_events` 只增不减——唯一读方 `ClaimOutboxBatch` 带
// `WHERE published_at IS NULL`（availability_projection_consume.go），已发布行没有任何读路径，
// 却一直占着堆与索引（生产实测 88 万行 / 483 MB）。
//
// 三条边界：
//  1. **只删 `published_at IS NOT NULL`**。未发布行是投影消费的唯一输入，删掉即永久缺口
//     （回填已 done，无补偿路径）。这不是冗余条件：同一时刻表里可能既有已发布行、
//     也有刚落库未认领的行。谓词由 deletePublishedOutboxBeforeBatchSQL 的单元钉子
//     （outbox_cleanup_test.go）与真库负例（jobs 包集成用例）共同守住。
//  2. 单批有界 + `FOR UPDATE SKIP LOCKED`：每批一个事务，不长锁、不让 WAL 一次暴涨。
//  3. 不加索引：`published_at` 无索引，`ORDER BY id` 走主键；认领侧走部分索引
//     `idx_outbox_events_unpublished`，两个行集互斥，清理不会阻塞消费。

// deletePublishedOutboxBeforeBatchSQL 是保留期清理的唯一删除语句。
//
// 截止时刻由调用方给定（不用 SQL 里的 `now() - interval`）：调度侧的时钟可注入，
// 测试才能与墙钟无关地钉住「哪一行该被删」。
const deletePublishedOutboxBeforeBatchSQL = `
WITH doomed AS (
	SELECT id FROM outbox_events
	WHERE published_at IS NOT NULL
	  AND published_at < $1
	ORDER BY id
	LIMIT $2
	FOR UPDATE SKIP LOCKED
)
DELETE FROM outbox_events WHERE id IN (SELECT id FROM doomed)`

// DeletePublishedOutboxBeforeBatch 删一批「已发布且早于 before」的 outbox 事件，返回删除行数。
//
// 用 Exec 读 CommandTag 而不是 QueryRow：批量 DELETE 的产物是受影响行数，不是结果集。
func (p *Pools) DeletePublishedOutboxBeforeBatch(
	ctx context.Context,
	before time.Time,
	batchSize int,
) (int, error) {
	pool, err := p.Control()
	if err != nil {
		return 0, err
	}
	tag, err := pool.Exec(ctx, deletePublishedOutboxBeforeBatchSQL, before, batchSize)
	if err != nil {
		return 0, fmt.Errorf("store: outbox 已发布事件清理失败: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
