package jobs

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 outbox 保留期清理的**真库**用例，覆盖「SQL 真的只删已发布且过期的行」。
//
// 为什么必须真库：谓词、`FOR UPDATE SKIP LOCKED` 与批循环的组合效果只有库能给答案——
// 尤其是**最关键的那条负例**：未发布但很旧的行必须留下。用替身测只能证明「我调了删除函数」，
// 证明不了「谓词没放过未发布行」。
//
// 门控沿用仓内既有的**可弃库**变量 `CCH_TEST_SYNC_DSN`（同 logcleanup_integration_test.go）：
// 本用例会真删行，落在共享测试库上等于替别人清库。未设置即跳过。
const outboxRetentionDisposableDSNEnv = "CCH_TEST_SYNC_DSN"

// outboxRetentionFixturePrefix 是本用例夹具的唯一标记（事后按它精确清理）。
const outboxRetentionFixturePrefix = "go-outbox-retention-it-"

func outboxRetentionDisposablePools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv(outboxRetentionDisposableDSNEnv)
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_SYNC_DSN（可弃库），跳过会真删行的 outbox 清理集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-outbox-retention-it",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// seedOutboxEvent 造一条 outbox 事件；publishedAt 为 nil 即未发布（投影消费的候选）。
func seedOutboxEvent(t *testing.T, pools *store.Pools, marker string, publishedAt *time.Time) string {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	var eventID string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO outbox_events (event_type, aggregate_type, aggregate_id, occurred_at, payload, published_at)
		 VALUES ($1, 'request', 1, now(), '{}'::jsonb, $2)
		 RETURNING event_id::text`,
		marker, publishedAt,
	).Scan(&eventID); err != nil {
		t.Fatalf("造 outbox 夹具失败: %v", err)
	}
	t.Cleanup(func() { deleteOutboxFixtures(context.Background(), pools, marker) })
	return eventID
}

func deleteOutboxFixtures(ctx context.Context, pools *store.Pools, marker string) {
	pool, err := pools.Control()
	if err != nil {
		return
	}
	_, _ = pool.Exec(ctx, `DELETE FROM outbox_events WHERE event_type = $1`, marker)
}

// outboxFixtureSurvives 报告某个 event_id 是否还在表里。
func outboxFixtureSurvives(t *testing.T, pools *store.Pools, eventID string) bool {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*)::int FROM outbox_events WHERE event_id = $1::uuid`, eventID,
	).Scan(&count); err != nil {
		t.Fatalf("查询夹具存在性失败: %v", err)
	}
	return count > 0
}

// TestIntegrationOutboxRetentionDeletesOnlyPublishedExpired 是保留期清理的核心断言。
//
// 夹具相对**固定截止时刻**定位（不相对墙钟），故断言与运行时刻无关：
//
//	已发布 + 过期（cutoff-1h）   → 必须删
//	已发布 + 未过期（cutoff+1h） → 必须留
//	已发布 + 恰好 cutoff        → 必须留（判据是 `published_at < cutoff`，严格小于）
//	未发布 + 极旧（cutoff-30d）  → **必须留**（最关键负例：删了即投影永久缺口）
func TestIntegrationOutboxRetentionDeletesOnlyPublishedExpired(t *testing.T) {
	pools := outboxRetentionDisposablePools(t)
	marker := fmt.Sprintf("%s%d", outboxRetentionFixturePrefix, time.Now().UnixNano())
	runAt := time.Now().Truncate(time.Second)
	cutoff := runAt.Add(-7 * 24 * time.Hour)

	publishedExpired := seedOutboxEvent(t, pools, marker, timePtr(cutoff.Add(-time.Hour)))
	publishedFresh := seedOutboxEvent(t, pools, marker, timePtr(cutoff.Add(time.Hour)))
	publishedBoundary := seedOutboxEvent(t, pools, marker, timePtr(cutoff))
	unpublishedAncient := seedOutboxEvent(t, pools, marker, nil)

	task := NewOpsOutboxEventsRetention(
		OpsDeps{Pools: pools, Now: func() time.Time { return runAt }, Logger: logx.New(nil)},
		OpsOutboxEventsRetentionConfig{Retention: 7 * 24 * time.Hour},
	)
	if _, err := task.Run(context.Background()); err != nil {
		t.Fatalf("清理执行失败: %v", err)
	}

	if outboxFixtureSurvives(t, pools, publishedExpired) {
		t.Fatal("已发布且早于截止时刻的行应被删除，仍然在表里")
	}
	assertOutboxSurvives(t, pools, publishedFresh, "已发布但未过期的行不得删除")
	assertOutboxSurvives(t, pools, publishedBoundary, "恰好落在截止时刻的行不得删除（判据是严格小于）")
	// 最关键的一条：未发布行不论多旧都不许动。
	assertOutboxSurvives(t, pools, unpublishedAncient,
		"未发布（published_at IS NULL）的行再旧也必须保留：它是投影消费的唯一输入，删掉即永久缺口")
}

func assertOutboxSurvives(t *testing.T, pools *store.Pools, eventID, why string) {
	t.Helper()
	if !outboxFixtureSurvives(t, pools, eventID) {
		t.Fatalf("%s（event_id=%s）", why, eventID)
	}
}

// TestIntegrationOutboxRetentionBatchedDeletion 验分批：造 3 批的量（批大小被压到 2），
// 断言全部过期已发布行都被删完，且未发布行一条不少。
//
// 批大小在任务层是常量（store 常量 1000），这里改用 store 的直接入口按 2 行一批驱动，
// 验的是「批循环能把多批删干净」这件事本身。
func TestIntegrationOutboxRetentionBatchedDeletion(t *testing.T) {
	pools := outboxRetentionDisposablePools(t)
	marker := fmt.Sprintf("%sbatched%d", outboxRetentionFixturePrefix, time.Now().UnixNano())
	runAt := time.Now().Truncate(time.Second)
	cutoff := runAt.Add(-24 * time.Hour)

	doomed := make([]string, 0, 5)
	for index := 0; index < 5; index++ {
		doomed = append(doomed,
			seedOutboxEvent(t, pools, marker, timePtr(cutoff.Add(-time.Duration(index+1)*time.Hour))))
	}
	// 未发布行混在同一批候选里：它必须跨批都活下来。
	unpublished := make([]string, 0, 2)
	for index := 0; index < 2; index++ {
		unpublished = append(unpublished, seedOutboxEvent(t, pools, marker, nil))
	}

	deleted := 0
	for round := 0; round < 10; round++ {
		batchDeleted, err := pools.DeletePublishedOutboxBeforeBatch(context.Background(), cutoff, 2)
		if err != nil {
			t.Fatalf("分批删除失败: %v", err)
		}
		deleted += batchDeleted
		if batchDeleted < 2 {
			break
		}
	}
	if deleted != len(doomed) {
		t.Fatalf("应删掉 %d 行已发布过期行，实际 %d 行", len(doomed), deleted)
	}
	for _, eventID := range doomed {
		if outboxFixtureSurvives(t, pools, eventID) {
			t.Fatalf("过期已发布行未被删完: %s", eventID)
		}
	}
	for _, eventID := range unpublished {
		assertOutboxSurvives(t, pools, eventID, "分批删除过程中未发布行被删了")
	}
}

func timePtr(value time.Time) *time.Time { return &value }
