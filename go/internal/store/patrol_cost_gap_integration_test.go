package store

import (
	"context"
	"testing"
	"time"
)

// TestIntegrationListCostGapRequestsSkipsDesignNull 钉住成本缺口扫描的误报面：
// 「设计上就没有成本」的行不得被报成缺口，而真缺口仍须报出。
//
// 三类设计性 NULL 各取一行（warmup 拦截、count_tokens、responses/compact），真缺口取
// 「可计费端点 + 未被拦截 + 成本为空」一行。行的形态按生产写法造：建行（成本不写）
// 再用终态写补上 status_code / blocked_by，与 terminal.SettleBlocked 的两步同形。
func TestIntegrationListCostGapRequestsSkipsDesignNull(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	designNull := []int64{
		createGapFixtureRow(t, pools, key, "/v1/messages", "warmup"),
		createGapFixtureRow(t, pools, key, "/v1/messages/count_tokens", ""),
		createGapFixtureRow(t, pools, key, "/v1/responses/compact", ""),
	}
	realGap := createGapFixtureRow(t, pools, key, "/v1/messages", "")

	seen := collectCostGapIDs(t, pools)
	for _, id := range designNull {
		if seen[id] {
			t.Fatalf("设计性 NULL 行不得报成成本缺口: id=%d", id)
		}
	}
	if !seen[realGap] {
		t.Fatalf("真缺口（可计费端点、未被拦截、成本为空）必须报出: id=%d", realGap)
	}
}

// createGapFixtureRow 造一行「终态已提交、cost_usd 为 NULL」的 message_request，返回行 id。
// blockedBy 为空串表示不写（保持 NULL）。
func createGapFixtureRow(t *testing.T, pools *Pools, key, endpoint, blockedBy string) int64 {
	t.Helper()
	ctx := context.Background()
	model := "go-store-gap-model"
	row, err := pools.CreateMessageRequest(ctx, CreateMessageRequestData{
		ProviderID: 1,
		UserID:     1,
		Key:        key,
		Model:      &model,
		Endpoint:   &endpoint,
		// CostUSD 不写（nil）＝ NULL：这正是被扫描的那一列。
	})
	if err != nil {
		t.Fatalf("创建 message_request 失败: %v", err)
	}
	statusCode := 200
	patch := DetailsPatch{StatusCode: &statusCode}
	if blockedBy != "" {
		value := blockedBy
		patch.BlockedBy = &value
	}
	won, err := pools.UpdateDetailsIfUnfinalized(ctx, row.ID, patch)
	if err != nil {
		t.Fatalf("写终态失败: %v", err)
	}
	if !won {
		t.Fatalf("终态写入未赢得该行: id=%d", row.ID)
	}
	return row.ID
}

// collectCostGapIDs 有界翻完整个缺口列表，返回命中的行 id 集合。
//
// 必须翻完而不能只看首页：缺口行按 (created_at, id) 升序返回，刚造的夹具排在老行之后。
// 上界取「50 页 × 200 条」——正常部署连一页都填不满（缺口稀有是巡检的前提）；
// 撞上界说明库里确实积压了大量缺口行，此时测试该红而不是悄悄放过。
func collectCostGapIDs(t *testing.T, pools *Pools) map[int64]bool {
	t.Helper()
	const (
		pageLimit = 200
		maxPages  = 50
	)
	ctx := context.Background()
	cutoff := time.Now().Add(time.Hour)
	cursor := UnsettledCursor{}
	seen := make(map[int64]bool)
	for page := 0; page < maxPages; page++ {
		rows, err := pools.ListCostGapRequests(ctx, cutoff, cursor, pageLimit)
		if err != nil {
			t.Fatalf("查询成本缺口行失败: %v", err)
		}
		for _, row := range rows {
			seen[row.ID] = true
			cursor = UnsettledCursor{CreatedAt: row.CreatedAt, ID: row.ID}
		}
		if len(rows) < pageLimit {
			return seen
		}
	}
	t.Fatalf("缺口列表翻过 %d 页仍未结束：库里积压了大量成本缺口行", maxPages)
	return nil
}
