package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件的用例只验证**编排语义**：哪些事件被应用、哪些被判非法、发了哪些桶增量、
// 标记了哪些已发布、重算了哪些供应商。SQL 本身的正确性由
// availproj_consume_integration_test.go 的「与 Node 真产物逐字段比对」负责。
//
// 这样分工的理由：编排错（例如把未知 outcome 也计入 excluded）与 SQL 错是两类缺陷，
// 混在一个真库用例里会互相掩盖；fake 记录调用让编排断言变得直接可读。

type recordedPublish struct {
	ids       []int64
	lastError *string
}

type fakeProjectionTx struct {
	events     []store.ProjectionOutboxEvent
	applied    map[int64]bool
	deltas     []store.ProjectionBucketDelta
	publishes  []recordedPublish
	recomputed [][]int64
	// failOnApply 让某次幂等登记报错，用来验证「错误上抛」。
	failOnApply bool
	// insertAppliedCalls / upsertBucketCalls 记录批原语的调用次数，
	// 用来钉住「一批只花一次往返」这个性能契约。
	insertAppliedCalls int
	upsertBucketCalls  int
}

func (f *fakeProjectionTx) ClaimOutboxBatch(_ context.Context, limit int) ([]store.ProjectionOutboxEvent, error) {
	if len(f.events) == 0 {
		return nil, nil
	}
	if limit > len(f.events) {
		limit = len(f.events)
	}
	claimed := f.events[:limit]
	f.events = f.events[limit:]
	return claimed, nil
}

// InsertAppliedRequests 模拟真库批形态的幂等登记：入参按 request_id 去重（首次优先），
// 返回本次新插入的集合；failOnApply 让整批失败（对应真库整条语句报错、一行不落）。
func (f *fakeProjectionTx) InsertAppliedRequests(
	_ context.Context,
	entries []store.AppliedRequestEntry,
) (map[int64]struct{}, error) {
	f.insertAppliedCalls++
	if f.failOnApply {
		return nil, errors.New("fake: 幂等登记失败")
	}
	fresh := make(map[int64]struct{}, len(entries))
	seen := make(map[int64]struct{}, len(entries))
	for _, entry := range entries {
		if _, duplicate := seen[entry.RequestID]; duplicate {
			continue
		}
		seen[entry.RequestID] = struct{}{}
		if f.applied[entry.RequestID] {
			continue
		}
		f.applied[entry.RequestID] = true
		fresh[entry.RequestID] = struct{}{}
	}
	return fresh, nil
}

func (f *fakeProjectionTx) UpsertAvailBuckets(_ context.Context, deltas []store.ProjectionBucketDelta) error {
	f.upsertBucketCalls++
	f.deltas = append(f.deltas, deltas...)
	return nil
}

func (f *fakeProjectionTx) MarkOutboxPublished(_ context.Context, ids []int64, lastError *string) error {
	cloned := make([]int64, len(ids))
	copy(cloned, ids)
	f.publishes = append(f.publishes, recordedPublish{ids: cloned, lastError: lastError})
	return nil
}

func (f *fakeProjectionTx) RecomputeAvailCurrent(_ context.Context, providerIDs []int64, _ int) error {
	cloned := make([]int64, len(providerIDs))
	copy(cloned, providerIDs)
	f.recomputed = append(f.recomputed, cloned)
	return nil
}

// fakeProjectionRunner 每轮 RunProjectionTx 都复用同一个 fake（事件队列跨批次保留）。
type fakeProjectionRunner struct {
	tx *fakeProjectionTx
}

func (r fakeProjectionRunner) RunProjectionTx(
	ctx context.Context,
	fn func(ctx context.Context, tx ProjectionTxOps) error,
) error {
	return fn(ctx, r.tx)
}

