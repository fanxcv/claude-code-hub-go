package dataplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// chatCandidates 让候选供应商落在 chat 线（openai-compatible），从而触发 anthropic→chat 转换。
//
// 与 dataplane_test.go 的 fakeCandidates 唯一差别就是 Type：那个钉在 ProviderClaude（同线直通，
// 不转换），本文件的场景必须真的走转换，否则复现不到用户所报的形状。
type chatCandidates struct{ url string }

func (c chatCandidates) Candidate(
	_ context.Context,
	selection pctx.ProviderSelection,
	_ SelectionFacts,
) (*forward.Candidate, route.Result, error) {
	return &forward.Candidate{
		// ConversionEnabled 对应 provider.protocol_conversion_enabled；不置真就退化为原样直通
		// （那样就复现不到跨线转换产生的畸形形状，测试会假绿）。
		ConversionEnabled: true,
		Provider: forward.Provider{
			ID:   selection.ProviderID,
			Name: selection.Name,
			Type: convert.ProviderOpenAICompatible,
			URL:  c.url,
			Key:  "fake-upstream-key",
		},
	}, route.Result{}, nil
}

func (c chatCandidates) Failover(context.Context, SelectionFacts) (*forward.Candidate, route.Result, error) {
	return nil, route.Result{}, nil
}

// strictUpstreamViolation 复刻真实上游（OpenCode 系「Console Go」）的校验：
// 每条 assistant 消息必须有非空 tool_calls，或有非空正文（字符串或 parts 数组）。
// 返回空串表示通过；否则返回上游原样的错误文案。
func strictUpstreamViolation(body []byte) string {
	var payload struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	for _, message := range payload.Messages {
		role, _ := message["role"].(string)
		if role != "assistant" {
			continue
		}
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			if strings.TrimSpace(content) != "" {
				continue
			}
		case []any:
			if len(content) > 0 {
				continue
			}
		}
		return "Invalid assistant message: content or tool_calls"
	}
	return ""
}

// strictUpstream 记录收到的正文，并按严格规则判决。
type strictUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newStrictUpstream(t *testing.T) *strictUpstream {
	t.Helper()
	fixture := &strictUpstream{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fixture.mu.Lock()
		fixture.bodies = append(fixture.bodies, string(body))
		fixture.mu.Unlock()

		if violation := strictUpstreamViolation(body); violation != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"`+violation+`","type":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *strictUpstream) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return ""
	}
	return f.bodies[len(f.bodies)-1]
}

func newChatHandler(t *testing.T, upstreamURL string) *Handler {
	t.Helper()
	dialClient, err := dial.New(dial.Options{})
	if err != nil {
		t.Fatalf("拨号器构造失败: %v", err)
	}
	auth := fakeAuth{user: guard.User{ID: 1, Name: "甲", IsEnabled: true}, key: guard.Key{ID: 2, Name: "k", UserID: 1}}
	handler, err := New(Options{
		Logger: logx.New(nil),
		Base: guard.Deps{
			Auth:      auth,
			Users:     auth,
			Settings:  fakeSettings{},
			Sensitive: fakeEmptySource{},
			Filters:   fakeEmptySource{},
			Provider: fakeProvider{selection: pctx.ProviderSelection{
				ProviderID: 7, Name: "严格上游", Type: string(convert.ProviderOpenAICompatible),
			}},
			MessageContext: &fakeMessageWriter{},
		},
		Candidates: chatCandidates{url: upstreamURL},
		Settlers:   func(*RequestState) Settler { return &fakeSettler{} },
		Forward:    forward.Deps{Dial: dialClient},
		Stream:     forward.StreamOptions{Budget: gate.DefaultBudget()},
	})
	if err != nil {
		t.Fatalf("数据面构造失败: %v", err)
	}
	return handler
}

// TestStrictUpstreamAcceptsThinkingOnlyAssistant 是本缺陷的端到端复现与判据：
// 入站 anthropic 请求里带一条「仅 thinking 块」的 assistant 消息，经转换后打向严格上游。
//
// 修复前：我们发出 `{"role":"assistant","content":"","reasoning_content":"…"}`，上游按
// `Invalid assistant message: content or tool_calls` 拒（用户所报现象）。
// 修复后：content 取思考文本、reasoning_content 保留，上游接受。
func TestStrictUpstreamAcceptsThinkingOnlyAssistant(t *testing.T) {
	upstream := newStrictUpstream(t)
	handler := newChatHandler(t, upstream.server.URL)

	body := `{"model":"deepseek-v4.1-flash","max_tokens":64,
		"messages":[
			{"role":"user","content":"读一下 a.txt"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"想了想要读文件","signature":"sig"}]},
			{"role":"user","content":"再来"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	sent := upstream.lastBody()
	if sent == "" {
		t.Fatal("假上游没有收到请求")
	}
	if violation := strictUpstreamViolation([]byte(sent)); violation != "" {
		t.Fatalf("上游会以此拒绝（%s）；实际发出的正文：%s", violation, sent)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("严格上游应接受，实际状态码 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(sent, `"reasoning_content"`) {
		t.Fatalf("思考必须仍然带回（否则 `reasoning_content must be passed back`）：%s", sent)
	}
}
