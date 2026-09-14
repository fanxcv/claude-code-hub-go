package forward

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/health"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// TestIntegrationHedgeSuccessAdvancesCircuitRecovery 是本轮 Q1 的决定性验收：
// **竞速流量**的成功必须能推进熔断状态，使「open 且窗口已过期」的供应商经由 half-open 回到 closed。
//
// 为什么必须用真 Redis：要证明的不是「调了哪个函数」，而是**写完之后状态真的动了**——
// 改前的缺口正是「竞速路径从不记账」，桩只能证明调用，证明不了状态迁移。
//
// 为什么要在 forward 包里接 health.Writer：装配层（dataplane/assemble.go）就是这么接的
// （deps.RecordSuccess -> healthWriter.RecordProviderSuccess）。本用例复刻那条接线，
// 于是「竞速胜者 → 记账 → 状态推进」这条链在真 Redis 上被端到端走通。
//
// 关于同步：记账发生在**胜者 attempt 的 goroutine**里，而 ForwardStreamHedge 在胜者提交时
// 就可能返回——断言若不等待写入，会时好时坏（本用例第一版就踩了这个坑）。
// 故每次竞速后都等回调信号再断言，而不是赌时序。
func TestIntegrationHedgeSuccessAdvancesCircuitRecovery(t *testing.T) {
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败: %v", err)
	}
	client := redis.NewClient(options)
	defer client.Close()

	ctx := context.Background()
	writer := health.NewWriter(health.Options{Redis: client, EndpointCircuitBreakerEnabled: true})

	// 被观察的供应商 = 竞速的**备选**（它先吐内容，故确定性地成为胜者）。
	const targetID = int64(910000001)
	stateKey := route.ProviderStateKeyPrefix + strconv.FormatInt(targetID, 10)
	configKey := route.ProviderConfigKeyPrefix + strconv.FormatInt(targetID, 10)
	t.Cleanup(func() {
		client.Del(ctx, stateKey, configKey)
	})

	// 夹具：与生产 156/145 同款——open 且窗口已过期、failureCount 非零、半开计数为 0。
	// 不写 config 键：阈值走默认（halfOpenSuccessThreshold=2），与生产默认一致。
	expired := time.Now().Add(-time.Minute).UnixMilli()
	if err := client.HSet(ctx, stateKey, map[string]any{
		"failureCount":         "11",
		"lastFailureTime":      strconv.FormatInt(time.Now().Add(-time.Hour).UnixMilli(), 10),
		"circuitState":         "open",
		"circuitOpenUntil":     strconv.FormatInt(expired, 10),
		"halfOpenSuccessCount": "0",
	}).Err(); err != nil {
		t.Fatalf("夹具写入失败: %v", err)
	}

	// 第一轮竞速：备选（被熔断那家）胜出 ⇒ 它的成功应把过期 open 推进为 half-open。
	runRaceToTarget(t, targetID, writer)
	state := client.HGetAll(ctx, stateKey).Val()
	if state["circuitState"] != "half-open" {
		t.Fatalf("竞速胜者的成功应把过期 open 推进为 half-open，实际 %q（改前竞速路径不记账，永远停在 open）",
			state["circuitState"])
	}
	if state["halfOpenSuccessCount"] != "1" {
		t.Fatalf("halfOpenSuccessCount 应为 1，实际 %q", state["halfOpenSuccessCount"])
	}
	if state["failureCount"] != "11" {
		t.Fatalf("迁移不得重置 failureCount（Node 也不重置），实际 %q", state["failureCount"])
	}

	// 第二轮竞速：达默认半开阈值 2 ⇒ 归 closed（熔断真的恢复了）。
	runRaceToTarget(t, targetID, writer)
	state = client.HGetAll(ctx, stateKey).Val()
	if state["circuitState"] != "closed" {
		t.Fatalf("半开成功达阈值后应归 closed，实际 %q（熔断必须能恢复）", state["circuitState"])
	}
	if state["failureCount"] != "0" || state["circuitOpenUntil"] != "" {
		t.Fatalf("归闭应清空 failureCount 与 circuitOpenUntil，实际 %+v", state)
	}

	// 反向：归闭后连续失败达阈值仍应重新开闸，且窗口在未来（证明这条链不是单向的）。
	for i := int64(0); i < route.DefaultFailureThreshold; i++ {
		if err := writer.RecordProviderFailure(ctx, targetID, errors.New("boom")); err != nil {
			t.Fatalf("记失败失败: %v", err)
		}
	}
	state = client.HGetAll(ctx, stateKey).Val()
	if state["circuitState"] != "open" {
		t.Fatalf("连续失败达阈值应重新开闸，实际 %q", state["circuitState"])
	}
	openUntil, err := strconv.ParseInt(state["circuitOpenUntil"], 10, 64)
	if err != nil {
		t.Fatalf("circuitOpenUntil 不是整数: %q", state["circuitOpenUntil"])
	}
	if openUntil <= time.Now().UnixMilli() {
		t.Fatalf("重新开闸的窗口应在未来，实际 %d", openUntil)
	}
}

