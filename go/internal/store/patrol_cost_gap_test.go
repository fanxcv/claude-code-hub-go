package store

import (
	"strings"
	"testing"
)

// TestListCostGapRequestsQueryShape 钉住成本缺口查询的谓词与分页形态：
// 终态已提交 + 成本为空 + 未软删 + 阈值 + (created_at,id) 行值游标，且不得用 OFFSET。
func TestListCostGapRequestsQueryShape(t *testing.T) {
	query := listCostGapRequestsQuery
	for _, want := range []string{
		"status_code IS NOT NULL",
		"cost_usd IS NULL",
		"deleted_at IS NULL",
		"created_at < $1",
		"(created_at, id) > ($2, $3)",
		"ORDER BY created_at ASC, id ASC",
		"LIMIT $4",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("成本缺口查询必须含 %q，实际:\n%s", want, query)
		}
	}
	if strings.Contains(query, "OFFSET") {
		t.Fatalf("不得用 OFFSET 分页（失败行会反复占据首页）:\n%s", query)
	}
	if strings.Contains(query, "UPDATE") || strings.Contains(query, "usage_ledger") {
		t.Fatalf("缺口扫描必须是只读查询:\n%s", query)
	}
}
