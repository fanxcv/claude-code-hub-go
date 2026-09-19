package limit

import (
	"fmt"
	"sync"
	"time"
)

// 本文件是「租约判定留下的丢弃日志」的限频器。
//
// limit.lease.plan_target_dropped 是「本维不结算、租约余额可能偏宽」的唯一排查入口，必须保留；
// 但它逐请求、逐维触发——取值域一旦整体漂移（例如库里新增了一个周期），高 QPS 下会以
// 「请求数×维数」的速率刷 Error 级日志，把日志面自身变成故障面。
// 这里按「目标」限频，被压掉的条数在下次放行时随 suppressed 字段报回，可观测性不丢。

// leaseDropLogEvery 是同一结算目标重复留痕的最小间隔。
const leaseDropLogEvery = time.Minute

// leaseDropLogMaxKeys 是键表上限；超出即整表清零（这条路径要的是有界内存，不是精确计数：
// 被清掉的键只会多打一条日志）。
const leaseDropLogMaxKeys = 1024

// leaseDropLogState 是限频器的进程级状态，按「目标」而不是按请求记账。
var leaseDropLogState = struct {
	mu         sync.Mutex
	lastAt     map[string]time.Time
	suppressed map[string]int
}{
	lastAt:     map[string]time.Time{},
	suppressed: map[string]int{},
}

// leaseDropLogKey 是限频键：同一实体、同一窗口、同一重置模式、同一主体 id 才算同一目标。
func leaseDropLogKey(dimension costDimension) string {
	return fmt.Sprintf("%s|%s|%s|%d", dimension.entity, dimension.period, dimension.resetMode, dimension.id)
}

// allowLeaseDropLog 判断该键此刻能否留痕，并给出自上次留痕以来被压掉的条数。
func allowLeaseDropLog(key string, now time.Time) (bool, int) {
	leaseDropLogState.mu.Lock()
	defer leaseDropLogState.mu.Unlock()
	if last, ok := leaseDropLogState.lastAt[key]; ok && now.Sub(last) < leaseDropLogEvery {
		leaseDropLogState.suppressed[key]++
		return false, 0
	}
	if len(leaseDropLogState.lastAt) >= leaseDropLogMaxKeys {
		leaseDropLogState.lastAt = map[string]time.Time{}
		leaseDropLogState.suppressed = map[string]int{}
	}
	suppressed := leaseDropLogState.suppressed[key]
	delete(leaseDropLogState.suppressed, key)
	leaseDropLogState.lastAt[key] = now
	return true, suppressed
}

// resetLeaseDropLogLimiter 清空限频状态（用例专用：同一进程内重复跑同一维度时不被上一轮挡住）。
func resetLeaseDropLogLimiter() {
	leaseDropLogState.mu.Lock()
	defer leaseDropLogState.mu.Unlock()
	leaseDropLogState.lastAt = map[string]time.Time{}
	leaseDropLogState.suppressed = map[string]int{}
}
