package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// 补齐只读与账本路径的覆盖：这些方法都是数据面实际会走的，不能只靠编译通过。
func TestIntegrationReadRemainingPaths(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()

	// 分道访问器与预算快照。
	if pools.Budget().Data <= 0 {
		t.Fatalf("预算快照异常: %+v", pools.Budget())
	}
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取 data 分道失败: %v", err)
	}
	if pool.Lane() == "" || pool.MaxOutstanding() <= 0 {
		t.Fatalf("分道元信息异常: lane=%q maxOutstanding=%d", pool.Lane(), pool.MaxOutstanding())
	}
	if pool.Raw() == nil {
		t.Fatal("Raw() 不应为 nil")
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("Ping 失败: %v", err)
	}

	// 控制面与写分道也要能取到（物理分道可能复用，但访问器必须可用）。
	if _, err := pools.Control(); err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	if _, err := pools.Writer(); err != nil {
		t.Fatalf("取 writer 分道失败: %v", err)
	}

	// 供应商按 id 读取：存在的与不存在的都要能区分。
	//
	// 共享库上「先列表、再按 id 复读」会撞并发的删除：并发的集成测试（另一进程、另一 worktree）
	// 正在回收自己的夹具，而夹具都排在优先级最低的一端，也就是列表首位。故取第一个仍在库里的。
	providers, err := pools.FindEnabledProviders(ctx)
	if err != nil {
		t.Fatalf("读取启用态供应商失败: %v", err)
	}
	for _, candidate := range providers {
		byID, err := pools.FindProviderByID(ctx, candidate.ID)
		if err == ErrNotFound {
			continue
		}
		if err != nil {
			t.Fatalf("按 id 读取供应商失败: %v", err)
		}
		if byID.ID != candidate.ID {
			t.Fatalf("按 id 读回的供应商不一致: %d != %d", byID.ID, candidate.ID)
		}
		break
	}
	if _, err := pools.FindProviderByID(ctx, 2147483647); err != ErrNotFound {
		t.Fatalf("不存在的供应商应当返回 ErrNotFound，实际 %v", err)
	}

	// 端点按供应商厂 + 类型读取：返回的每一行都必须属于该厂与该类型。
	endpoints, err := pools.FindEnabledProviderEndpointsByVendorAndType(ctx, 1, "claude")
	if err != nil {
		t.Fatalf("读取端点失败: %v", err)
	}
	for _, endpoint := range endpoints {
		if endpoint.VendorID != 1 || endpoint.ProviderType != "claude" || !endpoint.IsEnabled {
			t.Fatalf("端点查询返回了不匹配的行: %+v", endpoint)
		}
	}

	// 价格查表：不存在的模型返回 ErrNotFound。
	if _, err := pools.FindModelPrice(ctx, "go-store-it-nonexistent-model"); err != ErrNotFound {
		t.Fatalf("不存在的模型价格应当返回 ErrNotFound，实际 %v", err)
	}

	// 密钥查询命中路径（用库里已有的种子密钥）。
	keys, err := pools.FindKeyByValue(ctx, "sk-loadtest")
	if err != nil && err != ErrNotFound {
		t.Fatalf("按密钥查询失败: %v", err)
	}
	if err == nil && keys.Key != "sk-loadtest" {
		t.Fatalf("按密钥读回的值不一致: %q", keys.Key)
	}
}

