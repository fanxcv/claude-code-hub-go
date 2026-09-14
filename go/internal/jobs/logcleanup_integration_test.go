package jobs

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 真库集成：证明「SQL 真的按滚动截止时刻删对了行」。
//
// **为何必须用可弃库**：本用例会真删 `message_request` 的行——虽然它自己造的行带模型前缀，
// 但清理的判据是 `created_at <= cutoff`（不带前缀条件，与生产定时作业同形），故库里的其它
// 过期行也会被一起删掉。共享测试库（cch_loadtest / cch_smoke）上有真实历史数据，在上面跑
// 等于替别人清库。故沿用仓库既有的**可弃库**门槛 `CCH_TEST_SYNC_DSN`
// （`CREATE DATABASE x TEMPLATE cch_loadtest`），未设置即跳过：默认套件永不动共享数据。
// 复用同一个变量而不是再造第三个 DSN 开关，是为了让「可弃库」这件事只有一个旋钮。
const logCleanupDisposableDSNEnv = "CCH_TEST_SYNC_DSN"

const logCleanupFixturePrefix = "go-logcleanup-it-"

func logCleanupDisposablePools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv(logCleanupDisposableDSNEnv)
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_SYNC_DSN（可弃库），跳过会真删行的清理集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-logcleanup-it",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// seedLogCleanupRows 造三行夹具：严格过期行、**恰好落在截止时刻**的边界行、严格未过期行。
//
// 为何把边界行钉在 cutoff 上：断言必须与墙钟无关。“今天 02:30 跑、行建在 now-30d”这种写法会
// 随运行时刻改变边界关系（00:00–02:30 之间跑与 19:00 跑结论相反），属本仓已踩过的跨零点
// 亚稳类。故 cutoff 由调用方给定，三行相对它定位：cutoff-1h / cutoff / cutoff+1h。
//
// 触发器会为每行写一条账本行——本用例顺带钉住「清理只动 message_request，不碰 usage_ledger」
// 这条已声明的性质（store/admin_log_cleanup.go 文件头第 1 条）。
func seedLogCleanupRows(t *testing.T, pools *store.Pools, prefix string, cutoff time.Time) []int64 {
	t.Helper()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写连接失败: %v", err)
	}
	specs := []struct {
		label     string
		createdAt time.Time
	}{
		{"expired", cutoff.Add(-time.Hour)},
		{"boundary", cutoff},
		{"fresh", cutoff.Add(time.Hour)},
	}
	ids := make([]int64, 0, len(specs))
	for _, spec := range specs {
		var id int64
		if err := pool.QueryRow(context.Background(), `
			INSERT INTO message_request (
				provider_id, user_id, key, model, original_model, endpoint,
				status_code, input_tokens, output_tokens, cost_usd, duration_ms, ttfb_ms,
				error_message, user_agent, is_replay, created_at
			) VALUES (
				1, 1, $1, $2, $2, '/v1/messages',
				200, 1, 1, 0::numeric, 1, 1,
				NULL, 'logcleanup-it', false, $3::timestamptz
			) RETURNING id`,
			logCleanupFixturePrefix+spec.label, prefix+"-"+spec.label, spec.createdAt,
		).Scan(&id); err != nil {
			t.Fatalf("插入 %s 夹具行失败: %v", spec.label, err)
		}
		ids = append(ids, id)
	}
	t.Cleanup(func() {
		// 触发器只写不删：先删账本再删请求行（与仓库既有夹具同序）。
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(context.Background(),
			`DELETE FROM usage_ledger WHERE request_id = ANY($1::bigint[])`, ids)
		_, _ = cleanupPool.Exec(context.Background(),
			`DELETE FROM message_request WHERE id = ANY($1::bigint[])`, ids)
	})
	return ids
}

