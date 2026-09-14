package dataplane

import (
	"net/http"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
)

// normalizeForMatch 是入口查表用的归一化（与 ServeHTTP 同一把尺子）。
func normalizeForMatch(path string) string {
	return strings.ToLower(egress.NormalizePath(path))
}

// goDataPlanePathLedger 是「Node-only 32 条」台账。
// 它同时是**回归钉子**：这些路径每一条都必须在 Go 侧有归属（要么族透传、要么聚合处理器），
// 否则撤 Node 之后客户端会拿到 404/502。台账漏一条、族表改坏一条，本表就会红。
var goDataPlanePathLedger = []struct {
	method string
	path   string
	// modelList 为真表示该条属聚合式模型列表（需装配 ModelCatalog 才接管）。
	modelList bool
	// rawPassthrough 为真表示该条属原始透传（count_tokens / responses/compact）。
	rawPassthrough bool
	// format 是族面决定的客户端格式（聚合端点为按请求推断时填其默认面）。
	format convert.ClientFormat
}{
	{http.MethodPost, "/v1/assistants", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/audio/speech", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/audio/transcriptions", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/audio/translations", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/audio/voice_consents", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/audio/voices", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/batches", false, false, convert.FormatOpenAI},
	{http.MethodGet, "/v1/chat/completions/models", true, false, convert.FormatOpenAI},
	{http.MethodGet, "/v1/chat/models", true, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/chatkit", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/completions", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/containers", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/conversations", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/embeddings", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/evals", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/files", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/fine_tuning", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/images", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/messages/count_tokens", false, true, convert.FormatClaude},
	{http.MethodGet, "/v1/models", true, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/moderations", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/realtime", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/responses/compact", false, true, convert.FormatResponse},
	{http.MethodGet, "/v1/responses/models", true, false, convert.FormatResponse},
	{http.MethodPost, "/v1/skills", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/threads", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/uploads", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/vector_stores", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1/videos", false, false, convert.FormatOpenAI},
	{http.MethodPost, "/v1beta/files", false, false, convert.FormatGemini},
	{http.MethodGet, "/v1beta/models", true, false, convert.FormatGemini},
	{http.MethodPost, "/v1beta/models", false, false, convert.FormatGemini},
}

// TestLedgerPathsAreRoutedByGo 断言台账 32 条在 Go 侧都有归属。
func TestLedgerPathsAreRoutedByGo(t *testing.T) {
	for _, entry := range goDataPlanePathLedger {
		t.Run(entry.method+" "+entry.path, func(t *testing.T) {
			modelSpec, isModelList := matchModelListRoute(entry.method, normalizeForMatch(entry.path))
			if entry.modelList {
				if !isModelList {
					t.Fatalf("聚合式模型列表端点未被 matchModelListRoute 命中：撤 Node 后该端点无归属")
				}
				if modelSpec.ModelList == nil {
					t.Fatal("命中的聚合端点缺少 ModelList 规格")
				}
				return
			}
			if isModelList {
				t.Fatal("非聚合端点被 matchModelListRoute 命中：会抢走本该透传或回退的请求")
			}
			spec, ok := matchRoute(entry.method, entry.path)
			if !ok {
				t.Fatalf("族透传未命中：撤 Node 后该端点无归属")
			}
			if spec.ModelList != nil {
				t.Fatal("族透传不应产出 ModelList 规格")
			}
			if spec.RawPassthrough != entry.rawPassthrough {
				t.Fatalf("原始透传标记不符：want %v got %v", entry.rawPassthrough, spec.RawPassthrough)
			}
			if spec.Format != entry.format {
				t.Fatalf("客户端格式不符：want %q got %q", entry.format, spec.Format)
			}
			if spec.Family == "" {
				t.Fatal("协议族为空：pctx.ProtocolFrom 与请求日志的 protocolFrom 会缺")
			}
		})
	}
}

// TestRawPassthroughPathsUseRawPreset 断言两条原始透传端点的守卫预设与不重试语义。
//
// 依据 Node：RAW_PASSTHROUGH_ENDPOINT_POLICY 的 guardPreset=raw_passthrough、
// allowRetry/allowProviderSwitch=false（endpoint-policy.ts:39-52）。
func TestRawPassthroughPathsUseRawPreset(t *testing.T) {
	for _, path := range []string{"/v1/messages/count_tokens", "/v1/responses/compact"} {
		spec, ok := matchRoute(http.MethodPost, path)
		if !ok {
			t.Fatalf("%s 未命中路由", path)
		}
		if !spec.RawPassthrough {
			t.Fatalf("%s 未标记原始透传：会照常重试并切换供应商", path)
		}
		if spec.Policy.Preset != guard.PresetRawPassthrough {
			t.Fatalf("%s 守卫预设应为 raw_passthrough，实为 %q", path, spec.Policy.Preset)
		}
		if !spec.Policy.RawCrossProviderFallback {
			t.Fatalf("%s 的 RawCrossProviderFallback 应为真：Node 缺省落 RAW_SAFE_SESSION_PIPELINE", path)
		}
		if guard.EndpointAllowsRetryAndSwitch(spec.Policy.Preset) {
			t.Fatalf("%s 的预设报告允许重试/切换，与 Node 的 raw 策略相反", path)
		}
	}
}

// TestGenericFamiliesKeepChatPreset 断言普通族路径取 chat 预设且允许重试/切换。
//
// Node 的 DEFAULT_ENDPOINT_POLICY：guardPreset=chat、allowRetry/allowProviderSwitch=true。
func TestGenericFamiliesKeepChatPreset(t *testing.T) {
	for _, path := range []string{"/v1/embeddings", "/v1/files", "/v1/images", "/v1beta/files"} {
		spec, ok := matchRoute(http.MethodPost, path)
		if !ok {
			t.Fatalf("%s 未命中族透传", path)
		}
		if spec.RawPassthrough {
			t.Fatalf("%s 被误判为原始透传", path)
		}
		if spec.Policy.Preset != guard.PresetChat {
			t.Fatalf("%s 预设应为 chat，实为 %q", path, spec.Policy.Preset)
		}
		if !guard.EndpointAllowsRetryAndSwitch(spec.Policy.Preset) {
			t.Fatalf("%s 应允许重试/切换（Node 的 default 策略）", path)
		}
	}
}

// TestFamilyPassthroughAcceptsNonPostMethods 断言族透传不按方法挑拣。
//
// Node 的族目录建在 catch-all（app.all("*")）上，`DELETE /v1/files/{id}`、`GET /v1/batches/{id}`
// 都要代理；若这里只认 POST，这些请求会退回 Node，撤 Node 后直接 404。
func TestFamilyPassthroughAcceptsNonPostMethods(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/files/file-123"},
		{http.MethodDelete, "/v1/files/file-123"},
		{http.MethodGet, "/v1/batches/batch-1"},
		{http.MethodGet, "/v1/models/gpt-4o"},
		{http.MethodGet, "/v1beta/models/gemini-2.0-flash"},
		{http.MethodPost, "/v1beta/models/gemini-2.0-flash:generateContent"},
	}
	for _, item := range cases {
		if _, ok := matchRoute(item.method, item.path); !ok {
			t.Fatalf("%s %s 未命中族透传", item.method, item.path)
		}
	}
}

