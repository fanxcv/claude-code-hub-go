package guard

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// AuthFailureKind 是认证失败的性质，决定是否计入防爆破节流。
//
// 取值与 Node 侧 session.ts 的 AuthFailureKind 逐字一致。
type AuthFailureKind string

const (
	// FailureCredentials 表示凭据本身有问题（缺失、冲突、密钥不存在）。计入节流。
	FailureCredentials AuthFailureKind = "credentials"
	// FailureAccountState 表示凭据指向的记录真实存在，但账户或密钥状态不允许使用。不计入节流。
	FailureAccountState AuthFailureKind = "account_state"
)

// bearerPattern 对齐 Node 侧 /^Bearer\s+(.+)$/i。
var bearerPattern = regexp.MustCompile(`(?i)^Bearer\s+(.+)$`)

// authStep 复刻 ProxyAuthenticator.ensure。
//
// 步骤顺序是语义：先按 IP 做防爆破节流（钥匙还没验就打回，避免爆破打库），再解析凭据，
// 成功则重置节流计数，失败按性质决定是否计数。最后才写回鉴权结果。
func (d Deps) authStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		clientIP := d.resolveClientIP(ctx)
		if clientIP == "unknown" {
			clientIP = ""
		}
		ctx.SetClientIP(clientIP)

		headers := ctx.Headers()
		candidates := keyCandidates{
			Authorization: headers.Get("authorization"),
			APIKey:        headers.Get("x-api-key"),
			GeminiHeader:  headers.Get("x-goog-api-key"),
			GeminiQuery:   d.queryAPIKey(ctx),
		}

		candidateKey := candidates.unanimous()

		if d.AuthThrottle != nil {
			decision, err := d.AuthThrottle.Throttle(d.runContext(ctx), clientIP, candidateKey)
			if err != nil {
				return nil, err
			}
			if !decision.Allowed {
				response := BuildError(
					429,
					"Too many authentication failures. Please retry later.",
					"rate_limit_error",
				)
				if decision.RetryAfterSeconds != nil {
					response = response.WithHeader("Retry-After", fmt.Sprintf("%d", *decision.RetryAfterSeconds))
				}
				return response, nil
			}
		}

		outcome, outcomeErr := d.validate(ctx, candidates)
		if outcomeErr != nil {
			// 解析器自身故障（数据库不可达等）：不当作认证失败计数，也不谎报 401，
			// 更不能 fail-open 放行未鉴权请求。
			return nil, outcomeErr
		}

		if outcome.success {
			if d.AuthThrottle != nil {
				d.AuthThrottle.RecordAuthSuccess(d.runContext(ctx), clientIP, outcome.apiKey)
			}
			ctx.SetAuth(outcome.state)
			return nil, nil
		}

		// 只有凭据类失败才喂给防爆破节流：账户状态类失败匹配到了真实记录，计数会让管理员
		// 停用一个密钥就把密钥主的 IP 关进小黑屋。
		if outcome.failureKind != FailureAccountState && d.AuthThrottle != nil {
			recorded := outcome.apiKey
			if recorded == "" {
				recorded = candidateKey
			}
			d.AuthThrottle.RecordAuthFailure(d.runContext(ctx), clientIP, recorded)
		}

		return outcome.response, nil
	}
}

// keyCandidates 是一次请求里出现的全部凭据来源。
type keyCandidates struct {
	Authorization string
	APIKey        string
	GeminiHeader  string
	GeminiQuery   string
}

// provided 返回去空后的凭据列表，顺序与 Node 侧一致（Bearer、x-api-key、x-goog-api-key）。
func (c keyCandidates) provided() []string {
	keys := make([]string, 0, 3)
	if key := extractBearerKey(c.Authorization); key != "" {
		keys = append(keys, key)
	}
	if key := normalizeKey(c.APIKey); key != "" {
		keys = append(keys, key)
	}
	gemini := normalizeKey(c.GeminiHeader)
	if gemini == "" {
		gemini = normalizeKey(c.GeminiQuery)
	}
	if gemini != "" {
		keys = append(keys, gemini)
	}
	return keys
}

// unanimous 返回「多来源一致」时的单一凭据，否则返回空串。
//
// 节流键用候选凭据：多来源冲突时无法确定攻击者意图，此时一律返回空串，让节流退化为
// 纯按 IP 计数（与 Node 的 resolvePreAuthCandidateKey 一致）。
func (c keyCandidates) unanimous() string {
	keys := c.provided()
	if len(keys) == 0 {
		return ""
	}
	first := keys[0]
	for _, key := range keys[1:] {
		if key != first {
			return ""
		}
	}
	return first
}

// authOutcome 是一次认证判定的结果。
type authOutcome struct {
	success     bool
	failureKind AuthFailureKind
	state       pctx.AuthState
	apiKey      string
	response    *Response
}

