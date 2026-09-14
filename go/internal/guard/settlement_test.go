package guard

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// TestAdaptersApplyInjectsRateLimitSeams 钉住限流缝隙经 AdapterOptions 注入：
// 本包不能反向依赖 limit（会成环），所以生产装配把实现当接口传进来，装配必须真的接上。
func TestAdaptersApplyInjectsRateLimitSeams(t *testing.T) {
	pools := guardIntegrationPools(t)
	limiter := &fakeLimiter{throttle: ThrottleDecision{Allowed: true}}

	adapters, err := NewAdapters(AdapterOptions{
		Pools:        pools,
		Logger:       quietLogger(),
		RateLimit:    limiter,
		AuthThrottle: limiter,
	})
	if err != nil {
		t.Fatalf("构造适配器失败: %v", err)
	}
	t.Cleanup(adapters.Close)

	deps := Deps{Logger: quietLogger()}
	adapters.Apply(&deps)
	if deps.RateLimit != limiter {
		t.Fatalf("请求级限流未接线: %v", deps.RateLimit)
	}
	if deps.AuthThrottle != limiter {
		t.Fatalf("认证节流未接线: %v", deps.AuthThrottle)
	}
}

// TestAdaptersApplyKeepsCallerRateLimitWhenNotInjected 钉住不被 nil 覆盖：
// 未注入实现时 Apply 不得把调用方自己接的限流器清掉。
func TestAdaptersApplyKeepsCallerRateLimitWhenNotInjected(t *testing.T) {
	pools := guardIntegrationPools(t)
	adapters, err := NewAdapters(AdapterOptions{Pools: pools, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("构造适配器失败: %v", err)
	}
	t.Cleanup(adapters.Close)

	limiter := &fakeLimiter{throttle: ThrottleDecision{Allowed: true}}
	deps := Deps{Logger: quietLogger(), RateLimit: limiter, AuthThrottle: limiter}
	adapters.Apply(&deps)
	if deps.RateLimit != limiter || deps.AuthThrottle != limiter {
		t.Fatalf("未注入时不应覆盖调用方的限流器: rate=%v throttle=%v", deps.RateLimit, deps.AuthThrottle)
	}
}

// TestGuardOpensRowThenTerminalSettlesIt 是「谁建行、谁更新」的端到端见证（真实库）。
//
// 走真实对话链到 messageContext 开行，再把行标识交给终态包结算：断言同一条请求
// 自始至终只有一行，且终态列被更新在那一行上。
func TestGuardOpensRowThenTerminalSettlesIt(t *testing.T) {
	pools := guardIntegrationPools(t)
	adapters := guardIntegrationAdapters(t, pools)
	ctx := context.Background()
	cleanupStaleRows(t, pools, ctx)

	apiKey, userID := seedIdentity(t, pools, ctx, nil, testGroupTag)
	request := assembledContext(t, apiKey, chatBody())

	response, err := runChainWithRequest(t, adapters, request)
	if err != nil {
		t.Fatalf("跑链失败: %v", err)
	}
	if response != nil {
		t.Fatalf("应通过全链，被 %d 拦下: %s", response.Status, string(response.Body))
	}

	rowID, ok := request.MessageRequestID()
	if !ok || rowID == 0 {
		t.Fatalf("守卫链开行后应把行标识交给上下文: id=%d ok=%v", rowID, ok)
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	var rowsBefore int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM message_request WHERE user_id = $1`, userID).Scan(&rowsBefore); err != nil {
		t.Fatalf("统计请求日志行失败: %v", err)
	}
	if rowsBefore != 1 {
		t.Fatalf("开行阶段应恰好一行，得到 %d 行", rowsBefore)
	}

	// 终态结算：上下文已有行标识，故不得再建行。
	settler := terminal.New(terminal.StoreWriter{Pools: pools}, terminal.Options{})
	durationMS := 12
	result, err := settler.SettleContext(ctx, request, terminal.Settlement{
		StatusCode: 200,
		DurationMS: &durationMS,
		Model:      strPtr("claude-sonnet-4-5"),
	}, nil)
	if err != nil {
		t.Fatalf("终态结算失败: %v", err)
	}
	if !result.Committed {
		t.Fatalf("终态未提交: %+v", result)
	}

	var rowsAfter int
	var statusCode *int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM message_request WHERE user_id = $1`, userID).Scan(&rowsAfter); err != nil {
		t.Fatalf("统计请求日志行失败: %v", err)
	}
	if rowsAfter != 1 {
		t.Fatalf("结算不得再建行：期望 1 行，得到 %d 行", rowsAfter)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status_code FROM message_request WHERE id = $1`, rowID).Scan(&statusCode); err != nil {
		t.Fatalf("读取终态失败: %v", err)
	}
	if statusCode == nil || *statusCode != 200 {
		t.Fatalf("终态未写在 guard 开的行上: status=%v", statusCode)
	}

	// 第二条请求（新的上下文）不受影响：行标识是按请求隔离的。
	second := assembledContext(t, apiKey, chatBody())
	if _, err := runChainWithRequest(t, adapters, second); err != nil {
		t.Fatalf("第二条请求跑链失败: %v", err)
	}
	secondID, ok := second.MessageRequestID()
	if !ok || secondID == rowID {
		t.Fatalf("两条请求应有各自的行标识: first=%d second=%d ok=%v", rowID, secondID, ok)
	}
}

// strPtr 取字符串指针（测试内联载荷用）。
func strPtr(value string) *string { return &value }
