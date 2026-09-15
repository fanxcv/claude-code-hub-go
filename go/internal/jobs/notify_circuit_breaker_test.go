package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/notify"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住熔断告警的产生点：三重闸门、去重键形状、legacy/targets 两种投递路径与
// 数据组装（含端点级与供应商级的分支）。
//
// 真源：src/lib/notification/notifier.ts:11-110（sendCircuitBreakerAlert）、
// circuit-breaker.ts:602-627（供应商级调用点）、endpoint-circuit-breaker.ts:443-490（端点级）。

// fakeAlertStore 是发布器的读面替身。
type fakeAlertStore struct {
	settings    store.AdminNotificationSettings
	settingsErr error
	provider    *store.AdminProvider
	providerErr error
	endpoint    *store.AdminProviderEndpoint
	endpointErr error
	bindings    []store.AdminNotificationBinding
	bindingsErr error
	timezone    string

	providerLookups int
	bindingsLookups int
}

func (f *fakeAlertStore) AdminGetNotificationSettings(context.Context) (store.AdminNotificationSettings, error) {
	return f.settings, f.settingsErr
}

func (f *fakeAlertStore) AdminGetProviderByID(context.Context, int64) (*store.AdminProvider, error) {
	f.providerLookups++
	return f.provider, f.providerErr
}

func (f *fakeAlertStore) AdminGetProviderEndpointByID(context.Context, int64) (*store.AdminProviderEndpoint, error) {
	return f.endpoint, f.endpointErr
}

func (f *fakeAlertStore) AdminListNotificationBindings(context.Context, string) ([]store.AdminNotificationBinding, error) {
	f.bindingsLookups++
	return f.bindings, f.bindingsErr
}

func (f *fakeAlertStore) AdminSystemTimezoneOrUTC(context.Context) string {
	if f.timezone == "" {
		return "UTC"
	}
	return f.timezone
}

// fakeAlertDeliverer 记录投递请求。
type fakeAlertDeliverer struct {
	mu       sync.Mutex
	requests []NotifyDeliveryRequest
	result   NotifyDeliveryResult
	err      error
}

func (f *fakeAlertDeliverer) Deliver(_ context.Context, request NotifyDeliveryRequest) (NotifyDeliveryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.err != nil {
		return NotifyDeliveryResult{}, f.err
	}
	if f.result == (NotifyDeliveryResult{}) {
		return NotifyDeliveryResult{Success: true}, nil
	}
	return f.result, nil
}

func (f *fakeAlertDeliverer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeAlertDeliverer) first() NotifyDeliveryRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[0]
}

// fakeAlertDeduper 记录去重键的读写。
type fakeAlertDeduper struct {
	mu       sync.Mutex
	seen     map[string]bool
	marks    []string
	ttls     []time.Duration
	readErr  error
	writeErr error
}

func newFakeAlertDeduper() *fakeAlertDeduper {
	return &fakeAlertDeduper{seen: map[string]bool{}}
}

