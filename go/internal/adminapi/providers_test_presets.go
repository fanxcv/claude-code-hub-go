package adminapi

import (
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/providertest"
)

// providers_test_presets.go 实现 GET /providers/test:presets。
//
// 唯一真源：
//   - 路由与查询校验：src/app/api/v1/resources/providers/router.ts:819-836（`requireAuth("admin")`，
//     `ProviderTypeRouteQuerySchema`）；处理器 handlers.ts:487-505（getProviderTestPresets）；
//   - 动作：src/actions/providers.ts:5277-5307（getProviderTestPresets）；
//   - 预设数据：src/lib/provider-testing/presets.ts（本包 `internal/providertest` 已逐字移植）。
//
// 响应形状：**正文即数组** `[{id, description, defaultSuccessContains, defaultModel}]`
// （router 的 200 声明是 ProviderArrayResponseSchema，且 handlers 的 actionJson 对
// `result.data` 直出、不包 items——注意与同族 cache-effectiveness 的 `{items: [...]}` 不同）。
//
// 校验语义（按 zod 逐条对齐）：
//   - `providerType` 缺失 → `invalid_type`（z.enum 收到非 string）；
//   - 给了空串 → `invalid_enum_value`（是 string 但不在枚举里）；
//   - `ProviderTypeSchema` **不做 trim**（ProviderTypeQuerySchema 直接用 z.enum），故
//     `" claude "` 也是 `invalid_enum_value`；
//   - 隐藏类型（claude-auth / gemini-cli）仅当带 dashboard 兼容头且身份为管理员时才合法
//     （handlers.ts 的 InternalProviderTypeQuerySchema，与 provider_endpoints.go 同源判定）。
func handleProviderTestPresets(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		raw, present := request.URL.Query()["providerType"]
		if !present || len(raw) == 0 {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path:    []any{"providerType"},
				Code:    "invalid_type",
				Message: "Expected string, received undefined",
			}})
			return
		}
		providerType := raw[0]
		if !providerTypeAllowedForRequest(providerType, request) {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path:    []any{"providerType"},
				Code:    "invalid_enum_value",
				Message: "Invalid enum value",
			}})
			return
		}

		_ = deps // 本端点不读库；deps 保留以与同族处理器同签名（注册点统一传参）。
		ids := providertest.PresetIDsForTestPresetsEndpoint(providertest.ProviderType(providerType))
		items := make([]providerTestPresetPayload, 0, len(ids))
		for _, id := range ids {
			preset, ok := providertest.GetPreset(id)
			if !ok {
				continue
			}
			items = append(items, providerTestPresetPayload{
				ID:                     preset.ID,
				Description:            preset.Description,
				DefaultSuccessContains: preset.DefaultSuccessContains,
				DefaultModel:           preset.DefaultModel,
			})
		}
		adminWriteJSON(writer, http.StatusOK, items)
	}
}

// providerTestPresetPayload 逐字对应 actions/providers.ts:5295-5300 映射出的 PresetConfigResponse。
type providerTestPresetPayload struct {
	ID                     string `json:"id"`
	Description            string `json:"description"`
	DefaultSuccessContains string `json:"defaultSuccessContains"`
	DefaultModel           string `json:"defaultModel"`
}

// providerTypeAllowedForRequest 复刻「公开集 + 兼容头下的内部集」判定。
func providerTypeAllowedForRequest(providerType string, request *http.Request) bool {
	if !validProviderTypeValue(providerType) {
		return false
	}
	if hiddenProviderType(providerType) && !dashboardCompatRequest(request) {
		return false
	}
	return true
}
