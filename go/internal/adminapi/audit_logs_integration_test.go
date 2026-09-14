package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 **audit-logs 资源**（/api/v1/audit-logs）的真实 PG 集成测试。
//
// 夹具直接向 audit_log 插行（写面由 adminapi/audit.go 覆盖，这里要的是读面），并用
// operator_user_name 上的唯一标记做清理定位——**不能**用 `DELETE FROM audit_log WHERE id > x`：
// 那种写法会把并发跑的其它用例（甚至本地开发库）的行一起删掉。
//
// 三条断言的核心是 keyset 游标：同一毫秒内的行只靠 created_at 分页会漏或重，故夹具刻意让
// 三条行的 created_at 只差毫秒。

// auditLogFixture 是一次夹具的定位信息。
//
// base 是三条夹具行的**起点时刻**（见 seedAuditLogFixture 的时间选取）：断言用它构造一个
// 只含本夹具的时间窗，否则库里其它既有审计行（本地开发库、并发跑的其它包）会让「翻出几条」
// 这类断言失真。
type auditLogFixture struct {
	marker string
	ids    []int64
	base   time.Time
}

// seedAuditLogFixture 插三条审计行（created_at 递增 1 毫秒，成败各半），外加一条别的分类。
func seedAuditLogFixture(t *testing.T, pools *store.Pools) auditLogFixture {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	marker := fmt.Sprintf("go-adminapi-audit-it-%d", time.Now().UnixNano())
	// 起点取「400 天前」而不是「刚刚」：审计表是共享的，把夹具摆在远离当下的时刻，窗口断言
	// 就不会被别的测试写进来的行碰到。
	base := time.Now().UTC().Truncate(time.Millisecond).AddDate(0, 0, -400)
	fixture := auditLogFixture{marker: marker, base: base}

	rows := []struct {
		category string
		action   string
		success  bool
		before   string
		after    string
		offset   time.Duration
	}{
		{"key", "key.create", true, `{"name":"before"}`, `{"name":"after"}`, 0},
		{"key", "key.update", false, `null`, `{"name":"after"}`, time.Millisecond},
		{"user", "user.delete", true, `{"name":"gone"}`, `null`, 2 * time.Millisecond},
	}
	for index, row := range rows {
		var id int64
		err := pool.QueryRow(ctx, `
			INSERT INTO audit_log (
				action_category, action_type, target_type, target_id, target_name,
				before_value, after_value,
				operator_user_id, operator_user_name, operator_key_id, operator_key_name,
				operator_ip, user_agent, success, error_message, created_at
			) VALUES (
				$1, $2, 'key', $3, $4,
				$5::jsonb, $6::jsonb,
				7, $7, 8, $8,
				'203.0.113.7', $9, $10, $11, $12
			) RETURNING id`,
			row.category, row.action, fmt.Sprintf("%d", index), marker+"-target",
			row.before, row.after, marker, marker+"-key", marker+"-agent",
			row.success, nil, base.Add(row.offset),
		).Scan(&id)
		if err != nil {
			t.Fatalf("插入审计夹具行失败: %v", err)
		}
		fixture.ids = append(fixture.ids, id)
	}

	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(ctx, `DELETE FROM audit_log WHERE operator_user_name = $1`, marker)
	})
	return fixture
}

// auditLogRouter 建一个以该身份注入的路由表（audit-logs 只有 admin 档位）。
func auditLogRouter(t *testing.T, pools *store.Pools, isAdmin bool) *Router {
	t.Helper()
	principal := Principal{
		UserID:   1,
		Username: "audit-logs-it",
		IsAdmin:  isAdmin,
	}
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: principal}}})
	RegisterAuditLogs(router, Deps{
		Guard:    principalGuard{principal: principal},
		Problems: NewProblems(nil),
		Store:    pools,
	})
	return router
}

// window 返回只含本夹具三行的时间窗（左闭右开：base-1s ~ base+4ms）。
//
// 半开区间由 from/to 两个参数分别施加；这里给出的是两端，便于两处断言各自取用。
func (f auditLogFixture) window() (from string, to string) {
	return f.base.Add(-time.Second).Format(auditLogISOMilli),
		f.base.Add(4 * time.Millisecond).Format(auditLogISOMilli)
}

