package adminapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// `?state=all` 的分页组装：截断为真时 hasMore* 必为真，而分页切片、总数与字段名都不变。
//
// 为什么单列一条：读数被 session.MaxSessionListIDs 截断后，若 hasMore* 仍按截断后的长度算，
// 调用方翻到最后一页会看到假的「到底了」——那正是「有上限但没透出上限」的静默错误。
func TestBuildAllSessionsBodyMarksTruncationAsMore(t *testing.T) {
	rows := make([]activeSessionBody, 10)
	for index := range rows {
		rows[index] = activeSessionBody{SessionID: "s-" + string(rune('a'+index))}
	}
	query := sessionsListQuery{State: "all", ActivePage: 1, InactivePage: 1, PageSize: 20}

	complete := buildAllSessionsBody(rows, nil, query, false)
	if complete.TotalActive != 10 || complete.HasMoreActive || complete.TotalInactive != 0 || complete.HasMoreInactive {
		t.Fatalf("未截断且一页装得下时不该报「还有更多」：%+v", complete)
	}
	if len(complete.Active) != 10 {
		t.Fatalf("分页切片不受截断标记影响，收到 %d 行", len(complete.Active))
	}

	truncated := buildAllSessionsBody(rows, nil, query, true)
	if !truncated.HasMoreActive || !truncated.HasMoreInactive {
		t.Fatalf("截断必须被透出（hasMore* 为真）：%+v", truncated)
	}
	if truncated.TotalActive != complete.TotalActive || len(truncated.Active) != len(complete.Active) {
		t.Fatalf("截断标记不得改动总数与切片：%+v", truncated)
	}

	// 字段名是监控与前端按名取值的契约，截断前后都不许改。
	encoded, err := json.Marshal(truncated)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, field := range []string{
		`"active"`, `"inactive"`, `"totalActive"`, `"totalInactive"`, `"hasMoreActive"`, `"hasMoreInactive"`,
	} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("响应字段 %s 缺失: %s", field, encoded)
		}
	}

	// 第二页越界时仍是空切片（Node 的 slice 语义），不是 null。
	pageTwo := sessionsListQuery{State: "all", ActivePage: 2, InactivePage: 1, PageSize: 20}
	if body := buildAllSessionsBody(rows, nil, pageTwo, false); body.Active == nil || len(body.Active) != 0 {
		t.Fatalf("越界页应为空数组，收到 %#v", body.Active)
	}
}
