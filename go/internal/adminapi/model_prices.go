package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 model-prices 资源模块（批次 A / lane A1-5）。
//
// 唯一真源：
//   - 路由与状态码：src/app/api/v1/resources/model-prices/handlers.ts
//   - 业务规则：src/actions/model-prices.ts
//   - SQL：src/repository/model-price.ts（Go 侧在 internal/store/admin_model_prices.go）
//
// 本波实现 6 条**纯 PG** 端点：
//
//	GET    /model-prices                              分页列表（每模型一行、manual 优先）
//	GET    /model-prices/catalog                     模型目录（默认只给 chat）
//	GET    /model-prices/exists                      是否存在价格记录
//	PUT    /model-prices/{modelName}                 单模型价格写入（source=manual）
//	DELETE /model-prices/{modelName}                 单模型价格删除
//	POST   /model-prices/{modelName}/pricing:pinManual  把云端多供应商价固化成本地 manual
//
// **三条同步/上传端点已实现**（原「有意不实现」已收口，实现在 model_prices_sync.go）：
//
//	POST /model-prices:upload               上传价格表（CPT v1 JSON / 内部 JSON；TOML 见下）
//	POST /model-prices:syncLitellmCheck     检查云端同步会与哪些 manual 模型冲突（只读）
//	POST /model-prices:syncLitellm          立即同步云端价格表（覆盖 manual 需显式列出）
//
// 它们由 `RegisterModelPriceSyncRoutes` 注册（与本源文件的 6 条分开：那边只依赖 PG，
// 这三条还依赖云端拉取与写入分类，缺一即整组不注册、原样回退 Node）。
//
// 一条降级登记：**旧版 TOML 价格表上传在 Go 侧不支持**
// （未引入 TOML 库），HTTP 形状与 Node 的 TOML 解析失败同为 400 model_price.action_failed，
// 但 Node 会接受合法 TOML。补库即可闭合。
//
// 三条登记进对拍白名单的差异：
//  1. catalog 的并列排序用字符串序，不是 JS 的 localeCompare（只在 updatedAt 完全相同时才分叉）。
//  2. PUT/DELETE 的审计 before 快照只查精确模型名，不做 Node 的 aliases 回退查询。
//  3. 价格数据的 JSON 键序：写入前按 Node 的展开顺序构造，但落库后由 jsonb 归一（两端读的是
//     同一份 jsonb，故响应一致）。

// modelPriceItem 是响应体里的价格行（ModelPriceSchema）。
//
// 时间列在存储层已渲染成 Node 的 toISOString 形状，故这里是 string；jsonb 用 RawMessage 原样透传。
type modelPriceItem struct {
	ID        int64           `json:"id"`
	ModelName string          `json:"modelName"`
	PriceData json.RawMessage `json:"priceData"`
	Source    string          `json:"source"`
	CreatedAt string          `json:"createdAt"`
	UpdatedAt string          `json:"updatedAt"`
}

// modelPriceListResponse 对应 handlers.ts:41-47 的 jsonResponse。
type modelPriceListResponse struct {
	Items      []modelPriceItem `json:"items"`
	Page       int              `json:"page"`
	PageSize   int              `json:"pageSize"`
	Total      int64            `json:"total"`
	TotalPages int64            `json:"totalPages"`
}

// modelPriceCatalogItem 逐字对应 ModelPriceCatalogItemSchema。
type modelPriceCatalogItem struct {
	ModelName       string  `json:"modelName"`
	Vendor          *string `json:"vendor"`
	LitellmProvider *string `json:"litellmProvider"`
	UpdatedAt       string  `json:"updatedAt"`
}

// modelPriceCatalogResponse 对应 `jsonResponse({ items: result.data })`。
type modelPriceCatalogResponse struct {
	Items []modelPriceCatalogItem `json:"items"`
}

// modelPriceExistsResponse 对应 ModelPriceExistsResponseSchema。
type modelPriceExistsResponse struct {
	Exists bool `json:"exists"`
}

