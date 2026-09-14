package adminapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminauth"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// 本文件实现 Guard：管理面的四次门（凭据提取、令牌校验、档位与开关、CSRF）。
//
// 唯一真源：src/lib/api/v1/_shared/auth-middleware.ts（四次门与失败形状）、src/lib/auth.ts
// （validateAuthToken 的四条分支与 validateKey 的公共校验）、src/lib/api/v1/_shared/csrf.ts
// （CSRF 派生）、src/lib/api/auth-header-extractor.ts（凭据提取顺序）。
//
// 分支顺序是语义，不要重排：先「原始 ADMIN_TOKEN」——它是最短路径且与 Cookie 无关；再「签名的
// admin 会话令牌」（legacy 模式下一律拒绝，见 auth.ts:320-322）；再「不透明会话」（Redis）；
// 再 legacy/dual 的「API Key 直用」；opaque 模式下 API Key 直用不被接受（auth.ts:328-335），
// 这是 Node 的硬语义——opaque 之后浏览器只持 sid_，裸 Key 不再当会话用。

// authCookieName 与 auth.ts:47 的 AUTH_COOKIE_NAME 逐字一致。
const authCookieName = "auth-token"

// csrfHeader 与 constants.ts:5 的 CSRF_HEADER 逐字一致。
const csrfHeader = "X-CCH-CSRF"

// sessionKeyPrefix 与 redis-session-store.ts:13 的 SESSION_KEY_PREFIX 逐字一致。
const sessionKeyPrefix = "cch:session:"

// csrfWindow 与 csrf.ts:4 的 CSRF_WINDOW_MS 逐字一致。
const csrfWindow = 30 * time.Minute

// adminPrincipalUserID 与 auth.ts:191 的虚拟管理员用户 id 一致（键 id 同为 -1）。
const adminPrincipalUserID = -1

// adminPrincipalName 与 auth.ts:193 一致。
const adminPrincipalName = "Admin Token"

// adminKeyName 是 auth.ts:206 里合成密钥的 name（审计的 operator_key_name）。
const adminKeyName = "ADMIN_TOKEN"

// defaultAuthSessionTTL 与 auth-session-store/index.ts:21 一致（AUTH_SESSION_TTL_SECONDS 的默认值）。
const defaultAuthSessionTTL = 604800 * time.Second

// credentialType 与 auth.ts:148 的 AuthCredentialType 一致。
type credentialType string

const (
	credentialSession      credentialType = "session"
	credentialAdminToken   credentialType = "admin-token"
	credentialUserAPIKey   credentialType = "user-api-key"
	credentialNone         credentialType = "none"
	credentialSourceCookie                = "cookie"
)

// GuardOptions 是管理面守卫的构造参数。
type GuardOptions struct {
	// Pools 是数据库分道；密钥解析与角色判定都需要，故必填。
	Pools *store.Pools
	// Redis 是不透明会话（cch:session:<sid>）的读取端；nil 表示该路径一律拒绝。
	Redis redis.UniversalClient
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Problems 为 nil 时用内置实现。
	Problems ProblemWriter
	// AdminToken 是 ADMIN_TOKEN；空串表示未配置（据此拒绝签名令牌，与 auth.ts:350-360 一致）。
	AdminToken string
	// CSRFSecret 是 CSRF_SECRET；空串时退回 AdminToken，再退回会话令牌本身（csrf.ts:59-61）。
	CSRFSecret string
	// EnableAPIKeyAdminAccess 与 ENABLE_API_KEY_ADMIN_ACCESS 同源，默认 false。
	EnableAPIKeyAdminAccess bool
	// SessionTokenMode 是 SESSION_TOKEN_MODE：legacy / dual / opaque；空串按 opaque（env.schema 默认值）。
	SessionTokenMode string
	// AuthSessionTTL 是 AUTH_SESSION_TTL_SECONDS；为 0 时取 604800 秒。
	AuthSessionTTL time.Duration
	// Now 可注入时钟；nil 时用 time.Now。
	Now func() time.Time
	// Sessions 覆盖不透明会话读取（测试注入）；nil 时用 Redis 实现。
	Sessions SessionReader
}

