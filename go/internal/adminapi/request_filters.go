package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 request-filters 资源模块（批次 A / lane A1-4，7 条端点）。
//
// 唯一真源：
//   - 路由与状态码：src/app/api/v1/resources/request-filters/handlers.ts
//   - 业务规则：src/actions/request-filters.ts（Go 侧的校验移植在 request_filters_shared.go）
//   - SQL：src/repository/request-filters.ts（Go 侧在 internal/store/admin_request_filters.go）
//
// 七条端点：
//
//	GET    /request-filters                     列表（含禁用行，创建时间倒序）
//	POST   /request-filters                     创建（201 + Location）
//	POST   /request-filters/cache:refresh       重载并广播失效，返回 {count}
//	GET    /request-filters/options/providers   绑定选择器：供应商选项
//	GET    /request-filters/options/groups      绑定选择器：分组选项
//	PATCH  /request-filters/{id}                部分更新
//	DELETE /request-filters/{id}                删除（204）
//
// 三条登记进对拍白名单的差异（都写在对应函数注释里）：
//  1. ReDoS 判定用 Go 的 RE2（见 request_filters_shared.go 文件头）。
//  2. cache:refresh 的 count 由「启用行数」现算；Node 报的是引擎装载后的计数（同一行集）。
//  3. 状态码按显式错误码映射（request_filter.not_found / action_failed），不移植 Node 的
//     `detail.includes("不存在")` 子串判定——本模块的 action 文案是仓库里的中文字面量
//     （不随 locale 变），故两端状态码仍然一致。
//
// 审计：与 error-rules 同一结论——Node 侧本模块的写路径一条审计都不写（§3.3-11），只有 cache
// 失效广播（emitRequestFiltersUpdated），Go 侧照此。

// rfFilterPayload 是响应体里的一个过滤器（逐字对应 Node 的 RequestFilterSchema）。
type rfFilterPayload struct {
	ID             int64           `json:"id"`
	Name           string          `json:"name"`
	Description    *string         `json:"description"`
	Scope          string          `json:"scope"`
	Action         string          `json:"action"`
	MatchType      *string         `json:"matchType"`
	Target         string          `json:"target"`
	Replacement    json.RawMessage `json:"replacement"`
	Priority       int             `json:"priority"`
	IsEnabled      bool            `json:"isEnabled"`
	BindingType    string          `json:"bindingType"`
	ProviderIDs    json.RawMessage `json:"providerIds"`
	GroupTags      json.RawMessage `json:"groupTags"`
	RuleMode       string          `json:"ruleMode"`
	ExecutionPhase string          `json:"executionPhase"`
	Operations     json.RawMessage `json:"operations"`
	CreatedAt      string          `json:"createdAt"`
	UpdatedAt      string          `json:"updatedAt"`
}

// rfFilterListResponse 对应 `jsonResponse({ items: result.data })`。
type rfFilterListResponse struct {
	Items []rfFilterPayload `json:"items"`
}

// rfCacheRefreshResponse 对应 refreshRequestFiltersCache 的 `{ count }`。
type rfCacheRefreshResponse struct {
	Count int64 `json:"count"`
}

// rfProviderOptionsResponse 对应 `{ items: [{ id, name }] }`。
type rfProviderOptionsResponse struct {
	Items []store.AdminProviderOption `json:"items"`
}

// rfGroupOptionsResponse 对应 `{ items: string[] }`。
type rfGroupOptionsResponse struct {
	Items []string `json:"items"`
}

