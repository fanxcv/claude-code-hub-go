package guard

import (
	"regexp"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 本文件复刻 src/app/v1/_lib/proxy/client-detector.ts 与 ProxyClientGuard。
//
// 客户端限制是「用户配了才生效」的软开关：没配白名单与黑名单时完全跳过；只配黑名单时
// 缺 UA 也放行（没有 UA 匹配不上任何黑名单模式）；配了白名单则 UA 必填。

// ClaudeCodeKeywordPrefix 是内置关键字前缀，与 TS 侧同名常量一致。
const ClaudeCodeKeywordPrefix = "claude-code"

// builtinClientKeywords 与 TS 侧 BUILTIN_CLIENT_KEYWORDS 逐字一致。
var builtinClientKeywords = map[string]bool{
	"claude-code":           true,
	"claude-code-cli":       true,
	"claude-code-cli-sdk":   true,
	"claude-code-vscode":    true,
	"claude-code-sdk-ts":    true,
	"claude-code-sdk-py":    true,
	"claude-code-gh-action": true,
}

// codexFamilyRule 对应 TS 侧 CODEX_FAMILY_RULES 的一条。
type codexFamilyRule struct {
	pattern     *regexp.Regexp
	matchValues map[string]bool
}

// codexFamilyRules 的匹配值全为小写——TS 侧在非生产环境有断言，Go 侧由单测覆盖。
var codexFamilyRules = []codexFamilyRule{
	{regexp.MustCompile(`(?i)^codex desktop\b`), map[string]bool{"codex-cli": true, "codex desktop": true}},
	{regexp.MustCompile(`(?i)^codex[_-]?tui\b`), map[string]bool{"codex-cli": true, "codex_cli_core": true}},
	{regexp.MustCompile(`(?i)^codex[_-]?cli[_-]?rs\b`), map[string]bool{"codex-cli": true, "codex_cli_core": true}},
	{regexp.MustCompile(`(?i)^codex[_-]?exec\b`), map[string]bool{"codex-cli": true, "codex_exec": true}},
	{regexp.MustCompile(`(?i)^codex[_-]?vscode\b`), map[string]bool{"codex-cli": true, "codex_vscode": true}},
}

// cliEntrypointMap 把 Claude CLI 的 entrypoint 映射到子客户端关键字。
var cliEntrypointMap = map[string]string{
	"cli":                       "claude-code-cli",
	"sdk-cli":                   "claude-code-cli-sdk",
	"claude-vscode":             "claude-code-vscode",
	"sdk-ts":                    "claude-code-sdk-ts",
	"sdk-py":                    "claude-code-sdk-py",
	"claude-code-github-action": "claude-code-gh-action",
}

// cliEntrypointPattern 对齐 TS 侧 /^claude-cli\/\S+\s+\(external,\s*([^,)]+)/i。
var cliEntrypointPattern = regexp.MustCompile(`(?i)^claude-cli/\S+\s+\(external,\s*([^,)]+)`)

// clientSignals 是 Claude Code 信号确认的结果。
type clientSignals struct {
	confirmed     bool
	signals       []string
	supplementary []string
}

// normalizeClientText 复刻 TS 侧 normalize：小写化并去掉连字符与下划线。
func normalizeClientText(value string) string {
	replaced := strings.NewReplacer("-", "", "_", "").Replace(value)
	return strings.ToLower(replaced)
}

// matchesCodexFamilyAlias 复刻 TS 侧同名函数。
func matchesCodexFamilyAlias(pattern, userAgent string) bool {
	normalizedPattern := strings.ToLower(strings.TrimSpace(pattern))
	for _, rule := range codexFamilyRules {
		if rule.pattern.MatchString(userAgent) {
			return rule.matchValues[normalizedPattern]
		}
	}
	return false
}

// globMatch 复刻 TS 侧同名函数（单星号通配，回溯实现，行为与 TS 一致）。
func globMatch(pattern, text string) bool {
	lowerPattern := strings.ToLower(pattern)
	lowerText := strings.ToLower(text)
	patternIndex := 0
	textIndex := 0
	starPatternIndex := -1
	starTextIndex := -1
	for textIndex < len(lowerText) {
		switch {
		case patternIndex < len(lowerPattern) && lowerPattern[patternIndex] == lowerText[textIndex]:
			patternIndex++
			textIndex++
		case patternIndex < len(lowerPattern) && lowerPattern[patternIndex] == '*':
			starPatternIndex = patternIndex
			starTextIndex = textIndex
			patternIndex++
		case starPatternIndex >= 0:
			patternIndex = starPatternIndex + 1
			starTextIndex++
			textIndex = starTextIndex
		default:
			return false
		}
	}
	for patternIndex < len(lowerPattern) && lowerPattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(lowerPattern)
}

// isBuiltinClientKeyword 对应 TS 侧 isBuiltinKeyword。
func isBuiltinClientKeyword(pattern string) bool {
	return builtinClientKeywords[pattern]
}

// confirmClientSignals 复刻 confirmClaudeCodeSignals。
//
// 判定用三条头部信号（x-app: cli、UA 前缀 claude-cli/、anthropic-beta），metadata.user_id
// 在非 count_tokens 请求上必须同时存在。count_tokens 请求刻意不带 metadata.user_id，因此
// 只用三条头部信号确认——否则限制会对这类请求误拒。
func (d Deps) confirmClientSignals(ctx *pctx.Context) clientSignals {
	headers := ctx.Headers()
	result := clientSignals{}

	if headers.Get("x-app") == "cli" {
		result.signals = append(result.signals, "x-app-cli")
	}
	if strings.HasPrefix(strings.ToLower(userAgent(ctx)), "claude-cli/") {
		result.signals = append(result.signals, "ua-prefix")
	}
	if headers.Has("anthropic-beta") {
		result.signals = append(result.signals, "betas-present")
	}
	if metadataUserID(ctx, d) {
		result.signals = append(result.signals, "metadata-user-id")
	}
	if headers.Get("anthropic-dangerous-direct-browser-access") == "true" {
		result.supplementary = append(result.supplementary, "dangerous-browser-access")
	}

	hasHeaderSignals := containsSignal(result.signals, "x-app-cli") &&
		containsSignal(result.signals, "ua-prefix") &&
		containsSignal(result.signals, "betas-present")

	if isCountTokensPath(ctx.Path()) {
		result.confirmed = hasHeaderSignals
		return result
	}
	result.confirmed = containsSignal(result.signals, "metadata-user-id") && hasHeaderSignals
	return result
}

// metadataUserID 判定正文 metadata.user_id 是否为字符串。
func metadataUserID(ctx *pctx.Context, d Deps) bool {
	body, err := d.body(ctx)
	if err != nil {
		return false
	}
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = metadata["user_id"].(string)
	return ok
}

// isCountTokensPath 复刻 isCountTokensEndpointPath（去查询串、去尾斜杠、小写化）。
func isCountTokensPath(path string) bool {
	trimmed := path
	if index := strings.IndexByte(trimmed, '?'); index >= 0 {
		trimmed = trimmed[:index]
	}
	if len(trimmed) > 1 && strings.HasSuffix(trimmed, "/") {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return strings.ToLower(trimmed) == "/v1/messages/count_tokens"
}

// extractSubClient 复刻 extractSubClient。
func extractSubClient(agent string) string {
	match := cliEntrypointPattern.FindStringSubmatch(agent)
	if match == nil {
		return ""
	}
	entrypoint := strings.TrimSpace(match[1])
	return cliEntrypointMap[entrypoint]
}

// matchClientPattern 复刻 matchClientPattern。
func (d Deps) matchClientPattern(ctx *pctx.Context, pattern string, signals clientSignals) bool {
	if !isBuiltinClientKeyword(pattern) {
		agent := strings.TrimSpace(userAgent(ctx))
		if agent == "" {
			return false
		}
		if matchesCodexFamilyAlias(pattern, agent) {
			return true
		}
		if strings.Contains(pattern, "*") {
			return globMatch(pattern, agent)
		}
		normalizedPattern := normalizeClientText(pattern)
		if normalizedPattern == "" {
			return false
		}
		return strings.Contains(normalizeClientText(agent), normalizedPattern)
	}

	if !signals.confirmed {
		return false
	}
	if pattern == ClaudeCodeKeywordPrefix {
		return true
	}
	return extractSubClient(userAgent(ctx)) == pattern
}

// clientStep 复刻 ProxyClientGuard.ensure。
func (d Deps) clientStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		user, ok, err := d.currentUser(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}

		allowed := user.AllowedClients
		blocked := user.BlockedClients
		if len(allowed) == 0 && len(blocked) == 0 {
			return nil, nil
		}

		agent := strings.TrimSpace(userAgent(ctx))
		if agent == "" && len(allowed) > 0 {
			return BuildError(
				400,
				"Client not allowed. User-Agent header is required when client restrictions are configured.",
				"invalid_request_error",
			), nil
		}

		signals := d.confirmClientSignals(ctx)
		subClient := ""
		if signals.confirmed {
			subClient = extractSubClient(userAgent(ctx))
		}
		detected := subClient
		if detected == "" {
			detected = agent
		}
		detectedSuffix := ""
		if detected != "" {
			detectedSuffix = " (detected: " + detected + ")"
		}

		for _, pattern := range blocked {
			if d.matchClientPattern(ctx, pattern, signals) {
				return BuildError(
					400,
					"Client blocked"+detectedSuffix,
					"invalid_request_error",
				), nil
			}
		}

		if len(allowed) > 0 {
			for _, pattern := range allowed {
				if d.matchClientPattern(ctx, pattern, signals) {
					return nil, nil
				}
			}
			return BuildError(
				400,
				"Client not allowed"+detectedSuffix,
				"invalid_request_error",
			), nil
		}

		return nil, nil
	}
}

