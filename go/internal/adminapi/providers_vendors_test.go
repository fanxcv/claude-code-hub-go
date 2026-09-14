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
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 POST /providers/vendors:recluster 的用例。
//
// 夹具自钉：域名带 vendorsReclusterITMarker 前缀（每轮唯一），退出时按域名清掉厂商行
// （端点行走 FK ON DELETE CASCADE）；供应商行按 id 失效 + 软删。

const vendorsReclusterITMarker = "go-vendors-recluster-it"

// vendorsReclusterRouter 装配只注册本端点的路由。
func vendorsReclusterRouter(t *testing.T, pools *store.Pools) *Router {
	t.Helper()
	deps := Deps{
		Logger:   logx.New(nil),
		Guard:    principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Store:    pools,
		Problems: NewProblems(nil),
	}
	router := New(Options{Deps: deps})
	RegisterProvidersVendorsRoutes(router, deps)
	return router
}

// TestProvidersVendorsRegistrationGate 钉住「Store 未装配即不注册」。
func TestProvidersVendorsRegistrationGate(t *testing.T) {
	pools := testPools(t)
	if count := vendorsReclusterRouter(t, nil).RouteCount(); count != 0 {
		t.Fatalf("Store 未装配时应零注册，实际 %d 条", count)
	}
	if count := vendorsReclusterRouter(t, pools).RouteCount(); count != 1 {
		t.Fatalf("装配后应注册 1 条，实际 %d 条", count)
	}
}

// TestProvidersVendorsValidation 钉住 {confirm} 的 strict 与类型校验。
func TestProvidersVendorsValidation(t *testing.T) {
	router := vendorsReclusterRouter(t, &store.Pools{})
	cases := []struct {
		name string
		body string
	}{
		{"未知键", `{"confirm":false,"extra":1}`},
		{"confirm 非布尔", `{"confirm":"yes"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body := providersLimitCall(t, router, http.MethodPost,
				"/providers/vendors:recluster", testCase.body)
			if status != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d：%.200s", status, body)
			}
		})
	}
}

// vendorsReclusterIT 是本端点集成用例的装配。
type vendorsReclusterIT struct {
	router *Router
	pools  *store.Pools
	marker string
}

func newVendorsReclusterIT(t *testing.T) *vendorsReclusterIT {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	ctx := context.Background()
	pools, err := store.Open(ctx, store.Options{
		DSN:      dsn,
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	integration := &vendorsReclusterIT{
		pools:  pools,
		marker: fmt.Sprintf("%s-%d", vendorsReclusterITMarker, time.Now().UnixNano()),
	}
	integration.router = vendorsReclusterRouter(t, pools)
	return integration
}

// domain 造一个每轮唯一的厂域名（同轮内同域名的多个供应商应聚到同一个厂）。
func (integration *vendorsReclusterIT) domain(suffix string) string {
	return fmt.Sprintf("%s-%s.example.invalid", integration.marker, suffix)
}

// seedProviderWithVendor 种一个供应商，可挂在既有 vendor 上（vendorID=0 表示无厂）。
// 返回 providerID 与 vendorID。
func (integration *vendorsReclusterIT) seedProviderWithVendor(
	t *testing.T,
	name string,
	url string,
	websiteURL *string,
	vendorDomain string,
) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	pool, err := integration.pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}

	var vendorID int64
	if vendorDomain != "" {
		if err := pool.QueryRow(ctx, `
			INSERT INTO provider_vendors (website_domain, updated_at)
			VALUES ($1, now())
			ON CONFLICT (website_domain) DO UPDATE SET updated_at = now()
			RETURNING id`, vendorDomain).Scan(&vendorID); err != nil {
			t.Fatalf("建厂商失败: %v", err)
		}
	}

	var providerID int64
	if vendorDomain == "" {
		if err := pool.QueryRow(ctx, `
			INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority, group_tag, website_url)
			VALUES ($1, $2, 'upstream-not-used', 'codex', true, 1, 0, 'default', $3) RETURNING id`,
			name, url, websiteURL).Scan(&providerID); err != nil {
			t.Fatalf("建供应商失败: %v", err)
		}
	} else {
		if err := pool.QueryRow(ctx, `
			INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority, group_tag, website_url, provider_vendor_id)
			VALUES ($1, $2, 'upstream-not-used', 'codex', true, 1, 0, 'default', $3, $4) RETURNING id`,
			name, url, websiteURL, vendorID).Scan(&providerID); err != nil {
			t.Fatalf("建供应商失败: %v", err)
		}
	}

	t.Cleanup(func() {
		cleanup := context.Background()
		if _, err := pool.Exec(cleanup,
			`UPDATE providers SET is_enabled = false, deleted_at = now() WHERE id = $1`, providerID); err != nil {
			t.Errorf("清理供应商失败: %v", err)
		}
		// 轮内所有厂商行按域名前缀清（端点行随 FK 级联删除）。
		if _, err := pool.Exec(cleanup,
			`DELETE FROM provider_endpoints WHERE vendor_id IN (SELECT id FROM provider_vendors WHERE website_domain LIKE $1)`,
			integration.marker+"%"); err != nil {
			t.Errorf("清理端点失败: %v", err)
		}
		if _, err := pool.Exec(cleanup,
			`UPDATE providers SET provider_vendor_id = NULL WHERE provider_vendor_id IN
			   (SELECT id FROM provider_vendors WHERE website_domain LIKE $1)`,
			integration.marker+"%"); err != nil {
			t.Errorf("解除供应商厂商引用失败: %v", err)
		}
		if _, err := pool.Exec(cleanup,
			`DELETE FROM provider_vendors WHERE website_domain LIKE $1`, integration.marker+"%"); err != nil {
			t.Errorf("清理厂商失败: %v", err)
		}
	})
	return providerID, vendorID
}

// providerVendorOf 读回供应商当前挂的厂。
func (integration *vendorsReclusterIT) providerVendorOf(t *testing.T, providerID int64) *int64 {
	t.Helper()
	pool, err := integration.pools.Control()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	var vendorID *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT provider_vendor_id FROM providers WHERE id = $1`, providerID).Scan(&vendorID); err != nil {
		t.Fatalf("读供应商厂商失败: %v", err)
	}
	return vendorID
}

