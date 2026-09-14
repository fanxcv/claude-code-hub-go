package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 providers / provider-groups 已注册端点的真实 PG 集成测试（P0 lane）。
//
// 夹具纪律（与 A1-5 同法）：名称一律带 `go-pvd-<纳秒>` 前缀（唯一），清理按前缀精确删除，
// 且清理用的连接池不会被本测试关闭（testPools 的 Cleanup 是 LIFO，后注册的本清理先跑）。
// 列表类断言必须按自己的前缀过滤——库里有别的测试与生产数据。

// providerFixture 是一次集成测试的自有数据。
type providerFixture struct {
	prefix    string
	vendorID  int64
	enabledID int64
	otherID   int64
}

func providersRouter(t *testing.T, pools *store.Pools, deps *Deps) *Router {
	t.Helper()
	if deps.Guard == nil {
		deps.Guard = newTestGuard(t, GuardOptions{})
	}
	if deps.Problems == nil {
		deps.Problems = NewProblems(nil)
	}
	deps.Store = pools
	router := New(Options{Deps: *deps})
	RegisterProviders(router, *deps)
	RegisterProviderGroups(router, *deps)
	return router
}

// providerRequest 发一次带管理员凭据的请求；compat=true 时补 dashboard-compat 头。
func providerRequest(
	t *testing.T,
	router *Router,
	method, target, body, contentType string,
	compat bool,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if compat {
		request.Header.Set("X-CCH-Dashboard-Compat", "1")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// seedProviders 建一个厂商 + 两个供应商（一个可见类型、一个隐藏类型）、两个分组。
func seedProviders(t *testing.T, pools *store.Pools) *providerFixture {
	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("go-pvd-%d", time.Now().UnixNano())
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

	insert := func(name, providerType, url, key string, weight, priority int32, costMultiplier string) int64 {
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO providers (
				name, url, key, provider_vendor_id, is_enabled, weight, priority,
				cost_multiplier, group_tag, provider_type
			) VALUES ($1, $2, $3, $4, true, $5, $6, $7::numeric, $8, $9)
			RETURNING id`,
			name, url, key, vendorID, weight, priority, costMultiplier, prefix, providerType,
		).Scan(&id); err != nil {
			t.Fatalf("建供应商夹具失败: %v", err)
		}
		return id
	}

	fixture := &providerFixture{prefix: prefix, vendorID: vendorID}
	fixture.enabledID = insert(prefix+"-visible", "claude",
		"https://user:pass@"+prefix+".invalid/anthropic", "sk-abcdefghijklmnop", 10, 3, "1.5")
	fixture.otherID = insert(prefix+"-hidden", "claude-auth",
		"https://"+prefix+".invalid/relay", "sk-zzzzzzzzzzzzzzzz", 1, 7, "0.5")

	if _, err := pool.Exec(ctx,
		`INSERT INTO provider_groups (name, cost_multiplier, description)
		 VALUES ($1, 2.0, 'it'), ($2, 1.0, 'it')`, prefix+"-a", prefix+"-b"); err != nil {
		t.Fatalf("建分组夹具失败: %v", err)
	}

	t.Cleanup(func() {
		// 清理用的池独立于 pools（pools 的 Cleanup 在本函数之后才跑）。
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
		_, _ = writer.Exec(context.Background(), `DELETE FROM provider_groups WHERE name LIKE $1`, prefix+"%")
		_, _ = writer.Exec(context.Background(), `DELETE FROM provider_vendors WHERE website_domain = $1`, prefix+".invalid")
	})
	return fixture
}

// providerVisibleID 取夹具里那个"可见类型"的供应商在列表里的项。
func providerListItem(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("解析列表响应失败: %v（原文 %.200s）", err, body)
	}
	for _, item := range payload.Items {
		return item
	}
	return nil
}

// TestIntegrationProviderListShapeAndRedaction 钉住列表的形状与脱敏：
//   - 隐藏类型（claude-auth）默认不出现，dashboard-compat 头下出现
//   - maskedKey 是掩码而不是明文；响应里**没有** key 字段
//   - url 里的 userinfo 被换成 REDACTED（redaction.ts:36-47）
//   - q 与 providerType 过滤按 Node 的口径生效
func TestIntegrationProviderListShapeAndRedaction(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	router := providersRouter(t, pools, &Deps{})

	recorder := providerRequest(t, router, http.MethodGet,
		"/providers?q="+fixture.prefix, "", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	visible := providerListItem(t, recorder.Body.Bytes())
	if visible == nil {
		t.Fatal("列表里没有夹具供应商")
	}
	if visible["name"] != fixture.prefix+"-visible" {
		t.Fatalf("默认可见性是隐藏类型被过滤后只剩 visible，实际 %v", visible["name"])
	}
	if _, present := visible["key"]; present {
		t.Fatal("响应里出现了 key 字段：明文密钥泄漏")
	}
	masked, _ := visible["maskedKey"].(string)
	if strings.Contains(masked, "abcdefghijklmnop") {
		t.Fatalf("maskedKey 里含明文密钥: %q", masked)
	}
	if masked != "sk-a••••••mnop" {
		t.Fatalf("maskedKey 形状不对（应为 maskKey 的头4+掩码+尾4）: %q", masked)
	}
	rawURL, _ := visible["url"].(string)
	if !strings.Contains(rawURL, "REDACTED") || strings.Contains(rawURL, "user:pass") {
		t.Fatalf("url 里的凭据未被脱敏: %q", rawURL)
	}

	// dashboard-compat：隐藏类型现身。
	recorder = providerRequest(t, router, http.MethodGet,
		"/providers?q="+fixture.prefix, "", "", true)
	var payload struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析 compat 列表失败: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("compat 请求应看到两个夹具供应商，实际 %d", len(payload.Items))
	}

	// providerType 精确过滤。
	recorder = providerRequest(t, router, http.MethodGet,
		"/providers?providerType=claude", "", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("providerType 合法值应 200，实际 %d", recorder.Code)
	}
	// 非法值（隐藏类型）在非 compat 下被拒。
	recorder = providerRequest(t, router, http.MethodGet,
		"/providers?providerType=claude-auth", "", "", false)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("隐藏类型在非 compat 下应 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

// TestIntegrationProviderDetailRevealAndReset 覆盖详情 / 明文密钥 / 额度重置三条。
func TestIntegrationProviderDetailRevealAndReset(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	router := providersRouter(t, pools, &Deps{})

	recorder := providerRequest(t, router, http.MethodGet,
		fmt.Sprintf("/providers/%d", fixture.enabledID), "", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("详情应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "sk-abcdefghijklmnop") {
		t.Fatal("详情里出现了明文密钥")
	}

	recorder = providerRequest(t, router, http.MethodGet,
		fmt.Sprintf("/providers/%d", fixture.enabledID+100000), "", "", false)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("不存在的 id 应 404，实际 %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "provider.not_found") {
		t.Fatalf("404 的错误码应为 provider.not_found：%s", recorder.Body.String())
	}

	recorder = providerRequest(t, router, http.MethodGet,
		fmt.Sprintf("/providers/%d/key:reveal", fixture.enabledID), "", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reveal 应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var reveal struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &reveal); err != nil {
		t.Fatalf("解析 reveal 失败: %v", err)
	}
	if reveal.Key != "sk-abcdefghijklmnop" {
		t.Fatalf("reveal 应回明文，实际 %q", reveal.Key)
	}

	// 额度重置：写 total_cost_reset_at 并回 {ok:true}。
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	recorder = providerRequest(t, router, http.MethodPost,
		fmt.Sprintf("/providers/%d/usage:reset", fixture.enabledID), "", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("usage:reset 应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if strings.TrimSpace(recorder.Body.String()) != `{"ok":true}` {
		t.Fatalf("usage:reset 正文应为 {\"ok\":true}，实际 %s", recorder.Body.String())
	}
	var resetAt *string
	if err := writer.QueryRow(context.Background(),
		`SELECT to_char(total_cost_reset_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') FROM providers WHERE id = $1`,
		fixture.enabledID).Scan(&resetAt); err != nil {
		t.Fatalf("读回 total_cost_reset_at 失败: %v", err)
	}
	if resetAt == nil {
		t.Fatal("额度重置没有写 total_cost_reset_at")
	}
}

// TestIntegrationProviderAutoSortPreviewAndApply 钉住自动排序的两态语义：
// 预览不改库（applied=false），确认才写（且只写优先级）。
func TestIntegrationProviderAutoSortPreviewAndApply(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	router := providersRouter(t, pools, &Deps{})
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}

	// 先把两个夹具供应商的优先级改成同一个值：自动排序后应出现变更。
	if _, err := writer.Exec(context.Background(),
		`UPDATE providers SET priority = 99 WHERE name LIKE $1`, fixture.prefix+"%"); err != nil {
		t.Fatalf("预置优先级失败: %v", err)
	}

	recorder := providerRequest(t, router, http.MethodPost,
		"/providers:autoSortPriority", `{"confirm":false}`, "application/json", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("预览应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var preview struct {
		Groups  []map[string]any `json:"groups"`
		Changes []map[string]any `json:"changes"`
		Summary struct {
			TotalProviders int `json:"totalProviders"`
			ChangedCount   int `json:"changedCount"`
			GroupCount     int `json:"groupCount"`
		} `json:"summary"`
		Applied bool `json:"applied"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil {
		t.Fatalf("解析预览失败: %v", err)
	}
	if preview.Applied {
		t.Fatal("confirm=false 时 applied 应为 false")
	}
	if len(preview.Changes) == 0 {
		t.Fatal("预置同优先级后应有变更项")
	}
	var priorityAfterPreview int
	if err := writer.QueryRow(context.Background(),
		`SELECT priority FROM providers WHERE id = $1`, fixture.enabledID).Scan(&priorityAfterPreview); err != nil {
		t.Fatalf("读回优先级失败: %v", err)
	}
	if priorityAfterPreview != 99 {
		t.Fatalf("预览不应改库，实际 priority=%d", priorityAfterPreview)
	}

	// 夹具的两个供应商成本倍率是 1.5 与 0.5：排序后 0.5 的组优先级为 0、1.5 的为 1。
	recorder = providerRequest(t, router, http.MethodPost,
		"/providers:autoSortPriority", `{"confirm":true}`, "application/json", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("确认应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var applied struct {
		Applied bool `json:"applied"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &applied); err != nil {
		t.Fatalf("解析确认响应失败: %v", err)
	}
	if !applied.Applied {
		t.Fatal("confirm=true 时 applied 应为 true")
	}
	var hiddenPriority, visiblePriority int
	if err := writer.QueryRow(context.Background(),
		`SELECT priority FROM providers WHERE id = $1`, fixture.otherID).Scan(&hiddenPriority); err != nil {
		t.Fatalf("读回隐藏供应商优先级失败: %v", err)
	}
	if err := writer.QueryRow(context.Background(),
		`SELECT priority FROM providers WHERE id = $1`, fixture.enabledID).Scan(&visiblePriority); err != nil {
		t.Fatalf("读回可见供应商优先级失败: %v", err)
	}
	if hiddenPriority >= visiblePriority {
		t.Fatalf("低倍率（0.5）的优先级应更小：hidden=%d visible=%d", hiddenPriority, visiblePriority)
	}
}

// TestIntegrationProviderBatchUpdateWritesOnlyGivenFields 钉住批量更新的核心不变式：
// **未给出的字段不得被写成 NULL**（这是拼 SQL 而不是固定列的全部理由）。
func TestIntegrationProviderBatchUpdateWritesOnlyGivenFields(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	router := providersRouter(t, pools, &Deps{})
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	ctx := context.Background()

	body := fmt.Sprintf(`{"providerIds":[%d],"updates":{"is_enabled":false,"weight":42}}`, fixture.enabledID)
	recorder := providerRequest(t, router, http.MethodPost,
		"/providers:batchUpdate", body, "application/json", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("批量更新应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"updatedCount":1`) {
		t.Fatalf("updatedCount 应为 1：%s", recorder.Body.String())
	}

	var isEnabled bool
	var weight int
	var costMultiplier string
	var groupTag *string
	if err := writer.QueryRow(ctx,
		`SELECT is_enabled, weight, cost_multiplier::text, group_tag FROM providers WHERE id = $1`,
		fixture.enabledID).Scan(&isEnabled, &weight, &costMultiplier, &groupTag); err != nil {
		t.Fatalf("读回供应商失败: %v", err)
	}
	if isEnabled {
		t.Fatal("is_enabled 未被写入")
	}
	if weight != 42 {
		t.Fatalf("weight 应写成 42，实际 %d", weight)
	}
	if !strings.HasPrefix(costMultiplier, "1.5") {
		t.Fatalf("未给出的 cost_multiplier 被改写：%s", costMultiplier)
	}
	if groupTag == nil || *groupTag != fixture.prefix {
		t.Fatalf("未给出的 group_tag 被改写：%v", groupTag)
	}

	// 空补丁：Node 侧是 400（"请指定要更新的字段"）。
	recorder = providerRequest(t, router, http.MethodPost,
		`/providers:batchUpdate`, fmt.Sprintf(`{"providerIds":[%d],"updates":{}}`, fixture.enabledID),
		"application/json", true)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("空补丁应 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	// 不可见（不存在）的 id：404。
	recorder = providerRequest(t, router, http.MethodPost, "/providers:batchUpdate",
		`{"providerIds":[99999999],"updates":{"weight":1}}`, "application/json", true)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("不可见 id 应 404，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	// 未声明的键：strict 模式 400。
	recorder = providerRequest(t, router, http.MethodPost, "/providers:batchUpdate",
		fmt.Sprintf(`{"providerIds":[%d],"updates":{"nope":1}}`, fixture.enabledID),
		"application/json", true)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("未声明键应 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

// TestIntegrationProviderGroupsCRUD 覆盖分组的四条端点与其三种拒绝分支。
func TestIntegrationProviderGroupsCRUD(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	router := providersRouter(t, pools, &Deps{})
	ctx := context.Background()

	recorder := providerRequest(t, router, http.MethodGet, "/provider-groups", "", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("分组列表应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var listed struct {
		Items []struct {
			Name          string `json:"name"`
			ProviderCount int64  `json:"providerCount"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("解析分组列表失败: %v", err)
	}
	seen := map[string]int64{}
	for _, item := range listed.Items {
		seen[item.Name] = item.ProviderCount
	}
	// 夹具建了两个分组的行；-a 无人引用，-b 同样（引用计数只在 group_tag 命中时 > 0）。
	if count, ok := seen[fixture.prefix+"-a"]; !ok || count != 0 {
		t.Fatalf("夹具分组 -a 应在列表里且引用计数为 0：ok=%v count=%d", ok, count)
	}
	// 差异钉子：`prefix` 只出现在 providers.group_tag 里、没有对应的 provider_groups 行。
	// Node 的 getProviderGroups 会自愈补登记它；Go 侧**刻意不做隐式写**（见 handleListProviderGroups
	// 的注释），故它此刻不应出现在列表里。这条断言就是那份白名单差异的可执行凭据。
	if _, ok := seen[fixture.prefix]; ok {
		t.Fatal("provider_groups 里没有该行，但列表返回了它：自愈行为被意外引入了")
	}

	// 建一个分组行，再挂两个供应商上去，验证引用计数真的会数。
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	var countedID int64
	if err := writer.QueryRow(ctx,
		`INSERT INTO provider_groups (name, cost_multiplier, description)
		 VALUES ($1, 1.0, 'it') RETURNING id`, fixture.prefix+"-counted").Scan(&countedID); err != nil {
		t.Fatalf("建计数用分组失败: %v", err)
	}
	if _, err := writer.Exec(ctx,
		`UPDATE providers SET group_tag = $1 WHERE name LIKE $2`, fixture.prefix+"-counted", fixture.prefix+"%"); err != nil {
		t.Fatalf("改挂分组失败: %v", err)
	}
	recorder = providerRequest(t, router, http.MethodGet, "/provider-groups", "", "", false)
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("重解分组列表失败: %v", err)
	}
	counted := int64(-1)
	for _, item := range listed.Items {
		if item.Name == fixture.prefix+"-counted" {
			counted = item.ProviderCount
		}
	}
	if counted != 2 {
		t.Fatalf("挂上两个供应商后引用计数应为 2，实际 %d", counted)
	}

	// 创建：201 + Location + providerCount 0。
	recorder = providerRequest(t, router, http.MethodPost, "/provider-groups",
		fmt.Sprintf(`{"name":%q,"costMultiplier":3.25,"description":"it"}`, fixture.prefix+"-new"),
		"application/json", false)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("创建分组应 201，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); !strings.HasSuffix(location, "/provider-groups/") &&
		!strings.HasPrefix(location, MountPrefix+"/provider-groups/") {
		t.Fatalf("Location 头形状不对: %q", location)
	}
	var created struct {
		ID             int64   `json:"id"`
		CostMultiplier float64 `json:"costMultiplier"`
		ProviderCount  int64   `json:"providerCount"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析创建响应失败: %v", err)
	}
	if created.CostMultiplier != 3.25 {
		t.Fatalf("倍率未写入：%v", created.CostMultiplier)
	}

	// 重名：400 DUPLICATE_NAME。
	recorder = providerRequest(t, router, http.MethodPost, "/provider-groups",
		fmt.Sprintf(`{"name":%q}`, fixture.prefix+"-new"), "application/json", false)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "DUPLICATE_NAME") {
		t.Fatalf("重名应 400 DUPLICATE_NAME，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	// 更新：200 + 倍率变更落库。
	recorder = providerRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/provider-groups/%d", created.ID), `{"costMultiplier":0.75,"description":null}`,
		"application/json", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("更新分组应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var updated struct {
		CostMultiplier float64 `json:"costMultiplier"`
		Description    *string `json:"description"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &updated); err != nil {
		t.Fatalf("解析更新响应失败: %v", err)
	}
	if updated.CostMultiplier != 0.75 || updated.Description != nil {
		t.Fatalf("更新结果不符：%+v", updated)
	}

	// 删除被引用的分组：400 GROUP_IN_USE。
	var inUseID int64
	if err := writer.QueryRow(ctx,
		`SELECT id FROM provider_groups WHERE name = $1`, fixture.prefix+"-counted").Scan(&inUseID); err != nil {
		t.Fatalf("读回夹具分组 id 失败: %v", err)
	}
	recorder = providerRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/provider-groups/%d", inUseID), "", "", false)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "GROUP_IN_USE") {
		t.Fatalf("被引用分组应 400 GROUP_IN_USE，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	// 默认分组：400 CANNOT_DELETE_DEFAULT（default 组由迁移预置）。
	var defaultID int64
	if err := writer.QueryRow(ctx,
		`SELECT id FROM provider_groups WHERE name = 'default'`).Scan(&defaultID); err != nil {
		t.Skipf("库里没有 default 分组，跳过该分支: %v", err)
	}
	recorder = providerRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/provider-groups/%d", defaultID), "", "", false)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "CANNOT_DELETE_DEFAULT") {
		t.Fatalf("默认分组应 400 CANNOT_DELETE_DEFAULT，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	// 无人引用：204。
	recorder = providerRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/provider-groups/%d", created.ID), "", "", false)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("删除空分组应 204，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var remaining int
	if err := writer.QueryRow(ctx,
		`SELECT count(*) FROM provider_groups WHERE id = $1`, created.ID).Scan(&remaining); err != nil {
		t.Fatalf("读回删除结果失败: %v", err)
	}
	if remaining != 0 {
		t.Fatal("分组未被删除")
	}

	// 不存在的 id：404。
	recorder = providerRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/provider-groups/%d", created.ID), "", "", false)
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "provider_group.not_found") {
		t.Fatalf("不存在分组应 404 provider_group.not_found，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

// TestProviderRedactHeaderRecordNullStaysNull 钉住对拍 D4：customHeaders 为 SQL NULL 时
// 必须出 JSON null，而不是 {}。
//
// 根因在读取路径：本模块的行经 `row_to_json` 文本读回，SQL NULL 会变成 JSON 字面量 null（4 字节）。
// json.Unmarshal 把 null 解成 nil map 且不报错，若照常 make(map, 0) 再编码，就变成 {}。
// Node 的 redactHeaderRecord 对 null/undefined 直接返回 null（redaction.ts:25-34）。
func TestProviderRedactHeaderRecordNullStaysNull(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{name: "SQL NULL 的 row_to_json 字面量", raw: json.RawMessage(`null`), want: `null`},
		{name: "空 RawMessage", raw: json.RawMessage(``), want: `null`},
		{name: "空对象保持空对象", raw: json.RawMessage(`{}`), want: `{}`},
		{
			name: "敏感头脱敏、其余原样",
			raw:  json.RawMessage(`{"authorization":"Bearer s3cret","x-trace":"abc"}`),
			want: `{"authorization":"[REDACTED]","x-trace":"abc"}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := json.Marshal(providerRedactHeaderRecord(testCase.raw))
			if err != nil {
				t.Fatalf("编码失败: %v", err)
			}
			if string(encoded) != testCase.want {
				t.Fatalf("应为 %s，实际 %s", testCase.want, encoded)
			}
		})
	}
}
