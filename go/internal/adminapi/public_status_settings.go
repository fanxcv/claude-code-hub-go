package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// PUT /api/v1/public/status/settings。
//
// 唯一真源：src/actions/public-status.ts:62 的 savePublicStatusSettings（+ handlers.ts:91 的
// updatePublicStatusSettings，+ src/lib/api/v1/schemas/public.ts:11 的 PublicStatusSettingsUpdateSchema）。
//
// 逐条语义：
//
//  1. **体是 strict**：顶层与每个 group 元素都拒未知键（zod `.strict()`）；顶层三键必填
//     （windowHours 1..168 整数、interval 整数、groups 数组）；
//  2. **interval 必须在选项集内**（`[5,15,30,60]`）——不在则 400 action 失败
//     （Node 的 `statusPage.form.aggregationIntervalMinutesInvalid`，无 errorCode ⇒
//     `public_status.action_failed`）；
//  3. **启用分组集合**由 `collectEnabledPublicStatusGroups` 归一出（slug 去重、模型清洗）；
//  4. 对**全部**现有分组逐个比对：属于启用集合的写入其 publicStatus 段，其余写 null；
//     **note 要保留**（Node 先解析旧描述取 note，再与新 publicStatus 一起序列化）；
//     序列化结果超过 16 KiB → 400（`statusPage.form.descriptionTooLong`）；
//     只有描述真的变了才进更新集（`updatedGroupCount` 数的是这个集合）；
//  5. 更新系统设置两列（windowHours / interval）与上述分组；
//  6. **发布配置投影**：成功且写入 → `PUBLIC_STATUS_BACKGROUND_REFRESH_PENDING`
//     （聚合侧的 rebuild-hint 未移植，见 dashboard_cache.go 的同一条登记）；
//     失败或未写入 → `PUBLIC_STATUS_PROJECTION_PUBLISH_FAILED`；
//  7. 响应 `{updatedGroupCount, configVersion, publicStatusProjectionWarningCode}`。
//
// 与 Node 的登记差异：
//
//   - Node 把「更新设置 + 逐组更新描述」包在一个 DB 事务里，Go 没有跨方法的写事务
//     （store 的写方法各自持连接）。**部分失败时会留下「设置已改、部分分组未改」的中间态**——
//     这是本端点唯一的结构性差异，运维可用再次提交同一份表单收口（幂等：比对后只写变化的组）；
//   - `revalidatePath` 是 Next 的缓存 API，静态导出下不存在对应物，故不移植
//     （页面改为客户端取数后，数据新鲜度由前端每次请求决定）。

// publicStatusSettingsIntervalOptions 复刻 constants.ts 的 PUBLIC_STATUS_INTERVAL_OPTIONS。
var publicStatusSettingsIntervalOptions = map[int]struct{}{5: {}, 15: {}, 30: {}, 60: {}}

const (
	publicStatusSettingsMaxWindowHours = 168
	publicStatusProjectionFailedCode   = "PUBLIC_STATUS_PROJECTION_PUBLISH_FAILED"
	publicStatusProjectionPendingCode  = "PUBLIC_STATUS_BACKGROUND_REFRESH_PENDING"
)

// PublicStatusSettingsGroup 是请求体里的一个分组（schema 的同名字段）。
type PublicStatusSettingsGroup struct {
	GroupName       string                          `json:"groupName"`
	DisplayName     *string                         `json:"displayName"`
	PublicGroupSlug *string                         `json:"publicGroupSlug"`
	ExplanatoryCopy *string                         `json:"explanatoryCopy"`
	SortOrder       *float64                        `json:"sortOrder"`
	PublicModels    []PublicStatusSettingsModelSpec `json:"publicModels"`
}

// PublicStatusSettingsModelSpec 是公开模型项：允许裸字符串或对象两种写法
// （与 config.ts 的 sanitizePublicModels 同宽容度）。
type PublicStatusSettingsModelSpec struct {
	ModelKey             string  `json:"modelKey"`
	ProviderTypeOverride *string `json:"providerTypeOverride"`
}

type publicStatusSettingsAPI struct {
	pools      *store.Pools
	publisher  PublicStatusPublisher
	invalidate Invalidator
	problems   ProblemWriter
	logger     *logx.Logger
	now        func() time.Time
}

