package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 sensitive-words 资源模块（批次 A / lane A1-5，6 条端点）。
//
// 唯一真源：
//   - 路由与状态码：src/app/api/v1/resources/sensitive-words/handlers.ts
//   - 业务规则：src/actions/sensitive-words.ts
//   - SQL：src/repository/sensitive-words.ts（Go 侧在 internal/store/admin_sensitive_words.go）
//
// 六条端点：
//
//	GET    /sensitive-words                 列表（含禁用行，创建时间倒序）
//	POST   /sensitive-words                 创建（201 + Location）
//	POST   /sensitive-words/cache:refresh   重载并广播失效，返回 {stats}
//	GET    /sensitive-words/cache/stats     缓存统计
//	PATCH  /sensitive-words/{id}            部分更新
//	DELETE /sensitive-words/{id}            删除（204）
//
// 三条登记进对拍白名单的差异（都写在对应函数注释里）：
//  1. 正则校验用 Go 的 RE2（lookahead/backreference 之类会被拒），Node 用 JS RegExp。
//  2. cache:stats 的 lastReloadTime 是「本进程最近一次 refresh 的时刻」，未 refresh 过为 0；
//     Node 报的是其进程内检测器的最近装载时刻。
//  3. 状态码按显式错误码映射（sensitive_word.not_found / action_failed），不移植 Node 的
// `detail.includes("不存在")` 子串判定——。

