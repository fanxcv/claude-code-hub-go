package pubstatus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 接收器（`rollup_recorder.go`）的两件事必须钉住：
//  1. **分组缓存 TTL**：成功且非空 30 秒、空或失败 5 秒——这两个数字直接决定「配置生效多久后
//     公开页跟上」，也与 Node 的常量逐字对应（rollup-store.ts:28-29）；
//  2. **不返回错误**：任何失败只落计数与 warn（旁路不得影响结算）。这条在
//     `internal/terminal` 侧有端到端反证（TestSettleRollupFailureDoesNotAffectSettlement），
//     这里补「读不到分组」与「Redis 写失败」两条本地分支。

// fakeGroupStore 是只实现读路径的 PublicStatusStore 替身。
//
// gets 由 mu 保护：并发用例（TestSnapshotGroupSourceConcurrentGroups）会多 goroutine 同时读。
type fakeGroupStore struct {
	mu          sync.Mutex
	internalRaw string
	gets        int
}

func (s *fakeGroupStore) Ready(context.Context) bool { return true }
func (s *fakeGroupStore) Get(_ context.Context, key string) (string, bool) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	if key == "public-status:v2:config-version:current" {
		return "cfg-1", true
	}
	if key == "public-status:v2:config-internal:cfg-1" {
		if s.internalRaw == "" {
			return "", false
		}
		return s.internalRaw, true
	}
	return "", false
}
func (s *fakeGroupStore) PTTL(context.Context, string) (time.Duration, error) { return -1, nil }
func (s *fakeGroupStore) SetEX(context.Context, string, string, time.Duration) error {
	return nil
}
func (s *fakeGroupStore) SetPX(context.Context, string, string, time.Duration) error {
	return nil
}
func (s *fakeGroupStore) Set(context.Context, string, string) error { return nil }

// failingWriter 记录写入尝试并总是失败（模拟 Redis 写失败）。
type failingWriter struct{ calls int }

func (w *failingWriter) ApplyBatch(context.Context, string, string, string, []RollupIncrement, int) error {
	w.calls++
	return errors.New("redis down")
}

// countingWriter 记录成功写入的增量条数。
type countingWriter struct{ fields map[string]float64 }

func (w *countingWriter) ApplyBatch(_ context.Context, key, _ string, _ string, increments []RollupIncrement, _ int) error {
	if w.fields == nil {
		w.fields = map[string]float64{}
	}
	for _, increment := range increments {
		w.fields[key+"\x00"+BuildRollupField(increment.GroupID, increment.ModelKey, increment.Metric)] += increment.Value
	}
	return nil
}

const recorderTestInternalSnapshot = `{
  "configVersion": "cfg-1", "siteTitle": "t", "siteDescription": "",
  "timeZone": "Asia/Shanghai", "defaultIntervalMinutes": 15, "defaultRangeHours": 24,
  "groups": [{"sourceGroupId": 1, "sourceGroupName": "default", "slug": "default",
    "displayName": "Default", "sortOrder": 0, "description": null,
    "models": [{"publicModelKey": "m-1", "label": "M1", "vendorIconKey": "", "requestTypeBadge": "chat"}]}],
  "generatedAt": "2026-09-13T00:00:00.000Z"
}`

// TestSnapshotGroupSourceCachesPerNodeTTL 钉住 30s / 5s 两档缓存。
func TestSnapshotGroupSourceCachesPerNodeTTL(t *testing.T) {
	store := &fakeGroupStore{internalRaw: recorderTestInternalSnapshot}
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	source := NewSnapshotGroupSource(store, "", func() time.Time { return now }, nil)

	groups, ok := source.Groups(context.Background())
	if !ok || len(groups) != 1 {
		t.Fatalf("首次读取应得到 1 个分组，实际 ok=%v groups=%d", ok, len(groups))
	}
	readsAfterFirst := store.gets

	// 30 秒内不再读存储。
	now = now.Add(29 * time.Second)
	if _, _ = source.Groups(context.Background()); store.gets != readsAfterFirst {
		t.Fatalf("29 秒时应命中缓存，实际又读了 %d 次", store.gets-readsAfterFirst)
	}
	// 超过 30 秒重读。
	now = now.Add(2 * time.Second)
	if _, _ = source.Groups(context.Background()); store.gets == readsAfterFirst {
		t.Fatal("31 秒后应重新读取存储")
	}
}