func TestIntegrationLedgerRemainingPaths(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	ctx := context.Background()
	created := createRequestFixture(t, pools, key, false)
	if err := pools.UpdateCost(ctx, created.ID, "1.5"); err != nil {
		t.Fatalf("写入合法成本失败: %v", err)
	}
	cost, err := pools.FindCostUSD(ctx, created.ID)
	if err != nil {
		t.Fatalf("读回成本失败: %v", err)
	}
	if trimmed := trimCost(cost); trimmed != "1.5" {
		t.Fatalf("成本 = %q, want 1.5", cost)
	}

	start := time.Now().Add(-time.Hour)
	end := time.Now().Add(time.Hour)

	// 单窗口合计、全时段合计、带 resetAt 的合计。
	if _, err := pools.SumLedgerCostInTimeRange(ctx, LedgerEntityKey, key, start, end); err != nil {
		t.Fatalf("时间窗成本合计失败: %v", err)
	}
	if _, err := pools.SumLedgerTotalCost(ctx, LedgerEntityKey, key, nil); err != nil {
		t.Fatalf("全时段成本合计失败: %v", err)
	}
	resetAt := time.Now().Add(-30 * time.Minute)
	if _, err := pools.SumLedgerTotalCost(ctx, LedgerEntityKey, key, &resetAt); err != nil {
		t.Fatalf("带 resetAt 的成本合计失败: %v", err)
	}

	// 多窗口合计：返回顺序与入参一致，空入参返回空切片。
	values, err := pools.SumLedgerQuotaCosts(ctx, LedgerEntityKey, key, []TimeRange{
		{Start: start, End: end},
		{Start: end, End: end.Add(time.Hour)},
	})
	if err != nil {
		t.Fatalf("多窗口成本合计失败: %v", err)
	}
	if len(values) != 2 {
		t.Fatalf("多窗口返回值个数 = %d, want 2", len(values))
	}
	empty, err := pools.SumLedgerQuotaCosts(ctx, LedgerEntityKey, key, nil)
	if err != nil {
		t.Fatalf("空窗口入参不应报错: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空窗口应当返回空切片，实际 %v", empty)
	}

	// 批量合计：入参里的每个 id 都要有结果，缺省为 "0"。
	batch, err := pools.LedgerTotalCostBatch(ctx, LedgerEntityKey, []any{key, key + "-missing"}, 365)
	if err != nil {
		t.Fatalf("批量成本查询失败: %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("批量结果个数 = %d, want 2", len(batch))
	}
	if batch[key+"-missing"] != "0" {
		t.Fatalf("无数据的 id 应当为 0，实际 %q", batch[key+"-missing"])
	}
	// maxAgeDays 非正数表示不限时段。
	if _, err := pools.LedgerTotalCostBatch(ctx, LedgerEntityKey, []any{key}, 0); err != nil {
		t.Fatalf("不限时段的批量查询失败: %v", err)
	}
	// 空入参直接返回空表。
	emptyBatch, err := pools.LedgerTotalCostBatch(ctx, LedgerEntityKey, nil, 365)
	if err != nil {
		t.Fatalf("空入参批量查询失败: %v", err)
	}
	if len(emptyBatch) != 0 {
		t.Fatalf("空入参应当返回空表，实际 %v", emptyBatch)
	}
	// provider 维度不支持批量（与 TS 侧只支持 user/key 一致）。
	if _, err := pools.LedgerTotalCostBatch(ctx, LedgerEntityProvider, []any{1}, 365); err == nil {
		t.Fatal("provider 维度的批量查询必须报错")
	}

	// user 维度的时间窗计数。
	if _, err := pools.CountLedgerRequestsInTimeRange(ctx, LedgerEntityUser, 1, start, end); err != nil {
		t.Fatalf("user 维度计数失败: %v", err)
	}
	// 未知维度必须报错，而不是静默用错列。
	if _, err := pools.CountLedgerRequestsInTimeRange(ctx, LedgerEntityType("bogus"), 1, start, end); err == nil {
		t.Fatal("未知主体类型必须报错")
	}
}

func TestIntegrationProjectionWrapper(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	ctx := context.Background()
	created := createRequestFixture(t, pools, key, false)

	// 走 Pools 上的包装（内部取 control 分道）。
	fresh, err := pools.InsertProjAppliedRequest(ctx, created.ID, "3f2504e0-4f89-11d3-9a0c-0305e82c3301")
	if err != nil {
		t.Fatalf("投影写入失败: %v", err)
	}
	if !fresh {
		t.Fatal("首次投影应当判定为新应用")
	}
	// 非法 uuid 必须报错，与 TS 侧的 ::uuid 强制转换同语义。
	if _, err := pools.InsertProjAppliedRequest(ctx, created.ID+1, "not-a-uuid"); err == nil {
		t.Fatal("非法 uuid 必须报错")
	}
}

func TestIntegrationHedgeLoserOptionalTokenFields(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	ctx := context.Background()
	created := createRequestFixture(t, pools, key, false)

	input := int64(120)
	output := int64(45)
	entry := HedgeLoserEntry{
		ProviderID:    11,
		ProviderName:  "loser-provider",
		AttemptNumber: 1,
		InputTokens:   &input,
		OutputTokens:  &output,
	}
	if err := pools.AddHedgeLoserCost(ctx, created.ID, "0.125", entry); err != nil {
		t.Fatalf("累加带 token 明细的输家成本失败: %v", err)
	}

	// 明细必须原样落进 hedge_losers，供 winner 的加法表达式读取 costUsd。
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var payload string
	if err := pool.QueryRow(ctx,
		`SELECT hedge_losers::text FROM message_request WHERE id = $1`, created.ID).Scan(&payload); err != nil {
		t.Fatalf("读回 hedge_losers 失败: %v", err)
	}
	if payload == "" || payload == "null" {
		t.Fatalf("hedge_losers 未写入: %q", payload)
	}
	for _, fragment := range []string{"loser-provider", "costUsd", "inputTokens", "outputTokens"} {
		if !contains(payload, fragment) {
			t.Fatalf("hedge_losers 缺少 %s: %s", fragment, payload)
		}
	}
}

func contains(haystack string, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack string, needle string) int {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return index
		}
	}
	return -1
}

