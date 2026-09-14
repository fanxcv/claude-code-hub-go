package adminapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"
)

// 本文件钉住 `GET /providers/groups` 的**三分支形状**（对拍缺陷 D1）。
//
// 为什么值得单独立钉子：这条路径与 `/provider-groups`（另一资源的对象列表）同名不同义，
// 曾因共用处理器而让三条对拍用例的形状全错。形状断言必须落到**键集 + 类型**，
// 只比状态码或「非空」抓不住这次回归。

// providerGroupOptionsKeys 是三分支各自允许的顶层键集。
var providerGroupOptionsKeys = map[string][]string{
	"list":  {"items"},
	"count": {},
}

// TestProviderGroupOptionsParseQuery 钉住 ProviderGroupsQuerySchema 的解析与报错文案。
func TestProviderGroupOptionsParseQuery(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		include    string
		userID     int64
		hasUser    bool
		issues     int
		issueCode  string
		issueField string
	}{
		{name: "无查询", target: "/providers/groups"},
		{name: "include=count", target: "/providers/groups?include=count", include: "count"},
		{name: "userId 正整数", target: "/providers/groups?userId=7", userID: 7, hasUser: true},
		{name: "userId 与 include 并存", target: "/providers/groups?userId=7&include=count",
			include: "count", userID: 7, hasUser: true},
		{name: "include 非法枚举", target: "/providers/groups?include=items",
			issues: 1, issueCode: "invalid_enum_value", issueField: "include"},
		{name: "userId 非数", target: "/providers/groups?userId=abc",
			issues: 1, issueCode: "invalid_type", issueField: "userId"},
		{name: "userId 空串", target: "/providers/groups?userId=",
			issues: 1, issueCode: "too_small", issueField: "userId"},
		{name: "userId 零", target: "/providers/groups?userId=0",
			issues: 1, issueCode: "too_small", issueField: "userId"},
		{name: "userId 负数", target: "/providers/groups?userId=-3",
			issues: 1, issueCode: "too_small", issueField: "userId"},
		{name: "userId 小数", target: "/providers/groups?userId=1.5",
			issues: 1, issueCode: "invalid_type", issueField: "userId"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptestNewRequest(testCase.target)
			query, issues := providerGroupOptionsParseQuery(request)
			if len(issues) != testCase.issues {
				t.Fatalf("issue 数应为 %d，实际 %d：%+v", testCase.issues, len(issues), issues)
			}
			if testCase.issues > 0 {
				if issues[0].Code != testCase.issueCode {
					t.Errorf("issue code 应为 %s，实际 %s", testCase.issueCode, issues[0].Code)
				}
				if len(issues[0].Path) != 1 || issues[0].Path[0] != testCase.issueField {
					t.Errorf("issue path 应为 [%s]，实际 %v", testCase.issueField, issues[0].Path)
				}
				return
			}
			if query.Include != testCase.include {
				t.Errorf("include 应为 %q，实际 %q", testCase.include, query.Include)
			}
			if query.HasUser != testCase.hasUser || query.UserID != testCase.userID {
				t.Errorf("userId 应为 (%v, %d)，实际 (%v, %d)",
					testCase.hasUser, testCase.userID, query.HasUser, query.UserID)
			}
		})
	}
}

// TestProviderGroupOptionsTopLevelShape 钉住三分支的顶层 JSON 形状（不碰库，用替身 store 不可行，
// 故这一条只解析静态样本，真实的形状由集成用例断言）。
//
// 存在的理由：`count` 分支是**裸数组**——若有人把它包进 `{items: …}`，
// 集成用例的键集断言也能抓到，但这条能在无库环境立刻红。
func TestProviderGroupOptionsTopLevelShape(t *testing.T) {
	listBody, err := json.Marshal(providerGroupOptionsListResponse{Items: []string{"default"}})
	if err != nil {
		t.Fatalf("序列化 list 分支失败: %v", err)
	}
	var listObject map[string]any
	if err := json.Unmarshal(listBody, &listObject); err != nil {
		t.Fatalf("list 分支应为对象: %v", err)
	}
	if keys := sortedJSONKeys(listObject); !sameKeys(keys, providerGroupOptionsKeys["list"]) {
		t.Errorf("list 分支键集应为 %v，实际 %v", providerGroupOptionsKeys["list"], keys)
	}

	countBody, err := json.Marshal([]providerGroupCount{{Group: "default", ProviderCount: 1}})
	if err != nil {
		t.Fatalf("序列化 count 分支失败: %v", err)
	}
	var countArray []map[string]any
	if err := json.Unmarshal(countBody, &countArray); err != nil {
		t.Fatalf("count 分支应为裸数组: %v（正文 %s）", err, countBody)
	}
	if len(countArray) != 1 {
		t.Fatalf("count 分支应含 1 项，实际 %d", len(countArray))
	}
	if keys := sortedJSONKeys(countArray[0]); !sameKeys(keys, []string{"group", "providerCount"}) {
		t.Errorf("count 分支项键集应为 [group providerCount]，实际 %v", keys)
	}
	if _, ok := countArray[0]["providerCount"].(float64); !ok {
		t.Errorf("providerCount 应为数字，实际 %T", countArray[0]["providerCount"])
	}
}

// sortedJSONKeys 取对象键的升序列表。
func sortedJSONKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// sameKeys 比较两个键列表是否逐项相同（都已升序）。
func sameKeys(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// httptestNewRequest 造一个只带 URL 的请求（解析函数只读 query）。
func httptestNewRequest(target string) *http.Request {
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		panic(err)
	}
	return request
}
