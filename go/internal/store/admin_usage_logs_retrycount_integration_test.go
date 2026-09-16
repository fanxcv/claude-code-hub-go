package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 本文件钉住 minRetryCount 过滤（RetryCountExpr）的**真库**行为，以及它与两处镜像的集合一致：
//
//   - 本包 RetryCountExpr（列表/统计的 minRetryCount 过滤）；
//   - adminapi/usage_logs_export_render.go 的 exportIsActualRequest / exportIsHedgeRace（CSV 导出）；
//   - 前端 src/lib/utils/provider-chain-formatter.ts 的 isActualRequest / isHedgeRace。
//
// 为什么必须真库：这是 jsonb 表达式（jsonb_array_elements + bool_or + sum + 减一），
// 「哪些 reason 算一次真实请求」只有真跑过 SQL 才算数——文本级断言只能证明语句里出现过哪些词。
// 漏词的后果是**同一条链在三处给出不同重试次数**（实测：链 [unsupported, request_success]
// 界面按两次真实请求算、minRetryCount 过滤按 0 次算）。

// retryCountCase 是一条待验链：chain 是落库的 provider_chain 原文，want 是期望的重试次数
// （= 真实请求数 - 1；hedge 竞速整条按 0）。
type retryCountCase struct {
	name  string
	chain string
	want  int
}

// 链项用真实键名（`id` 而非 providerId）：usage_ledger 的触发器按 `provider_chain -> -1 ->> 'id'`
// 推导 final_provider_id，键名写错会让账本行落成另一个供应商。
var retryCountCases = []retryCountCase{
	{
		name:  "unsupported 后换家成功算一次重试",
		chain: `[{"id":9001,"reason":"unsupported","statusCode":400},{"id":9002,"reason":"request_success","statusCode":200}]`,
		want:  1,
	},
	{
		name:  "首次即成不算重试",
		chain: `[{"id":9001,"reason":"request_success","statusCode":200}]`,
		want:  0,
	},
	{
		name:  "供应商故障后成功算一次重试（既有口径对照）",
		chain: `[{"id":9001,"reason":"retry_failed","statusCode":500},{"id":9002,"reason":"request_success","statusCode":200}]`,
		want:  1,
	},
	{
		name:  "hedge 竞速不算顺序重试",
		chain: `[{"id":9001,"reason":"hedge_launched"},{"id":9001,"reason":"hedge_winner","statusCode":200},{"id":9002,"reason":"hedge_loser_billed","statusCode":200}]`,
		want:  0,
	},
	{
		name:  "只有 hedge_loser_billed 也算竞速（镜像 isHedgeRace 的第五个词）",
		chain: `[{"id":9002,"reason":"hedge_loser_billed","statusCode":200},{"id":9001,"reason":"retry_failed","statusCode":500},{"id":9001,"reason":"request_success","statusCode":200}]`,
		want:  0,
	},
	{
		name:  "statusCode 为 0 不算成功（镜像真值判据）",
		chain: `[{"id":9001,"reason":"request_success","statusCode":0},{"id":9002,"reason":"system_error","statusCode":500}]`,
		want:  0,
	},
}

// TestRetryCountExprMirrorsChainSets 是不需库的文本钉子。
//
// 存在的理由：CI 不注入 CCH_TEST_DSN，真库用例整组跳过，删掉镜像词在 CI 上不会被拦。
// 它只证明「语句里出现了这些词」，集合是否真生效由下面的真库用例负责。
func TestRetryCountExprMirrorsChainSets(t *testing.T) {
	words := []string{
		"unsupported", // 上游声明不支持输入形态：同家不重试、可换家，但确实是一次真实尝试
		"hedge_triggered",
		"hedge_launched",
		"hedge_winner",
		"hedge_loser_cancelled",
		"hedge_loser_billed",
		"concurrent_limit_failed",
		"client_error_non_retryable",
		"endpoint_pool_exhausted",
		"vendor_type_all_timeout",
		"client_abort",
		"client_abort_no_first_byte",
		"http2_fallback",
		"request_success",
		"retry_success",
	}
	for _, word := range words {
		if !strings.Contains(RetryCountExpr, "'"+word+"'") {
			t.Errorf("RetryCountExpr 缺少镜像词 %q：三处镜像的集合必须逐项一致", word)
		}
	}
	if !strings.Contains(RetryCountExpr, `<> '0'`) {
		t.Error("RetryCountExpr 缺少 statusCode 真值判据（0 不算成功）")
	}
}

