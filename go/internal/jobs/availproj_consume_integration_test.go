package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是消费侧的**真库集成验证**，也是「与 Node 对照」的落点：
//
//   - 夹具：testdata/availproj_consume_fixture.json（与 Node golden 脚本共用同一份）
//   - 期望：testdata/node_availproj_consume_golden.json（由 scripts/availproj-consume-golden.ts
//     直接调用 Node 的 processBatch 产出，见该脚本头部说明）
//   - 表结构：testdata/availproj_tables.sql（从真实 schema dump）
//
// 门控：只有设置了 CCH_AVAILPROJ_TEST_DSN（指向一个**可弃库**）才跑。用独立变量而不是
// CCH_TEST_DSN，是因为本用例会 TRUNCATE 四张投影表——那是破坏性操作，绝不能落在共享测试库上。
//
// 时间无关性：夹具里所有时刻都是相对「UTC 分钟截断的 now」的偏移，两侧各自 materialize，
// 故 golden 与本次运行永远可比（不会因为隔了几分钟而变红）。

const (
	availProjectionTestDSNEnv = "CCH_AVAILPROJ_TEST_DSN"
	availProjectionTables     = "outbox_events, proj_applied_requests, avail_bucket_1m, avail_current"
)

type fixtureEvent struct {
	EventID       string         `json:"event_id"`
	AggregateID   int64          `json:"aggregate_id"`
	MinuteOffset  int            `json:"minute_offset"`
	Payload       map[string]any `json:"payload"`
	PayloadString *string        `json:"payload_string"`
}

type fixtureExistingBucket struct {
	ProviderID             int64 `json:"provider_id"`
	OccurredMinuteOffset   int   `json:"occurred_minute_offset"`
	LastRequestAtSecondOff int   `json:"last_request_at_second_offset"`
	SuccessCnt             int   `json:"success_cnt"`
	FailureCnt             int   `json:"failure_cnt"`
	ExcludedCnt            int   `json:"excluded_cnt"`
	LatencyCnt             int   `json:"latency_cnt"`
	LatencySumMS           int64 `json:"latency_sum_ms"`
}

type fixtureExistingCurrent struct {
	ProviderID                int64   `json:"provider_id"`
	State                     string  `json:"state"`
	Availability              float64 `json:"availability"`
	RequestCount              int     `json:"request_count"`
	LastRequestAtMinuteOffset int     `json:"last_request_at_minute_offset"`
}

type projectionFixture struct {
	Events          []fixtureEvent           `json:"events"`
	ExistingBuckets []fixtureExistingBucket  `json:"existing_buckets"`
	ExistingCurrent []fixtureExistingCurrent `json:"existing_current"`
}

type goldenBucket struct {
	ProviderID                 int64  `json:"provider_id"`
	BucketOffsetSeconds        *int64 `json:"bucket_offset_seconds"`
	SuccessCnt                 int    `json:"success_cnt"`
	FailureCnt                 int    `json:"failure_cnt"`
	ExcludedCnt                int    `json:"excluded_cnt"`
	LatencyCnt                 int    `json:"latency_cnt"`
	LatencySumMS               int64  `json:"latency_sum_ms"`
	LastRequestAtOffsetSeconds *int64 `json:"last_request_at_offset_seconds"`
}

type goldenCurrent struct {
	ProviderID                 int64   `json:"provider_id"`
	State                      string  `json:"state"`
	Availability               float64 `json:"availability"`
	RequestCount               int     `json:"request_count"`
	LastRequestAtOffsetSeconds *int64  `json:"last_request_at_offset_seconds"`
}

type goldenOutbox struct {
	EventID   string  `json:"event_id"`
	Published bool    `json:"published"`
	Attempts  int     `json:"attempts"`
	LastError *string `json:"last_error"`
}

type projectionGolden struct {
	AppliedRequestIDs []int64         `json:"applied_request_ids"`
	Buckets           []goldenBucket  `json:"buckets"`
	Current           []goldenCurrent `json:"current"`
	Outbox            []goldenOutbox  `json:"outbox"`
}

func availProjectionPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv(availProjectionTestDSNEnv)
	if dsn == "" {
		t.Skipf("未设置 %s（需指向可弃库），跳过可用性投影集成测试", availProjectionTestDSNEnv)
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-availproj-it",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	return raw
}