// activeEndpointExists 读回 (vendor, type, url) 是否已有活跃端点行。
func (integration *vendorsReclusterIT) activeEndpointExists(
	t *testing.T,
	vendorID int64,
	providerType string,
	url string,
) bool {
	t.Helper()
	pool, err := integration.pools.Control()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(context.Background(), `
		SELECT EXISTS(SELECT 1 FROM provider_endpoints
			WHERE vendor_id = $1 AND provider_type = $2 AND url = $3 AND deleted_at IS NULL)`,
		vendorID, providerType, url).Scan(&exists); err != nil {
		t.Fatalf("探测端点行失败: %v", err)
	}
	return exists
}

// reclusterCall 调一次端点并解析响应。
func reclusterCall(t *testing.T, router *Router, confirm bool) vendorReclusterBody {
	t.Helper()
	body := fmt.Sprintf(`{"confirm":%t}`, confirm)
	status, raw := providersLimitCall(t, router, http.MethodPost, "/providers/vendors:recluster", body)
	if status != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%.300s", status, raw)
	}
	var decoded vendorReclusterBody
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("解析响应失败: %v；正文 %.300s", err, raw)
	}
	return decoded
}

type vendorReclusterBody struct {
	Preview struct {
		ProvidersMoved    int `json:"providersMoved"`
		VendorsCreated    int `json:"vendorsCreated"`
		VendorsToDelete   int `json:"vendorsToDelete"`
		SkippedInvalidURL int `json:"skippedInvalidUrl"`
	} `json:"preview"`
	Changes []struct {
		ProviderID      int64  `json:"providerId"`
		OldVendorID     int64  `json:"oldVendorId"`
		OldVendorDomain string `json:"oldVendorDomain"`
		NewVendorDomain string `json:"newVendorDomain"`
	} `json:"changes"`
	Applied bool `json:"applied"`
}

