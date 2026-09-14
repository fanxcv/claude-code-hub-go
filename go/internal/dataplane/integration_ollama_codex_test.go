package dataplane

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件的夹具来自**生产实测抓取的真实字节**，不是自造形状：
//
//	上游：https://ollama.com/v1/responses（生产 provider 145 "Ollama Codex"，provider_type=codex）
//	请求：{"model":"deepseek-v4.1-flash","input":"hi","reasoning":{"effort":"max"},"max_output_tokens":16}
//
// 生产现象（2026-09-13 近 6 小时）：该供应商 700 行里 **695 行 input/output_tokens 为 NULL**，
// 而同一供应商里 token 有值的 4 行全部是 `curl` 探测（不带 reasoning），
// 无 token 的行全部来自用户客户端（`pi (darwin)`，带 `reasoning.effort=max`）。
//
// 与既有夹具的差别（既有 `streamFramesFor` 用的是**想象的**形状：`response.completed` 包一层
// `response.usage`）：真实上游的终态事件是 **`response.incomplete`**（reason=max_output_tokens），
// usage 挂在**同一事件的 `response.usage`** 上；非流式则返回**裸 response 对象**（顶层 usage）。

// ollamaCodexStreamBody 是真实 SSE 的复刻：事件名/字段名/顺序与抓取一致，
// 只把模型生成的文本替换为占位（形状不变）。
const ollamaCodexStreamBody = "event: response.created\ndata: {\"response\":{\"id\":\"resp_repro\",\"object\":\"response\",\"created_at\":1789265154,\"completed_at\":null,\"status\":\"in_progress\",\"incomplete_details\":null,\"model\":\"deepseek-v4.1-flash\",\"previous_response_id\":null,\"instructions\":null,\"output\":[],\"error\":null,\"tools\":[],\"tool_choice\":\"auto\",\"truncation\":\"disabled\",\"parallel_tool_calls\":true,\"text\":{\"format\":{\"type\":\"text\"}},\"top_p\":1,\"presence_penalty\":0,\"frequency_penalty\":0,\"top_logprobs\":0,\"temperature\":1,\"reasoning\":{\"effort\":\"max\"},\"usage\":null,\"max_output_tokens\":16,\"max_tool_calls\":null,\"store\":false,\"background\":false,\"service_tier\":\"default\",\"metadata\":{},\"safety_identifier\":null,\"prompt_cache_key\":null},\"sequence_number\":0,\"type\":\"response.created\"}\n\n" +
	"event: response.in_progress\ndata: {\"response\":{\"id\":\"resp_repro\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"deepseek-v4.1-flash\",\"output\":[],\"reasoning\":{\"effort\":\"max\"},\"usage\":null},\"sequence_number\":1,\"type\":\"response.in_progress\"}\n\n" +
	"event: response.output_item.added\ndata: {\"item\":{\"id\":\"rs_repro\",\"type\":\"reasoning\",\"summary\":[]},\"output_index\":0,\"sequence_number\":2,\"type\":\"response.output_item.added\"}\n\n" +
	"event: response.reasoning_summary_text.delta\ndata: {\"delta\":\"think\",\"item_id\":\"rs_repro\",\"output_index\":0,\"sequence_number\":3,\"summary_index\":0,\"type\":\"response.reasoning_summary_text.delta\"}\n\n" +
	"event: response.reasoning_summary_text.done\ndata: {\"item_id\":\"rs_repro\",\"output_index\":0,\"summary_index\":0,\"text\":\"think\",\"sequence_number\":8,\"type\":\"response.reasoning_summary_text.done\"}\n\n" +
	"event: response.output_item.done\ndata: {\"item\":{\"id\":\"rs_repro\",\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"think\"}]},\"output_index\":0,\"sequence_number\":9,\"type\":\"response.output_item.done\"}\n\n" +
	"event: response.incomplete\ndata: {\"response\":{\"id\":\"resp_repro\",\"object\":\"response\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"model\":\"deepseek-v4.1-flash\",\"output\":[{\"id\":\"rs_repro\",\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"think\"}],\"encrypted_content\":\"think\"}],\"reasoning\":{\"effort\":\"max\"},\"usage\":{\"input_tokens\":31,\"output_tokens\":16,\"total_tokens\":47,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens_details\":{\"reasoning_tokens\":0}},\"max_output_tokens\":16,\"store\":false,\"background\":false,\"service_tier\":\"default\",\"metadata\":{}},\"sequence_number\":10,\"type\":\"response.incomplete\"}\n\n"

