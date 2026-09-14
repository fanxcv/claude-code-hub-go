package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 `GET /provider-vendors` 的**仪表盘兼容分支**（Node 的 getDashboardProviderVendors）用例。
//
// 为什么值得单测：它是「添加供应商」表单的数据源，2026-09-13 生产上该分支曾是 501，导致用户在界面上
// 加不了供应商（网关日志 admin_provider_endpoint_dashboard_vendors_not_implemented 恰好 3 次，
// 与表单轮询次数吻合）。这条语义有三处容易退化的地方，视觉上都看不出来：
//  1. 只该列**真有启用供应商**的 vendor——「启用/软删/vendor_id 为空」三种状态各是一种排除，
//     漏掉任一条件都会让表单里冒出「没有供应商的厂」；
//  2. `providerTypes` 的顺序必须照 Node 的 ProviderTypeSchema.options，而 **Go 常量的声明顺序与它不同**
//     （Go 里 openai-compatible 在 gemini 之前）——照声明顺序排会得到相反的相对次序；
//  3. `providerTypes` **只属于仪表盘分支**：默认分支的正文里多出该键就会与既有对拍/快照分叉。
//
// 共享库纪律（与本包其它集成用例同）：夹具自钉唯一域名/名字前缀，按精确 id 清，不动别人的行；
// 断言一律**按夹具前缀收敛**（库里还有真实 vendor 与 provider）。

// vendorsDashboardIT 是本组用例的装配。
type vendorsDashboardIT struct {
	pools  *store.Pools
	router *Router
	pool   *store.Pool
	prefix string
}

