package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pricing"
)

// 本文件补齐 model-prices 的**三兄弟**端点（批次 A 遗留的「有意不实现」3 条）：
//
//	POST /model-prices:upload             上传价格表（CPT v1 JSON / 内部 JSON；TOML 见降级说明）
//	POST /model-prices:syncLitellmCheck   检查云端同步会与哪些 manual 模型冲突（只读）
//	POST /model-prices:syncLitellm        立即同步云端价格表（覆盖 manual 需显式列出）
//
// 唯一真源：
//   - 路由与响应：src/app/api/v1/resources/model-prices/{router,handlers}.ts
//   - 业务规则：src/actions/model-prices.ts（uploadPriceTable / checkLiteLLMSyncConflicts /
//     syncLiteLLMPrices）
//   - 管线本身：internal/jobs（CPT 转换 + 云同步）与 internal/pricing（上传解析 + 写入分类）
//
// 与后台任务的关系：`internal/jobs` 的 PriceSyncer 负责 30 分钟一轮的自动同步（有 leader 锁与
// 版本短路）；本文件的 `syncLitellm` 端点复刻 Node 的「用户点立即同步」语义——**无锁、无短路**，
// 见 jobs 包的 SyncNow 注释。
//
// 三条有意差异：
// 1. **TOML 降级**：Go 侧未引入 TOML 库，旧版 TOML 价格表上传会失败（Node 会接受）。
//  2. **结果数组顺序**：Go 侧按模型名排序（map 无插入序），Node 用表格文档序；集合一致。
//  3. **冲突列表顺序**：同上，按模型名排序。

// modelPriceSyncRoutes 是本文件的三条路由。
//
// OperationID 与 Node 的 handler 函数名逐字一致（Hono 的 openapi 以函数名为 operationId）。
func modelPriceSyncRoutes(api *modelPriceSyncAPI) []Route {
	return []Route{
		{
			Method:      http.MethodPost,
			Path:        "/model-prices:upload",
			Access:      AccessAdmin,
			Module:      "model-prices",
			OperationID: "uploadModelPrices",
			Handler:     http.HandlerFunc(api.upload),
		},
		{
			Method:      http.MethodPost,
			Path:        "/model-prices:syncLitellmCheck",
			Access:      AccessAdmin,
			Module:      "model-prices",
			OperationID: "checkLiteLlmSync",
			Handler:     http.HandlerFunc(api.check),
		},
		{
			Method:      http.MethodPost,
			Path:        "/model-prices:syncLitellm",
			Access:      AccessAdmin,
			Module:      "model-prices",
			OperationID: "syncLiteLlmPrices",
			Handler:     http.HandlerFunc(api.sync),
		},
	}
}

// RegisterModelPriceSyncRoutes 注册 model-prices 的三条同步/上传端点。
//
// Store 未装配时不注册（整组回退 Node）：这三条都要读写价格表，没有库就无法作答。
func RegisterModelPriceSyncRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		logPriceSyncUnwired(deps, "store_missing")
		return
	}
	api := &modelPriceSyncAPI{deps: deps}
	// Router.Add 在守卫未装配时自行拒注册并记 Error（fail-closed），方法+路径重复时保留首次
	// 注册；两者都不返回错误，故这里无需判返回值。
	for _, route := range modelPriceSyncRoutes(api) {
		router.Add(route)
	}
}

func logPriceSyncUnwired(deps Deps, reason string) {
	if deps.Logger == nil {
		return
	}
	deps.Logger.Warn("model_price_sync_unwired", map[string]any{
		"reason": reason,
		"source": "internal/adminapi/model_prices_sync.go",
	})
}

type modelPriceSyncAPI struct {
	deps Deps
}

// newSyncer 按请求构造同步器：overwriteManual 是**每请求**参数，故不能复用后台任务那个实例。
//
// URL 取自与后台任务同一个环境变量（CCH_CLOUD_PRICE_TABLE_URL），使端点在测试与自建镜像
// 场景下指向同一处；未设置时用 cloud-price-table.ts:7 的官方地址。
func (api *modelPriceSyncAPI) newSyncer(overwriteManual []string) (*jobs.PriceSyncer, error) {
	return jobs.NewPriceSyncer(jobs.PriceSyncOptions{
		Pools:           api.deps.Store,
		Logger:          api.deps.Logger,
		URL:             strings.TrimSpace(os.Getenv("CCH_CLOUD_PRICE_TABLE_URL")),
		OverwriteManual: overwriteManual,
	})
}

