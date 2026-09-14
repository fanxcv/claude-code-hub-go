package limit

import (
	"sync"
	"time"
)

// AuthAbuseConfig 是认证失败节流的阈值，字段与 Node 侧 LoginAbuseConfig 一致。
type AuthAbuseConfig struct {
	// MaxAttemptsPerIP 是单 IP 在窗口内的失败上限。
	MaxAttemptsPerIP int
	// MaxAttemptsPerKey 是单密钥在窗口内的失败上限。
	MaxAttemptsPerKey int
	// WindowSeconds 是失败计数窗口。
	WindowSeconds int
	// LockoutSeconds 是触顶后的封禁时长。
	LockoutSeconds int
}

// ProxyAuthAbuseConfig 是数据面使用的阈值（auth-guard.ts 的 proxyAuthPolicy）。
//
// 与登录页的 DEFAULT_LOGIN_ABUSE_CONFIG 刻意不同：数据面是程序化调用，窗口更短、封禁更短，
// 20 次/5 分钟/封 10 分钟。
var ProxyAuthAbuseConfig = AuthAbuseConfig{
	MaxAttemptsPerIP:  20,
	MaxAttemptsPerKey: 20,
	WindowSeconds:     300,
	LockoutSeconds:    600,
}

// 与 Node 侧 LoginAbusePolicy 一致的两个清扫参数。
const (
	maxTrackedAbuseEntries = 10000
	abuseSweepInterval     = time.Minute
)

type abuseRecord struct {
	count        int
	firstAttempt time.Time
	// lockedUntil 为零值表示尚未封禁。
	lockedUntil time.Time
	locked      bool
}

// AuthThrottle 是认证失败节流器（LoginAbusePolicy 的移植）。
//
// 刻意是进程内状态：Node 侧同样是进程内 Map（登录页与数据面各自一份），不落 Redis。
// 多实例部署时每个实例各自计数，这与 Node 的行为一致，不是缺陷。
type AuthThrottle struct {
	mu       sync.Mutex
	config   AuthAbuseConfig
	attempts map[string]*abuseRecord
	// lastSweepAt 是上次清扫时刻，避免每次检查都全表遍历。
	lastSweepAt time.Time
	now         func() time.Time
}

// NewAuthThrottle 组装节流器；config 的零值字段按 ProxyAuthAbuseConfig 补齐。
func NewAuthThrottle(config AuthAbuseConfig, now func() time.Time) *AuthThrottle {
	if now == nil {
		now = time.Now
	}
	if config.MaxAttemptsPerIP <= 0 {
		config.MaxAttemptsPerIP = ProxyAuthAbuseConfig.MaxAttemptsPerIP
	}
	if config.MaxAttemptsPerKey <= 0 {
		config.MaxAttemptsPerKey = ProxyAuthAbuseConfig.MaxAttemptsPerKey
	}
	if config.WindowSeconds <= 0 {
		config.WindowSeconds = ProxyAuthAbuseConfig.WindowSeconds
	}
	if config.LockoutSeconds <= 0 {
		config.LockoutSeconds = ProxyAuthAbuseConfig.LockoutSeconds
	}
	return &AuthThrottle{config: config, attempts: map[string]*abuseRecord{}, now: now}
}

// Decision 是节流判定。
type Decision struct {
	Allowed bool
	// RetryAfterSeconds 仅在 Allowed 为 false 时有值。
	RetryAfterSeconds *int
	// Reason 取 ip_rate_limited 或 key_rate_limited。
	Reason string
}

// Check 判定某 IP 与候选密钥是否处于封禁/触顶状态。
//
// 顺序与 Node 一致：先判 IP，IP 被拒不看了；IP 通过且给了密钥才判密钥。
func (t *AuthThrottle) Check(ip string, key string) Decision {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweep(now)

	if decision := t.checkScope("ip:"+ip, t.config.MaxAttemptsPerIP, "ip_rate_limited", now); !decision.Allowed {
		return decision
	}
	if key == "" {
		return Decision{Allowed: true}
	}
	return t.checkScope("key:"+key, t.config.MaxAttemptsPerKey, "key_rate_limited", now)
}

// RecordFailure 记一次认证失败（IP 与密钥两维都记）。
func (t *AuthThrottle) RecordFailure(ip string, key string) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweep(now)
	t.recordFailure("ip:"+ip, t.config.MaxAttemptsPerIP, now)
	if key == "" {
		return
	}
	t.recordFailure("key:"+key, t.config.MaxAttemptsPerKey, now)
}

