package store

import (
	"context"
	"errors"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/jackc/pgx/v5"
)

// 准入凭据必须留在栈上：闭包 + sync.Once 的旧写法实测 1 alloc/op，而准入是每次 DB 操作
// 都走的热路径。这里钉住「0 alloc」，否则改回闭包会静默退回去。

func TestAcquireAdmissionStaysOnStack(t *testing.T) {
	pool := &Pool{lane: config.LaneData, maxOutstanding: 64}
	if allocs := testing.AllocsPerRun(100, func() {
		permit, err := pool.acquire()
		if err != nil {
			t.Fatalf("准入失败: %v", err)
		}
		permit.release()
	}); allocs != 0 {
		t.Fatalf("acquire+release 平均每次分配 %v 次，应为 0", allocs)
	}
	if got := pool.Outstanding(); got != 0 {
		t.Fatalf("在途数 = %d, want 0", got)
	}
}

// fakeTx 只实现被 admittedTx 转发的两个方法，其余留 nil 接口（未调用即不 panic）。
type fakeTx struct {
	pgx.Tx
	commitErr error
	calls     int
}

func (t *fakeTx) Commit(context.Context) error { t.calls++; return t.commitErr }

func (t *fakeTx) Rollback(context.Context) error { t.calls++; return nil }

// TestAdmittedTxReleasesOnceAcrossCommitAndRollback 钉住「Commit 后再 defer Rollback」
// 这条 pgx 惯用写法不会把在途计数减两次（减两次会让计数漂移成负数，等于放宽准入）。
func TestAdmittedTxReleasesOnceAcrossCommitAndRollback(t *testing.T) {
	pool := &Pool{lane: config.LaneData, maxOutstanding: 4}
	permit, err := pool.acquire()
	if err != nil {
		t.Fatalf("准入失败: %v", err)
	}
	inner := &fakeTx{}
	tx := &admittedTx{Tx: inner, permit: permit}

	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit 失败: %v", err)
	}
	if got := pool.Outstanding(); got != 0 {
		t.Fatalf("Commit 后在途数 = %d, want 0", got)
	}
	// 惯用写法：Commit 之后 defer 的 Rollback 仍会执行。
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback 失败: %v", err)
	}
	if got := pool.Outstanding(); got != 0 {
		t.Fatalf("Commit 后 Rollback 把计数减了两次：在途数 = %d, want 0", got)
	}
	if inner.calls != 2 {
		t.Fatalf("底层事务调用次数 = %d, want 2（转发不得被去重）", inner.calls)
	}

	// 反向顺序同样只减一次。
	permitAgain, err := pool.acquire()
	if err != nil {
		t.Fatalf("准入失败: %v", err)
	}
	rollbackFirst := &admittedTx{Tx: &fakeTx{}, permit: permitAgain}
	if err := rollbackFirst.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback 失败: %v", err)
	}
	if err := rollbackFirst.Commit(context.Background()); err != nil {
		t.Fatalf("Commit 失败: %v", err)
	}
	if got := pool.Outstanding(); got != 0 {
		t.Fatalf("Rollback 后 Commit 把计数减了两次：在途数 = %d, want 0", got)
	}
}

// fakeRow 让 admittedRow.Scan 的释放路径可测（不需要真库）。
type fakeRow struct{ pgx.Row }

func (r fakeRow) Scan(...any) error { return errors.New("boom") }

// TestAdmittedRowReleasesOnScan 钉住「Row 被 Scan 即释放」。
func TestAdmittedRowReleasesOnScan(t *testing.T) {
	pool := &Pool{lane: config.LaneData, maxOutstanding: 4}
	permit, err := pool.acquire()
	if err != nil {
		t.Fatalf("准入失败: %v", err)
	}
	row := &admittedRow{Row: fakeRow{}, permit: permit}
	if err := row.Scan(); err == nil {
		t.Fatal("底层错误必须透传")
	}
	if got := pool.Outstanding(); got != 0 {
		t.Fatalf("Scan 后在途数 = %d, want 0", got)
	}
}
