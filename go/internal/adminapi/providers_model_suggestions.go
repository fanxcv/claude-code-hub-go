package adminapi

import (
	"net/http"
	"sort"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// providers_model_suggestions.go 实现 GET /providers/model-suggestions。
//
// 唯一真源：
//   - 路由与鉴权：src/app/api/v1/resources/providers/router.ts:869（`requireAuth("admin")`，
//     查询 `ProviderModelSuggestionsQuerySchema` = `{ providerGroup?: string | null }`）；
//   - 处理逻辑：src/actions/providers.ts:5677-5717（getModelSuggestionsByProviderGroup）；
//   - 响应形状：handlers.ts:801 的 `actionJson` → `jsonResponse(result.data ?? { ok: true })`，
//     即**正文是 string[] 本体**，不套 `{ ok, data }` 信封（前端 `apiGet<string[]>` 亦然）。
//
// 语义要点（逐条对齐 Node）：
//   - 供应商集合取 `findAllProviders()` 后按 `isEnabled` 过滤——注意**不过滤隐藏类型**
//     （claude-auth / gemini-cli 照收），与数据面选路不同；
//   - 分组：`providerGroup` 为**空串或缺失**时用 `["default"]`；非空则按 Node 的
//     `parseGroupString` 切分（空白串会切出空集，此时**不**回落 default——Node 的
//     `"" ? … : …` 只判假值，空格串是真值）；
//   - 只收 `matchType === "exact"` 的规则，去重后**升序**（Node 的 `Array.from(set).sort()`）。
func handleProviderModelSuggestions(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		raw := request.URL.Query().Get("providerGroup")
		userGroups := []string{route.GroupDefault}
		if raw != "" {
			userGroups = route.SplitProviderGroups(raw)
		}

		providers, err := deps.Store.AdminListProviders(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}

		modelSet := make(map[string]struct{})
		for _, provider := range providers {
			if !provider.IsEnabled {
				continue
			}
			if !route.CheckProviderGroupMatch(provider.GroupTag, userGroups) {
				continue
			}
			for _, pattern := range route.AllowedModelExactPatterns(provider.AllowedModels) {
				modelSet[pattern] = struct{}{}
			}
		}

		models := make([]string, 0, len(modelSet))
		for model := range modelSet {
			models = append(models, model)
		}
		// Node 的 `.sort()` 是 UTF-16 码元序，Go 的 sort.Strings 是字节序；模型名是 ASCII，
		// 两者同序。非 ASCII 的模型名会分歧——留此注记以免后人误判为等价。
		sort.Strings(models)

		adminWriteJSON(writer, http.StatusOK, models)
	}
}
