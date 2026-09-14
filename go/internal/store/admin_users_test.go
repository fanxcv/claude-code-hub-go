package store

import (
	"context"
	"encoding/json"

	"strings"
	"testing"
	"time"
)

// 本文件覆盖 users 资源的落库面：金额舍入、游标解析、补丁 SQL 形状（不需库）与
// 列表/创建/更新/软删/批量更新（门控 CCH_TEST_DSN，真库）。

// TestDecimalRound 钉住金额舍入（Node Decimal.toDecimalPlaces(6) 的 ROUND_HALF_UP 语义）。
func TestDecimalRound(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0", "0"},
		{"1.2345675", "1.234568"},
		{"1.2345674", "1.234567"},
		{"0.0000005", "0.000001"},
		{"0.0000004", "0"},
		{"10", "10"},
		{"-1.2345675", "-1.234568"},
		{"0.100000", "0.1"},
		{"9.9999999", "10"},
	}
	for _, testCase := range cases {
		got, err := decimalRound(testCase.in, 6)
		if err != nil {
			t.Fatalf("舍入 %s 失败: %v", testCase.in, err)
		}
		if got != testCase.want {
			t.Fatalf("舍入 %s 应为 %s，实际 %s", testCase.in, testCase.want, got)
		}
	}
	if got := roundCostText("1.2345675", 6); got.String() != "1.234568" {
		t.Fatalf("roundCostText 结果不符: %s", got.String())
	}
}

// TestParseKeysetCursor 钉住游标解析（长度上限、JSON 形状、正整数 id）。
func TestParseKeysetCursor(t *testing.T) {
	value, id, ok := parseKeysetCursor(`{"v":"2026-09-12T00:00:00.000Z","id":7}`)
	if !ok || id != 7 || value != "2026-09-12T00:00:00.000Z" {
		t.Fatalf("游标解析不符: %v %d %v", value, id, ok)
	}
	if _, _, ok := parseKeysetCursor("12"); ok {
		t.Fatal("纯数字游标不是 keyset 游标")
	}
	if _, _, ok := parseKeysetCursor(`{"v":"x","id":0}`); ok {
		t.Fatal("id 必须为正整数")
	}
	if _, _, ok := parseKeysetCursor(strings.Repeat("x", 2000)); ok {
		t.Fatal("超长游标应被拒")
	}
}

// TestAdminUserPatchSQL 钉住补丁 SQL：只出现被提供的列，恒写 updated_at，空补丁不产 SET。
func TestAdminUserPatchSQL(t *testing.T) {
	name := "新名字"
	names, values := AdminUserPatch{Name: &name}.columns()
	if len(names) != 1 || names[0] != "name" || values[0] != name {
		t.Fatalf("补丁列不符: %v / %v", names, values)
	}
	if !(AdminUserPatch{}).IsEmpty() {
		t.Fatal("零值补丁应为空补丁")
	}
	cost := "1.5"
	names, _ = AdminUserPatch{Limit5hUSD: &cost, Limit5hResetMode: ptrString("fixed")}.columns()
	if strings.Join(names, ",") != "limit_5h_usd,limit_5h_reset_mode" {
		t.Fatalf("列顺序不符: %v", names)
	}
	names, _ = AdminUserPatch{ClearExpiresAt: true}.columns()
	if strings.Join(names, ",") != "expires_at" {
		t.Fatalf("清空过期时间应写 expires_at: %v", names)
	}
}

func ptrString(value string) *string { return &value }