// newVendorsDashboardIT 取真库并装配只挂 provider-endpoint 路由的 Router；门控未设置时跳过。
func newVendorsDashboardIT(t *testing.T) *vendorsDashboardIT {
	t.Helper()
	pools := systemSettingsIntegrationPools(t)
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	deps := Deps{
		Guard:    principalGuard{principal: Principal{UserID: 1, Username: "vendors-dash-it", IsAdmin: true}},
		Problems: NewProblems(nil),
		Store:    pools,
	}
	router := New(Options{Deps: deps})
	RegisterProviderEndpointRoutes(router, deps)

	prefix := fmt.Sprintf("go-vendors-dash-it-%d", time.Now().UnixNano())
	it := &vendorsDashboardIT{pools: pools, router: router, pool: pool, prefix: prefix}
	t.Cleanup(func() {
		ctx := context.Background()
		// 供应商行按名字前缀清（FK 的 ON DELETE 是 RESTRICT，必须先清 providers 再清 vendor）。
		if _, err := pool.Exec(ctx, `DELETE FROM providers WHERE name LIKE $1`, prefix+"%"); err != nil {
			t.Errorf("清供应商夹具失败: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM provider_vendors WHERE website_domain LIKE $1`, prefix+"%"); err != nil {
			t.Errorf("清厂商夹具失败: %v", err)
		}
	})
	return it
}

// domain 造一个每轮唯一的厂域名。
func (it *vendorsDashboardIT) domain(suffix string) string {
	return it.prefix + "-" + suffix + ".example.com"
}

// seedVendor 建一个厂，返回 id。
func (it *vendorsDashboardIT) seedVendor(t *testing.T, suffix string) int64 {
	t.Helper()
	var vendorID int64
	if err := it.pool.QueryRow(context.Background(),
		`INSERT INTO provider_vendors (website_domain, display_name) VALUES ($1, $2) RETURNING id`,
		it.domain(suffix), it.prefix+"-"+suffix,
	).Scan(&vendorID); err != nil {
		t.Fatalf("建厂商夹具失败: %v", err)
	}
	return vendorID
}

// seedProvider 建一个供应商；vendorID 为 nil 表示 provider_vendor_id 留空；
// disabled 置 is_enabled=false；softDeleted 置 deleted_at。
func (it *vendorsDashboardIT) seedProvider(
	t *testing.T,
	suffix string,
	providerType string,
	vendorID *int64,
	disabled bool,
	softDeleted bool,
) {
	t.Helper()
	name := it.prefix + "-" + suffix
	enabled := !disabled
	deletedAt := "NULL"
	if softDeleted {
		deletedAt = "now()"
	}
	vendorExpr := "NULL"
	if vendorID != nil {
		vendorExpr = fmt.Sprintf("%d", *vendorID)
	}
	statement := fmt.Sprintf(`
		INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority, group_tag,
		                       website_url, provider_vendor_id, deleted_at)
		VALUES ($1, $2, 'upstream-not-used', $3, $4, 1, 0, 'default', $5, %s, %s)`,
		vendorExpr, deletedAt)
	if _, err := it.pool.Exec(context.Background(), statement,
		name, "https://"+name+".example.com", providerType, enabled, "https://"+name+".example.com",
	); err != nil {
		t.Fatalf("建供应商夹具失败: %v", err)
	}
}

// call 发一次 GET /provider-vendors；compat 为真时带仪表盘兼容头。
func (it *vendorsDashboardIT) call(t *testing.T, compat bool) (int, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/provider-vendors", nil)
	if compat {
		request.Header.Set(dashboardCompatHeader, "1")
	}
	recorder := httptest.NewRecorder()
	it.router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

// vendorItems 解析 items 数组（保留原始键集，便于断言「有没有多出某个键」）。
func vendorItems(t *testing.T, body string) []map[string]any {
	t.Helper()
	var payload struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析 items 失败: %v（正文前 200 字符：%.200s）", err, body)
	}
	return payload.Items
}

// findVendorByDomain 在 items 里按 websiteDomain 找厂（找不到返回 nil）。
func findVendorByDomain(items []map[string]any, domain string) map[string]any {
	for _, item := range items {
		if value, ok := item["websiteDomain"].(string); ok && value == domain {
			return item
		}
	}
	return nil
}

// TestDashboardVendorsListsOnlyVendorsWithEnabledProviders 是分支的主用例。
//
// 一处不可达的登记：Node 的 `provider_vendor_id > 0` 在库层**插不出来**——`providers.provider_vendor_id`
// 对 `provider_vendors(id)` 有 FK（ON DELETE RESTRICT），而 id 由序列从 1 起，故 0 不是合法引用。
// 该条件仍照抄（parity），但用例无法为它造夹具；可达的是 `IS NULL` 那一支，由 orphan 夹具覆盖。
func TestDashboardVendorsListsOnlyVendorsWithEnabledProviders(t *testing.T) {
	it := newVendorsDashboardIT(t)

	// ① 主厂：四个类型**乱序插入**（openai-compatible → gemini → gemini-cli → codex），
	//    期望输出按 Node 枚举顺序 codex, gemini-cli, gemini, openai-compatible。
	mainVendor := it.seedVendor(t, "main")
	for _, providerType := range []string{"openai-compatible", "gemini", "gemini-cli", "codex"} {
		vendor := mainVendor
		it.seedProvider(t, "main-"+providerType, providerType, &vendor, false, false)
	}
	// 同一类型重复一名（DISTINCT 去重）：不该出现重复项。
	it.seedProvider(t, "main-codex-dup", "codex", &mainVendor, false, false)

	// ② 停用供应商所在的厂 → 不该出现。
	offVendor := it.seedVendor(t, "off")
	it.seedProvider(t, "off-claude", "claude", &offVendor, true, false)

	// ③ 软删供应商所在的厂 → 不该出现。
	goneVendor := it.seedVendor(t, "gone")
	it.seedProvider(t, "gone-claude", "claude", &goneVendor, false, true)

	// ④ 两个都该被排除的厂：
	//    - orphan：厂行存在但**没有任何供应商挂钩**→ 不该出现；
	//    - NULL 供应商：provider_vendor_id 为空的供应商（不属于任何厂）→ 也不该把厂带出来。
	_ = it.seedVendor(t, "orphan")
	it.seedProvider(t, "null-vendor", "claude", nil, false, false)

	code, body := it.call(t, true)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（正文：%s）", code, body)
	}
	// 空集必须是 [] 而不是 null：这是 Node 的 `return []` 语义。
	if strings.Contains(body, `"items":null`) {
		t.Fatalf("items 不得为 null：%s", body)
	}

	items := vendorItems(t, body)
	main := findVendorByDomain(items, it.domain("main"))
	if main == nil {
		t.Fatalf("主厂未出现在仪表盘列表中（域名 %s）", it.domain("main"))
	}

	rawTypes, ok := main["providerTypes"].([]any)
	if !ok {
		t.Fatalf("主厂缺 providerTypes 或类型不对：%v", main["providerTypes"])
	}
	types := make([]string, 0, len(rawTypes))
	for _, value := range rawTypes {
		types = append(types, fmt.Sprint(value))
	}
	want := []string{"codex", "gemini-cli", "gemini", "openai-compatible"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("providerTypes = %v，期望 %v（顺序必须照 Node 的 ProviderTypeSchema.options）", types, want)
	}

	for _, absent := range []string{"off", "gone", "orphan"} {
		if item := findVendorByDomain(items, it.domain(absent)); item != nil {
			t.Fatalf("%s 厂不该出现（停用/软删/无启用供应商）：%v", absent, item)
		}
	}

	// 契约：仪表盘分支的每一项都必须带非空 providerTypes（Node 在 map 之后 filter 掉空数组）。
	for _, item := range items {
		list, ok := item["providerTypes"].([]any)
		if !ok || len(list) == 0 {
			t.Fatalf("仪表盘分支的每一项都该有非空 providerTypes，此项目没有：%v", item)
		}
	}
}

