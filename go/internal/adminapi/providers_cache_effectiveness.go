package adminapi

import (
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// providers_cache_effectiveness.go 实现 GET /providers/cache-effectiveness。
//
// 唯一真源：
//   - 路由与查询校验：src/app/api/v1/resources/providers/router.ts:301-320（`requireAuth("admin")`，
//     `ProviderCacheEffectivenessListQuerySchema`）；
//   - 处理逻辑：handlers.ts:342-355（listProviderCacheEffectiveness）→
//     actions/provider-cache-effectiveness.ts:20-45（管理员只读指标）；
//   - 数据：repository/provider-cache-effectiveness.ts:27-45。
//
// 响应形状：`{ "items": ProviderCacheEffectivenessWindow[] }`（handlers 里显式包 items，
// 与 model-suggestions 那种「正文即数组」不同）。
//
// 查询校验按 Zod 语义（本包 `coerceInt`/`coerceOptionalInt` 已复刻 `z.coerce`：
// 空串与空白串都按 0 处理，故 `providerId=` 落 `too_small` 而不是 `invalid_type`）。
func handleProviderCacheEffectiveness(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		values := request.URL.Query()
		issues := make([]invalidParam, 0, 2)

		var providerID *int64
		if raw, ok, issue := coerceOptionalInt(values, "providerId", 1, 0); issue != nil {
			issues = append(issues, providerCacheEffectivenessIssue(*issue))
		} else if ok {
			value := int64(raw)
			providerID = &value
		}

		limit := 50 // Node 的 `.default(50)`
		if raw, ok, issue := coerceOptionalInt(values, "limit", 1, 200); issue != nil {
			issues = append(issues, providerCacheEffectivenessIssue(*issue))
		} else if ok {
			limit = raw
		}

		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		rows, err := deps.Store.AdminListProviderCacheEffectiveness(request.Context(), providerID, limit)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		if rows == nil {
			rows = []store.AdminProviderCacheEffectivenessWindow{}
		}
		adminWriteJSON(writer, http.StatusOK, map[string]any{"items": rows})
	}
}

// providerCacheEffectivenessIssue 把 usage-logs 的校验 issue 投影成本包的 invalidParam。
// 两个结构字段逐字相同（Path/Code/Message），转换只是为了让校验失败走统一的
// adminWriteValidationFailure 形状。
func providerCacheEffectivenessIssue(issue usageLogsValidationIssue) invalidParam {
	return invalidParam{Path: issue.Path, Code: issue.Code, Message: issue.Message}
}
