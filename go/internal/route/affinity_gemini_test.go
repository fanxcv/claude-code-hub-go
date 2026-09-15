package route

import (
	"encoding/json"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 黄金值来源：直接运行 Node 上游实现产出，不是手算或按 Go 侧反推。
//
//	bun run /tmp/gemini-golden/gen.ts   （该脚本 import 上游
//	github.com/ding113/claude-code-hub 的 src/app/v1/_lib/proxy/affinity/fingerprint.ts）
//
// 逐字对拍的字段：F_sys 的 fp/prefixBytes、每个会话边界的 depth/fp/prefixBytes。
// 断言的是**同一份语料**在两侧产出同一串指纹——这正是前缀亲和的命中前提。
type geminiGoldenBoundary struct {
	depth       int
	fp          string
	prefixBytes int
}

type geminiGoldenCase struct {
	name      string
	body      string
	format    convert.ClientFormat
	ok        bool
	sysFP     string
	sysPrefix int
	tail      []geminiGoldenBoundary
}

func geminiGoldenCases() []geminiGoldenCase {
	return []geminiGoldenCase{
		{
			name:      "gemini_basic（systemInstruction + functionDeclarations + 四轮 parts）",
			format:    convert.FormatGemini,
			ok:        true,
			sysFP:     "3c4299fe4e962bb7833cd926fabae879",
			sysPrefix: 101,
			tail: []geminiGoldenBoundary{
				{1, "0591d47510789398b156f7f7118bbf7a", 118},
				{2, "8b13c5b63f3210f2e05eb484f6410401", 148},
				{3, "eb9174bf8b015ab414596e21148a4c26", 181},
				{4, "ae2505311d9b8fbe418ad2e80f226858", 205},
			},
			body: `{
				"systemInstruction": {"parts": [{"text": "你是助手"}, {"text": "规则二"}]},
				"tools": [{"functionDeclarations": [{
					"name": "get", "description": "取数",
					"parameters": {"type": "object", "properties": {"id": {"type": "string"}}}
				}]}],
				"contents": [
					{"role": "user", "parts": [{"text": "你好"}]},
					{"role": "model", "parts": [{"functionCall": {"name": "get", "args": {"id": "7"}, "id": "call-1"}}]},
					{"role": "user", "parts": [{"functionResponse": {"name": "get", "response": {"ok": true}, "id": "call-1"}}]},
					{"role": "model", "parts": [{"text": "结果如下"}]}
				]
			}`,
		},
		{
			name:      "gemini_snake_and_media（system_instruction / inline_data / file_data / 蛇形 function_call 走兜底）",
			format:    convert.FormatGemini,
			ok:        true,
			sysFP:     "ddc5d5f37fbf37de9ff9263c9a3b32f9",
			sysPrefix: 19,
			tail: []geminiGoldenBoundary{
				{1, "a367c5fa58d91abd5c10242bb0a6048b", 107},
				{2, "604e1dcfb98c6ff7f24c8cc0333eeeeb", 168},
			},
			body: `{
				"system_instruction": {"parts": [{"text": "snake 系统"}]},
				"contents": [
					{"role": "user", "parts": [
						{"inline_data": {"mime_type": "image/png", "data": "QUJD"}},
						{"file_data": {"mime_type": "application/pdf", "file_uri": "gs://b/x.pdf"}}
					]},
					{"role": "model", "parts": [{"function_call": {"name": "snake", "args": {"a": 1}}}]}
				]
			}`,
		},
		{
			name:      "gemini_inline_no_data（内联件无 data ⇒ 摘要位为空）",
			format:    convert.FormatGemini,
			ok:        true,
			sysFP:     "ffe679bb831c95b67dc17819c63c5090",
			sysPrefix: 1,
			tail:      []geminiGoldenBoundary{{1, "30609a40f67c1c293041ab8cc4e69814", 24}},
			body:      `{"contents": [{"role": "user", "parts": [{"inlineData": {"mimeType": "image/jpeg"}}]}]}`,
		},
		{
			name:      "gemini_unknown_part（text 分支 + 不可归类 part 走兜底且剥顶层 id）",
			format:    convert.FormatGemini,
			ok:        true,
			sysFP:     "ffe679bb831c95b67dc17819c63c5090",
			sysPrefix: 1,
			tail:      []geminiGoldenBoundary{{1, "6378ed1765ee0ba80071568c0afcefff", 69}},
			body:      `{"contents": [{"role": "user", "parts": [{"thought": true, "text": "x"}, {"codeExecutionResult": {"outcome": "OK", "id": "e1"}}]}]}`,
		},
		{
			name:      "gemini_system_only（只有系统段 ⇒ 无会话边界，仍可指纹化）",
			format:    convert.FormatGemini,
			ok:        true,
			sysFP:     "eba656550da8904a29083e805eac4f8d",
			sysPrefix: 19,
			tail:      nil,
			body:      `{"systemInstruction": {"parts": [{"text": "只有系统"}]}, "contents": []}`,
		},
		{
			name:   "gemini_contents_not_array（不可指纹化）",
			format: convert.FormatGemini,
			ok:     false,
			body:   `{"contents": "nope"}`,
		},
		{
			name:      "gemini_bad_tools（无 functionDeclarations 的条目按声明本身对待）",
			format:    convert.FormatGemini,
			ok:        true,
			sysFP:     "9e6a61656f5efd57cd3a897e924791a2",
			sysPrefix: 61,
			tail:      []geminiGoldenBoundary{{1, "b37200c105d64d7bb18209249a4457e7", 78}},
			body: `{
				"tools": [
					{"name": "flat", "description": "非 functionDeclarations 形态"},
					{"functionDeclarations": [{"name": "d1", "parameters": {"type": "object"}}]}
				],
				"contents": [{"role": "user", "parts": [{"text": "工具"}]}]
			}`,
		},
		{
			name:      "gemini_cli_wrapped（正文嵌在 request 里）",
			format:    convert.FormatGeminiCLI,
			ok:        true,
			sysFP:     "3a21002bf27119aa5e239b809a5170f6",
			sysPrefix: 17,
			tail: []geminiGoldenBoundary{
				{1, "1c4f6bc7c2102e461b2b4d4a1244b8fd", 38},
				{2, "1e5001aaeb98494789bc21f43988d4c7", 60},
			},
			body: `{
				"model": "gemini-2.5-pro",
				"request": {
					"systemInstruction": {"parts": [{"text": "cli 系统"}]},
					"contents": [
						{"role": "user", "parts": [{"text": "cli 你好"}]},
						{"role": "model", "parts": [{"text": "cli 回复"}]}
					]
				}
			}`,
		},
		{
			name:      "gemini_cli_unwrapped（无 request 时按 gemini 处理）",
			format:    convert.FormatGeminiCLI,
			ok:        true,
			sysFP:     "ffe679bb831c95b67dc17819c63c5090",
			sysPrefix: 1,
			tail:      []geminiGoldenBoundary{{1, "d63b3ad08fc006cef8fae1af490b3e9c", 18}},
			body:      `{"contents": [{"role": "user", "parts": [{"text": "裸体"}]}]}`,
		},
	}
}

// TestFingerprintGeminiMatchesNodeGolden 断言 gemini / gemini-cli 两线的指纹链与 Node 实现逐字一致。
func TestFingerprintGeminiMatchesNodeGolden(t *testing.T) {
	for _, testCase := range geminiGoldenCases() {
		t.Run(testCase.name, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(testCase.body), &body); err != nil {
				t.Fatalf("语料解析失败: %v", err)
			}
			chain, ok := Fingerprint(body, testCase.format, 8)
			if ok != testCase.ok {
				t.Fatalf("可指纹化 = %v，期望 %v", ok, testCase.ok)
			}
			if !testCase.ok {
				return
			}
			if chain.Sys.FP != testCase.sysFP {
				t.Errorf("F_sys = %s，期望 %s", chain.Sys.FP, testCase.sysFP)
			}
			if chain.Sys.PrefixBytes != testCase.sysPrefix {
				t.Errorf("F_sys prefixBytes = %d，期望 %d", chain.Sys.PrefixBytes, testCase.sysPrefix)
			}
			if len(chain.Tail) != len(testCase.tail) {
				t.Fatalf("会话边界数 = %d，期望 %d", len(chain.Tail), len(testCase.tail))
			}
			for index, want := range testCase.tail {
				got := chain.Tail[index]
				if got.Depth != want.depth || got.FP != want.fp || got.PrefixBytes != want.prefixBytes {
					t.Errorf("边界 %d = {depth:%d fp:%s prefixBytes:%d}，期望 {depth:%d fp:%s prefixBytes:%d}",
						index, got.Depth, got.FP, got.PrefixBytes, want.depth, want.fp, want.prefixBytes)
				}
			}
		})
	}
}

