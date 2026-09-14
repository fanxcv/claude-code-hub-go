package httpapi

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件测「出站压缩」的四类事实：
//
//  1. 协商与类型判定（纯函数，表驱动）；
//  2. 压缩确实发生、且正文逐字节可还原；
//  3. **两条流式闸门各自独立成立**——这是本改动最需要守住的不变量，
//     误压事件流的后果远大于漏压一个响应；
//  4. 它真的挂在了处理器上（不是一段没人调用的实现）。

// newCompressTestServer 造一个只用于压缩测试的服务器（其余依赖留空）。
func newCompressTestServer() *Server {
	return &Server{logger: logx.New(nil)}
}

// serveCompress 用给定处理器跑一次请求，返回记录器。
func serveCompress(handler http.Handler, method, accept string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/api/v1/usage-logs?limit=50", nil)
	if accept != "" {
		request.Header.Set("Accept-Encoding", accept)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// decodeCompressed 按响应头解压正文；无编码时原样返回。
func decodeCompressed(t *testing.T, recorder *httptest.ResponseRecorder) []byte {
	t.Helper()
	raw := recorder.Body.Bytes()
	var reader io.Reader = bytes.NewReader(raw)
	switch recorder.Header().Get("Content-Encoding") {
	case encodingBrotli:
		reader = brotli.NewReader(reader)
	case encodingGzip:
		gzipReader, err := gzip.NewReader(reader)
		if err != nil {
			t.Fatalf("gzip 解压器构造失败: %v", err)
		}
		defer func() { _ = gzipReader.Close() }()
		reader = gzipReader
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	return body
}

// usageLogFields 是实测生产响应的 54 个字段名，按原顺序。
//
// 取自 2026-09-13 对生产的只读取数（`/api/v1/usage-logs?limit=50`）。
var usageLogFields = []string{
	"id", "createdAt", "createdAtRaw", "sessionId", "sourceSessionId", "sessionIdentityKind",
	"requestSequence", "userName", "keyName", "providerName", "model", "originalModel",
	"actualResponseModel", "endpoint", "statusCode", "inputTokens", "outputTokens",
	"cacheCreationInputTokens", "cacheReadInputTokens", "cacheCreation5mInputTokens",
	"cacheCreation1hInputTokens", "cacheTtlApplied", "theoreticalCacheTokens",
	"cacheScoreEligible", "cacheScoreExcludedReason", "costUsd", "costMultiplier",
	"groupCostMultiplier", "costBreakdown", "hedgeLosers", "durationMs", "ttftMs",
	"firstByteMs", "errorMessage", "providerChain", "routingTrace", "blockedBy",
	"blockedReason", "isReplay", "replaySourceRequestId", "userAgent", "clientIp",
	"messagesCount", "context1mApplied", "swapCacheTtlApplied", "specialSettings",
	"totalTokens", "cacheInputTotal", "actualCacheRate", "theoreticalCacheRate",
	"requestCacheCoefficientBp", "requestCacheMetricAvailability", "anthropicEffort",
	"sourceSessionIds",
}

// usageLogsShapedPayload 造一份与**实测生产响应**同形状的载荷。
//
// 口径（2026-09-13 只读取数）：`/api/v1/usage-logs?limit=50` 实测 **284,621 字节**、
// `Content-Encoding: identity`；顶层三键 `items`/`sourceSessionIdsByIdentity`/`pageInfo`，
// 50 行 × 54 字段、约 **5.3 KB / 行**，其中 `providerChain` ~2.0 KB、`routingTrace` ~1.2 KB、
// `specialSettings` ~0.5 KB。
//
// 这里按同样的字段名与嵌套结构生成，**值全部合成**（不落任何生产数据），但刻意保留真实 JSON 的
// 键重复度与嵌套形状——用「短值 + 大量 null」的简化载荷会把压缩比做得虚高，那样测出来的
// 收益不可采信。
func usageLogsShapedPayload() []byte {
	const rows = 50
	var builder bytes.Buffer
	builder.WriteString(`{"items":[`)
	for row := 0; row < rows; row++ {
		if row > 0 {
			builder.WriteByte(',')
		}
		writeUsageLogRow(&builder, row)
	}
	builder.WriteString(`],"sourceSessionIdsByIdentity":{`)
	for row := 0; row < 4; row++ {
		if row > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `"identity-%02d":["01a098%02d-c124-76bb-9423-8f881bc4406d"]`, row, row)
	}
	builder.WriteString(`},"pageInfo":{"hasMore":true,"nextCursor":"954000"}}`)
	return builder.Bytes()
}

// writeUsageLogRow 按真实字段顺序写一行。
func writeUsageLogRow(builder *bytes.Buffer, row int) {
	builder.WriteByte('{')
	for index, field := range usageLogFields {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(builder, "%q:", field)
		switch field {
		case "id":
			fmt.Fprintf(builder, "%d", 954000+row)
		case "createdAt":
			fmt.Fprintf(builder, `"2026-09-13T21:%02d:%02d+08:00"`, row%60, (row*7)%60)
		case "createdAtRaw":
			fmt.Fprintf(builder, `"2026-09-13T21:%02d:%02d.%06d437+08:00"`, row%60, (row*7)%60, row*1237)
		case "sourceSessionId":
			// 实测该字段平均 83 字节（比 sessionId 长：带来源前缀的复合身份）。
			fmt.Fprintf(builder, `"responses:session:01a098%02d-c124-76bb-9423-8f881bc4406d"`, row)
		case "sessionId":
			fmt.Fprintf(builder, `"01a098%02d-c124-76bb-9423-8f881bc4406d"`, row)
		case "replaySourceRequestId":
			fmt.Fprintf(builder, `"01a098%02d-c124-76bb-9423-8f881bc4406d"`, row)
		case "sessionIdentityKind":
			builder.WriteString(`"prefix_affinity"`)
		case "requestSequence":
			fmt.Fprintf(builder, "%d", row+1)
		case "userName", "keyName":
			fmt.Fprintf(builder, `"%s-%d"`, field, row%3)
		case "providerName":
			builder.WriteString(`"OpenCode X Chat"`)
		case "model":
			builder.WriteString(`"deepseek-v4.1-flash"`)
		case "originalModel":
			builder.WriteString(`"deepseek-v4.1-flash"`)
		case "actualResponseModel":
			builder.WriteString(`"deepseek-v4.1-flash-20260901"`)
		case "endpoint":
			builder.WriteString(`"/v1/responses"`)
		case "statusCode":
			builder.WriteString("200")
		case "inputTokens":
			fmt.Fprintf(builder, "%d", 3000+row*7)
		case "outputTokens":
			fmt.Fprintf(builder, "%d", 200+row*3)
		case "cacheCreationInputTokens":
			builder.WriteString("0")
		case "cacheReadInputTokens":
			fmt.Fprintf(builder, "%d", 2900+row*7)
		case "cacheCreation5mInputTokens", "cacheCreation1hInputTokens", "theoreticalCacheTokens":
			builder.WriteString("0")
		case "cacheTtlApplied":
			builder.WriteString("null")
		case "cacheScoreEligible":
			builder.WriteString("true")
		case "cacheScoreExcludedReason":
			builder.WriteString("null")
		case "costUsd":
			fmt.Fprintf(builder, `"0.0044%06d00000"`, row)
		case "costMultiplier", "groupCostMultiplier":
			builder.WriteString(`"1.000000000000000"`)
		case "costBreakdown":
			// 实测平均 237 字节。
			builder.WriteString(`{"input":"0.004100000000000","output":"0.000300000000000",` +
				`"cacheRead":"0.000000000000000","cacheCreation":"0.000000000000000",` +
				`"cacheCreation5m":"0.000000000000000","cacheCreation1h":"0.000000000000000",` +
				`"total":"0.004400000000000","currency":"USD","pricingVersion":"2026-09-01"}`)
		case "hedgeLosers":
			builder.WriteString(`[{"providerId":163,"providerName":"CommandCode Chat","reason":"hedge_loser_billed"}]`)
		case "durationMs":
			builder.WriteString("1204")
		case "ttftMs":
			builder.WriteString("392")
		case "firstByteMs":
			builder.WriteString("393")
		case "errorMessage":
			if row%5 == 0 {
				builder.WriteString(`"upstream returned 503: service temporarily unavailable (request id: 20260913214507823709368268d9d6Tjk2PIO8)"`)
			} else {
				builder.WriteString("null")
			}
		case "providerChain":
			// 实测该字段平均 1916 字节、中位 1987（多数行是完整链），也有空数组的行。
			// 各条目的供应商标识、时刻与结局随 row 变化——若整列逐行同文，压缩比会虚高数倍
			// （实测教训：逐行同文时这份合成载荷算出 66x，而生产真字节只有 22x）。
			if row%7 == 0 {
				builder.WriteString("[]")
			} else {
				writeChainEntries(builder, row)
			}
		case "routingTrace":
			// 实测该字段平均 1121 字节、中位 1150（多轮尝试各一条）。
			if row%7 == 0 {
				builder.WriteString("[]")
			} else {
				writeTraceEntries(builder, row)
			}
		case "specialSettings":
			// 实测该字段平均 511 字节。
			writeSpecialSettings(builder, row)
		case "blockedBy", "blockedReason":
			builder.WriteString("null")
		case "isReplay":
			builder.WriteString("false")
		case "userAgent":
			builder.WriteString(`"claude-cli/2.0.30 (external, cli) node/v22.23.2 linux x64"`)
		case "clientIp":
			fmt.Fprintf(builder, `"203.0.113.%d"`, row%250+1)
		case "messagesCount":
			fmt.Fprintf(builder, "%d", 1+row%12)
		case "context1mApplied", "swapCacheTtlApplied":
			builder.WriteString("false")
		case "totalTokens":
			fmt.Fprintf(builder, "%d", 3200+row*10)
		case "cacheInputTotal":
			fmt.Fprintf(builder, "%d", 2900+row*7)
		case "requestCacheMetricAvailability":
			builder.WriteString(`"available (cache read tokens reported by upstream)"`)
		case "actualCacheRate":
			builder.WriteString(`"0.966666666666667"`)
		case "theoreticalCacheRate":
			builder.WriteString(`"1.000000000000000"`)
		case "requestCacheCoefficientBp":
			builder.WriteString("9667")
		case "anthropicEffort":
			builder.WriteString(`"high"`)
		case "sourceSessionIds":
			fmt.Fprintf(builder,
				`["01a098%02d-c124-76bb-9423-8f881bc4406d","01a098%02d-c124-76bb-9423-8f881bc4406d"]`,
				row, row+50)
		default:
			fmt.Fprintf(builder, `"value-%d-%s"`, row, field)
		}
	}
	builder.WriteByte('}')
}

func TestNegotiateCompression(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		// 不发该头 / 空串：一律不压（连包装器都不建）。
		{"", ""},
		{"   ", ""},
		// 明确身份编码：不压。
		{"identity", ""},
		{"identity;q=1", ""},
		// 单个编码。
		{"gzip", encodingGzip},
		{"br", encodingBrotli},
		// 两个都接受：按服务端偏好取 br（体积始终更小）。
		{"br, gzip", encodingBrotli},
		{"gzip, br", encodingBrotli},
		{"gzip, deflate, br", encodingBrotli},
		// q=0 是明确拒绝。
		{"br;q=0", ""},
		{"br;q=0, gzip", encodingGzip},
		{"gzip;q=0, br", encodingBrotli},
		{"br;q=0, gzip;q=0", ""},
		// 大小写与空白不敏感。
		{"BR", encodingBrotli},
		{" GZIP ", encodingGzip},
		// 通配 `*` 只是给「未列出的编码」补一个 q 值，并不降低已列出项的优先级：
		// 因此 `gzip, *` 下 br 与 gzip 同为 q=1，按服务端偏好取 br（体积更小）。
		{"*", encodingBrotli},
		{"gzip, *", encodingBrotli},
		// 「显式项压过通配」的真实含义是**显式拒绝**优先：br;q=0 即便有 `*` 也不得选 br。
		{"br;q=0, *", encodingGzip},
		// 通配被整体拒绝时，只剩显式列出的那项。
		{"gzip, *;q=0", encodingGzip},
		{"*;q=0", ""},
		// 我们都不支持：不压（deflate 不在支持集内）。
		{"deflate", ""},
		{"deflate, sdch", ""},
		// q 值非法时退回缺省 1（不因为畸形参数就静默关掉压缩）。
		{"gzip;q=abc", encodingGzip},
	}
	for _, item := range cases {
		if got := negotiateCompression(item.header); got != item.want {
			t.Errorf("negotiateCompression(%q) = %q，期望 %q", item.header, got, item.want)
		}
	}
}

func TestCompressibleContentType(t *testing.T) {
	cases := []struct {
		contentType string
		want        bool
	}{
		// 会压：JSON（含 +json 后缀）与文本。
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/problem+json", true},
		{"text/plain; charset=utf-8", true},
		{"text/html; charset=utf-8", true},
		{"text/css", true},
		{"application/javascript", true},
		{"image/svg+xml", true},
		{"application/xml", true},
		// 不压：事件流（与 Flush 闸门互为备份）。
		{"text/event-stream", false},
		{"text/event-stream; charset=utf-8", false},
		{"text/x-event-stream", false},
		// 不压：装入容器时就已压缩的类型。
		{"application/zip", false},
		{"application/gzip", false},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", false},
		{"image/png", false},
		{"image/jpeg", false},
		{"font/woff2", false},
		{"video/mp4", false},
		{"audio/mpeg", false},
		{"application/pdf", false},
		{"application/octet-stream", false},
		// 不压：无类型（无法证明它不是二进制或事件流）。
		{"", false},
		// 类型带杂乱参数时仍能识别。
		{"text/event-stream;charset=utf-8", false},
	}
	for _, item := range cases {
		if got := compressibleContentType(item.contentType); got != item.want {
			t.Errorf("compressibleContentType(%q) = %v，期望 %v", item.contentType, got, item.want)
		}
	}
}

// TestLargeJSONIsCompressedAndRoundTrips 是主路径：大 JSON 被压、且正文逐字节可还原。
func TestLargeJSONIsCompressedAndRoundTrips(t *testing.T) {
	payload := usageLogsShapedPayload()
	// 生产实测为 284,621 字节；合成载荷必须落在同一量级，否则「同尺寸」这句话不成立。
	if len(payload) < 200*1024 || len(payload) > 400*1024 {
		t.Fatalf("测试载荷 %d 字节，偏离生产实测的 284,621 字节太多，代表性不足", len(payload))
	}
	t.Logf("测试载荷 %d 字节（生产实测 284,621 字节）", len(payload))
	for _, item := range []struct {
		accept string
		want   string
	}{
		{"br, gzip", encodingBrotli},
		{"br", encodingBrotli},
		{"gzip", encodingGzip},
	} {
		t.Run(item.want, func(t *testing.T) {
			handler := newCompressTestServer().withCompression(http.HandlerFunc(
				func(writer http.ResponseWriter, _ *http.Request) {
					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(http.StatusOK)
					_, _ = writer.Write(payload)
				}))
			recorder := serveCompress(handler, http.MethodGet, item.accept)

			if got := recorder.Header().Get("Content-Encoding"); got != item.want {
				t.Fatalf("Content-Encoding = %q，期望 %q", got, item.want)
			}
			if !bytes.Equal(decodeCompressed(t, recorder), payload) {
				t.Fatal("解压后的正文与原始载荷不一致")
			}
			// Vary 必须设：否则中间缓存会把压缩态发给不接受该编码的客户端。
			if got := recorder.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
				t.Fatalf("Vary = %q，应包含 Accept-Encoding", got)
			}
			// 压缩后长度不可预知，预置的 Content-Length 必须作废。
			if got := recorder.Header().Get("Content-Length"); got != "" {
				t.Fatalf("Content-Length 应为空，实际 %q", got)
			}
			if recorder.Body.Len() >= len(payload) {
				t.Fatalf("压缩后 %d 字节，未小于原始 %d 字节", recorder.Body.Len(), len(payload))
			}
			t.Logf("%s：%d -> %d 字节（%.2fx）", item.want, len(payload), recorder.Body.Len(),
				float64(len(payload))/float64(recorder.Body.Len()))
		})
	}
}

