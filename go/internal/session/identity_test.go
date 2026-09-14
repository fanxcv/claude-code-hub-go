package session

import (
	"regexp"
	"strings"
	"testing"
)

// TestParseClaudeMetadataUserID 覆盖 JSON 优先、旧版正则兜底与两者皆不中。
func TestParseClaudeMetadataUserID(t *testing.T) {
	cases := []struct {
		name        string
		input       any
		wantSession string
		wantFormat  string
		wantDevice  string
	}{
		{"JSON 格式", `{"session_id":"sess_json","device_id":"dev-1","account_uuid":"acc-1"}`,
			"sess_json", "json", "dev-1"},
		{"JSON 缺 device", `{"session_id":"sess_json2"}`, "sess_json2", "json", ""},
		{"旧版格式", "user_dev-9_account__session_sess_legacy", "sess_legacy", "legacy", "dev-9"},
		{"JSON 无 session_id 回落旧版", `{"device_id":"d"}`, "", "", ""},
		{"空串", "", "", "", ""},
		{"仅空白", "   ", "", "", ""},
		{"非字符串", 42, "", "", ""},
		{"nil", nil, "", "", ""},
		{"不是任何已知格式", "user_abc", "", "", ""},
		{"JSON session_id 为空白", `{"session_id":"   "}`, "", "", ""},
		{"JSON 内嵌旧版形制但不含 session", `{"foo":"user_x_account__session_y"}`, "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseClaudeMetadataUserID(tc.input)
			if got.SessionID != tc.wantSession || got.Format != tc.wantFormat || got.DeviceID != tc.wantDevice {
				t.Fatalf("解析不符\n got=%+v\nwant session=%q format=%q device=%q",
					got, tc.wantSession, tc.wantFormat, tc.wantDevice)
			}
		})
	}
}

// TestNormalizeCodexSessionID 覆盖长度与字符集双检。
func TestNormalizeCodexSessionID(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  string
	}{
		{"合法 UUID", "a1b2c3d4-e5f6-7890-abcd-ef1234567890", "a1b2c3d4-e5f6-7890-abcd-ef1234567890"},
		{"合法且带空白", "  codex_session_1234567890  ", "codex_session_1234567890"},
		{"含冒号点与下划线", "abc.def:ghi_jkl-1234567890mno", "abc.def:ghi_jkl-1234567890mno"},
		{"太短", "short", ""},
		{"恰好 21 字符", strings.Repeat("a", 21), strings.Repeat("a", 21)},
		{"恰好 256 字符", strings.Repeat("a", 256), strings.Repeat("a", 256)},
		{"超长 257 字符", strings.Repeat("a", 257), ""},
		{"非法字符", "bad id with spaces 1234567890", ""},
		{"非字符串", 12345, ""},
		{"空串", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeCodexSessionID(tc.input); got != tc.want {
				t.Fatalf("归一不符: got=%q want=%q", got, tc.want)
			}
		})
	}
}

// TestExtractCodexSessionIDPriority 钉住 Codex 会话 id 的优先级链。
func TestExtractCodexSessionIDPriority(t *testing.T) {
	valid := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	other := "b2c3d4e5-f6a7-8901-bcde-f12345678901"

	cases := []struct {
		name    string
		headers map[string][]string
		body    map[string]any
		want    string
	}{
		{"session_id 头最优先",
			map[string][]string{"session_id": {valid}, "x-session-id": {other}},
			map[string]any{"prompt_cache_key": other, "previous_response_id": other}, valid},
		{"其次 x-session-id 头",
			map[string][]string{"x-session-id": {other}},
			map[string]any{"prompt_cache_key": valid}, other},
		{"其次 prompt_cache_key", nil, map[string]any{"prompt_cache_key": valid}, valid},
		{"其次 metadata.session_id", nil,
			map[string]any{"metadata": map[string]any{"session_id": valid}}, valid},
		{"最后 previous_response_id 带前缀", nil,
			map[string]any{"previous_response_id": valid}, "codex_prev_" + valid},
		{"头非法则继续下探", map[string][]string{"session_id": {"short"}},
			map[string]any{"prompt_cache_key": valid}, valid},
		{"全部非法返回空", map[string][]string{"session_id": {"short"}},
			map[string]any{"prompt_cache_key": "also short"}, ""},
		{"previous_response_id 加前缀后超长则放弃", nil,
			map[string]any{"previous_response_id": strings.Repeat("a", 250)}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractCodexSessionID(tc.headers, tc.body); got != tc.want {
				t.Fatalf("提取不符: got=%q want=%q", got, tc.want)
			}
		})
	}
}