func newConsumerForTest(t *testing.T, tx *fakeProjectionTx, options ...func(*AvailProjectionConsumerOptions)) *AvailProjectionConsumer {
	t.Helper()
	opts := AvailProjectionConsumerOptions{
		runner:  fakeProjectionRunner{tx: tx},
		acquire: func(context.Context) (func(context.Context) error, bool, error) { return nil, true, nil },
		// 批间等待对单测无意义：置 0 走默认值，故这里显式给一个极小值。
		BusyDelay: time.Nanosecond,
	}
	for _, apply := range options {
		apply(&opts)
	}
	consumer, err := NewAvailProjectionConsumer(opts)
	if err != nil {
		t.Fatalf("构造消费侧失败: %v", err)
	}
	return consumer
}

func projectionEvent(id int64, eventID string, payload any) store.ProjectionOutboxEvent {
	var raw []byte
	switch typed := payload.(type) {
	case string:
		quoted, _ := json.Marshal(typed)
		raw = quoted
	case nil:
		raw = []byte("null")
	default:
		encoded, _ := json.Marshal(typed)
		raw = encoded
	}
	return store.ProjectionOutboxEvent{ID: id, EventID: eventID, Payload: raw}
}

func TestAvailConsumerProjectsSuccessAndFailureWithTruncatedLatency(t *testing.T) {
	base := "2026-09-13T00:00:00Z"
	tx := &fakeProjectionTx{applied: map[int64]bool{}}
	tx.events = []store.ProjectionOutboxEvent{
		projectionEvent(1, "11111111-1111-4111-8111-111111111101", map[string]any{
			"request_id": 9001, "provider_id": 101, "outcome": "success",
			"occurred_at": base, "duration_ms": 1500.7,
		}),
		projectionEvent(2, "11111111-1111-4111-8111-111111111102", map[string]any{
			"request_id": 9002, "provider_id": 101, "outcome": "failure",
			"occurred_at": "2026-09-13T00:00:30Z", "duration_ms": 250.9,
		}),
	}
	consumer := newConsumerForTest(t, tx)
	result, err := consumer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if result.Applied != 2 {
		t.Fatalf("应应用 2 条，实际 %d", result.Applied)
	}
	if len(tx.deltas) != 1 {
		t.Fatalf("同一分钟的两条应合并为一个桶增量，实际 %d 个", len(tx.deltas))
	}
	delta := tx.deltas[0]
	if delta.ProviderID != 101 || delta.SuccessCnt != 1 || delta.FailureCnt != 1 {
		t.Fatalf("桶计数不符: %+v", delta)
	}
	if delta.LatencyCnt != 2 || delta.LatencySumMS != 1500+250 {
		t.Fatalf("延迟应截断不四舍五入（trunc）：期望 cnt=2 sum=1750，实际 %+v", delta)
	}
	if !delta.LastRequestAt.Equal(time.Date(2026, 9, 13, 0, 0, 30, 0, time.UTC)) {
		t.Fatalf("lastRequestAt 应取最大 occurred_at，实际 %s", delta.LastRequestAt)
	}
	if delta.BucketStart.UTC().Format(time.RFC3339) != base {
		t.Fatalf("桶起点应为 occurred_at 的 UTC 分钟截断，实际 %s", delta.BucketStart.UTC().Format(time.RFC3339))
	}
	if len(tx.publishes) != 1 || len(tx.publishes[0].ids) != 2 || tx.publishes[0].lastError != nil {
		t.Fatalf("两条都应以 last_error=NULL 标记已发布，实际 %+v", tx.publishes)
	}
	if len(tx.recomputed) != 1 || len(tx.recomputed[0]) != 1 || tx.recomputed[0][0] != 101 {
		t.Fatalf("只应重算被触及的 101，实际 %+v", tx.recomputed)
	}
}

