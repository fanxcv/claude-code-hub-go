package dataplane

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestResolveEndpointFamilyCoversGapList 钉住「撤 Node 判据台账」里的数据面 32 条。
//
// 台账出处：“ §5（脚本 `scripts/admin-endpoint-gap.mjs`
// 的 `dataNodeOnly`）。这 32 条是 Node 有、Go 无的路径；本表断言它们在族表里的归属，
// 使得将来接通用透传时「哪条走透传、哪条走聚合、哪条根本不属于族」不再靠猜。
//
// 其中 `/v1/chat/models` 是**例外**：它不是族（Node 侧由
// `src/app/v1/_lib/models/available-models.ts:546` 的聚合处理器接住），
// 故期望「未命中族」——这也是它必须单独实现、不能靠透传兜住的原因。
func TestResolveEndpointFamilyCoversGapList(t *testing.T) {
	cases := []struct {
		path     string
		wantID   string // 空串表示期望**不**命中任何族
		whyEmpty string
	}{
		{path: "/v1/assistants", wantID: "openai-assistants"},
		{path: "/v1/audio/speech", wantID: "openai-audio-generation"},
		{path: "/v1/audio/transcriptions", wantID: "openai-audio-transcription"},
		{path: "/v1/audio/translations", wantID: "openai-audio-transcription"},
		{path: "/v1/audio/voice_consents", wantID: "openai-audio-resources"},
		{path: "/v1/audio/voices", wantID: "openai-audio-resources"},
		{path: "/v1/batches", wantID: "openai-batches"},
		{path: "/v1/chat/completions/models", wantID: "openai-chat-completions-resources"},
		{
			path:     "/v1/chat/models",
			wantID:   "",
			whyEmpty: "非族：由 available-models.ts 的聚合处理器接住（B′ 类，须单实现）",
		},
		{path: "/v1/chatkit", wantID: "openai-chatkit"},
		{path: "/v1/completions", wantID: "openai-completions"},
		{path: "/v1/containers", wantID: "openai-containers"},
		{path: "/v1/conversations", wantID: "openai-conversations"},
		{path: "/v1/embeddings", wantID: "openai-embeddings"},
		{path: "/v1/evals", wantID: "openai-evals"},
		{path: "/v1/files", wantID: "openai-files"},
		{path: "/v1/fine_tuning", wantID: "openai-fine-tuning"},
		{path: "/v1/images", wantID: "openai-images"},
		{path: "/v1/messages/count_tokens", wantID: "claude-count-tokens"},
		{path: "/v1/models", wantID: "openai-models"},
		{path: "/v1/moderations", wantID: "openai-moderations"},
		{path: "/v1/realtime", wantID: "openai-realtime-http"},
		{path: "/v1/responses/compact", wantID: "response-compact"},
		{path: "/v1/responses/models", wantID: "response-resources"},
		{path: "/v1/skills", wantID: "openai-skills"},
		{path: "/v1/threads", wantID: "openai-threads"},
		{path: "/v1/uploads", wantID: "openai-uploads"},
		{path: "/v1/vector_stores", wantID: "openai-vector-stores"},
		{path: "/v1/videos", wantID: "openai-videos"},
		{path: "/v1beta/files", wantID: "gemini-files"},
		{path: "/v1beta/models", wantID: "gemini-models-resource"},
	}
	if len(cases) != 31 {
		t.Fatalf("期望覆盖 31 条族可解析路径（32 条台账减去 1 条非族），实得 %d", len(cases))
	}
	for _, tc := range cases {
		family, ok := ResolveEndpointFamily(tc.path)
		if tc.wantID == "" {
			if ok {
				t.Errorf("%s 期望不命中族，实得 %s（%s）", tc.path, family.ID, tc.whyEmpty)
			}
			continue
		}
		if !ok {
			t.Errorf("%s 未命中任何族，期望 %s", tc.path, tc.wantID)
			continue
		}
		if family.ID != tc.wantID {
			t.Errorf("%s 命中族 %s，期望 %s", tc.path, family.ID, tc.wantID)
		}
	}
}

// TestResolveEndpointFamilyOrderSensitivity 钉住「首个命中即返回」的顺序语义。
//
// 若把 response-execution/response-resources 排到 response-compact 之前，
// `/v1/responses/compact` 会被判成普通 responses 路径（该族是原始透传，语义完全不同）。
func TestResolveEndpointFamilyOrderSensitivity(t *testing.T) {
	cases := map[string]string{
		"/v1/responses/compact":       "response-compact",
		"/v1/responses":               "response-execution",
		"/v1/responses/models":        "response-resources",
		"/v1/messages/count_tokens":   "claude-count-tokens",
		"/v1/messages":                "claude-messages",
		"/v1/chat/completions/models": "openai-chat-completions-resources",
		"/v1/chat/completions":        "openai-chat-completions",
		"/v1/completions/anything":    "openai-completions",
	}
	for path, want := range cases {
		family, ok := ResolveEndpointFamily(path)
		if !ok || family.ID != want {
			got := "未命中"
			if ok {
				got = family.ID
			}
			t.Errorf("%s 命中 %s，期望 %s", path, got, want)
		}
	}
}

