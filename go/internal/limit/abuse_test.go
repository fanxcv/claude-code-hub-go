package limit

import (
	"testing"
	"time"
)

func TestAuthThrottleLocksAfterThreshold(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	throttle := NewAuthThrottle(AuthAbuseConfig{
		MaxAttemptsPerIP:  3,
		MaxAttemptsPerKey: 3,
		WindowSeconds:     300,
		LockoutSeconds:    600,
	}, clock)

	if decision := throttle.Check("1.2.3.4", "sk-a"); !decision.Allowed {
		t.Fatalf("首次检查应放行: %+v", decision)
	}
	for attempt := 0; attempt < 3; attempt++ {
		throttle.RecordFailure("1.2.3.4", "sk-a")
	}

	decision := throttle.Check("1.2.3.4", "sk-a")
	if decision.Allowed {
		t.Fatalf("达到阈值后应被拒: %+v", decision)
	}
	if decision.Reason != "ip_rate_limited" {
		t.Errorf("拒绝原因 = %q，期望 ip_rate_limited", decision.Reason)
	}
	if decision.RetryAfterSeconds == nil || *decision.RetryAfterSeconds != 600 {
		t.Errorf("Retry-After = %v，期望 600", decision.RetryAfterSeconds)
	}

	// 另一个 IP 不受影响：计数按 scope 隔离。
	// 注意要传空密钥：同一密钥已被上面的失败记满，密钥维度此时也处于封禁中（这是 Node 的行为）。
	if other := throttle.Check("5.6.7.8", ""); !other.Allowed {
		t.Errorf("其它 IP 不应被同一 IP 的封禁牵连: %+v", other)
	}
}

func TestAuthThrottleKeyScopeAndSuccessReset(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	throttle := NewAuthThrottle(AuthAbuseConfig{MaxAttemptsPerIP: 10, MaxAttemptsPerKey: 3, WindowSeconds: 300, LockoutSeconds: 600}, clock)

	// 同一密钥从不同 IP 失败：密钥维度先触顶，IP 维度仍有余量，故原因应是 key_rate_limited。
	for attempt := 0; attempt < 3; attempt++ {
		throttle.RecordFailure("10.0.0."+itoa(int64(attempt)), "sk-b")
	}
	decision := throttle.Check("10.0.0.9", "sk-b")
	if decision.Allowed || decision.Reason != "key_rate_limited" {
		t.Fatalf("密钥维度应触顶: %+v", decision)
	}

	// 同 IP 无密钥时不判密钥维度。
	if plain := throttle.Check("10.0.0.9", ""); !plain.Allowed {
		t.Errorf("无候选密钥时不应按密钥维度拒绝: %+v", plain)
	}

	throttle.RecordSuccess("10.0.0.9", "sk-b")
	if after := throttle.Check("10.0.0.9", "sk-b"); !after.Allowed {
		t.Errorf("认证成功后应清零: %+v", after)
	}
}

func TestAuthThrottleWindowExpiryUnlocks(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	throttle := NewAuthThrottle(AuthAbuseConfig{MaxAttemptsPerIP: 2, MaxAttemptsPerKey: 2, WindowSeconds: 60, LockoutSeconds: 600}, clock)

	for attempt := 0; attempt < 2; attempt++ {
		throttle.RecordFailure("2.2.2.2", "")
	}
	if decision := throttle.Check("2.2.2.2", ""); decision.Allowed {
		t.Fatalf("触顶后应被拒: %+v", decision)
	}

	// 越过封禁时长即解锁（Node 侧同样只在检查时惰性清理）。
	now = now.Add(601 * time.Second)
	if decision := throttle.Check("2.2.2.2", ""); !decision.Allowed {
		t.Errorf("封禁到期后应放行: %+v", decision)
	}
}

func TestAuthThrottleThresholdOneLocksImmediately(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	throttle := NewAuthThrottle(AuthAbuseConfig{MaxAttemptsPerIP: 1, MaxAttemptsPerKey: 1, WindowSeconds: 300, LockoutSeconds: 60}, func() time.Time { return now })

	throttle.RecordFailure("3.3.3.3", "")
	if decision := throttle.Check("3.3.3.3", ""); decision.Allowed {
		t.Fatalf("阈值 1 应在首次失败即封禁: %+v", decision)
	}
}

func TestAuthThrottleRetryAfterRoundsUp(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	throttle := NewAuthThrottle(AuthAbuseConfig{MaxAttemptsPerIP: 1, MaxAttemptsPerKey: 1, WindowSeconds: 300, LockoutSeconds: 1}, func() time.Time { return now })

	throttle.RecordFailure("4.4.4.4", "")
	now = now.Add(500 * time.Millisecond)
	decision := throttle.Check("4.4.4.4", "")
	if decision.Allowed || decision.RetryAfterSeconds == nil {
		t.Fatalf("应处于封禁中: %+v", decision)
	}
	// Math.ceil(0.5s) = 1。
	if *decision.RetryAfterSeconds != 1 {
		t.Errorf("Retry-After = %d，期望 1（向上取整）", *decision.RetryAfterSeconds)
	}
}
