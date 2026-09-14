package dataplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// roundTripFunc 让测试用函数式 RoundTripper 顶替真实网络（dial.Options.Transport 是为此留的缝）。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// fakeModelCatalog 是 ModelCatalog 的测试替身：记录调用参数并回放固定行。
type fakeModelCatalog struct {
	rows []ModelProvider
	err  error
	// 记录最后一次调用的过滤条件，用于断言「固定类型集」与「按格式决策」的分派。
	lastTypes []string
	lastKeyID int64
	lastUser  int64
}

func (c *fakeModelCatalog) ProvidersForModelList(
	_ context.Context, providerTypes []string, keyID int64, userID int64,
) ([]ModelProvider, error) {
	c.lastTypes = append([]string(nil), providerTypes...)
	c.lastKeyID = keyID
	c.lastUser = userID
	if c.err != nil {
		return nil, c.err
	}
	return c.rows, nil
}

// newModelsTestHandler 建一个只够跑模型列表的数据面处理器。
func newModelsTestHandler(t *testing.T, catalog ModelCatalog, transport http.RoundTripper) *Handler {
	t.Helper()
	client, err := dial.New(dial.Options{Transport: transport})
	if err != nil {
		t.Fatalf("建拨号器失败: %v", err)
	}
	auth := fakeAuth{}
	handler, err := New(Options{
		Logger:       logx.New(nil),
		ModelCatalog: catalog,
		Forward:      forward.Deps{Dial: client},
		Base: guard.Deps{
			Auth:           auth,
			Users:          auth,
			Settings:       fakeSettings{},
			Sensitive:      fakeEmptySource{},
			Filters:        fakeEmptySource{},
			Provider:       fakeProvider{selection: pctx.ProviderSelection{ProviderID: 7, Name: "假供应商", Type: "claude"}},
			MessageContext: &fakeMessageWriter{},
		},
		// 模型列表路径不建行、不结算，但 New 要求这两个必填缝在位（与生产装配一致）。
		Candidates: fakeCandidates{url: "https://unused.test"},
		Settlers:   func(*RequestState) Settler { return &fakeSettler{} },
	})
	if err != nil {
		t.Fatalf("建数据面失败: %v", err)
	}
	return handler
}

// jsonTransport 回放固定 JSON 体并按路径区分（上游模型列表用）。
func jsonTransport(t *testing.T, bodies map[string]string) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, ok := bodies[request.URL.Path]
		if !ok {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":"not found"}`)),
				Request:    request,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})
}

// modelListSpecForTest 取一条聚合端点规格（按路径）。
func modelListSpecForTest(t *testing.T, method string, path string) routeSpecForTest {
	t.Helper()
	spec, ok := matchModelListRoute(strings.ToUpper(method), normalizeForMatch(path))
	if !ok {
		t.Fatalf("%s %s 未命中聚合端点表", method, path)
	}
	return routeSpecForTest{spec: spec}
}

// routeSpecForTest 只是把 routeSpec 包一层，避免测试里到处写内部字段名。
type routeSpecForTest struct{ spec routeSpec }

// decodeModels 解出响应 JSON。
func decodeModels(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (body=%s)", err, recorder.Body.String())
	}
	return payload
}