func TestAvailConsumerOutcomeBucketsOnlyCountKnownValues(t *testing.T) {
	// Node：success/failure/excluded 各计一门，**未知 outcome 三桶都不加**
	// （projection-worker.ts:318-320；这一条由 Node golden 对照揭出）。
	cases := []struct {
		name             string
		outcome          any
		wantSuccess      int
		wantFailure      int
		wantExcluded     int
		wantLatencyCnt   int
		wantLatencySumMS int64
	}{
		{name: "success 带延迟", outcome: "success", wantSuccess: 1, wantLatencyCnt: 1, wantLatencySumMS: 100},
		{name: "failure 带延迟", outcome: "failure", wantFailure: 1, wantLatencyCnt: 1, wantLatencySumMS: 100},
		{name: "excluded 不算延迟", outcome: "excluded", wantExcluded: 1},
		{name: "未知 outcome 三桶都不加", outcome: "weird", wantLatencyCnt: 0},
		{name: "缺失 outcome 视为 excluded", outcome: nil, wantExcluded: 1},
		{name: "falsy outcome 视为 excluded", outcome: false, wantExcluded: 1},
		{name: "零值 outcome 视为 excluded", outcome: float64(0), wantExcluded: 1},
	}

	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tx := &fakeProjectionTx{applied: map[int64]bool{}}
			payload := map[string]any{
				"request_id": 9100 + index, "provider_id": 201,
				"occurred_at": "2026-09-13T00:00:00Z", "duration_ms": 100.9,
			}
			if testCase.outcome != nil {
				payload["outcome"] = testCase.outcome
			}
			tx.events = []store.ProjectionOutboxEvent{
				projectionEvent(1, "11111111-1111-4111-8111-111111111201", payload),
			}
			consumer := newConsumerForTest(t, tx)
			if _, err := consumer.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce 失败: %v", err)
			}
			if len(tx.deltas) != 1 {
				t.Fatalf("应产生 1 个桶增量，实际 %d 个", len(tx.deltas))
			}
			delta := tx.deltas[0]
			if delta.SuccessCnt != testCase.wantSuccess || delta.FailureCnt != testCase.wantFailure ||
				delta.ExcludedCnt != testCase.wantExcluded {
				t.Fatalf("计数不符：期望 s=%d f=%d e=%d，实际 %+v",
					testCase.wantSuccess, testCase.wantFailure, testCase.wantExcluded, delta)
			}
			if delta.LatencyCnt != testCase.wantLatencyCnt || delta.LatencySumMS != testCase.wantLatencySumMS {
				t.Fatalf("延迟计数不符：期望 cnt=%d sum=%d，实际 %+v",
					testCase.wantLatencyCnt, testCase.wantLatencySumMS, delta)
			}
		})
	}
}

func TestAvailConsumerMarksInvalidRowsWithoutBlockingOthers(t *testing.T) {
	tx := &fakeProjectionTx{applied: map[int64]bool{}}
	tx.events = []store.ProjectionOutboxEvent{
		// 缺 request_id
		projectionEvent(1, "11111111-1111-4111-8111-111111111301", map[string]any{
			"provider_id": 301, "outcome": "success", "occurred_at": "2026-09-13T00:00:00Z",
		}),
		// payload 是字符串而非对象（合法 jsonb，但解析后不是对象）
		projectionEvent(2, "11111111-1111-4111-8111-111111111302", "not-an-object-json"),
		// 合法的一条：毒丸不得阻塞它
		projectionEvent(3, "11111111-1111-4111-8111-111111111303", map[string]any{
			"request_id": 9303, "provider_id": 301, "outcome": "success",
			"occurred_at": "2026-09-13T00:00:00Z", "duration_ms": 5,
		}),
	}
	consumer := newConsumerForTest(t, tx)
	result, err := consumer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if result.Applied != 1 {
		t.Fatalf("只应有 1 条被真正应用，实际 %d", result.Applied)
	}
	if len(tx.publishes) != 2 {
		t.Fatalf("应分别标记「合法」与「非法」两批，实际 %d 批：%+v", len(tx.publishes), tx.publishes)
	}
	invalid := tx.publishes[1]
	if len(invalid.ids) != 2 || invalid.lastError == nil || *invalid.lastError != "invalid payload" {
		t.Fatalf("非法行应以 last_error='invalid payload' 标记，实际 %+v", invalid)
	}
	if len(tx.recomputed) != 1 || len(tx.recomputed[0]) != 1 {
		t.Fatalf("只应重算被触及的 301，实际 %+v", tx.recomputed)
	}
}