// TestAdminUserListAndMutationIntegration 真库覆盖列表筛选、创建、更新、软删与批量更新。
func TestAdminUserListAndMutationIntegration(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()
	marker := itKey(t) + "-users"

	created, err := pools.CreateAdminUser(ctx, AdminUserCreateInput{
		Name:             marker,
		Description:      "集成用例",
		Tags:             []string{marker + "-tag"},
		Limit5hResetMode: "rolling",
		DailyResetMode:   "fixed",
		DailyResetTime:   "00:00",
	})
	if err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	t.Cleanup(func() { cleanupAdminUser(t, pools, created.ID) })

	if created.ID == 0 || created.Name != marker {
		t.Fatalf("创建结果不符: %+v", created)
	}
	if created.Tags == nil || len(created.Tags) != 1 {
		t.Fatalf("tags 应为数组: %+v", created.Tags)
	}

	shaped := created.NodeJSON()
	if shaped["description"] != "集成用例" || shaped["limit5hResetMode"] != "rolling" {
		t.Fatalf("NodeJSON 整形不符: %v", shaped)
	}
	if shaped["allowedClients"] == nil {
		t.Fatal("allowedClients 应为空数组而不是 null")
	}

	list, err := pools.ListAdminUsers(ctx, AdminUserListFilters{
		Limit: 5, SearchTerm: marker, TagFilters: []string{marker + "-tag"},
	})
	if err != nil {
		t.Fatalf("列表查询失败: %v", err)
	}
	if len(list.Users) != 1 || list.Users[0].ID != created.ID {
		t.Fatalf("列表未命中夹具: %+v", list)
	}

	// 状态筛选按夹具自证：同一标记下造「启用 + 停用」各一条，断言两种筛选各只命中自己那条。
	//
	// 旧写法查的是「全库 5 条 enabled，再看新建用户是否在其中」——而默认排序是 createdAt asc
	// （Node 同，src/repository/user.ts:207-208），共享库里只要有 5 条更早的行，新建用户就永远
	// 进不了第一页，用例与生产行为都正常却长期假红。改成夹具内自证后既与库体量无关，也比
	// 「第一页里看见」更强：筛选写错时（如忽略 is_enabled）两条断言必有一条不成立。
	disabledUser, err := pools.CreateAdminUser(ctx, AdminUserCreateInput{Name: marker + "-disabled"})
	if err != nil {
		t.Fatalf("创建停用夹具失败: %v", err)
	}
	t.Cleanup(func() { cleanupAdminUser(t, pools, disabledUser.ID) })
	disabled := false
	if ok, err := pools.UpdateAdminUser(ctx, disabledUser.ID, AdminUserPatch{IsEnabled: &disabled}); err != nil || !ok {
		t.Fatalf("停用夹具失败: %v %v", ok, err)
	}

	enabled, err := pools.ListAdminUsers(ctx, AdminUserListFilters{
		Limit: 5, SearchTerm: marker, StatusFilter: "enabled",
	})
	if err != nil {
		t.Fatalf("状态筛选失败: %v", err)
	}
	if len(enabled.Users) != 1 || enabled.Users[0].ID != created.ID {
		t.Fatalf("enabled 筛选应只命中启用夹具，得到 %+v", enabled.Users)
	}

	disabledList, err := pools.ListAdminUsers(ctx, AdminUserListFilters{
		Limit: 5, SearchTerm: marker, StatusFilter: "disabled",
	})
	if err != nil {
		t.Fatalf("停用筛选失败: %v", err)
	}
	if len(disabledList.Users) != 1 || disabledList.Users[0].ID != disabledUser.ID {
		t.Fatalf("disabled 筛选应只命中停用夹具，得到 %+v", disabledList.Users)
	}

	patch := AdminUserPatch{RPM: ptrInt64(42)}
	updated, err := pools.UpdateAdminUser(ctx, created.ID, patch)
	if err != nil || !updated {
		t.Fatalf("更新失败: %v %v", updated, err)
	}
	readBack, err := pools.FindAdminUserByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if readBack.RPM == nil || *readBack.RPM != 42 {
		t.Fatalf("rpm 未落库: %+v", readBack.RPM)
	}

	// 空补丁等价于读回（Node 同），不报错、命中行即 true。
	if ok, err := pools.UpdateAdminUser(ctx, created.ID, AdminUserPatch{}); err != nil || !ok {
		t.Fatalf("空补丁应命中但无写入: %v %v", ok, err)
	}
	if ok, err := pools.UpdateAdminUser(ctx, 2147483000, AdminUserPatch{}); err != nil || ok {
		t.Fatalf("不存在的 id 应 false: %v %v", ok, err)
	}

	// 批量更新：存在全部 → 成功；有缺失 → 回滚且不给 updated。
	second, err := pools.CreateAdminUser(ctx, AdminUserCreateInput{Name: marker + "-b"})
	if err != nil {
		t.Fatalf("创建第二个用户失败: %v", err)
	}
	t.Cleanup(func() { cleanupAdminUser(t, pools, second.ID) })

	updatedIDs, missing, err := pools.BatchUpdateAdminUsers(ctx, []int64{created.ID, second.ID},
		AdminUserBatchPatch{RPM: ptrInt64(7)})
	if err != nil || len(missing) != 0 || len(updatedIDs) != 2 {
		t.Fatalf("批量更新失败: %v %v %v", updatedIDs, missing, err)
	}
	_, missing, err = pools.BatchUpdateAdminUsers(ctx, []int64{created.ID, 2147483000},
		AdminUserBatchPatch{RPM: ptrInt64(9)})
	if err != nil || len(missing) != 1 {
		t.Fatalf("缺失 id 应被报出: %v %v", missing, err)
	}
	after, err := pools.FindAdminUserByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if after.RPM == nil || *after.RPM != 7 {
		t.Fatalf("缺失 id 时不应写入: %+v", after.RPM)
	}

	// 软删：命中后查询不到，但行仍在（账本与审计要能引用它）。
	deleted, err := pools.SoftDeleteAdminUser(ctx, created.ID)
	if err != nil || !deleted {
		t.Fatalf("软删失败: %v %v", deleted, err)
	}
	if _, err := pools.FindAdminUserByID(ctx, created.ID); err == nil {
		t.Fatal("软删后不应再被读到")
	}
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	var deletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM users WHERE id = $1`, created.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("读回软删标记失败: %v", err)
	}
	if deletedAt == nil {
		t.Fatal("软删应写 deleted_at")
	}
}

// TestAdminUserKeyUsageIntegration 真库覆盖密钥聚合：当日用量、调用数、最近使用与模型统计。
func TestAdminUserKeyUsageIntegration(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()
	marker := itKey(t) + "-usage"

	user, err := pools.CreateAdminUser(ctx, AdminUserCreateInput{Name: marker})
	if err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	t.Cleanup(func() { cleanupAdminUser(t, pools, user.ID) })

	keyValue := marker + "-key"
	keyID, err := pools.CreateAdminUserDefaultKey(ctx, user.ID, keyValue, "default", nil)
	if err != nil {
		t.Fatalf("创建密钥失败: %v", err)
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	model := "go-store-it-model"
	_, err = pool.Exec(ctx,
		`INSERT INTO usage_ledger (
			request_id, user_id, key, provider_id, final_provider_id, is_success, is_replay,
			model, cost_usd, input_tokens, output_tokens, created_at
		) VALUES ($1, $2, $3, 0, 0, true, false, $4, 1.2345675::numeric, 10, 20, now())`,
		int32(1_000_000_000+time.Now().UnixNano()%1_000_000_000), user.ID, keyValue, model)
	if err != nil {
		t.Fatalf("写账本失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM usage_ledger WHERE key = $1`, keyValue)
	})

	keys, err := pools.ListAdminUserKeys(ctx, []int64{user.ID})
	if err != nil {
		t.Fatalf("取密钥失败: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != keyID {
		t.Fatalf("密钥夹具不符: %+v", keys)
	}
	usage, err := pools.LoadAdminUserKeyUsage(ctx, keys)
	if err != nil {
		t.Fatalf("取用量失败: %v", err)
	}
	entry := usage[keyID]
	if entry.Today.TotalCost.String() != "1.234568" {
		t.Fatalf("当日成本应按 6 位舍入，实际 %s", entry.Today.TotalCost.String())
	}
	if entry.Statistics.TodayCallCount != 1 {
		t.Fatalf("当日调用数应为 1，实际 %d", entry.Statistics.TodayCallCount)
	}
	if entry.Statistics.LastUsedAt == nil {
		t.Fatal("最近使用时间应被读到")
	}
	if len(entry.Statistics.ModelStats) != 1 || entry.Statistics.ModelStats[0].Model != model {
		t.Fatalf("模型统计不符: %+v", entry.Statistics.ModelStats)
	}
}

