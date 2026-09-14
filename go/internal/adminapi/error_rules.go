package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 error-rules 资源模块（批次 A / lane A1-4，7 条端点）。
//
// 唯一真源：
//   - 路由与状态码：src/app/api/v1/resources/error-rules/handlers.ts
//   - 业务规则：src/actions/error-rules.ts
//   - SQL 与默认规则表：src/repository/error-rules.ts（Go 侧在 internal/store/admin_error_rules.go）
//   - 判定语义：src/lib/error-rule-detector.ts（Go 侧在 error_rules_shared.go 的 a14 前缀一族）
//
// 七条端点：
//
//	GET    /error-rules                  列表（含禁用行，创建时间倒序）
//	POST   /error-rules                  创建（201 + Location）
//	POST   /error-rules/cache:refresh    同步默认规则 + 重载 + 广播失效，返回 {stats, syncResult}
//	GET    /error-rules/cache/stats      缓存统计
//	POST   /error-rules:test             测试消息命中
//	PATCH  /error-rules/{id}             部分更新
//	DELETE /error-rules/{id}             删除（204）
//
// 三条登记进对拍白名单的差异（都写在对应函数注释里）：
//  1. 正则校验用 Go 的 RE2（环视/回溯引用会被拒），Node 用 JS RegExp + safe-regex。
//  2. cache:stats 的 lastReloadTime 是「本进程最近一次装载/刷新的时刻」，未装载过为 0；
//     Node 报的是其进程内检测器的最近装载时刻。
//  3. 状态码按显式错误码映射（error_rule.not_found / error_rule.action_failed），不移植 Node 的
// `detail.includes("不存在")` 子串判定——。
// 本模块的 action 文案是仓库里的中文字面量（不随 locale 变），故两端状态码仍然一致。
//
// 审计：Node 侧这两个模块的写路径**一条审计都不写**（§3.3-11 实测：error-rules 与
// request-filters 的目标函数调用数为 0，只有 cache 失效广播）。Go 侧照此：不写审计，只广播。

// a14ErrorRuleCategories 与 ErrorRuleCategorySchema（src/lib/api/v1/schemas/error-rules.ts:4-14）
// 的取值域一致。
var a14ErrorRuleCategories = []string{
	"prompt_limit",
	"content_filter",
	"pdf_limit",
	"thinking_error",
	"parameter_error",
	"invalid_request",
	"cache_limit",
}

// a14ErrorRuleMatchTypes 与 ErrorRuleMatchTypeSchema 一致。
var a14ErrorRuleMatchTypes = []string{"contains", "exact", "regex"}

// a14OverrideStatusMin / Max 与 actions/error-rules.ts:16-18 的两个常量一致。
const (
	a14OverrideStatusMin = 400
	a14OverrideStatusMax = 599
)

// a14ErrorRulesReloadedAt 是本进程最近一次装载错误规则的毫秒时间戳。
//
// 与 sensitive-words lane 同一取舍：Go 侧的规则判定不常驻（每次读库现编），故没有「检测器装载
// 时刻」这个东西；refresh 的动作我们自己是发起者，就自己记下这一时刻。Node 侧初值也是 0。
var a14ErrorRulesReloadedAt atomic.Int64

// a14ErrorRulePayload 是响应体里的一条规则（逐字对应 Node 的 ErrorRuleSchema）。
type a14ErrorRulePayload struct {
	ID                 int64           `json:"id"`
	Pattern            string          `json:"pattern"`
	MatchType          string          `json:"matchType"`
	Category           string          `json:"category"`
	Description        *string         `json:"description"`
	OverrideResponse   json.RawMessage `json:"overrideResponse"`
	OverrideStatusCode *int            `json:"overrideStatusCode"`
	IsEnabled          bool            `json:"isEnabled"`
	IsDefault          bool            `json:"isDefault"`
	Priority           int             `json:"priority"`
	CreatedAt          string          `json:"createdAt"`
	UpdatedAt          string          `json:"updatedAt"`
}

// a14ErrorRuleListResponse 对应 `jsonResponse({ items: result.data })`。
type a14ErrorRuleListResponse struct {
	Items []a14ErrorRulePayload `json:"items"`
}

// a14ErrorRuleCacheRefreshResponse 对应 refreshCacheAction 的 `{ stats, syncResult }`。
type a14ErrorRuleCacheRefreshResponse struct {
	Stats      a14ErrorRuleStats              `json:"stats"`
	SyncResult store.AdminErrorRuleSyncResult `json:"syncResult"`
}

