package jobs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// fakeMultiNotifyPayloads 一次给出多份 payload（成本预警多个对象超限时的形状）。
type fakeMultiNotifyPayloads struct {
	items []NotifyPayload
	calls int
}

func (p *fakeMultiNotifyPayloads) Payloads(
	context.Context,
	NotifyPayloadRequest,
) ([]NotifyPayload, error) {
	p.calls++
	return p.items, nil
}

// fakeNotifyCooldown 记录被写下的冷却键。
type fakeNotifyCooldown struct {
	keys []string
	ttl  time.Duration
	err  error
}

func (c *fakeNotifyCooldown) Set(_ context.Context, keys []string, ttl time.Duration) error {
	c.keys = append(c.keys, keys...)
	c.ttl = ttl
	return c.err
}

// notifyFanoutScheduler 装配一条「成本预警」任务，数据生成器给多份。
func notifyFanoutScheduler(
	t *testing.T,
	payloads NotifyPayloadSource,
	deliverer NotifyDeliverer,
	cooldown NotifyCooldownWriter,
) (*NotifyScheduler, *fakeNotifyClock) {
	t.Helper()
	clock := &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	options := NotifySchedulerOptions{
		Clock:     clock,
		Payloads:  payloads,
		Deliverer: deliverer,
		Cooldown:  cooldown,
		SettingsSource: func(context.Context) (store.AdminNotificationSettings, string, error) {
			return store.AdminNotificationSettings{
				Enabled:                true,
				UseLegacyMode:          true,
				CostAlertEnabled:       true,
				CostAlertWebhook:       notifyTestPtr("https://example.test/cost"),
				CostAlertCheckInterval: notifyTestPtr(60),
			}, "UTC", nil
		},
		Bindings: func(string) ([]store.AdminNotificationBinding, error) { return nil, nil },
		Leader:   &fakeNotifyLeader{},
		// 重试不等待：本用例只关心「几份、谁成功」。
		MaxAttempts: 2,
		RetryBase:   time.Second,
	}
	scheduler := NewNotifyScheduler(options)
	if err := scheduler.Reschedule(context.Background()); err != nil {
		t.Fatalf("重排失败: %v", err)
	}
	clock.advance(time.Hour)
	return scheduler, clock
}

// TestNotifyDeliversEveryPayload 钉住「一次算出的多份数据逐份投递」：
// 只发第一份会静默丢掉其余告警（Node 的成本预警就是多条）。
func TestNotifyDeliversEveryPayload(t *testing.T) {
	payloads := &fakeMultiNotifyPayloads{items: []NotifyPayload{
		{Data: json.RawMessage(`{"targetId":1}`)},
		{Data: json.RawMessage(`{"targetId":2}`)},
		{Data: json.RawMessage(`{"targetId":3}`)},
	}}
	deliverer := &fakeNotifyDeliverer{}
	scheduler, clock := notifyFanoutScheduler(t, payloads, deliverer, nil)

	results, err := scheduler.Tick(context.Background(), clock.Now())
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if len(deliverer.requests) != 3 {
		t.Fatalf("三份数据应投递三次，实际 %d", len(deliverer.requests))
	}
	if len(results) != 1 || results[0].Payloads != 3 || results[0].Delivered != 3 {
		t.Fatalf("结果应记 3 份全送达: %+v", results)
	}
}

// TestNotifyPartialFailureKeepsDeliveredAndCommitsTheirCooldown 钉住两件事：
// 逐份独立计数（一份失败不牵连其余），以及**冷却键只在投递成功后写下**。
func TestNotifyPartialFailureKeepsDeliveredAndCommitsTheirCooldown(t *testing.T) {
	// 第 2 份两次尝试都失败（MaxAttempts=2），第 1/3 份成功。
	deliverer := &fakeNotifyDeliverer{
		results: []NotifyDeliveryResult{{Success: true}, {Success: false, Error: "boom"}, {Success: false, Error: "boom"}, {Success: true}},
	}
	payloads := &fakeMultiNotifyPayloads{items: []NotifyPayload{
		{Data: json.RawMessage(`{"n":1}`), CooldownKeys: []string{"k1"}, CooldownTTL: 10 * time.Minute},
		{Data: json.RawMessage(`{"n":2}`), CooldownKeys: []string{"k2"}, CooldownTTL: 10 * time.Minute},
		{Data: json.RawMessage(`{"n":3}`), CooldownKeys: []string{"k3"}, CooldownTTL: 10 * time.Minute},
	}}
	cooldown := &fakeNotifyCooldown{}
	scheduler, clock := notifyFanoutScheduler(t, payloads, deliverer, cooldown)

	results, err := scheduler.Tick(context.Background(), clock.Now())
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("应有一条结果: %+v", results)
	}
	result := results[0]
	if result.Delivered != 2 || result.Attempts != 4 || result.Error != "boom" {
		t.Fatalf("逐份计数不对: %+v", result)
	}
	// 失败那份的键不得落下，否则下一轮它会被冷却掉而永远发不出去。
	if len(cooldown.keys) != 2 || cooldown.keys[0] != "k1" || cooldown.keys[1] != "k3" {
		t.Fatalf("只应写下成功份的冷却键: %v", cooldown.keys)
	}
	if cooldown.ttl != 10*time.Minute {
		t.Fatalf("冷却 TTL 应为 10 分钟，实际 %s", cooldown.ttl)
	}
}

// TestNotifySkippedTargetStopsRemainingPayloads 钉住「目标缺失不重试、也不继续投其余份」：
// 同一目标下剩余的份必然同样失败，继续打只会浪费上游配额。
func TestNotifySkippedTargetStopsRemainingPayloads(t *testing.T) {
	deliverer := &fakeNotifyDeliverer{results: []NotifyDeliveryResult{{Skipped: true}}}
	payloads := &fakeMultiNotifyPayloads{items: []NotifyPayload{
		{Data: json.RawMessage(`{"n":1}`)},
		{Data: json.RawMessage(`{"n":2}`)},
	}}
	scheduler, clock := notifyFanoutScheduler(t, payloads, deliverer, nil)

	results, err := scheduler.Tick(context.Background(), clock.Now())
	if err != nil {
		t.Fatalf("触发失败: %v", err)
	}
	if len(deliverer.requests) != 1 {
		t.Fatalf("目标缺失后不该再投其余份，实际 %d 次", len(deliverer.requests))
	}
	if len(results) != 1 || !results[0].Skipped || results[0].Reason != "target_missing_or_disabled" {
		t.Fatalf("应记为「目标缺失」跳过: %+v", results)
	}
}
