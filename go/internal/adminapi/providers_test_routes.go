package adminapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/providertest"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// providers_test_routes.go 实现 providers 测试族 6 条：
//
//	POST /providers/test:unified                 （router.ts:668-689 → handlers.ts:441-448）
//	POST /providers/{id:[0-9]+}/test              （router.ts:692-718 → handlers.ts:450-470）
//	POST /providers/test:anthropic-messages       （router.ts:721-742）
//	POST /providers/test:openai-chat-completions  （router.ts:745-766）
//	POST /providers/test:openai-responses         （router.ts:769-790）
//	POST /providers/test:gemini                   （router.ts:793-814）
//
// 共同语义（三条都是**实机对照得来**，不是推断）：
//   - 校验失败（代理地址、供应商 URL）与探测失败**都返回 HTTP 200** + `success:false`；
//     只有「action 层不可执行」（自定义正文非法、preset 不存在、供应商不存在）才走 Problem；
//   - unified 与 by-id 的 200 载荷是「统一测试结果」（`providertest.BuildUnifiedTestPayload`）；
//   - 四条定型端点的 200 载荷是 `{success, message, details:{responseTime, …}}`。
//
// 注册点由 `cmd/cchd/admin.go` 调用 `RegisterProvidersTestRoutes(router, deps)`。

const (
	// providerAPITestTimeoutMinMS/MaxMS 复刻 ProviderApiTestSchema 的 `.min(5000).max(120000)`。
	providerAPITestTimeoutMinMS = 5000
	providerAPITestTimeoutMaxMS = 120000

	// providerAPITestUnifiedByIDTimeoutMS 复刻 BY_ID_TEST_TIMEOUT_MS（actions/providers.ts:5179）。
	providerAPITestByIDTimeoutMS = 15000
	// providerAPITestByIDGeminiTimeoutMS 复刻 BY_ID_TEST_GEMINI_TIMEOUT_MS（5180）。
	providerAPITestByIDGeminiTimeoutMS = 60000
)

// providerAPITestBody 是 ProviderApiTestSchema（四条定型端点）解码后的体。
type providerAPITestBody struct {
	ProviderURL           string
	APIKey                string
	Model                 string
	ProxyURL              string
	ProxyFallbackToDirect bool
	TimeoutMS             *int64
}

// providerAPITestBodyFields 是 ProviderApiTestSchema 的允许键集合（strict）。
var providerAPITestBodyFields = []string{
	"providerUrl", "apiKey", "model", "proxyUrl", "proxyFallbackToDirect", "timeoutMs",
}

// providerUnifiedTestBody 是 ProviderUnifiedTestSchema 解码后的体。
type providerUnifiedTestBody struct {
	providerAPITestBody
	ProviderType       string
	LatencyThresholdMS *int64
	SuccessContains    string
	Preset             string
	CustomPayload      string
	CustomHeaders      map[string]string
}

// providerUnifiedTestBodyFields 是扩展后的允许键集合（strict）。
var providerUnifiedTestBodyFields = append(
	append([]string{}, providerAPITestBodyFields...),
	"providerType", "latencyThresholdMs", "successContains", "preset", "customPayload", "customHeaders",
)

