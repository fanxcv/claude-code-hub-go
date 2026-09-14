package egress

import (
	"context"
	"errors"
	"sync"
)

// ErrAdmissionClosed 表示前门已进入排空窗口，拒绝接纳新请求。
var ErrAdmissionClosed = errors.New("egress: 前门已停止接纳新请求")

// ErrDrainTimeout 表示排空超时，仍有在途请求未结束。
var ErrDrainTimeout = errors.New("egress: 排空超时，仍有在途请求")

// admission 维护在途计数与排空闸门。
//
// 语义要点：一旦进入排空，就不再重新开放（重开需新建前门实例）。理由是「在途归零」
// 是退出序列的唯一安全点：此刻关连接池、停后台任务才不会把尚未落库的终态一并带走；
// 若中途重新放行，归零时刻会被无限推迟。
type admission struct {
	mu       sync.Mutex
	cond     *sync.Cond
	count    int64
	draining bool
}

func newAdmission() *admission {
	instance := &admission{}
	instance.cond = sync.NewCond(&instance.mu)
	return instance
}

func (a *admission) tryEnter() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.draining {
		return ErrAdmissionClosed
	}
	a.count++
	return nil
}

func (a *admission) leave() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.count > 0 {
		a.count--
	}
	if a.count == 0 {
		a.cond.Broadcast()
	}
}

// inFlight 返回当前在途请求数。
func (a *admission) inFlight() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.count
}

func (a *admission) isDraining() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.draining
}

// beginDrain 置排空标记；返回是否为本次调用首次置位。
func (a *admission) beginDrain() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.draining {
		return false
	}
	a.draining = true
	a.cond.Broadcast()
	return true
}

// waitEmpty 等待在途归零；ctx 结束时返回 ErrDrainTimeout。
//
// 用一个 goroutine 把 ctx 结束转成 Broadcast，避免 Cond 无法被 ctx 唤醒。
func (a *admission) waitEmpty(ctx context.Context) error {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			a.mu.Lock()
			a.cond.Broadcast()
			a.mu.Unlock()
		case <-stop:
		}
	}()

	a.mu.Lock()
	defer a.mu.Unlock()
	for a.count > 0 {
		if err := ctx.Err(); err != nil {
			return ErrDrainTimeout
		}
		a.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return ErrDrainTimeout
	}
	return nil
}