// RegisterRequestFilters 注册本模块的七条路由。
//
// 路径参数写作 `{id:[1-9][0-9]*}` 的理由同 error-rules（见 error_rules.go 的 RegisterErrorRules）：
// 非法 id 的请求原样回退 Node，由 Node 用 zod 的形状作答。
func RegisterRequestFilters(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_request_filters_store_unwired", map[string]any{
				"module": "request-filters",
				"action": "routes_not_registered",
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/request-filters",
		Access:      AccessAdmin,
		Module:      "request-filters",
		OperationID: "listRequestFilters",
		Handler:     http.HandlerFunc(handleListRequestFilters(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/request-filters",
		Access:      AccessAdmin,
		Module:      "request-filters",
		OperationID: "createRequestFilter",
		Handler:     http.HandlerFunc(handleCreateRequestFilter(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/request-filters/cache:refresh",
		Access:      AccessAdmin,
		Module:      "request-filters",
		OperationID: "refreshRequestFiltersCache",
		Handler:     http.HandlerFunc(handleRefreshRequestFiltersCache(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/request-filters/options/providers",
		Access:      AccessAdmin,
		Module:      "request-filters",
		OperationID: "listProviderOptions",
		Handler:     http.HandlerFunc(handleListProviderOptions(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/request-filters/options/groups",
		Access:      AccessAdmin,
		Module:      "request-filters",
		OperationID: "listGroupOptions",
		Handler:     http.HandlerFunc(handleListGroupOptions(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPatch,
		Path:        "/request-filters/{id:[1-9][0-9]*}",
		Access:      AccessAdmin,
		Module:      "request-filters",
		OperationID: "updateRequestFilter",
		Handler:     http.HandlerFunc(handleUpdateRequestFilter(deps)),
	})
	router.Add(Route{
		Method:      http.MethodDelete,
		Path:        "/request-filters/{id:[1-9][0-9]*}",
		Access:      AccessAdmin,
		Module:      "request-filters",
		OperationID: "deleteRequestFilter",
		Handler:     http.HandlerFunc(handleDeleteRequestFilter(deps)),
	})
}

// handleListRequestFilters 复刻 listRequestFilters（handlers.ts:21-26）。
//
// Node 侧 action 在非 admin 时返回空数组；路由的 access 已是 admin 档位，故那条分支不存在。
func handleListRequestFilters(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		filters, err := deps.Store.AdminListRequestFilters(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, rfFailure(err))
			return
		}
		items := make([]rfFilterPayload, 0, len(filters))
		for _, filter := range filters {
			items = append(items, rfToPayload(filter))
		}
		adminWriteJSON(writer, http.StatusOK, rfFilterListResponse{Items: items})
	}
}

// handleCreateRequestFilter 复刻 createRequestFilter（handlers.ts:28-40）。
func handleCreateRequestFilter(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := a14ObjectFields(fields, "name", "description", "scope", "action", "target",
			"matchType", "replacement", "priority", "isEnabled", "bindingType", "providerIds",
			"groupTags", "ruleMode", "executionPhase", "operations")
		object.RejectUnknownKeys()
		name, _ := object.String("name", adminStringSpec{
			Required: true, Trim: true, MinRunes: 1, MaxRunes: 100,
		})
		description, hasDescription := object.String("description", adminStringSpec{
			Trim: true, MaxRunes: 500,
		})
		scope, _ := object.String("scope", adminStringSpec{Required: true, Enum: rfScopes})
		action, _ := object.String("action", adminStringSpec{Required: true, Enum: rfActions})
		target, _ := object.String("target", adminStringSpec{Required: true, Trim: true, MaxRunes: 500})
		matchTypeRaw, _ := a14JSONField(object, "matchType", a14ShapeEnum, true, rfMatchTypes)
		replacement, _ := a14JSONField(object, "replacement", a14ShapeAny, true, nil)
		priority, hasPriority := a14IntField(object, "priority", a14IntSpec{})
		isEnabled, hasIsEnabled := object.Bool("isEnabled")
		bindingType, hasBindingType := object.String("bindingType", adminStringSpec{Enum: rfBindingTypes})
		providerIDs, _ := a14JSONField(object, "providerIds", a14ShapeIntArray, true, nil)
		groupTags, _ := a14JSONField(object, "groupTags", a14ShapeStringArray, true, nil)
		ruleMode, hasRuleMode := object.String("ruleMode", adminStringSpec{Enum: rfRuleModes})
		executionPhase, hasExecutionPhase := object.String("executionPhase", adminStringSpec{
			Enum: rfExecutionPhases,
		})
		operations, _ := a14JSONField(object, "operations", a14ShapeRecordArray, true, nil)
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		matchType := rfNullableString(matchTypeRaw)
		payload := rfPayload{
			Name:        name,
			Scope:       scope,
			Action:      action,
			Target:      target,
			MatchType:   matchType,
			BindingType: optionalString(bindingType, hasBindingType),
			ProviderIDs: providerIDs,
			GroupTags:   groupTags,
			RuleMode:    optionalString(ruleMode, hasRuleMode),
			Operations:  operations,
		}
		if reason := rfValidatePayload(payload); reason != "" {
			adminProblemWriter(deps).WriteActionError(writer, request, rfActionFailure(reason))
			return
		}
		effectiveRuleMode := "simple"
		if hasRuleMode {
			effectiveRuleMode = ruleMode
		}
		effectivePhase := "guard"
		if hasExecutionPhase {
			effectivePhase = executionPhase
		}
		if reason := rfValidateAdvancedPhase(effectiveRuleMode, effectivePhase); reason != "" {
			adminProblemWriter(deps).WriteActionError(writer, request, rfActionFailure(reason))
			return
		}

		// 复刻 createRequestFilterAction 组装仓储入参时的默认值与 trim。
		input := store.AdminCreateRequestFilterInput{
			Name:           name,
			Scope:          scope,
			Action:         action,
			Target:         target,
			MatchType:      matchType,
			Replacement:    rfNullableJSON(replacement),
			Priority:       0,
			IsEnabled:      true,
			BindingType:    "global",
			ProviderIDs:    rfNullableJSON(providerIDs),
			GroupTags:      rfNullableJSON(groupTags),
			RuleMode:       effectiveRuleMode,
			ExecutionPhase: effectivePhase,
			Operations:     rfNullableJSON(operations),
		}
		if hasDescription {
			input.Description = &description
		}
		if hasPriority && priority != nil {
			input.Priority = *priority
		}
		if hasIsEnabled && isEnabled != nil {
			input.IsEnabled = *isEnabled
		}
		if hasBindingType {
			input.BindingType = bindingType
		}

		created, err := deps.Store.AdminCreateRequestFilter(request.Context(), input)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, rfFailure(err))
			return
		}

		// Node 侧 create → emitRequestFiltersUpdated（本进程 reload + Redis 广播）；Go 侧即广播 cfgsync 域。
		rfPublishRequestFilters(deps, request)

		adminWriteCreated(writer,
			fmt.Sprintf("%s/request-filters/%d", MountPrefix, created.ID),
			rfToPayload(created))
	}
}

// handleUpdateRequestFilter 复刻 updateRequestFilter（handlers.ts:42-56）。
//
// 校验链的顺序与 Node 逐条一致（advanced/guard → operations → ReDoS → 绑定约束），因为不同顺序
// 会让「同时有多个错误」的请求拿到不同的 400 文案。
func handleUpdateRequestFilter(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := a14PathID(request)
		if !ok {
			return
		}
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := a14ObjectFields(fields, "name", "description", "scope", "action", "target",
			"matchType", "replacement", "priority", "isEnabled", "bindingType", "providerIds",
			"groupTags", "ruleMode", "executionPhase", "operations")
		object.RejectUnknownKeys()
		name, hasName := object.String("name", adminStringSpec{Trim: true, MinRunes: 1, MaxRunes: 100})
		description, hasDescription := object.String("description", adminStringSpec{
			Trim: true, MaxRunes: 500,
		})
		scope, hasScope := object.String("scope", adminStringSpec{Enum: rfScopes})
		action, hasAction := object.String("action", adminStringSpec{Enum: rfActions})
		target, hasTarget := object.String("target", adminStringSpec{Trim: true, MaxRunes: 500})
		matchTypeRaw, hasMatchType := a14JSONField(object, "matchType", a14ShapeEnum, true, rfMatchTypes)
		replacement, hasReplacement := a14JSONField(object, "replacement", a14ShapeAny, true, nil)
		priority, hasPriority := a14IntField(object, "priority", a14IntSpec{})
		isEnabled, hasIsEnabled := object.Bool("isEnabled")
		bindingType, hasBindingType := object.String("bindingType", adminStringSpec{Enum: rfBindingTypes})
		providerIDs, hasProviderIDs := a14JSONField(object, "providerIds", a14ShapeIntArray, true, nil)
		groupTags, hasGroupTags := a14JSONField(object, "groupTags", a14ShapeStringArray, true, nil)
		ruleMode, hasRuleMode := object.String("ruleMode", adminStringSpec{Enum: rfRuleModes})
		executionPhase, hasExecutionPhase := object.String("executionPhase", adminStringSpec{
			Enum: rfExecutionPhases,
		})
		operations, hasOperations := a14JSONField(object, "operations", a14ShapeRecordArray, true, nil)
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		// needsExisting：只有这几类更新需要先取现状（Node 侧同一个判定）。
		needsExisting := hasRuleMode || hasOperations || hasExecutionPhase || hasTarget ||
			hasMatchType || hasAction || hasBindingType || hasProviderIDs || hasGroupTags
		var existing *store.AdminRequestFilter
		if needsExisting {
			current, err := deps.Store.AdminGetRequestFilterByID(request.Context(), id)
			if err == store.ErrNotFound {
				adminProblemWriter(deps).WriteActionError(writer, request, rfNotFound())
				return
			}
			if err != nil {
				adminProblemWriter(deps).WriteActionError(writer, request, rfFailure(err))
				return
			}
			existing = current
		}

		matchType := rfNullableString(matchTypeRaw)

		// 1) 高级模式只支持 final 阶段（取「生效后」的两个值，`??` 语义：null 视作未提供）。
		if hasRuleMode || hasExecutionPhase {
			effectiveRuleMode := rfEffectiveText(ruleMode, hasRuleMode, existing.RuleMode)
			effectivePhase := rfEffectiveText(executionPhase, hasExecutionPhase, existing.ExecutionPhase)
			if reason := rfValidateAdvancedPhase(effectiveRuleMode, effectivePhase); reason != "" {
				adminProblemWriter(deps).WriteActionError(writer, request, rfActionFailure(reason))
				return
			}
		}

		// 2) ruleMode 或 operations 变了就要重验 operations。
		if hasRuleMode || hasOperations {
			effectiveRuleMode := rfEffectiveText(ruleMode, hasRuleMode, existing.RuleMode)
			if effectiveRuleMode == "advanced" {
				effectiveOperations := existing.Operations
				if hasOperations {
					effectiveOperations = operations
				}
				if reason := rfValidateOperations(effectiveOperations); reason != "" {
					adminProblemWriter(deps).WriteActionError(writer, request, rfActionFailure(reason))
					return
				}
			}
		}

		// 3) ReDoS：target/matchType/action 任一变化都要按**生效后**的取值重验，否则可以绕过
		//    （例如先把 action 改成 text_replace、再单独把 matchType 改成 regex）。
		if hasTarget || hasMatchType || hasAction {
			effectiveTarget := rfEffectiveText(target, hasTarget, existing.Target)
			// 这里是 `??` 语义：显式传 null 的 matchType 回落到现状值。
			effectiveMatchType := ""
			if hasMatchType && matchType != nil {
				effectiveMatchType = *matchType
			} else if existing.MatchType != nil {
				effectiveMatchType = *existing.MatchType
			}
			effectiveAction := rfEffectiveText(action, hasAction, existing.Action)
			if effectiveAction == "text_replace" && effectiveMatchType == "regex" && effectiveTarget != "" {
				if _, err := regexp.Compile(effectiveTarget); err != nil {
					adminProblemWriter(deps).WriteActionError(writer, request,
						rfActionFailure("正则表达式存在 ReDoS 风险"))
					return
				}
			}
		}

		// 4) 绑定约束：用**生效后**的整组字段重跑 validatePayload（Node 侧也是整组重跑，
		//    因为 bindingType 与 providerIds/groupTags 是互相约束的）。
		if hasBindingType || hasProviderIDs || hasGroupTags {
			// 这一段 Node 用的是 `!== undefined` 语义：显式 null 就是 null，不回落到现状值。
			effectiveName := existing.Name
			if hasName {
				effectiveName = name
			}
			effectiveTarget := existing.Target
			if hasTarget {
				effectiveTarget = target
			}
			effectiveBindingType := existing.BindingType
			if hasBindingType {
				effectiveBindingType = bindingType
			}
			effectiveProviderIDs := existing.ProviderIDs
			if hasProviderIDs {
				effectiveProviderIDs = providerIDs
			}
			effectiveGroupTags := existing.GroupTags
			if hasGroupTags {
				effectiveGroupTags = groupTags
			}
			effectiveRuleMode := existing.RuleMode
			if hasRuleMode {
				effectiveRuleMode = ruleMode
			}
			effectiveOperations := existing.Operations
			if hasOperations {
				effectiveOperations = operations
			}
			reason := rfValidatePayload(rfPayload{
				Name:        effectiveName,
				Scope:       existing.Scope,
				Action:      existing.Action,
				Target:      effectiveTarget,
				BindingType: &effectiveBindingType,
				ProviderIDs: effectiveProviderIDs,
				GroupTags:   effectiveGroupTags,
				RuleMode:    &effectiveRuleMode,
				Operations:  effectiveOperations,
			})
			if reason != "" {
				adminProblemWriter(deps).WriteActionError(writer, request, rfActionFailure(reason))
				return
			}
		}

		update := store.AdminRequestFilterUpdate{}
		if hasName {
			update.Name = &name
		}
		if hasDescription {
			update.Description = store.AdminNullableText{Present: true, Value: &description}
		}
		if hasScope {
			update.Scope = &scope
		}
		if hasAction {
			update.Action = &action
		}
		if hasMatchType {
			update.MatchType = store.AdminNullableText{Present: true, Value: matchType}
		}
		if hasTarget {
			update.Target = &target
		}
		if hasReplacement {
			update.Replacement = store.AdminNullableJSON{Present: true, Raw: rfNullableJSON(replacement)}
		}
		if hasPriority {
			update.Priority = priority
		}
		if hasIsEnabled {
			update.IsEnabled = isEnabled
		}
		if hasBindingType {
			update.BindingType = &bindingType
		}
		if hasProviderIDs {
			update.ProviderIDs = store.AdminNullableJSON{Present: true, Raw: rfNullableJSON(providerIDs)}
		}
		if hasGroupTags {
			update.GroupTags = store.AdminNullableJSON{Present: true, Raw: rfNullableJSON(groupTags)}
		}
		if hasRuleMode {
			update.RuleMode = &ruleMode
		}
		if hasExecutionPhase {
			update.ExecutionPhase = &executionPhase
		}
		if hasOperations {
			update.Operations = store.AdminNullableJSON{Present: true, Raw: rfNullableJSON(operations)}
		}

		updated, err := deps.Store.AdminUpdateRequestFilter(request.Context(), id, update)
		if err == store.ErrNotFound {
			adminProblemWriter(deps).WriteActionError(writer, request, rfNotFound())
			return
		}
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, rfFailure(err))
			return
		}

		rfPublishRequestFilters(deps, request)
		adminWriteJSON(writer, http.StatusOK, rfToPayload(updated))
	}
}

