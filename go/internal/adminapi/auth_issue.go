package adminapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminauth"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件实现根级认证面（**不在** /api/v1 挂载点下）：POST /api/auth/login 与 POST /api/auth/logout。
//
// 唯一真源：src/app/api/auth/login/route.ts、src/app/api/auth/logout/route.ts、src/lib/auth.ts
// （validateKey / getLoginRedirectTarget / setAuthCookie / clearAuthCookie / detectSessionTokenKind /
// toKeyFingerprint）、src/lib/security/csrf-origin-guard.ts、src/lib/security/login-abuse-policy.ts、
// src/lib/security/auth-response-headers.ts、src/lib/auth-session-store/redis-session-store.ts。
//
// 为什么它是「门」：UI 静态化后，登录态不再由
// Node 中间件与 SSR 决定，而是由浏览器拿 Go 签发的 auth-token 去调 REST。这条路由不实现，纯 Go
// 部署就进不了后台——它是整条切换链上唯一没有替代品的端点。
//
// 三处与 Node 的既定差异（刻意，逐条登记）：
//
//  1. **CSRF 开发态旁路不复刻**：Node 在 `NODE_ENV=development && !enforceInDevelopment` 时直接放行
//     （csrf-origin-guard.ts:66-68）。Go 侧没有 NODE_ENV 语义，故一律执行真实判定——比 Node 更严，
//     不会让一个开发态旁路被误开在生产。
//  2. **错误文案取自 i18n 数据文件**：Node 走 next-intl 运行时；Go 侧把 messages/<locale>/auth.json
//     的文案编成常量表（见 authLocaleMessagesByLocale）。文案逐字相同，但**新增语言需要同步这张表**，
//     且 ru 的 cookieWarning 文案在数据文件里缺失，故留空（Node 侧同样会是空串）。
//  3. **失败分类（errorCode）的出现条件照抄 Node**：仅当请求带 x-forwarded-proto 头时才附 errorCode
//     （login/route.ts:104-106 的 shouldIncludeFailureTaxonomy）。这让直连与经前门两条路径的响应形状
//     不同——那是 Node 的既有语义，不是本实现的取舍。

// authCookiePath 与 Next 的 setAuthCookie 传入的 path 一致（auth.ts:274）。
const authCookiePath = "/"

// opaqueSessionIDPrefix 与 auth.ts:144 的 OPAQUE_SESSION_ID_PREFIX 逐字一致。
const opaqueSessionIDPrefix = "sid_"

// loginMaxBodyBytes 限制登录正文大小。
//
// Node 由 Next 的默认正文限制兜底；Go 侧显式设界，避免匿名端点被超大正文打满内存。
// 真实 key 是几百字节，4 KiB 已宽松一个数量级。
const loginMaxBodyBytes = 4 * 1024

// authAbuseUnknownIP 与 Node 的 "unknown" 同义：resolveClientIp 取不到 IP 时的占位。
//
// 必须有占位而不是空串：限流按 IP 分桶，空串会把所有取不到 IP 的调用方合并成同一个桶。
const authAbuseUnknownIP = "unknown"

// AuthIssuerOptions 是根级认证面的构造参数。
type AuthIssuerOptions struct {
	// Deps 提供 Logger / Problems / Audit / Store。
	Deps Deps
	// Redis 是不透明会话的写入端（cch:session:<sid>）；nil 表示只支持 legacy 模式。
	Redis redis.UniversalClient
	// AdminToken 是 ADMIN_TOKEN；空串表示未配置（此时无法用管理员令牌登录）。
	AdminToken string
	// SessionTokenMode 是 SESSION_TOKEN_MODE：legacy / dual / opaque；空串按 opaque。
	SessionTokenMode string
	// AuthSessionTTL 是 AUTH_SESSION_TTL_SECONDS；<=0 时取 604800 秒。
	AuthSessionTTL time.Duration
	// EnableSecureCookies 与 ENABLE_SECURE_COOKIES 同源，决定 cookie 的 Secure 属性。
	EnableSecureCookies bool
	// Now 可注入时钟；nil 时用 time.Now。
	Now func() time.Time
	// Accounts 覆盖账户投影读取（测试注入）；nil 时用 PG 实现。
	Accounts LoginAccountReader
	// Sessions 覆盖会话读写（测试注入）；nil 时用 Redis 实现。
	Sessions AuthSessionWriter
	// Abuse 覆盖登录限流器（测试注入）；nil 时新建一个进程级实例。
	Abuse *LoginAbusePolicy
}

// LoginAccount 是一次成功登录所需的账户投影（Node 的 AuthSession 的扁平化）。
//
// 只收 Node 侧真正读过的字段：响应用户名/描述/角色，重定向与 loginType 用角色与 canLoginWebUI
// （login/route.ts:335-345）。少列字段比多列安全——多列的字段会在两处实现之间悄悄分叉。
type LoginAccount struct {
	UserID          int64
	UserName        string
	UserDescription string
	UserRole        string
	UserEnabled     bool
	UserExpiresAt   *time.Time
	KeyID           int64
	KeyName         string
	CanLoginWebUI   bool
}

// LoginAccountReader 按密钥串读账户投影。
type LoginAccountReader interface {
	// LoginAccountByKey 返回账户；密钥不存在/禁用/过期时 found 为 false。
	LoginAccountByKey(ctx context.Context, key string) (LoginAccount, bool, error)
}

// AuthSessionWriter 是不透明会话的创建与吊销（Node 的 SessionStore 的两件事）。
type AuthSessionWriter interface {
	// CreateAuthSession 写入会话并返回会话 id（sid_<uuid>）。apiKey 是签发时那把密钥的原文，
	// 会话里要落它的 sha256 指纹——守卫找回会话时按指纹逐把比对（auth.ts:448）。
	CreateAuthSession(
		ctx context.Context,
		apiKey string,
		account LoginAccount,
		credentialType string,
		ttl time.Duration,
	) (string, error)
	// RevokeAuthSession 吊销会话；不存在不算错。
	RevokeAuthSession(ctx context.Context, sessionID string) error
}

