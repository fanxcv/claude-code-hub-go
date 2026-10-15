package adminapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件把管理面审计用的客户端 IP 统一到**一条可配置提取链**上，语义逐条对齐 Node 的
// `@/lib/ip`（src/lib/ip/extract-client-ip.ts + src/types/ip-extraction.ts）。
//
// 为什么要统一：先前三处各写一份，且两份与 Node 的默认口径相抵——
//
//   - keys 侧取 X-Forwarded-For **首跳**；
//   - users 侧只取 RemoteAddr（连头都不看）；
//   - model-prices / sensitive-words / error-rules / request-filters 共享的那份取 XFF 首跳。
//
// 而 Node 管理面走的是 `getClientIp(c.req.raw.headers)`（src/lib/api/v1/_shared/request-context.ts:10、
// auth-middleware.ts:207），即**先查 system_settings.ip_extraction_config，未配置时用
// DEFAULT_IP_EXTRACTION_CONFIG**，其默认链是 `x-real-ip` → `x-forwarded-for` 的**最右**一段
// （types/ip-extraction.ts:31-33）。最左 XFF 是客户端可控的（前面没有会覆写该头的代理时，
// 攻击者可以伪造任意值），Node 刻意不信任它——管理面审计与登录锁定读的是同一个函数，
// 取错一段等于把「按 IP 锁定」变成可绕过的摆设。
//
// 为什么在这里复刻一遍而不是复用 internal/guard 的实现：guard 的那份（adapters_ip.go）确实已
// 逐条对齐同一默认值，但它的规则类型与解析函数都未导出，导出面只有需要 SettingsCache 的
// IPExtractorAdapter，而 SettingsCache 的构造器同样未导出——管理面拿不到实例。复刻的是不到
// 百行的纯函数，代价比重开 guard 的导出面小。**两处若有一处改了默认链，另一处必须同改**；
// 漂移由本文件的 TestAuditClientIP* 与 guard 的 TestExtractClientIPDefaultChain* 两侧分别钉住。
//
// 与 Node 的最后一处差别：链走完仍无有效 IP 时 Node 记 null（审计列可空），本函数返回空串，
// 由调用方原样落库——两者在库里都是 NULL。
//
// 采信转发头的前置条件（与 Node 的差别，也是「不可伪造」的唯一保证）：**直连 peer 必须可信**。
// 转发头是客户端填的，只有前面确实有会追加/覆写它的代理时才能当客户端地址；peer 地址在 TCP 层
// 由内核定，伪造不了。故 peer 不在可信清单里（或取不到）时，本函数不看任何头、直接回 peer——
// 默认「什么都不信」，跨机代理用 CCH_TRUSTED_PROXY_CIDRS 显式声明。否则换一个
// `x-real-ip: <随机值>` 就能把按 IP 计数的登录锁定变成摆设。

// trustedProxyEnvVar 是可信代理清单（Go 专有变量，不进 TS 契约）：逗号分隔的 CIDR 或裸地址，
// 如 `10.0.0.0/8,192.168.0.0/16,203.0.113.7`。回环恒信（本机反代是常态），不必写进来。
const trustedProxyEnvVar = "CCH_TRUSTED_PROXY_CIDRS"

// auditIPPickMode 是链内取值模式：最右 / 最左 / 指定下标。
type auditIPPickMode struct {
	mode  int
	index int
}

const (
	auditIPPickRightmost = iota
	auditIPPickLeftmost
	auditIPPickIndex
)

// auditIPHeaderRule 是一条头部规则。
type auditIPHeaderRule struct {
	name string
	pick auditIPPickMode
}

// defaultAuditIPChain 对齐 types/ip-extraction.ts 的 DEFAULT_IP_EXTRACTION_CONFIG：
// 先 x-real-ip，再 x-forwarded-for 的最右一段，两者都取最右（单值头的最右即其自身）。
//
// 刻意**不**包含 cf-connecting-ip：只有在运算符自己确认前面有 Cloudflare 或可信代理时，
// 才应在系统设置里显式加它。
func defaultAuditIPChain() []auditIPHeaderRule {
	return []auditIPHeaderRule{
		{name: "x-real-ip", pick: auditIPPickMode{mode: auditIPPickRightmost}},
		{name: "x-forwarded-for", pick: auditIPPickMode{mode: auditIPPickRightmost}},
	}
}

// auditClientIP 解析审计用的客户端 IP。
//
// pools 为 nil（未装配）或读设置失败时退回默认链——与 Node 在缓存冷启动时的行为一致
// （lib/ip/index.ts 的 getClientIp：`settings?.ipExtractionConfig ?? DEFAULT_IP_EXTRACTION_CONFIG`）。
// 这里每次直读 system_settings 单行：审计只发生在管理面写路径（人工操作，QPS 极低），
// 省掉一层与失效总线耦合的内存缓存更划算；代价是每次多一条单行 SELECT。
func auditClientIP(ctx context.Context, pools *store.Pools, request *http.Request) string {
	if request == nil {
		return ""
	}
	// 不可信的 peer：不采信任何转发头（详见文件头）。顺带的好处是这类请求的审计行不再为空——
	// 旧实现链路无值时记 NULL，现在至少记下真实对端。
	peer := normalizeAuditIP(request.RemoteAddr)
	if !isTrustedProxyPeer(peer) {
		return peer
	}
	rules := defaultAuditIPChain()
	if pools != nil {
		if settings, err := pools.FindSystemSettings(ctx); err == nil && settings != nil {
			if parsed, ok := parseAuditIPExtractionConfig(settings.IPExtractionConfig); ok {
				rules = parsed
			}
		}
	}
	return extractAuditIP(request.Header, rules)
}

