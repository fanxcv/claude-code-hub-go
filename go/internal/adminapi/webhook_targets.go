package adminapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 webhook-targets 资源模块（6 条端点）。
//
// 唯一真源：
//   - 路由与状态码：src/app/api/v1/resources/webhook-targets/router.ts
//   - 处理器语义与脱敏：src/app/api/v1/resources/webhook-targets/handlers.ts
//   - 业务规则（校验顺序与归一化）：src/actions/webhook-targets.ts
//   - 请求/响应形状：src/lib/api/v1/schemas/webhook-targets.ts
//   - SQL：src/repository/webhook-targets.ts（Go 侧在 internal/store/admin_notifications.go）
//
// 六条端点：
//
//	GET    /webhook-targets              列表（id 倒序，密钥字段已脱敏）
//	POST   /webhook-targets              创建（201 + Location）
//	GET    /webhook-targets/{id}         详情（404 = webhook_target.not_found）
//	PATCH  /webhook-targets/{id}         部分更新（"[REDACTED]" 占位符保留原值）
//	DELETE /webhook-targets/{id}         删除（204，幂等）
//	POST   /webhook-targets/{id}:test    发一条测试通知（回 {latencyMs}）
//
// 登记进差异白名单的三项：
//  1. **投递正文文案简化**：见 webhook_deliver.go 文件头（信封与端点语义对齐，正文不逐字对齐）。
//  2. **语义错误的错误码**：Node 的 action 层把 zod 校验失败与业务校验失败都包成 ActionResult，
//     处理器统一映射成 400 + `webhook_target.action_failed`；Go 侧只把「形状/类型/URL 格式」
//     做成 zod 形状的 400（errorCode `request.validation_failed`），业务校验（供应商必填项、
//     代理地址、模板解析）走 action 形状的 400（errorCode `webhook_target.action_failed`）。
//     两者的 HTTP 状态码一致，差别只在 errorCode 与 invalidParams 的有无。
//  3. **创建目标的副作用**：Node 在「创建第一个目标」时会顺带把 notification_settings.useLegacyMode
//     关掉（数据迁移策略）。Go 侧执行同一步，但见 handleCreateWebhookTarget 里的说明。

// webhookTargetResponse 逐字对应 WebhookTargetSchema（脱敏后的形状）。
type webhookTargetResponse struct {
	ID                    int64           `json:"id"`
	Name                  string          `json:"name"`
	ProviderType          string          `json:"providerType"`
	WebhookURL            *string         `json:"webhookUrl"`
	TelegramBotToken      *string         `json:"telegramBotToken"`
	TelegramChatID        *string         `json:"telegramChatId"`
	DingtalkSecret        *string         `json:"dingtalkSecret"`
	CustomTemplate        json.RawMessage `json:"customTemplate"`
	CustomHeaders         json.RawMessage `json:"customHeaders"`
	ProxyURL              *string         `json:"proxyUrl"`
	ProxyFallbackToDirect bool            `json:"proxyFallbackToDirect"`
	IsEnabled             bool            `json:"isEnabled"`
	LastTestAt            *string         `json:"lastTestAt"`
	LastTestResult        json.RawMessage `json:"lastTestResult"`
	CreatedAt             *string         `json:"createdAt"`
	UpdatedAt             *string         `json:"updatedAt"`
}

// webhookTargetListResponse 对应 `jsonResponse({ items })`。
type webhookTargetListResponse struct {
	Items []webhookTargetResponse `json:"items"`
}

// webhookTargetTestResponse 对应 WebhookTargetTestResponseSchema。
type webhookTargetTestResponse struct {
	LatencyMS int64 `json:"latencyMs"`
}

// webhookProviderTypes 是 ProviderTypeSchema 的取值域。
var webhookProviderTypes = []string{"wechat", "feishu", "dingtalk", "telegram", "custom"}

// webhookNotificationTypes 是 WebhookNotificationTypeSchema 的取值域。
var webhookNotificationTypes = []string{
	"circuit_breaker", "daily_leaderboard", "cost_alert", "cache_hit_rate_alert",
}

// webhookTargetCreateFields 对应 WebhookTargetCreateSchema 的键集（strict）。
var webhookTargetCreateFields = []string{
	"name", "providerType", "webhookUrl", "telegramBotToken", "telegramChatId",
	"dingtalkSecret", "customTemplate", "customHeaders", "proxyUrl", "proxyFallbackToDirect",
	"isEnabled",
}