// ollamaCodexRequestBody 镜像用户客户端的请求体。
const ollamaCodexRequestBody = `{"model":"deepseek-v4.1-flash","input":"hi","stream":true,"reasoning":{"effort":"max"},"max_output_tokens":16}`

// ollamaCodexNonStreamBody 与非流式请求体（stream 省略，即非流式）。
const ollamaCodexNonStreamRequestBody = `{"model":"deepseek-v4.1-flash","input":"hi","reasoning":{"effort":"max"},"max_output_tokens":16}`

// TestIntegrationOllamaCodexStreamUsageCaptured 端到端复现「真实上游流 → 数据面 → 落库」：
// codex 供应商、/v1/responses、带 reasoning 的流式请求，终帧是 response.incomplete（带 usage）。
func TestIntegrationOllamaCodexStreamUsageCaptured(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range strings.SplitAfter(ollamaCodexStreamBody, "\n\n") {
			if frame == "" {
				continue
			}
			_, _ = io.WriteString(w, frame)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	pools := integrationStore(t)
	provisioned := provision(t, pools, upstream.URL, "codex")
	handler := newIntegrationHandler(t, provisioned)
	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses",
		strings.NewReader(ollamaCodexRequestBody))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", provisioned.apiKey)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d", response.StatusCode)
	}
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		// 读空客户端侧，确保流被完整消费（与用户客户端行为一致）。
	}
	_ = scanner.Err()

	row := waitForFinalizedRow(t, provisioned)
	t.Logf("落库行: status=%v input=%v output=%v cache_read=%v actual_model=%v",
		row["status_code"], row["input_tokens"], row["output_tokens"],
		row["cache_read_input_tokens"], row["actual_response_model"])

	if input := int(numberOrZero(row["input_tokens"])); input != 31 {
		t.Errorf("input_tokens 应为 31（真实流里 response.incomplete 带 usage），收到 %d", input)
	}
	if output := int(numberOrZero(row["output_tokens"])); output != 16 {
		t.Errorf("output_tokens 应为 16，收到 %d", output)
	}
}

// TestIntegrationOllamaCodexNonStreamUsageCaptured 同上，走非流式：真实响应体是
// **裸 response 对象**，usage 在顶层（夹具取自真实抓取）。
func TestIntegrationOllamaCodexNonStreamUsageCaptured(t *testing.T) {
	body := readOllamaCodexNonStreamFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	pools := integrationStore(t)
	provisioned := provision(t, pools, upstream.URL, "codex")
	handler := newIntegrationHandler(t, provisioned)
	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses",
		strings.NewReader(ollamaCodexNonStreamRequestBody))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", provisioned.apiKey)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d", response.StatusCode)
	}
	_, _ = io.Copy(io.Discard, response.Body)

	row := waitForFinalizedRow(t, provisioned)
	t.Logf("落库行: status=%v input=%v output=%v actual_model=%v",
		row["status_code"], row["input_tokens"], row["output_tokens"], row["actual_response_model"])

	if input := int(numberOrZero(row["input_tokens"])); input != 31 {
		t.Errorf("input_tokens 应为 31（非流式正文顶层 usage），收到 %d", input)
	}
	if output := int(numberOrZero(row["output_tokens"])); output != 16 {
		t.Errorf("output_tokens 应为 16，收到 %d", output)
	}
}