func countLogCleanupFixtures(t *testing.T, pools *store.Pools, model string) int {
	t.Helper()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写连接失败: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*)::int FROM message_request WHERE model = $1`, model).Scan(&count); err != nil {
		t.Fatalf("统计夹具行失败: %v", err)
	}
	return count
}

func countLedgerRowsForRequests(t *testing.T, pools *store.Pools, ids []int64) int {
	t.Helper()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写连接失败: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*)::int FROM usage_ledger WHERE request_id = ANY($1::bigint[])`, ids).Scan(&count); err != nil {
		t.Fatalf("统计账本行失败: %v", err)
	}
	return count
}

// logCleanupDueSchedule 返回一个「在任何墙钟时刻都判定为到点」的调度表达式。
//
// 原理：写成「当前分钟 + 每小时」（`M * * * *`，即 logCleanupPlan.hourly）。则不论 now 的分钟
// 数大于还是小于 M，最近一个已过去的计划时刻都在两小时以内，必落在补跑窗口内。
// 写成「当前时刻的小时+分钟」反而会脆：若当前分钟小于 cron 分钟且恰在整点附近，可能越出补跑窗口。
func logCleanupDueSchedule(now time.Time) string {
	return fmt.Sprintf("%d * * * *", now.Minute())
}

// newLogCleanupAgainstDB 用「假设置 + 真执行面」构造任务。
//
// 设置走假面是为了确定性（库里的 system_settings 可能被别的用例改过，且时区/调度值与这次断言无关）；
// 执行面走真库是这条用例的全部意义所在。这也是本文件与 logcleanup_test.go 的分工。
func newLogCleanupAgainstDB(t *testing.T, pools *store.Pools, now time.Time, retentionDays int) *LogCleanup {
	t.Helper()
	return newLogCleanupWithSeams(
		OpsDeps{Pools: pools, Now: func() time.Time { return now }, Logger: logx.New(nil)},
		LogCleanupConfig{Enabled: true},
		&fakeLogCleanupSettings{settings: cleanupSettings(
			true, &retentionDays, nil, stringPtr(logCleanupDueSchedule(now)),
		)},
		pools,
	)
}

func TestIntegrationLogCleanupDeletesOnlyExpiredRows(t *testing.T) {
	pools := logCleanupDisposablePools(t)
	prefix := fmt.Sprintf("%s%d", logCleanupFixturePrefix, time.Now().UnixNano())
	// 时钟固定为当前时刻，保留 30 天 → 截止时刻 = now - 30d，三行夹具相对它定位。
	runAt := time.Now().Truncate(time.Second)
	runAt = time.Date(runAt.Year(), runAt.Month(), runAt.Day(), runAt.Hour(), runAt.Minute(), 0, 0, time.Local)
	cutoff := runAt.AddDate(0, 0, -30)

	ids := seedLogCleanupRows(t, pools, prefix, cutoff)
	ledgerBefore := countLedgerRowsForRequests(t, pools, ids)

	task := newLogCleanupAgainstDB(t, pools, runAt, 30)
	outcome, err := task.Run(context.Background())
	if err != nil {
		t.Fatalf("清理执行失败: %v", err)
	}

	// 严格过期行应被删；严格未过期行必须留下。
	if got := countLogCleanupFixtures(t, pools, prefix+"-expired"); got != 0 {
		t.Fatalf("截止时刻之前一小时的行应被删除，仍剩 %d 行", got)
	}
	if got := countLogCleanupFixtures(t, pools, prefix+"-fresh"); got != 1 {
		t.Fatalf("截止时刻之后一小时的行不该被删（保留 30 天），实际剩 %d 行", got)
	}

	// 边界行：判据是 `created_at <= beforeDate`（Node 的 SQL 用 `<=`），恰好落在截止时刻的行
	// **应被删除**。这条是回归防线：把 `<=` 写成 `<` 时边界行为会变。
	if got := countLogCleanupFixtures(t, pools, prefix+"-boundary"); got != 0 {
		t.Fatalf("恰好落在截止时刻的行应被删除（Node 用 <=），仍剩 %d 行", got)
	}
	if outcome.Processed < 2 {
		t.Fatalf("本轮至少应删掉夹具里的 2 行，实际 Processed=%d", outcome.Processed)
	}

	// 账本不参与清理（store 层文件头第 1 条）：请求行被删后账本行仍在。
	if after := countLedgerRowsForRequests(t, pools, ids); after != ledgerBefore {
		t.Fatalf("usage_ledger 不参与清理：前 %d 行、后 %d 行", ledgerBefore, after)
	}
	if ledgerBefore == 0 {
		t.Fatal("账本行应被触发器写入（夹具或触发器有问题，该断言失去意义）")
	}
}

