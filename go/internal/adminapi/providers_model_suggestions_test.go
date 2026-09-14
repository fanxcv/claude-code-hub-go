package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 `GET /providers/model-suggestions` 的真实 PG 集成测试。
//
// 用例设计对准 Node 的语义边界（src/actions/providers.ts:5677-5717）：
//   - 只收 **enabled** 供应商（disabled 同组也不贡献）；
//   - 只收分组匹配的供应商（默认分组为 "default"）；
//   - 只收 `matchType=exact` 的规则（prefix/regex 等不贡献）；字符串数组项视为 exact；
//   - 跨供应商**去重**、结果**升序**、空集返回 `[]`（不是 null）。
//
// 夹具纪律与 providers_test.go 同法：名称带唯一前缀，按前缀清理。

// msFixture 是一次测试自有的供应商集合。
type msFixture struct {
	prefix string
	group  string
	models []string
}

// seedModelSuggestions 插入 5 个供应商，覆盖上表全部边界。
//
//	ms-match-exact ：启用 + 组匹配 + allowedModels=[exact a, prefix b, "c"]  → a、c
//	ms-match-dup   ：启用 + 组匹配 + allowedModels=[exact a]                  → a（去重）
//	ms-other-group ：启用 + 组不匹配 + allowedModels=[exact z]
//	ms-disabled    ：**禁用** + 组匹配 + allowedModels=[exact d]
//	ms-no-models   ：启用 + 组匹配 + allowedModels=null
func seedModelSuggestions(t *testing.T, pools *store.Pools) *msFixture {
	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("go-pvd-ms-%d", time.Now().UnixNano())
	group := prefix + "-g"
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}

	var vendorID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_vendors (website_domain, display_name) VALUES ($1, $2) RETURNING id`,
		prefix+".invalid", prefix,
	).Scan(&vendorID); err != nil {
		t.Fatalf("建厂商夹具失败: %v", err)
	}

	insert := func(name, groupTag string, enabled bool, allowedModels string) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO providers (
				name, url, key, provider_vendor_id, is_enabled, weight, priority,
				cost_multiplier, group_tag, provider_type, allowed_models
			) VALUES ($1, $2, $3, $4, $5, 1, 0, 1, $6, 'claude', $7::jsonb)`,
			name, "https://"+prefix+".invalid/anthropic", "sk-model-suggestions-fixture",
			vendorID, enabled, groupTag, allowedModels,
		); err != nil {
			t.Fatalf("建供应商夹具失败（%s）: %v", name, err)
		}
	}

	insert(prefix+"-match-exact", group, true,
		`[{"matchType":"exact","pattern":"a"},{"matchType":"prefix","pattern":"b"},"c"]`)
	insert(prefix+"-match-dup", group, true, `[{"matchType":"exact","pattern":"a"}]`)
	insert(prefix+"-other-group", prefix+"-other", true, `[{"matchType":"exact","pattern":"z"}]`)
	insert(prefix+"-disabled", group, false, `[{"matchType":"exact","pattern":"d"}]`)
	insert(prefix+"-no-models", group, true, `null`)

	fixture := &msFixture{prefix: prefix, group: group, models: []string{"a", "c"}}
	t.Cleanup(func() {
		// 清理池独立于 pools（pools 的 Cleanup 晚于本函数才跑）。
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
		_, _ = writer.Exec(context.Background(), `DELETE FROM providers WHERE name LIKE $1`, prefix+"%")
		_, _ = writer.Exec(context.Background(), `DELETE FROM provider_vendors WHERE website_domain = $1`, prefix+".invalid")
	})
	return fixture
}

// modelSuggestionsBody 取一次响应并解回 string[]。
func modelSuggestionsBody(t *testing.T, router *Router, target string) (int, []string) {
	t.Helper()
	recorder := providerRequest(t, router, http.MethodGet, target, "", "", false)
	var models []string
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &models); err != nil {
			t.Fatalf("响应正文不是 string[]: %v（正文 %s）", err, recorder.Body.String())
		}
	}
	return recorder.Code, models
}