// AuthIssuer 承载根级认证面的依赖与策略。
type AuthIssuer struct {
	pools      *store.Pools
	logger     *logx.Logger
	problems   ProblemWriter
	audit      AuditSink
	accounts   LoginAccountReader
	sessions   AuthSessionWriter
	abuse      *LoginAbusePolicy
	adminToken string
	mode       string
	sessionTTL time.Duration
	secure     bool
	now        func() time.Time
}

// NewAuthIssuer 装配根级认证面。
func NewAuthIssuer(options AuthIssuerOptions) (*AuthIssuer, error) {
	logger := options.Deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	problems := options.Deps.Problems
	if problems == nil {
		problems = NewProblems(logger)
	}

	mode := options.SessionTokenMode
	if mode == "" {
		mode = "opaque"
	}
	switch mode {
	case "legacy", "dual", "opaque":
	default:
		return nil, fmt.Errorf("adminapi: SESSION_TOKEN_MODE 取值非法: %q", mode)
	}

	ttl := options.AuthSessionTTL
	if ttl <= 0 {
		ttl = defaultAuthSessionTTL
	}

	accounts := options.Accounts
	if accounts == nil {
		pg, err := newPGLoginAccounts(options.Deps)
		if err != nil {
			return nil, err
		}
		accounts = pg
	}

	sessions := options.Sessions
	if sessions == nil {
		sessions = newRedisAuthSessions(options.Redis)
	}

	abuse := options.Abuse
	if abuse == nil {
		abuse = NewLoginAbusePolicy()
	}

	return &AuthIssuer{
		pools:      options.Deps.Store,
		logger:     logger,
		problems:   problems,
		audit:      options.Deps.Audit,
		accounts:   accounts,
		sessions:   sessions,
		abuse:      abuse,
		adminToken: options.AdminToken,
		mode:       mode,
		sessionTTL: ttl,
		secure:     options.EnableSecureCookies,
		now:        options.Now,
	}, nil
}

// SessionTokenMode 返回生效的会话令牌模式（供启动日志如实报告）。
func (i *AuthIssuer) SessionTokenMode() string {
	if i == nil {
		return ""
	}
	return i.mode
}

// RegisterAuthRoutes 注册根级认证面的两条路由。
//
// 路径是**绝对路径**（含 /api 前缀），因为这两条不在 /api/v1 挂载点下：Router 的归一化只剥
// MountPrefix，其余路径按原样匹配（见 router.go 的 normalizePath）。它们也不带管理面信封
// （Node 的 /api/auth/* 不发 X-API-Version，见 Route.NoManagementEnvelope）。
//
// 档位是 public：登录本身不能要求已登录。CSRF 与限流由处理器自持（Node 同样在 route.ts 内自建
// CSRF guard，不走认证中间件）。
func RegisterAuthRoutes(router *Router, _ Deps, issuer *AuthIssuer) {
	if issuer == nil {
		return
	}
	router.Add(Route{
		Method:               http.MethodPost,
		Path:                 "/api/auth/login",
		Access:               AccessPublic,
		Module:               "auth",
		OperationID:          "authLogin",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(issuer.handleLogin),
	})
	router.Add(Route{
		Method:               http.MethodPost,
		Path:                 "/api/auth/logout",
		Access:               AccessPublic,
		Module:               "auth",
		OperationID:          "authLogout",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(issuer.handleLogout),
	})
}

// ---- 登录 ----

// loginSuccessResponse 逐字对应 Node 的成功响应（login/route.ts:335-345）。
type loginSuccessResponse struct {
	// OK 恒为 true（Node 侧字面量）。
	OK bool `json:"ok"`
	// User 是登录用户的公开信息 + 角色（Node 侧内联对象）。
	User loginUserPayload `json:"user"`
	// RedirectTo 是登录后的落地路径。
	RedirectTo string `json:"redirectTo"`
	// LoginType 是 Node 的三分类（admin / dashboard_user / readonly_user）。
	LoginType string `json:"loginType"`
}

// loginUserPayload 是 user 的内联对象。
type loginUserPayload struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Role        string `json:"role"`
}

// loginErrorResponse 是失败响应。
//
// ErrorCode 只在请求带 x-forwarded-proto 时出现（见文件头差异 3），故用 omitempty：Node 的无分类
// 版本连字段都不发。
type loginErrorResponse struct {
	Error     string `json:"error"`
	ErrorCode string `json:"errorCode,omitempty"`
	// HTTPMismatchGuidance 只在 ENABLE_SECURE_COOKIES=true 且经 http 前门到达时附加
	// （login/route.ts:288-300 的 hasSecureCookieHttpMismatch）。
	HTTPMismatchGuidance string `json:"httpMismatchGuidance,omitempty"`
}