// TestIntegrationLogCleanupRetriesAfterFailure 钉住「失败不记账、同一窗口可重试补齐」。
//
// Node 侧对应的是 Bull 的 `attempts: 3`（失败重试）；Go 侧等价语义是「失败不写 lastRunKey，
// 下一跳（或重启后）重试同一窗口」。这条性质用单库很难看出，故这里用真库端到端验。
func TestIntegrationLogCleanupRetriesAfterFailure(t *testing.T) {
	pools := logCleanupDisposablePools(t)
	prefix := fmt.Sprintf("%s%d", logCleanupFixturePrefix, time.Now().UnixNano())
	runAt := time.Now().Truncate(time.Second)
	runAt = time.Date(runAt.Year(), runAt.Month(), runAt.Day(), runAt.Hour(), runAt.Minute(), 0, 0, time.Local)
	seedLogCleanupRows(t, pools, prefix, runAt.AddDate(0, 0, -30))

	// 已取消的 ctx：确定性拿到失败（等价于执行中被打断），且不得因失败就把窗口记账。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	failed := newLogCleanupAgainstDB(t, pools, runAt, 30)
	if _, err := failed.Run(cancelled); err == nil {
		t.Fatal("ctx 已取消时必须报错（否则「清理一直失败」只在结果字段里静默存在）")
	}
	if got := countLogCleanupFixtures(t, pools, prefix+"-expired"); got != 1 {
		t.Fatalf("失败那轮不该已经删掉行，实际剩 %d 行", got)
	}

	// 同一窗口重试必须能补齐。
	retried := newLogCleanupAgainstDB(t, pools, runAt, 30)
	outcome, err := retried.Run(context.Background())
	if err != nil {
		t.Fatalf("重试应成功: %v", err)
	}
	if outcome.Processed < 2 {
		t.Fatalf("重试应把夹具里的 2 行去掉，实际 Processed=%d", outcome.Processed)
	}
	if got := countLogCleanupFixtures(t, pools, prefix+"-expired"); got != 0 {
		t.Fatalf("重试后过期行应清空，实际剩 %d 行", got)
	}
}

func TestIntegrationLogCleanupIsIdempotent(t *testing.T) {
	pools := logCleanupDisposablePools(t)
	prefix := fmt.Sprintf("%s%d", logCleanupFixturePrefix, time.Now().UnixNano())
	runAt := time.Now().Truncate(time.Second)
	runAt = time.Date(runAt.Year(), runAt.Month(), runAt.Day(), runAt.Hour(), runAt.Minute(), 0, 0, time.Local)
	seedLogCleanupRows(t, pools, prefix, runAt.AddDate(0, 0, -30))

	// 第一次：用新实例（不共享 lastRunKey），模拟重启后再跑同一窗口。
	first := newLogCleanupAgainstDB(t, pools, runAt, 30)
	firstOutcome, err := first.Run(context.Background())
	if err != nil {
		t.Fatalf("首轮失败: %v", err)
	}

	// 第二次：新实例、同一时刻 → 删不到东西（幂等），且不得报错。
	second := newLogCleanupAgainstDB(t, pools, runAt, 30)
	secondOutcome, err := second.Run(context.Background())
	if err != nil {
		t.Fatalf("次轮失败: %v", err)
	}
	if secondOutcome.Processed != 0 {
		t.Fatalf("同一窗口重复执行应删 0 行（幂等），实际 %d（首轮 %d）", secondOutcome.Processed, firstOutcome.Processed)
	}
	if secondOutcome.Fields["vacuumPerformed"] != false {
		t.Fatalf("无删除时不应触发 VACUUM（Node 同判），实际 %+v", secondOutcome.Fields)
	}
}
