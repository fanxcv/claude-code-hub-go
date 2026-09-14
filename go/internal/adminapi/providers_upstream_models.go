package adminapi

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/providertest"
)

// providers_upstream_models.go 实现 POST /providers/upstream-models:fetch。
//
// 唯一真源：
//   - 路由与校验：src/app/api/v1/resources/providers/router.ts:841-870，
//     `ProviderFetchUpstreamModelsRouteSchema`（= ProviderFetchUpstreamModelsSchema，
//     ProviderApiTestSchema + providerType，**strict**）；
//   - 处理器：handlers.ts:508-524（fetchProviderUpstreamModels）；
//   - 动作：src/actions/providers.ts:5453-5630（fetchUpstreamModels 三分支）；
//   - URL/代理校验：src/lib/validation/provider-url.ts:14、src/lib/proxy-agent.ts:219。
//
// 响应形状：成功 = `{"models":[...],"source":"upstream"}`（actionJson 直出 result.data）；
// 失败 = Problem 信封，status 400、errorCode `provider.action_failed`
// （statusFromActionError 的默认分支：未命中 NOT_FOUND/权限/冲突类码 → 400）。

// upstreamModelsBody 是校验通过后的请求体。
type upstreamModelsBody struct {
	ProviderURL           string
	APIKey                string
	Model                 string
	ProxyURL              string
	ProxyFallbackToDirect bool
	TimeoutMS             time.Duration
}

// upstreamModelsBodyFields 是 strict schema 的允许键集合（与 ProviderApiTestSchema.extend 对齐）。
var upstreamModelsBodyFields = []string{
	"providerUrl", "apiKey", "model", "proxyUrl", "proxyFallbackToDirect", "timeoutMs", "providerType",
}

// upstreamFetchTimeoutDefault 复刻 actions/providers.ts:5370 的 UPSTREAM_FETCH_TIMEOUT_MS。
const upstreamFetchTimeoutDefault = 10 * time.Second

// upstreamFetchTimeoutMin/Max 复刻 schema 的 `.min(5000).max(120000)`。
const (
	upstreamFetchTimeoutMinMS = 5000
	upstreamFetchTimeoutMaxMS = 120000
)

func handleFetchProviderUpstreamModels(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if err != nil {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path: []any{}, Code: "invalid_type", Message: "Invalid JSON body",
			}})
			return
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path: []any{}, Code: "invalid_type", Message: "Invalid JSON body",
			}})
			return
		}

		body, issues := decodeUpstreamModelsBody(fields, dashboardCompatRequest(request))
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		// Node 的 validateProviderUrlForConnectivity：只做基础格式校验，失败时把消息当 action error 返回。
		if _, message, ok := providertest.ValidateProviderURLForConnectivity(body.ProviderURL); !ok {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("provider", errUpstreamModels(message)))
			return
		}
		if body.ProxyURL != "" && !providertest.IsValidProxyURL(body.ProxyURL) {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("provider", errUpstreamModels("代理地址格式无效")))
			return
		}

		client, err := providertest.NewUpstreamClient(
			providertest.ProxyConfig{URL: body.ProxyURL, FallbackToDirect: body.ProxyFallbackToDirect},
			body.TimeoutMS,
		)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("provider", errUpstreamModels(err.Error())))
			return
		}

		models, fetchErr := providertest.FetchUpstreamModels(
			request.Context(), client, providertest.ProviderType(providerTypeField(fields)), body.ProviderURL, body.APIKey,
		)
		if fetchErr != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("provider", errUpstreamModels(fetchErr.Error())))
			return
		}
		if models == nil {
			models = []string{}
		}
		adminWriteJSON(writer, http.StatusOK, map[string]any{"models": models, "source": "upstream"})
	}
}

// errUpstreamModels 让 ActionError 携带原始消息（写入日志；正文用统一的 Problem 形状）。
func errUpstreamModels(message string) error { return upstreamModelsError(message) }

type upstreamModelsError string

func (e upstreamModelsError) Error() string { return string(e) }

