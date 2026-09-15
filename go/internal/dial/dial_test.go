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

// TestBuildUpstreamURLJoinsEndpointRoot 钉住 Node buildProxyUrl 的「端点根 / 版本根」语义
// （用例逐条对齐 src/lib/v1-url.ts 的既有断言，见 tests/unit/app/v1/url.test.ts）。
//
// 生产事故（2026-09-15，ARK Codex 渠道）：供应商 url 填 `…/api/plan/v3`，本函数曾拼成
// `…/api/plan/v3/v1/chat/completions`（版本段重复，上游 404，整家渠道被摘出竞争），
// 而 Node 同一配置拼的是 `…/api/plan/v3/chat/completions`（上游 200）。
func TestBuildUpstreamURLJoinsEndpointRoot(t *testing.T) {
	cases := []struct {
		name string
		base string
		path string
		want string
	}{
		{
			"base 停在版本根：只补端点不重复版本段",
			"https://relay.example.com/openai/v1", "/v1/chat/completions",
			"https://relay.example.com/openai/v1/chat/completions",
		},
		{
			"事故配置：base 停在 /api/plan/v3",
			"https://ark.example.com/api/plan/v3", "/v1/chat/completions",
			"https://ark.example.com/api/plan/v3/chat/completions",
		},
		{
			"任意前缀的版本根 v4",
			"https://open.bigmodel.cn/api/coding/paas/v4", "/v1/chat/completions",
			"https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
		},
		{
			"带数字后缀的版本根 v1beta1",
			"https://relay.example.com/openai/v1beta1", "/v1/chat/completions",
			"https://relay.example.com/openai/v1beta1/chat/completions",
		},
		{
			"带 rc 后缀的版本根 v1rc1",
			"https://relay.example.com/openai/v1rc1", "/v1/chat/completions",
			"https://relay.example.com/openai/v1rc1/chat/completions",
		},
		{
			"版本根 + 资源后缀要留住",
			"https://relay.example.com/openai/v1", "/v1/chat/completions/cmpl_123/messages",
			"https://relay.example.com/openai/v1/chat/completions/cmpl_123/messages",
		},
		{
			"版本根 + models 资源",
			"https://relay.example.com/openai/v1", "/v1/models/gpt-4o",
			"https://relay.example.com/openai/v1/models/gpt-4o",
		},
		{
			"版本根 + images 端点",
			"https://relay.example.com/openai/v1", "/v1/images/generations",
			"https://relay.example.com/openai/v1/images/generations",
		},
		{
			"版本根 + audio 端点",
			"https://relay.example.com/openai/v1", "/v1/audio/transcriptions",
			"https://relay.example.com/openai/v1/audio/transcriptions",
		},
		{
			"base 是端点根（无版本段）：不补版本段",
			"https://relay.example.com/openai/responses", "/v1/responses",
			"https://relay.example.com/openai/responses",
		},
		{
			"base 是 embeddings 端点根",
			"https://relay.example.com/openai/embeddings", "/v1/embeddings",
			"https://relay.example.com/openai/embeddings",
		},
		{
			"端点根 + 资源后缀",
			"https://relay.example.com/openai/messages", "/v1/messages/count_tokens",
			"https://relay.example.com/openai/messages/count_tokens",
		},
		{
			"端点根（带版本段）+ 资源后缀",
			"https://relay.example.com/openai/v1/images", "/v1/images/edits",
			"https://relay.example.com/openai/v1/images/edits",
		},
		{
			"端点根 responses + 资源后缀",
			"https://relay.example.com/openai/responses", "/v1/responses/abc",
			"https://relay.example.com/openai/responses/abc",
		},
		{
			"gemini 端点根 + 模型动作后缀",
			"https://api.example.com/gemini/models", "/v1beta/models/gemini-1.5-pro:streamGenerateContent",
			"https://api.example.com/gemini/models/gemini-1.5-pro:streamGenerateContent",
		},
		{
			"v1internal 版本段同样可剥",
			"https://example.com/gemini/models", "/v1internal/models/gemini-2.5-flash:generateContent",
			"https://example.com/gemini/models/gemini-2.5-flash:generateContent",
		},
		{
			"v1api 不是版本根：照常标准拼接",
			"https://relay.example.com/proxy/v1api", "/v1/chat/completions",
			"https://relay.example.com/proxy/v1api/v1/chat/completions",
		},
		{
			"responses-archive 不得被折叠成 responses",
			"https://relay.example.com/openai/responses-archive", "/v1/responses",
			"https://relay.example.com/openai/responses-archive/v1/responses",
		},
		{
			"非端点路径的版本根不剥版本段",
			"https://relay.example.com/openai/v1", "/v1/responses-archive",
			"https://relay.example.com/openai/v1/v1/responses-archive",
		},
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

// TestBuildUpstreamURLKeepsQueryInEveryBranch 钉住「每条分支都带查询串」。
//
// 回归：原先 Case 1（base 已是请求路径前缀）直接返回，未写回查询，故 Gemini 官方端点形态
// 下 `?alt=sse` 会被静默吞掉（上游改回 JSON 数组，客户端拿到非流式响应）。
func TestBuildUpstreamURLKeepsQueryInEveryBranch(t *testing.T) {
	cases := []struct {
		name  string
		base  string
		path  string
		query string
		want  string
	}{
		{
			"Case 1（base 已是完整端点）带查询",
			"https://generativelanguage.example.com/v1beta/models/gemini-2.0-flash:streamGenerateContent",
			"/v1beta/models/gemini-2.0-flash:streamGenerateContent", "alt=sse",
			"https://generativelanguage.example.com/v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse",
		},
		{
			"Case 2（版本根）带查询",
			"https://ark.example.com/api/plan/v3", "/v1/chat/completions", "trace=1",
			"https://ark.example.com/api/plan/v3/chat/completions?trace=1",
		},
		{
			"Case 3（标准拼接）带查询",
			"https://api.example.com", "/v1/messages", "x=1",
			"https://api.example.com/v1/messages?x=1",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := BuildUpstreamURLWithQuery(testCase.base, testCase.path, testCase.query)
			if err != nil {
				t.Fatalf("BuildUpstreamURLWithQuery 返回错误: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("期望 %q，实际 %q", testCase.want, got)
			}
		})
	}

	// 空查询串时保留 base 自带查询（探针与回放路径依赖此语义）。
	got, err := BuildUpstreamURLWithQuery("https://api.example.com/v1/messages?from=base", "/v1/messages", "")
	if err != nil {
		t.Fatalf("BuildUpstreamURLWithQuery 返回错误: %v", err)
	}
	if want := "https://api.example.com/v1/messages?from=base"; got != want {
		t.Fatalf("空查询串应保留 base 查询：期望 %q，实际 %q", want, got)
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