// RegisterModelPrices 注册本模块只依赖 PG 的 6 条路由。
//
// 依赖云端拉取/写入分类的 3 条（upload / syncLitellmCheck / syncLitellm）在
// `RegisterModelPriceSyncRoutes`（model_prices_sync.go）注册。
func RegisterModelPrices(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_model_prices_store_unwired", map[string]any{
				"module": "model-prices",
				"action": "routes_not_registered",
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/model-prices",
		Access:      AccessAdmin,
		Module:      "model-prices",
		OperationID: "listModelPrices",
		Handler:     http.HandlerFunc(handleListModelPrices(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/model-prices/catalog",
		Access:      AccessAdmin,
		Module:      "model-prices",
		OperationID: "getModelPriceCatalog",
		Handler:     http.HandlerFunc(handleModelPriceCatalog(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/model-prices/exists",
		Access:      AccessAdmin,
		Module:      "model-prices",
		OperationID: "hasModelPrices",
		Handler:     http.HandlerFunc(handleModelPriceExists(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPut,
		Path:        "/model-prices/{modelName}",
		Access:      AccessAdmin,
		Module:      "model-prices",
		OperationID: "upsertModelPrice",
		Handler:     http.HandlerFunc(handleUpsertModelPrice(deps)),
	})
	router.Add(Route{
		Method:      http.MethodDelete,
		Path:        "/model-prices/{modelName}",
		Access:      AccessAdmin,
		Module:      "model-prices",
		OperationID: "deleteModelPrice",
		Handler:     http.HandlerFunc(handleDeleteModelPrice(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/model-prices/{modelName}/pricing:pinManual",
		Access:      AccessAdmin,
		Module:      "model-prices",
		OperationID: "pinModelPriceProvider",
		Handler:     http.HandlerFunc(handlePinModelPriceProvider(deps)),
	})
}

// handleListModelPrices 复刻 listModelPrices（handlers.ts:21-47）与 findAllLatestPricesPaginated。
func handleListModelPrices(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		issues := make([]invalidParam, 0, 2)

		page, ok := modelPriceQueryInt(query.Get("page"), 1, 1, 0)
		if !ok {
			issues = append(issues, modelPriceRangeIssue("page", "too_small", "Number must be greater than or equal to 1"))
		}
		pageSize, ok := modelPriceQueryInt(query.Get("pageSize"), 20, 1, 100)
		if !ok {
			issues = append(issues, modelPriceRangeIssue("pageSize", "too_big",
				"Number must be less than or equal to 100"))
		}
		source := strings.TrimSpace(query.Get("source"))
		if source != "" && source != "cloud" && source != "litellm" && source != "manual" {
			issues = append(issues, invalidParam{
				Path: []any{"source"}, Code: "invalid_enum_value",
				Message: fmt.Sprintf("Invalid enum value. Expected %s, received '%s'",
					adminEnumList([]string{"cloud", "litellm", "manual"}), source),
			})
		}
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		rows, total, err := deps.Store.AdminListModelPricesPaginated(request.Context(),
			store.AdminModelPriceQuery{
				Page:            page,
				PageSize:        pageSize,
				Search:          strings.TrimSpace(query.Get("search")),
				Source:          source,
				Vendor:          strings.TrimSpace(query.Get("vendor")),
				LitellmProvider: strings.TrimSpace(query.Get("litellmProvider")),
			})
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("model_price", err))
			return
		}

		items := make([]modelPriceItem, 0, len(rows))
		for _, row := range rows {
			items = append(items, modelPricePayload(row))
		}
		// totalPages = Math.ceil(total / pageSize)：整数除法向上取整。
		totalPages := int64(0)
		if pageSize > 0 {
			totalPages = (total + int64(pageSize) - 1) / int64(pageSize)
		}
		adminWriteJSON(writer, http.StatusOK, modelPriceListResponse{
			Items:      items,
			Page:       page,
			PageSize:   pageSize,
			Total:      total,
			TotalPages: totalPages,
		})
	}
}

// handleModelPriceCatalog 复刻 getModelPriceCatalog → getAvailableModelCatalog（actions/model-prices.ts:357-395）。
func handleModelPriceCatalog(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		scope := request.URL.Query().Get("scope")
		switch scope {
		case "":
			scope = "chat"
		case "chat", "all":
		default:
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path: []any{"scope"}, Code: "invalid_enum_value",
				Message: fmt.Sprintf("Invalid enum value. Expected %s, received '%s'",
					adminEnumList([]string{"chat", "all"}), scope),
			}})
			return
		}

		rows, err := deps.Store.AdminListLatestModelPrices(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("model_price", err))
			return
		}

		items := make([]modelPriceCatalogItem, 0, len(rows))
		for _, row := range rows {
			if scope != "all" && modelPriceStringField(row.PriceData, "mode") != "chat" {
				continue
			}
			vendor := modelPriceStringField(row.PriceData, "vendor")
			provider := modelPriceStringField(row.PriceData, "litellm_provider")
			item := modelPriceCatalogItem{
				ModelName: row.ModelName,
				UpdatedAt: adminStringOrEmpty(row.UpdatedAt),
			}
			if vendor != "" {
				item.Vendor = &vendor
			}
			if provider != "" {
				item.LitellmProvider = &provider
			}
			items = append(items, item)
		}
		// Node：先按 updatedAt 降序，再按 modelName 升序（localeCompare）。
		// 差异（白名单）：这里用字符串序做第二排序键，与 localeCompare 在大小写/标点上可能不同。
		sort.SliceStable(items, func(left, right int) bool {
			if items[left].UpdatedAt != items[right].UpdatedAt {
				return items[left].UpdatedAt > items[right].UpdatedAt
			}
			return items[left].ModelName < items[right].ModelName
		})
		adminWriteJSON(writer, http.StatusOK, modelPriceCatalogResponse{Items: items})
	}
}

