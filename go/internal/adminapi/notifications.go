package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 notifications 资源模块（5 条端点）。
//
// 唯一真源：
//   - 路由与状态码：src/app/api/v1/resources/notifications/router.ts
//   - 处理器语义与脱敏：src/app/api/v1/resources/notifications/handlers.ts
//   - 业务规则与审计：src/actions/notifications.ts、src/actions/notification-bindings.ts
//   - 形状：src/lib/api/v1/schemas/notifications.ts
//   - SQL：src/repository/notifications.ts、src/repository/notification-bindings.ts
//
// 五条端点：
//
//	GET /notifications/settings                   全局通知设置（四个 legacy webhook 字段脱敏）
//	PUT /notifications/settings                   部分更新 + 审计
//	POST /notifications/test-webhook              向任意 URL 试发一条（恒 200，正文带 success）
//	GET /notifications/types/{type}/bindings      某类型的绑定列表（内联脱敏后的目标）
//	PUT /notifications/types/{type}/bindings      整组替换该类型的绑定（204）
//
// 登记进差异白名单的项：
//  1. **任务重排已移植**：Node 的 updateNotificationSettingsAction / replaceBindings 在写完后调用
//     scheduleNotifications()（增删 repeatable job，让总开关/时间/间隔立即生效）。Go 侧由
//     jobs.NotifyScheduler 承担，经 Deps.NotifyScheduler 注入：设置了就重排，未设置只记
//     warn（`admin_notification_schedule_skipped`）。两处触发点见 notificationReschedule 的调用点。
//     Go 侧仍是 fail-open：重排失败只记日志，不影响已经落库的设置与响应（Node 的 try/catch 同判）。
//  2. `PUT /notifications/settings` 的 `[REDACTED]` 回显处理与 Node 相同（删除该键，保留原值），
//     但 Node 的 action 层还会把「未提供的键」从 before/after 审计快照里剔除——Go 侧直接写
//     完整前后快照（审计内容更全，不改变 API 响应）。

// notificationSettingsResponse 逐字对应 NotificationSettingsSchema（四个 legacy 字段已脱敏）。
type notificationSettingsResponse struct {
	ID                                      int64   `json:"id"`
	Enabled                                 bool    `json:"enabled"`
	UseLegacyMode                           bool    `json:"useLegacyMode"`
	CircuitBreakerEnabled                   bool    `json:"circuitBreakerEnabled"`
	CircuitBreakerWebhook                   *string `json:"circuitBreakerWebhook"`
	DailyLeaderboardEnabled                 bool    `json:"dailyLeaderboardEnabled"`
	DailyLeaderboardWebhook                 *string `json:"dailyLeaderboardWebhook"`
	DailyLeaderboardTime                    *string `json:"dailyLeaderboardTime"`
	DailyLeaderboardTopN                    *int    `json:"dailyLeaderboardTopN"`
	CostAlertEnabled                        bool    `json:"costAlertEnabled"`
	CostAlertWebhook                        *string `json:"costAlertWebhook"`
	CostAlertThreshold                      *string `json:"costAlertThreshold"`
	CostAlertCheckInterval                  *int    `json:"costAlertCheckInterval"`
	CacheHitRateAlertEnabled                bool    `json:"cacheHitRateAlertEnabled"`
	CacheHitRateAlertWebhook                *string `json:"cacheHitRateAlertWebhook"`
	CacheHitRateAlertWindowMode             *string `json:"cacheHitRateAlertWindowMode"`
	CacheHitRateAlertCheckInterval          *int    `json:"cacheHitRateAlertCheckInterval"`
	CacheHitRateAlertHistoricalLookbackDays *int    `json:"cacheHitRateAlertHistoricalLookbackDays"`
	CacheHitRateAlertMinEligibleRequests    *int    `json:"cacheHitRateAlertMinEligibleRequests"`
	CacheHitRateAlertMinEligibleTokens      *int    `json:"cacheHitRateAlertMinEligibleTokens"`
	CacheHitRateAlertAbsMin                 *string `json:"cacheHitRateAlertAbsMin"`
	CacheHitRateAlertDropRel                *string `json:"cacheHitRateAlertDropRel"`
	CacheHitRateAlertDropAbs                *string `json:"cacheHitRateAlertDropAbs"`
	CacheHitRateAlertCooldownMinutes        *int    `json:"cacheHitRateAlertCooldownMinutes"`
	CacheHitRateAlertTopN                   *int    `json:"cacheHitRateAlertTopN"`
	CreatedAt                               *string `json:"createdAt"`
	UpdatedAt                               *string `json:"updatedAt"`
}

