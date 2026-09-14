package dataplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/health"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件是「熔断必须影响行为，而不只影响展示」的装配级钉子。
//
// 背景（这是它存在的唯一理由）：写侧 health.Writer 一直有装配，管理面也照常显示「已熔断」，
// 但 route.NewHealthReader 在生产代码里从未被构造，route.Selector 在 Health 为 nil 时一律放行。
// 于是账本、页面、Redis 键全对，只有「请求还是打过去」不对——这类缺口不会有人报错。
//
// 判据走真实装配路径：NewStoreBacked（**刻意不注入 RouteOptions.Health**，复刻生产现状）
// + 真 PG + 真 Redis + httptest 假上游。同一用例内含反向基线：未开闸时必须真到达上游，
// 故「把装配改回 options.RouteOptions.Health」会让本用例变红。

// TestIntegrationAssembledHandlerExcludesCircuitOpenProvider 断言开闸后请求不再打到该供应商。
func TestIntegrationAssembledHandlerExcludesCircuitOpenProvider(t *testing.T) {
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过熔断装配集成测试")
	}
	redisOptions, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败（值已隐去）: %v", err)
	}
	client := redis.NewClient(redisOptions)
	t.Cleanup(func() { _ = client.Close() })

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_it","type":"message","role":"assistant",`+
			`"model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	pools := integrationStore(t)
	provisioned := provision(t, pools, upstream.URL, "claude")

	assembly, err := NewStoreBacked(StoreOptions{Pools: pools, Redis: client, Logger: logx.New(nil)})
	if err != nil {
		t.Fatalf("数据面装配失败: %v", err)
	}
	server := httptest.NewServer(assembly.Handler)
	defer server.Close()

	send := func() (int, string) {
		body := `{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"你好"}]}`
		request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("x-api-key", provisioned.apiKey)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer func() { _ = response.Body.Close() }()
		payload, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("读响应失败: %v", err)
		}
		return response.StatusCode, string(payload)
	}

	// 反向基线：熔断未开时该供应商是唯一最靠前候选，请求必须真打到上游。
	// 缺了这条，下面「没打到上游」也可能只是因为夹具本身选不中它（假绿）。
	if status, _ := send(); status != http.StatusOK || upstreamHits.Load() != 1 {
		t.Fatalf("未开闸时请求应到达上游一次，实际 status=%d hits=%d", status, upstreamHits.Load())
	}

	// 用真写侧记账把该供应商开到熔断（读写两侧由同一个 Redis 见证）。
	writer := health.NewWriter(health.Options{Redis: client})
	for attempt := 1; attempt <= 10; attempt++ {
		if err := writer.RecordProviderFailure(context.Background(), provisioned.providerID,
			errors.New("integration: upstream 503")); err != nil {
			t.Fatalf("第 %d 次失败记账报错: %v", attempt, err)
		}
	}

	// 先确认开闸事实本身成立：否则「没打到上游」可能只是别的原因（例如夹具失效）。
	reader := resolveHealth(StoreOptions{Redis: client})
	open, reason := reader.ProviderOpen(context.Background(), provisioned.providerID)
	if !open {
		t.Fatalf("夹具供应商应处于开闸窗口内：open=%v reason=%v", open, reason)
	}

	status, payload := send()
	if hits := upstreamHits.Load(); hits != 1 {
		t.Fatalf("开闸后不得再打到该上游：命中数应从 1 保持不变，实际 %d（status=%d body=%s）",
			hits, status, payload)
	}
}
