package adminapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件实现 usage-logs 的三条导出路由（Node 的 resources/usage-logs/handlers.ts:85-152）：
//
//	POST /usage-logs/exports                      read  同步出 CSV（JSON 正文 {"csv": "..."}）；
//	                                                   带 `Prefer: respond-async` 则转异步作业
//	GET  /usage-logs/exports/{jobId}              read  异步作业状态
//	GET  /usage-logs/exports/{jobId}/download     read  下载作业产物（CSV 文本 / XLSX 二进制）
//
// 三条路由**需要 Redis 的作业键值面**（status/result 两族键，TTL 15 分钟，键名与 Node 逐字
// 一致，切换期两侧互相可见）。没有键值面时不注册：请求照常回退 Node（那里是完整实现），
// 而不是由 Go 用一个「投了永远查不到」的半成品冒充。
//
// 权限档位一律 read（Node 路由表 211/243/265 行），越权由属主校验兜住：非 admin 的筛选条件被
// 强制改写为自己的 user id（resolveUsageLogFiltersForSession），且作业的属主不符时状态与下载
// 都按「作业不存在或已过期」作答——不泄露「这个 id 存在但不属于你」。

// usageLogsExportsModule 是导出侧的处理模块。
type usageLogsExportsModule struct {
	module *usageLogsModule
}

// exportActionProblems 取问题作答器（未装配时用内置兜底形状）。
func (exports *usageLogsExportsModule) problems() ProblemWriter {
	if exports.module.deps.Problems != nil {
		return exports.module.deps.Problems
	}
	return NewProblems(exports.module.deps.Logger)
}

// registerUsageLogsExports 注册三条导出路由（装配侧没给键值面时只记日志、不注册）。
func registerUsageLogsExports(router *Router, module *usageLogsModule, kv UsageLogsExportKV) {
	logger := module.deps.Logger
	if kv == nil {
		if logger != nil {
			logger.Info("admin_usage_logs_exports_unwired", map[string]any{
				"module": "usage-logs",
				"reason": "export_kv_missing",
				"routes": []string{
					"POST /usage-logs/exports",
					"GET /usage-logs/exports/{jobId}",
					"GET /usage-logs/exports/{jobId}/download",
				},
				"owner": "node",
			})
		}
		return
	}

	module.exports = newUsageLogsExportRuntime(kv, module.runUsageLogsExportJob)
	// 孤儿结果键的清扫循环：进程生命周期，随进程退出结束（见 sweepUsageLogsExportOrphans）。
	go module.runUsageLogsExportSweeper(context.Background())

	exports := &usageLogsExportsModule{module: module}
	router.Add(Route{
		Method: http.MethodPost, Path: "/usage-logs/exports", Access: AccessRead,
		Module: "usage-logs", OperationID: "createUsageLogsExport",
		Handler: http.HandlerFunc(exports.handleCreate),
	})
	router.Add(Route{
		Method: http.MethodGet, Path: "/usage-logs/exports/{jobId}", Access: AccessRead,
		Module: "usage-logs", OperationID: "getUsageLogsExportStatus",
		Handler: http.HandlerFunc(exports.handleStatus),
	})
	router.Add(Route{
		Method: http.MethodGet, Path: "/usage-logs/exports/{jobId}/download", Access: AccessRead,
		Module: "usage-logs", OperationID: "downloadUsageLogsExport",
		Handler: http.HandlerFunc(exports.handleDownload),
	})
}