// a14ErrorRuleTestRule 是 :test 命中的规则摘要。
type a14ErrorRuleTestRule struct {
	Category           string          `json:"category"`
	Pattern            string          `json:"pattern"`
	MatchType          string          `json:"matchType"`
	OverrideResponse   json.RawMessage `json:"overrideResponse"`
	OverrideStatusCode *int            `json:"overrideStatusCode"`
}

// a14ErrorRuleTestResponse 对应 testErrorRuleAction 的返回形状。
//
// Rule 与 Warnings 带 omitempty：Node 侧未命中时 `rule: undefined`、警告为空时 `warnings:
// undefined`，两者都不出现在 JSON 里。
type a14ErrorRuleTestResponse struct {
	Matched         bool                  `json:"matched"`
	Rule            *a14ErrorRuleTestRule `json:"rule,omitempty"`
	FinalResponse   json.RawMessage       `json:"finalResponse"`
	FinalStatusCode *int                  `json:"finalStatusCode"`
	Warnings        []string              `json:"warnings,omitempty"`
}

// RegisterErrorRules 注册本模块的七条路由。
//
// 装配方式遵循 deps.go 的约定：只调用 router.Add，不改本包共享文件。依赖不齐（无 Store）时记
// 一条 warn 并不注册——未注册的路由会原样回退 Node，那里有完整的实现。
//
// 路径参数写作 `{id:[1-9][0-9]*}` 是**有意**的（同 keys.go 的取舍）：Node 用 zod 的 coerce 校验
// id，非数字、0、带前导零的 id 都会得到 zod 形状的 400/归一化后的 200。让这些形态压根不命中
// Go 路由，请求就由 Node 亲自作答，形状天然一致，Go 侧不必复刻 zod 的 invalidParams 信封。
func RegisterErrorRules(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_error_rules_store_unwired", map[string]any{
				"module": "error-rules",
				"action": "routes_not_registered",
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/error-rules",
		Access:      AccessAdmin,
		Module:      "error-rules",
		OperationID: "listErrorRules",
		Handler:     http.HandlerFunc(handleListErrorRules(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/error-rules",
		Access:      AccessAdmin,
		Module:      "error-rules",
		OperationID: "createErrorRule",
		Handler:     http.HandlerFunc(handleCreateErrorRule(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/error-rules/cache:refresh",
		Access:      AccessAdmin,
		Module:      "error-rules",
		OperationID: "refreshErrorRulesCache",
		Handler:     http.HandlerFunc(handleRefreshErrorRulesCache(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/error-rules/cache/stats",
		Access:      AccessAdmin,
		Module:      "error-rules",
		OperationID: "getErrorRulesCacheStats",
		Handler:     http.HandlerFunc(handleErrorRulesCacheStats(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/error-rules:test",
		Access:      AccessAdmin,
		Module:      "error-rules",
		OperationID: "testErrorRule",
		Handler:     http.HandlerFunc(handleTestErrorRule(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPatch,
		Path:        "/error-rules/{id:[1-9][0-9]*}",
		Access:      AccessAdmin,
		Module:      "error-rules",
		OperationID: "updateErrorRule",
		Handler:     http.HandlerFunc(handleUpdateErrorRule(deps)),
	})
	router.Add(Route{
		Method:      http.MethodDelete,
		Path:        "/error-rules/{id:[1-9][0-9]*}",
		Access:      AccessAdmin,
		Module:      "error-rules",
		OperationID: "deleteErrorRule",
		Handler:     http.HandlerFunc(handleDeleteErrorRule(deps)),
	})
}

// handleListErrorRules 复刻 listErrorRules（handlers.ts:22-27）。
//
// Node 侧 action 在非 admin 时返回空数组而不是错误；路由的 access 已是 admin 档位，守卫保证
// 到这里的一定是管理员，故不存在那条分支。
func handleListErrorRules(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		rules, err := deps.Store.AdminListErrorRules(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, a14ErrorRuleFailure(err))
			return
		}
		items := make([]a14ErrorRulePayload, 0, len(rules))
		for _, rule := range rules {
			items = append(items, a14ErrorRuleToPayload(rule))
		}
		adminWriteJSON(writer, http.StatusOK, a14ErrorRuleListResponse{Items: items})
	}
}

// handleCreateErrorRule 复刻 createErrorRule（handlers.ts:29-41）。
func handleCreateErrorRule(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := a14ObjectFields(fields, "pattern", "category", "matchType", "description",
			"overrideResponse", "overrideStatusCode")
		object.RejectUnknownKeys()
		pattern, _ := object.String("pattern", adminStringSpec{
			Required: true, Trim: true, MinRunes: 1, MaxRunes: 1000,
		})
		category, _ := object.String("category", adminStringSpec{
			Required: true, Enum: a14ErrorRuleCategories,
		})
		matchType, hasMatchType := object.String("matchType", adminStringSpec{
			Enum: a14ErrorRuleMatchTypes,
		})
		description, hasDescription := object.String("description", adminStringSpec{
			Trim: true, MaxRunes: 500,
		})
		overrideResponse, _ := a14JSONField(object, "overrideResponse", a14ShapeObject, true, nil)
		overrideStatusCode, _ := a14IntField(object, "overrideStatusCode", a14IntSpec{
			Nullable: true, Min: a14IntPtr(a14OverrideStatusMin), Max: a14IntPtr(a14OverrideStatusMax),
		})
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		// 复刻 createErrorRuleAction 的默认值与校验链。
		if !hasMatchType {
			matchType = "regex"
		}
		if reason := a14ValidateErrorRulePattern(pattern, matchType); reason != "" {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("error_rule", "error_rule.action_failed", http.StatusBadRequest,
					fmt.Errorf("%s", reason)))
			return
		}
		if len(overrideResponse) > 0 && !a14IsJSONNull(overrideResponse) {
			if reason := a14ValidateErrorOverrideResponse(overrideResponse); reason != "" {
				adminProblemWriter(deps).WriteActionError(writer, request,
					NewActionError("error_rule", "error_rule.action_failed", http.StatusBadRequest,
						fmt.Errorf("%s", reason)))
				return
			}
		}

		var descriptionPointer *string
		if hasDescription {
			descriptionPointer = &description
		}
		created, err := deps.Store.AdminCreateErrorRule(request.Context(), store.AdminCreateErrorRuleInput{
			Pattern:            pattern,
			MatchType:          matchType,
			Category:           category,
			Description:        descriptionPointer,
			OverrideResponse:   a14NullableOverride(overrideResponse),
			OverrideStatusCode: overrideStatusCode,
		})
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, a14ErrorRuleFailure(err))
			return
		}

		// Node 侧 createErrorRuleAction：insert → emitErrorRulesUpdated（本进程 reload + Redis 广播）。
		// Go 侧对应「广播 cfgsync 域」：guard 的错误规则快照随之失效并重新装载。
		a14PublishErrorRules(deps, request)

		adminWriteCreated(writer,
			fmt.Sprintf("%s/error-rules/%d", MountPrefix, created.ID),
			a14ErrorRuleToPayload(created))
	}
}

// handleUpdateErrorRule 复刻 updateErrorRule（handlers.ts:43-58）。
func handleUpdateErrorRule(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := a14PathID(request)
		if !ok {
			return
		}
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := a14ObjectFields(fields, "pattern", "category", "matchType", "description",
			"overrideResponse", "overrideStatusCode", "isEnabled", "priority")
		object.RejectUnknownKeys()
		pattern, hasPattern := object.String("pattern", adminStringSpec{
			Trim: true, MinRunes: 1, MaxRunes: 1000,
		})
		category, hasCategory := object.String("category", adminStringSpec{
			Enum: a14ErrorRuleCategories,
		})
		matchType, hasMatchType := object.String("matchType", adminStringSpec{
			Enum: a14ErrorRuleMatchTypes,
		})
		description, hasDescription := object.String("description", adminStringSpec{
			Trim: true, MaxRunes: 500,
		})
		overrideResponse, hasOverrideResponse := a14JSONField(object, "overrideResponse",
			a14ShapeObject, true, nil)
		overrideStatusCode, hasOverrideStatusCode := a14IntField(object, "overrideStatusCode",
			a14IntSpec{
				Nullable: true,
				Min:      a14IntPtr(a14OverrideStatusMin),
				Max:      a14IntPtr(a14OverrideStatusMax),
			})
		isEnabled, hasIsEnabled := object.Bool("isEnabled")
		priority, hasPriority := a14IntField(object, "priority", a14IntSpec{})
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		current, err := deps.Store.AdminGetErrorRuleByID(request.Context(), id)
		if err == store.ErrNotFound {
			// Node 的「不存在」分支直接返回 action 错误（不写审计——本模块本就没有审计）。
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("error_rule", "error_rule.not_found", http.StatusNotFound,
					fmt.Errorf("错误规则不存在")))
			return
		}
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, a14ErrorRuleFailure(err))
			return
		}

		// 复刻 updateErrorRuleAction：按**最终**的 pattern/matchType 判正则，而不是只看这次的入参。
		finalPattern := current.Pattern
		if hasPattern {
			finalPattern = pattern
		}
		finalMatchType := current.MatchType
		if hasMatchType {
			finalMatchType = matchType
		}
		if reason := a14ValidateErrorRulePattern(finalPattern, finalMatchType); reason != "" {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("error_rule", "error_rule.action_failed", http.StatusBadRequest,
					fmt.Errorf("%s", reason)))
			return
		}
		if hasOverrideResponse && !a14IsJSONNull(overrideResponse) {
			if reason := a14ValidateErrorOverrideResponse(overrideResponse); reason != "" {
				adminProblemWriter(deps).WriteActionError(writer, request,
					NewActionError("error_rule", "error_rule.action_failed", http.StatusBadRequest,
						fmt.Errorf("%s", reason)))
				return
			}
		}

		update := store.AdminErrorRuleUpdate{}
		if hasPattern {
			update.Pattern = &pattern
		}
		if hasCategory {
			update.Category = &category
		}
		if hasMatchType {
			update.MatchType = &matchType
		}
		if hasDescription {
			update.Description = store.AdminNullableText{Present: true, Value: &description}
		}
		if hasOverrideResponse {
			update.OverrideResponse = store.AdminNullableJSON{
				Present: true,
				Raw:     a14NullableOverride(overrideResponse),
			}
		}
		if hasOverrideStatusCode {
			update.OverrideStatusCode = store.AdminNullableInt{Present: true, Value: overrideStatusCode}
		}
		if hasIsEnabled {
			update.IsEnabled = isEnabled
		}
		if hasPriority {
			update.Priority = priority
		}
		// 编辑默认规则即自动转为自定义规则，否则用户改动会被下一次「同步规则」覆盖。
		if current.IsDefault {
			converted := false
			update.IsDefault = &converted
		}

		updated, err := deps.Store.AdminUpdateErrorRule(request.Context(), id, update)
		if err == store.ErrNotFound {
			// 并发删除：Node 侧的防御性分支同样给「不存在或已被删除」。
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("error_rule", "error_rule.not_found", http.StatusNotFound,
					fmt.Errorf("错误规则不存在或已被删除")))
			return
		}
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, a14ErrorRuleFailure(err))
			return
		}

		a14PublishErrorRules(deps, request)
		adminWriteJSON(writer, http.StatusOK, a14ErrorRuleToPayload(updated))
	}
}

