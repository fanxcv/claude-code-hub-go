package replay

import (
	"context"
	"net/http"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// TestReplayHitAuditRowIsSettled 钉住「回放命中的审计行当场落终态」这条接线。
//
// 为何重要：message_request 的终态列只允许写一次（谓词 `status_code IS NULL`）。建行而不落终态
// 的行会永远停在「请求中」，随后被 patrol 以 499/CLIENT_ABORTED 补写（patrol.RepairStatusCode）
// ——那是「客户端断开」的语义，与事实相反：本次请求是**成功**由回放存档返回的。生产实测：
// 近 24h 全部未终态与 499 行里 290/306 都是这个缺口（provider_id=0、is_replay=true、无 provider_chain）。
//
// 反证：把 writeAuditRow 末尾那行 `a.settleAuditRow(ctx, createdID, statusCode)` 删掉，
// 本用例的 status_code 断言即红（退回 NULL），patrol 随后会把它写成 499。
func TestReplayHitAuditRowIsSettled(t *testing.T) {
	pools := openTestPools(t)
	attacher := NewAttacher(AttacherOptions{Pools: pools})

	apiKey := "sk-replay-audit-row-nail"
	header := http.Header{}
	header.Set("user-agent", "replay-audit-row-nail/1.0")
	req, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages", Headers: header})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}
	req.SetAuth(pctx.AuthState{KeyID: 7, UserID: 3, APIKey: apiKey})

	t.Cleanup(func() {
		pool, err := pools.Writer()
		if err != nil {
			return
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM message_request WHERE key = $1`, apiKey)
	})

	attacher.writeAuditRow(context.Background(), req, Identity{
		Model:    "replay-audit-row-nail-model",
		Endpoint: "/v1/messages",
		UserID:   3,
	}, http.StatusOK, "redis_completed", nil)

	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	var status *int
	var isReplay bool
	if err := pool.QueryRow(context.Background(),
		`SELECT status_code, is_replay FROM message_request WHERE key = $1`, apiKey,
	).Scan(&status, &isReplay); err != nil {
		t.Fatalf("读审计行失败: %v", err)
	}
	if !isReplay {
		t.Fatalf("审计行未标记 is_replay")
	}
	if status == nil {
		t.Fatalf("审计行未落终态：status_code 仍为 NULL（会永远停在「请求中」，随后被 patrol 补成 499）")
	}
	if *status != http.StatusOK {
		t.Fatalf("审计行终态应为 %d，实际 %d", http.StatusOK, *status)
	}
}