// notificationBindingResponse 逐字对应 NotificationBindingSchema（target 已脱敏）。
type notificationBindingResponse struct {
	ID               int64                 `json:"id"`
	NotificationType string                `json:"notificationType"`
	TargetID         int64                 `json:"targetId"`
	IsEnabled        bool                  `json:"isEnabled"`
	ScheduleCron     *string               `json:"scheduleCron"`
	ScheduleTimezone *string               `json:"scheduleTimezone"`
	TemplateOverride json.RawMessage       `json:"templateOverride"`
	CreatedAt        *string               `json:"createdAt"`
	Target           webhookTargetResponse `json:"target"`
}

// notificationBindingListResponse 对应 `jsonResponse({ items })`。
type notificationBindingListResponse struct {
	Items []notificationBindingResponse `json:"items"`
}

// notificationTestWebhookResponse 对应 NotificationTestWebhookResponseSchema。
type notificationTestWebhookResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// notificationSettingsUpdateFields 对应 NotificationSettingsUpdateSchema 的键集
// （NotificationSettingsSchema 去掉 id/createdAt/updatedAt 后的 .partial().strict()）。
var notificationSettingsUpdateFields = []string{
	"enabled", "useLegacyMode",
	"circuitBreakerEnabled", "circuitBreakerWebhook",
	"dailyLeaderboardEnabled", "dailyLeaderboardWebhook", "dailyLeaderboardTime",
	"dailyLeaderboardTopN",
	"costAlertEnabled", "costAlertWebhook", "costAlertThreshold", "costAlertCheckInterval",
	"cacheHitRateAlertEnabled", "cacheHitRateAlertWebhook", "cacheHitRateAlertWindowMode",
	"cacheHitRateAlertCheckInterval", "cacheHitRateAlertHistoricalLookbackDays",
	"cacheHitRateAlertMinEligibleRequests", "cacheHitRateAlertMinEligibleTokens",
	"cacheHitRateAlertAbsMin", "cacheHitRateAlertDropRel", "cacheHitRateAlertDropAbs",
	"cacheHitRateAlertCooldownMinutes", "cacheHitRateAlertTopN",
}

// notificationBindingFields 对应 NotificationBindingInputSchema 的键集（strict）。
var notificationBindingFields = []string{
	"targetId", "isEnabled", "scheduleCron", "scheduleTimezone", "templateOverride",
}

