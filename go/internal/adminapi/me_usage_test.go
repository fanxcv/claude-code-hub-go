package adminapi

import (
	"encoding/base64"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住 me 用量面里**不需要数据库**的三块：查询解析、日期窗换算、行投影。
// 它们各自对应 Node 的一处易错点，且错法都不会报错、只会静默给错数：
//   - 解析：游标必须两段齐全才算（Node 的 cursorCreatedAt && cursorId）；
//   - 日期窗：起点用**服务端时区**零点、终点取次日零点（开区间），时区错就是跨日错;
//   - 投影：计费模型随 billingModelSource 切换、成本取 numeric 文本、createdAt 只到毫秒。

func TestParseMeUsageLogsQueryCursorRequiresBothParts(t *testing.T) {
	cases := []struct {
		name      string
		target    string
		wantNil   bool
		wantID    int64
		wantLimit int
	}{
		{name: "两段齐全", target: "/x?cursorCreatedAt=2026-01-01T00:00:00.000000Z&cursorId=7",
			wantID: 7, wantLimit: meUsageLogsDefaultLimit},
		{name: "缺 id", target: "/x?cursorCreatedAt=2026-01-01T00:00:00.000000Z", wantNil: true,
			wantLimit: meUsageLogsDefaultLimit},
		{name: "缺时间", target: "/x?cursorId=7", wantNil: true, wantLimit: meUsageLogsDefaultLimit},
		{name: "自带 limit", target: "/x?limit=5", wantNil: true, wantLimit: 5},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			query, issues := parseMeUsageLogsQuery(httptest.NewRequest("GET", testCase.target, nil))
			if len(issues) != 0 {
				t.Fatalf("不该有校验错误: %+v", issues)
			}
			if testCase.wantNil {
				if query.Cursor != nil {
					t.Fatalf("不该解析出游标: %+v", query.Cursor)
				}
			} else if query.Cursor == nil || query.Cursor.ID != testCase.wantID {
				t.Fatalf("游标应为 id=%d，实际 %+v", testCase.wantID, query.Cursor)
			}
			if query.Limit != testCase.wantLimit {
				t.Fatalf("limit 应为 %d，实际 %d", testCase.wantLimit, query.Limit)
			}
		})
	}
}

func TestParseMeUsageLogsQueryRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		target string
		field  string
	}{
		{name: "limit 超上界", target: "/x?limit=101", field: "limit"},
		{name: "limit 非数", target: "/x?limit=abc", field: "limit"},
		{name: "page 下界", target: "/x?page=0", field: "page"},
		{name: "布尔非法", target: "/x?excludeStatusCode200=yes", field: "excludeStatusCode200"},
		{name: "cursorId 非正", target: "/x?cursorCreatedAt=2026-01-01T00:00:00Z&cursorId=0", field: "cursorId"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, issues := parseMeUsageLogsQuery(httptest.NewRequest("GET", testCase.target, nil))
			if len(issues) == 0 {
				t.Fatalf("%s 应报校验错误", testCase.target)
			}
			if path, _ := issues[0].Path[0].(string); path != testCase.field {
				t.Fatalf("错误应指向 %q，实际 %+v", testCase.field, issues)
			}
		})
	}
}

// TestParseMeUsageLogsQueryOffsetBranch 钉住「给了 page/pageSize 就走偏移分支」这一判据本身：
// 分支选错的表现是响应里 pageInfo 形状变化（前端分页器直接坏掉）。
func TestParseMeUsageLogsQueryOffsetBranch(t *testing.T) {
	offset, issues := parseMeUsageLogsQuery(httptest.NewRequest("GET", "/x?pageSize=1", nil))
	if len(issues) != 0 {
		t.Fatalf("不该有校验错误: %+v", issues)
	}
	if !offset.offsetPagination() {
		t.Fatal("给了 pageSize 应走偏移分支")
	}
	cursor, issues := parseMeUsageLogsQuery(httptest.NewRequest("GET", "/x?limit=1", nil))
	if len(issues) != 0 {
		t.Fatalf("不该有校验错误: %+v", issues)
	}
	if cursor.offsetPagination() {
		t.Fatal("只给 limit 应走游标分支")
	}
}