// TestModelListAggregatesAllowedModelsAndUpstream 覆盖取数两来源（allowed_models 与上游）与去重。
func TestModelListAggregatesAllowedModelsAndUpstream(t *testing.T) {
	catalog := &fakeModelCatalog{rows: []ModelProvider{
		{ID: 1, Name: "claude-box", Type: "claude", URL: "https://upstream.test",
			AllowedModels: json.RawMessage(`["claude-opus-4","claude-haiku-4"]`)},
		{ID: 2, Name: "claude-fetch", Type: "claude", URL: "https://upstream.test"},
	}}
	transport := jsonTransport(t, map[string]string{
		"/v1/models": `{"data":[{"id":"claude-opus-4"},{"id":"claude-sonnet-4"}]}`,
	})
	handler := newModelsTestHandler(t, catalog, transport)

	spec := modelListSpecForTest(t, http.MethodGet, "/v1/models")
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer test-key")
	recorder := httptest.NewRecorder()
	handler.serveModelList(recorder, request, spec.spec, pctx.AuthState{KeyID: 7, UserID: 9})

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	payload := decodeModels(t, recorder)
	if payload["object"] != "list" {
		t.Fatalf("object 应为 list，实为 %v", payload["object"])
	}
	data, _ := payload["data"].([]any)
	ids := make([]string, 0, len(data))
	for _, item := range data {
		entry, _ := item.(map[string]any)
		ids = append(ids, entry["id"].(string))
		if entry["object"] != "model" {
			t.Fatalf("data[].object 应为 model，实为 %v", entry["object"])
		}
		if entry["created"] == nil || entry["owned_by"] == nil {
			t.Fatalf("data[] 缺 created/owned_by: %v", entry)
		}
	}
	want := []string{"claude-opus-4", "claude-haiku-4", "claude-sonnet-4"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("模型顺序与去重不符：want %v got %v", want, ids)
	}
	// owned_by 走 inferOwner：claude-* 判 anthropic。
	first, _ := data[0].(map[string]any)
	if first["owned_by"] != "anthropic" {
		t.Fatalf("claude-* 的 owned_by 应为 anthropic，实为 %v", first["owned_by"])
	}
	if catalog.lastKeyID != 7 || catalog.lastUser != 9 {
		t.Fatalf("目录调用未带上认证身份：key=%d user=%d", catalog.lastKeyID, catalog.lastUser)
	}
	// 按客户端格式决策：无覆盖 + 无 anthropic/gemini 头 → openai → ["codex","openai-compatible"]。
	if strings.Join(catalog.lastTypes, ",") != "codex,openai-compatible" {
		// 本用例没有 anthropic/gemini 头、也没有 format 覆盖，故应落 openai 分支。
		t.Fatalf("默认应走 openai 类型集，实为 %v", catalog.lastTypes)
	}
}