// TestAggregateModelPathsRequireCatalog 断言聚合端点在**未接线目录**时不进透传。
//
// 这是安全边界：`/v1/models` 若被透传接走，客户端拿到的是上游某一家供应商的清单；
// 宁可回退 Node（当前双跑期的形态），也不能答错。
func TestAggregateModelPathsRequireCatalog(t *testing.T) {
	for _, path := range []string{
		"/v1/models",
		"/v1beta/models",
		"/v1/responses/models",
		"/v1/chat/completions/models",
		"/v1/chat/models",
	} {
		if _, ok := matchRoute(http.MethodGet, path); ok {
			t.Fatalf("GET %s 落入了透传路径：未接线目录时它必须回退 Node", path)
		}
		if _, ok := matchModelListRoute(http.MethodGet, normalizeForMatch(path)); !ok {
			t.Fatalf("GET %s 未命中聚合端点表", path)
		}
		// 同路径的非 GET 方法仍归族透传（Node 把聚合处理器挂在 app.get 上）。
		if path == "/v1beta/models" {
			if _, ok := matchRoute(http.MethodPost, path); !ok {
				t.Fatal("POST /v1beta/models 应走族透传（Node 的 catch-all）")
			}
		}
	}
}

// TestUnknownPathsStayUnrouted 断言伪造路径不进 Go：它们必须继续回退 Node。
func TestUnknownPathsStayUnrouted(t *testing.T) {
	for _, path := range []string{"/v1/definitely-not-real-xyz", "/v1/unknown/deep/path"} {
		if spec, ok := matchRoute(http.MethodPost, path); ok {
			t.Fatalf("%s 被 Go 接管（spec=%+v）：伪造路径应回退 Node", path, spec)
		}
	}
}

// TestMainRoutesKeepTheirSpecs 回归钉子：三条主对话路由的规格不得被族透传改写。
func TestMainRoutesKeepTheirSpecs(t *testing.T) {
	cases := []struct {
		path   string
		format convert.ClientFormat
		family egress.Family
	}{
		{"/v1/messages", convert.FormatClaude, egress.FamilyAnthropicMessages},
		{"/v1/chat/completions", convert.FormatOpenAI, egress.FamilyOpenAIChat},
		{"/v1/responses", convert.FormatResponse, egress.FamilyOpenAIResponses},
	}
	for _, item := range cases {
		spec, ok := matchRoute(http.MethodPost, item.path)
		if !ok {
			t.Fatalf("%s 未命中", item.path)
		}
		if spec.Format != item.format || spec.Family != item.family {
			t.Fatalf("%s 规格被改写：format=%q family=%q", item.path, spec.Format, spec.Family)
		}
		if spec.RawPassthrough {
			t.Fatalf("%s 被误判为原始透传", item.path)
		}
		if spec.Policy.Preset != guard.PresetChat {
			t.Fatalf("%s 预设应为 chat", item.path)
		}
	}
}