// failingProjectionRunner 让测试在真事务内注入失败。
type failingProjectionRunner struct {
	run func(inner func(ctx context.Context, tx ProjectionTxOps) error) error
}

func (r failingProjectionRunner) RunProjectionTx(
	ctx context.Context,
	fn func(ctx context.Context, tx ProjectionTxOps) error,
) error {
	return r.run(fn)
}

func truncateMinuteUTC(now time.Time) time.Time {
	return now.UTC().Truncate(time.Minute)
}

func ensureProjectionTables(t *testing.T, ctx context.Context, writer *store.Pool) {
	t.Helper()
	var existing int
	if err := writer.QueryRow(
		ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname='public'
		 AND tablename IN ('outbox_events','proj_applied_requests','avail_bucket_1m','avail_current')`,
	).Scan(&existing); err != nil {
		t.Fatalf("检查投影表存在性失败: %v", err)
	}
	if existing == 4 {
		return
	}
	ddl := readTestdata(t, "availproj_tables.sql")
	var lines []string
	for _, line := range strings.Split(string(ddl), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "\\") {
			continue
		}
		lines = append(lines, line)
	}
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";\n") {
		trimmed := strings.TrimSpace(statement)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if _, err := writer.Exec(ctx, trimmed); err != nil {
			t.Fatalf("建投影表失败（%s）: %v", trimmed[:min(len(trimmed), 60)], err)
		}
	}
}

func seedProjectionFixture(t *testing.T, ctx context.Context, writer *store.Pool, fixture projectionFixture, base time.Time) {
	t.Helper()
	for _, event := range fixture.Events {
		occurredAt := base.Add(time.Duration(event.MinuteOffset) * time.Minute)
		var payloadArg any
		if event.Payload == nil {
			// 非对象 payload（合法 jsonb 的字符串）：必须先引号化成 JSON 字面量，否则 ::jsonb 报错。
			quoted, err := json.Marshal(*event.PayloadString)
			if err != nil {
				t.Fatalf("序列化 payload 字符串失败: %v", err)
			}
			payloadArg = string(quoted)
		} else {
			materialized := make(map[string]any, len(event.Payload)+1)
			for key, value := range event.Payload {
				if key == "occurred_minute_offset" {
					continue
				}
				materialized[key] = value
			}
			materialized["occurred_at"] = occurredAt.Format(time.RFC3339Nano)
			encoded, err := json.Marshal(materialized)
			if err != nil {
				t.Fatalf("序列化 payload 失败: %v", err)
			}
			payloadArg = string(encoded)
		}
		if _, err := writer.Exec(ctx,
			`INSERT INTO outbox_events (event_id, event_type, aggregate_type, aggregate_id, occurred_at, payload)
			 VALUES ($1::uuid, 'request_finalized', 'message_request', $2, $3, $4::text::jsonb)`,
			event.EventID, event.AggregateID, occurredAt, payloadArg,
		); err != nil {
			t.Fatalf("插入夹具事件失败: %v", err)
		}
	}
	for _, bucket := range fixture.ExistingBuckets {
		if _, err := writer.Exec(ctx,
			`INSERT INTO avail_bucket_1m (provider_id, bucket_start, success_cnt, failure_cnt, excluded_cnt,
			                              latency_cnt, latency_sum_ms, last_request_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			bucket.ProviderID,
			base.Add(time.Duration(bucket.OccurredMinuteOffset)*time.Minute),
			bucket.SuccessCnt, bucket.FailureCnt, bucket.ExcludedCnt,
			bucket.LatencyCnt, bucket.LatencySumMS,
			base.Add(time.Duration(bucket.OccurredMinuteOffset)*time.Minute).Add(time.Duration(bucket.LastRequestAtSecondOff)*time.Second),
		); err != nil {
			t.Fatalf("插入既有桶失败: %v", err)
		}
	}
	for _, current := range fixture.ExistingCurrent {
		if _, err := writer.Exec(ctx,
			`INSERT INTO avail_current (provider_id, state, availability, request_count, last_request_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, now())`,
			current.ProviderID, current.State, current.Availability, current.RequestCount,
			base.Add(time.Duration(current.LastRequestAtMinuteOffset)*time.Minute),
		); err != nil {
			t.Fatalf("插入既有 avail_current 失败: %v", err)
		}
	}
}

func offsetSecondsPtr(value *time.Time, base time.Time) *int64 {
	if value == nil {
		return nil
	}
	seconds := int64(value.Sub(base).Seconds())
	return &seconds
}

