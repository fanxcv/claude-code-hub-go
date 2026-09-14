package terminal

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// integrationPools 读门控变量建池；未设置 CCH_TEST_DSN 时整组集成测试跳过。
func integrationPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-terminal-it",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// itKey 生成本次集成测试的唯一 key 前缀，用于事后精确清理，不污染既有数据。
func itKey(t *testing.T) string {
	t.Helper()
	return "go-terminal-it-" + time.Now().Format("20060102150405.000000000")
}

// cleanupRequest 删掉本次测试写入的 message_request 行及其账本行。
// 顺序反了就删不掉：账本行由触发器跟随 message_request 维护。
func cleanupRequest(t *testing.T, pools *store.Pools, id int64, key string) {
	t.Helper()
	t.Cleanup(func() {
		pool, err := pools.Control()
		if err != nil {
			return
		}
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, id); err != nil {
			t.Logf("清理 outbox_events 失败（不影响判定）: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM usage_ledger WHERE request_id = $1`, id); err != nil {
			t.Logf("清理 usage_ledger 失败: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM message_request WHERE key = $1`, key); err != nil {
			t.Logf("清理 message_request 失败: %v", err)
		}
	})
}

// 端到端：建开行 → 终态结算 → 读回，并核对触发器的两个产物（账本行与 outbox 事件）。
// 这是列集正确性的实测依据：账本行不缺字段、outbox 事件带得出 duration_ms。
func TestIntegrationCreateSettleAndReadBack(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()
	key := itKey(t)
	model := "gpt-5.6"
	userAgent := "terminal-it"
	clientIP := "127.0.0.1"
	endpoint := "/v1/responses"
	originalModel := model

	row, err := pools.CreateMessageRequest(ctx, store.CreateMessageRequestData{
		ProviderID:    1,
		UserID:        1,
		Key:           key,
		Model:         &model,
		OriginalModel: &originalModel,
		UserAgent:     &userAgent,
		ClientIP:      &clientIP,
		Endpoint:      &endpoint,
		MessagesCount: intPtr(1),
	})
	if err != nil {
		t.Fatalf("建行失败: %v", err)
	}
	cleanupRequest(t, pools, row.ID, key)

	cost, err := ComputeCost(CostInput{
		Usage:              Usage{InputTokens: int64Ptr(1), OutputTokens: int64Ptr(1)},
		PriceData:          []byte(`{"input_cost_per_token":0.000004,"output_cost_per_token":0.00002}`),
		ProviderMultiplier: floatPtr(1),
		GroupMultiplier:    floatPtr(1),
	})
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}

	settler := New(StoreWriter{Pools: pools}, Options{MaxAttempts: 3, Backoff: func(int) time.Duration { return 0 }})
	result, err := settler.Settle(ctx, row.ID, Settlement{
		StatusCode:    200,
		DurationMS:    intPtr(62),
		TTFTMS:        intPtr(43),
		FirstByteMS:   intPtr(43),
		Usage:         Usage{InputTokens: int64Ptr(1), OutputTokens: int64Ptr(1)},
		Cost:          &cost,
		ProviderChain: []byte(`[{"id":1,"name":"mock-upstream","reason":"request_success","statusCode":200}]`),
		Model:         &model,
	})
	if err != nil {
		t.Fatalf("终态结算失败: %v", err)
	}
	if !result.Committed {
		t.Fatal("首次终态写应当赢得该行")
	}
	if !result.CostWritten {
		t.Fatal("成本应当已入库")
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}

	// 读回终态列：status_code/duration_ms/ttfb_ms/provider_chain 必须都落到位。
	var statusCode, durationMS, ttfbMS, firstByteMS *int
	var costUSD *string
	if err := pool.QueryRow(ctx,
		`SELECT status_code, duration_ms, ttfb_ms, first_byte_ms, cost_usd::text
		 FROM message_request WHERE id = $1`, row.ID,
	).Scan(&statusCode, &durationMS, &ttfbMS, &firstByteMS, &costUSD); err != nil {
		t.Fatalf("读回终态行失败: %v", err)
	}
	if statusCode == nil || *statusCode != 200 {
		t.Fatalf("status_code = %v, want 200", statusCode)
	}
	if durationMS == nil || *durationMS != 62 {
		t.Fatalf("duration_ms = %v, want 62（TTFTMS 必须写进历史列 ttfb_ms）", durationMS)
	}
	if ttfbMS == nil || *ttfbMS != 43 {
		t.Fatalf("ttfb_ms = %v, want 43", ttfbMS)
	}
	if firstByteMS == nil || *firstByteMS != 43 {
		t.Fatalf("first_byte_ms = %v, want 43", firstByteMS)
	}
	if costUSD == nil || !strings.HasPrefix(*costUSD, "0.000024") {
		t.Fatalf("cost_usd = %v, want 0.000024", costUSD)
	}

	// 账本行由触发器产生，且关键列不得为空——这就是「列集必须完整」的实测判据。
	var (
		isSuccess        *bool
		finalProviderID  *int
		outcome          *string
		ledgerDuration   *int
		ledgerTTFB       *int
		ledgerFirstByte  *int
		ledgerInputToken *int64
		ledgerCost       *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT is_success, final_provider_id, success_rate_outcome, duration_ms, ttfb_ms,
		        first_byte_ms, input_tokens, cost_usd::text
		 FROM usage_ledger WHERE request_id = $1`, row.ID,
	).Scan(&isSuccess, &finalProviderID, &outcome, &ledgerDuration, &ledgerTTFB,
		&ledgerFirstByte, &ledgerInputToken, &ledgerCost); err != nil {
		t.Fatalf("账本行不存在或读取失败（列集不全时账本会缺字段）: %v", err)
	}
	if isSuccess == nil || !*isSuccess {
		t.Fatalf("is_success = %v, want true", isSuccess)
	}
	if finalProviderID == nil || *finalProviderID != 1 {
		t.Fatalf("final_provider_id = %v, want 1（取自 provider_chain 末段）", finalProviderID)
	}
	if outcome == nil || *outcome == "" {
		t.Fatal("success_rate_outcome 不应为空")
	}
	for name, value := range map[string]any{
		"duration_ms":   ledgerDuration,
		"ttfb_ms":       ledgerTTFB,
		"first_byte_ms": ledgerFirstByte,
		"input_tokens":  ledgerInputToken,
		"cost_usd":      ledgerCost,
	} {
		if value == nil {
			t.Fatalf("账本列 %s 为空——终态写缺了该监视列", name)
		}
	}

	// 重复终态写必须落败且不覆盖：这是过渡期防双写的最后防线。
	second, err := settler.Settle(ctx, row.ID, Settlement{
		StatusCode:    502,
		DurationMS:    intPtr(999),
		ProviderChain: []byte(`[{"id":1,"reason":"retry_failed"}]`),
		ErrorMessage:  strPtr("late overwrite attempt"),
	})
	if !errors.Is(err, ErrNotSettled) {
		t.Fatalf("重复终态写应返回 ErrNotSettled，得到 %v", err)
	}
	if second.Committed {
		t.Fatal("重复终态写不得计为赢得终态")
	}
	var afterStatus *int
	var afterError *string
	if err := pool.QueryRow(ctx,
		`SELECT status_code, error_message FROM message_request WHERE id = $1`, row.ID,
	).Scan(&afterStatus, &afterError); err != nil {
		t.Fatalf("复核终态行失败: %v", err)
	}
	if afterStatus == nil || *afterStatus != 200 {
		t.Fatalf("status_code 被覆盖为 %v", afterStatus)
	}
	if afterError != nil {
		t.Fatalf("error_message 被迟到写入污染: %v", *afterError)
	}
}

// 不变量 I3 的端到端证据：outbox 事件只在 status_code 首次非空的那条语句产生，
// 因此它的载荷必须已经带上 duration_ms。
func TestIntegrationOutboxEventCarriesDuration(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()
	key := itKey(t)
	model := "gpt-5.6"

	row, err := pools.CreateMessageRequest(ctx, store.CreateMessageRequestData{
		ProviderID: 1,
		UserID:     1,
		Key:        key,
		Model:      &model,
	})
	if err != nil {
		t.Fatalf("建行失败: %v", err)
	}
	cleanupRequest(t, pools, row.ID, key)

	settler := New(StoreWriter{Pools: pools}, Options{Backoff: func(int) time.Duration { return 0 }})
	if _, err := settler.Settle(ctx, row.ID, Settlement{
		StatusCode:    200,
		DurationMS:    intPtr(77),
		TTFTMS:        intPtr(11),
		ProviderChain: []byte(`[{"id":1,"reason":"request_success","statusCode":200}]`),
	}); err != nil {
		t.Fatalf("终态结算失败: %v", err)
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var payload []byte
	var eventCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*), MIN(payload::text) FROM outbox_events
		 WHERE aggregate_id = $1 AND event_type = 'request_finalized'`, row.ID,
	).Scan(&eventCount, &payload); err != nil {
		t.Fatalf("读取 outbox 事件失败: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("request_finalized 事件数 = %d, want 1（status_code 只应首次非空时产生一次）", eventCount)
	}
	// 事件载荷里 duration_ms 必须是本次结算写入的值，而不是 NULL。
	if !strings.Contains(string(payload), `"duration_ms": 77`) &&
		!strings.Contains(string(payload), `"duration_ms":77`) {
		t.Fatalf("outbox 事件载荷缺 duration_ms=77: %s", payload)
	}
}

// Go 侧的终态判据镜像必须与库内函数一致；用例与库内分支一一对应。
func TestIsFinalizedMatchesDatabaseFunction(t *testing.T) {
	pools := integrationPools(t)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	ctx := context.Background()

	emptyText := ""
	blocked := "sensitive_word"
	statusOK := 200
	errorMessage := "upstream aborted"
	chainFinal := []byte(`[{"reason":"hedge_loser_billed"}]`)
	chainOpen := []byte(`[{"reason":"initial_selection"}]`)
	chainWithStatus := []byte(`[{"reason":"initial_selection","statusCode":503}]`)
	chainStringStatus := []byte(`[{"reason":"initial_selection","statusCode":"503"}]`)
	chainOnlyFirstFinal := []byte(`[{"reason":"retry_success"},{"reason":"initial_selection"}]`)
	emptyChain := []byte(`[]`)

	cases := []struct {
		name  string
		facts FinalizationFacts
		chain any
	}{
		{name: "全空", facts: FinalizationFacts{}, chain: nil},
		{name: "blocked_by", facts: FinalizationFacts{BlockedBy: &blocked}, chain: nil},
		{name: "status_code", facts: FinalizationFacts{StatusCode: &statusOK}, chain: nil},
		{name: "error_message", facts: FinalizationFacts{ErrorMessage: &errorMessage}, chain: nil},
		{name: "error_message 空串", facts: FinalizationFacts{ErrorMessage: &emptyText}, chain: nil},
		{name: "链路末段白名单", facts: FinalizationFacts{ProviderChain: chainFinal}, chain: chainFinal},
		{name: "链路末段非白名单", facts: FinalizationFacts{ProviderChain: chainOpen}, chain: chainOpen},
		{name: "链路末段数值状态码", facts: FinalizationFacts{ProviderChain: chainWithStatus}, chain: chainWithStatus},
		{name: "链路末段字符串状态码", facts: FinalizationFacts{ProviderChain: chainStringStatus}, chain: chainStringStatus},
		{name: "只看末段", facts: FinalizationFacts{ProviderChain: chainOnlyFirstFinal}, chain: chainOnlyFirstFinal},
		{name: "空数组", facts: FinalizationFacts{ProviderChain: emptyChain}, chain: emptyChain},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var databaseResult bool
			if err := pool.QueryRow(ctx,
				`SELECT fn_is_message_request_finalized($1::varchar, $2::integer, $3::jsonb, $4::text)`,
				testCase.facts.BlockedBy, testCase.facts.StatusCode, testCase.chain, testCase.facts.ErrorMessage,
			).Scan(&databaseResult); err != nil {
				t.Fatalf("调用库内函数失败: %v", err)
			}
			goResult := IsFinalized(testCase.facts)
			if goResult != databaseResult {
				t.Fatalf("Go 判定 %v，库内函数判定 %v——镜像已漂移", goResult, databaseResult)
			}
		})
	}
}

