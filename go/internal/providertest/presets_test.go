package providertest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestPresetPayloadCopiesMatchNodeSource 是漂移钉子：本包的载荷副本必须与 Node 侧
// `src/lib/provider-testing/data/*.json` 逐字节一致。
//
// 为什么复制而不是单一真源：`go:embed` 不能引用模块目录之外的文件（`go/internal/providertest`
// 无法嵌入 `src/lib/...`）。故副本 + 钉子：漂移必红，同步靠人工。
func TestPresetPayloadCopiesMatchNodeSource(t *testing.T) {
	nodeDir := filepath.Join("..", "..", "..", "src", "lib", "provider-testing", "data")
	entries, err := os.ReadDir(filepath.Join("data"))
	if err != nil {
		t.Fatalf("读本方 data 目录失败: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("本方 data 目录为空：至少应有一个载荷")
	}
	compared := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		local, err := os.ReadFile(filepath.Join("data", name))
		if err != nil {
			t.Fatalf("读本方载荷 %s 失败: %v", name, err)
		}
		source, err := os.ReadFile(filepath.Join(nodeDir, name))
		if err != nil {
			t.Fatalf("读 Node 侧载荷 %s 失败（路径 %s）: %v", name, nodeDir, err)
		}
		if !bytes.Equal(local, source) {
			t.Errorf("载荷 %s 与 Node 源不一致（%d vs %d 字节）——请人工同步副本", name, len(local), len(source))
		}
		compared++
	}
	if compared < 9 {
		t.Errorf("期望比对 9 个载荷（presets.ts 引用的全部），实际 %d", compared)
	}
}

// TestPresetMappingMatchesNodeSource 钉住「类型 → 预设 id 列表」的顺序与内容。
func TestPresetMappingMatchesNodeSource(t *testing.T) {
	cases := []struct {
		providerType ProviderType
		want         []string
	}{
		{TypeClaude, []string{"cc_haiku_basic", "cc_beta_cli", "cc_public_thinking"}},
		{TypeClaudeAuth, []string{"cc_haiku_basic", "cc_beta_cli", "cc_public_thinking"}},
		{TypeCodex, []string{"cx_codex_basic", "cx_gpt_basic"}},
		{TypeOpenAICompatible, []string{"oa_chat_basic", "oa_chat_stream"}},
		{TypeGemini, []string{"gm_flash_basic", "gm_pro_basic"}},
		{TypeGeminiCLI, []string{"gm_flash_basic", "gm_pro_basic"}},
	}
	for _, testCase := range cases {
		got := PresetIDsForTestPresetsEndpoint(testCase.providerType)
		if len(got) != len(testCase.want) {
			t.Fatalf("%s: 预设数应为 %d，实际 %v", testCase.providerType, len(testCase.want), got)
		}
		for i := range got {
			if got[i] != testCase.want[i] {
				t.Errorf("%s[%d] 应为 %s，实际 %s", testCase.providerType, i, testCase.want[i], got[i])
			}
		}
	}
}

// TestPresetMetadataMatchesNodeSource 抽查预设元数据（默认模型/成功串/路径/UA），
// 这些字段会直接进响应或请求，抄错即行为错。
func TestPresetMetadataMatchesNodeSource(t *testing.T) {
	cases := []struct {
		id             string
		defaultModel   string
		successContain string
		path           string
		userAgent      string
	}{
		{"cc_haiku_basic", "claude-haiku-4-5-20251001", "pong", "/v1/messages", "claude-cli/2.1.84 (external, cli)"},
		{"cc_beta_cli", "claude-haiku-4-5-20251001", "pong", "/v1/messages?beta=true", "claude-cli/2.1.84 (external, cli)"},
		{"cc_public_thinking", "claude-sonnet-4-5-20250929", "pong", "/v1/messages", "claude-cli/2.1.76 (external, cli)"},
		{"cx_codex_basic", "gpt-5.5", "pong", "/v1/responses", "Codex-CLI/1.0"},
		{"cx_gpt_basic", "gpt-5.5", "pong", "/v1/responses", "Codex-CLI/1.0"},
		{"oa_chat_basic", "gpt-4.1-mini", "pong", "/v1/chat/completions", "OpenAI-Compatible/2026.04"},
		{"oa_chat_stream", "gpt-4.1-mini", "pong", "/v1/chat/completions", "OpenAI-Compatible/2026.04"},
		{"gm_flash_basic", "gemini-2.5-flash", "pong", "/v1beta/models/{model}:generateContent", "GeminiCLI/v24.11.0 (linux; x64)"},
		{"gm_pro_basic", "gemini-2.5-pro", "pong", "/v1beta/models/{model}:generateContent", "GeminiCLI/v24.11.0 (linux; x64)"},
	}
	for _, testCase := range cases {
		preset, ok := GetPreset(testCase.id)
		if !ok {
			t.Fatalf("预设 %s 不存在", testCase.id)
		}
		if preset.DefaultModel != testCase.defaultModel {
			t.Errorf("%s defaultModel 应为 %s，实际 %s", testCase.id, testCase.defaultModel, preset.DefaultModel)
		}
		if preset.DefaultSuccessContains != testCase.successContain {
			t.Errorf("%s defaultSuccessContains 应为 %s，实际 %s", testCase.id, testCase.successContain, preset.DefaultSuccessContains)
		}
		if preset.Path != testCase.path {
			t.Errorf("%s path 应为 %s，实际 %s", testCase.id, testCase.path, preset.Path)
		}
		if preset.UserAgent != testCase.userAgent {
			t.Errorf("%s userAgent 应为 %s，实际 %s", testCase.id, testCase.userAgent, preset.UserAgent)
		}
	}
}

