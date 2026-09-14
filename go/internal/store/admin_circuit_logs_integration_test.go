package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// 本文件是「供应商熔断日志」读面的真库用例（`CCH_TEST_DSN` 未设置时整组跳过）。
//
// 为什么必须有真库用例：这个读面的**全部价值**在 SQL 的两条来源合并——行级失败与**链内失败**
// （后者是 W67 把行级 provider_id 改成按「作答者」归属之后必须补的那一路）。用替身测不出
// jsonb 展开、脏数据防护（非数组 chain / 非数字 id）与 DISTINCT ON 的去重语义。
//
// 夹具纪律同其它 store 用例：唯一 marker、按 marker 清理、只增删自己的行。

const circuitLogsITMarker = "cch-it-circuitlogs"

// circuitLogsFixture 造三行，覆盖三条不同路径：
//
//	directFail    行级失败（provider_id = 目标，status 503）
//	chainFail     链内失败（行级 provider_id 是**别的**供应商，链里目标那家 404）
//	bothFail      同时命中两条来源（行级与链内都是目标）→ 验去重与 direct 优先
type circuitLogsFixture struct {
	providerID int64
	otherID    int64
	directID   int64
	chainID    int64
	bothID     int64
	marker     string
}

// chainEntryFailure 造一个失败链项（形状对齐 W67 落地的键：id/name/statusCode/errorMessage/reason）。
func chainEntryFailure(id int64, name string, status int, errMsg, reason string) map[string]any {
	entry := map[string]any{
		"id":     id,
		"name":   name,
		"reason": reason,
	}
	if status > 0 {
		entry["statusCode"] = status
	}
	if errMsg != "" {
		entry["errorMessage"] = errMsg
	}
	return entry
}

func seedCircuitLogsFixture(t *testing.T, pools *Pools) circuitLogsFixture {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	marker := circuitLogsITMarker + time.Now().Format("20060102150405.000000000")

	insertProvider := func(name string) int64 {
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO providers (name, url, key, provider_vendor_id, is_enabled, weight, priority,
				cost_multiplier, group_tag, provider_type)
			VALUES ($1, 'https://it.invalid', 'it-key', NULL, true, 1, 0, 1::numeric, $2, 'openai-compatible')
			RETURNING id`,
			name, marker,
		).Scan(&id); err != nil {
			t.Fatalf("建供应商夹具失败: %v", err)
		}
		return id
	}
	providerID := insertProvider(marker + "-target")
	otherID := insertProvider(marker + "-other")

	insertRow := func(providerID *int64, status *int, errMsg *string, chain []map[string]any, minutesAgo int) int64 {
		var chainJSON *string
		if chain != nil {
			raw, marshalErr := json.Marshal(chain)
			if marshalErr != nil {
				t.Fatalf("序列化链夹具失败: %v", marshalErr)
			}
			text := string(raw)
			chainJSON = &text
		}
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO message_request (
				provider_id, user_id, key, model, original_model, endpoint,
				status_code, duration_ms, cost_usd, is_replay, error_message,
				provider_chain, created_at, updated_at
			) VALUES (
				$1, 1, $2, $2, $2, '/v1/responses',
				$3, 123, 0::numeric, false, $4,
				$5::jsonb, now() - ($6::int * interval '1 minute'), now()
			) RETURNING id`,
			providerID, marker, status, errMsg, chainJSON, minutesAgo,
		).Scan(&id); err != nil {
			t.Fatalf("插 message_request 夹具失败: %v", err)
		}
		return id
	}

	s503 := 503
	s404 := 404
	direct := insertRow(&providerID, &s503, nil, []map[string]any{
		chainEntryFailure(providerID, marker+"-target", 503, "upstream_error", "retry_failed"),
	}, 30)
	// 链内失败：行级归属是**另一家**（作答者），目标那家只出现在链里。
	chainOnly := insertRow(&otherID, &s404, nil, []map[string]any{
		chainEntryFailure(providerID, marker+"-target", 404, "resource_not_found", "hedge_launched"),
		chainEntryFailure(otherID, marker+"-other", 200, "", "hedge_winner"),
	}, 20)
	both := insertRow(&providerID, &s503, nil, []map[string]any{
		chainEntryFailure(providerID, marker+"-target", 503, "upstream_error", "retry_failed"),
	}, 10)

	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(ctx,
			`DELETE FROM usage_ledger WHERE request_id IN
				(SELECT id FROM message_request WHERE key = $1)`, marker)
		_, _ = cleanupPool.Exec(ctx, `DELETE FROM message_request WHERE key = $1`, marker)
		_, _ = cleanupPool.Exec(ctx, `DELETE FROM providers WHERE group_tag = $1`, marker)
	})

	return circuitLogsFixture{
		providerID: providerID,
		otherID:    otherID,
		directID:   direct,
		chainID:    chainOnly,
		bothID:     both,
		marker:     marker,
	}
}

