package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/redis/go-redis/v9"
)

// 本文件钉住退出序列里的结算纪律：
//   - 排空之后、**关依赖之前**必须等终态落库——关连接池会把尚未发出的终态 UPDATE 带走；
//   - 等不到就必须报错退出（退出码非零）并如实带出未完成数，而不是静默成功；
//   - shutdown_started / shutdown_complete 必须带上待落库数（这个缺口在 info 级日志里原本不可见）。

// settlementStub 是实现了数据面结算等待面的假处理器。
type settlementStub struct {
	pending atomic.Int64
	// wait 决定 WaitSettlements 的行为：nil 表示立即归零。
	wait func(ctx context.Context) bool
	// onWait 在 WaitSettlements 被调用时记录顺序，用于断言「等结算先于关依赖」。
	onWait func()
}

func (s *settlementStub) ServeHTTP(http.ResponseWriter, *http.Request) {}

func (s *settlementStub) PendingSettlements() int64 { return s.pending.Load() }

func (s *settlementStub) WaitSettlements(ctx context.Context) bool {
	if s.onWait != nil {
		s.onWait()
	}
	if s.wait == nil {
		return true
	}
	return s.wait(ctx)
}

// logEventLines 把关心的原始日志行原样打出来：这两行是验收证据（go test -v 即可看到）。
func logEventLines(t *testing.T, logs string, events ...string) {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		for _, event := range events {
			if strings.Contains(line, `"event":"`+event+`"`) {
				t.Logf("原始日志 %s: %s", event, line)
			}
		}
	}
}

type bootOutcome struct {
	err   error
	logs  string
	order []string
}

// runBootWithSettlements 跑一次完整的进程生命周期，用 SIGTERM 收尾。
func runBootWithSettlements(t *testing.T, stub *settlementStub) bootOutcome {
	t.Helper()
	var (
		mu    sync.Mutex
		order []string
		logs  bytes.Buffer
	)
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}
	addresses := make(chan string, 1)
	signals := make(chan os.Signal, 1)
	port := strconv.Itoa(freePort(t))
	stub.onWait = func() { record("settle_wait") }

	options := startup{
		Logger: logx.New(&logs),
		LookupEnv: func(name string) (string, bool) {
			switch name {
			case "PORT":
				return port, true
			case "CCH_EGRESS_MODE":
				return "node", true
			}
			return "", false
		},
		OpenDeps: func(_ context.Context, _ config.Config, rules *cfgsync.Snapshot) (dependencies, error) {
			return &stubDeps{snapshot: rules}, nil
		},
		OpenSubscriber: func(config.Config) (redis.UniversalClient, error) { return nil, nil },
		RegisterDomains: func(_ context.Context, rules *rulesSync) error {
			return rules.Register(cfgsync.DomainAPIKeys, func(context.Context) error { return nil })
		},
		OpenDataPlane: func(_ context.Context, _ dataPlaneOptions) (http.Handler, func(), error) {
			return stub, func() { record("dataplane_close") }, nil
		},
		Listen: func(int) (net.Listener, error) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			addresses <- listener.Addr().String()
			return listener, nil
		},
		Signals:         func() (<-chan os.Signal, func()) { return signals, func() {} },
		DrainTimeout:    time.Second,
		ShutdownTimeout: time.Second,
		ProbeTimeout:    time.Second,
	}

	finished := make(chan error, 1)
	go func() { finished <- runWith(context.Background(), options) }()

	select {
	case <-addresses:
	case <-time.After(5 * time.Second):
		t.Fatal("监听未在限期内建立")
	}
	signals <- syscall.SIGTERM

	outcome := bootOutcome{}
	select {
	case outcome.err = <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("收到 SIGTERM 后未在限期内退出")
	}
	mu.Lock()
	outcome.order = append([]string(nil), order...)
	mu.Unlock()
	outcome.logs = logs.String()
	return outcome
}

// 排空窗口内终态没落完：必须报错（退出码非零）并如实带出未完成数。
func TestShutdownFailsWhenSettlementsRemain(t *testing.T) {
	stub := &settlementStub{pending: atomic.Int64{}}
	stub.pending.Store(2)
	stub.wait = func(ctx context.Context) bool {
		<-ctx.Done()
		return false
	}

	outcome := runBootWithSettlements(t, stub)
	logEventLines(t, outcome.logs, "shutdown_started", "settle_incomplete")
	if !errors.Is(outcome.err, errSettleIncomplete) {
		t.Fatalf("终态未落库时必须报错退出，收到 %v（日志 %s）", outcome.err, outcome.logs)
	}
	if !bytes.Contains([]byte(outcome.logs), []byte(`"event":"settle_incomplete"`)) ||
		!bytes.Contains([]byte(outcome.logs), []byte(`"pendingSettlements":2`)) {
		t.Fatalf("必须留下带未完成数的 settle_incomplete 日志：%s", outcome.logs)
	}
	if !bytes.Contains([]byte(outcome.logs), []byte(`"event":"shutdown_started"`)) {
		t.Fatalf("缺少 shutdown_started 日志：%s", outcome.logs)
	}
	if !bytes.Contains([]byte(outcome.logs), []byte(`"pendingSettlements":2`)) {
		t.Fatalf("shutdown_started 必须带出待落库数：%s", outcome.logs)
	}
}

// 终态已归零：正常退出，且 shutdown_complete 如实报 0；等结算发生在关依赖之前。
func TestShutdownWaitsForSettlementsBeforeClosingDataPlane(t *testing.T) {
	stub := &settlementStub{}

	outcome := runBootWithSettlements(t, stub)
	logEventLines(t, outcome.logs, "shutdown_started", "shutdown_complete")
	if outcome.err != nil {
		t.Fatalf("终态已落库时关闭不得报错: %v（日志 %s）", outcome.err, outcome.logs)
	}
	if !bytes.Contains([]byte(outcome.logs), []byte(`"event":"shutdown_complete"`)) {
		t.Fatalf("缺少 shutdown_complete 日志：%s", outcome.logs)
	}
	if !bytes.Contains([]byte(outcome.logs), []byte(`"pendingSettlements":0`)) {
		t.Fatalf("shutdown_complete 必须报出 0（缺口靠这个数才能被发现）：%s", outcome.logs)
	}
	waitIndex := indexOfStep(outcome.order, "settle_wait")
	closeIndex := indexOfStep(outcome.order, "dataplane_close")
	if waitIndex < 0 || closeIndex < 0 || waitIndex > closeIndex {
		t.Fatalf("等结算必须先于数据面收口，收到顺序 %v", outcome.order)
	}
}
