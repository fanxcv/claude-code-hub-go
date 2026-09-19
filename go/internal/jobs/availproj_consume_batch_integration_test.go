package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住两件只有真库才能证明的批量语义：
//
//  1. 幂等登记批内出现**同 request_id 两次**时，只落一行、且只报一个「新插入」；
//  2. 桶累加批内出现**同 (provider, bucket)** 两次时不报
//     「ON CONFLICT DO UPDATE cannot affect row a second time」，而是按改前的逐桶语义
//     把计数相加、last_request_at 取最大。
//
// 第 2 条是这次改批量写时唯一需要额外处理的坑：单条 INSERT 里两次命中同一行，PG 会直接报错，
// 而改前逐桶 Exec 是允许的。store.UpsertAvailBuckets 因此在内存里先合并同键增量。

func TestIntegrationAvailProjectionBatchPrimitivesHandleDuplicateKeys(t *testing.T) {
	pools := availProjectionPools(t)
	ctx := context.Background()

	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	ensureProjectionTables(t, ctx, writer)
	if _, err := writer.Exec(ctx, `TRUNCATE `+availProjectionTables+` RESTART IDENTITY`); err != nil {
		t.Fatalf("清表失败: %v", err)
	}

	base := truncateMinuteUTC(time.Now())
	later := base.Add(42 * time.Second)
	providerID := int64(991)

	err = pools.RunProjectionTx(ctx, func(ctx context.Context, tx *store.ProjectionTx) error {
		// 同 request_id 两条：真库侧只应落一行，返回值里只出现一次。
		fresh, err := tx.InsertAppliedRequests(ctx, []store.AppliedRequestEntry{
			{RequestID: 9901, EventID: "33333333-3333-4333-8333-333333333301"},
			{RequestID: 9901, EventID: "33333333-3333-4333-8333-333333333302"},
			{RequestID: 9902, EventID: "33333333-3333-4333-8333-333333333303"},
		})
		if err != nil {
			return err
		}
		if len(fresh) != 2 {
			t.Fatalf("应只报 2 个新插入的 request_id（9901 去重、9902 新增），实际 %d：%v", len(fresh), fresh)
		}
		if _, ok := fresh[9901]; !ok {
			t.Fatalf("9901 应在新插入集合里：%v", fresh)
		}
		if _, ok := fresh[9902]; !ok {
			t.Fatalf("9902 应在新插入集合里：%v", fresh)
		}

		// 同 (provider, bucket) 两条增量：必须成功且按「相加 / 取最大」合并。
		return tx.UpsertAvailBuckets(ctx, []store.ProjectionBucketDelta{
			{
				ProviderID: providerID, BucketStart: base,
				SuccessCnt: 1, LatencyCnt: 1, LatencySumMS: 10, LastRequestAt: base,
			},
			{
				ProviderID: providerID, BucketStart: base,
				FailureCnt: 2, LatencyCnt: 2, LatencySumMS: 20, LastRequestAt: later,
			},
		})
	})
	if err != nil {
		t.Fatalf("批量原语在真库上失败: %v", err)
	}

	var (
		appliedRows  int
		successCnt   int
		failureCnt   int
		latencyCnt   int
		latencySumMS int64
		lastRequest  time.Time
	)
	if err := writer.QueryRow(ctx,
		`SELECT COUNT(*) FROM proj_applied_requests WHERE request_id = 9901`).Scan(&appliedRows); err != nil {
		t.Fatalf("读幂等行失败: %v", err)
	}
	if appliedRows != 1 {
		t.Fatalf("同 request_id 应只落一行，实际 %d 行", appliedRows)
	}
	if err := writer.QueryRow(ctx,
		`SELECT success_cnt, failure_cnt, latency_cnt, latency_sum_ms, last_request_at
		 FROM avail_bucket_1m WHERE provider_id = $1 AND bucket_start = $2`,
		providerID, base,
	).Scan(&successCnt, &failureCnt, &latencyCnt, &latencySumMS, &lastRequest); err != nil {
		t.Fatalf("读桶失败: %v", err)
	}
	if successCnt != 1 || failureCnt != 2 || latencyCnt != 3 || latencySumMS != 30 {
		t.Fatalf("同键增量应按相加合并：期望 success=1 failure=2 latencyCnt=3 latencySum=30，实际 %d/%d/%d/%d",
			successCnt, failureCnt, latencyCnt, latencySumMS)
	}
	if !lastRequest.UTC().Equal(later.UTC()) {
		t.Fatalf("last_request_at 应取最大：期望 %s，实际 %s", later.UTC(), lastRequest.UTC())
	}
}