// RegisterPublicStatusSettingsRoute 注册 PUT /public/status/settings。
//
// 依赖：Store（必）。PublicStatusPublisher / Invalidator 缺失时**仍注册**——它们是可选增强，
// 缺了只影响警告码与缓存广播，不影响「把设置写进库」这件正事（与 Node 在发布失败时
// 回警告码而不是回 500 同判）。
func RegisterPublicStatusSettingsRoute(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_public_status_settings_unwired", map[string]any{
				"module": "public",
				"reason": "store_missing",
				"path":   "/public/status/settings",
			})
		}
		return
	}
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	problems := deps.Problems
	if problems == nil {
		problems = NewProblems(logger)
	}
	api := &publicStatusSettingsAPI{
		pools:      deps.Store,
		publisher:  deps.PublicStatusPublisher,
		invalidate: deps.Invalidator,
		problems:   problems,
		logger:     logger,
		now:        time.Now,
	}
	router.Add(Route{
		Method:      http.MethodPut,
		Path:        "/public/status/settings",
		Access:      AccessAdmin,
		Module:      "public",
		OperationID: "updatePublicStatusSettings",
		Handler:     http.HandlerFunc(api.handlePut),
	})
}

func (api *publicStatusSettingsAPI) handlePut(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()

	body, problems := readPublicStatusSettingsBody(request)
	if len(problems) > 0 {
		api.problems.WriteValidationError(writer, request, problems)
		return
	}
	if _, allowed := publicStatusSettingsIntervalOptions[body.PublicStatusAggregationIntervalMinutes]; !allowed {
		// Node：action 返回无 errorCode 的业务错 ⇒ 400 public_status.action_failed。
		api.problems.WriteActionError(writer, request,
			NewActionError("public_status", "public_status.action_failed", http.StatusBadRequest, nil))
		return
	}

	enabled := pubstatus.CollectEnabledPublicStatusGroups(configuredGroupInputs(body.Groups))

	current, err := api.pools.AdminListProviderGroups(ctx)
	if err != nil {
		api.writeFailure(writer, request, "list_groups", err)
		return
	}

	enabledByName := make(map[string]pubstatus.EnabledPublicStatusGroup, len(enabled))
	for _, group := range enabled {
		enabledByName[group.GroupName] = group
	}

	updates := make([]publicStatusGroupUpdate, 0, len(current))
	for _, group := range current {
		parsed := pubstatus.ParsePublicStatusDescription(group.Description)
		var next *pubstatus.PublicStatusGroupConfig
		if configured, ok := enabledByName[group.Name]; ok {
			next = &pubstatus.PublicStatusGroupConfig{
				DisplayName:     configured.DisplayName,
				PublicGroupSlug: configured.PublicGroupSlug,
				ExplanatoryCopy: configured.ExplanatoryCopy,
				SortOrder:       &configured.SortOrder,
				PublicModels:    configured.PublicModels,
			}
		}
		// note 必须原样保留：Node 用 `serializePublicStatusDescription({ note: existing.note, … })`。
		encoded := pubstatus.SerializePublicStatusDescription(pubstatus.ParsedPublicStatusDescription{
			Note:         parsed.Note,
			PublicStatus: next,
		})
		if pubstatus.ExceedsProviderGroupDescriptionLimit(encoded) {
			api.problems.WriteActionError(writer, request,
				NewActionError("public_status", "public_status.action_failed", http.StatusBadRequest, nil))
			return
		}
		if equalOptionalString(group.Description, encoded) {
			continue
		}
		// 两个字段都要带上：`description` 用于变更判定，`publicStatus` 供写入时与**重读到的 note**
		// 一起重新序列化（Node 在事务里也是「重新取 note + 复用新 publicStatus」）。
		updates = append(updates, publicStatusGroupUpdate{
			id:           group.ID,
			description:  encoded,
			publicStatus: next,
		})
	}

	settings, err := api.pools.EnsureAdminSystemSettings(ctx)
	if err != nil {
		api.writeFailure(writer, request, "read_settings", err)
		return
	}
	if _, err := api.pools.UpdateAdminSystemSettings(ctx, settings.ID, store.AdminSystemSettingsPatch{
		Updates: map[store.AdminSystemSettingsColumn]any{
			store.ColPublicStatusWindowHours:     body.PublicStatusWindowHours,
			store.ColPublicStatusAggregationMins: body.PublicStatusAggregationIntervalMinutes,
		},
	}); err != nil {
		api.writeFailure(writer, request, "update_settings", err)
		return
	}

	for _, update := range updates {
		// Node 在事务里重新读一次分组取 note；这里同样重读，避免用可能已被并发改写的旧值。
		fresh, err := api.pools.AdminGetProviderGroupByID(ctx, update.id)
		if err != nil {
			api.writeFailure(writer, request, "read_group", err)
			return
		}
		note := pubstatus.ParsePublicStatusDescription(groupDescription(fresh)).Note
		encoded := pubstatus.SerializePublicStatusDescription(pubstatus.ParsedPublicStatusDescription{
			Note:         note,
			PublicStatus: update.publicStatus,
		})
		if _, err := api.pools.AdminUpdateProviderGroup(ctx, update.id, nil,
			store.NullableString{Set: true, Value: encoded}); err != nil {
			api.writeFailure(writer, request, "update_group", err)
			return
		}
	}

	configVersion := fmt.Sprintf("cfg-%d", api.now().UnixMilli())
	warningCode := publicStatusProjectionFailedCode
	if api.publisher != nil {
		result, publishErr := api.publisher.PublishCurrentProjection(ctx, "save-public-status-settings")
		if publishErr == nil && result.Written {
			// 聚合侧的 rebuild-hint 未移植（见 dashboard_cache.go 的同一条登记），
			// 故发布成功也如实回 PENDING，让运维知道公开状态页不会自己刷新。
			warningCode = publicStatusProjectionPendingCode
			if result.ConfigVersion != "" {
				configVersion = result.ConfigVersion
			}
		}
	}

	api.broadcastInvalidation(ctx)

	writeShellJSON(writer, http.StatusOK, struct {
		UpdatedGroupCount                 int     `json:"updatedGroupCount"`
		ConfigVersion                     string  `json:"configVersion"`
		PublicStatusProjectionWarningCode *string `json:"publicStatusProjectionWarningCode"`
	}{
		UpdatedGroupCount:                 len(updates),
		ConfigVersion:                     configVersion,
		PublicStatusProjectionWarningCode: &warningCode,
	})
}

