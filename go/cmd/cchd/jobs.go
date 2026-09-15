// cchd 的后台任务装配：云价格同步与可用性投影回填。
//
// 与数据面的分工：任务不参与请求路径，起不来或失败都不应影响数据面可用性
// （与 Node 的 instrumentation 一致——后台任务失败只记日志）。
//
// 开关一律读 CCH_JOB* / CCH_JOBS* 这组 **Go 专有变量**（不进 go/env-parity.txt 对账清单），
// 默认全开：Node 下线后这些职责必须由 Go 承担，默认关掉会变成静默的功能缺失。
package main

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/notify"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

const (
	// defaultJobPriceSyncIntervalMS 对齐 Node 的 30 分钟。
	defaultJobPriceSyncIntervalMS = 30 * 60 * 1000
	// defaultJobAvailBackfillIntervalMS 是回填标记的复查间隔。
	defaultJobAvailBackfillIntervalMS = 5 * 60 * 1000
	// minJobIntervalMS 是间隔下限：更短的间隔只会让任务互相顶掉。
	minJobIntervalMS = 1000
	// jobStopGrace 是关闭时等待在途任务退出的上限。
	jobStopGrace = 15 * time.Second
)

// jobsOptions 是后台任务的装配缝。
type jobsOptions struct {
	Logger    *logx.Logger
	LookupEnv config.LookupEnvFunc
	// Pools 为 nil（未配置 DSN）时任务整体缺席。
	Pools *store.Pools
	// Notify 是通知调度器（由 boot 在管理面装配前建好：管理面的写路径要拿它做重排）。
	// 非 nil 时本函数把它的扫描任务注册进统一调度器——调度器与「何时跑」分开：
	// boot 只负责建对象，跑不跑、停不停由这里的 scheduler 管。
	Notify *jobs.NotifyScheduler
}

// jobsRuntime 持有调度器与它的生命周期。
type jobsRuntime struct {
	scheduler *jobs.Scheduler
	cancel    context.CancelFunc
	wait      *sync.WaitGroup
	logger    *logx.Logger
}

