package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// TestRunOpsRoundCancelsRoundsThatOutrunTheTimeout 钉住「每一轮都有独立上限」。
//
// 此前一轮直接吃进程 ctx：卡住的任务体会把该任务的 goroutine 连同 ticker 一起拖住，
// 后续轮次全部不再发生，而日志里只有「上一轮没完成」这一种痕迹。
func TestRunOpsRoundCancelsRoundsThatOutrunTheTimeout(t *testing.T) {
	previous := opsRoundTimeout
	opsRoundTimeout = 30 * time.Millisecond
	t.Cleanup(func() { opsRoundTimeout = previous })

	var logs bytes.Buffer
	bodySawCanceled := make(chan error, 1)
	roundReturned := make(chan struct{})
	go func() {
		defer close(roundReturned)
		runOpsRound(context.Background(), logx.New(&logs), "probe", func(ctx context.Context) (jobs.OpsOutcome, error) {
			// 卡住的一轮：只有 ctx 被取消才返回。
			<-ctx.Done()
			bodySawCanceled <- ctx.Err()
			return jobs.OpsOutcome{}, ctx.Err()
		})
	}()

	select {
	case <-roundReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("runOpsRound 未在单轮上限后返回：本轮超时没有生效")
	}

	select {
	case err := <-bodySawCanceled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("任务体拿到的取消原因 = %v, want context.DeadlineExceeded", err)
		}
	default:
		t.Fatal("任务体没收到取消信号")
	}

	out := logs.String()
	if !strings.Contains(out, "ops_job_round_failed") {
		t.Fatalf("超时必须落一条失败日志，实际 %q", out)
	}
	if !strings.Contains(out, "timeout") {
		t.Fatalf("超时日志应带可判别的 timeout 字段，实际 %q", out)
	}
}
