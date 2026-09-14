package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本文件钉住审计 IP 的统一提取链（audit_ip.go）。四条必测路径（XFF 存在 / 不存在 / 多跳 /
// 配置为 cf-connecting-ip）之外，另钉两条安全不变量：默认链**不**信任最左 XFF 与
// cf-connecting-ip（它们在没有可信代理覆写时是客户端可控的），这是 Node 默认值的全部要点。

// auditIPRequest 造一个带指定头的请求。
func auditIPRequest(headers map[string]string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/keys/1", nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return request
}

// TestAuditClientIPDefaultChainPrefersRealIP 覆盖「XFF 存在 / x-real-ip 存在」，并钉住
// 默认链的次序：x-real-ip 优先于 x-forwarded-for。
func TestAuditClientIPDefaultChainPrefersRealIP(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name:    "只带 XFF 时取最右一段",
			headers: map[string]string{"X-Forwarded-For": "203.0.113.9, 198.51.100.7"},
			want:    "198.51.100.7",
		},
		{
			name: "x-real-ip 优先于 XFF",
			headers: map[string]string{
				"X-Real-IP":       "192.0.2.10",
				"X-Forwarded-For": "203.0.113.9, 198.51.100.7",
			},
			want: "192.0.2.10",
		},
		{
			name:    "单值 XFF",
			headers: map[string]string{"X-Forwarded-For": "203.0.113.9"},
			want:    "203.0.113.9",
		},
		{
			name:    "XFF 带端口与空白",
			headers: map[string]string{"X-Forwarded-For": " 203.0.113.9:41234 , 198.51.100.7 "},
			want:    "198.51.100.7",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// pools 为 nil 即「未装配 / 缓存冷启动」，走的正是默认链。
			got := auditClientIP(t.Context(), nil, auditIPRequest(testCase.headers))
			if got != testCase.want {
				t.Fatalf("审计 IP = %q，期望 %q", got, testCase.want)
			}
		})
	}
}

// TestAuditClientIPDefaultChainIgnoresUntrustedHeaders 钉住安全不变量：没有可配置链时，
// 客户端可控的 cf-connecting-ip 与最左 XFF 都不得成为审计 IP。
func TestAuditClientIPDefaultChainIgnoresUntrustedHeaders(t *testing.T) {
	t.Run("只有 cf-connecting-ip 时无值", func(t *testing.T) {
		got := auditClientIP(t.Context(), nil, auditIPRequest(map[string]string{
			"CF-Connecting-IP": "203.0.113.9",
		}))
		if got != "" {
			t.Fatalf("审计 IP = %q，期望空串（默认链不信任 cf-connecting-ip）", got)
		}
	})

	t.Run("左端伪造值不得胜出", func(t *testing.T) {
		// 攻击者伪造最左段，可信代理把真实地址追加到最右。
		got := auditClientIP(t.Context(), nil, auditIPRequest(map[string]string{
			"X-Forwarded-For": "1.2.3.4, 198.51.100.7",
		}))
		if got != "198.51.100.7" {
			t.Fatalf("审计 IP = %q，期望最右的 198.51.100.7", got)
		}
	})
}

// TestAuditClientIPWithoutHeaders 覆盖「XFF 不存在」：链走完无值即空串（Node 记 null）。
func TestAuditClientIPWithoutHeaders(t *testing.T) {
	if got := auditClientIP(t.Context(), nil, auditIPRequest(nil)); got != "" {
		t.Fatalf("无头请求的审计 IP = %q，期望空串", got)
	}
	// 头存在但都不是合法 IP：同样不得把垃圾值写进审计列。
	if got := auditClientIP(t.Context(), nil, auditIPRequest(map[string]string{
		"X-Real-IP":       "not-an-ip",
		"X-Forwarded-For": "also-not-an-ip",
	})); got != "" {
		t.Fatalf("非法头值的审计 IP = %q，期望空串", got)
	}
}

