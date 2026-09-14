package store

import (
	"context"
	"math"
	"testing"
	"time"
)

// 本文件是统计面 store 层的真库用例，补端点级用例没覆盖的两件事：
//
//  1. **mixed 模式**（非管理员 + allowGlobalUsageView 时走的那条）：「自己的密钥明细 + 其他用户
//     汇总」两半的数据源与虚拟实体（user_id = -1 / "__others__"）；
//  2. **四个时间段的桶数**（today 24 / 7days 7 / 30days 30 / thisMonth = 今日是几号）。
//
// 夹具自钉唯一候选并按精确 id 清理（与其它 store 集成用例同纪律）。

const statisticsITKeyPrefix = "go-store-stats-it"

// statisticsFixture 是两份夹具：目标用户（自己）与另一个用户（others 的来源）。
type statisticsFixture struct {
	userID     int64
	keyID      int64
	keyString  string
	otherID    int64
	otherKey   string
	providerID int64
}

func seedStatisticsFixture(t *testing.T, pools *Pools) statisticsFixture {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}

	marker := itKey(t)
	fixture := statisticsFixture{
		keyString: statisticsITKeyPrefix + "-own-" + marker,
		otherKey:  statisticsITKeyPrefix + "-other-" + marker,
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority,
			group_tag, protocol_conversion_enabled)
		VALUES ($1, 'http://127.0.0.1:9', 'upstream-not-used', 'codex', true, 1, 0, 'default', true)
		RETURNING id`, statisticsITKeyPrefix+"-"+marker).Scan(&fixture.providerID); err != nil {
		t.Fatalf("建供应商失败: %v", err)
	}

	for _, target := range []struct {
		userID *int64
		keyID  *int64
		key    string
	}{
		{&fixture.userID, &fixture.keyID, fixture.keyString},
		{&fixture.otherID, new(int64), fixture.otherKey},
	} {
		if err := pool.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING id`,
			statisticsITKeyPrefix+"-"+marker).Scan(target.userID); err != nil {
			t.Fatalf("建用户失败: %v", err)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO keys (user_id, key, name, is_enabled, provider_group)
			VALUES ($1, $2, $3, true, 'default') RETURNING id`,
			*target.userID, target.key, target.key).Scan(target.keyID); err != nil {
			t.Fatalf("建密钥失败: %v", err)
		}
	}

	// 目标用户两行（1.5 + 0.5），另一个用户一行（3）；三行都在当前桶里。
	requestID := int(time.Now().UnixNano() % 1_000_000_000)
	insert := func(userID int64, key, cost string) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO usage_ledger (request_id, provider_id, user_id, key, final_provider_id, model,
				cost_usd, input_tokens, output_tokens, status_code, is_success, duration_ms,
				created_at, is_replay, blocked_by)
			VALUES ($1, $2, $3, $4, $2, $5, $6::numeric, 1, 1, 200, true, 1, now(), false, NULL)`,
			requestID, fixture.providerID, userID, key, statisticsITKeyPrefix, cost); err != nil {
			t.Fatalf("种账本行失败: %v", err)
		}
		requestID++
	}
	insert(fixture.userID, fixture.keyString, "1.5")
	insert(fixture.userID, fixture.keyString, "0.5")
	insert(fixture.otherID, fixture.otherKey, "3")

	t.Cleanup(func() {
		cleanup := context.Background()
		for _, statement := range []string{
			`DELETE FROM usage_ledger WHERE user_id = ANY($1)`,
			`DELETE FROM keys WHERE user_id = ANY($1)`,
			`DELETE FROM users WHERE id = ANY($1)`,
		} {
			if _, err := pool.Exec(cleanup, statement, []int64{fixture.userID, fixture.otherID}); err != nil {
				t.Errorf("清理夹具失败（%s）: %v", statement, err)
			}
		}
		if _, err := pool.Exec(cleanup,
			`UPDATE providers SET is_enabled = false, deleted_at = now() WHERE id = $1`,
			fixture.providerID); err != nil {
			t.Errorf("清理供应商夹具失败: %v", err)
		}
	})
	return fixture
}