// TestDashboardVendorsDefaultBranchOmitsProviderTypes 钉住「键只在仪表盘分支出现」。
func TestDashboardVendorsDefaultBranchOmitsProviderTypes(t *testing.T) {
	it := newVendorsDashboardIT(t)
	vendor := it.seedVendor(t, "main")
	it.seedProvider(t, "main-codex", "codex", &vendor, false, false)

	code, body := it.call(t, false)
	if code != http.StatusOK {
		t.Fatalf("默认分支状态码 = %d，期望 200（正文：%s）", code, body)
	}
	if strings.Contains(body, "providerTypes") {
		t.Fatalf("默认分支正文里不得出现 providerTypes（会与既有对拍/快照分叉）：%.400s", body)
	}
	// 默认分支列**全部** vendor，故夹具厂一定在（它证明「缺键」不是因为厂不在列表里）。
	if item := findVendorByDomain(vendorItems(t, body), it.domain("main")); item == nil {
		t.Fatalf("默认分支应列出全部厂商，夹具厂却在列表外")
	}

	// 仪表盘分支确实带该键（反向对照：证明上面的断言不是因为键名拼错）。
	_, dashBody := it.call(t, true)
	if !strings.Contains(dashBody, `"providerTypes"`) {
		t.Fatalf("仪表盘分支应带 providerTypes：%.400s", dashBody)
	}
}

// TestDashboardVendorPairsMatchNodeSQL 用**Node 的原 SQL 语义**（手写同一 WHERE/ORDER BY）对拍 store 查询。
//
// 共享库里已有真实供应商，故这里比对**全表**结果（读查询，不动数据）。
func TestDashboardVendorPairsMatchNodeSQL(t *testing.T) {
	it := newVendorsDashboardIT(t)
	ctx := context.Background()

	// Node 的 findEnabledProviderVendorTypePairs：四个条件 + 两列升序（orderBy asc(vendorId), asc(type)）。
	rows, err := it.pool.Query(ctx, `
		SELECT DISTINCT provider_vendor_id, provider_type
		  FROM providers
		 WHERE deleted_at IS NULL
		   AND is_enabled = true
		   AND provider_vendor_id IS NOT NULL
		   AND provider_vendor_id > 0
		 ORDER BY provider_vendor_id ASC, provider_type ASC`)
	if err != nil {
		t.Fatalf("跑 Node 口径 SQL 失败: %v", err)
	}
	var nodePairs []string
	for rows.Next() {
		var vendorID int64
		var providerType string
		if err := rows.Scan(&vendorID, &providerType); err != nil {
			rows.Close()
			t.Fatalf("读 Node 口径行失败: %v", err)
		}
		nodePairs = append(nodePairs, fmt.Sprintf("%d/%s", vendorID, providerType))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 Node 口径行失败: %v", err)
	}

	pairs, err := it.pools.AdminFindEnabledProviderVendorTypePairs(ctx)
	if err != nil {
		t.Fatalf("store 查询失败: %v", err)
	}
	goPairs := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		goPairs = append(goPairs, fmt.Sprintf("%d/%s", pair.VendorID, pair.ProviderType))
	}

	if strings.Join(nodePairs, ",") != strings.Join(goPairs, ",") {
		t.Fatalf("与 Node 口径不一致：\n  Node=%v\n  Go  =%v", nodePairs, goPairs)
	}
	// 防空跑：生产库不可能一对都没有（库里有启用供应商）。
	if len(nodePairs) == 0 {
		t.Fatalf("对拍样本为空，说明查询或环境不对（不应在生产库语义下为空）")
	}
}

