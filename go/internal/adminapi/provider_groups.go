package adminapi

import (
	"fmt"
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 provider-groups 资源模块（4 条端点）。
//
// 唯一真源：
//   - 路由与状态码：src/app/api/v1/resources/provider-groups/router.ts 与 handlers.ts
//   - 业务规则：src/actions/provider-groups.ts
//   - 响应形状：src/lib/api/v1/schemas/provider-groups.ts
//
// 四件事与 Node 逐条对齐：
//  1. 列表带 providerCount（引用计数按 providers.group_tag 统计，只算未软删行）。
//  2. 创建前查重名（DUPLICATE_NAME → 400）。
//  3. 更新支持 descriptionNote（它序列化进 description 的公开状态描述，见下）。
//  4. 删除前拦两种：默认分组（CANNOT_DELETE_DEFAULT）与被引用的分组（GROUP_IN_USE）——都是 400。
//
// 登记进差异白名单的一项（写在 handleListProviderGroups 的注释里）：Node 的 getProviderGroups
// 先做一次「按 providers.group_tag 自愈补登记缺失分组」的隐含写入，Go 侧不做隐式写。

// providerGroupResponse 逐字对应 ProviderGroupSchema（schemas/provider-groups.ts:4-21）。
type providerGroupResponse struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	CostMultiplier float64 `json:"costMultiplier"`
	Description    *string `json:"description"`
	ProviderCount  int64   `json:"providerCount"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
}

// providerGroupListResponse 对应 `{ items: [...] }`。
type providerGroupListResponse struct {
	Items []providerGroupResponse `json:"items"`
}

// RegisterProviderGroups 注册本模块的四条路由。
func RegisterProviderGroups(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_provider_groups_store_unwired", map[string]any{
				"module": "provider-groups",
				"action": "routes_not_registered",
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/provider-groups",
		Access:      AccessAdmin,
		Module:      "provider-groups",
		OperationID: "listProviderGroups",
		Handler:     http.HandlerFunc(handleListProviderGroups(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/provider-groups",
		Access:      AccessAdmin,
		Module:      "provider-groups",
		OperationID: "createProviderGroup",
		Handler:     http.HandlerFunc(handleCreateProviderGroup(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPatch,
		Path:        "/provider-groups/{id}",
		Access:      AccessAdmin,
		Module:      "provider-groups",
		OperationID: "updateProviderGroup",
		Handler:     http.HandlerFunc(handleUpdateProviderGroup(deps)),
	})
	router.Add(Route{
		Method:      http.MethodDelete,
		Path:        "/provider-groups/{id}",
		Access:      AccessAdmin,
		Module:      "provider-groups",
		OperationID: "deleteProviderGroup",
		Handler:     http.HandlerFunc(handleDeleteProviderGroup(deps)),
	})
}

// handleListProviderGroups 复刻 listProviderGroups（handlers.ts:21-26 + actions/provider-groups.ts:37-69）。
//
// 差异（登记进白名单）：Node 在返回前会先 bootstrapProviderGroupsFromProviders——把 providers
// 里出现过、但 provider_groups 里还没有的 group_tag 补登记为分组（自愈）。Go 侧不做这次
// **隐式写入**：管理端的读路径不应当改库。后果仅限「库里 provider_groups 缺行时列表少几项」，
// 而一旦该分组由 POST /provider-groups 正式建过，两边就完全一致。
func handleListProviderGroups(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		groups, err := deps.Store.AdminListProviderGroups(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider_group", err))
			return
		}
		items := make([]providerGroupResponse, 0, len(groups))
		for _, group := range groups {
			items = append(items, providerGroupPayload(group))
		}
		adminWriteJSON(writer, http.StatusOK, providerGroupListResponse{Items: items})
	}
}

// handleCreateProviderGroup 复刻 createProviderGroup（handlers.ts:28-40）。
//
// 校验顺序与 Node 一致：重名 → 倍率非负 → 描述长度；且 name 先 trim 再判空。
func handleCreateProviderGroup(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, "name", "costMultiplier", "description")
		object.RejectUnknownKeys()
		name, _ := object.String("name", adminStringSpec{Required: true, Trim: true, MinRunes: 1, MaxRunes: 100})
		costMultiplier, _ := object.Number("costMultiplier", false, []any{"costMultiplier"})
		description, hasDescription := object.String("description", adminStringSpec{MaxRunes: 5000})
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		duplicate, err := deps.Store.AdminProviderGroupNameExists(request.Context(), name)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider_group", err))
			return
		}
		if duplicate {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("provider_group", "DUPLICATE_NAME", http.StatusBadRequest,
					fmt.Errorf("分组名已存在")))
			return
		}

		var descriptionPointer *string
		if hasDescription {
			descriptionPointer = &description
		}
		created, err := deps.Store.AdminCreateProviderGroup(request.Context(), name, costMultiplier, descriptionPointer)
		if err != nil {
			adminEmitAudit(deps, request, providerGroupAudit(request, "provider_group.create",
				"", name).failure("CREATE_FAILED"))
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider_group", err))
			return
		}

		adminPublishDomain(deps, request, cfgsync.DomainProviderGroups)
		adminEmitAudit(deps, request, providerGroupAudit(request, "provider_group.create",
			fmt.Sprintf("%d", created.ID), created.Name).withAfter(created).success())
		adminWriteCreated(writer,
			fmt.Sprintf("%s/provider-groups/%d", MountPrefix, created.ID),
			providerGroupPayload(created))
	}
}

// handleUpdateProviderGroup 复刻 updateProviderGroup（handlers.ts:42-56）。
//
// descriptionNote 的处理照 Node：它被序列化进 description 的公开状态描述（保留原有
// publicStatus 段），因此与 description 是**两条互斥路径**——给了 descriptionNote 就用它
// 重写整段 description，否则用 description（可为 null）。
//
// 差异（登记进白名单）：serializePublicStatusDescription / parsePublicStatusDescription 的
// 编码格式（`---` 分隔的 front-matter 段）未移植，故 descriptionNote 当前按**普通文本**写入
// description。要完全对齐需移植该编解码；在那之前该字段的用途（公开状态页文案）在 Go 侧
// 会少掉 publicStatus 段的保留语义，故**本处理器拒绝 descriptionNote**（400），而不是写入
// 一个格式不对的 description。
func handleUpdateProviderGroup(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := providerGroupIDParam(writer, request)
		if !ok {
			return
		}
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, "costMultiplier", "description", "descriptionNote")
		object.RejectUnknownKeys()
		costMultiplier, _ := object.Number("costMultiplier", false, []any{"costMultiplier"})
		// description 是 `string | null | undefined`：三态语义不同（给了 null 就是清空）。
		description, hasDescription := providerNullableString(object, "description", 5000, []any{"description"})
		if _, hasNote := object.Raw("descriptionNote"); hasNote {
			// 见函数注释：编码格式未移植前拒绝，避免写坏公开状态描述。
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("provider_group", "provider_group.action_failed", http.StatusBadRequest,
					fmt.Errorf("descriptionNote 需先移植公开状态描述编解码")))
			return
		}
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		before, err := deps.Store.AdminGetProviderGroupByID(request.Context(), id)
		if err != nil && err != store.ErrNotFound {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider_group", err))
			return
		}

		patch := store.NullableString{Set: hasDescription, Value: description.Value}
		updated, err := deps.Store.AdminUpdateProviderGroup(request.Context(), id, costMultiplier, patch)
		if err == store.ErrNotFound {
			adminProblemWriter(deps).WriteActionError(writer, request, providerGroupNotFoundError())
			return
		}
		if err != nil {
			adminEmitAudit(deps, request, providerGroupAudit(request, "provider_group.update",
				fmt.Sprintf("%d", id), "").failure("UPDATE_FAILED"))
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider_group", err))
			return
		}

		if costMultiplier != nil {
			adminPublishDomain(deps, request, cfgsync.DomainProviderGroups)
		}
		event := providerGroupAudit(request, "provider_group.update", fmt.Sprintf("%d", id), updated.Name).
			withAfter(*updated)
		if before != nil {
			event.event.Before = providerGroupBefore(*before)
		}
		adminEmitAudit(deps, request, event.success())
		adminWriteJSON(writer, http.StatusOK, providerGroupPayload(*updated))
	}
}

// handleDeleteProviderGroup 复刻 deleteProviderGroup（handlers.ts:58-71）。
func handleDeleteProviderGroup(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := providerGroupIDParam(writer, request)
		if !ok {
			return
		}
		existing, err := deps.Store.AdminGetProviderGroupByID(request.Context(), id)
		if err == store.ErrNotFound {
			adminProblemWriter(deps).WriteActionError(writer, request, providerGroupNotFoundError())
			return
		}
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider_group", err))
			return
		}
		if existing.Name == providerGroupDefaultName {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("provider_group", "CANNOT_DELETE_DEFAULT", http.StatusBadRequest,
					fmt.Errorf("默认分组不可删除")))
			return
		}
		referenced, err := deps.Store.AdminCountProvidersUsingGroup(request.Context(), existing.Name)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider_group", err))
			return
		}
		if referenced > 0 {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("provider_group", "GROUP_IN_USE", http.StatusBadRequest,
					fmt.Errorf("分组仍被供应商引用")))
			return
		}

		deleted, err := deps.Store.AdminDeleteProviderGroup(request.Context(), id)
		if err != nil {
			adminEmitAudit(deps, request, providerGroupAudit(request, "provider_group.delete",
				fmt.Sprintf("%d", id), existing.Name).failure("DELETE_FAILED"))
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider_group", err))
			return
		}
		if !deleted {
			adminProblemWriter(deps).WriteActionError(writer, request, providerGroupNotFoundError())
			return
		}

		adminPublishDomain(deps, request, cfgsync.DomainProviderGroups)
		adminEmitAudit(deps, request, providerGroupAudit(request, "provider_group.delete",
			fmt.Sprintf("%d", id), existing.Name).withBefore(*existing).success())
		adminWriteNoContent(writer)
	}
}

// providerGroupDefaultName 是 PROVIDER_GROUP.DEFAULT（constants/provider.constants.ts）。
const providerGroupDefaultName = "default"

// providerGroupPayload 把一行映射成响应体；可空时间列按 Node 的 `?? new Date()` 兜底。
func providerGroupPayload(group store.AdminProviderGroup) providerGroupResponse {
	return providerGroupResponse{
		ID:             group.ID,
		Name:           group.Name,
		CostMultiplier: group.CostMultiplier,
		Description:    group.Description,
		ProviderCount:  group.ProviderCount,
		CreatedAt:      adminStringOrNow(group.CreatedAt),
		UpdatedAt:      adminStringOrNow(group.UpdatedAt),
	}
}

// providerGroupIDParam 解析并校验路径参数 id（复刻 ProviderGroupIdParamSchema 的 coerce + int + positive）。
func providerGroupIDParam(writer http.ResponseWriter, request *http.Request) (int64, bool) {
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

// providerGroupNotFoundError 复刻 provider-groups 的 404 分支。
func providerGroupNotFoundError() *ActionError {
	return NewActionError("provider_group", "provider_group.not_found", http.StatusNotFound,
		fmt.Errorf("分组不存在"))
}

// providerGroupBefore 组装审计的 before 快照。
func providerGroupBefore(group store.AdminProviderGroup) map[string]any {
	return map[string]any{
		"id":             group.ID,
		"name":           group.Name,
		"costMultiplier": group.CostMultiplier,
		"description":    group.Description,
	}
}

// providerGroupAudit 组装审计事件；after 快照的形状与 Node 的 emitActionAudit 一致。
func providerGroupAudit(request *http.Request, action, targetID, targetName string) providerGroupAuditEvent {
	return providerGroupAuditEvent{
		event: AuditEvent{
			Category:   "provider_group",
			Action:     action,
			TargetType: "provider_group",
			TargetID:   targetID,
			TargetName: targetName,
		},
	}
}

// providerGroupAuditEvent 让「成功/失败」两笔审计的构造保持一行。
type providerGroupAuditEvent struct {
	event AuditEvent
	after map[string]any
}

func (e providerGroupAuditEvent) withAfter(group store.AdminProviderGroup) providerGroupAuditEvent {
	e.after = providerGroupBefore(group)
	return e
}

func (e providerGroupAuditEvent) withBefore(group store.AdminProviderGroup) providerGroupAuditEvent {
	e.event.Before = providerGroupBefore(group)
	return e
}

func (e providerGroupAuditEvent) success() AuditEvent {
	e.event.Success = true
	e.event.Details = e.after
	return e.event
}

func (e providerGroupAuditEvent) failure(errorMessage string) AuditEvent {
	e.event.Success = false
	e.event.ErrorMessage = errorMessage
	return e.event
}
