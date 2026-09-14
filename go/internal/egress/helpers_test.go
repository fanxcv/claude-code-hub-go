package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// defaultTestTimeout 是排空相关用例的短超时，避免等待真实长时间超时。
const defaultTestTimeout = 2 * time.Second

func newRequest(method, path string, body io.Reader) *http.Request {
	return httptest.NewRequest(method, path, body)
}

func newRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

// newTestFrontDoor 建一个只带默认配置的前门（原用例会注入 Node 回退目标与归属白名单，
// 二者已随 Node 退役删除）。
func newTestFrontDoor(t *testing.T) *FrontDoor {
	t.Helper()
	return New(nil)
}

// enterFrontDoor 让一个请求穿过前门中间件并在 next 里挂住，用于制造在途请求。
// 返回释放函数与已进入 next 的信号。
func enterFrontDoor(t *testing.T, frontDoor *FrontDoor) (release func(), entered <-chan struct{}, done <-chan struct{}) {
	t.Helper()
	releaseCh := make(chan struct{})
	enteredCh := make(chan struct{})
	doneCh := make(chan struct{})
	handler := frontDoor.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(enteredCh)
		<-releaseCh
		w.WriteHeader(http.StatusOK)
	}))
	go func() {
		handler.ServeHTTP(newRecorder(), newRequest(http.MethodPost, "/v1/responses", nil))
		close(doneCh)
	}()
	return func() { close(releaseCh) }, enteredCh, doneCh
}

func TestAdmissionRejectsAfterDrain(t *testing.T) {
	instance := newAdmission()

	if err := instance.tryEnter(); err != nil {
		t.Fatalf("排空前应可接纳: %v", err)
	}
	if !instance.beginDrain() {
		t.Fatal("首次 beginDrain 应返回 true")
	}
	if instance.beginDrain() {
		t.Fatal("重复 beginDrain 应返回 false")
	}
	if !instance.isDraining() {
		t.Fatal("排空标记未置位")
	}
	if err := instance.tryEnter(); err != ErrAdmissionClosed {
		t.Fatalf("排空后应返回 ErrAdmissionClosed，得到 %v", err)
	}

	// 在途请求结束后排空完成。
	instance.leave()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if err := instance.waitEmpty(ctx); err != nil {
		t.Fatalf("在途归零后 waitEmpty 应成功: %v", err)
	}
	if instance.inFlight() != 0 {
		t.Fatalf("在途计数应为 0，得到 %d", instance.inFlight())
	}
}

func TestAdmissionWaitEmptyTimesOutWithInFlight(t *testing.T) {
	instance := newAdmission()
	if err := instance.tryEnter(); err != nil {
		t.Fatalf("应可接纳: %v", err)
	}
	instance.beginDrain()

	ctx, cancel := context.WithTimeout(context.Background(), 20*defaultTestTimeout/1000)
	defer cancel()
	if err := instance.waitEmpty(ctx); err != ErrDrainTimeout {
		t.Fatalf("有在途请求时超时应返回 ErrDrainTimeout，得到 %v", err)
	}
}