// TestHeaderValueTakesFirst 钉住 Node 的 headers.get 语义：多值取第一个。
func TestHeaderValueTakesFirst(t *testing.T) {
	headers := map[string][]string{"session_id": {"first-session-id-1234", "second-session-id-5678"}}
	if got := headerValue(headers, "session_id"); got != "first-session-id-1234" {
		t.Fatalf("多值头应取第一个: got=%q", got)
	}
	if got := headerValue(headers, "missing"); got != "" {
		t.Fatalf("缺失头应返回空: got=%q", got)
	}
	if got := headerValue(nil, "session_id"); got != "" {
		t.Fatalf("nil 头表应返回空: got=%q", got)
	}
}

// TestExtractClientSessionID 覆盖 Claude 与 Codex 两条入口路径。
func TestExtractClientSessionID(t *testing.T) {
	codexSession := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	cases := []struct {
		name    string
		body    map[string]any
		headers map[string][]string
		want    string
	}{
		{"Claude metadata.user_id JSON", map[string]any{
			"metadata": map[string]any{"user_id": `{"session_id":"sess_json"}`},
		}, nil, "sess_json"},
		{"Claude metadata.user_id 旧版", map[string]any{
			"metadata": map[string]any{"user_id": "user_dev_account__session_sess_legacy"},
		}, nil, "sess_legacy"},
		{"回落到 metadata.session_id", map[string]any{
			"metadata": map[string]any{"session_id": "sess_meta"},
		}, nil, "sess_meta"},
		{"user_id 非法则回落 session_id", map[string]any{
			"metadata": map[string]any{"user_id": "nonsense", "session_id": "sess_meta2"},
		}, nil, "sess_meta2"},
		{"Codex 走 body.input 数组", map[string]any{
			"input":    []any{map[string]any{"role": "user"}},
			"metadata": map[string]any{"user_id": `{"session_id":"sess_json"}`},
		}, map[string][]string{"session_id": {codexSession}}, codexSession},
		{"Codex 但无可识别会话", map[string]any{
			"input": []any{map[string]any{"role": "user"}},
		}, nil, ""},
		{"无 metadata", map[string]any{"model": "claude"}, nil, ""},
		{"metadata 非对象", map[string]any{"metadata": "oops"}, nil, ""},
		{"nil body", nil, nil, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractClientSessionID(tc.body, tc.headers); got != tc.want {
				t.Fatalf("提取不符: got=%q want=%q", got, tc.want)
			}
		})
	}
}

// codexInputBody 造一个「Codex 路径」判定所需的正文（顶层 input 是数组）。
func codexInputBody() map[string]any {
	return map[string]any{"input": []any{map[string]any{"type": "message"}}}
}

// TestExtractClientSessionIDCodexPathIsReached 单独钉住「顶层 input 为数组 ⇒ 走 Codex 提取」
// 这条分支：Go 的 []any 类型断言与 Node 的 Array.isArray 语义必须一致，否则 prompt_cache_key
// 这类 Codex 专有来源会被整条跳过。
func TestExtractClientSessionIDCodexPathIsReached(t *testing.T) {
	body := codexInputBody()
	body["prompt_cache_key"] = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	if got := ExtractClientSessionID(body, nil); got != "a1b2c3d4-e5f6-7890-abcd-ef1234567890" {
		t.Fatalf("Codex 路径未生效: got=%q", got)
	}

	// input 不是数组（Claude 请求）时不得走 Codex 路径。
	claudeBody := map[string]any{
		"input":            "not-an-array",
		"metadata":         map[string]any{"session_id": "sess_claude"},
		"prompt_cache_key": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
	}
	if got := ExtractClientSessionID(claudeBody, nil); got != "sess_claude" {
		t.Fatalf("Claude 路径被 Codex 分支劫持: got=%q", got)
	}
}

