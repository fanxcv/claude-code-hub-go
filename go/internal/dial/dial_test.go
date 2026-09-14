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
)

func TestBuildUpstreamURLKeepsBasePathPrefix(t *testing.T) {
	cases := []struct {
		name string
		base string
		path string
		want string
	}{
		{"无前缀", "http://host:4321", "/v1/responses", "http://host:4321/v1/responses"},
		{"末尾斜杠", "http://host:4321/", "/v1/responses", "http://host:4321/v1/responses"},
		{"路径前缀必须保留", "https://opencode.ai/zen/go/", "/v1/responses", "https://opencode.ai/zen/go/v1/responses"},
		{"路径前缀无斜杠", "https://host/openai", "/v1/chat/completions", "https://host/openai/v1/chat/completions"},
		{"base 已含端点段不重复", "https://host/v1/messages", "/v1/messages", "https://host/v1/messages"},
		{"相似但不相同的尾巴不能折叠", "https://host/v1api", "/v1/messages", "https://host/v1api/v1/messages"},
		{"archive 端点不能折叠成 responses", "https://host/v1/responses-archive", "/v1/responses", "https://host/v1/responses-archive/v1/responses"},
		{"相对 path 自动补斜杠", "http://host/base", "v1/responses", "http://host/base/v1/responses"},
		{"空 path 保持 base", "http://host/base/", "", "http://host/base/"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := BuildUpstreamURL(testCase.base, testCase.path)
			if err != nil {
				t.Fatalf("BuildUpstreamURL 返回错误: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("期望 %q，实际 %q", testCase.want, got)
			}
		})
	}
}

func TestBuildUpstreamURLRejectsRelativeBase(t *testing.T) {
	if _, err := BuildUpstreamURL("host/v1", "/messages"); !errors.Is(err, ErrRequestBuild) {
		t.Fatalf("相对 base 必须以 ErrRequestBuild 拒绝，实际 %v", err)
	}
	if _, err := BuildUpstreamURL("", "/messages"); !errors.Is(err, ErrRequestBuild) {
		t.Fatalf("空 base 必须以 ErrRequestBuild 拒绝，实际 %v", err)
	}
}

func TestResolveClientIP(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		wantIP  string
		wantFF  string
	}{
		{
			name:    "只有 xff 时取首段",
			headers: map[string]string{"X-Forwarded-For": "1.1.1.1, 2.2.2.2"},
			wantIP:  "1.1.1.1",
			wantFF:  "1.1.1.1, 2.2.2.2",
		},
		{
			name:    "xff 缺失时退到 x-real-ip",
			headers: map[string]string{"X-Real-Ip": "3.3.3.3"},
			wantIP:  "3.3.3.3",
			wantFF:  "3.3.3.3",
		},
		{
			name:    "xff 优先于 x-real-ip",
			headers: map[string]string{"X-Forwarded-For": "1.1.1.1", "X-Real-Ip": "9.9.9.9"},
			wantIP:  "1.1.1.1",
			wantFF:  "1.1.1.1",
		},
		{
			name:    "多个 ff 头按顺序展开",
			headers: map[string]string{"X-Forwarded-For": "1.1.1.1,2.2.2.2", "X-Client-Ip": "4.4.4.4"},
			wantIP:  "1.1.1.1",
			wantFF:  "1.1.1.1, 2.2.2.2",
		},
		{
			name:    "全空时无 IP",
			headers: map[string]string{},
			wantIP:  "",
			wantFF:  "",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			headers := http.Header{}
			for key, value := range testCase.headers {
				headers.Set(key, value)
			}
			resolved := ResolveClientIP(headers)
			if resolved.ClientIp != testCase.wantIP {
				t.Fatalf("ClientIp 期望 %q，实际 %q", testCase.wantIP, resolved.ClientIp)
			}
			if resolved.XForwardedFor != testCase.wantFF {
				t.Fatalf("XForwardedFor 期望 %q，实际 %q", testCase.wantFF, resolved.XForwardedFor)
			}
		})
	}
}