// handleDeleteErrorRule 复刻 deleteErrorRule（handlers.ts:59-72）。
func handleDeleteErrorRule(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := a14PathID(request)
		if !ok {
			return
		}
		deleted, err := deps.Store.AdminDeleteErrorRule(request.Context(), id)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, a14ErrorRuleFailure(err))
			return
		}
		if !deleted {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("error_rule", "error_rule.not_found", http.StatusNotFound,
					fmt.Errorf("错误规则不存在")))
			return
		}
		a14PublishErrorRules(deps, request)
		adminWriteNoContent(writer)
	}
}

// handleRefreshErrorRulesCache 复刻 refreshCacheAction（actions/error-rules.ts:418-477）。
//
// 三步：同步默认规则到库 → 重新装载 → 返回 {stats, syncResult}。
//
// 差异（登记进白名单）：Node 的 refresh **不**广播 Redis（它只在本进程 reload）；Go 侧必须广播
// cfgsync 域，否则 guard 的错误规则快照不会跟着更新——Go 没有「本进程 reload」这个动作。
func handleRefreshErrorRulesCache(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		syncResult, err := deps.Store.AdminSyncDefaultErrorRules(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, a14ErrorRuleFailure(err))
			return
		}
		a14PublishErrorRules(deps, request)
		stats, err := a14ErrorRuleCurrentStats(request.Context(), deps)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, a14ErrorRuleFailure(err))
			return
		}
		a14ErrorRulesReloadedAt.Store(adminNowMillis())
		stats.LastReloadTime = a14ErrorRulesReloadedAt.Load()
		adminWriteJSON(writer, http.StatusOK, a14ErrorRuleCacheRefreshResponse{
			Stats:      stats,
			SyncResult: syncResult,
		})
	}
}