// TestAuditClientIPHonorsConfiguredChain 覆盖「配置为 cf-connecting-ip」：管理员显式信任
// 该头时，链首规则即命中，且缺省 pick 为最右。
func TestAuditClientIPHonorsConfiguredChain(t *testing.T) {
	rules, ok := parseAuditIPExtractionConfig(json.RawMessage(`{"headers":[{"name":"cf-connecting-ip"}]}`))
	if !ok {
		t.Fatal("配置解析失败")
	}
	request := auditIPRequest(map[string]string{
		"CF-Connecting-IP": "203.0.113.9",
		"X-Real-IP":        "192.0.2.10",
		"X-Forwarded-For":  "1.2.3.4, 198.51.100.7",
	})
	if got := extractAuditIP(request.Header, rules); got != "203.0.113.9" {
		t.Fatalf("审计 IP = %q，期望 cf-connecting-ip 的 203.0.113.9", got)
	}

	// 规则次序即优先级：把 XFF 放在前面时它胜出，且 pick 缺省为最右。
	rules, ok = parseAuditIPExtractionConfig(json.RawMessage(
		`{"headers":[{"name":"x-forwarded-for"},{"name":"cf-connecting-ip"}]}`))
	if !ok {
		t.Fatal("配置解析失败")
	}
	if got := extractAuditIP(request.Header, rules); got != "198.51.100.7" {
		t.Fatalf("审计 IP = %q，期望 XFF 最右的 198.51.100.7", got)
	}
}

// TestParseAuditIPExtractionConfigEdgeCases 钉住与 Node 一致的三处边界：显式空数组是
// 「不信任任何头」、单条非法规则只跳过自身、下标越界跳过该规则。
func TestParseAuditIPExtractionConfigEdgeCases(t *testing.T) {
	t.Run("null 与空字节视为未配置", func(t *testing.T) {
		for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(``)} {
			if _, ok := parseAuditIPExtractionConfig(raw); ok {
				t.Fatalf("raw=%q 应视为未配置（退回默认链）", string(raw))
			}
		}
	})

	t.Run("显式空数组是不信任任何头", func(t *testing.T) {
		rules, ok := parseAuditIPExtractionConfig(json.RawMessage(`{"headers":[]}`))
		if !ok {
			t.Fatal("空数组应被接受为一条有效配置")
		}
		if got := extractAuditIP(auditIPRequest(map[string]string{
			"X-Real-IP": "192.0.2.10",
		}).Header, rules); got != "" {
			t.Fatalf("空规则链的审计 IP = %q，期望空串", got)
		}
	})

	t.Run("非法单项只跳过自身", func(t *testing.T) {
		rules, ok := parseAuditIPExtractionConfig(json.RawMessage(
			`{"headers":[{"name":""},{"name":"x-forwarded-for","pick":"Leftmost"},{"name":"x-real-ip"}]}`))
		if !ok {
			t.Fatal("配置应被接受")
		}
		if len(rules) != 1 {
			t.Fatalf("有效规则数 = %d，期望 1", len(rules))
		}
		// "Leftmost" 在 Node 里是非法值（大小写敏感），整条跳过；剩下的 x-real-ip 胜出。
		if got := extractAuditIP(auditIPRequest(map[string]string{
			"X-Real-IP":       "192.0.2.10",
			"X-Forwarded-For": "1.2.3.4, 198.51.100.7",
		}).Header, rules); got != "192.0.2.10" {
			t.Fatalf("审计 IP = %q，期望 192.0.2.10", got)
		}
	})

	t.Run("下标越界跳过该规则", func(t *testing.T) {
		rules, ok := parseAuditIPExtractionConfig(json.RawMessage(
			`{"headers":[{"name":"x-forwarded-for","pick":{"kind":"index","index":5}},{"name":"x-real-ip"}]}`))
		if !ok {
			t.Fatal("配置应被接受")
		}
		if got := extractAuditIP(auditIPRequest(map[string]string{
			"X-Real-IP":       "192.0.2.10",
			"X-Forwarded-For": "1.2.3.4, 198.51.100.7",
		}).Header, rules); got != "192.0.2.10" {
			t.Fatalf("审计 IP = %q，期望跳过越界规则后取 192.0.2.10", got)
		}
	})

	t.Run("指定下标命中", func(t *testing.T) {
		rules, ok := parseAuditIPExtractionConfig(json.RawMessage(
			`{"headers":[{"name":"x-forwarded-for","pick":{"kind":"index","index":0}}]}`))
		if !ok {
			t.Fatal("配置应被接受")
		}
		if got := extractAuditIP(auditIPRequest(map[string]string{
			"X-Forwarded-For": "1.2.3.4, 198.51.100.7",
		}).Header, rules); got != "1.2.3.4" {
			t.Fatalf("审计 IP = %q，期望下标 0 的 1.2.3.4", got)
		}
	})
}

