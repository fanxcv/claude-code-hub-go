package providertest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// ErrUnsupportedProxyScheme 表示代理协议本包不支持。
//
// 与 Node 的差异（登记）：Node 经 undici 的 socks-proxy-agent 支持 socks4://，本包只做
// http/https（原生 CONNECT）与 socks5（x/net/proxy）；socks4 显式失败而不是静默直连——
// 静默直连会让「测试连通性」结论失真。
var ErrUnsupportedProxyScheme = errors.New("providertest: 不支持的代理协议")

// ProxyConfig 复刻 Node 的 provider 级代理三件套（`src/lib/proxy-agent.ts`）。
type ProxyConfig struct {
	// URL 为空表示不使用代理。
	URL string
	// FallbackToDirect 为真时，代理建连失败允许回退直连（Node 的 proxyFallbackToDirect）。
	FallbackToDirect bool
}

// NewUpstreamClient 构造一个用于「供应商连通性/协议探测」的 HTTP 客户端。
//
// 与 `internal/dial` 的分工：`internal/dial` 是**数据面**拨号器，它显式拒绝代理
// （`ErrUnsupportedUpstreamTransport`）；供应商探测面必须支持代理（Node 行为如此），
// 故这里单独建一个小客户端，只承担探测用途。
func NewUpstreamClient(config ProxyConfig, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		DisableCompression:    true,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     false,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	proxyURL := strings.TrimSpace(config.URL)
	if proxyURL == "" {
		return &http.Client{Transport: transport, Timeout: timeout}, nil
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("%w: 地址无法解析", ErrUnsupportedProxyScheme)
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		// http.Transport 原生支持 http(s) 代理（对 https 目标走 CONNECT），无需自造隧道。
		transport.Proxy = http.ProxyURL(parsed)
	case "socks5", "socks5h":
		dialer, err := socks5Dialer(parsed)
		if err != nil {
			return nil, err
		}
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			type contextDialer interface {
				DialContext(ctx context.Context, network, address string) (net.Conn, error)
			}
			if ctxDialer, ok := dialer.(contextDialer); ok {
				return ctxDialer.DialContext(ctx, network, address)
			}
			return dialer.Dial(network, address)
		}
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProxyScheme, parsed.Scheme)
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

// socks5Dialer 按 Node 的地址语义（user:pass@host:port）构造 SOCKS5 拨号器。
func socks5Dialer(parsed *url.URL) (xproxy.Dialer, error) {
	host := parsed.Host
	if !strings.Contains(host, ":") {
		host = net.JoinHostPort(host, "1080")
	}
	var auth *xproxy.Auth
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		auth = &xproxy.Auth{User: parsed.User.Username(), Password: password}
	}
	return xproxy.SOCKS5("tcp", host, auth, &net.Dialer{Timeout: 10 * time.Second})
}

// IsValidProxyURL 复刻 Node 的 isValidProxyUrl（`src/lib/proxy-agent.ts:219`）：
// 只认 http/https/socks4/socks5 且必须有 host。
func IsValidProxyURL(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks4", "socks5", "socks5h":
		return parsed.Host != ""
	default:
		return false
	}
}

// ValidateProviderURLForConnectivity 复刻 Node 的 validateProviderUrlForConnectivity
// （`src/lib/validation/provider-url.ts:14`）：仅做基础格式校验，不限制内网地址与端口。
//
// 返回 (normalizedURL, 错误消息)。Node 的错误消息固定为「供应商地址格式无效」。
func ValidateProviderURLForConnectivity(raw string) (string, string, bool) {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", "供应商地址格式无效", false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return trimmed, "", true
	default:
		return "", "供应商地址格式无效", false
	}
}
