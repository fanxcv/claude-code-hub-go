package adminapi

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/providertest"
)

// providers_test_proxy.go 实现 POST /providers/test:proxy。
//
// 唯一真源：
//   - 路由与校验：src/app/api/v1/resources/providers/router.ts:641-666，
//     `ProviderProxyTestSchema` = `{providerUrl, proxyUrl?, proxyFallbackToDirect?}` **strict**；
//   - 动作：src/actions/providers.ts:3410-3545（testProviderProxy）。
//
// 关键语义：**校验失败也返回 200**。Node 把「URL 格式非法」「代理地址非法」都包成
// `{ok:true, data:{success:false, message, details}}`——即 HTTP 200 + success:false。
// 只有最外层 catch（本实现里无对应路径）才走 action error（Problem 400）。
//
// 响应形状（成功路径全为 200，正文即 data）：
//
//	成功   {"success":true,"message":"成功连接到 <host>","details":{"statusCode":…,"responseTime":…,"usedProxy":…,"proxyUrl":…}}
//	失败   {"success":false,"message":…,"details":{…,"error":…,"errorType":…}}
//
// 注意 `details` 里的 `proxyUrl` 在 Node 侧**仅在代理被使用时**出现（对象字面量里 `proxyUrl: proxyConfig?.proxyUrl`）；
// Go 用 `omitempty` 对齐「不用代理则无该键」。
func handleTestProviderProxy(deps Deps) http.HandlerFunc {
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

		providerURL, proxyURL, fallback, issues := decodeProxyTestBody(fields)
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		// URL 格式校验失败：200 + success:false（不是 400）。
		if _, message, ok := providertest.ValidateProviderURLForConnectivity(providerURL); !ok {
			adminWriteJSON(writer, http.StatusOK, proxyTestPayload{
				Success: false,
				Message: message,
				Details: &proxyTestDetails{
					Error:     "仅支持 HTTP 和 HTTPS 协议",
					ErrorType: "InvalidProviderUrl",
				},
			})
			return
		}
		if proxyURL != "" && !providertest.IsValidProxyURL(proxyURL) {
			adminWriteJSON(writer, http.StatusOK, proxyTestPayload{
				Success: false,
				Message: "代理地址格式无效",
				Details: &proxyTestDetails{
					Error:     "支持格式: http://, https://, socks5://, socks4://",
					ErrorType: "InvalidProxyUrl",
				},
			})
			return
		}

		proxyConfig := providertest.ProxyConfig{URL: proxyURL, FallbackToDirect: fallback}
		client, err := providertest.NewUpstreamClient(proxyConfig, providertest.ApiTestTimeout())
		if err != nil {
			// 代理协议不支持（本包只做 http/https/socks5）：按「代理地址格式无效」同形返回，
			// 不静默直连——否则「测试连通性」会给出误导性的成功。
			adminWriteJSON(writer, http.StatusOK, proxyTestPayload{
				Success: false,
				Message: "代理地址格式无效",
				Details: &proxyTestDetails{
					Error:     err.Error(),
					ErrorType: "InvalidProxyUrl",
				},
			})
			return
		}

		result := providertest.TestProxyConnectivity(
			request.Context(), client, strings.TrimSpace(providerURL), proxyConfig, providertest.ApiTestTimeout(),
		)
		adminWriteJSON(writer, http.StatusOK, proxyTestPayloadFromResult(result))
	}
}

// proxyTestPayload 复刻 testProviderProxy 的 data 形状。
type proxyTestPayload struct {
	Success bool              `json:"success"`
	Message string            `json:"message"`
	Details *proxyTestDetails `json:"details,omitempty"`
}

type proxyTestDetails struct {
	StatusCode     *int   `json:"statusCode,omitempty"`
	ResponseTimeMS *int64 `json:"responseTime,omitempty"`
	UsedProxy      *bool  `json:"usedProxy,omitempty"`
	ProxyURL       string `json:"proxyUrl,omitempty"`
	Error          string `json:"error,omitempty"`
	ErrorType      string `json:"errorType,omitempty"`
}

func proxyTestPayloadFromResult(result providertest.ProxyTestResult) proxyTestPayload {
	usedProxy := result.UsedProxy
	responseTime := result.ResponseTimeMS
	if result.Success {
		return proxyTestPayload{
			Success: true,
			Message: result.Message,
			Details: &proxyTestDetails{
				StatusCode:     result.StatusCode,
				ResponseTimeMS: &responseTime,
				UsedProxy:      &usedProxy,
				ProxyURL:       result.ProxyURL,
			},
		}
	}
	return proxyTestPayload{
		Success: false,
		Message: result.Message,
		Details: &proxyTestDetails{
			ResponseTimeMS: &responseTime,
			UsedProxy:      &usedProxy,
			ProxyURL:       result.ProxyURL,
			Error:          result.Error,
			ErrorType:      result.ErrorType,
		},
	}
}

// decodeProxyTestBody 复刻 ProviderProxyTestSchema（strict）的校验。
func decodeProxyTestBody(fields map[string]json.RawMessage) (string, string, bool, []invalidParam) {
	allowed := map[string]bool{"providerUrl": true, "proxyUrl": true, "proxyFallbackToDirect": true}
	issues := make([]invalidParam, 0, 1)
	unknown := make([]string, 0, 1)
	for name := range fields {
		if !allowed[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		issues = append(issues, invalidParam{
			Path: []any{name}, Code: "unrecognized_keys", Message: "Unrecognized key: " + name,
		})
	}

	var providerURL, proxyURL string
	fallback := false

	if raw, present := fields["providerUrl"]; !present {
		issues = append(issues, invalidParam{Path: []any{"providerUrl"}, Code: "invalid_type", Message: "Required"})
	} else if value, ok := upstreamString(raw); ok {
		if !providerIsAbsoluteURL(strings.TrimSpace(value)) {
			issues = append(issues, invalidParam{Path: []any{"providerUrl"}, Code: "invalid_format", Message: "Invalid URL"})
		} else {
			providerURL = strings.TrimSpace(value)
		}
	} else {
		issues = append(issues, invalidParam{Path: []any{"providerUrl"}, Code: "invalid_type", Message: "Expected string, received " + jsonTypeName(raw)})
	}

	if raw, present := fields["proxyUrl"]; present && !isJSONNull(raw) {
		if value, ok := upstreamString(raw); ok {
			proxyURL = strings.TrimSpace(value)
		} else {
			issues = append(issues, invalidParam{Path: []any{"proxyUrl"}, Code: "invalid_type", Message: "Expected string, received " + jsonTypeName(raw)})
		}
	}

	if raw, present := fields["proxyFallbackToDirect"]; present {
		if value, ok := upstreamBool(raw); ok {
			fallback = value
		} else {
			issues = append(issues, invalidParam{Path: []any{"proxyFallbackToDirect"}, Code: "invalid_type", Message: "Expected boolean, received " + jsonTypeName(raw)})
		}
	}

	return providerURL, proxyURL, fallback, issues
}
