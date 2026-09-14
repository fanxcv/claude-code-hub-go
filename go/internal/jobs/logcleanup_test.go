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

// ---- 假面 ----
//
// 到点判定、开关语义、截止时刻的计算全在这层被穷尽覆盖：它们都是纯逻辑，用真库测只会更慢
// 且更脆。真库只负责证明「SQL 按截止时刻删对了行」（见 logcleanup_integration_test.go）。

type fakeLogCleanupSettings struct {
	settings *store.AdminSystemSettings
	err      error
	calls    int
}

func (f *fakeLogCleanupSettings) EnsureAdminSystemSettings(context.Context) (*store.AdminSystemSettings, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.settings, nil
}

type fakeLogCleanupRunner struct {
	calls      int
	lastCond   store.AdminLogCleanupConditions
	lastDryRun bool
	result     store.AdminLogCleanupResult
}

func (f *fakeLogCleanupRunner) AdminCleanupUsageLogs(
	_ context.Context,
	conditions store.AdminLogCleanupConditions,
	dryRun bool,
) store.AdminLogCleanupResult {
	f.calls++
	f.lastCond = conditions
	f.lastDryRun = dryRun
	return f.result
}

// testLogCleanup 组装一个「时钟可变、依赖全假」的任务，并返回日志缓冲以便断言告警次数。
func testLogCleanup(
	t *testing.T,
	now *time.Time,
	settings *store.AdminSystemSettings,
	result store.AdminLogCleanupResult,
) (*LogCleanup, *fakeLogCleanupSettings, *fakeLogCleanupRunner, *strings.Builder) {
	t.Helper()
	var logs strings.Builder
	settingsSource := &fakeLogCleanupSettings{settings: settings}
	runner := &fakeLogCleanupRunner{result: result}
	task := newLogCleanupWithSeams(
		OpsDeps{Now: func() time.Time { return *now }, Logger: logx.New(&logs)},
		LogCleanupConfig{Enabled: true},
		settingsSource,
		runner,
	)
	return task, settingsSource, runner, &logs
}

func cleanupSettings(enabled bool, retentionDays *int, batchSize *int, schedule *string) *store.AdminSystemSettings {
	return &store.AdminSystemSettings{
		EnableAutoCleanup:    &enabled,
		CleanupRetentionDays: retentionDays,
		CleanupBatchSize:     batchSize,
		CleanupSchedule:      schedule,
	}
}

func intPtr(value int) *int          { return &value }
func stringPtr(value string) *string { return &value }

func mustLocalTime(t *testing.T, layout string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", layout, time.Local)
	if err != nil {
		t.Fatalf("构造时刻失败: %v", err)
	}
	return parsed
}

// ---- 调度表达式解析 ----

func TestParseLogCleanupSchedule(t *testing.T) {
	cases := []struct {
		spec       string
		wantHourly bool
		wantHour   int
		wantMin    int
		wantWarn   bool
	}{
		{spec: "0 2 * * *", wantHour: 2, wantMin: 0},
		{spec: "30 3 * * *", wantHour: 3, wantMin: 30},
		{spec: "15 * * * *", wantHourly: true, wantMin: 15},
		// 以下都属「表达力之外」：必须回退并告警，不能静默改语义。
		{spec: "@daily", wantWarn: true},
		{spec: "0 2 * * 1", wantWarn: true},
		{spec: "0 2 1 * *", wantWarn: true},
		{spec: "*/15 * * * *", wantWarn: true},
		{spec: "61 2 * * *", wantWarn: true},
		{spec: "0 24 * * *", wantWarn: true},
		{spec: "0 2", wantWarn: true},
		{spec: "", wantWarn: true},
	}
	for _, tc := range cases {
		plan, warning := parseLogCleanupSchedule(tc.spec)
		if tc.wantWarn {
			if warning == "" {
				t.Errorf("%q 应回退并告警，实际 plan=%+v warning=%q", tc.spec, plan, warning)
			}
			if plan.spec != logCleanupDefaultSchedule {
				t.Errorf("%q 回退后 spec 应为默认值，实际 %q", tc.spec, plan.spec)
			}
			continue
		}
		if warning != "" {
			t.Errorf("%q 不该告警，实际 %q", tc.spec, warning)
		}
		if plan.hourly != tc.wantHourly || plan.hour != tc.wantHour || plan.minute != tc.wantMin {
			t.Errorf("%q 解析错误: %+v", tc.spec, plan)
		}
		if plan.spec != tc.spec {
			t.Errorf("%q 生效 spec 应原样保留，实际 %q", tc.spec, plan.spec)
		}
	}
}

