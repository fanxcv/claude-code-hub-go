package forward

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// sharedLogger 是本包在未注入 Logger 时使用的兜底日志器（写 stderr）。
var sharedLogger = sync.OnceValue(func() *logx.Logger { return logx.New(nil) })

// ProtectedAuthHeaderNames 是绝不允许由供应商自定义头覆盖的鉴权头，对齐
// src/lib/custom-headers.ts 的 PROTECTED_AUTH_HEADER_NAMES。
var ProtectedAuthHeaderNames = map[string]bool{
	"authorization":  true,
	"x-api-key":      true,
	"x-goog-api-key": true,
}

// ReservedInternalHeaderNames 是本进程内部标记头，对齐 responses-ws/internal-secret.ts
// 的 RESERVED_INTERNAL_HEADERS：入口已剥离，出站也不允许供应商自定义头重新注入。
var ReservedInternalHeaderNames = []string{
	"x-cch-client-transport",
	"x-cch-responses-ws-forward",
	"x-cch-responses-ws-session",
	"x-cch-internal-secret",
}

// outboundTransportHeaderBlacklist 是出站黑名单，对齐 forwarder.ts 的
// OUTBOUND_TRANSPORT_HEADER_BLACKLIST。这些头由传输层自己决定，客户端值不得透传。
var outboundTransportHeaderBlacklist = []string{
	"content-length",
	"connection",
	"transfer-encoding",
}

// clientIPHeaderNames 是为保留客户端 IP 而豁免黑名单的头（对齐 HeaderProcessor 的 clientIpHeaders）。
var clientIPHeaderNames = []string{
	"x-forwarded-for",
	"x-real-ip",
	"x-client-ip",
	"x-originating-ip",
	"x-remote-ip",
	"x-remote-addr",
}

// defaultHeaderBlacklist 是 HeaderProcessor 的默认黑名单：客户端 IP 链、转发信息、CDN 与追踪头。
var defaultHeaderBlacklist = []string{
	"x-forwarded-for",
	"x-real-ip",
	"x-client-ip",
	"x-originating-ip",
	"x-remote-ip",
	"x-remote-addr",
	"x-forwarded-host",
	"x-forwarded-port",
	"x-forwarded-proto",
	"forwarded",
	"cf-connecting-ip",
	"cf-ipcountry",
	"cf-ray",
	"cf-visitor",
	"true-client-ip",
	"x-cluster-client-ip",
	"fastly-client-ip",
	"x-azure-clientip",
	"x-azure-fdid",
	"x-azure-ref",
	"akamai-origin-hop",
	"x-akamai-config-log-detail",
	"x-request-id",
	"x-correlation-id",
	"x-trace-id",
	"x-amzn-trace-id",
	"x-b3-traceid",
	"x-b3-spanid",
	"x-b3-parentspanid",
	"x-b3-sampled",
	"traceparent",
	"tracestate",
}

// HeaderInput 是出站 headers 构造的输入。
type HeaderInput struct {
	// ClientHeaders 是客户端入口 headers（原始快照）。
	ClientHeaders http.Header
	// Provider 提供鉴权头、自定义头与客户端 IP 保留开关。
	Provider Provider
	// BaseURL 是本次尝试的上游基址，用于生成 host 覆盖。
	BaseURL string
	// CacheTTL1h 为真时补齐 anthropic-beta 的 1h 缓存标记。
	CacheTTL1h bool
	// ClientUserAgent 是客户端原始 User-Agent。
	ClientUserAgent string
	// FilteredUserAgent 是请求过滤器处理后的 User-Agent。
	FilteredUserAgent string
	// UserAgentModified 为真表示过滤器改写或删除了 User-Agent（codex 的取值分支出处）。
	UserAgentModified bool
}

// defaultCodexUserAgent 对齐 Node 侧 DEFAULT_CODEX_USER_AGENT。
const defaultCodexUserAgent = "codex-cli/0.0.0"

