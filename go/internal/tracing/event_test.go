package tracing

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// decodeBatch 解出上报体（断言 Content-Encoding 确实是 gzip）。
func decodeBatch(t *testing.T, payload []byte) batchPayload {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("上报体必须是 gzip: %v", err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	var decoded batchPayload
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("上报体必须是合法 JSON: %v (原文 %s)", err, raw)
	}
	return decoded
}

func int64Ptr(value int64) *int64 { return &value }

func sampleRecord() terminal.TraceRecord {
	started := time.Date(2026, 9, 20, 1, 2, 3, 0, time.UTC)
	return terminal.TraceRecord{
		ID:            1234,
		UserID:        7,
		Name:          "POST /v1/messages",
		StartedAt:     started,
		EndedAt:       started.Add(1500 * time.Millisecond),
		StatusCode:    200,
		Model:         "claude-sonnet-4",
		Usage:         terminal.Usage{InputTokens: int64Ptr(11), OutputTokens: int64Ptr(22)},
		HasCost:       true,
		CostUSD:       "0.012345",
		ProviderChain: []byte(`[{"providerId":9}]`),
		RoutingTrace:  []byte(`{"reason":"prefix_affinity"}`),
	}
}

// trace 事件必须带上按行 id 派生的事件 id、ISO8601 时间与选路留痕（且留痕是 JSON 对象，
// 不是被转义的字符串——字符串会让下游只能看不能查）。
func TestEncodeBatchTraceEventFields(t *testing.T) {
	payload, err := encodeBatch([]terminal.TraceRecord{sampleRecord()})
	if err != nil {
		t.Fatalf("编码不应失败: %v", err)
	}
	batch := decodeBatch(t, payload)
	if len(batch.Batch) != 2 {
		t.Fatalf("一条带成本的记录应产生 trace + generation 两条事件，得到 %d", len(batch.Batch))
	}
	trace := batch.Batch[0]
	if trace.ID != "trace-1234" || trace.Type != "trace-create" {
		t.Fatalf("trace 事件封装不符: %+v", trace)
	}
	if trace.Timestamp != "2026-09-20T01:02:04.5Z" {
		t.Fatalf("timestamp 应为 ISO8601 UTC，得到 %q", trace.Timestamp)
	}
	if trace.Body["id"] != "trace-1234" || trace.Body["name"] != "POST /v1/messages" {
		t.Fatalf("trace body 缺少 id/name: %+v", trace.Body)
	}
	if trace.Body["userId"] != "7" {
		t.Fatalf("userId 应是字符串形状的 7，得到 %v", trace.Body["userId"])
	}
	metadata, ok := trace.Body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata 必须是对象: %+v", trace.Body["metadata"])
	}
	if metadata["status_code"] != float64(200) {
		t.Fatalf("metadata.status_code 不符: %v", metadata["status_code"])
	}
	if metadata["duration_ms"] != float64(1500) {
		t.Fatalf("metadata.duration_ms 应由起止时刻推出 1500，得到 %v", metadata["duration_ms"])
	}
	if metadata["cost_usd"] != "0.012345" {
		t.Fatalf("成本精确串必须保留: %v", metadata["cost_usd"])
	}
	chain, ok := metadata["provider_chain"].([]any)
	if !ok || len(chain) != 1 {
		t.Fatalf("provider_chain 应嵌成 JSON 数组，得到 %T %v", metadata["provider_chain"], metadata["provider_chain"])
	}
	if _, isString := metadata["routing_trace"].(string); isString {
		t.Fatalf("routing_trace 不应被转成字符串: %v", metadata["routing_trace"])
	}
	// 结构性保证：上报体里不得出现正文。
	for _, key := range []string{"input", "output", "prompt", "completion"} {
		if _, exists := trace.Body[key]; exists {
			t.Fatalf("trace body 不得带正文类字段 %q: %+v", key, trace.Body)
		}
	}
}

// generation 事件：usage 双形状（v2 usage + v3 usageDetails）、成本数值与 traceId 关联。
func TestEncodeBatchGenerationUsageAndCost(t *testing.T) {
	record := sampleRecord()
	record.Usage = terminal.Usage{
		InputTokens:              int64Ptr(11),
		OutputTokens:             int64Ptr(22),
		CacheReadInputTokens:     int64Ptr(33),
		CacheCreationInputTokens: int64Ptr(44),
	}
	payload, err := encodeBatch([]terminal.TraceRecord{record})
	if err != nil {
		t.Fatalf("编码不应失败: %v", err)
	}
	batch := decodeBatch(t, payload)
	generation := batch.Batch[1]
	if generation.ID != "generation-1234" || generation.Type != "generation-create" {
		t.Fatalf("generation 事件封装不符: %+v", generation)
	}
	if generation.Body["traceId"] != "trace-1234" {
		t.Fatalf("generation 必须关联 trace: %+v", generation.Body)
	}
	if generation.Body["model"] != "claude-sonnet-4" {
		t.Fatalf("generation.model 不符: %v", generation.Body["model"])
	}
	usage, ok := generation.Body["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage 必须是对象: %+v", generation.Body["usage"])
	}
	if usage["input"] != float64(11) || usage["output"] != float64(22) || usage["total"] != float64(33) {
		t.Fatalf("usage 数值不符: %+v", usage)
	}
	if usage["unit"] != "TOKENS" {
		t.Fatalf("usage.unit 应为 TOKENS: %v", usage["unit"])
	}
	details, ok := generation.Body["usageDetails"].(map[string]any)
	if !ok {
		t.Fatalf("usageDetails 必须是对象: %+v", generation.Body["usageDetails"])
	}
	if details["cache_read_input_tokens"] != float64(33) {
		t.Fatalf("usageDetails 缺缓存读计数: %+v", details)
	}
	if generation.Body["totalCost"] != 0.012345 {
		t.Fatalf("totalCost 应为数值: %v", generation.Body["totalCost"])
	}
	if _, exists := generation.Body["startTime"]; !exists {
		t.Fatalf("generation 必须带 startTime: %+v", generation.Body)
	}
}