// 监视集漂移：库内触发器定义是权威源，Go 侧清单必须逐列一致。
func TestTriggerMonitorColumnsMatchDatabase(t *testing.T) {
	pools := integrationPools(t)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	ctx := context.Background()

	rows, err := pool.Query(ctx,
		`SELECT tgname, pg_get_triggerdef(oid) FROM pg_trigger
		 WHERE tgrelid = 'message_request'::regclass AND NOT tgisinternal`)
	if err != nil {
		t.Fatalf("读取触发器定义失败: %v", err)
	}
	defer rows.Close()

	definitions := map[string]string{}
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			t.Fatalf("扫描触发器定义失败: %v", err)
		}
		definitions[name] = definition
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历触发器定义失败: %v", err)
	}

	assertSameColumns(t, "trg_upsert_usage_ledger", definitions, ledgerMonitoredColumns)
	assertSameColumns(t, "message_request_outbox_aiud", definitions, outboxMonitoredColumns)
}

// assertSameColumns 从触发器定义里解析 `UPDATE OF <列...>` 列表并与 Go 侧清单对集合。
func assertSameColumns(t *testing.T, trigger string, definitions map[string]string, expected []string) {
	t.Helper()
	definition, ok := definitions[trigger]
	if !ok {
		t.Fatalf("库内没有触发器 %s", trigger)
	}
	marker := "UPDATE OF "
	index := strings.Index(definition, marker)
	if index < 0 {
		t.Fatalf("触发器 %s 的定义里没有 UPDATE OF 列表: %s", trigger, definition)
	}
	list := definition[index+len(marker):]
	if end := strings.Index(list, " ON "); end >= 0 {
		list = list[:end]
	}
	actual := map[string]bool{}
	for _, column := range strings.Split(list, ",") {
		actual[strings.TrimSpace(column)] = true
	}
	if len(actual) != len(expected) {
		t.Fatalf("%s 监视列数 = %d，Go 侧清单 = %d；库内列表: %v",
			trigger, len(actual), len(expected), list)
	}
	for _, column := range expected {
		if !actual[column] {
			t.Fatalf("%s 未监视 Go 侧清单里的 %s", trigger, column)
		}
	}
}