// TestSmallResponseIsNotCompressed：小于阈值的响应不压（压了更大，且丢掉 net/http 的自动定长）。
func TestSmallResponseIsNotCompressed(t *testing.T) {
	handler := newCompressTestServer().withCompression(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writeJSON(writer, http.StatusOK, map[string]any{"status": "pong"})
		}))
	recorder := serveCompress(handler, http.MethodGet, "br, gzip")
	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("小响应不应压缩，实际 Content-Encoding = %q", got)
	}
	if got := recorder.Body.String(); !strings.Contains(got, "pong") {
		t.Fatalf("正文被改动: %q", got)
	}
}

// TestEventStreamGateHolds 是流式闸门①：声明为事件流的响应一律不压。
//
// 这条用**真实的 SSE 形状**打：先写头、Flush 一次响应头，再逐事件写并 Flush。
func TestEventStreamGateHolds(t *testing.T) {
	handler := newCompressTestServer().withCompression(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			flusher, ok := writer.(http.Flusher)
			if !ok {
				t.Error("包装器未实现 http.Flusher：内部 SSE 端点会因此回 500")
				return
			}
			writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			writer.Header().Set("Cache-Control", "no-cache")
			writer.WriteHeader(http.StatusOK)
			flusher.Flush()
			for index := 0; index < 3; index++ {
				fmt.Fprintf(writer, "event: tick\ndata: %d\n\n", index)
				flusher.Flush()
			}
		}))
	recorder := serveCompress(handler, http.MethodGet, "br, gzip")

	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("事件流被压缩了（Content-Encoding = %q）——这会让逐事件可见性消失", got)
	}
	if got := recorder.Header().Get("Vary"); strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("事件流未被压缩就不该声明 Vary: Accept-Encoding，实际 %q", got)
	}
	body := recorder.Body.String()
	for index := 0; index < 3; index++ {
		if !strings.Contains(body, fmt.Sprintf("data: %d", index)) {
			t.Fatalf("事件 %d 未原样下发: %q", index, body)
		}
	}
}

