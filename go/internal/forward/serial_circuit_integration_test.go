package forward

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/health"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/redis/go-redis/v9"
)

// 本文件补上 Q1 的另一半：**串行**路径的成功记账（非流式 Forward 与流式 ForwardStream）。
//
// 为什么必须单独钉：竞速路径（hedge.go）原先只记失败，导致「开闸后永远无人推进半开计数」
// → 熔断回不到 closed（生产现象：一批供应商永远显示「熔断恢复中」）。那一半已由
// hedge_circuit_integration_test.go 钉住；但**串行**是另一条独立代码路径
// （Forward/ForwardStream → forwardLoop），两条路各自可能漏记，必须各自有真 Redis 证据。
//
// 为什么用真 Redis：要证明的不是「调了哪个函数」，而是**写完之后状态真的动了**。
// 桩只能证明调用，证明不了键形制（字段名写错在桩上照样绿）。
//
// 为什么在 forward 包里接 health.Writer：装配层（dataplane/assemble.go 的 recordSuccess）
// 就是这么接的（deps.RecordSuccess -> healthWriter.RecordProviderSuccess）——本用例复刻那条接线，
// 于是「串行成功 → 记账 → 状态推进」这条链在真 Redis 上被端到端走通。
func TestIntegrationSerialSuccessAdvancesCircuitRecovery(t *testing.T) {
	t.Run("非流式", func(t *testing.T) {
		client, writer := circuitIntegrationRedis(t)
		const targetID = int64(920000001)
		key := seedExpiredOpenProvider(t, client, targetID)

		server := fakeServer(t, 200, `{"id":"msg_1","content":[{"type":"text","text":"pong"}]}`)
		recorded := newRecordingSuccess(t, writer)
		deps := Deps{
			Dial:          newTestDial(t),
			Facts:         newTestFacts(),
			Limits:        Limits{RetryDelay: time.Millisecond},
			RecordSuccess: recorded.hook,
		}

		for round, want := range []struct{ state, count string }{
			{"half-open", "1"},
			{"closed", "0"},
		} {
			candidate := newTestCandidate(targetID, "被熔断的供应商", server.URL, 1)
			result, err := Forward(context.Background(), nil, candidate, deps)
			if err != nil {
				t.Fatalf("第 %d 轮 Forward 失败: %v", round+1, err)
			}
			if result.StatusCode != 200 {
				t.Fatalf("第 %d 轮状态码应为 200，实际 %d", round+1, result.StatusCode)
			}
			recorded.waitFor(t, round+1)
			assertProviderCircuit(t, client, key, want.state, want.count)
		}
	})

	t.Run("流式", func(t *testing.T) {
		client, writer := circuitIntegrationRedis(t)
		const targetID = int64(920000002)
		key := seedExpiredOpenProvider(t, client, targetID)

		server := streamServer(t, "text/event-stream", claudeStreamChunks(), true)
		recorded := newRecordingSuccess(t, writer)
		deps := Deps{
			Dial:          newTestDial(t),
			Facts:         newStreamFacts(),
			Limits:        Limits{RetryDelay: time.Millisecond},
			RecordSuccess: recorded.hook,
		}

		for round, want := range []struct{ state, count string }{
			{"half-open", "1"},
			{"closed", "0"},
		} {
			candidate := newTestCandidate(targetID, "被熔断的供应商", server.URL, 1)
			result, err := ForwardStream(context.Background(), newTestPctx(t), candidate, deps,
				StreamOptions{Format: convert.FormatClaude})
			if err != nil {
				t.Fatalf("第 %d 轮 ForwardStream 失败: %v", round+1, err)
			}
			if result.Stream != nil {
				if _, outcome := consumeStream(t, result.Stream); outcome.Kind != TerminalCompleted {
					t.Fatalf("第 %d 轮流未正常收尾: %+v", round+1, outcome)
				}
			}
			recorded.waitFor(t, round+1)
			assertProviderCircuit(t, client, key, want.state, want.count)
		}
	})
}