// handleLogin 复刻 POST /api/auth/login。
func (i *AuthIssuer) handleLogin(writer http.ResponseWriter, request *http.Request) {
	if !checkSameOrigin(request) {
		i.writeAuthJSON(writer, http.StatusForbidden, map[string]any{"errorCode": "CSRF_REJECTED"})
		return
	}

	locale := resolveRequestLocale(request)
	messages := authMessagesFor(locale)
	ip := i.clientIP(request)
	abuseKey := ip
	if abuseKey == "" {
		abuseKey = authAbuseUnknownIP
	}
	userAgent := request.Header.Get("user-agent")
	taxonomy := hasForwardedProto(request)

	decision := i.abuse.Check(abuseKey, "")
	if !decision.Allowed {
		i.emitAudit(request, AuditEvent{
			Category:     "auth",
			Action:       "login.rate_limited",
			IP:           ip,
			UserAgent:    userAgent,
			Success:      false,
			ErrorMessage: "RATE_LIMITED",
		})
		var headers map[string]string
		if decision.RetryAfterSeconds != nil {
			headers = map[string]string{"Retry-After": strconv.Itoa(*decision.RetryAfterSeconds)}
		}
		i.writeAuthJSONWithHeaders(writer, http.StatusTooManyRequests, loginErrorResponse{
			Error:     messages.LoginFailed,
			ErrorCode: "RATE_LIMITED",
		}, headers)
		return
	}

	// 正文解析失败在 Node 里落进外层 catch → 500（login/route.ts:360-375）。
	key, present, ok := i.decodeLoginBody(writer, request)
	if !ok {
		return
	}

	if !present || key == "" {
		i.rejectMissingKey(writer, request, ip, userAgent, taxonomy, messages)
		return
	}

	account, found, err := i.resolveLoginAccount(request.Context(), key)
	if err != nil {
		// 依赖故障与 Node 的外层 catch 同义（它也会得到 500 SERVER_ERROR）。
		i.logger.Error("auth_login_lookup_failed", map[string]any{"error": err.Error()})
		i.writeAuthJSON(writer, http.StatusInternalServerError, serverErrorBody(taxonomy, messages))
		return
	}
	if !found {
		i.rejectInvalidKey(writer, request, ip, userAgent, taxonomy, messages, locale)
		return
	}

	cookieValue, failure := i.issueCookie(request.Context(), key, account)
	if failure != nil {
		i.logger.Error("auth_login_session_create_failed", map[string]any{"error": failure.Error()})
		i.writeAuthJSON(writer, http.StatusServiceUnavailable, loginErrorResponse{
			Error:     messages.ServerError,
			ErrorCode: "SESSION_CREATE_FAILED",
		})
		return
	}

	i.abuse.RecordSuccess(abuseKey, "")

	loginType := loginTypeFor(account)
	i.emitAudit(request, AuditEvent{
		Category: "auth",
		Action:   "login.success",
		Principal: Principal{
			UserID:   account.UserID,
			Username: account.UserName,
			KeyID:    account.KeyID,
			KeyName:  account.KeyName,
		},
		TargetType: "user",
		TargetID:   strconv.FormatInt(account.UserID, 10),
		TargetName: account.UserName,
		Details:    map[string]any{"loginType": loginType},
		IP:         ip,
		UserAgent:  userAgent,
		Success:    true,
	})

	i.writeAuthJSONWithHeaders(writer, http.StatusOK, loginSuccessResponse{
		OK: true,
		User: loginUserPayload{
			ID:          account.UserID,
			Name:        account.UserName,
			Description: account.UserDescription,
			Role:        account.UserRole,
		},
		RedirectTo: loginRedirectTarget(account),
		LoginType:  loginType,
	}, map[string]string{"Set-Cookie": i.setAuthCookieHeader(cookieValue)})
}

// rejectMissingKey 作答「正文里没有 key」分支（login/route.ts:181-204）。
func (i *AuthIssuer) rejectMissingKey(
	writer http.ResponseWriter,
	request *http.Request,
	ip string,
	userAgent string,
	taxonomy bool,
	messages authLocaleMessages,
) {
	i.abuse.RecordFailure(abuseScopeKey(ip), "")
	i.emitAudit(request, AuditEvent{
		Category:     "auth",
		Action:       "login.failure",
		IP:           ip,
		UserAgent:    userAgent,
		Success:      false,
		ErrorMessage: "KEY_REQUIRED",
	})
	body := loginErrorResponse{Error: messages.APIKeyRequired}
	if taxonomy {
		body.ErrorCode = "KEY_REQUIRED"
	}
	i.writeAuthJSON(writer, http.StatusBadRequest, body)
}

// rejectInvalidKey 作答「密钥无效/过期/用户停用」分支（login/route.ts:206-244）。
func (i *AuthIssuer) rejectInvalidKey(
	writer http.ResponseWriter,
	request *http.Request,
	ip string,
	userAgent string,
	taxonomy bool,
	messages authLocaleMessages,
	locale string,
) {
	i.abuse.RecordFailure(abuseScopeKey(ip), "")
	i.emitAudit(request, AuditEvent{
		Category:     "auth",
		Action:       "login.failure",
		IP:           ip,
		UserAgent:    userAgent,
		Success:      false,
		ErrorMessage: "KEY_INVALID",
	})
	body := loginErrorResponse{Error: messages.APIKeyInvalidOrExpired}
	if taxonomy {
		body.ErrorCode = "KEY_INVALID"
		if i.hasSecureCookieHTTPMismatch(request) {
			body.HTTPMismatchGuidance = authMessagesFor(locale).CookieWarningDescription
		}
	}
	i.writeAuthJSON(writer, http.StatusUnauthorized, body)
}

// abuseScopeKey 取限流分桶键（与 Node 的 resolveClientIp 同口径）。
func abuseScopeKey(ip string) string {
	if ip == "" {
		return authAbuseUnknownIP
	}
	return ip
}

// decodeLoginBody 读并解析登录正文，返回 key 字段与「字段是否存在」。
//
// 解析失败时已作答 500（与 Node 的外层 catch 同义），返回值 ok 为 false。
func (i *AuthIssuer) decodeLoginBody(writer http.ResponseWriter, request *http.Request) (string, bool, bool) {
	body := http.MaxBytesReader(writer, request.Body, loginMaxBodyBytes)
	var payload struct {
		Key *string `json:"key"`
	}
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		i.logger.Warn("auth_login_body_invalid", map[string]any{"error": err.Error()})
		taxonomy := hasForwardedProto(request)
		i.writeAuthJSON(writer, http.StatusInternalServerError,
			serverErrorBody(taxonomy, authMessagesFor(resolveRequestLocale(request))))
		return "", false, false
	}
	if payload.Key == nil {
		// Node 的 `!key || typeof key !== "string"`：字段缺失（undefined）与空串同路。
		return "", false, true
	}
	return *payload.Key, true, true
}

// serverErrorBody 是 500 的形状（无分类时不带 errorCode）。
func serverErrorBody(taxonomy bool, messages authLocaleMessages) loginErrorResponse {
	body := loginErrorResponse{Error: messages.ServerError}
	if taxonomy {
		body.ErrorCode = "SERVER_ERROR"
	}
	return body
}