// TestFlushBeforeFirstBodyByteStaysUncompressed 是流式闸门②，且**独立于类型**。
//
// 这是数据面真实形状（internal/dataplane/stream.go）：WriteHeader 后立刻 Flush 一次响应头，
// 之后才逐块写正文。这里刻意用**可压缩的** Content-Type，以证明守住它的是 Flush 闸门本身，
// 而不是类型白名单。
func TestFlushBeforeFirstBodyByteStaysUncompressed(t *testing.T) {
	handler := newCompressTestServer().withCompression(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			// 刻意声明成可压缩类型：若这里仍被压，说明丢的是 Flush 闸门。
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush() // 首个正文字节之前 Flush

			for index := 0; index < 4; index++ {
				_, _ = writer.Write([]byte(strings.Repeat("x", 4096)))
				writer.(http.Flusher).Flush()
			}
		}))
	recorder := serveCompress(handler, http.MethodGet, "br, gzip")

	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("首个正文字节前已 Flush 的响应被压缩了（Content-Encoding = %q）", got)
	}
	if recorder.Body.Len() != 4*4096 {
		t.Fatalf("正文被改动：%d 字节，期望 %d", recorder.Body.Len(), 4*4096)
	}
}

// TestFlushPropagatesToUnderlyingWriter：Flush 必须真的穿透到下层。
//
// 内部两处流式路径都以 `writer.(http.Flusher)` 决定能否流式；包装器即使实现了该接口却
// 不把 Flush 透传下去，表现就是「服务端以为在流式、客户端什么也收不到」。
func TestFlushPropagatesToUnderlyingWriter(t *testing.T) {
	spy := &flushCounter{ResponseRecorder: httptest.NewRecorder()}
	handler := newCompressTestServer().withCompression(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			_, _ = writer.Write([]byte("data: 1\n\n"))
			writer.(http.Flusher).Flush()
		}))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs/stream", nil)
	request.Header.Set("Accept-Encoding", "br, gzip")
	handler.ServeHTTP(spy, request)

	if spy.flushes < 2 {
		t.Fatalf("下层只收到 %d 次 Flush，期望 >= 2：Flush 未穿透", spy.flushes)
	}
}

