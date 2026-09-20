package tracing

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 事件类型（Langfuse ingestion 的 type 取值）。
const (
	eventTypeTrace      = "trace-create"
	eventTypeGeneration = "generation-create"
	levelError          = "ERROR"
)

// batchPayload 是 ingestion 的请求体：一次请求打包多条事件。
type batchPayload struct {
	Batch []ingestionEvent `json:"batch"`
}

// ingestionEvent 是单条事件的信封（type + timestamp + body）。
type ingestionEvent struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Timestamp string         `json:"timestamp"`
	Body      map[string]any `json:"body"`
}

// 事件 id 是**确定性**的（按行 id 派生）。理由：不引入 uuid 生成，且 Langfuse 按事件 id
// 去重——同一条事实被重发（例如收尾 flush 与常规 flush 撞车）不会被算成两条。
func traceEventID(id int64) string      { return "trace-" + strconv.FormatInt(id, 10) }
func generationEventID(id int64) string { return "generation-" + strconv.FormatInt(id, 10) }
func errorEventID(id int64) string      { return "error-" + strconv.FormatInt(id, 10) }

// encodeBatch 把一批终态事实编成可发送的请求体（已 gzip）。
func encodeBatch(records []terminal.TraceRecord) ([]byte, error) {
	events := make([]ingestionEvent, 0, len(records)*2)
	for _, record := range records {
		events = append(events, traceEvent(record))
		if generation, ok := generationEvent(record); ok {
			events = append(events, generation)
		}
		if errorEvent, ok := recordErrorEvent(record); ok {
			events = append(events, errorEvent)
		}
	}
	raw, err := json.Marshal(batchPayload{Batch: events})
	if err != nil {
		return nil, err
	}
	return gzipPayload(raw)
}

// traceEvent 是每次终态必有的一条（一次请求 = 一条 trace）。
func traceEvent(record terminal.TraceRecord) ingestionEvent {
	return ingestionEvent{
		ID:        traceEventID(record.ID),
		Type:      eventTypeTrace,
		Timestamp: isoTime(record.EndedAt),
		Body: map[string]any{
			"id":        traceEventID(record.ID),
			"timestamp": isoTime(record.EndedAt),
			"name":      record.Name,
			"userId":    strconv.FormatInt(record.UserID, 10),
			"metadata":  traceMetadata(record),
		},
	}
}

// traceMetadata 是 trace 上的可选事实。空值一律不写键——Langfuse 的 metadata 是自由对象，
// 写一串空串只会让下游查询难以区分「没有」与「是空」。
func traceMetadata(record terminal.TraceRecord) map[string]any {
	metadata := map[string]any{"status_code": record.StatusCode}
	if record.Model != "" {
		metadata["model"] = record.Model
	}
	if record.ErrorMessage != "" {
		metadata["error_message"] = record.ErrorMessage
	}
	if record.BlockedBy != "" {
		metadata["blocked_by"] = record.BlockedBy
	}
	if record.HasCost {
		// 十进制串原样保留：Langfuse 侧的数值字段是浮点，账务口径要看精确值。
		metadata["cost_usd"] = record.CostUSD
	}
	if duration := record.EndedAt.Sub(record.StartedAt); duration > 0 {
		metadata["duration_ms"] = duration.Milliseconds()
	}
	if chain := rawJSON(record.ProviderChain); chain != nil {
		metadata["provider_chain"] = chain
	}
	if trace := rawJSON(record.RoutingTrace); trace != nil {
		metadata["routing_trace"] = trace
	}
	return metadata
}

// generationEvent 只在本次请求产生了金额时发（有成本即有模型用量）。
//
// usage / totalCost 是 v2 形状，usageDetails / costDetails 是 v3 形状，两者同时给出：
// 本环境无法对真收集器核验版本，给两份比赌一份安全（见 doc.go 的未核验项）。
func generationEvent(record terminal.TraceRecord) (ingestionEvent, bool) {
	if !record.HasCost {
		return ingestionEvent{}, false
	}
	body := map[string]any{
		"id":        generationEventID(record.ID),
		"traceId":   traceEventID(record.ID),
		"name":      generationName(record),
		"startTime": isoTime(record.StartedAt),
		"endTime":   isoTime(record.EndedAt),
	}
	if record.Model != "" {
		body["model"] = record.Model
	}
	usage, details := usageFields(record.Usage)
	if len(usage) > 0 {
		body["usage"] = usage
	}
	if len(details) > 0 {
		body["usageDetails"] = details
	}
	if cost, ok := costValue(record.CostUSD); ok {
		body["totalCost"] = cost
		body["costDetails"] = map[string]any{"total": cost}
	}
	return ingestionEvent{
		ID:        generationEventID(record.ID),
		Type:      eventTypeGeneration,
		Timestamp: isoTime(record.EndedAt),
		Body:      body,
	}, true
}