// TestCalculateMessagesHash 覆盖降级哈希的三条规则：只取前 3 条、文本拼接、无内容返回空。
func TestCalculateMessagesHash(t *testing.T) {
	// 期望值硬编码：哈希参与会话复用判定，切换期两侧必须得到同一摘要。
	textOnly := CalculateMessagesHash([]any{
		map[string]any{"role": "user", "content": "hello"},
	})
	if textOnly != "2cf24dba5fb0a30e" {
		t.Fatalf("单条字符串内容哈希不符: got=%q", textOnly)
	}

	// 块数组按 type=text 拼接，块之间不插分隔符。
	blocks := CalculateMessagesHash([]any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "image", "source": "x"},
		}},
	})
	if blocks != textOnly {
		t.Fatalf("块数组应得到与纯文本相同的摘要: got=%q want=%q", blocks, textOnly)
	}

	// 多条之间用 "|" 连接，且只取前 3 条（第 4 条即便有内容也不参与）。
	head := CalculateMessagesHash([]any{
		map[string]any{"content": "a"},
		map[string]any{"content": "b"},
		map[string]any{"content": "c"},
	})
	tailIncluded := CalculateMessagesHash([]any{
		map[string]any{"content": "a"},
		map[string]any{"content": "b"},
		map[string]any{"content": "c"},
		map[string]any{"content": "d"},
	})
	if head != tailIncluded {
		t.Fatalf("第 4 条不应参与哈希: head=%q tail=%q", head, tailIncluded)
	}
	if head != "a52dd81bfd5e4e66" {
		t.Fatalf("三条内容摘要不符: got=%q", head)
	}

	empty := []struct {
		name  string
		input any
	}{
		{"nil", nil},
		{"非数组", "hello"},
		{"空数组", []any{}},
		{"全空内容", []any{map[string]any{"content": ""}}},
		{"非对象消息", []any{"oops"}},
		{"块无文本", []any{map[string]any{"content": []any{map[string]any{"type": "image"}}}}},
		{"块非对象", []any{map[string]any{"content": []any{"oops"}}}},
	}
	for _, tc := range empty {
		t.Run("空哈希/"+tc.name, func(t *testing.T) {
			if got := CalculateMessagesHash(tc.input); got != "" {
				t.Fatalf("应返回空哈希: got=%q", got)
			}
		})
	}
}

// TestGenerateSessionIDAndToken 钉住两个随机标识的形制。
func TestGenerateSessionIDAndToken(t *testing.T) {
	sessionIDPattern := regexp.MustCompile(`^sess_[0-9a-z]+_[0-9a-f]{12}$`)
	tokenPattern := regexp.MustCompile(`^[0-9a-f]{32}$`)

	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		sessionID := GenerateSessionID()
		if !sessionIDPattern.MatchString(sessionID) {
			t.Fatalf("会话 id 形制不符: %q", sessionID)
		}
		if seen[sessionID] {
			t.Fatalf("会话 id 重复: %q", sessionID)
		}
		seen[sessionID] = true

		if token := GenerateToken(); !tokenPattern.MatchString(token) {
			t.Fatalf("租约 token 形制不符: %q", token)
		}
	}
}

// TestParseMetadataBody 覆盖正文顶层 metadata 的取值。
func TestParseMetadataBody(t *testing.T) {
	if got := parseMetadataBody(map[string]any{"metadata": map[string]any{"a": 1}}); got == nil {
		t.Fatal("应取到 metadata 对象")
	}
	for _, body := range []map[string]any{
		nil,
		{"metadata": "oops"},
		{"metadata": nil},
		{"model": "claude"},
	} {
		if got := parseMetadataBody(body); got != nil {
			t.Fatalf("不应取到 metadata: %+v", got)
		}
	}
}