// ---- 到点判定 ----

func TestLogCleanupSkipsBeforeScheduledTime(t *testing.T) {
	now := mustLocalTime(t, "2026-03-05 01:30")
	task, _, runner, _ := testLogCleanup(t, &now, cleanupSettings(true, nil, nil, stringPtr("0 2 * * *")), store.AdminLogCleanupResult{})

	outcome, err := task.Run(context.Background())
	if err != nil {
		t.Fatalf("未到点不该报错: %v", err)
	}
	if runner.calls != 0 {
		t.Fatalf("未到点不得执行删除，实际调用 %d 次", runner.calls)
	}
	if outcome.Fields["skipped"] != "not_due" {
		t.Fatalf("应记为 not_due，实际 %+v", outcome.Fields)
	}
}

func TestLogCleanupRunsAtScheduleAndComputesRollingCutoff(t *testing.T) {
	// 02:37 启动：计划时刻是 02:00，已过 → 必须补跑（Bull 的 missed repeat 同样补跑）。
	now := mustLocalTime(t, "2026-03-05 02:37")
	task, _, runner, _ := testLogCleanup(
		t, &now,
		cleanupSettings(true, intPtr(45), intPtr(2048), stringPtr("0 2 * * *")),
		store.AdminLogCleanupResult{TotalDeleted: 1234, SoftDeletedPurged: 6, BatchCount: 3, DurationMS: 900, VacuumPerformed: true},
	)

	outcome, err := task.Run(context.Background())
	if err != nil {
		t.Fatalf("到点执行失败: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("到点应执行一次，实际 %d", runner.calls)
	}
	if runner.lastDryRun {
		t.Fatal("调度路径不得 dry-run（Node 的定时作业是真删）")
	}
	cutoff := runner.lastCond.BeforeDate
	if cutoff == nil {
		t.Fatal("必须给出 beforeDate")
	}
	want := now.AddDate(0, 0, -45)
	if !cutoff.Equal(want) {
		t.Fatalf("beforeDate 应为 now - 45 天（滚动计算）: got %s want %s", cutoff, want)
	}
	if runner.lastCond.AfterDate != nil || len(runner.lastCond.UserIDs) != 0 || runner.lastCond.OnlyBlocked {
		t.Fatalf("定时清理只按 beforeDate 过滤（Node 的 conditions: {}）: %+v", runner.lastCond)
	}
	if outcome.Processed != 1240 {
		t.Fatalf("Processed 应为删除总行数 1240，实际 %d", outcome.Processed)
	}
	if outcome.Fields["totalDeleted"] != int64(1234) || outcome.Fields["softDeleted"] != int64(6) {
		t.Fatalf("日志字段应沿用 Node 名: %+v", outcome.Fields)
	}
	if outcome.Fields["batchSize"] != 2048 || outcome.Fields["retentionDays"] != 45 {
		t.Fatalf("日志里的设置值应来自系统设置: %+v", outcome.Fields)
	}
}

func TestLogCleanupRunsAtMostOncePerWindow(t *testing.T) {
	now := mustLocalTime(t, "2026-03-05 02:05")
	task, _, runner, _ := testLogCleanup(t, &now, cleanupSettings(true, nil, nil, stringPtr("0 2 * * *")), store.AdminLogCleanupResult{TotalDeleted: 5})

	if _, err := task.Run(context.Background()); err != nil {
		t.Fatalf("首轮失败: %v", err)
	}
	now = mustLocalTime(t, "2026-03-05 03:05")
	outcome, err := task.Run(context.Background())
	if err != nil {
		t.Fatalf("同窗口第二轮失败: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("同一天不得重复清理，实际调用 %d 次", runner.calls)
	}
	if outcome.Fields["skipped"] != "not_due" || outcome.Fields["alreadyRan"] != true {
		t.Fatalf("应记为同窗口已跑: %+v", outcome.Fields)
	}

	// 跨天到点后必须再跑（窗口键按本地日期滚动）。
	now = mustLocalTime(t, "2026-03-06 02:01")
	if _, err := task.Run(context.Background()); err != nil {
		t.Fatalf("次日执行失败: %v", err)
	}
	if runner.calls != 2 {
		t.Fatalf("次日应再清理一次，实际调用 %d 次", runner.calls)
	}
}

func TestLogCleanupHourlyScheduleRunsOncePerHour(t *testing.T) {
	now := mustLocalTime(t, "2026-03-05 09:20")
	task, _, runner, _ := testLogCleanup(t, &now, cleanupSettings(true, nil, nil, stringPtr("15 * * * *")), store.AdminLogCleanupResult{TotalDeleted: 1})

	if _, err := task.Run(context.Background()); err != nil {
		t.Fatalf("首轮失败: %v", err)
	}
	now = mustLocalTime(t, "2026-03-05 09:40")
	if _, err := task.Run(context.Background()); err != nil {
		t.Fatalf("同小时第二轮失败: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("同一小时内不得重复清理，实际 %d 次", runner.calls)
	}
	// 10:05 仍属「最近一个已过去的计划时刻是 09:15」的窗口（已跑）→ 不得再跑。
	now = mustLocalTime(t, "2026-03-05 10:05")
	if _, err := task.Run(context.Background()); err != nil {
		t.Fatalf("未到 10:15 时失败: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("10:15 之前不得清理，实际 %d 次", runner.calls)
	}
	// 10:20 已过 10:15 → 新窗口，应再跑一次。
	now = mustLocalTime(t, "2026-03-05 10:20")
	if _, err := task.Run(context.Background()); err != nil {
		t.Fatalf("次小时失败: %v", err)
	}
	if runner.calls != 2 {
		t.Fatalf("跨小时应再清理一次，实际 %d 次", runner.calls)
	}
}

// TestLogCleanupHourlyTicksNeverMissSchedule 钉住「tick 相位与 cron 分钟数错开」这一类。
//
// 背景：调度器是固定间隔（默认 1 小时一跳）。若进程在 :10 启动而 cron 写 `15 * * * *`，
// 则每一跳都落在计划时刻**之前**。若到点判定写成「本窗口的计划时刻已过」，就会**一次都不跑**；
// 用「最近一个已过去的计划时刻」判定，则每跳处理上一个窗口，效果仍是每小时一次。
func TestLogCleanupHourlyTicksNeverMissSchedule(t *testing.T) {
	schedule := stringPtr("15 * * * *")
	// 进程在 :10 启动 → 每跳都落在 :10（即永远早于本小时的 :15）。
	now := mustLocalTime(t, "2026-03-05 09:10")
	task, _, runner, _ := testLogCleanup(t, &now, cleanupSettings(true, nil, nil, schedule), store.AdminLogCleanupResult{})

	ticks := []string{"2026-03-05 09:10", "2026-03-05 10:10", "2026-03-05 11:10", "2026-03-05 12:10"}
	for index, tick := range ticks {
		now = mustLocalTime(t, tick)
		if _, err := task.Run(context.Background()); err != nil {
			t.Fatalf("第 %d 跳（%s）失败: %v", index+1, tick, err)
		}
		if runner.calls != index+1 {
			t.Fatalf("第 %d 跳后累计执行应为 %d，实际 %d", index+1, index+1, runner.calls)
		}
	}
}

// ---- 开关与错误路径 ----

func TestLogCleanupHonoursSystemSettingDisable(t *testing.T) {
	now := mustLocalTime(t, "2026-03-05 02:30")
	task, _, runner, _ := testLogCleanup(t, &now, cleanupSettings(false, nil, nil, nil), store.AdminLogCleanupResult{TotalDeleted: 9})

	outcome, err := task.Run(context.Background())
	if err != nil {
		t.Fatalf("关闭时不该报错: %v", err)
	}
	if runner.calls != 0 {
		t.Fatalf("系统设置关闭时不得清理，实际 %d 次", runner.calls)
	}
	if outcome.Fields["skipped"] != "system_setting_disabled" {
		t.Fatalf("应记为 system_setting_disabled: %+v", outcome.Fields)
	}
}

func TestLogCleanupDefaultsWhenSettingsMissing(t *testing.T) {
	// 所有可空列都为 nil：保留天数取 30、批大小记 10000、调度取 0 2 * * *。
	now := mustLocalTime(t, "2026-03-05 02:00")
	task, _, runner, _ := testLogCleanup(t, &now, cleanupSettings(true, nil, nil, nil), store.AdminLogCleanupResult{})

	outcome, err := task.Run(context.Background())
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("到点应执行，实际 %d", runner.calls)
	}
	if got := runner.lastCond.BeforeDate; got == nil || !got.Equal(now.AddDate(0, 0, -logCleanupDefaultRetentionDays)) {
		t.Fatalf("缺设置时保留天数应为 30 天: %v", got)
	}
	if outcome.Fields["batchSize"] != logCleanupDefaultBatchSize {
		t.Fatalf("缺设置时批大小时日志应记默认值: %+v", outcome.Fields)
	}
}

func TestLogCleanupUnsupportedScheduleWarnsOnceAndFallsBack(t *testing.T) {
	now := mustLocalTime(t, "2026-03-05 02:30")
	task, _, runner, logs := testLogCleanup(t, &now, cleanupSettings(true, nil, nil, stringPtr("@hourly")), store.AdminLogCleanupResult{})

	for i := 0; i < 3; i++ {
		if _, err := task.Run(context.Background()); err != nil {
			t.Fatalf("第 %d 轮失败: %v", i+1, err)
		}
	}
	if runner.calls != 1 {
		t.Fatalf("回退到每日 02:00 后只应跑一次，实际 %d", runner.calls)
	}
	if got := strings.Count(logs.String(), "log_cleanup_schedule_unsupported"); got != 1 {
		t.Fatalf("同一表达式只应告警一次，实际 %d 次", got)
	}
}

func TestLogCleanupStoreErrorPropagatesAndStaysRetryable(t *testing.T) {
	now := mustLocalTime(t, "2026-03-05 02:30")
	task, _, runner, _ := testLogCleanup(
		t, &now,
		cleanupSettings(true, nil, nil, nil),
		store.AdminLogCleanupResult{TotalDeleted: 7, Error: "store: 清理日志失败: boom"},
	)

	_, err := task.Run(context.Background())
	if err == nil {
		t.Fatal("store 的错误必须翻成 error（否则「清理一直失败」只在结果字段里静默存在）")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("错误应带上原文: %v", err)
	}

	// 失败不得记账：同一窗口的下一跳要能重试（Node 的 attempts: 3 与此等价）。
	runner.result = store.AdminLogCleanupResult{}
	if _, err := task.Run(context.Background()); err != nil {
		t.Fatalf("重试应成功: %v", err)
	}
	if runner.calls != 2 {
		t.Fatalf("失败后同一窗口应可重试，实际调用 %d 次", runner.calls)
	}
}

func TestLogCleanupRejectsNegativeRetention(t *testing.T) {
	now := mustLocalTime(t, "2026-03-05 02:30")
	task, _, runner, _ := testLogCleanup(t, &now, cleanupSettings(true, intPtr(-1), nil, nil), store.AdminLogCleanupResult{})

	if _, err := task.Run(context.Background()); err == nil {
		t.Fatal("负数保留天数必须拒绝（等价于删未来）")
	}
	if runner.calls != 0 {
		t.Fatalf("非法设置不得触发删除，实际 %d 次", runner.calls)
	}
}

func TestLogCleanupSettingsReadFailureIsVisible(t *testing.T) {
	now := mustLocalTime(t, "2026-03-05 02:30")
	var logs strings.Builder
	source := &fakeLogCleanupSettings{err: errors.New("boom")}
	task := newLogCleanupWithSeams(
		OpsDeps{Now: func() time.Time { return now }, Logger: logx.New(&logs)},
		LogCleanupConfig{Enabled: true},
		source,
		&fakeLogCleanupRunner{},
	)
	if _, err := task.Run(context.Background()); err == nil {
		t.Fatal("读设置失败必须冒泡（否则会静默不清理）")
	}
}

func TestLogCleanupWithoutPoolsIsExplicit(t *testing.T) {
	task := NewLogCleanup(OpsDeps{}, LogCleanupConfig{Enabled: true})
	if _, err := task.Run(context.Background()); err == nil {
		t.Fatal("无连接池时必须显式报错（装配缺陷应可见）")
	}
}

// TestLogCleanupFitsOpsTaskContract 把「协调者要接的那一行」在测试里先跑一遍。
//
// 接法是 OpsTask（与探活/清理/outbox 同族，由 cmd/cchd/jobs_ops.go 的 ticker 驱动），
// 不是 scheduler.Task（后者是价格同步那一族的固定日历入口）。这里按 OpsTask 的形状驱动一轮，
// 证明：① Run 的签名直接兼容；② 一轮的 Outcome 能落出预期的字段（巡检靠它）。
func TestLogCleanupFitsOpsTaskContract(t *testing.T) {
	now := mustLocalTime(t, "2026-03-05 02:30")
	var logs strings.Builder
	runner := &fakeLogCleanupRunner{result: store.AdminLogCleanupResult{TotalDeleted: 7}}
	task := newLogCleanupWithSeams(
		OpsDeps{Now: func() time.Time { return now }, Logger: logx.New(&logs)},
		LogCleanupConfig{Enabled: true, CheckInterval: time.Hour},
		&fakeLogCleanupSettings{settings: cleanupSettings(true, nil, nil, nil)},
		runner,
	)

	// 与 OpsTasks 里的构造同形（这就是要加的那一行）。
	opsTask := OpsTask{Name: "log-cleanup", Every: time.Hour, Run: task.Run}
	if opsTask.Name != "log-cleanup" || opsTask.Every != time.Hour {
		t.Fatalf("OpsTask 形状不符: %+v", opsTask)
	}

	outcome, err := opsTask.Run(context.Background())
	if err != nil {
		t.Fatalf("一轮执行失败: %v", err)
	}
	if outcome.Processed != 7 {
		t.Fatalf("Outcome.Processed 应为删除行数 7，实际 %d", outcome.Processed)
	}
	if outcome.Fields["beforeDate"] == nil {
		t.Fatalf("Outcome.Fields 应带 beforeDate 供巡检: %+v", outcome.Fields)
	}
}

// ---- 开关解析 ----

func TestLogCleanupConfigFromEnv(t *testing.T) {
	if cfg := LogCleanupConfigFromEnv(opsLookupFrom(nil)); !cfg.Enabled {
		t.Fatal("默认必须开启：Node 停掉后若默认关闭，自动清理会静默停摆")
	}
	for _, value := range []string{"false", "0", "no", "off"} {
		cfg := LogCleanupConfigFromEnv(opsLookupFrom(map[string]string{"CCH_JOB_LOG_CLEANUP_ENABLED": value}))
		if cfg.Enabled {
			t.Fatalf("CCH_JOB_LOG_CLEANUP_ENABLED=%s 应关闭任务", value)
		}
	}
	cfg := LogCleanupConfigFromEnv(opsLookupFrom(map[string]string{"CCH_JOB_LOG_CLEANUP_TICK_MS": "60000"}))
	if cfg.CheckInterval != time.Minute {
		t.Fatalf("tick 覆盖失效: %s", cfg.CheckInterval)
	}
}
