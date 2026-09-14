package dataplane

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// TestNonStreamFactsOllamaCodexRepro 用**生产实测抓到的真实响应体**复现：
// 非流式 `/v1/responses`（codex 供应商 → openai-responses 协议）的正文里 usage 明明存在，
// 落库的 input_tokens/output_tokens 却是 NULL。
//
// 夹具来源：对 https://ollama.com/v1/responses 的真实非流式请求（provider 145 "Ollama Codex"，
// 2026-09-13 抓取），请求体镜像用户客户端：
//
//	{"model":"deepseek-v4.1-flash","input":"hi","reasoning":{"effort":"max"},"max_output_tokens":16}
//
// 真实响应形状：**裸 response 对象**（不是 {"response":{...}} 包一层），
// 其中 `usage` 与 `model` 都在**顶层**，`status = "incomplete"`。
//
// 生产对照（近 6 小时）：provider 145 = 700 行 / 695 行 token 为 NULL（99.3%）；
// 同供应商里 token 有值的 4 行全部是 curl 探测（非流式、无 reasoning effort），
// 而 NULL 的行全部来自用户客户端（`pi (darwin)`, 带 `reasoning.effort=max`）。
func TestNonStreamFactsOllamaCodexRepro(t *testing.T) {
	path := filepath.Join("testdata", "ollama-codex-nonstream-responses.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读夹具失败: %v", err)
	}

	plan := &forward.Plan{Protocol: convert.ProtocolOpenAIResponses}
	usage, model := nonStreamFacts(plan, body)

	t.Logf("nonStreamFacts: model=%q usage=%+v", model, usage)
	if model == "" {
		t.Errorf("模型名应被取到（来源行确有 actual_response_model）")
	}
	if usage == nil {
		t.Fatalf("应取到用量（正文顶层 usage = input 31 / output 16 / cached 0），实际为 nil")
	}
	if usage.InputTokens == nil || *usage.InputTokens != 31 {
		t.Errorf("input_tokens 应为 31，实际 %v", usage.InputTokens)
	}
	if usage.OutputTokens == nil || *usage.OutputTokens != 16 {
		t.Errorf("output_tokens 应为 16，实际 %v", usage.OutputTokens)
	}
}