// handleCreate 复刻 createUsageLogsExport。
func (exports *usageLogsExportsModule) handleCreate(writer http.ResponseWriter, request *http.Request) {
	input, issues := parseUsageLogsExportCreateBody(request)
	if len(issues) > 0 {
		writeUsageLogsValidationProblem(writer, request, issues)
		return
	}

	preferAsync := strings.Contains(strings.ToLower(request.Header.Get("Prefer")), "respond-async")
	// XLSX 要把所有匹配行读进内存组装，故只走异步作业；同步分支恒为 CSV（Node handlers.ts:91-99）。
	if !preferAsync && input.Format == "xlsx" {
		exports.problems().WriteProblem(writer, request, http.StatusBadRequest,
			"usage_logs.xlsx_requires_async",
			"xlsx export requires asynchronous processing (set 'Prefer: respond-async').")
		return
	}

	principal, _ := PrincipalFrom(request.Context())
	filters := input.Query.filters()
	if !principal.IsAdmin {
		// Node 的 resolveUsageLogFiltersForSession：非 admin 只看得到自己的日志。
		userID := principal.UserID
		filters.UserID = &userID
	}

	if !preferAsync {
		exports.handleSyncExport(writer, request, filters)
		return
	}
	exports.handleStartExport(writer, request, principal, filters, input.Format)
}

func (exports *usageLogsExportsModule) handleSyncExport(
	writer http.ResponseWriter,
	request *http.Request,
	filters store.UsageLogFilters,
) {
	location, timezone, err := usageLogsExportSystemLocation(request.Context(), exports.module.deps.Store)
	if err != nil {
		exports.module.writeActionError(writer, request, err)
		return
	}
	content, err := exports.module.buildUsageLogsExport(request.Context(), usageLogsExportBuild{
		filters:  filters,
		format:   "csv",
		location: location,
		timezone: timezone,
	})
	if err != nil {
		exports.module.writeActionError(writer, request, err)
		return
	}
	exports.writeJSON(writer, request, http.StatusOK, jsonObject{}.set("csv", content))
}

func (exports *usageLogsExportsModule) handleStartExport(
	writer http.ResponseWriter,
	request *http.Request,
	principal Principal,
	filters store.UsageLogFilters,
	format string,
) {
	jobID, err := newUsageLogsExportJobID()
	if err != nil {
		exports.module.writeActionError(writer, request, err)
		return
	}
	err = exports.module.startUsageLogsExport(request.Context(), principal.UserID, filters, format, jobID)
	switch {
	case err == nil:
	case errors.Is(err, errUsageLogsExportQueueFull):
		// 队列满是对 Node 语义的一处有意偏离（Node 无界并发）：如实报错，不静默丢弃作业。
		exports.problems().WriteProblem(writer, request, http.StatusServiceUnavailable,
			"usage_logs.export_queue_full",
			"Export queue is full; retry shortly.")
		return
	default:
		exports.module.writeActionError(writer, request, err)
		return
	}

	statusURL := AdminUsageLogsExportsPath + "/" + jobID
	writer.Header().Set("Location", statusURL)
	exports.writeJSON(writer, request, http.StatusAccepted, jsonObject{}.
		set("jobId", jobID).
		set("status", string(exportStatusQueued)).
		set("statusUrl", statusURL))
}

// AdminUsageLogsExportsPath 是导出作业的状态地址前缀（Node handlers.ts:110 硬编码同一形状）。
const AdminUsageLogsExportsPath = MountPrefix + "/usage-logs/exports"

// handleStatus 复刻 getUsageLogsExportStatus。
func (exports *usageLogsExportsModule) handleStatus(writer http.ResponseWriter, request *http.Request) {
	job, ok := exports.ownedJob(writer, request)
	if !ok {
		return
	}
	exports.writeJSON(writer, request, http.StatusOK, exportStatusObject(exportStatusOf(*job)))
}