// TestIntegrationChartStatisticsBuckets 钉住四个时间段的桶数（时区固定 UTC 以求可复现）。
func TestIntegrationChartStatisticsBuckets(t *testing.T) {
	pools := openTestPools(t)
	fixture := seedStatisticsFixture(t, pools)
	ctx := context.Background()
	const timezone = "UTC"

	for _, item := range []struct {
		timeRange AdminStatisticsRange
		want      int
	}{
		{AdminStatisticsToday, 24},
		{AdminStatistics7Days, 7},
		{AdminStatistics30Days, 30},
		// thisMonth 是本月 1 日到今天，桶数等于今天的号数。
		{AdminStatisticsThisMonth, time.Now().UTC().Day()},
	} {
		rows, err := pools.AdminChartKeyStatistics(ctx, fixture.userID, item.timeRange, timezone)
		if err != nil {
			t.Fatalf("%s：查询密钥统计失败: %v", item.timeRange, err)
		}
		// 1 个实体 × 桶数。
		if len(rows) != item.want {
			t.Fatalf("%s 应有 %d 行（1 实体 × %d 桶），实际 %d",
				item.timeRange, item.want, item.want, len(rows))
		}
		wantResolution := "day"
		if item.timeRange == AdminStatisticsToday {
			wantResolution = "hour"
		}
		if got := item.timeRange.Resolution(); got != wantResolution {
			t.Fatalf("%s 的分辨率应为 %s，实际 %s", item.timeRange, wantResolution, got)
		}

		// 夹具的两行都在当前桶里：有数据的行消费是 numeric 文本，其余是零填充。
		calls, filled, empty := int64(0), 0, 0
		for _, row := range rows {
			if row.KeyID != fixture.keyID || row.KeyName != fixture.keyString {
				t.Fatalf("行的密钥应为 (%d, %q)，实际 (%d, %q)",
					fixture.keyID, fixture.keyString, row.KeyID, row.KeyName)
			}
			calls += row.APICalls
			if row.ZeroFilled {
				empty++
				if row.APICalls != 0 || row.CostText != "0" {
					t.Fatalf("零填充行必须是 0 / \"0\"，实际 %d / %q", row.APICalls, row.CostText)
				}
				continue
			}
			filled++
			if row.APICalls != 2 || row.CostText != "2.000000000000000" {
				t.Fatalf("%s 的填实行应为 2 次 / 2.000000000000000，实际 %d / %q",
					item.timeRange, row.APICalls, row.CostText)
			}
		}
		if calls != 2 {
			t.Fatalf("%s 的总计数应为 2，实际 %d", item.timeRange, calls)
		}
		if filled == 0 || filled+empty != item.want {
			t.Fatalf("%s 应有 %d 个桶（含至少一个填实行），实际 填充 %d / 空 %d",
				item.timeRange, item.want, filled, empty)
		}
	}
}