// 没有值的计数不写键：把「没有缓存读」写成 0 会让下游的缓存命中率统计失真。
func TestEncodeBatchOmitsAbsentUsageKeys(t *testing.T) {
	record := sampleRecord()
	record.Usage = terminal.Usage{InputTokens: int64Ptr(5)}
	payload, err := encodeBatch([]terminal.TraceRecord{record})
	if err != nil {
		t.Fatalf("编码不应失败: %v", err)
	}
	generation := decodeBatch(t, payload).Batch[1]
	usage := generation.Body["usage"].(map[string]any)
	if _, exists := usage["output"]; exists {
		t.Fatalf("output 缺值时不写键: %+v", usage)
	}
	details := generation.Body["usageDetails"].(map[string]any)
	if _, exists := details["cache_read_input_tokens"]; exists {
		t.Fatalf("缓存读缺值时不写键: %+v", details)
	}
}

// 错误事实补一条 level=ERROR 的事件，且成本串畸形时不发数值字段（发 0 会被读成「免费」）。
func TestEncodeBatchErrorEventAndMalformedCost(t *testing.T) {
	record := sampleRecord()
	record.CostUSD = "not-a-number"
	record.StatusCode = 502
	record.ErrorMessage = "upstream 502"
	payload, err := encodeBatch([]terminal.TraceRecord{record})
	if err != nil {
		t.Fatalf("编码不应失败: %v", err)
	}
	batch := decodeBatch(t, payload)
	if len(batch.Batch) != 3 {
		t.Fatalf("带错误与成本的记录应产生三条事件，得到 %d", len(batch.Batch))
	}
	errorEvent := batch.Batch[2]
	if errorEvent.Type != "event-create" || errorEvent.Body["level"] != levelError {
		t.Fatalf("错误事件封装不符: %+v", errorEvent)
	}
	if errorEvent.Body["statusMessage"] != "upstream 502" {
		t.Fatalf("statusMessage 应带错误文本: %+v", errorEvent.Body)
	}
	if _, exists := batch.Batch[1].Body["totalCost"]; exists {
		t.Fatalf("成本串畸形时不得发数值字段: %+v", batch.Batch[1].Body)
	}
	metadata := batch.Batch[0].Body["metadata"].(map[string]any)
	if metadata["cost_usd"] != "not-a-number" {
		t.Fatalf("精确串仍应保留: %v", metadata["cost_usd"])
	}
}

// 不计费的请求只有 trace 一条；被拦截的请求（无模型）也要能被检索到。
func TestEncodeBatchWithoutCostEmitsTraceOnly(t *testing.T) {
	payload, err := encodeBatch([]terminal.TraceRecord{{
		ID:         9,
		StatusCode: 403,
		BlockedBy:  "sensitive_word",
		StartedAt:  time.Unix(0, 0).UTC(),
		EndedAt:    time.Unix(0, 0).UTC(),
	}})
	if err != nil {
		t.Fatalf("编码不应失败: %v", err)
	}
	batch := decodeBatch(t, payload)
	if len(batch.Batch) != 2 {
		t.Fatalf("不计费但有拦截事实应产生 trace + 错误事件，得到 %d", len(batch.Batch))
	}
	if batch.Batch[1].Body["statusMessage"] != "sensitive_word" {
		t.Fatalf("拦截事实应作为 statusMessage 上报: %+v", batch.Batch[1].Body)
	}
	metadata := batch.Batch[0].Body["metadata"].(map[string]any)
	if metadata["model"] != nil {
		t.Fatalf("无模型时不写 model 键: %+v", metadata)
	}
}

// 事件 id 必须由行 id 派生：同一条事实重复上报要落到同一个事件上（Langfuse 按 id 去重）。
func TestEventIDsAreDeterministic(t *testing.T) {
	if traceEventID(7) != traceEventID(7) || traceEventID(7) == traceEventID(8) {
		t.Fatalf("trace 事件 id 应由行 id 确定性派生")
	}
	if generationEventID(7) == traceEventID(7) {
		t.Fatalf("不同种类的事件 id 不得相同（否则会被去重掉一条）")
	}
}
