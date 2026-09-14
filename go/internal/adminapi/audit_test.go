package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// TestAuditEmitWritesRow 真落库：列映射与脱敏都按 Node 的口径。
func TestAuditEmitWritesRow(t *testing.T) {
	pools := testPools(t)
	userID := fixtureUser(t, pools, "admin", true)
	audit, err := NewAuditLog(Deps{}, AuditLogOptions{Pools: pools})
	if err != nil {
		t.Fatalf("建审计写入器失败: %v", err)
	}

	marker := fmt.Sprintf("%d", time.Now().UnixNano())
	audit.Emit(context.Background(), AuditEvent{
		Category:   "key",
		Principal:  Principal{UserID: userID, Username: "夹具操作人", KeyID: -1, KeyName: "管理台密钥"},
		Action:     "key.create",
		TargetType: "key",
		TargetID:   marker,
		TargetName: "demo-key",
		IP:         "203.0.113.9",
		UserAgent:  "curl/8.5.0",
		Success:    true,
		Before:     map[string]any{"enabled": false},
		Details: map[string]any{
			"name": "demo",
			"key":  "sk-should-be-redacted",
			"nested": map[string]any{
				"apiKey": "sk-nested-secret",
				"plain":  "keep-me",
			},
			"list": []any{map[string]any{"token": "tok-secret"}, "kept"},
		},
	})

	row := awaitAuditRow(t, pools, marker)
	t.Cleanup(func() { deleteAuditRow(t, pools, row["id"]) })

	if row["action_category"] != "key" || row["action_type"] != "key.create" {
		t.Fatalf("分类/动作不符: %v / %v", row["action_category"], row["action_type"])
	}
	if row["target_type"] != "key" || row["target_id"] != marker {
		t.Fatalf("目标字段不符: %v / %v", row["target_type"], row["target_id"])
	}
	if fmt.Sprint(row["operator_user_id"]) != fmt.Sprint(userID) {
		t.Fatalf("操作人 id 不符: %v", row["operator_user_id"])
	}
	if row["operator_user_name"] != "夹具操作人" {
		t.Fatalf("操作人名不符: %v", row["operator_user_name"])
	}
	if fmt.Sprint(row["operator_key_id"]) != "-1" {
		t.Fatalf("密钥 id 不符（ADMIN_TOKEN 虚拟密钥为 -1）: %v", row["operator_key_id"])
	}
	if row["operator_ip"] != "203.0.113.9" {
		t.Fatalf("IP 不符: %v", row["operator_ip"])
	}
	if row["success"] != true {
		t.Fatalf("显式 Success=true 应原样落库: %v", row["success"])
	}
	if row["target_name"] != "demo-key" {
		t.Fatalf("target_name 不符: %v", row["target_name"])
	}
	if row["operator_key_name"] != "管理台密钥" {
		t.Fatalf("operator_key_name 不符: %v", row["operator_key_name"])
	}
	if row["user_agent"] != "curl/8.5.0" {
		t.Fatalf("user_agent 不符: %v", row["user_agent"])
	}
	if row["error_message"] != nil {
		t.Fatalf("成功审计的 error_message 应为 NULL: %v", row["error_message"])
	}
	before, _ := row["before_value"].(map[string]any)
	if before == nil || before["enabled"] != false {
		t.Fatalf("before_value 不符: %v", row["before_value"])
	}

	snapshot, _ := row["after_value"].(map[string]any)
	if snapshot == nil {
		t.Fatalf("after_value 应被写入: %v", row["after_value"])
	}
	if snapshot["key"] != "[REDACTED]" {
		t.Fatalf("顶层敏感键未脱敏: %v", snapshot["key"])
	}
	if snapshot["name"] != "demo" {
		t.Fatalf("非敏感键不该被改写: %v", snapshot["name"])
	}
	nested, _ := snapshot["nested"].(map[string]any)
	if nested == nil || nested["apiKey"] != "[REDACTED]" || nested["plain"] != "keep-me" {
		t.Fatalf("嵌套脱敏不符: %v", snapshot["nested"])
	}
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "sk-nested-secret") || strings.Contains(string(encoded), "tok-secret") {
		t.Fatalf("快照里仍有凭据原文: %s", encoded)
	}
}

