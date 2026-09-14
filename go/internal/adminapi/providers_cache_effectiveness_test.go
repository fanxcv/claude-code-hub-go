package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 `GET /providers/cache-effectiveness` 的真实 PG 集成测试。
//
// 用例对准 Node 的语义边界（repository/provider-cache-effectiveness.ts:27-45 与
// schemas/provider-cache-effectiveness.ts:5-12）：
//   - 排序 `window_start DESC, id DESC`；
//   - limit 缺席默认 50、clamp 到 1..200；providerId 缺席不过滤；
//   - 响应是 `{ items: [...] }` 信封（不是裸数组）；
//   - 查询校验按 Zod：`providerId=` 落 `too_small`（空串经 coerce 变 0），`limit=201` 落 `too_big`。
//
// 夹具纪律：`model` 列带唯一前缀，按前缀清理；provider_id 用合成值（该列无外键）。

type cacheEffectivenessFixture struct {
	prefix     string
	model      string
	providerID int64
}

// seedCacheEffectiveness 插入 3 行：两行同 provider 不同窗口起点、一行另一 provider。
func seedCacheEffectiveness(t *testing.T, pools *store.Pools) *cacheEffectivenessFixture {
	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("go-pce-%d", time.Now().UnixNano())
	providerID := time.Now().UnixNano() % 1_000_000_000
	otherProviderID := providerID + 1
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}

	insert := func(owner int64, model string, windowStart string, effectivenessBp int) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO provider_cache_effectiveness (
				provider_id, model, cache_ttl_bucket, window_start, window_end,
				sample_count, eligible_count, theoretical_cache_tokens, observed_cache_read_tokens,
				raw_effectiveness_bp, confidence_bp, effectiveness_bp
			) VALUES ($1, $2, '5m', $3::timestamptz, ($3::timestamptz + interval '5 minutes'),
				10, 8, 1000, 750, $4, 9000, $4)`,
			owner, model, windowStart, effectivenessBp,
		); err != nil {
			t.Fatalf("建缓存效果夹具失败: %v", err)
		}
	}

	insert(providerID, prefix+"-old", "2026-01-01T00:00:00Z", 5000)
	insert(providerID, prefix+"-new", "2026-02-01T00:00:00Z", 6000)
	insert(otherProviderID, prefix+"-other", "2026-03-01T00:00:00Z", 7000)

	t.Cleanup(func() {
		cleanup, err := store.Open(context.Background(), store.Options{
			DSN:      os.Getenv("CCH_TEST_DSN"),
			Budget:   config.SplitPoolBudget(6),
			Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		})
		if err != nil {
			t.Logf("清理池建立失败（跳过清理）: %v", err)
			return
		}
		defer func() { _ = cleanup.Close() }()
		writer, err := cleanup.Writer()
		if err != nil {
			return
		}
		_, _ = writer.Exec(context.Background(), `DELETE FROM provider_cache_effectiveness WHERE model LIKE $1`, prefix+"%")
	})

	return &cacheEffectivenessFixture{prefix: prefix, model: prefix + "-new", providerID: providerID}
}

type cacheEffectivenessRow struct {
	ID             int64   `json:"id"`
	ProviderID     int64   `json:"providerId"`
	Model          string  `json:"model"`
	WindowStart    string  `json:"windowStart"`
	SampleCount    int     `json:"sampleCount"`
	EffectivenessB int     `json:"effectivenessBp"`
	CreatedAt      *string `json:"createdAt"`
}

// TestProviderCacheEffectivenessFilterOrderAndEnvelope 覆盖过滤、排序与信封形状。
func TestProviderCacheEffectivenessFilterOrderAndEnvelope(t *testing.T) {
	pools := testPools(t)
	fixture := seedCacheEffectiveness(t, pools)
	router := providersRouter(t, pools, &Deps{})

	recorder := providerRequest(t, router, http.MethodGet,
		fmt.Sprintf("/providers/cache-effectiveness?providerId=%d", fixture.providerID), "", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d（正文 %s）", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Items []cacheEffectivenessRow `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 {items:[...]}: %v（正文 %s）", err, recorder.Body.String())
	}
	if len(body.Items) != 2 {
		t.Fatalf("providerId 过滤后应有 2 行，实际 %d（%+v）", len(body.Items), body.Items)
	}
	// 排序：window_start DESC → 2 月那行在前。
	if body.Items[0].Model != fixture.model {
		t.Fatalf("首行应为最新窗口 %s，实际 %s", fixture.model, body.Items[0].Model)
	}
	// 字段值抽样：窗口时间与 ISO 毫秒格式（Node 的 toISOString 形状）。
	if body.Items[0].WindowStart != "2026-02-01T00:00:00.000Z" {
		t.Fatalf("windowStart 形状应为 ISO 毫秒 UTC，实际 %q", body.Items[0].WindowStart)
	}
	if body.Items[0].SampleCount != 10 || body.Items[0].EffectivenessB != 6000 {
		t.Fatalf("字段值不符：%+v", body.Items[0])
	}
}

// TestProviderCacheEffectivenessLimitClampAndValidation 覆盖 limit 与 Zod 校验语义。
func TestProviderCacheEffectivenessLimitClampAndValidation(t *testing.T) {
	pools := testPools(t)
	fixture := seedCacheEffectiveness(t, pools)
	router := providersRouter(t, pools, &Deps{})

	// limit=1 时只回 1 行。
	recorder := providerRequest(t, router, http.MethodGet,
		fmt.Sprintf("/providers/cache-effectiveness?providerId=%d&limit=1", fixture.providerID), "", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("limit=1 应 200，收到 %d", recorder.Code)
	}
	var body struct {
		Items []cacheEffectivenessRow `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("limit=1 应回 1 行，实际 %d", len(body.Items))
	}

	cases := []struct {
		query string
		code  string
		path  string
	}{
		{"providerId=0", "too_small", "providerId"},
		{"providerId=", "too_small", "providerId"},
		{"limit=0", "too_small", "limit"},
		{"limit=201", "too_big", "limit"},
		{"limit=abc", "invalid_type", "limit"},
	}
	for _, item := range cases {
		recorder := providerRequest(t, router, http.MethodGet, "/providers/cache-effectiveness?"+item.query, "", "", false)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，收到 %d（正文 %s）", item.query, recorder.Code, recorder.Body.String())
		}
		var problem struct {
			InvalidParams []struct {
				Path []any  `json:"path"`
				Code string `json:"code"`
			} `json:"invalidParams"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
			t.Fatalf("%s 的 400 正文解析失败: %v（正文 %s）", item.query, err, recorder.Body.String())
		}
		if len(problem.InvalidParams) == 0 {
			t.Fatalf("%s 的 400 应带 invalidParams，实际 %s", item.query, recorder.Body.String())
		}
		got := problem.InvalidParams[0]
		if got.Code != item.code {
			t.Fatalf("%s 的 code 应为 %s，实际 %s", item.query, item.code, got.Code)
		}
		if len(got.Path) == 0 || fmt.Sprint(got.Path[0]) != item.path {
			t.Fatalf("%s 的 path 应为 %s，实际 %v", item.query, item.path, got.Path)
		}
	}
}