// handleErrorRulesCacheStats 复刻 getErrorRulesCacheStats（handlers.ts:81-94）。
//
// Node 侧 action 在非 admin 时返回 null，handler 把 null 映射成 403 auth.forbidden；路由的
// access 已是 admin 档位，故那条分支不可能走到。Node 的 getCacheStats 会先 ensureInitialized
// （懒装载），Go 侧每次现读库，语义上是「已装载」。
func handleErrorRulesCacheStats(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		stats, err := a14ErrorRuleCurrentStats(request.Context(), deps)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, a14ErrorRuleFailure(err))
			return
		}
		adminWriteJSON(writer, http.StatusOK, stats)
	}
}

// handleTestErrorRule 复刻 testErrorRule（handlers.ts:96-109）与 testErrorRuleAction。
//
// 检测用**原始**消息（与运行时的 error-handler 一致），只用 trim 做空值判定——空消息由 schema
// 的 min(1) 挡下（Node 侧 action 的「测试消息不能为空」因此不可达）。
func handleTestErrorRule(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := a14ObjectFields(fields, "message")
		object.RejectUnknownKeys()
		message, _ := object.String("message", adminStringSpec{
			Required: true, Trim: true, MinRunes: 1,
		})
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		rules, err := deps.Store.AdminListActiveErrorRules(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, a14ErrorRuleFailure(err))
			return
		}
		index := a14BuildErrorRuleIndex(rules)
		matched, found := index.detect(message)

		response := a14ErrorRuleTestResponse{Matched: found}
		if !found {
			adminWriteJSON(writer, http.StatusOK, response)
			return
		}

		matchType := matched.rule.MatchType
		if !a14ContainsString(a14ErrorRuleMatchTypes, matchType) {
			matchType = "regex"
		}
		response.Rule = &a14ErrorRuleTestRule{
			Category:           matched.rule.Category,
			Pattern:            matched.rule.Pattern,
			MatchType:          matchType,
			OverrideResponse:   matched.override,
			OverrideStatusCode: matched.overrideStatusCode,
		}

		warnings := make([]string, 0, 2)
		if len(matched.override) > 0 {
			final, finalWarnings, ok := a14BuildFinalOverride(matched.override, message)
			if ok {
				response.FinalResponse = final
			}
			warnings = append(warnings, finalWarnings...)
		}
		if matched.overrideStatusCode != nil {
			if *matched.overrideStatusCode < a14OverrideStatusMin || *matched.overrideStatusCode > a14OverrideStatusMax {
				warnings = append(warnings, fmt.Sprintf(
					"覆写状态码 %d 非整数或超出有效范围（%d-%d），运行时将使用上游状态码",
					*matched.overrideStatusCode, a14OverrideStatusMin, a14OverrideStatusMax))
			} else {
				finalStatusCode := *matched.overrideStatusCode
				response.FinalStatusCode = &finalStatusCode
			}
		}
		if len(warnings) > 0 {
			response.Warnings = warnings
		}
		adminWriteJSON(writer, http.StatusOK, response)
	}
}