// RegisterWebhookTargets 注册本模块的六条路由。
func RegisterWebhookTargets(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_webhook_targets_store_unwired", map[string]any{
				"module": "webhook-targets",
				"action": "routes_not_registered",
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/webhook-targets",
		Access:      AccessAdmin,
		Module:      "webhook-targets",
		OperationID: "listWebhookTargets",
		Handler:     http.HandlerFunc(handleListWebhookTargets(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/webhook-targets",
		Access:      AccessAdmin,
		Module:      "webhook-targets",
		OperationID: "createWebhookTarget",
		Handler:     http.HandlerFunc(handleCreateWebhookTarget(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/webhook-targets/{id}",
		Access:      AccessAdmin,
		Module:      "webhook-targets",
		OperationID: "getWebhookTarget",
		Handler:     http.HandlerFunc(handleGetWebhookTarget(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPatch,
		Path:        "/webhook-targets/{id}",
		Access:      AccessAdmin,
		Module:      "webhook-targets",
		OperationID: "updateWebhookTarget",
		Handler:     http.HandlerFunc(handleUpdateWebhookTarget(deps)),
	})
	router.Add(Route{
		Method:      http.MethodDelete,
		Path:        "/webhook-targets/{id}",
		Access:      AccessAdmin,
		Module:      "webhook-targets",
		OperationID: "deleteWebhookTarget",
		Handler:     http.HandlerFunc(handleDeleteWebhookTarget(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/webhook-targets/{id:[0-9]+}:test",
		Access:      AccessAdmin,
		Module:      "webhook-targets",
		OperationID: "testWebhookTarget",
		Handler:     http.HandlerFunc(handleTestWebhookTarget(deps)),
	})
}

// handleListWebhookTargets 复刻 listWebhookTargets（handlers.ts:28-36）。
func handleListWebhookTargets(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		targets, err := deps.Store.AdminListWebhookTargets(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("webhook_target", err))
			return
		}
		items := make([]webhookTargetResponse, 0, len(targets))
		for _, target := range targets {
			items = append(items, webhookTargetPayload(target))
		}
		adminWriteJSON(writer, http.StatusOK, webhookTargetListResponse{Items: items})
	}
}

// handleGetWebhookTarget 复刻 getWebhookTarget（handlers.ts:38-58）。
//
// Node 取整表再在内存里 find：语义等价于按 id 取一行（包括 404），Go 侧直接按 id 查。
func handleGetWebhookTarget(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := webhookTargetIDParam(writer, request)
		if !ok {
			return
		}
		target, err := deps.Store.AdminGetWebhookTarget(request.Context(), id)
		if err == store.ErrNotFound {
			webhookTargetNotFound(writer, request, deps)
			return
		}
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("webhook_target", err))
			return
		}
		adminWriteJSON(writer, http.StatusOK, webhookTargetPayload(target))
	}
}

// handleCreateWebhookTarget 复刻 createWebhookTarget（handlers.ts:60-91 + actions:326-353）。
func handleCreateWebhookTarget(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, webhookTargetCreateFields...)
		object.RejectUnknownKeys()
		input, payloadIssues := webhookParseTargetInput(object, webhookTargetParseCreate)
		if issues := object.issues0(); issues != nil || payloadIssues != nil {
			adminWriteValidationFailure(writer, request, issues)
			return
		}
		if webhookHasRedactedPlaceholder(fields) {
			adminProblemWriter(deps).WriteProblem(writer, request, http.StatusUnprocessableEntity,
				"webhook_target.redacted_placeholder_rejected",
				"Redacted placeholders cannot be used when creating webhook targets.")
			return
		}
		normalized, actionErr := webhookNormalizeCreate(input)
		if actionErr != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, actionErr)
			return
		}

		created, err := deps.Store.AdminCreateWebhookTarget(request.Context(),
			store.AdminCreateWebhookTargetInput{
				Name:                  normalized.Name,
				ProviderType:          normalized.ProviderType,
				WebhookURL:            normalized.WebhookURL,
				TelegramBotToken:      normalized.TelegramBotToken,
				TelegramChatID:        normalized.TelegramChatID,
				DingtalkSecret:        normalized.DingtalkSecret,
				CustomTemplate:        normalized.CustomTemplate,
				CustomHeaders:         normalized.CustomHeaders,
				ProxyURL:              normalized.ProxyURL,
				ProxyFallbackToDirect: normalized.ProxyFallbackToDirect,
				IsEnabled:             normalized.IsEnabled,
			})
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("webhook_target", err))
			return
		}

		// 数据迁移策略（actions/webhook-targets.ts:336-341）：创建目标后若仍处于 legacy 模式，
		// 则关掉它。Node 忽略这一步的失败（它只是「顺手迁移」），Go 侧同样只记 warn——
		// 迁移失败不该让已经建好的目标回滚。
		webhookDisableLegacyMode(deps, request)

		adminWriteCreated(writer,
			fmt.Sprintf("%s/webhook-targets/%d", MountPrefix, created.ID),
			webhookTargetPayload(created))
	}
}

