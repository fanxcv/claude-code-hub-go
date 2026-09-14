package jobs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 **usage 日志自动清理**的后台驱动（审计缺口 G4）。
//
// 语义出处（Node）：
//   - src/instrumentation.ts:687 —— 启动期调用 `scheduleAutoCleanup()`；
//   - src/lib/log-cleanup/cleanup-queue.ts:125-172 —— 读**系统设置**决定是否调度、保留天数、
//     批大小与 cron（`enableAutoCleanup` / `cleanupRetentionDays ?? 30` /
//     `cleanupBatchSize ?? 10000` / `cleanupSchedule ?? "0 2 * * *"`），并只存
//     `retentionDays` 让 beforeDate 在执行时随时间滚动（该文件 :146-148 的注释）；
//   - src/lib/log-cleanup/service.ts —— 删除实现本身，Go 侧已在
//     `store/admin_log_cleanup.go` 逐字复刻，本文件**只做驱动，不重写删除逻辑**。
//
// 为什么需要它：Node 停掉后，唯一会按天清理 `message_request` 的驱动随之消失，表会无限增长
// ——这笔帐不会出现在任何「端点差分」清单里（没有请求触发它），只能靠后台作业审计发现。
//
// **与 Node 的差异（登记，不假装等价）**：
//  1. **cron 表达力**：Node 用 Bull 的 5 段 cron（任意表达式）。本实现只识别两种形态
//     ——「每日一次」（`M H * * *`）与「每小时一次」（`M * * * *`）；其余表达式记 warn 并
//     回退到每日 02:00。理由：引入 cron 解析器会多一个可错的输入面，而生产默认值就是每日一次。
//  2. **失败重试**：Node 的 Bull 任务 `attempts: 3` + 指数退避（首次 60s）。本实现失败后
//     等下一轮 tick（默认 1 小时）再试，失败只体现在日志与计数上（与 jobs 包其它任务同判）。
//     代价：一次瞬时故障要等一小时才重试；收益：不与请求路径抢重试预算。
//  3. **互斥范围**：Node 的 Bull repeatable 天然跨进程去重（Redis 里的重复作业键），但**没有
//     可与 Go 共享的锁名**；本实现的锁只在 Go 副本之间互斥。故**双跑期 Node 与 Go 可能各跑
//     一次**：删除是 `FOR UPDATE SKIP LOCKED` + 幂等的，重复执行的代价是重复工作与两次
//     VACUUM，不会误删。要完全回避可置 `CCH_JOB_LOG_CLEANUP_ENABLED=false`（见 doc.go 的开关表）。
//  4. **批大小**：Node 把 `cleanupBatchSize` 传进 service；Go 的 `AdminCleanupUsageLogs`
//     固定 10000（store 复刻的常量，手工入口同样如此）。本文件读该设置**仅用于日志**，
//     不改变删除批大小——要真正参数化得改 store 层，属另一处改动。
//  5. **时区**：Node 的 Bull cron 未指定 `tz` → 按进程本地时区（容器 `TZ=Asia/Shanghai`）。
//     本实现同样用**本地时区**（`deps.now()` 的 Location），与 Node 同判。

