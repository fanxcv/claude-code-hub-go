package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// fakeBackfillStore 记录回填对 store 的调用，用来在无库条件下断言分块与幂等语义。
type fakeBackfillStore struct {
	done bool
	// insertedPerChunk 决定每个分块返回的入队行数。
	insertedPerChunk int
	failEnqueue      error

	chunks       []string
	markerCalls  int
	lastRangeDay int
	lastInserted int
}

func (f *fakeBackfillStore) ProjectionBackfillDone(context.Context) (bool, error) { return f.done, nil }

func (f *fakeBackfillStore) EnqueueProjectionBackfillChunk(_ context.Context, fromISO, toISO string) (int, error) {
	if f.failEnqueue != nil {
		return 0, f.failEnqueue
	}
	f.chunks = append(f.chunks, fromISO+"|"+toISO)
	return f.insertedPerChunk, nil
}

func (f *fakeBackfillStore) MarkProjectionBackfillDone(_ context.Context, rangeDays, inserted int) error {
	f.markerCalls++
	f.lastRangeDay = rangeDays
	f.lastInserted = inserted
	f.done = true
	return nil
}

func newBackfillFixture(storeImpl backfillStore, now time.Time, acquire lockAcquirer) *AvailBackfill {
	backfill, err := NewAvailBackfill(AvailBackfillOptions{
		// Pools 是装配必填项（生产守卫）；store 才是本测试要替换的写入面。
		Pools:      &store.Pools{},
		store:      storeImpl,
		acquire:    acquire,
		RangeDays:  1,
		ChunkHours: 6,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		panic(err)
	}
	return backfill
}

func allowAllLocks() lockAcquirer {
	return func(context.Context) (func(context.Context) error, bool, error) {
		return func(context.Context) error { return nil }, true, nil
	}
}

func TestAvailBackfillEnqueuesLeftClosedRightOpenChunks(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	fake := &fakeBackfillStore{insertedPerChunk: 7}
	backfill := newBackfillFixture(fake, now, allowAllLocks())

	result, err := backfill.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("回填失败: %v", err)
	}
	// 1 天 / 6 小时 = 4 块。
	if len(fake.chunks) != 4 || result.Chunks != 4 {
		t.Fatalf("应分成 4 块, 实际 %d（结果 %d）", len(fake.chunks), result.Chunks)
	}
	if result.Inserted != 28 {
		t.Fatalf("入队总数应为 4x7=28, 实际 %d", result.Inserted)
	}
	wantFirst := now.Add(-24*time.Hour).Format(time.RFC3339Nano) + "|" + now.Add(-18*time.Hour).Format(time.RFC3339Nano)
	if fake.chunks[0] != wantFirst {
		t.Fatalf("首块窗口应为 %s, 实际 %s", wantFirst, fake.chunks[0])
	}
	// 末块右端必须收敛到 now（左闭右开，不能越界到未来）。
	if !strings.HasSuffix(fake.chunks[3], "|"+now.Format(time.RFC3339Nano)) {
		t.Fatalf("末块右端应为 now, 实际 %s", fake.chunks[3])
	}
	if fake.markerCalls != 1 || fake.lastRangeDay != 1 || fake.lastInserted != 28 {
		t.Fatalf("完成标记应记 rangeDays=1/inserted=28, 实际 calls=%d range=%d inserted=%d",
			fake.markerCalls, fake.lastRangeDay, fake.lastInserted)
	}
}

func TestAvailBackfillIsIdempotentViaMarker(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	fake := &fakeBackfillStore{insertedPerChunk: 1}
	backfill := newBackfillFixture(fake, now, allowAllLocks())

	if _, err := backfill.RunOnce(context.Background()); err != nil {
		t.Fatalf("首轮失败: %v", err)
	}
	firstChunks := len(fake.chunks)

	// 第二轮：标记已写 → 直接跳过，不再入队、不再写标记。
	result, err := backfill.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	if !result.Skipped || result.SkippedReason != "already_done" {
		t.Fatalf("第二轮应因标记跳过, 实际 %+v", result)
	}
	if len(fake.chunks) != firstChunks {
		t.Fatalf("第二轮不得再入队, 实际分块数 %d", len(fake.chunks))
	}
	if fake.markerCalls != 1 {
		t.Fatalf("标记只应写一次, 实际 %d", fake.markerCalls)
	}
}

func TestAvailBackfillSkipsWhenLockHeld(t *testing.T) {
	fake := &fakeBackfillStore{}
	backfill := newBackfillFixture(fake, time.Now(), func(context.Context) (func(context.Context) error, bool, error) {
		return nil, false, nil
	})

	result, err := backfill.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("未取得锁不应报错: %v", err)
	}
	if !result.Skipped || result.SkippedReason != "lock_held" {
		t.Fatalf("应因锁跳过, 实际 %+v", result)
	}
	if len(fake.chunks) != 0 || fake.markerCalls != 0 {
		t.Fatal("未取得锁时不得入队或写标记")
	}
}

func TestAvailBackfillInterruptedDoesNotWriteMarker(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	fake := &fakeBackfillStore{}
	backfill := newBackfillFixture(fake, now, allowAllLocks())

	ctx, cancel := context.WithCancel(context.Background())
	// 第一块入队后立即取消：中断必须放弃写标记，下次启动重跑。
	backfill.store = &cancelAfterFirstChunk{inner: fake, cancel: cancel}

	result, err := backfill.RunOnce(ctx)
	if err != nil {
		t.Fatalf("中断不应报错: %v", err)
	}
	if !result.Interrupted {
		t.Fatalf("应标记为中断, 实际 %+v", result)
	}
	if fake.markerCalls != 0 {
		t.Fatal("中断时不得写完成标记")
	}
	if len(fake.chunks) > 1 {
		t.Fatalf("中断后不得继续入队, 实际分块数 %d", len(fake.chunks))
	}
}

// cancelAfterFirstChunk 在首次入队后取消上下文，模拟进程收到终止信号。
type cancelAfterFirstChunk struct {
	inner  *fakeBackfillStore
	cancel context.CancelFunc
}

func (c *cancelAfterFirstChunk) ProjectionBackfillDone(ctx context.Context) (bool, error) {
	return c.inner.ProjectionBackfillDone(ctx)
}

func (c *cancelAfterFirstChunk) EnqueueProjectionBackfillChunk(ctx context.Context, fromISO, toISO string) (int, error) {
	inserted, err := c.inner.EnqueueProjectionBackfillChunk(ctx, fromISO, toISO)
	c.cancel()
	return inserted, err
}

func (c *cancelAfterFirstChunk) MarkProjectionBackfillDone(ctx context.Context, rangeDays, inserted int) error {
	return c.inner.MarkProjectionBackfillDone(ctx, rangeDays, inserted)
}

func TestAvailBackfillPropagatesEnqueueFailure(t *testing.T) {
	fake := &fakeBackfillStore{failEnqueue: errors.New("入队失败（测试注入）")}
	backfill := newBackfillFixture(fake, time.Now(), allowAllLocks())

	result, err := backfill.RunOnce(context.Background())
	if err == nil {
		t.Fatal("入队失败必须上报")
	}
	if result == nil || result.Inserted != 0 {
		t.Fatalf("失败时应返回已完成的计数, 实际 %+v", result)
	}
	if fake.markerCalls != 0 {
		t.Fatal("失败时不得写完成标记")
	}
}
