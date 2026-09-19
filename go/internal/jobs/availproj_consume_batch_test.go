package jobs

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// TestAvailConsumerBatchesRoundTripsAndDedupesWithinBatch 钉住两件事：
//
//  1. 一批只花**常数次**往返：改前每条事件一次幂等登记、每个桶一次累加（批上限 300 → 最多
//     600 次往返），现在幂等登记与桶累加各一次；
//  2. 批内同 request_id 的语义不变：改前是「首次 fresh=true、次次 false」，故只有首次那份
//     计数进桶（这条语义靠编排侧的首次出现去重保住，因为单条 INSERT 只回一行）。
func TestAvailConsumerBatchesRoundTripsAndDedupesWithinBatch(t *testing.T) {
	base := "2026-09-13T00:00:00Z"
	tx := &fakeProjectionTx{applied: map[int64]bool{}}
	tx.events = []store.ProjectionOutboxEvent{
		projectionEvent(1, "22222222-2222-4222-8222-222222222201", map[string]any{
			"request_id": 9101, "provider_id": 101, "outcome": "success",
			"occurred_at": base, "duration_ms": 100,
		}),
		// 与上一条同 request_id：不该再算一次「新应用」，计数也只有一份。
		projectionEvent(2, "22222222-2222-4222-8222-222222222202", map[string]any{
			"request_id": 9101, "provider_id": 101, "outcome": "success",
			"occurred_at": base, "duration_ms": 100,
		}),
		projectionEvent(3, "22222222-2222-4222-8222-222222222203", map[string]any{
			"request_id": 9102, "provider_id": 101, "outcome": "failure",
			"occurred_at": base, "duration_ms": 7,
		}),
		projectionEvent(4, "22222222-2222-4222-8222-222222222204", map[string]any{
			"request_id": 9103, "provider_id": 202, "outcome": "success",
			"occurred_at": base, "duration_ms": 3,
		}),
		// 非法载荷（缺 provider_id）：进 invalid 分支，不参与登记与累加。
		projectionEvent(5, "22222222-2222-4222-8222-222222222205", map[string]any{
			"request_id": 9104, "outcome": "success", "occurred_at": base,
		}),
	}

	consumer := newConsumerForTest(t, tx)
	result, err := consumer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if result.Applied != 3 {
		t.Fatalf("应应用 3 条（9101/9102/9103），实际 %d", result.Applied)
	}
	if tx.insertAppliedCalls != 1 {
		t.Fatalf("幂等登记应只花 1 次往返，实际 %d 次", tx.insertAppliedCalls)
	}
	if tx.upsertBucketCalls != 1 {
		t.Fatalf("桶累加应只花 1 次往返，实际 %d 次", tx.upsertBucketCalls)
	}
	if len(tx.deltas) != 2 {
		t.Fatalf("应产生 2 个桶增量（provider 101/202），实际 %d 个：%+v", len(tx.deltas), tx.deltas)
	}
	// 增量是 map 遍历产物 → 顺序不定（真库侧才排序）；按 provider 取。
	byProvider := make(map[int64]store.ProjectionBucketDelta, len(tx.deltas))
	for _, delta := range tx.deltas {
		byProvider[delta.ProviderID] = delta
	}
	if first := byProvider[101]; first.ProviderID != 101 || first.SuccessCnt != 1 || first.FailureCnt != 1 {
		t.Fatalf("101 桶应只计首次 9101 的成功与 9102 的失败，实际 %+v", first)
	}
	if second := byProvider[202]; second.ProviderID != 202 || second.SuccessCnt != 1 {
		t.Fatalf("202 桶计数不符: %+v", second)
	}
	// 非法行仍以 last_error 标记发布，且与合法行分两批。
	if len(tx.publishes) != 2 {
		t.Fatalf("应分别标记「合法」与「非法」两批，实际 %d 批：%+v", len(tx.publishes), tx.publishes)
	}
}