const (
	// LogCleanupLockKey 是 Go 副本之间的互斥键。
	//
	// 命名沿用本包既有约定（`locks:*` 前缀），**不是**与 Node 共享的锁：Node 的调度去重靠
	// Bull 的重复作业键，没有等价的锁名可对齐（见文件头差异 3）。
	LogCleanupLockKey = "locks:log-cleanup"
	// logCleanupCheckInterval 是「本任务多久判一次是否到点」的 tick。
	//
	// 调度器只支持固定间隔，而 Node 的语义是「每天 02:00」（日历语义）；故这里用 1 小时一跳
	// 做到点判定：一跳的开销只是读一次系统设置（一次小查询），到点才真干活。
	// 1 小时也决定了「重启补跑」的粒度：进程在 02:30 重启，第一跳（启动即跑）就会补跑当天。
	logCleanupCheckInterval = time.Hour
	// logCleanupLockTTL 是互斥锁的租约：比一轮清理的常见耗时长得多（大表清理按 10000 行一批
	// 提交，分钟级），过短会让同进程的下一跳抢到自己还没做完的锁。
	logCleanupLockTTL = 30 * time.Minute
	// logCleanupDefaultRetentionDays 与 Node 的 `cleanupRetentionDays ?? 30` 同值。
	logCleanupDefaultRetentionDays = 30
	// logCleanupDefaultBatchSize 与 Node 的 `cleanupBatchSize ?? 10000` 同值（仅用于日志）。
	logCleanupDefaultBatchSize = 10000
	// logCleanupDefaultSchedule 与 Node 的 `cleanupSchedule ?? "0 2 * * *"` 同值。
	logCleanupDefaultSchedule = "0 2 * * *"
	// logCleanupCatchUpWindow 是「补跑上限」：计划时刻过去多久之内还允许补跑。
	//
	// 为什么需要它：tick 是固定间隔，其相位可与 cron 的分钟数错开（例如进程在 :10 启动而 cron 写
	// `15 * * * *`），若只判「本窗口计划时刻已过」，那两个时刻会永远互相错过、**一次都不跑**。
	// 故改判「最近一个已过去的计划时刻 + 该窗口未跑」。
	//
	// 为何又要有上限：不加限的话，「01:30 启动 + 每日 02:00」会立刻补跑昨天那一轮（运维心智是
	// 「每天 02:00 清理」）；加上限后 01:30 启动只等 02:00，而 03:00 启动仍会补跑（那轮确实被
	// 重启错过了，Bull 的 missed repeat 同样会补）。
	logCleanupCatchUpWindow = 2 * time.Hour
)

// LogCleanupConfig 是本任务自身的开关（业务口径一律读系统设置，见 Run）。
type LogCleanupConfig struct {
	// Enabled 为 false 时整任务不注册（由 OpsTasks 判定）。
	Enabled bool
	// CheckInterval 是到点判定间隔；<=0 时取 logCleanupCheckInterval。
	CheckInterval time.Duration
	// LockTTL 是互斥租约；<=0 时取 logCleanupLockTTL。
	LockTTL time.Duration
}

// LogCleanupConfigFromEnv 读本任务的开关。
//
// 环境变量一律带 `CCH_JOB_` 前缀（Go 专有，不进 env-parity 对账清单）。
// **默认开启**：Node 停掉后若默认关闭，自动清理会静默停摆——这正是本任务要修的那类问题
// （与 doc.go「为什么默认开启」同一条理由）。
func LogCleanupConfigFromEnv(lookup OpsEnv) LogCleanupConfig {
	cfg := LogCleanupConfig{
		Enabled:       true,
		CheckInterval: logCleanupCheckInterval,
		LockTTL:       logCleanupLockTTL,
	}
	if value, ok := opsLookup(lookup, "CCH_JOB_LOG_CLEANUP_ENABLED"); ok {
		cfg.Enabled = !(value == "false" || value == "0" || value == "no" || value == "off")
	}
	if value, ok := opsLookup(lookup, "CCH_JOB_LOG_CLEANUP_TICK_MS"); ok {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			cfg.CheckInterval = time.Duration(parsed) * time.Millisecond
		}
	}
	return cfg
}

// logCleanupSettings 是本任务需要的设置读取面（`*store.Pools` 满足）。
//
// 单独抽出来是为了让「到点判定 / 开关语义 / 截止时刻」这些纯逻辑能在**无数据库**的单元测试里
// 被穷尽覆盖：真库集成用例只覆盖「SQL 真的按截止时刻删对了行」那一小块。
type logCleanupSettings interface {
	EnsureAdminSystemSettings(ctx context.Context) (*store.AdminSystemSettings, error)
}