// publicStatusGroupUpdate 是一条待写回的分组描述。
type publicStatusGroupUpdate struct {
	id           int64
	description  *string
	publicStatus *pubstatus.PublicStatusGroupConfig
}

// configuredGroupInputs 把请求体映射成 collectEnabledPublicStatusGroups 的入参
// （Node 的 normalizeEnabledGroups：note 恒为 null，公共段逐字段带过去）。
func configuredGroupInputs(groups []PublicStatusSettingsGroup) []pubstatus.PublicStatusConfiguredGroupInput {
	inputs := make([]pubstatus.PublicStatusConfiguredGroupInput, 0, len(groups))
	for _, group := range groups {
		models := make([]pubstatus.PublicStatusModelConfig, 0, len(group.PublicModels))
		for _, model := range group.PublicModels {
			entry := pubstatus.PublicStatusModelConfig{ModelKey: model.ModelKey}
			if model.ProviderTypeOverride != nil {
				entry.ProviderTypeOverride = *model.ProviderTypeOverride
			}
			models = append(models, entry)
		}
		displayName := ""
		if group.DisplayName != nil {
			displayName = *group.DisplayName
		}
		slug := ""
		if group.PublicGroupSlug != nil {
			slug = *group.PublicGroupSlug
		}
		inputs = append(inputs, pubstatus.PublicStatusConfiguredGroupInput{
			GroupName: group.GroupName,
			ParsedPublicStatusDescription: pubstatus.ParsedPublicStatusDescription{
				PublicStatus: &pubstatus.PublicStatusGroupConfig{
					DisplayName:     displayName,
					PublicGroupSlug: slug,
					ExplanatoryCopy: group.ExplanatoryCopy,
					SortOrder:       group.SortOrder,
					PublicModels:    models,
				},
			},
		})
	}
	return inputs
}

