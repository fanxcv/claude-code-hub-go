package pubstatus

import (
	"testing"
	"time"
)

// TestRunSingleFlightReleasesEntryWhenComputePanics 钉住「重建 panic 不得挂死同键调用者」。
//
// 原实现把收尾写在正常路径上：compute 一 panic，done 永不关闭、条目永不删除，
// 同键的等待者会全部停在 <-entry.done 上——一个后台重建的 panic 会变成整条投影链的静默挂死。
func TestRunSingleFlightReleasesEntryWhenComputePanics(t *testing.T) {
	flightKey := "panic-release-" + t.Name()

	result, err := runSingleFlight(flightKey, func() (RebuildResult, error) {
		panic("boom")
	})
	if err == nil {
		t.Fatalf("compute panic 必须转成错误返回，实际 result=%+v", result)
	}

	inFlightMu.Lock()
	_, exists := inFlightRebuilds[flightKey]
	inFlightMu.Unlock()
	if exists {
		t.Fatal("panic 后单飞条目必须删除，否则同键调用者永久阻塞")
	}

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		if _, retryErr := runSingleFlight(flightKey, func() (RebuildResult, error) {
			return RebuildResult{Status: RebuildStatusUpdated}, nil
		}); retryErr != nil {
			t.Errorf("panic 之后同键重算应当成功: %v", retryErr)
		}
	}()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("panic 后同键调用被阻塞：单飞条目没有被释放")
	}
}