// handleDeleteRequestFilter 复刻 deleteRequestFilter（handlers.ts:58-71）。
func handleDeleteRequestFilter(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := a14PathID(request)
		if !ok {
			return
		}
		deleted, err := deps.Store.AdminDeleteRequestFilter(request.Context(), id)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, rfFailure(err))
			return
		}
		if !deleted {
			adminProblemWriter(deps).WriteActionError(writer, request, rfNotFound())
			return
		}
		rfPublishRequestFilters(deps, request)
		adminWriteNoContent(writer)
	}
}

// handleRefreshRequestFiltersCache 复刻 refreshRequestFiltersCache（actions/request-filters.ts:439-456）。
//
// Node 做的是「引擎 reload 后报 getStats().count」；Go 侧没有常驻引擎，count 就是启用行数
// （引擎把这批行恰好分到四个桶，故两者相等），并广播 cfgsync 域让 guard 的过滤器快照失效。
func handleRefreshRequestFiltersCache(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		count, err := deps.Store.AdminCountActiveRequestFilters(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, rfFailure(err))
			return
		}
		rfPublishRequestFilters(deps, request)
		adminWriteJSON(writer, http.StatusOK, rfCacheRefreshResponse{Count: count})
	}
}

// handleListProviderOptions 复刻 listProviderOptions（handlers.ts:80-85）。
func handleListProviderOptions(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		options, err := deps.Store.AdminListProviderOptions(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, rfFailure(err))
			return
		}
		if options == nil {
			options = []store.AdminProviderOption{}
		}
		adminWriteJSON(writer, http.StatusOK, rfProviderOptionsResponse{Items: options})
	}
}

