package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件覆盖 outbox 保留期清理的**编排**（无库）：批循环的终止条件、单轮上界、
// 失锁时的让位、以及 Outcome 的字段形状。
//
// 删除谓词本身（只删已发布行）由两处守住：store 包的文本钉子
// （outbox_cleanup_test.go）与真库负例（outbox_retention_integration_test.go）。

// fakeOutboxRetentionStore 按预先排好的批次返回删除数，并记录调用参数。
type fakeOutboxRetentionStore struct {
	batches      []int
	calls        int
	lastBefore   time.Time
	lastBatchCap int
}

func (f *fakeOutboxRetentionStore) deleteBatch(
	_ context.Context,
	before time.Time,
	batchSize int,
) (int, error) {
	f.lastBefore = before
	f.lastBatchCap = batchSize
	index := f.calls
	f.calls++
	if index < len(f.batches) {
		return f.batches[index], nil
	}
	return 0, nil
}

// newOutboxRetentionForTest 装配一个只依赖假删除面的任务（无库、无 Redis）。
//
// Redis 为 nil 时 AcquireOpsLeaderLock 退化为进程内持锁，故锁不需要替身。
func newOutboxRetentionForTest(
	deps OpsDeps,
	retention time.Duration,
	deleter *fakeOutboxRetentionStore,
) *OpsOutboxEventsRetention {
	task := NewOpsOutboxEventsRetention(deps, OpsOutboxEventsRetentionConfig{Retention: retention})
	task.deleteBatch = deleter.deleteBatch
	return task
}

func TestOutboxRetentionStopsWhenBatchIsNotFull(t *testing.T) {
	now := time.Date(2026, 3, 5, 2, 30, 0, 0, time.UTC)
	deleter := &fakeOutboxRetentionStore{batches: []int{3}}
	var logs strings.Builder
	task := newOutboxRetentionForTest(
		OpsDeps{Now: func() time.Time { return now }, Logger: logx.New(&logs)},
		7*24*time.Hour,
		deleter,
	)

	outcome, err := task.Run(context.Background())
	if err != nil {
		t.Fatalf("一轮执行失败: %v", err)
	}
	if deleter.calls != 1 {
		t.Fatalf("没删满一批就该停（只 1 次删除），实际调用 %d 次", deleter.calls)
	}
	if outcome.Processed != 3 {
		t.Fatalf("Processed 应为删除行数 3，实际 %d", outcome.Processed)
	}
	if outcome.Fields["deleted"] != 3 {
		t.Fatalf("Fields.deleted 应为 3: %+v", outcome.Fields)
	}
	wantCutoff := now.Add(-7 * 24 * time.Hour)
	if !deleter.lastBefore.Equal(wantCutoff) {
		t.Fatalf("截止时刻应为 now-7d（%s），实际 %s", wantCutoff, deleter.lastBefore)
	}
	if deleter.lastBatchCap != outboxEventsRetentionBatchSize {
		t.Fatalf("单批上限应为 %d，实际 %d", outboxEventsRetentionBatchSize, deleter.lastBatchCap)
	}
	if !strings.Contains(logs.String(), "outbox_retention_completed") {
		t.Fatalf("删到行时该留 completed 日志（含进度），实际日志: %s", logs.String())
	}
}

// 分批边界三档：不足一批 / 恰好一批 / 超过一批。
//
// 「恰好一批」必须多跑一次才能知道真的没了——这是「本批没删满即停」判据的直接后果，
// 用例把它钉住，免得将来把它优化成「删满即停」而漏掉剩余行。
func TestOutboxRetentionBatchBoundaries(t *testing.T) {
	full := outboxEventsRetentionBatchSize
	cases := []struct {
		name        string
		batches     []int
		wantCalls   int
		wantDeleted int
	}{
		{name: "不足一批", batches: []int{full - 1}, wantCalls: 1, wantDeleted: full - 1},
		{name: "恰好一批", batches: []int{full}, wantCalls: 2, wantDeleted: full},
		{name: "超过一批", batches: []int{full, full, 42}, wantCalls: 3, wantDeleted: 2*full + 42},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Date(2026, 3, 5, 2, 30, 0, 0, time.UTC)
			deleter := &fakeOutboxRetentionStore{batches: testCase.batches}
			task := newOutboxRetentionForTest(
				OpsDeps{Now: func() time.Time { return now }}, time.Hour, deleter,
			)
			outcome, err := task.Run(context.Background())
			if err != nil {
				t.Fatalf("一轮执行失败: %v", err)
			}
			if deleter.calls != testCase.wantCalls {
				t.Fatalf("%s：应调用删除 %d 次（末次空批用于确认已清空），实际 %d 次",
					testCase.name, testCase.wantCalls, deleter.calls)
			}
			if outcome.Processed != testCase.wantDeleted {
				t.Fatalf("%s：Processed 应为 %d，实际 %d",
					testCase.name, testCase.wantDeleted, outcome.Processed)
			}
		})
	}
}