// RegisterProvidersTestRoutes 注册测试族路由。
//
// 缺 Store 时与同族其他 registrar 同判：**整族不注册**并留 warn（by-id 要读库）。
// unified/typed 五条本可无库运行，但它们与 by-id 共享同一套 schema 与 runner，
// 分两档注册只会让「缺库时部分可用」这种半吊子状态更难排障；生产装配必然有 Store。
func RegisterProvidersTestRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		logProvidersTestUnwired(deps, "store_missing", "/providers/test:*")
		return
	}
	routes := []Route{
		{
			Method:      http.MethodPost,
			Path:        "/providers/test:unified",
			Access:      AccessAdmin,
			Module:      "providers",
			OperationID: "testProviderUnified",
			Handler:     http.HandlerFunc(handleTestProviderUnified(deps)),
		},
		{
			Method:      http.MethodPost,
			Path:        "/providers/{id:[0-9]+}/test",
			Access:      AccessAdmin,
			Module:      "providers",
			OperationID: "testProviderById",
			Handler:     http.HandlerFunc(handleTestProviderByID(deps)),
		},
		{
			Method:      http.MethodPost,
			Path:        "/providers/test:anthropic-messages",
			Access:      AccessAdmin,
			Module:      "providers",
			OperationID: "testProviderAnthropic",
			Handler:     http.HandlerFunc(handleTestProviderAnthropic(deps)),
		},
		{
			Method:      http.MethodPost,
			Path:        "/providers/test:openai-chat-completions",
			Access:      AccessAdmin,
			Module:      "providers",
			OperationID: "testProviderOpenAIChat",
			Handler:     http.HandlerFunc(handleTestProviderOpenAIChat(deps)),
		},
		{
			Method:      http.MethodPost,
			Path:        "/providers/test:openai-responses",
			Access:      AccessAdmin,
			Module:      "providers",
			OperationID: "testProviderOpenAIResponses",
			Handler:     http.HandlerFunc(handleTestProviderOpenAIResponses(deps)),
		},
		{
			Method:      http.MethodPost,
			Path:        "/providers/test:gemini",
			Access:      AccessAdmin,
			Module:      "providers",
			OperationID: "testProviderGemini",
			Handler:     http.HandlerFunc(handleTestProviderGemini(deps)),
		},
	}
	for _, route := range routes {
		router.Add(route)
	}
}

// handleTestProviderUnified 复刻 handlers.ts:441-448 + actions/providers.ts:5063-5117。
func handleTestProviderUnified(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := readJSONObject(writer, request)
		if !ok {
			return
		}
		body, issues := decodeProviderUnifiedTestBody(fields, providerDashboardCompat(request))
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		// 复刻 5072-5077：URL 基础校验失败 → action error（Problem 400）。
		if _, message, ok := providertest.ValidateProviderURLForConnectivity(body.ProviderURL); !ok {
			writeProviderActionError(deps, writer, request, message)
			return
		}

		// 复刻 5079-5088：customHeaders 归一化；错误码**不含原始值**（避免泄漏）。
		normalizedHeaders, headerCode := normalizeProviderTestCustomHeaders(body.CustomHeaders)
		if headerCode != "" {
			writeProviderActionError(deps, writer, request, "custom_headers_"+headerCode)
			return
		}

		config := providertest.Config{
			ProviderURL:           body.ProviderURL,
			APIKey:                body.APIKey,
			ProviderType:          providertest.ProviderType(body.ProviderType),
			Model:                 body.Model,
			ProxyURL:              body.ProxyURL,
			ProxyFallbackToDirect: body.ProxyFallbackToDirect,
			LatencyThresholdMS:    body.LatencyThresholdMS,
			SuccessContains:       body.SuccessContains,
			TimeoutMS:             body.TimeoutMS,
			Preset:                body.Preset,
			CustomPayload:         body.CustomPayload,
			CustomHeaders:         normalizedHeaders,
		}
		result, err := providertest.ExecuteProviderTest(request.Context(), config, nil)
		if err != nil {
			// 复刻 5111-5114：执行期错误（无效自定义正文 / preset 不存在 / 不支持的供应商类型）
			// 走 action error，正文是 Problem 400。
			writeProviderActionError(deps, writer, request, err.Error())
			return
		}
		adminWriteJSON(writer, http.StatusOK, providertest.BuildUnifiedTestPayload(result))
	}
}