// RegisterNotifications 注册本模块的五条路由。
func RegisterNotifications(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_notifications_store_unwired", map[string]any{
				"module": "notifications",
				"action": "routes_not_registered",
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/notifications/settings",
		Access:      AccessAdmin,
		Module:      "notifications",
		OperationID: "getNotificationSettings",
		Handler:     http.HandlerFunc(handleGetNotificationSettings(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPut,
		Path:        "/notifications/settings",
		Access:      AccessAdmin,
		Module:      "notifications",
		OperationID: "updateNotificationSettings",
		Handler:     http.HandlerFunc(handleUpdateNotificationSettings(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/notifications/test-webhook",
		Access:      AccessAdmin,
		Module:      "notifications",
		OperationID: "testNotificationWebhook",
		Handler:     http.HandlerFunc(handleTestNotificationWebhook(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/notifications/types/{type}/bindings",
		Access:      AccessAdmin,
		Module:      "notifications",
		OperationID: "listNotificationBindings",
		Handler:     http.HandlerFunc(handleListNotificationBindings(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPut,
		Path:        "/notifications/types/{type}/bindings",
		Access:      AccessAdmin,
		Module:      "notifications",
		OperationID: "replaceNotificationBindings",
		Handler:     http.HandlerFunc(handleReplaceNotificationBindings(deps)),
	})
}

// handleGetNotificationSettings 复刻 getNotificationSettings + sanitizeLegacyNotificationSettingsResponse。
func handleGetNotificationSettings(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		settings, err := deps.Store.AdminGetNotificationSettings(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("notification", err))
			return
		}
		adminWriteJSON(writer, http.StatusOK, notificationSettingsPayload(settings))
	}
}

// handleUpdateNotificationSettings 复刻 updateNotificationSettings（handlers.ts:20-35 + actions:39-76）。
func handleUpdateNotificationSettings(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, notificationSettingsUpdateFields...)
		object.RejectUnknownKeys()
		update := notificationParseSettingsUpdate(object)
		if issues := object.issues0(); issues != nil {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		before, err := deps.Store.AdminGetNotificationSettings(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("notification", err))
			return
		}
		updated, err := deps.Store.AdminUpdateNotificationSettings(request.Context(), before.ID, update)
		if err != nil {
			adminEmitAudit(deps, request, AuditEvent{
				Category:     "notification",
				Action:       "notification.update",
				TargetType:   "notification",
				Success:      false,
				ErrorMessage: "UPDATE_FAILED",
			})
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("notification", err))
			return
		}

		// 见文件头差异 #1：Node 在此重排 Bull 作业；Go 侧由注入的调度器重排（未装配则记 warn）。
		notificationReschedule(deps, request)

		event := AuditEvent{
			Category:   "notification",
			Action:     "notification.update",
			TargetType: "notification",
			Before:     notificationAuditSnapshot(before),
			Details:    notificationAuditSnapshot(updated),
			Success:    true,
		}
		adminEmitAudit(deps, request, event)
		adminWriteJSON(writer, http.StatusOK, notificationSettingsPayload(updated))
	}
}

// handleTestNotificationWebhook 复刻 testNotificationWebhook（handlers.ts:47-61 + actions:81-102）。
//
// 注意状态码：Node 的处理器对 `!result.ok` 才走 actionError，而 testWebhookAction 无论成败都返回
// `ok: true`（data 里带 success/error），故**恒 200**；失败信息在正文里。
func handleTestNotificationWebhook(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, "webhookUrl", "type")
		object.RejectUnknownKeys()
		webhookURL, _ := object.String("webhookUrl",
			adminStringSpec{Required: true, Trim: true, MinRunes: 1})
		notificationType, _ := object.String("type",
			adminStringSpec{Required: true, Enum: webhookNotificationTypes})
		if issues := object.issues0(); issues != nil {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		// Node 的 notifier 由 URL 推断渠道：只认企业微信与飞书，其它主机名一律报错。
		providerType, err := notificationDetectProvider(webhookURL)
		if err != nil {
			adminWriteJSON(writer, http.StatusOK, notificationTestWebhookResponse{
				Success: false,
				Error:   err.Error(),
			})
			return
		}
		timezone := deps.Store.AdminSystemTimezoneOrUTC(request.Context())
		url := webhookURL
		result := adminSendWebhook(request.Context(), store.AdminWebhookTarget{
			ProviderType: providerType,
			WebhookURL:   &url,
		}, webhookSendOptions{
			NotificationType: notificationType,
			Timezone:         timezone,
			// NotificationTestWebhook 用 `new WebhookNotifier(url, {maxRetries: 1})`：总尝试 1 次。
			MaxAttempts: 1,
		})
		adminWriteJSON(writer, http.StatusOK, notificationTestWebhookResponse{
			Success: result.Success,
			Error:   result.Error,
		})
	}
}

// handleListNotificationBindings 复刻 getNotificationBindings（handlers.ts:63-74）。
func handleListNotificationBindings(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		notificationType, ok := notificationTypeParam(writer, request)
		if !ok {
			return
		}
		bindings, err := deps.Store.AdminListNotificationBindings(request.Context(), notificationType)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("notification", err))
			return
		}
		items := make([]notificationBindingResponse, 0, len(bindings))
		for _, binding := range bindings {
			items = append(items, notificationBindingPayload(binding))
		}
		adminWriteJSON(writer, http.StatusOK, notificationBindingListResponse{Items: items})
	}
}