// flushCounter 记录 Flush 次数。
type flushCounter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (c *flushCounter) Flush() { c.flushes++ }

// TestAlreadyEncodedResponseIsNotRecompressed：已有 Content-Encoding 的正文不得二次压缩。
func TestAlreadyEncodedResponseIsNotRecompressed(t *testing.T) {
	// 模拟 uiapp 直出的 brotli 存储态：正文已是压缩字节，且带自己的 Content-Encoding。
	var stored bytes.Buffer
	writer := brotli.NewWriterLevel(&stored, compressBrotliQuality)
	plain := []byte(strings.Repeat("stored-asset-", 4096))
	if _, err := writer.Write(plain); err != nil {
		t.Fatalf("准备载荷失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("准备载荷失败: %v", err)
	}
	storedBytes := stored.Bytes()

	handler := newCompressTestServer().withCompression(http.HandlerFunc(
		func(target http.ResponseWriter, _ *http.Request) {
			target.Header().Set("Content-Type", "application/javascript")
			target.Header().Set("Content-Encoding", "br")
			target.WriteHeader(http.StatusOK)
			_, _ = target.Write(storedBytes)
		}))
	recorder := serveCompress(handler, http.MethodGet, "br, gzip")

	if got := recorder.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q，期望保持 br", got)
	}
	if !bytes.Equal(recorder.Body.Bytes(), storedBytes) {
		t.Fatal("已编码的正文被二次压缩/改动了")
	}
}