// 单轮批数上界：一直删满也不会无限跑下去（首轮 88 万行的收敛上界）。
func TestOutboxRetentionStopsAtMaxBatches(t *testing.T) {
	now := time.Date(2026, 3, 5, 2, 30, 0, 0, time.UTC)
	batches := make([]int, outboxEventsRetentionMaxBatches+10)
	for index := range batches {
		batches[index] = outboxEventsRetentionBatchSize
	}
	deleter := &fakeOutboxRetentionStore{batches: batches}
	task := newOutboxRetentionForTest(OpsDeps{Now: func() time.Time { return now }}, time.Hour, deleter)

	outcome, err := task.Run(context.Background())
	if err != nil {
		t.Fatalf("一轮执行失败: %v", err)
	}
	if deleter.calls != outboxEventsRetentionMaxBatches {
		t.Fatalf("单轮应止于 %d 批，实际 %d 批", outboxEventsRetentionMaxBatches, deleter.calls)
	}
	if outcome.Processed != outboxEventsRetentionMaxBatches*outboxEventsRetentionBatchSize {
		t.Fatalf("Processed 与批数不一致: %d", outcome.Processed)
	}
}

// 单轮时长上界：注入时钟让「跑太久」可测（真库上这条只能靠超时观察）。
func TestOutboxRetentionStopsAtMaxDuration(t *testing.T) {
	clock := time.Date(2026, 3, 5, 2, 30, 0, 0, time.UTC)
	step := outboxEventsRetentionMaxDuration / 3
	task := NewOpsOutboxEventsRetention(
		OpsDeps{Now: func() time.Time { return clock }},
		OpsOutboxEventsRetentionConfig{Retention: time.Hour},
	)
	calls := 0
	task.deleteBatch = func(context.Context, time.Time, int) (int, error) {
		calls++
		clock = clock.Add(step)
		return outboxEventsRetentionBatchSize, nil
	}

	if _, err := task.Run(context.Background()); err != nil {
		t.Fatalf("一轮执行失败: %v", err)
	}
	// 每批推进 1/3 预算，判据在循环头：第 1/2/3 批开跑时均未超预算，第 4 批前收口。
	if calls != 3 {
		t.Fatalf("时长上限应止于 3 批（每批推进 1/3 预算），实际跑了 %d 批", calls)
	}
}

func TestOutboxRetentionWithoutPoolsIsExplicit(t *testing.T) {
	task := NewOpsOutboxEventsRetention(OpsDeps{}, OpsOutboxEventsRetentionConfig{})
	if _, err := task.Run(context.Background()); err == nil {
		t.Fatal("无连接池时必须显式报错（装配缺陷应可见）")
	}
}

// 删除失败要冒泡，且已删的数量仍随 Outcome 返回（巡检据此判断「删到一半」）。
func TestOutboxRetentionSurfacesDeleteFailureWithPartialCount(t *testing.T) {
	now := time.Date(2026, 3, 5, 2, 30, 0, 0, time.UTC)
	var logs strings.Builder
	task := NewOpsOutboxEventsRetention(
		OpsDeps{Now: func() time.Time { return now }, Logger: logx.New(&logs)},
		OpsOutboxEventsRetentionConfig{Retention: time.Hour},
	)
	calls := 0
	task.deleteBatch = func(context.Context, time.Time, int) (int, error) {
		calls++
		if calls == 1 {
			return outboxEventsRetentionBatchSize, nil
		}
		return 0, errors.New("boom")
	}

	outcome, err := task.Run(context.Background())
	if err == nil {
		t.Fatal("删除失败必须冒泡（否则会静默不清理）")
	}
	if outcome.Processed != outboxEventsRetentionBatchSize {
		t.Fatalf("失败时也应带回已删行数，实际 %d", outcome.Processed)
	}
	if !strings.Contains(logs.String(), "outbox_retention_partial") {
		t.Fatalf("删到一半失败应留 partial 日志，实际日志: %s", logs.String())
	}
}