// handleReplaceNotificationBindings 复刻 updateNotificationBindings（handlers.ts:76-91）。
func handleReplaceNotificationBindings(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		notificationType, ok := notificationTypeParam(writer, request)
		if !ok {
			return
		}
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, "items")
		object.RejectUnknownKeys()
		raw, present := object.Raw("items")
		if !present {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path: []any{"items"}, Code: "invalid_type", Message: "Required",
			}})
			return
		}
		items, parseErr := notificationParseBindingItems(raw)
		if parseErr != nil {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path: []any{"items"}, Code: "invalid_type", Message: parseErr.Error(),
			}})
			return
		}
		if issues := object.issues0(); issues != nil {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		inputs, err := notificationNormalizeBindings(deps, request, items)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("notification", err))
			return
		}
		if err := deps.Store.AdminReplaceNotificationBindings(request.Context(), notificationType,
			inputs); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("notification", err))
			return
		}
		// 绑定变了，定时任务的「谁收、按什么时刻收」都变了：与 settings PUT 同样重排。
		notificationReschedule(deps, request)
		adminWriteNoContent(writer)
	}
}

// notificationSettingsPayload 复刻 sanitizeLegacyNotificationSettingsResponse：四个 legacy webhook
// 字段非空即 "[REDACTED]"。
func notificationSettingsPayload(settings store.AdminNotificationSettings) notificationSettingsResponse {
	return notificationSettingsResponse{
		ID:                                      settings.ID,
		Enabled:                                 settings.Enabled,
		UseLegacyMode:                           settings.UseLegacyMode,
		CircuitBreakerEnabled:                   settings.CircuitBreakerEnabled,
		CircuitBreakerWebhook:                   notificationRedactWebhook(settings.CircuitBreakerWebhook),
		DailyLeaderboardEnabled:                 settings.DailyLeaderboardEnabled,
		DailyLeaderboardWebhook:                 notificationRedactWebhook(settings.DailyLeaderboardWebhook),
		DailyLeaderboardTime:                    settings.DailyLeaderboardTime,
		DailyLeaderboardTopN:                    settings.DailyLeaderboardTopN,
		CostAlertEnabled:                        settings.CostAlertEnabled,
		CostAlertWebhook:                        notificationRedactWebhook(settings.CostAlertWebhook),
		CostAlertThreshold:                      settings.CostAlertThreshold,
		CostAlertCheckInterval:                  settings.CostAlertCheckInterval,
		CacheHitRateAlertEnabled:                settings.CacheHitRateAlertEnabled,
		CacheHitRateAlertWebhook:                notificationRedactWebhook(settings.CacheHitRateAlertWebhook),
		CacheHitRateAlertWindowMode:             settings.CacheHitRateAlertWindowMode,
		CacheHitRateAlertCheckInterval:          settings.CacheHitRateAlertCheckInterval,
		CacheHitRateAlertHistoricalLookbackDays: settings.CacheHitRateAlertHistoricalLookbackDays,
		CacheHitRateAlertMinEligibleRequests:    settings.CacheHitRateAlertMinEligibleRequests,
		CacheHitRateAlertMinEligibleTokens:      settings.CacheHitRateAlertMinEligibleTokens,
		CacheHitRateAlertAbsMin:                 settings.CacheHitRateAlertAbsMin,
		CacheHitRateAlertDropRel:                settings.CacheHitRateAlertDropRel,
		CacheHitRateAlertDropAbs:                settings.CacheHitRateAlertDropAbs,
		CacheHitRateAlertCooldownMinutes:        settings.CacheHitRateAlertCooldownMinutes,
		CacheHitRateAlertTopN:                   settings.CacheHitRateAlertTopN,
		CreatedAt:                               settings.CreatedAt,
		UpdatedAt:                               settings.UpdatedAt,
	}
}

// notificationRedactWebhook 复刻 redactWebhookField：undefined 保持 undefined、空值保持空值、
// 非空值换成 "[REDACTED]"。Go 侧 nil 即 JSON null（与 Node 的 null 一致）。
func notificationRedactWebhook(value *string) *string {
	if value == nil || *value == "" {
		return value
	}
	redacted := "[REDACTED]"
	return &redacted
}

// notificationBindingPayload 复刻 sanitizeBinding：内联目标按目标的脱敏规则处理。
func notificationBindingPayload(binding store.AdminNotificationBinding) notificationBindingResponse {
	return notificationBindingResponse{
		ID:               binding.ID,
		NotificationType: binding.NotificationType,
		TargetID:         binding.TargetID,
		IsEnabled:        binding.IsEnabled,
		ScheduleCron:     binding.ScheduleCron,
		ScheduleTimezone: binding.ScheduleTimezone,
		TemplateOverride: binding.TemplateOverride,
		CreatedAt:        binding.CreatedAt,
		Target:           webhookTargetPayload(binding.Target),
	}
}

