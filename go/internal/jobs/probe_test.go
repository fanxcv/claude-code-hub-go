package jobs

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// HTTP 拨测的成败判据：<500 算活（4xx 说明服务在，鉴权/路径问题不该判死端点）。
func TestProbeHTTPStatusCodeSemantics(t *testing.T) {
	cases := []struct {
		status      int
		wantOK      bool
		wantErrType string
	}{
		{200, true, ""},
		{301, true, ""},
		{404, true, ""},
		{429, true, ""},
		{500, false, "http_5xx"},
		{503, false, "http_5xx"},
	}
	for _, testCase := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(testCase.status)
		}))
		outcome := probeEndpointHTTP(context.Background(), server.Client(), "head", server.URL)
		server.Close()

		if outcome.OK != testCase.wantOK {
			t.Fatalf("状态 %d 的 ok 应为 %v，得到 %v", testCase.status, testCase.wantOK, outcome.OK)
		}
		if testCase.wantErrType != "" {
			if outcome.ErrorType == nil || *outcome.ErrorType != testCase.wantErrType {
				t.Fatalf("状态 %d 的错误类型应为 %q，得到 %v", testCase.status, testCase.wantErrType, outcome.ErrorType)
			}
		} else if outcome.ErrorType != nil {
			t.Fatalf("状态 %d 不应带错误类型，得到 %q", testCase.status, *outcome.ErrorType)
		}
		if outcome.StatusCode == nil || *outcome.StatusCode != testCase.status {
			t.Fatalf("状态码应回填 %d，得到 %v", testCase.status, outcome.StatusCode)
		}
	}
}

// 只有「拿不到状态码」才回落 GET：5xx 是明确答复，再打一次纯属噪声。
func TestProbeHTTPFallsBackToGetOnlyOnTransportFailure(t *testing.T) {
	var headCount, getCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodHead {
			headCount++
			// 关掉连接制造传输层失败，而不是返回 5xx。
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				t.Error("测试服务器不支持 hijack")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		getCount++
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	outcome := probeEndpointHTTP(context.Background(), server.Client(), "head", server.URL)
	if !outcome.OK {
		t.Fatalf("HEAD 失败后 GET 应成功，得到 %+v", outcome)
	}
	if headCount != 1 || getCount != 1 {
		t.Fatalf("应各打一次（HEAD 失败 → GET 重试），得到 head=%d get=%d", headCount, getCount)
	}

	// 5xx 不重试：只打一次 HEAD。
	headCount, getCount = 0, 0
	broken := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodHead {
			headCount++
		} else {
			getCount++
		}
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer broken.Close()

	outcome = probeEndpointHTTP(context.Background(), broken.Client(), "head", broken.URL)
	if outcome.OK {
		t.Fatal("5xx 不应判活")
	}
	if headCount != 1 || getCount != 0 {
		t.Fatalf("5xx 不应回落 GET，得到 head=%d get=%d", headCount, getCount)
	}
}

// TCP 拨测：端口能连上即活；连不上/超时按 Node 的分类回报。
func TestProbeTCPSemantics(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("测试监听失败: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = connection.Close()
		}
	}()

	outcome := probeEndpointTCP(context.Background(), "http://"+listener.Addr().String()+"/health", time.Second)
	if !outcome.OK {
		t.Fatalf("可连接端口应判活，得到 %+v", outcome)
	}
	if outcome.LatencyMS == nil {
		t.Fatal("成功拨测应带延迟")
	}
	// TCP 拨测没有 HTTP 状态码——它只证明这一层是活的。
	if outcome.StatusCode != nil {
		t.Fatalf("TCP 拨测不应带状态码，得到 %v", *outcome.StatusCode)
	}

	// 关闭后的端口：分类为 network_error（不是 timeout）。
	closedAddress := listener.Addr().String()
	_ = listener.Close()
	outcome = probeEndpointTCP(context.Background(), "http://"+closedAddress, time.Second)
	if outcome.OK {
		t.Fatal("已关闭的端口不应判活")
	}
	if outcome.ErrorType == nil || *outcome.ErrorType != "network_error" {
		t.Fatalf("连接拒绝应记为 network_error，得到 %v", outcome.ErrorType)
	}

	// 非法 URL：单独一类，且不回显原串（避免把凭据写进库）。
	outcome = probeEndpointTCP(context.Background(), "http://[::1]:namedport", time.Second)
	if outcome.OK {
		t.Fatal("非法 URL 不应判活")
	}
	if outcome.ErrorType == nil || *outcome.ErrorType != "invalid_url" {
		t.Fatalf("非法 URL 应记为 invalid_url，得到 %v", outcome.ErrorType)
	}
	if outcome.ErrorMessage == nil || strings.Contains(*outcome.ErrorMessage, "namedport") {
		t.Fatalf("非法 URL 的错误描述不应回显原串，得到 %v", outcome.ErrorMessage)
	}
}

// 拨测不受 ctx 取消之外的超时影响：连不上的地址要在超时内收敛。
func TestProbeTCPHonoursTimeout(t *testing.T) {
	// 203.0.113.0/24 是 TEST-NET-3，正常网络下不会有主机应答（拨测会超时而不是立即失败）。
	startedAt := time.Now()
	outcome := probeEndpointTCP(context.Background(), "http://203.0.113.1:81", 200*time.Millisecond)
	elapsed := time.Since(startedAt)

	if outcome.OK {
		t.Skip("测试环境把 TEST-NET-3 解析到了可达主机，跳过")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("拨测应受超时约束，实际耗时 %v", elapsed)
	}
	if outcome.ErrorType == nil || (*outcome.ErrorType != "timeout" && *outcome.ErrorType != "network_error") {
		t.Fatalf("超时/不可达应分类为 timeout 或 network_error，得到 %v", outcome.ErrorType)
	}
}

// 熔断记账用的错误描述：HTTP 码优先，其次错误类型，都不带原始上游串。
func TestProbeOutcomeCircuitCause(t *testing.T) {
	status := 503
	errorType := "http_5xx"

	if got := (probeOutcome{StatusCode: &status, ErrorType: &errorType}).circuitCause(); got != "HTTP 503" {
		t.Fatalf("有状态码时应优先用 HTTP 码，得到 %q", got)
	}
	if got := (probeOutcome{ErrorType: &errorType}).circuitCause(); got != "http_5xx" {
		t.Fatalf("无状态码时应回退错误类型，得到 %q", got)
	}
	if got := (probeOutcome{}).circuitCause(); got != "probe_failed" {
		t.Fatalf("两者皆无时应兜底 probe_failed，得到 %q", got)
	}
}