// TestRawPassthroughFamilies 钉住原始透传族恰好是两条（对齐 Node 的
// `rawPassthroughEndpointPathSet`：count_tokens 与 responses/compact）。
//
// 这两条须走「不转换、不改写」的转发路径；多一条或少一条都会让语义悄悄偏掉。
func TestRawPassthroughFamilies(t *testing.T) {
	got := map[string]bool{}
	for _, family := range KnownEndpointFamilies() {
		if family.RawPassthrough {
			got[family.ID] = true
		}
	}
	want := map[string]bool{"claude-count-tokens": true, "response-compact": true}
	if len(got) != len(want) {
		t.Fatalf("原始透传族应为 %d 条，实得 %d 条：%v", len(want), len(got), got)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("族 %s 应为原始透传", id)
		}
	}
}

// TestNormalizeEndpointPath 钉住归一化：去查询、去尾斜杠（根除外）、转小写。
//
// 若不做归一化，`/v1/Messages?x=1` 与 `/v1/messages/` 会被判成未命中族而整条回退 Node。
func TestNormalizeEndpointPath(t *testing.T) {
	cases := map[string]string{
		"/v1/Messages":         "/v1/messages",
		"/v1/messages/":        "/v1/messages",
		"/v1/messages?x=1":     "/v1/messages",
		"/v1/models/?a=b":      "/v1/models",
		"/":                    "/",
		"/v1beta/Models":       "/v1beta/models",
		"/v1/audio/Voices/":    "/v1/audio/voices",
		"/v1/responses/COMPAC": "/v1/responses/compac",
	}
	for in, want := range cases {
		if got := normalizeEndpointPath(in); got != want {
			t.Errorf("normalizeEndpointPath(%q) = %q，期望 %q", in, got, want)
		}
	}
	if family, ok := ResolveEndpointFamily("/v1/MODELS/?limit=1"); !ok || family.ID != "openai-models" {
		t.Errorf("归一化后应命中 openai-models，实得 ok=%v id=%s", ok, family.ID)
	}
}

// TestPrefixMatchingRespectsSegments 钉住前缀匹配按路径段。
//
// 裸 strings.HasPrefix 会让 `/v1/filesx` 命中 `/v1/files`、`/v1/modelsfoo` 命中 `/v1/models`，
// 把不属于数据面的路径也吞进来（本仓在管理面已踩过同类「静默吞掉」事故）。
func TestPrefixMatchingRespectsSegments(t *testing.T) {
	shouldMiss := []string{
		"/v1/filesx",
		"/v1/modelsfoo",
		"/v1/assistantsX",
		"/v1/definitely-not-real-xyz",
		"/v1/chat/models",
		"/v2/messages",
		"/v1/models/foo:bar", // action 形态只归 Gemini 族，且前缀是 /v1/models/
	}
	for _, path := range shouldMiss {
		if family, ok := ResolveEndpointFamily(path); ok {
			t.Errorf("%s 不应命中任何族，实得 %s", path, family.ID)
		}
	}
	// 反证：同名前缀 + 合法子路径应命中（否则上面的否定断言可能只是因为全都匹配不上）。
	if _, ok := ResolveEndpointFamily("/v1/files/abc/content"); !ok {
		t.Error("/v1/files/abc/content 应命中 openai-files（前缀按段匹配）")
	}
	if family, ok := ResolveEndpointFamily("/v1/models/gpt-4o"); !ok || family.ID != "openai-models" {
		t.Errorf("/v1/models/gpt-4o 应命中 openai-models，实得 ok=%v id=%s", ok, family.ID)
	}
}