// TestMeUsageDateRangeUsesServerTimezone 钉住跨时区换算：+08:00 的 2026-01-02 零点
// 就是 UTC 的 2026-01-01T16:00:00Z，而终点是**次日**零点（开区间上界）。
func TestMeUsageDateRangeUsesServerTimezone(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载时区失败: %v", err)
	}
	start, end := meUsageDateRange("2026-01-02", "2026-01-02", shanghai)
	if start == nil || end == nil {
		t.Fatalf("应换算出两端: %v %v", start, end)
	}
	wantStart := time.Date(2026, 1, 2, 0, 0, 0, 0, shanghai).UnixMilli()
	wantEnd := time.Date(2026, 1, 3, 0, 0, 0, 0, shanghai).UnixMilli()
	if *start != wantStart {
		t.Fatalf("起点应为本地零点 %d，实际 %d", wantStart, *start)
	}
	if *end != wantEnd {
		t.Fatalf("终点应为次日零点 %d，实际 %d", wantEnd, *end)
	}

	// 非严格 ISO 日期按未提供处理（Node 的 /^\d{4}-\d{2}-\d{2}$/ 不匹配即 undefined）。
	if looseStart, looseEnd := meUsageDateRange("2026-1-2", "2026-01-02T00:00:00Z", time.UTC); looseStart != nil || looseEnd != nil {
		t.Fatalf("非严格日期不该参与换算: %v %v", looseStart, looseEnd)
	}
}

// TestProjectMeUsageEntryDerivations 钉住三处派生：计费模型随口径切换、重定向标注、成本与时间格式。
func TestProjectMeUsageEntryDerivations(t *testing.T) {
	original := "claude-3-opus-20240229"
	model := "claude-3-5-sonnet-20241022"
	cost := "0.250000000000000"
	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 678900000, time.UTC)
	cacheRead := int64(40)
	row := store.MeUsageSlimRow{
		ID:                   42,
		CreatedAt:            &createdAt,
		Model:                &model,
		OriginalModel:        &original,
		CostUSD:              &cost,
		CacheReadInputTokens: &cacheRead,
	}

	newSource := projectMeUsageEntry(row, "new")
	if newSource.BillingModel != model {
		t.Fatalf("billingModelSource=new 时计费模型应为请求模型，实际 %v", newSource.BillingModel)
	}
	if newSource.ModelRedirect != original+" → "+model {
		t.Fatalf("重定向标注应为 %q，实际 %v", original+" → "+model, newSource.ModelRedirect)
	}
	if newSource.Cost != 0.25 {
		t.Fatalf("成本应为 0.25，实际 %v", newSource.Cost)
	}
	if newSource.CreatedAt != "2026-01-02T03:04:05.678Z" {
		t.Fatalf("createdAt 应为毫秒精度 ISO，实际 %v", newSource.CreatedAt)
	}

	originalSource := projectMeUsageEntry(row, "original")
	if originalSource.BillingModel != original {
		t.Fatalf("billingModelSource=original 时计费模型应为原始模型，实际 %v", originalSource.BillingModel)
	}

	// 未记录 F3b 三列时，可用性标记为 not_recorded（与 Node 的 deriveRequestCacheMetrics 一致）。
	if newSource.RequestCacheMetricAvailability != string(cacheMetricNotRecorded) {
		t.Fatalf("缺 F3b 列时应为 not_recorded，实际 %s", newSource.RequestCacheMetricAvailability)
	}

	// 同模型时不得出现重定向标注。
	row.OriginalModel = &model
	if entry := projectMeUsageEntry(row, "new"); entry.ModelRedirect != nil {
		t.Fatalf("同模型不该有重定向标注: %v", entry.ModelRedirect)
	}
}

// TestMeUsageCursorTokenShape 钉住游标令牌：base64url 无填充，正文 {createdAt,id}。
func TestMeUsageCursorTokenShape(t *testing.T) {
	token, ok := meUsageCursorToken(&store.UsageLogCursor{
		CreatedAt: "2026-01-02T03:04:05.678900Z",
		ID:        9,
	}).(string)
	if !ok {
		t.Fatal("游标应为字符串")
	}
	// 用 RawURLEncoding 解码（与 normalizeUsageLogsCursor 的 base64url 无填充一致）。
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("不是合法的 base64url: %v", err)
	}
	if string(decoded) != `{"createdAt":"2026-01-02T03:04:05.678900Z","id":9}` {
		t.Fatalf("游标正文不符: %s", decoded)
	}
	if meUsageCursorToken(nil) != nil {
		t.Fatal("无游标时应渲染为 null")
	}
}