// notificationAuditSnapshot 把设置行摊平成审计快照（形状与 Node 的 before/after 一致：
// 就是仓储返回的对象）。
func notificationAuditSnapshot(settings store.AdminNotificationSettings) map[string]any {
	return map[string]any{
		"id":                               settings.ID,
		"enabled":                          settings.Enabled,
		"useLegacyMode":                    settings.UseLegacyMode,
		"circuitBreakerEnabled":            settings.CircuitBreakerEnabled,
		"circuitBreakerWebhook":            settings.CircuitBreakerWebhook,
		"dailyLeaderboardEnabled":          settings.DailyLeaderboardEnabled,
		"dailyLeaderboardWebhook":          settings.DailyLeaderboardWebhook,
		"dailyLeaderboardTime":             settings.DailyLeaderboardTime,
		"dailyLeaderboardTopN":             settings.DailyLeaderboardTopN,
		"costAlertEnabled":                 settings.CostAlertEnabled,
		"costAlertWebhook":                 settings.CostAlertWebhook,
		"costAlertThreshold":               settings.CostAlertThreshold,
		"costAlertCheckInterval":           settings.CostAlertCheckInterval,
		"cacheHitRateAlertEnabled":         settings.CacheHitRateAlertEnabled,
		"cacheHitRateAlertWindowMode":      settings.CacheHitRateAlertWindowMode,
		"cacheHitRateAlertCheckInterval":   settings.CacheHitRateAlertCheckInterval,
		"cacheHitRateAlertCooldownMinutes": settings.CacheHitRateAlertCooldownMinutes,
		"cacheHitRateAlertTopN":            settings.CacheHitRateAlertTopN,
		"cacheHitRateAlertAbsMin":          settings.CacheHitRateAlertAbsMin,
		"cacheHitRateAlertDropRel":         settings.CacheHitRateAlertDropRel,
		"cacheHitRateAlertDropAbs":         settings.CacheHitRateAlertDropAbs,
	}
}

// notificationReschedule 重排通知定时任务（见文件头差异 #1）。
//
// 两条纪律：
//   - **fail-open**：重排失败只记日志，不改变已经成功的 HTTP 响应（Node 的 catch-并-日志同判）；
//   - **未装配要说出来**：scheduler 为 nil 时记 warn，因为「设置改了但定时通知仍按旧时刻触发」
//     是静默错行为，管理页看不出来。
func notificationReschedule(deps Deps, request *http.Request) {
	if deps.Logger == nil {
		return
	}
	if deps.NotifyScheduler == nil {
		deps.Logger.Warn("admin_notification_schedule_skipped", map[string]any{
			"reason": "通知调度器未装配",
			"path":   request.URL.Path,
		})
		return
	}
	if err := deps.NotifyScheduler.Reschedule(request.Context()); err != nil {
		deps.Logger.Warn("admin_notification_reschedule_failed", map[string]any{
			"path":  request.URL.Path,
			"error": err.Error(),
		})
	}
}

// notificationDetectProvider 复刻 WebhookNotifier.detectProvider：只认两家主机名。
func notificationDetectProvider(webhookURL string) (string, error) {
	parsedURL, err := url.Parse(webhookURL)
	if err != nil {
		return "", fmt.Errorf("Unsupported webhook hostname: %s", webhookURL)
	}
	parsed := parsedURL.Hostname()
	switch parsed {
	case "qyapi.weixin.qq.com":
		return "wechat", nil
	case "open.feishu.cn":
		return "feishu", nil
	default:
		return "", fmt.Errorf("Unsupported webhook hostname: %s", parsed)
	}
}

// notificationTypeParam 解析 {type} 并校验枚举（Node 用 NotificationTypeParamSchema 走 zod 400）。
func notificationTypeParam(writer http.ResponseWriter, request *http.Request) (string, bool) {
	value := ParamsFrom(request.Context())["type"]
	for _, candidate := range webhookNotificationTypes {
		if value == candidate {
			return value, true
		}
	}
	adminWriteValidationFailure(writer, request, []invalidParam{{
		Path:    []any{"type"},
		Code:    "invalid_enum_value",
		Message: "Invalid enum value. Expected " + adminEnumList(webhookNotificationTypes) + ", received '" + value + "'",
	}})
	return "", false
}

