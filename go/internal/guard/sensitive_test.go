package guard

import (
	"errors"
	"strings"
	"testing"
)

// 敏感词守卫是内容治理路径：检测顺序、命中片段与文案都要钉死。

func TestDetectSensitiveOrder(t *testing.T) {
	compiled := compileSensitiveWords([]SensitiveWord{
		{Word: "badword", MatchType: MatchContains},
		{Word: "exactly", MatchType: MatchExact},
		{Word: `\d{3}-\d{4}`, MatchType: MatchRegex},
	}, Deps{})

	cases := []struct {
		name          string
		text          string
		wantMatched   bool
		wantWord      string
		wantMatchType string
	}{
		{name: "包含命中", text: "这是 BadWord 的一个片段", wantMatched: true, wantWord: "badword", wantMatchType: MatchContains},
		{name: "精确命中需要整串相等", text: "  exactly  ", wantMatched: true, wantMatchType: MatchExact},
		{name: "精确匹配不看子串", text: "not exactly here", wantMatched: false},
		{name: "正则命中", text: "电话 555-1234", wantMatched: true, wantMatchType: MatchRegex},
		{name: "无命中", text: "正常内容", wantMatched: false},
		{name: "空文本不命中", text: "", wantMatched: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			match, matched := detectSensitive(testCase.text, compiled)
			if matched != testCase.wantMatched {
				t.Fatalf("命中结果应为 %v，收到 %v", testCase.wantMatched, matched)
			}
			if !matched {
				return
			}
			if testCase.wantWord != "" && match.Word != testCase.wantWord {
				t.Fatalf("命中词应为 %q，收到 %q", testCase.wantWord, match.Word)
			}
			if testCase.wantMatchType != "" && match.MatchType != testCase.wantMatchType {
				t.Fatalf("匹配类型应为 %q，收到 %q", testCase.wantMatchType, match.MatchType)
			}
		})
	}
}

// 包含匹配的命中片段带上下文，且不得超过窗口。
func TestExtractMatchedText(t *testing.T) {
	long := strings.Repeat("A", 30) + "badword" + strings.Repeat("B", 30)
	snippet := extractMatchedText(long, "badword")
	if !strings.HasPrefix(snippet, "...") {
		t.Fatalf("前有截断时应加省略号: %q", snippet)
	}
	if !strings.Contains(snippet, "badword") {
		t.Fatalf("片段应包含命中词: %q", snippet)
	}
	if len([]rune(snippet)) > 20+7+20+3 {
		t.Fatalf("片段过长: %d 字符", len([]rune(snippet)))
	}

	// 未命中时降级为前 50 字符。
	fallback := extractMatchedText(strings.Repeat("C", 80), "missing")
	if len([]rune(fallback)) != 50 {
		t.Fatalf("降级片段应为 50 字符，收到 %d", len([]rune(fallback)))
	}
}

// 多字节字符不得被切碎（Go 按 rune 截取，与 Node 的 UTF-16 码元截取等价）。
func TestExtractMatchedTextKeepsRunesIntact(t *testing.T) {
	text := strings.Repeat("中", 30) + "敏感" + strings.Repeat("文", 30)
	snippet := extractMatchedText(text, "敏感")
	if !strings.Contains(snippet, "敏感") {
		t.Fatalf("片段应包含命中词: %q", snippet)
	}
	for _, character := range snippet {
		if character == '\uFFFD' {
			t.Fatalf("片段出现替换字符，说明按字节截断了多字节字符: %q", snippet)
		}
	}
}

// 拦截文案逐段对齐 Node。
func TestBuildSensitiveErrorMessage(t *testing.T) {
	message := buildSensitiveErrorMessage(sensitiveMatch{
		Word:        "badword",
		MatchType:   MatchContains,
		MatchedText: "a badword b",
	})
	for _, part := range []string{`请求包含敏感词："badword"`, `匹配内容："a badword b"`, "匹配类型：包含匹配", "请修改后重试。"} {
		if !strings.Contains(message, part) {
			t.Fatalf("文案缺少 %q：%s", part, message)
		}
	}

	// 命中内容与词相同时不重复输出匹配内容段。
	same := buildSensitiveErrorMessage(sensitiveMatch{Word: "w", MatchType: MatchExact, MatchedText: "w"})
	if strings.Contains(same, "匹配内容") {
		t.Fatalf("命中内容与词相同时不应输出匹配内容段: %s", same)
	}
	if !strings.Contains(same, "匹配类型：精确匹配") {
		t.Fatalf("应输出精确匹配标签: %s", same)
	}
	if !strings.Contains(buildSensitiveErrorMessage(sensitiveMatch{Word: "w", MatchType: MatchRegex}), "正则匹配") {
		t.Fatal("正则命中应输出正则匹配标签")
	}
}

// 词表为空时快速放行。
func TestSensitiveStepEmptyWordList(t *testing.T) {
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "badword"}}}
	factory, _ := bodyFactory(t, body)
	deps := Deps{Sensitive: fakeSensitive{}, Body: factory}
	ctx := newContext(t, nil, body)

	response, err := deps.sensitiveStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("空词表应放行，收到 %d", response.Status)
	}
}