// handleListGroupOptions 复刻 listGroupOptions（handlers.ts:87-94）。
func handleListGroupOptions(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		tags, err := deps.Store.AdminDistinctProviderGroups(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, rfFailure(err))
			return
		}
		adminWriteJSON(writer, http.StatusOK, rfGroupOptionsResponse{Items: rfGroupOptions(tags)})
	}
}

// rfToPayload 把一行映射成响应体；可空时间列按 Node 的 `?? new Date()` 兜底。
func rfToPayload(filter store.AdminRequestFilter) rfFilterPayload {
	return rfFilterPayload{
		ID:             filter.ID,
		Name:           filter.Name,
		Description:    filter.Description,
		Scope:          filter.Scope,
		Action:         filter.Action,
		MatchType:      filter.MatchType,
		Target:         filter.Target,
		Replacement:    filter.Replacement,
		Priority:       filter.Priority,
		IsEnabled:      filter.IsEnabled,
		BindingType:    filter.BindingType,
		ProviderIDs:    filter.ProviderIDs,
		GroupTags:      filter.GroupTags,
		RuleMode:       filter.RuleMode,
		ExecutionPhase: filter.ExecutionPhase,
		Operations:     filter.Operations,
		CreatedAt:      adminStringOrNow(filter.CreatedAt),
		UpdatedAt:      adminStringOrNow(filter.UpdatedAt),
	}
}

