package jobs

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住「复用常驻会话」这一优化不可越过的两条语义。门控 CCH_TEST_DSN。
//
//	用法：CCH_TEST_DSN=... go test -run TestLeader -v ./internal/jobs/

// TestLeaderSameNameIsExclusiveWithinProcess 是**语义钉子**。
//
// 最初的实现每次取锁都用一条新会话，于是同进程内第二次取同一个锁会得到「被占」
// （pg_try_advisory_lock 在另一条会话上失败）。改成复用常驻会话后，PG 的**同会话重入**会让
// 第二次取锁成功（advisory 锁按会话计数），语义就反了 —— 可用性投影的回填与消费共用同一个
// 锁名，正是这种重入会真出事的地方。本钉子钉住：同一锁名在进程内仍然互斥。
func TestLeaderSameNameIsExclusiveWithinProcess(t *testing.T) {
	pools := profilePools(t)
	ctx := context.Background()
	name := fmt.Sprintf("cch-test:leader-exclusive:%d", time.Now().UnixNano())

	first, acquired, err := AcquireLeader(ctx, pools, name)
	if err != nil {
		t.Fatalf("首次取锁失败: %v", err)
	}
	if !acquired {
		t.Fatal("首次取锁应成功")
	}

	second, acquired, err := AcquireLeader(ctx, pools, name)
	if err != nil {
		t.Fatalf("同锁名第二次取锁应快速给出结论而不是报错: %v", err)
	}
	if acquired || second != nil {
		t.Fatal("同进程内同一锁名应互斥：第二次取锁不该成功")
	}

	if err := first.Release(ctx); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	// 释放后必须能再取到，否则锁会永久卡死。
	third, acquired, err := AcquireLeader(ctx, pools, name)
	if err != nil || !acquired {
		t.Fatalf("释放后应可再取锁（err=%v acquired=%v）", err, acquired)
	}
	if err := third.Release(ctx); err != nil {
		t.Fatalf("二次释放失败: %v", err)
	}
}

// TestLeaderDistinctNamesDoNotShareSession 是**并发安全钉子**。
//
// 一条 pgx 连接只允许一个写入者：若不同锁名共用同一条常驻会话，两个 goroutine 并发取锁就会
// 撞进 pgx 内部并 panic（实测踩过 `BUG: slow write timer already active`）。
// 这里既断言「每个锁名各占一条连接」（按 application_name 数后端），也在并发取放中让
// 共享连接必然暴露。
func TestLeaderDistinctNamesDoNotShareSession(t *testing.T) {
	pools := profilePools(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("cch-test:leader-distinct:%d", time.Now().UnixNano())
	const names = 4

	before := countDedicatedBackends(t, pools)

	var waitGroup sync.WaitGroup
	locks := make([]*LeaderLock, names)
	errors := make([]error, names)
	for index := 0; index < names; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			lock, acquired, err := AcquireLeader(ctx, pools, fmt.Sprintf("%s:%d", prefix, index))
			if err != nil {
				errors[index] = err
				return
			}
			if !acquired {
				errors[index] = fmt.Errorf("锁 %d 应取到", index)
				return
			}
			locks[index] = lock
		}(index)
	}
	waitGroup.Wait()
	for index, err := range errors {
		if err != nil {
			t.Fatalf("并发取锁第 %d 条失败: %v", index, err)
		}
	}

	after := countDedicatedBackends(t, pools)
	if after-before < names {
		t.Fatalf("期望 %d 个锁名各自占一条专用连接，实际只多出 %d 条（共享会话会让并发写撞 panic）", names, after-before)
	}

	for _, lock := range locks {
		if err := lock.Release(ctx); err != nil {
			t.Fatalf("释放失败: %v", err)
		}
	}
}

// countDedicatedBackends 数本进程当前活着的专用连接数（按 application_name 认）。
func countDedicatedBackends(t *testing.T, pools *store.Pools) int {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制池失败: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE application_name LIKE '%dedicated%' AND pid <> pg_backend_pid()`).Scan(&count); err != nil {
		t.Fatalf("读 pg_stat_activity 失败: %v", err)
	}
	return count
}
