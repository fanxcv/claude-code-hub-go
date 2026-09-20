package guard

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件复刻 ProxySensitiveWordGuard 与 sensitive-word-detector 的检测语义。
//
// 检测顺序是契约的一部分：包含 → 精确 → 正则。顺序变化会改变「同一条文本命中哪个词」的
// 结果，而命中的词会写进拦截文案与审计字段。

// SensitiveMatchType 是匹配类型，取值与 Node 侧枚举逐字一致。
const (
	MatchContains = "contains"
	MatchExact    = "exact"
	MatchRegex    = "regex"
)

// sensitiveMatch 是一次命中结果。
type sensitiveMatch struct {
	Word        string
	MatchType   string
	MatchedText string
}

// sensitiveStep 复刻 ProxySensitiveWordGuard.ensure。
//
// 语义：词表为空即快速放行；任何检测期错误都 fail-open 放行（与 Node 一致）——敏感词是
// 内容治理而非访问控制，不该因为词表加载失败把正常流量全部打回。
func (d Deps) sensitiveStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		if d.Sensitive == nil {
			return nil, nil
		}

		words, err := d.Sensitive.SensitiveWords(d.runContext(ctx))
		if err != nil {
			d.logger().Error("guard.sensitive.source_failed", map[string]any{"error": err.Error()})
			return nil, nil
		}
		if len(words) == 0 {
			return nil, nil
		}

		body, err := d.body(ctx)
		if err != nil {
			// 无正文可检：放行（Node 侧提取不到文本时同样放行）。
			return nil, nil
		}
		texts := extractTextFromMessages(body)
		if len(texts) == 0 {
			return nil, nil
		}

		compiled := compileSensitiveWords(words, d)
		for _, text := range texts {
			match, matched := detectSensitive(text, compiled)
			if !matched {
				continue
			}

			auth, _ := authState(ctx)
			d.logger().Warn("guard.sensitive.blocked", map[string]any{
				"userId":      auth.UserID,
				"keyId":       auth.KeyID,
				"word":        match.Word,
				"matchType":   match.MatchType,
				"matchedText": match.MatchedText,
			})

			if d.BlockedLog != nil {
				// Node 侧是 void 异步写入，失败不影响拦截。Go 侧同步调用并忽略错误：
				// 拦截本身已是低频错误路径，为它派生 goroutine 只会换来一个无界并发面。
				reason, _ := json.Marshal(map[string]any{
					"word":        match.Word,
					"matchType":   match.MatchType,
					"matchedText": match.MatchedText,
				})
				record := BlockedRecord{
					KeyID:        auth.KeyID,
					UserID:       auth.UserID,
					APIKey:       auth.APIKey,
					Model:        d.requestedModel(ctx),
					StatusCode:   400,
					BlockedBy:    "sensitive_word",
					Reason:       reason,
					ErrorMessage: "请求包含敏感词：\"" + match.Word + "\"",
				}
				if err := d.BlockedLog.RecordBlocked(d.runContext(ctx), ctx, record); err != nil {
					d.logger().Error("guard.sensitive.log_failed", map[string]any{"error": err.Error()})
				}
			}

			return BuildError(400, buildSensitiveErrorMessage(match), ""), nil
		}

		return nil, nil
	}
}

// compiledSensitiveWords 是按匹配类型分好的词表。
//
// 正则按词条一次编译，随快照变化重编——Node 侧同样把 RegExp 缓存在过滤器对象上。
type compiledSensitiveWords struct {
	contains []string
	exact    map[string]bool
	regex    []compiledWord
}

type compiledWord struct {
	word    string
	pattern *regexp.Regexp
}

// compileSensitiveWords 编译词表；无法编译的正则跳过并记日志（与 Node 一致）。
func compileSensitiveWords(words []SensitiveWord, d Deps) compiledSensitiveWords {
	compiled := compiledSensitiveWords{exact: map[string]bool{}}
	for _, word := range words {
		lowered := strings.ToLower(word.Word)
		switch word.MatchType {
		case MatchContains:
			compiled.contains = append(compiled.contains, lowered)
		case MatchExact:
			compiled.exact[lowered] = true
		case MatchRegex:
			pattern, err := regexp.Compile("(?i)" + word.Word)
			if err != nil {
				d.logger().Error("guard.sensitive.invalid_regex", map[string]any{
					"word":  word.Word,
					"error": err.Error(),
				})
				continue
			}
			compiled.regex = append(compiled.regex, compiledWord{word: word.Word, pattern: pattern})
		default:
			d.logger().Warn("guard.sensitive.unknown_match_type", map[string]any{
				"matchType": word.MatchType,
			})
		}
	}
	return compiled
}