// startJobs 按环境开关登记并启动后台任务；未配置 DSN 或无任务开启时返回 nil。
func startJobs(ctx context.Context, options jobsOptions) (*jobsRuntime, error) {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	lookup := options.LookupEnv
	if lookup == nil {
		lookup = config.LookupFromOS()
	}

	enabled := boolEnv(lookup, "CCH_JOBS_ENABLED", true)
	priceSyncEnabled := enabled && boolEnv(lookup, "CCH_JOB_PRICE_SYNC_ENABLED", true)
	backfillEnabled := enabled && boolEnv(lookup, "CCH_JOB_AVAIL_BACKFILL_ENABLED", true)

	notifyEnabled := enabled && boolEnv(lookup, "CCH_JOB_NOTIFY_ENABLED", false)
	cacheEffectivenessEnabled := enabled && boolEnv(lookup, "CCH_JOB_CACHE_EFFECTIVENESS_ENABLED", true)

	// 守卫必须枚举**全部**任务开关：漏一个就会出现「只开该任务时整块被跳过」。
	if !priceSyncEnabled && !backfillEnabled && !cacheEffectivenessEnabled &&
		!(notifyEnabled && options.Notify != nil) {
		logger.Info("jobs_disabled", map[string]any{"master": enabled})
		return nil, nil
	}
	if options.Pools == nil {
		logger.Warn("jobs_unavailable", map[string]any{
			"reason": "DSN not configured",
			"action": "background jobs skipped",
		})
		return nil, nil
	}

	scheduler := jobs.NewScheduler(jobs.SchedulerOptions{Logger: logger})

	if priceSyncEnabled {
		syncer, err := jobs.NewPriceSyncer(jobs.PriceSyncOptions{
			Pools:  options.Pools,
			Logger: logger,
			URL:    stringEnv(lookup, "CCH_CLOUD_PRICE_TABLE_URL"),
		})
		if err != nil {
			return nil, err
		}
		interval := time.Duration(intEnv(lookup, "CCH_JOB_PRICE_SYNC_INTERVAL_MS", defaultJobPriceSyncIntervalMS)) * time.Millisecond
		if err := scheduler.Register(syncer.Task(interval)); err != nil {
			return nil, err
		}
	}

	if backfillEnabled {
		backfill, err := jobs.NewAvailBackfill(jobs.AvailBackfillOptions{
			Pools:  options.Pools,
			Logger: logger,
		})
		if err != nil {
			return nil, err
		}
		interval := time.Duration(intEnv(lookup, "CCH_JOB_AVAIL_BACKFILL_INTERVAL_MS", defaultJobAvailBackfillIntervalMS)) * time.Millisecond
		if err := scheduler.Register(backfill.Task(interval)); err != nil {
			return nil, err
		}
	}

	// 可用性投影消费侧（G1）：Node 的 projection-worker 随 Node 下线而消失，不接这一路则
	// 投影冻结（可用性面板与 avail_current 不再更新）。**刻意不挂在 backfill 开关下**——
	// 那个开关管的是历史补投，本任务管增量消费，两者都关等于投影全停。
	if options.Pools != nil {
		consumer, err := jobs.NewAvailProjectionConsumer(jobs.AvailProjectionConsumerOptions{
			Pools:  options.Pools,
			Logger: logger,
		})
		if err != nil {
			return nil, err
		}
		interval := time.Duration(intEnv(lookup, "CCH_JOB_AVAIL_PROJECTION_INTERVAL_MS", 200)) * time.Millisecond
		if err := scheduler.Register(consumer.Task(interval)); err != nil {
			return nil, err
		}
	}

	if notifyEnabled && options.Notify != nil {
		interval := time.Duration(intEnv(lookup, "CCH_JOB_NOTIFY_TICK_MS", 30*1000)) * time.Millisecond
		if err := scheduler.Register(options.Notify.Task(interval)); err != nil {
			return nil, err
		}
	}

	// 缓存命中率窗口聚合（审计缺口 G3）：Node 每 5 分钟写 provider_cache_effectiveness，
	// Go 此前只读该表 → Node 停后命中率页与排行榜系数会冻结。
	if cacheEffectivenessEnabled {
		job, err := jobs.NewCacheEffectiveness(jobs.CacheEffectivenessOptions{
			Pools:  options.Pools,
			Logger: logger,
			// 系统设置优先、env 兜底（Node proxy-runtime.ts:77-78）；env 默认 true。
			EnabledByEnv: boolEnv(lookup, "ENABLE_CACHE_EFFECTIVENESS", true),
		})
		if err != nil {
			return nil, err
		}
		interval := time.Duration(intEnv(lookup, "CCH_JOB_CACHE_EFFECTIVENESS_INTERVAL_MS",
			int(jobs.CacheEffectivenessDefaultEvery/time.Millisecond))) * time.Millisecond
		if err := scheduler.Register(job.Task(interval)); err != nil {
			return nil, err
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	wait := &sync.WaitGroup{}
	scheduler.Start(runCtx, wait)

	logger.Info("jobs_started", map[string]any{"jobs": scheduler.Names()})
	return &jobsRuntime{scheduler: scheduler, cancel: cancel, wait: wait, logger: logger}, nil
}

// newNotifyScheduler 建通知调度器（Node 的 notification-queue 调度面）。
//
// **默认关闭**（CCH_JOB_NOTIFY_ENABLED=false），这是刻意的：
//   - Node 与 Go 并存期间，Node 的 Bull 里还挂着同一批 repeatable 作业；两边同时按同一规则
//     跑就是**重复发送**（收件人会收到两份成本预警）。Node 下线时把本开关打开即可接管。
//   - 开关放在这里而不是 internal/jobs：internal/jobs 只回答「怎么调度」，
//     「这个进程要不要调度」属于进程装配（与本文件其它 CCH_JOB* 开关同一处）。
//
// 与价格同步的差别：那边两端幂等（同一份价格表覆盖写），双跑只是浪费 IO；
// 通知是外部副作用，双跑是可见的错，故这里默认关而不是默认开。
func newNotifyScheduler(
	logger *logx.Logger,
	pools *store.Pools,
	redisClient redis.UniversalClient,
	lookup config.LookupEnvFunc,
) *jobs.NotifyScheduler {
	if lookup == nil {
		lookup = config.LookupFromOS()
	}
	if pools == nil {
		// 无 DSN：设置与绑定都读不到，建出来也只会在每轮触发时记错。
		logger.Info("notify_scheduler_skipped", map[string]any{"reason": "no_dsn"})
		return nil
	}
	if !boolEnv(lookup, "CCH_JOBS_ENABLED", true) {
		return nil
	}
	if !boolEnv(lookup, "CCH_JOB_NOTIFY_ENABLED", false) {
		logger.Info("notify_scheduler_disabled", map[string]any{
			"env":    "CCH_JOB_NOTIFY_ENABLED",
			"reason": "Node 并存期避免与 Bull 的 repeatable 作业双发",
		})
		return nil
	}
	// 冷却去重：同一个实现既做「发送前读一遍」（生成器用它压制冷却期内的条目），
	// 也做「发送成功后写下」（调度器投递回执处），故一个实例注入两处。
	cooldown := notify.NewRedisCooldown(redisClient)
	if cooldown == nil {
		logger.Warn("notify_cooldown_absent", map[string]any{
			"reason": "未配置 REDIS_URL，缓存命中率告警不做冷却去重",
		})
	}
	return jobs.NewNotifyScheduler(jobs.NotifySchedulerOptions{
		Pools:  pools,
		Logger: logger,
		// 投递复用管理面的 webhook 投递层（信封/签名/响应判定），
		// 数据生成器同处装配（四种通知类型的数据面都在 internal/adminapi + internal/notify）。
		Deliverer: adminapi.NewNotificationDelivery(pools, logger),
		Payloads: adminapi.NewNotificationAlerts(&notify.Generators{
			Leaderboard: pools,
			Cost:        pools,
			Cache:       pools,
			Logger:      logger,
			Cooldown:    cooldown,
		}, logger),
		Cooldown: cooldown,
	})
}

// Stop 取消调度并等待任务退出（有界）。
//
// 必须在关闭连接池之前调用：任务在用池，先关池会让在途写入直接报错。
func (r *jobsRuntime) Stop() {
	if r == nil {
		return
	}
	r.cancel()
	done := make(chan struct{})
	go func() {
		r.wait.Wait()
		close(done)
	}()
	select {
	case <-done:
		r.logger.Info("jobs_stopped", map[string]any{"stats": statsToLog(r.scheduler.Stats())})
	case <-time.After(jobStopGrace):
		// 超时不阻塞退出：任务体尊重 ctx，卡住的只会是外部依赖（网络/DB）。
		r.logger.Warn("jobs_stop_timeout", map[string]any{
			"graceMs": jobStopGrace.Milliseconds(),
			"stats":   statsToLog(r.scheduler.Stats()),
		})
	}
}

// statsToLog 把执行计数压成可读结构。
func statsToLog(stats []jobs.Stats) []map[string]any {
	out := make([]map[string]any, 0, len(stats))
	for _, item := range stats {
		out = append(out, map[string]any{
			"job": item.Name, "runs": item.Runs,
			"failures": item.Failures, "skips": item.Skips,
		})
	}
	return out
}

// boolEnv 复刻 Node env schema 的 booleanTransform：`s != "false" && s != "0"`，大小写敏感；
// 未设置或空串取默认值。
func boolEnv(lookup config.LookupEnvFunc, name string, fallback bool) bool {
	raw, ok := lookup(name)
	if !ok || raw == "" {
		return fallback
	}
	return raw != "false" && raw != "0"
}

// intEnv 读整数并施加下限；非法值退回默认（后台任务的开关不该让进程起不来）。
func intEnv(lookup config.LookupEnvFunc, name string, fallback int) int {
	raw, ok := lookup(name)
	if !ok || raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < minJobIntervalMS {
		return fallback
	}
	return parsed
}

// stringEnv 读字符串；未设置取空串（由下游用默认值）。
func stringEnv(lookup config.LookupEnvFunc, name string) string {
	raw, _ := lookup(name)
	return raw
}
