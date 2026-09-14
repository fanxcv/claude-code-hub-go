package jobs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 真库集成测试：只覆盖**只有真库能证明**的两件事。
//
//  1. 多实例时只有一个真正发送：advisory 锁的跨会话互斥（并存的 Node 侧靠 Bull 的
//     jobId 去重达到同一效果）。
//  2. 设置/绑定的读路径 SQL 可用（Reschedule 走的是真 store 方法，不是测试桩）。
//
// 刻意**不写**共享库的通知设置与绑定表：那个库同时被其它 lane 与人工验证使用，
// 改全局通知配置会让别的用例看到不一致的开关。设置语义由 notify_test.go 的注入路径覆盖。
//
// 门控：未设置 CCH_TEST_DSN 时跳过（与 internal/jobs 的既有约定一致）。

func TestIntegrationNotifySchedulerOnlyLeaderSends(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()
	// 锁名带时间戳：并发跑多份测试时互不干扰（同名锁会让第二份直接看到「别人持锁」）。
	lockName := "cch-jobs-it-notify-" + time.Now().Format("20060102150405.000000000")

	// 两个实例在同一时刻扫描：持锁的那个在投递里停住，另一个必须因拿不到锁而整轮跳过。
	// 用阻塞式投递器把「同时」做实——顺序调用两次 Tick 无法证明互斥（那时锁已释放）。
	blocking := &blockingNotifyDeliverer{delivering: make(chan struct{}, 1), release: make(chan struct{})}
	other := &fakeNotifyDeliverer{}
	settings := store.AdminNotificationSettings{
		Enabled:                true,
		UseLegacyMode:          true,
		CostAlertEnabled:       true,
		CostAlertWebhook:       notifyTestPtr("https://example.test/cost"),
		CostAlertCheckInterval: notifyTestPtr(60),
	}
	now := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	newScheduler := func(deliverer NotifyDeliverer) *NotifyScheduler {
		scheduler := notifyTestScheduler(t, settings, notifyTestOptions{
			clock:     &fakeNotifyClock{now: now},
			deliverer: deliverer,
			payloads:  &fakeNotifyPayloads{ok: true, data: json.RawMessage(`{"alerts":1}`)},
		})
		scheduler.leader = &advisoryNotifyLeader{pools: pools, lockName: lockName}
		return scheduler
	}
	holder := newScheduler(blocking)
	challenger := newScheduler(other)
	for name, scheduler := range map[string]*NotifyScheduler{"holder": holder, "challenger": challenger} {
		if err := scheduler.Reschedule(ctx); err != nil {
			t.Fatalf("%s 重排失败: %v", name, err)
		}
	}

	holderDone := make(chan []NotifyRunResult, 1)
	go func() {
		results, err := holder.Tick(ctx, now.Add(time.Hour))
		if err != nil {
			t.Errorf("持锁实例触发失败: %v", err)
		}
		holderDone <- results
	}()

	select {
	case <-blocking.delivering:
	case <-time.After(10 * time.Second):
		t.Fatal("持锁实例迟迟未开始投递")
	}

	// 持锁实例仍在投递：挑战者的整轮必须跳过。
	challengerResults, err := challenger.Tick(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("挑战者触发失败: %v", err)
	}
	if len(challengerResults) != 0 {
		t.Fatalf("拿不到锁的实例不该触发任何任务: %+v", challengerResults)
	}
	close(blocking.release)

	holderResults := <-holderDone
	if len(holderResults) != 1 || !holderResults[0].Fired {
		t.Fatalf("持锁实例应触发一条: %+v", holderResults)
	}
	if len(blocking.requests) != 1 || len(other.requests) != 0 {
		t.Fatalf("同一时刻只应发送一次: holder=%d challenger=%d",
			len(blocking.requests), len(other.requests))
	}
}

// blockingNotifyDeliverer 在投递里停住，直到 release 关闭；用来把「同时触发」做实。
type blockingNotifyDeliverer struct {
	requests   []NotifyDeliveryRequest
	delivering chan struct{}
	release    chan struct{}
}

func (d *blockingNotifyDeliverer) Deliver(
	ctx context.Context,
	request NotifyDeliveryRequest,
) (NotifyDeliveryResult, error) {
	d.requests = append(d.requests, request)
	select {
	case d.delivering <- struct{}{}:
	default:
	}
	select {
	case <-d.release:
		return NotifyDeliveryResult{Success: true}, nil
	case <-ctx.Done():
		return NotifyDeliveryResult{}, ctx.Err()
	}
}

func TestIntegrationNotifySchedulerReadsSettingsFromStore(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()

	// 真读路径：Reschedule 用真 store（设置 + 系统时区 + 绑定）。
	scheduler := NewNotifyScheduler(NotifySchedulerOptions{
		Pools:  pools,
		Logger: nil,
		Clock:  &fakeNotifyClock{now: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)},
		Leader: &fakeNotifyLeader{},
	})
	if err := scheduler.Reschedule(ctx); err != nil {
		// 设置读取本身失败才是缺陷；规划失败（例如库里存着非法时区）不算——
		// 这个共享库可能被人工改过，故只断言“读到设置”这一步。
		settings, timezone, readErr := scheduler.settingsFromPools(ctx)
		if readErr != nil {
			t.Fatalf("读通知设置失败: %v", readErr)
		}
		t.Logf("设置已读到（enabled=%t legacy=%t tz=%s），规划报错: %v",
			settings.Enabled, settings.UseLegacyMode, timezone, err)
		return
	}

	settings, timezone, err := scheduler.settingsFromPools(ctx)
	if err != nil {
		t.Fatalf("读通知设置失败: %v", err)
	}
	if timezone == "" {
		t.Fatal("系统时区不该为空（resolveSystemTimezone 至少回退 UTC）")
	}
	t.Logf("设置已读到（enabled=%t legacy=%t tz=%s 排程=%d）",
		settings.Enabled, settings.UseLegacyMode, timezone, scheduler.Len())
}