// TestIntegrationReclusterPreviewDoesNotWrite 钉住「预览不落库」与域名口径。
func TestIntegrationReclusterPreviewDoesNotWrite(t *testing.T) {
	integration := newVendorsReclusterIT(t)
	newDomain := integration.domain("preview")
	website := "https://" + newDomain + "/page"
	// 供应商当前挂在一个**不同的**旧域名上 ⇒ 应产生一条 change。
	providerID, oldVendorID := integration.seedProviderWithVendor(
		t, integration.marker+"-preview", "http://127.0.0.1:9", &website, integration.domain("old"))

	response := reclusterCall(t, integration.router, false)
	if response.Applied {
		t.Fatalf("预览不应置 applied=true")
	}
	if response.Preview.ProvidersMoved == 0 {
		t.Fatalf("应至少有一条变更，实际 %+v", response.Preview)
	}
	change := findReclusterChange(t, response, providerID)
	if change.NewVendorDomain != newDomain {
		t.Fatalf("新域名应为 %s，实际 %s", newDomain, change.NewVendorDomain)
	}
	if change.OldVendorDomain != integration.domain("old") {
		t.Fatalf("旧域名应为 %s，实际 %s", integration.domain("old"), change.OldVendorDomain)
	}
	if change.OldVendorID != oldVendorID {
		t.Fatalf("旧厂 id 应为 %d，实际 %d", oldVendorID, change.OldVendorID)
	}
	// 关键：预览阶段不得改库。
	if got := integration.providerVendorOf(t, providerID); got == nil || *got != oldVendorID {
		t.Fatalf("预览不应改动 provider_vendor_id：期望 %d，实际 %v", oldVendorID, got)
	}
}

// TestIntegrationReclusterApplyMovesVendorsAndBackfillsEndpoints 钉住 apply 的四步副作用。
func TestIntegrationReclusterApplyMovesVendorsAndBackfillsEndpoints(t *testing.T) {
	integration := newVendorsReclusterIT(t)
	sharedDomain := integration.domain("shared")
	website := "https://" + sharedDomain
	// 两个供应商共用同一新域名 ⇒ 应聚到同一个厂（Node 的 vendorsCreated 是去重计数）。
	firstID, _ := integration.seedProviderWithVendor(
		t, integration.marker+"-a", "http://127.0.0.1:9/a", &website, "")
	secondID, _ := integration.seedProviderWithVendor(
		t, integration.marker+"-b", "http://127.0.0.1:9/b", &website, "")
	// 隐藏类型也不该被过滤：这条是维护工具，Node 不做可见性过滤。
	hiddenID, _ := integration.seedProviderWithVendor(
		t, integration.marker+"-hidden", "http://127.0.0.1:9/hidden", &website, "")

	preview := reclusterCall(t, integration.router, false)
	if preview.Preview.ProvidersMoved < 3 {
		t.Fatalf("三条供应商都应入变更，实际 %d", preview.Preview.ProvidersMoved)
	}
	// 同域名的三条应指向同一个新域名（这是本用例的核心口径）。
	for _, providerID := range []int64{firstID, secondID, hiddenID} {
		change := findReclusterChange(t, preview, providerID)
		if change.NewVendorDomain != sharedDomain {
			t.Fatalf("供应商 %d 的新域名应为 %s，实际 %s", providerID, sharedDomain, change.NewVendorDomain)
		}
	}
	applied := reclusterCall(t, integration.router, true)
	if !applied.Applied {
		t.Fatalf("apply 应置 applied=true")
	}
	if applied.Preview.ProvidersMoved != preview.Preview.ProvidersMoved {
		t.Fatalf("apply 与 preview 的 providersMoved 应一致：%d vs %d",
			preview.Preview.ProvidersMoved, applied.Preview.ProvidersMoved)
	}
	// vendorsCreated 是**全表去重计数**（Node 同样扫全部供应商），共享测试库里可能有别的
	// 供应商也参与变更，故这里只断言下界与「同一域名只算一个」的相对关系：
	// 本用例三个同域名供应商最多贡献 1。
	if applied.Preview.VendorsCreated < 1 {
		t.Fatalf("至少应计一个新厂，实际 %d", applied.Preview.VendorsCreated)
	}
	if applied.Preview.VendorsCreated > preview.Preview.VendorsCreated {
		t.Fatalf("apply 不应比 preview 多出新厂：%d vs %d",
			applied.Preview.VendorsCreated, preview.Preview.VendorsCreated)
	}

	// 三个供应商都应挂到同一个新厂上。
	var vendorID *int64
	for _, providerID := range []int64{firstID, secondID, hiddenID} {
		got := integration.providerVendorOf(t, providerID)
		if got == nil || *got <= 0 {
			t.Fatalf("供应商 %d 应已挂厂，实际 %v", providerID, got)
		}
		if vendorID == nil {
			vendorID = got
			continue
		}
		if *got != *vendorID {
			t.Fatalf("同域名的供应商应聚到同一个厂：%d vs %d", *vendorID, *got)
		}
	}
	// 端点回填：新厂下应有对应的活跃端点行（URL 逐字保留）。
	for _, url := range []string{"http://127.0.0.1:9/a", "http://127.0.0.1:9/b", "http://127.0.0.1:9/hidden"} {
		if !integration.activeEndpointExists(t, *vendorID, "codex", url) {
			t.Fatalf("回填后应有活跃端点行：vendor=%d url=%s", *vendorID, url)
		}
	}
	// 幂等：再次预览应零变更（域名已一致）。
	again := reclusterCall(t, integration.router, false)
	if change := findReclusterChangeOptional(again, firstID); change != nil {
		t.Fatalf("已对齐的供应商不应再入变更：%+v", *change)
	}
}

