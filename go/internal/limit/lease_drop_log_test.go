package limit

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// TestAllowLeaseDropLogSuppressesRepeatsAndReportsSuppressed 钉住限频语义：
// 窗口内只放行一条，被压掉的条数在下次放行时报回（可观测性不丢）。
func TestAllowLeaseDropLogSuppressesRepeatsAndReportsSuppressed(t *testing.T) {
	resetLeaseDropLogLimiter()
	base := time.Now()
	if allowed, _ := allowLeaseDropLog("key-a", base); !allowed {
		t.Fatal("首条必须放行")
	}
	for i := 0; i < 3; i++ {
		if allowed, _ := allowLeaseDropLog("key-a", base.Add(leaseDropLogEvery/2)); allowed {
			t.Fatal("限频窗口内不得重复放行")
		}
	}
	allowed, suppressed := allowLeaseDropLog("key-a", base.Add(leaseDropLogEvery+time.Second))
	if !allowed {
		t.Fatal("窗口外必须重新放行")
	}
	if suppressed != 3 {
		t.Fatalf("被压掉的条数 = %d, want 3", suppressed)
	}
}

// TestRememberLeaseTargetDropsAreRateLimited 钉住逐请求路径确实被限频（不只是限频器本身可用）。
//
// 取值域漂移时，这条 Error 日志会按「请求数×维数」触发——高 QPS 下日志面自身会变成故障面。
func TestRememberLeaseTargetDropsAreRateLimited(t *testing.T) {
	resetLeaseDropLogLimiter()
	dimension := costDimension{entity: EntityUser, id: 424242, period: "yearly", resetMode: ResetRolling}
	var plan pctx.LeaseSettlementPlan

	var first bytes.Buffer
	rememberLeaseTarget(logx.New(&first), &plan, dimension)
	if !strings.Contains(first.String(), "limit.lease.plan_target_dropped") {
		t.Fatalf("首次丢弃必须留痕，实际 %q", first.String())
	}
	if !plan.Empty() {
		t.Fatalf("取值域外的维度不得入计划: %+v", plan.Targets)
	}

	var repeated bytes.Buffer
	rememberLeaseTarget(logx.New(&repeated), &plan, dimension)
	if repeated.Len() != 0 {
		t.Fatalf("限频窗口内不得重复留痕，实际 %q", repeated.String())
	}
}