// TestModelListShapesFollowClientDialect 覆盖三种响应形状（Node 的 anthropic/gemini/openai）。
func TestModelListShapesFollowClientDialect(t *testing.T) {
	catalog := &fakeModelCatalog{rows: []ModelProvider{
		{ID: 1, Name: "box", Type: "claude", URL: "https://upstream.test",
			AllowedModels: json.RawMessage(`["claude-opus-4"]`)},
	}}
	handler := newModelsTestHandler(t, catalog, jsonTransport(t, nil))
	spec := modelListSpecForTest(t, http.MethodGet, "/v1/models")

	// anthropic 形状：靠 anthropic-version 头判定。
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("anthropic-version", "2023-06-01")
	recorder := httptest.NewRecorder()
	handler.serveModelList(recorder, request, spec.spec, pctx.AuthState{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("anthropic 形状状态码应为 200，实为 %d", recorder.Code)
	}
	payload := decodeModels(t, recorder)
	data, _ := payload["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("anthropic 形状 data 长度应为 1，实为 %d", len(data))
	}
	entry, _ := data[0].(map[string]any)
	if entry["type"] != "model" || entry["display_name"] != "claude-opus-4" || entry["created_at"] == nil {
		t.Fatalf("anthropic 形状字段不符: %v", entry)
	}
	if payload["has_more"] != false {
		t.Fatalf("anthropic 形状应有 has_more=false")
	}
	if strings.Join(catalog.lastTypes, ",") != "claude,claude-auth" {
		t.Fatalf("anthropic 形状应选 claude 类型集，实为 %v", catalog.lastTypes)
	}

	// gemini 形状：靠 x-goog-api-key 头判定。
	catalog.lastTypes = nil
	request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	request.Header.Set("x-goog-api-key", "gemini-key")
	recorder = httptest.NewRecorder()
	geminiSpec := modelListSpecForTest(t, http.MethodGet, "/v1beta/models")
	handler.serveModelList(recorder, request, geminiSpec.spec, pctx.AuthState{})
	payload = decodeModels(t, recorder)
	models, _ := payload["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("gemini 形状 models 长度应为 1，实为 %d", len(models))
	}
	geminiEntry, _ := models[0].(map[string]any)
	if geminiEntry["name"] != "models/claude-opus-4" {
		t.Fatalf("gemini 形状 name 应为 models/<id>，实为 %v", geminiEntry["name"])
	}
	methods, _ := geminiEntry["supportedGenerationMethods"].([]any)
	if len(methods) != 1 || methods[0] != "generateContent" {
		t.Fatalf("gemini 形状 supportedGenerationMethods 不符: %v", methods)
	}
	if strings.Join(catalog.lastTypes, ",") != "gemini,gemini-cli" {
		t.Fatalf("gemini 形状应选 gemini 类型集，实为 %v", catalog.lastTypes)
	}
}

// TestFixedModelListRoutesPinProviderTypes 覆盖三个固定类型集端点（Node 的固定处理器）。
func TestFixedModelListRoutesPinProviderTypes(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v1/responses/models", "codex"},
		{"/v1/chat/completions/models", "openai-compatible"},
		{"/v1/chat/models", "openai-compatible"},
	}
	for _, item := range cases {
		catalog := &fakeModelCatalog{rows: nil}
		handler := newModelsTestHandler(t, catalog, jsonTransport(t, nil))
		spec := modelListSpecForTest(t, http.MethodGet, item.path)
		// 即便带上 anthropic 头也不该改变固定类型集（Node 的固定处理器不读方言）。
		request := httptest.NewRequest(http.MethodGet, item.path, nil)
		request.Header.Set("anthropic-version", "2023-06-01")
		recorder := httptest.NewRecorder()
		handler.serveModelList(recorder, request, spec.spec, pctx.AuthState{})
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s 状态码应为 200，实为 %d", item.path, recorder.Code)
		}
		if strings.Join(catalog.lastTypes, ",") != item.want {
			t.Fatalf("%s 的供应商类型集应为 %q，实为 %v", item.path, item.want, catalog.lastTypes)
		}
		// 固定处理器恒用 OpenAI 形状。
		payload := decodeModels(t, recorder)
		if payload["object"] != "list" {
			t.Fatalf("%s 应恒为 OpenAI 形状（object=list），实为 %v", item.path, payload["object"])
		}
	}
}