// handleUpdateWebhookTarget 复刻 updateWebhookTarget（handlers.ts:93-131 + actions:355-380）。
func handleUpdateWebhookTarget(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := webhookTargetIDParam(writer, request)
		if !ok {
			return
		}
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, webhookTargetCreateFields...)
		object.RejectUnknownKeys()
		input, payloadIssues := webhookParseTargetInput(object, webhookTargetParseUpdate)
		if issues := object.issues0(); issues != nil || payloadIssues != nil {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		existing, err := deps.Store.AdminGetWebhookTarget(request.Context(), id)
		if err == store.ErrNotFound {
			webhookTargetNotFound(writer, request, deps)
			return
		}
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("webhook_target", err))
			return
		}

		// 未解析的脱敏头回显：Node 在 action 之前就拦下（422）。
		if unresolved := webhookUnresolvedRedactedHeaders(input.CustomHeaders, existing.CustomHeaders); unresolved {
			adminProblemWriter(deps).WriteProblem(writer, request, http.StatusUnprocessableEntity,
				"webhook_target.redacted_placeholder_rejected",
				"Redacted placeholders cannot be used for renamed custom header fields.")
			return
		}
		input.CustomHeaders = webhookRestoreRedactedHeaders(input.CustomHeaders, existing.CustomHeaders)

		normalized, actionErr := webhookNormalizeUpdate(existing, input)
		if actionErr != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, actionErr)
			return
		}

		updated, err := deps.Store.AdminUpdateWebhookTarget(request.Context(), id,
			store.AdminWebhookTargetUpdate{
				Name:                  &normalized.Name,
				ProviderType:          &normalized.ProviderType,
				WebhookURL:            store.AdminNullableText{Present: true, Value: normalized.WebhookURL},
				TelegramBotToken:      store.AdminNullableText{Present: true, Value: normalized.TelegramBotToken},
				TelegramChatID:        store.AdminNullableText{Present: true, Value: normalized.TelegramChatID},
				DingtalkSecret:        store.AdminNullableText{Present: true, Value: normalized.DingtalkSecret},
				CustomTemplate:        store.AdminNullableJSON{Present: true, Raw: normalized.CustomTemplate},
				CustomHeaders:         store.AdminNullableJSON{Present: true, Raw: normalized.CustomHeaders},
				ProxyURL:              store.AdminNullableText{Present: true, Value: normalized.ProxyURL},
				ProxyFallbackToDirect: &normalized.ProxyFallbackToDirect,
				IsEnabled:             &normalized.IsEnabled,
			})
		if err == store.ErrNotFound {
			webhookTargetNotFound(writer, request, deps)
			return
		}
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("webhook_target", err))
			return
		}
		adminWriteJSON(writer, http.StatusOK, webhookTargetPayload(updated))
	}
}

// handleDeleteWebhookTarget 复刻 deleteWebhookTarget（handlers.ts:120-133）。
//
// 幂等：目标不存在也回 204（Node 侧 delete 不带 returning，故不会 404）。
func handleDeleteWebhookTarget(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := webhookTargetIDParam(writer, request)
		if !ok {
			return
		}
		if _, err := deps.Store.AdminDeleteWebhookTarget(request.Context(), id); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("webhook_target", err))
			return
		}
		adminWriteNoContent(writer)
	}
}

// handleTestWebhookTarget 复刻 testWebhookTarget（handlers.ts:135-151 + actions:432-480）。
//
// 语义：目标不存在 → 404；投递失败 → 400（errorCode webhook_target.action_failed）并把失败结果
// 写回 last_test_result；成功 → 200 + {latencyMs}。
func handleTestWebhookTarget(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := webhookTargetIDParam(writer, request)
		if !ok {
			return
		}
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, "notificationType")
		object.RejectUnknownKeys()
		notificationType, _ := object.String("notificationType",
			adminStringSpec{Required: true, Enum: webhookNotificationTypes})
		if issues := object.issues0(); issues != nil {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		target, err := deps.Store.AdminGetWebhookTarget(request.Context(), id)
		if err == store.ErrNotFound {
			// Node 的 action 回「推送目标不存在」，处理器把它映射成 404（detail.includes("不存在")）。
			adminProblemWriter(deps).WriteProblem(writer, request, http.StatusNotFound,
				"webhook_target.action_failed", problemTitle(http.StatusNotFound))
			return
		}
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("webhook_target", err))
			return
		}

		timezone := deps.Store.AdminSystemTimezoneOrUTC(request.Context())
		result := adminSendWebhook(request.Context(), target, webhookSendOptions{
			NotificationType: notificationType,
			Timezone:         timezone,
			// WebhookNotifier 默认 maxRetries=3 → 总尝试 3 次。
			MaxAttempts: 3,
		})
		webhookPersistTestResult(deps, request, id, result)

		if !result.Success {
			adminProblemWriter(deps).WriteProblem(writer, request, http.StatusBadRequest,
				"webhook_target.action_failed", problemTitle(http.StatusBadRequest))
			return
		}
		adminWriteJSON(writer, http.StatusOK, webhookTargetTestResponse{LatencyMS: result.LatencyMS})
	}
}

// webhookPersistTestResult 把一次投递结果写回目标行（失败只记 warn，与 Node 相同）。
func webhookPersistTestResult(deps Deps, request *http.Request, id int64, result webhookSendResult) {
	payload, err := adminMarshalJSON(result)
	if err != nil {
		return
	}
	if err := deps.Store.AdminUpdateWebhookTargetTestResult(request.Context(), id, payload); err != nil &&
		deps.Logger != nil {
		deps.Logger.Warn("admin_webhook_test_result_write_failed", map[string]any{
			"targetId": id,
			"error":    err.Error(),
		})
	}
}

// webhookDisableLegacyMode 复刻「创建目标时关掉 legacy 模式」的副作用。
func webhookDisableLegacyMode(deps Deps, request *http.Request) {
	settings, err := deps.Store.AdminGetNotificationSettings(request.Context())
	if err != nil || !settings.UseLegacyMode {
		return
	}
	disabled := false
	if _, err := deps.Store.AdminUpdateNotificationSettings(request.Context(), settings.ID,
		store.AdminNotificationSettingsUpdate{UseLegacyMode: &disabled}); err != nil &&
		deps.Logger != nil {
		deps.Logger.Warn("admin_webhook_legacy_mode_disable_failed", map[string]any{
			"error": err.Error(),
		})
	}
}

