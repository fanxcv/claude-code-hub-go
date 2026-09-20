package terminal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/patrol"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是真库下的异步终态写用例（S3/S4）：
//
//   - 一批 flush 之后，行、账本行、outbox 事件三处产物各恰好一条；
//   - 重复投递幂等（库内谓词吸收，账本与 outbox 不重复产生）；
//   - 崩溃窗口（入队后进程没了、队列从未 flush）留下的未终态行，由既有 patrol 补终态，
//     且队列恢复后的迟到 flush 不得覆盖它（单一赢家仍是库内谓词）。
//
// 未设置 CCH_TEST_DSN 时整组跳过（与 integration_test.go 同一门控）。

// asyncTestRow 建一条本次用例独有的开行（key 里带纳秒时间戳，便于精确清理）。
func asyncTestRow(t *testing.T, pools *store.Pools, ctx context.Context, tag string) store.MessageRequest {
	t.Helper()
	key := itKey(t) + "-" + tag
	model := "gpt-5.6"
	originalModel := model
	userAgent := "terminal-async-it"
	clientIP := "127.0.0.1"
	endpoint := "/v1/responses"
	row, err := pools.CreateMessageRequest(ctx, store.CreateMessageRequestData{
		ProviderID:    1,
		UserID:        1,
		Key:           key,
		Model:         &model,
		OriginalModel: &originalModel,
		UserAgent:     &userAgent,
		ClientIP:      &clientIP,
		Endpoint:      &endpoint,
		MessagesCount: intPtr(1),
	})
	if err != nil {
		t.Fatalf("建行失败: %v", err)
	}
	cleanupRequest(t, pools, row.ID, key)
	return row
}

// asyncTestSettlement 造一份「会赢下终态并计费」的结算输入。
func asyncTestSettlement(durationMS int) Settlement {
	return Settlement{
		StatusCode:    200,
		DurationMS:    intPtr(durationMS),
		TTFTMS:        intPtr(43),
		FirstByteMS:   intPtr(43),
		Usage:         Usage{InputTokens: int64Ptr(1), OutputTokens: int64Ptr(1)},
		Cost:          &Cost{Total: "0.000024", Breakdown: []byte(`{"total":"0.000024"}`)},
		ProviderChain: []byte(`[{"id":1,"name":"mock-upstream","reason":"request_success","statusCode":200}]`),
	}
}

