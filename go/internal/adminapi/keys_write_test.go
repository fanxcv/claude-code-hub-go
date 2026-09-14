package adminapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminauth"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 keys 写路径（A1-2 续写）的测试：两道写会话门、zod 形状的 400 信封、以及
// create/list/update/delete/batchUpdate 的真库行为。
//
// 前两组**不**依赖真库：写会话门与请求校验都发生在任何 SQL 之前，用零值 *store.Pools 就能
// 命中（这样这两组在无 DB 的环境里也跑）。第三组门控 CCH_TEST_DSN（testPools 自身门控）。

// TestAdminKeyWriteSessionGateDeniesReadOnlySession 钉住 requireKeyWriteSession 的判据：
// 非管理员且凭据密钥 canLoginWebUi 不为 true ⇒ 403 auth.forbidden；无身份 ⇒ 401 auth.missing。
//
// 这是「只读密钥不得改密钥」的唯一防线：读档认证会放行 canLoginWebUi=false 的会话，若这道门
// 判错，只读密钥就能 PATCH 自己的 canLoginWebUi 完成自提权。
func TestAdminKeyWriteSessionGateDeniesReadOnlySession(t *testing.T) {
	readOnly := Principal{UserID: 42, Username: "reader", CanLoginWebUI: false, WebSession: true}
	mutations := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{"patch", http.MethodPatch, "/api/v1/keys/1234", `{"name":"k"}`},
		{"delete", http.MethodDelete, "/api/v1/keys/1234", ""},
		{"enable", http.MethodPost, "/api/v1/keys/1234:enable", `{"enabled":false}`},
		{"renew", http.MethodPost, "/api/v1/keys/1234:renew", `{"expiresAt":"2030-01-01"}`},
	}
	for _, item := range mutations {
		t.Run(item.name, func(t *testing.T) {
			router := newKeysTestRouter(&store.Pools{}, readOnly, nil, nil)
			status, body, raw := serveKeys(t, router, item.method, item.target, item.body)
			if status != http.StatusForbidden {
				t.Fatalf("只读会话应 403，实际 %d：%s", status, raw)
			}
			if body["errorCode"] != "auth.forbidden" {
				t.Fatalf("errorCode 应为 auth.forbidden，实际 %v", body["errorCode"])
			}
			if body["detail"] != "Read-only sessions cannot manage keys." {
				t.Fatalf("detail 与 Node 不符：%v", body["detail"])
			}
		})
	}

	// 无身份（守卫被绕过 / 未装配）⇒ 401 auth.missing，绝不当作合法调用方放行。
	router := newKeysTestRouter(&store.Pools{}, Principal{}, nil, nil)
	status, body, _ := serveKeys(t, router, http.MethodPatch, "/api/v1/keys/1234", `{"name":"k"}`)
	if status != http.StatusUnauthorized || body["errorCode"] != "auth.missing" {
		t.Fatalf("无身份应 401 auth.missing，实际 %d %v", status, body["errorCode"])
	}

	// 放行路径（管理员、canLoginWebUi=true）在此**不**断言：过了这道门就会打库，而零值
	// *store.Pools 的 Lane() 会 panic（nil map）。放行路径由真库集成用例覆盖
	// （TestAdminKeyCreateListPatchDeleteIntegration 的管理员、TestAdminKeySelfServiceCreate…）。
}

// TestAdminKeyCreateSelfRequiresWebUILogin 钉住 /users:self/keys 的 deny-by-default：
// 只有 canLoginWebUi 明确为 true 的会话才能自助建密钥（管理员也走这条判据，与 Node 一致：
// createSelfKey 校验的是 session.key?.canLoginWebUi 而非角色）。
func TestAdminKeyCreateSelfRequiresWebUILogin(t *testing.T) {
	router := newKeysTestRouter(&store.Pools{}, Principal{UserID: 42, CanLoginWebUI: false}, nil, nil)
	status, body, _ := serveKeys(t, router, http.MethodPost, "/api/v1/users:self/keys", `{"name":"new"}`)
	if status != http.StatusForbidden || body["errorCode"] != "auth.forbidden" {
		t.Fatalf("只读会话自助建密钥应 403 auth.forbidden，实际 %d %v", status, body["errorCode"])
	}
	if body["detail"] != "Read-only sessions cannot create keys." {
		t.Fatalf("detail 与 Node 不符：%v", body["detail"])
	}
}

