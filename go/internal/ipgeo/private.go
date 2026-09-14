package ipgeo

import (
	"net/netip"
	"strings"
)

// 本文件是 Node `src/lib/ip/private-ip.ts` 的逐条移植。
//
// 为什么不直接用 Go 的 `IsPrivate()`：两者的范围**不等价**。Node 这份把
// **CGN（100.64/10）**与 **0.0.0.0 / 255.255.255.255** 也算私有（因为有 Tailscale 之类的
// 私有覆盖网会真的出现在访问日志里），而 Go 的 `netip.Addr.IsPrivate()` 不含 CGN；
// 反向地，Go 会额外覆盖一些 Node 未列的范围。归属判定差一个网段，运维看到的
// 就是「同样的 IP，Node 说私有不查了、Go 却去查上游」——所以这里按 Node 的清单逐条写死。
//
// 与 Node 的第二处一致性要求：非法输入返回 false（Node 的注释原文："Invalid input returns false
// (callers decide what to do with 'unknown')"）。IPv4-mapped IPv6（::ffff:a.b.c.d）要按其内嵌的
// IPv4 判，且这一条对 Node 的 `isIP` 判为 v6 的那类输入同样成立。

// IsPrivate 复刻 Node 的 isPrivateIp。
func IsPrivate(raw string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if addr.Is4() {
		return isPrivateV4(addr)
	}
	// IPv4-mapped IPv6（::ffff:1.2.3.4）按内嵌 v4 判——Node 在 isPrivateIpv6 里显式解包。
	if addr.Is4In6() {
		return isPrivateV4(addr.Unmap())
	}
	return isPrivateV6(addr)
}

// IsValidIP 对应 Node 的 `isIP(ip) !== 0`：只判「是不是合法的 IPv4/IPv6 字面量」。
func IsValidIP(raw string) bool {
	_, err := netip.ParseAddr(strings.TrimSpace(raw))
	return err == nil
}

func isPrivateV4(addr netip.Addr) bool {
	// 逐条对应 Node 的 isPrivateIpv4 清单（顺序保持，便于与源码对照）。
	for _, prefix := range privateV4Prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	// Node 另有两个单点值（不在上面的前缀里，因为 /32 与 /8 的语义不同）。
	return addr == netip.MustParseAddr("0.0.0.0") || addr == netip.MustParseAddr("255.255.255.255")
}

// privateV4Prefixes 是 Node isPrivateIpv4 列出的网段，逐条同序。
var privateV4Prefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGN：Node 有、Go 的 IsPrivate() 没有
}

func isPrivateV6(addr netip.Addr) bool {
	if addr == netip.IPv6Loopback() || addr == netip.IPv6Unspecified() { // ::1 与 ::
		return true
	}
	// Node 的 `lower.startsWith("fe80:") || lower === "fe80::" || /^fe[89ab]/.test(lower)`
	// 三条合起来就是 fe80::/10（fe80..febf），与 netip 的 IsLinkLocalUnicast 同范围。
	if addr.IsLinkLocalUnicast() {
		return true
	}
	return ulaPrefix.Contains(addr) // fc00::/7（ULA）
}

var ulaPrefix = netip.MustParsePrefix("fc00::/7")