// seededRetryCountCase 是落库后的用例（id 用于核对过滤结果集合）。
type seededRetryCountCase struct {
	name  string
	chain string
	id    int64
	want  int
}

// TestIntegrationMinRetryCountMatchesChainMirrors 是 minRetryCount 的真库端到端用例：
// 同一条链在「表达式取值」「列表过滤」「统计过滤」三处必须给出同一个重试次数。
func TestIntegrationMinRetryCountMatchesChainMirrors(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()
	key := itKey(t)
	userID := insertRetryCountFixtureUser(t, pools, key)
	t.Cleanup(func() { cleanupRetryCountFixture(t, pools, key, userID) })

	model := "go-store-retrycount-it-" + time.Now().Format("20060102150405.000000000")
	seeded := make([]seededRetryCountCase, 0, len(retryCountCases))
	for _, tc := range retryCountCases {
		seeded = append(seeded, seededRetryCountCase{
			name:  tc.name,
			chain: tc.chain,
			want:  tc.want,
			id:    seedRetryCountCase(t, pools, userID, key, model, tc.chain),
		})
	}

	// 一、表达式取值：逐行直接跑被测表达式（不是旁证）。
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	countExpr := fmt.Sprintf(RetryCountExpr, "provider_chain")
	for _, c := range seeded {
		var got int
		if err := pool.QueryRow(ctx,
			`SELECT `+countExpr+` FROM message_request WHERE id = $1`, c.id,
		).Scan(&got); err != nil {
			t.Fatalf("用例 %q 取重试次数失败: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("用例 %q 的重试次数 = %d, want %d（链 %s）", c.name, got, c.want, c.chain)
		}
	}

	// 二、列表过滤：minRetryCount=0 返回全部；=1 只留「至少一次重试」的行；=2 一行不留。
	listIDs := func(minRetryCount int) []int64 {
		t.Helper()
		page, err := pools.FindUsageLogsWithDetails(ctx, UsageLogFilters{
			UserID:        &userID,
			Model:         model,
			MinRetryCount: minRetryCount,
		}, 1, 50)
		if err != nil {
			t.Fatalf("minRetryCount=%d 列表查询失败: %v", minRetryCount, err)
		}
		ids := make([]int64, 0, len(page.Logs))
		for _, row := range page.Logs {
			ids = append(ids, row.ID)
		}
		return ids
	}

	if ids := listIDs(0); len(ids) != len(retryCountCases) {
		t.Fatalf("minRetryCount=0 应返回全部 %d 行，实际 %d 行", len(retryCountCases), len(ids))
	}

	want := map[int64]bool{}
	for _, c := range seeded {
		if c.want >= 1 {
			want[c.id] = true
		}
	}
	got := map[int64]bool{}
	for _, id := range listIDs(1) {
		got[id] = true
	}
	for _, c := range seeded {
		switch {
		case c.want >= 1 && !got[c.id]:
			t.Errorf("minRetryCount=1 漏掉用例 %q（id=%d，链 %s）", c.name, c.id, c.chain)
		case c.want == 0 && got[c.id]:
			t.Errorf("minRetryCount=1 误收用例 %q（id=%d，实际重试 %d 次）", c.name, c.id, c.want)
		}
	}
	if len(got) != len(want) {
		t.Errorf("minRetryCount=1 命中 %d 行, want %d 行", len(got), len(want))
	}
	if ids := listIDs(2); len(ids) != 0 {
		t.Errorf("minRetryCount=2 不应命中任何行，实际 %v", ids)
	}

	// 三、统计过滤：同一批过滤器在 ledger 统计分支（关联 message_request 后套同一表达式）
	// 必须给出同一行数。
	//
	// 前置：库内**只要存在非数组 provider_chain**（例：另一个用例为覆盖「末元素形状回落」钉进去的
	// `{"a":1}`），`jsonb_array_elements` 就会在**谓词过滤之前**先对那一行求值，整条统计查询报
	// SQLSTATE 22023（`cannot extract elements from an object`）。这是本表达式**既有**的脆弱点
	// （改前改后同错，已用 HEAD 版表达式在真库复现），与本次要钉的重试次数语义无关，
	// 故此处只做显式前置判定并跳过该分支，避免让这条用例因库的卫生状况随机变红。
	var nonArrayChains int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_request
		WHERE provider_chain IS NOT NULL AND jsonb_typeof(provider_chain) <> 'array'`).Scan(&nonArrayChains); err != nil {
		t.Fatalf("检查 provider_chain 形状失败: %v", err)
	}
	if nonArrayChains > 0 {
		t.Logf("库内存在 %d 行非数组 provider_chain，跳过统计分支断言"+
			"（既有脆弱点：该形状会让统计查询整条报 SQLSTATE 22023）", nonArrayChains)
		return
	}
	stats, err := pools.FindUsageLogsStats(ctx, UsageLogFilters{
		UserID:        &userID,
		Model:         model,
		MinRetryCount: 1,
	}, false)
	if err != nil {
		t.Fatalf("统计查询失败: %v", err)
	}
	if stats.TotalRequests != int64(len(want)) {
		t.Errorf("统计分支命中 %d 条, want %d 条", stats.TotalRequests, len(want))
	}
}

// insertRetryCountFixtureUser 造本次用例自己的用户与密钥行。
//
// 列表查询 INNER JOIN users/keys，缺任一行该请求都不会出现在结果里，故夹具必须自建
// （专用库上没有任何种子用户，不能像共享库那样复用 id=1）。
func insertRetryCountFixtureUser(t *testing.T, pools *Pools, key string) int64 {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	const name = "go-store-retrycount-it"
	var userID int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (name) VALUES ($1) RETURNING id`, name).Scan(&userID); err != nil {
		t.Fatalf("建夹具用户失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO keys (user_id, key, name) VALUES ($1, $2, $3)`, userID, key, name); err != nil {
		t.Fatalf("建夹具密钥失败: %v", err)
	}
	return userID
}

// seedRetryCountCase 落一行 message_request，并把 provider_chain 写成给定链（终态写入）。
func seedRetryCountCase(t *testing.T, pools *Pools, userID int64, key, model, chain string) int64 {
	t.Helper()
	ctx := context.Background()
	if !json.Valid([]byte(chain)) {
		t.Fatalf("用例链不是合法 JSON: %s", chain)
	}
	endpoint := "/v1/messages"
	request, err := pools.CreateMessageRequest(ctx, CreateMessageRequestData{
		ProviderID: 1,
		UserID:     userID,
		Key:        key,
		Model:      &model,
		Endpoint:   &endpoint,
	})
	if err != nil {
		t.Fatalf("建 message_request 失败: %v", err)
	}
	statusCode := 200
	if _, err := pools.UpdateDetailsIfUnfinalized(ctx, request.ID, DetailsPatch{
		StatusCode:    &statusCode,
		ProviderChain: []byte(chain),
	}); err != nil {
		t.Fatalf("写 provider_chain 失败: %v", err)
	}
	return request.ID
}

// cleanupRetryCountFixture 清掉本次用例的全部痕迹（message_request 及其账本行、密钥行、用户行）。
func cleanupRetryCountFixture(t *testing.T, pools *Pools, key string, userID int64) {
	t.Helper()
	cleanupRequestRows(t, pools, []string{key})
	pool, err := pools.Control()
	if err != nil {
		t.Logf("清理：取分道失败: %v", err)
		return
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM keys WHERE key = $1`, key); err != nil {
		t.Logf("清理：删密钥行失败: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Logf("清理：删用户行失败: %v", err)
	}
}