// 命中即拦截，并记录被拦截请求。
func TestSensitiveStepBlocksAndRecords(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-4",
		"messages": []any{map[string]any{
			"role":    "user",
			"content": "请帮我写 badword 的说明",
		}},
	}
	factory, _ := bodyFactory(t, body)
	recorder := &fakeBlockedLog{}
	deps := Deps{
		Sensitive:  fakeSensitive{words: []SensitiveWord{{Word: "badword", MatchType: MatchContains}}},
		BlockedLog: recorder,
		Body:       factory,
	}
	ctx := newContext(t, nil, body)
	withAuth(ctx, 3, 7, "sk-x")

	response, err := deps.sensitiveStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response == nil || response.Status != 400 {
		t.Fatalf("应拦截并返回 400，收到 %v", response)
	}
	if got := errorField(t, response, "type"); got != "invalid_request_error" {
		t.Fatalf("type 应为 invalid_request_error，收到 %s", got)
	}
	if !strings.Contains(errorField(t, response, "message"), "badword") {
		t.Fatalf("文案应含命中词：%s", string(response.Body))
	}
	if len(recorder.records) != 1 {
		t.Fatalf("应记录一条拦截，收到 %d", len(recorder.records))
	}
	record := recorder.records[0]
	if record.BlockedBy != "sensitive_word" || record.StatusCode != 400 {
		t.Fatalf("拦截记录字段不符: %+v", record)
	}
	if record.KeyID != 3 || record.UserID != 7 {
		t.Fatalf("拦截记录应带身份: %+v", record)
	}
	if !strings.Contains(string(record.Reason), "badword") {
		t.Fatalf("拦截原因应含命中词: %s", string(record.Reason))
	}
	if record.Model != "claude-sonnet-4" {
		t.Fatalf("拦截记录应带模型名，收到 %q", record.Model)
	}
}

// system 字段与 Response API 的 input 也在检测范围内；非法词表项只记日志。
func TestSensitiveStepScansSystemAndInput(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{
			name: "system 字符串",
			body: map[string]any{"system": "badword 出现在系统提示", "messages": []any{}},
		},
		{
			name: "system 块数组",
			body: map[string]any{"system": []any{map[string]any{"type": "text", "text": "badword"}}, "messages": []any{}},
		},
		{
			name: "Response API 的 input",
			body: map[string]any{"input": []any{map[string]any{"role": "user", "content": "badword"}}},
		},
		{
			name: "图片接口的 prompt",
			body: map[string]any{"prompt": "badword"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			factory, _ := bodyFactory(t, testCase.body)
			deps := Deps{
				Sensitive: fakeSensitive{words: []SensitiveWord{
					{Word: "(?<=a)b", MatchType: MatchRegex}, // RE2 不支持：按不匹配处理
					{Word: "badword", MatchType: MatchContains},
				}},
				Body: factory,
			}
			ctx := newContext(t, nil, testCase.body)
			response, err := deps.sensitiveStep()(ctx)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if response == nil {
				t.Fatalf("应命中并拦截，正文为 %v", testCase.body)
			}
		})
	}
}

// 助手消息与空文本不参与检测。
func TestSensitiveStepIgnoresAssistantMessages(t *testing.T) {
	body := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": "badword"}}}
	factory, _ := bodyFactory(t, body)
	deps := Deps{
		Sensitive: fakeSensitive{words: []SensitiveWord{{Word: "badword", MatchType: MatchContains}}},
		Body:      factory,
	}
	ctx := newContext(t, nil, body)

	response, err := deps.sensitiveStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("助手消息不应触发拦截，收到 %d", response.Status)
	}
}

// 词表读取失败 fail-open 放行：内容治理不该因为配置不可达把流量全打回。
func TestSensitiveStepFailOpenOnSourceError(t *testing.T) {
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "badword"}}}
	factory, _ := bodyFactory(t, body)
	deps := Deps{Sensitive: fakeSensitive{err: errors.New("敏感词表不可达")}, Body: factory}
	ctx := newContext(t, nil, body)

	response, err := deps.sensitiveStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("词表失败时应放行，收到 %d", response.Status)
	}
}

// 缝隙缺失时不检测（过渡期的显式缺口）。
func TestSensitiveStepWithoutSeam(t *testing.T) {
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "badword"}}}
	factory, _ := bodyFactory(t, body)
	ctx := newContext(t, nil, body)

	response, err := Deps{Body: factory}.sensitiveStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("缝隙缺失时应跳过，收到 %d", response.Status)
	}
}

// 文本提取器覆盖四种消息形态。
func TestExtractTextFromMessages(t *testing.T) {
	texts := extractTextFromMessages(map[string]any{
		"prompt": []any{"p1", "p2"},
		"system": "sys",
		"messages": []any{
			map[string]any{"role": "user", "content": "m1"},
			map[string]any{"role": "assistant", "content": "ignored"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "m2"},
				map[string]any{"content": "m3"},
				"m4",
			}},
		},
		"input": []any{
			map[string]any{"role": "user", "content": "i1"},
		},
	})

	want := []string{"p1", "p2", "sys", "m1", "m2", "m3", "m4", "i1"}
	if len(texts) != len(want) {
		t.Fatalf("应提取 %d 段文本，收到 %d (%v)", len(want), len(texts), texts)
	}
	for index, expected := range want {
		if texts[index] != expected {
			t.Fatalf("第 %d 段应为 %q，收到 %q", index, expected, texts[index])
		}
	}
}

// 空文本被过滤掉，避免无意义的检测调用。
func TestExtractTextFromMessagesDropsEmpty(t *testing.T) {
	texts := extractTextFromMessages(map[string]any{
		"system":   []any{map[string]any{"text": ""}},
		"messages": []any{map[string]any{"role": "user", "content": ""}},
	})
	if len(texts) != 0 {
		t.Fatalf("空文本应被过滤，收到 %v", texts)
	}
}