// handleModelPriceExists 复刻 hasModelPrices → hasPriceTable（actions/model-prices.ts:426-441）。
//
// Node 侧在「会话不是 admin」时退化为 hasAnyPriceRecords；本路由的 access 是 admin 档位，
// 故恒走 admin 分支——而那两个分支对管理员是同一件事（findAllLatestPrices 非空 ⟺ 有行）。
func handleModelPriceExists(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		exists, err := deps.Store.AdminHasAnyModelPrice(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("model_price", err))
			return
		}
		adminWriteJSON(writer, http.StatusOK, modelPriceExistsResponse{Exists: exists})
	}
}

// handleUpsertModelPrice 复刻 upsertModelPrice（handlers.ts:106-118）→ upsertSingleModelPrice。
//
// 注意：**模型名取请求体里的 modelName，不取路径参数**。Node 的 handler 把 body 整体交给
// action，路径参数只用于路由匹配（handlers.ts:106-116）。这是既有行为，不"修正"成路径优先——
// 那会让 PUT /model-prices/a 带 body {"modelName":"b"} 在两端口径不同。
func handleUpsertModelPrice(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		input, issues := modelPriceSingleInputOf(fields)
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		priceData, err := modelPriceBuildData(input)
		if err != nil {
			adminEmitAudit(deps, request, modelPriceAudit(deps.Store, request, "model_price.upsert",
				"", input.modelName, nil).failure("UPSERT_FAILED"))
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("model_price", err))
			return
		}

		before, err := deps.Store.AdminFindLatestModelPriceByName(request.Context(), input.modelName)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
			return
		}
		// upsertModelPrice 的默认来源是 manual（actions/model-prices.ts:790）。
		upserted, err := deps.Store.AdminUpsertModelPrice(request.Context(), input.modelName, priceData, "manual")
		if err != nil {
			adminEmitAudit(deps, request, modelPriceAudit(deps.Store, request, "model_price.upsert",
				"", input.modelName, nil).failure("UPSERT_FAILED"))
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("model_price", err))
			return
		}

		event := modelPriceAudit(deps.Store, request, "model_price.upsert",
			fmt.Sprintf("%d", upserted.ID), upserted.ModelName, before)
		event.event.Details = modelPriceAuditPayload(upserted)
		adminEmitAudit(deps, request, event.success())

		adminWriteJSON(writer, http.StatusOK, modelPricePayload(upserted))
	}
}