// TestClientVersionProbeReturnsEmptyManifest 覆盖 Codex 内置清单探针（ETag + 304）。
func TestClientVersionProbeReturnsEmptyManifest(t *testing.T) {
	catalog := &fakeModelCatalog{}
	handler := newModelsTestHandler(t, catalog, jsonTransport(t, nil))
	spec := modelListSpecForTest(t, http.MethodGet, "/v1/models")

	request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.48.0", nil)
	recorder := httptest.NewRecorder()
	handler.serveModelList(recorder, request, spec.spec, pctx.AuthState{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d", recorder.Code)
	}
	if got := recorder.Header().Get("ETag"); got != codexModelsManifestETag {
		t.Fatalf("ETag 应为 %q，实为 %q", codexModelsManifestETag, got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control 应为 no-cache，实为 %q", got)
	}
	payload := decodeModels(t, recorder)
	models, _ := payload["models"].([]any)
	if len(models) != 0 {
		t.Fatalf("探针应回空清单，实为 %v", models)
	}
	// 探针不应触达供应商目录（Node 在读到 client_version 后直接返回）。
	if catalog.lastTypes != nil {
		t.Fatalf("探针分支不应查询供应商目录，实为 %v", catalog.lastTypes)
	}

	// 带 If-None-Match 时应答 304。
	request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.48.0", nil)
	request.Header.Set("If-None-Match", codexModelsManifestETag)
	recorder = httptest.NewRecorder()
	handler.serveModelList(recorder, request, spec.spec, pctx.AuthState{})
	if recorder.Code != http.StatusNotModified {
		t.Fatalf("ETag 命中应回 304，实为 %d", recorder.Code)
	}
}

// TestProviderFetchFailureIsIsolated 覆盖单供应商失败不影响整体（Node available-models.ts:321-327）。
func TestProviderFetchFailureIsIsolated(t *testing.T) {
	catalog := &fakeModelCatalog{rows: []ModelProvider{
		{ID: 1, Name: "broken", Type: "claude", URL: "https://broken.test"},
		{ID: 2, Name: "healthy", Type: "claude", URL: "https://upstream.test"},
	}}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Host, "broken") {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Status:     "502 Bad Gateway",
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":"upstream down"}`)),
				Request:    request,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"claude-sonnet-4"}]}`)),
			Request:    request,
		}, nil
	})
	handler := newModelsTestHandler(t, catalog, transport)
	spec := modelListSpecForTest(t, http.MethodGet, "/v1/models")
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()
	handler.serveModelList(recorder, request, spec.spec, pctx.AuthState{})

	if recorder.Code != http.StatusOK {
		t.Fatalf("单家失败不应改变状态码，实为 %d", recorder.Code)
	}
	payload := decodeModels(t, recorder)
	data, _ := payload["data"].([]any)
	ids := make([]string, 0, len(data))
	for _, item := range data {
		entry, _ := item.(map[string]any)
		ids = append(ids, entry["id"].(string))
	}
	if strings.Join(ids, ",") != "claude-sonnet-4" {
		t.Fatalf("应保留健康供应商（走上游取数）的模型，实为 %v", ids)
	}
}

// TestCatalogFailureReturns503 覆盖目录读数失败：5xx 而不是空清单。
func TestCatalogFailureReturns503(t *testing.T) {
	catalog := &fakeModelCatalog{err: context.DeadlineExceeded}
	handler := newModelsTestHandler(t, catalog, jsonTransport(t, nil))
	spec := modelListSpecForTest(t, http.MethodGet, "/v1/models")
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()
	handler.serveModelList(recorder, request, spec.spec, pctx.AuthState{})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("目录读失败应为 503，实为 %d", recorder.Code)
	}
}

// TestExactAllowedModelsParsing 覆盖 allowed_models 的 exact 规则提取（Node normalizer 同义）。
func TestExactAllowedModelsParsing(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"字符串等价 exact", `["a","b"]`, "a,b"},
		{"对象 exact", `[{"matchType":"exact","pattern":"a"}]`, "a"},
		{"非 exact 被丢弃", `[{"matchType":"prefix","pattern":"claude-"},{"matchType":"exact","pattern":"x"}]`, "x"},
		{"空 pattern 被丢弃", `[{"matchType":"exact","pattern":"  "}]`, ""},
		{"非法条目被丢弃", `[123,{"matchType":"exact","pattern":"ok"}]`, "ok"},
		{"空值", `null`, ""},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			got := strings.Join(exactAllowedModels(json.RawMessage(item.raw)), ",")
			if got != item.want {
				t.Fatalf("want %q got %q", item.want, got)
			}
		})
	}
}

// TestInferModelOwner 覆盖 owner 推断（Node inferOwner 同义）。
func TestInferModelOwner(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4":  "anthropic",
		"gpt-5":          "openai",
		"o1-preview":     "openai",
		"gemini-2.0-pro": "google",
		"deepseek-v4":    "deepseek",
		"qwen-max":       "alibaba",
		"phi-4":          "unknown",
	}
	for model, want := range cases {
		if got := inferModelOwner(model); got != want {
			t.Fatalf("%s 的 owner 应为 %q，实为 %q", model, want, got)
		}
	}
}
