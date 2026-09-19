package limit

import (
	"context"
	"testing"
	"time"
)

// TestFixed5hTTLUsesCallerContext 钉住封顶响应路径上的 TTL 读取用调用方 ctx。
//
// 这一读原先硬写 context.Background()：客户端已断开时，它仍会占一次 Redis 往返与
// 连接池名额，取消无法中断。用例需要真 Redis（未设置 CCH_TEST_REDIS_URL 时跳过）。
func TestFixed5hTTLUsesCallerContext(t *testing.T) {
	client := integrationClient(t)
	id := uniqueID(t)
	cleanupKeys(t, client, Cost5hKey(EntityKey, id, ResetFixed))

	now := time.Now().UTC()
	if err := client.Raw().Set(context.Background(), Cost5hKey(EntityKey, id, ResetFixed), "1", 5*time.Hour).Err(); err != nil {
		t.Fatalf("造窗口键失败: %v", err)
	}
	service := &Service{windows: NewCostWindows(client, nil)}
	dimension := costDimension{entity: EntityKey, id: id, period: Period5h, resetMode: ResetFixed}

	if got := service.fixed5hTTL(context.Background(), dimension, now); got == nil {
		t.Fatal("未取消时应读到剩余 TTL")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := service.fixed5hTTL(canceled, dimension, now); got != nil {
		t.Fatalf("调用方 ctx 已取消，不该继续读 Redis，实际剩余 TTL = %d", *got)
	}
}
