package dataplane

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// 本文件钉住「取行只认自己的夹具」这一口径。
// 真实假红（本文件的用例就是它的确定性复现）：共享测试库里别的包会留下**形状不符**的
// message_request 夹具行（熔断日志夹具插的就是对象 `{"id":-1}`，见
// internal/store/admin_circuit_logs_integration_test.go 的 insertRow）。取行若按
// 「共享 user 的最新 N 行」（`ORDER BY id DESC`），就会把这些行认成自己的请求，
// 并在解析 provider_chain 时 Fatal —— 而它们的出现时机取决于**别的包**跑得多快，
// 于是表现为「全模块并行偶发红、单包复跑必绿」。

// TestIntegrationFixtureRowAttribution 断言：库里存在「别人的、更新的、形状不符的」行时，
// 助手仍取回**本用例自己那条**行。
func TestIntegrationFixtureRowAttribution(t *testing.T) {
	pools := integrationStore(t)
	upstream := newIntegrationStreamUpstream(t, http.StatusOK)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))

	status, _ := integrationStreamRequest(t, pools, provisioned, "/v1/messages")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}
	own := waitForFinalizedRow(t, provisioned)
	ownID, ok := own["id"].(float64)
	if !ok {
		t.Fatalf("自己的行没有 id：%v", own["id"])
	}

	// 造一条「别人的」行：同一个共享 user、id 更新、provider_chain 是**对象**而非数组。
	// 这正是别的包留下的行长什么样——不设身份收窄与形状守卫就会把它当成本次请求的行。
	const foreignKey = "dataplane-fixture-attribution"
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	ctx := context.Background()
	var foreignID int64
	if err := writer.QueryRow(ctx, `
		INSERT INTO message_request (
			provider_id, user_id, key, model, original_model, endpoint,
			status_code, duration_ms, cost_usd, is_replay, error_message,
			provider_chain, created_at, updated_at
		) VALUES (
			$1, $2, $3, $3, $3, '/v1/responses',
			200, 123, 0::numeric, false, NULL,
			'{"id": -1}'::jsonb, now(), now()
		) RETURNING id`,
		provisioned.providerID, provisioned.userID, foreignKey,
	).Scan(&foreignID); err != nil {
		t.Fatalf("插入「别人的」夹具行失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = writer.Exec(cleanupCtx,
			`DELETE FROM usage_ledger WHERE request_id IN
				(SELECT id FROM message_request WHERE key = $1)`, foreignKey)
		_, _ = writer.Exec(cleanupCtx, `DELETE FROM message_request WHERE key = $1`, foreignKey)
	})
	if int64(foreignID) <= int64(ownID) {
		t.Fatalf("夹具行应先于本次请求落库才构成干扰场景：foreign=%d own=%d", foreignID, int64(ownID))
	}

	// 关键断言：取回的必须是**自己那条**（id 相等），而不是更新的那条别人的行。
	row := latestRowContainingProvider(t, provisioned, provisioned.providerID)
	gotID, ok := row["id"].(float64)
	if !ok {
		t.Fatalf("取回的行没有 id：%v", row["id"])
	}
	if int64(gotID) != int64(ownID) {
		t.Fatalf("取回了别人的行：期望 id=%d（自己的），实际 id=%d（foreign=%d）",
			int64(ownID), int64(gotID), foreignID)
	}
	// 取回的行必须真的带自己的链（形状可解析）。
	if chain := providerChainOf(t, row); len(chain) == 0 {
		t.Fatalf("取回的行链为空：%v", row["provider_chain"])
	}
}