// readPublicStatusSettingsBody 解析并校验请求体（Node：parseHonoJsonBody + .strict() schema）。
//
// 严格性逐条对齐 zod：未知键、类型不符、越界都算校验失败；分组元素同样拒未知键。
func readPublicStatusSettingsBody(request *http.Request) (publicStatusSettingsBody, []InvalidParam) {
	raw, err := io.ReadAll(io.LimitReader(request.Body, 4<<20))
	if err != nil {
		return publicStatusSettingsBody{}, []InvalidParam{{
			Path: []any{}, Code: "invalid_body", Message: "Request body could not be read.",
		}}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return publicStatusSettingsBody{}, []InvalidParam{{
			Path: []any{}, Code: "invalid_json", Message: "Invalid JSON body.",
		}}
	}

	problems := make([]InvalidParam, 0, 2)
	allowedTopLevel := map[string]struct{}{
		"publicStatusWindowHours": {}, "publicStatusAggregationIntervalMinutes": {}, "groups": {},
	}
	for key := range object {
		if _, ok := allowedTopLevel[key]; !ok {
			// zod `.strict()` 的未知键：路径 + unrecognized_keys。
			problems = append(problems, InvalidParam{
				Path: []any{key}, Code: "unrecognized_keys", Message: "Unrecognized key(s) in object",
			})
		}
	}

	var body publicStatusSettingsBody
	if value, ok := object["publicStatusWindowHours"]; ok {
		var hours int
		if err := json.Unmarshal(value, &hours); err != nil || hours < 1 || hours > publicStatusSettingsMaxWindowHours {
			problems = append(problems, InvalidParam{
				Path: []any{"publicStatusWindowHours"}, Code: "invalid_type",
				Message: "Number must be an integer between 1 and 168",
			})
		} else {
			body.PublicStatusWindowHours = hours
		}
	} else {
		problems = append(problems, InvalidParam{
			Path: []any{"publicStatusWindowHours"}, Code: "invalid_type", Message: "Required",
		})
	}

	if value, ok := object["publicStatusAggregationIntervalMinutes"]; ok {
		var minutes int
		if err := json.Unmarshal(value, &minutes); err != nil {
			problems = append(problems, InvalidParam{
				Path: []any{"publicStatusAggregationIntervalMinutes"}, Code: "invalid_type",
				Message: "Expected number, received non-number",
			})
		} else {
			body.PublicStatusAggregationIntervalMinutes = minutes
		}
	} else {
		problems = append(problems, InvalidParam{
			Path: []any{"publicStatusAggregationIntervalMinutes"}, Code: "invalid_type", Message: "Required",
		})
	}

	if value, ok := object["groups"]; ok {
		groups, groupProblems := readPublicStatusSettingsGroups(value)
		problems = append(problems, groupProblems...)
		body.Groups = groups
	} else {
		problems = append(problems, InvalidParam{
			Path: []any{"groups"}, Code: "invalid_type", Message: "Required",
		})
	}

	if len(problems) > 0 {
		return publicStatusSettingsBody{}, problems
	}
	return body, nil
}

type publicStatusSettingsBody struct {
	PublicStatusWindowHours                int
	PublicStatusAggregationIntervalMinutes int
	Groups                                 []PublicStatusSettingsGroup
}

func readPublicStatusSettingsGroups(raw json.RawMessage) ([]PublicStatusSettingsGroup, []InvalidParam) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, []InvalidParam{{
			Path: []any{"groups"}, Code: "invalid_type", Message: "Expected array, received non-array",
		}}
	}
	allowed := map[string]struct{}{
		"groupName": {}, "displayName": {}, "publicGroupSlug": {},
		"explanatoryCopy": {}, "sortOrder": {}, "publicModels": {},
	}
	problems := make([]InvalidParam, 0)
	groups := make([]PublicStatusSettingsGroup, 0, len(entries))
	for index, entry := range entries {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(entry, &object); err != nil {
			problems = append(problems, InvalidParam{
				Path: []any{"groups", index}, Code: "invalid_type", Message: "Expected object",
			})
			continue
		}
		for key := range object {
			if _, ok := allowed[key]; !ok {
				problems = append(problems, InvalidParam{
					Path: []any{"groups", index, key}, Code: "unrecognized_keys",
					Message: "Unrecognized key(s) in object",
				})
			}
		}
		var group PublicStatusSettingsGroup
		if value, ok := object["groupName"]; ok {
			if err := json.Unmarshal(value, &group.GroupName); err != nil || group.GroupName == "" {
				problems = append(problems, InvalidParam{
					Path: []any{"groups", index, "groupName"}, Code: "too_small",
					Message: "String must contain at least 1 character(s)",
				})
			}
		} else {
			problems = append(problems, InvalidParam{
				Path: []any{"groups", index, "groupName"}, Code: "invalid_type", Message: "Required",
			})
		}
		if value, ok := object["displayName"]; ok {
			_ = json.Unmarshal(value, &group.DisplayName)
		}
		if value, ok := object["publicGroupSlug"]; ok {
			_ = json.Unmarshal(value, &group.PublicGroupSlug)
		}
		if value, ok := object["explanatoryCopy"]; ok {
			_ = json.Unmarshal(value, &group.ExplanatoryCopy)
		}
		if value, ok := object["sortOrder"]; ok {
			_ = json.Unmarshal(value, &group.SortOrder)
		}
		if value, ok := object["publicModels"]; ok {
			models, modelProblems := readPublicStatusSettingsModels(value, index)
			problems = append(problems, modelProblems...)
			group.PublicModels = models
		} else {
			problems = append(problems, InvalidParam{
				Path: []any{"groups", index, "publicModels"}, Code: "invalid_type", Message: "Required",
			})
		}
		groups = append(groups, group)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return groups, nil
}

