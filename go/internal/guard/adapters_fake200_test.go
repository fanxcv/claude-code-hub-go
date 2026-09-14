package guard

import (
	"context"
	"regexp"
	"testing"
)

// 本文件钉住假 200 检测的判定边界：只认强信号、状态码尽量贴近语义、推断不出时回 502。

func TestFake200DetectorStrongSignals(t *testing.T) {
	detector := newFake200Detector()
	cases := []struct {
		name       string
		body       string
		wantOK     bool
		wantStatus int
	}{
		{
			name:       "HTML 错误页",
			body:       "<!DOCTYPE html><html><body>Cloudflare Error 1020</body></html>",
			wantOK:     true,
			wantStatus: 403,
		},
		{
			name:       "JSON 顶层 error 字符串",
			body:       `{"error":"当前无可用凭证"}`,
			wantOK:     true,
			wantStatus: 502,
		},
		{
			name:       "JSON error.message",
			body:       `{"error":{"type":"rate_limit_error","message":"Too many requests, please retry later"}}`,
			wantOK:     true,
			wantStatus: 429,
		},
		{
			name:       "OpenAI Responses failed",
			body:       `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"rate_limit_exceeded","message":"Rate limit reached"}}}`,
			wantOK:     true,
			wantStatus: 429,
		},
		{
			name:       "正常正文不得误判",
			body:       `{"id":"chatcmpl-1","choices":[{"message":{"content":"error handling in Go"}}]}`,
			wantOK:     false,
			wantStatus: 0,
		},
		{
			name:       "纯文本不得误判",
			body:       "too many requests later maybe",
			wantOK:     false,
			wantStatus: 0,
		},
		{
			name:       "JSON 数组不猜",
			body:       `[{"error":"x"}]`,
			wantOK:     false,
			wantStatus: 0,
		},
		{
			name:       "空 error 不算错误",
			body:       `{"error":"","id":"x"}`,
			wantOK:     false,
			wantStatus: 0,
		},
		{
			name:       "结构化状态码优先",
			body:       `{"error":{"status":503,"message":"upstream busy"}}`,
			wantOK:     true,
			wantStatus: 503,
		},
		{
			name:       "结构化 400 标记",
			body:       `{"error":{"type":"invalid_request_error","message":"bad input"}}`,
			wantOK:     true,
			wantStatus: 400,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, _, ok := detector.Detect("anthropic-messages", false, testCase.body)
			if ok != testCase.wantOK {
				t.Fatalf("命中判定 = %v，期望 %v（body=%.60s）", ok, testCase.wantOK, testCase.body)
			}
			if ok && status != testCase.wantStatus {
				t.Fatalf("推断状态码 = %d，期望 %d", status, testCase.wantStatus)
			}
		})
	}
}

func TestFake200DetectorSSEEvents(t *testing.T) {
	detector := newFake200Detector()
	sse := "event: message\ndata: {\"type\":\"message_start\"}\n\n" +
		"data: {\"error\":{\"message\":\"insufficient balance\"}}\n\n"
	status, _, ok := detector.Detect("anthropic-messages", true, sse)
	if !ok || status != 402 {
		t.Fatalf("SSE 内错误事件应被判定为 402，得到 ok=%v status=%d", ok, status)
	}

	healthy := "event: message\ndata: {\"type\":\"message_start\"}\n\ndata: [DONE]\n"
	if _, _, ok := detector.Detect("anthropic-messages", true, healthy); ok {
		t.Fatal("正常 SSE 不得判定为错误")
	}
}

func TestFake200DetectorEmptyBodyIsNotItsConcern(t *testing.T) {
	// 空正文由转发层的 EmptyResponse 分支判定，检测器不得在这里重复定义（否则会改变分类）。
	if _, _, ok := newFake200Detector().Detect("anthropic-messages", false, "   \n "); ok {
		t.Fatal("空正文不应由检测器判定")
	}
}

