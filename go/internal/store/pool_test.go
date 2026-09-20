package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
)

// 准入计数与上限判定复刻 admitted-client：计数达上限立刻失败，不排队。
func TestAdmissionRejectsAtLimit(t *testing.T) {
	pool := &Pool{lane: config.LaneData, maxOutstanding: 2}

	releaseFirst, err := pool.acquire()
	if err != nil {
		t.Fatalf("第一次准入应当成功: %v", err)
	}
	releaseSecond, err := pool.acquire()
	if err != nil {
		t.Fatalf("第二次准入应当成功: %v", err)
	}
	if got := pool.Outstanding(); got != 2 {
		t.Fatalf("在途数 = %d, want 2", got)
	}

	_, err = pool.acquire()
	if err == nil {
		t.Fatal("达到上限后必须拒绝")
	}
	var admission *AdmissionError
	if !errors.As(err, &admission) {
		t.Fatalf("错误类型 = %T, want *AdmissionError", err)
	}
	if admission.Code() != AdmissionErrorCode {
		t.Fatalf("错误码 = %q, want %q", admission.Code(), AdmissionErrorCode)
	}
	if !strings.Contains(admission.Error(), "exceeded 2 outstanding operations") {
		t.Fatalf("错误文案与 TS 不一致: %q", admission.Error())
	}
	if admission.SafeMessage() != "Database pool admission exceeded (pool=data, maxOutstanding=2)" {
		t.Fatalf("对外文案与 TS 不一致: %q", admission.SafeMessage())
	}
	if !isAdmission(err) {
		t.Fatal("IsAdmissionError 应当判别为真")
	}

	releaseFirst()
	releaseFirst() // 重复释放必须幂等，否则在途计数会漂移成负数。
	if got := pool.Outstanding(); got != 1 {
		t.Fatalf("重复释放后计数 = %d, want 1", got)
	}
	releaseSecond()
	if got := pool.Outstanding(); got != 0 {
		t.Fatalf("全部释放后计数 = %d, want 0", got)
	}
}

func TestAdmissionIsConcurrencySafe(t *testing.T) {
	const maxOutstanding = 8
	const workers = 64

	pool := &Pool{lane: config.LaneControl, maxOutstanding: maxOutstanding}
	releaseAll := make(chan struct{})
	allAttempted := sync.WaitGroup{}
	allAttempted.Add(workers)
	done := sync.WaitGroup{}
	done.Add(workers)

	var mutex sync.Mutex
	accepted := 0
	rejected := 0

	for index := 0; index < workers; index++ {
		go func() {
			defer done.Done()
			release, err := pool.acquire()
			mutex.Lock()
			if err != nil {
				rejected++
			} else {
				accepted++
			}
			mutex.Unlock()
			// 已尝试的标志必须在阻塞之前点亮，否则主协程无法判定「所有尝试都已发生」。
			allAttempted.Done()
			if err != nil {
				return
			}
			// 持住不放，制造真实并发：此时在途数应当恰好顶到上限。
			<-releaseAll
			release()
		}()
	}

	allAttempted.Wait()
	if got := pool.Outstanding(); got != maxOutstanding {
		t.Fatalf("同时持有时在途数 = %d, want %d", got, maxOutstanding)
	}
	if accepted != maxOutstanding {
		t.Fatalf("放行 %d 次, want %d", accepted, maxOutstanding)
	}
	if rejected != workers-maxOutstanding {
		t.Fatalf("拒绝 %d 次, want %d", rejected, workers-maxOutstanding)
	}

	close(releaseAll)
	done.Wait()
	if got := pool.Outstanding(); got != 0 {
		t.Fatalf("全部释放后在途数应当归零，实际 %d", got)
	}
}

// 分道预算与准入上限必须与 TS 侧的具体数字一致（plan 文档 §8 与 db.ts 的注释）。
func TestPoolBudgetMatchesTS(t *testing.T) {
	production := config.SplitPoolBudget(config.DefaultProductionPoolTotal)
	if production.Data != 31 || production.Control != 8 || production.Writer != 1 {
		t.Fatalf("DB_POOL_MAX=40 分道错误: %+v", production)
	}
	if got := production.MaxOutstanding(config.LaneData); got != 248 {
		t.Fatalf("data 准入上限 = %d, want 248", got)
	}

	single := config.SplitPoolBudget(1)
	if single.Data != 0 || single.Control != 1 || single.Writer != 0 {
		t.Fatalf("total=1 分道错误: %+v", single)
	}
	if got := single.PhysicalLane(config.LaneData); got != config.LaneControl {
		t.Fatalf("total=1 时 data 应复用 control，实际 %s", got)
	}
	if got := single.MaxOutstanding(config.LaneData); got != maxAdmissionFloor {
		t.Fatalf("复用分道后的准入上限 = %d, want %d", got, maxAdmissionFloor)
	}
}

// maxAdmissionFloor 复刻 admitted-client 的 MIN_OUTSTANDING_PER_POOL。
const maxAdmissionFloor = 32

func TestOpenRequiresDSN(t *testing.T) {
	if _, err := Open(context.Background(), Options{}); err == nil {
		t.Fatal("缺 DSN 必须报错")
	}
}

func TestClosedPoolsRejectUse(t *testing.T) {
	pools, err := Open(context.Background(), Options{
		DSN:    "postgres://user:pass@127.0.0.1:1/db",
		Budget: config.SplitPoolBudget(4),
	})
	if err != nil {
		t.Fatalf("Open 应当成功（不建连接）: %v", err)
	}
	if err := pools.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	_, err = pools.Data()
	if err == nil {
		t.Fatal("关闭后取分道必须失败")
	}
	var lifecycle *LifecycleError
	if !errors.As(err, &lifecycle) {
		t.Fatalf("错误类型 = %T, want *LifecycleError", err)
	}
	if !strings.Contains(lifecycle.Error(), "closed") {
		t.Fatalf("错误文案 = %q", lifecycle.Error())
	}
	if lifecycle.Code() != PoolLifecycleErrorCode {
		t.Fatalf("错误码 = %q", lifecycle.Code())
	}
	// 重复 Close 必须幂等。
	if err := pools.Close(); err != nil {
		t.Fatalf("重复 Close 应当返回同一结果: %v", err)
	}
}

// isAdmission 是测试内的准入错误判别：生产侧只留 errors.As（导出包装无人调用，已删）。
func isAdmission(err error) bool {
	var admission *AdmissionError
	return errors.As(err, &admission)
}