// SessionReader 读不透明会话。
type SessionReader interface {
	// Read 返回会话记录；不存在返回 ErrSessionNotFound。
	Read(ctx context.Context, sessionID string) (OpaqueSession, error)
}

// ErrSessionNotFound 表示会话不存在或已过期。
var ErrSessionNotFound = errors.New("adminapi: opaque session not found")

// OpaqueSession 是 Redis 里的会话记录（redis-session-store.ts:47-88 的 SessionData）。
type OpaqueSession struct {
	SessionID      string
	KeyFingerprint string
	CredentialType credentialType
	UserID         int64
	UserRole       string
	CreatedAt      int64
	ExpiresAt      int64
}

// AuthGuard 实现 Guard。
type AuthGuard struct {
	pools        *store.Pools
	redis        redis.UniversalClient
	sessions     SessionReader
	logger       *logx.Logger
	problems     ProblemWriter
	adminToken   string
	csrfSecret   string
	apiKeyAdmin  bool
	mode         string
	sessionTTL   time.Duration
	now          func() time.Time
	keys         guard.AuthResolver
	disposeGuard func()
}

// NewAuthGuard 装配管理面守卫。
//
// 复用 guard 的 AuthStore 而不是自带一份密钥 SELECT：密钥解析有「同一条 key 串匹配多行时取
// 最有利状态」「负结果不进缓存」等细节，管理面与数据面必须同口径，两处实现迟早会分叉。
// 代价是整包适配器一起构造（guard 的意图是「一个结构体让接线方一眼看出缺什么」）；本文件
// 只用其中的 Auth 一项，其余保持惰性，不产生查询。
func NewAuthGuard(deps Deps, options GuardOptions) (*AuthGuard, error) {
	if options.Pools == nil {
		return nil, errors.New("adminapi: 管理面守卫需要 store.Pools")
	}
	logger := options.Logger
	if logger == nil {
		logger = deps.Logger
	}
	if logger == nil {
		logger = logx.New(nil)
	}
	problems := options.Problems
	if problems == nil {
		problems = deps.Problems
	}
	if problems == nil {
		problems = NewProblems(nil)
	}

	adapters, err := guard.NewAdapters(guard.AdapterOptions{Pools: options.Pools, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("adminapi: 构造密钥解析器失败: %w", err)
	}

	mode := options.SessionTokenMode
	if mode == "" {
		mode = "opaque"
	}
	switch mode {
	case "legacy", "dual", "opaque":
	default:
		adapters.Close()
		return nil, fmt.Errorf("adminapi: SESSION_TOKEN_MODE 取值非法: %q", mode)
	}

	ttl := options.AuthSessionTTL
	if ttl <= 0 {
		ttl = defaultAuthSessionTTL
	}

	guardImpl := &AuthGuard{
		pools:        options.Pools,
		redis:        options.Redis,
		logger:       logger,
		problems:     problems,
		adminToken:   options.AdminToken,
		csrfSecret:   options.CSRFSecret,
		apiKeyAdmin:  options.EnableAPIKeyAdminAccess,
		mode:         mode,
		sessionTTL:   ttl,
		now:          options.Now,
		keys:         adapters.Auth,
		disposeGuard: adapters.Close,
	}
	if options.Sessions != nil {
		guardImpl.sessions = options.Sessions
	} else if options.Redis != nil {
		guardImpl.sessions = redisSessionReader{client: options.Redis}
	}
	return guardImpl, nil
}

// Close 释放守卫持有的缓存与订阅。
func (g *AuthGuard) Close() {
	if g == nil || g.disposeGuard == nil {
		return
	}
	g.disposeGuard()
	g.disposeGuard = nil
}

// SessionTokenMode 返回生效的会话令牌模式（legacy / dual / opaque）。
//
// 供启动日志如实报告：模式决定「裸密钥能不能当会话用」，而两者都不报错，只表现成登录行为不同。
func (g *AuthGuard) SessionTokenMode() string {
	if g == nil {
		return ""
	}
	return g.mode
}

// Wrap 实现 Guard：认证或授权失败时直接作答，不放行到 next。
func (g *AuthGuard) Wrap(level AccessLevel, next http.Handler) http.Handler {
	if level == AccessPublic {
		// 与 Node 的 resolveAuth(c, "public") 同义（auth-middleware.ts:69-76）：不提取凭据、
		// 不往上下文放身份——处理器拿到的是匿名请求。
		return next
	}
	if level != AccessRead && level != AccessAdmin {
		// 未知档位一律 fail-closed：宁可用 500 暴露装配错误，也不要让一条权限声明被当成
		// 「无需认证」放行。Node 的档位是 OpenAPI 声明，类型系统外不会出现该情形。
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			g.logger.Error("admin_access_level_unknown", map[string]any{
				"level": string(level),
				"path":  request.URL.Path,
			})
			g.problems.WriteProblem(writer, request, http.StatusInternalServerError, "", "")
		})
	}

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, failure, err := g.authenticate(request, level)
		if err != nil {
			// 依赖故障（库/Redis 不可达）：既不放行也不谎报 401，让调用方区分「凭据不对」与
			// 「服务坏了」。
			g.logger.Error("admin_auth_dependency_failed", map[string]any{
				"path":  request.URL.Path,
				"error": err.Error(),
			})
			g.problems.WriteProblem(writer, request, http.StatusInternalServerError, "", "")
			return
		}
		if failure != nil {
			g.problems.WriteProblem(writer, request, failure.status, failure.code, failure.detail)
			return
		}
		next.ServeHTTP(writer, request.WithContext(WithPrincipal(request.Context(), principal)))
	})
}