func TestFake200DetectorBOMStripped(t *testing.T) {
	body := "\uFEFF" + `{"error":"upstream unauthorized: invalid api key"}`
	status, _, ok := newFake200Detector().Detect("anthropic-messages", false, body)
	if !ok || status != 401 {
		t.Fatalf("带 BOM 的 JSON 应被识别为 401，得到 ok=%v status=%d", ok, status)
	}
}

// TestCompiledRuleMatchesSemantics 钉住三条判定语义：contains 走小写子串、
// exact 走「trim 后小写全等」、regex 大小写不敏感。
func TestCompiledRuleMatchesSemantics(t *testing.T) {
	snapshot := &compiledErrorRule{
		contains: []string{"context length exceeded"},
		exact:    map[string]struct{}{"invalid request": {}},
		regex:    []*regexp.Regexp{regexp.MustCompile(`(?i)model .* is not supported`)},
	}
	cases := []struct {
		content string
		want    bool
	}{
		{"Error: Context Length Exceeded for model x", true},
		{"INVALID REQUEST", true},
		{"invalid request body", false},
		{"MODEL foo IS NOT SUPPORTED", true},
		{"", false},
		{"   ", false},
		{"some other provider error", false},
	}
	for _, testCase := range cases {
		if got := snapshot.matches(testCase.content); got != testCase.want {
			t.Fatalf("matches(%q) = %v，期望 %v", testCase.content, got, testCase.want)
		}
	}
}

// TestErrorRuleCacheLoadsFromDatabase 用真实库验证装载：启用态规则进表，禁用态不进。
// 只建 scoped 数据（唯一 pattern + cch-guard-it- 前缀），跑完自清。
func TestErrorRuleCacheLoadsFromDatabase(t *testing.T) {
	pools := guardIntegrationPools(t)
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制道失败: %v", err)
	}
	// 两条规则的文案必须互不包含：contains 是子串判定，同前缀会让「禁用态不得命中」失败。
	const enabledPattern = "cch-guard-it-error-rule-enabled-token"
	if _, err := pool.Exec(ctx, `DELETE FROM error_rules WHERE pattern LIKE $1`, "cch-guard-it-%"); err != nil {
		t.Fatalf("清理历史残留失败: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO error_rules (pattern, match_type, category, is_enabled) VALUES ($1, 'contains', 'test', true)`,
		enabledPattern,
	); err != nil {
		t.Fatalf("插入测试规则失败: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO error_rules (pattern, match_type, category, is_enabled) VALUES ($1, 'contains', 'test', false)`,
		"cch-guard-it-error-rule-disabled-token",
	); err != nil {
		t.Fatalf("插入禁用规则失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM error_rules WHERE pattern LIKE $1`, "cch-guard-it-%")
	})

	cache := newErrorRuleCache(pools, nil, quietLogger())
	if !cache.Matches("upstream said: " + enabledPattern) {
		t.Fatal("启用态 contains 规则应命中")
	}
	if cache.Matches("upstream said: cch-guard-it-error-rule-disabled-token") {
		t.Fatal("禁用态规则不得命中")
	}
	if cache.Loads() != 1 {
		t.Fatalf("快照应只装载一次，得到 %d", cache.Loads())
	}
}

// TestFake200DetectorDetailTruncation 钉住文案截断（超长错误正文不得原样进日志）。
func TestFake200DetectorDetailTruncation(t *testing.T) {
	long := make([]byte, 0, 400)
	long = append(long, `{"error":"`...)
	for i := 0; i < 300; i++ {
		long = append(long, 'x')
	}
	long = append(long, '"', '}')
	_, message, ok := newFake200Detector().Detect("anthropic-messages", false, string(long))
	if !ok {
		t.Fatal("应命中")
	}
	if len([]rune(message)) != fake200DetailMaxLen+1 {
		t.Fatalf("文案应截断到 %d 个字符加省略号，得到 %d", fake200DetailMaxLen, len([]rune(message)))
	}
}
