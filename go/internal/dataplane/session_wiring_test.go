package dataplane

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件钉住会话接线的两件事：
//  1. 绑定结果必须按请求记录（否则请求日志的两个会话列永远是 NULL）；
//  2. 装配在给了 Redis 时必须真的接上 SessionBinder，且不再把它算作缺口。

// fakeBinder 是可控的绑定实现。
type fakeBinder struct {
	result guard.SessionResult
	err    error
	calls  int
}

func (f *fakeBinder) Ensure(context.Context, guard.SessionRequest) (guard.SessionResult, error) {
	f.calls++
	if f.err != nil {
		return guard.SessionResult{}, f.err
	}
	return f.result, nil
}

func TestSessionCaptureRecordsResultForRequestLog(t *testing.T) {
	inner := &fakeBinder{result: guard.SessionResult{SessionID: "sess_x", Sequence: 3}}
	capture := newSessionCapture(inner)

	// 绑定之前：没有会话身份，两列写 NULL。
	if _, ok := capture.lookup(nil); ok {
		t.Fatal("未绑定前不应有会话身份")
	}

	got, err := capture.Ensure(context.Background(), guard.SessionRequest{KeyID: 1})
	if err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	if got.SessionID != "sess_x" || got.Sequence != 3 {
		t.Fatalf("Ensure 应原样转发结果，得到 %+v", got)
	}
	recorded, ok := capture.lookup(nil)
	if !ok || recorded.SessionID != "sess_x" || recorded.Sequence != 3 {
		t.Fatalf("绑定后应可读出会话身份，得到 %+v ok=%v", recorded, ok)
	}
}

func TestSessionCaptureDoesNotRecordFailures(t *testing.T) {
	capture := newSessionCapture(&fakeBinder{err: errors.New("redis down")})
	if _, err := capture.Ensure(context.Background(), guard.SessionRequest{KeyID: 1}); err == nil {
		t.Fatal("内层失败应向外传播")
	}
	if _, ok := capture.lookup(nil); ok {
		t.Fatal("绑定失败不得写出会话身份（否则日志会把不存在的会话记下来）")
	}
}

func TestSessionCaptureWithoutInnerIsInert(t *testing.T) {
	capture := newSessionCapture(nil)
	if _, err := capture.Ensure(context.Background(), guard.SessionRequest{KeyID: 1}); err != nil {
		t.Fatalf("未接线时 Ensure 应无错误: %v", err)
	}
	if _, ok := capture.lookup(nil); ok {
		t.Fatal("未接线时不应有会话身份")
	}
}

// TestAssemblyReportsSessionBinderGap 钉住缺口清单与真实接线一致：
// 没有 Redis 时报缺口，给了 Redis 就不报。
func TestAssemblyReportsSessionBinderGap(t *testing.T) {
	pools := integrationStore(t)
	logger := logx.New(nil)

	withoutRedis, err := NewStoreBacked(StoreOptions{Pools: pools, Logger: logger})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if withoutRedis.SessionBinder != nil {
		t.Fatal("没有 Redis 时不应接上会话绑定")
	}
	if !containsString(withoutRedis.Missing, "SessionBinder") {
		t.Fatalf("没有 Redis 时应如实报出 SessionBinder 缺口，得到 %v", withoutRedis.Missing)
	}

	rdb := dataPlaneIntegrationRedis(t)
	withRedis, err := NewStoreBacked(StoreOptions{Pools: pools, Redis: rdb, Logger: logger})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if withRedis.SessionBinder == nil {
		t.Fatal("给了 Redis 时必须接上会话绑定")
	}
	if containsString(withRedis.Missing, "SessionBinder") {
		t.Fatalf("已接线不得再报缺口，得到 %v", withRedis.Missing)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// dataPlaneIntegrationRedis 建真实 Redis（DB >= 13 的仓库隔离纪律）。
func dataPlaneIntegrationRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过数据面 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败：%v", err)
	}
	if options.DB < 13 {
		t.Fatalf("CCH_TEST_REDIS_URL 必须使用 DB index >= 13，收到 %d", options.DB)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	return client
}