func TestApplyClientIPHeadersRespectsPreserveFlag(t *testing.T) {
	source := http.Header{"X-Forwarded-For": []string{"1.1.1.1, 2.2.2.2"}}

	off := http.Header{}
	ApplyClientIPHeaders(off, IPHeaders{PreserveClientIp: false, Headers: source})
	if off.Get("x-forwarded-for") != "" || off.Get("x-real-ip") != "" {
		t.Fatalf("preserveClientIp=false 不得注入 IP 头，实际 %v", off)
	}

	on := http.Header{}
	ApplyClientIPHeaders(on, IPHeaders{PreserveClientIp: true, Headers: source})
	if got := on.Get("x-forwarded-for"); got != "1.1.1.1, 2.2.2.2" {
		t.Fatalf("x-forwarded-for 期望 %q，实际 %q", "1.1.1.1, 2.2.2.2", got)
	}
	if got := on.Get("x-real-ip"); got != "1.1.1.1" {
		t.Fatalf("x-real-ip 期望 %q，实际 %q", "1.1.1.1", got)
	}
}

func TestNewRejectsProxyURL(t *testing.T) {
	_, err := New(Options{ProxyURL: "socks5://127.0.0.1:1080"})
	if !errors.Is(err, ErrUnsupportedUpstreamTransport) {
		t.Fatalf("SOCKS 代理必须以 ErrUnsupportedUpstreamTransport 拒绝，实际 %v", err)
	}
	if strings.Contains(err.Error(), "socks5://127.0.0.1:1080") == false {
		t.Fatalf("错误信息应保留目标形态以便排障，实际 %q", err.Error())
	}
}

func TestForceIdentityEncoding(t *testing.T) {
	header := http.Header{"Accept-Encoding": []string{"gzip, br"}}
	ForceIdentityEncoding(header)
	if got := header.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("Accept-Encoding 必须为 identity，实际 %q", got)
	}
}

func TestPoolAdmissionRejectsInsteadOfQueueing(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// 立即写出响应头，让 RoundTrip 及时返回；随后把响应体挂住，使准入名额保持占用。
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
	}))
	defer server.Close()
	defer close(release)

	client, err := New(Options{MaxUpstreamConnections: 1})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer client.Close()

	first, err := client.RoundTrip(context.Background(), Request{Method: http.MethodPost, URL: server.URL, ContentLength: 0})
	if err != nil {
		t.Fatalf("首个请求应当进入准入: %v", err)
	}
	defer first.Body.Close()

	started := time.Now()
	_, err = client.RoundTrip(context.Background(), Request{Method: http.MethodPost, URL: server.URL, ContentLength: 0})
	if !errors.Is(err, ErrPoolAdmissionExceeded) {
		t.Fatalf("超限必须以 ErrPoolAdmissionExceeded 立即失败，实际 %v", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("超限必须立即返回而不是排队，实际耗时 %s", elapsed)
	}
}

func TestRoundTripPassesBytesThroughAndForcesIdentity(t *testing.T) {
	const payloadBytes = 512 * 1024
	payload := strings.Repeat("x", payloadBytes)
	var sawAcceptEncoding atomic.Value
	var sawHost atomic.Value

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sawAcceptEncoding.Store(request.Header.Get("Accept-Encoding"))
		sawHost.Store(request.Host)
		writer.Header().Set("X-Upstream-Marker", "kept")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, payload)
	}))
	defer server.Close()

	client, err := New(Options{})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer client.Close()

	response, err := client.RoundTrip(context.Background(), Request{
		Method:        http.MethodPost,
		URL:           server.URL + "/v1/responses",
		Headers:       http.Header{"Content-Type": []string{"application/json"}},
		Body:          strings.NewReader(`{"input":"hi"}`),
		ContentLength: int64(len(`{"input":"hi"}`)),
	})
	if err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码期望 200，实际 %d", response.StatusCode)
	}
	if got := response.Header.Get("X-Upstream-Marker"); got != "kept" {
		t.Fatalf("上游响应头必须原样保留，实际 %q", got)
	}
	if got := sawAcceptEncoding.Load(); got != "identity" {
		t.Fatalf("上游必须收到 Accept-Encoding: identity，实际 %v", got)
	}
	if got := sawHost.Load(); !strings.Contains(fmt.Sprint(got), "127.0.0.1") {
		t.Fatalf("Host 头必须与请求目标一致，实际 %v", got)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	if len(body) != payloadBytes {
		t.Fatalf("透传字节数期望 %d，实际 %d", payloadBytes, len(body))
	}
}