// TestNoBodyAndHeadResponses：204 与 HEAD 不压，且状态码原样。
func TestNoBodyAndHeadResponses(t *testing.T) {
	t.Run("204", func(t *testing.T) {
		handler := newCompressTestServer().withCompression(http.HandlerFunc(
			func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusNoContent)
			}))
		recorder := serveCompress(handler, http.MethodGet, "br, gzip")
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("状态码 = %d，期望 204", recorder.Code)
		}
		if got := recorder.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("204 不应压缩，实际 %q", got)
		}
	})
	t.Run("HEAD", func(t *testing.T) {
		handler := newCompressTestServer().withCompression(http.HandlerFunc(
			func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("Content-Length", "204800")
				writer.WriteHeader(http.StatusOK)
			}))
		recorder := serveCompress(handler, http.MethodHead, "br, gzip")
		if got := recorder.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("HEAD 不应压缩，实际 %q", got)
		}
		if got := recorder.Header().Get("Content-Length"); got != "204800" {
			t.Fatalf("HEAD 的 Content-Length 应保持与 GET 一致，实际 %q", got)
		}
	})
}

// TestContentLengthDroppedWhenCompressing：声明了 Content-Length 的响应被压后必须抹掉它。
//
// 留着它的故障形态是「客户端按压缩前的长度截断或挂住」，表现为响应不完整而不是报错。
func TestContentLengthDroppedWhenCompressing(t *testing.T) {
	payload := []byte(strings.Repeat("compressible-", 20480))
	handler := newCompressTestServer().withCompression(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(payload)
		}))
	recorder := serveCompress(handler, http.MethodGet, "gzip")
	if got := recorder.Header().Get("Content-Encoding"); got != encodingGzip {
		t.Fatalf("Content-Encoding = %q，期望 gzip", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != "" {
		t.Fatalf("压缩后仍带 Content-Length = %q", got)
	}
	if !bytes.Equal(decodeCompressed(t, recorder), payload) {
		t.Fatal("解压后正文与原始不一致")
	}
}

// TestCompressionMountedOnServerHandler 证明它挂在真处理器上（不是没人调用的实现）。
func TestCompressionMountedOnServerHandler(t *testing.T) {
	payload := usageLogsShapedPayload()
	server := New(ServerOptions{
		Logger: logx.New(nil),
		AdminPlane: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("X-API-Version", "1")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(payload)
		}),
	})
	handler := server.Handler()

	// 管理面大响应对接受 br 的客户端必须被压缩。
	compressed := serveCompress(handler, http.MethodGet, "br, gzip")
	if got := compressed.Header().Get("Content-Encoding"); got != encodingBrotli {
		t.Fatalf("管理面大响应未被压缩（Content-Encoding = %q）", got)
	}
	if !bytes.Equal(decodeCompressed(t, compressed), payload) {
		t.Fatal("管理面响应解压后与原始不一致")
	}
	t.Logf("经 Server.Handler() 的传输量：%d -> %d 字节", len(payload), compressed.Body.Len())

	// 不接受压缩的客户端拿到的仍是完整明文（且没有 Content-Encoding）。
	plain := serveCompress(handler, http.MethodGet, "")
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("未请求压缩的客户端收到了 Content-Encoding = %q", got)
	}
	if !bytes.Equal(plain.Body.Bytes(), payload) {
		t.Fatal("未请求压缩的客户端正文被改动")
	}

	// 探针这类小响应不受影响。
	probe := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/v1/_ping", nil)
		request.Header.Set("Accept-Encoding", "br, gzip")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}()
	if probe.Code != http.StatusOK {
		t.Fatalf("/v1/_ping 状态码 = %d，期望 200", probe.Code)
	}
	if got := probe.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("/v1/_ping 被压缩（%q），小响应不该压", got)
	}
}