// RecordSuccess 在认证成功后清零两维计数。
func (t *AuthThrottle) RecordSuccess(ip string, key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.attempts, "ip:"+ip)
	if key != "" {
		delete(t.attempts, "key:"+key)
	}
}

func (t *AuthThrottle) checkScope(scopeKey string, threshold int, reason string, now time.Time) Decision {
	record, ok := t.attempts[scopeKey]
	if !ok {
		return Decision{Allowed: true}
	}
	if record.locked {
		if record.lockedUntil.After(now) {
			return Decision{Allowed: false, RetryAfterSeconds: retryAfter(record.lockedUntil, now), Reason: reason}
		}
		delete(t.attempts, scopeKey)
		return Decision{Allowed: true}
	}
	if t.windowExpired(record, now) {
		delete(t.attempts, scopeKey)
		return Decision{Allowed: true}
	}
	if record.count >= threshold {
		lockedUntil := now.Add(time.Duration(t.config.LockoutSeconds) * time.Second)
		// LRU 提升：删除再插入让被锁条目挪到 Map 末尾，清扫时最后才被淘汰。
		delete(t.attempts, scopeKey)
		t.attempts[scopeKey] = &abuseRecord{
			count:        record.count,
			firstAttempt: record.firstAttempt,
			lockedUntil:  lockedUntil,
			locked:       true,
		}
		return Decision{Allowed: false, RetryAfterSeconds: retryAfter(lockedUntil, now), Reason: reason}
	}
	return Decision{Allowed: true}
}

func (t *AuthThrottle) recordFailure(scopeKey string, threshold int, now time.Time) {
	record, ok := t.attempts[scopeKey]
	if !ok {
		t.attempts[scopeKey] = t.firstRecord(now, threshold)
		return
	}
	if record.locked {
		if record.lockedUntil.After(now) {
			return
		}
		delete(t.attempts, scopeKey)
		t.attempts[scopeKey] = t.firstRecord(now, threshold)
		return
	}
	if t.windowExpired(record, now) {
		delete(t.attempts, scopeKey)
		t.attempts[scopeKey] = t.firstRecord(now, threshold)
		return
	}

	next := &abuseRecord{count: record.count + 1, firstAttempt: record.firstAttempt}
	if next.count >= threshold {
		next.lockedUntil = now.Add(time.Duration(t.config.LockoutSeconds) * time.Second)
		next.locked = true
	}
	delete(t.attempts, scopeKey)
	t.attempts[scopeKey] = next
}

func (t *AuthThrottle) firstRecord(now time.Time, threshold int) *abuseRecord {
	record := &abuseRecord{count: 1, firstAttempt: now}
	// 阈值 <= 1 时第一次失败即封禁（Node 侧 createFirstRecord 的同款处理）。
	if threshold <= 1 {
		record.lockedUntil = now.Add(time.Duration(t.config.LockoutSeconds) * time.Second)
		record.locked = true
	}
	return record
}

func (t *AuthThrottle) windowExpired(record *abuseRecord, now time.Time) bool {
	return now.Sub(record.firstAttempt) >= time.Duration(t.config.WindowSeconds)*time.Second
}

// sweep 清理过期条目，并在超量时按插入顺序淘汰最旧者。
func (t *AuthThrottle) sweep(now time.Time) {
	if !t.lastSweepAt.IsZero() && now.Sub(t.lastSweepAt) < abuseSweepInterval {
		return
	}
	t.lastSweepAt = now
	for scopeKey, record := range t.attempts {
		if record.locked {
			if !record.lockedUntil.After(now) {
				delete(t.attempts, scopeKey)
			}
			continue
		}
		if t.windowExpired(record, now) {
			delete(t.attempts, scopeKey)
		}
	}
	if len(t.attempts) <= maxTrackedAbuseEntries {
		return
	}
	excess := len(t.attempts) - maxTrackedAbuseEntries
	for scopeKey := range t.attempts {
		if excess == 0 {
			break
		}
		delete(t.attempts, scopeKey)
		excess--
	}
}

func retryAfter(until time.Time, now time.Time) *int {
	seconds := int(until.Sub(now).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	// Node 侧是 Math.ceil((lockedUntil - now) / 1000)：不足一秒也算 1 秒。
	if until.Sub(now)%time.Second != 0 {
		seconds++
	}
	return &seconds
}