// TestFingerprintGeminiAppendedTurnKeepsPrefix 断言 gemini 会话追加一轮后，旧边界指纹与字节数不变。
//
// 这是前缀亲和能复用的充要条件：新请求的最长未变祖先必须落在上一轮已绑定的指纹上。
func TestFingerprintGeminiAppendedTurnKeepsPrefix(t *testing.T) {
	first := `{"contents": [
		{"role": "user", "parts": [{"text": "第一问"}]},
		{"role": "model", "parts": [{"text": "第一答"}]}
	]}`
	second := `{"contents": [
		{"role": "user", "parts": [{"text": "第一问"}]},
		{"role": "model", "parts": [{"text": "第一答"}]},
		{"role": "user", "parts": [{"text": "第二问"}]}
	]}`
	var firstBody, secondBody map[string]any
	if err := json.Unmarshal([]byte(first), &firstBody); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := json.Unmarshal([]byte(second), &secondBody); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	firstChain, ok := Fingerprint(firstBody, convert.FormatGemini, 8)
	if !ok || len(firstChain.Tail) != 2 {
		t.Fatalf("首轮应产出 2 条边界，实际 %d", len(firstChain.Tail))
	}
	secondChain, ok := Fingerprint(secondBody, convert.FormatGemini, 8)
	if !ok || len(secondChain.Tail) != 3 {
		t.Fatalf("次轮应产出 3 条边界，实际 %d", len(secondChain.Tail))
	}
	if firstChain.Sys.FP != secondChain.Sys.FP {
		t.Errorf("追加轮次不应改动 F_sys：%s → %s", firstChain.Sys.FP, secondChain.Sys.FP)
	}
	for index := range firstChain.Tail {
		if firstChain.Tail[index] != secondChain.Tail[index] {
			t.Errorf("边界 %d 应逐字不变：%+v → %+v", index, firstChain.Tail[index], secondChain.Tail[index])
		}
	}
}