// handleDeleteModelPrice 复刻 deleteModelPrice（handlers.ts:120-133）→ deleteSingleModelPrice。
func handleDeleteModelPrice(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		modelName := strings.TrimSpace(ParamsFrom(request.Context())["modelName"])
		if modelName == "" {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path: []any{"modelName"}, Code: "too_small", Message: "String must contain at least 1 character(s)",
			}})
			return
		}

		before, err := deps.Store.AdminFindLatestModelPriceByName(request.Context(), modelName)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
			return
		}
		if err := deps.Store.AdminDeleteModelPriceByName(request.Context(), modelName); err != nil {
			adminEmitAudit(deps, request, modelPriceAudit(deps.Store, request, "model_price.delete",
				"", modelName, nil).failure("DELETE_FAILED"))
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
			return
		}

		event := modelPriceAudit(deps.Store, request, "model_price.delete", modelPriceBeforeID(before), modelName, before)
		adminEmitAudit(deps, request, event.success())
		adminWriteNoContent(writer)
	}
}

// handlePinModelPriceProvider 复刻 pinModelPriceProvider（handlers.ts:135-151）→
// pinModelPricingProviderAsManual（actions/model-prices.ts:873-924）。
//
// 两条「未找到」都映射成 404 model_price.not_found：Node 的 handler 按 `detail.includes("未找到")`
// 判定，而这两条文案都含「未找到」（handlers.ts:153-163）。
func handlePinModelPriceProvider(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		modelName := strings.TrimSpace(ParamsFrom(request.Context())["modelName"])
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, "pricingProviderKey")
		object.RejectUnknownKeys()
		pricingProviderKey, _ := object.String("pricingProviderKey", adminStringSpec{
			Required: true, Trim: true, MinRunes: 1,
		})
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}
		if modelName == "" {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path: []any{"modelName"}, Code: "too_small", Message: "String must contain at least 1 character(s)",
			}})
			return
		}

		// Node：先找 cloud，再找 litellm（actions/model-prices.ts:893-895）。
		latest, err := deps.Store.AdminFindLatestModelPriceByNameAndSource(request.Context(), modelName, "cloud")
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
			return
		}
		if latest == nil {
			latest, err = deps.Store.AdminFindLatestModelPriceByNameAndSource(request.Context(), modelName, "litellm")
			if err != nil {
				adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
				return
			}
		}
		if latest == nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("model_price", "model_price.not_found", http.StatusNotFound,
					fmt.Errorf("未找到云端模型价格")))
			return
		}

		manualData, ok := modelPriceManualFromProvider(latest.PriceData, modelName, pricingProviderKey)
		if !ok {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("model_price", "model_price.not_found", http.StatusNotFound,
					fmt.Errorf("未找到对应的多供应商价格节点")))
			return
		}

		upserted, err := deps.Store.AdminUpsertModelPrice(request.Context(), modelName, manualData, "manual")
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("model_price", err))
			return
		}
		// pinManual 在 Node 侧**不写审计**（§8.4 的 16 处写路径不含它）。
		adminWriteJSON(writer, http.StatusOK, modelPricePayload(upserted))
	}
}

// modelPriceSingleInput 是 SingleModelPriceSchema 校验后的输入。
type modelPriceSingleInput struct {
	modelName                   string
	displayName                 string
	mode                        string
	litellmProvider             string
	supportsPromptCaching       *bool
	extraFields                 map[string]any
	inputCostPerToken           *float64
	outputCostPerToken          *float64
	outputCostPerImage          *float64
	inputCostPerRequest         *float64
	cacheReadInputTokenCost     *float64
	cacheCreationInputTokenCost *float64
	cacheCreationAbove1hrCost   *float64
}

// modelPriceSingleFields 是 SingleModelPriceSchema 的全部键（strict 模式据此拒绝未知键）。
var modelPriceSingleFields = []string{
	"modelName", "displayName", "mode", "litellmProvider", "supportsPromptCaching",
	"inputCostPerToken", "outputCostPerToken", "outputCostPerImage", "inputCostPerRequest",
	"cacheReadInputTokenCost", "cacheCreationInputTokenCost", "cacheCreationInputTokenCostAbove1hr",
	"extraFieldsJson",
}