// handleTestProviderByID 复刻 handlers.ts:450-470 + actions/providers.ts:5184-5246。
func handleTestProviderByID(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := providerIDParam(writer, request)
		if !ok {
			return
		}
		fields, ok := readJSONObject(writer, request)
		if !ok {
			return
		}
		model, issues := decodeProviderTestByIDBody(fields)
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		// 复刻 handlers.ts:462-467：先按可见性找（不可见即 404 provider.not_found）。
		provider, err := providerFindVisible(request, deps, id)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, err)
			return
		}

		// 复刻 5196-5200：URL 基础校验失败 → action error。
		if _, message, ok := providertest.ValidateProviderURLForConnectivity(provider.URL); !ok {
			writeProviderActionError(deps, writer, request, message)
			return
		}

		isGemini := strings.HasPrefix(provider.ProviderType, "gemini")
		timeoutMS := int64(providerAPITestByIDTimeoutMS)
		if isGemini {
			timeoutMS = providerAPITestByIDGeminiTimeoutMS
		}

		// 与 Node 的差异（登记）：Node 对 Gemini 的 JSON 凭据会先换 OAuth access token
		// （`GeminiAuth.getAccessToken`，本仓未移植）。**交换失败时** Node 的行为与本实现一致：
		// 原样用 x-goog-api-key（`isJsonCreds` 保持 false），故 geminiBearerAuth 恒 false。
		config := providertest.Config{
			ProviderID:            strconv.FormatInt(provider.ID, 10),
			ProviderURL:           provider.URL,
			APIKey:                provider.Key,
			ProviderType:          providertest.ProviderType(provider.ProviderType),
			Model:                 model,
			ProxyURL:              providerProxyURL(provider),
			ProxyFallbackToDirect: providerProxyFallback(provider),
			CustomHeaders:         providerCustomHeadersForTest(provider),
			TimeoutMS:             &timeoutMS,
		}
		result, err := providertest.ExecuteProviderTest(request.Context(), config, nil)
		if err != nil {
			writeProviderActionError(deps, writer, request, err.Error())
			return
		}
		adminWriteJSON(writer, http.StatusOK, providertest.BuildUnifiedTestPayload(result))
	}
}

// handleTestProviderAnthropic 复刻 handlers.ts:472-474 → actions/providers.ts:4679。
func handleTestProviderAnthropic(deps Deps) http.HandlerFunc {
	return typedTestHandler(deps, providertest.TypedConfigAnthropicMessages)
}

// handleTestProviderOpenAIChat 复刻 handlers.ts:476-478 → actions/providers.ts:4705。
func handleTestProviderOpenAIChat(deps Deps) http.HandlerFunc {
	return typedTestHandler(deps, providertest.TypedConfigOpenAIChatCompletions)
}

// handleTestProviderOpenAIResponses 复刻 handlers.ts:480-482 → actions/providers.ts:4739。
func handleTestProviderOpenAIResponses(deps Deps) http.HandlerFunc {
	return typedTestHandler(deps, providertest.TypedConfigOpenAIResponses)
}

// typedTestHandler 是三条「单次尝试」定型端点的共用骨架。
func typedTestHandler(deps Deps, config func() providertest.TypedOptions) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := readJSONObject(writer, request)
		if !ok {
			return
		}
		body, issues := decodeProviderAPITestBody(fields)
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}
		args := providertest.TypedArgs{
			ProviderURL:           body.ProviderURL,
			APIKey:                body.APIKey,
			Model:                 body.Model,
			ProxyURL:              body.ProxyURL,
			ProxyFallbackToDirect: body.ProxyFallbackToDirect,
			TimeoutMS:             body.TimeoutMS,
		}
		result := providertest.ExecuteProviderAPITest(request.Context(), args, config(), nil)
		adminWriteJSON(writer, http.StatusOK, result)
	}
}