// TestStreamingIsIncrementalOverTheWire 是端到端的一条硬证据：
// 客户端能在处理器**尚未返回**时读到第一个事件。
//
// 这条用真 socket：若压缩层缓冲了事件流，这里会超时——正是 SSE 被破坏时的真实表现。
func TestStreamingIsIncrementalOverTheWire(t *testing.T) {
	secondEvent := make(chan struct{})
	handler := newCompressTestServer().withCompression(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			flusher := writer.(http.Flusher)
			writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte("event: first\ndata: 1\n\n"))
			flusher.Flush()
			// 等到客户端确实读到了第一个事件，才写第二个——证明它是逐事件可见的。
			select {
			case <-secondEvent:
			case <-time.After(5 * time.Second):
			}
			_, _ = writer.Write([]byte("event: second\ndata: 2\n\n"))
			flusher.Flush()
		}))
	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/usage-logs/stream", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	// 显式声明接受 gzip（客户端自己设该头时不会自动解压，正好用来证明「没被压」）。
	request.Header.Set("Accept-Encoding", "gzip")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if got := response.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("事件流被压缩（Content-Encoding = %q）", got)
	}
	reader := newLineReader(response.Body)
	firstLine, err := reader.readLine(t)
	if err != nil {
		t.Fatalf("读取首个事件失败: %v", err)
	}
	if !strings.Contains(firstLine, "event: first") {
		t.Fatalf("首行 = %q，期望第一个事件", firstLine)
	}
	close(secondEvent)
}