// handleDownload 复刻 downloadUsageLogsExport。
func (exports *usageLogsExportsModule) handleDownload(writer http.ResponseWriter, request *http.Request) {
	job, ok := exports.ownedJob(writer, request)
	if !ok {
		return
	}
	switch {
	case job.Status == exportStatusFailed:
		message := "Export failed"
		if job.Error != nil {
			message = *job.Error
		}
		exports.module.writeActionError(writer, request, errors.New(message))
		return
	case job.Status != exportStatusCompleted:
		exports.module.writeActionError(writer, request, errors.New("Export not yet completed"))
		return
	}

	payload, found, err := exports.module.exportKV().Get(
		request.Context(), usageLogsExportResultKey(job.JobID))
	if err != nil {
		exports.module.writeActionError(writer, request, err)
		return
	}
	if !found {
		exports.module.writeActionError(writer, request,
			errors.New("Export file not found or expired"))
		return
	}

	extension := exportFileExtension(job.Format)
	filename := "usage-logs-" + job.JobID + "." + extension
	writer.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if job.Format == "xlsx" {
		decoded, decodeErr := base64.StdEncoding.DecodeString(string(payload))
		if decodeErr != nil {
			exports.module.writeActionError(writer, request, decodeErr)
			return
		}
		writer.Header().Set("Content-Type",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(decoded)
		return
	}
	writer.Header().Set("Content-Type", "text/csv; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(payload)
}

// ownedJob 取作业并做属主校验：查不到、过期、或属主不符，一律「作业不存在或已过期」。
//
// 文案必须与 Node 一字不差：调用方（UI）据此判 404，而 action 错误映射按子串定状态码。
func (exports *usageLogsExportsModule) ownedJob(
	writer http.ResponseWriter,
	request *http.Request,
) (*usageLogsExportJobRecord, bool) {
	jobID, ok := usageLogsExportJobIDFromRequest(request)
	if !ok {
		writeUsageLogsValidationProblem(writer, request, []usageLogsValidationIssue{{
			Path: []any{"jobId"}, Code: "too_small",
			Message: "String must contain at least 1 character(s)",
		}})
		return nil, false
	}
	principal, _ := PrincipalFrom(request.Context())
	job, err := exports.module.readExportJob(request.Context(), jobID)
	if err != nil {
		exports.module.writeActionError(writer, request, err)
		return nil, false
	}
	if job == nil || job.OwnerUserID != principal.UserID {
		exports.module.writeActionError(writer, request,
			errors.New("Export job not found or expired"))
		return nil, false
	}
	return job, true
}

func exportStatusObject(status usageLogsExportStatus) jsonObject {
	body := jsonObject{}.
		set("jobId", status.JobID).
		set("status", string(status.Status)).
		set("processedRows", status.ProcessedRows).
		set("totalRows", status.TotalRows).
		set("progressPercent", status.ProgressPercent).
		set("format", status.Format)
	if status.Error != nil {
		body = body.set("error", *status.Error)
	}
	return body
}

// writeJSON 按 Node 的 jsonResponse 作答（application/json，不转义 HTML 字符、键序即插入序）。
func (exports *usageLogsExportsModule) writeJSON(
	writer http.ResponseWriter,
	request *http.Request,
	status int,
	body jsonObject,
) {
	payload, err := body.marshalJSON()
	if err != nil {
		exports.problems().WriteProblem(writer, request, http.StatusInternalServerError,
			"request.internal_error", "")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

// usageLogsExportJobIDFromRequest 取路径参数 jobId；空串视为校验失败（zod 的 min(1)）。
func usageLogsExportJobIDFromRequest(request *http.Request) (string, bool) {
	params := ParamsFrom(request.Context())
	raw, ok := params["jobId"]
	if !ok {
		return "", false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	return raw, true
}

// --- 请求体解析 --------------------------------------------------------------

// usageLogsExportInput 是创建导出的入参：筛选条件复用查询侧的 usageLogsQuery（于是 filters()
// 的映射也复用，两条路径的筛选语义不可能分叉），外加 format。
type usageLogsExportInput struct {
	Query  usageLogsQuery
	Format string
}

// usageLogsExportCreateFields 是 UsageLogsExportCreateSchema 的字段全集（.strict() 的允许集）。
var usageLogsExportCreateFields = []string{
	"sessionId", "userId", "keyId", "providerId", "model", "actualResponseModelMismatch",
	"statusCode", "excludeStatusCode200", "endpoint", "minRetryCount", "replayFilter",
	"startTime", "endTime", "format",
}

// parseUsageLogsExportCreateBody 复刻 parseHonoJsonBody + UsageLogsExportCreateSchema。
//
// 与查询侧的三处差别（因为这里是 JSON 体而不是查询串）：布尔确实可能是 JSON 布尔；
// 数值确实可能是 JSON 数值；未知字段在 .strict() 下是错误。
func parseUsageLogsExportCreateBody(
	request *http.Request,
) (usageLogsExportInput, []usageLogsValidationIssue) {
	input := usageLogsExportInput{Format: "csv"}
	raw, err := keysReadBody(request)
	if err != nil {
		return input, []usageLogsValidationIssue{{
			Path: []any{}, Code: "invalid_type", Message: "Expected object, received invalid",
		}}
	}
	issues := make([]usageLogsValidationIssue, 0, 4)
	// keysReadBody/keysRejectUnknown 复用 keys 资源的同形实现：两者都是「字段 → 原始 JSON」
	// 映射 + zod 的 unrecognized_keys 语义，重复一份只会让两处慢慢分叉；只是键集不同。
	for _, rejected := range keysRejectUnknown(raw, usageLogsExportCreateFields...) {
		issues = append(issues, usageLogsValidationIssue{
			Path: rejected.Path, Code: rejected.Code, Message: rejected.Message,
		})
	}

	if value, ok := raw["sessionId"]; ok {
		text, valid := exportJSONString(value)
		if !valid {
			issues = append(issues, exportInvalidType("sessionId", "Expected string, received number"))
		} else {
			input.Query.SessionID = text
		}
	}
	for _, field := range []string{"model", "endpoint"} {
		value, ok := raw[field]
		if !ok {
			continue
		}
		text, valid := exportJSONString(value)
		if !valid {
			issues = append(issues, exportInvalidType(field, "Expected string, received number"))
			continue
		}
		if field == "model" {
			input.Query.Model = text
		} else {
			input.Query.Endpoint = text
		}
	}

	if number, present, issue := exportJSONNumber(raw, "userId"); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		value := int64(number)
		input.Query.UserID = &value
	}
	if number, present, issue := exportJSONNumber(raw, "keyId"); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		value := int64(number)
		input.Query.KeyID = &value
	}
	if number, present, issue := exportJSONNumber(raw, "providerId"); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		value := int64(number)
		input.Query.ProviderID = &value
	}
	if number, present, issue := exportJSONNumber(raw, "startTime"); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		value := int64(number)
		input.Query.StartTime = &value
	}
	if number, present, issue := exportJSONNumber(raw, "endTime"); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		value := int64(number)
		input.Query.EndTime = &value
	}
	if number, present, issue := exportJSONIntField(raw, "statusCode", 0, 0); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		value := number
		input.Query.StatusCode = &value
	}
	if number, present, issue := exportJSONIntField(raw, "minRetryCount", 0, 0); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		if number < 0 {
			issues = append(issues, exportTooSmall("minRetryCount", 0))
		} else {
			input.Query.MinRetry = number
		}
	}

	if value, present, issue := exportJSONBool(raw, "actualResponseModelMismatch"); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		input.Query.Mismatch = *value
	}
	if value, present, issue := exportJSONBool(raw, "excludeStatusCode200"); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		input.Query.StatusExcl = value
	}

	if value, ok := raw["replayFilter"]; ok {
		text, valid := exportJSONString(value)
		if !valid {
			issues = append(issues, exportInvalidType("replayFilter", "Expected string, received number"))
		} else {
			switch text {
			case "all", "replay", "non-replay":
				input.Query.Replay = store.UsageLogReplayFilter(text)
			default:
				issues = append(issues, usageLogsValidationIssue{
					Path: []any{"replayFilter"}, Code: "invalid_enum_value",
					Message: "Invalid enum value. Expected 'all' | 'replay' | 'non-replay', received '" + text + "'",
				})
			}
		}
	}

	if value, ok := raw["format"]; ok {
		text, valid := exportJSONString(value)
		switch {
		case !valid:
			issues = append(issues, exportInvalidType("format", "Expected string, received number"))
		case text == "csv" || text == "xlsx":
			input.Format = text
		default:
			issues = append(issues, usageLogsValidationIssue{
				Path: []any{"format"}, Code: "invalid_enum_value",
				Message: "Invalid enum value. Expected 'csv' | 'xlsx', received '" + text + "'",
			})
		}
	}

	return input, issues
}