// readOllamaCodexNonStreamFixture 读取真实抓取的非流式响应体夹具。
func readOllamaCodexNonStreamFixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "ollama-codex-nonstream-responses.json"))
	if err != nil {
		t.Fatalf("读夹具失败: %v", err)
	}
	return body
}

// TestIntegrationOllamaCodexHugeSingleLineUsageCaptured 覆盖**超大单行**形状：
// 真实上游在长推理/工具调用时会把整段内容塞进**一条** data 行（可达数百 KB），
// 这会超过分帧器缓冲上限而走「头尾窗口回退」。本用例断言该回退能拿回 usage。
func TestIntegrationOllamaCodexHugeSingleLineUsageCaptured(t *testing.T) {
	huge := strings.Repeat("x", 300<<10) // 300 KiB 的单行
	terminal := `{"response":{"id":"resp_huge","object":"response","status":"completed",` +
		`"model":"deepseek-v4.1-flash","output":[{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"` + huge + `"}]}],` +
		`"usage":{"input_tokens":31,"output_tokens":16,"total_tokens":47,` +
		`"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}},` +
		`"sequence_number":9,"type":"response.completed"}`
	streamBody := "event: response.created\ndata: {\"response\":{\"id\":\"resp_huge\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"deepseek-v4.1-flash\"},\"sequence_number\":0,\"type\":\"response.created\"}\n\n" +
		"event: response.completed\ndata: " + terminal + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range strings.SplitAfter(streamBody, "\n\n") {
			if frame == "" {
				continue
			}
			_, _ = io.WriteString(w, frame)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	pools := integrationStore(t)
	provisioned := provision(t, pools, upstream.URL, "codex")
	handler := newIntegrationHandler(t, provisioned)
	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses",
		strings.NewReader(ollamaCodexRequestBody))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", provisioned.apiKey)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)

	row := waitForFinalizedRow(t, provisioned)
	t.Logf("落库行: status=%v input=%v output=%v actual_model=%v",
		row["status_code"], row["input_tokens"], row["output_tokens"], row["actual_response_model"])
	if input := int(numberOrZero(row["input_tokens"])); input != 31 {
		t.Errorf("超大单行时 input_tokens 应为 31（走头尾窗口回退），收到 %d", input)
	}
	if output := int(numberOrZero(row["output_tokens"])); output != 16 {
		t.Errorf("超大单行时 output_tokens 应为 16，收到 %d", output)
	}
}

