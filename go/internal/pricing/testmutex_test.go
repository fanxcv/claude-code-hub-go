package pricing

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// modelPricesTestLockName 是跨包串行化「触碰 model_prices 表」用例的会话级咨询锁名。
//
// 为什么必须有：价格同步的整表切换（store.DeleteCloudPricesNotIn，谓词是「不在保留列表里的
// 非 manual 行」）其保留列表在调用**之前**生成，故调用瞬间之后新插入的行必然被删。共享测试库
// 上兄弟包的用例于是互相删夹具——本包「改价应判 updated」会拿到 `Added`，因为刚插入的行已被
// internal/jobs 的整表切换删掉。逐条把被删行补回只能把窗口缩到毫秒级，消不掉竞态；
// 把所有触碰该表的用例用同一把锁串行化才是根治。
//
// 同一个字面量在 internal/{pricing,jobs,adminapi,terminal} 的 *_test.go 里各写一份
// （test-only 助手无法跨包共享），改名必须四处同改；见。
const modelPricesTestLockName = "claude-code-hub:model-prices-tests"

// lockModelPricesTable 取 model_prices 的跨包互斥锁，并注册在用例结束时释放。
//
// 三个实现要点（都不是可选）：
//   - **独立连接**（pools.OpenDedicatedConn）而不是从分道池借：会话级 advisory lock 要求持锁与
//     放锁是同一条会话；从池里借还会占住分道连接，被测用例自己再取连接就会饿死
//     （internal/jobs 的 replay-cleanup 用例已踩过这个坑）。
//   - **等待有上限**（SET LOCAL lock_timeout，与取锁同一事务，不给池里的连接留脏设置）：持锁者
//     若异常未释放，门禁应当是「明确失败」而不是无限挂住——挂住的用例会让整套门禁无从判断。
//   - **自证锁生效**（查 pg_locks）：会话级锁写错锁名或换错连接都不报错，不查等于没锁。
func lockModelPricesTable(t *testing.T, pools *store.Pools) {
	t.Helper()
	ctx := context.Background()

	conn, lockPool, err := pools.OpenDedicatedConn(ctx)
	if err != nil {
		t.Fatalf("建立互斥用独立连接失败: %v", err)
	}
	// 先注册关连接：t.Cleanup 后进先出，故下面注册的放锁会先执行。
	t.Cleanup(func() {
		conn.Release()
		lockPool.Close()
	})

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("开启取锁事务失败: %v", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '120s'`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("设置取锁等待上限失败: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, modelPricesTestLockName); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("取 model_prices 互斥锁失败（等待超上限，可能有别的用例持锁未释放）: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交取锁事务失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, modelPricesTestLockName)
	})

	var held int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted`).Scan(&held); err != nil {
		t.Fatalf("校验咨询锁失败: %v", err)
	}
	if held == 0 {
		t.Fatal("咨询锁未生效：本会话在 pg_locks 里查不到 granted 的 advisory lock")
	}
}

// modelPricesLockCopyPaths 是其余三份 test-only 助手的路径（相对本包目录，即 go test 的工作目录）。
var modelPricesLockCopyPaths = []string{
	"../jobs/testmutex_test.go",
	"../adminapi/testmutex_test.go",
	"../terminal/testmutex_test.go",
}

// TestModelPricesLockNameAgreesAcrossPackages 钉住四份副本的锁名字面量逐字相同。
//
// 为何要钉：四份 test-only 助手无法共享常量，而**锁名不一致是个静默失败**——各用各的锁，
// 互斥消失，用例照旧互相删夹具，且不会有任何断言变红（正是本文件要根治的那类 bug）。
// 故直接从源码里抠字面量比对，而不是靠「记得四处同改」。
func TestModelPricesLockNameAgreesAcrossPackages(t *testing.T) {
	if len(modelPricesLockCopyPaths) != 3 {
		t.Fatalf("应在其它三个包各查一份副本，实际清单 %d 份（清单被改过就要重核计数）", len(modelPricesLockCopyPaths))
	}
	want := fmt.Sprintf("const modelPricesTestLockName = %q", modelPricesTestLockName)
	for _, path := range modelPricesLockCopyPaths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s 失败（清单路径或工作目录变了）: %v", path, err)
		}
		if !strings.Contains(string(raw), want) {
			t.Fatalf("%s 的锁名与本包不一致：互斥会静默失效。四处必须逐字相同（含本包）", path)
		}
	}
}