// modelPriceSingleInputOf 校验请求体。
func modelPriceSingleInputOf(fields map[string]json.RawMessage) (modelPriceSingleInput, []invalidParam) {
	object := adminNewObject(fields, modelPriceSingleFields...)
	object.RejectUnknownKeys()

	input := modelPriceSingleInput{}
	input.modelName, _ = object.String("modelName", adminStringSpec{Required: true, Trim: true, MinRunes: 1})
	input.displayName, _ = object.String("displayName", adminStringSpec{Trim: true, MaxRunes: 0})
	input.mode, _ = object.String("mode", adminStringSpec{
		Required: true, Enum: []string{"chat", "image_generation", "completion"},
	})
	input.litellmProvider, _ = object.String("litellmProvider", adminStringSpec{Trim: true})
	input.supportsPromptCaching, _ = object.Bool("supportsPromptCaching")
	input.inputCostPerToken, _ = object.Number("inputCostPerToken", false, nil)
	input.outputCostPerToken, _ = object.Number("outputCostPerToken", false, nil)
	input.outputCostPerImage, _ = object.Number("outputCostPerImage", false, nil)
	input.inputCostPerRequest, _ = object.Number("inputCostPerRequest", false, nil)
	input.cacheReadInputTokenCost, _ = object.Number("cacheReadInputTokenCost", false, nil)
	input.cacheCreationInputTokenCost, _ = object.Number("cacheCreationInputTokenCost", false, nil)
	input.cacheCreationAbove1hrCost, _ = object.Number("cacheCreationInputTokenCostAbove1hr", false, nil)

	extraFieldsJson, _ := object.String("extraFieldsJson", adminStringSpec{Trim: true})
	if strings.TrimSpace(extraFieldsJson) != "" {
		var parsed any
		if err := json.Unmarshal([]byte(extraFieldsJson), &parsed); err != nil {
			object.fail([]any{"extraFieldsJson"}, "custom", "高级字段 JSON 解析失败")
		} else if _, isObject := parsed.(map[string]any); !isObject {
			object.fail([]any{"extraFieldsJson"}, "custom", "高级字段必须是 JSON 对象")
		} else {
			sanitized, err := modelPriceSanitizeExtra(parsed, "")
			if err != nil {
				object.fail([]any{"extraFieldsJson"}, "custom", err.Error())
			} else {
				input.extraFields = sanitized.(map[string]any)
			}
		}
	}
	return input, object.issues0()
}

// modelPriceBuildData 复刻 upsertSingleModelPrice 里价格数据的构造（actions/model-prices.ts:740-770）。
//
// 顺序与 Node 一致：先铺 extraPriceData，再用已知字段覆盖。落库后由 jsonb 归一，故键序不影响响应。
func modelPriceBuildData(input modelPriceSingleInput) (json.RawMessage, error) {
	data := map[string]any{}
	for key, value := range input.extraFields {
		data[key] = value
	}
	data["mode"] = input.mode
	if input.displayName != "" {
		data["display_name"] = input.displayName
	}
	if input.litellmProvider != "" {
		data["litellm_provider"] = input.litellmProvider
	}
	if input.supportsPromptCaching != nil {
		data["supports_prompt_caching"] = *input.supportsPromptCaching
	}
	setNumber := func(key string, value *float64) {
		if value != nil {
			data[key] = *value
		}
	}
	setNumber("input_cost_per_token", input.inputCostPerToken)
	setNumber("output_cost_per_token", input.outputCostPerToken)
	setNumber("output_cost_per_image", input.outputCostPerImage)
	setNumber("input_cost_per_request", input.inputCostPerRequest)
	setNumber("cache_read_input_token_cost", input.cacheReadInputTokenCost)
	setNumber("cache_creation_input_token_cost", input.cacheCreationInputTokenCost)
	setNumber("cache_creation_input_token_cost_above_1hr", input.cacheCreationAbove1hrCost)

	encoded, err := adminMarshalJSON(data)
	if err != nil {
		return nil, fmt.Errorf("模型价格数据序列化失败: %w", err)
	}
	return encoded, nil
}

