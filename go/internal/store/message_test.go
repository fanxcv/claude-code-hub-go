package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildDetailsPatchQueryOnlyIncludesProvidedColumns(t *testing.T) {
	statusCode := 503
	query, args := BuildDetailsPatchQuery(42, DetailsPatch{StatusCode: &statusCode})

	if !strings.Contains(query, `"status_code" = $1`) {
		t.Fatalf("缺少 status_code 赋值: %s", query)
	}
	if !strings.Contains(query, `"updated_at" = now()`) {
		t.Fatalf("缺少 updated_at: %s", query)
	}
	if !strings.Contains(query, "WHERE id = $2 AND status_code IS NULL RETURNING id") {
		t.Fatalf("幂等谓词或返回子句不对: %s", query)
	}
	// 未提供的列绝不能出现在 SET 里，否则会静默覆盖终态字段。
	for _, absent := range []string{"duration_ms", "input_tokens", "error_message", "model"} {
		if strings.Contains(query, absent) {
			t.Fatalf("未提供的列 %s 出现在 SET 中: %s", absent, query)
		}
	}
	if len(args) != 2 {
		t.Fatalf("参数个数 = %d, want 2", len(args))
	}
	// 整型必须以 int64 传参：pgx 不接受平台相关的 int。
	if got, ok := args[0].(int64); !ok || got != int64(statusCode) {
		t.Fatalf("第一个参数 = %#v, want int64(%d)", args[0], statusCode)
	}
	if got, ok := args[1].(int64); !ok || got != 42 {
		t.Fatalf("最后一个参数应为 int64(42)，实际 %#v", args[1])
	}
}

func TestBuildDetailsPatchQueryJSONBAssignmentCarriesCast(t *testing.T) {
	query, args := BuildDetailsPatchQuery(7, DetailsPatch{
		RoutingTrace: []byte(`{"version":1}`),
	})
	if !strings.Contains(query, `"routing_trace" = $1::jsonb`) {
		t.Fatalf("jsonb 赋值必须带 ::jsonb 转换: %s", query)
	}
	if len(args) != 2 {
		t.Fatalf("参数个数 = %d, want 2", len(args))
	}
	if string(args[0].([]byte)) != `{"version":1}` {
		t.Fatalf("jsonb 参数 = %v", args[0])
	}
}

func TestBuildDetailsPatchQueryEmptyPatchStillSetsUpdatedAt(t *testing.T) {
	query, args := BuildDetailsPatchQuery(9, DetailsPatch{})
	if query != `UPDATE message_request SET "updated_at" = now() WHERE id = $1 AND status_code IS NULL RETURNING id` {
		t.Fatalf("空 patch 的语句形状不对: %s", query)
	}
	if len(args) != 1 || args[0] != int64(9) {
		t.Fatalf("参数 = %v, want [9]", args)
	}
}

func TestMessageRequestColumnsAreUniqueAndCoverNotNull(t *testing.T) {
	seen := map[string]bool{}
	for _, column := range messageRequestColumns {
		if seen[column] {
			t.Fatalf("插入列重复: %s", column)
		}
		seen[column] = true
	}
	// 这四列在库里是 NOT NULL 且无默认值（information_schema 实测），漏掉会导致插入失败。
	for _, required := range []string{"provider_id", "user_id", "key", "is_replay"} {
		if !seen[required] {
			t.Fatalf("缺少 NOT NULL 列: %s", required)
		}
	}
}

func TestInsertValueRejectsUnknownColumn(t *testing.T) {
	if _, err := insertValue("not_a_column", CreateMessageRequestData{}); err == nil {
		t.Fatal("未知列必须报错，避免静默写出 NULL")
	}
}

func TestMarshalHedgeLoserEntryAndGuardShareDedupFields(t *testing.T) {
	entry := HedgeLoserEntry{ProviderID: 12, AttemptNumber: 2}
	guard, err := MarshalHedgeLoserGuard(entry)
	if err != nil {
		t.Fatalf("序列化去重键失败: %v", err)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(guard, &decoded); err != nil {
		t.Fatalf("去重键不是合法 JSON 数组: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("去重键应当只有一个元素，实际 %d", len(decoded))
	}
	if decoded[0]["providerId"] != float64(12) || decoded[0]["attemptNumber"] != float64(2) {
		t.Fatalf("去重键字段不对: %+v", decoded[0])
	}

	loser, err := MarshalHedgeLoserEntry(entry, "0.500000000000000")
	if err != nil {
		t.Fatalf("序列化输家条目失败: %v", err)
	}
	var loserDecoded []map[string]any
	if err := json.Unmarshal(loser, &loserDecoded); err != nil {
		t.Fatalf("输家条目不是合法 JSON 数组: %v", err)
	}
	if loserDecoded[0]["providerId"] != float64(12) {
		t.Fatalf("输家条目字段不对: %+v", loserDecoded[0])
	}
	// winner 的加法表达式读的就是 costUsd，缺了它赢家成本会漏计输家部分。
	if loserDecoded[0]["costUsd"] != "0.500000000000000" {
		t.Fatalf("输家条目缺少 costUsd: %+v", loserDecoded[0])
	}
	if _, ok := loserDecoded[0]["providerName"]; !ok {
		t.Fatalf("输家条目缺少 providerName: %+v", loserDecoded[0])
	}
}

func TestNumericArgAndJSONBArgTreatEmptyAsNull(t *testing.T) {
	empty := ""
	if got := numericArg(&empty); got != nil {
		t.Fatalf("空字符串应写成 NULL，实际 %v", got)
	}
	if got := numericArg(nil); got != nil {
		t.Fatalf("nil 应写成 NULL，实际 %v", got)
	}
	value := "1.5"
	if got := numericArg(&value); got != "1.5" {
		t.Fatalf("非空值应原样传参，实际 %v", got)
	}
	if got := jsonbArg(nil); got != nil {
		t.Fatalf("空 JSON 应写成 NULL，实际 %v", got)
	}
	if got := jsonbArg([]byte(`{}`)); string(got.([]byte)) != `{}` {
		t.Fatalf("非空 JSON 应原样传参，实际 %v", got)
	}
}

// 计费条件必须与 ledger-conditions.ts 的语义一致：排除拦截行、replay 行与不计费端点。
func TestBillingConditionShape(t *testing.T) {
	for _, fragment := range []string{"blocked_by IS NULL", "is_replay = false", "REGEXP_REPLACE"} {
		if !strings.Contains(BillingCondition, fragment) {
			t.Fatalf("计费条件缺少片段 %q: %s", fragment, BillingCondition)
		}
	}
	for _, endpoint := range NonBillingEndpoints {
		if !strings.Contains(BillingCondition, endpoint) {
			t.Fatalf("计费条件缺少不计费端点 %s", endpoint)
		}
	}
	if strings.Contains(AuditCondition, "is_replay = false") {
		t.Fatal("审计条件不得排除 replay 行")
	}
}

func TestLedgerEntityColumnMapping(t *testing.T) {
	cases := map[LedgerEntityType]string{
		LedgerEntityUser:     "user_id",
		LedgerEntityKey:      "key",
		LedgerEntityProvider: "final_provider_id",
	}
	for entityType, want := range cases {
		got, err := entityType.column()
		if err != nil {
			t.Fatalf("%s 应当有列映射: %v", entityType, err)
		}
		if got != want {
			t.Fatalf("%s -> %q, want %q", entityType, got, want)
		}
	}
	if _, err := LedgerEntityType("bogus").column(); err == nil {
		t.Fatal("未知主体类型必须报错")
	}
}