// resolveLoginAccount 判定凭据：先是 ADMIN_TOKEN，再是库里的密钥。
func (i *AuthIssuer) resolveLoginAccount(ctx context.Context, key string) (LoginAccount, bool, error) {
	if i.adminToken != "" && constantTimeEqual(key, i.adminToken) {
		// 与 Node 的合成身份逐字一致（auth.ts:196-235）：id/keyId 均为 -1，角色 admin。
		return LoginAccount{
			UserID:          adminPrincipalUserID,
			UserName:        adminPrincipalName,
			UserDescription: "Environment admin session",
			UserRole:        "admin",
			UserEnabled:     true,
			KeyID:           adminPrincipalUserID,
			KeyName:         adminKeyName,
			CanLoginWebUI:   true,
		}, true, nil
	}
	account, found, err := i.accounts.LoginAccountByKey(ctx, key)
	if err != nil || !found {
		return LoginAccount{}, false, err
	}
	// 用户停用/过期与 Node 的 validateKey 同路（auth.ts:246-256）：返回 null → 401。
	if !account.UserEnabled {
		return LoginAccount{}, false, nil
	}
	if account.UserExpiresAt != nil && !account.UserExpiresAt.After(i.clock()) {
		return LoginAccount{}, false, nil
	}
	return account, true, nil
}

// issueCookie 按 SESSION_TOKEN_MODE 决定 cookie 值（login/route.ts:246-286）。
//
// 失败时返回 error：opaque 模式下签不出会话就是 503 SESSION_CREATE_FAILED（Node 同语义）。
func (i *AuthIssuer) issueCookie(ctx context.Context, key string, account LoginAccount) (string, error) {
	switch i.mode {
	case "legacy":
		return key, nil

	case "dual":
		// dual 下同时下发裸 Key 与会话；会话写失败只记 warn（login/route.ts:255-262）。
		if _, err := i.createSession(ctx, key, account); err != nil {
			i.logger.Warn("auth_login_opaque_session_failed", map[string]any{"error": err.Error()})
		}
		return key, nil

	default:
		if account.KeyID == adminPrincipalUserID {
			token, err := adminauth.CreateSignedToken(i.adminToken, i.sessionTTL, i.clock())
			if err != nil {
				return "", err
			}
			return token, nil
		}
		return i.createSession(ctx, key, account)
	}
}

// createSession 写入不透明会话。
func (i *AuthIssuer) createSession(ctx context.Context, key string, account LoginAccount) (string, error) {
	if i.sessions == nil {
		return "", errors.New("adminapi: 未装配不透明会话写入端")
	}
	return i.sessions.CreateAuthSession(ctx, key, account, credentialTypeFor(account), i.sessionTTL)
}

// credentialTypeFor 复刻 classifyLoginCredential（login/route.ts:139-147）。
func credentialTypeFor(account LoginAccount) string {
	if account.KeyID == adminPrincipalUserID {
		return string(credentialAdminToken)
	}
	if account.CanLoginWebUI {
		return string(credentialSession)
	}
	return string(credentialUserAPIKey)
}

// loginRedirectTarget 复刻 auth.ts:260-264 的 getLoginRedirectTarget。
func loginRedirectTarget(account LoginAccount) string {
	if account.UserRole == "admin" || account.CanLoginWebUI {
		return "/dashboard"
	}
	return "/my-usage"
}

// loginTypeFor 复刻 login/route.ts:335-341 的 loginType。
func loginTypeFor(account LoginAccount) string {
	switch {
	case account.UserRole == "admin":
		return "admin"
	case account.CanLoginWebUI:
		return "dashboard_user"
	default:
		return "readonly_user"
	}
}

// ---- 登出 ----

// handleLogout 复刻 POST /api/auth/logout。
func (i *AuthIssuer) handleLogout(writer http.ResponseWriter, request *http.Request) {
	if !checkSameOrigin(request) {
		i.writeAuthJSON(writer, http.StatusForbidden, map[string]any{"errorCode": "CSRF_REJECTED"})
		return
	}

	// 非 legacy 模式下先吊销不透明会话，再清 cookie（logout/route.ts:43-59）。
	if i.mode != "legacy" {
		token := cookieValue(request.Header.Get("Cookie"), authCookieName)
		if token != "" && strings.HasPrefix(token, opaqueSessionIDPrefix) && i.sessions != nil {
			if err := i.sessions.RevokeAuthSession(request.Context(), token); err != nil {
				i.logger.Warn("auth_logout_revoke_failed", map[string]any{"error": err.Error()})
			}
		}
	}

	i.writeAuthJSONWithHeaders(writer, http.StatusOK, map[string]any{"ok": true},
		map[string]string{"Set-Cookie": clearAuthCookieHeader()})
}

// ---- cookie 序列化 ----

// setAuthCookieHeader 序列化 Set-Cookie，逐字对齐 Next 的 stringifyCookie（含属性顺序）。
//
// Next 的属性顺序是 Path → Expires → Max-Age → Domain → Secure → HttpOnly → SameSite
// （@edge-runtime/cookies 的 stringifyCookie），且 setAuthCookie 的 maxAge 会被 normalizeCookie
// 展开成绝对 Expires。用 http.SetCookie 得不到同样的顺序与取值编码（它会按自己的规则给值加引号），
// 而 cookie 字节是要与 Node 对拍的，故此处手工拼。
func (i *AuthIssuer) setAuthCookieHeader(value string) string {
	parts := []string{
		authCookieName + "=" + encodeURIComponent(value),
		"Path=" + authCookiePath,
		"Expires=" + i.clock().Add(i.sessionTTL).UTC().Format(http.TimeFormat),
		"Max-Age=" + strconv.FormatInt(int64(i.sessionTTL/time.Second), 10),
	}
	if i.secure {
		parts = append(parts, "Secure")
	}
	parts = append(parts, "HttpOnly", "SameSite=lax")
	return strings.Join(parts, "; ")
}