// webhookTargetPayload 复刻 sanitizeWebhookTarget（handlers.ts:153-162）。
//
// webhookUrl 与 telegramBotToken / dingtalkSecret 是写死的一族：url 恒为占位符（非空时）、
// 两个密钥恒为 null（write-only）。
func webhookTargetPayload(target store.AdminWebhookTarget) webhookTargetResponse {
	var webhookURL *string
	if target.WebhookURL != nil {
		redacted := "[REDACTED]"
		webhookURL = &redacted
	}
	return webhookTargetResponse{
		ID:                    target.ID,
		Name:                  target.Name,
		ProviderType:          target.ProviderType,
		WebhookURL:            webhookURL,
		TelegramBotToken:      nil,
		TelegramChatID:        target.TelegramChatID,
		DingtalkSecret:        nil,
		CustomTemplate:        target.CustomTemplate,
		CustomHeaders:         webhookRedactHeaders(target.CustomHeaders),
		ProxyURL:              webhookRedactURLCredentials(target.ProxyURL),
		ProxyFallbackToDirect: target.ProxyFallbackToDirect,
		IsEnabled:             target.IsEnabled,
		LastTestAt:            target.LastTestAt,
		LastTestResult:        target.LastTestResult,
		CreatedAt:             target.CreatedAt,
		UpdatedAt:             target.UpdatedAt,
	}
}

// webhookTargetIDParam 解析并校验路径参数 id（复刻 WebhookTargetIdParamSchema：coerce + int + 正数）。
func webhookTargetIDParam(writer http.ResponseWriter, request *http.Request) (int64, bool) {
	id, ok := adminCoerceInt(ParamsFrom(request.Context())["id"])
	if !ok || id <= 0 {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path:    []any{"id"},
			Code:    "invalid_type",
			Message: "Expected number, received string",
		}})
		return 0, false
	}
	return int64(id), true
}

// webhookTargetNotFound 复刻 handlers 的 404 分支（detail 是固定英文文案）。
func webhookTargetNotFound(writer http.ResponseWriter, request *http.Request, deps Deps) {
	adminProblemWriter(deps).WriteProblem(writer, request, http.StatusNotFound,
		"webhook_target.not_found", "Webhook target was not found.")
}

// webhookActionError 构造 action 形状的失败（状态码按 Node 的 detail 子串判定映射）。
func webhookActionError(message string) *ActionError {
	status := http.StatusBadRequest
	switch {
	case strings.Contains(message, "不存在"):
		status = http.StatusNotFound
	case strings.Contains(message, "权限"):
		status = http.StatusForbidden
	}
	return NewActionError("webhook_target", "webhook_target.action_failed", status,
		errors.New(message))
}

// webhookTargetInput 是解析后的输入（区分「未出现」与「出现了 null」）。
type webhookTargetInput struct {
	Name                  string
	NamePresent           bool
	ProviderType          string
	ProviderTypePresent   bool
	WebhookURL            store.NullableString
	TelegramBotToken      store.NullableString
	TelegramChatID        store.NullableString
	DingtalkSecret        store.NullableString
	CustomTemplate        json.RawMessage
	CustomTemplatePresent bool
	// CustomTemplateIsString 表示自定义模板以字符串形式给出（需要 JSON.parse）。
	CustomTemplateIsString bool
	CustomHeaders          json.RawMessage
	CustomHeadersPresent   bool
	ProxyURL               store.NullableString
	ProxyFallbackToDirect  *bool
	IsEnabled              *bool
}

// webhookTargetParseMode 区分创建与更新的必填项差异。
type webhookTargetParseMode int

const (
	webhookTargetParseCreate webhookTargetParseMode = iota
	webhookTargetParseUpdate
)

// webhookParseTargetInput 按 schema 读取字段并累计 zod 形状的校验失败。
//
// 第二个返回值为非 nil 表示「形状错误」（统一按 400 的 zod 信封作答）；第一个返回值只在无错时可用。
func webhookParseTargetInput(
	object *adminObject,
	mode webhookTargetParseMode,
) (webhookTargetInput, error) {
	var input webhookTargetInput

	input.Name, input.NamePresent = object.String("name", adminStringSpec{
		Required: mode == webhookTargetParseCreate,
		Trim:     true,
		MinRunes: 1,
		MaxRunes: 100,
	})
	input.ProviderType, input.ProviderTypePresent = object.String("providerType", adminStringSpec{
		Required: mode == webhookTargetParseCreate,
		Enum:     webhookProviderTypes,
	})

	input.WebhookURL = webhookParseNullableString(object, "webhookUrl", webhookURLUpdateSpec(mode))
	input.TelegramBotToken = webhookParseNullableString(object, "telegramBotToken", nil)
	input.TelegramChatID = webhookParseNullableString(object, "telegramChatId", nil)
	input.DingtalkSecret = webhookParseNullableString(object, "dingtalkSecret", nil)
	input.ProxyURL = webhookParseNullableString(object, "proxyUrl", nil)

	var present bool
	input.CustomTemplate, present, input.CustomTemplateIsString =
		webhookParseCustomTemplate(object, "customTemplate")
	input.CustomTemplatePresent = present
	input.CustomHeaders, input.CustomHeadersPresent = webhookParseHeaderRecord(object, "customHeaders")

	input.ProxyFallbackToDirect, _ = object.Bool("proxyFallbackToDirect")
	input.IsEnabled, _ = object.Bool("isEnabled")

	if issues := object.issues0(); issues != nil {
		return webhookTargetInput{}, errors.New("validation")
	}
	return input, nil
}