// auditLogGet 发一次 GET，返回状态码与已解析的 JSON 正文（正文不是 JSON 时也返回原始串）。
func auditLogGet(t *testing.T, router *Router, target string) (int, map[string]any, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	raw := recorder.Body.String()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		return recorder.Code, nil, raw
	}
	return recorder.Code, body, raw
}

// auditLogItems 取出 items 数组（并校验它是数组）。
func auditLogItems(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("响应缺 items 数组：%+v", body)
	}
	items := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		item, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("items 元素应为对象：%+v", entry)
		}
		items = append(items, item)
	}
	return items
}

// TestAuditLogsKeysetPagination 钉住游标分页不漏不重（同一毫秒内的多行也要稳定分页）。
func TestAuditLogsKeysetPagination(t *testing.T) {
	pools := meOpenPools(t)
	fixture := seedAuditLogFixture(t, pools)
	router := auditLogRouter(t, pools, true)

	from, to := fixture.window()
	seen := map[int64]bool{}
	cursor := ""
	pages := 0
	for {
		target := "/audit-logs?limit=1&category=key&from=" + from + "&to=" + to
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		status, body, raw := auditLogGet(t, router, target)
		if status != http.StatusOK {
			t.Fatalf("状态应为 200，实际 %d：%s", status, raw)
		}
		items := auditLogItems(t, body)
		for _, item := range items {
			id := int64(item["id"].(float64))
			if seen[id] {
				t.Fatalf("游标分页出现重复行：id=%d", id)
			}
			seen[id] = true
		}
		pageInfo, ok := body["pageInfo"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺 pageInfo：%+v", body)
		}
		if limit := pageInfo["limit"].(float64); limit != 1 {
			t.Errorf("pageInfo.limit 应回显 1，实际 %v", limit)
		}
		pages++
		if pages > 5 {
			t.Fatal("分页未收敛（游标可能没生效）")
		}
		hasMore, _ := pageInfo["hasMore"].(bool)
		if !hasMore {
			if pageInfo["nextCursor"] != nil {
				t.Errorf("hasMore=false 时 nextCursor 应为 null，实际 %v", pageInfo["nextCursor"])
			}
			break
		}
		next, ok := pageInfo["nextCursor"].(string)
		if !ok || next == "" {
			t.Fatalf("hasMore=true 时应有 nextCursor：%+v", pageInfo)
		}
		cursor = next
	}

	// 夹具里 category=key 的行有两条（category=user 的那条不该出现）；窗口内不含别的行。
	if len(seen) != 2 {
		t.Fatalf("category=key 应翻出 2 行，实际 %d：%v", len(seen), seen)
	}
	for _, id := range fixture.ids[:2] {
		if !seen[id] {
			t.Errorf("夹具行 %d 未被翻出", id)
		}
	}
	if seen[fixture.ids[2]] {
		t.Errorf("category=user 的行 %d 不该出现在 category=key 的结果里", fixture.ids[2])
	}
}