// clearAuthCookieHeader 复刻 clearAuthCookie：Next 的 delete 只带 name/value/expires，
// 因此**没有** Path、HttpOnly、SameSite（@edge-runtime/cookies 的 ResponseCookies.delete）。
func clearAuthCookieHeader() string {
	return authCookieName + "=; Expires=" + time.Unix(0, 0).UTC().Format(http.TimeFormat)
}

// ---- CSRF 同源门 ----

// checkSameOrigin 复刻 createCsrfOriginGuard（allowedOrigins 空、allowSameOrigin true、不信任
// X-Forwarded-Host；开发态旁路按文件头差异 1 不复刻）。
func checkSameOrigin(request *http.Request) bool {
	fetchSite := strings.ToLower(strings.TrimSpace(request.Header.Get("sec-fetch-site")))
	if fetchSite == "same-origin" {
		return true
	}

	origin := strings.ToLower(strings.TrimSpace(request.Header.Get("origin")))
	if origin == "" {
		// 缺 Origin 且不是跨站：放行。命令行客户端与同源表单提交都落在这里。
		return fetchSite != "cross-site"
	}

	host := strings.ToLower(strings.TrimSpace(request.Header.Get("host")))
	if host == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return parsed.Host == host
}

// hasForwardedProto 复刻 shouldIncludeFailureTaxonomy（login/route.ts:104-106）。
func hasForwardedProto(request *http.Request) bool {
	return request.Header.Get("x-forwarded-proto") != ""
}

// hasSecureCookieHTTPMismatch 复刻 login/route.ts:96-100。
func (i *AuthIssuer) hasSecureCookieHTTPMismatch(request *http.Request) bool {
	if !i.secure {
		return false
	}
	proto := request.Header.Get("x-forwarded-proto")
	if index := strings.IndexByte(proto, ','); index >= 0 {
		proto = proto[:index]
	}
	return strings.TrimSpace(proto) == "http"
}

// clientIP 解析登录面用的客户端 IP（与审计同一条可配置提取链，见 audit_ip.go）。
func (i *AuthIssuer) clientIP(request *http.Request) string {
	return auditClientIP(request.Context(), i.pools, request)
}

// emitAudit 发一条审计（Deps.Audit 为 nil 时是空操作）。
func (i *AuthIssuer) emitAudit(request *http.Request, event AuditEvent) {
	if i.audit == nil {
		return
	}
	ctx := context.Background()
	if request != nil {
		ctx = request.Context()
	}
	i.audit.Emit(ctx, event)
}

// ---- 响应写出 ----

// writeAuthJSON 作答 JSON 并附认证面的响应头（withAuthResponseHeaders）。
func (i *AuthIssuer) writeAuthJSON(writer http.ResponseWriter, status int, body any) {
	i.writeAuthJSONWithHeaders(writer, status, body, nil)
}

// writeAuthJSONWithHeaders 作答 JSON，附认证面响应头与额外头。
func (i *AuthIssuer) writeAuthJSONWithHeaders(
	writer http.ResponseWriter,
	status int,
	body any,
	headers map[string]string,
) {
	header := writer.Header()
	applyAuthEnvelopeHeaders(header, i.secure)
	for name, value := range headers {
		header.Set(name, value)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(writer, "", http.StatusInternalServerError)
		return
	}
	// Content-Type 与 Node 一致：application/json（Next 的 NextResponse.json 不带 charset）。
	header.Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}

// clock 取当前时间。
func (i *AuthIssuer) clock() time.Time {
	if i.now != nil {
		return i.now()
	}
	return time.Now()
}

// ---- 账户投影（PG） ----

// pgLoginAccounts 用 guard 的密钥解析器 + 一次管理面投影读取来实现账户查询。
//
// 为什么复用 guard.AuthResolver 而不是自带一条 SELECT：密钥解析有「同一条 key 串匹配多行时取最
// 有利状态」「负结果不进缓存」等细节，数据面、管理面、登录面三处必须同口径。自带一条就是第三条会
// 漂移的实现，而它漂移的表现是「能调 API 但登不进后台」。
type pgLoginAccounts struct {
	pools *store.Pools
	keys  guard.AuthResolver
}

// newPGLoginAccounts 建账户读取器；无连接池时返回一个恒「未找到」的实现（fail-closed）。
func newPGLoginAccounts(deps Deps) (*pgLoginAccounts, error) {
	if deps.Store == nil {
		// 无 DSN：只有 ADMIN_TOKEN 登得进。比 Node 更宽松（Node 无 DB 时直接 500），但仍安全。
		return &pgLoginAccounts{}, nil
	}
	adapters, err := guard.NewAdapters(guard.AdapterOptions{Pools: deps.Store, Logger: deps.Logger})
	if err != nil {
		return nil, fmt.Errorf("adminapi: 构造登录面密钥解析器失败: %w", err)
	}
	return &pgLoginAccounts{pools: deps.Store, keys: adapters.Auth}, nil
}

