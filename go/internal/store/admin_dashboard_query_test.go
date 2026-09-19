package store

import (
	"strings"
	"testing"
)

// TestAdminDashboardOverviewQueryIsSingleScan 钉住概览聚合只有**一次** usage_ledger 扫描。
//
// 为什么必须钉：原先 8 个标量子查询各扫一遍同一批行，合并成一次扫描 + FILTER 后逐列结果相同
// （等价性依据写在 adminDashboardOverviewQuery 的注释里），真库用例只能验数值、验不出扫描次数，
// 因此这里对着语句本身钉。
func TestAdminDashboardOverviewQueryIsSingleScan(t *testing.T) {
	query := adminDashboardOverviewQuery

	if got := strings.Count(query, "FROM usage_ledger"); got != 1 {
		t.Fatalf("usage_ledger 应只被扫一次，实际出现 %d 次 FROM", got)
	}
	if got := strings.Count(query, "FILTER ("); got != 8 {
		t.Fatalf("应有 8 个 FILTER 聚合（今日 4 + 昨日 3 + RPM 1），实际 %d 个", got)
	}
	if strings.Contains(query, "(SELECT") {
		t.Fatalf("不应再有标量子查询：%s", query)
	}

	// 粗筛：三个窗口下界的最小值必须留在外层 WHERE 上，否则规划器会退化成全表扫。
	if !strings.Contains(query, "created_at >= bounds.yesterday_start") {
		t.Fatalf("缺少窗口粗筛条件 created_at >= bounds.yesterday_start")
	}
	// 口径与用户过滤逐字保留。
	if !strings.Contains(query, BillingCondition) {
		t.Fatalf("缺少账本计费口径 BillingCondition")
	}
	if !strings.Contains(query, "($2::bigint IS NULL OR user_id = $2)") {
		t.Fatalf("缺少 user 过滤")
	}
	// 窗口边界与 Node 逐字对齐：今日上界排他、RPM 无上界。
	for _, want := range []string{
		"created_at < bounds.tomorrow_start",
		"created_at < bounds.yesterday_end",
		"created_at >= CURRENT_TIMESTAMP - INTERVAL '1 minute'",
		"count(*) FILTER (WHERE NOT is_success",
		"::float8",
		"::text",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("缺少片段 %q", want)
		}
	}
}

// TestRollupFactsQueryScansColumnsDirectly 钉住 rollup 事实回读不再经过 row_to_json。
//
// 返回值逐列相同，行为层面测不出差别；这条钉的是「别把 JSON 编解码加回来」。
func TestRollupFactsQueryScansColumnsDirectly(t *testing.T) {
	if strings.Contains(rollupFactsQuery, "row_to_json") {
		t.Fatalf("不应再经 row_to_json：%s", rollupFactsQuery)
	}
	head, _, found := strings.Cut(rollupFactsQuery, "FROM message_request")
	if !found {
		t.Fatalf("查询缺少 message_request 扫描：%s", rollupFactsQuery)
	}
	for _, column := range []string{"created_at", "model", "original_model", "duration_ms"} {
		if !strings.Contains(head, column) {
			t.Fatalf("SELECT 列表缺少列 %s：%s", column, head)
		}
	}
	if got := strings.Count(rollupFactsQuery, "$1"); got != 1 {
		t.Fatalf("主键占位符应只出现一次，实际 %d 次", got)
	}
	for _, want := range []string{"id = $1", "deleted_at IS NULL", ExcludeWarmupCondition} {
		if !strings.Contains(rollupFactsQuery, want) {
			t.Fatalf("过滤条件缺少片段 %q", want)
		}
	}
}