// TestPresetPayloadModelOverride 钉住 getPresetPayload 的「仅当模板有 model 键才覆写」语义
// （Gemini 模板没有 model 键，故传 model 也不该塞进去）。
func TestPresetPayloadModelOverride(t *testing.T) {
	payload, err := PresetPayload("cc_haiku_basic", "claude-sonnet-4-5-20250929")
	if err != nil {
		t.Fatalf("取载荷失败: %v", err)
	}
	if got := payload["model"]; got != "claude-sonnet-4-5-20250929" {
		t.Errorf("claude 载荷应覆写 model，实际 %v", got)
	}

	gemini, err := PresetPayload("gm_flash_basic", "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("取载荷失败: %v", err)
	}
	if _, has := gemini["model"]; has {
		t.Error("Gemini 载荷不应被塞入 model（走 URL path）")
	}
	if _, has := gemini["contents"]; !has {
		t.Error("Gemini 载荷应保留 contents")
	}

	// 深拷贝：改返回值不得污染预设本体。
	payload["stream"] = false
	fresh, _ := PresetPayload("cc_haiku_basic", "")
	if fresh["stream"] != true {
		t.Errorf("载荷必须是深拷贝（预设本体不应被改动），实际 stream=%v", fresh["stream"])
	}
}

// TestExecutionPresetCandidatesScoring 钉住打分与稳定排序：
// 命中 modelHints +50、urlHints +30、codex/gemini 专属 boost +40（且命中 hint 后不叠加）。
func TestExecutionPresetCandidatesScoring(t *testing.T) {
	claudeHaiku := ExecutionPresetCandidates(TypeClaude, "https://api.anthropic.com", "claude-haiku-4-5-20251001")
	if claudeHaiku[0].ID != "cc_haiku_basic" {
		t.Errorf("haiku 模型应首选 cc_haiku_basic，实际 %s", claudeHaiku[0].ID)
	}

	claudeSonnet := ExecutionPresetCandidates(TypeClaude, "https://api.anthropic.com", "claude-sonnet-4-5-20250929")
	if claudeSonnet[0].ID != "cc_public_thinking" {
		t.Errorf("sonnet 模型应首选 cc_public_thinking，实际 %s", claudeSonnet[0].ID)
	}

	// urlHints 命中 beta/relay 时 cc_beta_cli 应升到首位（100+? 不含 modelHints）。
	betaURL := ExecutionPresetCandidates(TypeClaude, "https://relay.example.com/v1?beta=true", "")
	if betaURL[0].ID != "cc_beta_cli" {
		t.Errorf("relay+beta 的 URL 应首选 cc_beta_cli，实际 %s", betaURL[0].ID)
	}

	codex := ExecutionPresetCandidates(TypeCodex, "https://api.openai.com", "gpt-5.5-codex")
	if codex[0].ID != "cx_codex_basic" {
		t.Errorf("含 codex 的模型应首选 cx_codex_basic，实际 %s", codex[0].ID)
	}
	gpt := ExecutionPresetCandidates(TypeCodex, "https://api.openai.com", "gpt-5.5")
	if gpt[0].ID != "cx_gpt_basic" {
		t.Errorf("非 codex 模型应首选 cx_gpt_basic，实际 %s", gpt[0].ID)
	}

	flash := ExecutionPresetCandidates(TypeGemini, "https://generativelanguage.googleapis.com", "gemini-2.5-flash")
	if flash[0].ID != "gm_flash_basic" {
		t.Errorf("flash 模型应首选 gm_flash_basic，实际 %s", flash[0].ID)
	}
	pro := ExecutionPresetCandidates(TypeGemini, "https://generativelanguage.googleapis.com", "gemini-2.5-pro")
	if pro[0].ID != "gm_pro_basic" {
		t.Errorf("pro 模型应首选 gm_pro_basic，实际 %s", pro[0].ID)
	}

	// 无 model 时：urlHints 不命中 → 按 score 降序（cx_codex_basic 100 > cx_gpt_basic 85）。
	noModel := ExecutionPresetCandidates(TypeCodex, "https://api.openai.com", "")
	if noModel[0].ID != "cx_codex_basic" {
		t.Errorf("无 model 时应按 score 降序，实际首位 %s", noModel[0].ID)
	}
}

// TestIsPresetCompatible 与 DefaultPreset。
func TestIsPresetCompatible(t *testing.T) {
	if !IsPresetCompatible("cc_haiku_basic", TypeClaude) {
		t.Error("cc_haiku_basic 应兼容 claude")
	}
	if IsPresetCompatible("cc_haiku_basic", TypeCodex) {
		t.Error("cc_haiku_basic 不应兼容 codex")
	}
	if _, ok := DefaultPreset(TypeOpenAICompatible); !ok {
		t.Error("openai-compatible 应有默认预设")
	}
	if _, ok := PresetPayload("nope", ""); ok == nil {
		t.Error("不存在的预设应返回错误")
	}
}
