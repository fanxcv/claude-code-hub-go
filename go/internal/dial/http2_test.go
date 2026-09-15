package dial

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// roundTripFunc 是测试用的 RoundTripper 桩。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// tlsTestServer 起一个支持 ALPN h2 的 TLS 测试服务器。
//
// 返回的 transport 已带该服务器的自签根证书，供拨号器两套传输共用——否则 h1/h2 两条路径会各自
// 因为证书校验失败而误判「传输层有问题」。
func tlsTestServer(t *testing.T, handler http.Handler) (*httptest.Server, *http.Transport) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	transport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("测试服务器未给出 *http.Transport")
	}
	return server, transport
}

// patchTLS 把测试服务器的信任配置装进拨号器的两套传输。
func patchTLS(client *Client, transport *http.Transport) {
	if h1, ok := client.http.Transport.(*http.Transport); ok {
		h1.TLSClientConfig = transport.TLSClientConfig
	}
	if h2Client := client.http2Client(); h2Client != nil {
		if h2, ok := h2Client.Transport.(*http.Transport); ok {
			h2.TLSClientConfig = transport.TLSClientConfig
		}
	}
}

func TestHTTP2EnabledSelectsHTTP2Transport(t *testing.T) {
	var sawProto atomic.Value
	server, tlsTransport := tlsTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sawProto.Store(request.Proto)
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, "ok")
	}))

	client, err := New(Options{HTTP2Enabled: func(context.Context) bool { return true }})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer client.Close()
	patchTLS(client, tlsTransport)

	response, err := client.RoundTrip(context.Background(), Request{Method: http.MethodPost, URL: server.URL + "/v1/responses"})
	if err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}
	defer response.Body.Close()

	if response.Proto != "HTTP/2.0" {
		t.Fatalf("开关开启时上游应收到 HTTP/2，实际 %q", response.Proto)
	}
	if got := fmt.Sprint(sawProto.Load()); got != "HTTP/2.0" {
		t.Fatalf("上游侧协议期望 HTTP/2.0，实际 %v", got)
	}
}

func TestHTTP2DisabledByDefaultUsesHTTP1(t *testing.T) {
	server, tlsTransport := tlsTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))

	client, err := New(Options{})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer client.Close()
	patchTLS(client, tlsTransport)

	response, err := client.RoundTrip(context.Background(), Request{Method: http.MethodPost, URL: server.URL + "/v1/responses"})
	if err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}
	defer response.Body.Close()

	if response.Proto != "HTTP/1.1" {
		t.Fatalf("未开启开关时必须停在 HTTP/1.1（与端口历史行为一致），实际 %q", response.Proto)
	}
}

func TestHTTP2ProtocolErrorFallsBackAndQuarantines(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, "ok")
	}))
	defer server.Close()

	client, err := New(Options{HTTP2Enabled: func(context.Context) bool { return true }})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer client.Close()

	// 注入「必然报 HTTP/2 协议错误」的 h2 传输：用真实的 http2 错误类型，避免只测到字符串匹配。
	var h2Attempts atomic.Int64
	client.h2Once.Do(func() {})
	client.h2 = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		h2Attempts.Add(1)
		return nil, http2.StreamError{StreamID: 1, Code: http2.ErrCodeProtocol}
	})}

	response, err := client.RoundTrip(context.Background(), Request{
		Method:        http.MethodPost,
		URL:           server.URL + "/v1/responses",
		Body:          strings.NewReader(`{"input":"hi"}`),
		ContentLength: int64(len(`{"input":"hi"}`)),
	})
	if err != nil {
		t.Fatalf("HTTP/2 协议错误必须透明回退 HTTP/1.1 成功，实际错误 %v", err)
	}
	defer response.Body.Close()

	if h2Attempts.Load() != 1 {
		t.Fatalf("h2 只应尝试一次，实际 %d 次", h2Attempts.Load())
	}
	if hits.Load() != 1 {
		t.Fatalf("回退后的 HTTP/1.1 请求应到达上游一次，实际 %d", hits.Load())
	}
	if !client.quarantine.quarantined(server.URL, time.Now()) {
		t.Fatalf("出现 h2 协议错误后该路由必须被隔离")
	}
	if client.http2Eligible(context.Background(), server.URL) {
		t.Fatalf("隔离期内不得再尝试 HTTP/2")
	}
}