// notificationSettingsUpdate 是本模块对 store 部分更新的解析结果。
func notificationParseSettingsUpdate(object *adminObject) store.AdminNotificationSettingsUpdate {
	var update store.AdminNotificationSettingsUpdate

	update.Enabled, _ = object.Bool("enabled")
	update.UseLegacyMode, _ = object.Bool("useLegacyMode")
	update.CircuitBreakerEnabled, _ = object.Bool("circuitBreakerEnabled")
	update.CircuitBreakerWebhook = notificationNullableWebhook(object, "circuitBreakerWebhook")
	update.DailyLeaderboardEnabled, _ = object.Bool("dailyLeaderboardEnabled")
	update.DailyLeaderboardWebhook = notificationNullableWebhook(object, "dailyLeaderboardWebhook")
	update.DailyLeaderboardTime = notificationNullableText(object, "dailyLeaderboardTime", 10)
	update.DailyLeaderboardTopN = notificationNullableInt(object, "dailyLeaderboardTopN")
	update.CostAlertEnabled, _ = object.Bool("costAlertEnabled")
	update.CostAlertWebhook = notificationNullableWebhook(object, "costAlertWebhook")
	update.CostAlertThreshold = notificationNullableText(object, "costAlertThreshold", 0)
	update.CostAlertCheckInterval = notificationNullableInt(object, "costAlertCheckInterval")
	update.CacheHitRateAlertEnabled, _ = object.Bool("cacheHitRateAlertEnabled")
	update.CacheHitRateAlertWebhook = notificationNullableWebhook(object, "cacheHitRateAlertWebhook")
	update.CacheHitRateAlertWindowMode = notificationNullableText(object, "cacheHitRateAlertWindowMode", 10)
	update.CacheHitRateAlertCheckInterval = notificationNullableInt(object, "cacheHitRateAlertCheckInterval")
	update.CacheHitRateAlertHistoricalLookbackDays = notificationNullableInt(object,
		"cacheHitRateAlertHistoricalLookbackDays")
	update.CacheHitRateAlertMinEligibleRequests = notificationNullableInt(object,
		"cacheHitRateAlertMinEligibleRequests")
	update.CacheHitRateAlertMinEligibleTokens = notificationNullableInt(object,
		"cacheHitRateAlertMinEligibleTokens")
	update.CacheHitRateAlertAbsMin = notificationNullableText(object, "cacheHitRateAlertAbsMin", 0)
	update.CacheHitRateAlertDropRel = notificationNullableText(object, "cacheHitRateAlertDropRel", 0)
	update.CacheHitRateAlertDropAbs = notificationNullableText(object, "cacheHitRateAlertDropAbs", 0)
	update.CacheHitRateAlertCooldownMinutes = notificationNullableInt(object, "cacheHitRateAlertCooldownMinutes")
	update.CacheHitRateAlertTopN = notificationNullableInt(object, "cacheHitRateAlertTopN")

	return update
}

// notificationNullableWebhook 读一个 legacy webhook 字段，并把 "[REDACTED]" 回显剔除
// （复刻 preserveLegacyNotificationSettingsUpdateInput：删除该键 = 保留原值）。
func notificationNullableWebhook(object *adminObject, key string) store.AdminNullableText {
	raw, present := object.Raw(key)
	if !present {
		return store.AdminNullableText{}
	}
	if adminJSONTypeName(raw) == "string" {
		var value string
		if err := json.Unmarshal(raw, &value); err == nil && value == "[REDACTED]" {
			return store.AdminNullableText{}
		}
	}
	return notificationNullableText(object, key, 512)
}

// notificationNullableText 读一个 `string | null | undefined` 字段并转成 store.AdminNullableText。
func notificationNullableText(object *adminObject, key string, maxRunes int) store.AdminNullableText {
	value, _ := providerNullableString(object, key, maxRunes, []any{key})
	return toNullableText(value)
}

// notificationNullableInt 读一个 `number(int) | null | undefined` 字段。
func notificationNullableInt(object *adminObject, key string) store.AdminNullableInt {
	raw, present := object.Raw(key)
	if !present {
		return store.AdminNullableInt{}
	}
	if adminJSONTypeName(raw) == "null" {
		return store.AdminNullableInt{Present: true, Value: nil}
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		object.fail([]any{key}, "invalid_type", adminTypeMessage("number", raw))
		return store.AdminNullableInt{Present: true}
	}
	if value != float64(int(value)) {
		object.fail([]any{key}, "invalid_type", "Expected int, received float")
		return store.AdminNullableInt{Present: true}
	}
	intValue := int(value)
	return store.AdminNullableInt{Present: true, Value: &intValue}
}

