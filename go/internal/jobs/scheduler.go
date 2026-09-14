package jobs

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// Scheduler 是后台任务的唯一驱动：固定间隔、单例执行、每任务超时、启动错峰。
//
// 为什么不用 cron 表达式：Node 侧这两个任务都是固定间隔（价格同步 30 分钟一次、
// 回填在启动时一次），没有日历语义；引入 cron 解析只会多一个可错的输入面。
//
// 三条硬约束（与 Node 的差别写在 README）：
//   - **单例**：同一任务的上一次还在跑时，本次 tick 直接跳过并记 warn，不排队、不并发。
//     排队会让「一轮 5 秒的同步」在网络变慢时堆成数轮同时打库。
//   - **超时**：每任务一个 Timeout，超时取消其 ctx；任务必须尊重 ctx（回填分块会检查）。
//   - **错峰**：注册顺序决定首轮延迟（默认每档 3 秒）。价格同步要拉 28 MiB JSON、
//     回填要扫 100 天 message_request，两者同时起会让启动期的内存与 IO 峰值叠加。
type Scheduler struct {
	logger *logx.Logger
	// stagger 是相邻任务的启动延迟差。
	stagger time.Duration

	mu    sync.Mutex
	tasks []*scheduledTask
	names map[string]struct{}
}

type scheduledTask struct {
	spec     Task
	sequence int
	running  atomic.Bool
	runs     atomic.Int64
	failures atomic.Int64
	skips    atomic.Int64
}

// Task 是一个可被调度器驱动的任务定义。
type Task struct {
	// Name 是任务名（小写短横线），用于日志与重复注册检查。
	Name string
	// Interval 是两次执行之间的最小间隔；<=0 表示只跑一次（启动时）。
	Interval time.Duration
	// Timeout 是单次执行的超时上限；<=0 时取 defaultTaskTimeout。
	Timeout time.Duration
	// Run 是任务本体；必须尊重 ctx 的取消。
	Run func(ctx context.Context) error
}

// defaultTaskTimeout 是未显式指定时的单次执行上限。
//
// 价格同步要拉 28 MiB 并写约 1.1 万行，长尾可达分钟级；回填要按 6 小时一片扫 100 天。
// 取 10 分钟：足够覆盖实测（同步约 5 秒级），又不至于让一个卡死的任务永久占住调度循环。
const defaultTaskTimeout = 10 * time.Minute

// defaultStagger 是相邻任务的启动延迟差。
const defaultStagger = 3 * time.Second

// SchedulerOptions 是构造参数。
type SchedulerOptions struct {
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Stagger 覆盖默认错峰间隔（测试用 0 以便即时断言）。
	Stagger time.Duration
}

// NewScheduler 建一个空调度器。
func NewScheduler(options SchedulerOptions) *Scheduler {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	stagger := options.Stagger
	if options.Stagger == 0 {
		stagger = defaultStagger
	}
	return &Scheduler{
		logger:  logger,
		stagger: stagger,
		names:   map[string]struct{}{},
	}
}

// Register 登记一个任务；名字重复即报错（静默覆盖会让两个任务互相顶掉）。
func (s *Scheduler) Register(spec Task) error {
	if spec.Name == "" {
		return fmt.Errorf("jobs: 任务缺少名字")
	}
	if spec.Run == nil {
		return fmt.Errorf("jobs: 任务 %s 缺少执行体", spec.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, duplicate := s.names[spec.Name]; duplicate {
		return fmt.Errorf("jobs: 任务名重复: %s", spec.Name)
	}
	s.names[spec.Name] = struct{}{}
	s.tasks = append(s.tasks, &scheduledTask{spec: spec, sequence: len(s.tasks)})
	return nil
}

// Names 返回按注册顺序排列的任务名（供启动日志与测试断言）。
func (s *Scheduler) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.tasks))
	for _, task := range s.tasks {
		names = append(names, task.spec.Name)
	}
	return names
}

// Len 返回任务数量。
func (s *Scheduler) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tasks)
}

// RunNow 立即执行指定任务一次（受超时与单例约束）。
//
// 用途：启动期的首轮（价格同步的「启动即跑一次」与回填的 bootstrap）、以及测试。
func (s *Scheduler) RunNow(ctx context.Context, name string) error {
	s.mu.Lock()
	var target *scheduledTask
	for _, task := range s.tasks {
		if task.spec.Name == name {
			target = task
			break
		}
	}
	s.mu.Unlock()
	if target == nil {
		return fmt.Errorf("jobs: 未登记的任务: %s", name)
	}
	return s.execute(ctx, target)
}

// execute 在单例与超时约束下跑一次任务体；返回任务自身的错误（超时也归为错误）。
func (s *Scheduler) execute(ctx context.Context, task *scheduledTask) error {
	if !task.running.CompareAndSwap(false, true) {
		task.skips.Add(1)
		s.logger.Warn("job_tick_skipped", map[string]any{
			"job":    task.spec.Name,
			"reason": "previous_run_still_running",
			"skips":  task.skips.Load(),
		})
		return nil
	}
	defer task.running.Store(false)

	timeout := task.spec.Timeout
	if timeout <= 0 {
		timeout = defaultTaskTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	s.logger.Info("job_started", map[string]any{"job": task.spec.Name})
	err := task.spec.Run(runCtx)
	elapsed := time.Since(started)
	task.runs.Add(1)

	if err != nil {
		task.failures.Add(1)
		// 失败不 panic、不停调度：下一轮照常尝试，失败只体现在日志与计数上。
		s.logger.Error("job_failed", map[string]any{
			"job":       task.spec.Name,
			"error":     err.Error(),
			"elapsedMs": elapsed.Milliseconds(),
			"failures":  task.failures.Load(),
		})
		return err
	}
	s.logger.Info("job_finished", map[string]any{
		"job":       task.spec.Name,
		"elapsedMs": elapsed.Milliseconds(),
	})
	return nil
}

// Start 起后台循环；每个任务一个 goroutine，直到 ctx 取消。
//
// 返回后调用方仍需 Wait 才能确认全部退出。
func (s *Scheduler) Start(ctx context.Context, wait *sync.WaitGroup) {
	s.mu.Lock()
	tasks := append([]*scheduledTask(nil), s.tasks...)
	stagger := s.stagger
	s.mu.Unlock()

	for _, task := range tasks {
		wait.Add(1)
		go func(task *scheduledTask) {
			defer wait.Done()
			s.loop(ctx, task, time.Duration(task.sequence)*stagger)
		}(task)
	}
}

// loop 是单任务的调度循环：先等错峰延迟，跑首轮，之后按 Interval 重复。
func (s *Scheduler) loop(ctx context.Context, task *scheduledTask, delay time.Duration) {
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
	_ = s.execute(ctx, task)

	if task.spec.Interval <= 0 {
		return
	}

	ticker := time.NewTicker(task.spec.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.execute(ctx, task)
		}
	}
}

// Stats 返回各任务的执行计数，供日志与测试。
type Stats struct {
	Name     string
	Runs     int64
	Failures int64
	Skips    int64
}

// Stats 返回按任务名排序的执行计数。
func (s *Scheduler) Stats() []Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := make([]Stats, 0, len(s.tasks))
	for _, task := range s.tasks {
		stats = append(stats, Stats{
			Name:     task.spec.Name,
			Runs:     task.runs.Load(),
			Failures: task.failures.Load(),
			Skips:    task.skips.Load(),
		})
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Name < stats[j].Name })
	return stats
}