// handleTestProviderGemini 复刻 handlers.ts:484-486 → actions/providers.ts:4782-4947。
//
// 与另外三条的差别：**两次尝试**。首次只走 header 认证；若失败且 message 里是 401/403，
// 再用「URL 查询串 + header」重试一次，成功时在文案后追加 ` [FALLBACK:URL_PARAM]`。
func handleTestProviderGemini(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := readJSONObject(writer, request)
		if !ok {
			return
		}
		body, issues := decodeProviderAPITestBody(fields)
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		// 复刻 4810-4817：超时范围先于执行校验（越界时 200 + success:false，文案含秒数）。
		if body.TimeoutMS != nil {
			if *body.TimeoutMS < providerAPITestTimeoutMinMS || *body.TimeoutMS > providerAPITestTimeoutMaxMS {
				adminWriteJSON(writer, http.StatusOK, map[string]any{
					"success": false,
					"message": fmt.Sprintf("超时时间必须在 %d-%d 秒之间",
						providerAPITestTimeoutMinMS/1000, providerAPITestTimeoutMaxMS/1000),
				})
				return
			}
		}

		args := providertest.TypedArgs{
			ProviderURL:           body.ProviderURL,
			APIKey:                body.APIKey,
			Model:                 body.Model,
			ProxyURL:              body.ProxyURL,
			ProxyFallbackToDirect: body.ProxyFallbackToDirect,
			TimeoutMS:             body.TimeoutMS,
		}

		first := providertest.ExecuteProviderAPITest(request.Context(), args, providertest.TypedConfigGemini(false), nil)
		if success, _ := first["success"].(bool); success {
			adminWriteJSON(writer, http.StatusOK, first)
			return
		}
		if !providertest.GeminiAuthErrorFromResult(first) {
			adminWriteJSON(writer, http.StatusOK, first)
			return
		}

		second := providertest.ExecuteProviderAPITest(request.Context(), args, providertest.TypedConfigGeminiURLParam(), nil)
		if success, _ := second["success"].(bool); success {
			// 复刻 4939-4948：成功时在文案后加 [FALLBACK:URL_PARAM]。
			if message, ok := second["message"].(string); ok {
				second["message"] = message + " [FALLBACK:URL_PARAM]"
			}
		}
		adminWriteJSON(writer, http.StatusOK, second)
	}
}

// readJSONObject 读请求体为「原始键值对」，坏 JSON 走与 Node 同形的 400（复刻 parseJson 的失败分支）。
func readJSONObject(writer http.ResponseWriter, request *http.Request) (map[string]json.RawMessage, bool) {
	raw, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path: []any{}, Code: "invalid_type", Message: "Invalid JSON body",
		}})
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path: []any{}, Code: "invalid_type", Message: "Invalid JSON body",
		}})
		return nil, false
	}
	return fields, true
}

// decodeProviderAPITestBody 复刻 ProviderApiTestSchema（strict）的校验。
func decodeProviderAPITestBody(fields map[string]json.RawMessage) (providerAPITestBody, []invalidParam) {
	body, issues := decodeAPITestShared(fields, providerAPITestBodyFields)
	return body, issues
}

