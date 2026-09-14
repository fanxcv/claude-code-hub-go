package guard

import (
	"context"
	"encoding/json"
	"net"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件实现 IPExtractor 缝隙：按 system_settings.ip_extraction_config 的规则链解析客户端 IP。
//
// 为什么需要它（而不是用 ctx.ClientIP）：信任边界在运算符手里——哪个头可以信，取决于前面
// 是否真的有一层会覆写该头的代理。Node 侧的默认链刻意**不**信任 cf-connecting-ip 与最左
// XFF（那是客户端可控的），本文件逐条对齐同一默认值（见 defaultIPHeaderRules）。
//
// 配置读取走 SettingsCache（进程级快照，事件失效），故本适配器不自己缓存解码结果：
// 每个请求解一次不到 200 字节的 JSON，比再引入一层「解码缓存与快照不同步」的风险划算。

// ipPickMode 是 XFF 取值模式：leftmost / rightmost / 指定下标。
type ipPickMode struct {
	mode  int
	index int
}

const (
	ipPickRightmost = iota
	ipPickLeftmost
	ipPickIndex
)

// ipHeaderRule 是一条头部规则。
type ipHeaderRule struct {
	name string
	pick ipPickMode
}

// defaultIPHeaderRules 对齐 types/ip-extraction.ts 的 DEFAULT_IP_EXTRACTION_CONFIG。
func defaultIPHeaderRules() []ipHeaderRule {
	return []ipHeaderRule{
		{name: "x-real-ip", pick: ipPickMode{mode: ipPickRightmost}},
		{name: "x-forwarded-for", pick: ipPickMode{mode: ipPickRightmost}},
	}
}

// IPExtractorAdapter 实现 IPExtractor。
type IPExtractorAdapter struct {
	settings *SettingsCache
	logger   *logx.Logger
}

// newIPExtractor 构造适配器。settings 为 nil 时只用默认链（无信任覆盖）。
func newIPExtractor(settings *SettingsCache, logger *logx.Logger) *IPExtractorAdapter {
	if logger == nil {
		logger = defaultLogger
	}
	return &IPExtractorAdapter{settings: settings, logger: logger}
}

// ClientIP 按配置链解析；链走完仍无有效 IP 时返回空串（调用方退回上下文里的入口取值）。
func (a *IPExtractorAdapter) ClientIP(ctx context.Context, headers map[string][]string) (string, error) {
	rules := defaultIPHeaderRules()
	if a.settings != nil {
		settings, err := a.settings.FindSystemSettings(ctx)
		if err != nil {
			// 读不到设置不阻断请求：用保守默认链（与 Node 的 && config 缺省一致）。
			return extractClientIP(headers, rules), nil
		}
		if parsed, ok := parseIPExtractionConfig(settings.IPExtractionConfig); ok {
			rules = parsed
		}
	}
	return extractClientIP(headers, rules), nil
}

// parseIPExtractionConfig 解码配置；形状非法时返回 false，由调用方退回默认链。
//
// 空 headers 数组按「配置存在但无规则」处理（返回 true、空规则），与 Node 侧把显式空数组
// 当作「不信任任何头」的语义一致——静默退回默认链等于忽略管理员的显式选择。
func parseIPExtractionConfig(raw json.RawMessage) ([]ipHeaderRule, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var config struct {
		Headers []struct {
			Name string          `json:"name"`
			Pick json.RawMessage `json:"pick"`
		} `json:"headers"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, false
	}
	rules := make([]ipHeaderRule, 0, len(config.Headers))
	for _, header := range config.Headers {
		name := strings.TrimSpace(header.Name)
		if name == "" {
			continue
		}
		pick, ok := parseIPPick(header.Pick)
		if !ok {
			// 非法 pick 与 Node 侧一致：整条规则跳过，不改变其余规则。
			continue
		}
		rules = append(rules, ipHeaderRule{name: strings.ToLower(name), pick: pick})
	}
	return rules, true
}

// parseIPPick 解 XffPick：缺省为 rightmost，字符串取 leftmost/rightmost，对象取 {kind,index}。
func parseIPPick(raw json.RawMessage) (ipPickMode, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return ipPickMode{mode: ipPickRightmost}, true
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case "leftmost":
			return ipPickMode{mode: ipPickLeftmost}, true
		case "rightmost":
			return ipPickMode{mode: ipPickRightmost}, true
		default:
			return ipPickMode{}, false
		}
	}
	var spec struct {
		Kind  string `json:"kind"`
		Index int    `json:"index"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return ipPickMode{}, false
	}
	if spec.Kind != "index" {
		return ipPickMode{}, false
	}
	return ipPickMode{mode: ipPickIndex, index: spec.Index}, true
}

// extractClientIP 逐条规则取值，第一条产出有效 IP 的规则胜出。
func extractClientIP(headers map[string][]string, rules []ipHeaderRule) string {
	for _, rule := range rules {
		raw := readHeaderValue(headers, rule.name)
		if raw == "" {
			continue
		}
		entries := splitHeaderChain(raw)
		if len(entries) == 0 {
			continue
		}
		picked, ok := pickFromChain(entries, rule.pick)
		if !ok {
			continue
		}
		if ip := normalizeIP(picked); ip != "" {
			return ip
		}
	}
	return ""
}

// readHeaderValue 取头部首值。
//
// 大小写不敏感地扫一遍：入口把 http.Header 直接传进来时键是规范形（X-Real-Ip），但测试与
// 其它调用方可能传原始小写名，两种都要能命中。
func readHeaderValue(headers map[string][]string, name string) string {
	if len(headers) == 0 {
		return ""
	}
	if values, ok := headers[name]; ok && len(values) > 0 {
		return values[0]
	}
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		if len(values) > 0 {
			return values[0]
		}
		return ""
	}
	return ""
}

// splitHeaderChain 按逗号切链并去空白与空项。
func splitHeaderChain(raw string) []string {
	parts := strings.Split(raw, ",")
	entries := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			entries = append(entries, trimmed)
		}
	}
	return entries
}

// pickFromChain 按模式取链中一项；下标越界返回 false（与 Node 侧一致：跳过该规则）。
func pickFromChain(entries []string, pick ipPickMode) (string, bool) {
	switch pick.mode {
	case ipPickLeftmost:
		return entries[0], true
	case ipPickIndex:
		if pick.index < 0 || pick.index >= len(entries) {
			return "", false
		}
		return entries[pick.index], true
	default:
		return entries[len(entries)-1], true
	}
}

// normalizeIP 把原始值规整成裸 IP（去端口、去 IPv6 方括号）；不是 IP 时返回空串。
func normalizeIP(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	// 方括号 IPv6，可带端口："[::1]" / "[2001:db8::1]:443"
	if strings.HasPrefix(trimmed, "[") {
		end := strings.Index(trimmed, "]")
		if end < 0 {
			return ""
		}
		inner := trimmed[1:end]
		rest := trimmed[end+1:]
		if rest != "" && !strings.HasPrefix(rest, ":") {
			return ""
		}
		if net.ParseIP(inner) != nil {
			return inner
		}
		return ""
	}
	// 裸 IPv6 含多个冒号，绝不能按端口切；只有「恰好一个冒号且左侧是 IPv4」才去端口。
	if strings.Count(trimmed, ":") == 1 {
		host, _, _ := strings.Cut(trimmed, ":")
		if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
			return host
		}
	}
	if net.ParseIP(trimmed) != nil {
		return trimmed
	}
	return ""
}