// TestFingerprintGeminiIgnoresToolOrderAndVolatileIds 断言 gemini 工具顺序与易变 id 不进指纹。
//
// 覆盖两种客户端现实：functionDeclarations 顺序可能变；functionCall/functionResponse 的 id 是
// 网关可能重写的易变量（Node 两条工具分支都只取 name + 载荷）。
func TestFingerprintGeminiIgnoresToolOrderAndVolatileIds(t *testing.T) {
	left := `{
		"tools": [{"functionDeclarations": [{"name": "b"}, {"name": "a"}]}],
		"contents": [{"role": "model", "parts": [{"functionCall": {"name": "a", "args": {"x": 1}, "id": "id-1"}}]}]
	}`
	right := `{
		"tools": [{"functionDeclarations": [{"name": "a"}, {"name": "b"}]}],
		"contents": [{"role": "model", "parts": [{"functionCall": {"name": "a", "args": {"x": 1}, "id": "id-2"}}]}]
	}`
	var leftBody, rightBody map[string]any
	if err := json.Unmarshal([]byte(left), &leftBody); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := json.Unmarshal([]byte(right), &rightBody); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	leftChain, leftOK := Fingerprint(leftBody, convert.FormatGemini, 8)
	rightChain, rightOK := Fingerprint(rightBody, convert.FormatGemini, 8)
	if !leftOK || !rightOK {
		t.Fatalf("两侧都应可指纹化（left=%v right=%v）", leftOK, rightOK)
	}
	if leftChain.Sys.FP != rightChain.Sys.FP {
		t.Errorf("工具顺序不应改动 F_sys：%s vs %s", leftChain.Sys.FP, rightChain.Sys.FP)
	}
	if len(leftChain.Tail) != 1 || len(rightChain.Tail) != 1 {
		t.Fatalf("两侧各应有 1 条边界，实际 %d / %d", len(leftChain.Tail), len(rightChain.Tail))
	}
	if leftChain.Tail[0].FP != rightChain.Tail[0].FP {
		t.Errorf("易变 id 不应进指纹：%s vs %s", leftChain.Tail[0].FP, rightChain.Tail[0].FP)
	}
}
