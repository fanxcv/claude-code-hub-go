package dial

import (
	"net/http"
	"strings"
)

// ClientIP 是 resolveClientIp 的结果，语义对齐 forwarder.ts:9116。
//
// 候选收集顺序：x-forwarded-for 的各段（按顺序）、x-real-ip、x-client-ip、x-originating-ip、
// x-remote-ip、x-remote-addr。ClientIp 取首个非空候选；XForwardedFor 在有 XFF 时取各段拼接，
// 否则退化为 ClientIp。
type ClientIP struct {
	ClientIp      string
	XForwardedFor string
}

// ResolveClientIP 按 forwarder.ts 的规则解析客户端 IP。
func ResolveClientIP(header http.Header) ClientIP {
	forwarded := splitForwarded(header.Values("x-forwarded-for"))

	candidates := make([]string, 0, len(forwarded)+5)
	candidates = append(candidates, forwarded...)
	for _, name := range []string{
		"x-real-ip",
		"x-client-ip",
		"x-originating-ip",
		"x-remote-ip",
		"x-remote-addr",
	} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			candidates = append(candidates, value)
		}
	}

	resolved := ClientIP{}
	if len(candidates) > 0 {
		resolved.ClientIp = candidates[0]
	}
	if len(forwarded) > 0 {
		resolved.XForwardedFor = strings.Join(forwarded, ", ")
	} else {
		resolved.XForwardedFor = resolved.ClientIp
	}
	return resolved
}

// ApplyClientIPHeaders 按 provider.preserveClientIp 语义决定是否向上游注入 IP 头。
//
// 依据 forwarder.ts:9036：仅在 preserveClientIp 为真时写入；x-forwarded-for 取解析出的 XFF，
// x-real-ip 取首个候选 IP。
func ApplyClientIPHeaders(target http.Header, source IPHeaders) {
	if !source.PreserveClientIp {
		return
	}
	resolved := ResolveClientIP(source.Headers)
	if resolved.XForwardedFor != "" {
		target.Set("x-forwarded-for", resolved.XForwardedFor)
	}
	if resolved.ClientIp != "" {
		target.Set("x-real-ip", resolved.ClientIp)
	}
}

// splitForwarded 展开多值头里的逗号分隔段并去空白，丢弃空段。
func splitForwarded(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
	}
	return out
}