func TestAvailConsumerSkipsDuplicateDeliveryWithoutDoubleCounting(t *testing.T) {
	tx := &fakeProjectionTx{applied: map[int64]bool{}}
	tx.events = []store.ProjectionOutboxEvent{
		projectionEvent(1, "11111111-1111-4111-8111-111111111401", map[string]any{
			"request_id": 9401, "provider_id": 401, "outcome": "success",
			"occurred_at": "2026-09-13T00:00:00Z", "duration_ms": 10,
		}),
		// 同一 request_id 的重复投递（不同 event_id）：不得重复计
		projectionEvent(2, "11111111-1111-4111-8111-111111111402", map[string]any{
			"request_id": 9401, "provider_id": 401, "outcome": "success",
			"occurred_at": "2026-09-13T00:01:00Z", "duration_ms": 10,
		}),
	}
	consumer := newConsumerForTest(t, tx)
	result, err := consumer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if result.Applied != 1 {
		t.Fatalf("重复投递只应计一次，实际 %d", result.Applied)
	}
	if len(tx.deltas) != 1 {
		t.Fatalf("重复投递不得新增桶增量，实际 %d 个", len(tx.deltas))
	}
	// 两条 outbox 行都仍要标记已发布（Node 同判：否则会被反复认领）。
	marked := 0
	for _, publish := range tx.publishes {
		marked += len(publish.ids)
	}
	if marked != 2 {
		t.Fatalf("两条 outbox 行都应标记已发布，实际标记 %d 条", marked)
	}
}

func TestAvailConsumerReturnsErrorWhenBatchFails(t *testing.T) {
	tx := &fakeProjectionTx{applied: map[int64]bool{}, failOnApply: true}
	tx.events = []store.ProjectionOutboxEvent{
		projectionEvent(1, "11111111-1111-4111-8111-111111111501", map[string]any{
			"request_id": 9501, "provider_id": 501, "outcome": "success",
			"occurred_at": "2026-09-13T00:00:00Z",
		}),
	}
	consumer := newConsumerForTest(t, tx)
	if _, err := consumer.RunOnce(context.Background()); err == nil {
		t.Fatal("批内失败应上抛错误（真库下由事务回滚释放认领）")
	}
	if len(tx.publishes) != 0 {
		t.Fatalf("失败时不得标记已发布，实际 %+v", tx.publishes)
	}
}

func TestAvailConsumerSkipsWhenLockHeld(t *testing.T) {
	tx := &fakeProjectionTx{applied: map[int64]bool{}}
	tx.events = []store.ProjectionOutboxEvent{
		projectionEvent(1, "11111111-1111-4111-8111-111111111601", map[string]any{
			"request_id": 9601, "provider_id": 601, "outcome": "success",
			"occurred_at": "2026-09-13T00:00:00Z",
		}),
	}
	consumer := newConsumerForTest(t, tx, func(options *AvailProjectionConsumerOptions) {
		options.acquire = func(context.Context) (func(context.Context) error, bool, error) { return nil, false, nil }
	})
	result, err := consumer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if !result.Skipped || result.Applied != 0 {
		t.Fatalf("未取得锁应整轮跳过，实际 %+v", result)
	}
	if tx.applied[9601] {
		t.Fatal("未取得锁时不得应用任何事件")
	}
}

func TestAvailConsumerRunsUpToMaxBatchesThenStops(t *testing.T) {
	// 3 批 × 每批 2 条 == batchSize，故循环应继续；maxBatches=2 时恰好停在 2 批。
	tx := &fakeProjectionTx{applied: map[int64]bool{}}
	for index := 0; index < 6; index++ {
		tx.events = append(tx.events, projectionEvent(int64(index+1), eventIDForIndex(index), map[string]any{
			"request_id": 9700 + index, "provider_id": 701, "outcome": "success",
			"occurred_at": "2026-09-13T00:00:00Z",
		}))
	}
	consumer := newConsumerForTest(t, tx, func(options *AvailProjectionConsumerOptions) {
		options.BatchSize = 2
		options.MaxBatches = 2
	})
	result, err := consumer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if result.Batches != 2 || result.Applied != 4 {
		t.Fatalf("应跑满 2 批共 4 条，实际 %+v", result)
	}
}