// validate 复刻 ProxyAuthenticator.validate。
func (d Deps) validate(ctx *pctx.Context, candidates keyCandidates) (authOutcome, error) {
	keys := candidates.provided()

	if len(keys) == 0 {
		return authOutcome{
			failureKind: FailureCredentials,
			response: BuildError(
				401,
				"未提供认证凭据。请在 Authorization 头部、x-api-key 头部或 x-goog-api-key 头部中包含 API 密钥。",
				"authentication_error",
			),
		}, nil
	}

	first := keys[0]
	for _, key := range keys[1:] {
		if key != first {
			d.logger().Warn("guard.auth.conflicting_keys", map[string]any{"keyCount": len(keys)})
			return authOutcome{
				failureKind: FailureCredentials,
				response: BuildError(
					401,
					"提供了多个冲突的 API 密钥。请仅使用一种认证方式。",
					"authentication_error",
				),
			}, nil
		}
	}

	apiKey := first
	locale := d.locale(ctx)

	if d.Auth == nil {
		return authOutcome{}, errors.New("guard: 认证缝隙未接线，无法校验凭据")
	}

	resolution, err := d.Auth.ResolveAPIKey(d.runContext(ctx), apiKey)
	if err != nil {
		switch {
		case errors.Is(err, ErrKeyNotFound):
			d.logger().Debug("guard.auth.key_not_found", map[string]any{"apiKeyLength": len(apiKey)})
			return authOutcome{
				apiKey:      apiKey,
				failureKind: FailureCredentials,
				response: BuildError(
					401,
					Message(locale, MessageInvalidAPIKey),
					"invalid_api_key",
				),
			}, nil
		case errors.Is(err, ErrKeyDisabled):
			d.logger().Warn("guard.auth.key_disabled", map[string]any{"apiKeyLength": len(apiKey)})
			return authOutcome{
				apiKey:      apiKey,
				failureKind: FailureAccountState,
				response: BuildError(
					401,
					Message(locale, MessageAPIKeyDisabled),
					"key_disabled",
				),
			}, nil
		case errors.Is(err, ErrKeyExpired):
			d.logger().Warn("guard.auth.key_expired", map[string]any{"apiKeyLength": len(apiKey)})
			return authOutcome{
				apiKey:      apiKey,
				failureKind: FailureAccountState,
				response: BuildError(
					401,
					Message(locale, MessageAPIKeyExpired),
					"key_expired",
				),
			}, nil
		default:
			return authOutcome{}, fmt.Errorf("guard: API 密钥解析失败: %w", err)
		}
	}

	user := resolution.User
	if !user.IsEnabled {
		d.logger().Warn("guard.auth.user_disabled", map[string]any{"userId": user.ID})
		return authOutcome{
			apiKey:      apiKey,
			failureKind: FailureAccountState,
			response: BuildError(
				401,
				"用户账户已被禁用。请联系管理员。",
				"user_disabled",
			),
		}, nil
	}

	if user.ExpiresAt != nil && !user.ExpiresAt.After(time.Now()) {
		d.logger().Warn("guard.auth.user_expired", map[string]any{
			"userId":    user.ID,
			"expiresAt": user.ExpiresAt.UTC().Format(time.RFC3339),
		})
		if d.ExpiryMarker != nil {
			// 惰性过期标记是尽力而为：失败不影响本次拒绝。
			_ = d.ExpiryMarker.MarkUserExpired(d.runContext(ctx), user.ID)
		}
		return authOutcome{
			apiKey:      apiKey,
			failureKind: FailureAccountState,
			response: BuildError(
				401,
				fmt.Sprintf("用户账户已于 %s 过期。请续费订阅。", user.ExpiresAt.UTC().Format("2006-01-02")),
				"user_expired",
			),
		}, nil
	}

	return authOutcome{
		success: true,
		apiKey:  apiKey,
		state: pctx.AuthState{
			KeyID:    resolution.Key.ID,
			UserID:   user.ID,
			UserName: user.Name,
			KeyName:  resolution.Key.Name,
			APIKey:   apiKey,
		},
	}, nil
}

// resolveClientIP 解析客户端 IP。
//
// 有 IP 缝隙时以它为准（信任判断与 ip_extraction_config 都在那里）；否则退回上下文
// 入口已解析的值。
func (d Deps) resolveClientIP(ctx *pctx.Context) string {
	if d.IP != nil {
		ip, err := d.IP.ClientIP(d.runContext(ctx), rawHeaders(ctx.Headers()))
		if err != nil {
			d.logger().Warn("guard.auth.ip_extraction_failed", map[string]any{"error": err.Error()})
		} else if strings.TrimSpace(ip) != "" {
			return strings.TrimSpace(ip)
		}
	}
	return strings.TrimSpace(ctx.ClientIP())
}

// queryAPIKey 取 Gemini CLI 的 key 查询参数。
//
// pctx 只保留路径、不含查询串，因此这条凭据必须由入口缝隙注入；未注入时按无此凭据处理。
func (d Deps) queryAPIKey(ctx *pctx.Context) string {
	if d.QueryAPIKey == nil {
		return ""
	}
	return d.QueryAPIKey(ctx)
}

// locale 解析本次请求的语种。
func (d Deps) locale(ctx *pctx.Context) string {
	if d.Locale != "" {
		return d.Locale
	}
	headers := ctx.Headers()
	return ResolveLocale(
		cookieValue(headers.Get("cookie"), LocaleCookieName),
		headers.Get("accept-language"),
	)
}

// extractBearerKey 复刻 ProxyAuthenticator.extractKeyFromAuthorization。
func extractBearerKey(authHeader string) string {
	trimmed := strings.TrimSpace(authHeader)
	if trimmed == "" {
		return ""
	}
	match := bearerPattern.FindStringSubmatch(trimmed)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

// normalizeKey 复刻 ProxyAuthenticator.normalizeKey。
func normalizeKey(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	return trimmed
}

// extractAPIKeyFromHeaders 从请求头中提取 API Key，供非守卫流程复用。
//
// 支持 Authorization: Bearer、x-api-key 与 x-goog-api-key 三种方式，与
// src/lib/api/auth-header-extractor.ts 的对外行为一致。
func extractAPIKeyFromHeaders(headers map[string]string) string {
	candidates := keyCandidates{
		Authorization: headers["authorization"],
		APIKey:        headers["x-api-key"],
		GeminiHeader:  headers["x-goog-api-key"],
	}
	keys := candidates.provided()
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}
