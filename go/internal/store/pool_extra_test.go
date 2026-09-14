package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
)

func TestAdmissionErrorAccessors(t *testing.T) {
	err := &AdmissionError{lane: config.LaneWriter, maxOutstanding: 248}
	if err.Lane() != config.LaneWriter {
		t.Fatalf("Lane() = %q", err.Lane())
	}
	if err.MaxOutstanding() != 248 {
		t.Fatalf("MaxOutstanding() = %d", err.MaxOutstanding())
	}
	if !strings.Contains(err.SafeMessage(), "pool=writer") {
		t.Fatalf("SafeMessage() = %q", err.SafeMessage())
	}
}

func TestMillisParam(t *testing.T) {
	if got := millisParam(1500 * time.Millisecond); got != "1500" {
		t.Fatalf("millisParam = %q, want 1500", got)
	}
	if got := millisParam(90 * time.Second); got != "90000" {
		t.Fatalf("millisParam = %q, want 90000", got)
	}
}

// 准入失败时 QueryRow 必须返回一个会在 Scan 处报错的 Row，而不是 nil（否则调用方会 panic）。
func TestQueryRowReturnsErrorRowWhenRejected(t *testing.T) {
	pool := &Pool{lane: config.LaneData, maxOutstanding: 0}
	row := pool.QueryRow(context.Background(), "SELECT 1")
	if row == nil {
		t.Fatal("准入被拒时必须返回非 nil 的 Row")
	}
	var value int
	err := row.Scan(&value)
	if err == nil {
		t.Fatal("Scan 必须返回准入错误")
	}
	if !IsAdmissionError(err) {
		t.Fatalf("错误类型 = %T, want *AdmissionError", err)
	}
}

// 准入失败时 Query 与 Exec 都必须立刻失败，且不触碰底层池。
func TestQueryAndExecRejectedWhenAtLimit(t *testing.T) {
	pool := &Pool{lane: config.LaneData, maxOutstanding: 0}
	if _, err := pool.Query(context.Background(), "SELECT 1"); !IsAdmissionError(err) {
		t.Fatalf("Query 应当返回准入错误，实际 %v", err)
	}
	if _, err := pool.Exec(context.Background(), "SELECT 1"); !IsAdmissionError(err) {
		t.Fatalf("Exec 应当返回准入错误，实际 %v", err)
	}
	if _, err := pool.Begin(context.Background()); !IsAdmissionError(err) {
		t.Fatalf("Begin 应当返回准入错误，实际 %v", err)
	}
}