// decodeProviderUnifiedTestBody 复刻 ProviderUnifiedTestSchema（strict）。
//
// 兼容头（dashboard compat）下 providerType 放宽到内部类型（claude-auth / gemini-cli），
// 与 handlers.ts:61-63 的 InternalProviderUnifiedTestSchema 同判。
func decodeProviderUnifiedTestBody(
	fields map[string]json.RawMessage,
	dashboardCompat bool,
) (providerUnifiedTestBody, []invalidParam) {
	shared, issues := decodeAPITestShared(fields, providerUnifiedTestBodyFields)
	result := providerUnifiedTestBody{providerAPITestBody: shared}

	// latencyThresholdMs: z.number().int().min(1).optional()
	if raw, present := fields["latencyThresholdMs"]; present {
		if value, ok := upstreamInt(raw); ok {
			if value < 1 {
				issues = append(issues, invalidParam{
					Path: []any{"latencyThresholdMs"}, Code: "too_small",
					Message: "Number must be greater than or equal to 1",
				})
			} else {
				v := int64(value)
				result.LatencyThresholdMS = &v
			}
		} else {
			issues = append(issues, invalidParam{
				Path: []any{"latencyThresholdMs"}, Code: "invalid_type",
				Message: "Expected number, received " + jsonTypeName(raw),
			})
		}
	}

	// successContains / preset / customPayload: z.string().optional()
	for _, name := range []string{"successContains", "preset", "customPayload"} {
		if raw, present := fields[name]; present && !isJSONNull(raw) {
			value, ok := upstreamString(raw)
			if !ok {
				issues = append(issues, invalidParam{
					Path: []any{name}, Code: "invalid_type",
					Message: "Expected string, received " + jsonTypeName(raw),
				})
				continue
			}
			switch name {
			case "successContains":
				result.SuccessContains = value
			case "preset":
				result.Preset = value
			case "customPayload":
				result.CustomPayload = value
			}
		}
	}

	// customHeaders: z.record(z.string(), z.string()).optional()
	if raw, present := fields["customHeaders"]; present && !isJSONNull(raw) {
		headers, ok := decodeStringRecord(raw)
		if !ok {
			issues = append(issues, invalidParam{
				Path: []any{"customHeaders"}, Code: "invalid_type",
				Message: "Expected object, received " + jsonTypeName(raw),
			})
		} else {
			result.CustomHeaders = headers
		}
	}

	// providerType: enum（必填；兼容头下含隐藏类型）
	if raw, present := fields["providerType"]; !present {
		issues = append(issues, invalidParam{
			Path: []any{"providerType"}, Code: "invalid_type", Message: "Required",
		})
	} else if value, ok := upstreamString(raw); ok {
		if !validProviderTypeValue(value) || (hiddenProviderType(value) && !dashboardCompat) {
			issues = append(issues, invalidParam{
				Path: []any{"providerType"}, Code: "invalid_enum_value", Message: "Invalid enum value",
			})
		} else {
			result.ProviderType = value
		}
	} else {
		issues = append(issues, invalidParam{
			Path: []any{"providerType"}, Code: "invalid_type",
			Message: "Expected string, received " + jsonTypeName(raw),
		})
	}

	return result, issues
}