// TestKeysValidationErrorEnvelopeMatchesZod 钉住 fromZodError 的 400 形状（两例）：
// 未知字段（.strict()）与必填字段缺失。
//
// 形状逐字对照 src/lib/api/v1/_shared/error-envelope.ts:40-62：title 恒为 "Validation failed"、
// detail 恒为 "One or more fields are invalid."、errorCode 恒为 request.validation_failed、
// 正文带 invalidParams 明细。invalidParams 的 message 文案不逐字对齐 zod（已登记白名单）。
func TestKeysValidationErrorEnvelopeMatchesZod(t *testing.T) {
	admin := Principal{UserID: 1, IsAdmin: true, CanLoginWebUI: true}

	t.Run("unknown_field", func(t *testing.T) {
		router := newKeysTestRouter(&store.Pools{}, admin, nil, nil)
		status, body, raw := serveKeys(t, router, http.MethodPatch, "/api/v1/keys/1234",
			`{"name":"k","bogus":1}`)
		if status != http.StatusBadRequest {
			t.Fatalf("应 400，实际 %d：%s", status, raw)
		}
		assertZodEnvelope(t, body)
		params, ok := body["invalidParams"].([]any)
		if !ok || len(params) != 1 {
			t.Fatalf("invalidParams 应为 1 项，实际 %v", body["invalidParams"])
		}
		item := params[0].(map[string]any)
		if item["code"] != "unrecognized_keys" {
			t.Fatalf("code 应为 unrecognized_keys，实际 %v", item["code"])
		}
		if path, _ := item["path"].([]any); len(path) != 1 || path[0] != "bogus" {
			t.Fatalf("path 应为 [bogus]，实际 %v", item["path"])
		}
	})

	t.Run("missing_required_name", func(t *testing.T) {
		router := newKeysTestRouter(&store.Pools{}, admin, nil, nil)
		status, body, raw := serveKeys(t, router, http.MethodPost, "/api/v1/users/1234/keys", `{}`)
		if status != http.StatusBadRequest {
			t.Fatalf("应 400，实际 %d：%s", status, raw)
		}
		assertZodEnvelope(t, body)
		params, ok := body["invalidParams"].([]any)
		if !ok || len(params) != 1 {
			t.Fatalf("invalidParams 应为 1 项，实际 %v", body["invalidParams"])
		}
		item := params[0].(map[string]any)
		if item["code"] != "invalid_type" {
			t.Fatalf("code 应为 invalid_type，实际 %v", item["code"])
		}
		if path, _ := item["path"].([]any); len(path) != 1 || path[0] != "name" {
			t.Fatalf("path 应为 [name]，实际 %v", item["path"])
		}
	})

	t.Run("unknown_include", func(t *testing.T) {
		router := newKeysTestRouter(&store.Pools{}, admin, nil, nil)
		status, body, raw := serveKeys(t, router, http.MethodGet,
			"/api/v1/users/1234/keys?include=bogus", "")
		if status != http.StatusBadRequest {
			t.Fatalf("应 400，实际 %d：%s", status, raw)
		}
		assertZodEnvelope(t, body)
	})
}

// assertZodEnvelope 断言 zod 形状的 400 信封。
func assertZodEnvelope(t *testing.T, body map[string]any) {
	t.Helper()
	if body["title"] != "Validation failed" {
		t.Fatalf("title 应为 Validation failed，实际 %v", body["title"])
	}
	if body["detail"] != "One or more fields are invalid." {
		t.Fatalf("detail 与 Node 不符：%v", body["detail"])
	}
	if body["errorCode"] != "request.validation_failed" {
		t.Fatalf("errorCode 应为 request.validation_failed，实际 %v", body["errorCode"])
	}
	if _, ok := body["invalidParams"]; !ok {
		t.Fatalf("正文应含 invalidParams：%v", body)
	}
}

