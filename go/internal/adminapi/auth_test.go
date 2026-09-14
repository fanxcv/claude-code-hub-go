package adminapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminauth"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件的测试分两类：
//   - 不需要数据库的：用 &store.Pools{} 占位（守卫在走到密钥解析前就返回），覆盖凭据提取、
//     签名令牌、CSRF、档位与 fail-closed 语义。
//   - 需要数据库的：门控 CCH_TEST_DSN，真建 users/keys 行，覆盖密钥解析与档位判定。

const testAdminToken = "test-admin-token-0123456789"

// testClock 固定时钟：CSRF 分桶与令牌过期判定都需要可确定的时间。
var testClock = time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)

// ---- 夹具 ----

// testPools 取真库连接池；未设置 CCH_TEST_DSN 时跳过。
func testPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-adminapi-it",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// fixtureUser 建一个用户，返回 id；测试结束时删除。
func fixtureUser(t *testing.T, pools *store.Pools, role string, isEnabled bool) int64 {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	name := "go-adminapi-it-" + time.Now().Format("20060102150405.000000000")
	var userID int64
	err = pool.QueryRow(context.Background(),
		`INSERT INTO users (name, role, is_enabled) VALUES ($1, $2, $3) RETURNING id`,
		name, role, isEnabled,
	).Scan(&userID)
	if err != nil {
		t.Fatalf("建用户夹具失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(context.Background(), `DELETE FROM keys WHERE user_id = $1`, userID)
		_, _ = cleanupPool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
	return userID
}

// fixtureKeyOptions 是密钥夹具的参数。
type fixtureKeyOptions struct {
	userID        int64
	canLoginWebUI bool
	isEnabled     bool
	expiresAt     *time.Time
}

// fixtureKey 建一把密钥，返回 id 与密钥原文。
func fixtureKey(t *testing.T, pools *store.Pools, options fixtureKeyOptions) (int64, string) {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	keyValue := "go-adminapi-it-key-" + time.Now().Format("20060102150405.000000000") + "-" +
		fmt.Sprint(time.Now().UnixNano()%1000000)
	var keyID int64
	err = pool.QueryRow(context.Background(),
		`INSERT INTO keys (user_id, key, name, is_enabled, can_login_web_ui, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		options.userID, keyValue, "go-adminapi-it", options.isEnabled, options.canLoginWebUI, options.expiresAt,
	).Scan(&keyID)
	if err != nil {
		t.Fatalf("建密钥夹具失败: %v", err)
	}
	return keyID, keyValue
}

// ----- 守卫驱动器 -----

// guardResult 一次请求的结果。
type guardResult struct {
	status    int
	body      map[string]any
	passed    bool
	principal Principal
}

// newTestGuard 建一个守卫；options 里未给的部分补测试默认值。
func newTestGuard(t *testing.T, options GuardOptions) *AuthGuard {
	t.Helper()
	if options.Pools == nil {
		options.Pools = &store.Pools{}
	}
	if options.AdminToken == "" {
		options.AdminToken = testAdminToken
	}
	if options.SessionTokenMode == "" {
		options.SessionTokenMode = "opaque"
	}
	if options.Now == nil {
		options.Now = func() time.Time { return testClock }
	}
	guard, err := NewAuthGuard(Deps{}, options)
	if err != nil {
		t.Fatalf("装配守卫失败: %v", err)
	}
	t.Cleanup(guard.Close)
	return guard
}

// drive 跑一次请求并返回结果。
func drive(t *testing.T, guard Guard, level AccessLevel, method, target string, headers map[string]string) guardResult {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()

	result := guardResult{}
	handler := guard.Wrap(level, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		result.passed = true
		result.principal, _ = PrincipalFrom(request.Context())
		writer.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(recorder, request)

	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()
	result.status = response.StatusCode
	if response.Body != nil {
		_ = json.NewDecoder(response.Body).Decode(&result.body)
	}
	return result
}

// csrfFor 独立复刻 CSRF 派生（按 csrf.ts 的公式手写，不复用被测代码）。
func csrfFor(authToken, secret string, userID int64, now time.Time) string {
	bucket := now.UnixMilli() / (30 * time.Minute).Milliseconds()
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s:%d:%d", authToken, userID, bucket)
	return fmt.Sprintf("%d.%s", bucket, base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
}

// fakeSessions 是固定返回一条记录的会话读取器。
type fakeSessions struct {
	session OpaqueSession
	err     error
}

func (f fakeSessions) Read(context.Context, string) (OpaqueSession, error) {
	if f.err != nil {
		return OpaqueSession{}, f.err
	}
	if f.session.SessionID == "" {
		return OpaqueSession{}, ErrSessionNotFound
	}
	return f.session, nil
}

// ---- 无数据库的用例 ----

// TestAuthMissingCredential 复刻 auth-middleware.ts:80-87。
func TestAuthMissingCredential(t *testing.T) {
	result := drive(t, newTestGuard(t, GuardOptions{}), AccessRead, http.MethodGet, "/api/v1/keys", nil)
	if result.status != http.StatusUnauthorized || result.body["errorCode"] != "auth.missing" {
		t.Fatalf("缺凭据应 401 auth.missing: status=%d body=%v", result.status, result.body)
	}
	if result.body["detail"] != "Authentication is required." || result.body["instance"] != "/api/v1/keys" {
		t.Fatalf("信封字段与 Node 不一致: %v", result.body)
	}
	if result.passed {
		t.Fatal("认证失败不得放行到 next")
	}
}

// TestAuthSignedCookieValid 验证签名 admin 会话令牌（auth.ts:362-372）。
func TestAuthSignedCookieValid(t *testing.T) {
	token, err := adminauth.CreateSignedToken(testAdminToken, time.Hour, testClock)
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}
	result := drive(t, newTestGuard(t, GuardOptions{}), AccessAdmin, http.MethodGet, "/api/v1/keys",
		map[string]string{"Cookie": authCookieName + "=" + token})
	if result.status != http.StatusOK || !result.passed {
		t.Fatalf("有效签名令牌应放行: status=%d body=%v", result.status, result.body)
	}
	if result.principal.UserID != -1 || !result.principal.IsAdmin || result.principal.KeyID != -1 {
		t.Fatalf("管理员身份字段不符: %+v", result.principal)
	}
	if result.principal.Username != "Admin Token" {
		t.Fatalf("管理员用户名不符: %q", result.principal.Username)
	}
}

// TestAuthSignedCookieInvalid 与过期。
func TestAuthSignedCookieInvalid(t *testing.T) {
	guard := newTestGuard(t, GuardOptions{})
	invalid := drive(t, guard, AccessAdmin, http.MethodGet, "/api/v1/keys",
		map[string]string{"Cookie": authCookieName + "=cch_admin_session_v1.abc.def"})
	if invalid.status != http.StatusUnauthorized || invalid.body["errorCode"] != "auth.invalid" {
		t.Fatalf("签名不匹配应 401 auth.invalid: %d %v", invalid.status, invalid.body)
	}

	expiredToken, err := adminauth.CreateSignedToken(testAdminToken, time.Hour, testClock.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}
	expired := drive(t, guard, AccessAdmin, http.MethodGet, "/api/v1/keys",
		map[string]string{"Cookie": authCookieName + "=" + expiredToken})
	if expired.status != http.StatusUnauthorized || expired.body["errorCode"] != "auth.invalid" {
		t.Fatalf("过期令牌应 401 auth.invalid: %d %v", expired.status, expired.body)
	}
}

// TestAuthRawAdminTokenViaBearer 验证裸 ADMIN_TOKEN（auth.ts:328-333）。
func TestAuthRawAdminTokenViaBearer(t *testing.T) {
	result := drive(t, newTestGuard(t, GuardOptions{}), AccessAdmin, http.MethodGet, "/api/v1/keys",
		map[string]string{"Authorization": "Bearer " + testAdminToken})
	if result.status != http.StatusOK || !result.principal.IsAdmin {
		t.Fatalf("裸 ADMIN_TOKEN 应放行: %d %v", result.status, result.body)
	}
	// 合成密钥的 name 也要与 Node 一致（auth.ts:206）：审计的 operator_key_name 逐列对拍。
	if result.principal.KeyName != "ADMIN_TOKEN" || result.principal.KeyID != -1 {
		t.Fatalf("合成密钥身份不符: %+v", result.principal)
	}
}

// TestAuthLegacyModeRejectsSignedToken 复刻 auth.ts:320-322。
func TestAuthLegacyModeRejectsSignedToken(t *testing.T) {
	token, err := adminauth.CreateSignedToken(testAdminToken, time.Hour, testClock)
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}
	result := drive(t, newTestGuard(t, GuardOptions{SessionTokenMode: "legacy"}), AccessRead,
		http.MethodGet, "/api/v1/keys", map[string]string{"Cookie": authCookieName + "=" + token})
	if result.status != http.StatusUnauthorized || result.body["errorCode"] != "auth.invalid" {
		t.Fatalf("legacy 模式应拒绝签名令牌: %d %v", result.status, result.body)
	}
}

// TestAuthOpaqueModeRejectsRawKey 验证 opaque 模式下裸 Key 不被当会话（auth.ts:328-335）。
func TestAuthOpaqueModeRejectsRawKey(t *testing.T) {
	guard := newTestGuard(t, GuardOptions{
		SessionTokenMode: "opaque",
		Sessions:         fakeSessions{}, // 一律未命中
	})
	result := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys",
		map[string]string{"x-api-key": "sk-raw-key-not-a-session"})
	if result.status != http.StatusUnauthorized || result.body["errorCode"] != "auth.invalid" {
		t.Fatalf("opaque 模式应拒绝裸 Key: %d %v", result.status, result.body)
	}
}

// TestAuthCSRF GatedForCookieMutations 复刻 auth-middleware.ts:121-136。
func TestAuthCSRFGatedForCookieMutations(t *testing.T) {
	guard := newTestGuard(t, GuardOptions{CSRFSecret: "test-csrf-secret-0123456789"})

	missing := drive(t, guard, AccessRead, http.MethodPost, "/api/v1/keys",
		map[string]string{"Cookie": authCookieName + "=" + testAdminToken})
	if missing.status != http.StatusForbidden || missing.body["errorCode"] != "auth.csrf_invalid" {
		t.Fatalf("cookie 变更请求缺 CSRF 应 403: %d %v", missing.status, missing.body)
	}

	valid := drive(t, guard, AccessRead, http.MethodPost, "/api/v1/keys", map[string]string{
		"Cookie":   authCookieName + "=" + testAdminToken,
		csrfHeader: csrfFor(testAdminToken, "test-csrf-secret-0123456789", -1, testClock),
	})
	if valid.status != http.StatusOK {
		t.Fatalf("合法 CSRF 应放行: %d %v", valid.status, valid.body)
	}

	// 上一分桶仍接受（csrf.ts:42），上上分桶拒绝。
	previousBucket := drive(t, guard, AccessRead, http.MethodPost, "/api/v1/keys", map[string]string{
		"Cookie":   authCookieName + "=" + testAdminToken,
		csrfHeader: csrfFor(testAdminToken, "test-csrf-secret-0123456789", -1, testClock.Add(-30*time.Minute)),
	})
	if previousBucket.status != http.StatusOK {
		t.Fatalf("上一分桶应接受: %d %v", previousBucket.status, previousBucket.body)
	}
	stale := drive(t, guard, AccessRead, http.MethodPost, "/api/v1/keys", map[string]string{
		"Cookie":   authCookieName + "=" + testAdminToken,
		csrfHeader: csrfFor(testAdminToken, "test-csrf-secret-0123456789", -1, testClock.Add(-90*time.Minute)),
	})
	if stale.status != http.StatusForbidden || stale.body["errorCode"] != "auth.csrf_invalid" {
		t.Fatalf("过期分桶应拒绝: %d %v", stale.status, stale.body)
	}
}

// TestAuthCSRFNotRequiredForHeaderSource 验证头来源不触发 CSRF（CSRF 只对 cookie 来源生效）。
func TestAuthCSRFNotRequiredForHeaderSource(t *testing.T) {
	result := drive(t, newTestGuard(t, GuardOptions{}), AccessRead, http.MethodPost, "/api/v1/keys",
		map[string]string{"Authorization": "Bearer " + testAdminToken})
	if result.status != http.StatusOK || !result.passed {
		t.Fatalf("头来源不需要 CSRF: %d %v", result.status, result.body)
	}
}

// TestAuthUnknownAccessLevelFailsClosed 验证未知档位不可能是「无需认证」。
//
// 用 "superuser" 而不是 "public"：后者是 Node 的合法档位（AuthTier），A0-3 起由本守卫当作
// 匿名放行档处理，见 TestAuthPublicLevelSkipsAuthentication。
func TestAuthUnknownAccessLevelFailsClosed(t *testing.T) {
	result := drive(t, newTestGuard(t, GuardOptions{}), AccessLevel("superuser"), http.MethodGet, "/api/v1/keys", nil)
	if result.status != http.StatusInternalServerError || result.body["errorCode"] != "internal.error" {
		t.Fatalf("未知档位应 fail-closed 500: %d %v", result.status, result.body)
	}
	if result.passed {
		t.Fatal("未知档位不得放行")
	}
}

// TestAuthPublicLevelSkipsAuthentication 验证 public 档位不认证、不放身份
// （auth-middleware.ts:69-76：public 直接返回空身份，连凭据都不提取）。
func TestAuthPublicLevelSkipsAuthentication(t *testing.T) {
	result := drive(t, newTestGuard(t, GuardOptions{}), AccessPublic, http.MethodGet, "/api/v1/health", nil)
	if result.status != http.StatusOK || !result.passed {
		t.Fatalf("public 档位应直接放行: %d %v", result.status, result.body)
	}
	if result.principal.Token != "" || result.principal.IsAdmin || result.principal.UserID != 0 {
		t.Fatalf("public 档位不该产生身份: %+v", result.principal)
	}
}

// TestAuthDependencyFailureIs500 验证依赖故障不谎报 401。
//
// 用「已关闭的连接池」制造依赖故障：这是真库门控用例（非库用例拿不到合法的 Pools）。
func TestAuthDependencyFailureIs500(t *testing.T) {
	pools := testPools(t)
	if err := pools.Close(); err != nil {
		t.Fatalf("关闭连接池失败: %v", err)
	}
	guard := newTestGuard(t, GuardOptions{Pools: pools, SessionTokenMode: "dual"})
	result := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys",
		map[string]string{"x-api-key": "sk-anything"})
	if result.status != http.StatusInternalServerError {
		t.Fatalf("依赖故障应 500: %d %v", result.status, result.body)
	}
}

// TestAuthExtractCredentialOrder 验证提取顺序与来源判定（auth-middleware.ts:21-47）。
func TestAuthExtractCredentialOrder(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    credential
	}{
		{"bearer 优先", map[string]string{"Authorization": "Bearer  aaa", "x-api-key": "bbb"}, credential{"aaa", "bearer"}},
		{"bearer 非法回退 x-api-key", map[string]string{"Authorization": "Token aaa", "x-api-key": "bbb"}, credential{"bbb", "api-key"}},
		{"只有 cookie", map[string]string{"Cookie": "other=1; " + authCookieName + "=ccc"}, credential{"ccc", "cookie"}},
		{"cookie 百分号解码", map[string]string{"Cookie": authCookieName + "=a%20b"}, credential{"a b", "cookie"}},
		{"非法百分号视为无 cookie", map[string]string{"Cookie": authCookieName + "=a%2"}, credential{}},
		{"空 cookie 值视为无", map[string]string{"Cookie": authCookieName + "="}, credential{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/keys", nil)
			for name, value := range testCase.headers {
				request.Header.Set(name, value)
			}
			if got := extractCredential(request); got != testCase.want {
				t.Fatalf("extractCredential=%+v, 期望 %+v", got, testCase.want)
			}
		})
	}
}

// ---- 数据库用例 ----

// TestAuthSessionTokenModeBareKeyMatrix 是三种 SESSION_TOKEN_MODE 下「裸密钥能不能当会话」的
// 真库矩阵（auth.ts:320-335）。
//
// 为什么值得单独一条：三个模式都不报错，只表现成登录行为不同（opaque 之后浏览器只持 sid_，
// 裸 Key 不再当会话用）；把某个模式误配成 dual 等于让一枚长期有效的密钥绕过登录。
// 矩阵里每个模式都走同一条真实路径（真库密钥 + x-api-key 头 + read 档），只换模式。
func TestAuthSessionTokenModeBareKeyMatrix(t *testing.T) {
	pools := testPools(t)
	userID := fixtureUser(t, pools, "user", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: true, isEnabled: true,
	})

	cases := []struct {
		mode       string
		wantStatus int
		wantCode   string
	}{
		// legacy / dual 都仍然接受裸密钥（auth.ts:324-326）。
		{mode: "legacy", wantStatus: http.StatusOK},
		{mode: "dual", wantStatus: http.StatusOK},
		// opaque 不接受：拒绝形状与其它凭据无效完全一致。
		{mode: "opaque", wantStatus: http.StatusUnauthorized, wantCode: "auth.invalid"},
	}
	for _, testCase := range cases {
		t.Run(testCase.mode, func(t *testing.T) {
			guard := newTestGuard(t, GuardOptions{
				Pools:            pools,
				SessionTokenMode: testCase.mode,
				Sessions:         fakeSessions{}, // 一律未命中：本矩阵只看裸密钥分支
			})
			result := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys",
				map[string]string{"x-api-key": keyValue})
			if result.status != testCase.wantStatus {
				t.Fatalf("%s 模式下裸密钥应 %d，收到 %d（%v）",
					testCase.mode, testCase.wantStatus, result.status, result.body)
			}
			if testCase.wantCode != "" && result.body["errorCode"] != testCase.wantCode {
				t.Fatalf("%s 模式下拒绝形状不符: %v", testCase.mode, result.body)
			}
			if testCase.wantStatus == http.StatusOK && result.principal.UserID != userID {
				t.Fatalf("%s 模式下身份未映射到密钥主人: %+v", testCase.mode, result.principal)
			}
		})
	}
}

// TestAuthAPIKeyReadAndAdminTiers 覆盖 Bearer / X-Api-Key 两条路径与两档权限。
func TestAuthAPIKeyReadAndAdminTiers(t *testing.T) {
	pools := testPools(t)
	userID := fixtureUser(t, pools, "user", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: true, isEnabled: true,
	})
	guard := newTestGuard(t, GuardOptions{Pools: pools, SessionTokenMode: "dual"})

	viaBearer := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys",
		map[string]string{"Authorization": "Bearer " + keyValue})
	if viaBearer.status != http.StatusOK || !viaBearer.passed {
		t.Fatalf("Bearer 有效密钥应放行: %d %v", viaBearer.status, viaBearer.body)
	}
	if viaBearer.principal.UserID != userID || viaBearer.principal.KeyID != keyID || viaBearer.principal.IsAdmin {
		t.Fatalf("身份字段不符: %+v", viaBearer.principal)
	}
	// operator_key_name 取实时投影（审计列需要）：夹具的 name 是固定串。
	if viaBearer.principal.KeyName != "go-adminapi-it" {
		t.Fatalf("密钥名应取实时投影: %+v", viaBearer.principal)
	}

	viaHeaderKey := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys",
		map[string]string{"x-api-key": keyValue})
	if viaHeaderKey.status != http.StatusOK {
		t.Fatalf("x-api-key 有效密钥应放行: %d %v", viaHeaderKey.status, viaHeaderKey.body)
	}

	forbidden := drive(t, guard, AccessAdmin, http.MethodGet, "/api/v1/keys",
		map[string]string{"x-api-key": keyValue})
	if forbidden.status != http.StatusForbidden || forbidden.body["errorCode"] != "auth.forbidden" {
		t.Fatalf("非管理员访问 admin 档应 403 auth.forbidden: %d %v", forbidden.status, forbidden.body)
	}
}

// TestAuthAPIKeyAdminGate 覆盖 ENABLE_API_KEY_ADMIN_ACCESS（auth-middleware.ts:112-119）。
func TestAuthAPIKeyAdminGate(t *testing.T) {
	pools := testPools(t)
	userID := fixtureUser(t, pools, "admin", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: true, isEnabled: true,
	})

	disabled := drive(t, newTestGuard(t, GuardOptions{Pools: pools, SessionTokenMode: "dual", EnableAPIKeyAdminAccess: false}),
		AccessAdmin, http.MethodGet, "/api/v1/keys", map[string]string{"x-api-key": keyValue})
	if disabled.status != http.StatusForbidden || disabled.body["errorCode"] != "auth.api_key_admin_disabled" {
		t.Fatalf("开关关闭时应 403 auth.api_key_admin_disabled: %d %v", disabled.status, disabled.body)
	}

	enabled := drive(t, newTestGuard(t, GuardOptions{Pools: pools, SessionTokenMode: "dual", EnableAPIKeyAdminAccess: true}),
		AccessAdmin, http.MethodGet, "/api/v1/keys", map[string]string{"x-api-key": keyValue})
	if enabled.status != http.StatusOK || !enabled.principal.IsAdmin {
		t.Fatalf("开关打开时应放行: %d %v", enabled.status, enabled.body)
	}
}

// TestAuthWebUILoginGate 覆盖 canLoginWebUi（auth.ts:253-255）。
func TestAuthWebUILoginGate(t *testing.T) {
	pools := testPools(t)
	userID := fixtureUser(t, pools, "admin", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: false, isEnabled: true,
	})
	guard := newTestGuard(t, GuardOptions{Pools: pools, SessionTokenMode: "dual", EnableAPIKeyAdminAccess: true})

	readTier := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys", map[string]string{"x-api-key": keyValue})
	if readTier.status != http.StatusOK {
		t.Fatalf("read 档不要求 canLoginWebUi: %d %v", readTier.status, readTier.body)
	}

	adminTier := drive(t, guard, AccessAdmin, http.MethodGet, "/api/v1/keys", map[string]string{"x-api-key": keyValue})
	if adminTier.status != http.StatusUnauthorized || adminTier.body["errorCode"] != "auth.invalid" {
		t.Fatalf("admin 档缺 canLoginWebUi 应 401 auth.invalid: %d %v", adminTier.status, adminTier.body)
	}
}

// TestAuthDisabledUserAndExpiredKey 覆盖用户禁用与密钥过期。
func TestAuthDisabledUserAndExpiredKey(t *testing.T) {
	pools := testPools(t)
	disabledUserID := fixtureUser(t, pools, "admin", false)
	_, disabledKey := fixtureKey(t, pools, fixtureKeyOptions{
		userID: disabledUserID, canLoginWebUI: true, isEnabled: true,
	})

	activeUserID := fixtureUser(t, pools, "admin", true)
	// 用真实时间而不是测试时钟：密钥过期由 guard 的 AuthStore 判定（它用 time.Now()）。
	past := time.Now().Add(-time.Hour)
	_, expiredKey := fixtureKey(t, pools, fixtureKeyOptions{
		userID: activeUserID, canLoginWebUI: true, isEnabled: true, expiresAt: &past,
	})

	guard := newTestGuard(t, GuardOptions{Pools: pools, SessionTokenMode: "dual"})
	disabled := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys", map[string]string{"x-api-key": disabledKey})
	if disabled.status != http.StatusUnauthorized {
		t.Fatalf("禁用用户应 401: %d %v", disabled.status, disabled.body)
	}
	expired := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys", map[string]string{"x-api-key": expiredKey})
	if expired.status != http.StatusUnauthorized {
		t.Fatalf("过期密钥应 401: %d %v", expired.status, expired.body)
	}
}

// TestAuthOpaqueSession 覆盖不透明会话：指纹命中放行、角色取实时投影、过期拒绝。
func TestAuthOpaqueSession(t *testing.T) {
	pools := testPools(t)
	userID := fixtureUser(t, pools, "user", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: true, isEnabled: true,
	})

	session := OpaqueSession{
		SessionID:      "sid_test",
		KeyFingerprint: keyFingerprint(keyValue),
		CredentialType: credentialSession,
		UserID:         userID,
		// 会话快照里写着 admin：必须以库里的 role 为准，不能被快照提权。
		UserRole:  "admin",
		CreatedAt: testClock.Add(-time.Hour).UnixMilli(),
		ExpiresAt: testClock.Add(time.Hour).UnixMilli(),
	}
	guard := newTestGuard(t, GuardOptions{Pools: pools, Sessions: fakeSessions{session: session}})

	read := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys",
		map[string]string{"Cookie": authCookieName + "=sid_test"})
	if read.status != http.StatusOK || read.principal.KeyID != keyID {
		t.Fatalf("不透明会话应放行并解出密钥 id: %d %v %+v", read.status, read.body, read.principal)
	}
	if read.principal.IsAdmin {
		t.Fatal("会话快照声称 admin 而库里是 user：不得提权")
	}

	admin := drive(t, guard, AccessAdmin, http.MethodGet, "/api/v1/keys",
		map[string]string{"Cookie": authCookieName + "=sid_test"})
	if admin.status != http.StatusForbidden {
		t.Fatalf("库里非管理员访问 admin 档应 403: %d %v", admin.status, admin.body)
	}

	expired := session
	expired.ExpiresAt = testClock.Add(-time.Minute).UnixMilli()
	expiredGuard := newTestGuard(t, GuardOptions{Pools: pools, Sessions: fakeSessions{session: expired}})
	expiredResult := drive(t, expiredGuard, AccessRead, http.MethodGet, "/api/v1/keys",
		map[string]string{"Cookie": authCookieName + "=sid_test"})
	if expiredResult.status != http.StatusUnauthorized {
		t.Fatalf("过期不透明会话应 401: %d %v", expiredResult.status, expiredResult.body)
	}

	// 指纹不匹配：同一用户下的另一把密钥也会被逐一试过，全不匹配即拒绝。
	otherFingerprint := session
	otherFingerprint.KeyFingerprint = keyFingerprint("sk-not-this-user-key")
	otherGuard := newTestGuard(t, GuardOptions{Pools: pools, Sessions: fakeSessions{session: otherFingerprint}})
	otherResult := drive(t, otherGuard, AccessRead, http.MethodGet, "/api/v1/keys",
		map[string]string{"Cookie": authCookieName + "=sid_test"})
	if otherResult.status != http.StatusUnauthorized {
		t.Fatalf("指纹不匹配应 401: %d %v", otherResult.status, otherResult.body)
	}
}

// TestAuthOpaqueSessionAdminFingerprint 覆盖 userId = -1 的虚拟管理员会话（auth.ts:439-446）。
func TestAuthOpaqueSessionAdminFingerprint(t *testing.T) {
	session := OpaqueSession{
		SessionID:      "sid_admin",
		KeyFingerprint: keyFingerprint(testAdminToken),
		CredentialType: credentialAdminToken,
		UserID:         -1,
		UserRole:       "admin",
		CreatedAt:      testClock.Add(-time.Hour).UnixMilli(),
		ExpiresAt:      testClock.Add(time.Hour).UnixMilli(),
	}
	guard := newTestGuard(t, GuardOptions{Sessions: fakeSessions{session: session}})
	result := drive(t, guard, AccessAdmin, http.MethodGet, "/api/v1/keys",
		map[string]string{"Cookie": authCookieName + "=sid_admin"})
	if result.status != http.StatusOK || !result.principal.IsAdmin || result.principal.UserID != -1 {
		t.Fatalf("管理员指纹应放行: %d %v %+v", result.status, result.body, result.principal)
	}

	wrongFingerprint := session
	wrongFingerprint.KeyFingerprint = keyFingerprint("not-the-admin-token")
	wrongGuard := newTestGuard(t, GuardOptions{Sessions: fakeSessions{session: wrongFingerprint}})
	wrong := drive(t, wrongGuard, AccessAdmin, http.MethodGet, "/api/v1/keys",
		map[string]string{"Cookie": authCookieName + "=sid_admin"})
	if wrong.status != http.StatusUnauthorized {
		t.Fatalf("指纹不符应 401: %d %v", wrong.status, wrong.body)
	}
}

// TestAuthSessionReadFailureFallsThrough 验证 Redis 故障按「未命中」继续，而不是当成凭据错误。
func TestAuthSessionReadFailureFallsThrough(t *testing.T) {
	pools := testPools(t)
	if err := pools.Close(); err != nil {
		t.Fatalf("关闭连接池失败: %v", err)
	}
	guard := newTestGuard(t, GuardOptions{
		Pools:            pools,
		SessionTokenMode: "dual",
		Sessions:         fakeSessions{err: errors.New("redis 不可达")},
	})
	// dual 模式下未命中会继续走 API Key 路径；此处库也不可用，故 500 而不是 401——
	// 关键是它**没有**在会话读失败处就地拒绝。
	result := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys",
		map[string]string{"Cookie": authCookieName + "=sid_whatever"})
	if result.status != http.StatusInternalServerError {
		t.Fatalf("会话读失败应继续到密钥路径（依赖故障 500）: %d %v", result.status, result.body)
	}
}

// TestAuthDualModeCookieRawKeySkipsApiKeyAdminGate 复刻 auth.ts:172-177：
// cookie 来源的裸 Key 是浏览器会话，不受 ENABLE_API_KEY_ADMIN_ACCESS 限制。
func TestAuthDualModeCookieRawKeySkipsApiKeyAdminGate(t *testing.T) {
	pools := testPools(t)
	userID := fixtureUser(t, pools, "admin", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: true, isEnabled: true,
	})
	guard := newTestGuard(t, GuardOptions{
		Pools:            pools,
		SessionTokenMode: "dual",
		Sessions:         fakeSessions{},
	})

	result := drive(t, guard, AccessAdmin, http.MethodPost, "/api/v1/keys", map[string]string{
		"Cookie":   authCookieName + "=" + keyValue,
		csrfHeader: csrfFor(keyValue, testAdminToken, userID, testClock),
	})
	if result.status != http.StatusOK {
		t.Fatalf("dual 模式 cookie 裸 Key 应放行（不受 api key admin 开关限制）: %d %v", result.status, result.body)
	}
}

// TestAuthModeValidation 验证非法 SESSION_TOKEN_MODE 在构造期失败。
func TestAuthModeValidation(t *testing.T) {
	if _, err := NewAuthGuard(Deps{}, GuardOptions{Pools: &store.Pools{}, SessionTokenMode: "nonsense"}); err == nil {
		t.Fatal("非法 SESSION_TOKEN_MODE 应在构造期失败")
	}
	if _, err := NewAuthGuard(Deps{}, GuardOptions{}); err == nil {
		t.Fatal("缺 store.Pools 应在构造期失败")
	}
}
