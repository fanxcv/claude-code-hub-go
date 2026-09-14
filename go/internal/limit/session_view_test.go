package limit

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
)

// TestIntegrationWithSessionBindsPhysicalSessionID 钉住视图语义：并发额度记账用的是**调用方给的**
// 物理会话 id，而不是现场生成的。
//
// 判据取两侧都成立的形态：同一会话重复请求放行（这正是原子性带来的收益），换一个会话就触顶。
// 若视图没生效（仍走现场生成），第一步的第二次请求就会被当成新会话而拒绝——反证成立。
func TestIntegrationWithSessionBindsPhysicalSessionID(t *testing.T) {
	client := integrationClient(t)
	keyID, userID := uniqueID(t), uniqueID(t)+7
	cleanupKeys(t, client, KeyActiveSessionsKey(keyID), UserActiveSessionsKey(userID))
	ctx := context.Background()

	service := newSessionScopedService(t, client, keyID, userID)

	sameSession := service.WithSession(func(*pctx.Context) (string, error) { return "sess-fixed", nil })
	for round := 1; round <= 2; round++ {
		block, err := sameSession.Check(ctx, sessionScopedRequest(t, keyID, userID))
		if err != nil {
			t.Fatalf("第 %d 次检查不应是故障: %v", round, err)
		}
		if block != nil {
			t.Fatalf("同一会话的第 %d 次请求应放行（并发上限 1 只对第二个会话生效），收到 %+v", round, block)
		}
	}

	other := service.WithSession(func(*pctx.Context) (string, error) { return "sess-other", nil })
	block, err := other.Check(ctx, sessionScopedRequest(t, keyID, userID))
	if err != nil {
		t.Fatalf("拦截不应是步骤故障: %v", err)
	}
	if block == nil {
		t.Fatal("第二个会话应被并发上限拒绝；放行说明视图给的 id 没被采纳")
	}
	if block.Status != http.StatusTooManyRequests {
		t.Fatalf("并发超限应返回 429，收到 %d", block.Status)
	}
	if block.ErrorType != "rate_limit_error" {
		t.Fatalf("错误类型应为 rate_limit_error，收到 %q", block.ErrorType)
	}
	if !strings.Contains(block.BlockedReason, "concurrent_sessions") {
		t.Fatalf("拦截理由应记并发维度，收到 %q", block.BlockedReason)
	}
	if block.RetryAfterSeconds == nil {
		t.Fatal("并发超限应带重试秒数（守卫步骤据此写 Retry-After）")
	}
}

// TestIntegrationWithSessionNilSourceFallsBackToGenerated 记录**兜底语义的代价**：来源为 nil 时
// 每次检查都现场生成会话 id，于是「同一个会话」的第二个请求被当成新会话——在并发上限 1 下直接被拒。
//
// 这条不是缺陷断言而是行为钉住：会话绑定没接线时会过严（而不是放行），与 Node 的差异必须可见。
func TestIntegrationWithSessionNilSourceFallsBackToGenerated(t *testing.T) {
	client := integrationClient(t)
	keyID, userID := uniqueID(t), uniqueID(t)+9
	cleanupKeys(t, client, KeyActiveSessionsKey(keyID), UserActiveSessionsKey(userID))
	ctx := context.Background()

	service := newSessionScopedService(t, client, keyID, userID)
	fallback := service.WithSession(nil)

	if block, err := fallback.Check(ctx, sessionScopedRequest(t, keyID, userID)); err != nil || block != nil {
		t.Fatalf("首次请求应放行: block=%+v err=%v", block, err)
	}
	block, err := fallback.Check(ctx, sessionScopedRequest(t, keyID, userID))
	if err != nil {
		t.Fatalf("第二次检查不应是故障: %v", err)
	}
	if block == nil {
		t.Fatal("兜底路径每次生成新 id，第二个请求应被当成新会话而触顶；放行说明兜底没生效")
	}
}

// newSessionScopedService 建一个只带并发上限的限流服务（不碰账本回退与成本窗口）。
func newSessionScopedService(t *testing.T, client *ratelimit.Client, keyID, userID int64) *Service {
	t.Helper()
	service, err := New(Config{
		Quotas: &fakeQuotaSource{
			key:  KeyQuota{KeyID: keyID, KeyHash: "sk-session-view"},
			user: UserQuota{UserID: userID, LimitConcurrentSessions: 1},
		},
		Redis:    client,
		Location: time.UTC,
		Logger:   logx.New(io.Discard),
	})
	if err != nil {
		t.Fatalf("组装限流服务失败: %v", err)
	}
	return service
}

// sessionScopedRequest 造一个已鉴权的请求上下文（限流在无 key/user 时直接放行）。
func sessionScopedRequest(t *testing.T, keyID, userID int64) *pctx.Context {
	t.Helper()
	request, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	request.SetAuth(pctx.AuthState{KeyID: keyID, UserID: userID})
	return request
}