// containsSignal 报告信号名是否出现过。
func containsSignal(signals []string, wanted string) bool {
	for _, signal := range signals {
		if signal == wanted {
			return true
		}
	}
	return false
}

// ProviderClientRestriction 复刻 `isClientAllowedDetailed` 在**供应商名单**上的用法
// （Node 的 pickRandomProvider Step 1，provider-selector.ts:1266-1279）。
//
// 与密钥级 clientStep 的区别只在出口形态：那里是「拒请求 + 400 文案」，这里是「给选路一个
// 可留痕的判定结果」（reason `client_restriction`，details 取 matchType，并带
// clientRestrictionContext）。匹配语义与信号确认**复用同一套**函数，不另写一份。
//
// 调用约定：两侧名单都空时 Node 直接放行且不做判定，调用方应在此之前跳过（返回 nil）。
func (d Deps) ProviderClientRestriction(
	ctx *pctx.Context,
	allowedClients []string,
	blockedClients []string,
) *route.ClientRestriction {
	signals := d.confirmClientSignals(ctx)
	agent := strings.TrimSpace(userAgent(ctx))
	subClient := ""
	if signals.confirmed {
		subClient = extractSubClient(agent)
	}
	// Node：detectedClient = subClient || ua || undefined。
	detected := subClient
	if detected == "" {
		detected = agent
	}

	result := &route.ClientRestriction{
		Allowed:          true,
		MatchType:        "allowed",
		DetectedClient:   detected,
		CheckedAllowlist: nonNilPatterns(allowedClients),
		CheckedBlocklist: nonNilPatterns(blockedClients),
	}

	// 先黑名单：命中即拒（Node 的 blockedClients.find(matches)，按名单顺序取第一条）。
	if pattern, hit := firstMatchingPattern(blockedClients, d, ctx, signals); hit {
		result.Allowed = false
		result.MatchType = route.MatchTypeBlocklistHit
		result.MatchedPattern = pattern
		return result
	}

	// 无白名单 → 放行（Node：allowedClients.length === 0 返回 allowed=true）。
	if len(allowedClients) == 0 {
		return result
	}

	if pattern, hit := firstMatchingPattern(allowedClients, d, ctx, signals); hit {
		result.MatchedPattern = pattern
		return result
	}

	result.Allowed = false
	result.MatchType = route.MatchTypeAllowlistMiss
	return result
}

// firstMatchingPattern 按名单顺序找第一条命中的模式。
func firstMatchingPattern(
	patterns []string,
	d Deps,
	ctx *pctx.Context,
	signals clientSignals,
) (string, bool) {
	for _, pattern := range patterns {
		if d.matchClientPattern(ctx, pattern, signals) {
			return pattern, true
		}
	}
	return "", false
}

// nonNilPatterns 把 nil 名单规整为**非 nil 空切片**：落链时 `[]` 与缺失在 JSON 里不同形，
// Node 侧这两项恒为数组（clientRestrictionContext.providerAllowlist/providerBlocklist）。
func nonNilPatterns(patterns []string) []string {
	if patterns == nil {
		return []string{}
	}
	return patterns
}