// TestSnapshotGroupSourceShortTTLWhenEmpty 钉住「读空用 5 秒短 TTL」。
func TestSnapshotGroupSourceShortTTLWhenEmpty(t *testing.T) {
	store := &fakeGroupStore{} // 没有内部快照 → 读不到
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	source := NewSnapshotGroupSource(store, "", func() time.Time { return now }, nil)

	if _, ok := source.Groups(context.Background()); ok {
		t.Fatal("读不到内部快照时应返回 ok=false（调用方据此跳过写入）")
	}
	reads := store.gets
	now = now.Add(4 * time.Second)
	if _, _ = source.Groups(context.Background()); store.gets != reads {
		t.Fatal("4 秒时应命中短 TTL 缓存")
	}
	now = now.Add(2 * time.Second)
	if _, _ = source.Groups(context.Background()); store.gets == reads {
		t.Fatal("6 秒后应重试读取（短 TTL 到期）")
	}
}

// TestRecorderWritesAndCounts 钉住成功路径的计数与字段形态。
func TestRecorderWritesAndCounts(t *testing.T) {
	store := &fakeGroupStore{internalRaw: recorderTestInternalSnapshot}
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	metrics := &RecorderMetrics{}
	writer := &countingWriter{}
	recorder := NewRollupRecorder(
		writer,
		NewSnapshotGroupSource(store, "", func() time.Time { return now }, nil),
		"",
		nil,
		metrics,
	)
	reason := "request_success"
	groupTag := "default"
	model := "m-1"
	recorder.RecordTerminal(context.Background(), RollupEvent{
		CreatedAt:     now,
		Model:         &model,
		DurationMs:    floatPtr(1000),
		TTFTMs:        floatPtr(300), // 有 TTFT 才有 ttfb_sum/ttfb_count 两条
		FirstByteMs:   floatPtr(100),
		OutputTokens:  int64PtrValue(100),
		ProviderChain: []ProviderChainItem{{Reason: &reason, GroupTag: &groupTag}},
	})

	if metrics.Written != 1 || metrics.Skipped != 0 || metrics.Failed != 0 {
		t.Fatalf("应记 written=1，实际 %+v", *metrics)
	}
	if len(writer.fields) != 5 {
		t.Fatalf("成功事件应写 5 条增量（success + ttfb_sum/count + tps_sum/count），实际 %d：%v",
			len(writer.fields), writer.fields)
	}
}

// TestRecorderSwallowsWriteFailure 钉住「写失败只记计数，不冒泡」。
func TestRecorderSwallowsWriteFailure(t *testing.T) {
	store := &fakeGroupStore{internalRaw: recorderTestInternalSnapshot}
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	metrics := &RecorderMetrics{}
	recorder := NewRollupRecorder(
		&failingWriter{},
		NewSnapshotGroupSource(store, "", func() time.Time { return now }, nil),
		"",
		nil,
		metrics,
	)
	reason := "request_success"
	groupTag := "default"
	model := "m-1"
	recorder.RecordTerminal(context.Background(), RollupEvent{
		CreatedAt:     now,
		Model:         &model,
		ProviderChain: []ProviderChainItem{{Reason: &reason, GroupTag: &groupTag}},
	})
	if metrics.Failed != 1 {
		t.Fatalf("Redis 写失败应记 failed=1，实际 %+v", *metrics)
	}
	if metrics.LastError == "" {
		t.Fatal("失败原因应留在计数面里（旁路不冒泡，只有计数可见）")
	}
}

// TestRecorderSkipsWhenGroupsUnavailable 钉住「分组读不到就不写」。
func TestRecorderSkipsWhenGroupsUnavailable(t *testing.T) {
	metrics := &RecorderMetrics{}
	writer := &countingWriter{}
	recorder := NewRollupRecorder(writer, NewSnapshotGroupSource(&fakeGroupStore{}, "", time.Now, nil), "", nil, metrics)
	model := "m-1"
	recorder.RecordTerminal(context.Background(), RollupEvent{CreatedAt: time.Now(), Model: &model})
	if metrics.Skipped != 1 || metrics.Written != 0 {
		t.Fatalf("分组读不到应记 skipped=1，实际 %+v", *metrics)
	}
	if len(writer.fields) != 0 {
		t.Fatal("分组读不到时不该写任何字段")
	}
}