// webhookURLUpdateSpec 复刻两种 schema 对 webhookUrl 的取值要求。
//
// 创建用 WebhookTargetCreateSchema（`.url()`）；更新用 WebhookTargetUpdateSchema，它额外放行
// 字面量 "[REDACTED]"（前端把读到的脱敏值原样回传时保留原值）。proxyUrl 与其余字符串没有这个
// 例外分支，故只对 webhookUrl 放开。
func webhookURLUpdateSpec(mode webhookTargetParseMode) func(string) bool {
	return func(value string) bool {
		if mode == webhookTargetParseUpdate && value == "[REDACTED]" {
			return true
		}
		parsed, err := url.Parse(value)
		if err != nil {
			return false
		}
		return parsed.Scheme != "" && parsed.Host != ""
	}
}

// webhookParseNullableString 读一个 `string | null | undefined` 字段。
//
// check 非 nil 时对非空值做格式校验（zod 的 .url()）；失败按 invalid_string 报错。
func webhookParseNullableString(
	object *adminObject,
	key string,
	check func(string) bool,
) store.NullableString {
	value, present := providerNullableString(object, key, 0, []any{key})
	if !present || value.Value == nil || check == nil {
		return value
	}
	if !check(*value.Value) {
		object.fail([]any{key}, "invalid_string", "Invalid url")
	}
	return value
}

// webhookParseCustomTemplate 读 customTemplate（`string | record | null | undefined`）。
//
// 字符串形式在 action 层才 JSON.parse（见 normalizeTargetInput 的 parseCustomTemplate），故这里
// 只做「是字符串还是对象」的分流，解析失败留给业务校验报「自定义模板必须是 JSON 对象」。
func webhookParseCustomTemplate(
	object *adminObject,
	key string,
) (json.RawMessage, bool, bool) {
	raw, present := object.Raw(key)
	if !present {
		return nil, false, false
	}
	switch adminJSONTypeName(raw) {
	case "null":
		return nil, true, false
	case "string":
		return raw, true, true
	case "object":
		return raw, true, false
	default:
		object.fail([]any{key}, "invalid_type", adminTypeMessage("string", raw))
		return nil, true, false
	}
}

// webhookParseHeaderRecord 读 customHeaders（`record<string, string> | null | undefined`）。
func webhookParseHeaderRecord(object *adminObject, key string) (json.RawMessage, bool) {
	raw, present := object.Raw(key)
	if !present {
		return nil, false
	}
	switch adminJSONTypeName(raw) {
	case "null":
		return nil, true
	case "object":
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			object.fail([]any{key}, "invalid_type", adminTypeMessage("object", raw))
			return nil, true
		}
		for name, value := range decoded {
			if _, ok := value.(string); !ok {
				object.fail([]any{key, name}, "invalid_type", adminTypeMessage("string", nil))
				return nil, true
			}
		}
		return raw, true
	default:
		object.fail([]any{key}, "invalid_type", adminTypeMessage("object", raw))
		return nil, true
	}
}

// webhookNormalizedTarget 是归一化后的写入值（对应 actions 的 normalizeTargetInput 返回体）。
type webhookNormalizedTarget struct {
	Name                  string
	ProviderType          string
	WebhookURL            *string
	TelegramBotToken      *string
	TelegramChatID        *string
	DingtalkSecret        *string
	CustomTemplate        json.RawMessage
	CustomHeaders         json.RawMessage
	ProxyURL              *string
	ProxyFallbackToDirect bool
	IsEnabled             bool
}

// webhookNormalizeCreate 复刻 normalizeTargetInput。
func webhookNormalizeCreate(input webhookTargetInput) (webhookNormalizedTarget, *ActionError) {
	providerType := input.ProviderType
	webhookURL := webhookTrimToNull(input.WebhookURL.Value)
	botToken := webhookTrimToNull(input.TelegramBotToken.Value)
	chatID := webhookTrimToNull(input.TelegramChatID.Value)
	dingtalkSecret := webhookTrimToNull(input.DingtalkSecret.Value)
	proxyURL := webhookTrimToNull(input.ProxyURL.Value)

	if proxyURL != nil && !webhookIsValidProxyURL(*proxyURL) {
		return webhookNormalizedTarget{}, webhookActionError(
			"代理地址格式不正确（支持 http:// https:// socks5:// socks4://）")
	}

	customTemplate, actionErr := webhookResolveCustomTemplate(providerType, input, nil)
	if actionErr != nil {
		return webhookNormalizedTarget{}, actionErr
	}
	if err := webhookValidateProviderConfig(providerType, webhookURL, botToken, chatID,
		webhookCustomTemplateProvided(providerType, customTemplate)); err != nil {
		return webhookNormalizedTarget{}, webhookActionError(err.Error())
	}

	customHeaders, actionErr := webhookResolveCustomHeaders(providerType, input, nil)
	if actionErr != nil {
		return webhookNormalizedTarget{}, actionErr
	}

	isEnabled := true
	if input.IsEnabled != nil {
		isEnabled = *input.IsEnabled
	}
	fallbackToDirect := false
	if input.ProxyFallbackToDirect != nil {
		fallbackToDirect = *input.ProxyFallbackToDirect
	}

	return webhookNormalizedTarget{
		Name:                  strings.TrimSpace(input.Name),
		ProviderType:          providerType,
		WebhookURL:            webhookGatedURL(providerType, webhookURL),
		TelegramBotToken:      webhookGatedBotToken(providerType, botToken),
		TelegramChatID:        webhookGatedChatID(providerType, chatID),
		DingtalkSecret:        webhookGatedDingtalkSecret(providerType, dingtalkSecret),
		CustomTemplate:        webhookGatedRaw(providerType == "custom", customTemplate),
		CustomHeaders:         webhookGatedRaw(providerType == "custom", customHeaders),
		ProxyURL:              proxyURL,
		ProxyFallbackToDirect: fallbackToDirect,
		IsEnabled:             isEnabled,
	}, nil
}

