package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是通知面与两条 admin ops 的端到端用例（真 PG）：
//   - webhook-targets：创建（201 + Location + 脱敏）→ 详情 → 更新（"[REDACTED]" 保留原值）→ 删除
//     → 404 三处 → 测试投递（打本地假 webhook）。
//   - notifications：设置读写（回显脱敏 + 审计落一笔）、绑定整组替换与列表（内联目标已脱敏）。
//   - admin ops：日志级别读写；日志清理的「无条件拒绝 / dryRun 计数 / 真删只删命中行」。
//
// 权限档位：本文件同时断言每条路由注册时使用的档位（admin），避免「注册成功但档位写错」。

const notificationsITWebhookMarker = "go-admin-notifications-it"

// notificationsITAuditSink 记录被写下的审计事件。
type notificationsITAuditSink struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (sink *notificationsITAuditSink) Emit(_ context.Context, event AuditEvent) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.events = append(sink.events, event)
}

func (sink *notificationsITAuditSink) snapshot() []AuditEvent {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]AuditEvent{}, sink.events...)
}

// notificationsITGuard 记录档位并把管理员身份放进上下文（认证语义由 A0-2 的守卫负责）。
type notificationsITGuard struct {
	mu     sync.Mutex
	levels []AccessLevel
}

func (guard *notificationsITGuard) Wrap(level AccessLevel, next http.Handler) http.Handler {
	guard.mu.Lock()
	guard.levels = append(guard.levels, level)
	guard.mu.Unlock()
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		next.ServeHTTP(writer, request.WithContext(WithPrincipal(request.Context(), Principal{
			UserID:   1,
			Username: "notifications-it",
			IsAdmin:  true,
		})))
	})
}

func (guard *notificationsITGuard) snapshot() []AccessLevel {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return append([]AccessLevel{}, guard.levels...)
}

// notificationsIT 是本文件的装配。
type notificationsIT struct {
	router *Router
	pools  *store.Pools
	guard  *notificationsITGuard
	audit  *notificationsITAuditSink
}

