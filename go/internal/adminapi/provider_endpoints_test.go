package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是**根级价格读端点**（/api/prices、/api/prices/vendors）的用例。
//
// 重点是 /api/prices/vendors 的两条分支（对拍 D5）：cloud_pricing_catalog 存在且 vendors 非空数组时
// 走优先分支（原样透出该列 + catalog.version），否则降级到按 model_prices 行统计（version=null）。
//
// 表纪律：cloud_pricing_catalog 是**单行最新快照**，生产/其它 lane 都直接读它。本文件的用例先整表
// 备份、再写夹具、结束后原样还原（用 json_populate_record 逐列回填，含 id 与两个 jsonb 列），
// 绝不把夹具数据留在库里。

// rootPriceVendorsRouter 装配一个只挂 provider-endpoint 路由的 Router（含两条根级价格读端点）。
func rootPriceVendorsRouter(t *testing.T, pools *store.Pools) *Router {
	t.Helper()
	deps := Deps{
		Guard:    principalGuard{principal: Principal{UserID: 1, Username: "prices-it", IsAdmin: true}},
		Problems: NewProblems(nil),
		Store:    pools,
	}
	router := New(Options{Deps: deps})
	RegisterProviderEndpointRoutes(router, deps)
	return router
}

// backupCloudPricingCatalog 整表备份为逐行 row_to_json 文本，并注册还原。
func backupCloudPricingCatalog(t *testing.T, pools *store.Pools) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT row_to_json(c)::text FROM cloud_pricing_catalog c ORDER BY id`)
	if err != nil {
		t.Fatalf("备份价格目录失败: %v", err)
	}
	var backup []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			rows.Close()
			t.Fatalf("读取备份行失败: %v", err)
		}
		backup = append(backup, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历备份行失败: %v", err)
	}

	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		if _, err := cleanupPool.Exec(ctx, `DELETE FROM cloud_pricing_catalog`); err != nil {
			t.Errorf("还原前清空价格目录失败: %v", err)
			return
		}
		for _, row := range backup {
			if _, err := cleanupPool.Exec(ctx,
				`INSERT INTO cloud_pricing_catalog
				 SELECT * FROM json_populate_record(null::cloud_pricing_catalog, $1::json)`, row); err != nil {
				t.Errorf("还原价格目录失败: %v（行 %s）", err, row)
			}
		}
	})
}

// setCloudPricingCatalog 把目录表置成单行夹具（version 用唯一值，vendors 原样落 jsonb）。
func setCloudPricingCatalog(t *testing.T, pools *store.Pools, version, vendorsJSON string) {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM cloud_pricing_catalog`); err != nil {
		t.Fatalf("清空价格目录失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO cloud_pricing_catalog (version, currency, providers, vendors, model_count)
		 VALUES ($1, 'USD', '{}'::jsonb, $2::jsonb, 0)`, version, vendorsJSON); err != nil {
		t.Fatalf("写入价格目录夹具失败: %v", err)
	}
}

func getRootPriceVendors(t *testing.T, router *Router) (int, map[string]any, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/prices/vendors", nil))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		return recorder.Code, nil, recorder.Body.String()
	}
	return recorder.Code, body, recorder.Body.String()
}

// TestRootPriceVendorsCatalogBranch 钉住对拍 D5：目录存在且 vendors 非空数组时，
// version 是目录里的版本号（Node 读 catalog.version，Go 曾恒为 null），vendors 原样透出。
func TestRootPriceVendorsCatalogBranch(t *testing.T) {
	pools := testPools(t)
	backupCloudPricingCatalog(t, pools)

	version := fmt.Sprintf("prices-it-%d", time.Now().UnixNano())
	// 多带一个 CloudVendorSummary 之外的键：Node 是把该列直接给前端，Go 也必须原样透出。
	setCloudPricingCatalog(t, pools, version,
		`[{"vendor":"acme","name":"Acme","modelCount":2,"extra":"keep-me"}]`)

	router := rootPriceVendorsRouter(t, pools)
	status, body, raw := getRootPriceVendors(t, router)
	if status != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%s", status, raw)
	}
	if body["ok"] != true {
		t.Fatalf("ok 应为 true：%s", raw)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("应返回 data 对象：%s", raw)
	}
	if data["version"] != version {
		t.Fatalf("version 应为目录里的 %q，实际 %v：%s", version, data["version"], raw)
	}
	vendors, ok := data["vendors"].([]any)
	if !ok || len(vendors) != 1 {
		t.Fatalf("应透出 1 条 vendor，实际 %v：%s", data["vendors"], raw)
	}
	first, ok := vendors[0].(map[string]any)
	if !ok {
		t.Fatalf("vendor 应为对象：%s", raw)
	}
	if first["vendor"] != "acme" || first["extra"] != "keep-me" {
		t.Fatalf("vendor 应逐字透出（含目录里的额外键），实际 %v：%s", first, raw)
	}
}

// TestRootPriceVendorsFallsBackWhenCatalogUnusable 钉住降级分支的三个入口：
// 无目录行、vendors 为空数组、vendors 不是数组（Node 的 `Array.isArray && length > 0` 同判）。
func TestRootPriceVendorsFallsBackWhenCatalogUnusable(t *testing.T) {
	pools := testPools(t)
	backupCloudPricingCatalog(t, pools)
	router := rootPriceVendorsRouter(t, pools)
	nonce := time.Now().UnixNano()

	cases := []struct {
		name    string
		seed    bool
		vendors string
	}{
		{name: "无目录行"},
		{name: "vendors 为空数组", seed: true, vendors: `[]`},
		{name: "vendors 不是数组", seed: true, vendors: `{"vendor":"acme"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pool, err := pools.Control()
			if err != nil {
				t.Fatalf("取控制分道失败: %v", err)
			}
			if _, err := pool.Exec(context.Background(), `DELETE FROM cloud_pricing_catalog`); err != nil {
				t.Fatalf("清空价格目录失败: %v", err)
			}
			if testCase.seed {
				setCloudPricingCatalog(t, pools, fmt.Sprintf("prices-it-%d", nonce), testCase.vendors)
			}

			status, body, raw := getRootPriceVendors(t, router)
			if status != http.StatusOK {
				t.Fatalf("应 200，实际 %d：%s", status, raw)
			}
			data, ok := body["data"].(map[string]any)
			if !ok {
				t.Fatalf("应返回 data 对象：%s", raw)
			}
			if data["version"] != nil {
				t.Fatalf("降级分支的 version 应为 null，实际 %v：%s", data["version"], raw)
			}
			if _, ok := data["vendors"].([]any); !ok {
				t.Fatalf("降级分支应仍返回 vendors 数组：%s", raw)
			}
		})
	}
}