// toNullableText 把 store.NullableString 转成 store.AdminNullableText。
func toNullableText(value store.NullableString) store.AdminNullableText {
	if !value.Set {
		return store.AdminNullableText{}
	}
	return store.AdminNullableText{Present: true, Value: value.Value}
}

// notificationBindingInput 是一条绑定的解析结果。
type notificationBindingInput struct {
	TargetID         int64
	IsEnabled        *bool
	ScheduleCron     store.NullableString
	ScheduleTimezone store.NullableString
	TemplateOverride json.RawMessage
	TemplatePresent  bool
}

// notificationParseBindingItems 解析 `{ items: [...] }` 的数组元素。
func notificationParseBindingItems(raw json.RawMessage) ([]notificationBindingInput, error) {
	if adminJSONTypeName(raw) != "array" {
		return nil, fmt.Errorf("Expected array, received %s", adminJSONTypeName(raw))
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("Expected array, received string")
	}
	parsed := make([]notificationBindingInput, 0, len(items))
	for index, item := range items {
		object := adminNewObject(item, notificationBindingFields...)
		object.RejectUnknownKeys()
		targetID := int64(0)
		if rawTarget, present := object.Raw("targetId"); present {
			parsed, ok := webhookCoercePositiveInt(rawTarget)
			if !ok {
				object.fail([]any{index, "targetId"}, "invalid_type", "Expected number, received string")
			} else {
				targetID = parsed
			}
		} else {
			object.fail([]any{index, "targetId"}, "invalid_type", "Required")
		}
		input := notificationBindingInput{TargetID: targetID}
		input.IsEnabled, _ = object.Bool("isEnabled")
		input.ScheduleCron, _ = providerNullableString(object, "scheduleCron", 100,
			[]any{index, "scheduleCron"})
		input.ScheduleTimezone, _ = providerNullableString(object, "scheduleTimezone", 50,
			[]any{index, "scheduleTimezone"})
		template, present := object.Raw("templateOverride")
		if present {
			input.TemplatePresent = true
			if adminJSONTypeName(template) != "null" {
				if adminJSONTypeName(template) != "object" {
					object.fail([]any{index, "templateOverride"}, "invalid_type",
						adminTypeMessage("object", template))
				} else {
					input.TemplateOverride = template
				}
			}
		}
		if issues := object.issues0(); issues != nil {
			return nil, fmt.Errorf("invalid binding item")
		}
		parsed = append(parsed, input)
	}
	return parsed, nil
}

// notificationNormalizeBindings 复刻 upsertBindings 的归一化：过滤非法 targetId、补默认时区。
func notificationNormalizeBindings(
	deps Deps,
	request *http.Request,
	items []notificationBindingInput,
) ([]store.AdminNotificationBindingInput, error) {
	// Node 的 resolveSystemTimezone()：绑定未给时区时补系统时区（缺失则 UTC）。
	var defaultTimezone string
	needsDefault := false
	for _, item := range items {
		if !item.ScheduleTimezone.Set || item.ScheduleTimezone.Value == nil {
			needsDefault = true
			break
		}
	}
	if needsDefault {
		defaultTimezone = deps.Store.AdminSystemTimezoneOrUTC(request.Context())
	}

	inputs := make([]store.AdminNotificationBindingInput, 0, len(items))
	for _, item := range items {
		if item.TargetID <= 0 {
			continue
		}
		input := store.AdminNotificationBindingInput{
			TargetID:  item.TargetID,
			IsEnabled: true,
		}
		if item.IsEnabled != nil {
			input.IsEnabled = *item.IsEnabled
		}
		if item.ScheduleCron.Set {
			input.ScheduleCron = item.ScheduleCron.Value
		}
		if item.ScheduleTimezone.Set {
			input.ScheduleTimezone = item.ScheduleTimezone.Value
		}
		if input.ScheduleTimezone == nil {
			input.ScheduleTimezone = &defaultTimezone
		}
		if item.TemplatePresent {
			input.TemplateOverride = item.TemplateOverride
		}
		inputs = append(inputs, input)
	}
	return inputs, nil
}
