package providertest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ApiTestTimeoutLimits 复刻 actions/providers.ts:144-148 的 API_TEST_TIMEOUT_LIMITS。
const (
	apiTestTimeoutDefaultMS = 15000
	apiTestTimeoutMinMS     = 5000
	apiTestTimeoutMaxMS     = 120000
)

// ApiTestTimeout 复刻 resolveApiTestTimeoutMs：读 `API_TEST_TIMEOUT_MS`，
// 非法或在 [5000,120000] 之外回落 15000。
//
// 契约现状（登记）：`API_TEST_TIMEOUT_MS` **尚未并入 Go 的 config 契约**
// （`internal/config/env.go` 与 `go/env-parity.txt` 里都没有它），故此处直接读环境变量；
// 并入 config 属跨包改动，留给协调者决定（本 lane 不改 config）。
func ApiTestTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("API_TEST_TIMEOUT_MS"))
	if raw == "" {
		return apiTestTimeoutDefaultMS * time.Millisecond
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < apiTestTimeoutMinMS || parsed > apiTestTimeoutMaxMS {
		return apiTestTimeoutDefaultMS * time.Millisecond
	}
	return time.Duration(parsed) * time.Millisecond
}

// ProxyTestResult 逐字对应 actions/providers.ts:3410-3428 里 testProviderProxy 的返回 data。
type ProxyTestResult struct {
	Success        bool
	Message        string
	StatusCode     *int
	ResponseTimeMS int64
	UsedProxy      bool
	ProxyURL       string
	Error          string
	ErrorType      string
}

// TestProxyConnectivity 复刻 testProviderProxy 的连接部分（actions/providers.ts:3468-3545）：
// 对 `providerUrl` 发一次 **HEAD**，带代理 dispatcher 与超时；**任何 HTTP 状态码都算成功**
// （Node 不检查 response.ok——代理测试只回答「连得上吗」）。
//
// 错误分类（逐条对应 Node 的 isProxyError / isClientAbortError 判定）：
//   - 超时/取消 -> "Timeout"
//   - 走查了代理且失败（连接被拒/DNS/超时类）-> "ProxyError"
//   - 其余 -> "NetworkError"
//
// 已知差异（登记）：`message`/`error` 里的文本是 Go 的错误串，Node 的是 undici 的措辞；
// 对拍时不逐字比对这两个字段。
func TestProxyConnectivity(
	ctx context.Context,
	client *http.Client,
	providerURL string,
	proxy ProxyConfig,
	timeout time.Duration,
) ProxyTestResult {
	host := ""
	if parsed, err := url.Parse(providerURL); err == nil {
		host = parsed.Hostname()
	}

	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodHead, providerURL, nil)
	if err != nil {
		return ProxyTestResult{
			Message:   "连接失败: " + err.Error(),
			Error:     err.Error(),
			ErrorType: "NetworkError",
		}
	}

	start := time.Now()
	response, err := client.Do(request)
	elapsed := time.Since(start).Milliseconds()
	usedProxy := strings.TrimSpace(proxy.URL) != ""

	if err != nil {
		return ProxyTestResult{
			Message:        "连接失败: " + err.Error(),
			ResponseTimeMS: elapsed,
			UsedProxy:      usedProxy,
			ProxyURL:       proxy.URL,
			Error:          err.Error(),
			ErrorType:      classifyConnectivityError(err, usedProxy),
		}
	}
	defer func() { _ = response.Body.Close() }()

	status := response.StatusCode
	return ProxyTestResult{
		Success:        true,
		Message:        fmt.Sprintf("成功连接到 %s", host),
		StatusCode:     &status,
		ResponseTimeMS: elapsed,
		UsedProxy:      usedProxy,
		ProxyURL:       proxy.URL,
	}
}

// classifyConnectivityError 复刻 Node 的两级判定。
func classifyConnectivityError(err error, usedProxy bool) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "Timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "Timeout"
	}
	if usedProxy {
		// Node 的 isProxyError：消息里含 proxy / ECONNREFUSED / ENOTFOUND / ETIMEDOUT。
		// Go 侧用等价条件：代理在用时，任何连接类错误都归因代理（直连与代理混在一处时，
		// 归因代理比归因直连更符合「代理测试」的语义）。
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "econnrefused") || strings.Contains(message, "no such host") ||
			strings.Contains(message, "connection refused") || strings.Contains(message, "proxy") ||
			strings.Contains(message, "i/o timeout") || strings.Contains(message, "network is unreachable") {
			return "ProxyError"
		}
		return "ProxyError"
	}
	return "NetworkError"
}