// 本轮被取消时：不执行任何删除，也不报错（进程排空时正在跑的一轮不该被记成失败）。
//
// 「另一副本持锁 → skipped=not_leader」那条分支与 cleanup.go 的探活清理同形，需两个锁持有者
// 才能造出来（真 Redis），留给集成用例；这里钉住的是取消路径本身。
func TestOutboxRetentionDoesNotDeleteWhenRoundCancelled(t *testing.T) {
	deleter := &fakeOutboxRetentionStore{batches: []int{5}}
	task := newOutboxRetentionForTest(OpsDeps{}, time.Hour, deleter)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, err := task.Run(ctx)
	if err != nil {
		t.Fatalf("本轮被取消不应报错（排空不是故障）: %v", err)
	}
	if deleter.calls != 0 {
		t.Fatalf("已取消的 ctx 上不得执行任何删除，实际 %d 次", deleter.calls)
	}
	if outcome.Processed != 0 {
		t.Fatalf("取消的一轮删除数应为 0，实际 %d", outcome.Processed)
	}
}

// 与 OpsTasks 的接线契约：任务名、间隔与 Run 的形状都要对得上。
func TestOutboxRetentionFitsOpsTaskContract(t *testing.T) {
	now := time.Date(2026, 3, 5, 2, 30, 0, 0, time.UTC)
	deleter := &fakeOutboxRetentionStore{batches: []int{2}}
	task := newOutboxRetentionForTest(
		OpsDeps{Now: func() time.Time { return now }},
		outboxEventsRetentionDefault,
		deleter,
	)
	opsTask := OpsTask{Name: "outbox-events-retention", Every: task.cfg.Interval, Run: task.Run}
	if opsTask.Every != outboxEventsRetentionDefaultInterval {
		t.Fatalf("缺省间隔应为 %s，实际 %s", outboxEventsRetentionDefaultInterval, opsTask.Every)
	}
	outcome, err := opsTask.Run(context.Background())
	if err != nil {
		t.Fatalf("一轮执行失败: %v", err)
	}
	if outcome.Fields["batches"] == nil || outcome.Fields["deleted"] == nil {
		t.Fatalf("Outcome.Fields 应带 batches 与 deleted 供运维看进度: %+v", outcome.Fields)
	}
}

// 出厂值语义：显式给 0 时补齐为 7 天 / 1 小时（装配侧漏传不该变成「保留 0 天」）。
func TestOutboxRetentionConfigDefaults(t *testing.T) {
	task := NewOpsOutboxEventsRetention(OpsDeps{}, OpsOutboxEventsRetentionConfig{})
	if task.cfg.Retention != 7*24*time.Hour {
		t.Fatalf("保留期缺省应为 7 天，实际 %s", task.cfg.Retention)
	}
	if task.cfg.Interval != time.Hour {
		t.Fatalf("间隔缺省应为 1 小时，实际 %s", task.cfg.Interval)
	}
	if task.deleteBatch != nil {
		t.Fatal("无连接池时不该装配删除面（应留 nil 让 Run 显式报错）")
	}
}

// OpsTasks 必须真的把它登记进去：漏登记就变成「代码写了、生产从不跑」的静默死项。
// 池是惰性连库的（store.Open 不拨号），故本用例无需外部依赖。
func TestOpsTasksRegistersOutboxRetention(t *testing.T) {
	pools, err := store.Open(context.Background(), store.Options{DSN: "postgres://placeholder@127.0.0.1:1/db"})
	if err != nil {
		t.Fatalf("建池句柄失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	deps := OpsDeps{Pools: pools}
	task := NewOpsOutboxEventsRetention(deps, OpsOutboxEventsRetentionConfig{Interval: 42 * time.Minute})
	tasks := OpsTasks(deps, OpsTaskOptions{OutboxRetention: task})

	for _, candidate := range tasks {
		if candidate.Name == "outbox-events-retention" {
			if candidate.Every != 42*time.Minute {
				t.Fatalf("间隔应取配置值 42m，实际 %s", candidate.Every)
			}
			return
		}
	}
	t.Fatalf("OpsTasks 未登记 outbox-events-retention（漏登记即生产永不执行），实际任务: %v", taskNames(tasks))
}

func taskNames(tasks []OpsTask) []string {
	names := make([]string, 0, len(tasks))
	for _, task := range tasks {
		names = append(names, task.Name)
	}
	return names
}