// authFailure 是一次拒绝的作答内容。
type authFailure struct {
	status int
	code   string
	detail string
}

// authenticate 复刻 resolveAuth（auth-middleware.ts:68-145）。
func (g *AuthGuard) authenticate(request *http.Request, tier AccessLevel) (Principal, *authFailure, error) {
	credential := extractCredential(request)
	if credential.token == "" {
		return Principal{}, &authFailure{
			status: http.StatusUnauthorized,
			code:   "auth.missing",
			detail: "Authentication is required.",
		}, nil
	}

	resolved, err := g.resolveToken(request.Context(), credential)
	if err != nil {
		return Principal{}, nil, err
	}
	if resolved.failure != nil {
		return Principal{}, resolved.failure, nil
	}

	allowReadOnlyAccess := tier == AccessRead
	principal := resolved.principal
	resolutionType := resolved.credentialType

	// validateKey 的公共校验：用户禁用/过期一律失效；未开只读放行时还要求密钥允许登录 Web UI。
	if !resolved.userEnabled {
		return Principal{}, invalidCredentialsFailure(), nil
	}
	if resolved.userExpiresAt != nil && !resolved.userExpiresAt.After(g.clock()) {
		return Principal{}, invalidCredentialsFailure(), nil
	}
	if !allowReadOnlyAccess && !resolved.keyCanLoginWebUI {
		return Principal{}, invalidCredentialsFailure(), nil
	}

	if tier == AccessAdmin {
		if !principal.IsAdmin {
			return Principal{}, &authFailure{
				status: http.StatusForbidden,
				code:   "auth.forbidden",
				detail: "Admin access is required.",
			}, nil
		}
		if resolutionType == credentialUserAPIKey && !g.apiKeyAdmin {
			return Principal{}, &authFailure{
				status: http.StatusForbidden,
				code:   "auth.api_key_admin_disabled",
				detail: "API key admin access is disabled.",
			}, nil
		}
	}

	if credential.source == credentialSourceCookie &&
		isMutationMethod(request.Method) &&
		!g.verifyCSRF(request.Header.Get(csrfHeader), credential.token, principal.UserID) {
		return Principal{}, &authFailure{
			status: http.StatusForbidden,
			code:   "auth.csrf_invalid",
			detail: "CSRF token is missing or invalid.",
		}, nil
	}

	principal.Token = credential.token
	// A1-2：把认证层已经算出但先前未外传的两项交给处理器（见 Principal 字段注释）。
	// canLoginWebUI 取本凭据所用密钥的列（ADMIN_TOKEN 的合成密钥恒 true）；webSession 取
	// 凭据来源。两者都不重新查库，用的是本函数已经解析出来的同一份事实。
	principal.CanLoginWebUI = resolved.keyCanLoginWebUI
	principal.WebSession = credential.source == credentialSourceCookie
	return principal, nil, nil
}

