package pubstatus

import (
	"context"
	"errors"
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
type fakeGroupStore struct {
	internalRaw string
	gets        int
}

func (s *fakeGroupStore) Ready(context.Context) bool { return true }
func (s *fakeGroupStore) Get(_ context.Context, key string) (string, bool) {
	s.gets++
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

func (w *failingWriter) HIncrByFloat(context.Context, string, string, float64) error {
	w.calls++
	return errors.New("redis down")
}
func (w *failingWriter) SetNX(context.Context, string, string) (bool, error) {
	return false, errors.New("redis down")
}
func (w *failingWriter) Expire(context.Context, string, int) error { return errors.New("redis down") }

// countingWriter 记录成功写入的增量条数。
type countingWriter struct{ fields map[string]float64 }

func (w *countingWriter) HIncrByFloat(_ context.Context, key, field string, increment float64) error {
	if w.fields == nil {
		w.fields = map[string]float64{}
	}
	w.fields[key+"\x00"+field] += increment
	return nil
}
func (w *countingWriter) SetNX(context.Context, string, string) (bool, error) { return true, nil }
func (w *countingWriter) Expire(context.Context, string, int) error           { return nil }

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