func (f *fakeAlertDeduper) Seen(_ context.Context, keys []string) (bool, error) {
	if f.readErr != nil {
		return false, f.readErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range keys {
		if f.seen[key] {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeAlertDeduper) Mark(_ context.Context, keys []string, ttl time.Duration) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range keys {
		f.seen[key] = true
	}
	f.marks = append(f.marks, keys...)
	f.ttls = append(f.ttls, ttl)
	return nil
}

// providerAlertSettings 是一份「开关全开、targets 模式」的设置。
func providerAlertSettings() store.AdminNotificationSettings {
	return store.AdminNotificationSettings{
		Enabled:               true,
		CircuitBreakerEnabled: true,
	}
}

// fixedAlertNow 是发布器用的固定时刻。
var fixedAlertNow = time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

// TestCircuitBreakerAlertProviderPath 钉住供应商级：查名字 → 逐绑投递 → 正文可解出。
func TestCircuitBreakerAlertProviderPath(t *testing.T) {
	deliverer := &fakeAlertDeliverer{}
	deduper := newFakeAlertDeduper()
	source := &fakeAlertStore{
		settings: providerAlertSettings(),
		provider: &store.AdminProvider{ID: 7, Name: "供应商甲"},
		bindings: []store.AdminNotificationBinding{
			{ID: 11, TargetID: 21, IsEnabled: true},
			{ID: 12, TargetID: 22, IsEnabled: false}, // 停用的绑定应跳过
			{ID: 13, TargetID: 23, IsEnabled: true},
		},
	}
	publisher := &CircuitBreakerAlertPublisher{
		Pools: source, Deliverer: deliverer, Dedup: deduper, Now: func() time.Time { return fixedAlertNow },
		// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
		Clock: &fakeNotifyClock{now: fixedAlertNow},
	}

	enqueued := publisher.NotifyCircuitBreakerAlert(t.Context(), CircuitBreakerAlertEvent{
		Source:       CircuitBreakerSourceProvider,
		ProviderID:   7,
		FailureCount: 5,
		OpenUntilMS:  fixedAlertNow.Add(30 * time.Minute).UnixMilli(),
		Cause:        errors.New("connection timeout"),
	})
	if !enqueued {
		t.Fatal("应至少投递一条")
	}
	if deliverer.count() != 2 {
		t.Fatalf("停用的绑定应跳过，实际投递 %d 条", deliverer.count())
	}

	for _, request := range deliverer.requests {
		if request.Type != NotifyTypeCircuitBreaker {
			t.Fatalf("任务类型不符：%q", request.Type)
		}
		if request.TargetID == 22 {
			t.Fatal("停用的绑定不应投递")
		}
		var data notify.CircuitBreakerAlertData
		if err := json.Unmarshal(request.Data, &data); err != nil {
			t.Fatalf("数据不是预期形状：%v", err)
		}
		if data.ProviderName != "供应商甲" || data.ProviderID != 7 {
			t.Fatalf("数据不符：%+v", data)
		}
		if data.FailureCount != 5 {
			t.Fatalf("失败次数不符：%+v", data)
		}
		// Node 的 retryAt 是 ISO 毫秒（circuit-breaker.ts:559 的 toISOString）。
		if data.RetryAt != "2026-01-02T15:34:05.000Z" {
			t.Fatalf("恢复时刻应为 ISO 毫秒：%q", data.RetryAt)
		}
		if data.LastError != "connection timeout" {
			t.Fatalf("最后错误不符：%q", data.LastError)
		}
	}

	// 去重键：`circuit-breaker-alert:{providerId}:provider`（notifier.ts:31）。
	if len(deduper.marks) != 1 || deduper.marks[0] != "circuit-breaker-alert:7:provider" {
		t.Fatalf("去重键不符：%v", deduper.marks)
	}
	if len(deduper.ttls) != 1 || deduper.ttls[0] != 5*time.Minute {
		t.Fatalf("去重 TTL 应为 5 分钟：%v", deduper.ttls)
	}
}

// TestCircuitBreakerAlertEndpointPath 钉住端点级：名字用 label、带上 url 与 endpointId，
// 且去重键带 endpoint 段（同供应商的不同端点互不吃冷却）。
func TestCircuitBreakerAlertEndpointPath(t *testing.T) {
	deliverer := &fakeAlertDeliverer{}
	deduper := newFakeAlertDeduper()
	label := "端点标签"
	source := &fakeAlertStore{
		settings: providerAlertSettings(),
		endpoint: &store.AdminProviderEndpoint{
			ID: 42, VendorID: 83, URL: "https://relay.example.com/v1", Label: &label,
		},
		bindings: []store.AdminNotificationBinding{{ID: 11, TargetID: 21, IsEnabled: true}},
	}
	publisher := &CircuitBreakerAlertPublisher{
		Pools: source, Deliverer: deliverer, Dedup: deduper, Now: func() time.Time { return fixedAlertNow },
		// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
		Clock: &fakeNotifyClock{now: fixedAlertNow},
	}

	if !publisher.NotifyCircuitBreakerAlert(t.Context(), CircuitBreakerAlertEvent{
		Source:       CircuitBreakerSourceEndpoint,
		EndpointID:   42,
		FailureCount: 3,
		OpenUntilMS:  fixedAlertNow.Add(time.Hour).UnixMilli(),
	}) {
		t.Fatal("应投递一条")
	}

	var data notify.CircuitBreakerAlertData
	if err := json.Unmarshal(deliverer.first().Data, &data); err != nil {
		t.Fatalf("数据不是预期形状：%v", err)
	}
	if data.IncidentSource != notify.IncidentSourceEndpoint {
		t.Fatalf("来源应为 endpoint：%q", data.IncidentSource)
	}
	if data.ProviderName != "端点标签" {
		t.Fatalf("名字应取端点的 label：%q", data.ProviderName)
	}
	if data.EndpointID == nil || *data.EndpointID != 42 {
		t.Fatalf("端点 id 不符：%+v", data.EndpointID)
	}
	if data.EndpointURL != "https://relay.example.com/v1" {
		t.Fatalf("端点 url 不符：%q", data.EndpointURL)
	}
	// 供应商 id 换成端点所属的 vendor（Node endpoint-circuit-breaker.ts:464-472）。
	if data.ProviderID != 83 {
		t.Fatalf("providerId 应为 vendor id：%d", data.ProviderID)
	}
	if len(deduper.marks) != 1 || deduper.marks[0] != "circuit-breaker-alert:83:endpoint:42" {
		t.Fatalf("端点级去重键不符：%v", deduper.marks)
	}
}

// TestCircuitBreakerAlertEndpointLookupNotFoundStillSends 钉住「查不到端点行仍发」
// （Node 只是少了 url/label，不放弃告警）。
func TestCircuitBreakerAlertEndpointLookupNotFoundStillSends(t *testing.T) {
	deliverer := &fakeAlertDeliverer{}
	source := &fakeAlertStore{
		settings:    providerAlertSettings(),
		endpointErr: store.ErrNotFound,
		bindings:    []store.AdminNotificationBinding{{ID: 11, TargetID: 21, IsEnabled: true}},
	}
	publisher := &CircuitBreakerAlertPublisher{
		Pools: source, Deliverer: deliverer, Now: func() time.Time { return fixedAlertNow },
		// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
		Clock: &fakeNotifyClock{now: fixedAlertNow},
	}
	if !publisher.NotifyCircuitBreakerAlert(t.Context(), CircuitBreakerAlertEvent{
		Source: CircuitBreakerSourceEndpoint, EndpointID: 42, FailureCount: 3,
	}) {
		t.Fatal("端点行读不到也应照发")
	}
	var data notify.CircuitBreakerAlertData
	if err := json.Unmarshal(deliverer.first().Data, &data); err != nil {
		t.Fatalf("数据不是预期形状：%v", err)
	}
	// Node 在拿不到 label 时用 `endpoint:{id}` 当名字。
	if data.ProviderName != "endpoint:42" {
		t.Fatalf("无 label 时的名字不符：%q", data.ProviderName)
	}
	if data.EndpointURL != "" {
		t.Fatalf("无端点行时不应有 url：%q", data.EndpointURL)
	}
}

// TestCircuitBreakerAlertGates 钉住三重闸门：开关关、供应商查不到、无绑定、legacy 缺 webhook。
func TestCircuitBreakerAlertGates(t *testing.T) {
	event := CircuitBreakerAlertEvent{
		Source: CircuitBreakerSourceProvider, ProviderID: 7, FailureCount: 5,
		OpenUntilMS: fixedAlertNow.Add(time.Hour).UnixMilli(),
	}

	t.Run("总开关关闭不发", func(t *testing.T) {
		deliverer := &fakeAlertDeliverer{}
		publisher := &CircuitBreakerAlertPublisher{
			Pools: &fakeAlertStore{
				settings: store.AdminNotificationSettings{Enabled: false, CircuitBreakerEnabled: true},
				provider: &store.AdminProvider{ID: 7, Name: "甲"},
			},
			Deliverer: deliverer, Now: func() time.Time { return fixedAlertNow },
			// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
			Clock: &fakeNotifyClock{now: fixedAlertNow},
		}
		if publisher.NotifyCircuitBreakerAlert(t.Context(), event) {
			t.Fatal("总开关关闭不应投递")
		}
		if deliverer.count() != 0 {
			t.Fatalf("不应有投递，实际 %d", deliverer.count())
		}
	})

	t.Run("熔断子开关关闭不发", func(t *testing.T) {
		deliverer := &fakeAlertDeliverer{}
		publisher := &CircuitBreakerAlertPublisher{
			Pools: &fakeAlertStore{
				settings: store.AdminNotificationSettings{Enabled: true, CircuitBreakerEnabled: false},
				provider: &store.AdminProvider{ID: 7, Name: "甲"},
			},
			Deliverer: deliverer, Now: func() time.Time { return fixedAlertNow },
			// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
			Clock: &fakeNotifyClock{now: fixedAlertNow},
		}
		if publisher.NotifyCircuitBreakerAlert(t.Context(), event) {
			t.Fatal("熔断子开关关闭不应投递")
		}
	})

	t.Run("供应商不存在时放弃且不查绑定", func(t *testing.T) {
		deliverer := &fakeAlertDeliverer{}
		source := &fakeAlertStore{
			settings: providerAlertSettings(), providerErr: store.ErrNotFound,
			bindings: []store.AdminNotificationBinding{{ID: 11, TargetID: 21, IsEnabled: true}},
		}
		publisher := &CircuitBreakerAlertPublisher{
			Pools: source, Deliverer: deliverer, Now: func() time.Time { return fixedAlertNow },
			// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
			Clock: &fakeNotifyClock{now: fixedAlertNow},
		}
		if publisher.NotifyCircuitBreakerAlert(t.Context(), event) {
			t.Fatal("查不到供应商应放弃（Node 同判）")
		}
		if source.bindingsLookups != 0 {
			t.Fatal("放弃路径不应再读绑定")
		}
	})

	t.Run("无绑定不发", func(t *testing.T) {
		deliverer := &fakeAlertDeliverer{}
		publisher := &CircuitBreakerAlertPublisher{
			Pools: &fakeAlertStore{
				settings: providerAlertSettings(), provider: &store.AdminProvider{ID: 7, Name: "甲"},
			},
			Deliverer: deliverer, Now: func() time.Time { return fixedAlertNow },
			// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
			Clock: &fakeNotifyClock{now: fixedAlertNow},
		}
		if publisher.NotifyCircuitBreakerAlert(t.Context(), event) {
			t.Fatal("无绑定不应投递")
		}
	})

	t.Run("legacy 缺 webhook 不发", func(t *testing.T) {
		deliverer := &fakeAlertDeliverer{}
		publisher := &CircuitBreakerAlertPublisher{
			Pools: &fakeAlertStore{
				settings: func() store.AdminNotificationSettings {
					settings := providerAlertSettings()
					settings.UseLegacyMode = true
					return settings
				}(),
				provider: &store.AdminProvider{ID: 7, Name: "甲"},
			},
			Deliverer: deliverer, Now: func() time.Time { return fixedAlertNow },
			// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
			Clock: &fakeNotifyClock{now: fixedAlertNow},
		}
		if publisher.NotifyCircuitBreakerAlert(t.Context(), event) {
			t.Fatal("legacy 缺 webhook 不应投递")
		}
	})

	t.Run("legacy 有 webhook 走单 URL", func(t *testing.T) {
		deliverer := &fakeAlertDeliverer{}
		webhook := "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=placeholder"
		publisher := &CircuitBreakerAlertPublisher{
			Pools: &fakeAlertStore{
				settings: func() store.AdminNotificationSettings {
					settings := providerAlertSettings()
					settings.UseLegacyMode = true
					settings.CircuitBreakerWebhook = &webhook
					return settings
				}(),
				provider: &store.AdminProvider{ID: 7, Name: "甲"},
			},
			Deliverer: deliverer, Now: func() time.Time { return fixedAlertNow },
			// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
			Clock: &fakeNotifyClock{now: fixedAlertNow},
		}
		if !publisher.NotifyCircuitBreakerAlert(t.Context(), event) {
			t.Fatal("legacy 有 webhook 应投递")
		}
		if deliverer.first().WebhookURL != webhook {
			t.Fatalf("legacy 应走单 URL：%+v", deliverer.first())
		}
		// legacy 不走绑定，故不应查绑定表。
		if deliverer.first().TargetID != 0 {
			t.Fatalf("legacy 不应带 targetId：%+v", deliverer.first())
		}
	})

	t.Run("未装配投递器时如实记 error 且不入队", func(t *testing.T) {
		publisher := &CircuitBreakerAlertPublisher{
			Pools: &fakeAlertStore{
				settings: providerAlertSettings(), provider: &store.AdminProvider{ID: 7, Name: "甲"},
				bindings: []store.AdminNotificationBinding{{ID: 11, TargetID: 21, IsEnabled: true}},
			},
			Now: func() time.Time { return fixedAlertNow },
			// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
			Clock: &fakeNotifyClock{now: fixedAlertNow},
		}
		if publisher.NotifyCircuitBreakerAlert(t.Context(), event) {
			t.Fatal("未装配投递器不应报告成功")
		}
	})
}

// TestCircuitBreakerAlertDedup 钉住 5 分钟去重：第二次不发，读失败时照发且不占坑。
func TestCircuitBreakerAlertDedup(t *testing.T) {
	event := CircuitBreakerAlertEvent{
		Source: CircuitBreakerSourceProvider, ProviderID: 7, FailureCount: 5,
		OpenUntilMS: fixedAlertNow.Add(time.Hour).UnixMilli(),
	}
	newPublisher := func(deduper NotifyDeduper, deliverer *fakeAlertDeliverer) *CircuitBreakerAlertPublisher {
		return &CircuitBreakerAlertPublisher{
			Pools: &fakeAlertStore{
				settings: providerAlertSettings(), provider: &store.AdminProvider{ID: 7, Name: "甲"},
				bindings: []store.AdminNotificationBinding{{ID: 11, TargetID: 21, IsEnabled: true}},
			},
			Deliverer: deliverer, Dedup: deduper, Now: func() time.Time { return fixedAlertNow },
			// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
			Clock: &fakeNotifyClock{now: fixedAlertNow},
		}
	}

	t.Run("窗口内第二次被抑制", func(t *testing.T) {
		deliverer := &fakeAlertDeliverer{}
		deduper := newFakeAlertDeduper()
		publisher := newPublisher(deduper, deliverer)
		if !publisher.NotifyCircuitBreakerAlert(t.Context(), event) {
			t.Fatal("第一次应投递")
		}
		if publisher.NotifyCircuitBreakerAlert(t.Context(), event) {
			t.Fatal("窗口内第二次应被抑制")
		}
		if deliverer.count() != 1 {
			t.Fatalf("应只投递一次，实际 %d", deliverer.count())
		}
	})

	t.Run("读去重失败仍照发", func(t *testing.T) {
		deliverer := &fakeAlertDeliverer{}
		deduper := newFakeAlertDeduper()
		deduper.readErr = errors.New("redis down")
		publisher := newPublisher(deduper, deliverer)
		if !publisher.NotifyCircuitBreakerAlert(t.Context(), event) {
			t.Fatal("去重读失败不应阻断告警（漏发比重发坏）")
		}
	})

	t.Run("写去重失败不影响投递结论", func(t *testing.T) {
		deliverer := &fakeAlertDeliverer{}
		deduper := newFakeAlertDeduper()
		deduper.writeErr = errors.New("redis down")
		publisher := newPublisher(deduper, deliverer)
		if !publisher.NotifyCircuitBreakerAlert(t.Context(), event) {
			t.Fatal("写去重失败不应改变投递成功的事实")
		}
		if deliverer.count() != 1 {
			t.Fatalf("应投递一次，实际 %d", deliverer.count())
		}
	})

	t.Run("无去重器时不去重", func(t *testing.T) {
		deliverer := &fakeAlertDeliverer{}
		publisher := newPublisher(nil, deliverer)
		publisher.NotifyCircuitBreakerAlert(t.Context(), event)
		publisher.NotifyCircuitBreakerAlert(t.Context(), event)
		if deliverer.count() != 2 {
			t.Fatalf("无去重器时应照发两次，实际 %d", deliverer.count())
		}
	})
}

// TestCircuitBreakerAlertDelivererFailure 钉住投递失败时的结论（不入队、不占坑）。
func TestCircuitBreakerAlertDelivererFailure(t *testing.T) {
	deliverer := &fakeAlertDeliverer{err: errors.New("dial tcp: connection refused")}
	deduper := newFakeAlertDeduper()
	publisher := &CircuitBreakerAlertPublisher{
		Pools: &fakeAlertStore{
			settings: providerAlertSettings(), provider: &store.AdminProvider{ID: 7, Name: "甲"},
			bindings: []store.AdminNotificationBinding{{ID: 11, TargetID: 21, IsEnabled: true}},
		},
		Deliverer: deliverer, Dedup: deduper, Now: func() time.Time { return fixedAlertNow },
		// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
		Clock: &fakeNotifyClock{now: fixedAlertNow},
	}
	if publisher.NotifyCircuitBreakerAlert(t.Context(), CircuitBreakerAlertEvent{
		Source: CircuitBreakerSourceProvider, ProviderID: 7, FailureCount: 5,
	}) {
		t.Fatal("投递失败不应报告入队成功")
	}
	if len(deduper.marks) != 0 {
		t.Fatalf("投递失败不应占冷却坑：%v", deduper.marks)
	}
}

// TestCircuitBreakerAlertAdaptersMatchHealthCallback 钉住两个适配方法与 health 回调的同签名。
//
// 装配处会把 `publisher.OnProviderOpened` 直接塞给 health.Options；签名一旦漂移，
// 那行赋值会编译不过——这个用例把它变成一条断言（用函数值赋值做编译期校验）。
func TestCircuitBreakerAlertAdaptersMatchHealthCallback(t *testing.T) {
	publisher := &CircuitBreakerAlertPublisher{}
	var providerCallback func(int64, int64, int64, error) = publisher.OnProviderOpened
	var endpointCallback func(int64, int64, int64, error) = publisher.OnEndpointOpened
	if providerCallback == nil || endpointCallback == nil {
		t.Fatal("适配方法不应为空")
	}

	// 空发布器（未装配 Pools）应立即返回且不 panic——health 的回调在熔断路径上，
	// 一旦 panic 会把熔断本身带崩。
	providerCallback(7, 5, fixedAlertNow.UnixMilli(), nil)
	endpointCallback(42, 3, fixedAlertNow.UnixMilli(), nil)

	// 适配方法必须**不阻塞**：它跑在请求路径上，而投递带 60s/120s 退避。
	// 用一个永不返回的投递器验证「调用即返回」。
	blocking := &blockingAlertDeliverer{release: make(chan struct{})}
	live := &CircuitBreakerAlertPublisher{
		Pools: &fakeAlertStore{
			settings: providerAlertSettings(), provider: &store.AdminProvider{ID: 7, Name: "甲"},
			bindings: []store.AdminNotificationBinding{{ID: 11, TargetID: 21, IsEnabled: true}},
		},
		Deliverer: blocking, Clock: &fakeNotifyClock{now: fixedAlertNow},
		Now: func() time.Time { return fixedAlertNow },
	}
	done := make(chan struct{})
	go func() {
		live.OnProviderOpened(7, 5, fixedAlertNow.UnixMilli(), nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("适配方法不应阻塞在投递上（它在请求路径上）")
	}
	// 后台那条仍卡在投递里；放行以便 goroutine 结束（不等它也可，仅为不泄漏）。
	close(blocking.release)
}

// blockingAlertDeliverer 直到放行才返回，用来验证适配方法不阻塞。
type blockingAlertDeliverer struct {
	release chan struct{}
	once    sync.Once
}

func (b *blockingAlertDeliverer) Deliver(context.Context, NotifyDeliveryRequest) (NotifyDeliveryResult, error) {
	<-b.release
	b.once.Do(func() {})
	return NotifyDeliveryResult{}, errors.New("released")
}

// TestCircuitBreakerAlertKeys 钉住两套键形状与来源段的区别。
func TestCircuitBreakerAlertKeys(t *testing.T) {
	providerKey := circuitBreakerAlertKey(CircuitBreakerSourceProvider, 7, 0)
	if providerKey != "circuit-breaker-alert:7:provider" {
		t.Fatalf("供应商级键不符：%q", providerKey)
	}
	endpointKey := circuitBreakerAlertKey(CircuitBreakerSourceEndpoint, 83, 42)
	if endpointKey != "circuit-breaker-alert:83:endpoint:42" {
		t.Fatalf("端点级键不符：%q", endpointKey)
	}
	// 来源缺省时（Node 的 `data.incidentSource ?? "provider"`）按供应商级。
	if got := circuitBreakerAlertKey("", 7, 0); got != providerKey {
		t.Fatalf("来源缺省应按供应商级：%q", got)
	}
	// 端点级但端点 id 为 0 时退回来源段（Node 的 `endpointId != null` 判定）。
	if got := circuitBreakerAlertKey(CircuitBreakerSourceEndpoint, 83, 0); got != "circuit-breaker-alert:83:endpoint" {
		t.Fatalf("端点 id 缺失时键不符：%q", got)
	}
}

// TestNewCircuitBreakerAlertPublisherDefaults 钉住构造入口：只给必需的三个面也能用，
// 且缺 Logger / Now 时被补成安全默认（而不是等到开闸那一刻才空指针）。
func TestNewCircuitBreakerAlertPublisherDefaults(t *testing.T) {
	deliverer := &fakeAlertDeliverer{}
	publisher := NewCircuitBreakerAlertPublisher(CircuitBreakerAlertOptions{
		Pools: &fakeAlertStore{
			settings: providerAlertSettings(), provider: &store.AdminProvider{ID: 7, Name: "甲"},
			bindings: []store.AdminNotificationBinding{{ID: 11, TargetID: 21, IsEnabled: true}},
		},
		Deliverer: deliverer,
	})
	if publisher.Logger == nil {
		t.Fatal("Logger 应被补成默认值")
	}
	if publisher.Now == nil {
		t.Fatal("Now 应被补成默认值")
	}
	// 未传 Dedup：按「不去重」处理，与 Node 无 Redis 的分支一致。
	if publisher.Dedup != nil {
		t.Fatal("未传 Dedup 时应为 nil（不去重）")
	}
	if !publisher.NotifyCircuitBreakerAlert(t.Context(), CircuitBreakerAlertEvent{
		Source: CircuitBreakerSourceProvider, ProviderID: 7, FailureCount: 5,
	}) {
		t.Fatal("应投递一条")
	}
	if deliverer.count() != 1 {
		t.Fatalf("应投递一次，实际 %d", deliverer.count())
	}
}

// TestCircuitBreakerAlertRenderedText 钉住「事件 → 结构化消息 → 渠道正文」这条链。
//
// 发布器只负责把 Data 交出去，正文由渲染入口产出；这条用例把两半接起来，
// 保证熔断告警真的能渲染出一条可读的正文（而不是只在两端各自通过）。
func TestCircuitBreakerAlertRenderedText(t *testing.T) {
	deliverer := &fakeAlertDeliverer{}
	publisher := &CircuitBreakerAlertPublisher{
		Pools: &fakeAlertStore{
			settings: providerAlertSettings(), provider: &store.AdminProvider{ID: 7, Name: "供应商甲"},
			bindings: []store.AdminNotificationBinding{{ID: 11, TargetID: 21, IsEnabled: true}},
		},
		Deliverer: deliverer, Now: func() time.Time { return fixedAlertNow },
		// 快时钟：失败路径的重试退避（60s + 120s）不该让用例真等三分钟。
		Clock: &fakeNotifyClock{now: fixedAlertNow},
	}
	publisher.NotifyCircuitBreakerAlert(t.Context(), CircuitBreakerAlertEvent{
		Source: CircuitBreakerSourceProvider, ProviderID: 7, FailureCount: 5,
		OpenUntilMS: fixedAlertNow.Add(30 * time.Minute).UnixMilli(),
		Cause:       errors.New("connection timeout"),
	})

	request := deliverer.first()
	message, err := notify.BuildDeliveryMessage("circuit_breaker", request.Data, "UTC", fixedAlertNow)
	if err != nil {
		t.Fatalf("构建正文失败：%v", err)
	}
	rendered, err := notify.RenderWebhook("wechat", message, notify.WebhookRenderConfig{},
		notify.WebhookRenderOptions{NotificationType: "circuit_breaker", Data: request.Data, Timezone: "UTC"})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	var body struct {
		Markdown struct {
			Content string `json:"content"`
		} `json:"markdown"`
	}
	if err := json.Unmarshal(rendered.Body, &body); err != nil {
		t.Fatalf("请求体不是 JSON：%v", err)
	}
	for _, want := range []string{
		"## 供应商熔断告警",
		"> 供应商 供应商甲 (ID: 7) 已触发熔断保护",
		"**详细信息**",
		"失败次数: 5 次",
		"预计恢复: 2026/01/02 15:34:05",
		"最后错误: connection timeout",
		"熔断器将在预计时间后自动恢复",
	} {
		if !strings.Contains(body.Markdown.Content, want) {
			t.Fatalf("正文缺少 %q：\n%s", want, body.Markdown.Content)
		}
	}
	// 正文里不得出现任何 emoji（仓库指南 §8）。
	for _, r := range body.Markdown.Content {
		if (r >= 0x1F300 && r <= 0x1FAFF) || (r >= 0x2600 && r <= 0x27BF) {
			t.Fatalf("正文出现 emoji：%U", r)
		}
	}
}