// detectSensitive 复刻 detect：包含 → 精确 → 正则。
func detectSensitive(text string, compiled compiledSensitiveWords) (sensitiveMatch, bool) {
	if text == "" {
		return sensitiveMatch{}, false
	}
	lowered := strings.ToLower(text)
	trimmed := strings.TrimSpace(lowered)

	for _, word := range compiled.contains {
		if strings.Contains(lowered, word) {
			return sensitiveMatch{
				Word:        word,
				MatchType:   MatchContains,
				MatchedText: extractMatchedText(text, word),
			}, true
		}
	}

	if compiled.exact[trimmed] {
		return sensitiveMatch{
			Word:        trimmed,
			MatchType:   MatchExact,
			MatchedText: strings.TrimSpace(text),
		}, true
	}

	for _, item := range compiled.regex {
		if found := item.pattern.FindString(text); found != "" {
			return sensitiveMatch{
				Word:        item.word,
				MatchType:   MatchRegex,
				MatchedText: found,
			}, true
		}
	}

	return sensitiveMatch{}, false
}

// extractMatchedText 复刻 extractMatchedText：命中词前后各 20 字符，前有截断时加省略号。
//
// 注意它是按「字符」截取的（Node 的 substring 以 UTF-16 码元为单位）。Go 按字节截取会切碎
// 多字节字符，故这里按 rune 截取：中文场景下等价，且不会产出半个汉字。
func extractMatchedText(text, word string) string {
	lowered := strings.ToLower(text)
	index := strings.Index(lowered, strings.ToLower(word))
	if index < 0 {
		return firstRunes(text, 50)
	}

	// 字节下标转 rune 下标：命中词之前的字节数即前文长度。
	prefixRunes := len([]rune(text[:index]))
	total := len([]rune(text))
	wordRunes := len([]rune(word))

	start := prefixRunes - 20
	if start < 0 {
		start = 0
	}
	end := prefixRunes + wordRunes + 20
	if end > total {
		end = total
	}

	runes := []rune(text)
	snippet := string(runes[start:end])
	if start > 0 {
		return "..." + snippet
	}
	return snippet
}

// firstRunes 取前 n 个字符。
func firstRunes(text string, count int) string {
	runes := []rune(text)
	if len(runes) <= count {
		return text
	}
	return string(runes[:count])
}

// buildSensitiveErrorMessage 复刻 buildErrorMessage。
func buildSensitiveErrorMessage(match sensitiveMatch) string {
	parts := []string{"请求包含敏感词：\"" + match.Word + "\""}
	if match.MatchedText != "" && match.MatchedText != match.Word {
		parts = append(parts, "匹配内容：\""+match.MatchedText+"\"")
	}
	if match.MatchType != "" {
		labels := map[string]string{
			MatchContains: "包含匹配",
			MatchExact:    "精确匹配",
			MatchRegex:    "正则匹配",
		}
		label := labels[match.MatchType]
		if label == "" {
			label = match.MatchType
		}
		parts = append(parts, "匹配类型："+label)
	}
	parts = append(parts, "请修改后重试。")
	return strings.Join(parts, "，")
}

// extractTextFromMessages 复刻 lib/message-extractor.ts 的 extractTextFromMessages。
//
// 覆盖 Claude（system/messages）、Response API（input）与图片接口的顶层 prompt。
func extractTextFromMessages(message map[string]any) []string {
	var texts []string

	if prompt, ok := message["prompt"].(string); ok {
		texts = append(texts, prompt)
	} else if prompt, ok := message["prompt"].([]any); ok {
		for _, item := range prompt {
			if text, ok := item.(string); ok {
				texts = append(texts, text)
			}
		}
	}

	if system, ok := message["system"]; ok {
		texts = append(texts, extractSystemText(system)...)
	}
	if messages, ok := message["messages"].([]any); ok {
		texts = append(texts, extractRoleText(messages)...)
	}
	if input, ok := message["input"].([]any); ok {
		texts = append(texts, extractRoleText(input)...)
	}

	filtered := make([]string, 0, len(texts))
	for _, text := range texts {
		if len(text) > 0 {
			filtered = append(filtered, text)
		}
	}
	return filtered
}

// extractSystemText 复刻 extractSystemText：system 可以是字符串或块数组。
func extractSystemText(system any) []string {
	switch value := system.(type) {
	case string:
		return []string{value}
	case []any:
		var texts []string
		for _, item := range value {
			if text := extractTextFromBlock(item); text != "" {
				texts = append(texts, text)
			}
		}
		return texts
	default:
		return nil
	}
}

// extractRoleText 复刻 extractMessagesText 与 extractInputText：只取 role=user 的消息。
func extractRoleText(messages []any) []string {
	var texts []string
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role != "user" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			texts = append(texts, content)
		case []any:
			for _, block := range content {
				if text := extractTextFromBlock(block); text != "" {
					texts = append(texts, text)
				}
			}
		}
	}
	return texts
}

// extractTextFromBlock 复刻 extractTextFromBlock：优先 text 字段，其次 content 字段。
func extractTextFromBlock(block any) string {
	if text, ok := block.(string); ok {
		return text
	}
	object, ok := block.(map[string]any)
	if !ok {
		return ""
	}
	if text, ok := object["text"].(string); ok {
		return text
	}
	if text, ok := object["content"].(string); ok {
		return text
	}
	return ""
}