// BuildUpstreamHeaders 构造发往上游的 headers，覆盖顺序与 forwarder.ts 的 buildHeaders 逐条一致：
// host/content-type/accept-encoding → 供应商自定义头 → 鉴权头 → codex UA → 客户端 IP → 缓存 beta。
func BuildUpstreamHeaders(in HeaderInput) http.Header {
	overrides := map[string]string{
		"host":            extractHost(in.BaseURL),
		"content-type":    "application/json",
		"accept-encoding": "identity",
	}

	applyProviderCustomHeaders(overrides, in.Provider.CustomHeaders)

	providerType := in.Provider.Type
	switch providerType {
	case convert.ProviderClaude, convert.ProviderClaudeAuth:
		for name, value := range ResolveAnthropicAuthHeaders(
			in.Provider.Key, in.BaseURL, providerType == convert.ProviderClaudeAuth,
		) {
			overrides[name] = value
		}
	case convert.ProviderCodex, convert.ProviderOpenAICompatible:
		overrides["authorization"] = "Bearer " + in.Provider.Key
	case convert.ProviderGemini, convert.ProviderGeminiCLI:
		// Node 的 buildGeminiHeaders：API Key 走 x-goog-api-key，OAuth 令牌走 Authorization: Bearer；
		// gemini-cli 另带一个写死的客户端标识。
		token, isAPIKey := geminiCredential(in.Provider.Key)
		if isAPIKey {
			overrides[geminiAPIKeyHeader] = token
		} else {
			overrides["authorization"] = "Bearer " + token
		}
		if providerType == convert.ProviderGeminiCLI {
			overrides[geminiCLIClientHeader] = geminiCLIClientValue
		}
	}

	if providerType == convert.ProviderGemini || providerType == convert.ProviderGeminiCLI {
		// Node 在这里恒写 user-agent（客户端没带就落到 claude-code-hub），而其它线是透传语义。
		resolved := in.ClientUserAgent
		if in.UserAgentModified {
			resolved = in.FilteredUserAgent
		}
		if resolved == "" {
			resolved = geminiDefaultUserAgent
		}
		overrides["user-agent"] = resolved
	}

	if providerType == convert.ProviderCodex {
		resolved := in.ClientUserAgent
		if in.UserAgentModified {
			resolved = in.FilteredUserAgent
			if resolved == "" {
				resolved = in.ClientUserAgent
			}
		}
		if resolved == "" {
			resolved = defaultCodexUserAgent
		}
		overrides["user-agent"] = resolved
	}

	if in.CacheTTL1h {
		overrides["anthropic-beta"] = mergeAnthropicCacheTTLBetaFlag(in.ClientHeaders.Get("anthropic-beta"))
	}

	blacklist := make([]string, 0, len(outboundTransportHeaderBlacklist)+len(ReservedInternalHeaderNames)+2)
	blacklist = append(blacklist, outboundTransportHeaderBlacklist...)
	blacklist = append(blacklist, ReservedInternalHeaderNames...)
	// 客户端自带的两种代理凭据都不得透传：x-api-key 是三线通用，x-goog-api-key 是 Gemini 形态。
	blacklist = append(blacklist, "x-api-key", geminiAPIKeyHeader)

	out := processHeaders(in.ClientHeaders, headerProcessorConfig{
		blacklist:               blacklist,
		overrides:               overrides,
		preserveClientIPHeaders: in.Provider.PreserveClientIP,
	})

	// 客户端 IP 头：仅当 provider.preserveClientIp 为真时按客户端原始头重新注入。
	dial.ApplyClientIPHeaders(out, dial.IPHeaders{
		PreserveClientIp: in.Provider.PreserveClientIP,
		Headers:          in.ClientHeaders,
	})
	return out
}

// applyProviderCustomHeaders 把供应商自定义头合并进 overrides。
//
// 鉴权头与「保留名」（host 与传输层黑名单）一律剥离：HeaderProcessor 的覆盖发生在黑名单
// 过滤之后，任何进入 overrides 的名字都绕过了过滤，历史脏数据可借此改写 Host 或请求分帧。
func applyProviderCustomHeaders(overrides map[string]string, custom map[string]string) {
	for name, value := range custom {
		lower := strings.ToLower(name)
		if ProtectedAuthHeaderNames[lower] {
			continue
		}
		if lower == "host" || contains(outboundTransportHeaderBlacklist, lower) || contains(ReservedInternalHeaderNames, lower) {
			continue
		}
		overrides[name] = value
	}
}

type headerProcessorConfig struct {
	blacklist               []string
	overrides               map[string]string
	preserveClientIPHeaders bool
}

