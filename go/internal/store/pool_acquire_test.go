package store

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
)

// TestAcquireNeverExceedsLimitUnderContention 钉住并发下的准入上限。
//
// 原来的「先 Load 再 Add」在两步之间留了窗口：并发调用者会同时越过上限，
// 瞬时在途数可以超过 maxOutstanding（准入形同虚设）。这里用起跑线把并发压满，
// 每个持有者进入临界区后再核一次「同时在途数是否超过上限」。
func TestAcquireNeverExceedsLimitUnderContention(t *testing.T) {
	const (
		maxOutstanding = 4
		callers        = 512
		trials         = 8
	)
	for trial := 0; trial < trials; trial++ {
		pool := &Pool{lane: config.LaneData, maxOutstanding: maxOutstanding}

		var (
			start    = make(chan struct{})
			held     atomic.Int64
			peak     atomic.Int64
			overflow atomic.Bool
			wg       sync.WaitGroup
		)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				release, err := pool.acquire()
				if err != nil {
					return
				}
				defer release()
				now := held.Add(1)
				for observed := peak.Load(); now > observed; observed = peak.Load() {
					if peak.CompareAndSwap(observed, now) {
						break
					}
				}
				if now > maxOutstanding {
					overflow.Store(true)
				}
				held.Add(-1)
			}()
		}
		close(start)
		wg.Wait()

		if overflow.Load() {
			t.Fatalf("第 %d 轮并发在途数峰值 %d 超过上限 %d：准入被绕过", trial, peak.Load(), maxOutstanding)
		}
		if got := pool.Outstanding(); got != 0 {
			t.Fatalf("全部释放后在途数 = %d, want 0", got)
		}
	}
}
