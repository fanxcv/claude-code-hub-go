package guard

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// fakeWarmupArtifacts 记录 warmup 抢答的工件写入。
type fakeWarmupArtifacts struct {
	requests []WarmupArtifactRequest
	err      error
}

func (f *fakeWarmupArtifacts) StoreWarmupResponse(
	_ context.Context, request WarmupArtifactRequest,
) error {
	f.requests = append(f.requests, request)
	return f.err
}

// newSessionStepContext 造一个走 session 步骤的上下文；协议族决定注入是否命中 Claude 线。
func newSessionStepContext(t *testing.T, path string, family egress.Family) *pctx.Context {
	t.Helper()
	pc, err := pctx.New(pctx.Init{
		Method:       "POST",
		Path:         path,
		ProtocolFrom: family,
		Logger:       quietLogger(),
	})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	pc.SetAuth(pctx.AuthState{KeyID: 42, UserID: 7, APIKey: "sk-guard", KeyName: "k", UserName: "u"})
	return pc
}

// sessionStepDeps 造一套只接本文件所需缝隙的依赖。
func sessionStepDeps(
	t *testing.T, body map[string]any, settings settingsStub, session *fakeBinder,
) (Deps, *fakeBody) {
	t.Helper()
	factory, access := bodyFactory(t, body)
	deps := Deps{
		Logger:         quietLogger(),
		Settings:       fakeSettings{settings: settings},
		Sessions:       session,
		Body:           factory,
		RequestContext: func(*pctx.Context) context.Context { return context.Background() },
		ClaudeMetadata: func(b map[string]any, keyID int64, sessionID, userAgent string) bool {
			return injectTestClaudeMetadata(b, keyID, sessionID, userAgent)
		},
	}
	return deps, access
}