func exportInvalidType(field, message string) usageLogsValidationIssue {
	return usageLogsValidationIssue{Path: []any{field}, Code: "invalid_type", Message: message}
}

func exportTooSmall(field string, min int) usageLogsValidationIssue {
	return usageLogsValidationIssue{
		Path: []any{field}, Code: "too_small",
		Message: "Number must be greater than or equal to " + strconv.Itoa(min),
	}
}

// exportJSONString 要求该字段是 JSON 字符串。
func exportJSONString(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, `"`) {
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", false
	}
	return text, true
}

// exportJSONNumber 复刻 `z.coerce.number()`（无整型约束的字段：userId/keyId/providerId/
// startTime/endTime）。
//
// 复用查询侧的 coerceOptionalNumber，只把 JSON 原始值翻译成「JS Number() 会拿到的文本」：
// 数值原样、字符串去引号、true/false 为 1/0、null 为空串（Number(null)===0）、
// 对象为 NaN（zod 报 invalid_type: Expected number, received nan）。
func exportJSONNumber(
	raw map[string]json.RawMessage,
	field string,
) (float64, bool, *usageLogsValidationIssue) {
	text, present, issue := exportJSONNumberText(raw, field)
	if issue != nil || !present {
		return 0, false, issue
	}
	return coerceOptionalNumber(map[string][]string{field: {text}}, field)
}