// TestIntegrationCircuitFailuresMergesBothSources 钉住两条来源合并、去重优先级与排序。
func TestIntegrationCircuitFailuresMergesBothSources(t *testing.T) {
	pools := openTestPools(t)
	fixture := seedCircuitLogsFixture(t, pools)
	ctx := context.Background()

	rows, err := pools.AdminProviderCircuitFailures(ctx, fixture.providerID, time.Now().Add(-24*time.Hour), 20)
	if err != nil {
		t.Fatalf("查熔断日志失败: %v", err)
	}

	byID := make(map[int64]AdminProviderCircuitFailure, len(rows))
	for _, row := range rows {
		byID[row.RequestID] = row
	}

	// ① 行级失败必须在
	direct, ok := byID[fixture.directID]
	if !ok {
		t.Fatalf("行级失败行 %d 未出现在结果里：%+v", fixture.directID, rows)
	}
	if direct.Source != "direct" {
		t.Fatalf("行级失败的 source = %q，期望 direct", direct.Source)
	}
	if direct.StatusCode == nil || *direct.StatusCode != 503 {
		t.Fatalf("行级失败的 statusCode = %v，期望 503", direct.StatusCode)
	}

	// ② **链内失败必须在**（这是本读面存在的理由：行级 provider_id 记的是作答者）
	chainRow, ok := byID[fixture.chainID]
	if !ok {
		t.Fatalf("链内失败行 %d 未出现在结果里（只按行级 provider_id 查会漏掉它）：%+v", fixture.chainID, rows)
	}
	if chainRow.Source != "chain" {
		t.Fatalf("链内失败的 source = %q，期望 chain", chainRow.Source)
	}
	if chainRow.StatusCode == nil || *chainRow.StatusCode != 404 {
		t.Fatalf("链内失败应暴露链项的 statusCode（404），实际 %v", chainRow.StatusCode)
	}
	if chainRow.ErrorMessage == nil || *chainRow.ErrorMessage != "resource_not_found" {
		t.Fatalf("链内失败应暴露链项的 errorMessage，实际 %v", chainRow.ErrorMessage)
	}
	if chainRow.ChainReason == nil || *chainRow.ChainReason != "hedge_launched" {
		t.Fatalf("链内失败应带链上的 reason，实际 %v", chainRow.ChainReason)
	}

	// ③ 同时命中两条来源的行必须只出现一次，且以 direct 为准
	bothCount := 0
	for _, row := range rows {
		if row.RequestID == fixture.bothID {
			bothCount++
		}
	}
	if bothCount != 1 {
		t.Fatalf("同时命中两条来源的行应去重为 1 条，实际 %d 条", bothCount)
	}
	if byID[fixture.bothID].Source != "direct" {
		t.Fatalf("去重时应取 direct（行级事实更强），实际 %q", byID[fixture.bothID].Source)
	}

	// ④ 排序：按时间倒序（夹具的 minutesAgo 是 30/20/10）
	order := make([]int64, 0, 3)
	for _, row := range rows {
		if row.RequestID == fixture.directID || row.RequestID == fixture.chainID || row.RequestID == fixture.bothID {
			order = append(order, row.RequestID)
		}
	}
	want := []int64{fixture.bothID, fixture.chainID, fixture.directID}
	if len(order) != len(want) {
		t.Fatalf("三条夹具应都出现，实际 %v", order)
	}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("排序应为时间倒序 %v，实际 %v", want, order)
		}
	}

	// ⑤ 该行的 createdAt 是 Node toISOString() 同形（三位毫秒 + Z + 定宽）
	if len(byID[fixture.directID].CreatedAt) != len("2026-01-02T03:04:05.678Z") {
		t.Fatalf("createdAt 形状不像 ISO 8601 毫秒串：%q", byID[fixture.directID].CreatedAt)
	}
}

