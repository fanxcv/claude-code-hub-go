package limit

import (
	"context"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件钉住 ENABLE_RATE_LIMIT=false 的语义（Config.Disabled）：只放行拦截，不跳过判定。
//
// 为什么布尔形状是「判定照跑、丢弃拦截」而不是「短路放行」：同一个 Service 兼任防爆破节流、
// 租约结算与供应商额度三职（装配见 cmd/cchd）。短路判定会连租约切片一起不做，终态结算侧就
// 没有计划可扣——那是把账务旁路一起关掉，远超「关限流」的字面语义。

// disabledTestService 组装一个「必拦截」的服务，开关由参数给定。
// 拦截条件是 TestCheckKeyTotalLimitBlocksWith402 的那一份：账本已花 $10.5、硬上限 $5。
func disabledTestService(t *testing.T, disabled bool) (*Service, *fakeQuotaSource) {
	t.Helper()
	quotas := &fakeQuotaSource{key: KeyQuota{KeyID: 1, KeyHash: "sk-total", LimitTotalUSD: floatPtr(5)}}
	ledger := &fakeLedger{totals: map[string]float64{"key/sk-total": 10.5}}
	service, err := New(Config{
		Quotas:     quotas,
		Ledger:     ledger,
		Disabled:   disabled,
		Location:   time.UTC,
		Now:        func() time.Time { return fixedNow },
		SessionID:  func(*pctx.Context) (string, error) { return "sess_test", nil },
		SessionTTL: 300 * time.Second,
	})
	if err != nil {
		t.Fatalf("组装限流服务失败: %v", err)
	}
	return service, quotas
}

func TestRateLimitDisabledPassesButStillJudges(t *testing.T) {
	// 先立对照：同一份「必拦截」的装配，开关为 true 时必须拦住——否则下面的放行断言可能
	// 只是「这份装配本来就不拦」，测不到 Disabled 的效果。
	enabled, _ := disabledTestService(t, false)
	block, err := enabled.Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil {
		t.Fatal("开关为 true 时总额度触顶应拦截（对照失效）")
	}

	service, quotas := disabledTestService(t, true)
	block, err = service.Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block != nil {
		t.Fatalf("开关为 false 时不得拦截，得到 %+v", block)
	}
	// 判定必须真的跑过（读了限额快照），否则租约切片与并发记账这些判定副作用也会一起丢，
	// 终态结算侧便没有计划可扣。
	if quotas.keyCalls == 0 {
		t.Fatal("开关为 false 时仍须读限额快照（只放行拦截，不跳过判定）")
	}
}

// WithSession 的每请求视图是另一个判定入口（wire.go），必须同样受 Disabled 约束，
// 否则会话绑定路径会绕开开关。
func TestRateLimitDisabledPassesInSessionBoundView(t *testing.T) {
	service, _ := disabledTestService(t, true)
	bound := service.WithSession(nil)
	block, err := bound.Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block != nil {
		t.Fatalf("每请求视图同样不得拦截，得到 %+v", block)
	}
}