// lineReader 逐行读（只为上面那条流式证据服务，避免引入 bufio 的整块预读）。
type lineReader struct {
	body io.Reader
	rest []byte
}

func newLineReader(body io.Reader) *lineReader { return &lineReader{body: body} }

func (r *lineReader) readLine(t *testing.T) (string, error) {
	t.Helper()
	buffer := make([]byte, 1)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		count, err := r.body.Read(buffer)
		if count > 0 {
			r.rest = append(r.rest, buffer[0])
			if buffer[0] == '\n' {
				return strings.TrimRight(string(r.rest), "\r\n"), nil
			}
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("等待首行超时（事件流可能被缓冲了）")
}

// TestTransferBytesBeforeAfter 给出「改前 -> 改后」的传输字节对照。
//
// 改前 = 客户端不发 Accept-Encoding（即本改动上线前的实际行为：所有动态响应 identity）；
// 改后 = 客户端按浏览器的方式接受 br。两次走**同一个处理器与同一份载荷**。
func TestTransferBytesBeforeAfter(t *testing.T) {
	payload := usageLogsShapedPayload()
	server := New(ServerOptions{
		Logger: logx.New(nil),
		AdminPlane: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(payload)
		}),
	})
	handler := server.Handler()

	before := serveCompress(handler, http.MethodGet, "")
	afterBrotli := serveCompress(handler, http.MethodGet, "br, gzip")
	afterGzip := serveCompress(handler, http.MethodGet, "gzip")

	t.Logf("载荷 %d 字节", len(payload))
	t.Logf("改前（identity）：%d 字节", before.Body.Len())
	t.Logf("改后（br）：      %d 字节（%.2fx，省 %.1f%%）", afterBrotli.Body.Len(),
		float64(before.Body.Len())/float64(afterBrotli.Body.Len()),
		100*(1-float64(afterBrotli.Body.Len())/float64(before.Body.Len())))
	t.Logf("改后（gzip）：    %d 字节（%.2fx，省 %.1f%%）", afterGzip.Body.Len(),
		float64(before.Body.Len())/float64(afterGzip.Body.Len()),
		100*(1-float64(afterGzip.Body.Len())/float64(before.Body.Len())))

	if afterBrotli.Body.Len() >= before.Body.Len() {
		t.Fatal("压缩后传输量没有下降")
	}
}

// BenchmarkCompressDynamicJSON 量压缩的 CPU 代价与压缩比（用于「延迟 vs 流量」的取舍复核）。
func BenchmarkCompressDynamicJSON(b *testing.B) {
	payload := usageLogsShapedPayload()
	handler := newCompressTestServer().withCompression(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(payload)
		}))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs", nil)
		request.Header.Set("Accept-Encoding", "br, gzip")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
	}
}

// chainProviders 是候选链条目里出现的供应商样本（名称与优先级取自实测响应）。
var chainProviders = []struct {
	id         int
	name       string
	priority   int
	vendorID   int
	vendorType string
}{
	{163, "CommandCode Chat", 4, 12, "openai-compatible"},
	{138, "OpenCode X Chat", 2, 9, "openai-compatible"},
	{145, "Ollama Codex", 3, 7, "codex"},
	{161, "MM_Codex", 4, 11, "codex"},
	{113, "Any Router_Codex", 4, 6, "codex"},
	{156, "HC_Chat", 1, 3, "openai-compatible"},
}

// chainReasons 与 chainCircuitStates 是实测链条目里出现过的结局与熔断态。
var (
	chainReasons = []string{
		"affinity_hit", "initial_selection", "hedge_launched", "hedge_winner",
		"retry_failed", "request_success", "circuit_open", "model_not_allowed",
	}
	chainCircuitStates = []string{"closed", "closed", "half-open", "open"}
	chainErrors        = []string{
		"upstream unavailable: connection reset by peer before first byte",
		"resource_not_found: model not deployed",
		"rate limit exceeded: 429 too many requests (retry after 12s)",
		"invalid_request_error: field messages required",
	}
)