// isTrustedProxyPeer 判直连 peer 是否可信：回环恒信，其余由 CCH_TRUSTED_PROXY_CIDRS 声明。
//
// peer 为空或不是合法 IP（取不到对端）一律按不可信处理：拿不准时不采信可伪造的头。
// 清单项解析失败只跳过该项（与 parseAuditIPExtractionConfig 对非法规则同一处置），
// 不静默放宽，也不把整条清单作废。
func isTrustedProxyPeer(peer string) bool {
	addr := net.ParseIP(peer)
	if addr == nil {
		return false
	}
	if addr.IsLoopback() {
		return true
	}
	for _, item := range strings.Split(os.Getenv(trustedProxyEnvVar), ",") {
		if network := parseTrustedProxyEntry(item); network != nil && network.Contains(addr) {
			return true
		}
	}
	return false
}

// parseTrustedProxyEntry 解一条清单项：CIDR 或裸地址（裸 v4 作 /32、裸 v6 作 /128）。
func parseTrustedProxyEntry(raw string) *net.IPNet {
	entry := strings.TrimSpace(raw)
	if entry == "" {
		return nil
	}
	if _, network, err := net.ParseCIDR(entry); err == nil {
		return network
	}
	addr := net.ParseIP(entry)
	if addr == nil {
		return nil
	}
	if v4 := addr.To4(); v4 != nil {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}
	}
	return &net.IPNet{IP: addr, Mask: net.CIDRMask(128, 128)}
}

// parseAuditIPExtractionConfig 解码 ip_extraction_config；形状非法时返回 false，由调用方退回默认链。
//
// 与 Node 一致的两处细节：① 显式空数组 `{"headers":[]}` 是「不信任任何头」（返回 true、空规则），
// 静默退回默认链等于忽略管理员的显式选择；② 单条规则非法（name 空或 pick 非法）只跳过该条，
// 不影响其余规则。
func parseAuditIPExtractionConfig(raw json.RawMessage) ([]auditIPHeaderRule, bool) {
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
	rules := make([]auditIPHeaderRule, 0, len(config.Headers))
	for _, header := range config.Headers {
		name := strings.TrimSpace(header.Name)
		if name == "" {
			continue
		}
		pick, ok := parseAuditIPPick(header.Pick)
		if !ok {
			continue
		}
		rules = append(rules, auditIPHeaderRule{name: name, pick: pick})
	}
	return rules, true
}

// parseAuditIPPick 解 XffPick：缺省与 "rightmost" 同义；对象只认 {"kind":"index","index":n}。
func parseAuditIPPick(raw json.RawMessage) (auditIPPickMode, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return auditIPPickMode{mode: auditIPPickRightmost}, true
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		// 大小写敏感，与 Node 的 switch 一致："Leftmost" 在 Node 里是非法值（整条规则跳过），
		// 这里放宽反而会让同一份配置在两边得到不同结果。
		switch mode {
		case "rightmost", "":
			return auditIPPickMode{mode: auditIPPickRightmost}, true
		case "leftmost":
			return auditIPPickMode{mode: auditIPPickLeftmost}, true
		default:
			return auditIPPickMode{}, false
		}
	}
	var spec struct {
		Kind  string `json:"kind"`
		Index int    `json:"index"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return auditIPPickMode{}, false
	}
	if spec.Kind != "index" {
		return auditIPPickMode{}, false
	}
	return auditIPPickMode{mode: auditIPPickIndex, index: spec.Index}, true
}

// extractAuditIP 逐条规则取值，第一条产出有效 IP 的规则胜出；链走完仍无值返回空串。
func extractAuditIP(header http.Header, rules []auditIPHeaderRule) string {
	if header == nil {
		return ""
	}
	for _, rule := range rules {
		raw := strings.TrimSpace(header.Get(rule.name))
		if raw == "" {
			continue
		}
		entries := auditIPSplitChain(raw)
		if len(entries) == 0 {
			continue
		}
		picked, ok := auditIPPickFromChain(entries, rule.pick)
		if !ok {
			continue
		}
		if ip := normalizeAuditIP(picked); ip != "" {
			return ip
		}
	}
	return ""
}

// auditIPSplitChain 按逗号切链并去空白与空项。
func auditIPSplitChain(raw string) []string {
	parts := strings.Split(raw, ",")
	entries := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			entries = append(entries, trimmed)
		}
	}
	return entries
}

// auditIPPickFromChain 按下标取链中一项；下标越界返回 false（与 Node 一致：跳过该规则）。
func auditIPPickFromChain(entries []string, pick auditIPPickMode) (string, bool) {
	switch pick.mode {
	case auditIPPickLeftmost:
		return entries[0], true
	case auditIPPickIndex:
		if pick.index < 0 || pick.index >= len(entries) {
			return "", false
		}
		return entries[pick.index], true
	default:
		return entries[len(entries)-1], true
	}
}

// normalizeAuditIP 把原始值规整成裸 IP（去端口、去 IPv6 方括号）；不是 IP 时返回空串。
//
// 对齐 extract-client-ip.ts 的 normalizeIp：带方括号的 IPv6 可带端口；无方括号时只有
// 「恰好一个冒号且冒号前是合法 IPv4」才剥端口（IPv6 本身就含冒号，不能按冒号切）。
func normalizeAuditIP(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
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
		if net.ParseIP(inner) == nil {
			return ""
		}
		return inner
	}
	if parts := strings.Split(trimmed, ":"); len(parts) == 2 && net.ParseIP(parts[0]) != nil {
		return parts[0]
	}
	if net.ParseIP(trimmed) == nil {
		return ""
	}
	return trimmed
}
