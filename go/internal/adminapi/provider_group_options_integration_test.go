package adminapi

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
)

// 本文件是 `GET /providers/groups` 三分支对着真 PG 的形状钉子（对拍缺陷 D1）。
//
// 断言纪律：库里同时存在生产数据与其它 lane 的夹具，故凡是「全量集合」的断言都退化成
// 「自己那一项在 / 形状对」——只有形状与自己的分组计数是精确断言。

// TestProviderGroupOptionsBranchesEndToEnd 钉住三分支：字符串数组 / 裸数组 / 用户过滤。
func TestProviderGroupOptionsBranchesEndToEnd(t *testing.T) {
	pools := meOpenPools(t)
	fixture := seedProviders(t, pools)
	router := providersRouter(t, pools, &Deps{})

	// 夹具里两个供应商的 group_tag 都是 fixture.prefix，故该分组的计数恒为 2。
	const expectedCount = 2

	callList := func(t *testing.T, target string) []string {
		t.Helper()
		recorder := providerRequest(t, router, "GET", target, "", "", false)
		if recorder.Code != 200 {
			t.Fatalf("%s 状态应为 200，实际 %d：%s", target, recorder.Code, recorder.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 应为对象：%v（正文 %s）", target, err, recorder.Body.String())
		}
		if keys := sortedJSONKeys(body); !sameKeys(keys, []string{"items"}) {
			t.Fatalf("%s 顶层键应只有 items，实际 %v", target, keys)
		}
		raw, ok := body["items"].([]any)
		if !ok {
			t.Fatalf("%s 的 items 应为数组，实际 %T", target, body["items"])
		}
		items := make([]string, 0, len(raw))
		for _, entry := range raw {
			text, ok := entry.(string)
			if !ok {
				t.Fatalf("%s 的 items 元素应为**字符串**，实际 %T（%v）", target, entry, entry)
			}
			items = append(items, text)
		}
		return items
	}

	callCount := func(t *testing.T) []map[string]any {
		t.Helper()
		target := "/providers/groups?include=count"
		recorder := providerRequest(t, router, "GET", target, "", "", false)
		if recorder.Code != 200 {
			t.Fatalf("%s 状态应为 200，实际 %d：%s", target, recorder.Code, recorder.Body.String())
		}
		// count 分支是**裸数组**：包进对象即回归。
		var body []map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 应为裸数组：%v（正文 %s）", target, err, recorder.Body.String())
		}
		for index, entry := range body {
			if keys := sortedJSONKeys(entry); !sameKeys(keys, []string{"group", "providerCount"}) {
				t.Fatalf("第 %d 项键集应为 [group providerCount]，实际 %v", index, keys)
			}
			if _, ok := entry["providerCount"].(float64); !ok {
				t.Fatalf("第 %d 项的 providerCount 应为数字，实际 %T", index, entry["providerCount"])
			}
		}
		return body
	}

	t.Run("无查询：字符串数组且以 default 打头", func(t *testing.T) {
		items := callList(t, "/providers/groups")
		if len(items) == 0 {
			t.Fatal("items 不应为空（库里至少有 default 与夹具分组）")
		}
		if items[0] != "default" {
			t.Errorf("首项应为 default（Node 的 allGroupsWithDefault 语义），实际 %q", items[0])
		}
		if !containsString(items, fixture.prefix) {
			t.Errorf("items 应含夹具分组 %q，实际 %v", fixture.prefix, items)
		}
		// 夹具建的两个 provider_groups 行**没有**供应商引用，故不属于「出现在 group_tag 里的分组」。
		for _, absent := range []string{fixture.prefix + "-a", fixture.prefix + "-b"} {
			if containsString(items, absent) {
				t.Errorf("%q 只存在于 provider_groups 表、未出现在任何 group_tag，不该出现在选项里", absent)
			}
		}
	})

	t.Run("include=count：裸数组含夹具分组的精确计数", func(t *testing.T) {
		body := callCount(t)
		if len(body) == 0 {
			t.Fatal("count 分支不应为空")
		}
		if body[0]["group"] != "default" {
			t.Errorf("default 应排在首位，实际首项 %v", body[0]["group"])
		}
		found := false
		for _, entry := range body {
			if entry["group"] != fixture.prefix {
				continue
			}
			found = true
			if count := entry["providerCount"].(float64); count != expectedCount {
				t.Errorf("%q 的 providerCount 应为 %d，实际 %v", fixture.prefix, expectedCount, count)
			}
		}
		if !found {
			t.Errorf("count 分支应含夹具分组 %q，实际 %+v", fixture.prefix, body)
		}
	})

	t.Run("userId 指定该分组的用户：只留自己那一项", func(t *testing.T) {
		userID := fixtureUser(t, pools, "user", true)
		pool, err := pools.Control()
		if err != nil {
			t.Fatalf("取控制分道失败: %v", err)
		}
		if _, err := pool.Exec(context.Background(),
			`UPDATE users SET provider_group = $1 WHERE id = $2`, fixture.prefix, userID); err != nil {
			t.Fatalf("设置用户分组失败: %v", err)
		}
		items := callList(t, "/providers/groups?userId="+strconv.Itoa(int(userID)))
		if !sameKeys(items, []string{"default", fixture.prefix}) {
			t.Errorf("应恰为 [default %s]，实际 %v", fixture.prefix, items)
		}
	})

	t.Run("userId 未配分组的用户：只有 default", func(t *testing.T) {
		userID := fixtureUser(t, pools, "user", true)
		pool, err := pools.Control()
		if err != nil {
			t.Fatalf("取控制分道失败: %v", err)
		}
		// 空串与 NULL 在 Node 的 `|| "default"` 下等价；这里用空串钉住那条分支。
		if _, err := pool.Exec(context.Background(),
			`UPDATE users SET provider_group = '' WHERE id = $1`, userID); err != nil {
			t.Fatalf("清空用户分组失败: %v", err)
		}
		items := callList(t, "/providers/groups?userId="+strconv.Itoa(int(userID)))
		if !sameKeys(items, []string{"default"}) {
			t.Errorf("应恰为 [default]，实际 %v", items)
		}
	})

	t.Run("userId 通配符用户：与无查询同形", func(t *testing.T) {
		userID := fixtureUser(t, pools, "user", true)
		pool, err := pools.Control()
		if err != nil {
			t.Fatalf("取控制分道失败: %v", err)
		}
		if _, err := pool.Exec(context.Background(),
			`UPDATE users SET provider_group = '*' WHERE id = $1`, userID); err != nil {
			t.Fatalf("设置通配符分组失败: %v", err)
		}
		items := callList(t, "/providers/groups?userId="+strconv.Itoa(int(userID)))
		if !containsString(items, fixture.prefix) {
			t.Errorf("通配符用户应看到全部分组（含 %q），实际 %v", fixture.prefix, items)
		}
	})

	t.Run("用户不存在：回落只有 default", func(t *testing.T) {
		// 一个必然不存在的 id：Node 的 `user?.providerGroup || "default"` 走 default 分支。
		items := callList(t, "/providers/groups?userId=2147483000")
		if !sameKeys(items, []string{"default"}) {
			t.Errorf("用户不存在时应恰为 [default]，实际 %v", items)
		}
	})

	t.Run("参数非法：400 且不动库", func(t *testing.T) {
		for _, target := range []string{
			"/providers/groups?include=items",
			"/providers/groups?userId=abc",
			"/providers/groups?userId=0",
		} {
			recorder := providerRequest(t, router, "GET", target, "", "", false)
			if recorder.Code != 400 {
				t.Errorf("%s 状态应为 400，实际 %d：%s", target, recorder.Code, recorder.Body.String())
				continue
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Errorf("%s 的 400 应是 JSON：%v", target, err)
				continue
			}
			if _, ok := body["invalidParams"].([]any); !ok {
				t.Errorf("%s 的 400 应带 invalidParams，实际 %v", target, body)
			}
		}
	})
}