// TestSessionStepClaudeMetadataGates 钉住注入的四个门：开关、原始端点回退、协议族、Codex 形态。
func TestSessionStepClaudeMetadataGates(t *testing.T) {
	settings := func(enabled bool) settingsStub {
		return settingsStub{claudeMetadata: enabled}
	}

	t.Run("开关打开 + Claude 线：注入", func(t *testing.T) {
		body := map[string]any{"messages": []any{}}
		session := &fakeBinder{result: SessionResult{SessionID: "sess-1", Sequence: 3}}
		deps, access := sessionStepDeps(t, body, settings(true), session)
		if _, err := deps.sessionStep()(newSessionStepContext(t, "/v1/messages", egress.FamilyAnthropicMessages)); err != nil {
			t.Fatalf("步骤失败: %v", err)
		}
		metadata, ok := access.current["metadata"].(map[string]any)
		if !ok || metadata["user_id"] == nil {
			t.Fatalf("未注入 metadata.user_id: %#v", access.current)
		}
		if access.stores == 0 {
			t.Fatal("注入后未写回正文——出站请求会拿到未注入的那份")
		}
	})

	t.Run("开关关闭：不注入", func(t *testing.T) {
		body := map[string]any{"messages": []any{}}
		session := &fakeBinder{result: SessionResult{SessionID: "sess-1", Sequence: 3}}
		deps, access := sessionStepDeps(t, body, settings(false), session)
		if _, err := deps.sessionStep()(newSessionStepContext(t, "/v1/messages", egress.FamilyAnthropicMessages)); err != nil {
			t.Fatalf("步骤失败: %v", err)
		}
		if _, ok := access.current["metadata"]; ok {
			t.Fatalf("开关关闭却写入了 metadata: %#v", access.current)
		}
		if access.stores != 0 {
			t.Fatal("开关关闭却写回了正文")
		}
	})

	t.Run("非 Claude 线（openai-chat）：不注入", func(t *testing.T) {
		body := map[string]any{"messages": []any{}}
		session := &fakeBinder{result: SessionResult{SessionID: "sess-1", Sequence: 3}}
		deps, access := sessionStepDeps(t, body, settings(true), session)
		if _, err := deps.sessionStep()(newSessionStepContext(t, "/v1/chat/completions", egress.FamilyOpenAIChat)); err != nil {
			t.Fatalf("步骤失败: %v", err)
		}
		if _, ok := access.current["metadata"]; ok {
			t.Fatalf("非 Claude 线却写入了 metadata: %#v", access.current)
		}
	})

	t.Run("Codex 形态（有 input 数组）：即使走 Claude 线也不注入", func(t *testing.T) {
		body := map[string]any{"input": []any{}}
		session := &fakeBinder{result: SessionResult{SessionID: "sess-1", Sequence: 3}}
		deps, access := sessionStepDeps(t, body, settings(true), session)
		if _, err := deps.sessionStep()(newSessionStepContext(t, "/v1/messages", egress.FamilyAnthropicMessages)); err != nil {
			t.Fatalf("步骤失败: %v", err)
		}
		if _, ok := access.current["metadata"]; ok {
			t.Fatalf("Codex 形态却写入了 metadata: %#v", access.current)
		}
	})

	t.Run("原始端点跨供应商回退：不注入", func(t *testing.T) {
		body := map[string]any{"messages": []any{}}
		session := &fakeBinder{result: SessionResult{SessionID: "sess-1", Sequence: 3}}
		stub := settings(true)
		stub.allowRawFallback = true
		deps, access := sessionStepDeps(t, body, stub, session)
		if _, err := deps.sessionStep()(newSessionStepContext(t, "/v1/messages", egress.FamilyAnthropicMessages)); err != nil {
			t.Fatalf("步骤失败: %v", err)
		}
		if _, ok := access.current["metadata"]; ok {
			t.Fatalf("原始回退却写入了 metadata: %#v", access.current)
		}
	})

	t.Run("缝隙未接线：跳过而不报错", func(t *testing.T) {
		body := map[string]any{"messages": []any{}}
		session := &fakeBinder{result: SessionResult{SessionID: "sess-1", Sequence: 3}}
		deps, access := sessionStepDeps(t, body, settings(true), session)
		deps.ClaudeMetadata = nil
		if _, err := deps.sessionStep()(newSessionStepContext(t, "/v1/messages", egress.FamilyAnthropicMessages)); err != nil {
			t.Fatalf("步骤失败: %v", err)
		}
		if _, ok := access.current["metadata"]; ok {
			t.Fatalf("未接线却写入了 metadata: %#v", access.current)
		}
	})
}

// runSessionThenWarmup 按链上顺序跑「会话步骤 → 预热步骤」。
//
// 顺序是必须的：预热步骤读的 ctx.ShouldPersistDebugArtifacts 由会话步骤按高并发开关设置
// （链上两者紧邻，会话在前），单独跑预热步骤会读到「默认关闭」而永远不落工件。
func runSessionThenWarmup(
	t *testing.T, deps Deps, pc *pctx.Context,
) (*Response, error) {
	t.Helper()
	if _, err := deps.sessionStep()(pc); err != nil {
		t.Fatalf("会话步骤失败: %v", err)
	}
	return deps.warmupStep()(pc)
}