// decodeAPITestShared 是 ProviderApiTestSchema 六个字段的共用解码（strict：先查未知键）。
func decodeAPITestShared(
	fields map[string]json.RawMessage,
	allowedFields []string,
) (providerAPITestBody, []invalidParam) {
	issues := make([]invalidParam, 0, 2)
	allowed := make(map[string]bool, len(allowedFields))
	for _, name := range allowedFields {
		allowed[name] = true
	}
	unknown := make([]string, 0, 1)
	for name := range fields {
		if !allowed[name] {
			unknown = append(unknown, name)
		}
	}
	// map 迭代无序：排序让同一请求的报错顺序稳定（对拍逐项比对）。
	sort.Strings(unknown)
	for _, name := range unknown {
		issues = append(issues, invalidParam{
			Path: []any{name}, Code: "unrecognized_keys", Message: "Unrecognized key: " + name,
		})
	}

	var body providerAPITestBody

	// providerUrl: z.string().trim().url()
	if raw, present := fields["providerUrl"]; !present {
		issues = append(issues, invalidParam{Path: []any{"providerUrl"}, Code: "invalid_type", Message: "Required"})
	} else if value, ok := upstreamString(raw); ok {
		trimmed := strings.TrimSpace(value)
		if !providerIsAbsoluteURL(trimmed) {
			issues = append(issues, invalidParam{Path: []any{"providerUrl"}, Code: "invalid_format", Message: "Invalid URL"})
		} else {
			body.ProviderURL = trimmed
		}
	} else {
		issues = append(issues, invalidParam{
			Path: []any{"providerUrl"}, Code: "invalid_type",
			Message: "Expected string, received " + jsonTypeName(raw),
		})
	}

	// apiKey: z.string().min(1)（未 trim）
	if raw, present := fields["apiKey"]; !present {
		issues = append(issues, invalidParam{Path: []any{"apiKey"}, Code: "invalid_type", Message: "Required"})
	} else if value, ok := upstreamString(raw); ok {
		if value == "" {
			issues = append(issues, invalidParam{
				Path: []any{"apiKey"}, Code: "too_small",
				Message: "String must contain at least 1 character(s)",
			})
		} else {
			body.APIKey = value
		}
	} else {
		issues = append(issues, invalidParam{
			Path: []any{"apiKey"}, Code: "invalid_type",
			Message: "Expected string, received " + jsonTypeName(raw),
		})
	}

	// model: z.string().trim().optional()
	if raw, present := fields["model"]; present && !isJSONNull(raw) {
		if value, ok := upstreamString(raw); ok {
			body.Model = strings.TrimSpace(value)
		} else {
			issues = append(issues, invalidParam{
				Path: []any{"model"}, Code: "invalid_type",
				Message: "Expected string, received " + jsonTypeName(raw),
			})
		}
	}

	// proxyUrl: z.string().trim().nullable().optional()
	if raw, present := fields["proxyUrl"]; present && !isJSONNull(raw) {
		if value, ok := upstreamString(raw); ok {
			body.ProxyURL = strings.TrimSpace(value)
		} else {
			issues = append(issues, invalidParam{
				Path: []any{"proxyUrl"}, Code: "invalid_type",
				Message: "Expected string, received " + jsonTypeName(raw),
			})
		}
	}

	// proxyFallbackToDirect: z.boolean().optional()
	if raw, present := fields["proxyFallbackToDirect"]; present {
		if value, ok := upstreamBool(raw); ok {
			body.ProxyFallbackToDirect = value
		} else {
			issues = append(issues, invalidParam{
				Path: []any{"proxyFallbackToDirect"}, Code: "invalid_type",
				Message: "Expected boolean, received " + jsonTypeName(raw),
			})
		}
	}

	// timeoutMs: z.number().int().min(5000).max(120000).optional()
	if raw, present := fields["timeoutMs"]; present {
		value, ok := upstreamInt(raw)
		switch {
		case !ok:
			issues = append(issues, invalidParam{
				Path: []any{"timeoutMs"}, Code: "invalid_type",
				Message: "Expected number, received " + jsonTypeName(raw),
			})
		case value < providerAPITestTimeoutMinMS:
			issues = append(issues, invalidParam{
				Path: []any{"timeoutMs"}, Code: "too_small",
				Message: "Number must be greater than or equal to " + strconv.Itoa(providerAPITestTimeoutMinMS),
			})
		case value > providerAPITestTimeoutMaxMS:
			issues = append(issues, invalidParam{
				Path: []any{"timeoutMs"}, Code: "too_big",
				Message: "Number must be less than or equal to " + strconv.Itoa(providerAPITestTimeoutMaxMS),
			})
		default:
			v := int64(value)
			body.TimeoutMS = &v
		}
	}

	return body, issues
}