// LoginAccountByKey 实现 LoginAccountReader。
func (p *pgLoginAccounts) LoginAccountByKey(ctx context.Context, key string) (LoginAccount, bool, error) {
	if p == nil || p.pools == nil || p.keys == nil {
		return LoginAccount{}, false, nil
	}
	resolution, err := p.keys.ResolveAPIKey(ctx, key)
	if err != nil {
		switch {
		case errors.Is(err, guard.ErrKeyNotFound),
			errors.Is(err, guard.ErrKeyDisabled),
			errors.Is(err, guard.ErrKeyExpired):
			return LoginAccount{}, false, nil
		default:
			return LoginAccount{}, false, err
		}
	}

	// 描述与 canLoginWebUI 不在 guard 的用户切面里（数据面不需要那两列），按主键补一次读。
	const query = `SELECT row_to_json(t)::text FROM (
		SELECT u.description AS user_description, u.role AS user_role,
		       k.name AS key_name, k.can_login_web_ui
		FROM keys k JOIN users u ON u.id = k.user_id
		WHERE k.id = $1 AND k.deleted_at IS NULL AND u.deleted_at IS NULL
	) t`
	rows, err := queryJSONRows(ctx, p.pools, query, resolution.Key.ID)
	if err != nil {
		return LoginAccount{}, false, err
	}
	if len(rows) == 0 {
		return LoginAccount{}, false, nil
	}
	var row struct {
		UserDescription *string `json:"user_description"`
		UserRole        *string `json:"user_role"`
		KeyName         *string `json:"key_name"`
		CanLoginWebUI   *bool   `json:"can_login_web_ui"`
	}
	if err := json.Unmarshal([]byte(rows[0]), &row); err != nil {
		return LoginAccount{}, false, fmt.Errorf("adminapi: 登录账户投影反序列化失败: %w", err)
	}

	account := LoginAccount{
		UserID:        resolution.User.ID,
		UserName:      resolution.User.Name,
		UserEnabled:   resolution.User.IsEnabled,
		UserExpiresAt: resolution.User.ExpiresAt,
		KeyID:         resolution.Key.ID,
	}
	if row.UserDescription != nil {
		account.UserDescription = *row.UserDescription
	}
	if row.UserRole != nil {
		account.UserRole = *row.UserRole
	}
	if row.KeyName != nil {
		account.KeyName = *row.KeyName
	}
	if row.CanLoginWebUI != nil {
		account.CanLoginWebUI = *row.CanLoginWebUI
	}
	return account, true, nil
}

// ---- 不透明会话（Redis） ----

// redisAuthSessions 实现 AuthSessionWriter。
type redisAuthSessions struct {
	client redis.UniversalClient
}

// newRedisAuthSessions 建会话读写端；client 为 nil 时返回 nil（处理器据此作答 503）。
func newRedisAuthSessions(client redis.UniversalClient) AuthSessionWriter {
	if client == nil {
		return nil
	}
	return redisAuthSessions{client: client}
}

// CreateAuthSession 写会话（redis-session-store.ts:130-160）。
//
// payload 的七个字段与 SessionData 一一对应，键名逐字相同：解析端既有 Node 的 parseSessionData，
// 也有 adminapi 的 redisSessionReader.Read，任何键名改动都会让会话在两侧同时失效。
func (s redisAuthSessions) CreateAuthSession(
	ctx context.Context,
	apiKey string,
	account LoginAccount,
	credentialType string,
	ttl time.Duration,
) (string, error) {
	if s.client == nil {
		return "", errors.New("adminapi: Redis 未装配")
	}
	ttlSeconds := int64(ttl / time.Second)
	if ttlSeconds < 1 {
		ttlSeconds = int64(defaultAuthSessionTTL / time.Second)
	}

	sessionID := opaqueSessionIDPrefix + randomUUIDString()
	if sessionID == opaqueSessionIDPrefix {
		// 随机源不可用：宁可不签发，也不要写一个 id 为空的会话（那会让所有浏览器共用同一个键）。
		return "", errors.New("adminapi: 生成会话 id 失败")
	}

	createdAt := time.Now().UnixMilli()
	payload, err := json.Marshal(map[string]any{
		"sessionId":      sessionID,
		"keyFingerprint": keyFingerprint(apiKey),
		"credentialType": credentialType,
		"userId":         account.UserID,
		"userRole":       account.UserRole,
		"createdAt":      createdAt,
		"expiresAt":      createdAt + ttlSeconds*1000,
	})
	if err != nil {
		return "", err
	}
	key := sessionKeyPrefix + sessionID
	if err := s.client.SetEx(ctx, key, string(payload), time.Duration(ttlSeconds)*time.Second).Err(); err != nil {
		return "", err
	}
	return sessionID, nil
}

// RevokeAuthSession 删会话（redis-session-store.ts:181-200）。
func (s redisAuthSessions) RevokeAuthSession(ctx context.Context, sessionID string) error {
	if s.client == nil || sessionID == "" {
		return nil
	}
	return s.client.Del(ctx, sessionKeyPrefix+sessionID).Err()
}

// randomUUIDString 产出与 crypto.randomUUID() 同形状的 v4 UUID；随机源失败时返回空串。
func randomUUIDString() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// ---- 登录限流（进程内，复刻 LoginAbusePolicy） ----

// LoginAbuseConfig 与 login-abuse-policy.ts:1-6 的 LoginAbuseConfig 同名同义。
type LoginAbuseConfig struct {
	// MaxAttemptsPerIP 是单 IP 的窗口内失败阈值。
	MaxAttemptsPerIP int
	// MaxAttemptsPerKey 是单 key 的窗口内失败阈值（登录路径不传 key，故当前用不到）。
	MaxAttemptsPerKey int
	// Window 是计数窗口。
	Window time.Duration
	// Lockout 是超限后的锁定时长。
	Lockout time.Duration
}

// DefaultLoginAbuseConfig 与 DEFAULT_LOGIN_ABUSE_CONFIG 逐值一致（10/10/300s/900s）。
func DefaultLoginAbuseConfig() LoginAbuseConfig {
	return LoginAbuseConfig{
		MaxAttemptsPerIP:  10,
		MaxAttemptsPerKey: 10,
		Window:            300 * time.Second,
		Lockout:           900 * time.Second,
	}
}

// loginAbuseSweepInterval 与 SWEEP_INTERVAL_MS 一致。
const loginAbuseSweepInterval = 60 * time.Second

// loginAbuseMaxEntries 与 MAX_TRACKED_ENTRIES 一致。
const loginAbuseMaxEntries = 10_000

// LoginAbuseDecision 是一次限流判定的结果。
type LoginAbuseDecision struct {
	// Allowed 为 false 时请求应被拒。
	Allowed bool
	// RetryAfterSeconds 仅在拒绝时给（用于 Retry-After 头）。
	RetryAfterSeconds *int
	// Reason 是分桶名（ip_rate_limited / key_rate_limited）。
	Reason string
}

