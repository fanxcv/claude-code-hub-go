package adminapi

import (
	"regexp"
	"sync"
)

// 本文件是**错误文案的值形态脱敏**：把上游错误里可能夹带的凭据/身份信息改写成占位符。
//
// 为什么需要它（与既有脱敏的分工）：
//   - `audit.go` 的 `redactSensitive` 走的是**键名**（Key 命中敏感名单即整个值替换）——它只对
//     结构化对象有意义；
//   - 而错误文案是**自由文本**，凭据以 `https://user:pass@host`、`sk-…`、`Bearer …` 这类
//     **形态**出现，键名表对它完全无效。
//
// 口径来源（不是自造）：逐条移植 Node 的 `sanitizeErrorTextForDetail`
// （`src/lib/utils/upstream-error-detection.ts:373-403`）。**为什么必须移植而不是另写一套**：
// 同一条上游错误在 Node 界面与 Go 界面上必须被改写成同一形状，否则「同一个错误两边显示不同」
// 会被当成新缺陷追查。替换文案也逐字保留（`[REDACTED]` / `[REDACTED_KEY]` / `[JWT]` /
// `[EMAIL]` / `[PATH]`）。
//
// **有意超出 Node 口径的一条**（已登记，见报告 §6）：URL 里的内嵌凭据
// （`scheme://user:pass@host`）。理由：Node 的规则集里没有这一条，它只能靠邮箱规则**偶然**收掉
// `pass@host.tld`——而（a）用户名会留下，（b）主机是**裸 IP 或带端口**时（`https://u:p@10.0.0.1:8080`）
// 邮箱规则根本不匹配（`\.[A-Za-z]{2,}` 要求字母 TLD），于是**用户名与密码全段明文落到界面上**。
// 而带内嵌凭据的上游 URL 正是本网关的常见配置形态（中转/内网 relay），故这一条必须补。
//
// 与 Node 同的**上限**（其源码注释即写明）：目的不是「完美脱敏」，而是降低意外夹带的风险。
// 已知覆盖不到：非常规密钥前缀、被截断的 token 片段、非 ASCII 编码的凭据、以及任何不在下面
// 八条规则内的形态。

var (
	// errorTextRedactOnce 保证正则只编译一次（端点可能一次返回上百条错误文案）。
	errorTextRedactOnce sync.Once
	errorTextRedact     []errorTextRedactRule
)

// errorTextRedactRule 是一条值形态改写规则。
type errorTextRedactRule struct {
	pattern     *regexp.Regexp
	replacement string
}

// RedactedErrorTokens 是脱敏后可能出现的占位符集合。
//
// 导出给测试与调用方共用：判定「这条文案是否已被改写」时不必自己再列一遍字面量。
var RedactedErrorTokens = []string{"[REDACTED]", "[REDACTED_KEY]", "[JWT]", "[EMAIL]", "[PATH]"}

// errorTextRedactRules 按 Node 的**应用顺序**返回规则。
//
// 顺序有语义：先收掉 `Bearer <token>` 整段，再收 `sk-` 前缀密钥——否则 `Bearer sk-…` 会被
// 后一条先改掉半截，得到的占位符与 Node 不同。
func errorTextRedactRules() []errorTextRedactRule {
	errorTextRedactOnce.Do(func() {
		errorTextRedact = []errorTextRedactRule{
			// 0) **URL 内嵌凭据**（有意超出 Node：见文件头注释）。
			//    放在最前：它是「最具体」的一条，先收掉整段 `scheme://user:pass@`，
			//    后续的密钥/邮箱规则就不必去啃半截 URL。
			//    `[^/\s:@]+:[^/\s@]+@` 只吃「userinfo 段」：遇到第一个 `/` 即停，不会吞掉路径。
			{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}[REDACTED]@"},
			// 1) Bearer token（Node：/Bearer\s+[A-Za-z0-9._-]+/gi）
			{regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._-]+`), "Bearer [REDACTED]"},
			// 2) 常见 API key 前缀（Node：/\b(?:sk|rk|pk)-[A-Za-z0-9_-]{16,}\b/giu）
			{regexp.MustCompile(`(?i)\b(?:sk|rk|pk)-[A-Za-z0-9_-]{16,}\b`), "[REDACTED_KEY]"},
			// 3) Google API key（Node：/\bAIza[0-9A-Za-z_-]{16,}\b/g）
			{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{16,}\b`), "[REDACTED_KEY]"},
			// 4) JWT（Node：base64url 三段，首段 eyJ 开头）
			{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`), "[JWT]"},
			// 5) 邮箱（Node：/\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b/g）
			{regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`), "[EMAIL]"},
			// 6) 通用敏感键值（Node：/\b(password|token|secret|api[_-]?key)\b\s*[:=]\s*['"]?[^'"\s]+['"]?/gi
			//    → `$1:***`）。Go 的 ReplaceAllString 支持 `$1` 组引用，故替换串与 Node 逐字一致。
			{regexp.MustCompile(`(?i)\b(password|token|secret|api[_-]?key)\b\s*[:=]\s*['"]?[^'"\s]+['"]?`), "$1:***"},
			// 7) 配置文件路径（Node：/\/[\w.-]+\.(?:env|ya?ml|json|conf|ini)/gi → [PATH]）
			{regexp.MustCompile(`(?i)/[\w.-]+\.(?:env|ya?ml|json|conf|ini)`), "[PATH]"},
		}
	})
	return errorTextRedact
}

// redactErrorText 按 Node 口径改写一段错误文案，并报告**是否发生了改写**。
//
// 第二个返回值不是装饰：界面上看到 `[REDACTED_KEY]` 的运维需要知道「这是被改写过的文案，
// 不是上游原话」——否则会拿着改写后的串去搜上游日志而找不到。故响应里每一条错误都带
// `redacted` 布尔，就是从这里来的。
func redactErrorText(text string) (string, bool) {
	if text == "" {
		return text, false
	}
	redacted := text
	for _, rule := range errorTextRedactRules() {
		redacted = rule.pattern.ReplaceAllString(redacted, rule.replacement)
	}
	return redacted, redacted != text
}

// redactErrorTextPointer 是 redactErrorText 对可空字段的包装：nil 进、nil 出。
//
// 返回的布尔是「整行是否发生过脱敏」，供调用方在行级聚合出一个 `redacted` 标记。
func redactErrorTextPointer(text *string) (*string, bool) {
	if text == nil {
		return nil, false
	}
	redacted, changed := redactErrorText(*text)
	return &redacted, changed
}