func generationName(record terminal.TraceRecord) string {
	if record.Model != "" {
		return record.Model
	}
	return "generation"
}

// usageFields 产出 v2 的 usage（input/output/total/unit）与 v3 的 usageDetails（按类目）。
//
// 计数是**指针**（nil 表示该项没有值），故 nil 不写键：把「没有缓存读」写成 0 会让
// 下游的缓存命中率统计失真。
func usageFields(usage terminal.Usage) (map[string]any, map[string]any) {
	legacy := map[string]any{}
	details := map[string]any{}
	var total int64
	if tokenTotal(&total, usage.InputTokens, details, "input") {
		legacy["input"] = *usage.InputTokens
	}
	if tokenTotal(&total, usage.OutputTokens, details, "output") {
		legacy["output"] = *usage.OutputTokens
	}
	addDetail(details, "cache_creation_input_tokens", usage.CacheCreationInputTokens)
	addDetail(details, "cache_creation_5m_input_tokens", usage.CacheCreation5mInputTokens)
	addDetail(details, "cache_creation_1h_input_tokens", usage.CacheCreation1hInputTokens)
	addDetail(details, "cache_read_input_tokens", usage.CacheReadInputTokens)
	addDetail(details, "input_image_tokens", usage.InputImageTokens)
	addDetail(details, "output_image_tokens", usage.OutputImageTokens)
	if total > 0 {
		legacy["total"] = total
		legacy["unit"] = "TOKENS"
	}
	return legacy, details
}

// tokenTotal 把一项主计数累进 total 并写入 details，返回该项是否有值。
func tokenTotal(total *int64, value *int64, details map[string]any, key string) bool {
	if value == nil {
		return false
	}
	*total += *value
	details[key] = *value
	return true
}

// addDetail 写入可选的拆项计数。0 也照写：拆项与主计数不同，「显式给了 0」是有效事实。
func addDetail(details map[string]any, key string, value *int64) {
	if value == nil {
		return
	}
	details[key] = *value
}

// recordErrorEvent 在本次终态带错误事实时补一条 level=ERROR 的事件。
//
// 失败请求也要能被检索到：trace 的 metadata 里有 status_code，但「按 level 过滤错误」
// 是 Langfuse 上最常用的视图，值得多一条事件。
func recordErrorEvent(record terminal.TraceRecord) (ingestionEvent, bool) {
	message := record.ErrorMessage
	if message == "" {
		message = record.BlockedBy
	}
	if message == "" {
		return ingestionEvent{}, false
	}
	metadata := map[string]any{"status_code": record.StatusCode}
	if record.BlockedBy != "" {
		metadata["blocked_by"] = record.BlockedBy
	}
	return ingestionEvent{
		ID:        errorEventID(record.ID),
		Type:      "event-create",
		Timestamp: isoTime(record.EndedAt),
		Body: map[string]any{
			"id":            errorEventID(record.ID),
			"traceId":       traceEventID(record.ID),
			"name":          "terminal",
			"startTime":     isoTime(record.EndedAt),
			"level":         levelError,
			"statusMessage": message,
			"metadata":      metadata,
		},
	}, true
}

// costValue 把十进制成本串转成数值。转不动时不发数值字段（精确串仍在 trace 的 metadata 里），
// 而不是发一个 0——0 会被读成「这次请求免费」。
func costValue(costUSD string) (float64, bool) {
	if costUSD == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(costUSD, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// rawJSON 把 jsonb 原文嵌成 JSON 值而不是字符串（字符串会让下游只能看、不能查）。
// 非 JSON 或空值退回 nil（调用方据此不写键）。
func rawJSON(raw []byte) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return string(trimmed)
	}
	return decoded
}

// isoTime 是 Langfuse 的时间形状（ISO8601 / RFC3339，纳秒精度、UTC）。
func isoTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
