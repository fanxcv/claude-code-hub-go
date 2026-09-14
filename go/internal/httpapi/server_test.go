package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// stubProber 用固定结果替代真实依赖探测。
type stubProber struct {
	pg    DependencyStatus
	redis DependencyStatus
	rules DependencyStatus
	note  string
}

func (s stubProber) PingPG(context.Context) (DependencyStatus, string) {
	return s.pg, s.note
}

func (s stubProber) PingRedis(context.Context) (DependencyStatus, string) {
	return s.redis, s.note
}

func (s stubProber) RulesStatus() (DependencyStatus, string) {
	return s.rules, s.note
}

func newTestServer(t *testing.T, prober Prober, listening bool) (*Server, *bytes.Buffer) {
	t.Helper()
	logBuffer := &bytes.Buffer{}
	return New(ServerOptions{
		Logger:    logx.New(logBuffer),
		Prober:    prober,
		Listening: func() bool { return listening },
	}), logBuffer
}

func TestReadyzReportsAllDependencies(t *testing.T) {
	server, _ := newTestServer(t, stubProber{
		pg: StatusOK, redis: StatusOK, rules: StatusOK,
	}, true)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("全部依赖就绪时应返回 200，收到 %d", recorder.Code)
	}
	var report ReadyReport
	if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatalf("响应体必须是 JSON: %v", err)
	}
	if !report.Ready {
		t.Fatalf("四项全 ok 时 ready 应为 true，收到 %+v", report)
	}
	for name, status := range map[string]DependencyStatus{
		"self": report.Self, "pg": report.PG, "redis": report.Redis, "rules": report.Rules,
	} {
		if status != StatusOK {
			t.Errorf("%s 应为 ok，收到 %q", name, status)
		}
	}
	if report.At == "" {
		t.Errorf("响应体应带时间戳")
	}
}

func TestReadyzFailsWhenDependencyDegraded(t *testing.T) {
	cases := []struct {
		name      string
		prober    Prober
		listening bool
		wantPG    DependencyStatus
	}{
		{"pg 不可用", stubProber{pg: StatusError, redis: StatusOK, rules: StatusOK, note: "connection refused"}, true, StatusError},
		{"redis 未配置", stubProber{pg: StatusOK, redis: StatusNotSet, rules: StatusOK}, true, StatusOK},
		{"规则快照未装载", stubProber{pg: StatusOK, redis: StatusOK, rules: StatusNotLoaded}, true, StatusOK},
		{"自身未监听", stubProber{pg: StatusOK, redis: StatusOK, rules: StatusOK}, false, StatusOK},
		{"探测未接线", nil, true, StatusUnavailable},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, _ := newTestServer(t, testCase.prober, testCase.listening)
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("依赖未就绪时应返回 503，收到 %d", recorder.Code)
			}
			var report ReadyReport
			if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
				t.Fatalf("响应体必须是 JSON: %v", err)
			}
			if report.Ready {
				t.Fatalf("依赖未就绪时 ready 不得为 true: %+v", report)
			}
			if report.PG != testCase.wantPG {
				t.Errorf("pg 状态应为 %q，收到 %q", testCase.wantPG, report.PG)
			}
			if len(report.Failing) == 0 {
				t.Fatalf("未就绪时必须列出未就绪项: %+v", report)
			}
		})
	}
}