// TestAuditLogsFilters 钉住三处筛选：success（只认字面量 "false"）、category、时间窗。
func TestAuditLogsFilters(t *testing.T) {
	pools := meOpenPools(t)
	fixture := seedAuditLogFixture(t, pools)
	router := auditLogRouter(t, pools, true)
	from, to := fixture.window()

	markerFiltered := func(t *testing.T, target string) []map[string]any {
		t.Helper()
		status, body, raw := auditLogGet(t, router, target)
		if status != http.StatusOK {
			t.Fatalf("状态应为 200，实际 %d：%s", status, raw)
		}
		return auditLogItems(t, body)
	}

	t.Run("success=false 只出失败行", func(t *testing.T) {
		items := markerFiltered(t, "/audit-logs?success=false&category=key&from="+from+"&to="+to)
		if len(items) != 1 {
			t.Fatalf("应只出 1 条失败行，实际 %d", len(items))
		}
		if items[0]["success"] != false {
			t.Errorf("过滤后仍有成功行：%+v", items[0])
		}
	})

	// success=1 不是 400 以外的任何东西（z.enum 的语义）——这里顺带钉住「不是被当没传」。
	t.Run("success=1 是校验错误", func(t *testing.T) {
		status, _, raw := auditLogGet(t, router, "/audit-logs?success=1")
		if status != http.StatusBadRequest {
			t.Fatalf("状态应为 400，实际 %d：%s", status, raw)
		}
	})

	t.Run("时间窗", func(t *testing.T) {
		// 夹具三条行依次在 base、base+1ms（category=key）与 base+2ms（category=user）。
		// from 取 base（闭区间左端）→ 两条 category=key；to 取 base → 只剩第一条。
		iso := fixture.base.Format(auditLogISOMilli)
		_, upper := fixture.window()
		items := markerFiltered(t, "/audit-logs?category=key&from="+iso+"&to="+upper)
		if len(items) != 2 {
			t.Fatalf("from 起算应有 2 条 category=key 的行，实际 %d", len(items))
		}
		items = markerFiltered(t, "/audit-logs?category=key&from="+iso+"&to="+iso)
		if len(items) != 1 {
			t.Fatalf("to 为上界（含）时应有 1 条，实际 %d", len(items))
		}
	})
}

// auditLogISOMilli 是带毫秒的 ISO 8601 格式（zod 的 datetime({offset:true}) 与 Go 的
// RFC3339 都接受；用 RFC3339 常量会丢毫秒，那样的窗口会把夹具行本身排除在外）。
const auditLogISOMilli = "2006-01-02T15:04:05.000Z07:00"

// TestAuditLogDetail 钉住单行读取：字段透传（jsonb 原样）+ 不存在时的 404。
func TestAuditLogDetail(t *testing.T) {
	pools := meOpenPools(t)
	fixture := seedAuditLogFixture(t, pools)
	router := auditLogRouter(t, pools, true)

	t.Run("命中", func(t *testing.T) {
		status, body, raw := auditLogGet(t,
			router, fmt.Sprintf("/audit-logs/%d", fixture.ids[0]))
		if status != http.StatusOK {
			t.Fatalf("状态应为 200，实际 %d：%s", status, raw)
		}
		if body["actionCategory"] != "key" || body["actionType"] != "key.create" {
			t.Errorf("分类/动作不对：%+v", body)
		}
		if body["success"] != true {
			t.Errorf("success 应透传：%+v", body)
		}
		if body["operatorIp"] != "203.0.113.7" {
			t.Errorf("operatorIp 应透传：%+v", body)
		}
		// jsonb 原样透传：beforeValue 是对象，afterValue 也应存在（不是字符串化的 JSON）。
		before, ok := body["beforeValue"].(map[string]any)
		if !ok || before["name"] != "before" {
			t.Errorf("beforeValue 应是原样的 JSON 对象：%+v", body["beforeValue"])
		}
		if _, ok := body["createdAt"].(string); !ok {
			t.Errorf("createdAt 应是 ISO 串：%+v", body["createdAt"])
		}
	})

	t.Run("不存在", func(t *testing.T) {
		status, body, raw := auditLogGet(t, router, "/audit-logs/2147483000")
		if status != http.StatusNotFound {
			t.Fatalf("状态应为 404，实际 %d：%s", status, raw)
		}
		if body == nil {
			t.Fatalf("404 应是 JSON problem：%s", raw)
		}
		if body["errorCode"] != "audit_log.not_found" {
			t.Errorf("errorCode 应为 audit_log.not_found，实际 %v", body["errorCode"])
		}
		if body["detail"] != "Audit log was not found." {
			t.Errorf("detail 应为 Node 的固定文案，实际 %v", body["detail"])
		}
	})

	t.Run("id 非正整数是校验错误", func(t *testing.T) {
		status, _, raw := auditLogGet(t, router, "/audit-logs/abc")
		if status != http.StatusBadRequest {
			t.Fatalf("状态应为 400，实际 %d：%s", status, raw)
		}
	})
}