// logCleanupRunner 是删除执行面（`*store.Pools` 满足）。
type logCleanupRunner interface {
	AdminCleanupUsageLogs(
		ctx context.Context,
		conditions store.AdminLogCleanupConditions,
		dryRun bool,
	) store.AdminLogCleanupResult
}

// LogCleanup 是日志自动清理任务。
type LogCleanup struct {
	deps     OpsDeps
	cfg      LogCleanupConfig
	settings logCleanupSettings
	runner   logCleanupRunner

	mu sync.Mutex
	// lastRunKey 是「上一次成功跑过的窗口键」（每日形态为 YYYY-MM-DD，每小时形态为
	// YYYY-MM-DDTHH）。用它保证同一窗口最多跑一次：tick 是固定间隔，靠时刻相等判到点会在
	// 「启动时刻的分钟数 ≠ cron 分钟数」时永不触发（例如 02:37 启动 → 每跳都在 :37）。
	lastRunKey string
	// warnedSpec 记录已就「表达式不支持」告警过的 spec，避免每跳重复刷日志。
	warnedSpec string
}

// NewLogCleanup 构造任务（生产装配用）。
func NewLogCleanup(deps OpsDeps, cfg LogCleanupConfig) *LogCleanup {
	task := &LogCleanup{deps: deps, cfg: cfg}
	if deps.Pools != nil {
		task.settings = deps.Pools
		task.runner = deps.Pools
	}
	return task
}

// newLogCleanupWithSeams 供测试注入假设置源与假执行面。
func newLogCleanupWithSeams(
	deps OpsDeps,
	cfg LogCleanupConfig,
	settings logCleanupSettings,
	runner logCleanupRunner,
) *LogCleanup {
	return &LogCleanup{deps: deps, cfg: cfg, settings: settings, runner: runner}
}

// 注册形状的编译期证明：本任务的 Run 与 OpsTask.Run 同型，接进 OpsTasks 时不需要适配器。
// 协调者要加的是 tasks.go 里的三处（选项字段、默认值与 append），具体行见报告。
var _ = OpsTask{Name: "log-cleanup", Run: (*LogCleanup)(nil).Run}