// TestIntegrationChartMixedStatistics 钉住 mixed 模式的两半：
// ownKeys 只含自己的密钥，othersAggregate 只含**其他用户**且实体是虚拟的 -1 / "__others__"。
func TestIntegrationChartMixedStatistics(t *testing.T) {
	pools := openTestPools(t)
	fixture := seedStatisticsFixture(t, pools)
	ctx := context.Background()

	ownKeys, others, err := pools.AdminChartMixedStatistics(ctx, fixture.userID,
		AdminStatisticsToday, "UTC")
	if err != nil {
		t.Fatalf("mixed 查询失败: %v", err)
	}
	if len(ownKeys) != 24 {
		t.Fatalf("ownKeys 应有 24 行，实际 %d", len(ownKeys))
	}
	if len(others) != 24 {
		t.Fatalf("othersAggregate 应有 24 行，实际 %d", len(others))
	}

	ownCalls, ownCost := int64(0), ""
	for _, row := range ownKeys {
		if row.KeyID != fixture.keyID || row.KeyName != fixture.keyString {
			t.Fatalf("ownKeys 只应含自己的密钥 %q，实际 %q", fixture.keyString, row.KeyName)
		}
		ownCalls += row.APICalls
		if !row.ZeroFilled {
			ownCost = row.CostText
		}
	}
	if ownCalls != 2 || ownCost != "2.000000000000000" {
		t.Fatalf("ownKeys 应为 2 次 / 2.000000000000000，实际 %d / %q", ownCalls, ownCost)
	}

	otherCalls := int64(0)
	for _, row := range others {
		if row.UserID != -1 || row.UserName != "__others__" {
			t.Fatalf("othersAggregate 的实体应为 -1 / \"__others__\"，实际 %d / %q",
				row.UserID, row.UserName)
		}
		otherCalls += row.APICalls
	}

	// others 是**全库其他用户**的合计，共享库里还有别的 lane 的数据，故不能写死数值。
	//
	// 旧写法另跑一条 SQL 算期望值：两次查询之间没有快照隔离，别的包正好插入账本行时期望就会偏大
	// （全模块并行时稳定 +1，单包恒绿——既是假红，又掩盖真缺陷）。
	//
	// 改为 **SUT 自证的分区恒等式**：mixed 的 own + others 恒等于全库合计，与「谁是 own」无关。
	// 换一个被排除的用户再问一次，两边合计必须相等。断言只用 SUT 的输出（不读全库计数），
	// 与库体量无关；真缺陷（自己的行没被排除 / 真实行被丢）在任何快照下都不成立。
	// 并发的其它包写库会让两次读数落到不同快照（正是旧写法的病根），这类瞬时不一致重算即可；
	// 真缺陷重算多少次都不成立。
	const partitionAttempts = 8
	var (
		ownA, othersA, ownB, othersB mixedStatisticsPartition
		consistent                   bool
	)
	for range partitionAttempts {
		ownA, othersA = readMixedStatistics(t, pools, fixture.userID)
		ownB, othersB = readMixedStatistics(t, pools, fixture.otherID)
		if ownA.plus(othersA).equal(ownB.plus(othersB)) {
			consistent = true
			break
		}
	}
	if !consistent {
		t.Fatalf("重算 %d 次后分区合计仍不一致：own(A)=%+v others(A)=%+v | own(B)=%+v others(B)=%+v",
			partitionAttempts, ownA, othersA, ownB, othersB)
	}
	// 夹具自证：自己的半恒为 2 次；被排除的用户必须真的进了 others（它贡献 3 次）。
	if ownA.calls != 2 {
		t.Fatalf("own(A) 应为 2 次，实际 %d", ownA.calls)
	}
	if otherCalls < 3 {
		t.Fatalf("others 应含被排除用户的 3 次调用，实际合计仅 %d 次", otherCalls)
	}
}

// mixedStatisticsPartition 是 mixed 查询两半的合计（分区恒等式自证用）。
type mixedStatisticsPartition struct {
	calls int64
	cost  float64
}

func (p mixedStatisticsPartition) plus(other mixedStatisticsPartition) mixedStatisticsPartition {
	return mixedStatisticsPartition{calls: p.calls + other.calls, cost: p.cost + other.cost}
}

// equal 按相对容差比对：逐桶浮点相加与一次性求和会有末位差。
func (p mixedStatisticsPartition) equal(other mixedStatisticsPartition) bool {
	if p.calls != other.calls {
		return false
	}
	scale := math.Max(1, math.Max(math.Abs(p.cost), math.Abs(other.cost)))
	return math.Abs(p.cost-other.cost) <= 1e-9*scale
}

