package tracing

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

func testRecord(id int64) terminal.TraceRecord {
	started := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)
	return terminal.TraceRecord{
		ID:         id,
		UserID:     3,
		Name:       "POST /v1/messages",
		StartedAt:  started,
		EndedAt:    started.Add(time.Second),
		StatusCode: 200,
		Model:      "claude-sonnet-4",
	}
}

func newTestTracer(t *testing.T, collector *collector, options Options) *Tracer {
	t.Helper()
	if options.BaseURL == "" {
		options.BaseURL = collector.server.URL
	}
	if options.PublicKey == "" {
		options.PublicKey = "pk-unit-test"
	}
	if options.SecretKey == "" {
		options.SecretKey = "sk-unit-test"
	}
	tracer := New(options)
	if tracer == nil {
		t.Fatalf("给了 key 时不应返回 nil")
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// 收集器可能被 hold 住：先放行，避免收尾等不到。
		collector.release()
		tracer.Close(closeCtx)
	})
	return tracer
}

// 契约字段落到请求上：端点路径、Basic 认证（public:secret）、gzip。
func TestRequestCarriesContractFields(t *testing.T) {
	collector := newCollector(t, 207)
	tracer := newTestTracer(t, collector, Options{SampleRate: 1, FlushInterval: time.Millisecond})
	tracer.RecordTerminal(testRecord(1))
	tracer.Close(context.Background())

	captured := collector.captured()
	if len(captured) != 1 {
		t.Fatalf("应恰好发出一次批量请求，得到 %d 次", len(captured))
	}
	request := captured[0]
	if request.path != "/api/public/ingestion" {
		t.Fatalf("端点路径不符: %q", request.path)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-unit-test:sk-unit-test"))
	if request.authorization != wantAuth {
		t.Fatalf("Basic 头不符: %q", request.authorization)
	}
	if request.contentEncoding != "gzip" {
		t.Fatalf("正文应为 gzip: %q", request.contentEncoding)
	}
	batch := decodeBatch(t, request.body)
	if len(batch.Batch) != 1 || batch.Batch[0].ID != "trace-1" {
		t.Fatalf("请求体不含预期的 trace 事件: %+v", batch.Batch)
	}
	if tracer.Counters().Sent != 1 {
		t.Fatalf("成功发出计数应为 1: %+v", tracer.Counters())
	}
}

// 缺任一 key 即整体关闭：New 返回 nil，且经 AsTracer 后是**真 nil 接口**
// （否则接缝的 nil 判断拦不住，一次调用就 panic）。
func TestMissingKeysDisableEverything(t *testing.T) {
	cases := map[string]Options{
		"缺 public": {PublicKey: "", SecretKey: "sk"},
		"缺 secret": {PublicKey: "pk", SecretKey: ""},
		"只有空白":     {PublicKey: "  ", SecretKey: "  "},
	}
	for name, options := range cases {
		t.Run(name, func(t *testing.T) {
			tracer := New(options)
			if tracer != nil {
				t.Fatalf("缺 key 时必须返回 nil")
			}
			var seam terminal.Tracer = AsTracer(tracer)
			if seam != nil {
				t.Fatalf("经 AsTracer 后必须是真 nil 接口，得到 %#v", seam)
			}
			// nil 安全：调用与关闭都不得 panic。
			tracer.RecordTerminal(testRecord(1))
			tracer.Close(context.Background())
			if counters := tracer.Counters(); counters != (Counters{}) {
				t.Fatalf("关闭态不应有计数: %+v", counters)
			}
		})
	}
}

// 日志里不得出现凭据：只允许出现「已启用」这类事实与脱敏字段。
func TestLogsNeverContainKeys(t *testing.T) {
	collector := newCollector(t, 500)
	logger := &recordingLogger{}
	tracer := newTestTracer(t, collector, Options{
		SampleRate:    1,
		FlushInterval: time.Millisecond,
		Logger:        logger,
		Debug:         true,
	})
	tracer.RecordTerminal(testRecord(1))
	tracer.Close(context.Background())

	lines := strings.Join(logger.lines(), "\n")
	if lines == "" {
		t.Fatalf("失败时必须留痕，日志为空")
	}
	for _, secret := range []string{"pk-unit-test", "sk-unit-test"} {
		if strings.Contains(lines, secret) {
			t.Fatalf("日志里出现了凭据原文 %q:\n%s", secret, lines)
		}
	}
	// 也不能出现 base64(public:secret)——它等价于明文凭据。
	encoded := base64.StdEncoding.EncodeToString([]byte("pk-unit-test:sk-unit-test"))
	if strings.Contains(lines, encoded) {
		t.Fatalf("日志里出现了 Basic 凭据串:\n%s", lines)
	}
}

// 收集器报 5xx 时只计数与留痕：不重试、不阻塞、不 panic。
func TestCollectorFailureIsCountedNotRetried(t *testing.T) {
	collector := newCollector(t, 500)
	logger := &recordingLogger{}
	tracer := newTestTracer(t, collector, Options{
		SampleRate:    1,
		FlushInterval: time.Millisecond,
		Logger:        logger,
	})
	for id := int64(1); id <= 3; id++ {
		tracer.RecordTerminal(testRecord(id))
	}
	tracer.Close(context.Background())

	counters := tracer.Counters()
	if counters.Failed != 3 {
		t.Fatalf("三条失败应记三条失败计数: %+v", counters)
	}
	if counters.Sent != 0 {
		t.Fatalf("失败不得计入成功: %+v", counters)
	}
	if !logger.hasEvent("tracing.send_rejected") {
		t.Fatalf("失败必须留痕: %v", logger.lines())
	}
	if collector.count() != 1 {
		t.Fatalf("失败后不得重试（应只发一次）: %d", collector.count())
	}
}

// 默认值：空 BaseURL 取契约默认基址，队列按 DefaultQueueSize 建。
func TestDefaults(t *testing.T) {
	tracer := New(Options{PublicKey: "pk", SecretKey: "sk"})
	if tracer == nil {
		t.Fatalf("给了 key 时不应返回 nil")
	}
	t.Cleanup(func() { tracer.Close(context.Background()) })
	if tracer.endpoint != DefaultBaseURL+ingestionPath {
		t.Fatalf("默认端点不符: %q", tracer.endpoint)
	}
	if cap(tracer.queue) != DefaultQueueSize {
		t.Fatalf("默认队列容量不符: %d", cap(tracer.queue))
	}
	if tracer.batchSize != DefaultBatchSize || tracer.flushInterval != DefaultFlushInterval {
		t.Fatalf("默认批参数不符: %d %s", tracer.batchSize, tracer.flushInterval)
	}
	// BaseURL 末尾多余斜杠不得拼出双斜杠路径。
	tracer2 := New(Options{BaseURL: "https://example.com/", PublicKey: "pk", SecretKey: "sk"})
	t.Cleanup(func() { tracer2.Close(context.Background()) })
	if tracer2.endpoint != "https://example.com"+ingestionPath {
		t.Fatalf("尾斜杠未归一: %q", tracer2.endpoint)
	}
}