// TestWarmupStepStoresArtifacts 钉住抢答的四条工件：开时写、高并发时不写、无会话时不写。
//
// 顺带钉住两步骤的联动：预热请求**不该**被注入 Claude metadata（Node 的 warmupMaybeIntercepted
// 门），而这条门要读到会话步骤写下的同一个会话身份。
func TestWarmupStepStoresArtifacts(t *testing.T) {
	warmupBody := func() map[string]any {
		return map[string]any{
			"model": "claude-sonnet-4",
			"messages": []any{map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "text", "text": "warmup",
					"cache_control": map[string]any{"type": "ephemeral"},
				}},
			}},
		}
	}

	t.Run("抢答且允许工件：写四条", func(t *testing.T) {
		artifacts := &fakeWarmupArtifacts{}
		body := warmupBody()
		session := &fakeBinder{result: SessionResult{SessionID: "sess-w", Sequence: 5}}
		deps, _ := sessionStepDeps(t, body, settingsStub{interceptWarmup: true}, session)
		deps.WarmupArtifacts = artifacts
		deps.SessionLookup = session.lookupResult
		deps.WarmupLog = &fakeWarmupLog{}

		pc := newSessionStepContext(t, "/v1/messages", egress.FamilyAnthropicMessages)
		response, err := runSessionThenWarmup(t, deps, pc)
		if err != nil {
			t.Fatalf("步骤失败: %v", err)
		}
		if response == nil || response.Status != 200 {
			t.Fatalf("未抢答: %+v", response)
		}
		if _, injected := body["metadata"]; injected {
			t.Fatalf("预热请求不该被注入 metadata: %#v", body)
		}
		if len(artifacts.requests) != 1 {
			t.Fatalf("工件写入次数 = %d，期望 1", len(artifacts.requests))
		}
		got := artifacts.requests[0]
		if got.SessionID != "sess-w" || got.Sequence != 5 || got.KeyID != 42 || got.StatusCode != 200 {
			t.Fatalf("工件输入不符: %+v", got)
		}
		if got.Method != "POST" {
			t.Fatalf("method = %q", got.Method)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(got.Body), &payload); err != nil {
			t.Fatalf("抢答体不是合法 JSON: %v", err)
		}
		if payload["type"] != "message" {
			t.Fatalf("抢答体形状不符: %#v", payload)
		}
	})

	t.Run("高并发模式（禁调试工件）：不写", func(t *testing.T) {
		artifacts := &fakeWarmupArtifacts{}
		session := &fakeBinder{result: SessionResult{SessionID: "sess-w", Sequence: 5}}
		deps, _ := sessionStepDeps(t, warmupBody(),
			settingsStub{interceptWarmup: true, highConcurrency: true}, session)
		deps.WarmupArtifacts = artifacts
		deps.SessionLookup = session.lookupResult
		deps.WarmupLog = &fakeWarmupLog{}

		if _, err := runSessionThenWarmup(t, deps, newSessionStepContext(t, "/v1/messages", egress.FamilyAnthropicMessages)); err != nil {
			t.Fatalf("步骤失败: %v", err)
		}
		if len(artifacts.requests) != 0 {
			t.Fatalf("高并发模式仍写了工件: %+v", artifacts.requests)
		}
	})

	t.Run("无会话身份：不写也不报错", func(t *testing.T) {
		artifacts := &fakeWarmupArtifacts{}
		session := &fakeBinder{}
		deps, _ := sessionStepDeps(t, warmupBody(), settingsStub{interceptWarmup: true}, session)
		deps.WarmupArtifacts = artifacts
		deps.SessionLookup = session.lookupResult
		deps.WarmupLog = &fakeWarmupLog{}

		response, err := runSessionThenWarmup(t, deps, newSessionStepContext(t, "/v1/messages", egress.FamilyAnthropicMessages))
		if err != nil {
			t.Fatalf("步骤失败: %v", err)
		}
		if response == nil {
			t.Fatal("仍应抢答（工件是旁路）")
		}
		if len(artifacts.requests) != 0 {
			t.Fatalf("无会话身份仍写了工件: %+v", artifacts.requests)
		}
	})
}

// injectTestClaudeMetadata 是测试用的最小注入器：只做 user_id 的有无判定与写入，
// 不经 session 包（守卫测试不跨包断言形制，形制由 session 包自己的用例钉住）。
func injectTestClaudeMetadata(body map[string]any, keyID int64, sessionID, userAgent string) bool {
	if body == nil || keyID == 0 || sessionID == "" {
		return false
	}
	metadata, _ := body["metadata"].(map[string]any)
	if metadata != nil {
		if text, ok := metadata["user_id"].(string); ok && text != "" {
			return false
		}
	}
	if metadata == nil {
		metadata = map[string]any{}
		body["metadata"] = metadata
	}
	metadata["user_id"] = sessionID
	return true
}
