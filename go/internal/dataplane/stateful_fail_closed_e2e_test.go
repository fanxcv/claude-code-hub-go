package dataplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync/atomic"
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

// 本文件钉住「状态型字段跨协议线 fail-closed」在 **HTTP 层**的落地：状态码、文案、结算。
//
// 为什么必须补这一层（reviewer F1）：forward 侧把错误交回来只完成了一半。串行路径曾返回
// **非 nil 的 Result + 错误**，而数据面只在 `result == nil` 分支把该拒绝翻成 400；非 nil 会
// 掉进「尝试耗尽」分支被译成 502，并且因为 forward 的结算 defer 以 len(Attempts) > 0 为前提、
// 这条路径 Attempts 恒为空，请求**完全没有终态**——客户端拿到 502、库里查不到这次请求。
// 只测 forward 包发现不了这两件事，故判据取在 HTTP 层：400 + 点名字段 + 恰好一次结算。

// statefulCrossLineCandidates 让候选落在 openai-compatible 线，逼迫 /v1/responses 的正文走
// 跨协议转换——只有会转正文的候选才谈得上「承载不了状态型字段」。
type statefulCrossLineCandidates struct{ url string }

func (c statefulCrossLineCandidates) Candidate(
	_ context.Context,
	selection pctx.ProviderSelection,
	_ SelectionFacts,
) (*forward.Candidate, route.Result, error) {
	return &forward.Candidate{
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

func (c statefulCrossLineCandidates) Failover(context.Context, SelectionFacts) (*forward.Candidate, route.Result, error) {
	return nil, route.Result{}, nil
}

// newStatefulFailClosedHandler 装配一个最小数据面：候选全是要跨线的 openai-compatible。
func newStatefulFailClosedHandler(t *testing.T, upstreamURL string) (*Handler, *fakeSettler) {
	t.Helper()
	dialClient, err := dial.New(dial.Options{})
	if err != nil {
		t.Fatalf("拨号器构造失败: %v", err)
	}
	settler := &fakeSettler{}
	handler, err := New(Options{
		Logger: logx.New(nil),
		Base: guard.Deps{
			Auth:      authorizingAuth,
			Users:     authorizingAuth,
			Settings:  fakeSettings{},
			Sensitive: fakeEmptySource{},
			Filters:   fakeEmptySource{},
			Provider: fakeProvider{selection: pctx.ProviderSelection{
				ProviderID: 7, Name: "跨线供应商", Type: string(convert.ProviderOpenAICompatible),
			}},
			MessageContext: &fakeMessageWriter{},
		},
		Candidates: statefulCrossLineCandidates{url: upstreamURL},
		Settlers:   func(*RequestState) Settler { return settler },
		Forward:    forward.Deps{Dial: dialClient},
		Stream:     forward.StreamOptions{Budget: gate.DefaultBudget()},
	})
	if err != nil {
		t.Fatalf("数据面构造失败: %v", err)
	}
	return handler, settler
}

// TestStatefulFailClosedReturns400AndSettles 是 F1 的端到端判据。
//
// 客户端发一条带 previous_response_id 的 Responses 请求，池中候选全需跨线（承载不了该字段）。
// 期望：400（不是 502）、文案点名字段、上游零拨号、且**恰好一次**终态结算（状态码同为 400）。
func TestStatefulFailClosedReturns400AndSettles(t *testing.T) {
	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		dump, _ := httputil.DumpRequest(r, false)
		t.Logf("上游不该收到请求，实际收到：%s", dump)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	handler, settler := newStatefulFailClosedHandler(t, upstream.URL)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-5","previous_response_id":"resp_1","input":"继续"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态码应为 400（客户端改道可自救），收到 %d：%s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, "previous_response_id") {
		t.Fatalf("文案必须点名冲突字段，实际：%s", body)
	}
	if hits := atomic.LoadInt32(&upstreamHits); hits != 0 {
		t.Fatalf("承载不了的候选被拨号 %d 次", hits)
	}

	settler.mu.Lock()
	settlements := append([]settledNonStream(nil), settler.nonStream...)
	settler.mu.Unlock()
	if len(settlements) != 1 {
		t.Fatalf("fail-closed 请求必须恰好结算一次（否则库里查不到这次请求），实际 %d 次：%+v",
			len(settlements), settlements)
	}
	if settlements[0].Status != http.StatusBadRequest {
		t.Fatalf("结算状态码应为 400，实际 %d", settlements[0].Status)
	}
}
