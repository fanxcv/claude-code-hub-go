package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// 表结构对应关系用 information_schema 实测断言，而不是人工抄写列清单：
// 只要 schema.ts 改了列名或可空性而这些断言没跟着改，测试就会红。

func tableColumns(t *testing.T, pools *Pools, table string) map[string]struct {
	nullable   bool
	hasDefault bool
} {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	rows, err := pool.Query(context.Background(),
		`SELECT column_name, is_nullable, (column_default IS NOT NULL)
		 FROM information_schema.columns WHERE table_name = $1`, table)
	if err != nil {
		t.Fatalf("读取 %s 的列定义失败: %v", table, err)
	}
	defer rows.Close()

	result := map[string]struct {
		nullable   bool
		hasDefault bool
	}{}
	for rows.Next() {
		var name, nullable string
		var hasDefault bool
		if err := rows.Scan(&name, &nullable, &hasDefault); err != nil {
			t.Fatalf("扫描列定义失败: %v", err)
		}
		result[name] = struct {
			nullable   bool
			hasDefault bool
		}{nullable: nullable == "YES", hasDefault: hasDefault}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历列定义失败: %v", err)
	}
	if len(result) == 0 {
		t.Fatalf("表 %s 不存在或没有列", table)
	}
	return result
}

// 写入路径引用的每一列都必须真实存在。
func TestSchemaDriftWriteColumnsExist(t *testing.T) {
	pools := openTestPools(t)
	columns := tableColumns(t, pools, "message_request")
	for _, column := range messageRequestColumns {
		if _, ok := columns[column]; !ok {
			t.Fatalf("message_request 不存在列 %s（写入路径会直接报错）", column)
		}
	}
}

// NOT NULL 且无默认值的列必须全部出现在插入列里，否则首次插入必然失败。
func TestSchemaDriftInsertCoversRequiredColumns(t *testing.T) {
	pools := openTestPools(t)
	columns := tableColumns(t, pools, "message_request")

	inserted := map[string]bool{}
	for _, column := range messageRequestColumns {
		inserted[column] = true
	}
	// 自增主键与带默认值的时间戳不需要显式写。
	for name, definition := range columns {
		if definition.nullable || definition.hasDefault {
			continue
		}
		if !inserted[name] {
			t.Fatalf("message_request.%s 是 NOT NULL 且无默认值，但插入列里没有它", name)
		}
	}
}

// 只读视图的 json tag 必须与真实列名一一对应，否则 row_to_json 的反序列化会静默漏字段。
func TestSchemaDriftReadStructTagsExist(t *testing.T) {
	pools := openTestPools(t)
	cases := []struct {
		table   string
		payload any
	}{
		{table: "system_settings", payload: SystemSettings{}},
		{table: "providers", payload: Provider{}},
		{table: "provider_endpoints", payload: ProviderEndpoint{}},
		{table: "keys", payload: APIKey{}},
		{table: "model_prices", payload: ModelPrice{}},
	}
	for _, testCase := range cases {
		columns := tableColumns(t, pools, testCase.table)
		value := reflect.TypeOf(testCase.payload)
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			tag := field.Tag.Get("json")
			name := strings.Split(tag, ",")[0]
			if name == "" || name == "-" {
				t.Fatalf("%s.%s 缺少 json tag", value.Name(), field.Name)
			}
			if _, ok := columns[name]; !ok {
				t.Fatalf("%s.%s 的 json tag %q 不是 %s 的列",
					value.Name(), field.Name, name, testCase.table)
			}
		}
	}
}

// 选路四列的可空性被 route 侧桥接依赖：priority 与 protocol_conversion_enabled 被读成必然存在的值
// （Node 用 `?? 0` 与 `=== true` 判读）。一旦改列成可空且出现 NULL，桥接就会静默给出错值，
// 因此在这里把可空性钉住。
func TestSchemaDriftRoutingColumnsNullable(t *testing.T) {
	pools := openTestPools(t)
	columns := tableColumns(t, pools, "providers")
	cases := []struct {
		column   string
		nullable bool
	}{
		{column: "provider_vendor_id", nullable: true},
		{column: "group_priorities", nullable: true},
		{column: "protocol_conversion_enabled", nullable: false},
		{column: "disable_session_reuse", nullable: false},
		{column: "priority", nullable: false},
	}
	for _, testCase := range cases {
		definition, ok := columns[testCase.column]
		if !ok {
			t.Fatalf("providers 不存在列 %s", testCase.column)
		}
		if definition.nullable != testCase.nullable {
			t.Fatalf("providers.%s 的 is_nullable = %v, 期望 %v",
				testCase.column, definition.nullable, testCase.nullable)
		}
	}
}

// 投影去重的幂等键必须真的是唯一约束，否则 ON CONFLICT (request_id) 会报错。
func TestSchemaDriftProjAppliedRequestUniqueKey(t *testing.T) {
	pools := openTestPools(t)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM pg_constraint
		 WHERE conrelid = 'proj_applied_requests'::regclass
		   AND contype IN ('p', 'u')
		   AND pg_get_constraintdef(oid) LIKE '%(request_id)%'`).Scan(&count); err != nil {
		t.Fatalf("查询 proj_applied_requests 约束失败: %v", err)
	}
	if count == 0 {
		t.Fatal("proj_applied_requests 缺少 request_id 上的主键/唯一约束，ON CONFLICT DO NOTHING 会失败")
	}
}

// usage_ledger 由触发器维护：Go 侧只读它，因此这条断言是「不要试图自己写账本」的依据。
func TestSchemaDriftUsageLedgerIsTriggerPopulated(t *testing.T) {
	pools := openTestPools(t)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
		 WHERE c.relname = 'message_request' AND NOT t.tgisinternal`).Scan(&count); err != nil {
		t.Fatalf("查询触发器失败: %v", err)
	}
	if count == 0 {
		t.Fatal("message_request 上没有触发器，usage_ledger 的写入来源与预期不符")
	}
}