// modelPriceSanitizeExtra 复刻 sanitizeExtraPriceData（actions/model-prices.ts:604-630）。
//
// 两条规则：剔除 __proto__/constructor/prototype 键（原型链污染防护），以及「价格类字段路径的
// 叶子必须是非负数字」——字符串形式的数字也接受（价格表里常见 "0.000003"）。
func modelPriceSanitizeExtra(value any, path string) (any, error) {
	switch node := value.(type) {
	case []any:
		result := make([]any, 0, len(node))
		for index, item := range node {
			sanitized, err := modelPriceSanitizeExtra(item, fmt.Sprintf("%s[%d]", path, index))
			if err != nil {
				return nil, err
			}
			result = append(result, sanitized)
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(node))
		for key, item := range node {
			if key == "__proto__" || key == "constructor" || key == "prototype" {
				continue
			}
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			sanitized, err := modelPriceSanitizeExtra(item, childPath)
			if err != nil {
				return nil, err
			}
			result[key] = sanitized
		}
		return result, nil
	default:
		if path != "" && modelPriceIsPriceLikePath(path) {
			if text, isText := value.(string); isText {
				parsed, err := parseNonNegativeNumber(strings.TrimSpace(text))
				if err != nil {
					return nil, fmt.Errorf("%s 必须是非负数", path)
				}
				return parsed, nil
			}
			parsed, err := parseNonNegativeNumberValue(value)
			if err != nil {
				return nil, fmt.Errorf("%s 必须是非负数", path)
			}
			return parsed, nil
		}
		return value, nil
	}
}

// modelPriceIsPriceLikePath 复刻 isPriceLikeFieldPath：任一段命中价格关键词即算价格类。
func modelPriceIsPriceLikePath(path string) bool {
	for _, segment := range strings.Split(path, ".") {
		if modelPricePriceLikeKeyPattern.MatchString(segment) {
			return true
		}
	}
	return false
}

