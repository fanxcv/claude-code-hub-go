package adminapi

import (
	"strings"
	"testing"
)

// 本文件是值形态脱敏（error_text_redact.go）的纯函数用例。
//
// 为什么逐条钉：这些规则是**从 Node 移植的契约**（同一条错误文案在两界面上必须被改写成同一形状），
// 将来有人"顺手优化"正则（例如把 `{16,}` 改小、把顺序调换）就会静默改变契约形状——
// 而这类改动在集成用例里多半仍然"能过"。故每条规则各钉一个样例。
//
// 同时钉住**有意超出 Node 的那一条**（URL 内嵌凭据）与它存在的理由：裸 IP/带端口主机。

func TestRedactErrorTextRules(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		mustHave []string
		mustNot  []string
	}{
		{
			name:     "URL 内嵌凭据（域名主机）",
			input:    "dial https://leaky-user:leaky-pass@relay.example.com/v1 failed",
			mustHave: []string{"https://[REDACTED]@"},
			mustNot:  []string{"leaky-user", "leaky-pass"},
		},
		{
			// 这条就是扩展的**理由**：Node 的规则集在裸 IP / 带端口主机下整段留明文。
			name:     "URL 内嵌凭据（裸 IP + 端口）",
			input:    "dial https://relay-user:relay-pass@192.0.2.10:8080/v1/responses failed",
			mustHave: []string{"https://[REDACTED]@"},
			mustNot:  []string{"relay-user", "relay-pass"},
		},
		{
			name:     "Bearer token",
			input:    "upstream said 401 with Authorization: Bearer abcDEF123456ghiJKL",
			mustHave: []string{"Bearer [REDACTED]"},
			mustNot:  []string{"abcDEF123456ghiJKL"},
		},
		{
			name:     "sk- 前缀密钥（≥16 位）",
			input:    "invalid key sk-proj-ABCDEFGHIJKLMNOPQRSTUVWX rejected",
			mustHave: []string{"[REDACTED_KEY]"},
			mustNot:  []string{"sk-proj-ABCDEFGHIJKLMNOPQRSTUVWX"},
		},
		{
			name:     "Google AIza 密钥",
			input:    "key AIzaSyA1B2C3D4E5F6G7H8I9J0KLMNOP invalid",
			mustHave: []string{"[REDACTED_KEY]"},
			mustNot:  []string{"AIzaSyA1B2C3D4E5F6G7H8I9J0KLMNOP"},
		},
		{
			name:     "JWT",
			input:    "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk rejected",
			mustHave: []string{"[JWT]"},
		},
		{
			name:     "邮箱",
			input:    "contact ops-leak@example.com for access",
			mustHave: []string{"[EMAIL]"},
			mustNot:  []string{"ops-leak@example.com"},
		},
		{
			name:     "键值形态的密码",
			input:    "config password=hunter2secret loaded",
			mustHave: []string{"password:***"},
			mustNot:  []string{"hunter2secret"},
		},
		{
			name:     "配置文件路径",
			input:    "failed to read /etc/cch-secret.env",
			mustHave: []string{"[PATH]"},
			mustNot:  []string{"/etc/cch-secret.env"},
		},
		{
			// 干净文案**不得**被改写：否则 redacted 标记永远为 true、失去信息量。
			name:    "干净文案保持不变",
			input:   "upstream_error",
			mustNot: []string{"[REDACTED", "[EMAIL]", "[JWT]", "[PATH]"},
		},
		{
			name:    "context canceled 保持不变",
			input:   "context canceled",
			mustNot: []string{"[REDACTED", "[EMAIL]", "[JWT]", "[PATH]"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			output, changed := redactErrorText(testCase.input)
			for _, fragment := range testCase.mustHave {
				if !strings.Contains(output, fragment) {
					t.Errorf("输出应含 %q，实际 %q", fragment, output)
				}
			}
			for _, fragment := range testCase.mustNot {
				if strings.Contains(output, fragment) {
					t.Errorf("输出不应含 %q，实际 %q", fragment, output)
				}
			}
			// changed 与「输出是否等于输入」必须一致：它是界面判断「这是改写过的文案」的唯一依据。
			if changed != (output != testCase.input) {
				t.Errorf("changed=%v 与实际是否改写不一致（输入 %q，输出 %q）", changed, testCase.input, output)
			}
		})
	}
}

// TestRedactErrorTextPointerHandlesNil 钉住可空字段的包装：nil 进、nil 出、changed=false。
func TestRedactErrorTextPointerHandlesNil(t *testing.T) {
	value, changed := redactErrorTextPointer(nil)
	if value != nil || changed {
		t.Fatalf("nil 入参应原样返回 nil 且 changed=false，实际 (%v, %v)", value, changed)
	}

	message := "sk-proj-ABCDEFGHIJKLMNOPQRSTUVWX"
	redacted, changed := redactErrorTextPointer(&message)
	if !changed {
		t.Fatal("含密钥的文案应报告 changed=true")
	}
	if redacted == nil || strings.Contains(*redacted, "sk-proj-") {
		t.Fatalf("密钥应被收掉，实际 %v", redacted)
	}
}

// TestRedactErrorTextURLRuleKeepsPath 钉住 URL 规则**只吃 userinfo 段**、不吃路径。
//
// 这是「补一条超出 Node 的规则」最容易踩的坑：正则写宽一点就会把整个 URL 连路径一起吞掉，
// 而路径（`/v1/responses`）正是排查时最有用的信息。
func TestRedactErrorTextURLRuleKeepsPath(t *testing.T) {
	input := "POST https://user:pass@relay.example.com/v1/responses failed with 502"
	output, _ := redactErrorText(input)
	if strings.Contains(output, "user") || strings.Contains(output, "pass") {
		t.Errorf("凭据应被收掉，实际 %q", output)
	}
	if !strings.Contains(output, "/v1/responses") {
		t.Errorf("路径不得被吞掉，实际 %q", output)
	}
	if !strings.Contains(output, "relay.example.com") {
		t.Errorf("主机名应保留（排查需要知道打的是哪台），实际 %q", output)
	}
}