// webhookNormalizeUpdate 复刻 normalizeTargetUpdateInput：未出现的字段沿用既有值。
func webhookNormalizeUpdate(
	existing store.AdminWebhookTarget,
	input webhookTargetInput,
) (webhookNormalizedTarget, *ActionError) {
	providerType := existing.ProviderType
	if input.ProviderTypePresent && input.ProviderType != "" {
		providerType = input.ProviderType
	}

	webhookURL := webhookPickString(input.WebhookURL, existing.WebhookURL)
	botToken := webhookPickString(input.TelegramBotToken, existing.TelegramBotToken)
	chatID := webhookPickString(input.TelegramChatID, existing.TelegramChatID)
	dingtalkSecret := webhookPickString(input.DingtalkSecret, existing.DingtalkSecret)
	proxyURL := webhookPickString(input.ProxyURL, existing.ProxyURL)

	// 未解析的脱敏 URL 回显：Node 在 action 前把 "[REDACTED]" 删掉（保留原值）。
	if webhookURL != nil && *webhookURL == "[REDACTED]" {
		webhookURL = existing.WebhookURL
	}

	if proxyURL != nil && webhookIsRedactedURLEcho(*proxyURL, existing.ProxyURL) {
		proxyURL = existing.ProxyURL
	}
	if proxyURL != nil && !webhookIsValidProxyURL(*proxyURL) {
		return webhookNormalizedTarget{}, webhookActionError(
			"代理地址格式不正确（支持 http:// https:// socks5:// socks4://）")
	}

	customTemplate, actionErr := webhookResolveCustomTemplate(providerType, input, &existing)
	if actionErr != nil {
		return webhookNormalizedTarget{}, actionErr
	}
	if err := webhookValidateProviderConfig(providerType, webhookURL, botToken, chatID,
		webhookCustomTemplateProvided(providerType, customTemplate)); err != nil {
		return webhookNormalizedTarget{}, webhookActionError(err.Error())
	}
	customHeaders, actionErr := webhookResolveCustomHeaders(providerType, input, &existing)
	if actionErr != nil {
		return webhookNormalizedTarget{}, actionErr
	}

	name := existing.Name
	if input.NamePresent {
		name = strings.TrimSpace(input.Name)
	}
	fallbackToDirect := existing.ProxyFallbackToDirect
	if input.ProxyFallbackToDirect != nil {
		fallbackToDirect = *input.ProxyFallbackToDirect
	}
	isEnabled := existing.IsEnabled
	if input.IsEnabled != nil {
		isEnabled = *input.IsEnabled
	}

	return webhookNormalizedTarget{
		Name:                  name,
		ProviderType:          providerType,
		WebhookURL:            webhookGatedURL(providerType, webhookURL),
		TelegramBotToken:      webhookGatedBotToken(providerType, botToken),
		TelegramChatID:        webhookGatedChatID(providerType, chatID),
		DingtalkSecret:        webhookGatedDingtalkSecret(providerType, dingtalkSecret),
		CustomTemplate:        webhookGatedRaw(providerType == "custom", customTemplate),
		CustomHeaders:         webhookGatedRaw(providerType == "custom", customHeaders),
		ProxyURL:              proxyURL,
		ProxyFallbackToDirect: fallbackToDirect,
		IsEnabled:             isEnabled,
	}, nil
}

// webhookResolveCustomTemplate 复刻 parseCustomTemplate：字符串走 JSON.parse 并要求是对象。
//
// existing 非 nil 时表示更新路径（未提供则沿用既有模板）；创建路径的 nil 模板会由
// webhookValidateProviderConfig 判成非法（custom 必须给模板），与 Node 一致。
func webhookResolveCustomTemplate(
	providerType string,
	input webhookTargetInput,
	existing *store.AdminWebhookTarget,
) (json.RawMessage, *ActionError) {
	if providerType != "custom" {
		return nil, nil
	}
	raw := input.CustomTemplate
	if !input.CustomTemplatePresent && existing != nil {
		raw = existing.CustomTemplate
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if input.CustomTemplateIsString && input.CustomTemplatePresent {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, webhookActionError("自定义模板必须是 JSON 对象")
		}
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			return nil, nil
		}
		var decoded any
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			return nil, webhookActionError("自定义模板必须是 JSON 对象")
		}
		if _, ok := decoded.(map[string]any); !ok {
			return nil, webhookActionError("自定义模板必须是 JSON 对象")
		}
		return json.RawMessage(trimmed), nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, webhookActionError("自定义模板必须是 JSON 对象")
	}
	if _, ok := decoded.(map[string]any); !ok {
		return nil, webhookActionError("自定义模板必须是 JSON 对象")
	}
	return raw, nil
}