// decodeUpstreamModelsBody 复刻 strict object 的校验：未知键、必填、类型、范围。
func decodeUpstreamModelsBody(fields map[string]json.RawMessage, dashboardCompat bool) (upstreamModelsBody, []invalidParam) {
	issues := make([]invalidParam, 0, 2)

	allowed := make(map[string]bool, len(upstreamModelsBodyFields))
	for _, name := range upstreamModelsBodyFields {
		allowed[name] = true
	}
	unknown := make([]string, 0, 1)
	for name := range fields {
		if !allowed[name] {
			unknown = append(unknown, name)
		}
	}
	// map 迭代无序：排序让同一请求的报错顺序稳定（对拍时逐项比对）。
	sort.Strings(unknown)
	for _, name := range unknown {
		issues = append(issues, invalidParam{
			Path: []any{name}, Code: "unrecognized_keys", Message: "Unrecognized key: " + name,
		})
	}

	body := upstreamModelsBody{TimeoutMS: upstreamFetchTimeoutDefault}

	// providerUrl: z.string().trim().url()
	if raw, present := fields["providerUrl"]; !present {
		issues = append(issues, invalidParam{Path: []any{"providerUrl"}, Code: "invalid_type", Message: "Required"})
	} else if value, ok := upstreamString(raw); ok {
		if !providerIsAbsoluteURL(strings.TrimSpace(value)) {
			issues = append(issues, invalidParam{Path: []any{"providerUrl"}, Code: "invalid_format", Message: "Invalid URL"})
		} else {
			body.ProviderURL = strings.TrimSpace(value)
		}
	} else {
		issues = append(issues, invalidParam{Path: []any{"providerUrl"}, Code: "invalid_type", Message: "Expected string, received " + jsonTypeName(raw)})
	}

	// apiKey: z.string().min(1)（注意：schema 未 trim）
	if raw, present := fields["apiKey"]; !present {
		issues = append(issues, invalidParam{Path: []any{"apiKey"}, Code: "invalid_type", Message: "Required"})
	} else if value, ok := upstreamString(raw); ok {
		if value == "" {
			issues = append(issues, invalidParam{Path: []any{"apiKey"}, Code: "too_small", Message: "String must contain at least 1 character(s)"})
		} else {
			body.APIKey = value
		}
	} else {
		issues = append(issues, invalidParam{Path: []any{"apiKey"}, Code: "invalid_type", Message: "Expected string, received " + jsonTypeName(raw)})
	}

	// model: z.string().trim().optional()
	if raw, present := fields["model"]; present && !isJSONNull(raw) {
		if value, ok := upstreamString(raw); ok {
			body.Model = strings.TrimSpace(value)
		} else {
			issues = append(issues, invalidParam{Path: []any{"model"}, Code: "invalid_type", Message: "Expected string, received " + jsonTypeName(raw)})
		}
	}

	// proxyUrl: z.string().trim().nullable().optional()
	if raw, present := fields["proxyUrl"]; present && !isJSONNull(raw) {
		if value, ok := upstreamString(raw); ok {
			body.ProxyURL = strings.TrimSpace(value)
		} else {
			issues = append(issues, invalidParam{Path: []any{"proxyUrl"}, Code: "invalid_type", Message: "Expected string, received " + jsonTypeName(raw)})
		}
	}

	// proxyFallbackToDirect: z.boolean().optional()
	if raw, present := fields["proxyFallbackToDirect"]; present {
		if value, ok := upstreamBool(raw); ok {
			body.ProxyFallbackToDirect = value
		} else {
			issues = append(issues, invalidParam{Path: []any{"proxyFallbackToDirect"}, Code: "invalid_type", Message: "Expected boolean, received " + jsonTypeName(raw)})
		}
	}

	// timeoutMs: z.number().int().min(5000).max(120000).optional()
	if raw, present := fields["timeoutMs"]; present {
		value, ok := upstreamInt(raw)
		switch {
		case !ok:
			issues = append(issues, invalidParam{Path: []any{"timeoutMs"}, Code: "invalid_type", Message: "Expected number, received " + jsonTypeName(raw)})
		case value < upstreamFetchTimeoutMinMS:
			issues = append(issues, invalidParam{Path: []any{"timeoutMs"}, Code: "too_small", Message: "Number must be greater than or equal to " + strconv.Itoa(upstreamFetchTimeoutMinMS)})
		case value > upstreamFetchTimeoutMaxMS:
			issues = append(issues, invalidParam{Path: []any{"timeoutMs"}, Code: "too_big", Message: "Number must be less than or equal to " + strconv.Itoa(upstreamFetchTimeoutMaxMS)})
		default:
			body.TimeoutMS = time.Duration(value) * time.Millisecond
		}
	}

	// providerType: enum（公开集；兼容头下含隐藏类型）
	if raw, present := fields["providerType"]; !present {
		issues = append(issues, invalidParam{Path: []any{"providerType"}, Code: "invalid_type", Message: "Required"})
	} else if value, ok := upstreamString(raw); ok {
		if !validProviderTypeValue(value) || (hiddenProviderType(value) && !dashboardCompat) {
			issues = append(issues, invalidParam{Path: []any{"providerType"}, Code: "invalid_enum_value", Message: "Invalid enum value"})
		}
	} else {
		issues = append(issues, invalidParam{Path: []any{"providerType"}, Code: "invalid_type", Message: "Expected string, received " + jsonTypeName(raw)})
	}

	return body, issues
}

// providerTypeField 取出已校验过的 providerType 原始值（失败分支已被早退挡住）。
func providerTypeField(fields map[string]json.RawMessage) string {
	value, ok := upstreamString(fields["providerType"])
	if !ok {
		return ""
	}
	return value
}

func upstreamString(raw json.RawMessage) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func upstreamBool(raw json.RawMessage) (bool, bool) {
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}

func upstreamInt(raw json.RawMessage) (int, bool) {
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	if value != float64(int(value)) {
		return 0, false
	}
	return int(value), true
}

// jsonTypeName 复刻 zod 报错里的 received 类型名。
func jsonTypeName(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "":
		return "undefined"
	case trimmed == "null":
		return "null"
	case trimmed == "true" || trimmed == "false":
		return "boolean"
	case strings.HasPrefix(trimmed, "{"):
		return "object"
	case strings.HasPrefix(trimmed, "["):
		return "array"
	case strings.HasPrefix(trimmed, "\""):
		return "string"
	default:
		return "number"
	}
}