// loginAbuseRecord 是一个分桶的计数。
type loginAbuseRecord struct {
	count        int
	firstAttempt time.Time
	lockedUntil  time.Time
}

// LoginAbusePolicy 复刻 LoginAbusePolicy 的语义（进程内、按 IP/Key 分桶、超限锁定）。
//
// 与 Node 的差异（有意）：Node 用 Map 的插入序遍历做 LRU 淘汰，Go 的 map 无序，故用显式的次序
// 切片做 FIFO 淘汰。可观测差别只在「同一时刻大量不同 IP 攻击时的淘汰次序」，不影响单 IP 的判定。
type LoginAbusePolicy struct {
	mu        sync.Mutex
	attempts  map[string]*loginAbuseRecord
	order     []string
	config    LoginAbuseConfig
	lastSweep time.Time
	now       func() time.Time
}

// NewLoginAbusePolicy 建限流器（默认配置，与 Node 的 new LoginAbusePolicy() 同）。
func NewLoginAbusePolicy() *LoginAbusePolicy {
	return &LoginAbusePolicy{attempts: map[string]*loginAbuseRecord{}, config: DefaultLoginAbuseConfig()}
}

// WithConfig 覆盖配置（测试用）。
func (p *LoginAbusePolicy) WithConfig(config LoginAbuseConfig) *LoginAbusePolicy {
	p.config = config
	return p
}

// WithNow 注入时钟（测试用）。
func (p *LoginAbusePolicy) WithNow(now func() time.Time) *LoginAbusePolicy {
	p.now = now
	return p
}

// Check 判定是否放行（Node 的 check(ip, key)；登录路径只传 ip）。
func (p *LoginAbusePolicy) Check(ip string, key string) LoginAbuseDecision {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.clock()
	p.sweep(now)

	ipDecision := p.checkScope(ipScope(ip), p.config.MaxAttemptsPerIP, "ip_rate_limited", now)
	if !ipDecision.Allowed || key == "" {
		return ipDecision
	}
	return p.checkScope(keyScope(key), p.config.MaxAttemptsPerKey, "key_rate_limited", now)
}

// RecordFailure 记一次失败。
func (p *LoginAbusePolicy) RecordFailure(ip string, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	p.recordFailure(ipScope(ip), p.config.MaxAttemptsPerIP, now)
	if key != "" {
		p.recordFailure(keyScope(key), p.config.MaxAttemptsPerKey, now)
	}
}

// RecordSuccess 清空该分桶（与 Node 的 recordSuccess → reset 同义）。
func (p *LoginAbusePolicy) RecordSuccess(ip string, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.remove(ipScope(ip))
	if key != "" {
		p.remove(keyScope(key))
	}
}

// checkScope 复刻 checkScope。
func (p *LoginAbusePolicy) checkScope(scope string, threshold int, reason string, now time.Time) LoginAbuseDecision {
	record, ok := p.attempts[scope]
	if !ok {
		return LoginAbuseDecision{Allowed: true}
	}
	if !record.lockedUntil.IsZero() {
		if record.lockedUntil.After(now) {
			retry := retryAfterSeconds(record.lockedUntil, now)
			return LoginAbuseDecision{Allowed: false, RetryAfterSeconds: &retry, Reason: reason}
		}
		p.remove(scope)
		return LoginAbuseDecision{Allowed: true}
	}
	if p.windowExpired(record, now) {
		p.remove(scope)
		return LoginAbuseDecision{Allowed: true}
	}
	if record.count >= threshold {
		lockedUntil := now.Add(p.config.Lockout)
		p.remove(scope)
		p.insert(scope, &loginAbuseRecord{
			count:        record.count,
			firstAttempt: record.firstAttempt,
			lockedUntil:  lockedUntil,
		})
		retry := retryAfterSeconds(lockedUntil, now)
		return LoginAbuseDecision{Allowed: false, RetryAfterSeconds: &retry, Reason: reason}
	}
	// LRU 语义：命中的桶挪到队尾（Node 的 delete + set）。
	p.remove(scope)
	p.insert(scope, record)
	return LoginAbuseDecision{Allowed: true}
}

// recordFailure 复刻 recordFailureForScope。
func (p *LoginAbusePolicy) recordFailure(scope string, threshold int, now time.Time) {
	record, ok := p.attempts[scope]
	if !ok {
		p.insert(scope, &loginAbuseRecord{count: 1, firstAttempt: now})
		return
	}
	if !record.lockedUntil.IsZero() {
		if record.lockedUntil.After(now) {
			return
		}
		p.remove(scope)
		p.insert(scope, &loginAbuseRecord{count: 1, firstAttempt: now})
		return
	}
	if p.windowExpired(record, now) {
		p.remove(scope)
		p.insert(scope, &loginAbuseRecord{count: 1, firstAttempt: now})
		return
	}
	// 达到阈值不在记数时锁定：Node 同样在 check 时才锁定（记满后下一次 check 触发锁）。
	if record.count < threshold {
		record.count++
	}
	p.remove(scope)
	p.insert(scope, record)
}

// sweep 复刻 sweepStaleEntries。
func (p *LoginAbusePolicy) sweep(now time.Time) {
	if now.Sub(p.lastSweep) < loginAbuseSweepInterval {
		return
	}
	p.lastSweep = now
	for scope, record := range p.attempts {
		if !record.lockedUntil.IsZero() {
			if !record.lockedUntil.After(now) {
				p.remove(scope)
			}
			continue
		}
		if p.windowExpired(record, now) {
			p.remove(scope)
		}
	}
	for len(p.attempts) > loginAbuseMaxEntries && len(p.order) > 0 {
		p.remove(p.order[0])
	}
}

// windowExpired 复刻 isWindowExpired。
func (p *LoginAbusePolicy) windowExpired(record *loginAbuseRecord, now time.Time) bool {
	return now.Sub(record.firstAttempt) > p.config.Window
}

// insert 写入并登记淘汰次序。
func (p *LoginAbusePolicy) insert(scope string, record *loginAbuseRecord) {
	if _, exists := p.attempts[scope]; !exists {
		p.order = append(p.order, scope)
	}
	p.attempts[scope] = record
}