// TestGeminiModelActionParsing 钉住 Gemini 的 `{model}:{action}` 解析规则。
//
// 规则（对齐 Node 的 matchGeminiModelAction）：model 不得含 `/` 或 `:`，action 非空不含 `/`
// 且须在允许集合内——这样 `/v1beta/models/a/b:generateContent` 这类多段路径不会被误吞。
func TestGeminiModelActionParsing(t *testing.T) {
	hit := map[string]string{
		"/v1beta/models/gemini-2.0-flash:generateContent":       "gemini-generate-content",
		"/v1beta/models/gemini-2.0-flash:streamGenerateContent": "gemini-stream-generate-content",
		"/v1beta/models/gemini-2.0-flash:countTokens":           "gemini-count-tokens",
		"/v1/publishers/google/models/gemini-2.0:embedContent":  "gemini-embed-content",
		"/v1beta/models/gemini-2.0:predict":                     "gemini-predict",
		"/v1internal/models/gemini-2.5-flash:generateContent":   "gemini-cli-generate-content",
		"/v1/models/gemini-2.0:generateContent":                 "gemini-generate-content",
		"/v1beta/models/gemini-2.0":                             "gemini-models-resource", // 不带 action 即模型资源族（Node 同义）
	}
	for path, want := range hit {
		if family, ok := ResolveEndpointFamily(path); !ok || family.ID != want {
			got := "未命中"
			if ok {
				got = family.ID
			}
			t.Errorf("%s 命中 %s，期望 %s", path, got, want)
		}
	}
	miss := []string{
		"/v1beta/models/gemini-2.0:notAnAction",       // action 不在集合内
		"/v1beta/models/a/b:generateContent",          // 模型名含 `/`
		"/v1beta/models/:generateContent",             // 模型名为空
		"/v1internal/models/gemini-2.5-flash:predict", // CLI 线不含 predict
		"/v1/publishers/google/models/gemini-2.0:predictLongRunning/extra",
	}
	for _, path := range miss {
		if family, ok := ResolveEndpointFamily(path); ok {
			t.Errorf("%s 不应命中族，实得 %s", path, family.ID)
		}
	}
}

// TestFamilyCatalogSyncsWithNodeSource 逐条比对 Node 的族目录：id、顺序、以及 surface 必须一致。
//
// 漂移即红——族表是通用透传的路由依据，两侧不一致会让「Node 会代理的路径」在 Go 侧被误判。
// 与 Node 源文件不在同一 checkout 时（例如只拷了 go/ 的构建环境）跳过。
func TestFamilyCatalogSyncsWithNodeSource(t *testing.T) {
	sourcePath := filepath.Join("..", "..", "..", "src", "app", "v1", "_lib", "proxy", "endpoint-family-catalog.ts")
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Skipf("Node 侧族目录不可读（非本仓完整 checkout？）：%v", err)
	}
	source := string(raw)

	idPattern := regexp.MustCompile(`(?m)^\s*id:\s*"([^"]+)",`)
	surfacePattern := regexp.MustCompile(`(?m)^\s*surface:\s*"([^"]+)",`)
	sourceIDs := idPattern.FindAllStringSubmatch(source, -1)
	sourceSurfaces := surfacePattern.FindAllStringSubmatch(source, -1)
	if len(sourceIDs) == 0 {
		t.Fatal("未能从 Node 源文件抽出任何族 id——解析器与源码形态可能已脱节")
	}

	families := KnownEndpointFamilies()
	if len(families) != len(sourceIDs) {
		t.Fatalf("族数量不一致：Go %d，Node %d", len(families), len(sourceIDs))
	}
	for i, family := range families {
		if family.ID != sourceIDs[i][1] {
			t.Errorf("第 %d 条族 id 不一致：Go=%s Node=%s（顺序也必须一致）", i, family.ID, sourceIDs[i][1])
		}
		if i < len(sourceSurfaces) && string(family.Surface) != sourceSurfaces[i][1] {
			t.Errorf("族 %s 的 surface 不一致：Go=%s Node=%s", family.ID, family.Surface, sourceSurfaces[i][1])
		}
	}

	// 原始透传在 Node 侧是独立集合（endpoint-policy.ts 的 rawPassthroughEndpointPathSet，
	// 引用 V1_ENDPOINT_PATHS 常量而非族 id），故这里只做弱校验：集合名与两个常量仍在即可。
	// 强校验（恰好两条、且是哪两条）由 TestRawPassthroughFamilies 在 Go 侧负责。
	policyRaw, err := os.ReadFile(filepath.Join("..", "..", "..", "src", "app", "v1", "_lib", "proxy", "endpoint-policy.ts"))
	if err != nil {
		t.Skipf("Node 侧端点策略不可读：%v", err)
	}
	policySource := string(policyRaw)
	if !strings.Contains(policySource, "rawPassthroughEndpointPathSet") {
		t.Error("Node 侧 rawPassthroughEndpointPathSet 不见了——原始透传语义可能已改，须复核 Go 侧两条族")
	}
	for _, constName := range []string{"V1_ENDPOINT_PATHS.MESSAGES_COUNT_TOKENS", "V1_ENDPOINT_PATHS.RESPONSES_COMPACT"} {
		if !strings.Contains(policySource, constName) {
			t.Errorf("Node 侧原始透传集合不再包含 %s", constName)
		}
	}
}