// newNotificationsIT 装配真依赖；缺门控变量即跳过。
func newNotificationsIT(t *testing.T) *notificationsIT {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	ctx := context.Background()
	pools, err := store.Open(ctx, store.Options{
		DSN:      dsn,
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	guard := &notificationsITGuard{}
	audit := &notificationsITAuditSink{}
	deps := Deps{
		Guard:    guard,
		Store:    pools,
		Audit:    audit,
		Problems: NewProblems(nil),
	}
	router := New(Options{Deps: deps})
	RegisterWebhookTargets(router, deps)
	RegisterNotifications(router, deps)
	RegisterAdminOps(router, deps)

	integration := &notificationsIT{router: router, pools: pools, guard: guard, audit: audit}
	t.Cleanup(integration.cleanup)
	return integration
}

// cleanup 删掉本用例造的目标（按名字前缀精确清理，不动别人的行）。
func (integration *notificationsIT) cleanup() {
	ctx := context.Background()
	pool, err := integration.pools.Writer()
	if err != nil {
		return
	}
	_, _ = pool.Exec(ctx, `DELETE FROM notification_target_bindings WHERE target_id IN (
		SELECT id FROM webhook_targets WHERE name LIKE $1)`, notificationsITWebhookMarker+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM webhook_targets WHERE name LIKE $1`,
		notificationsITWebhookMarker+"%")
}

// newTarget 造一个自定义渠道目标（含自定义头与代理地址，便于验证脱敏）。
func (integration *notificationsIT) newTarget(t *testing.T, name string) int64 {
	t.Helper()
	payload := map[string]any{
		"name":         notificationsITWebhookMarker + "-" + name,
		"providerType": "custom",
		"webhookUrl":   "https://example.com/hook",
		"customTemplate": map[string]any{
			"text": "{{title}}",
		},
		"customHeaders": map[string]any{"Authorization": "Bearer secret-value", "X-Trace": "trace-1"},
		"proxyUrl":      "http://user:pass@127.0.0.1:1080",
	}
	body, _ := json.Marshal(payload)
	recorder := integration.call(t, http.MethodPost, MountPrefix+"/webhook-targets", string(body))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("创建目标应得 201，实际 %d（%s）", recorder.Code, recorder.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("创建响应不是 JSON：%v", err)
	}
	return created.ID
}

// call 发一个请求并返回响应。
func (integration *notificationsIT) call(
	t *testing.T,
	method, path, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	integration.router.ServeHTTP(recorder, request)
	return recorder
}

// TestWebhookTargetsRoutesEndToEnd 钉住六条端点的往返与脱敏语义。
func TestWebhookTargetsRoutesEndToEnd(t *testing.T) {
	integration := newNotificationsIT(t)
	id := integration.newTarget(t, "crud")

	t.Run("创建响应已脱敏且带 Location", func(t *testing.T) {
		payload := map[string]any{
			"name":         notificationsITWebhookMarker + "-explicit",
			"providerType": "custom",
			"webhookUrl":   "https://example.com/hook",
			"customTemplate": map[string]any{
				"text": "x",
			},
			"customHeaders": map[string]any{"X-Api-Key": "abc123", "X-Trace": "t"},
			"proxyUrl":      "http://user:pass@127.0.0.1:1080",
		}
		body, _ := json.Marshal(payload)
		recorder := integration.call(t, http.MethodPost, MountPrefix+"/webhook-targets", string(body))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("应得 201，实际 %d（%s）", recorder.Code, recorder.Body.String())
		}
		location := recorder.Header().Get("Location")
		if !strings.HasPrefix(location, MountPrefix+"/webhook-targets/") {
			t.Fatalf("Location 形状不对：%s", location)
		}
		response := recorder.Body.String()
		if !strings.Contains(response, `"webhookUrl":"[REDACTED]"`) {
			t.Fatalf("webhookUrl 应脱敏：%s", response)
		}
		if !strings.Contains(response, `"X-Api-Key":"[REDACTED]"`) {
			t.Fatalf("密钥型自定义头应脱敏：%s", response)
		}
		if !strings.Contains(response, `"X-Trace":"t"`) {
			t.Fatalf("普通自定义头应原样返回：%s", response)
		}
		if !strings.Contains(response, `http://REDACTED:REDACTED@127.0.0.1:1080`) {
			t.Fatalf("代理地址凭据应脱敏：%s", response)
		}
	})

	t.Run("详情与 404", func(t *testing.T) {
		ok := integration.call(t, http.MethodGet, fmt.Sprintf("%s/webhook-targets/%d", MountPrefix, id), "")
		if ok.Code != http.StatusOK {
			t.Fatalf("详情应得 200，实际 %d", ok.Code)
		}
		missing := integration.call(t, http.MethodGet, MountPrefix+"/webhook-targets/999999999", "")
		if missing.Code != http.StatusNotFound {
			t.Fatalf("详情 404 期望，实际 %d", missing.Code)
		}
		if !strings.Contains(missing.Body.String(), "webhook_target.not_found") {
			t.Fatalf("404 错误码不对：%s", missing.Body.String())
		}
	})

	t.Run("更新保留脱敏占位符对应的原值", func(t *testing.T) {
		payload := map[string]any{
			"webhookUrl":    "[REDACTED]",
			"customHeaders": map[string]any{"Authorization": "[REDACTED]", "X-Trace": "trace-2"},
		}
		body, _ := json.Marshal(payload)
		recorder := integration.call(t, http.MethodPatch,
			fmt.Sprintf("%s/webhook-targets/%d", MountPrefix, id), string(body))
		if recorder.Code != http.StatusOK {
			t.Fatalf("更新应得 200，实际 %d（%s）", recorder.Code, recorder.Body.String())
		}

		// 库里必须仍是原值：占位符不得落库。
		pool, err := integration.pools.Control()
		if err != nil {
			t.Fatalf("取分道失败：%v", err)
		}
		var webhookURL *string
		var headers []byte
		if err := pool.QueryRow(context.Background(),
			"SELECT webhook_url, custom_headers FROM webhook_targets WHERE id = $1", id).
			Scan(&webhookURL, &headers); err != nil {
			t.Fatalf("读回目标失败：%v", err)
		}
		if webhookURL == nil || *webhookURL != "https://example.com/hook" {
			t.Fatalf("webhookUrl 应保留原值，实际 %v", webhookURL)
		}
		var headerMap map[string]string
		if err := json.Unmarshal(headers, &headerMap); err != nil {
			t.Fatalf("自定义头不是对象：%s", string(headers))
		}
		if headerMap["Authorization"] != "Bearer secret-value" {
			t.Fatalf("Authorization 应保留原值，实际 %q", headerMap["Authorization"])
		}
		if headerMap["X-Trace"] != "trace-2" {
			t.Fatalf("普通头应更新，实际 %q", headerMap["X-Trace"])
		}
	})

	t.Run("未解析的脱敏头回显得 422", func(t *testing.T) {
		payload := map[string]any{
			"customHeaders": map[string]any{"X-New-Secret": "[REDACTED]"},
		}
		body, _ := json.Marshal(payload)
		recorder := integration.call(t, http.MethodPatch,
			fmt.Sprintf("%s/webhook-targets/%d", MountPrefix, id), string(body))
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("应得 422，实际 %d（%s）", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("测试投递打真本地 webhook 并回写结果", func(t *testing.T) {
		// 假接收端：企业微信的成功形状。
		receiver := newWebhookTestServer(t, http.StatusOK, `{"errcode":0,"errmsg":"ok"}`)
		payload := map[string]any{
			"name":         notificationsITWebhookMarker + "-delivery",
			"providerType": "wechat",
			"webhookUrl":   receiver.server.URL,
		}
		body, _ := json.Marshal(payload)
		created := integration.call(t, http.MethodPost, MountPrefix+"/webhook-targets", string(body))
		if created.Code != http.StatusCreated {
			t.Fatalf("建投递目标失败：%d（%s）", created.Code, created.Body.String())
		}
		var createdBody struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(created.Body.Bytes(), &createdBody)

		testBody := `{"notificationType":"circuit_breaker"}`
		recorder := integration.call(t, http.MethodPost,
			fmt.Sprintf("%s/webhook-targets/%d:test", MountPrefix, createdBody.ID), testBody)
		if recorder.Code != http.StatusOK {
			t.Fatalf("测试投递应得 200，实际 %d（%s）", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "latencyMs") {
			t.Fatalf("响应应含 latencyMs：%s", recorder.Body.String())
		}
		if receiver.requests.Load() != 1 {
			t.Fatalf("接收端应收 1 次，实际 %d", receiver.requests.Load())
		}

		// last_test_result 必须已写回（Node 同法）。
		pool, err := integration.pools.Control()
		if err != nil {
			t.Fatalf("取分道失败：%v", err)
		}
		var result []byte
		if err := pool.QueryRow(context.Background(),
			"SELECT last_test_result FROM webhook_targets WHERE id = $1", createdBody.ID).
			Scan(&result); err != nil {
			t.Fatalf("读回测试结果失败：%v", err)
		}
		// 库里是 jsonb：PG 回读时会规范化空白（`"success": true`），故按 JSON 解析而不是比字符串。
		var testResult struct {
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(result, &testResult); err != nil {
			t.Fatalf("测试结果不是 JSON：%s", string(result))
		}
		if !testResult.Success {
			t.Fatalf("测试结果应记为成功：%s", string(result))
		}

		// 不存在的目标的测试请求：404。
		missing := integration.call(t, http.MethodPost,
			fmt.Sprintf("%s/webhook-targets/%d:test", MountPrefix, createdBody.ID+1_000_000), testBody)
		if missing.Code != http.StatusNotFound {
			t.Fatalf("测试不存在的目标应 404，实际 %d（%s）", missing.Code, missing.Body.String())
		}
	})

	t.Run("删除幂等且是 204", func(t *testing.T) {
		recorder := integration.call(t, http.MethodDelete,
			fmt.Sprintf("%s/webhook-targets/%d", MountPrefix, id), "")
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("删除应得 204，实际 %d", recorder.Code)
		}
		again := integration.call(t, http.MethodDelete,
			fmt.Sprintf("%s/webhook-targets/%d", MountPrefix, id), "")
		if again.Code != http.StatusNoContent {
			t.Fatalf("重复删除仍应 204，实际 %d", again.Code)
		}
	})

	t.Run("严格的未知键与档位", func(t *testing.T) {
		recorder := integration.call(t, http.MethodPost, MountPrefix+"/webhook-targets",
			`{"name":"x","providerType":"custom","unknownKey":1}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("未知键应得 400，实际 %d（%s）", recorder.Code, recorder.Body.String())
		}
		for _, level := range integration.guard.snapshot() {
			if level != AccessAdmin {
				t.Fatalf("webhook-targets 各条都应是 admin 档位，实际出现 %s", level)
			}
		}
	})
}

// TestNotificationsRoutesEndToEnd 钉住设置与绑定的往返。
func TestNotificationsRoutesEndToEnd(t *testing.T) {
	integration := newNotificationsIT(t)
	targetID := integration.newTarget(t, "bindings")
	before := len(integration.audit.snapshot())

	t.Run("设置读写的脱敏与审计", func(t *testing.T) {
		read := integration.call(t, http.MethodGet, MountPrefix+"/notifications/settings", "")
		if read.Code != http.StatusOK {
			t.Fatalf("读设置应得 200，实际 %d（%s）", read.Code, read.Body.String())
		}

		payload := `{"circuitBreakerWebhook":"https://example.com/cb","enabled":true}`
		updated := integration.call(t, http.MethodPut, MountPrefix+"/notifications/settings", payload)
		if updated.Code != http.StatusOK {
			t.Fatalf("写设置应得 200，实际 %d（%s）", updated.Code, updated.Body.String())
		}
		if !strings.Contains(updated.Body.String(), `"circuitBreakerWebhook":"[REDACTED]"`) {
			t.Fatalf("legacy webhook 应脱敏：%s", updated.Body.String())
		}

		// 回显脱敏占位符时不得把 "[REDACTED]" 写进库。
		echo := integration.call(t, http.MethodPut, MountPrefix+"/notifications/settings",
			`{"circuitBreakerWebhook":"[REDACTED]"}`)
		if echo.Code != http.StatusOK {
			t.Fatalf("回显更新应得 200，实际 %d（%s）", echo.Code, echo.Body.String())
		}
		pool, err := integration.pools.Control()
		if err != nil {
			t.Fatalf("取分道失败：%v", err)
		}
		var stored *string
		if err := pool.QueryRow(context.Background(),
			"SELECT circuit_breaker_webhook FROM notification_settings LIMIT 1").Scan(&stored); err != nil {
			t.Fatalf("读回设置失败：%v", err)
		}
		if stored == nil || *stored == "[REDACTED]" {
			t.Fatalf("占位符不应落库，实际 %v", stored)
		}

		events := integration.audit.snapshot()
		if len(events) <= before {
			t.Fatal("设置更新应写下审计")
		}
		last := events[len(events)-1]
		if last.Category != "notification" || last.Action != "notification.update" || !last.Success {
			t.Fatalf("审计事件形状不对：%+v", last)
		}
	})

	t.Run("绑定整组替换与列表", func(t *testing.T) {
		payload := fmt.Sprintf(`{"items":[{"targetId":%d,"isEnabled":true}]}`, targetID)
		replaced := integration.call(t, http.MethodPut,
			MountPrefix+"/notifications/types/cost_alert/bindings", payload)
		if replaced.Code != http.StatusNoContent {
			t.Fatalf("替换绑定应得 204，实际 %d（%s）", replaced.Code, replaced.Body.String())
		}

		listed := integration.call(t, http.MethodGet,
			MountPrefix+"/notifications/types/cost_alert/bindings", "")
		if listed.Code != http.StatusOK {
			t.Fatalf("列绑定应得 200，实际 %d（%s）", listed.Code, listed.Body.String())
		}
		var body struct {
			Items []struct {
				TargetID int64 `json:"targetId"`
				Target   struct {
					Name        string            `json:"name"`
					WebhookURL  string            `json:"webhookUrl"`
					CustomHeads string            `json:"-"`
					Headers     map[string]string `json:"customHeaders"`
				} `json:"target"`
			} `json:"items"`
		}
		if err := json.Unmarshal(listed.Body.Bytes(), &body); err != nil {
			t.Fatalf("绑定响应不是 JSON：%v", err)
		}
		if len(body.Items) != 1 || body.Items[0].TargetID != targetID {
			t.Fatalf("绑定列表不符：%s", listed.Body.String())
		}
		if body.Items[0].Target.Headers["Authorization"] != "[REDACTED]" {
			t.Fatalf("内联目标的自定义头应脱敏：%s", listed.Body.String())
		}

		// 空列表 = 清空该类型的所有绑定。
		cleared := integration.call(t, http.MethodPut,
			MountPrefix+"/notifications/types/cost_alert/bindings", `{"items":[]}`)
		if cleared.Code != http.StatusNoContent {
			t.Fatalf("清空绑定应得 204，实际 %d", cleared.Code)
		}
		after := integration.call(t, http.MethodGet,
			MountPrefix+"/notifications/types/cost_alert/bindings", "")
		if !strings.Contains(after.Body.String(), `"items":[]`) {
			t.Fatalf("清空后应回空列表：%s", after.Body.String())
		}
	})

	t.Run("类型枚举非法得 400，测试 webhook 恒 200", func(t *testing.T) {
		bad := integration.call(t, http.MethodGet,
			MountPrefix+"/notifications/types/not_a_type/bindings", "")
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("非法类型应得 400，实际 %d", bad.Code)
		}
		// 本端点的渠道由 URL 主机名推断，只认企业微信与飞书（Node 同法）；本地假接收端必然
		// 落到「不支持的主机名」分支——这本身就是必须钉住的行为。
		receiver := newWebhookTestServer(t, http.StatusOK, `{"errcode":0,"errmsg":"ok"}`)
		payload := fmt.Sprintf(`{"webhookUrl":%q,"type":"cost_alert"}`, receiver.server.URL)
		tested := integration.call(t, http.MethodPost, MountPrefix+"/notifications/test-webhook", payload)
		if tested.Code != http.StatusOK {
			t.Fatalf("test-webhook 应恒 200，实际 %d（%s）", tested.Code, tested.Body.String())
		}
		if !strings.Contains(tested.Body.String(), `"success":false`) ||
			!strings.Contains(tested.Body.String(), "Unsupported webhook hostname") {
			t.Fatalf("非企业微信/飞书主机名应回 success=false：%s", tested.Body.String())
		}
		if receiver.requests.Load() != 0 {
			t.Fatalf("不应向不支持的主机名投递，实际 %d 次", receiver.requests.Load())
		}
	})
}

// TestAdminOpsRoutesEndToEnd 钉住两条 admin ops 的行为。
func TestAdminOpsRoutesEndToEnd(t *testing.T) {
	integration := newNotificationsIT(t)

	t.Run("日志级别读写", func(t *testing.T) {
		read := integration.call(t, http.MethodGet, "/api/admin/log-level", "")
		if read.Code != http.StatusOK {
			t.Fatalf("读日志级别应得 200，实际 %d", read.Code)
		}
		original := strings.TrimSpace(read.Body.String())
		t.Cleanup(func() {
			integration.call(t, http.MethodPost, "/api/admin/log-level", `{"level":"debug"}`)
		})

		invalid := integration.call(t, http.MethodPost, "/api/admin/log-level", `{"level":"verbose"}`)
		if invalid.Code != http.StatusBadRequest {
			t.Fatalf("非法级别应得 400，实际 %d", invalid.Code)
		}
		if !strings.Contains(invalid.Body.String(), "无效的日志级别") ||
			!strings.Contains(invalid.Body.String(), `"validLevels"`) {
			t.Fatalf("400 正文形状不对：%s", invalid.Body.String())
		}

		updated := integration.call(t, http.MethodPost, "/api/admin/log-level", `{"level":"warn"}`)
		if updated.Code != http.StatusOK {
			t.Fatalf("设置级别应得 200，实际 %d（%s）", updated.Code, updated.Body.String())
		}
		after := integration.call(t, http.MethodGet, "/api/admin/log-level", "")
		if !strings.Contains(after.Body.String(), `"level":"warn"`) {
			t.Fatalf("级别应已生效：%s（改前 %s）", after.Body.String(), original)
		}
	})

	t.Run("清理：无条件拒绝 / dryRun 计数 / 真删只删命中行", func(t *testing.T) {
		userID := integration.newMessageRequestOwner(t)

		noConditions := integration.call(t, http.MethodPost, "/api/admin/log-cleanup/manual", `{}`)
		if noConditions.Code != http.StatusOK {
			t.Fatalf("无条件请求应得 200（正文带 error），实际 %d", noConditions.Code)
		}
		if !strings.Contains(noConditions.Body.String(), "No cleanup conditions specified") {
			t.Fatalf("应拒绝无条件清理：%s", noConditions.Body.String())
		}

		dry := fmt.Sprintf(`{"userIds":[%d],"dryRun":true}`, userID)
		counted := integration.call(t, http.MethodPost, "/api/admin/log-cleanup/manual", dry)
		if counted.Code != http.StatusOK {
			t.Fatalf("dryRun 应得 200，实际 %d（%s）", counted.Code, counted.Body.String())
		}
		if !strings.Contains(counted.Body.String(), `"totalDeleted":2`) {
			t.Fatalf("dryRun 应数出 2 行：%s", counted.Body.String())
		}

		real := fmt.Sprintf(`{"userIds":[%d]}`, userID)
		deleted := integration.call(t, http.MethodPost, "/api/admin/log-cleanup/manual", real)
		if deleted.Code != http.StatusOK {
			t.Fatalf("真删应得 200，实际 %d（%s）", deleted.Code, deleted.Body.String())
		}
		// totalDeleted 只数「活跃行」，软删行单独计入 softDeletedPurged（Node 同口径）。
		if !strings.Contains(deleted.Body.String(), `"totalDeleted":1`) ||
			!strings.Contains(deleted.Body.String(), `"softDeletedPurged":1`) {
			t.Fatalf("真删口径不对：%s", deleted.Body.String())
		}
		if !strings.Contains(deleted.Body.String(), `"success":true`) {
			t.Fatalf("成功标志不对：%s", deleted.Body.String())
		}

		pool, err := integration.pools.Control()
		if err != nil {
			t.Fatalf("取分道失败：%v", err)
		}
		var remaining int
		if err := pool.QueryRow(context.Background(),
			"SELECT COUNT(*)::int FROM message_request WHERE user_id = $1", userID).Scan(&remaining); err != nil {
			t.Fatalf("统计剩余行失败：%v", err)
		}
		if remaining != 0 {
			t.Fatalf("命中行应全删，实际剩 %d", remaining)
		}
	})

	t.Run("档位为 admin", func(t *testing.T) {
		for _, level := range integration.guard.snapshot() {
			if level != AccessAdmin {
				t.Fatalf("admin ops 应是 admin 档位，实际 %s", level)
			}
		}
	})
}

// newMessageRequestOwner 造一个专属用户并插两行 message_request（一行软删），返回 userID。
//
// 只插必需列（provider_id / user_id / key），其余取列默认。
func (integration *notificationsIT) newMessageRequestOwner(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := integration.pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	name := fmt.Sprintf("go-admin-ops-it-%d-%d", time.Now().UnixNano(), os.Getpid())
	var userID int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING id`, name).
		Scan(&userID); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool, err := integration.pools.Writer()
		if err != nil {
			return
		}
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM message_request WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM usage_ledger WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, userID)
	})

	key := fmt.Sprintf("it-key-%d", userID)
	if _, err := pool.Exec(ctx, `INSERT INTO message_request (provider_id, user_id, key, created_at)
		VALUES (0, $1, $2, now())`, userID, key); err != nil {
		t.Fatalf("插日志行失败: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO message_request (provider_id, user_id, key, created_at, deleted_at)
		VALUES (0, $1, $2, now(), now())`, userID, key); err != nil {
		t.Fatalf("插软删行失败: %v", err)
	}
	return userID
}