func dumpProjectionState(t *testing.T, ctx context.Context, writer *store.Pool, base time.Time) projectionGolden {
	t.Helper()
	state := projectionGolden{}

	rows, err := writer.Query(ctx,
		`SELECT provider_id, bucket_start, success_cnt, failure_cnt, excluded_cnt,
		        latency_cnt, latency_sum_ms, last_request_at
		 FROM avail_bucket_1m ORDER BY provider_id, bucket_start`)
	if err != nil {
		t.Fatalf("读桶失败: %v", err)
	}
	for rows.Next() {
		var bucket goldenBucket
		var bucketStart time.Time
		var lastRequestAt *time.Time
		if err := rows.Scan(&bucket.ProviderID, &bucketStart, &bucket.SuccessCnt, &bucket.FailureCnt,
			&bucket.ExcludedCnt, &bucket.LatencyCnt, &bucket.LatencySumMS, &lastRequestAt); err != nil {
			rows.Close()
			t.Fatalf("扫描桶失败: %v", err)
		}
		bucket.BucketOffsetSeconds = offsetSecondsPtr(&bucketStart, base)
		bucket.LastRequestAtOffsetSeconds = offsetSecondsPtr(lastRequestAt, base)
		state.Buckets = append(state.Buckets, bucket)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历桶失败: %v", err)
	}

	currentRows, err := writer.Query(ctx,
		`SELECT provider_id, state, availability, request_count, last_request_at
		 FROM avail_current ORDER BY provider_id`)
	if err != nil {
		t.Fatalf("读 avail_current 失败: %v", err)
	}
	for currentRows.Next() {
		var current goldenCurrent
		var lastRequestAt *time.Time
		if err := currentRows.Scan(&current.ProviderID, &current.State, &current.Availability,
			&current.RequestCount, &lastRequestAt); err != nil {
			currentRows.Close()
			t.Fatalf("扫描 avail_current 失败: %v", err)
		}
		current.LastRequestAtOffsetSeconds = offsetSecondsPtr(lastRequestAt, base)
		state.Current = append(state.Current, current)
	}
	currentRows.Close()

	outboxRows, err := writer.Query(ctx,
		`SELECT event_id::text, published_at, attempts, last_error FROM outbox_events ORDER BY event_id`)
	if err != nil {
		t.Fatalf("读 outbox 失败: %v", err)
	}
	for outboxRows.Next() {
		var entry goldenOutbox
		var publishedAt *time.Time
		if err := outboxRows.Scan(&entry.EventID, &publishedAt, &entry.Attempts, &entry.LastError); err != nil {
			outboxRows.Close()
			t.Fatalf("扫描 outbox 失败: %v", err)
		}
		entry.Published = publishedAt != nil
		state.Outbox = append(state.Outbox, entry)
	}
	outboxRows.Close()

	appliedRows, err := writer.Query(ctx, `SELECT request_id FROM proj_applied_requests ORDER BY request_id`)
	if err != nil {
		t.Fatalf("读 applied 失败: %v", err)
	}
	for appliedRows.Next() {
		var requestID int64
		if err := appliedRows.Scan(&requestID); err != nil {
			appliedRows.Close()
			t.Fatalf("扫描 applied 失败: %v", err)
		}
		state.AppliedRequestIDs = append(state.AppliedRequestIDs, requestID)
	}
	appliedRows.Close()

	sort.Slice(state.Buckets, func(i, j int) bool {
		if state.Buckets[i].ProviderID != state.Buckets[j].ProviderID {
			return state.Buckets[i].ProviderID < state.Buckets[j].ProviderID
		}
		return *state.Buckets[i].BucketOffsetSeconds < *state.Buckets[j].BucketOffsetSeconds
	})
	return state
}

func compareInt64Ptr(t *testing.T, label string, want, got *int64) {
	t.Helper()
	if want == nil || got == nil {
		if want != got {
			t.Fatalf("%s：期望 %v 实际 %v", label, formatPtr(want), formatPtr(got))
		}
		return
	}
	if *want != *got {
		t.Fatalf("%s：期望 %d 实际 %d", label, *want, *got)
	}
}

func formatPtr(value *int64) string {
	if value == nil {
		return "nil"
	}
	return fmt.Sprintf("%d", *value)
}