// 一批三条：行/账本/outbox 三处产物各一条，且成本确实落在批里。
func TestIntegrationAsyncBatchWritesRowLedgerAndOutbox(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()
	queue := NewWriteQueue(AsyncOptions{MaxPending: 32, BatchSize: 3, FlushInterval: time.Hour})
	t.Cleanup(queue.Stop)
	settler := New(StoreWriter{Pools: pools}, Options{
		MaxAttempts: 3,
		Backoff:     func(int) time.Duration { return 0 },
		Queue:       queue,
	})

	rows := make([]store.MessageRequest, 0, 3)
	for i := 0; i < 3; i++ {
		row := asyncTestRow(t, pools, ctx, fmt.Sprintf("batch-%d", i))
		rows = append(rows, row)
		result, err := settler.Settle(ctx, row.ID, asyncTestSettlement(62+i))
		if err != nil {
			t.Fatalf("入队失败: %v", err)
		}
		if !result.Queued {
			t.Fatalf("异步模式下 Settle 应先入队: %+v", result)
		}
		// 入队后立刻查：终态尚未落库（这正是「响应尾链不再等写」的可观测形态）。
		assertRowUnsettled(t, pools, ctx, row.ID)
	}

	flushCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := queue.Flush(flushCtx); err != nil {
		t.Fatalf("冲队列失败: %v", err)
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	for index, row := range rows {
		var statusCode *int
		var costUSD *string
		if err := pool.QueryRow(ctx,
			`SELECT status_code, cost_usd::text FROM message_request WHERE id = $1`, row.ID,
		).Scan(&statusCode, &costUSD); err != nil {
			t.Fatalf("读回终态行失败: %v", err)
		}
		if statusCode == nil || *statusCode != 200 {
			t.Fatalf("第 %d 行 status_code = %v, want 200", index, statusCode)
		}
		if costUSD == nil || !strings.HasPrefix(*costUSD, "0.000024") {
			t.Fatalf("第 %d 行 cost_usd = %v, want 0.000024（成本必须与终态同批落在其后）", index, costUSD)
		}

		var ledgerCount int
		var isSuccess *bool
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*), bool_and(is_success) FROM usage_ledger WHERE request_id = $1`, row.ID,
		).Scan(&ledgerCount, &isSuccess); err != nil {
			t.Fatalf("读账本行失败: %v", err)
		}
		if ledgerCount != 1 {
			t.Fatalf("第 %d 行账本行数 = %d, want 1（触发器随批量写产生，不得重复）", index, ledgerCount)
		}
		if isSuccess == nil || !*isSuccess {
			t.Fatalf("第 %d 行 is_success = %v, want true", index, isSuccess)
		}

		var outboxCount int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM outbox_events
			 WHERE aggregate_id = $1 AND event_type = 'request_finalized'`, row.ID,
		).Scan(&outboxCount); err != nil {
			t.Fatalf("读 outbox 事件失败: %v", err)
		}
		if outboxCount != 1 {
			t.Fatalf("第 %d 行 request_finalized 事件数 = %d, want 1", index, outboxCount)
		}
	}

	// 重复投递：谓词吸收，三处产物都不得翻倍。
	dup, err := settler.Settle(ctx, rows[0].ID, Settlement{
		StatusCode:   502,
		DurationMS:   intPtr(999),
		ErrorMessage: strPtr("late overwrite attempt"),
	})
	if err != nil {
		t.Fatalf("重复投递不得报错（它是幂等结论而非失败）: %v", err)
	}
	if !dup.Queued {
		t.Fatalf("重复投递同样先入队: %+v", dup)
	}
	if err := queue.Flush(flushCtx); err != nil {
		t.Fatalf("二次冲队列失败: %v", err)
	}
	var statusAfter *int
	if err := pool.QueryRow(ctx,
		`SELECT status_code FROM message_request WHERE id = $1`, rows[0].ID,
	).Scan(&statusAfter); err != nil {
		t.Fatalf("复核终态行失败: %v", err)
	}
	if statusAfter == nil || *statusAfter != 200 {
		t.Fatalf("重复投递覆盖了终态：status_code = %v, want 200", statusAfter)
	}
	var ledgerAfter, outboxAfter int
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT COUNT(*) FROM usage_ledger WHERE request_id = $1),
		        (SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'request_finalized')`,
		rows[0].ID,
	).Scan(&ledgerAfter, &outboxAfter); err != nil {
		t.Fatalf("复核产物计数失败: %v", err)
	}
	if ledgerAfter != 1 || outboxAfter != 1 {
		t.Fatalf("重复投递让产物翻倍：ledger=%d outbox=%d, want 1/1", ledgerAfter, outboxAfter)
	}
	if stats := queue.Stats(); stats.CostGap != 0 || stats.Failed != 0 {
		t.Fatalf("本用例不应出现写入失败：%+v", stats)
	}
}

// 崩溃窗口：记录入队后进程没了（队列从未 flush），行留在未终态；patrol 按既有语义补 499，
// 且队列恢复后的迟到 flush 不得覆盖它。
//
// 候选列举刻意**只喂本次的行**：真实 DBStore 的列举会扫全库（共享开发库上会顺手补掉别人的行，
// 那不该由一条测试决定）。补写走真实库与真实谓词，故「单一赢家」与账本重建都是实测。
func TestIntegrationPatrolRepairsAsyncCrashWindowRow(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()
	queue := NewWriteQueue(AsyncOptions{MaxPending: 8, BatchSize: 100, FlushInterval: time.Hour})
	t.Cleanup(queue.Stop)
	settler := New(StoreWriter{Pools: pools}, Options{
		MaxAttempts: 3,
		Backoff:     func(int) time.Duration { return 0 },
		Queue:       queue,
	})

	row := asyncTestRow(t, pools, ctx, "crash-window")
	if _, err := settler.Settle(ctx, row.ID, asyncTestSettlement(70)); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	// 批未满、间隔未到：此时它正躺在内存队列里，硬崩即丢——正是 patrol 要兜的窗口。
	assertRowUnsettled(t, pools, ctx, row.ID)

	patrolStore := &scopedUnsettledStore{
		db:  patrol.NewDBStore(pools),
		ids: map[int64]bool{row.ID: true},
	}
	// 时钟前移而不是等待：UnsettledAfter 的语义是「多久没终态」，与真实墙钟无关。
	runner, err := patrol.New(patrol.Options{
		Store:          patrolStore,
		Logger:         logx.New(nil),
		UnsettledAfter: time.Minute,
		Now:            func() time.Time { return time.Now().Add(10 * time.Minute) },
	})
	if err != nil {
		t.Fatalf("构造巡检失败: %v", err)
	}
	result, err := runner.RunOnce(ctx)
	if err != nil {
		t.Fatalf("巡检执行失败: %v", err)
	}
	if result.Repaired != 1 {
		t.Fatalf("巡检应补掉 1 行，实际 result=%+v", result)
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var statusCode *int
	var errorMessage, errorStack *string
	if err := pool.QueryRow(ctx,
		`SELECT status_code, error_message, error_stack FROM message_request WHERE id = $1`, row.ID,
	).Scan(&statusCode, &errorMessage, &errorStack); err != nil {
		t.Fatalf("读回补写后的行失败: %v", err)
	}
	if statusCode == nil || *statusCode != patrol.RepairStatusCode {
		t.Fatalf("status_code = %v, want %d（patrol 的补写口径）", statusCode, patrol.RepairStatusCode)
	}
	if errorMessage == nil || *errorMessage != patrol.RepairErrorMessage {
		t.Fatalf("error_message = %v, want %q", errorMessage, patrol.RepairErrorMessage)
	}
	if errorStack == nil || !strings.HasPrefix(*errorStack, patrol.MarkerPrefix) {
		t.Fatalf("error_stack 必须带可追溯标记 %q，得到 %v", patrol.MarkerPrefix, errorStack)
	}
	// 账本随补写重建为「不成功」：保守，不虚增成功率。
	var isSuccess *bool
	if err := pool.QueryRow(ctx,
		`SELECT is_success FROM usage_ledger WHERE request_id = $1`, row.ID,
	).Scan(&isSuccess); err != nil {
		t.Fatalf("读账本行失败: %v", err)
	}
	if isSuccess == nil || *isSuccess {
		t.Fatalf("补写后 is_success = %v, want false（不得虚增成功）", isSuccess)
	}

	// 队列恢复（进程重启后不可能有这条，这里用 Stop 模拟「终于写出去」）：迟到的终态不得覆盖。
	queue.Stop()
	var statusAfter *int
	var costAfter *string
	if err := pool.QueryRow(ctx,
		`SELECT status_code, cost_usd::text FROM message_request WHERE id = $1`, row.ID,
	).Scan(&statusAfter, &costAfter); err != nil {
		t.Fatalf("复核迟到写失败: %v", err)
	}
	if statusAfter == nil || *statusAfter != patrol.RepairStatusCode {
		t.Fatalf("迟到 flush 覆盖了 patrol 的终态：status_code = %v", statusAfter)
	}
	if costAfter != nil {
		t.Fatalf("迟到 flush 不得给已终态的行补成本：cost_usd = %v", costAfter)
	}
	stats := queue.Stats()
	// 这一条走的是「未赢得该行」的幂等结论（patrol 先补了终态），不是写入失败、
	// 也不是本队列写下的行：三种计数分开看才能既证明「确实执行了一次写入」，
	// 又不把幂等去重报成落库量。
	if stats.NotSettled != 1 || stats.Failed != 0 || stats.Rows != 0 {
		t.Fatalf("迟到 flush 应确认「未赢得该行」（NotSettled=1），且不得计成失败或落库：%+v", stats)
	}
}

// scopedUnsettledStore 把真实库的候选列举收窄到指定行，补写仍走真实库。
type scopedUnsettledStore struct {
	db  *patrol.DBStore
	ids map[int64]bool
}

func (s *scopedUnsettledStore) ListUnsettledRequests(
	ctx context.Context,
	cutoff time.Time,
	after store.UnsettledCursor,
	limit int,
) ([]store.UnsettledRequest, error) {
	rows, err := s.db.ListUnsettledRequests(ctx, cutoff, after, limit)
	if err != nil {
		return nil, err
	}
	scoped := make([]store.UnsettledRequest, 0, len(rows))
	for _, row := range rows {
		if s.ids[row.ID] {
			scoped = append(scoped, row)
		}
	}
	return scoped, nil
}

func (s *scopedUnsettledStore) RepairUnsettled(
	ctx context.Context,
	id int64,
	patch store.DetailsPatch,
) (bool, error) {
	if !s.ids[id] {
		return false, errors.New("scopedUnsettledStore: 不该补写本次用例之外的行")
	}
	return s.db.RepairUnsettled(ctx, id, patch)
}

// assertRowUnsettled 断言该行尚未终态。
func assertRowUnsettled(t *testing.T, pools *store.Pools, ctx context.Context, id int64) {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var unsettled bool
	if err := pool.QueryRow(ctx,
		`SELECT status_code IS NULL FROM message_request WHERE id = $1`, id,
	).Scan(&unsettled); err != nil {
		t.Fatalf("查行状态失败: %v", err)
	}
	if !unsettled {
		t.Fatalf("行 %d 应尚未终态（否则本用例的前提不成立）", id)
	}
}
