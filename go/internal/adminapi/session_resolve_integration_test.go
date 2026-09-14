package adminapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// 本文件是只读会话解析入口的**真库 + 真 Redis** 用例。它钉的是「壳能不能在首帧给出真实角色」
// 这件事：管理员合成身份、真实用户的不透明会话、legacy/dual 档的裸 Key，以及 opaque 档对裸 Key
// 的拒绝。门控：CCH_TEST_DSN（库）与 CCH_TEST_REDIS_URL（不透明会话）。
//
// 夹具纪律同 auth_test.go：用户与密钥按精确 id 清理，会话按精确 id 删。

// newSessionTestGuard 建一个用真池与真 Redis 的守卫（时钟取真实时间：不透明会话的过期判定
// 与会话签发共用同一条时间轴，固定时钟会让两者错位）。
func newSessionTestGuard(t *testing.T, pools *store.Pools, client redis.UniversalClient, mode string) *AuthGuard {
	t.Helper()
	return newTestGuard(t, GuardOptions{
		Pools:            pools,
		Redis:            client,
		SessionTokenMode: mode,
		Now:              time.Now,
	})
}

// 不透明用户会话：解析出真实用户、**实时角色**与密钥登录权限。
//
// 角色刻意与写进会话的 userRole 不一致（会话里写 "user"、库里是 "admin"）：Node 的
// convertToAuthSession 走 validateKey 实时读库，会话里的 userRole 只是契约快照。
// 若实现改成读会话快照，本用例立刻变红。
func TestResolveSessionOpaqueSessionOnRealRedis(t *testing.T) {
	pools := testPools(t)
	client := testRedis(t)
	userID := fixtureUser(t, pools, "admin", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{userID: userID, canLoginWebUI: true, isEnabled: true})

	writer := newRedisAuthSessions(client)
	sessionID, err := writer.CreateAuthSession(context.Background(), keyValue, LoginAccount{
		UserID: userID, UserRole: "user", KeyID: keyID, CanLoginWebUI: true,
	}, "session", time.Hour)
	if err != nil {
		t.Fatalf("写会话失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), sessionKeyPrefix+sessionID).Err() })

	guard := newSessionTestGuard(t, pools, client, "opaque")
	snapshot, err := resolve(t, guard, authCookieName+"="+sessionID)
	if err != nil {
		t.Fatalf("解析不应报错: %v", err)
	}
	if snapshot == nil {
		t.Fatal("不透明会话必须解析出会话")
	}
	if snapshot.UserID != userID || snapshot.UserName != userFixtureName(t, pools, userID) {
		t.Errorf("用户应为 id=%d，实际 %+v", userID, snapshot)
	}
	if snapshot.Role != "admin" {
		t.Errorf("角色必须取实时投影（库里是 admin），实际 %q", snapshot.Role)
	}
	if !snapshot.CanLoginWebUI {
		t.Errorf("key.canLoginWebUi 应取密钥列，实际 %+v", snapshot)
	}

	// ADMIN_TOKEN 回归：同一守卫下裸 ADMIN_TOKEN 仍解析成合成管理员。
	adminSnapshot, err := resolve(t, guard, authCookieName+"="+testAdminToken)
	if err != nil || adminSnapshot == nil {
		t.Fatalf("裸 ADMIN_TOKEN 应解析出会话，实际 %+v / %v", adminSnapshot, err)
	}
	if adminSnapshot.UserID != adminPrincipalUserID || adminSnapshot.Role != "admin" {
		t.Errorf("ADMIN_TOKEN 身份应为合成管理员，实际 %+v", adminSnapshot)
	}
}

// 裸 Key（legacy / dual 档）解析出用户；opaque 档必须拒绝（浏览器只持 sid_，裸 Key 不再当会话用）。
func TestResolveSessionRawKeyBySessionMode(t *testing.T) {
	pools := testPools(t)
	client := testRedis(t)
	userID := fixtureUser(t, pools, "user", true)
	// canLoginWebUI=false：读档放行它，但仍必须解析成会话——否则只读会话在壳里看不到自己的身份。
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{userID: userID, canLoginWebUI: false, isEnabled: true})

	for _, mode := range []string{"legacy", "dual"} {
		guard := newSessionTestGuard(t, pools, client, mode)
		snapshot, err := resolve(t, guard, authCookieName+"="+keyValue)
		if err != nil {
			t.Fatalf("%s 档解析不应报错: %v", mode, err)
		}
		if snapshot == nil {
			t.Fatalf("%s 档的裸 Key 必须解析出会话", mode)
		}
		if snapshot.UserID != userID || snapshot.Role != "user" {
			t.Errorf("%s 档应为用户 %d / user，实际 %+v", mode, userID, snapshot)
		}
		if snapshot.CanLoginWebUI {
			t.Errorf("%s 档应带出密钥的 canLoginWebUi=false，实际 %+v", mode, snapshot)
		}
	}

	opaque := newSessionTestGuard(t, pools, client, "opaque")
	if snapshot, err := resolve(t, opaque, authCookieName+"="+keyValue); err != nil || snapshot != nil {
		t.Fatalf("opaque 档不得接受裸 Key，实际 %+v / %v", snapshot, err)
	}
}

// 失效凭据一律不是会话：用户被禁用、密钥过期。
func TestResolveSessionRejectsInvalidatedCredentials(t *testing.T) {
	pools := testPools(t)
	client := testRedis(t)
	guard := newSessionTestGuard(t, pools, client, "dual")

	disabledUserID := fixtureUser(t, pools, "user", false)
	_, disabledKey := fixtureKey(t, pools, fixtureKeyOptions{userID: disabledUserID, canLoginWebUI: true, isEnabled: true})
	if snapshot, err := resolve(t, guard, authCookieName+"="+disabledKey); err != nil || snapshot != nil {
		t.Fatalf("禁用用户的密钥不得解析成会话，实际 %+v / %v", snapshot, err)
	}

	activeUserID := fixtureUser(t, pools, "user", true)
	expired := time.Now().Add(-time.Hour)
	_, expiredKey := fixtureKey(t, pools, fixtureKeyOptions{
		userID: activeUserID, canLoginWebUI: true, isEnabled: true, expiresAt: &expired,
	})
	if snapshot, err := resolve(t, guard, authCookieName+"="+expiredKey); err != nil || snapshot != nil {
		t.Fatalf("过期密钥不得解析成会话，实际 %+v / %v", snapshot, err)
	}
}

// 依赖故障必须报错而不是当成未登录：池已关时解析走 err 那一路（前端据此区分「没登录」与
// 「服务坏了」）。这是本入口与 Wrap 一致的地方——两者都不把故障谎报成凭据错误。
func TestResolveSessionDependencyFailureIsAnError(t *testing.T) {
	pools := testPools(t)
	client := testRedis(t)
	userID := fixtureUser(t, pools, "user", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{userID: userID, canLoginWebUI: true, isEnabled: true})

	guard := newSessionTestGuard(t, pools, client, "dual")
	if err := pools.Close(); err != nil {
		t.Fatalf("关闭连接池失败: %v", err)
	}
	snapshot, err := resolve(t, guard, authCookieName+"="+keyValue)
	if err == nil {
		t.Fatalf("连接池不可用时必须返回错误，实际 %+v", snapshot)
	}
	if snapshot != nil {
		t.Fatalf("依赖故障不得给出会话快照，实际 %+v", snapshot)
	}
}

// userFixtureName 读夹具用户的 name（用例要断言快照里的用户名取自库里而不是会话快照）。
func userFixtureName(t *testing.T, pools *store.Pools, userID int64) string {
	t.Helper()
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取数据分道失败: %v", err)
	}
	var name string
	if err := pool.QueryRow(context.Background(), `SELECT name FROM users WHERE id = $1`, userID).Scan(&name); err != nil {
		t.Fatalf("读用户夹具失败: %v", err)
	}
	return name
}

// 本用例也顺带钉住「头凭据不参与页面会话判定」的真库形态：带 Authorization 的请求仍无会话。
func TestResolveSessionIgnoresHeadersOnRealGuard(t *testing.T) {
	pools := testPools(t)
	client := testRedis(t)
	userID := fixtureUser(t, pools, "admin", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{userID: userID, canLoginWebUI: true, isEnabled: true})

	guard := newSessionTestGuard(t, pools, client, "dual")
	request := httptest.NewRequest(http.MethodGet, "/zh-CN/dashboard", nil)
	request.Header.Set("Authorization", "Bearer "+keyValue)
	request.Header.Set("X-API-Key", keyValue)
	snapshot, err := guard.ResolveSession(context.Background(), request)
	if err != nil || snapshot != nil {
		t.Fatalf("头凭据不得解析成页面会话，实际 %+v / %v", snapshot, err)
	}
}
