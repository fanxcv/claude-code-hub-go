package adminapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件覆盖「通知调度器接线」的无库部分：重排触发点、任务类型映射、投递的错路径。
// 真库路径（目标解析、跳过被停用目标）见 notifications_integration_test.go 的同名主题。

// fakeNotifyRescheduler 记录重排调用次数。
type fakeNotifyRescheduler struct {
	calls int
	err   error
}

func (r *fakeNotifyRescheduler) Reschedule(context.Context) error {
	r.calls++
	return r.err
}

func TestNotificationRescheduleCallsWiredScheduler(t *testing.T) {
	// 装配了就重排（Node 的 scheduleNotifications 调用点语义）。
	scheduler := &fakeNotifyRescheduler{}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/notifications/settings", nil)
	notificationReschedule(Deps{Logger: logx.New(nil), NotifyScheduler: scheduler}, request)
	if scheduler.calls != 1 {
		t.Fatalf("应重排一次，实际 %d", scheduler.calls)
	}

	// 重排失败必须 fail-open：只记日志，不 panic（调用方响应已经发出）。
	broken := &fakeNotifyRescheduler{err: errors.New("redis down")}
	notificationReschedule(Deps{Logger: logx.New(nil), NotifyScheduler: broken}, request)
	if broken.calls != 1 {
		t.Fatalf("失败路径也应尝试一次，实际 %d", broken.calls)
	}
}

func TestNotificationRescheduleWithoutSchedulerIsNoop(t *testing.T) {
	// 未装配：只记 warn，不 panic（与 Node 的 fail-open 同判）。
	request := httptest.NewRequest(http.MethodPut, "/api/v1/notifications/settings", nil)
	notificationReschedule(Deps{Logger: logx.New(nil)}, request)
	// Logger 为 nil 时也必须安全（装配早期的调用）。
	notificationReschedule(Deps{}, request)
}

func TestNotificationTypeFromJobType(t *testing.T) {
	cases := map[string]string{
		jobs.NotifyTypeCircuitBreaker:    "circuit_breaker",
		jobs.NotifyTypeDailyLeaderboard:  "daily_leaderboard",
		jobs.NotifyTypeCostAlert:         "cost_alert",
		jobs.NotifyTypeCacheHitRateAlert: "cache_hit_rate_alert",
	}
	for jobType, want := range cases {
		got, ok := notificationTypeFromJobType(jobType)
		if !ok || got != want {
			t.Fatalf("%s 应映射为 %s，实际 %q ok=%t", jobType, want, got, ok)
		}
	}
	if _, ok := notificationTypeFromJobType("unknown"); ok {
		t.Fatal("未知类型应被拒（Node 的 switch 无 default，会抛错）")
	}
}

func TestNotificationDeliveryRejectsUnsupportedLegacyHost(t *testing.T) {
	// Node 的 detectProvider 只认企业微信与飞书：其它主机名一律报错（任务层会退避重试）。
	delivery := NewNotificationDelivery(nil, logx.New(nil))
	_, err := delivery.Deliver(context.Background(), jobs.NotifyDeliveryRequest{
		Type:       jobs.NotifyTypeCostAlert,
		WebhookURL: "https://example.test/hook",
	})
	if err == nil {
		t.Fatal("不支持的主机名应报错")
	}
}

func TestNotificationDeliveryRejectsMissingDestination(t *testing.T) {
	delivery := NewNotificationDelivery(nil, logx.New(nil))
	// 既无 URL 也无目标 id：Node 抛 "Missing notification destination"。
	if _, err := delivery.Deliver(context.Background(), jobs.NotifyDeliveryRequest{
		Type: jobs.NotifyTypeCostAlert,
	}); err == nil {
		t.Fatal("缺投递目的地应报错")
	}
	// 有目标 id 但没有池：如实报错（而不是静默成功）。
	if _, err := delivery.Deliver(context.Background(), jobs.NotifyDeliveryRequest{
		Type:     jobs.NotifyTypeCostAlert,
		TargetID: 1,
	}); err == nil {
		t.Fatal("未装配连接池应报错")
	}
	// 未知类型：报错并带上类型名，便于定位。
	if _, err := delivery.Deliver(context.Background(), jobs.NotifyDeliveryRequest{
		Type:       "unknown",
		WebhookURL: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send",
	}); err == nil {
		t.Fatal("未知类型应报错")
	}
}

func TestNotificationAlertsReportsNoDataUntilGeneratorsPorted(t *testing.T) {
	alerts := NewNotificationAlerts(logx.New(nil))
	payload, ok, err := alerts.Payload(context.Background(), jobs.NotifyPayloadRequest{
		Type:     jobs.NotifyTypeDailyLeaderboard,
		Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("无数据不该是错误: %v", err)
	}
	if ok || len(payload) != 0 {
		t.Fatalf("生成器未移植前应回「无数据」: ok=%t payload=%s", ok, payload)
	}
	if ref := notificationPayloadSourceRef(jobs.NotifyTypeCostAlert); ref == "" {
		t.Fatal("未移植的生成器应带上 TS 位置")
	}
}