// TestIntegrationSerialFailureReopensCircuitWithFreshWindow 钉住反向：串行路径的失败也要
// 以**新的**窗口重新开闸。缺了这条，「恢复」就可能是单向的假象——归闭后失败却不重开，
// 熔断等于失效。
func TestIntegrationSerialFailureReopensCircuitWithFreshWindow(t *testing.T) {
	client, writer := circuitIntegrationRedis(t)
	const targetID = int64(920000003)
	key := seedExpiredOpenProvider(t, client, targetID)

	server := fakeServer(t, 200, `{"id":"msg_1","content":[{"type":"text","text":"pong"}]}`)
	recorded := newRecordingSuccess(t, writer)
	deps := Deps{
		Dial:          newTestDial(t),
		Facts:         newTestFacts(),
		Limits:        Limits{RetryDelay: time.Millisecond},
		RecordSuccess: recorded.hook,
	}

	// 先走完恢复周期，免得「重开」被误认成「从未归闭」。
	for round := 1; round <= 2; round++ {
		if _, err := Forward(context.Background(), nil,
			newTestCandidate(targetID, "被熔断的供应商", server.URL, 1), deps); err != nil {
			t.Fatalf("第 %d 轮 Forward 失败: %v", round, err)
		}
		recorded.waitFor(t, round)
	}
	assertProviderCircuit(t, client, key, "closed", "0")

	for i := int64(0); i < route.DefaultFailureThreshold; i++ {
		if err := writer.RecordProviderFailure(context.Background(), targetID, nil); err != nil {
			t.Fatalf("记失败失败: %v", err)
		}
	}
	state := client.HGetAll(context.Background(), key).Val()
	if state["circuitState"] != "open" {
		t.Fatalf("归闭后连续失败达阈值应重新开闸，实际 %q", state["circuitState"])
	}
	openUntil, err := strconv.ParseInt(state["circuitOpenUntil"], 10, 64)
	if err != nil {
		t.Fatalf("circuitOpenUntil 不是整数: %q", state["circuitOpenUntil"])
	}
	if openUntil <= time.Now().UnixMilli() {
		t.Fatalf("重新开闸的窗口应在未来，实际 %d", openUntil)
	}
}

// circuitIntegrationRedis 建真 Redis 客户端与熔断写入器；未设 CCH_TEST_REDIS_URL 时跳过。
func circuitIntegrationRedis(t *testing.T) (redis.UniversalClient, *health.Writer) {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败: %v", err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	return client, health.NewWriter(health.Options{Redis: client, EndpointCircuitBreakerEnabled: true})
}

// seedExpiredOpenProvider 写入「与生产 156/145 同款」的夹具：open 且窗口已过期、计数非零、半开计数 0。
func seedExpiredOpenProvider(t *testing.T, client redis.UniversalClient, providerID int64) string {
	t.Helper()
	ctx := context.Background()
	key := route.ProviderStateKeyPrefix + strconv.FormatInt(providerID, 10)
	configKey := route.ProviderConfigKeyPrefix + strconv.FormatInt(providerID, 10)
	t.Cleanup(func() { client.Del(context.Background(), key, configKey) })

	// 不写 config 键：阈值走默认（halfOpenSuccessThreshold=2），与生产默认一致。
	if err := client.HSet(ctx, key, map[string]any{
		"failureCount":         "11",
		"lastFailureTime":      strconv.FormatInt(time.Now().Add(-time.Hour).UnixMilli(), 10),
		"circuitState":         "open",
		"circuitOpenUntil":     strconv.FormatInt(time.Now().Add(-time.Minute).UnixMilli(), 10),
		"halfOpenSuccessCount": "0",
	}).Err(); err != nil {
		t.Fatalf("夹具写入失败: %v", err)
	}
	return key
}

// assertProviderCircuit 断言熔断状态与半开计数两个字段（分开断言是为了失败时报出是哪一项）。
func assertProviderCircuit(t *testing.T, client redis.UniversalClient, key, wantState, wantCount string) {
	t.Helper()
	state := client.HGetAll(context.Background(), key).Val()
	if state["circuitState"] != wantState {
		t.Fatalf("circuitState 期望 %q，实际 %q（串行成功的记账若漏了，熔断永远回不到 closed）",
			wantState, state["circuitState"])
	}
	if state["halfOpenSuccessCount"] != wantCount {
		t.Fatalf("halfOpenSuccessCount 期望 %q，实际 %q", wantCount, state["halfOpenSuccessCount"])
	}
}

// recordingSuccess 把 RecordSuccess 接到真写入器上，并记下调用次数以便等待落盘。
//
// 串行路径的记账是同步的（在 forwardLoop 里调完才返回），但仍按次数等待而不是赌时序。
type recordingSuccess struct {
	writer *health.Writer
	count  chan int
	errs   chan error
}

func newRecordingSuccess(t *testing.T, writer *health.Writer) *recordingSuccess {
	t.Helper()
	recorder := &recordingSuccess{writer: writer, count: make(chan int, 16), errs: make(chan error, 16)}
	t.Cleanup(func() {
		select {
		case err := <-recorder.errs:
			t.Errorf("熔断记账失败: %v", err)
		default:
		}
	})
	return recorder
}

func (r *recordingSuccess) hook(ctx context.Context, providerID int64, _ int64) {
	if err := r.writer.RecordProviderSuccess(ctx, providerID); err != nil {
		r.errs <- err
		return
	}
	r.count <- 1
}

func (r *recordingSuccess) waitFor(t *testing.T, round int) {
	t.Helper()
	select {
	case <-r.errs:
		t.Fatalf("第 %d 轮记账失败", round)
	case <-r.count:
	case <-time.After(5 * time.Second):
		t.Fatalf("第 %d 轮 5s 内未记账（串行路径漏记成功？）", round)
	}
}
