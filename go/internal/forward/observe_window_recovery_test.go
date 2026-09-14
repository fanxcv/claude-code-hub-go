package forward

import (
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// TestObserverWindowRecoverySetsUsageSeen 钉住一处**曾造成生产事故**的不变量：
// 头尾窗口回退补回用量时，必须同时把 UsageSeen 置真。
//
// 事故（2026-09-13）：上游把整段回答作为**一条超长行**送来（实测 ollama.com 的长回答/推理），
// 分帧器触发缓冲上限并整体丢弃该帧，观测器转由头尾窗口回退补回 model 与 usage。
// 回退把 `snapshot.Usage` 填对了，却忘了置 `UsageSeen`；而结算层的判据正是 `UsageSeen`
// （`dataplane` 只在它为真时写用量列）→ **回退拿回的用量被整包丢弃**。
//
// 生产签名：模型名有值（来自首帧的正常解析）、input_tokens/output_tokens/cost_usd 全空。
// 实测该供应商近 6 小时 700 行里 695 行如此（99.3%），而 Node 时代同供应商同模型
// 每小时仅 0~5 行缺用量。修复即本用例断言的那一行。
func TestObserverWindowRecoverySetsUsageSeen(t *testing.T) {
	// 300 KiB 的单行：远超分帧器缓冲上限（max(HeadBytes, 64 KiB)），必然 parserLost。
	huge := strings.Repeat("x", 300<<10)
	terminal := `{"response":{"id":"resp_huge","object":"response","status":"completed",` +
		`"model":"deepseek-v4.1-flash","output":[{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"` + huge + `"}]}],` +
		`"usage":{"input_tokens":31,"output_tokens":16,"total_tokens":47,` +
		`"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}},` +
		`"sequence_number":9,"type":"response.completed"}`
	stream := "event: response.created\ndata: {\"response\":{\"id\":\"resp_huge\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"deepseek-v4.1-flash\"},\"sequence_number\":0,\"type\":\"response.created\"}\n\n" +
		"event: response.completed\ndata: " + terminal + "\n\n"

	observer := NewObserver(ObservationOptions{StartedAt: time.Now(), Format: convert.FormatResponse})
	payload := []byte(stream)
	for cursor := 0; cursor < len(payload); cursor += 64 << 10 {
		end := cursor + (64 << 10)
		if end > len(payload) {
			end = len(payload)
		}
		observer.Push(payload[cursor:end])
	}
	if !observer.parserLost {
		t.Fatalf("夹具未触发分帧器丢帧（parserLost=false）：本用例已失去覆盖意义，请调整超长行尺寸")
	}

	snapshot := observer.Snapshot()
	if !snapshot.UsageSeen {
		t.Fatalf("窗口回退补回了用量却没置 UsageSeen：结算层会整包丢弃它（生产事故签名）")
	}
	if snapshot.Usage.InputTokens == nil || *snapshot.Usage.InputTokens != 31 {
		t.Errorf("input_tokens 应为 31，实际 %v", snapshot.Usage.InputTokens)
	}
	if snapshot.Usage.OutputTokens == nil || *snapshot.Usage.OutputTokens != 16 {
		t.Errorf("output_tokens 应为 16，实际 %v", snapshot.Usage.OutputTokens)
	}
	if snapshot.Model != "deepseek-v4.1-flash" {
		t.Errorf("模型名应为 deepseek-v4.1-flash，实际 %q", snapshot.Model)
	}
}
