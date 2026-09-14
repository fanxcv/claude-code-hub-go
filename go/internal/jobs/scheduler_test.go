package jobs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 调度器的三条硬约束各有钉子：单例（不并发）、超时（能取消）、失败不 panic 且继续调度。

func TestSchedulerRunsTaskOnceAndSkipsOverlap(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	scheduler := NewScheduler(SchedulerOptions{Stagger: time.Nanosecond}) // 覆盖默认错峰，测试即时
	var running atomic.Int32
	var maxConcurrent atomic.Int32

	err := scheduler.Register(Task{
		Name: "blocking",
		Run: func(context.Context) error {
			if current := running.Add(1); current > maxConcurrent.Load() {
				maxConcurrent.Store(current)
			}
			started <- struct{}{}
			<-release
			running.Add(-1)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("登记失败: %v", err)
	}

	wait := &sync.WaitGroup{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduler.Start(ctx, wait)

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("任务未启动")
	}

	// 第二次调用必须被跳过（返回 nil，不排队、不并发）。
	if err := scheduler.RunNow(context.Background(), "blocking"); err != nil {
		t.Fatalf("并发时的 RunNow 应静默跳过, 实际 %v", err)
	}
	if maxConcurrent.Load() != 1 {
		t.Fatalf("同一任务不得并发执行, 实际最大并发 %d", maxConcurrent.Load())
	}

	stats := scheduler.Stats()
	if len(stats) != 1 || stats[0].Skips != 1 {
		t.Fatalf("应记录一次跳过, 实际 %+v", stats)
	}
	close(release)
	cancel()
	wait.Wait()
}

func TestSchedulerTimeoutCancelsTaskContext(t *testing.T) {
	var observed error
	done := make(chan struct{})
	scheduler := NewScheduler(SchedulerOptions{Stagger: time.Nanosecond})
	err := scheduler.Register(Task{
		Name:    "slow",
		Timeout: 50 * time.Millisecond,
		Run: func(ctx context.Context) error {
			<-ctx.Done()
			observed = ctx.Err()
			close(done)
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("登记失败: %v", err)
	}

	err = scheduler.RunNow(context.Background(), "slow")
	if err == nil {
		t.Fatal("超时必须上报为错误")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("任务上下文未被取消")
	}
	if !errors.Is(observed, context.DeadlineExceeded) {
		t.Fatalf("任务应看到 DeadlineExceeded, 实际 %v", observed)
	}
}

func TestSchedulerFailureDoesNotStopSubsequentRuns(t *testing.T) {
	var calls atomic.Int32
	scheduler := NewScheduler(SchedulerOptions{Stagger: time.Nanosecond})
	err := scheduler.Register(Task{
		Name: "flaky",
		Run: func(context.Context) error {
			if calls.Add(1) == 1 {
				return errors.New("首轮失败（测试注入）")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("登记失败: %v", err)
	}

	if err := scheduler.RunNow(context.Background(), "flaky"); err == nil {
		t.Fatal("首轮应返回错误")
	}
	if err := scheduler.RunNow(context.Background(), "flaky"); err != nil {
		t.Fatalf("第二轮不应被首轮失败影响: %v", err)
	}
	stats := scheduler.Stats()
	if stats[0].Runs != 2 || stats[0].Failures != 1 {
		t.Fatalf("计数不符: %+v", stats)
	}
}

func TestSchedulerRejectsDuplicateNamesAndUnknownTasks(t *testing.T) {
	scheduler := NewScheduler(SchedulerOptions{})
	task := Task{Name: "dup", Run: func(context.Context) error { return nil }}
	if err := scheduler.Register(task); err != nil {
		t.Fatalf("首次登记应成功: %v", err)
	}
	if err := scheduler.Register(task); err == nil {
		t.Fatal("重名登记必须报错")
	}
	if err := scheduler.Register(Task{Name: "no-run"}); err == nil {
		t.Fatal("缺执行体必须报错")
	}
	if err := scheduler.RunNow(context.Background(), "missing"); err == nil {
		t.Fatal("未登记的任务必须报错")
	}
	if names := scheduler.Names(); len(names) != 1 || names[0] != "dup" {
		t.Fatalf("任务名清单不符: %v", names)
	}
}

func TestSchedulerRepeatsOnIntervalAndStopsOnCancel(t *testing.T) {
	var runs atomic.Int32
	done := make(chan struct{})
	scheduler := NewScheduler(SchedulerOptions{Stagger: time.Nanosecond})
	err := scheduler.Register(Task{
		Name:     "ticking",
		Interval: 10 * time.Millisecond,
		Run: func(context.Context) error {
			if runs.Add(1) >= 3 {
				select {
				case <-done:
				default:
					close(done)
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("登记失败: %v", err)
	}

	wait := &sync.WaitGroup{}
	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Start(ctx, wait)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("任务未按间隔重复执行")
	}
	cancel()
	wait.Wait()
	canceledRuns := runs.Load()

	// 取消后不再执行。
	time.Sleep(50 * time.Millisecond)
	if runs.Load() != canceledRuns {
		t.Fatalf("取消后仍在执行: %d -> %d", canceledRuns, runs.Load())
	}
}
