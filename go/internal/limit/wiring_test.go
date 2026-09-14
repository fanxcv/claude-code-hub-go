package limit

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// TestServiceBlocksOnRealGuardChain 钉住限流接线的真实组合：真实限流服务 + 真实守卫步骤。
//
// 只组装限流那一步（StepRateLimit），不掺任何假的 auth/settings：本测试要证的是
// 「limit 的判定经守卫步骤翻译后，客户端看到的状态码、Retry-After 与文案都正确」，
// 而不是再测一遍守卫链的步骤顺序。
func TestServiceBlocksOnRealGuardChain(t *testing.T) {
	client := integrationClient(t)
	keyID, userID := uniqueID(t), uniqueID(t)+3
	cleanupKeys(t, client, KeyActiveSessionsKey(keyID), UserActiveSessionsKey(userID), RPMWindowKey(userID))

	service, err := New(Config{
		Quotas: &fakeQuotaSource{
			key:  KeyQuota{KeyID: keyID, KeyHash: "sk-wiring"},
			user: UserQuota{UserID: userID, RPM: 1},
		},
		Redis:    client,
		Location: time.UTC,
		Logger:   logx.New(nil),
	})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}

	chain, err := guard.Build(
		guard.Pipeline{Name: "limit-wiring", Steps: []guard.StepKey{guard.StepRateLimit}},
		guard.Deps{RateLimit: service, Logger: logx.New(nil)}.Steps(),
	)
	if err != nil {
		t.Fatalf("组装守卫链失败: %v", err)
	}

	run := func() (*guard.Response, error) {
		request, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
		if err != nil {
			t.Fatalf("构造上下文失败: %v", err)
		}
		request.SetAuth(pctx.AuthState{KeyID: keyID, UserID: userID})
		return chain.Run(request)
	}

	if response, err := run(); err != nil || response != nil {
		t.Fatalf("RPM 首请求应放行: response=%+v err=%v", response, err)
	}

	response, err := run()
	if err != nil {
		t.Fatalf("限流拦截不应是步骤故障: %v", err)
	}
	if response == nil || response.Status != http.StatusTooManyRequests {
		t.Fatalf("超限应返回 429，得到 %+v", response)
	}
	if got := response.Headers.Get("Retry-After"); got == "" {
		t.Fatalf("超限响应应带 Retry-After: %v", response.Headers)
	}
	body := string(response.Body)
	if !strings.Contains(body, `"type":"rate_limit_error"`) {
		t.Fatalf("错误类型不符: %s", body)
	}
	// 文案对齐 Node：触发维度写在判定（BlockedReason）里，客户端可见的是这条频率超限文案。
	if !strings.Contains(body, "请求频率超限") {
		t.Fatalf("错误文案不符: %s", body)
	}
}
