package dataplane

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 断线风暴 + 立即退出的端到端证据（真实数据库、真实终态 UPDATE）。
//
// 复现的是 的 F2：客户端在流中途集中断开后，
// handler 立刻返回、前门在途归零，而终态结算还在游离协程里；进程随后收摊关掉连接池
// （退出序列的最后一步），尚未发出的终态 UPDATE 随进程消失，行永久留在 status_code IS NULL。
//
// 与生产退出序列的等价关系：
//   - 所有 handler 返回 = 前门在途归零 = 排空窗口结束（drainAndShutdown 等的是这个）；
//   - 紧接着关连接池 = closeAll（生产的最后一步）；
//   - 之后用另一条连接读回 = 进程重启后的账本。
//
// 为了让「结算来不及」变成确定性事实，终态写被注入的包装器拖慢（真实扇出只有百毫秒量级，
// 靠时序碰运气会得到「有时红有时绿」的测试）。

// stormStreams 是并发断线的流数。
const stormStreams = 8

// stormSettleDelay 是注入的终态写延迟。
const stormSettleDelay = 400 * time.Millisecond

// slowTerminalWriter 只拖慢终态写，其余方法原样委托给真实实现。
type slowTerminalWriter struct {
	inner terminal.Writer
	delay time.Duration
}

func (w slowTerminalWriter) CreateMessageRequest(
	ctx context.Context,
	data store.CreateMessageRequestData,
) (store.MessageRequest, error) {
	return w.inner.CreateMessageRequest(ctx, data)
}

func (w slowTerminalWriter) UpdateDetailsIfUnfinalized(
	ctx context.Context,
	id int64,
	patch store.DetailsPatch,
) (bool, error) {
	time.Sleep(w.delay)
	return w.inner.UpdateDetailsIfUnfinalized(ctx, id, patch)
}

func (w slowTerminalWriter) UpdateWinnerCost(
	ctx context.Context,
	id int64,
	winnerCost string,
	costBreakdown []byte,
) error {
	return w.inner.UpdateWinnerCost(ctx, id, winnerCost, costBreakdown)
}

func (w slowTerminalWriter) FindModelPrice(ctx context.Context, modelName string) (*store.ModelPrice, error) {
	return w.inner.FindModelPrice(ctx, modelName)
}

func TestDisconnectStormKeepsTerminalRowsWhenProcessExits(t *testing.T) {
	if os.Getenv("CCH_TEST_DSN") == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	writePools := integrationStore(t)
	// 关掉写池之后用它读回结果：进程重启后账本是新连接读的。
	readPools := integrationStore(t)

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		_, _ = io.WriteString(w,
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"甲\"}}\n\n")
		flusher.Flush()
		// 断线期间上游仍在推流：这正是「客户端中断 + 结算延后」的现场。
		<-release
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	// 夹具用 readPools 建：本用例随后会主动关掉 writePools（退出序列的最后一步），
	// 而建夹具时的 t.Cleanup 要到用例正文之后才跑——若夹具挂在 writePools 上，回收
	// 就会静默失败，在共享库里留下一条指向已关闭假上游的深层僵尸行。
	provisioned := provision(t, readPools, upstream.URL, providerTypeFor("/v1/messages"))

	settler := terminal.New(slowTerminalWriter{
		inner: terminal.StoreWriter{Pools: writePools},
		delay: stormSettleDelay,
	}, terminal.Options{})
	assembly, err := NewStoreBacked(StoreOptions{
		Pools:   writePools,
		Logger:  logx.New(nil),
		Settler: settler,
	})
	if err != nil {
		t.Fatalf("数据面装配失败: %v", err)
	}

	var handlers sync.WaitGroup
	handlers.Add(stormStreams)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assembly.Handler.ServeHTTP(w, r)
		handlers.Done()
	}))
	defer server.Close()

	var clients sync.WaitGroup
	for index := 0; index < stormStreams; index++ {
		clients.Add(1)
		go func() {
			defer clients.Done()
			request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages",
				strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"你好"}]}`))
			if err != nil {
				return
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("x-api-key", provisioned.apiKey)
			response, err := server.Client().Do(request)
			if err != nil {
				return
			}
			// 读到首帧即断线：断线风暴的形状（客户端不等终态）。
			buffer := make([]byte, 256)
			_, _ = response.Body.Read(buffer)
			_ = response.Body.Close()
		}()
	}

	waitForGroup(t, &clients, "客户端全部断线", 5*time.Second)
	close(release)
	waitForGroup(t, &handlers, "排空（handler 全部返回）", 5*time.Second)

	// 退出序列的最后一步：关依赖。此处若仍有终态未落库，它们就永久丢失。
	if err := writePools.Close(); err != nil {
		t.Fatalf("关闭写池失败: %v", err)
	}

	total, unsettled := countRequestsByTerminal(t, readPools, provisioned.providerID)
	// 屏障缺失时这里既可能缺行（行由结算器开）也可能留下未终态的行，两者都是同一个静默丢失。
	if unsettled != 0 || total < stormStreams {
		t.Fatalf("关连接池时终态尚未落库：共 %d 行（应不少于 %d 行），其中 %d 行未终态",
			total, stormStreams, unsettled)
	}
}

// waitForGroup 等一组 goroutine 完成；超时即失败。
func waitForGroup(t *testing.T, group *sync.WaitGroup, what string, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("%s 未在 %v 内完成", what, timeout)
	}
}

// countRequestsByTerminal 返回该供应商的总行数与其中未落终态的行数。
func countRequestsByTerminal(t *testing.T, pools *store.Pools, providerID int64) (total int, unsettled int) {
	t.Helper()
	reader, err := pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	if err := reader.QueryRow(context.Background(), `
		SELECT count(*), count(*) FILTER (WHERE status_code IS NULL)
		FROM message_request WHERE provider_id = $1`, providerID,
	).Scan(&total, &unsettled); err != nil {
		t.Fatalf("统计请求日志失败: %v", err)
	}
	return total, unsettled
}