// rfNotFound 是「记录不存在」的 action 错误（Node 文案：记录不存在 → 404 request_filter.not_found）。
func rfNotFound() *ActionError {
	return NewActionError("request_filter", "request_filter.not_found", http.StatusNotFound,
		fmt.Errorf("记录不存在"))
}

// rfActionFailure 把业务校验失败映射成 action 错误：Node 侧这类失败一律 400 + action_failed。
func rfActionFailure(reason string) *ActionError {
	return NewActionError("request_filter", "request_filter.action_failed", http.StatusBadRequest,
		fmt.Errorf("%s", reason))
}

// rfFailure 把存储层失败映射成 action 错误（同 error-rules：Node 侧 catch 分支也是 400）。
func rfFailure(err error) *ActionError {
	return adminActionFailure("request_filter", err)
}

// rfPublishRequestFilters 广播请求过滤器域失效；Invalidator 未装配时是空操作。
func rfPublishRequestFilters(deps Deps, request *http.Request) {
	adminPublishDomain(deps, request, cfgsync.DomainRequestFilters)
}

// rfNullableString 把「JSON null / 枚举字符串」转成可空字符串指针。
func rfNullableString(raw json.RawMessage) *string {
	if len(raw) == 0 || a14IsJSONNull(raw) {
		return nil
	}
	value, ok := a14JSONString(raw)
	if !ok {
		return nil
	}
	return &value
}

// optionalString 在「请求里出现了该字段」时返回其地址。
func optionalString(value string, present bool) *string {
	if !present {
		return nil
	}
	return &value
}

// rfEffectiveText 复刻 Node 的 `updates.x ?? existing.x`：未提供**或显式 null** 都回落到现状值。
func rfEffectiveText(value string, present bool, existing string) string {
	if present {
		return value
	}
	return existing
}

// rfNullableJSON 把请求里的 JSON 字节转成可落库形态：缺失 / JSON null → nil（即 SQL NULL）。
func rfNullableJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || a14IsJSONNull(raw) {
		return nil
	}
	return raw
}
