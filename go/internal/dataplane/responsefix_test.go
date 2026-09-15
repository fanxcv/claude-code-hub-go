package dataplane

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/responsefix"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// responseFixSettingsStub 是响应修复器的设置桩。
type responseFixSettingsStub struct {
	settings *store.SystemSettings
	err      error
}

func (f responseFixSettingsStub) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return f.settings, f.err
}

func enabledResponseFixSettings(config string) *store.SystemSettings {
	return &store.SystemSettings{
		EnableResponseFixer: true,
		ResponseFixerConfig: json.RawMessage(config),
	}
}

// newResponseFixHandler 构造只带响应修复接线的处理器。
func newResponseFixHandler(settings *store.SystemSettings, wiring ResponseFixWiring) *Handler {
	return &Handler{
		options: Options{
			Base:        guard.Deps{Settings: responseFixSettingsStub{settings: settings}},
			ResponseFix: wiring,
		},
		logger: logx.New(nil),
	}
}

// requestStateWithRowID 造一个已开行的请求状态（审计追加需要行 id）。
func requestStateWithRowID(t *testing.T, id int64) *RequestState {
	t.Helper()
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/responses"})
	if err != nil {
		t.Fatalf("构造请求上下文失败：%v", err)
	}
	if err := pc.SetMessageRequestID(id); err != nil {
		t.Fatalf("写入行 id 失败：%v", err)
	}
	return &RequestState{PC: pc}
}

func TestFixNonStreamBodyDisabledLeavesBytesUntouched(t *testing.T) {
	handler := newResponseFixHandler(enabledResponseFixSettings(`{"fixEncoding": false, "fixTruncatedJson": false}`), ResponseFixWiring{})
	body := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"a":1}`)...)

	got := handler.fixNonStreamBody(context.Background(), nil, http.Header{}, body)

	if string(got) != string(body) {
		t.Fatalf("子项全关时应原样返回：got %q", got)
	}
}

func TestFixNonStreamBodyEnableSwitchOffLeavesBytesUntouched(t *testing.T) {
	handler := newResponseFixHandler(&store.SystemSettings{EnableResponseFixer: false}, ResponseFixWiring{})
	body := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"a":1}`)...)

	got := handler.fixNonStreamBody(context.Background(), nil, http.Header{}, body)

	if string(got) != string(body) {
		t.Fatalf("enable_response_fixer=false 时应完全不动：got %q", got)
	}
}

func TestFixNonStreamBodyRepairsAndAppendsAudit(t *testing.T) {
	var gotID int64
	var gotEntries []byte
	wiring := ResponseFixWiring{
		AppendSpecialSettings: func(_ context.Context, id int64, entries []byte) error {
			gotID, gotEntries = id, entries
			return nil
		},
	}
	handler := newResponseFixHandler(enabledResponseFixSettings(`{"maxFixSize": 1048576}`), wiring)
	state := requestStateWithRowID(t, 4321)
	body := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"key":`)...)

	got := handler.fixNonStreamBody(context.Background(), state, http.Header{"Content-Type": {"application/json"}}, body)

	if string(got) != `{"key":null}` {
		t.Fatalf("非流式正文应被修复：got %q", got)
	}
	if gotID != 4321 {
		t.Fatalf("审计应追加到本次请求行：got id=%d", gotID)
	}
	var entries []map[string]any
	if err := json.Unmarshal(gotEntries, &entries); err != nil {
		t.Fatalf("审计条目应是 JSON 数组：%v", err)
	}
	if len(entries) != 1 || entries[0]["type"] != "response_fixer" {
		t.Fatalf("审计条目形状不符：%v", entries)
	}
}

func TestFixNonStreamBodySkipsOpaqueContentEncoding(t *testing.T) {
	handler := newResponseFixHandler(enabledResponseFixSettings(`{}`), ResponseFixWiring{})
	body := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"a":1}`)...)
	headers := http.Header{"Content-Encoding": {"gzip"}}

	got := handler.fixNonStreamBody(context.Background(), nil, headers, body)

	if string(got) != string(body) {
		t.Fatalf("带内容编码的正文必须原样透传（修压过的字节只会弄坏它）：got %q", got)
	}
}