// remove 删除并同步淘汰次序。
func (p *LoginAbusePolicy) remove(scope string) {
	if _, ok := p.attempts[scope]; !ok {
		return
	}
	delete(p.attempts, scope)
	for index, entry := range p.order {
		if entry == scope {
			p.order = append(p.order[:index], p.order[index+1:]...)
			break
		}
	}
}

// clock 取当前时间。
func (p *LoginAbusePolicy) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// retryAfterSeconds 复刻 calculateRetryAfterSeconds：向上取整到秒，至少 1。
func retryAfterSeconds(until time.Time, now time.Time) int {
	remaining := until.Sub(now)
	if remaining <= 0 {
		return 1
	}
	seconds := int((remaining + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

// ipScope 与 toIpScope 同义（Node 里是 `ip:${ip}`）。
func ipScope(ip string) string { return "ip:" + ip }

// keyScope 与 toKeyScope 同义（Node 里是 `key:${key}`；Node 未做哈希，此处同样不哈希）。
//
// 刻意与 Node 一致地不做哈希：这是进程内 map 的键，不外泄；加哈希只会让两侧的调试输出分叉。
func keyScope(key string) string { return "key:" + key }

// ---- i18n 文案（取自 messages/<locale>/auth.json） ----

// authLocaleOrder 与 src/i18n/config.ts 的 locales 顺序逐字一致：Accept-Language 按此顺序匹配。
var authLocaleOrder = []string{"zh-CN", "zh-TW", "en", "ru", "ja"}

// authDefaultLocale 与 defaultLocale 一致。
const authDefaultLocale = "zh-CN"

// authLocaleCookieName 与 localeCookieName 一致。
const authLocaleCookieName = "NEXT_LOCALE"

// authLocaleMessages 是登录面用到的文案（node_modules 之外的唯一一份文案副本）。
type authLocaleMessages struct {
	LoginFailed              string
	APIKeyRequired           string
	APIKeyInvalidOrExpired   string
	ServerError              string
	CookieWarningDescription string
}

// authLocaleMessagesByLocale 逐字取自 messages/<locale>/auth.json 的 errors 与 security.cookieWarningDescription。
var authLocaleMessagesByLocale = map[string]authLocaleMessages{
	"zh-CN": {
		LoginFailed:              "登录失败",
		APIKeyRequired:           "请输入 API Key",
		APIKeyInvalidOrExpired:   "API Key 无效或已过期",
		ServerError:              "登录失败，请稍后重试",
		CookieWarningDescription: "您正在使用 HTTP 访问系统，浏览器安全策略可能阻止 Cookie 设置导致登录失败。",
	},
	"zh-TW": {
		LoginFailed:              "登錄失敗",
		APIKeyRequired:           "請輸入 API Key",
		APIKeyInvalidOrExpired:   "API Key 無效或已過期",
		ServerError:              "登錄失敗，請稍後重試",
		CookieWarningDescription: "您正在使用 HTTP 存取系統，瀏覽器安全政策可能阻止 Cookie 設定導致登錄失敗。",
	},
	"en": {
		LoginFailed:              "Login failed",
		APIKeyRequired:           "Please enter API Key",
		APIKeyInvalidOrExpired:   "API Key is invalid or expired",
		ServerError:              "Login failed, please try again later",
		CookieWarningDescription: "You are accessing the system via HTTP; browser security policies may block cookie settings and cause login failures.",
	},
	"ru": {
		LoginFailed:              "Ошибка входа",
		APIKeyRequired:           "Пожалуйста, введите API ключ",
		APIKeyInvalidOrExpired:   "API ключ недействителен или истёк",
		ServerError:              "Ошибка входа, попробуйте позже",
		CookieWarningDescription: "",
	},
	"ja": {
		LoginFailed:              "ログインに失敗しました",
		APIKeyRequired:           "API Keyを入力してください",
		APIKeyInvalidOrExpired:   "API Keyが無効または期限切れです",
		ServerError:              "ログインに失敗しました。しばらく後に再度お試しください",
		CookieWarningDescription: "HTTP 経由でシステムにアクセスしています。ブラウザーのセキュリティポリシーが Cookie の設定を妨げる可能性があり、ログインが失敗する可能性があります。",
	},
}

// authMessagesFor 取某 locale 的文案；未注册 locale 落默认。
func authMessagesFor(locale string) authLocaleMessages {
	if messages, ok := authLocaleMessagesByLocale[locale]; ok {
		return messages
	}
	return authLocaleMessagesByLocale[authDefaultLocale]
}

// resolveRequestLocale 复刻 getLocaleFromRequest（login/route.ts:36-57）。
func resolveRequestLocale(request *http.Request) string {
	if cookie := cookieValue(request.Header.Get("Cookie"), authLocaleCookieName); cookie != "" {
		for _, locale := range authLocaleOrder {
			if cookie == locale {
				return locale
			}
		}
	}
	if accept := strings.ToLower(request.Header.Get("accept-language")); accept != "" {
		for _, locale := range authLocaleOrder {
			if strings.Contains(accept, strings.ToLower(locale)) {
				return locale
			}
		}
	}
	return authDefaultLocale
}

// applyAuthEnvelopeHeaders 复刻 withAuthResponseHeaders：no-store + 安全头，**不含**版本头。
//
// 与 Router 的管理面信封只差一处：Node 的 /api/auth/* 不发 X-API-Version（它不在管理面应用上）。
// 其余安全头与 csp 取值同源（security-headers.ts:38-58）。
func applyAuthEnvelopeHeaders(header http.Header, enableHSTS bool) {
	header.Set("Cache-Control", "no-store, no-cache, must-revalidate")
	header.Set("Pragma", "no-cache")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	header.Set("X-DNS-Prefetch-Control", "off")
	header.Set("Content-Security-Policy-Report-Only", defaultCSP)
	if enableHSTS {
		header.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}
