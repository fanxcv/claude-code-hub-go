package adminapi

import (
	"encoding/json"
	"net/http"
)

// 本文件实现 GET /api/v1/me/usage-logs/full（Node：router.ts:144 → handlers.ts listMeUsageLogsFull
// → actions/my-usage.ts getMyUsageLogsBatchFull → repository/usage-logs.ts findReadonlyUsageLogsBatchForKey）。
//
// 与 /me/usage-logs 的差别有两处：
//  1. 行是**完整行**（管理面 /usage-logs 的批次形状），不是 slim 行；
//  2. 行要按 Node 的 scrubUsageLogsBatchForReadonly 做**只读脱敏**——本人可见自己的用量，
//     但看不到身份名、供应商名、错误正文、成本明细与供应商链路里的请求体和上游原文。
//
// 一处**有意的降级**：响应缺
// `sourceSessionIdsByIdentity`。Node 侧它由 loadUsageLogSourceSessionIdsByIdentity 填充
// （按 keyString 范围查 message_request 与 usage_ledger 的 source_session_id），
// Go 侧的同名装载器只有管理面版本（按 UsageLogFilters 范围），for-key 版本尚未移植。
// 该字段缺失等价于 Node 的 includeSourceSessionIds=false 形态：UI 的用量表照常渲染，
// 只少一层「源会话」跳转。补它需要两条按 key 范围的查询 + 去重排序，属独立小项。

// handleMeUsageLogsFull 复刻 listMeUsageLogsFull。
func (api *meAPI) handleMeUsageLogsFull(writer http.ResponseWriter, request *http.Request) {
	query, issues := parseMeUsageLogsQuery(request)
	if len(issues) > 0 {
		writeUsageLogsValidationProblem(writer, request, issues)
		return
	}

	ctx := request.Context()
	_, keyString, ok, err := api.meUsageSubject(ctx)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}

	filters := query.filters(keyString)
	// startTime/endTime 同时给了就用它们（与 /me/usage-logs 同判）。
	if query.StartTime == nil && query.EndTime == nil {
		location, locErr := meSystemLocation(ctx, api.pools)
		if locErr != nil {
			api.writeMeFailure(writer, request, locErr)
			return
		}
		start, end := meUsageDateRange(query.StartDate, query.EndDate, location)
		filters.StartTime = start
		filters.EndTime = end
	}

	logs := []jsonObject{}
	hasMore := false
	var nextCursor any
	if ok {
		batch, readErr := api.pools.FindReadonlyUsageLogsBatchForKey(ctx, filters, query.Cursor, query.Limit)
		if readErr != nil {
			api.writeMeFailure(writer, request, readErr)
			return
		}
		logs = make([]jsonObject, 0, len(batch.Rows))
		for _, row := range batch.Rows {
			switch {
			case row.Message != nil:
				logs = append(logs, scrubMeUsageFullRow(usageLogRowFields(*row.Message, false)))
			case row.Ledger != nil:
				logs = append(logs, scrubMeUsageFullRow(ledgerFallbackRowFields(row.Ledger.LedgerUsageLogRow)))
			}
		}
		hasMore = batch.HasMore
		nextCursor = meUsageCursorToken(batch.NextCursor)
	}

	writeOrderedJSONResponse(writer, http.StatusOK, jsonObject{}.
		set("logs", logs).
		set("nextCursor", nextCursor).
		set("hasMore", hasMore))
}

// scrubMeUsageFullRow 复刻 scrubUsageLogsBatchForReadonly（actions/my-usage.ts:90）。
func scrubMeUsageFullRow(row jsonObject) jsonObject {
	scrubbed := make(jsonObject, 0, len(row))
	for _, field := range row {
		switch field.Key {
		case "userName", "keyName":
			scrubbed = scrubbed.set(field.Key, "")
		case "providerName", "errorMessage", "blockedReason", "userAgent", "messagesCount",
			"costMultiplier", "groupCostMultiplier", "costBreakdown", "_liveChain":
			scrubbed = scrubbed.set(field.Key, nil)
		case "providerChain":
			scrubbed = scrubbed.set(field.Key, scrubMeUsageProviderChain(field.Value))
		case "specialSettings":
			scrubbed = scrubbed.set(field.Key, scrubMeUsageSpecialSettings(field.Value))
		default:
			scrubbed = scrubbed.set(field.Key, field.Value)
		}
	}
	return scrubbed
}

// scrubMeUsageProviderChain 复刻 scrubProviderChainRequestForReadonly（actions/my-usage.ts:42）。
//
// 规则：错误详情里去掉 `request`（它带请求体）；`rawCrossProviderFallbackEnabled === true` 时
// 再去掉 `clientError` 与 provider 的 `upstreamBody`/`upstreamParsed`；无 errorDetails 的项原样保留。
func scrubMeUsageProviderChain(value any) any {
	raw, ok := value.(json.RawMessage)
	if !ok || len(raw) == 0 {
		return value
	}
	var chain []map[string]any
	if err := json.Unmarshal(raw, &chain); err != nil {
		// 解不开就保持原样：宁可多一层可见性也不要把整列吞掉（与 Node 的「非对象即跳过」同向）。
		return value
	}
	for _, item := range chain {
		details, ok := item["errorDetails"].(map[string]any)
		if !ok {
			continue
		}
		delete(details, "request")

		strong, _ := item["rawCrossProviderFallbackEnabled"].(bool)
		provider, hasProvider := details["provider"].(map[string]any)
		if strong {
			delete(details, "clientError")
			if hasProvider {
				delete(provider, "upstreamBody")
				delete(provider, "upstreamParsed")
			}
		}
		if !hasProvider {
			// Node 在 provider 缺席时把它设为 undefined（键消失）。
			delete(details, "provider")
		}
	}
	encoded, err := json.Marshal(chain)
	if err != nil {
		return value
	}
	return json.RawMessage(encoded)
}

// scrubMeUsageSpecialSettings 复刻 scrubSpecialSettingsForReadonly（actions/my-usage.ts:78）：
// 仅 guard_intercept 类的 reason 置 null，其余原样。
func scrubMeUsageSpecialSettings(value any) any {
	settings, ok := value.([]jsonObject)
	if !ok {
		return value
	}
	scrubbed := make([]jsonObject, 0, len(settings))
	for _, setting := range settings {
		isGuardIntercept := false
		for _, field := range setting {
			if field.Key == "type" {
				if text, isText := field.Value.(string); isText && text == "guard_intercept" {
					isGuardIntercept = true
				}
			}
		}
		if !isGuardIntercept {
			scrubbed = append(scrubbed, setting)
			continue
		}
		replaced := make(jsonObject, 0, len(setting))
		for _, field := range setting {
			if field.Key == "reason" {
				replaced = replaced.set(field.Key, nil)
				continue
			}
			replaced = replaced.set(field.Key, field.Value)
		}
		scrubbed = append(scrubbed, replaced)
	}
	return scrubbed
}
