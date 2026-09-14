package jobs

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件量「取一次 advisory 锁要开几条新 PG 会话」。
//
// 用 pg_stat_database.sessions 的增量作为判据：它是**累计建立的会话数**，不随连接归还而减少，
// 因此「N 次取锁 → sessions 增量 N」就等价于「每次取锁都新开一条连接、用完即关」。
// 这比看 pg_stat_activity 的瞬时连接数更可靠（瞬时值看不出开关频率）。
//
// 门控 CCH_TEST_DSN；它是秒级检查，但仍不进默认门禁的必要性不大——留在这里是给改前后的
// 同一把尺子。用法：
//
//	CCH_TEST_DSN=... go test -run TestProfileLockConnectionChurn -v ./internal/jobs/
func TestProfileLockConnectionChurn(t *testing.T) {
	pools := profilePools(t)
	ctx := context.Background()

	const ticks = 20
	before := profileSessionCount(t, pools)
	consumer, err := NewAvailProjectionConsumer(AvailProjectionConsumerOptions{Pools: pools})
	if err != nil {
		t.Fatalf("构造消费器失败: %v", err)
	}
	for index := 0; index < ticks; index++ {
		if _, err := consumer.RunOnce(ctx); err != nil {
			t.Fatalf("第 %d 轮失败: %v", index, err)
		}
	}
	after := profileSessionCount(t, pools)
	t.Logf("取锁 %d 次 → 新增 PG 会话 %d 条（%.2f 条/次）", ticks, after-before, float64(after-before)/ticks)
	if after-before >= int64(ticks) {
		t.Logf("判读：每次取锁都新开会话（改前形态）")
	} else {
		t.Logf("判读：会话被复用（增量远小于次数即说明复用了长连接）")
	}
}

// profileSessionCount 读当前库累计建立的会话数。
func profileSessionCount(t *testing.T, pools *store.Pools) int64 {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制池失败: %v", err)
	}
	var sessions int64
	if err := pool.QueryRow(context.Background(),
		`SELECT sessions FROM pg_stat_database WHERE datname = current_database()`).Scan(&sessions); err != nil {
		t.Fatalf("读 sessions 失败: %v", err)
	}
	return sessions
}
