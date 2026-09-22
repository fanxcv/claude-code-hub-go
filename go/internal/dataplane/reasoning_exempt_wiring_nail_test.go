package dataplane

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ingress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件钉住「推理型请求豁免提交前速率闸」这条链**在 dataplane 侧的两跳**。
//
// 为什么必须有源码钉子：「本次是不是推理型」是逐请求事实，而 StreamOptions 的装配是
// 进程级的一次赋值 + 逐请求的几行覆盖（dataplane.go 的 streamOptions 段）。摘掉那一行覆盖，
// 两侧的单测**全绿**——判据函数自己测得出 true、forward 的豁免自己测得出 θ=0——而真实请求上
// 豁免静默失效（StackOptions.ReasoningRequest 恒为零值）。这正是本仓反复的「配置在最后一跳
// 丢失」缺陷（同类见同目录 slow_probe_wiring_nail_test.go / slow_post_commit_wiring_nail_test.go）。
var reasoningRequestWiring = regexp.MustCompile(
	`(?m)^\s*streamOptions\.ReasoningRequest = state\.requestSeeksReasoning\s*$`)

var reasoningRequestCaptureWiring = regexp.MustCompile(
	`(?m)^\s*state\.requestSeeksReasoning = specialsettings\.RequestSeeksReasoning\(parsed, spec\.Format, state\.PC\.Path\(\)\)\s*$`)

func TestStreamOptionsReceiveReasoningRequestFlag(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("dataplane.go"))
	if err != nil {
		t.Fatalf("读取 dataplane.go 失败：%v", err)
	}
	if !reasoningRequestWiring.Match(source) {
		t.Fatalf("dataplane.go 里找不到「推理型事实 → forward.StreamOptions.ReasoningRequest」的接线：\n" +
			"  期望形如 `streamOptions.ReasoningRequest = state.requestSeeksReasoning`\n" +
			"  该行缺失时推理型请求的 θ 不被清零，速率闸照旧裁决（豁免静默失效）。")
	}

	probe, err := os.ReadFile(filepath.Join("effort_probe.go"))
	if err != nil {
		t.Fatalf("读取 effort_probe.go 失败：%v", err)
	}
	if !reasoningRequestCaptureWiring.Match(probe) {
		t.Fatalf("effort_probe.go 里找不到「请求正文 → 逐请求事实」的判据接线：\n" +
			"  期望形如 `state.requestSeeksReasoning = specialsettings.RequestSeeksReasoning(parsed, spec.Format, state.PC.Path())`\n" +
			"  该行缺失时上面那行恒把零值搬进 StreamOptions，豁免静默失效。")
	}
}

// TestCaptureRequestedEffortMarksReasoningRequest 是这条链的**行为**钉子：判据必须由
// captureRequestedEffort 从真实请求正文（经 pctx → 守卫链的正文访问器）算出并落在请求状态上。
//
// 为何不能只留源码正则：正则证明「那行字还在」，但不证明判据读的是**本次请求的正文**——
// 传错 format/路径或读到别人的 body 都会让正则绿着而行为错。
func TestCaptureRequestedEffortMarksReasoningRequest(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		format convert.ClientFormat
		body   string
		want   bool
	}{
		{
			// 生产命中的那一格：codex 线 reasoning.effort（special_settings 的 codex_reasoning_effort）。
			name:   "responses 线 codex 推理档",
			path:   "/v1/responses",
			format: convert.FormatResponse,
			body:   `{"model":"gpt-5-codex","reasoning":{"effort":"max"},"input":[]}`,
			want:   true,
		},
		{
			name:   "anthropic 线扩展思考",
			path:   "/v1/messages",
			format: convert.FormatClaude,
			body:   `{"model":"claude-sonnet-4-5","thinking":{"type":"enabled","budget_tokens":8192}}`,
			want:   true,
		},
		{
			name:   "anthropic 线无思考",
			path:   "/v1/messages",
			format: convert.FormatClaude,
			body:   `{"model":"claude-sonnet-4-5","max_tokens":16}`,
			want:   false,
		},
		{
			name:   "openai 线 reasoning_effort 显式关掉",
			path:   "/v1/chat/completions",
			format: convert.FormatOpenAI,
			body:   `{"model":"gpt-5","reasoning_effort":"none"}`,
			want:   false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase := testCase
			pc := newReasoningProbeContext(t, testCase.path, testCase.body)
			state := &RequestState{PC: pc}
			handler := &Handler{}
			body := newBodyAccess(context.Background(), pc, guard.BodyAccessOptions{Ingress: ingress.DefaultOptions()})

			handler.captureRequestedEffort(state, routeSpec{Format: testCase.format, Path: testCase.path}, body)

			if state.requestSeeksReasoning != testCase.want {
				t.Fatalf("requestSeeksReasoning = %v，期望 %v（path=%s body=%s）",
					state.requestSeeksReasoning, testCase.want, testCase.path, testCase.body)
			}
		})
	}
}

// newReasoningProbeContext 造一个带正文的请求上下文，供 captureRequestedEffort 走真实读取路径。
func newReasoningProbeContext(t *testing.T, path string, body string) *pctx.Context {
	t.Helper()
	pc, err := pctx.New(pctx.Init{
		Method:  http.MethodPost,
		Path:    path,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    io.NopCloser(strings.NewReader(body)),
	})
	if err != nil {
		t.Fatalf("构造 pctx 失败：%v", err)
	}
	return pc
}
