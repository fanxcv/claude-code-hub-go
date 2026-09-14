package dataplane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// 本文件守护「协议转换失败」这条审计的**产出**与**互斥**。
//
// 为什么值得单独一组用例：这条审计是**超越 Node parity** 的可观测性增强（Node 对转换失败是静默的），
// 因此没有任何对拍基准能发现它回归——只有本文件的断言。两个失败面都会让用户失去排查手段：
//   - 不产出：使用记录页上「没转换」与「转换失败」长得一样（本任务要修的原状）；
//   - 与成功条目同时产出：界面会同时画「已转换」与「转换失败」，给出互相矛盾的结论。

// decodeEntries 把 append 出来的 JSON 数组解回条目切片（测试用的唯一读取口径）。
func decodeEntries(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	if payload == nil {
		return nil
	}
	var entries []map[string]any
	if err := json.Unmarshal(payload, &entries); err != nil {
		t.Fatalf("审计数组不可解析：%v（原文 %s）", err, payload)
	}
	return entries
}

// TestAppendEntriesRecordsConversionFailureInsteadOfSuccess 钉住失败与成功**互斥且二选一**。
func TestAppendEntriesRecordsConversionFailureInsteadOfSuccess(t *testing.T) {
	// 失败：Conversion 被置回 nil，只剩失败事实（forward.BuildPlan 的产物形态）。
	failed := &forward.Plan{
		Protocol: convert.ProtocolOpenAIChat,
		ConversionFailure: &convert.ConversionFailure{
			ClientProtocol: convert.ProtocolAnthropicMessages,
			TargetProtocol: convert.ProtocolOpenAIChat,
			Phase:          convert.PhaseBodyConversion,
			Reason:         "forward: 请求正文不是合法 JSON 对象: unexpected end of JSON input",
			Fallback:       true,
		},
	}
	entries := decodeEntries(t, specialSettingsAppendEntries(nil, failed))
	failureEntry := findEntryOrNil(entries, "protocol_conversion_failed")
	if failureEntry == nil {
		t.Fatalf("转换失败必须产出失败条目，实际条目：%v", entries)
	}
	if got := findEntryOrNil(entries, "protocol_conversion"); got != nil {
		t.Errorf("失败时不得同时产出成功条目：%v", got)
	}
	if failureEntry["phase"] != string(convert.PhaseBodyConversion) {
		t.Errorf("phase 应为 body_conversion，实际 %v", failureEntry["phase"])
	}
	if failureEntry["clientProtocol"] != "anthropic-messages" || failureEntry["targetProtocol"] != "openai-chat" {
		t.Errorf("协议对应逐字落库，实际 %v / %v", failureEntry["clientProtocol"], failureEntry["targetProtocol"])
	}
	if failureEntry["fallback"] != true {
		t.Errorf("本次确实回退了，fallback 应为 true，实际 %v", failureEntry["fallback"])
	}
	if reason, _ := failureEntry["reason"].(string); reason == "" {
		t.Error("失败原因不得为空")
	}
}

// TestAppendEntriesRecordsConversionSuccessWithoutFailure 钉住成功路径不带失败痕迹。
func TestAppendEntriesRecordsConversionSuccessWithoutFailure(t *testing.T) {
	plan := &forward.Plan{
		Protocol: convert.ProtocolOpenAIChat,
		Body:     []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
		Conversion: &convert.ConversionPlan{
			ClientProtocol: convert.ProtocolAnthropicMessages,
			TargetProtocol: convert.ProtocolOpenAIChat,
		},
	}
	entries := decodeEntries(t, specialSettingsAppendEntries(nil, plan))
	if findEntryOrNil(entries, "protocol_conversion") == nil {
		t.Fatalf("转换成功必须产出成功条目，实际条目：%v", entries)
	}
	if got := findEntryOrNil(entries, "protocol_conversion_failed"); got != nil {
		t.Errorf("成功路径不得产出失败条目：%v", got)
	}
}