// TestAuditCategoryDerivation 覆盖前缀表（含 login.* 归 auth 这一族）。
func TestAuditCategoryDerivation(t *testing.T) {
	cases := map[string]string{
		"key.create":             "key",
		"user.update":            "user",
		"provider.delete":        "provider",
		"provider_group.create":  "provider_group",
		"system_settings.update": "system_settings",
		"notification.create":    "notification",
		"sensitive_word.update":  "sensitive_word",
		"model_price.create":     "model_price",
		"login.success":          "auth",
		"auth.something":         "auth",
	}
	for action, want := range cases {
		got, ok := auditCategory(action)
		if !ok || got != want {
			t.Errorf("auditCategory(%q)=(%q,%v), 期望 %q", action, got, ok, want)
		}
	}
	for _, action := range []string{"", "   ", "nosuchcat.do"} {
		if _, ok := auditCategory(action); ok {
			t.Errorf("auditCategory(%q) 不该命中", action)
		}
	}
}

// TestAuditUnknownCategorySkipsWrite 验证推导不出分类时记 warn 且不落库。
func TestAuditUnknownCategorySkipsWrite(t *testing.T) {
	pools := testPools(t)
	audit, err := NewAuditLog(Deps{}, AuditLogOptions{Pools: pools})
	if err != nil {
		t.Fatalf("建审计写入器失败: %v", err)
	}
	marker := fmt.Sprintf("%d", time.Now().UnixNano())
	audit.Emit(context.Background(), AuditEvent{Action: "nosuchcat.do", TargetID: marker})

	time.Sleep(300 * time.Millisecond)
	if count := countAuditRows(t, pools, marker); count != 0 {
		t.Fatalf("分类未知时不该落库，实际 %d 行", count)
	}
}

// TestAuditRedaction 单测脱敏本身（大小写不敏感、数组、深度上限、不改写非敏感值）。
func TestAuditRedaction(t *testing.T) {
	input := map[string]any{
		"Key":            "a",
		"API_KEY":        "b",
		"ApiKey":         "c",
		"webhook-secret": "d",
		"Authorization":  "e",
		"keep":           "f",
		"deep":           map[string]any{"level": "g"},
	}
	redacted, _ := redactSensitive(input, 0).(map[string]any)
	for _, key := range []string{"Key", "API_KEY", "ApiKey", "webhook-secret", "Authorization"} {
		if redacted[key] != auditRedacted {
			t.Errorf("%q 未脱敏: %v", key, redacted[key])
		}
	}
	if redacted["keep"] != "f" {
		t.Errorf("非敏感键被改写: %v", redacted["keep"])
	}

	deep := map[string]any{}
	cursor := deep
	for index := 0; index < auditMaxDepth+4; index++ {
		next := map[string]any{}
		cursor["next"] = next
		cursor = next
	}
	encoded, err := json.Marshal(redactSensitive(deep, 0))
	if err != nil {
		t.Fatalf("深度受限的脱敏应可序列化: %v", err)
	}
	if !strings.Contains(string(encoded), auditCircular) {
		t.Fatalf("超深嵌套应被 [Circular] 截断: %s", encoded)
	}
}

// TestAuditNilDetailsWritesNull 验证无快照时 after_value 为 NULL。
func TestAuditNilDetailsWritesNull(t *testing.T) {
	pools := testPools(t)
	audit, err := NewAuditLog(Deps{}, AuditLogOptions{Pools: pools})
	if err != nil {
		t.Fatalf("建审计写入器失败: %v", err)
	}
	marker := fmt.Sprintf("%d", time.Now().UnixNano())
	audit.Emit(context.Background(), AuditEvent{Action: "user.delete", TargetID: marker})

	row := awaitAuditRow(t, pools, marker)
	t.Cleanup(func() { deleteAuditRow(t, pools, row["id"]) })
	if row["after_value"] != nil {
		t.Fatalf("无快照时 after_value 应为 NULL: %v", row["after_value"])
	}
	if row["action_category"] != "user" {
		t.Fatalf("分类不符: %v", row["action_category"])
	}
}