// TestNormalizeAuditIP 钉住规整规则（对齐 extract-client-ip.ts 的 normalizeIp）。
func TestNormalizeAuditIP(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"203.0.113.9", "203.0.113.9"},
		{"203.0.113.9:41234", "203.0.113.9"},
		{" 203.0.113.9 ", "203.0.113.9"},
		{"[2001:db8::1]", "2001:db8::1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"2001:db8::1", "2001:db8::1"},
		{"", ""},
		{"not-an-ip", ""},
		{"203.0.113.9:41:234", ""},
		{"[not-an-ip]", ""},
	}
	for _, testCase := range cases {
		if got := normalizeAuditIP(testCase.raw); got != testCase.want {
			t.Errorf("normalizeAuditIP(%q) = %q，期望 %q", testCase.raw, got, testCase.want)
		}
	}
}

// TestAuditClientIPWiredIntokeysAndUsersEmitters 钉住**接线**：两个发事件的入口（keys 的
// emitKeyAudit 与 users 的 emitUserAudit）都必须走统一链，而不是各自旧有的实现。
//
// 用例刻意让旧实现与正确结果不同：旧 keys 实现取最左段（伪造值），旧 users 实现取 RemoteAddr
// （离疑地址），两者都拿不到可信代理追加在最右的真实地址。
func TestAuditClientIPWiredIntokeysAndUsersEmitters(t *testing.T) {
	request := auditIPRequest(map[string]string{
		"X-Forwarded-For": "1.2.3.4, 198.51.100.7",
	})
	request.RemoteAddr = "10.9.9.9:51234"

	t.Run("keys", func(t *testing.T) {
		sink := &recordingAudit{}
		emitKeyAudit(Deps{Audit: sink}, request, AuditEvent{Action: "key.update"})
		if len(sink.events) != 1 {
			t.Fatalf("审计事件数 = %d，期望 1", len(sink.events))
		}
		if got := sink.events[0].IP; got != "198.51.100.7" {
			t.Fatalf("keys 审计 IP = %q，期望可信代理追加的 198.51.100.7", got)
		}
	})

	t.Run("users", func(t *testing.T) {
		sink := &recordingAudit{}
		api := &usersAPI{audit: sink}
		api.emitUserAudit(request, Principal{UserID: 1}, "user.update", 1, "someone", nil, nil, true, "")
		if len(sink.events) != 1 {
			t.Fatalf("审计事件数 = %d，期望 1", len(sink.events))
		}
		if got := sink.events[0].IP; got != "198.51.100.7" {
			t.Fatalf("users 审计 IP = %q，期望可信代理追加的 198.51.100.7", got)
		}
	})
}

// TestDefaultAuditIPChainMatchesNodeDefault 钉住默认链本身：与 types/ip-extraction.ts 的
// DEFAULT_IP_EXTRACTION_CONFIG 逐项一致（guard 侧的同名默认链由 adapters_ip_test.go 钉住；
// 两处若有一处改了默认值，这条会红）。
func TestDefaultAuditIPChainMatchesNodeDefault(t *testing.T) {
	rules := defaultAuditIPChain()
	want := []auditIPHeaderRule{
		{name: "x-real-ip", pick: auditIPPickMode{mode: auditIPPickRightmost}},
		{name: "x-forwarded-for", pick: auditIPPickMode{mode: auditIPPickRightmost}},
	}
	if len(rules) != len(want) {
		t.Fatalf("默认链规则数 = %d，期望 %d", len(rules), len(want))
	}
	for index := range want {
		if rules[index] != want[index] {
			t.Fatalf("默认链第 %d 条 = %+v，期望 %+v", index, rules[index], want[index])
		}
	}
}