func TestRoundTripPassesNon2xxThrough(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(writer, `{"error":"upstream busy"}`)
	}))
	defer server.Close()

	client, err := New(Options{})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer client.Close()

	response, err := client.RoundTrip(context.Background(), Request{Method: http.MethodPost, URL: server.URL, ContentLength: 0})
	if err != nil {
		t.Fatalf("非 2xx 不应变成传输错误: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("状态码期望 503，实际 %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	if string(body) != `{"error":"upstream busy"}` {
		t.Fatalf("非 2xx 正文必须原样透传，实际 %q", string(body))
	}
}

func TestRoundTripHeadersTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	client, err := New(Options{HeadersTimeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer client.Close()

	_, err = client.RoundTrip(context.Background(), Request{Method: http.MethodPost, URL: server.URL, ContentLength: 0})
	if !errors.Is(err, ErrHeadersTimeout) {
		t.Fatalf("等响应头超时必须以 ErrHeadersTimeout 分类，实际 %v", err)
	}
}

func TestRoundTripBodyIdleTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Errorf("测试服务器不支持 Flush")
			return
		}
		_, _ = io.WriteString(writer, "data: first\n\n")
		flusher.Flush()
		// 之后长时间不再产出，触发响应体空闲超时。
		time.Sleep(2 * time.Second)
	}))
	defer server.Close()

	client, err := New(Options{BodyIdleTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer client.Close()

	response, err := client.RoundTrip(context.Background(), Request{Method: http.MethodPost, URL: server.URL, ContentLength: 0})
	if err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}
	defer response.Body.Close()

	buffer := make([]byte, 256)
	if _, err := response.Body.Read(buffer); err != nil {
		t.Fatalf("首次读应当成功: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, err = response.Body.Read(buffer)
		if err != nil {
			break
		}
	}
	if !errors.Is(err, ErrBodyIdleTimeout) {
		t.Fatalf("响应体空闲超时必须以 ErrBodyIdleTimeout 分类，实际 %v", err)
	}
}

func TestRoundTripUpstreamClosedEarly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			t.Errorf("测试服务器不支持 Hijack")
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		// 声明 100 字节正文却只发 10 字节后直接断开。
		_, _ = connection.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n0123456789"))
		_ = connection.Close()
	}))
	defer server.Close()

	client, err := New(Options{BodyIdleTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer client.Close()

	response, err := client.RoundTrip(context.Background(), Request{Method: http.MethodPost, URL: server.URL, ContentLength: 0})
	if err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}
	defer response.Body.Close()

	_, err = io.ReadAll(response.Body)
	if err == nil {
		t.Fatalf("上游提前断开必须报错")
	}
	if !errors.Is(err, ErrUpstreamClosed) && !errors.Is(err, ErrBodyIdleTimeout) {
		t.Fatalf("提前断开必须以 ErrUpstreamClosed（或空闲超时）分类，实际 %v", err)
	}
}

func TestRoundTripContextCanceledIsClassified(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	client, err := New(Options{HeadersTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err = client.RoundTrip(ctx, Request{Method: http.MethodPost, URL: server.URL, ContentLength: 0})
	if !errors.Is(err, ErrContextCanceled) {
		t.Fatalf("取消必须以 ErrContextCanceled 分类，实际 %v", err)
	}
}