func TestNewResponseFixStreamRequiresSSEContentType(t *testing.T) {
	handler := newResponseFixHandler(enabledResponseFixSettings(`{}`), ResponseFixWiring{})

	if fixer := handler.newResponseFixStream(context.Background(), nil, http.Header{"Content-Type": {"application/json"}}); fixer != nil {
		t.Fatalf("非 SSE 响应不该启用流式修复器")
	}
	if fixer := handler.newResponseFixStream(context.Background(), nil, http.Header{"Content-Type": {"text/event-stream"}}); fixer == nil {
		t.Fatalf("SSE 响应应启用流式修复器")
	}
	if fixer := handler.newResponseFixStream(context.Background(), nil, http.Header{"Content-Type": {"Text/Event-Stream; charset=utf-8"}}); fixer == nil {
		t.Fatalf("Content-Type 判定应大小写不敏感")
	}
}

func TestRecordResponseFixAuditSkipsWhenNoHit(t *testing.T) {
	called := false
	wiring := ResponseFixWiring{
		AppendSpecialSettings: func(context.Context, int64, []byte) error {
			called = true
			return nil
		},
	}
	handler := newResponseFixHandler(enabledResponseFixSettings(`{}`), wiring)

	handler.recordResponseFixAudit(context.Background(), requestStateWithRowID(t, 99), responsefix.Audit{})

	if called {
		t.Fatalf("没有修复时不写审计（Node 的 audit.hit 门控）")
	}
}

func TestRecordResponseFixAuditSkipsWithoutRowID(t *testing.T) {
	called := false
	wiring := ResponseFixWiring{
		AppendSpecialSettings: func(context.Context, int64, []byte) error {
			called = true
			return nil
		},
	}
	handler := newResponseFixHandler(enabledResponseFixSettings(`{}`), wiring)
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/responses"})
	if err != nil {
		t.Fatalf("构造请求上下文失败：%v", err)
	}

	handler.recordResponseFixAudit(context.Background(), &RequestState{PC: pc}, responsefix.Audit{Hit: true, JSONApplied: true})

	if called {
		t.Fatalf("行 id 未就绪时不该写审计")
	}
}

func TestResponseFixClientResponsesOnlyForResponsesFormat(t *testing.T) {
	handler := newResponseFixHandler(enabledResponseFixSettings(`{}`), ResponseFixWiring{})
	state := &RequestState{Format: convert.FormatOpenAI}

	fixer := handler.newResponseFixStream(context.Background(), state, http.Header{"Content-Type": {"text/event-stream"}})
	if fixer == nil {
		t.Fatalf("应构造出流式修复器")
	}
	chunk := `{"id":"chatcmpl-keep","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`
	out := fixer.Write([]byte("data: " + chunk + "\n\n"))

	if want := "data: " + chunk + "\n\n"; string(out) != want {
		t.Fatalf("非 responses 客户端不该过滤 chat 帧：got %q", out)
	}
}

func TestResponseFixResponsesFormatFiltersInertChunk(t *testing.T) {
	handler := newResponseFixHandler(enabledResponseFixSettings(`{}`), ResponseFixWiring{})
	state := &RequestState{Format: convert.FormatResponse}

	fixer := handler.newResponseFixStream(context.Background(), state, http.Header{"Content-Type": {"text/event-stream"}})
	chunk := `{"id":"chatcmpl-drop","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`
	out := fixer.Write([]byte("data: " + chunk + "\n\n"))

	if string(out) != "" {
		t.Fatalf("responses 客户端应过滤惰性 chat 帧：got %q", out)
	}
}

func TestResponseFixStreamSkipsOpaqueContentEncoding(t *testing.T) {
	handler := newResponseFixHandler(enabledResponseFixSettings(`{}`), ResponseFixWiring{})
	headers := http.Header{
		"Content-Type":     {"text/event-stream"},
		"Content-Encoding": {"gzip"},
	}

	if fixer := handler.newResponseFixStream(context.Background(), nil, headers); fixer != nil {
		t.Fatalf("带内容编码的流必须原样透传（与转换器同一跳过条件）")
	}
}

func TestResponseFixSettingsLookupFailureLeavesBytesUntouched(t *testing.T) {
	handler := newResponseFixHandler(nil, ResponseFixWiring{})
	body := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"a":1}`)...)

	got := handler.fixNonStreamBody(context.Background(), nil, http.Header{}, body)

	if string(got) != string(body) {
		t.Fatalf("取不到设置时不该动字节：got %q", got)
	}
}
