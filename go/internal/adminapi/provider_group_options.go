package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 `GET /providers/groups` 的处理器——**不是** provider-groups 资源。
//
// 唯一真源：
//   - 路由与三分支：src/app/api/v1/resources/providers/handlers.ts:317-339（listProviderGroups）
//   - 业务语义：src/actions/providers.ts:473-508（getAvailableProviderGroups）
//     与 :511-538（getProviderGroupsWithCount）
//   - 查询 schema：src/lib/api/v1/schemas/providers.ts:349-355（ProviderGroupsQuerySchema）
//
// 三个分支（Node 原样）：
//
//	无查询 / ?userId=N  → `{"items": ["default", …]}`（**字符串数组**）
//	?include=count      → **裸数组** `[{"group": "…", "providerCount": N}]`
//
// 为什么单独一个文件：同一路径此前被挂在 provider-groups 资源的列表处理器上
// （providers.go 的路由指向 handleListProviderGroups），于是三条用例的形状全错
// ——两种响应都成了 provider_groups 表的对象数组。两个资源同名不同义，路由不能共用。

// providerGroupOptionsIncludeValues 是 ProviderGroupsQuerySchema 里 include 的枚举全集。
var providerGroupOptionsIncludeValues = []string{"count"}

// providerGroupOptionsListResponse 对应非 count 分支的 `{ items: string[] }`。
type providerGroupOptionsListResponse struct {
	Items []string `json:"items"`
}

// providerGroupCount 对应 count 分支的 `{ group: string, providerCount: number }`。
type providerGroupCount struct {
	Group         string `json:"group"`
	ProviderCount int    `json:"providerCount"`
}

// providerGroupOptionsQuery 是 ProviderGroupsQuerySchema 的解析结果。
type providerGroupOptionsQuery struct {
	Include string
	UserID  int64
	HasUser bool
}

// handleListProviderGroupOptions 复刻 listProviderGroups 的三分支。
func handleListProviderGroupOptions(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		query, issues := providerGroupOptionsParseQuery(request)
		if len(issues) > 0 {
			adminProblemWriter(deps).WriteValidationError(writer, request, issues)
			return
		}

		if query.Include == "count" {
			counts, err := providerGroupOptionsCounts(request.Context(), deps)
			if err != nil {
				adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
				return
			}
			adminWriteJSON(writer, http.StatusOK, counts)
			return
		}

		items, err := providerGroupOptionsForUser(request.Context(), deps, query)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		adminWriteJSON(writer, http.StatusOK, providerGroupOptionsListResponse{Items: items})
	}
}

// providerGroupOptionsParseQuery 复刻 ProviderGroupsQuerySchema 的解析与报错。
//
// 报错顺序按 schema 的键序（include 在前、userId 在后），与 zod 收集 issue 的顺序一致。
func providerGroupOptionsParseQuery(request *http.Request) (providerGroupOptionsQuery, []InvalidParam) {
	values := request.URL.Query()
	result := providerGroupOptionsQuery{}
	var issues []InvalidParam

	if raw := strings.TrimSpace(values.Get("include")); raw != "" {
		matched := false
		for _, candidate := range providerGroupOptionsIncludeValues {
			if candidate == raw {
				matched = true
			}
		}
		if matched {
			result.Include = raw
		} else {
			issues = append(issues, InvalidParam{
				Path: []any{"include"}, Code: "invalid_enum_value",
				Message: fmt.Sprintf("Invalid enum value. Expected %s, received %q",
					keysEnumLabel(providerGroupOptionsIncludeValues), raw),
			})
		}
	}

	if values.Has("userId") {
		raw := strings.TrimSpace(values.Get("userId"))
		switch {
		case raw == "":
			// zod 的 coerce 把空串变 0，再被 .positive() 打回。
			issues = append(issues, InvalidParam{
				Path: []any{"userId"}, Code: "too_small", Message: "Number must be greater than 0",
			})
		default:
			parsed, err := strconv.ParseFloat(raw, 64)
			switch {
			case err != nil:
				issues = append(issues, InvalidParam{
					Path: []any{"userId"}, Code: "invalid_type", Message: "Expected number, received nan",
				})
			case parsed != float64(int64(parsed)):
				issues = append(issues, InvalidParam{
					Path: []any{"userId"}, Code: "invalid_type", Message: "Expected integer, received float",
				})
			case int64(parsed) <= 0:
				issues = append(issues, InvalidParam{
					Path: []any{"userId"}, Code: "too_small", Message: "Number must be greater than 0",
				})
			default:
				result.UserID = int64(parsed)
				result.HasUser = true
			}
		}
	}

	if len(issues) > 0 {
		return result, issues
	}
	return result, nil
}

