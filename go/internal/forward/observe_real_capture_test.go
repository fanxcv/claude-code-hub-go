package forward

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件用**真实抓取的上游流**校验观测器（夹具见 testdata/README-scale.md）。
//
// 夹具来源：2026-09-13 对生产上游 `https://ollama.com/v1/responses`（provider 145
// `Ollama Codex`，`provider_type=codex`）的真实抓取。该供应商正是生产上用量缺失的
// 主要来源（Go 时代 3665 行里 1655 行无用量）。
//
// 缺夹具时跳过（夹具被清理不应让门禁假红），但**不得静默跳过整组**：
// 只要夹具在，就必须真的把事实抽出来。
func TestObserverRealOllamaCodexCapture(t *testing.T) {
	path := filepath.Join("testdata", "ollama-codex-responses-small.sse")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("缺少真实抓取夹具 %s：%v", path, err)
	}

	observer := NewObserver(ObservationOptions{
		StartedAt: time.Now(),
		Format:    convert.FormatResponse,
	})
	// 真实网络里是 4~8 KiB 的读块；这里按 4 KiB 喂入。
	for offset := 0; offset < len(raw); offset += 4 << 10 {
		end := offset + (4 << 10)
		if end > len(raw) {
			end = len(raw)
		}
		observer.Push(raw[offset:end])
	}
	snapshot := observer.Snapshot()

	if snapshot.Model != "deepseek-v4.1-flash" {
		t.Fatalf("模型名 = %q，期望 deepseek-v4.1-flash", snapshot.Model)
	}
	if snapshot.Usage.InputTokens == nil || *snapshot.Usage.InputTokens != 32 {
		t.Fatalf("usage.input_tokens = %v，期望 32（真实抓取：input 32 / output 16）", snapshot.Usage.InputTokens)
	}
	if snapshot.Usage.OutputTokens == nil || *snapshot.Usage.OutputTokens != 16 {
		t.Fatalf("usage.output_tokens = %v，期望 16", snapshot.Usage.OutputTokens)
	}
	// 这个夹具的流以 response.incomplete 结尾（触顶 max_output_tokens），
	// 不是 completed——观测器应如实记录「见到 incomplete」而不是假装完成。
	if !snapshot.SawIncomplete {
		t.Fatal("应记录见到 response.incomplete（该夹具以触顶结束）")
	}
	if snapshot.BufferOverflow {
		t.Fatal("真实抓取的流不应被判成缓冲溢出（该流最长行仅约 1.1 KiB）")
	}
}