// processHeaders 复刻 HeaderProcessor.process：先按黑名单过滤客户端头，再整体套用覆盖。
func processHeaders(client http.Header, cfg headerProcessorConfig) http.Header {
	blacklist := make(map[string]bool, len(defaultHeaderBlacklist)+len(cfg.blacklist))
	for _, name := range defaultHeaderBlacklist {
		if cfg.preserveClientIPHeaders && contains(clientIPHeaderNames, name) {
			continue
		}
		blacklist[name] = true
	}
	// preserveAuthorization 恒为 false：客户端鉴权头绝不透传上游。
	blacklist["authorization"] = true
	for _, name := range cfg.blacklist {
		blacklist[strings.ToLower(name)] = true
	}

	out := make(http.Header, len(client)+len(cfg.overrides))
	for name, values := range client {
		if blacklist[strings.ToLower(name)] {
			continue
		}
		out[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	for name, value := range cfg.overrides {
		out.Set(name, value)
	}
	return out
}

// ResolveAnthropicAuthHeaders 解析 claude / claude-auth 供应商的鉴权头，对齐
// src/app/v1/_lib/headers.ts 的同名函数。
//
// AWS External Anthropic 网关既拒绝 Bearer 也拒绝两者共存，故先于一切判定，只发 x-api-key。
func ResolveAnthropicAuthHeaders(apiKey, providerURL string, forceBearerOnly bool) map[string]string {
	if isAwsExternalAnthropicURL(providerURL) {
		if forceBearerOnly {
			sharedLogger().Warn("forward: AWS External Anthropic 网关忽略 forceBearerOnly，只发 x-api-key", map[string]any{
				"provider_host": safeHost(providerURL),
			})
		}
		return map[string]string{"x-api-key": apiKey}
	}
	if forceBearerOnly || looksLikeAnthropicProxyURL(providerURL) {
		return map[string]string{"authorization": "Bearer " + apiKey}
	}
	return map[string]string{
		"authorization": "Bearer " + apiKey,
		"x-api-key":     apiKey,
	}
}

var (
	anthropicProxyHostPattern   = regexp.MustCompile(`(?:^|[.-])(proxy|relay|gateway|router|worker|openrouter|api2d|oaipro)(?:[.-]|$)`)
	awsExternalAnthropicPattern = regexp.MustCompile(`^aws-external-anthropic\.[a-z]+(?:-[a-z]+)+-\d+\.api\.aws$`)
	hostFromURLFallbackPattern  = regexp.MustCompile(`^https?://([^/]+)`)
)

// looksLikeAnthropicProxyURL 对齐 Node 侧同名判定：官方域名不算代理，其余按 relay 标识匹配。
func looksLikeAnthropicProxyURL(providerURL string) bool {
	if providerURL == "" {
		return false
	}
	hostname := strings.ToLower(hostOf(providerURL))
	if hostname == "" {
		return false
	}
	if hostname == "anthropic.com" || strings.HasSuffix(hostname, ".anthropic.com") ||
		hostname == "claude.ai" || strings.HasSuffix(hostname, ".claude.ai") {
		return false
	}
	return anthropicProxyHostPattern.MatchString(hostname)
}

// isAwsExternalAnthropicURL 对齐 Node 侧同名判定；区域段限定为 AWS 实际命名，防伪造主机。
func isAwsExternalAnthropicURL(providerURL string) bool {
	if providerURL == "" {
		return false
	}
	hostname := strings.ToLower(hostOf(providerURL))
	return hostname != "" && awsExternalAnthropicPattern.MatchString(hostname)
}

// mergeAnthropicCacheTTLBetaFlag 按 Node 语义合并 anthropic-beta：
// 先按逗号拆分去重，再无条件补齐 extended-cache-ttl 与其依赖 prompt-caching。
func mergeAnthropicCacheTTLBetaFlag(existing string) string {
	flags := make([]string, 0, 4)
	seen := map[string]bool{}
	for _, part := range strings.Split(existing, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		flags = append(flags, trimmed)
	}
	for _, required := range []string{"extended-cache-ttl-2025-04-11", "prompt-caching-2024-07-31"} {
		if seen[required] {
			continue
		}
		seen[required] = true
		flags = append(flags, required)
	}
	return strings.Join(flags, ", ")
}

// extractHost 从基址取 host，对齐 HeaderProcessor.extractHost 的兜底行为。
func extractHost(baseURL string) string {
	if host := hostOf(baseURL); host != "" {
		return host
	}
	if match := hostFromURLFallbackPattern.FindStringSubmatch(baseURL); match != nil {
		return match[1]
	}
	return "localhost"
}

func hostOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}

func safeHost(rawURL string) string {
	if host := hostOf(rawURL); host != "" {
		return host
	}
	return "unknown"
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
