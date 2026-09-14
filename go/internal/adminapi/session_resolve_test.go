package adminapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminauth"
)

// 本文件是只读会话解析入口的**无依赖**用例：解析分支、cookie 语义、以及「未登录」与
// 「依赖故障」的区分。真库与真 Redis 的部分在 session_resolve_integration_test.go。

// resolve 跑一次解析。
func resolve(t *testing.T, guard *AuthGuard, cookie string) (*SessionSnapshot, error) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/zh-CN/dashboard", nil)
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	return guard.ResolveSession(context.Background(), request)
}

// ADMIN_TOKEN 的三种 cookie 形态：裸令牌、签名令牌解析成管理员；签名令牌在 legacy 档被拒。
func TestResolveSessionAdminTokenBranches(t *testing.T) {
	signed, err := adminauth.CreateSignedToken(testAdminToken, time.Hour, testClock)
	if err != nil {
		t.Fatalf("签发测试令牌失败: %v", err)
	}
	expired, err := adminauth.CreateSignedToken(testAdminToken, time.Hour, testClock.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("签发过期令牌失败: %v", err)
	}

	cases := []struct {
		name   string
		guard  GuardOptions
		cookie string
		want   bool
	}{
		{"无 cookie", GuardOptions{}, "", false},
		{"其他 cookie", GuardOptions{}, "other=value", false},
		{"空值", GuardOptions{}, authCookieName + "=", false},
		{"裸 ADMIN_TOKEN", GuardOptions{}, authCookieName + "=" + testAdminToken, true},
		{"签名令牌", GuardOptions{}, authCookieName + "=" + signed, true},
		{"过期签名令牌", GuardOptions{}, authCookieName + "=" + expired, false},
		{"伪造签名令牌", GuardOptions{}, authCookieName + "=" + signed + "x", false},
		{"legacy 档拒签名令牌", GuardOptions{SessionTokenMode: "legacy"}, authCookieName + "=" + signed, false},
		{"legacy 档仍收裸 ADMIN_TOKEN", GuardOptions{SessionTokenMode: "legacy"},
			authCookieName + "=" + testAdminToken, true},
		{"dual 档收签名令牌", GuardOptions{SessionTokenMode: "dual"}, authCookieName + "=" + signed, true},
		{"未配置 ADMIN_TOKEN 时裸值不算会话",
			GuardOptions{AdminToken: " ", SessionTokenMode: "opaque"}, authCookieName + "=" + testAdminToken, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			guard := newTestGuard(t, testCase.guard)
			snapshot, err := resolve(t, guard, testCase.cookie)
			if err != nil {
				t.Fatalf("解析不应报错: %v", err)
			}
			if (snapshot != nil) != testCase.want {
				t.Fatalf("是否解析出会话应为 %v，实际 %+v", testCase.want, snapshot)
			}
			if snapshot == nil {
				return
			}
			if snapshot.Role != "admin" || snapshot.UserID != adminPrincipalUserID ||
				snapshot.UserName != adminPrincipalName {
				t.Errorf("ADMIN_TOKEN 身份应与 adminPrincipal 一致，实际 %+v", snapshot)
			}
			if !snapshot.CanLoginWebUI {
				t.Errorf("ADMIN_TOKEN 的合成密钥 canLoginWebUi 恒为 true，实际 %+v", snapshot)
			}
		})
	}
}

// 未登录是 (nil, nil)：不是错误。依赖故障（池已关、Redis 不可达）走的是 err 那一路，
// 因为“没登录”与“服务坏了”在前端是两种处置（踢到登录页 vs 显示故障）——
// err 那一路在 session_resolve_integration_test.go 用真池的关闭态镇住（空 store.Pools 会在
// 查询时 panic，不能当夹具）。
func TestResolveSessionAnonymousIsNotAnError(t *testing.T) {
	// opaque 档 + 无 Redis 且无池：分支只剩「裸 Key 不被接受」，结果是未登录而不是报错。
	guard := newTestGuard(t, GuardOptions{SessionTokenMode: "opaque"})
	for _, cookie := range []string{"", authCookieName + "=sess-not-a-session", authCookieName + "=sk-not-a-key"} {
		if snapshot, err := resolve(t, guard, cookie); err != nil || snapshot != nil {
			t.Fatalf("cookie %q 应为未登录且无错误，实际 %+v / %v", cookie, snapshot, err)
		}
	}
}

// 只认 cookie：头凭据是 API 面的形态，页面面不得据此判定登录（与 Node 的页面侧解析同源）。
func TestResolveSessionIgnoresHeaderCredentials(t *testing.T) {
	guard := newTestGuard(t, GuardOptions{})
	for _, headers := range []map[string]string{
		{"Authorization": "Bearer " + testAdminToken},
		{"X-API-Key": testAdminToken},
	} {
		request := httptest.NewRequest(http.MethodGet, "/zh-CN/dashboard", nil)
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		snapshot, err := guard.ResolveSession(context.Background(), request)
		if err != nil {
			t.Fatalf("解析不应报错: %v", err)
		}
		if snapshot != nil {
			t.Fatalf("头凭据不得解析成页面会话，实际 %+v", snapshot)
		}
	}
}

// 空守卫（nil）不得崩：管理面没装起来时 UI 面仍会调用它。
func TestResolveSessionNilGuard(t *testing.T) {
	var guard *AuthGuard
	snapshot, err := guard.ResolveSession(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil || snapshot != nil {
		t.Fatalf("nil 守卫应返回未登录且无错误，实际 %+v / %v", snapshot, err)
	}
}