// ---- 真库集成（门控 CCH_TEST_DSN）----

// TestAdminKeyCreateListPatchDeleteIntegration 覆盖 create → list → patch → enable → delete
// 这条主线，以及删除「最后一个启用密钥」的保护。
func TestAdminKeyCreateListPatchDeleteIntegration(t *testing.T) {
	pools := testPools(t)
	ownerID := fixtureUser(t, pools, "user", true)
	admin := Principal{UserID: 1, Username: "admin", IsAdmin: true, CanLoginWebUI: true}
	audit := &recordingAudit{}
	invalidator := &recordingInvalidator{}
	router := newKeysTestRouter(pools, admin, audit, invalidator)

	// 建两把：第一把用于主线，第二把保证「最后一个启用密钥」保护不会误伤（用户始终剩一把启用的）。
	first := createKeyViaAPI(t, router, ownerID, `{"name":"it-first","limit5hUsd":3}`)
	second := createKeyViaAPI(t, router, ownerID, `{"name":"it-second"}`)

	// 列表：明文被换成 maskedKey，且没有 key 字段。
	status, body, raw := serveKeys(t, router, http.MethodGet, keysListPath(ownerID), "")
	if status != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d：%s", status, raw)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) < 2 {
		t.Fatalf("items 应至少 2 项，实际 %v", body["items"])
	}
	for _, entry := range items {
		item, _ := entry.(map[string]any)
		if _, exists := item["key"]; exists {
			t.Fatalf("列表不得回传密钥明文：%v", item)
		}
		if item["maskedKey"] == nil || item["maskedKey"] == "" {
			t.Fatalf("列表项应有 maskedKey：%v", item)
		}
	}

	// include=statistics：形状换成 KeyStatistics（含 keyId/modelStats）。
	status, body, raw = serveKeys(t, router, http.MethodGet, keysListPath(ownerID)+"?include=statistics", "")
	if status != http.StatusOK {
		t.Fatalf("统计列表应 200，实际 %d：%s", status, raw)
	}
	stats, _ := body["items"].([]any)
	if len(stats) == 0 {
		t.Fatalf("统计列表不应为空")
	}
	statItem, _ := stats[0].(map[string]any)
	if statItem["keyId"] == nil || statItem["todayCallCount"] == nil {
		t.Fatalf("统计项形状不符：%v", statItem)
	}
	if _, exists := statItem["modelStats"]; !exists {
		t.Fatalf("统计项应含 modelStats：%v", statItem)
	}

	// PATCH：只改 name 与限额，其余列保持不动（部分更新语义）。
	status, _, raw = serveKeys(t, router, http.MethodPatch, keyPath(first, ""),
		`{"name":"it-first-renamed","limitTotalUsd":7}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH 应 200，实际 %d：%s", status, raw)
	}
	if got := readKeyColumn(t, mustControl(t, pools), first, "name"); got != "it-first-renamed" {
		t.Fatalf("name 未更新：%q", got)
	}
	if got := readKeyColumn(t, mustControl(t, pools), first, "limit_total_usd"); got != "7.00" {
		t.Fatalf("limit_total_usd 应为 7.00，实际 %q", got)
	}
	if len(invalidator.keyAuth) == 0 {
		t.Fatalf("PATCH 应失效该密钥的认证缓存")
	}

	// :enable 禁用（用户还剩第二把启用的，故允许）。
	status, _, raw = serveKeys(t, router, http.MethodPost, keyPath(first, ":enable"), `{"enabled":false}`)
	if status != http.StatusOK {
		t.Fatalf("禁用应 200，实际 %d：%s", status, raw)
	}
	if got := readKeyColumn(t, mustControl(t, pools), first, "is_enabled"); got != "false" {
		t.Fatalf("is_enabled 应为 false，实际 %q", got)
	}

	// DELETE：删掉第一把，剩第二把。
	status, _, raw = serveKeys(t, router, http.MethodDelete, keyPath(first, ""), "")
	if status != http.StatusNoContent {
		t.Fatalf("删除应 204，实际 %d：%s", status, raw)
	}
	if got := readKeyColumn(t, mustControl(t, pools), first, "deleted_at"); got == "" {
		t.Fatalf("删除应为软删（deleted_at 非空）")
	}

	// 只剩一把启用的密钥时，禁用与删除都必须被拒（CANNOT_DISABLE_LAST_KEY / CANNOT_DELETE_LAST_KEY）。
	status, body, _ = serveKeys(t, router, http.MethodPost, keyPath(second, ":enable"), `{"enabled":false}`)
	if status != http.StatusBadRequest || body["errorCode"] != "CANNOT_DISABLE_LAST_KEY" {
		t.Fatalf("禁用最后一个启用密钥应 400 CANNOT_DISABLE_LAST_KEY，实际 %d %v", status, body["errorCode"])
	}
	status, body, _ = serveKeys(t, router, http.MethodDelete, keyPath(second, ""), "")
	if status != http.StatusBadRequest || body["errorCode"] != "CANNOT_DELETE_LAST_KEY" {
		t.Fatalf("删除最后一个启用密钥应 400 CANNOT_DELETE_LAST_KEY，实际 %d %v", status, body["errorCode"])
	}

	// 审计：create 两条 + update 一条 + delete 一条（禁用不写审计，与 Node 的 toggleKeyEnabled 一致）。
	actions := make([]string, 0, len(audit.events))
	for _, event := range audit.events {
		actions = append(actions, event.Action)
	}
	joined := strings.Join(actions, ",")
	if strings.Count(joined, "key.create") != 2 {
		t.Fatalf("应写两条 key.create 审计，实际 %v", actions)
	}
	if strings.Count(joined, "key.update") != 1 {
		t.Fatalf("应写一条 key.update 审计，实际 %v", actions)
	}
	if strings.Count(joined, "key.delete") != 1 {
		t.Fatalf("应写一条 key.delete 审计，实际 %v", actions)
	}
	for _, event := range audit.events {
		if event.Category != "key" || event.Principal.UserID != admin.UserID {
			t.Fatalf("审计分类与操作者不符：%+v", event)
		}
	}
}

// TestAdminKeySelfServiceCreateRespectsSessionUser 钉住 /users:self/keys 的目标属主恒为会话身份，
// 且 canLoginWebUI=false 的会话被拒（deny-by-default）。
func TestAdminKeySelfServiceCreateRespectsSessionUser(t *testing.T) {
	pools := testPools(t)
	ownerID := fixtureUser(t, pools, "user", true)

	reader := Principal{UserID: ownerID, CanLoginWebUI: false, WebSession: true}
	router := newKeysTestRouter(pools, reader, &recordingAudit{}, nil)
	status, body, _ := serveKeys(t, router, http.MethodPost, "/api/v1/users:self/keys", `{"name":"self"}`)
	if status != http.StatusForbidden || body["errorCode"] != "auth.forbidden" {
		t.Fatalf("只读会话应 403 auth.forbidden，实际 %d %v", status, body["errorCode"])
	}

	writer := Principal{UserID: ownerID, CanLoginWebUI: true, WebSession: true}
	router = newKeysTestRouter(pools, writer, &recordingAudit{}, nil)
	status, body, raw := serveKeys(t, router, http.MethodPost, "/api/v1/users:self/keys", `{"name":"self"}`)
	if status != http.StatusCreated {
		t.Fatalf("自助建密钥应 201，实际 %d：%s", status, raw)
	}
	createdID, ok := body["id"].(float64)
	if !ok {
		t.Fatalf("响应应含 id：%v", body)
	}
	if got := readKeyColumn(t, mustControl(t, pools), int64(createdID), "user_id"); got != formatInt(ownerID) {
		t.Fatalf("新建密钥的属主应为会话身份 %d，实际 %q", ownerID, got)
	}
	if generated, _ := body["generatedKey"].(string); !strings.HasPrefix(generated, "sk-") {
		t.Fatalf("generatedKey 应为 sk- 前缀：%v", body["generatedKey"])
	}
}

// TestAdminKeyBatchUpdateIntegration 覆盖 /keys:batchUpdate 的事务行为：正常批量、缺 id 回滚、
// 禁用后「每个用户至少一把启用密钥」的复核。
func TestAdminKeyBatchUpdateIntegration(t *testing.T) {
	pools := testPools(t)
	ownerID := fixtureUser(t, pools, "user", true)
	admin := Principal{UserID: 1, IsAdmin: true, CanLoginWebUI: true}
	router := newKeysTestRouter(pools, admin, &recordingAudit{}, &recordingInvalidator{})

	first := createKeyViaAPI(t, router, ownerID, `{"name":"batch-a"}`)
	second := createKeyViaAPI(t, router, ownerID, `{"name":"batch-b"}`)

	body := `{"keyIds":[` + formatInt(first) + `,` + formatInt(second) + `],"updates":{"limit5hUsd":5}}`
	status, payload, raw := serveKeys(t, router, http.MethodPost, "/api/v1/keys:batchUpdate", body)
	if status != http.StatusOK {
		t.Fatalf("批量更新应 200，实际 %d：%s", status, raw)
	}
	if payload["updatedCount"] != float64(2) {
		t.Fatalf("updatedCount 应为 2，实际 %v", payload["updatedCount"])
	}
	for _, keyID := range []int64{first, second} {
		if got := readKeyColumn(t, mustControl(t, pools), keyID, "limit_5h_usd"); got != "5.00" {
			t.Fatalf("key %d 的 limit_5h_usd 应为 5.00，实际 %q", keyID, got)
		}
	}

	// 含不存在的 id：整批回滚，已存在的那些也不能被改动。
	missing := int64(2147483000)
	body = `{"keyIds":[` + formatInt(first) + `,` + formatInt(missing) + `],"updates":{"limit5hUsd":9}}`
	status, payload, _ = serveKeys(t, router, http.MethodPost, "/api/v1/keys:batchUpdate", body)
	if status != http.StatusNotFound || payload["errorCode"] != "NOT_FOUND" {
		t.Fatalf("含缺失 id 应 404 NOT_FOUND，实际 %d %v", status, payload["errorCode"])
	}
	if got := readKeyColumn(t, mustControl(t, pools), first, "limit_5h_usd"); got != "5.00" {
		t.Fatalf("回滚后 limit_5h_usd 应仍为 5.00，实际 %q", got)
	}

	// 空 updates：EMPTY_UPDATE。
	body = `{"keyIds":[` + formatInt(first) + `],"updates":{}}`
	status, payload, _ = serveKeys(t, router, http.MethodPost, "/api/v1/keys:batchUpdate", body)
	if status != http.StatusBadRequest || payload["errorCode"] != "EMPTY_UPDATE" {
		t.Fatalf("空 updates 应 400 EMPTY_UPDATE，实际 %d %v", status, payload["errorCode"])
	}

	// 把用户的两把密钥一起禁用：会违反「至少一把启用」⇒ 整批回滚。
	body = `{"keyIds":[` + formatInt(first) + `,` + formatInt(second) + `],"updates":{"isEnabled":false}}`
	status, payload, _ = serveKeys(t, router, http.MethodPost, "/api/v1/keys:batchUpdate", body)
	if status != http.StatusBadRequest || payload["errorCode"] != "CANNOT_DISABLE_LAST_KEY" {
		t.Fatalf("整批禁用应 400 CANNOT_DISABLE_LAST_KEY，实际 %d %v", status, payload["errorCode"])
	}
	if got := readKeyColumn(t, mustControl(t, pools), first, "is_enabled"); got != "true" {
		t.Fatalf("回滚后 is_enabled 应为 true，实际 %q", got)
	}
}

// createKeyViaAPI 建一把密钥并返回 id（走真实的创建路由，覆盖校验链与落库）。
func createKeyViaAPI(t *testing.T, router *Router, userID int64, body string) int64 {
	t.Helper()
	status, payload, raw := serveKeys(t, router, http.MethodPost, keysListPath(userID), body)
	if status != http.StatusCreated {
		t.Fatalf("建密钥应 201，实际 %d：%s", status, raw)
	}
	id, ok := payload["id"].(float64)
	if !ok {
		t.Fatalf("创建响应应含 id：%v", payload)
	}
	return int64(id)
}

// keysListPath 是 /users/{id}/keys 的请求路径。
func keysListPath(userID int64) string {
	return "/api/v1/users/" + formatInt(userID) + "/keys"
}

// mustControl 取控制分道（夹具与断言共用）。
func mustControl(t *testing.T, pools *store.Pools) *store.Pool {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	return pool
}

// formatInt 渲染整数（供路径与 JSON 拼接）。
func formatInt(value int64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// TestPrincipalCarriesCredentialKeyWebUIPermission 钉住认证层把「凭据密钥的 canLoginWebUi」与
// 「凭据是否来自浏览器会话」交给处理器。
//
// 这是 keys 写路径唯一的判据来源（A1-2 为此扩了 Principal）：认证层本来就已经算出
// keyCanLoginWebUI（auth.go 的 resolvedPrincipal），不传出来，处理器就只能瞎猜——而猜错的
// 两种方向都危险：判宽了让只读密钥自提权，判窄了让管理员会话建不了密钥。
func TestPrincipalCarriesCredentialKeyWebUIPermission(t *testing.T) {
	pools := testPools(t)
	userID := fixtureUser(t, pools, "user", true)
	_, readOnlyKey := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: false, isEnabled: true,
	})
	_, webKey := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: true, isEnabled: true,
	})
	guard := newTestGuard(t, GuardOptions{Pools: pools, SessionTokenMode: "dual"})

	// canLoginWebUi=false 的密钥：read 档仍然放行（只读会话），但身份必须显示它不能写。
	readOnly := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys",
		map[string]string{"Authorization": "Bearer " + readOnlyKey})
	if !readOnly.passed {
		t.Fatalf("只读会话应放行到 read 档: %d %v", readOnly.status, readOnly.body)
	}
	if readOnly.principal.CanLoginWebUI {
		t.Fatalf("canLoginWebUi=false 的密钥不应被标成允许写: %+v", readOnly.principal)
	}
	if readOnly.principal.WebSession {
		t.Fatalf("Authorization 头携带的凭据不是浏览器会话: %+v", readOnly.principal)
	}

	// canLoginWebUi=true 的密钥：同一来源，但允许写。
	writable := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys",
		map[string]string{"Authorization": "Bearer " + webKey})
	if !writable.principal.CanLoginWebUI {
		t.Fatalf("canLoginWebUi=true 的密钥应被标成允许写: %+v", writable.principal)
	}

	// Cookie 携带的签名会话：WebSession 为 true；ADMIN_TOKEN 的合成密钥恒允许写。
	token, err := adminauth.CreateSignedToken(testAdminToken, time.Hour, testClock)
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}
	viaCookie := drive(t, guard, AccessRead, http.MethodGet, "/api/v1/keys",
		map[string]string{"Cookie": authCookieName + "=" + token})
	if !viaCookie.passed {
		t.Fatalf("签名会话应放行: %d %v", viaCookie.status, viaCookie.body)
	}
	if !viaCookie.principal.WebSession {
		t.Fatalf("Cookie 携带的凭据应标为浏览器会话: %+v", viaCookie.principal)
	}
	if !viaCookie.principal.CanLoginWebUI {
		t.Fatalf("ADMIN_TOKEN 的合成密钥应允许写: %+v", viaCookie.principal)
	}
}
