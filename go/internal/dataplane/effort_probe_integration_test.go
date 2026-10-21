package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIntegrationEffortProbeMatchesUpstreamBody 是「转换后思考强度」口径的**核心证据**：
// 落库的 forwardedEffort 必须等于**假上游实际收到**的 body 里那个字段的值。
//
// 为什么这条必须端到端：这个字段的用途是当**协议转换正确性探针**。若它由记录侧二次推导
// （例如「客户端给 high，那就记 high」），那么转换把值改了、丢了，记录照样显示 high，
// 探针就失去了意义——只有「与上游实收值同源」才证明得了转换。
//
// 本用例构造跨协议：Anthropic 客户端（`/v1/messages` + `output_config.effort`）→
// openai-compatible 供应商（转换到 chat 线，字段名变成 `reasoning_effort`）。
func TestIntegrationEffortProbeMatchesUpstreamBody(t *testing.T) {
	type upstreamCapture struct {
		body map[string]any
	}
	captured := &upstreamCapture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","model":"claude-sonnet-4-5",`+
			`"usage":{"input_tokens":1,"output_tokens":1},"content":[]}`)
	}))
	defer upstream.Close()

	// 跨协议：客户端是 anthropic，供应商是 openai-compatible → 计划里必然发生转换。
	pools := integrationStore(t)
	provisioned := provision(t, pools, upstream.URL, "openai-compatible")
	// 非原生协议对的供应商必须显式打开协议转换开关，否则候选过滤会把它排除掉
	// （见 route/filter.go 的 ReasonProtocolConversionDisabled），请求会落回库里其它 claude 供应商。
	enableProtocolConversion(t, provisioned)
	handler := newIntegrationHandler(t, provisioned)

	requestBody := `{"model":"claude-sonnet-4-5","max_tokens":16,"output_config":{"effort":"high"},` +
		`"messages":[{"role":"user","content":"你好"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", provisioned.apiKey)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}

	// 唯一事实来源：假上游收到的正文。
	upstreamEffort, _ := captured.body["reasoning_effort"].(string)
	if upstreamEffort == "" {
		t.Fatalf("假上游正文里应有 reasoning_effort（跨协议转换的产物），实收：%v", captured.body)
	}

	row := waitForFinalizedRow(t, provisioned)
	entries := specialSettingsOf(t, row)

	clientEntry := findEntry(t, entries, "anthropic_effort")
	if got := clientEntry["effort"]; got != "high" {
		t.Errorf("客户端侧条目应记 output_config.effort=high，实际 %v", got)
	}

	probe := findEntry(t, entries, "thinking_effort_forwarded")
	if got := probe["forwardedEffort"]; got != upstreamEffort {
		t.Errorf("探针的 forwardedEffort 必须等于上游实收值：落库=%v 上游=%q", got, upstreamEffort)
	}
	if got := probe["requestedEffort"]; got != "high" {
		t.Errorf("探针要同时留客户端请求值：实际 %v", got)
	}
	if got := probe["dropped"]; got != false {
		t.Errorf("转换保留了该字段，dropped 应为 false：%v", got)
	}
	if got := probe["converted"]; got != true {
		t.Errorf("跨协议请求应记 converted=true：%v", got)
	}

	// 协议转换审计：跨协议时**必须**有，且协议对取自真实用于构造上游端点的计划。
	// 它是使用记录页「协议转换」列的唯一数据源（`protocol-conversion-display.tsx`）。
	conversion := findEntry(t, entries, "protocol_conversion")
	if got := conversion["clientProtocol"]; got != "anthropic-messages" {
		t.Errorf("客户端协议线应为 anthropic-messages，实际 %v", got)
	}
	if got := conversion["targetProtocol"]; got != "openai-chat" {
		t.Errorf("目标协议线应为 openai-chat，实际 %v", got)
	}
	if got := conversion["hit"]; got != true {
		t.Errorf("命中标记应为 true（未命中的反向标记是反模式）：%v", got)
	}
}

// TestIntegrationEffortProbeOnNativePassthrough 覆盖原生直通：无转换时按**客户端字段路径**
// 读上游正文，且不得把「读不到」误报成 dropped。
func TestIntegrationEffortProbeOnNativePassthrough(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &received)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","model":"claude-sonnet-4-5",`+
			`"usage":{"input_tokens":1,"output_tokens":1},"content":[]}`)
	}))
	defer upstream.Close()

	pools := integrationStore(t)
	// 客户端 anthropic + 供应商 claude：同协议，计划里不转换。
	provisioned := provision(t, pools, upstream.URL, "claude")
	handler := newIntegrationHandler(t, provisioned)

	requestBody := `{"model":"claude-sonnet-4-5","max_tokens":16,"output_config":{"effort":"medium"},` +
		`"messages":[{"role":"user","content":"你好"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", provisioned.apiKey)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}

	nativeEffort, _ := received["output_config"].(map[string]any)
	upstreamEffort, _ := nativeEffort["effort"].(string)
	if upstreamEffort == "" {
		t.Fatalf("原生直通时上游正文应原样保留 output_config.effort，实收：%v", received)
	}

	row := waitForFinalizedRow(t, provisioned)
	probe := findEntry(t, specialSettingsOf(t, row), "thinking_effort_forwarded")
	if got := probe["forwardedEffort"]; got != upstreamEffort {
		t.Errorf("原生直通也要与上游实收值一致：落库=%v 上游=%q", got, upstreamEffort)
	}
	if got := probe["converted"]; got != false {
		t.Errorf("未发生转换应记 converted=false：%v", got)
	}
	if got := probe["dropped"]; got != false {
		t.Errorf("未转换时不得断言丢弃：%v", got)
	}

	// 未施加转换就不写协议转换审计——Node 的同一取舍见 `forwarder.ts:3778` 的注释：
	// 「未转换的请求（conversionPlan 为 null）不写反向标记，否则等于给全部原生请求白写一条」。
	if entry := findEntryOrNil(specialSettingsOf(t, row), "protocol_conversion"); entry != nil {
		t.Errorf("原生直通不应有协议转换审计，实际：%v", entry)
	}
}

// TestIntegrationEffortProbeOnNativePassthroughWithModelRedirect 钉住「两个时点的正文同源」：
// 拨号期交给上游的字节，与结算期探针读到的字节，必须是同一份。
//
// 为什么这条用例必须存在：模型名改写后的出站正文现在是**分段**表示（前缀段 + 目标名段 + 后缀段，
// 均引用入站正文的子切片），请求期**不物化**——没有一整片连续字节可读。结算期的思考强度探针是
// 这份分段正文在 /v1/messages 链路上唯一的结算期消费方（WS 帧正文不走这条链路；缓存 TTL 解析只在
// 正文含 cache_control 时才读）。因此探针读到的值若与上游实收不一致，只有两种可能：分段拼错了，
// 或物化入口压根没被调用（`plan.Body` 在分段计划上恒为 nil，探针会静默读到空值）。两条都不是单测
// 能发现的：单测看不到「拨号期 reader 逐段吐出的字节」与「结算期物化出的字节」是否一致，只有走
// 完整条链才比得出来。
//
// 构造：客户端 anthropic + 供应商 claude（原生直通，不转换）+ 该供应商配了一条
// `claude-sonnet-4-5 → glm-4.6` 的重定向规则——三者同时成立才会走分段改写。
// 该夹具供应商的 anthropic_adaptive_thinking 是 SQL NULL（经 row_to_json 读回字面量 `null`），
// 覆写的零配置早退成立，故计划确实保持分段——这一点由 forward 包的同形单测
// `TestBuildPlanModelRewriteStaysSegmentedForZeroConfigOverrides` 钉住（本包看不到 plan 的内部字段）。
func TestIntegrationEffortProbeOnNativePassthroughWithModelRedirect(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &received)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","model":"glm-4.6",`+
			`"usage":{"input_tokens":1,"output_tokens":1},"content":[]}`)
	}))
	defer upstream.Close()

	pools := integrationStore(t)
	provisioned := provision(t, pools, upstream.URL, "claude")
	const redirectTarget = "glm-4.6"
	setProviderModelRedirects(t, pools, provisioned.providerID,
		fmt.Sprintf(`[{"matchType":"exact","source":%q,"target":%q}]`, "claude-sonnet-4-5", redirectTarget))
	handler := newIntegrationHandler(t, provisioned)

	requestBody := `{"model":"claude-sonnet-4-5","max_tokens":16,"output_config":{"effort":"medium"},` +
		`"messages":[{"role":"user","content":"你好"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", provisioned.apiKey)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}

	// ① 拨号期事实：上游实收的 model 是重定向目标，说明改写确实施加了（且出站是分段 reader 逐段给出的）。
	if got := received["model"]; got != redirectTarget {
		t.Fatalf("上游实收 model 应为重定向目标 %q，实际 %v（正文：%v）", redirectTarget, got, received)
	}
	nativeEffort, _ := received["output_config"].(map[string]any)
	upstreamEffort, _ := nativeEffort["effort"].(string)
	if upstreamEffort == "" {
		t.Fatalf("原生直通时上游正文应原样保留 output_config.effort，实收：%v", received)
	}

	// ② 结算期事实：探针从 MaterializedBody() 拼出的正文里读到的值，必须与①的上游实收值相同。
	// 两者相等即证明拨号期逐段读出的字节与结算期物化出的字节同源——改写没有在两处各算一遍。
	row := waitForFinalizedRow(t, provisioned)
	probe := findEntry(t, specialSettingsOf(t, row), "thinking_effort_forwarded")
	if got := probe["forwardedEffort"]; got != upstreamEffort {
		t.Errorf("分段正文在两个时点必须同源：结算期物化出的 forwardedEffort=%v 拨号期上游实收=%q",
			got, upstreamEffort)
	}
	if got := probe["requestedEffort"]; got != "medium" {
		t.Errorf("探针要同时留客户端请求值：实际 %v", got)
	}
	if got := probe["dropped"]; got != false {
		t.Errorf("重定向只换模型名，effort 仍在正文里，dropped 应为 false：%v", got)
	}
	if got := probe["converted"]; got != false {
		t.Errorf("原生直通（含重定向）不发生协议转换，应记 converted=false：%v", got)
	}
}

// enableProtocolConversion 打开测试供应商的协议转换开关。
func enableProtocolConversion(t *testing.T, provisioned provisioning) {
	t.Helper()
	writer, err := provisioned.pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	if _, err := writer.Exec(context.Background(),
		`UPDATE providers SET protocol_conversion_enabled = true WHERE id = $1`,
		provisioned.providerID); err != nil {
		t.Fatalf("打开协议转换开关失败: %v", err)
	}
}

// specialSettingsOf 解析行上的 special_settings 列（jsonb → []map）。
func specialSettingsOf(t *testing.T, row map[string]any) []map[string]any {
	t.Helper()
	raw, ok := row["special_settings"]
	if !ok || raw == nil {
		t.Fatalf("special_settings 列不应为 NULL：%v", row)
	}
	serialized, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("序列化 special_settings 失败: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(serialized, &entries); err != nil {
		t.Fatalf("special_settings 不是 JSON 数组: %v（%s）", err, string(serialized))
	}
	return entries
}

// findEntry 取指定 type 的条目；找不到即失败（缺失就是缺陷，不是可选）。
func findEntry(t *testing.T, entries []map[string]any, settingType string) map[string]any {
	t.Helper()
	if entry := findEntryOrNil(entries, settingType); entry != nil {
		return entry
	}
	t.Fatalf("special_settings 里应有 %q，实际：%v", settingType, entries)
	return nil
}

// findEntryOrNil 与 findEntry 同义，但缺失时返回 nil——用于「**不得**存在」那类断言。
func findEntryOrNil(entries []map[string]any, settingType string) map[string]any {
	for _, entry := range entries {
		if entry["type"] == settingType {
			return entry
		}
	}
	return nil
}