// decodeProviderTestByIDBody 复刻 ProviderTestByIdSchema（strict，只有一个可选 model）。
func decodeProviderTestByIDBody(fields map[string]json.RawMessage) (string, []invalidParam) {
	issues := make([]invalidParam, 0, 1)
	unknown := make([]string, 0, 1)
	for name := range fields {
		if name != "model" {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		issues = append(issues, invalidParam{
			Path: []any{name}, Code: "unrecognized_keys", Message: "Unrecognized key: " + name,
		})
	}

	model := ""
	if raw, present := fields["model"]; present && !isJSONNull(raw) {
		value, ok := upstreamString(raw)
		switch {
		case !ok:
			issues = append(issues, invalidParam{
				Path: []any{"model"}, Code: "invalid_type",
				Message: "Expected string, received " + jsonTypeName(raw),
			})
		case strings.TrimSpace(value) == "":
			issues = append(issues, invalidParam{
				Path: []any{"model"}, Code: "too_small",
				Message: "String must contain at least 1 character(s)",
			})
		default:
			model = strings.TrimSpace(value)
		}
	}
	return model, issues
}

// decodeStringRecord 复刻 z.record(z.string(), z.string())。
func decodeStringRecord(raw json.RawMessage) (map[string]string, bool) {
	var decoded map[string]string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

// writeProviderActionError 复刻 handlers.ts:829-836 的 actionError：Problem + errorCode provider.action_failed。
func writeProviderActionError(deps Deps, writer http.ResponseWriter, request *http.Request, message string) {
	adminProblemWriter(deps).WriteActionError(writer, request,
		adminActionFailure("provider", providerActionMessage(message)))
}

type providerActionMessage string

func (e providerActionMessage) Error() string { return string(e) }

// normalizeProviderTestCustomHeaders 复刻 `normalizeCustomHeadersRecord`（src/lib/custom-headers.ts:33-72）。
//
// 只做能在「已通过 zod record 校验」之后仍会失败的检查：空名、CRLF、非法名、受保护名。
// 返回错误码（空串表示通过），调用方拼成 `custom_headers_<code>` 的 action error。
func normalizeProviderTestCustomHeaders(input map[string]string) (map[string]string, string) {
	if len(input) == 0 {
		return nil, ""
	}
	names := make([]string, 0, len(input))
	for name := range input {
		names = append(names, name)
	}
	sort.Strings(names)

	normalized := make(map[string]string, len(input))
	for _, name := range names {
		value := input[name]
		if name == "" || strings.TrimSpace(name) == "" {
			return nil, "empty_name"
		}
		if hasCRLF(name) || hasCRLF(value) {
			return nil, "crlf"
		}
		if !providerTestHeaderNameValid(name) {
			return nil, "invalid_name"
		}
		if providerTestProtectedHeaders[strings.ToLower(name)] {
			return nil, "protected_name"
		}
		normalized[name] = value
	}
	return normalized, ""
}

func hasCRLF(value string) bool {
	return strings.ContainsAny(value, "\r\n")
}

// providerTestHeaderNameValid 复刻 HTTP_TOKEN_NAME_REGEX（custom-headers.ts:22）。
func providerTestHeaderNameValid(name string) bool {
	if name == "" {
		return false
	}
	const extra = "!#$%&'*+.^_`|~-"
	for _, symbol := range name {
		switch {
		case symbol >= '0' && symbol <= '9':
		case symbol >= 'A' && symbol <= 'Z':
		case symbol >= 'a' && symbol <= 'z':
		case strings.ContainsRune(extra, symbol):
		default:
			return false
		}
	}
	return true
}

// providerTestProtectedHeaders 复刻 PROTECTED_AUTH_HEADER_NAMES（custom-headers.ts:24-28）。
var providerTestProtectedHeaders = map[string]bool{
	"authorization":  true,
	"x-api-key":      true,
	"x-goog-api-key": true,
}

func providerProxyURL(provider *store.AdminProvider) string {
	if provider.ProxyURL == nil {
		return ""
	}
	return strings.TrimSpace(*provider.ProxyURL)
}

func providerProxyFallback(provider *store.AdminProvider) bool {
	return provider.ProxyFallbackToDirect
}

// logProvidersTestUnwired 记录测试族未装配（与 keys/其他 registrar 同风格）。
func logProvidersTestUnwired(deps Deps, reason, scope string) {
	if deps.Logger != nil {
		deps.Logger.Warn("admin_providers_test_unwired", map[string]any{
			"reason": reason,
			"scope":  scope,
			"action": "routes_not_registered",
		})
	}
}

// providerCustomHeadersForTest 把库里存的 custom_headers 解成测试用记录。
func providerCustomHeadersForTest(provider *store.AdminProvider) map[string]string {
	if len(provider.CustomHeaders) == 0 {
		return nil
	}
	var decoded map[string]string
	if err := json.Unmarshal(provider.CustomHeaders, &decoded); err != nil {
		return nil
	}
	return decoded
}