// Run 执行一轮（到点才真删）。
//
// 返回的 OpsOutcome.Processed 是**本轮实际删除的行数**（未到点/未获锁/被设置关闭时为 0），
// Fields 沿用 Node 的日志字段名，便于与 Node 的 `log_cleanup_completed` 对照。
func (c *LogCleanup) Run(ctx context.Context) (OpsOutcome, error) {
	if c.settings == nil || c.runner == nil {
		// 与其它任务同判：装配缺陷显式可见，不静默当「本轮无工作」。
		return OpsOutcome{}, errors.New("jobs: 日志自动清理需要数据库连接池")
	}

	settings, err := c.settings.EnsureAdminSystemSettings(ctx)
	if err != nil {
		return OpsOutcome{}, fmt.Errorf("jobs: 读系统设置失败: %w", err)
	}

	// 设置项与 Node 逐项同源；缺项时取 Node 的出厂默认（cleanup-queue.ts:148-160）。
	enabled := settings.EnableAutoCleanup != nil && *settings.EnableAutoCleanup
	retentionDays := logCleanupDefaultRetentionDays
	if settings.CleanupRetentionDays != nil {
		retentionDays = *settings.CleanupRetentionDays
	}
	batchSize := logCleanupDefaultBatchSize
	if settings.CleanupBatchSize != nil {
		batchSize = *settings.CleanupBatchSize
	}
	spec := strings.TrimSpace(derefLogCleanupString(settings.CleanupSchedule))
	if spec == "" {
		spec = logCleanupDefaultSchedule
	}

	if !enabled {
		// Node 在 `enableAutoCleanup` 关闭时不但不调度，还会**移除已存在的重复作业**
		// （cleanup-queue.ts:131-136）。本实现没有可移除的持久作业（到点判定在内存里），
		// 语义等价：不再执行。
		return OpsOutcome{Fields: map[string]any{"skipped": "system_setting_disabled"}}, nil
	}
	if retentionDays < 0 {
		// 负数保留天数等于「删掉未来」，一律拒绝执行（Node 侧 zod 已挡，这里做二次防线）。
		return OpsOutcome{}, fmt.Errorf("jobs: 保留天数非法: %d", retentionDays)
	}

	plan, warning := parseLogCleanupSchedule(spec)
	c.warnOnceOnce(warning)

	now := c.deps.now()
	key, inst := plan.latestInstant(now)
	c.mu.Lock()
	alreadyRan := c.lastRunKey == key
	c.mu.Unlock()
	if alreadyRan || now.Sub(inst) > logCleanupCatchUpWindow {
		return OpsOutcome{
			Fields: map[string]any{
				"skipped":     "not_due",
				"schedule":    plan.spec,
				"lastDueAt":   inst.Format(time.RFC3339),
				"nextDueAt":   nextLogCleanupDue(plan, now).Format(time.RFC3339),
				"windowKey":   key,
				"alreadyRan":  alreadyRan,
				"retentionDd": retentionDays,
			},
		}, nil
	}

	lockTTL := c.cfg.LockTTL
	if lockTTL <= 0 {
		lockTTL = logCleanupLockTTL
	}
	lock, acquired, err := c.deps.AcquireOpsLeaderLock(ctx, LogCleanupLockKey, lockTTL)
	if err != nil {
		return OpsOutcome{}, err
	}
	if !acquired {
		// 另有 Go 副本在跑（或上一次还没做完）：本轮跳过，下一跳再判。
		return OpsOutcome{Fields: map[string]any{"skipped": "not_leader"}}, nil
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()

	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	stopKeepAlive := startOpsKeepAlive(workCtx, c.deps, lock, lockTTL, "log_cleanup", cancelWork)
	defer stopKeepAlive()

	// beforeDate 在**执行时**按当时的 now 计算（Node 的滚动语义，cleanup-queue.ts:73-77）：
	// 若在调度时刻就把 beforeDate 算好并持久化，恰好跨天的那次触发会少删一天。
	cutoff := now.AddDate(0, 0, -retentionDays)
	conditions := store.AdminLogCleanupConditions{BeforeDate: &cutoff}

	result := c.runner.AdminCleanupUsageLogs(workCtx, conditions, false)
	deleted := int(result.TotalDeleted + result.SoftDeletedPurged)
	fields := map[string]any{
		"retentionDays":   retentionDays,
		"schedule":        plan.spec,
		"beforeDate":      cutoff.Format(time.RFC3339),
		"batchSize":       batchSize,
		"totalDeleted":    result.TotalDeleted,
		"softDeleted":     result.SoftDeletedPurged,
		"batchCount":      result.BatchCount,
		"vacuumPerformed": result.VacuumPerformed,
		"durationMs":      result.DurationMS,
	}
	if result.Error != "" {
		// store 层的约定：错误装在结果里（返回类型不带 error）。这里翻成 error 让调度器
		// 记 job_failed 并计入失败计数——否则「清理一直失败」只会在结果字段里静默存在。
		return OpsOutcome{Processed: deleted, Fields: fields}, errors.New(result.Error)
	}

	// 只有成功才记账：失败时下一跳仍会重试同一窗口（相当于 Node 的重试语义）。
	c.mu.Lock()
	c.lastRunKey = key
	c.mu.Unlock()

	if deleted > 0 {
		c.deps.logger().Info("log_cleanup_completed", fields)
	}
	return OpsOutcome{Processed: deleted, Fields: fields}, nil
}

// warnOnceOnce 对同一个 spec 只告警一次。
func (c *LogCleanup) warnOnceOnce(warning string) {
	if warning == "" {
		return
	}
	c.mu.Lock()
	already := c.warnedSpec == warning
	if !already {
		c.warnedSpec = warning
	}
	c.mu.Unlock()
	if already {
		return
	}
	c.deps.logger().Warn("log_cleanup_schedule_unsupported", map[string]any{
		"warning":  warning,
		"fallback": logCleanupDefaultSchedule,
	})
}

// logCleanupPlan 是解析后的调度计划。
type logCleanupPlan struct {
	// spec 是生效的表达式（回退时是默认值），供日志与巡检对照。
	spec string
	// hourly 为 true 表示「每小时一次」（`M * * * *`），否则为「每日一次」（`M H * * *`）。
	hourly bool
	hour   int
	minute int
}

// latestInstant 返回「不晚于 now 的最近一个计划时刻」及其窗口键。
//
// 窗口键的粒度即去重粒度：每日形态按本地日期，每小时形态按本地小时。
// 用「最近一个已过去的计划时刻」而不是「本窗口的计划时刻」，正是为了容忍 tick 相位与 cron
// 分钟数错开（见 logCleanupCatchUpWindow 的说明）。
func (p logCleanupPlan) latestInstant(now time.Time) (key string, inst time.Time) {
	if p.hourly {
		inst = time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), p.minute, 0, 0, now.Location())
		if now.Before(inst) {
			inst = inst.Add(-time.Hour)
		}
		return inst.Format("2006-01-02T15"), inst
	}
	inst = time.Date(now.Year(), now.Month(), now.Day(), p.hour, p.minute, 0, 0, now.Location())
	if now.Before(inst) {
		inst = inst.AddDate(0, 0, -1)
	}
	return inst.Format("2006-01-02"), inst
}