// blockingProjectionRunner 让第一个 RunOnce 停在事务里，直到测试放行。
//
// 为什么要它：真实并发下「第二次调用是否恰好落在第一次执行期间」是时序问题，
// 用 4 个 goroutine 裸跑会时快时慢（本用例初版因此在全包并行时偶发失败）。
// 把第一次钉在临界区里，重叠就从「大概率」变成「必然」。
type blockingProjectionRunner struct {
	entered chan struct{}
	release chan struct{}
}

func (r blockingProjectionRunner) RunProjectionTx(
	ctx context.Context,
	fn func(ctx context.Context, tx ProjectionTxOps) error,
) error {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	<-r.release
	return fn(ctx, &fakeProjectionTx{applied: map[int64]bool{}})
}

func TestAvailConsumerIsSingleFlightAcrossConcurrentRuns(t *testing.T) {
	runner := blockingProjectionRunner{entered: make(chan struct{}, 1), release: make(chan struct{})}
	consumer, err := NewAvailProjectionConsumer(AvailProjectionConsumerOptions{
		runner:  runner,
		acquire: func(context.Context) (func(context.Context) error, bool, error) { return nil, true, nil },
	})
	if err != nil {
		t.Fatalf("构造消费侧失败: %v", err)
	}

	firstDone := make(chan *AvailProjectionConsumeResult, 1)
	go func() {
		result, _ := consumer.RunOnce(context.Background())
		firstDone <- result
	}()
	<-runner.entered // 第一个调用已进入临界区

	// 其余 3 次调用必须被单例挡下（复刻 Node 的 currentPromise 语义）。
	skipped := 0
	for index := 0; index < 3; index++ {
		result, err := consumer.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("并发调用不应报错: %v", err)
		}
		if result.Skipped {
			skipped++
		}
	}
	if skipped != 3 {
		t.Fatalf("临界区内的 3 次调用都该被挡下，实际 %d", skipped)
	}

	close(runner.release)
	first := <-firstDone
	if first == nil || first.Skipped {
		t.Fatalf("第一个调用不该被跳过: %+v", first)
	}
}

func eventIDForIndex(index int) string {
	const template = "11111111-1111-4111-8111-11111111170"
	return template + string(rune('0'+index))
}

// TestAsProjectionTimestampAcceptsProductionShapes 钉住「生产形态的时刻」能被解析。
//
// 为何值得单列一例：事件载荷由触发器 `jsonb_build_object('occurred_at', mr.created_at)` 产生，
// 即 `to_jsonb(timestamptz)` 的形态——**带时区偏移**（如 `+08:00`），不必是 `Z`。
// 解析器现在只认 `time.RFC3339Nano`；若哪天换成更窄的 layout，真实事件会被静默判成毒丸
// （不报错、只丢投影），而这恰是最难从日志里看出来的失效：没有异常，只有面板不再前进。
func TestAsProjectionTimestampAcceptsProductionShapes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want time.Time
		ok   bool
	}{
		{"UTC Z", "2026-09-13T01:02:30Z", time.Date(2026, 9, 13, 1, 2, 30, 0, time.UTC), true},
		{"带 +08:00 偏移（生产形态）", "2026-09-13T09:02:30+08:00", time.Date(2026, 9, 13, 1, 2, 30, 0, time.UTC), true},
		{"带小数秒", "2026-09-13T01:02:30.123456Z", time.Date(2026, 9, 13, 1, 2, 30, 123456000, time.UTC), true},
		{"两侧空白", "  2026-09-13T01:02:30Z  ", time.Date(2026, 9, 13, 1, 2, 30, 0, time.UTC), true},
		{"非字符串", 1789227750, time.Time{}, false},
		{"空串", "", time.Time{}, false},
		{"空格分隔（非 RFC 3339）", "2026-09-13 01:02:30", time.Time{}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := asProjectionTimestamp(testCase.in)
			if ok != testCase.ok {
				t.Fatalf("可解析性应为 %t，实际 %t（输入 %#v）", testCase.ok, ok, testCase.in)
			}
			if !testCase.ok {
				return
			}
			if !got.Equal(testCase.want) {
				t.Fatalf("解析结果应为 %s，实际 %s", testCase.want, got)
			}
		})
	}
}
