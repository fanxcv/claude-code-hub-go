package store

import (
	"strings"
	"testing"
)

func TestNormalizeAdminSessionIdentityKind(t *testing.T) {
	prefix := "prefix_affinity"
	session := "session_id"
	unknown := "something_else"
	cases := []struct {
		name  string
		input *string
		want  string
	}{
		{"前缀亲和", &prefix, "prefix_affinity"},
		{"普通会话", &session, "session_id"},
		{"空值归为普通会话", nil, "session_id"},
		{"未知字面量归为普通会话", &unknown, "session_id"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := normalizeAdminSessionIdentityKind(item.input); got != item.want {
				t.Fatalf("= %q, want %q", got, item.want)
			}
		})
	}
}

func TestResolveAdminSessionIdentityKind(t *testing.T) {
	prefix := "prefix_affinity"
	session := "session_id"
	null := (*string)(nil)

	// 全同一种类 → 该种类。
	if got := resolveAdminSessionIdentityKind([]*string{&prefix, &prefix}); got == nil || *got != "prefix_affinity" {
		t.Fatalf("全前缀亲和应给出 prefix_affinity，实际 %v", got)
	}
	// NULL 与 'session_id' 同归一档，故不是混用。
	if got := resolveAdminSessionIdentityKind([]*string{&session, null}); got == nil || *got != "session_id" {
		t.Fatalf("NULL 与 session_id 应同归 session_id，实际 %v", got)
	}
	// 两类都有 → nil（Node 的 Set.size > 1）。
	if got := resolveAdminSessionIdentityKind([]*string{&prefix, &session}); got != nil {
		t.Fatalf("混用应给出 nil，实际 %q", *got)
	}
	// 空集合 → nil（Node 的 `[...[0] ?? null]`）。
	if got := resolveAdminSessionIdentityKind(nil); got != nil {
		t.Fatalf("空集合应给出 nil，实际 %q", *got)
	}
}

func TestResolveAdminSessionCacheTTLApplied(t *testing.T) {
	// 无值 → null。
	if got := resolveAdminSessionCacheTTLApplied(nil); got != nil {
		t.Fatalf("无值应为 nil，实际 %q", *got)
	}
	// 单值 → 原值。
	if got := resolveAdminSessionCacheTTLApplied([]string{"5m"}); got == nil || *got != "5m" {
		t.Fatalf("单值应为原值，实际 %v", got)
	}
	// 多值 → 字面量 "mixed"。
	if got := resolveAdminSessionCacheTTLApplied([]string{"5m", "1h"}); got == nil || *got != "mixed" {
		t.Fatalf("多值应为 mixed，实际 %v", got)
	}
	// 两个相同值不会出现：SQL 已 GROUP BY 去重，这里固定「长度即去重后个数」的契约。
	if got := resolveAdminSessionCacheTTLApplied([]string{"5m", "5m"}); got == nil || *got != "mixed" {
		t.Fatalf("长度 2 一律 mixed，实际 %v", got)
	}
}

func TestParseAdminSessionFingerprintChain(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"空输入", "", nil},
		{"json null", "null", nil},
		{"数组", `["a","b"]`, []string{"a", "b"}},
		{"跳过空串", `["a","","b"]`, []string{"a", "b"}},
		{"跳过非字符串项", `["a",1,{"x":1},["b"],"c"]`, []string{"a", "c"}},
		{"对象按非数组处理", `{"0":"a"}`, nil},
		{"标量按非数组处理", `"a"`, nil},
		{"坏 JSON 按非数组处理", `["a"`, nil},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			got := parseAdminSessionFingerprintChain([]byte(item.input))
			if len(got) != len(item.want) {
				t.Fatalf("= %v, want %v", got, item.want)
			}
			for index := range item.want {
				if got[index] != item.want[index] {
					t.Fatalf("= %v, want %v", got, item.want)
				}
			}
		})
	}
}

func TestAdminSessionProviderName(t *testing.T) {
	name := "OpenAI"
	empty := ""
	if got := adminSessionProviderName(&name, 7); got != "OpenAI" {
		t.Fatalf("有名应用名，实际 %q", got)
	}
	// 空串在 JS 里是假值，故同样走兜底。
	if got := adminSessionProviderName(&empty, 7); got != "Provider #7" {
		t.Fatalf("空名应回退，实际 %q", got)
	}
	if got := adminSessionProviderName(nil, 12); got != "Provider #12" {
		t.Fatalf("无名应回退，实际 %q", got)
	}
}

func TestLedgerCanonicalSessionCondition(t *testing.T) {
	// 非保留前缀：只比规范列，值占位符只占一个参数。
	args := []any{}
	got := ledgerCanonicalSessionCondition("sid-a", &args)
	if got != "COALESCE(session_identity, session_id) = $1" {
		t.Fatalf("非保留前缀条件不对: %s", got)
	}
	if len(args) != 1 || args[0] != "sid-a" {
		t.Fatalf("参数不对: %#v", args)
	}

	// 保留前缀：追加物理列条件，且复用同一占位符（不额外占参数）。
	args = []any{}
	got = ledgerCanonicalSessionCondition("pfx:abc", &args)
	if !strings.Contains(got, "COALESCE(session_identity, session_id) = $1") {
		t.Fatalf("缺少规范列条件: %s", got)
	}
	if !strings.Contains(got, "AND session_identity = $1") {
		t.Fatalf("保留前缀必须同时约束物理列: %s", got)
	}
	if len(args) != 1 {
		t.Fatalf("复用占位符时参数应为 1 个，实际 %#v", args)
	}
}