// webhookResolveCustomHeaders 复刻 customHeaders 的处理：只有 custom 渠道保留。
func webhookResolveCustomHeaders(
	providerType string,
	input webhookTargetInput,
	existing *store.AdminWebhookTarget,
) (json.RawMessage, *ActionError) {
	if providerType != "custom" {
		return nil, nil
	}
	if input.CustomHeadersPresent {
		if len(input.CustomHeaders) == 0 {
			return nil, nil
		}
		return input.CustomHeaders, nil
	}
	if existing != nil {
		return existing.CustomHeaders, nil
	}
	return nil, nil
}

// webhookCustomTemplateProvided 判断「模板参数是否已提供」（Node 的 `!== undefined`）。
//
// custom 渠道下模板恒被视为已提供（可能是 null），因此未给模板时会走到「自定义 Webhook 需要
// 配置模板」的报错分支——这正是 Node 的行为。
func webhookCustomTemplateProvided(providerType string, template json.RawMessage) bool {
	if providerType != "custom" {
		return true
	}
	return len(template) > 0 && string(template) != "null"
}

// webhookValidateProviderConfig 复刻 validateProviderConfig。
func webhookValidateProviderConfig(
	providerType string,
	webhookURL, botToken, chatID *string,
	templateProvided bool,
) error {
	if providerType == "telegram" {
		if webhookDeref(botToken) == "" || webhookDeref(chatID) == "" {
			return errors.New("Telegram 需要 Bot Token 和 Chat ID")
		}
		return nil
	}
	if webhookDeref(webhookURL) == "" {
		return errors.New("Webhook URL 不能为空")
	}
	if providerType == "custom" && !templateProvided {
		return errors.New("自定义 Webhook 需要配置模板")
	}
	return nil
}

// webhookPickString 取「出现则用新值，否则沿用既有值」的可空字符串。
func webhookPickString(input store.NullableString, existing *string) *string {
	if !input.Set {
		return existing
	}
	return input.Value
}

// webhookTrimToNull 复刻 trimToNull：trim 后为空即 nil。
func webhookTrimToNull(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// webhookGatedURL 复刻 `webhookUrl: providerType === "telegram" ? null : webhookUrl`。
func webhookGatedURL(providerType string, value *string) *string {
	if providerType == "telegram" {
		return nil
	}
	return value
}

// webhookGatedBotToken / webhookGatedChatID 复刻 telegram 专属字段的门控。
func webhookGatedBotToken(providerType string, value *string) *string {
	if providerType == "telegram" {
		return value
	}
	return nil
}

func webhookGatedChatID(providerType string, value *string) *string {
	if providerType == "telegram" {
		return value
	}
	return nil
}

// webhookGatedDingtalkSecret 复刻 `dingtalkSecret: providerType === "dingtalk" ? … : null`。
func webhookGatedDingtalkSecret(providerType string, value *string) *string {
	if providerType == "dingtalk" {
		return value
	}
	return nil
}

// webhookGatedRaw 复刻 custom 专属字段的门控（非 custom 一律 null）。
func webhookGatedRaw(isCustom bool, raw json.RawMessage) json.RawMessage {
	if !isCustom {
		return nil
	}
	return raw
}

// webhookIsValidProxyURL 复刻 isValidProxyUrl：协议白名单 + 必须有 hostname。
func webhookIsValidProxyURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks4":
	default:
		return false
	}
	return parsed.Hostname() != ""
}

// webhookRedactHeaders 复刻 redactHeaderRecord：名字命中密钥模式的值换成 "[REDACTED]"。
//
// 输出保持 JSON 对象（响应体里 customHeaders 是 object | null），键序不保证——Node 侧同样是
// Object.fromEntries 的重建结果，键序本就不是契约。
func webhookRedactHeaders(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var headers map[string]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		return raw
	}
	redacted := make(map[string]string, len(headers))
	for name, value := range headers {
		if webhookIsSecretHeaderName(name) {
			redacted[name] = "[REDACTED]"
			continue
		}
		redacted[name] = value
	}
	payload, err := adminMarshalJSON(redacted)
	if err != nil {
		return raw
	}
	return payload
}

// webhookSecretHeaderNames / webhookSecretHeaderPatterns 复刻 redaction.ts 的两张表。
var webhookSecretHeaderNames = map[string]struct{}{
	"authorization": {}, "x-api-key": {}, "cookie": {}, "set-cookie": {},
}