// invalidCredentialsFailure 是 auth.ts:96-101 的 401 形状。
func invalidCredentialsFailure() *authFailure {
	return &authFailure{
		status: http.StatusUnauthorized,
		code:   "auth.invalid",
		detail: "Authentication is invalid or expired.",
	}
}

// credential 是一次请求里提取到的凭据。
type credential struct {
	token  string
	source string
}

// extractCredential 复刻 extractManagementAuthToken（auth-middleware.ts:21-47）。
//
// 顺序是语义：Authorization 头优先于 x-api-key，头优先于 Cookie。因此「同时带 Cookie 与
// 头」的请求不会被当成 Cookie 来源——CSRF 那道门也就不会误伤 CLI。
func extractCredential(request *http.Request) credential {
	if key := bearerToken(request.Header.Get("Authorization")); key != "" {
		return credential{token: key, source: "bearer"}
	}
	if key := strings.TrimSpace(request.Header.Get("x-api-key")); key != "" {
		return credential{token: key, source: "api-key"}
	}
	if token := cookieValue(request.Header.Get("Cookie"), authCookieName); token != "" {
		return credential{token: token, source: credentialSourceCookie}
	}
	return credential{}
}

// bearerToken 复刻 auth-header-extractor.ts:19-23 的 /^Bearer\s+(.+)$/i。
func bearerToken(header string) string {
	trimmed := strings.TrimSpace(header)
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "bearer") {
		return ""
	}
	rest := trimmed[len("bearer"):]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return ""
	}
	return strings.TrimSpace(rest)
}

// cookieValue 复刻 auth-middleware.ts:49-66 的 getAuthCookieFromHeader。
func cookieValue(rawCookie, name string) string {
	for _, pair := range strings.Split(rawCookie, ";") {
		parts := strings.Split(strings.TrimSpace(pair), "=")
		if len(parts) < 2 || parts[0] != name {
			continue
		}
		value := strings.TrimSpace(strings.Join(parts[1:], "="))
		if value == "" {
			return ""
		}
		decoded, err := decodeURIComponent(value)
		if err != nil {
			return ""
		}
		return decoded
	}
	return ""
}

// resolvedPrincipal 是令牌校验的结果。
type resolvedPrincipal struct {
	principal        Principal
	credentialType   credentialType
	userEnabled      bool
	userExpiresAt    *time.Time
	keyCanLoginWebUI bool
	failure          *authFailure
}

// resolveToken 复刻 validateAuthToken（auth.ts:288-336）的四条分支。
func (g *AuthGuard) resolveToken(ctx context.Context, credential credential) (resolvedPrincipal, error) {
	token := credential.token

	// 1) 原始 ADMIN_TOKEN：与 Cookie 无关的最短路径，opaque 模式下同样接受（auth.ts:328-333）。
	if g.adminToken != "" && constantTimeEqual(token, g.adminToken) {
		return g.adminPrincipal(), nil
	}

	// 2) 签名的 admin 会话令牌。legacy 模式下一律拒绝（auth.ts:320-322）。
	if adminauth.IsSignedTokenFormat(token) {
		if g.mode == "legacy" || g.adminToken == "" {
			return resolvedPrincipal{failure: invalidCredentialsFailure()}, nil
		}
		if err := adminauth.VerifySignedToken(token, g.adminToken, g.sessionTTL, g.clock()); err != nil {
			g.logger.Debug("admin_auth_signed_token_rejected", map[string]any{
				"reason": err.Error(),
			})
			return resolvedPrincipal{failure: invalidCredentialsFailure()}, nil
		}
		return g.adminPrincipal(), nil
	}

	// 3) 不透明会话：mode != legacy 时对任意令牌都尝试一次（Node 亦然，不按前缀分流）。
	if g.mode != "legacy" && g.sessions != nil {
		session, err := g.sessions.Read(ctx, token)
		switch {
		case err == nil:
			return g.resolveOpaqueSession(ctx, session, credential)
		case errors.Is(err, ErrSessionNotFound):
			// 未命中是常态（裸 Key、过期会话）：继续往下走 legacy/opaque 分支。
		default:
			// Redis 故障与「会话不存在」必须区分：前者不该被当成凭据错误，
			// 但也不能放行；Node 的做法是记 warn 后按未命中继续（auth.ts:313-317）。
			g.logger.Warn("admin_auth_session_read_failed", map[string]any{
				"error": err.Error(),
			})
		}
	}

	// 4) legacy / dual：令牌就是 API Key（auth.ts:324-326）。
	if g.mode == "legacy" || g.mode == "dual" {
		return g.resolveAPIKey(ctx, token, classifyKeySource(credential))
	}

	// 5) opaque：裸 Key 不被接受（只剩 ADMIN_TOKEN 一条路，已在第 1 步处理）。
	return resolvedPrincipal{failure: invalidCredentialsFailure()}, nil
}