// a14BuildFinalOverride 复刻 testErrorRuleAction 里「模拟运行时处理」的那一段：
// 去掉 request_id、message 为空则回退原始消息，产出最终响应体与告警。
func a14BuildFinalOverride(override json.RawMessage, rawMessage string) (json.RawMessage, []string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(override, &fields); err != nil {
		return nil, nil, false
	}
	warnings := make([]string, 0, 1)

	// 1. 验证覆写响应格式（与 error-handler.ts 运行时逻辑一致）。
	if reason := a14ValidateErrorOverrideResponse(override); reason != "" {
		warnings = append(warnings, reason+"，运行时将跳过响应体覆写")
		return nil, warnings, false
	}

	// 2. 移除 request_id（运行时会从上游注入）。
	delete(fields, "request_id")

	// 3. message 为空则回退到原始错误消息。
	errorObject, _ := a14ObjectField(fields, "error")
	messageRaw, hasMessage := errorObject["message"]
	overrideMessage, isString := a14JSONString(messageRaw)
	isMessageEmpty := !hasMessage || !isString || overrideMessage == ""
	if isMessageEmpty {
		warnings = append(warnings, "覆写响应的 message 为空，运行时将回退到原始错误消息")
		overrideMessage = rawMessage
	}

	// finalResponse = { ...responseWithoutRequestId, error: { ...errorObj, message } }
	final := make(map[string]json.RawMessage, len(fields)+1)
	for key, value := range fields {
		final[key] = value
	}
	merged := make(map[string]json.RawMessage, len(errorObject)+1)
	for key, value := range errorObject {
		merged[key] = value
	}
	encodedMessage, err := json.Marshal(overrideMessage)
	if err != nil {
		return nil, warnings, false
	}
	merged["message"] = encodedMessage
	encodedError, err := json.Marshal(merged)
	if err != nil {
		return nil, warnings, false
	}
	final["error"] = encodedError
	encoded, err := json.Marshal(final)
	if err != nil {
		return nil, warnings, false
	}
	return encoded, warnings, true
}