// writeChainEntries 写一条候选链：首条是选择期条目，其余是尝试条目。
//
// 条目数与其中每个值都随 row 变化，以保留真实载荷的「同形状但不同内容」特征。
func writeChainEntries(builder *bytes.Buffer, row int) {
	const entries = 3
	builder.WriteByte('[')
	for index := 0; index < entries; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		provider := chainProviders[(row+index)%len(chainProviders)]
		fmt.Fprintf(builder,
			`{"id":%d,"name":%q,"reason":%q,"weight":%d,"affinity":%t,`+
				`"groupTag":"chat,CC-Paid,codex","priority":%d,"vendorId":%d,`+
				`"timestamp":"2026-09-13T21:%02d:%02d.%06d789+08:00","circuitState":%q,`+
				`"providerType":%q,"costMultiplier":"1.000000000000000",`+
				`"selectionMethod":%q,"decisionContext":{"candidateCount":%d,`+
				`"eligibleCount":%d,"excludedByCircuit":%d,"excludedByModel":0,`+
				`"excludedByClient":0,"excludedByGroup":0,"stickyProbe":%t,`+
				`"probeWinner":%d,"probeLoser":%d,"stickyFingerprint":"%08x",`+
				`"affinityScope":"session_prefix","gateReason":%q,"fallbackReason":%q}`,
			provider.id, provider.name, chainReasons[(row+index)%len(chainReasons)],
			1+index, index == 0, provider.priority, provider.vendorID,
			row%60, (row*3+index)%60, row*1237+index,
			chainCircuitStates[(row+index)%len(chainCircuitStates)],
			provider.vendorType,
			[]string{"prefix_affinity", "weighted_random", "hedge_candidate", "retry"}[index%4],
			4+row%4, 3+row%4, (row+index)%3, index == 0,
			chainProviders[row%len(chainProviders)].id, chainProviders[(row+1)%len(chainProviders)].id,
			row*2654435761%0xffffffff+index,
			[]string{"", "", "circuit_open"}[index%3],
			[]string{"", "provider_unavailable", ""}[index%3],
		)
		if index > 0 {
			// 尝试条目额外带状态码与错误说明（选择期条目没有这两项）。
			fmt.Fprintf(builder,
				`,"statusCode":%d,"errorMessage":%q,"durationMs":%d`,
				200+index*100+row%7, chainErrors[(row+index)%len(chainErrors)], 11+row*13%900,
			)
		}
	}
	builder.WriteByte(']')
}

// writeTraceEntries 写选路轨迹：逐轮尝试各一条。
func writeTraceEntries(builder *bytes.Buffer, row int) {
	kinds := []string{"sticky", "normal", "fallback"}
	reasons := []string{"sticky_probe_started", "hedge_launched", "retry_failed", "request_success"}
	builder.WriteByte('[')
	for index := 0; index < 4; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		provider := chainProviders[(row+index)%len(chainProviders)]
		fmt.Fprintf(builder,
			`{"kind":%q,"providerId":%d,"providerName":%q,"reason":%q,`+
				`"at":"2026-09-13T21:%02d:%02d.%09d+08:00","durationMs":%d,"statusCode":%d,`+
				`"winner":%t,"attemptIndex":%d,"simulated":false,"stream":true,`+
				`"firstByteMs":%d,"errorMessage":%q}`,
			kinds[index%len(kinds)], provider.id, provider.name,
			reasons[(row+index)%len(reasons)],
			row%60, (row*5+index)%60, (row*7919+index)*1000,
			39+row*17%1200+index*11, 200+index*101%300,
			index == 3, index, row*7%400+index,
			func() string {
				if index == 0 || index == 3 {
					return ""
				}
				return chainErrors[(row+index)%len(chainErrors)]
			}(),
		)
	}
	builder.WriteByte(']')
}

// writeSpecialSettings 写特殊设置块（实测该字段平均 511 字节）。
func writeSpecialSettings(builder *bytes.Buffer, row int) {
	fmt.Fprintf(builder,
		`{"conversion":{"from":"anthropic","to":"openai-chat","fallback":%t,`+
			`"nativePair":false,"framesConverted":%d},`+
			`"effortForwarded":{"field":"reasoning_effort","value":%q,"clamped":false,`+
			`"explicit":true,"overriddenByProvider":%t},`+
			`"customHeaders":{"x-cch-session":"01a098%02d-c124-76bb-9423-8f881bc4406d",`+
			`"x-cch-trace":"enabled","x-cch-provider":"%d"},`+
			`"protocolCompat":%q,"redacted":[],`+
			`"requestFilter":{"matched":[],"blocked":false},`+
			`"sensitiveWords":{"scanned":true,"hits":%d},`+
			`"modelRedirect":{"applied":false,"target":""}}`,
		row%5 == 0, 24+row*3,
		[]string{"high", "medium", "low", "high"}[row%4], row%3 == 0,
		row, 100+row%70,
		[]string{"native_pair", "convert_required"}[row%2], row%11,
	)
}