func TestLedgerPerSessionOwnerConditionKeepsOwnersSeparate(t *testing.T) {
	args := []any{}
	got := ledgerPerSessionOwnerCondition([]adminSessionLedgerRef{
		{CanonicalIdentity: "sid-a", UserID: 11},
		{CanonicalIdentity: "pfx:b", UserID: 22},
	}, &args)

	// 两个会话必须各自绑定自己的 user_id——共用一个 owner 会把不同用户的账本行并进来。
	if !strings.Contains(got, "COALESCE(session_identity, session_id) = $1 AND user_id = $2") {
		t.Fatalf("首个会话条件不对: %s", got)
	}
	if !strings.Contains(got, "COALESCE(session_identity, session_id) = $3 AND session_identity = $3 AND user_id = $4") {
		t.Fatalf("第二个（保留前缀）会话条件不对: %s", got)
	}
	if !strings.HasPrefix(got, "(") || !strings.HasSuffix(got, ")") {
		t.Fatalf("整体应被括号包住: %s", got)
	}
	want := []any{"sid-a", int64(11), "pfx:b", int64(22)}
	if len(args) != len(want) {
		t.Fatalf("参数个数 = %d, want %d (%#v)", len(args), len(want), args)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("参数 %d = %#v, want %#v", index, args[index], want[index])
		}
	}
}

func TestPhysicalSessionSourceKey(t *testing.T) {
	if got := physicalSessionSourceKey("sid-a", 3); got != "sid-a:3" {
		t.Fatalf("= %q, want %q", got, "sid-a:3")
	}
	// 同 session 不同 key、同 key 不同 session 都必须分开。
	seen := map[string]struct{}{}
	for _, item := range []struct {
		sessionID string
		keyID     int64
	}{
		{"sid-a", 3}, {"sid-a", 4}, {"sid-b", 3},
	} {
		seen[physicalSessionSourceKey(item.sessionID, item.keyID)] = struct{}{}
	}
	if len(seen) != 3 {
		t.Fatalf("三个组合应给出三个键，实际 %d", len(seen))
	}
}

func TestAdminSessionCanonicalConditionColumns(t *testing.T) {
	// prefix 为空串时必须与改造前逐字相同（既有端点 /sessions/{id}/requests 走的就是这一支）：
	// 这一支没有真库或单测覆盖，故在此钉住列名与占位符形状。
	condition, args := adminSessionCanonicalCondition("sid-a", 0, "")
	if condition != "COALESCE(session_identity, session_id) = $1" {
		t.Fatalf("单表非保留条件 = %q", condition)
	}
	if len(args) != 1 || args[0] != "sid-a" {
		t.Fatalf("参数 = %#v", args)
	}

	// 带 JOIN 的查询必须限定前缀，否则与 keys.user_id 撞名。
	condition, args = adminSessionCanonicalCondition("sid-a", 77, "mr.")
	if condition != "COALESCE(mr.session_identity, mr.session_id) = $1 AND mr.user_id = $2" {
		t.Fatalf("带前缀条件 = %q", condition)
	}
	if len(args) != 2 || args[0] != "sid-a" || args[1] != int64(77) {
		t.Fatalf("参数 = %#v", args)
	}

	// 保留前缀 + owner：物理列条件单独占一个占位符（与 Node 的两个 drizzle 参数同形，
	// 两个参数值相同），owner 接着占一个。
	condition, args = adminSessionCanonicalCondition("pfx:x", 77, "mr.")
	want := "COALESCE(mr.session_identity, mr.session_id) = $1 AND " +
		"(mr.session_identity = $2 OR mr.session_identity IS NULL) AND mr.user_id = $3"
	if condition != want {
		t.Fatalf("保留前缀条件 = %q, want %q", condition, want)
	}
	wantArgs := []any{"pfx:x", "pfx:x", int64(77)}
	if len(args) != len(wantArgs) {
		t.Fatalf("参数 = %#v", args)
	}
	for index := range wantArgs {
		if args[index] != wantArgs[index] {
			t.Fatalf("参数 %d = %#v, want %#v", index, args[index], wantArgs[index])
		}
	}

	// 保留前缀不带 owner：物理列必须**非空**等于该串（否则会把物理 session_id 同名的行放进来）。
	condition, args = adminSessionCanonicalCondition("pfx:x", 0, "mr.")
	want = "COALESCE(mr.session_identity, mr.session_id) = $1 AND mr.session_identity = $2"
	if condition != want {
		t.Fatalf("保留前缀无 owner 条件 = %q, want %q", condition, want)
	}
	if len(args) != 2 {
		t.Fatalf("参数 = %#v", args)
	}
}

func TestContainsString(t *testing.T) {
	if !containsString([]string{"a", "b"}, "b") {
		t.Fatal("应命中")
	}
	if containsString([]string{"a", "b"}, "c") {
		t.Fatal("不应命中")
	}
	if containsString(nil, "a") {
		t.Fatal("空切片不应命中")
	}
}