// TestAuditFailureWritesRow 验证失败审计真落库：success=false、error_message 与快照齐备。
//
// 这条是 A0-3 扩面的直接验收：扩面之前该事件只能写成 success = TRUE（假成功）。
func TestAuditFailureWritesRow(t *testing.T) {
	pools := testPools(t)
	audit, err := NewAuditLog(Deps{}, AuditLogOptions{Pools: pools})
	if err != nil {
		t.Fatalf("建审计写入器失败: %v", err)
	}
	marker := fmt.Sprintf("%d", time.Now().UnixNano())
	audit.Emit(context.Background(), AuditEvent{
		// Category 留空，走前缀推导：auth 类事件的 action 是 login.*，分类与前缀不相等。
		Principal:    Principal{UserID: 3, Username: "被拒者"},
		Action:       "login.failure",
		TargetType:   "user",
		TargetID:     marker,
		TargetName:   "被拒者",
		UserAgent:    "Mozilla/5.0",
		Success:      false,
		ErrorMessage: "Invalid credentials",
		Details:      map[string]any{"password": "should-be-redacted"},
	})

	row := awaitAuditRow(t, pools, marker)
	t.Cleanup(func() { deleteAuditRow(t, pools, row["id"]) })

	if row["success"] != false {
		t.Fatalf("失败审计的 success 应为 false: %v", row["success"])
	}
	if row["error_message"] != "Invalid credentials" {
		t.Fatalf("error_message 不符: %v", row["error_message"])
	}
	if row["action_category"] != "auth" {
		t.Fatalf("login.* 应归 auth: %v", row["action_category"])
	}
	if row["target_name"] != "被拒者" {
		t.Fatalf("target_name 不符: %v", row["target_name"])
	}
	if row["user_agent"] != "Mozilla/5.0" {
		t.Fatalf("user_agent 不符: %v", row["user_agent"])
	}
	snapshot, _ := row["after_value"].(map[string]any)
	if snapshot == nil || snapshot["password"] != auditRedacted {
		t.Fatalf("失败审计的快照也要脱敏: %v", row["after_value"])
	}
}

// TestResolveAuditCategory 验证显式 category 优先、非法显式值被拒、空值时退回前缀推导。
func TestResolveAuditCategory(t *testing.T) {
	cases := []struct {
		category string
		action   string
		want     string
		ok       bool
	}{
		{category: "system_settings", action: "key.delete", want: "system_settings", ok: true},
		{category: "", action: "key.delete", want: "key", ok: true},
		{category: "nosuchcat", action: "key.delete", want: "", ok: false},
		{category: "", action: "nosuchcat.do", want: "", ok: false},
	}
	for _, item := range cases {
		got, ok := resolveAuditCategory(item.category, item.action)
		if got != item.want || ok != item.ok {
			t.Errorf("resolveAuditCategory(%q,%q)=(%q,%v), 期望 (%q,%v)",
				item.category, item.action, got, ok, item.want, item.ok)
		}
	}
}

// awaitAuditRow 轮询等待审计行出现（Emit 是 fire-and-forget）。
func awaitAuditRow(t *testing.T, pools *store.Pools, marker string) map[string]any {
	t.Helper()
	const query = `SELECT row_to_json(t)::text FROM (
		SELECT id, action_category, action_type, target_type, target_id, target_name,
		       before_value, after_value, operator_user_id, operator_user_name, operator_key_id,
		       operator_key_name, operator_ip, user_agent, success, error_message
		FROM audit_log WHERE target_id = $1 ORDER BY id DESC LIMIT 1
	) t`
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := queryJSONRows(context.Background(), pools, query, marker)
		if err == nil && len(rows) > 0 {
			var row map[string]any
			if err := json.Unmarshal([]byte(rows[0]), &row); err != nil {
				t.Fatalf("审计行解析失败: %v", err)
			}
			return row
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待审计行超时（target_id=%s）", marker)
	return nil
}

// countAuditRows 数目标 id 的行数。
func countAuditRows(t *testing.T, pools *store.Pools, marker string) int {
	t.Helper()
	const query = `SELECT row_to_json(t)::text FROM (
		SELECT count(*) AS row_count FROM audit_log WHERE target_id = $1
	) t`
	rows, err := queryJSONRows(context.Background(), pools, query, marker)
	if err != nil {
		t.Fatalf("统计审计行失败: %v", err)
	}
	if len(rows) == 0 {
		return 0
	}
	var row struct {
		Count float64 `json:"row_count"`
	}
	if err := json.Unmarshal([]byte(rows[0]), &row); err != nil {
		t.Fatalf("统计结果解析失败: %v", err)
	}
	return int(row.Count)
}

// deleteAuditRow 删除一行审计。
func deleteAuditRow(t *testing.T, pools *store.Pools, id any) {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		return
	}
	_, _ = pool.Exec(context.Background(), `DELETE FROM audit_log WHERE id = $1::bigint`, fmt.Sprint(id))
}
