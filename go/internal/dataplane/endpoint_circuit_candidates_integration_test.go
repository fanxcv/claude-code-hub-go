package dataplane

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住「端点级熔断必须拦住转发候选」这条契约。
//
// 为什么要有它：Go 的 EndpointOpen 早就实现了（route.HealthReader，复刻 Node 的
// isEndpointCircuitOpen），但装配侧**从未调用**它 —— 结果是「页面显示某端点已熔断、请求照打
// 该端点」。Node 的 getPreferredProviderEndpoints 在 ENABLE_ENDPOINT_CIRCUIT_BREAKER 打开时
// 会剔掉 circuitState==="open" 的端点（窗口已过期的按半开放行），本包此前没有对应步骤。
//
// 门控：未设置 CCH_TEST_DSN / CCH_TEST_REDIS_URL 时整组跳过（与同目录其余集成用例一致）。

// endpointCircuitFixture 是一个带厂与两个端点的供应商夹具。
type endpointCircuitFixture struct {
	providerID int64
	// endpoints 按插入顺序（sort_order 0、1），下标即排序位次。
	endpoints []fixtureEndpoint
}

type fixtureEndpoint struct {
	id  int64
	url string
}

// provisionEndpointFixture 插一个厂 + 两个启用态端点 + 引用该厂的供应商。
//
// 为什么必须真库：候选装配读的是 provider_endpoints 与 providers 的真实列（含 vendor 关联），
// 用假 Source 就测不到「端点从哪来」这一段。
func provisionEndpointFixture(t *testing.T, pools *store.Pools) endpointCircuitFixture {
	t.Helper()
	ctx := context.Background()

	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}

	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	var vendorID int64
	if err := writer.QueryRow(ctx,
		`INSERT INTO provider_vendors (website_domain) VALUES ($1) RETURNING id`,
		"endpoint-circuit-it-"+stamp+".example",
	).Scan(&vendorID); err != nil {
		t.Fatalf("插入测试厂失败: %v", err)
	}

	fixture := endpointCircuitFixture{}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = writer.Exec(cleanupCtx, `DELETE FROM providers WHERE provider_vendor_id = $1`, vendorID)
		// 端点随厂级联删除（provider_endpoints.vendor_id 是 ON DELETE CASCADE）。
		_, _ = writer.Exec(cleanupCtx, `DELETE FROM provider_vendors WHERE id = $1`, vendorID)
	})

	for index := 0; index < 2; index++ {
		url := "http://127.0.0.1:1/endpoint-" + strconv.Itoa(index)
		var endpointID int64
		if err := writer.QueryRow(ctx, `
			INSERT INTO provider_endpoints (vendor_id, provider_type, url, sort_order, is_enabled)
			VALUES ($1, 'openai-compatible', $2, $3, true)
			RETURNING id`, vendorID, url, index).Scan(&endpointID); err != nil {
			t.Fatalf("插入测试端点失败: %v", err)
		}
		fixture.endpoints = append(fixture.endpoints, fixtureEndpoint{id: endpointID, url: url})
	}

	if err := writer.QueryRow(ctx, `
		INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority, cost_multiplier, provider_vendor_id)
		VALUES ($1, $2, $3, 'openai-compatible', true, 1, 0, 1.0, $4)
		RETURNING id`,
		"endpoint-circuit-it-"+stamp, "http://127.0.0.1:1/legacy-fallback",
		"fake-endpoint-circuit-key", vendorID,
	).Scan(&fixture.providerID); err != nil {
		t.Fatalf("插入测试供应商失败: %v", err)
	}

	return fixture
}