func TestHTTP2ProtocolErrorWithoutRewindableBodyDoesNotRetry(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := New(Options{HTTP2Enabled: func(context.Context) bool { return true }})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer client.Close()
	client.h2Once.Do(func() {})
	client.h2 = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("stream error: stream ID 3; REFUSED_STREAM")
	})}

	// io.NopCloser 只实现 Read：正文不可回到起点，重试会把残缺正文再发一次，故必须放弃重试。
	_, err = client.RoundTrip(context.Background(), Request{
		Method: http.MethodPost,
		URL:    server.URL + "/v1/responses",
		Body:   io.NopCloser(strings.NewReader(`{"input":"hi"}`)),
	})
	if err == nil {
		t.Fatalf("正文不可重放时不得重试，必须把失败交回调用方")
	}
	if hits.Load() != 0 {
		t.Fatalf("正文不可重放时不得发出第二次请求，实际到达上游 %d 次", hits.Load())
	}
	if !errors.Is(err, ErrConnect) {
		t.Fatalf("错误必须可判别（ErrConnect），实际 %v", err)
	}
}

func TestIsHTTP2ProtocolError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"Go 的流错误", http2.StreamError{StreamID: 1, Code: http2.ErrCodeProtocol}, true},
		{"Go 的 GOAWAY", http2.GoAwayError{}, true},
		{"Node 风格代码串", errors.New("ERR_HTTP2_GOAWAY_SESSION"), true},
		{"包装两层后仍能识别", fmt.Errorf("上游失败: %w", fmt.Errorf("wrap: %w", errors.New("received GOAWAY with error code ENHANCE_YOUR_CALM"))), true},
		{"REFUSED_STREAM", errors.New("stream error: stream ID 5; REFUSED_STREAM"), true},
		{"普通建连失败", errors.New("dial tcp 127.0.0.1:443: connect: connection refused"), false},
		{"超时", context.DeadlineExceeded, false},
		{"空错误", nil, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isHTTP2ProtocolError(testCase.err); got != testCase.want {
				t.Fatalf("isHTTP2ProtocolError(%v) 期望 %v，实际 %v", testCase.err, testCase.want, got)
			}
		})
	}
}

func TestHTTP2QuarantineExpiresAndStaysBounded(t *testing.T) {
	quarantine := newHTTP2Quarantine()
	now := time.Now()
	quarantine.quarantine("https://upstream.example.com/v1/messages", now)

	if !quarantine.quarantined("https://upstream.example.com/v1/messages", now.Add(time.Minute)) {
		t.Fatalf("隔离期内必须报告已隔离")
	}
	if !quarantine.quarantined("https://upstream.example.com/other-path", now.Add(time.Minute)) {
		t.Fatalf("隔离粒度是 origin，同源的其它路径也必须视为已隔离")
	}
	if quarantine.quarantined("https://other.example.com/v1/messages", now.Add(time.Minute)) {
		t.Fatalf("不同 origin 不得被牵连")
	}
	if quarantine.quarantined("https://upstream.example.com/v1/messages", now.Add(http2QuarantineTTL+time.Second)) {
		t.Fatalf("超过 TTL 必须自动解禁（对齐 Node 的 5 分钟）")
	}

	for index := 0; index < http2QuarantineMaxRoutes+50; index++ {
		quarantine.quarantine(fmt.Sprintf("https://host-%d.example.com/v1/messages", index), now)
	}
	if got := quarantine.count(); got > http2QuarantineMaxRoutes {
		t.Fatalf("隔离表必须有界（上限 %d），实际 %d", http2QuarantineMaxRoutes, got)
	}
}

func TestRewindBody(t *testing.T) {
	readable := strings.NewReader("payload")
	if !rewindBody(readable) {
		t.Fatalf("可 Seek 的正文应能回到起点")
	}
	if _, err := io.ReadAll(readable); err != nil {
		t.Fatalf("回到起点后应可重新读完: %v", err)
	}
	if rewindBody(nil) != true {
		t.Fatalf("无正文视为可重试")
	}
	if rewindBody(io.NopCloser(strings.NewReader("payload"))) {
		t.Fatalf("不可 Seek 的正文不得被当作可重放")
	}
}