// classifyKeySource 复刻 classifyCredential 的末段（auth.ts:172-177）：Cookie 来源的裸 Key
// 是一次浏览器登录留下的会话，不按「程序化 API Key」处理，因此不受
// ENABLE_API_KEY_ADMIN_ACCESS 那道门限制。
func classifyKeySource(credential credential) credentialType {
	if credential.source == credentialSourceCookie {
		return credentialSession
	}
	return credentialUserAPIKey
}

// resolveAPIKey 用 guard 的密钥解析器取用户与密钥，并补一次角色/登录权限投影。
func (g *AuthGuard) resolveAPIKey(
	ctx context.Context,
	apiKey string,
	credentialType credentialType,
) (resolvedPrincipal, error) {
	resolution, err := g.keys.ResolveAPIKey(ctx, apiKey)
	if err != nil {
		switch {
		case errors.Is(err, guard.ErrKeyNotFound),
			errors.Is(err, guard.ErrKeyDisabled),
			errors.Is(err, guard.ErrKeyExpired):
			return resolvedPrincipal{failure: invalidCredentialsFailure()}, nil
		default:
			return resolvedPrincipal{}, fmt.Errorf("adminapi: 密钥解析失败: %w", err)
		}
	}

	row, err := g.managementRowByKeyID(ctx, resolution.Key.ID)
	if err != nil {
		return resolvedPrincipal{}, err
	}
	if row == nil {
		// 解析到了密钥却查不到管理面投影：说明投影查询与密钥查询口径不一致（例如软删竞态）。
		// 按凭据无效处理，不猜角色。
		return resolvedPrincipal{failure: invalidCredentialsFailure()}, nil
	}

	return resolvedPrincipal{
		principal: Principal{
			UserID:   resolution.User.ID,
			Username: resolution.User.Name,
			IsAdmin:  row.UserRole != nil && *row.UserRole == "admin",
			KeyID:    resolution.Key.ID,
			KeyName:  row.KeyName,
		},
		credentialType:   credentialType,
		userEnabled:      resolution.User.IsEnabled,
		userExpiresAt:    resolution.User.ExpiresAt,
		keyCanLoginWebUI: row.CanLoginWebUI != nil && *row.CanLoginWebUI,
	}, nil
}

// resolveOpaqueSession 复刻 convertToAuthSession（auth.ts:431-458）。
func (g *AuthGuard) resolveOpaqueSession(
	ctx context.Context,
	session OpaqueSession,
	credential credential,
) (resolvedPrincipal, error) {
	if session.ExpiresAt <= g.clock().UnixMilli() {
		g.logger.Warn("admin_auth_opaque_session_expired", map[string]any{
			"userId": session.UserID,
		})
		return resolvedPrincipal{failure: invalidCredentialsFailure()}, nil
	}

	fingerprint := normalizeKeyFingerprint(session.KeyFingerprint)

	// 虚拟管理员用户（id = -1）在库里没有密钥：指纹直接对 ADMIN_TOKEN 验。
	if session.UserID == adminPrincipalUserID {
		if g.adminToken == "" || !constantTimeEqual(fingerprint, keyFingerprint(g.adminToken)) {
			return resolvedPrincipal{failure: invalidCredentialsFailure()}, nil
		}
		return g.adminPrincipal(), nil
	}

	candidates, err := g.keyValuesByUser(ctx, session.UserID)
	if err != nil {
		return resolvedPrincipal{}, err
	}
	for _, candidate := range candidates {
		if !constantTimeEqual(fingerprint, keyFingerprint(candidate.Value)) {
			continue
		}
		resolved, err := g.resolveAPIKey(ctx, candidate.Value, sessionCredentialType(session))
		if err != nil {
			return resolvedPrincipal{}, err
		}
		// 角色只取实时投影，**不用**会话里的 userRole 快照：Node 的 convertToAuthSession 走
		// validateKey（实时读库），会话里的 userRole 仅供契约完整性校验，不参与授权。
		return resolved, nil
	}
	return resolvedPrincipal{failure: invalidCredentialsFailure()}, nil
}

