package adminapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是两条根级 admin ops 端点。
//
// 唯一真源：
//   - GET/POST /api/admin/log-level：src/app/api/admin/log-level/route.ts
//   - POST /api/admin/log-cleanup/manual：src/app/api/admin/log-cleanup/manual/route.ts
//   - 清理语义：src/lib/log-cleanup/service.ts（Go 侧在 internal/store/admin_log_cleanup.go）
//
// 两条端点都在**管理面之外**（Node 的 /api/admin/* 不属于 /api/v1 那一套），故：
//   - 路径写绝对形式，且 NoManagementEnvelope=true（不打 X-API-Version，与 Node 一致）。
//   - 作答形状也不同：Node 用的是裸 `Response.json`（不是 RFC7807 Problem）。
//     下面两个 handler 因此手写 JSON，不经 adminProblemWriter。
//
// 登记进差异白名单的两项：
//  1. **未认证/无权限的作答**：Node 这两条路由自己 getSession() 判定，非 admin 一律 401 +
//     纯文本 "Unauthorized"（log-level）或 401 + `{"error":"Unauthorized"}`（log-cleanup）。
//     Go 侧沿用共享 Guard（AccessAdmin），未认证是 401 Problem JSON、已认证非管理员是 403。
//     HTTP 状态码在「未登录」这一档上一致；「已登录但非管理员」由 403 与 401 之别，
//     且正文是 Problem 而不是裸 JSON。要逐字对齐需给这两条路由单开一个不作答的档位，
//     不值得——本波登记为差异。
//  2. **log-level 的持久性**：Node 的 setLogLevel 只改进程内变量（重启即回初始级别）；
//     Go 侧同一语义（进程内原子值）。两边都不落库，一致。

// logLevelResponse 逐字对应 GET /api/admin/log-level 的正文。
type logLevelResponse struct {
	Level string `json:"level"`
}

// logLevelErrorResponse 对应 POST 的校验失败正文（Node 的 `{ error, validLevels }`）。
type logLevelErrorResponse struct {
	Error       string   `json:"error"`
	ValidLevels []string `json:"validLevels"`
}

// logLevelSuccessResponse 对应 POST 的成功正文。
type logLevelSuccessResponse struct {
	Success bool   `json:"success"`
	Level   string `json:"level"`
}

// logCleanupResponse 对应 POST /api/admin/log-cleanup/manual 的成功正文。
type logCleanupResponse struct {
	Success           bool   `json:"success"`
	TotalDeleted      int64  `json:"totalDeleted"`
	BatchCount        int    `json:"batchCount"`
	DurationMS        int64  `json:"durationMs"`
	SoftDeletedPurged int64  `json:"softDeletedPurged"`
	VacuumPerformed   bool   `json:"vacuumPerformed"`
	Error             string `json:"error,omitempty"`
}

// logCleanupErrorResponse 对应清理请求的失败正文（Node 的 `{ error, details }`）。
type logCleanupErrorResponse struct {
	Error   string `json:"error"`
	Details any    `json:"details,omitempty"`
}

// logCleanupRequest 是清理请求体（对应 cleanupRequestSchema）。
type logCleanupRequest struct {
	BeforeDate      *string         `json:"beforeDate"`
	AfterDate       *string         `json:"afterDate"`
	UserIDs         []int64         `json:"userIds"`
	ProviderIDs     []int64         `json:"providerIds"`
	StatusCodes     []int           `json:"statusCodes"`
	StatusCodeRange *statusCodeSpan `json:"statusCodeRange"`
	OnlyBlocked     *bool           `json:"onlyBlocked"`
	DryRun          *bool           `json:"dryRun"`
}

// statusCodeSpan 对应 `{ min, max }`。
type statusCodeSpan struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// RegisterAdminOps 注册两条根级 admin ops 端点。
func RegisterAdminOps(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_ops_store_unwired", map[string]any{
				"module": "admin-ops",
				"action": "routes_not_registered",
			})
		}
		return
	}
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/admin/log-level",
		Access:               AccessAdmin,
		Module:               "admin-ops",
		OperationID:          "getAdminLogLevel",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(handleGetLogLevel(deps)),
	})
	router.Add(Route{
		Method:               http.MethodPost,
		Path:                 "/api/admin/log-level",
		Access:               AccessAdmin,
		Module:               "admin-ops",
		OperationID:          "setAdminLogLevel",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(handleSetLogLevel(deps)),
	})
	router.Add(Route{
		Method:               http.MethodPost,
		Path:                 "/api/admin/log-cleanup/manual",
		Access:               AccessAdmin,
		Module:               "admin-ops",
		OperationID:          "runManualLogCleanup",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(handleManualLogCleanup(deps)),
	})
}