// sensitiveWordResponse 是响应体里的敏感词（逐字对应 Node 的 SensitiveWordSchema）。
type sensitiveWordResponse struct {
	ID          int64   `json:"id"`
	Word        string  `json:"word"`
	MatchType   string  `json:"matchType"`
	Description *string `json:"description"`
	IsEnabled   bool    `json:"isEnabled"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
}

// sensitiveWordListResponse 对应 `jsonResponse({ items: result.data })`。
type sensitiveWordListResponse struct {
	Items []sensitiveWordResponse `json:"items"`
}

// sensitiveWordStats 逐字对应 detector.getStats()（src/lib/sensitive-word-detector.ts:239-248）。
type sensitiveWordStats struct {
	ContainsCount  int64 `json:"containsCount"`
	ExactCount     int64 `json:"exactCount"`
	RegexCount     int64 `json:"regexCount"`
	TotalCount     int64 `json:"totalCount"`
	LastReloadTime int64 `json:"lastReloadTime"`
	IsLoading      bool  `json:"isLoading"`
}

// sensitiveWordCacheRefreshResponse 对应 refreshCacheAction 的 `{ stats }`。
type sensitiveWordCacheRefreshResponse struct {
	Stats sensitiveWordStats `json:"stats"`
}

// sensitiveWordsReloadedAt 是本进程最近一次 cache:refresh 的毫秒时间戳。
//
// 为什么是进程内变量而不是从检测器读：Go 侧的检测器是 guard 的 SensitiveCache（快照 + 失效
// 通道，不暴露装载时刻）。refresh 的动作我们自己是发起者，故自己记下这一时刻；Node 侧
// lastReloadTime 初值也是 0，语义一致（白名单差异仅在于「非本进程触发的装载」）。
var sensitiveWordsReloadedAt atomic.Int64

// sensitiveWordCreateSchema 对应 SensitiveWordCreateSchema（strict）。
var sensitiveWordCreateFields = []string{"word", "matchType", "description"}

// sensitiveWordUpdateFields 对应 SensitiveWordCreateSchema.extend({ isEnabled }).partial().strict()。
var sensitiveWordUpdateFields = []string{"word", "matchType", "description", "isEnabled"}

// RegisterSensitiveWords 注册本模块的六条路由。
//
// 装配方式遵循 deps.go 的约定：只调用 router.Add，不改本包共享文件。依赖不齐（无 Store）时
// 记一条 warn 并**不注册**——未注册的路由会原样回退 Node，那里有完整的实现。
func RegisterSensitiveWords(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_sensitive_words_store_unwired", map[string]any{
				"module": "sensitive-words",
				"action": "routes_not_registered",
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/sensitive-words",
		Access:      AccessAdmin,
		Module:      "sensitive-words",
		OperationID: "listSensitiveWords",
		Handler:     http.HandlerFunc(handleListSensitiveWords(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/sensitive-words",
		Access:      AccessAdmin,
		Module:      "sensitive-words",
		OperationID: "createSensitiveWord",
		Handler:     http.HandlerFunc(handleCreateSensitiveWord(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/sensitive-words/cache:refresh",
		Access:      AccessAdmin,
		Module:      "sensitive-words",
		OperationID: "refreshSensitiveWordsCache",
		Handler:     http.HandlerFunc(handleRefreshSensitiveWordsCache(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/sensitive-words/cache/stats",
		Access:      AccessAdmin,
		Module:      "sensitive-words",
		OperationID: "getSensitiveWordsCacheStats",
		Handler:     http.HandlerFunc(handleSensitiveWordsCacheStats(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPatch,
		Path:        "/sensitive-words/{id}",
		Access:      AccessAdmin,
		Module:      "sensitive-words",
		OperationID: "updateSensitiveWord",
		Handler:     http.HandlerFunc(handleUpdateSensitiveWord(deps)),
	})
	router.Add(Route{
		Method:      http.MethodDelete,
		Path:        "/sensitive-words/{id}",
		Access:      AccessAdmin,
		Module:      "sensitive-words",
		OperationID: "deleteSensitiveWord",
		Handler:     http.HandlerFunc(handleDeleteSensitiveWord(deps)),
	})
}

// handleListSensitiveWords 复刻 listSensitiveWords（handlers.ts:21-26）。
//
// Node 侧 action 在非 admin 时返回空数组而不是错误；路由的 access 已是 admin 档位，守卫
// 保证到这里的一定是管理员，故不存在那条分支。
func handleListSensitiveWords(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		words, err := deps.Store.AdminListSensitiveWords(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("sensitive_word", err))
			return
		}
		items := make([]sensitiveWordResponse, 0, len(words))
		for _, word := range words {
			items = append(items, sensitiveWordPayload(word))
		}
		adminWriteJSON(writer, http.StatusOK, sensitiveWordListResponse{Items: items})
	}
}

// handleCreateSensitiveWord 复刻 createSensitiveWord（handlers.ts:28-40）。
func handleCreateSensitiveWord(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, sensitiveWordCreateFields...)
		object.RejectUnknownKeys()
		word, _ := object.String("word", adminStringSpec{
			Required: true, Trim: true, MinRunes: 1, MaxRunes: 500,
		})
		matchType, _ := object.String("matchType", adminStringSpec{
			Required: true, Enum: sensitiveWordMatchTypes,
		})
		description, hasDescription := object.String("description", adminStringSpec{Trim: true, MaxRunes: 500})
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		// 复刻 createSensitiveWordAction 的规则：regex 类型必须先能编译。
		if matchType == "regex" {
			if !sensitiveWordRegexValid(word) {
				adminProblemWriter(deps).WriteActionError(writer, request,
					NewActionError("sensitive_word", "sensitive_word.action_failed",
						http.StatusBadRequest, fmt.Errorf("敏感词正则非法")))
				return
			}
		}

		var descriptionPointer *string
		if hasDescription {
			descriptionPointer = &description
		}
		created, err := deps.Store.AdminCreateSensitiveWord(request.Context(), word, matchType, descriptionPointer)
		if err != nil {
			adminEmitAudit(deps, request, sensitiveWordAudit(deps.Store, request,
				"sensitive_word.create", "", word, store.AdminSensitiveWord{}).failure("CREATE_FAILED"))
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("sensitive_word", err))
			return
		}

		adminPublishDomain(deps, request, cfgsync.DomainSensitiveWords)
		event := sensitiveWordAudit(deps.Store, request, "sensitive_word.create",
			strconv.FormatInt(created.ID, 10), created.Word, created)
		adminEmitAudit(deps, request, event.success())

		adminWriteCreated(writer,
			fmt.Sprintf("%s/sensitive-words/%d", MountPrefix, created.ID),
			sensitiveWordPayload(created))
	}
}

// handleUpdateSensitiveWord 复刻 updateSensitiveWord（handlers.ts:42-56）。
func handleUpdateSensitiveWord(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := sensitiveWordIDParam(writer, request)
		if !ok {
			return
		}
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, sensitiveWordUpdateFields...)
		object.RejectUnknownKeys()
		word, hasWord := object.String("word", adminStringSpec{Trim: true, MinRunes: 1, MaxRunes: 500})
		matchType, hasMatchType := object.String("matchType", adminStringSpec{Enum: sensitiveWordMatchTypes})
		description, hasDescription := object.String("description", adminStringSpec{Trim: true, MaxRunes: 500})
		isEnabled, _ := object.Bool("isEnabled")
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		// 与 Node 逐字一致：只在**同一次请求里同时**给了 word 与 matchType=regex 时才校验正则。
		// 单给 word（例如给已有 regex 行换模式串）在 Node 侧不校验，这里也不校验——这不是疏漏，
		// 是刻意对齐的既有行为。
		if hasWord && hasMatchType && matchType == "regex" && !sensitiveWordRegexValid(word) {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("sensitive_word", "sensitive_word.action_failed",
					http.StatusBadRequest, fmt.Errorf("敏感词正则非法")))
			return
		}

		update := store.AdminSensitiveWordUpdate{}
		if hasWord {
			update.Word = &word
		}
		if hasMatchType {
			update.MatchType = &matchType
		}
		if hasDescription {
			update.Description = &description
		}
		update.IsEnabled = isEnabled

		updated, err := deps.Store.AdminUpdateSensitiveWord(request.Context(), id, update)
		if err == store.ErrNotFound {
			// Node 的 not-found 分支直接 return（actions/sensitive-words.ts:143-148），**不写审计**：
			// 失败审计只属于 catch 分支（真异常）。这里保持一致，否则审计行比 Node 多。
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("sensitive_word", "sensitive_word.not_found", http.StatusNotFound,
					fmt.Errorf("敏感词不存在")))
			return
		}
		if err != nil {
			adminEmitAudit(deps, request, sensitiveWordAudit(deps.Store, request, "sensitive_word.update",
				strconv.FormatInt(id, 10), "", store.AdminSensitiveWord{}).failure("UPDATE_FAILED"))
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("sensitive_word", err))
			return
		}

		adminPublishDomain(deps, request, cfgsync.DomainSensitiveWords)
		adminEmitAudit(deps, request, sensitiveWordAudit(deps.Store, request, "sensitive_word.update",
			strconv.FormatInt(id, 10), updated.Word, updated).success())

		adminWriteJSON(writer, http.StatusOK, sensitiveWordPayload(updated))
	}
}

// handleDeleteSensitiveWord 复刻 deleteSensitiveWord（handlers.ts:58-71）。
func handleDeleteSensitiveWord(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := sensitiveWordIDParam(writer, request)
		if !ok {
			return
		}
		deleted, err := deps.Store.AdminDeleteSensitiveWord(request.Context(), id)
		if err != nil {
			adminEmitAudit(deps, request, sensitiveWordAudit(deps.Store, request, "sensitive_word.delete",
				strconv.FormatInt(id, 10), "", store.AdminSensitiveWord{}).failure("DELETE_FAILED"))
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("sensitive_word", err))
			return
		}
		if !deleted {
			// 同上：Node 的「不存在」分支不写审计。
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("sensitive_word", "sensitive_word.not_found", http.StatusNotFound,
					fmt.Errorf("敏感词不存在")))
			return
		}

		adminPublishDomain(deps, request, cfgsync.DomainSensitiveWords)
		adminEmitAudit(deps, request, sensitiveWordAudit(deps.Store, request, "sensitive_word.delete",
			strconv.FormatInt(id, 10), "", store.AdminSensitiveWord{}).success())
		adminWriteNoContent(writer)
	}
}

// handleRefreshSensitiveWordsCache 复刻 refreshCacheAction（actions/sensitive-words.ts:268-303）。
//
// Node 的 refresh 做两件事：让检测器重新从库里装载（reload → getActiveSensitiveWords）＋
// 广播失效。Go 侧对应：广播 cfgsync.DomainSensitiveWords 让 guard 的快照失效（下次用到时
// 重新装载），统计则直接按「启用行」现算——这与 reload 之后检测器里的内容同源，且不会把
// 一份陈旧快照的数字报成「刚刷新过的」。
func handleRefreshSensitiveWordsCache(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		stats, err := sensitiveWordCurrentStats(request.Context(), deps)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("sensitive_word", err))
			return
		}
		adminPublishDomain(deps, request, cfgsync.DomainSensitiveWords)
		sensitiveWordsReloadedAt.Store(adminNowMillis())
		stats.LastReloadTime = sensitiveWordsReloadedAt.Load()
		adminWriteJSON(writer, http.StatusOK, sensitiveWordCacheRefreshResponse{Stats: stats})
	}
}

// handleSensitiveWordsCacheStats 复刻 getSensitiveWordsCacheStats（handlers.ts:80-93）。
//
// Node 侧 action 在非 admin 时返回 null，handler 把 null 映射成 403 auth.forbidden。路由的
// access 已是 admin 档位，故这里不可能走到那条分支——守卫不放行就根本到不了本函数。
func handleSensitiveWordsCacheStats(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		stats, err := sensitiveWordCurrentStats(request.Context(), deps)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("sensitive_word", err))
			return
		}
		adminWriteJSON(writer, http.StatusOK, stats)
	}
}

// sensitiveWordPayload 把一行映射成响应体；可空时间列按 Node 的 `?? new Date()` 兜底。
func sensitiveWordPayload(word store.AdminSensitiveWord) sensitiveWordResponse {
	return sensitiveWordResponse{
		ID:          word.ID,
		Word:        word.Word,
		MatchType:   word.MatchType,
		Description: word.Description,
		IsEnabled:   word.IsEnabled,
		CreatedAt:   adminStringOrNow(word.CreatedAt),
		UpdatedAt:   adminStringOrNow(word.UpdatedAt),
	}
}

// sensitiveWordCurrentStats 现算缓存统计。
func sensitiveWordCurrentStats(ctx context.Context, deps Deps) (sensitiveWordStats, error) {
	counts, err := deps.Store.AdminCountActiveSensitiveWords(ctx)
	if err != nil {
		return sensitiveWordStats{}, err
	}
	return sensitiveWordStats{
		ContainsCount:  counts.Contains,
		ExactCount:     counts.Exact,
		RegexCount:     counts.Regex,
		TotalCount:     counts.Total(),
		LastReloadTime: sensitiveWordsReloadedAt.Load(),
		// Go 侧没有异步装载状态：守卫的快照是同步取用的，任何时刻都不处于「装载中」。
		IsLoading: false,
	}, nil
}

// sensitiveWordIDParam 解析并校验路径参数 id（复刻 SensitiveWordIdParamSchema 的 coerce + int + positive）。
func sensitiveWordIDParam(writer http.ResponseWriter, request *http.Request) (int64, bool) {
	raw := ParamsFrom(request.Context())["id"]
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path:    []any{"id"},
			Code:    "invalid_type",
			Message: "Expected number, received string",
		}})
		return 0, false
	}
	return parsed, true
}

// sensitiveWordRegexValid 校验正则。
//
// 差异（登记进白名单）：Go 的 regexp 是 RE2，JS 的 RegExp 支持反向引用与 lookaround。
// `(?<=a)b` 这类模式在 Node 侧合法、在 Go 侧被拒——宁可拒绝（400）也不要放行一个 Go 编译
// 不了的词条到库里（那会让守卫在装载期失败，影响面比一个 400 大得多）。
func sensitiveWordRegexValid(pattern string) bool {
	_, err := regexp.Compile(pattern)
	return err == nil
}

var sensitiveWordMatchTypes = []string{"contains", "exact", "regex"}

// sensitiveWordAudit 组装审计事件；after 快照的形状与 Node 的 emitActionAudit 一致。
func sensitiveWordAudit(
	pools *store.Pools,
	request *http.Request,
	action, targetID, targetName string,
	word store.AdminSensitiveWord,
) sensitiveWordAuditEvent {
	event := sensitiveWordAuditEvent{event: adminAuditEvent(pools, request, action, "sensitive_word", targetID, targetName)}
	if word.ID != 0 {
		event.after = map[string]any{
			"id":          word.ID,
			"word":        word.Word,
			"matchType":   word.MatchType,
			"description": word.Description,
			"isEnabled":   word.IsEnabled,
		}
	}
	return event
}

// sensitiveWordAuditEvent 让「成功/失败」两笔审计的构造保持一行。
type sensitiveWordAuditEvent struct {
	event AuditEvent
	after map[string]any
}

func (e sensitiveWordAuditEvent) success() AuditEvent {
	e.event.Success = true
	e.event.Details = e.after
	return e.event
}

func (e sensitiveWordAuditEvent) failure(errorMessage string) AuditEvent {
	e.event.Success = false
	e.event.ErrorMessage = errorMessage
	return e.event
}