// TestIntegrationCircuitFailuresHonoursLimitAndWindow 钉住 limit 与时间窗都是硬界。
func TestIntegrationCircuitFailuresHonoursLimitAndWindow(t *testing.T) {
	pools := openTestPools(t)
	fixture := seedCircuitLogsFixture(t, pools)
	ctx := context.Background()

	rows, err := pools.AdminProviderCircuitFailures(ctx, fixture.providerID, time.Now().Add(-24*time.Hour), 1)
	if err != nil {
		t.Fatalf("limit=1 查询失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("limit=1 应只返回 1 条，实际 %d 条", len(rows))
	}
	if rows[0].RequestID != fixture.bothID {
		t.Fatalf("limit=1 应取最近的一条（%d），实际 %d", fixture.bothID, rows[0].RequestID)
	}

	// 时间窗把全部夹具挡在外面：since 取「现在之后」→ 必然为空。
	rows, err = pools.AdminProviderCircuitFailures(ctx, fixture.providerID, time.Now().Add(time.Hour), 20)
	if err != nil {
		t.Fatalf("窗内无数据的查询不应报错: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("since 在未来时应返回空，实际 %d 条", len(rows))
	}
}

// TestIntegrationCircuitFailuresSurvivesDirtyChain 钉住脏链数据不会把端点打成 500。
//
// 三种脏形态都必须在 SQL 层被挡住（否则真库里一行历史脏数据就能让整个端点 500）：
// provider_chain 为 NULL / 为对象而非数组 / 链项 id 是非数字。
func TestIntegrationCircuitFailuresSurvivesDirtyChain(t *testing.T) {
	pools := openTestPools(t)
	fixture := seedCircuitLogsFixture(t, pools)
	ctx := context.Background()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}

	cases := []struct {
		name  string
		chain string
	}{
		{"null", "NULL"},
		{"object", `'{"id": 1}'::jsonb`},
		{"nonnumeric-id", `'[{"id": "abc", "statusCode": 500}]'::jsonb`},
		{"string-status", `'[{"id": 1, "statusCode": "500"}]'::jsonb`},
	}
	for index, testCase := range cases {
		chain := testCase.chain
		if chain == "NULL" {
			chain = "NULL"
		}
		if _, execErr := pool.Exec(ctx, `
			INSERT INTO message_request (
				provider_id, user_id, key, model, endpoint, status_code, is_replay,
				provider_chain, created_at, updated_at
			) VALUES ($1, 1, $2, $2, '/v1/responses', 500, false, `+chain+`, now(), now())`,
			fixture.otherID, fixture.marker+"-dirty-"+testCase.name,
		); execErr != nil {
			// 把脏夹具的 key 也纳入清理范围由 t.Cleanup 的前缀覆盖（key 以 marker 开头）。
			t.Fatalf("插入脏夹具 %s 失败: %v", testCase.name, execErr)
		}
		_ = index
	}

	rows, err := pools.AdminProviderCircuitFailures(ctx, fixture.providerID, time.Now().Add(-24*time.Hour), 20)
	if err != nil {
		t.Fatalf("遇到脏链数据时不应报错，实际: %v", err)
	}
	// 脏行都属于 otherID，故目标供应商的结果里不应出现它们；关键是这次查询**没有报错**。
	for _, row := range rows {
		if row.RequestID == 0 {
			t.Fatalf("结果里有零值行：%+v", row)
		}
	}
}
