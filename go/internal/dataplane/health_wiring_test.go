package dataplane

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/health"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 本文件钉住「装配层必须给选路器一个真的熔断读取面」这条契约。
//
// 为什么要有它：写侧（health.Writer）一直是有装配的，管理面也照常显示「已熔断」，
// 但 route.NewHealthReader 在生产代码里**从未被构造过**（全仓只有测试构造），而选路器与
// 端点门控在 Health 为 nil 时一律放行。于是熔断只影响展示、不影响行为——查起来极费时，
// 因为账本、页面、写侧键全都正常，只有「请求还是打过去」这一件事不对。
//
// 判据取自真实数据面行为：写侧记够失败 → Redis 开闸 → **装配层兜底出来的读取面**必须看得见。

// TestResolveHealthPolicy 钉兜底策略本身（不依赖外部服务）。
func TestResolveHealthPolicy(t *testing.T) {
	injected := route.NewHealthReader(route.HealthOptions{})
	withInjected := resolveHealth(StoreOptions{RouteOptions: route.Options{Health: injected}})
	if withInjected != injected {
		t.Error("已注入的读取面必须原样沿用（保留显式装配与测试注入点）")
	}

	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = client.Close() })

	fallback := resolveHealth(StoreOptions{Redis: client})
	if fallback == nil {
		t.Fatal("配了 Redis 却没注入 Health 时必须兜底构造：否则熔断在生产选路里全程失效")
	}
	if fallback == injected {
		t.Error("兜底不应返回传入的那个实例")
	}

	if got := resolveHealth(StoreOptions{}); got != nil {
		t.Errorf("无 Redis 时应返回 nil（fail-open，与 Node 无 Redis 同义），实际 %+v", got)
	}
}

// TestResolveHealthSeesWriterState 是端到端那条：用**真的写侧**开闸，再看兜底读取面是否可见。
//
// 这条同时钉住「读写同一键空间」——键前缀或字段名改单侧时会红，而仅靠策略单测抓不到。
// 门控：未设置 CCH_TEST_REDIS_URL 时跳过（与同目录其余集成用例一致）。
func TestResolveHealthSeesWriterState(t *testing.T) {
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过熔断读写配对集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败（值已隐去）: %v", err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })

	// 用随机 ID 避免与其它用例或真实供应商撞键；退出时删掉自己那把状态键
	// （failureCount 与半开计数都是同一 hash 的字段，见 health/writer.go）。
	providerID := int64(9_100_000 + rand.Intn(90_000))
	stateKey := route.ProviderStateKeyPrefix + strconv.FormatInt(providerID, 10)
	ctx := context.Background()
	t.Cleanup(func() { _ = client.Del(context.Background(), stateKey).Err() })

	now := time.Now()
	writer := health.NewWriter(health.Options{Redis: client, Now: func() time.Time { return now }})
	reader := resolveHealth(StoreOptions{Redis: client, Now: func() time.Time { return now }})
	if reader == nil {
		t.Fatal("装配层应兜底出读取面")
	}

	if open, _ := reader.ProviderOpen(ctx, providerID); open {
		t.Fatal("未记账前不应视为已开闸")
	}
	// 记够阈值（默认 5 次）触发开闸；把每一次都断言清楚，避免「没记进去」被当成「读不到」。
	cause := errors.New("integration: upstream 503")
	var opened bool
	for attempt := 1; attempt <= 12; attempt++ {
		if err := writer.RecordProviderFailure(ctx, providerID, cause); err != nil {
			t.Fatalf("第 %d 次失败记账报错: %v", attempt, err)
		}
		if open, _ := reader.ProviderOpen(ctx, providerID); open {
			opened = true
			break
		}
	}
	if !opened {
		t.Fatal("记够失败后写侧仍未开闸：读写两侧至少有一侧没接到真 Redis")
	}
	if state := reader.ProviderState(ctx, providerID); state != route.StateOpen {
		t.Errorf("开闸窗口内的链上状态应为 %q，实际 %q", route.StateOpen, state)
	}

	// 窗口过期即放行（半开试探）——反向断言，防止把过期判定写成「只要 raw=open 就拒」。
	expired := time.Now().Add(48 * time.Hour)
	expiredReader := resolveHealth(StoreOptions{Redis: client, Now: func() time.Time { return expired }})
	if open, _ := expiredReader.ProviderOpen(ctx, providerID); open {
		t.Error("窗口过期后必须放行试探，否则熔断永远无法恢复")
	}
}