// TestIntegrationHedgeLoserDoesNotAdvanceCircuit 钉住另一半：**输家**（被取消/引流的那家）
// 的成功不得推进它自己的熔断状态——否则「取消」会被当成「健康」，多路竞速足以把半开计数刷满。
func TestIntegrationHedgeLoserDoesNotAdvanceCircuit(t *testing.T) {
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败: %v", err)
	}
	client := redis.NewClient(options)
	defer client.Close()

	ctx := context.Background()
	writer := health.NewWriter(health.Options{Redis: client, EndpointCircuitBreakerEnabled: true})

	// 被观察者 = **首发**（它被挂起、随后成为输家）。
	const loserID = int64(910000011)
	stateKey := route.ProviderStateKeyPrefix + strconv.FormatInt(loserID, 10)
	t.Cleanup(func() { client.Del(ctx, stateKey) })

	expired := time.Now().Add(-time.Minute).UnixMilli()
	if err := client.HSet(ctx, stateKey, map[string]any{
		"failureCount":         "11",
		"circuitState":         "open",
		"circuitOpenUntil":     strconv.FormatInt(expired, 10),
		"halfOpenSuccessCount": "0",
	}).Err(); err != nil {
		t.Fatalf("夹具写入失败: %v", err)
	}

	// 首发挂起（必成输家），备选吐内容（胜者）。
	blocked := make(chan struct{})
	initial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-blocked
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(func() {
		close(blocked)
		initial.Close()
	})
	winner := streamServer(t, "text/event-stream", claudeStreamChunks(), true)

	harness := newHedgeHarness(t)
	harness.setSelect([]*Candidate{sseCandidate(910000012, "胜者供应商", winner.URL, 0)})
	recorded := make(chan int64, 8)
	harness.deps.RecordSuccess = func(callCtx context.Context, id int64, _ int64) {
		if err := writer.RecordProviderSuccess(callCtx, id); err != nil {
			t.Errorf("记账失败: %v", err)
		}
		recorded <- id
	}

	result, err := runHedgeWithThreshold(t, harness, sseCandidate(loserID, "输家供应商", initial.URL, 1))
	if err != nil {
		t.Fatalf("ForwardStreamHedge 失败: %v", err)
	}
	if result.Stream != nil {
		_, _ = consumeStream(t, result.Stream)
	}
	waitForRecord(t, recorded)

	state := client.HGetAll(ctx, stateKey).Val()
	if state["circuitState"] != "open" || state["halfOpenSuccessCount"] != "0" {
		t.Fatalf("输家不得推进自己的熔断状态，实际 %+v", state)
	}
}

// runRaceToTarget 让 targetID 作为竞速**备选**确定性地胜出，并等它的记账落盘后再返回。
//
// 确定性来自既有用例的同一模式（TestHedgeFirstValidContentWinsAndLoserCancelled）：
// 首发挂起不吐字节、备选立刻吐内容 ⇒ 备选必为胜者。
func runRaceToTarget(t *testing.T, targetID int64, writer *health.Writer) {
	t.Helper()

	blocked := make(chan struct{})
	initial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-blocked
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(func() {
		close(blocked)
		initial.Close()
	})
	winner := streamServer(t, "text/event-stream", claudeStreamChunks(), true)

	harness := newHedgeHarness(t)
	harness.setSelect([]*Candidate{sseCandidate(targetID, "目标供应商", winner.URL, 0)})
	recorded := make(chan int64, 8)
	harness.deps.RecordSuccess = func(ctx context.Context, id int64, _ int64) {
		if err := writer.RecordProviderSuccess(ctx, id); err != nil {
			t.Errorf("记账失败: %v", err)
		}
		recorded <- id
	}

	result, err := runHedgeWithThreshold(t, harness, sseCandidate(targetID+1000, "挂起的首发", initial.URL, 1))
	if err != nil {
		t.Fatalf("ForwardStreamHedge 失败: %v", err)
	}
	if result.Provider.ID != targetID {
		t.Fatalf("胜者应为目标供应商 %d，实际 %d", targetID, result.Provider.ID)
	}
	if result.Stream != nil {
		_, _ = consumeStream(t, result.Stream)
	}
	waitForRecord(t, recorded)
}

// waitForRecord 等待胜者的记账回调落盘（记账在胜者 goroutine 里异步发生）。
func waitForRecord(t *testing.T, recorded <-chan int64) {
	t.Helper()
	select {
	case <-recorded:
	case <-time.After(5 * time.Second):
		t.Fatalf("5s 内未收到胜者记账回调（竞速路径漏记成功？）")
	}
}
