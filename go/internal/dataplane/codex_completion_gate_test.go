package dataplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住「Codex 会话补全闸门」的两因子接线：端点因子由数据面按**路由**注入
// （guard.Deps.EndpointRawPassthrough），会话守卫再与系统设置取与。
//
// 根因回归：Go 曾把闸门直接赋成系统设置值，丢掉 Node 的端点因子
// （session.ts:574-582：`endpointPolicy.allowRawCrossProviderFallback && 设置`）。
// 生产该设置为 true，于是 /v1/responses 的闸门被永久关死、prompt_cache_key 从不注入
// ——本文件第一个用例就是那个症状的正例。

// codexGateSettings 返回可调的设置快照（只看补全与原始回退两列）。
type codexGateSettings struct {
	codexCompletion  bool
	allowRawFallback bool
}

func (s codexGateSettings) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return &store.SystemSettings{
		EnableCodexSessionIDCompletion:               s.codexCompletion,
		AllowNonConversationEndpointProviderFallback: s.allowRawFallback,
	}, nil
}

// countingCodexCompleter 记录补全被调用了几次。
type countingCodexCompleter struct {
	calls int
}

func (c *countingCodexCompleter) Complete(
	_ context.Context,
	_ guard.CodexSessionCompletionRequest,
) (guard.CodexSessionCompletionResult, error) {
	c.calls++
	return guard.CodexSessionCompletionResult{
		Applied:   true,
		Action:    "completed_missing_fields",
		Source:    "header_session_id",
		SessionID: "01a0a2a1-c7ff-7747-81cc-4e27411e8938",
	}, nil
}

// newCodexGateHandler 装配一个最小数据面：只关心闸门（补全开关与原始回退都打开）。
func newCodexGateHandler(t *testing.T, completer *countingCodexCompleter) *Handler {
	t.Helper()
	dialClient, err := dial.New(dial.Options{})
	if err != nil {
		t.Fatalf("拨号器构造失败: %v", err)
	}
	auth := fakeAuth{
		user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
		key:  guard.Key{ID: 2, Name: "k", UserID: 1},
	}
	handler, err := New(Options{
		Logger: logx.New(nil),
		Base: guard.Deps{
			Auth:      auth,
			Users:     auth,
			Settings:  codexGateSettings{codexCompletion: true, allowRawFallback: true},
			Sensitive: fakeEmptySource{},
			Filters:   fakeEmptySource{},
			Provider: fakeProvider{selection: pctx.ProviderSelection{
				ProviderID: 7, Name: "假供应商", Type: "openai-compatible",
			}},
			MessageContext:  &fakeMessageWriter{},
			CodexCompletion: completer,
		},
		// 上游不可达是刻意的：本用例只断言守卫链里的补全是否被调用，
		// 转发失败与否不影响该断言（失败会走 503 分支，仍算「已过会话步骤」）。
		Candidates: &typedCandidates{url: "http://127.0.0.1:1", providerType: "openai-compatible"},
		Settlers:   func(*RequestState) Settler { return &fakeSettler{} },
		Forward:    forward.Deps{Dial: dialClient},
	})
	if err != nil {
		t.Fatalf("数据面构造失败: %v", err)
	}
	return handler
}

// TestCodexCompletionGateEndToEnd 是生产症状的端到端回归：设置开启原始回退时，
// /v1/responses（普通端点）必须照常补全，count_tokens 与 responses/compact（原始透传端点）必须跳过。
func TestCodexCompletionGateEndToEnd(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		wantCall bool
	}{
		{"普通端点 /v1/responses：闸门开", "/v1/responses", true},
		{"原始透传端点 count_tokens：闸门关", "/v1/messages/count_tokens", false},
		{"原始透传端点 responses/compact：闸门关", "/v1/responses/compact", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			completer := &countingCodexCompleter{}
			handler := newCodexGateHandler(t, completer)

			body := `{"model":"gpt-5-codex","input":[{"type":"message","content":"ping"}]}`
			request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("x-api-key", "fake-client-key")
			handler.ServeHTTP(httptest.NewRecorder(), request)

			if got := completer.calls > 0; got != testCase.wantCall {
				t.Fatalf("%s 的补全执行 = %v，期望 %v（实际调用 %d 次）",
					testCase.path, got, testCase.wantCall, completer.calls)
			}
		})
	}
}