// modelPriceManualFromProvider 复刻 buildManualPriceDataFromProviderPricing（actions/model-prices.ts:77-99）。
//
// `pricing: undefined` 的语义是**删除**该键：JSON.stringify 会丢掉 undefined，而我们的 map 里
// 必须显式 delete，否则入库会多一个 null 的 pricing。
func modelPriceManualFromProvider(
	basePriceData json.RawMessage,
	modelName string,
	pricingProviderKey string,
) (json.RawMessage, bool) {
	var base map[string]any
	if err := json.Unmarshal(basePriceData, &base); err != nil {
		return nil, false
	}
	pricing, isObject := base["pricing"].(map[string]any)
	if !isObject {
		return nil, false
	}
	node, isObject := pricing[pricingProviderKey].(map[string]any)
	if !isObject {
		return nil, false
	}
	merged := make(map[string]any, len(base)+len(node)+3)
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range node {
		merged[key] = value
	}
	delete(merged, "pricing")
	merged["litellm_provider"] = pricingProviderKey
	merged["selected_pricing_provider"] = pricingProviderKey
	merged["selected_pricing_source_model"] = modelName
	merged["selected_pricing_resolution"] = "manual_pin"

	encoded, err := adminMarshalJSON(merged)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// modelPricePayload 把一行映射成响应体。
func modelPricePayload(row store.AdminModelPrice) modelPriceItem {
	return modelPriceItem{
		ID:        row.ID,
		ModelName: row.ModelName,
		PriceData: row.PriceData,
		Source:    row.Source,
		CreatedAt: adminStringOrNow(row.CreatedAt),
		UpdatedAt: adminStringOrNow(row.UpdatedAt),
	}
}

// modelPriceAuditPayload 复刻 upsert 审计的 after 快照（actions/model-prices.ts:777-782）。
func modelPriceAuditPayload(row store.AdminModelPrice) map[string]any {
	return map[string]any{
		"id":        row.ID,
		"modelName": row.ModelName,
		"source":    row.Source,
		"priceData": json.RawMessage(row.PriceData),
	}
}

// modelPriceBeforeID 复刻 delete 审计的 targetId：Node 是 `beforePrice ? String(beforePrice.id) : null`。
func modelPriceBeforeID(row *store.AdminModelPrice) string {
	if row == nil {
		return ""
	}
	return fmt.Sprintf("%d", row.ID)
}

// modelPriceAudit 组装审计事件。
//
// before 为 nil（查询失败或库中无该模型）时不写 before_value——Node 侧 `before: beforePrice ?? undefined`
// 同理会让该键缺席。
type modelPriceAuditEvent struct {
	event AuditEvent
}

func modelPriceAudit(
	pools *store.Pools,
	request *http.Request,
	action, targetID, targetName string,
	before *store.AdminModelPrice,
) modelPriceAuditEvent {
	event := adminAuditEvent(pools, request, action, "model_price", targetID, targetName)
	if before != nil {
		event.Before = map[string]any{
			"id":        before.ID,
			"modelName": before.ModelName,
			"priceData": json.RawMessage(before.PriceData),
			"source":    before.Source,
			"createdAt": adminStringOrEmpty(before.CreatedAt),
			"updatedAt": adminStringOrEmpty(before.UpdatedAt),
		}
	}
	return modelPriceAuditEvent{event: event}
}

func (e modelPriceAuditEvent) success() AuditEvent {
	e.event.Success = true
	return e.event
}

func (e modelPriceAuditEvent) failure(errorMessage string) AuditEvent {
	e.event.Success = false
	e.event.ErrorMessage = errorMessage
	return e.event
}

// modelPriceStringField 从 jsonb 里取字符串字段；非字符串（或缺失）返回空串——与 Node 的
// `typeof x === "string" ? x : null` 同义（调用方据此决定输出 null 还是值）。
func modelPriceStringField(priceData json.RawMessage, key string) string {
	if len(priceData) == 0 {
		return ""
	}
	var data map[string]any
	if err := json.Unmarshal(priceData, &data); err != nil {
		return ""
	}
	value, _ := data[key].(string)
	return value
}

// modelPriceQueryInt 解析分页/范围类的查询参数（复刻 z.coerce.number().int().min().max().default()）。
//
// 返回 (值, 是否通过)。缺省值按 spec 的 defaultValue 给。
func modelPriceQueryInt(raw string, defaultValue, min, max int) (int, bool) {
	if strings.TrimSpace(raw) == "" {
		return defaultValue, true
	}
	value, ok := adminCoerceInt(raw)
	if !ok {
		return defaultValue, false
	}
	if value < min {
		return defaultValue, false
	}
	if max > 0 && value > max {
		return defaultValue, false
	}
	return value, true
}

// modelPriceRangeIssue 构造一条范围校验失败：**语义码必须区分方向**（zod 的 too_small / too_big）。
// 固定写 too_small 曾让「pageSize 超过上限」也被报成 too_small（消息却说 less than or equal）。
func modelPriceRangeIssue(field, code, message string) invalidParam {
	return invalidParam{Path: []any{field}, Code: code, Message: message}
}

// modelPricePriceLikeKeyPattern 复刻 isPriceLikeFieldKey 的关键词表
// （src/lib/utils/model-price-fields.ts:114-118）。
var modelPricePriceLikeKeyPattern = regexp.MustCompile(
	`(?i)(cost|price|rate|multiplier|per_|second|session|query|page|pixel|character|dbu)`)

// parseNonNegativeNumber 复刻 sanitizeExtraPriceData 对字符串叶子的处理：trim 后按 JS Number()
// 解析，必须是有限非负数。空串在 JS 里被 `value.trim()` 拦在门外（不是走 Number），故这里也报错。
func parseNonNegativeNumber(text string) (float64, error) {
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("不是合法数字")
	}
	if parsed < 0 {
		return 0, fmt.Errorf("必须是非负数")
	}
	return parsed, nil
}

// parseNonNegativeNumberValue 处理非字符串叶子：JS 侧 `typeof value !== "number"` 直接抛错。
func parseNonNegativeNumberValue(value any) (float64, error) {
	number, ok := value.(float64)
	if !ok {
		return 0, fmt.Errorf("必须是非负数")
	}
	if number < 0 {
		return 0, fmt.Errorf("必须是非负数")
	}
	return number, nil
}