// sessionCredentialType 复刻 parseSessionData 的 credentialType 归一（redis-session-store.ts:69-79）。
func sessionCredentialType(session OpaqueSession) credentialType {
	switch session.CredentialType {
	case credentialSession, credentialAdminToken, credentialUserAPIKey:
		return session.CredentialType
	default:
		if session.UserID == adminPrincipalUserID {
			return credentialAdminToken
		}
		return credentialUserAPIKey
	}
}

// adminPrincipal 是 ADMIN_TOKEN 与签名令牌共用的身份（auth.ts:188-233 的虚拟用户）。
//
// keyCanLoginWebUI 恒为 true：Node 合成密钥时把它置 true，故管理员会话不会被 Web UI 登录门拒绝。
// KeyName 取 Node 合成密钥的 name（auth.ts:206 的 "ADMIN_TOKEN"）——审计的 operator_key_name
// 是逐列对拍的，留空会与 Node 分叉。
func (g *AuthGuard) adminPrincipal() resolvedPrincipal {
	return resolvedPrincipal{
		principal: Principal{
			UserID:   adminPrincipalUserID,
			Username: adminPrincipalName,
			IsAdmin:  true,
			KeyID:    adminPrincipalUserID,
			KeyName:  adminKeyName,
		},
		credentialType:   credentialAdminToken,
		userEnabled:      true,
		keyCanLoginWebUI: true,
	}
}

// verifyCSRF 复刻 csrf.ts:29-49 的 verifyCsrfToken。
func (g *AuthGuard) verifyCSRF(token, authToken string, userID int64) bool {
	if token == "" {
		return false
	}
	bucketText, signature, found := strings.Cut(token, ".")
	if !found || signature == "" {
		return false
	}
	bucket, err := parseCSRFBucket(bucketText)
	if err != nil {
		return false
	}
	current := g.clock().UnixMilli() / csrfWindow.Milliseconds()
	if bucket != current && bucket != current-1 {
		return false
	}
	secret := g.csrfSecret
	if secret == "" {
		secret = g.adminToken
	}
	if secret == "" {
		// csrf.ts:59-61 的末项兜底：没有显式 secret 时用会话令牌本身。
		secret = authToken
	}
	expected := signCSRF(secret, csrfPayload(authToken, userID, bucket))
	return subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) == 1
}

// signCSRF 复刻 signCsrfPayload：HMAC-SHA256 后取 base64url（无填充）。
func signCSRF(secret, payload string) string {
	mac := hmacSHA256([]byte(secret), []byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac)
}

// csrfPayload 复刻 csrf.ts:24 的 `${authToken}:${userId}:${bucket}`。
func csrfPayload(authToken string, userID int64, bucket int64) string {
	return fmt.Sprintf("%s:%d:%d", authToken, userID, bucket)
}

// parseCSRFBucket 复刻 csrf.ts:38-39 的 Number.isInteger(bucketText) 校验。
func parseCSRFBucket(text string) (int64, error) {
	if text == "" {
		return 0, errors.New("adminapi: 空 CSRF 分桶")
	}
	var value int64
	for index := 0; index < len(text); index++ {
		if text[index] < '0' || text[index] > '9' {
			return 0, errors.New("adminapi: CSRF 分桶非整数")
		}
		value = value*10 + int64(text[index]-'0')
	}
	return value, nil
}