// TestSortedDashboardProviderTypesFollowsNodeOrder 是排序口径的纯函数用例（含空集与非 nil 保证）。
func TestSortedDashboardProviderTypesFollowsNodeOrder(t *testing.T) {
	cases := []struct {
		name string
		set  []string
		want []string
	}{
		{name: "全六类型按 Node 顺序", set: []string{"openai-compatible", "gemini", "gemini-cli", "codex", "claude-auth", "claude"},
			want: []string{"claude", "claude-auth", "codex", "gemini-cli", "gemini", "openai-compatible"}},
		{name: "未知类型排最后", set: []string{"openai-compatible", "zzz-unknown", "claude"},
			want: []string{"claude", "openai-compatible", "zzz-unknown"}},
		{name: "多个未知类型按字典序收敛", set: []string{"bbb", "aaa", "claude"},
			want: []string{"claude", "aaa", "bbb"}},
		{name: "单元素", set: []string{"gemini"}, want: []string{"gemini"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			set := make(map[string]struct{}, len(testCase.set))
			for _, value := range testCase.set {
				set[value] = struct{}{}
			}
			got := sortedDashboardProviderTypes(set)
			if strings.Join(got, ",") != strings.Join(testCase.want, ",") {
				t.Fatalf("got %v, want %v", got, testCase.want)
			}
		})
	}

	// 空集必须返回**非 nil** 的空切片：JSON 里要落成 []，不能是 null。
	empty := sortedDashboardProviderTypes(nil)
	if empty == nil {
		t.Fatalf("空集应返回非 nil 切片")
	}
	if len(empty) != 0 {
		t.Fatalf("空集应返回空切片，got %v", empty)
	}
}

// TestDashboardProviderTypeOrderMatchesNodeSchema 是**结构钉子**：顺序表必须与 Node 的
// `ProviderTypeSchema.options` 逐项一致。
//
// 基准自 2026-09 起是**冻结值**，不再是源码：node 退役删掉了该顺序表的源头
// `src/actions/provider-endpoints.ts`。冻结值逐字取自 tag `pre-node-removal`
// （commit `7f39ec64ca6ec66b10541b68b20753e49fef1a09`）的最后一份该文件的 `ProviderTypeSchema`，
// 即下面这个序列。
//
// 语义变化（登记）：本钉子由「对 **live Node schema** 断言」降为「对**冻结快照**断言」。
//   - 仍能抓：Go 顺序表被人改动、或与冻结序不一致。
//   - 不再能抓：Node 侧后来改了顺序（该文件已不存在，不可能再改）——今后顺序只由产品决策改，
//     改这里的冻结值就等于改契约。
//
// 为什么不能照 Go 常量的声明顺序断言：那会得到**相反**的相对次序（openai-compatible 在
// gemini 之前），而这是**肉眼看不出来**的退化——表单里的类型顺序错了不会报错，只是长期错着。
//
// 上限（有意）：Node 侧已不存在，「Node 新增类型要同步」这条提醒再也不会触发。
func TestDashboardProviderTypeOrderMatchesNodeSchema(t *testing.T) {
	// 冻结自 src/actions/provider-endpoints.ts 的 ProviderTypeSchema（见本测试头注释）。
	nodeOrder := []string{"claude", "claude-auth", "codex", "gemini-cli", "gemini", "openai-compatible"}

	goOrder := make([]string, 0, len(dashboardProviderTypeOrder))
	for value := range dashboardProviderTypeOrder {
		goOrder = append(goOrder, value)
	}
	sort.Slice(goOrder, func(left, right int) bool {
		return dashboardProviderTypeOrder[goOrder[left]] < dashboardProviderTypeOrder[goOrder[right]]
	})

	if strings.Join(nodeOrder, ",") != strings.Join(goOrder, ",") {
		t.Fatalf("顺序表与冻结的 Node 顺序不一致：\n  Node（冻结）=%v\n  Go        =%v\n（冻结值出处：tag pre-node-removal 的 src/actions/provider-endpoints.ts ProviderTypeSchema）",
			nodeOrder, goOrder)
	}
	if len(goOrder) != len(nodeOrder) {
		t.Fatalf("类型数量不一致：冻结值=%d Go=%d（新增供应商类型时要同步顺序表，并在此更新冻结值）", len(nodeOrder), len(goOrder))
	}
}
