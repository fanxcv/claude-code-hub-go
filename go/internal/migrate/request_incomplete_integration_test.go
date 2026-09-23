package migrate

import (
	"context"
	"testing"
)

// TestRequestIncompleteAvailabilityOutcome 用真 PG 验证本迁移的业务分叉与 0137 的断流判据。
func TestRequestIncompleteAvailabilityOutcome(t *testing.T) {
	dsn := scratchDatabase(t, testDSN(t))
	pool := openTestPool(t, dsn)
	ctx := context.Background()
	if _, err := Up(ctx, pool); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	cases := []struct {
		name    string
		message *string
		chain   string
		want    string
	}{
		{"输出额度触顶", textRef("request_incomplete"), "[]", "excluded"},
		{"上游正文断流", textRef("upstream_stream_cut"), "[]", "failure"},
		{"正常完成", nil, "[]", "success"},
		{"既有客户端中断", textRef("context canceled"), `[{"reason":"client_abort","statusCode":200}]`, "excluded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			err := pool.QueryRow(ctx, `SELECT fn_compute_message_request_success_rate_outcome(NULL, 200, $1, $2::jsonb)`, tc.message, tc.chain).Scan(&got)
			if err != nil {
				t.Fatalf("读取可用性结论失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("可用性结论=%q，期望 %q", got, tc.want)
			}
		})
	}
}

func textRef(value string) *string { return &value }