// a14ErrorRuleToPayload 把一行映射成响应体；可空列按 Node 的 `?? new Date()` 与
// sanitizeOverrideResponse 兜底。
func a14ErrorRuleToPayload(rule store.AdminErrorRule) a14ErrorRulePayload {
	return a14ErrorRulePayload{
		ID:                 rule.ID,
		Pattern:            rule.Pattern,
		MatchType:          rule.MatchType,
		Category:           rule.Category,
		Description:        rule.Description,
		OverrideResponse:   a14ErrorRuleOverride(rule.OverrideResponse),
		OverrideStatusCode: rule.OverrideStatusCode,
		IsEnabled:          rule.IsEnabled,
		IsDefault:          rule.IsDefault,
		Priority:           rule.Priority,
		CreatedAt:          adminStringOrNow(rule.CreatedAt),
		UpdatedAt:          adminStringOrNow(rule.UpdatedAt),
	}
}

// a14ErrorRuleCurrentStats 现算缓存统计。
func a14ErrorRuleCurrentStats(ctx context.Context, deps Deps) (a14ErrorRuleStats, error) {
	rules, err := deps.Store.AdminListActiveErrorRules(ctx)
	if err != nil {
		return a14ErrorRuleStats{}, err
	}
	return a14BuildErrorRuleIndex(rules).stats(a14ErrorRulesReloadedAt.Load()), nil
}

// a14ValidateErrorRulePattern 复刻 create/update 的正则校验（safe-regex + 语法两段）。
//
// 差异（登记进白名单）：safe-regex 无 Go 对应物，RE2 又不可能回溯爆炸，故只留「能否编译」。
// 代价是「JS 能编译、RE2 不能」的样式（环视、回溯引用）在 Go 侧被拒 400，而 Node 侧放行。
func a14ValidateErrorRulePattern(pattern, matchType string) string {
	if matchType != "regex" {
		return ""
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return "无效的正则表达式"
	}
	return ""
}

// a14NullableOverride 把「请求里的覆写体字节」转成可落库的形态：JSON null / 缺失 → nil。
func a14NullableOverride(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || a14IsJSONNull(raw) {
		return nil
	}
	return raw
}

// a14ErrorRuleFailure 把存储层失败映射成 action 错误。
//
// 状态码 400 与 Node 同形：action 的 catch 分支只按「不存在」/「权限」两个子串判 404/403，
// 其余一律 400——数据库故障在 Node 侧也是 400。
func a14ErrorRuleFailure(err error) *ActionError {
	return adminActionFailure("error_rule", err)
}

// a14PublishErrorRules 广播错误规则域失效；Invalidator 未装配时是空操作。
func a14PublishErrorRules(deps Deps, request *http.Request) {
	adminPublishDomain(deps, request, cfgsync.DomainErrorRules)
}

// a14PathID 取路径参数 id（路由正则已保证它是正十进制整数，故这里只需转换）。
func a14PathID(request *http.Request) (int64, bool) {
	raw := ParamsFrom(request.Context())["id"]
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		// 路由正则不允许走到这里；真走到了说明路由表被改坏了，按未命中处理更安全。
		return 0, false
	}
	return parsed, true
}

// a14IntPtr 取整数指针（给 a14IntSpec 的 Min/Max 用）。
func a14IntPtr(value int) *int { return &value }

// a14ContainsString 判断取值是否落在枚举里。
func a14ContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