// TestIntegrationAvailProjectionConsumeMatchesNodeGolden 是 G1 的核心验收：
// 同一份夹具分别由 **Node 的 processBatch** 与 **Go 的消费侧**处理，落库状态逐字段一致。
func TestIntegrationAvailProjectionConsumeMatchesNodeGolden(t *testing.T) {
	pools := availProjectionPools(t)
	ctx := context.Background()

	var fixture projectionFixture
	if err := json.Unmarshal(readTestdata(t, "availproj_consume_fixture.json"), &fixture); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	var golden projectionGolden
	if err := json.Unmarshal(readTestdata(t, "node_availproj_consume_golden.json"), &golden); err != nil {
		t.Fatalf("解析 golden 失败: %v", err)
	}

	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	ensureProjectionTables(t, ctx, writer)
	if _, err := writer.Exec(ctx, `TRUNCATE `+availProjectionTables+` RESTART IDENTITY`); err != nil {
		t.Fatalf("清表失败: %v", err)
	}
	base := truncateMinuteUTC(time.Now())
	seedProjectionFixture(t, ctx, writer, fixture, base)

	consumer, err := NewAvailProjectionConsumer(AvailProjectionConsumerOptions{Pools: pools})
	if err != nil {
		t.Fatalf("构造消费侧失败: %v", err)
	}
	result, err := consumer.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}

	state := dumpProjectionState(t, ctx, writer, base)

	// 1) 应用条数与幂等集合
	if result.Applied != len(golden.AppliedRequestIDs) {
		t.Fatalf("应用条数应与 Node 一致：期望 %d 实际 %d", len(golden.AppliedRequestIDs), result.Applied)
	}
	if fmt.Sprint(state.AppliedRequestIDs) != fmt.Sprint(golden.AppliedRequestIDs) {
		t.Fatalf("幂等集合应与 Node 一致：\n  node=%v\n  go  =%v", golden.AppliedRequestIDs, state.AppliedRequestIDs)
	}

	// 2) 桶逐条比对
	if len(state.Buckets) != len(golden.Buckets) {
		t.Fatalf("桶数应与 Node 一致：期望 %d 实际 %d\n  node=%+v\n  go  =%+v",
			len(golden.Buckets), len(state.Buckets), golden.Buckets, state.Buckets)
	}
	for index := range golden.Buckets {
		want, got := golden.Buckets[index], state.Buckets[index]
		if want.ProviderID != got.ProviderID {
			t.Fatalf("第 %d 个桶的 provider 不符：期望 %d 实际 %d", index, want.ProviderID, got.ProviderID)
		}
		compareInt64Ptr(t, fmt.Sprintf("桶 %d 的 bucket_offset", want.ProviderID), want.BucketOffsetSeconds, got.BucketOffsetSeconds)
		if want.SuccessCnt != got.SuccessCnt || want.FailureCnt != got.FailureCnt ||
			want.ExcludedCnt != got.ExcludedCnt || want.LatencyCnt != got.LatencyCnt ||
			want.LatencySumMS != got.LatencySumMS {
			t.Fatalf("桶 %d@%v 计数不符：\n  node=%+v\n  go  =%+v",
				want.ProviderID, formatPtr(want.BucketOffsetSeconds), want, got)
		}
		compareInt64Ptr(t, fmt.Sprintf("桶 %d 的 last_request_at", want.ProviderID),
			want.LastRequestAtOffsetSeconds, got.LastRequestAtOffsetSeconds)
	}

	// 3) avail_current 逐条比对
	if len(state.Current) != len(golden.Current) {
		t.Fatalf("avail_current 行数应与 Node 一致：期望 %d 实际 %d\n  node=%+v\n  go  =%+v",
			len(golden.Current), len(state.Current), golden.Current, state.Current)
	}
	for index := range golden.Current {
		want, got := golden.Current[index], state.Current[index]
		if want.ProviderID != got.ProviderID || want.State != got.State ||
			want.Availability != got.Availability || want.RequestCount != got.RequestCount {
			t.Fatalf("avail_current 第 %d 行不符（provider=%d）：\n  node=%+v\n  go  =%+v",
				index, want.ProviderID, want, got)
		}
		compareInt64Ptr(t, fmt.Sprintf("avail_current %d 的 last_request_at", want.ProviderID),
			want.LastRequestAtOffsetSeconds, got.LastRequestAtOffsetSeconds)
	}

	// 4) outbox 标记录逐条比对（含毒丸的 last_error）
	if len(state.Outbox) != len(golden.Outbox) {
		t.Fatalf("outbox 行数应与夹具一致：期望 %d 实际 %d", len(golden.Outbox), len(state.Outbox))
	}
	for index := range golden.Outbox {
		want, got := golden.Outbox[index], state.Outbox[index]
		if want.EventID != got.EventID || want.Published != got.Published ||
			want.Attempts != got.Attempts {
			t.Fatalf("outbox 第 %d 行不符：\n  node=%+v\n  go  =%+v", index, want, got)
		}
		wantError := ""
		if want.LastError != nil {
			wantError = *want.LastError
		}
		gotError := ""
		if got.LastError != nil {
			gotError = *got.LastError
		}
		if wantError != gotError {
			t.Fatalf("outbox %s 的 last_error 不符：node=%q go=%q", want.EventID, wantError, gotError)
		}
	}

	t.Logf("与 Node golden 逐字段一致：applied=%d buckets=%d current=%d outbox=%d",
		result.Applied, len(state.Buckets), len(state.Current), len(state.Outbox))
}

