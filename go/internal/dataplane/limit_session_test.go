package dataplane

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件钉住「限流用本次请求的物理会话 id」这条接线：会话绑定结果由 sessionCapture 每请求自持，
// 限流服务看不到它，必须由数据面在每请求装配处注入视图。少了这层，并发额度记账会退化为
// 「每次现场生成 id」——即同一个会话的第二个请求被当成新会话（见 limit 包的 session view 测试）。

// stubSessionBinder 是会话绑定的最小实现：固定返回一个 id。
type stubSessionBinder struct{ id string }

func (b stubSessionBinder) Ensure(context.Context, guard.SessionRequest) (guard.SessionResult, error) {
	return guard.SessionResult{SessionID: b.id}, nil
}

// recordingLimiter 记录 WithSession 是否被调用，以及来源函数最终给出的会话 id。
type recordingLimiter struct {
	bound  bool
	source func(*pctx.Context) (string, error)
	seenID string
	seenAt int
}

func (l *recordingLimiter) Throttle(context.Context, string, string) (guard.ThrottleDecision, error) {
	return guard.ThrottleDecision{Allowed: true}, nil
}

func (l *recordingLimiter) RecordAuthSuccess(context.Context, string, string) {}

func (l *recordingLimiter) RecordAuthFailure(context.Context, string, string) {}

func (l *recordingLimiter) Check(_ context.Context, req *pctx.Context) (*guard.RateLimitBlock, error) {
	l.seenAt++
	if l.source != nil {
		id, err := l.source(req)
		if err != nil {
			return nil, err
		}
		l.seenID = id
	}
	return nil, nil
}

// WithSession 实现可选视图协议：返回自身，便于断言。
func (l *recordingLimiter) WithSession(source func(*pctx.Context) (string, error)) guard.RateLimiter {
	l.bound = true
	l.source = source
	return l
}

// legacyLimiter 是不支持视图协议的旧实现（只有四个必需方法）。
type legacyLimiter struct{ checks int }

func (l *legacyLimiter) Throttle(context.Context, string, string) (guard.ThrottleDecision, error) {
	return guard.ThrottleDecision{Allowed: true}, nil
}

func (l *legacyLimiter) RecordAuthSuccess(context.Context, string, string) {}

func (l *legacyLimiter) RecordAuthFailure(context.Context, string, string) {}

func (l *legacyLimiter) Check(context.Context, *pctx.Context) (*guard.RateLimitBlock, error) {
	l.checks++
	return nil, nil
}

// TestBindSessionLimiterInjectsPhysicalSessionID 是主用例：绑定后，限流拿到的是**会话绑定步骤
// 的真实结果**，而不是空串或现场生成的 id。
func TestBindSessionLimiterInjectsPhysicalSessionID(t *testing.T) {
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	limiter := &recordingLimiter{}
	deps := &guard.Deps{RateLimit: limiter}
	capture := newSessionCapture(stubSessionBinder{id: "sess-bound-1"})

	// 绑定发生在会话步骤之前：此时还没有会话身份，来源必须是空串（由限流服务兜底生成并留痕），
	// 而不是在这里编一个 id——两处各写一套生成口径会更难排障。
	bindSessionLimiter(deps, capture)
	if !limiter.bound {
		t.Fatal("实现支持视图协议时必须注入视图")
	}
	if _, err := deps.RateLimit.Check(context.Background(), pc); err != nil {
		t.Fatalf("Check 不应报错: %v", err)
	}
	if limiter.seenID != "" {
		t.Fatalf("会话尚未绑定时来源应给空串，收到 %q", limiter.seenID)
	}

	// 会话步骤跑过之后再取：必须是真实会话 id。
	if _, err := capture.Ensure(context.Background(), guard.SessionRequest{KeyID: 1}); err != nil {
		t.Fatalf("会话绑定失败: %v", err)
	}
	if _, err := deps.RateLimit.Check(context.Background(), pc); err != nil {
		t.Fatalf("Check 不应报错: %v", err)
	}
	if limiter.seenID != "sess-bound-1" {
		t.Fatalf("并发额度记账必须用本次请求的物理会话 id，收到 %q", limiter.seenID)
	}
}

// TestBindSessionLimiterKeepsLegacyImplementations 覆盖降级分支：不支持视图协议的实现保持原样，
// 既不崩也不被替换（其行为退化为现场生成会话 id，与接线前一致）。
func TestBindSessionLimiterKeepsLegacyImplementations(t *testing.T) {
	legacy := &legacyLimiter{}
	deps := &guard.Deps{RateLimit: legacy}
	bindSessionLimiter(deps, newSessionCapture(stubSessionBinder{id: "sess-x"}))
	if deps.RateLimit != legacy {
		t.Fatalf("不支持视图协议的实现不得被替换，收到 %T", deps.RateLimit)
	}
}

// TestBindSessionLimiterToleratesMissingLimiter 覆盖「限流未接线」：nil 依赖时不做任何事。
func TestBindSessionLimiterToleratesMissingLimiter(t *testing.T) {
	deps := &guard.Deps{}
	bindSessionLimiter(deps, newSessionCapture(nil))
	if deps.RateLimit != nil {
		t.Fatalf("没有限流实现时不应凭空造一个：%v", deps.RateLimit)
	}
	bindSessionLimiter(nil, newSessionCapture(nil))
}

// TestSessionCaptureWithoutBinderKeepsEmptyID 钉住无绑定实现时的取值：空串而不是错误。
func TestSessionCaptureWithoutBinderKeepsEmptyID(t *testing.T) {
	capture := newSessionCapture(nil)
	if _, err := capture.Ensure(context.Background(), guard.SessionRequest{}); err != nil {
		t.Fatalf("无实现时 Ensure 应无错: %v", err)
	}
	id, err := capture.sessionID(nil)
	if err != nil {
		t.Fatalf("sessionID 不应报错: %v", err)
	}
	if id != "" {
		t.Fatalf("未绑定过时应给空串，收到 %q", id)
	}
}