// isMutationMethod 复刻 csrf.ts:51-53。
func isMutationMethod(method string) bool {
	switch strings.ToUpper(method) {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}

// keyFingerprint 复刻 auth.ts:419-425 的 toKeyFingerprint。
func keyFingerprint(key string) string {
	digest := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// normalizeKeyFingerprint 复刻 auth.ts:427-429。
func normalizeKeyFingerprint(fingerprint string) string {
	if strings.HasPrefix(fingerprint, "sha256:") {
		return fingerprint
	}
	return "sha256:" + fingerprint
}

// constantTimeEqual 复刻 lib/security/constant-time-compare 的定长比较。
func constantTimeEqual(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

// clock 取当前时间。
func (g *AuthGuard) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// ---- 管理面投影（角色、Web UI 登录权限、按键取值的候选列表） ----

// managementRow 是管理面认证所需的密钥/用户投影。
type managementRow struct {
	KeyID         int64   `json:"key_id"`
	KeyName       string  `json:"key_name"`
	CanLoginWebUI *bool   `json:"can_login_web_ui"`
	UserID        int64   `json:"user_id"`
	UserName      string  `json:"user_name"`
	UserRole      *string `json:"user_role"`
}

// managementRowByKeyID 读某个密钥的管理面投影。
//
// 为什么另开一条查询而不是扩 guard 的 AuthStore：guard 的用户切面刻意只收守卫链真正读的
// 字段（无 role、无 can_login_web_ui），管理面的这两列不属于数据面职责。按主键读一行，
// 无缓存——管理面流量低，且角色变更必须立刻生效（缓存会让「刚删的管理员」多活一个 TTL）。
func (g *AuthGuard) managementRowByKeyID(ctx context.Context, keyID int64) (*managementRow, error) {
	const query = `SELECT row_to_json(t)::text FROM (
		SELECT k.id AS key_id, k.name AS key_name, k.can_login_web_ui,
		       u.id AS user_id, u.name AS user_name, u.role AS user_role
		FROM keys k JOIN users u ON u.id = k.user_id
		WHERE k.id = $1 AND k.deleted_at IS NULL AND u.deleted_at IS NULL
	) t`
	rows, err := queryJSONRows(ctx, g.pools, query, keyID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	var row managementRow
	if err := json.Unmarshal([]byte(rows[0]), &row); err != nil {
		return nil, fmt.Errorf("adminapi: 管理面投影反序列化失败: %w", err)
	}
	return &row, nil
}

// keyValue 是按用户取密钥候选时的一行。
type keyValue struct {
	KeyID int64  `json:"key_id"`
	Value string `json:"key_value"`
}

// keyValuesByUser 读某个用户名下的全部密钥串（复刻 auth.ts:448 的 findKeyList）。
//
// 只用于不透明会话的指纹比对：会话里存的是签发时那把 Key 的 sha256 指纹，必须逐把比对
// 才能找回是哪一把。软删的行不参与（与 findKeyList 的 deleted_at 过滤一致）。
func (g *AuthGuard) keyValuesByUser(ctx context.Context, userID int64) ([]keyValue, error) {
	const query = `SELECT row_to_json(t)::text FROM (
		SELECT k.id AS key_id, k.key AS key_value
		FROM keys k
		WHERE k.user_id = $1 AND k.deleted_at IS NULL
	) t`
	rows, err := queryJSONRows(ctx, g.pools, query, userID)
	if err != nil {
		return nil, err
	}
	candidates := make([]keyValue, 0, len(rows))
	for _, raw := range rows {
		var row keyValue
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("adminapi: 密钥候选反序列化失败: %w", err)
		}
		candidates = append(candidates, row)
	}
	return candidates, nil
}

// queryJSONRows 只读查询（Data 分道），返回每行的 row_to_json 文本。
//
// 用 row_to_json 而不是逐列 Scan：加列不会漏读，列名即 JSON 键名（与 store/guard 的既有一致）。
func queryJSONRows(ctx context.Context, pools *store.Pools, query string, args ...any) ([]string, error) {
	pool, err := pools.Data()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("adminapi: 只读查询失败: %w", err)
	}
	defer rows.Close()

	results := make([]string, 0, 4)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("adminapi: 读取只读行失败: %w", err)
		}
		results = append(results, payload)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("adminapi: 只读遍历失败: %w", err)
	}
	return results, nil
}