// TestIntegrationReclusterSkipsAndCleansOldVendor 钉住 skippedInvalidUrl 与旧厂清理。
func TestIntegrationReclusterSkipsAndCleansOldVendor(t *testing.T) {
	integration := newVendorsReclusterIT(t)
	website := "https://" + integration.domain("moved")
	// 正常一条：旧厂在新挂之后应变成空厂并被清掉。
	providerID, oldVendorID := integration.seedProviderWithVendor(
		t, integration.marker+"-cleanup", "http://127.0.0.1:9/cleanup", &website, integration.domain("stale"))
	// 一条 URL 解析不出域名：计入 skippedInvalidUrl，且不产生 change。
	invalidURL := "http://[invalid-host"
	invalidID, _ := integration.seedProviderWithVendor(
		t, integration.marker+"-invalid", invalidURL, nil, "")

	response := reclusterCall(t, integration.router, true)
	if response.Preview.SkippedInvalidURL < 1 {
		t.Fatalf("应有 skippedInvalidUrl，实际 %+v", response.Preview)
	}
	if change := findReclusterChangeOptional(response, invalidID); change != nil {
		t.Fatalf("URL 不可解析的供应商不应入变更：%+v", *change)
	}
	if change := findReclusterChangeOptional(response, providerID); change == nil {
		t.Fatalf("应有一条针对 %d 的变更", providerID)
	}

	pool, err := integration.pools.Control()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	var oldVendorAlive bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM provider_vendors WHERE id = $1)`, oldVendorID).Scan(&oldVendorAlive); err != nil {
		t.Fatalf("探测旧厂失败: %v", err)
	}
	if oldVendorAlive {
		t.Fatalf("重挂后旧厂（已无供应商与端点）应被清掉：vendorId=%d", oldVendorID)
	}
}

// findReclusterChange 取指定供应商的变更（缺失即失败）。
func findReclusterChange(t *testing.T, body vendorReclusterBody, providerID int64) struct {
	ProviderID      int64  `json:"providerId"`
	OldVendorID     int64  `json:"oldVendorId"`
	OldVendorDomain string `json:"oldVendorDomain"`
	NewVendorDomain string `json:"newVendorDomain"`
} {
	t.Helper()
	change := findReclusterChangeOptional(body, providerID)
	if change == nil {
		t.Fatalf("未找到供应商 %d 的变更：%+v", providerID, body.Changes)
	}
	return *change
}

// findReclusterChangeOptional 取指定供应商的变更（缺失返回 nil）。
func findReclusterChangeOptional(body vendorReclusterBody, providerID int64) *struct {
	ProviderID      int64  `json:"providerId"`
	OldVendorID     int64  `json:"oldVendorId"`
	OldVendorDomain string `json:"oldVendorDomain"`
	NewVendorDomain string `json:"newVendorDomain"`
} {
	for index := range body.Changes {
		if body.Changes[index].ProviderID == providerID {
			return &body.Changes[index]
		}
	}
	return nil
}