func readPublicStatusSettingsModels(
	raw json.RawMessage,
	groupIndex int,
) ([]PublicStatusSettingsModelSpec, []InvalidParam) {
	// 与 config.ts 的 sanitizePublicModels 同宽容度：裸字符串或对象都收。
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, []InvalidParam{{
			Path: []any{"groups", groupIndex, "publicModels"}, Code: "invalid_type",
			Message: "Expected array, received non-array",
		}}
	}
	models := make([]PublicStatusSettingsModelSpec, 0, len(entries))
	for index, entry := range entries {
		var literal string
		if err := json.Unmarshal(entry, &literal); err == nil {
			models = append(models, PublicStatusSettingsModelSpec{ModelKey: literal})
			continue
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(entry, &object); err != nil {
			return nil, []InvalidParam{{
				Path: []any{"groups", groupIndex, "publicModels", index}, Code: "invalid_type",
				Message: "Expected string or object",
			}}
		}
		var model PublicStatusSettingsModelSpec
		if value, ok := object["modelKey"]; ok {
			if err := json.Unmarshal(value, &model.ModelKey); err != nil || model.ModelKey == "" {
				return nil, []InvalidParam{{
					Path: []any{"groups", groupIndex, "publicModels", index, "modelKey"},
					Code: "too_small", Message: "String must contain at least 1 character(s)",
				}}
			}
		} else {
			return nil, []InvalidParam{{
				Path: []any{"groups", groupIndex, "publicModels", index, "modelKey"},
				Code: "invalid_type", Message: "Required",
			}}
		}
		if value, ok := object["providerTypeOverride"]; ok {
			_ = json.Unmarshal(value, &model.ProviderTypeOverride)
		}
		models = append(models, model)
	}
	return models, nil
}

func (api *publicStatusSettingsAPI) writeFailure(
	writer http.ResponseWriter,
	request *http.Request,
	stage string,
	err error,
) {
	api.logger.Error("admin_public_status_settings_failed", map[string]any{
		"stage": stage,
		"path":  request.URL.Path,
		"error": err.Error(),
	})
	api.problems.WriteActionError(writer, request,
		NewActionError("public_status", "public_status.action_failed", http.StatusBadRequest, nil))
}

// broadcastInvalidation 广播两处缓存失效（Node 的 invalidateSystemSettingsCache +
// invalidateConfiguredPublicStatusGroupsCache）。Go 侧走 cfgsync 的域广播：system_settings 域
// 对应前者；分组描述落在 provider_groups，对应 providers 域（配置域枚举见 cfgsync.go）。
func (api *publicStatusSettingsAPI) broadcastInvalidation(ctx context.Context) {
	if api.invalidate == nil {
		return
	}
	api.invalidate.PublishDomain(ctx, cfgsync.DomainSystemSettings)
	api.invalidate.PublishDomain(ctx, cfgsync.DomainProviders)
}

func groupDescription(group *store.AdminProviderGroup) *string {
	if group == nil {
		return nil
	}
	return group.Description
}

// equalOptionalString 比两个可空字符串（nil 与空串**不等**：落库形态不同）。
func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