// ---- 不透明会话的 Redis 读取 ----

// redisSessionReader 从 Redis 读会话（redis-session-store.ts:14 的 key 布局）。
type redisSessionReader struct {
	client redis.UniversalClient
}

// Read 复刻 RedisSessionStore.read + parseSessionData。
func (r redisSessionReader) Read(ctx context.Context, sessionID string) (OpaqueSession, error) {
	raw, err := r.client.Get(ctx, sessionKeyPrefix+sessionID).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return OpaqueSession{}, ErrSessionNotFound
		}
		return OpaqueSession{}, fmt.Errorf("adminapi: 读会话失败: %w", err)
	}
	var parsed struct {
		SessionID      string  `json:"sessionId"`
		KeyFingerprint string  `json:"keyFingerprint"`
		UserRole       string  `json:"userRole"`
		UserID         int64   `json:"userId"`
		CredentialType string  `json:"credentialType"`
		CreatedAt      float64 `json:"createdAt"`
		ExpiresAt      float64 `json:"expiresAt"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return OpaqueSession{}, ErrSessionNotFound
	}
	if parsed.SessionID == "" || parsed.KeyFingerprint == "" || parsed.UserRole == "" {
		return OpaqueSession{}, ErrSessionNotFound
	}
	if parsed.CreatedAt <= 0 || parsed.ExpiresAt <= 0 {
		return OpaqueSession{}, ErrSessionNotFound
	}
	credential := credentialType(parsed.CredentialType)
	if credential != credentialSession && credential != credentialAdminToken &&
		credential != credentialUserAPIKey {
		if parsed.UserID == adminPrincipalUserID {
			credential = credentialAdminToken
		} else {
			credential = credentialSession
		}
	}
	return OpaqueSession{
		SessionID:      parsed.SessionID,
		KeyFingerprint: parsed.KeyFingerprint,
		CredentialType: credential,
		UserID:         parsed.UserID,
		UserRole:       parsed.UserRole,
		CreatedAt:      int64(parsed.CreatedAt),
		ExpiresAt:      int64(parsed.ExpiresAt),
	}, nil
}

// ---- 身份上下文 ----

// hmacSHA256 是 CSRF 派生用的 HMAC-SHA256。
func hmacSHA256(key, payload []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return mac.Sum(nil)
}

// decodeURIComponent 复刻 JS 的 decodeURIComponent：%XX 与 UTF-8 序列，非法序列报错。
//
// 非法序列必须报错而不是原样返回：Node 侧对非法 cookie 值返回 undefined，也就是「没有这个
// cookie」。原样返回会让一个残缺的百分号序列冒充成令牌。
func decodeURIComponent(value string) (string, error) {
	if !strings.ContainsRune(value, '%') {
		return value, nil
	}
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character != '%' {
			builder.WriteByte(character)
			continue
		}
		if index+2 >= len(value) {
			return "", errors.New("adminapi: cookie 百分号转义不完整")
		}
		high, okHigh := hexDigit(value[index+1])
		low, okLow := hexDigit(value[index+2])
		if !okHigh || !okLow {
			return "", errors.New("adminapi: cookie 百分号转义非十六进制")
		}
		builder.WriteByte(high<<4 | low)
		index += 2
	}
	decoded := builder.String()
	if !utf8.ValidString(decoded) {
		return "", errors.New("adminapi: cookie 值不是合法 UTF-8")
	}
	return decoded, nil
}

// hexDigit 解析一个十六进制字符。
func hexDigit(character byte) (byte, bool) {
	switch {
	case character >= '0' && character <= '9':
		return character - '0', true
	case character >= 'a' && character <= 'f':
		return character - 'a' + 10, true
	case character >= 'A' && character <= 'F':
		return character - 'A' + 10, true
	default:
		return 0, false
	}
}

// principalContextKey 是身份在请求上下文里的键。
type principalContextKey struct{}

// WithPrincipal 把已认证身份放进上下文。
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

// PrincipalFrom 取回已认证身份；未经守卫的请求返回零值与 false。
//
// 资源模块（A1）用它在审计与失效归属里取操作人，不得自己再解析一次凭据。
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}