// TestIntegrationOllamaCodexLargeManyFrameStreamUsageCaptured 覆盖**真实规模的多帧流**：
// 实测上游对 2000 字回答会发 ~609 KB / ~7138 行的小帧流（最长行仅 42 KB），
// 用量在最后一帧。本用例断言这种「大而正常」的流不会丢用量。
func TestIntegrationOllamaCodexLargeManyFrameStreamUsageCaptured(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("event: response.created\ndata: {\"response\":{\"id\":\"resp_big\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"deepseek-v4.1-flash\"},\"sequence_number\":0,\"type\":\"response.created\"}\n\n")
	// 2000 帧内容增量（每帧约 300 字节），模拟真实长回答。
	chunk := strings.Repeat("a", 280)
	for index := 0; index < 2000; index++ {
		builder.WriteString("event: response.output_text.delta\ndata: {\"delta\":\"" + chunk + "\",\"item_id\":\"msg_1\",\"output_index\":0,\"sequence_number\":")
		builder.WriteString(strconv.Itoa(index + 1))
		builder.WriteString(",\"type\":\"response.output_text.delta\"}\n\n")
	}
	builder.WriteString("event: response.completed\ndata: {\"response\":{\"id\":\"resp_big\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"deepseek-v4.1-flash\",\"usage\":{\"input_tokens\":31,\"output_tokens\":16,\"total_tokens\":47,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens_details\":{\"reasoning_tokens\":0}}},\"sequence_number\":2001,\"type\":\"response.completed\"}\n\n")
	streamBody := builder.String()
	t.Logf("夹具规模: %d 字节", len(streamBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range strings.SplitAfter(streamBody, "\n\n") {
			if frame == "" {
				continue
			}
			_, _ = io.WriteString(w, frame)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	pools := integrationStore(t)
	provisioned := provision(t, pools, upstream.URL, "codex")
	handler := newIntegrationHandler(t, provisioned)
	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses",
		strings.NewReader(ollamaCodexRequestBody))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", provisioned.apiKey)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)

	row := waitForFinalizedRow(t, provisioned)
	t.Logf("落库行: status=%v input=%v output=%v actual_model=%v",
		row["status_code"], row["input_tokens"], row["output_tokens"], row["actual_response_model"])
	if input := int(numberOrZero(row["input_tokens"])); input != 31 {
		t.Errorf("大流量的 input_tokens 应为 31，收到 %d", input)
	}
	if output := int(numberOrZero(row["output_tokens"])); output != 16 {
		t.Errorf("大流量的 output_tokens 应为 16，收到 %d", output)
	}
}

// TestIntegrationOllamaCodexConcurrentStreamsKeepUsage 覆盖**并发在途**（用户客户端实测
// 常有 4 个以上同时在途）：并发不应让任何一路丢用量。
func TestIntegrationOllamaCodexConcurrentStreamsKeepUsage(t *testing.T) {
	const streams = 8
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range strings.SplitAfter(ollamaCodexStreamBody, "\n\n") {
			if frame == "" {
				continue
			}
			_, _ = io.WriteString(w, frame)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	pools := integrationStore(t)
	provisioned := provision(t, pools, upstream.URL, "codex")
	handler := newIntegrationHandler(t, provisioned)
	server := httptest.NewServer(handler)
	defer server.Close()

	var group sync.WaitGroup
	failures := make(chan string, streams)
	for index := 0; index < streams; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses",
				strings.NewReader(ollamaCodexRequestBody))
			if err != nil {
				failures <- err.Error()
				return
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("x-api-key", provisioned.apiKey)
			response, err := server.Client().Do(request)
			if err != nil {
				failures <- err.Error()
				return
			}
			defer func() { _ = response.Body.Close() }()
			_, _ = io.Copy(io.Discard, response.Body)
		}()
	}
	group.Wait()
	close(failures)
	for message := range failures {
		t.Fatalf("并发请求失败: %s", message)
	}

	rows := waitForFinalizedRows(t, provisioned, streams)
	missing := 0
	for _, row := range rows {
		if int(numberOrZero(row["input_tokens"])) != 31 || int(numberOrZero(row["output_tokens"])) != 16 {
			missing++
			t.Errorf("并发下某行丢了用量: id=%v input=%v output=%v", row["id"], row["input_tokens"], row["output_tokens"])
		}
	}
	t.Logf("并发 %d 路：%d 行落库，丢用量 %d 行", streams, len(rows), missing)
}

// waitForFinalizedRows 等最近 count 行全部终态。
func waitForFinalizedRows(t *testing.T, provisioned provisioning, count int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		rows := lastRequestRows(t, provisioned, count)
		settled := 0
		for _, row := range rows {
			if row["status_code"] != nil {
				settled++
			}
		}
		if settled >= count {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("终态未在 20s 内落齐：%d/%d 已落", settled, count)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// lastRequestRows 读回该供应商最近 count 行请求日志。
func lastRequestRows(t *testing.T, provisioned provisioning, count int) []map[string]any {
	t.Helper()
	reader, err := provisioned.pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	rows, err := reader.Query(context.Background(), `
		SELECT row_to_json(t)::text FROM (
			SELECT * FROM message_request
			WHERE provider_id = $1 AND id > $2
			ORDER BY id DESC LIMIT $3
		) t`, provisioned.providerID, provisioned.watermark, count)
	if err != nil {
		t.Fatalf("读取请求日志失败: %v", err)
	}
	defer rows.Close()
	out := make([]map[string]any, 0, count)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(payload), &row); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		out = append(out, row)
	}
	return out
}