// 拦截类终态的端到端：走 store 的真实插入面建行，再写终态；顺带覆盖 StoreWriter 的
// 建行与取价适配。敏感词拦截在 TS 侧是「建行即终态」，Go 侧是「先开行、再终态」两步，
// 本测试断言两步之后的可见结果与 TS 语义一致：不计费、不进账本的计费集。
func TestIntegrationSettleBlocked(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()
	key := itKey(t)
	model := "gpt-5.6"

	settler := New(StoreWriter{Pools: pools}, Options{Backoff: func(int) time.Duration { return 0 }})
	result, err := settler.SettleBlocked(ctx, store.CreateMessageRequestData{
		ProviderID: 0, // 与 TS 一致：被拦截的请求没有供应商
		UserID:     1,
		Key:        key,
		Model:      &model,
		CostUSD:    strPtr("0"),
	}, Settlement{
		StatusCode:    400,
		BlockedBy:     strPtr("sensitive_word"),
		BlockedReason: strPtr(`{"word":"x","matchType":"exact"}`),
		ErrorMessage:  strPtr(`请求包含敏感词："x"`),
	})
	if err != nil {
		t.Fatalf("拦截类结算失败: %v", err)
	}
	if !result.Committed {
		t.Fatal("拦截类结算应当赢得终态")
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var id int64
	var statusCode *int
	var blockedBy, blockedReason, errorMessage *string
	if err := pool.QueryRow(ctx,
		`SELECT id, status_code, blocked_by, blocked_reason, error_message
		 FROM message_request WHERE key = $1`, key,
	).Scan(&id, &statusCode, &blockedBy, &blockedReason, &errorMessage); err != nil {
		t.Fatalf("读回拦截行失败: %v", err)
	}
	cleanupRequest(t, pools, id, key)

	if statusCode == nil || *statusCode != 400 {
		t.Fatalf("status_code = %v, want 400", statusCode)
	}
	if blockedBy == nil || *blockedBy != "sensitive_word" {
		t.Fatalf("blocked_by = %v, want sensitive_word", blockedBy)
	}
	if blockedReason == nil || *blockedReason == "" {
		t.Fatal("blocked_reason 应落库")
	}
	if errorMessage == nil || *errorMessage == "" {
		t.Fatal("error_message 应落库")
	}

	// 账本行必须反映「不计费」：blocked_by 非空即被 ledger.go 的计费条件排除。
	var ledgerBlockedBy *string
	var isSuccess *bool
	var costUSD *string
	if err := pool.QueryRow(ctx,
		`SELECT blocked_by, is_success, cost_usd::text FROM usage_ledger WHERE request_id = $1`, id,
	).Scan(&ledgerBlockedBy, &isSuccess, &costUSD); err != nil {
		t.Fatalf("账本行读取失败: %v", err)
	}
	if ledgerBlockedBy == nil || *ledgerBlockedBy != "sensitive_word" {
		t.Fatalf("账本 blocked_by = %v, want sensitive_word", ledgerBlockedBy)
	}
	if isSuccess == nil || *isSuccess {
		t.Fatalf("被拦截的请求 is_success = %v, want false", isSuccess)
	}
	var billable int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM usage_ledger WHERE request_id = $1 AND `+store.BillingCondition, id,
	).Scan(&billable); err != nil {
		t.Fatalf("计费集判定失败: %v", err)
	}
	if billable != 0 {
		t.Fatal("被拦截的请求不得进入计费集")
	}
}

// StoreWriter 必须把「价格表没有该模型」翻译成 ErrPriceNotFound，而不是把
// store.ErrNotFound 漏给调用方——否则计费会以「查询失败」的名义报错，而不是按
// TS 语义静默跳过。
func TestIntegrationStoreWriterTranslatesMissingPrice(t *testing.T) {
	pools := integrationPools(t)
	// 跨包互斥：下面「取一个 model_name、再按名读回」是两段式，中间会被并发的整表切换删行。
	lockModelPricesTable(t, pools)
	writer := StoreWriter{Pools: pools}

	if _, err := writer.FindModelPrice(context.Background(), "definitely-not-a-model-"+itKey(t)); !errors.Is(err, ErrPriceNotFound) {
		t.Fatalf("不存在的模型应返回 ErrPriceNotFound，得到 %v", err)
	}

	var modelName string
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	err = pool.QueryRow(context.Background(),
		`SELECT model_name FROM model_prices LIMIT 1`).Scan(&modelName)
	if err != nil {
		t.Skip("价格表为空，跳过命中路径")
	}
	price, err := writer.FindModelPrice(context.Background(), modelName)
	if err != nil {
		t.Fatalf("命中价格应成功: %v", err)
	}
	if price == nil || price.ModelName != modelName {
		t.Fatalf("读回的价格与请求不符: %+v", price)
	}
}