// nextLogCleanupDue 返回下一次计划时刻（仅用于日志：让运维一眼看出「为什么现在没跑」）。
func nextLogCleanupDue(p logCleanupPlan, now time.Time) time.Time {
	_, inst := p.latestInstant(now)
	if p.hourly {
		return inst.Add(time.Hour)
	}
	return inst.AddDate(0, 0, 1)
}

// parseLogCleanupSchedule 解析 Node 的 5 段 cron，只支持每日与每小时两种形态。
//
// 第二个返回值非空时表示「表达式不被支持，已回退默认」——调用方据此告警（不静默改写语义）。
func parseLogCleanupSchedule(spec string) (logCleanupPlan, string) {
	fallback := func(reason string) (logCleanupPlan, string) {
		plan, _ := parseLogCleanupSchedule(logCleanupDefaultSchedule)
		plan.spec = logCleanupDefaultSchedule
		return plan, fmt.Sprintf("调度表达式 %q 不受支持（%s），已回退 %s", spec, reason, logCleanupDefaultSchedule)
	}

	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return fallback(fmt.Sprintf("需要 5 段，实际 %d 段", len(fields)))
	}
	minute, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return fallback("分钟字段: " + err.Error())
	}
	dom, month, dow := fields[2], fields[3], fields[4]
	if dom != "*" || month != "*" || dow != "*" {
		return fallback("仅支持「每日」或「每小时」形态（日/月/星期字段必须为 *）")
	}
	if fields[1] == "*" {
		return logCleanupPlan{spec: spec, hourly: true, minute: minute}, ""
	}
	hour, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return fallback("小时字段: " + err.Error())
	}
	return logCleanupPlan{spec: spec, hour: hour, minute: minute}, ""
}

// parseCronField 解析单个数字字段（不支持 `*`、区间、步长与列表）。
func parseCronField(field string, min, max int) (int, error) {
	value, err := strconv.Atoi(field)
	if err != nil {
		return 0, fmt.Errorf("非数字 %q", field)
	}
	if value < min || value > max {
		return 0, fmt.Errorf("超出 [%d,%d]: %d", min, max, value)
	}
	return value, nil
}

// derefLogCleanupString 展开可空字符串（nil 与空串同义）。
func derefLogCleanupString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