// exportJSONIntField 复刻 `z.coerce.number().int()`（可带上下界）：statusCode 与 minRetryCount。
//
// 与 exportJSONNumber 分开的理由：`.int()` 不是「有边界才算整数」——statusCode 没有边界，
// 却仍拒绝小数（200.5 在 zod 里是 invalid_type: Expected integer, received float）。
func exportJSONIntField(
	raw map[string]json.RawMessage,
	field string,
	min, max int,
) (int, bool, *usageLogsValidationIssue) {
	text, present, issue := exportJSONNumberText(raw, field)
	if issue != nil || !present {
		return 0, false, issue
	}
	return coerceOptionalInt(map[string][]string{field: {text}}, field, min, max)
}

// exportJSONNumberText 取字段的「JS Number() 输入文本」；present=false 表示字段缺席。
func exportJSONNumberText(
	raw map[string]json.RawMessage,
	field string,
) (string, bool, *usageLogsValidationIssue) {
	value, ok := raw[field]
	if !ok {
		return "", false, nil
	}
	text, valid := exportNumberText(value)
	if !valid {
		issue := exportInvalidType(field, "Expected number, received nan")
		return "", false, &issue
	}
	return text, true, nil
}

// exportNumberText 把 JSON 原始值翻成 JS Number() 的输入文本。
func exportNumberText(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "" || trimmed == "null":
		// Number(null) === 0；空串同样为 0（与 parseCoercedNumber 的既有语义一致）。
		return "", true
	case trimmed == "true":
		return "1", true
	case trimmed == "false":
		return "0", true
	case strings.HasPrefix(trimmed, `"`):
		text, ok := exportJSONString(raw)
		if !ok {
			return "", false
		}
		return text, true
	case strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "["):
		// Number({}) / Number([]) 分别为 NaN 与 0：数组会被 zod 判为 NaN？不，Number([])===0。
		if strings.HasPrefix(trimmed, "[") {
			return "", true
		}
		return "", false
	default:
		return trimmed, true
	}
}

// exportJSONBool 复刻 `z.union([literal("true"), literal("false"), boolean]).optional()`。
func exportJSONBool(
	raw map[string]json.RawMessage,
	field string,
) (*bool, bool, *usageLogsValidationIssue) {
	value, ok := raw[field]
	if !ok {
		return nil, false, nil
	}
	trimmed := strings.TrimSpace(string(value))
	switch trimmed {
	case "true", `"true"`:
		return coerceOptionalBool(map[string][]string{field: {"true"}}, field)
	case "false", `"false"`:
		return coerceOptionalBool(map[string][]string{field: {"false"}}, field)
	default:
		issue := usageLogsValidationIssue{
			Path: []any{field}, Code: "invalid_union", Message: "Invalid input",
		}
		return nil, false, &issue
	}
}