// 选路四列（provider_vendor_id / group_priorities / protocol_conversion_enabled /
// disable_session_reuse）必须能从只读视图读出，且与绕过 row_to_json 的原始 SQL 逐字段一致。
// route 包正是因为这几列缺读才自持过一份 SQL；这条断言是「以后仍走同一份视图」的守卫。
func TestIntegrationReadProviderRoutingColumns(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取 data 分道失败: %v", err)
	}

	providers, err := pools.FindEnabledProviders(ctx)
	if err != nil {
		t.Fatalf("读取启用态供应商失败: %v", err)
	}
	if len(providers) == 0 {
		t.Skip("库中没有启用态供应商，跳过该断言")
	}

	for _, provider := range providers {
		var vendorID *int64
		var groupPriorities *string
		var conversionEnabled, disableSessionReuse bool
		if err := pool.QueryRow(ctx,
			`SELECT provider_vendor_id, group_priorities::text,
			        protocol_conversion_enabled, disable_session_reuse
			 FROM providers WHERE id = $1`,
			provider.ID,
		).Scan(&vendorID, &groupPriorities, &conversionEnabled, &disableSessionReuse); err != nil {
			// 共享库上「先列表、再按 id 复读」天然会撞并发的删除：任何另一个进程（并发的
			// 集成测试、别的 worktree 里的同一条用例）此时回收自己的夹具，就会让这一行消失。
			// 行已不存在，就没有「投影与原始列不一致」可言——跳过，不当成失败。
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			t.Fatalf("读取供应商 %d 的原始列失败: %v", provider.ID, err)
		}

		if !sameInt64Pointer(provider.ProviderVendorID, vendorID) {
			t.Fatalf("供应商 %d 的 provider_vendor_id = %v, 原始列为 %v",
				provider.ID, provider.ProviderVendorID, vendorID)
		}
		if provider.ProtocolConversionEnabled != conversionEnabled {
			t.Fatalf("供应商 %d 的 protocol_conversion_enabled = %v, 原始列为 %v",
				provider.ID, provider.ProtocolConversionEnabled, conversionEnabled)
		}
		if provider.DisableSessionReuse != disableSessionReuse {
			t.Fatalf("供应商 %d 的 disable_session_reuse = %v, 原始列为 %v",
				provider.ID, provider.DisableSessionReuse, disableSessionReuse)
		}

		// group_priorities 现在按**原文**读取（脏值不得让整行失败），可用覆盖由 DecodeGroupPriorities 挑。
		//
		// 断言只压「干净值」：共享库上可能存在其它 lane 或历史遗留写坏的脏行，那种行只要求
		// 「读取不失败 + 报出问题」，不该让本用例变红（这本身就是本次修复的不变量）。
		decoded, issues := DecodeGroupPriorities(provider.GroupPriorities)
		if groupPriorities == nil || *groupPriorities == "null" {
			if len(decoded) != 0 || len(issues) != 0 {
				t.Fatalf("供应商 %d 的 group_priorities 应为空且无问题, 实际 %v / %v",
					provider.ID, decoded, issues)
			}
			continue
		}
		expected := map[string]int{}
		if err := json.Unmarshal([]byte(*groupPriorities), &expected); err != nil {
			if len(issues) == 0 {
				t.Fatalf("供应商 %d 的原始 group_priorities %q 解不开，但视图没报问题",
					provider.ID, *groupPriorities)
			}
			continue
		}
		if len(issues) != 0 || len(decoded) != len(expected) {
			t.Fatalf("供应商 %d 的 group_priorities = %v（问题 %v）, 原始列为 %v",
				provider.ID, decoded, issues, expected)
		}
		for key, want := range expected {
			if decoded[key] != want {
				t.Fatalf("供应商 %d 的 group_priorities[%s] = %d, 原始列为 %d",
					provider.ID, key, decoded[key], want)
			}
		}
	}
}

func sameInt64Pointer(left *int64, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