// upload 复刻 uploadModelPrices（handlers.ts:67-78）→ uploadPriceTable。
func (api *modelPriceSyncAPI) upload(writer http.ResponseWriter, request *http.Request) {
	fields, ok := adminReadJSONObject(writer, request, api.deps)
	if !ok {
		return
	}
	body := adminNewObject(fields, "content", "overwriteManual")
	body.RejectUnknownKeys()
	content, _ := body.String("content", adminStringSpec{Required: true, MinRunes: 1})
	overwriteManual, _ := body.StringArray("overwriteManual")
	if issues := body.issues0(); len(issues) > 0 {
		adminWriteValidationFailure(writer, request, issues)
		return
	}

	entries, err := pricing.ParseUploadContent(content)
	if err != nil {
		api.auditUpload(request, nil, err)
		adminProblemWriter(api.deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
		return
	}

	// 用户显式上传 → source=manual（权威导入，不被后续云端同步覆盖）。
	result, err := pricing.WriteEntries(request.Context(), api.deps.Store, entries,
		pricing.SourceManual, overwriteManual, api.deps.Logger)
	if err != nil {
		api.auditUpload(request, nil, err)
		adminProblemWriter(api.deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
		return
	}
	api.auditUpload(request, result, nil)
	adminWriteJSON(writer, http.StatusOK, result)
}

// check 复刻 checkLiteLlmSync（handlers.ts:80-87）→ checkLiteLLMSyncConflicts。
func (api *modelPriceSyncAPI) check(writer http.ResponseWriter, request *http.Request) {
	syncer, err := api.newSyncer(nil)
	if err != nil {
		adminProblemWriter(api.deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
		return
	}
	converted, err := syncer.LoadConvertedTable(request.Context())
	if err != nil {
		adminProblemWriter(api.deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
		return
	}
	conflicts, err := pricing.FindConflicts(request.Context(), api.deps.Store, converted.Models)
	if err != nil {
		adminProblemWriter(api.deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
		return
	}
	adminWriteJSON(writer, http.StatusOK, conflicts)
}

// sync 复刻 syncLiteLlmPrices（handlers.ts:89-99）→ syncLiteLLMPrices。
func (api *modelPriceSyncAPI) sync(writer http.ResponseWriter, request *http.Request) {
	fields, ok := adminReadJSONObject(writer, request, api.deps)
	if !ok {
		return
	}
	body := adminNewObject(fields, "overwriteManual")
	body.RejectUnknownKeys()
	overwriteManual, _ := body.StringArray("overwriteManual")
	if issues := body.issues0(); len(issues) > 0 {
		adminWriteValidationFailure(writer, request, issues)
		return
	}

	syncer, err := api.newSyncer(overwriteManual)
	if err != nil {
		api.auditSync(request, overwriteManual, nil, err.Error())
		adminProblemWriter(api.deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
		return
	}
	result, err := syncer.SyncNow(request.Context())
	if err != nil {
		api.auditSync(request, overwriteManual, nil, auditErrorCode(err))
		adminProblemWriter(api.deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
		return
	}
	api.auditSync(request, overwriteManual, result, "")
	adminWriteJSON(writer, http.StatusOK, result)
}

// auditErrorCode 复刻 Node 审计里的错误码：非业务失败统一记 "SYNC_FAILED"
// （actions/model-prices.ts:600 的 catch 分支），业务失败记具体文案。
func auditErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "SYNC_FAILED"
	}
	return err.Error()
}

// auditUpload 对应 uploadPriceTable 的 emitActionAudit（成功记计数、失败记错误文案）。
func (api *modelPriceSyncAPI) auditUpload(request *http.Request, result *jobs.PriceUpdateResult, failure error) {
	event := modelPriceAudit(api.deps.Store, request, "model_price.bulk_upload", "", "", nil)
	if failure != nil {
		adminEmitAudit(api.deps, request, event.failure(failure.Error()))
		return
	}
	event.event.Details = priceUpdateDetails(result)
	adminEmitAudit(api.deps, request, event.success())
}

// auditSync 对应 syncLiteLLMPrices 的 emitActionAudit。
func (api *modelPriceSyncAPI) auditSync(
	request *http.Request,
	overwriteManual []string,
	result *jobs.PriceUpdateResult,
	failureMessage string,
) {
	event := modelPriceAudit(api.deps.Store, request, "model_price.sync_litellm", "", "", nil)
	if failureMessage != "" {
		adminEmitAudit(api.deps, request, event.failure(failureMessage))
		return
	}
	details := priceUpdateDetails(result)
	details["overwriteManualCount"] = len(overwriteManual)
	event.event.Details = details
	adminEmitAudit(api.deps, request, event.success())
}

// priceUpdateDetails 复刻 Node 审计里的 after 计数（两处 action 的字段名逐字对齐）。
func priceUpdateDetails(result *jobs.PriceUpdateResult) map[string]any {
	if result == nil {
		return map[string]any{}
	}
	return map[string]any{
		"added":            len(result.Added),
		"updated":          len(result.Updated),
		"unchanged":        len(result.Unchanged),
		"failed":           len(result.Failed),
		"skippedConflicts": len(result.SkippedConflicts),
		"total":            result.Total,
	}
}

// 编译期确认响应体与 Node 的 ModelPriceUpdateResult 同形（字段名由 jobs 的类型标签决定）。
var _ = func() json.RawMessage {
	encoded, _ := json.Marshal(jobs.PriceUpdateResult{})
	return encoded
}