// providerGroupOptionsCounts 复刻 getProviderGroupsWithCount。
//
// 计数按 providers 逐行展开（同一供应商的 `a,b` 两个分组各记一次），展开语义来自
// resolveProviderGroupsWithDefault：NULL / 空串落回 ["default"]。排序同 Node：default 恒在最前，
// 其余按名字升序（Node 用 localeCompare，纯 ASCII 与 Go 的码点序一致）。
func providerGroupOptionsCounts(ctx context.Context, deps Deps) ([]providerGroupCount, error) {
	tags, err := deps.Store.AdminAllProviderGroupTags(ctx)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(tags))
	for _, tag := range tags {
		for _, group := range rfResolveGroupsWithDefault(tag) {
			if group != "" {
				counts[group]++
			}
		}
	}
	result := make([]providerGroupCount, 0, len(counts))
	for group, count := range counts {
		result = append(result, providerGroupCount{Group: group, ProviderCount: count})
	}
	sortProviderGroupCounts(result)
	return result, nil
}

// providerGroupOptionsForUser 复刻 getAvailableProviderGroups。
//
// 无 userId 时返回全部分组；有 userId 时先看该用户配置的 providerGroup：含通配符 `*` 则仍是全部，
// 否则只留用户可用的那几个——且 `default` 恒定出现在首位（Node 那句 `["default", …filtered]`）。
// 用户不存在时 Node 的 `user?.providerGroup || "default"` 落到 ["default"]，故只返 default。
func providerGroupOptionsForUser(
	ctx context.Context,
	deps Deps,
	query providerGroupOptionsQuery,
) ([]string, error) {
	tags, err := deps.Store.AdminDistinctProviderGroups(ctx)
	if err != nil {
		return nil, err
	}
	all := rfGroupOptions(tags)
	if !query.HasUser {
		return all, nil
	}

	userGroups := []string{rfDefaultGroup}
	user, lookupErr := deps.Store.FindAdminUserByID(ctx, query.UserID)
	if lookupErr != nil && lookupErr != store.ErrNotFound {
		return nil, lookupErr
	}
	if lookupErr == nil && user != nil {
		value := ""
		if user.ProviderGroup != nil {
			value = *user.ProviderGroup
		}
		if strings.TrimSpace(value) == "" {
			value = rfDefaultGroup
		}
		userGroups = rfSplitGroups(value)
	}

	allowed := make(map[string]struct{}, len(userGroups))
	hasWildcard := false
	for _, group := range userGroups {
		if group == providerGroupWildcard {
			hasWildcard = true
		}
		allowed[group] = struct{}{}
	}
	if hasWildcard {
		return all, nil
	}

	result := make([]string, 0, len(all))
	result = append(result, rfDefaultGroup)
	for _, option := range all {
		if option == rfDefaultGroup {
			continue
		}
		if _, ok := allowed[option]; ok {
			result = append(result, option)
		}
	}
	return result, nil
}

// providerGroupWildcard 是 PROVIDER_GROUP.ALL（src/lib/constants/provider.constants.ts:40）。
const providerGroupWildcard = "*"

// sortProviderGroupCounts 复刻 Node 的 default 优先 + 名字升序。
func sortProviderGroupCounts(items []providerGroupCount) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Group == rfDefaultGroup {
			return true
		}
		if items[j].Group == rfDefaultGroup {
			return false
		}
		return items[i].Group < items[j].Group
	})
}