// endpointCircuitRedis 建 Redis 客户端；未设置门控变量时跳过。
func endpointCircuitRedis(t *testing.T) *redis.Client {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过端点熔断候选集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败（值已隐去）: %v", err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// markEndpointOpen 按生产同源键形制写一条端点熔断状态。
func markEndpointOpen(t *testing.T, client *redis.Client, endpointID int64, openUntilMS int64) {
	t.Helper()
	key := route.EndpointStateKeyPrefix + strconv.FormatInt(endpointID, 10)
	if err := client.HSet(context.Background(), key, map[string]interface{}{
		"circuitState":     string(route.StateOpen),
		"circuitOpenUntil": strconv.FormatInt(openUntilMS, 10),
		"failureCount":     "3",
	}).Err(); err != nil {
		t.Fatalf("写端点熔断状态失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })
}

// candidateEndpoints 取候选的端点序列（Provider.Endpoints 是转发层实际会尝试的列表）。
func candidateEndpoints(t *testing.T, source *candidateSource, providerID int64) []forward.Endpoint {
	t.Helper()
	candidate, _, err := source.Candidate(context.Background(), pctx.ProviderSelection{
		ProviderID: providerID,
		Name:       "端点熔断夹具",
	}, SelectionFacts{})
	if err != nil {
		t.Fatalf("投影候选失败: %v", err)
	}
	return candidate.Provider.Endpoints
}

func endpointURLs(endpoints []forward.Endpoint) []string {
	urls := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		urls = append(urls, endpoint.URL)
	}
	return urls
}

// TestIntegrationEndpointCircuitOpenExcludedFromCandidates 是本次修复的验收。
//
// 四个断言分别对应四件必须成立的事：
//  1. 开关打开且窗口内 open -> 该端点**不得**进候选（修复点）；
//  2. 开关关闭 -> 照旧全部进候选（开关必须真被尊重，否则「开关无效」是另一处缺陷）；
//  3. 开关打开但窗口已过期 -> 放行（半开试探，与供应商级同源判定）；
//  4. 全部端点都被剔掉 -> 候选为空，由转发层按既有文档回退 Provider.URL
//     （Node 非严格模式的语义，本包不得静默改成「无可用端点」）。
func TestIntegrationEndpointCircuitOpenExcludedFromCandidates(t *testing.T) {
	pools := integrationStore(t)
	client := endpointCircuitRedis(t)
	fixture := provisionEndpointFixture(t, pools)

	frozen := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	openUntilMS := frozen.Add(5 * time.Minute).UnixMilli()
	markEndpointOpen(t, client, fixture.endpoints[0].id, openUntilMS)

	build := func(breakerEnabled bool, now time.Time) *candidateSource {
		reader := route.NewHealthReader(route.HealthOptions{
			Redis:                         client,
			EndpointCircuitBreakerEnabled: breakerEnabled,
			Now:                           func() time.Time { return now },
		})
		return newCandidateSource(pools, nil, reader, logx.New(nil))
	}

	// 1. 开关打开 + 窗口内 open：第一个端点必须被剔掉。
	open := candidateEndpoints(t, build(true, frozen), fixture.providerID)
	urls := endpointURLs(open)
	if len(urls) != 1 || urls[0] != fixture.endpoints[1].url {
		t.Fatalf("开关打开时窗口内 open 的端点应被剔除，实际候选 %v（期望只有 %s）",
			urls, fixture.endpoints[1].url)
	}
	if open[0].ID != fixture.endpoints[1].id {
		t.Fatalf("候选端点 id 应为 %d，收到 %d", fixture.endpoints[1].id, open[0].ID)
	}

	// 2. 开关关闭：两个端点都应进候选（否则是「开关无效」的第二处缺陷）。
	if got := endpointURLs(candidateEndpoints(t, build(false, frozen), fixture.providerID)); len(got) != 2 {
		t.Fatalf("开关关闭时不应剔除任何端点，实际候选 %v", got)
	}

	// 3. 窗口已过期：按半开放行（与 ProviderOpen 的过期判定同源）。
	afterWindow := frozen.Add(time.Hour)
	if got := endpointURLs(candidateEndpoints(t, build(true, afterWindow), fixture.providerID)); len(got) != 2 {
		t.Fatalf("窗口已过期应放行试探，实际候选 %v", got)
	}

	// 4. 全部端点熔断：候选为空（转发层据 Provider.Endpoints 为空这一事实回退 Provider.URL）。
	markEndpointOpen(t, client, fixture.endpoints[1].id, openUntilMS)
	empty := candidateEndpoints(t, build(true, frozen), fixture.providerID)
	if len(empty) != 0 {
		t.Fatalf("全部端点被剔除时候选端点应为空，实际 %v", endpointURLs(empty))
	}
}