// TestIntegrationAvailProjectionConsumeIsIdempotentAcrossRuns 验证「重复跑不重复计」：
// 第二轮必须零应用、零桶自增（幂等表 + 已发布标记共同保证）。
func TestIntegrationAvailProjectionConsumeIsIdempotentAcrossRuns(t *testing.T) {
	pools := availProjectionPools(t)
	ctx := context.Background()

	var fixture projectionFixture
	if err := json.Unmarshal(readTestdata(t, "availproj_consume_fixture.json"), &fixture); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	ensureProjectionTables(t, ctx, writer)
	if _, err := writer.Exec(ctx, `TRUNCATE `+availProjectionTables+` RESTART IDENTITY`); err != nil {
		t.Fatalf("清表失败: %v", err)
	}
	base := truncateMinuteUTC(time.Now())
	seedProjectionFixture(t, ctx, writer, fixture, base)

	consumer, err := NewAvailProjectionConsumer(AvailProjectionConsumerOptions{Pools: pools})
	if err != nil {
		t.Fatalf("构造消费侧失败: %v", err)
	}
	first, err := consumer.RunOnce(ctx)
	if err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if first.Applied == 0 {
		t.Fatal("第一轮应有应用")
	}
	before := dumpProjectionState(t, ctx, writer, base)

	second, err := consumer.RunOnce(ctx)
	if err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	if second.Applied != 0 {
		t.Fatalf("第二轮不得再应用，实际 %d", second.Applied)
	}
	after := dumpProjectionState(t, ctx, writer, base)
	// 注意用 JSON 而不是 fmt.Sprint：桶结构里的偏移量是 *int64，Sprint 打印的是指针地址。
	beforeJSON, _ := json.Marshal(before.Buckets)
	afterJSON, _ := json.Marshal(after.Buckets)
	if string(afterJSON) != string(beforeJSON) {
		t.Fatalf("第二轮不得改动桶：\n  before=%s\n  after =%s", beforeJSON, afterJSON)
	}
	if fmt.Sprint(after.AppliedRequestIDs) != fmt.Sprint(before.AppliedRequestIDs) {
		t.Fatalf("第二轮不得改动幂等集合")
	}
}

// failingMarkOps 在「标记已发布」这一步注入失败，用来验证真库事务回滚。
//
// 为什么不用「非法 uuid」之类手段：event_id 列就是 uuid 类型，非法值在 INSERT 时就被拒，
// 根本进不了 outbox，构造不出「认领后失败」的场景。装饰器把失败点放在**认领之后**，
// 正好对应真实故障（应用阶段报错），且可复现。
type failingMarkOps struct {
	ProjectionTxOps
}

func (f failingMarkOps) MarkOutboxPublished(context.Context, []int64, *string) error {
	return fmt.Errorf("注入失败：标记已发布阶段")
}