// TestAdminUserNodeJSONShape 钉住 toUser 的默认值语义（不需要库）。
func TestAdminUserNodeJSONShape(t *testing.T) {
	role := "user"
	shaped := AdminUserRow{
		ID: 7, Name: "u", Role: &role, CreatedAt: time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC),
		UpdatedAt: time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC),
	}.NodeJSON()
	if shaped["description"] != "" {
		t.Fatalf("description 空值应为空串: %v", shaped["description"])
	}
	if shaped["rpm"] != nil || shaped["dailyQuota"] != nil {
		t.Fatalf("rpm/dailyQuota 空值应为 null: %v %v", shaped["rpm"], shaped["dailyQuota"])
	}
	if shaped["limit5hResetMode"] != "rolling" || shaped["dailyResetMode"] != "fixed" ||
		shaped["dailyResetTime"] != "00:00" || shaped["isEnabled"] != true {
		t.Fatalf("默认值不符: %v", shaped)
	}
	if shaped["createdAt"] != "2026-09-12T01:02:03.000Z" {
		t.Fatalf("时间应输出 Node 的毫秒三位 ISO，实际 %v", shaped["createdAt"])
	}
	if tags, ok := shaped["tags"].([]string); !ok || len(tags) != 0 {
		t.Fatalf("tags 应为空数组: %v", shaped["tags"])
	}
	encoded, err := json.Marshal(shaped)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(encoded), `"tags":[]`) {
		t.Fatalf("空数组不应序列化成 null: %s", encoded)
	}
}

// cleanupAdminUser 硬删测试用户与其密钥、账本行（夹具自清）。
func cleanupAdminUser(t *testing.T, pools *Pools, userID int64) {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		return
	}
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM usage_ledger WHERE user_id = $1`, userID)
	_, _ = pool.Exec(ctx, `DELETE FROM keys WHERE user_id = $1`, userID)
	_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
}

func ptrInt64(value int64) *int64 { return &value }