func TestPingEndpoint(t *testing.T) {
	server, _ := newTestServer(t, nil, true)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/_ping", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("/v1/_ping 应返回 200，收到 %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"pong"`) {
		t.Fatalf("响应体应含 pong，收到 %q", recorder.Body.String())
	}
}

// 数据面未装配时必须 503 而不是 501：501 会把「本进程还没实现」谎报成协议不支持。
func TestDataPlaneMissingReturns503(t *testing.T) {
	server, _ := newTestServer(t, nil, true)
	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions", "/v1beta/models"} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))

		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s 应返回 503，收到 %d", path, recorder.Code)
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "data_plane_unavailable") || !strings.Contains(body, path) {
			t.Fatalf("%s 的 503 响应体应注明数据面未装配并回显路径，收到 %q", path, body)
		}
	}
}

// 数据面挂载后，请求必须经前门中间件再落到 DataPlane。
func TestFrontDoorWrapsDataPlane(t *testing.T) {
	var seen []string
	frontDoor := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			seen = append(seen, "frontdoor:"+request.URL.Path)
			next.ServeHTTP(writer, request)
		})
	}
	server := New(ServerOptions{
		FrontDoor: frontDoor,
		DataPlane: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			seen = append(seen, "dataplane:"+request.URL.Path)
			writeJSON(writer, http.StatusOK, map[string]any{"path": request.URL.Path})
		}),
	})

	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1beta/models"} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s 应落到数据面并返回 200，收到 %d", path, recorder.Code)
		}
	}
	want := []string{
		"frontdoor:/v1/messages", "dataplane:/v1/messages",
		"frontdoor:/v1/responses", "dataplane:/v1/responses",
		"frontdoor:/v1beta/models", "dataplane:/v1beta/models",
	}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("前门与数据面的调用顺序应为 %v，收到 %v", want, seen)
	}
}

// 探针不归前门管：排空窗口内前门拒掉一切请求时，探针仍须作答。
func TestProbesBypassFrontDoor(t *testing.T) {
	rejected := 0
	frontDoor := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			rejected++
			writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "backend_switching"})
		})
	}
	server := New(ServerOptions{
		Prober:    stubProber{pg: StatusOK, redis: StatusOK, rules: StatusOK},
		FrontDoor: frontDoor,
		DataPlane: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	})

	pingRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(pingRecorder, httptest.NewRequest(http.MethodGet, "/v1/_ping", nil))
	if pingRecorder.Code != http.StatusOK {
		t.Fatalf("/v1/_ping 应绕过前门返回 200，收到 %d", pingRecorder.Code)
	}

	readyRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(readyRecorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if readyRecorder.Code != http.StatusOK {
		t.Fatalf("/readyz 应绕过前门返回 200，收到 %d", readyRecorder.Code)
	}

	dataRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(dataRecorder, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if dataRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("数据面请求应被前门拒掉，收到 %d", dataRecorder.Code)
	}
	if rejected != 1 {
		t.Fatalf("前门只应看到数据面请求，收到 %d 次", rejected)
	}
}

// 排空中 /readyz 必须 503，否则探针会把流量继续导来。
func TestReadyzFailsWhileDraining(t *testing.T) {
	server := New(ServerOptions{
		Prober:   stubProber{pg: StatusOK, redis: StatusOK, rules: StatusOK},
		Draining: func() bool { return true },
	})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("排空中应返回 503，收到 %d", recorder.Code)
	}
	var report ReadyReport
	if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatalf("响应体必须是 JSON: %v", err)
	}
	if report.Ready || report.Self != StatusError {
		t.Fatalf("排空中 self 应为 error 且 ready 为 false，收到 %+v", report)
	}
	if report.Notes["self"] != "draining" {
		t.Fatalf("应注明排空原因，收到 %v", report.Notes)
	}
}

// 流式响应经请求日志包装后仍必须能逐块 Flush。
func TestResponseWriterKeepsFlusher(t *testing.T) {
	buffer := &bytes.Buffer{}
	server := New(ServerOptions{Logger: logx.New(buffer)})
	handler := server.withRequestLog(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Error("包装后的 ResponseWriter 必须实现 http.Flusher")
			return
		}
		_, _ = writer.Write([]byte("chunk"))
		flusher.Flush()
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	if !recorder.Flushed {
		t.Fatal("Flush 必须透传到下层 ResponseWriter")
	}
}

func TestUnknownRouteReturns404(t *testing.T) {
	server, _ := newTestServer(t, nil, true)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/providers", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("非 /v1 路径应返回 404，收到 %d", recorder.Code)
	}
}

func TestRootEndpointDescribesService(t *testing.T) {
	server, _ := newTestServer(t, nil, true)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("根路径应返回 200，收到 %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "cchd") {
		t.Fatalf("根路径应自报服务名，收到 %q", body)
	}
}

// 未显式 WriteHeader 的响应体写入必须被记为 200，否则日志会漏状态码。
func TestStatusRecorderDefaultsTo200(t *testing.T) {
	buffer := &bytes.Buffer{}
	server := New(ServerOptions{Logger: logx.New(buffer), Listening: func() bool { return true }})
	handler := server.withRequestLog(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("no explicit status"))
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/raw", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("未显式写状态码时应为 200，收到 %d", recorder.Code)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buffer.String())), &record); err != nil {
		t.Fatalf("日志必须是单行 JSON: %v", err)
	}
	if record["status"] != float64(http.StatusOK) {
		t.Fatalf("日志应记下默认状态码 200，收到 %v", record["status"])
	}
}

// New 必须能在零值参数下可用，否则骨架在依赖未接线时会 panic。
func TestNewWithoutOptions(t *testing.T) {
	server := New(ServerOptions{})
	if server.logger == nil {
		t.Fatal("未传 Logger 时应回退到默认日志器")
	}
	if !server.listening() {
		t.Fatal("未传 Listening 时应视为已监听")
	}
	if server.probeTimeout <= 0 {
		t.Fatal("未传 ProbeTimeout 时应回退到默认超时")
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/_ping", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("零值装配下 /v1/_ping 应可用，收到 %d", recorder.Code)
	}
}

// 请求日志只记方法、路径、状态、耗时：正文与 Authorization 绝不落盘。
func TestRequestLogOmitsBodyAndAuthorization(t *testing.T) {
	const secret = "sk-super-secret-key"
	const body = `{"model":"gpt-5.6","input":"body-must-not-be-logged"}`

	server, logBuffer := newTestServer(t, nil, true)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	logged := logBuffer.String()
	if logged == "" {
		t.Fatal("请求日志不得为空")
	}
	if strings.Contains(logged, secret) {
		t.Fatalf("请求日志泄漏了 Authorization: %s", logged)
	}
	if strings.Contains(logged, "body-must-not-be-logged") {
		t.Fatalf("请求日志泄漏了正文: %s", logged)
	}
	if strings.Contains(strings.ToLower(logged), "authorization") {
		t.Fatalf("请求日志不得出现 Authorization 字段名: %s", logged)
	}

	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(logged)), &record); err != nil {
		t.Fatalf("日志必须是单行 JSON: %v", err)
	}
	if record["event"] != "http_request" {
		t.Fatalf("日志事件名应为 http_request，收到 %v", record["event"])
	}
	for _, key := range []string{"method", "path", "status", "duration"} {
		if _, ok := record[key]; !ok {
			t.Fatalf("日志缺少字段 %s: %s", key, logged)
		}
	}
	if record["status"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("日志状态码应为 503，收到 %v", record["status"])
	}
}

// 配置结论只进 notes、不参与就绪判定：亲和关掉时 /readyz 必须仍是 200，但必须能查到为什么。
func TestReadyzMergesConfigurationNotes(t *testing.T) {
	server := New(ServerOptions{
		Logger:        logx.New(&bytes.Buffer{}),
		Prober:        stubProber{pg: StatusOK, redis: StatusOK, rules: StatusOK},
		Listening:     func() bool { return true },
		Configuration: func() map[string]string { return map[string]string{"affinity": "enabled（来源 env）"} },
	})

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("配置结论不得影响就绪：收到 %d", recorder.Code)
	}
	var report ReadyReport
	if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatalf("响应体必须是 JSON: %v", err)
	}
	if report.Notes["affinity"] != "enabled（来源 env）" {
		t.Fatalf("notes 必须带亲和结论，收到 %+v", report.Notes)
	}
}

// 页面面挂载后，非 API 路径必须经 Pages 中间件（SSR 页面由 Go 反代回 Node 的唯一依据）。
func TestPagesWrapsNonAPIPaths(t *testing.T) {
	var seen []string
	pages := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			seen = append(seen, "pages:"+request.URL.Path)
			next.ServeHTTP(writer, request)
		})
	}
	frontDoor := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			seen = append(seen, "frontdoor:"+request.URL.Path)
			next.ServeHTTP(writer, request)
		})
	}
	server := New(ServerOptions{
		Prober:    stubProber{pg: StatusOK, redis: StatusOK, rules: StatusOK},
		FrontDoor: frontDoor,
		Pages:     pages,
		DataPlane: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	})

	for _, path := range []string{"/zh-CN/dashboard", "/login", "/_next/static/chunks/app.js"} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		// 未接管时的落点是本进程自答（404），关键是它必须经过页面面判定。
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s 应由本进程自答 404，收到 %d", path, recorder.Code)
		}
		if len(seen) == 0 || !strings.HasPrefix(seen[len(seen)-1], "pages:"+path) {
			t.Fatalf("%s 应经页面面中间件，收到 %v", path, seen)
		}
	}
	// 探针与 API 前缀不进页面面：它们的归属另有其门。
	seen = nil
	for _, path := range []string{"/readyz", "/v1/_ping", "/v1/messages"} {
		server.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	for _, entry := range seen {
		if strings.HasPrefix(entry, "pages:") {
			t.Fatalf("API 与探针路径不应进页面面，收到 %v", seen)
		}
	}
}