// handleGetLogLevel 复刻 GET /api/admin/log-level。
func handleGetLogLevel(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		adminWriteJSON(writer, http.StatusOK, logLevelResponse{Level: logx.CurrentLevel()})
	}
}

// handleSetLogLevel 复刻 POST /api/admin/log-level。
//
// 校验顺序与文案照 Node：非法级别回 400 + `{"error":"无效的日志级别","validLevels":[...]}`；
// 请求体不是合法 JSON 时回 500 + `{"error":"设置失败"}`（Node 在 catch 里统一这样作答）。
func handleSetLogLevel(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Level string `json:"level"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			adminWriteJSON(writer, http.StatusInternalServerError,
				map[string]string{"error": "设置失败"})
			return
		}
		if !logx.SetLevel(payload.Level) {
			adminWriteJSON(writer, http.StatusBadRequest, logLevelErrorResponse{
				Error:       "无效的日志级别",
				ValidLevels: logLevelNames(),
			})
			return
		}
		adminWriteJSON(writer, http.StatusOK, logLevelSuccessResponse{
			Success: true,
			Level:   payload.Level,
		})
	}
}

// handleManualLogCleanup 复刻 POST /api/admin/log-cleanup/manual。
func handleManualLogCleanup(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		var payload logCleanupRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			adminWriteJSON(writer, http.StatusBadRequest, logCleanupErrorResponse{
				Error:   "请求参数格式错误",
				Details: []map[string]any{{"code": "invalid_json", "message": err.Error()}},
			})
			return
		}

		conditions, issues := payload.cleanupConditions()
		if issues != nil {
			adminWriteJSON(writer, http.StatusBadRequest, logCleanupErrorResponse{
				Error:   "请求参数格式错误",
				Details: issues,
			})
			return
		}

		dryRun := payload.DryRun != nil && *payload.DryRun
		result := deps.Store.AdminCleanupUsageLogs(request.Context(), conditions, dryRun)
		adminWriteJSON(writer, http.StatusOK, logCleanupResponse{
			Success:           result.Error == "",
			TotalDeleted:      result.TotalDeleted,
			BatchCount:        result.BatchCount,
			DurationMS:        result.DurationMS,
			SoftDeletedPurged: result.SoftDeletedPurged,
			VacuumPerformed:   result.VacuumPerformed,
			Error:             result.Error,
		})
	}
}

// cleanupConditions 把请求体转成存储层条件；返回非 nil 的 issues 表示校验失败。
//
// 日期用 RFC3339 解析（Node 走 `new Date(...)`：无法解析的字符串会得到 Invalid Date，随后
// 会以「无有效条件」收尾；Go 侧把解析失败直接当参数错误，更早暴露问题——登记为差异）。
func (r logCleanupRequest) cleanupConditions() (store.AdminLogCleanupConditions, []map[string]any) {
	conditions := store.AdminLogCleanupConditions{
		UserIDs:     r.UserIDs,
		ProviderIDs: r.ProviderIDs,
		StatusCodes: r.StatusCodes,
	}
	if r.OnlyBlocked != nil {
		conditions.OnlyBlocked = *r.OnlyBlocked
	}
	if r.BeforeDate != nil {
		parsed, err := time.Parse(time.RFC3339, *r.BeforeDate)
		if err != nil {
			return conditions, []map[string]any{{
				"path":    []any{"beforeDate"},
				"code":    "invalid_string",
				"message": "Invalid ISO 8601 datetime",
			}}
		}
		conditions.BeforeDate = &parsed
	}
	if r.AfterDate != nil {
		parsed, err := time.Parse(time.RFC3339, *r.AfterDate)
		if err != nil {
			return conditions, []map[string]any{{
				"path":    []any{"afterDate"},
				"code":    "invalid_string",
				"message": "Invalid ISO 8601 datetime",
			}}
		}
		conditions.AfterDate = &parsed
	}
	if r.StatusCodeRange != nil {
		conditions.StatusCodeRange = &store.AdminStatusCodeRange{
			Min: r.StatusCodeRange.Min,
			Max: r.StatusCodeRange.Max,
		}
	}
	return conditions, nil
}

// logLevelNames 是 validLevels 的字符串形式。
func logLevelNames() []string {
	names := make([]string, 0, len(logx.ValidLevels))
	for _, level := range logx.ValidLevels {
		names = append(names, string(level))
	}
	return names
}