func webhookIsSecretHeaderName(name string) bool {
	lower := strings.ToLower(name)
	if _, ok := webhookSecretHeaderNames[lower]; ok {
		return true
	}
	for _, needle := range []string{"authorization", "apikey", "api-key", "api_key", "cookie",
		"token", "secret", "password"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// webhookRedactURLCredentials 复刻 redactUrlCredentials：带凭据时把用户名/口令换成 REDACTED。
func webhookRedactURLCredentials(value *string) *string {
	return webhookRedactedURLCredentials(value)
}

// webhookRedactedURLCredentials 的语义与上者相同，返回字符串（便于比较）。
func webhookRedactedURLCredentials(value *string) *string {
	if value == nil {
		return nil
	}
	parsed, err := url.Parse(*value)
	if err != nil {
		return value
	}
	if parsed.User == nil {
		return value
	}
	// 与 Node 同法：把用户名与口令都换成字面量 REDACTED 再序列化。
	parsed.User = url.UserPassword("REDACTED", "REDACTED")
	result := parsed.String()
	return &result
}

// webhookIsRedactedURLEcho 复刻 isRedactedUrlEcho：入参等于「脱敏后的既有值」即视为回显。
func webhookIsRedactedURLEcho(incoming string, existing *string) bool {
	if existing == nil {
		return false
	}
	redacted := webhookRedactedURLCredentials(existing)
	if redacted == nil {
		return false
	}
	if *redacted == *existing {
		return false
	}
	return *redacted == incoming
}

// webhookRestoreRedactedHeaders 复刻 restoreRedactedHeaderValues：把 "[REDACTED]" 还原成既有值。
//
// 大小写不敏感地回退到既有头（Node 同法：先按原名，再按小写名）。
func webhookRestoreRedactedHeaders(incoming, existing json.RawMessage) json.RawMessage {
	if len(incoming) == 0 {
		return incoming
	}
	var incomingMap map[string]string
	if err := json.Unmarshal(incoming, &incomingMap); err != nil {
		return incoming
	}
	if len(existing) == 0 || string(existing) == "null" {
		return incoming
	}
	var existingMap map[string]string
	if err := json.Unmarshal(existing, &existingMap); err != nil {
		return incoming
	}
	redactedExisting := map[string]string{}
	for name, value := range existingMap {
		if webhookIsSecretHeaderName(name) {
			redactedExisting[name] = "[REDACTED]"
			continue
		}
		redactedExisting[name] = value
	}
	existingByLower := map[string]string{}
	redactedByLower := map[string]string{}
	for name, value := range existingMap {
		existingByLower[strings.ToLower(name)] = value
	}
	for name, value := range redactedExisting {
		redactedByLower[strings.ToLower(name)] = value
	}

	restored := make(map[string]string, len(incomingMap))
	for name, value := range incomingMap {
		if value != "[REDACTED]" {
			restored[name] = value
			continue
		}
		if redactedExisting[name] == "[REDACTED]" {
			restored[name] = existingMap[name]
			continue
		}
		if redactedByLower[strings.ToLower(name)] == "[REDACTED]" {
			restored[name] = existingByLower[strings.ToLower(name)]
			continue
		}
		restored[name] = value
	}
	payload, err := adminMarshalJSON(restored)
	if err != nil {
		return incoming
	}
	return payload
}

// webhookUnresolvedRedactedHeaders 复刻 hasUnresolvedRedactedHeaderEcho。
func webhookUnresolvedRedactedHeaders(incoming, existing json.RawMessage) bool {
	if len(incoming) == 0 || string(incoming) == "null" {
		return false
	}
	var incomingMap map[string]string
	if err := json.Unmarshal(incoming, &incomingMap); err != nil {
		return false
	}
	redactedExisting := map[string]struct{}{}
	if len(existing) > 0 && string(existing) != "null" {
		var existingMap map[string]string
		if err := json.Unmarshal(existing, &existingMap); err == nil {
			for name := range existingMap {
				if webhookIsSecretHeaderName(name) {
					redactedExisting[strings.ToLower(name)] = struct{}{}
				}
			}
		}
	}
	for name, value := range incomingMap {
		if value == "[REDACTED]" {
			if _, ok := redactedExisting[strings.ToLower(name)]; !ok {
				return true
			}
		}
	}
	return false
}

// webhookHasRedactedPlaceholder 复刻 hasLegacyRedactedWritePlaceholders：任一字符串值含占位符。
func webhookHasRedactedPlaceholder(fields map[string]json.RawMessage) bool {
	for _, raw := range fields {
		if webhookValueHasRedactedPlaceholder(raw) {
			return true
		}
	}
	return false
}

func webhookValueHasRedactedPlaceholder(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "" || trimmed == "null":
		return false
	case strings.HasPrefix(trimmed, "\""):
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return false
		}
		return strings.Contains(text, "[REDACTED]") ||
			strings.Contains(text, "REDACTED:REDACTED@")
	case strings.HasPrefix(trimmed, "{"), strings.HasPrefix(trimmed, "["):
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return false
		}
		return webhookAnyHasRedactedPlaceholder(decoded)
	default:
		return false
	}
}

func webhookAnyHasRedactedPlaceholder(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.Contains(typed, "[REDACTED]") || strings.Contains(typed, "REDACTED:REDACTED@")
	case []any:
		for _, item := range typed {
			if webhookAnyHasRedactedPlaceholder(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if webhookAnyHasRedactedPlaceholder(item) {
				return true
			}
		}
	}
	return false
}

// webhookCoercePositiveInt 读取一个「coerce 后的正整数」字段（绑定输入里的 targetId 用）。
func webhookCoercePositiveInt(raw json.RawMessage) (int64, bool) {
	if adminJSONTypeName(raw) != "number" {
		return 0, false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	if value != float64(int64(value)) || value <= 0 {
		return 0, false
	}
	return int64(value), true
}