// readMixedStatistics 跑一次 mixed 查询并把两半折成合计。
func readMixedStatistics(t *testing.T, pools *Pools, userID int64) (mixedStatisticsPartition, mixedStatisticsPartition) {
	t.Helper()
	ownKeys, others, err := pools.AdminChartMixedStatistics(context.Background(), userID,
		AdminStatisticsToday, "UTC")
	if err != nil {
		t.Fatalf("mixed 分区查询失败: %v", err)
	}
	own, other := mixedStatisticsPartition{}, mixedStatisticsPartition{}
	for _, row := range ownKeys {
		own.calls += row.APICalls
		own.cost += parseNumericText(row.CostText)
	}
	for _, row := range others {
		other.calls += row.APICalls
		other.cost += parseNumericText(row.CostText)
	}
	return own, other
}

// chartUserStatisticsAttempts 是「两次活跃用户集合读数一致」的最大尝试次数。
//
// 5 次足以在共享库的常规颤动下拿到一个稳定窗口；持续 5 次都变说明这套件不该在这台库上跑，
// 那时 Skip（见用例内的注释）。
const chartUserStatisticsAttempts = 5

// sameActiveUserSet 比较两次读数是否**逐个相同**（不能只比长度：一增一删时长度可能相等）。
func sameActiveUserSet(a, b []AdminStatisticsEntity) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}

// TestIntegrationChartUserStatisticsZeroFill 钉住 users 模式：全库未软删用户 × 桶数的笛卡尔积。
func TestIntegrationChartUserStatisticsZeroFill(t *testing.T) {
	pools := openTestPools(t)
	fixture := seedStatisticsFixture(t, pools)
	ctx := context.Background()

	// 为何前后各读一次活跃用户集合：本用例断言的是「全库活跃用户 × 桶」的笛卡尔积，它只有在
	// **测量窗内集合不变**时才是可判定的。而共享库上的 `users` 表会被兄弟套件改变（任何建用户 /
	// 软删用户的用例都会），两次读不一致时这个断言就不再成立。实测（全模块并行）：期望
	// 7×365=2555，实际 2562=7×366——两次读之间多了一个用户，报告出来却是「零填充错了」。
	// 故：两次集合一致才断言；否则有界重试；用尽则 Skip 并说明是环境不允许，而不是产品有问题。
	var entities []AdminStatisticsEntity
	var rows []AdminStatisticsUserRow
	for attempt := 1; ; attempt++ {
		before, err := pools.AdminActiveUserEntities(ctx)
		if err != nil {
			t.Fatalf("查询活跃用户失败: %v", err)
		}
		rows, err = pools.AdminChartUserStatistics(ctx, AdminStatistics7Days, "UTC")
		if err != nil {
			t.Fatalf("查询用户统计失败: %v", err)
		}
		after, err := pools.AdminActiveUserEntities(ctx)
		if err != nil {
			t.Fatalf("复读活跃用户失败: %v", err)
		}
		if sameActiveUserSet(before, after) {
			entities = before
			break
		}
		if attempt == chartUserStatisticsAttempts {
			t.Skipf("共享库的活跃用户集合在测量窗内持续变化（前 %d 个、后 %d 个）：本用例要求集合稳定，跳过而不是报假红",
				len(before), len(after))
		}
	}
	if len(rows) != 7*len(entities) {
		t.Fatalf("users 模式应有 7 × %d = %d 行，实际 %d",
			len(entities), 7*len(entities), len(rows))
	}
	// 顺序：桶升序为主、实体名升序为次。
	for index, row := range rows {
		wantBucket := index / len(entities)
		wantEntity := index % len(entities)
		if !row.Bucket.Equal(rows[wantBucket*len(entities)].Bucket) {
			t.Fatalf("第 %d 行的桶不对（应为第 %d 个桶）", index, wantBucket)
		}
		if row.UserID != entities[wantEntity].ID {
			t.Fatalf("第 %d 行的实体应为 %d，实际 %d",
				index, entities[wantEntity].ID, row.UserID)
		}
	}

	calls := int64(0)
	for _, row := range rows {
		if row.UserID == fixture.userID {
			calls += row.APICalls
		}
	}
	if calls != 2 {
		t.Fatalf("夹具用户的 7 天计数应为 2，实际 %d", calls)
	}
}