// TestIntegrationAvailProjectionConsumeRollbackReleasesClaim 验证「失败即释放认领」：
//
// Node 侧没有独立 visibility timeout，靠的就是事务回滚后行仍 `published_at IS NULL`。
// 这里让真实事务在「标记已发布」步骤失败，断言：
//   - RunOnce 上抛错误；
//   - 该批**没有任何**行被标记已发布，也没有留下幂等行（整批回滚）；
//   - 换成正常消费者重跑，原本被回滚的事件仍能被处理（认领确实被释放了）。
func TestIntegrationAvailProjectionConsumeRollbackReleasesClaim(t *testing.T) {
	pools := availProjectionPools(t)
	ctx := context.Background()

	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	ensureProjectionTables(t, ctx, writer)
	if _, err := writer.Exec(ctx, `TRUNCATE `+availProjectionTables+` RESTART IDENTITY`); err != nil {
		t.Fatalf("清表失败: %v", err)
	}
	base := truncateMinuteUTC(time.Now())
	payload, _ := json.Marshal(map[string]any{
		"request_id": 9801, "provider_id": 801, "outcome": "success",
		"occurred_at": base.Format(time.RFC3339Nano), "duration_ms": 7,
	})
	if _, err := writer.Exec(ctx,
		`INSERT INTO outbox_events (event_id, event_type, aggregate_type, aggregate_id, occurred_at, payload)
		 VALUES (gen_random_uuid(), 'request_finalized', 'message_request', 9801, $1, $2::text::jsonb)`,
		base, string(payload),
	); err != nil {
		t.Fatalf("插入合法行失败: %v", err)
	}

	failingRunner := failingProjectionRunner{
		run: func(inner func(ctx context.Context, tx ProjectionTxOps) error) error {
			return pools.RunProjectionTx(ctx, func(ctx context.Context, tx *store.ProjectionTx) error {
				return inner(ctx, failingMarkOps{ProjectionTxOps: tx})
			})
		},
	}
	failingConsumer, err := NewAvailProjectionConsumer(AvailProjectionConsumerOptions{
		runner:  failingRunner,
		acquire: func(context.Context) (func(context.Context) error, bool, error) { return nil, true, nil },
	})
	if err != nil {
		t.Fatalf("构造注入失败的消费侧失败: %v", err)
	}
	if _, err := failingConsumer.RunOnce(ctx); err == nil {
		t.Fatal("注入的失败应被上抛（真库下整批回滚）")
	}

	var published int
	if err := writer.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL`).Scan(&published); err != nil {
		t.Fatalf("统计已发布失败: %v", err)
	}
	if published != 0 {
		t.Fatalf("回滚后不得有行被标记已发布，实际 %d", published)
	}
	var applied int
	if err := writer.QueryRow(ctx, `SELECT count(*) FROM proj_applied_requests`).Scan(&applied); err != nil {
		t.Fatalf("统计幂等行失败: %v", err)
	}
	if applied != 0 {
		t.Fatalf("回滚后不得留下幂等行，实际 %d", applied)
	}

	// 换正常消费者重跑：被回滚的事件仍应被处理 → 认领确已释放。
	consumer, err := NewAvailProjectionConsumer(AvailProjectionConsumerOptions{Pools: pools})
	if err != nil {
		t.Fatalf("构造消费侧失败: %v", err)
	}
	result, err := consumer.RunOnce(ctx)
	if err != nil {
		t.Fatalf("第二次 RunOnce 失败: %v", err)
	}
	if result.Applied != 1 {
		t.Fatalf("坏行移除后应能处理剩下那条，实际应用 %d", result.Applied)
	}
}

// TestIntegrationAvailProjectionCurrentMatchesIndependentAggregation 是 avail_current 的
// **独立对账**：不看 golden，改用 Go 算术从 avail_bucket_1m 反推窗口内的期望值，与
// reconsider 后的 avail_current 逐条比对。
//
// 为什么要这一条：golden 证明「与 Node 一致」，但那只说明两边跑同一套 SQL。这一条换一条
// 独立路径（Go 侧算成功率与三档状态），能抓住「SQL 一起写错」这类同源错误。
func TestIntegrationAvailProjectionCurrentMatchesIndependentAggregation(t *testing.T) {
	pools := availProjectionPools(t)
	ctx := context.Background()

	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	var fixture projectionFixture
	if err := json.Unmarshal(readTestdata(t, "availproj_consume_fixture.json"), &fixture); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	ensureProjectionTables(t, ctx, writer)
	if _, err := writer.Exec(ctx, `TRUNCATE `+availProjectionTables+` RESTART IDENTITY`); err != nil {
		t.Fatalf("清表失败: %v", err)
	}
	base := truncateMinuteUTC(time.Now())
	seedProjectionFixture(t, ctx, writer, fixture, base)

	consumer, err := NewAvailProjectionConsumer(AvailProjectionConsumerOptions{Pools: pools})
	if err != nil {
		t.Fatalf("构造消费侧失败: %v", err)
	}
	if _, err := consumer.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}

	// Node 只重算**本批被触及**的 provider（projection-worker.ts:271 的 touchedProviders）：
	// 未被触及的行连"置 unknown"都不会做。故断言必须按此口径分两类。
	touched := map[int64]bool{}
	for _, event := range fixture.Events {
		if event.Payload == nil {
			continue
		}
		if providerID, ok := event.Payload["provider_id"].(float64); ok {
			touched[int64(providerID)] = true
		}
	}
	if len(touched) == 0 {
		t.Fatal("夹具里没有被触及的 provider，对账无效")
	}

	type aggregate struct {
		success int
		failure int
	}
	byProvider := map[int64]aggregate{}
	rows, err := writer.Query(ctx,
		`SELECT provider_id, success_cnt, failure_cnt FROM avail_bucket_1m
		 WHERE bucket_start >= now() - ($1 * INTERVAL '1 minute')`,
		availCurrentWindowMinutes)
	if err != nil {
		t.Fatalf("读窗口内桶失败: %v", err)
	}
	// defer 而非循环后 Close：断言失败会走 runtime.Goexit 跳过后续语句，
	// 未关闭的游标会占住连接，让 pools.Close() 在清理阶段**挂死**（而不是干脆地失败）。
	defer rows.Close()
	for rows.Next() {
		var providerID int64
		var success, failure int
		if err := rows.Scan(&providerID, &success, &failure); err != nil {
			rows.Close()
			t.Fatalf("扫描桶失败: %v", err)
		}
		total := byProvider[providerID]
		total.success += success
		total.failure += failure
		byProvider[providerID] = total
	}
	rows.Close()

	currentRows, err := writer.Query(ctx, `SELECT provider_id, state, availability, request_count FROM avail_current`)
	if err != nil {
		t.Fatalf("读 avail_current 失败: %v", err)
	}
	defer currentRows.Close()
	checked := 0
	for currentRows.Next() {
		var providerID int64
		var state string
		var availability float64
		var requestCount int
		if err := currentRows.Scan(&providerID, &state, &availability, &requestCount); err != nil {
			currentRows.Close()
			t.Fatalf("扫描 avail_current 失败: %v", err)
		}
		if !touched[providerID] {
			// 未被触及：此行应保持夹具预置值不变（本项目里是 provider 106）。
			unchanged := false
			for _, existing := range fixture.ExistingCurrent {
				if existing.ProviderID == providerID {
					unchanged = existing.State == state &&
						existing.Availability == availability &&
						existing.RequestCount == requestCount
				}
			}
			if !unchanged {
				t.Fatalf("未被触及的 provider %d 不应被改动，实际 %s/%v/%d",
					providerID, state, availability, requestCount)
			}
			checked++
			continue
		}
		total, hasTraffic := byProvider[providerID]
		if !hasTraffic || total.success+total.failure == 0 {
			if state != "unknown" || availability != 0 || requestCount != 0 {
				t.Fatalf("窗口内无样本的 provider %d 应为 unknown/0/0，实际 %s/%v/%d",
					providerID, state, availability, requestCount)
			}
			checked++
			continue
		}
		wantCount := total.success + total.failure
		wantAvailability := float64(total.success) / float64(wantCount)
		wantState := "red"
		switch {
		case wantAvailability >= 0.8:
			wantState = "green"
		case wantAvailability >= 0.5:
			wantState = "yellow"
		}
		if requestCount != wantCount {
			t.Fatalf("provider %d 的 request_count 应为 %d，实际 %d", providerID, wantCount, requestCount)
		}
		if diff := availability - wantAvailability; diff > 1e-12 || diff < -1e-12 {
			t.Fatalf("provider %d 的 availability 应为 %v，实际 %v", providerID, wantAvailability, availability)
		}
		if state != wantState {
			t.Fatalf("provider %d 的状态应为 %s，实际 %s", providerID, wantState, state)
		}
		checked++
	}
	currentRows.Close()
	if checked == 0 {
		t.Fatal("没有比对到任何 avail_current 行，对账无效")
	}
	t.Logf("独立对账通过：比对 %d 行 avail_current", checked)
}