func floatPtr(value float64) *float64  { return &value }
func int64PtrValue(value int64) *int64 { return &value }

// discardWriter 吞掉一切写入，供并发用例使用（countingWriter 记录 map，不是并发安全的）。
type discardWriter struct{}

func (discardWriter) ApplyBatch(context.Context, string, string, string, []RollupIncrement, int) error {
	return nil
}

// recordingLevelLogger 记录三级的调用，供状态转移上报用例断言。
//
// 刻意实现 Warn/Debug/Info 三个方法：生产 logger（logx.Logger）三个都有，而 pubstatus.Logger
// 只要求 Warn——可选面正是靠类型断言接上。
type recordingLevelLogger struct {
	mu     sync.Mutex
	events []string
}

func (l *recordingLevelLogger) record(level, event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, level+":"+event)
}

func (l *recordingLevelLogger) Warn(event string, _ map[string]any)  { l.record("warn", event) }
func (l *recordingLevelLogger) Debug(event string, _ map[string]any) { l.record("debug", event) }
func (l *recordingLevelLogger) Info(event string, _ map[string]any)  { l.record("info", event) }

func (l *recordingLevelLogger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// TestSnapshotGroupSourceReportsUnavailableByTransition 钉住快照不可用的**状态转移**上报：
// 进入时报一次 warn，持续期间只留 debug 足迹，恢复时报一次 info，之后再次不可用会再报一次。
//
// 为什么必顶：这条 warn 在生产以 emptyConfiguredGroupsCacheTTL（5s）的节奏刷屏（实测 11 条/分钟），
// 而读不到本身就分「本仓无周期性重发」与「Redis 抖动/快照键被清」两种成因，直接降成 debug
// 会把后者一并埋掉。
func TestSnapshotGroupSourceReportsUnavailableByTransition(t *testing.T) {
	store := &fakeGroupStore{} // internalRaw 为空 = 快照读不到
	base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	clock := base
	logger := &recordingLevelLogger{}
	source := NewSnapshotGroupSource(store, "", func() time.Time { return clock }, logger)

	// 第 1 次：进入不可用，应报 warn。
	if _, ok := source.Groups(context.Background()); ok {
		t.Fatal("快照缺失时应返回不可用")
	}
	if got := logger.snapshot(); len(got) != 1 || got[0] != "warn:public_status_rollup_groups_unavailable" {
		t.Fatalf("首次不可用应报一次 warn，实际 %v", got)
	}

	// 持续期：跨过 5s 空缓存 TTL 再调两次，只应有 debug 足迹，不得再加 warn。
	for i := 0; i < 2; i++ {
		clock = clock.Add(emptyConfiguredGroupsCacheTTL + time.Second)
		if _, ok := source.Groups(context.Background()); ok {
			t.Fatal("快照仍缺失时应保持不可用")
		}
	}
	got := logger.snapshot()
	warns := 0
	debugs := 0
	for _, event := range got {
		switch event {
		case "warn:public_status_rollup_groups_unavailable":
			warns++
		case "debug:public_status_rollup_groups_unavailable":
			debugs++
		}
	}
	if warns != 1 || debugs != 2 {
		t.Fatalf("持续期应只留 debug（warn=1 debug=2），实际 %v", got)
	}

	// 恢复：快照可读 -> 报一次 info，且不得再报 warn。
	store.mu.Lock()
	store.internalRaw = recorderTestInternalSnapshot
	store.mu.Unlock()
	clock = clock.Add(emptyConfiguredGroupsCacheTTL + time.Second)
	groups, ok := source.Groups(context.Background())
	if !ok || len(groups) != 1 {
		t.Fatalf("恢复后应读到 1 个分组，实际 ok=%v groups=%d", ok, len(groups))
	}
	got = logger.snapshot()
	if got[len(got)-1] != "info:public_status_rollup_groups_recovered" {
		t.Fatalf("恢复时应报一次 info，实际末尾 %q，全量 %v", got[len(got)-1], got)
	}

	// 再次不可用属新一期：应再报一次 warn（不是「整进程只报一次」）。
	store.mu.Lock()
	store.internalRaw = ""
	store.mu.Unlock()
	clock = clock.Add(configuredGroupsCacheTTL + time.Second)
	if _, ok := source.Groups(context.Background()); ok {
		t.Fatal("快照再次缺失时应报不可用")
	}
	got = logger.snapshot()
	lastWarn := 0
	for _, event := range got {
		if event == "warn:public_status_rollup_groups_unavailable" {
			lastWarn++
		}
	}
	if lastWarn != 2 {
		t.Fatalf("再次进入不可用应另报一次 warn（合计 2），实际 %d：%v", lastWarn, got)
	}
}

// TestSnapshotGroupSourceLevelsAreOptional 钉住「只实现 Warn 的 logger 不会崩、也不会降级走 warn」。
//
// 生产上 pubstatus.Logger 的必需集就是 Warn，而 terminal.RollupRecorder 之外还有别的既有替身；
// 若为此把必需集加宽，所有只实现 Warn 的组装点会编译失败。
type warnOnlyLogger struct{ warns int }

func (l *warnOnlyLogger) Warn(string, map[string]any) { l.warns++ }

func TestSnapshotGroupSourceLevelsAreOptional(t *testing.T) {
	store := &fakeGroupStore{}
	clock := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	logger := &warnOnlyLogger{}
	source := NewSnapshotGroupSource(store, "", func() time.Time { return clock }, logger)

	for i := 0; i < 3; i++ {
		clock = clock.Add(emptyConfiguredGroupsCacheTTL + time.Second)
		if _, ok := source.Groups(context.Background()); ok {
			t.Fatal("快照缺失时应返回不可用")
		}
	}
	store.mu.Lock()
	store.internalRaw = recorderTestInternalSnapshot
	store.mu.Unlock()
	clock = clock.Add(emptyConfiguredGroupsCacheTTL + time.Second)
	if _, ok := source.Groups(context.Background()); !ok {
		t.Fatal("恢复后应读到分组")
	}
	if logger.warns != 1 {
		t.Fatalf("缺 Debug/Info 面时不得回落 warn，也不得多报：warns=%d", logger.warns)
	}
}

// TestSnapshotGroupSourceConcurrentGroups 钉住「分组来源并发调用无数据竞争」。
//
// 生产形态：本实例是单例（dataplane 装配一次），终态结算路径可并发调用。
// 加锁前 -race 会在 loaded/expiresAt 的读与 cached/loaded/expiresAt 的写上互报。
// 时钟每次前进 1 秒，使并发里既有命中缓存也有过期重载（两条分支都要过）。
func TestSnapshotGroupSourceConcurrentGroups(t *testing.T) {
	store := &fakeGroupStore{internalRaw: recorderTestInternalSnapshot}
	base := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	var ticks int64
	source := NewSnapshotGroupSource(store, "", func() time.Time {
		return base.Add(time.Duration(atomic.AddInt64(&ticks, 1)) * time.Second)
	}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				groups, ok := source.Groups(context.Background())
				if !ok || len(groups) != 1 {
					t.Errorf("并发读取应稳定得到 1 个分组，实际 ok=%v groups=%d", ok, len(groups))
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestRecorderMetricsConcurrent 钉住「计数面在并发结算下不竞争且不丢计数」。
//
// 生产形态：接收器是单例，多个终态结算 goroutine 同时打它；观测方可随时读计数。
// 加锁前 -race 会报 count() 的写与 Metrics() 的读竞争。
func TestRecorderMetricsConcurrent(t *testing.T) {
	store := &fakeGroupStore{internalRaw: recorderTestInternalSnapshot}
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	metrics := &RecorderMetrics{}
	recorder := NewRollupRecorder(
		discardWriter{},
		NewSnapshotGroupSource(store, "", func() time.Time { return now }, nil),
		"",
		nil,
		metrics,
	)

	const events = 200
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < events/8; j++ {
				reason := "request_success"
				groupTag := "default"
				model := "m-1"
				recorder.RecordTerminal(context.Background(), RollupEvent{
					CreatedAt:     now,
					Model:         &model,
					ProviderChain: []ProviderChainItem{{Reason: &reason, GroupTag: &groupTag}},
				})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			_ = recorder.Metrics()
		}
	}()
	wg.Wait()

	if got := recorder.Metrics(); got.Written != events || got.Skipped != 0 || got.Failed != 0 {
		t.Fatalf("并发结算后应记 written=%d，实际 %+v", events, got)
	}
}