// containsModels 断言 models 里同时含期望项（列表类接口会带上库中其他数据，故只判包含）。
func containsModels(t *testing.T, got []string, want []string) {
	t.Helper()
	index := make(map[string]struct{}, len(got))
	for _, model := range got {
		index[model] = struct{}{}
	}
	for _, model := range want {
		if _, ok := index[model]; !ok {
			t.Fatalf("期望含模型 %q，实际 %v", model, got)
		}
	}
}

func notContainsModels(t *testing.T, got []string, blocked []string) {
	t.Helper()
	for _, model := range blocked {
		for _, item := range got {
			if item == model {
				t.Fatalf("不应含模型 %q，实际 %v", model, got)
			}
		}
	}
}

// TestProviderModelSuggestionsFiltersAndDedupes 覆盖过滤、去重与升序。
func TestProviderModelSuggestionsFiltersAndDedupes(t *testing.T) {
	pools := testPools(t)
	fixture := seedModelSuggestions(t, pools)
	router := providersRouter(t, pools, &Deps{})

	code, models := modelSuggestionsBody(t, router, "/providers/model-suggestions?providerGroup="+fixture.group)
	if code != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d", code)
	}
	containsModels(t, models, fixture.models)
	// 组不匹配、被禁用、无规则的三个供应商都不贡献。
	notContainsModels(t, models, []string{"z", "d", "b"})
	// 升序：本次数据里 a/c 的相对顺序必须成立（列表含其他数据，故只判相对）。
	if indexOf(models, "a") > indexOf(models, "c") {
		t.Fatalf("结果未升序: %v", models)
	}
}

// TestProviderModelSuggestionsGroupFallbackAndWhitespace 覆盖默认分组与空白串的分歧。
//
// Node 的 `providerGroup ? … : ["default"]` 只判假值：缺失/空串 → default；
// **空白串是真值** → parseGroupString 切出空集 → 结果为空。两者行为不同，必须分别钉住。
func TestProviderModelSuggestionsGroupFallbackAndWhitespace(t *testing.T) {
	pools := testPools(t)
	fixture := seedModelSuggestions(t, pools)
	router := providersRouter(t, pools, &Deps{})

	// 夹具供应商的组是自有前缀，默认分组（"default"）匹配不到它。
	code, models := modelSuggestionsBody(t, router, "/providers/model-suggestions")
	if code != http.StatusOK {
		t.Fatalf("缺参时状态码应为 200，收到 %d", code)
	}
	notContainsModels(t, models, fixture.models)

	code, models = modelSuggestionsBody(t, router, "/providers/model-suggestions?providerGroup=")
	if code != http.StatusOK {
		t.Fatalf("空串时状态码应为 200，收到 %d", code)
	}
	notContainsModels(t, models, fixture.models)

	code, models = modelSuggestionsBody(t, router, "/providers/model-suggestions?providerGroup=%20")
	if code != http.StatusOK {
		t.Fatalf("空白串时状态码应为 200，收到 %d", code)
	}
	if len(models) != 0 {
		t.Fatalf("空白串应切出空分组集合、结果为空，实际 %v", models)
	}
}

// TestProviderModelSuggestionsWildcardGroupAllowsAll 覆盖 `*` 分组全通过。
func TestProviderModelSuggestionsWildcardGroupAllowsAll(t *testing.T) {
	pools := testPools(t)
	fixture := seedModelSuggestions(t, pools)
	router := providersRouter(t, pools, &Deps{})

	code, models := modelSuggestionsBody(t, router, "/providers/model-suggestions?providerGroup=%2A")
	if code != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d", code)
	}
	containsModels(t, models, fixture.models)
	// 通配仍受 isEnabled 约束：被禁用那条的 pattern 不得出现。
	notContainsModels(t, models, []string{"d"})
}

// TestProviderModelSuggestionsRequiresAdmin 钉住鉴权面（Node 的 requireAuth("admin")）。
func TestProviderModelSuggestionsRequiresAdmin(t *testing.T) {
	pools := testPools(t)
	seedModelSuggestions(t, pools)
	router := providersRouter(t, pools, &Deps{})

	request := httptest.NewRequest(http.MethodGet, "/providers/model-suggestions", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401，收到 %d", recorder.Code)
	}
}

func indexOf(items []string, target string) int {
	for index, item := range items {
		if item == target {
			return index
		}
	}
	return -1
}