// TestAppendEntriesRecordsNothingForNativeRequest 钉住原生同协议请求不留任何转换痕迹。
//
// 反向断言：给每个原生请求白写一条会淹掉真正的失败信号（Node 对成功条目也持同一取舍）。
func TestAppendEntriesRecordsNothingForNativeRequest(t *testing.T) {
	plan := &forward.Plan{
		Protocol: convert.ProtocolAnthropicMessages,
		Body:     []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`),
	}
	if entries := decodeEntries(t, specialSettingsAppendEntries(nil, plan)); len(entries) != 0 {
		t.Fatalf("原生同协议请求不得产出任何转换审计，实际 %v", entries)
	}
}

// TestIntegrationConversionFailureRecordedOnRealRow 真库端到端：转换失败的请求仍完成，且审计可读。
//
// 这条用例是本任务的**主验收**：它同时证明三件事——
//  1. 回退语义未变（请求照常拿到 200，不是 500）；
//  2. 失败事实真的落到了 `special_settings` 列（不是只在内存里）；
//  3. 落库内容可读（协议对 + 阶段 + 原因 + 已回退），足以定位问题。
//
// 触发方式（含一层取证）：客户端 anthropic 方言 + 供应商 openai-compatible（**跨协议**），
// 客户端路径取 `/v1/messages/count_tokens`——它属「跨线**只允许同线**」的原始透传端点，
// 故转换计划成立（协议对拿到了）却**找不到目标协议线的上游路径** → 走 `phase=path_resolution` 的回退。
//
// 为何不用「非法 JSON 正文」触发 `phase=body_conversion`：实测**不可达**——守卫层
// `guard.NewBodyAccessor` 对非法 JSON 直接返回 `ErrBodyUnavailable`，而数据面的 `planFacts`
// 忽略该错误、拿到空体（`HasBody=false`），于是正文转换根本不被尝试，反而会写一条**成功**条目。
// 该不可达性已作为发现记入报告（它解释了为何这个洞长期没被发现）。
func TestIntegrationConversionFailureRecordedOnRealRow(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","model":"claude-sonnet-4-5",`+
			`"usage":{"input_tokens":%d,"output_tokens":%d},"content":[]}`, testInputTokens, testOutputTokens)
	}))
	defer upstream.Close()

	pools := integrationStore(t)
	// 关键：供应商是 openai-compatible，而客户端打的是 anthropic 的 /v1/messages → 需要转换。
	provisioned := provision(t, pools, upstream.URL, "openai-compatible")
	// 跨协议供应商**必须显式开启**协议转换才会成为候选（`protocol_conversion_enabled` 默认为
	// false，未开则被选路层的兼容性过滤排除，请求会故障转移到库里其它夹具供应商）。
	enableProtocolConversion(t, provisioned)
	handler := newIntegrationHandler(t, provisioned)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"你好"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", provisioned.apiKey)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	// ① 回退语义未变：请求照常完成（假上游恒 200），且确实由本夹具的假上游作答。
	if recorder.Code != http.StatusOK {
		t.Fatalf("转换失败应回退而非报错，状态码应为 200，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "msg_1") {
		t.Fatalf("响应应来自本夹具的假上游，收到 %q", recorder.Body.String())
	}

	row := waitForFinalizedRow(t, provisioned)
	entries := specialSettingsOf(t, row)
	failureEntry := findEntryOrNil(entries, "protocol_conversion_failed")
	if failureEntry == nil {
		t.Fatalf("审计里必须有 protocol_conversion_failed，实际 %v", entries)
	}
	if got := findEntryOrNil(entries, "protocol_conversion"); got != nil {
		t.Errorf("失败时不得同时写成功条目：%v", got)
	}
	// ② 落库内容可读：协议对 + 阶段 + 原因 + 是否回退，缺一项就定位不到问题。
	if failureEntry["clientProtocol"] != "anthropic-messages" {
		t.Errorf("clientProtocol 应为 anthropic-messages，实际 %v", failureEntry["clientProtocol"])
	}
	if failureEntry["targetProtocol"] != "openai-chat" {
		t.Errorf("targetProtocol 应为 openai-chat，实际 %v", failureEntry["targetProtocol"])
	}
	if failureEntry["phase"] != string(convert.PhasePathResolution) {
		t.Errorf("phase 应为 path_resolution，实际 %v", failureEntry["phase"])
	}
	if reason, _ := failureEntry["reason"].(string); reason == "" {
		t.Error("reason 不得为空——它就是排查转换问题的入口")
	}
	if failureEntry["fallback"] != true {
		t.Errorf("fallback 应为 true（本次确实回退），实际 %v", failureEntry["fallback"])
	}
}
