package limit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// KeyQuota 是密钥维度的限额字段快照，字段与 Node 侧 key 对象上的配额列一一对应。
//
// 取自认证快照/cfgsync，不在热路径查库：Node 侧这些字段来自 authState.key，
// 也是认证阶段一次性读出的。
type KeyQuota struct {
	KeyID  int64
	UserID int64
	// KeyHash 是 usage_ledger.key 列存的密钥明文（账本按密钥字符串聚合，不按 id）。
	KeyHash string

	Limit5hUSD              *float64
	Limit5hResetMode        ResetMode
	LimitDailyUSD           *float64
	DailyResetTime          string
	DailyResetMode          ResetMode
	LimitWeeklyUSD          *float64
	LimitMonthlyUSD         *float64
	LimitTotalUSD           *float64
	LimitConcurrentSessions int

	CostResetAt        *time.Time
	Limit5hCostResetAt *time.Time
}

// UserQuota 是用户维度的限额字段快照。
type UserQuota struct {
	UserID int64

	Limit5hUSD              *float64
	Limit5hResetMode        ResetMode
	LimitDailyUSD           *float64
	DailyResetTime          string
	DailyResetMode          ResetMode
	LimitWeeklyUSD          *float64
	LimitMonthlyUSD         *float64
	LimitTotalUSD           *float64
	LimitConcurrentSessions int
	// RPM 为 0 表示不限速。
	RPM int

	CostResetAt        *time.Time
	Limit5hCostResetAt *time.Time
}

// QuotaSource 提供限额快照。
type QuotaSource interface {
	KeyQuota(ctx context.Context, keyID int64) (KeyQuota, error)
	UserQuota(ctx context.Context, userID int64) (UserQuota, error)
}

// LedgerReader 是 Redis 不可用/缓存未命中时的账本回退（Node 侧 statistics 仓储的那几个求和函数）。
//
// 方法签名与 store.Pools 对应方法一致，故接线时直接传 *store.Pools。
type LedgerReader interface {
	SumLedgerCostInTimeRange(ctx context.Context, entityType store.LedgerEntityType, entityID any, start time.Time, end time.Time) (string, error)
	SumLedgerTotalCost(ctx context.Context, entityType store.LedgerEntityType, entityID any, resetAt *time.Time) (string, error)
}

// Config 是限流服务的装配依赖。
type Config struct {
	// Quotas 必需：没有限额快照就无从判定。
	Quotas QuotaSource
	// Ledger 可空：为空时账本回退路径不可用（Redis 不可用即放行并留痕）。
	Ledger LedgerReader
	// Redis 可空：为空时全部走账本回退。
	Redis *ratelimit.Client
	// LeaseSettings 可空。非空且 Redis 可用时，Key/User 的四个周期限额改走**租约**判定
	// （对齐 Node rate-limit-guard 的 checkCostLimitsWithLease）；为空时退回窗口/账本路径。
	// 两者不是「新旧实现」：租约是 Node 当前在用的判定口径，窗口路径是它的回退面。
	LeaseSettings QuotaLeaseSettingsReader
	// SessionID 取本次请求已分配的物理会话 id。
	//
	// 会话绑定步骤把结果写在 pctx 之外由实现自持（guard.SessionBinder 的约定），因此并发维度
	// 需要一个取值函数；为空时按 Node 的兜底语义现场生成一个并留痕（会牺牲原子性优势）。
	SessionID func(*pctx.Context) (string, error)
	// Location 是自然窗口（daily fixed / 周 / 月）的时区；为空按 UTC。
	// Node 侧的解析链是 system_settings.timezone -> env TZ -> UTC。
	Location *time.Location
	// SessionTTL 是并发会话的记账 TTL（Node 侧 SESSION_TTL，默认 300s）。
	SessionTTL time.Duration
	// Abuse 是认证失败节流阈值；零值字段按 ProxyAuthAbuseConfig 补齐。
	Abuse AuthAbuseConfig
	// Locale 覆盖默认语种（默认 zh-CN）。
	Locale string
	// Now 可注入，便于测试窗口边界。
	Now func() time.Time
	// Logger 为空时写 stderr。
	Logger *logx.Logger
}

// Service 实现 guard.RateLimiter：认证节流 + 请求级多维限流。
type Service struct {
	cfg      Config
	windows  *CostWindows
	sessions *SessionTracker
	throttle *AuthThrottle
	leases   *LeaseService
	loc      *time.Location
	log      *logx.Logger
	now      func() time.Time
}

// New 组装限流服务。
func New(cfg Config) (*Service, error) {
	if cfg.Quotas == nil {
		return nil, errors.New("limit: 缺少限额快照来源 QuotaSource")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	loc := cfg.Location
	if loc == nil {
		loc = time.UTC
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		cfg:      cfg,
		windows:  NewCostWindows(cfg.Redis, logger),
		sessions: NewSessionTracker(cfg.Redis, cfg.SessionTTL, logger),
		throttle: NewAuthThrottle(cfg.Abuse, now),
		leases:   NewLeaseService(cfg.Redis, cfg.LeaseSettings, cfg.Ledger, loc, now, logger),
		loc:      loc,
		log:      logger,
		now:      now,
	}, nil
}

// Throttle 实现 guard.RateLimiter：认证前的失败节流。
func (s *Service) Throttle(_ context.Context, clientIP string, candidateKey string) (guard.ThrottleDecision, error) {
	decision := s.throttle.Check(clientIP, candidateKey)
	return guard.ThrottleDecision{
		Allowed:           decision.Allowed,
		RetryAfterSeconds: decision.RetryAfterSeconds,
	}, nil
}

// RecordAuthSuccess 实现 guard.RateLimiter：认证成功清零失败计数。
func (s *Service) RecordAuthSuccess(_ context.Context, clientIP string, candidateKey string) {
	s.throttle.RecordSuccess(clientIP, candidateKey)
}

// RecordAuthFailure 实现 guard.RateLimiter：认证失败记账。
func (s *Service) RecordAuthFailure(_ context.Context, clientIP string, candidateKey string) {
	s.throttle.RecordFailure(clientIP, candidateKey)
}

// TrackCost 转发成功后记账（对应 Node 侧 RateLimitService.trackCost）。
//
// 记账失败只记日志：Node 侧同样是静默失败（记账不该让一次成功请求变成错误响应）。
func (s *Service) TrackCost(ctx context.Context, in TrackCostInput) {
	if err := s.windows.TrackCost(ctx, in, s.loc, s.now()); err != nil {
		s.log.Error("limit.track.failed", map[string]any{"error": err.Error(), "keyId": in.KeyID})
	}
}

// Sessions 暴露并发会话层，供转发路径占用/释放供应商并发额度。
func (s *Service) Sessions() *SessionTracker { return s.sessions }

// Check 实现 guard.RateLimiter：按 Node 侧 ProxyRateLimitGuard.ensure 的顺序逐维判定。
//
// 顺序是契约（注释里写明「硬上限优先、同窗口 Key->User 交替、资源/频率保护靠前」）：
//  1. Key 总限额   2. User 总限额
//  3. Key/User 并发 Session   4. User RPM
//  5. Key 5h   6. User 5h   7. Key 每日   8. User 每日
//  9. Key 周   10. User 周   11. Key 月   12. User 月
//
// 任一维触顶即返回拦截块；全部通过返回 nil。
func (s *Service) Check(ctx context.Context, req *pctx.Context) (*guard.RateLimitBlock, error) {
	// 会话 id 的默认来源是 cfg.SessionID（进程级装配时由接线方注入；为空则现场生成并留痕）。
	// 每请求视图（WithSession）走 check 并自带来源，见 wire.go。
	return s.check(ctx, req, func() (string, error) { return s.sessionID(req) })
}

// check 是限流判定的实现主体。会话 id 的取值由调用方给定：进程级调用走 cfg.SessionID 兜底，
// 每请求视图走会话绑定步骤的实际结果——并发额度记账必须用同一个物理会话 id 才有原子性可言。
func (s *Service) check(
	ctx context.Context,
	req *pctx.Context,
	sessionID func() (string, error),
) (*guard.RateLimitBlock, error) {
	auth, ok := req.Auth()
	// 无用户/密钥时直接放行：Node 侧 `if (!user || !key) return;`。
	if !ok || auth.KeyID == 0 || auth.UserID == 0 {
		return nil, nil
	}

	keyQuota, err := s.cfg.Quotas.KeyQuota(ctx, auth.KeyID)
	if err != nil {
		return s.failOpen("limit.check.key_quota_failed", err, map[string]any{"keyId": auth.KeyID})
	}
	userQuota, err := s.cfg.Quotas.UserQuota(ctx, auth.UserID)
	if err != nil {
		return s.failOpen("limit.check.user_quota_failed", err, map[string]any{"userId": auth.UserID})
	}

	now := s.now()
	keyCostResetAt := LaterReset(keyQuota.CostResetAt, userQuota.CostResetAt)
	user5hCostResetAt := LaterReset(userQuota.CostResetAt, userQuota.Limit5hCostResetAt)

	// 第一层：永久硬限制。
	if block := s.totalLimit(ctx, EntityKey, keyQuota.KeyID, keyQuota.KeyHash, keyQuota.LimitTotalUSD, keyCostResetAt, now); block != nil {
		return block, nil
	}
	if block := s.totalLimit(ctx, EntityUser, userQuota.UserID, "", userQuota.LimitTotalUSD, userQuota.CostResetAt, now); block != nil {
		return block, nil
	}

	// 第二层：资源/频率保护。
	if block := s.concurrentLimit(ctx, keyQuota, userQuota, now, sessionID); block != nil {
		return block, nil
	}
	if block := s.rpmLimit(ctx, userQuota, now); block != nil {
		return block, nil
	}

	// 租约结算计划：只记**本次确实以租约完成判定**的切片（主体 id + 窗口 + 生效的重置模式）。
	// 终态结算靠它扣减，判定与结算必须落在同一组键上，否则会扣到另一份键上而静默不生效。
	//
	// 为什么逐维记而不是开局按主体粗记一份：同一条请求的维度可以一半走租约、一半回退账本
	// （leaseCostLimit 的 decided=false，见下），粗记会让结算去扣**没有参与本次判定**的切片。
	// 为什么判完之后才落进上下文：中途读者不该看到半份计划。
	var leasePlan pctx.LeaseSettlementPlan

	// 第三层与第四层：短期到中长期周期限额，同窗口内 Key 先行。
	dimensions := []costDimension{
		{entity: EntityKey, id: keyQuota.KeyID, keyHash: keyQuota.KeyHash, period: Period5h, amount: keyQuota.Limit5hUSD, resetMode: defaultMode(keyQuota.Limit5hResetMode, ResetRolling), costResetAt: keyCostResetAt},
		{entity: EntityUser, id: userQuota.UserID, period: Period5h, amount: userQuota.Limit5hUSD, resetMode: defaultMode(userQuota.Limit5hResetMode, ResetRolling), costResetAt: userQuota.CostResetAt, limit5hCostResetAt: user5hCostResetAt},
		{entity: EntityKey, id: keyQuota.KeyID, keyHash: keyQuota.KeyHash, period: PeriodDaily, amount: keyQuota.LimitDailyUSD, resetMode: defaultMode(keyQuota.DailyResetMode, ResetFixed), resetTime: keyQuota.DailyResetTime, costResetAt: keyCostResetAt},
		{entity: EntityUser, id: userQuota.UserID, period: PeriodDaily, amount: userQuota.LimitDailyUSD, resetMode: defaultMode(userQuota.DailyResetMode, ResetFixed), resetTime: userQuota.DailyResetTime, costResetAt: userQuota.CostResetAt},
		{entity: EntityKey, id: keyQuota.KeyID, keyHash: keyQuota.KeyHash, period: PeriodWeekly, amount: keyQuota.LimitWeeklyUSD, resetMode: ResetFixed, costResetAt: keyCostResetAt},
		{entity: EntityUser, id: userQuota.UserID, period: PeriodWeekly, amount: userQuota.LimitWeeklyUSD, resetMode: ResetFixed, costResetAt: userQuota.CostResetAt},
		{entity: EntityKey, id: keyQuota.KeyID, keyHash: keyQuota.KeyHash, period: PeriodMonthly, amount: keyQuota.LimitMonthlyUSD, resetMode: ResetFixed, costResetAt: keyCostResetAt},
		{entity: EntityUser, id: userQuota.UserID, period: PeriodMonthly, amount: userQuota.LimitMonthlyUSD, resetMode: ResetFixed, costResetAt: userQuota.CostResetAt},
	}
	for _, dimension := range dimensions {
		if dimension.amount == nil || *dimension.amount <= 0 {
			continue
		}
		if s.leases != nil {
			if block, decided := s.leaseCostLimit(ctx, dimension, now); decided {
				// 这一维确实用租约判定了：它的切片必须进计划，哪怕后面被拒或 fail-open。
				// 取值域外的取值（例如库里写了一个既非 rolling 也非 fixed 的模式）会在这里
				// 被拦下并留痕，不静默少扣（见 rememberLeaseTarget）。
				rememberLeaseTarget(s.log, &leasePlan, dimension)
				if block != nil {
					// 触顶即拒：被拒的请求不产生上游成本，没有要扣的东西。
					return block, nil
				}
				continue
			}
		}
		current, exceeded, checkErr := s.costLimit(ctx, dimension, now)
		if checkErr != nil {
			// Fail Open 放行的请求照样会花钱：已经用租约判过的切片必须留在计划里，
			// 否则那些切片在刷新窗口内会比实际更宽——正是本接线要修的缺口。
			rememberLeasePlan(req, leasePlan)
			return s.failOpen("limit.check.cost_failed", checkErr, map[string]any{
				"entity": string(dimension.entity),
				"period": string(dimension.period),
			})
		}
		if exceeded {
			return s.costBlock(ctx, dimension, current, now), nil
		}
	}
	rememberLeasePlan(req, leasePlan)
	return nil, nil
}

// failOpen 是统一的失败开放出口：判定无法完成时放行并留 Error 日志。
//
// 与 Node 侧一致：限流的任何一环不可用都不应把正常请求变成 5xx（RateLimitService 内部同样
// 以 Fail Open 为默认）。代价是负载路径上可能出现额度超支，故必须留痕以便排查。
func (s *Service) failOpen(event string, err error, fields map[string]any) (*guard.RateLimitBlock, error) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["error"] = err.Error()
	fields["note"] = "限流判定失败，按 Fail Open 放行"
	s.log.Error(event, fields)
	return nil, nil
}

// concurrentLimit 复刻第 3 维：Key/User 并发 Session 的原子「检查 + 追踪」。
func (s *Service) concurrentLimit(
	ctx context.Context,
	keyQuota KeyQuota,
	userQuota UserQuota,
	now time.Time,
	sessionID func() (string, error),
) *guard.RateLimitBlock {
	effectiveKeyLimit, normalizedUserLimit := resolveConcurrentLimits(keyQuota.LimitConcurrentSessions, userQuota.LimitConcurrentSessions)
	if effectiveKeyLimit <= 0 && normalizedUserLimit <= 0 {
		return nil
	}

	id, err := sessionID()
	if err != nil {
		s.log.Error("limit.check.session_id_failed", map[string]any{"error": err.Error()})
		return nil
	}

	result, err := s.sessions.CheckAndTrackKeyUserSession(ctx, keyQuota.KeyID, userQuota.UserID, id, effectiveKeyLimit, normalizedUserLimit)
	if err != nil {
		s.log.Error("limit.check.concurrent_failed", map[string]any{"error": err.Error()})
		return nil
	}
	if result.Allowed {
		return nil
	}

	current := result.Current
	limit := result.Limit
	if current == 0 {
		// 兜底：Lua 的返回项缺失时按对应维度的实时计数填补（Node 侧同样的 fallback）。
		if result.RejectedBy == string(EntityUser) {
			current, limit = result.UserCount, normalizedUserLimit
		} else {
			current, limit = result.KeyCount, effectiveKeyLimit
		}
	}

	s.log.Warn("limit.check.concurrent_rejected", map[string]any{
		"rejectedBy": result.RejectedBy,
		"current":    current,
		"limit":      limit,
	})
	return &guard.RateLimitBlock{
		Status:    429,
		ErrorType: "rate_limit_error",
		Message: s.render(MessageConcurrentSessionsExceeded, map[string]string{
			"current": strconv.Itoa(current),
			"limit":   strconv.Itoa(limit),
		}),
		RetryAfterSeconds: retryAfterSeconds(&now, now),
		BlockedReason:     s.blockedReason("concurrent_sessions", float64(current), float64(limit), &now),
		// Node 的并发拦截用「此刻」作为重置时刻（guard 里 resetTime = new Date().toISOString()）。
		LimitType: "concurrent_sessions",
		Current:   float64(current),
		Limit:     float64(limit),
		ResetTime: isoMillis(now),
	}
}

// ResolveKeyConcurrentSessionLimit 复刻 resolveKeyConcurrentSessionLimit
// （src/lib/rate-limit/concurrent-session-limit.ts:34-45）：Key 自身上限优先，否则回退 User 上限。
//
// 导出给管理面读档用：getKeyLimitUsage / getKeyQuotaUsage 展示的 `concurrentSessions.limit`
// 与数据面真正拦下的上限必须是同一个数（否则弹窗说的上限与实际拦截口径不一致）。
func ResolveKeyConcurrentSessionLimit(keyLimit, userLimit int) int {
	if normalized := normalizeConcurrentLimit(keyLimit); normalized > 0 {
		return normalized
	}
	return normalizeConcurrentLimit(userLimit)
}

// resolveConcurrentLimits 复刻 resolveKeyUserConcurrentSessionLimits：
// 同时给出有效 Key 上限与归一化 User 上限（并发判定两个维度都要）。
func resolveConcurrentLimits(keyLimit, userLimit int) (int, int) {
	return ResolveKeyConcurrentSessionLimit(keyLimit, userLimit), normalizeConcurrentLimit(userLimit)
}

// normalizeConcurrentLimit 复刻 normalizeConcurrentSessionLimit：非正即 0（无限制）。
func normalizeConcurrentLimit(value int) int {
	if value <= 0 {
		return 0
	}
	return value
}

// sessionID 取本次请求的物理会话 id，缺失时按 Node 的兜底语义现场生成。
func (s *Service) sessionID(req *pctx.Context) (string, error) {
	if s.cfg.SessionID != nil {
		id, err := s.cfg.SessionID(req)
		if err != nil {
			return "", err
		}
		if id != "" {
			return id, nil
		}
	}
	generated, err := generateSessionID(s.now())
	if err != nil {
		return "", err
	}
	// Node 侧同样在这条路径上留 warn：现场生成的 id 没能写回会话，会牺牲原子性优势。
	s.log.Warn("limit.check.session_id_generated", map[string]any{
		"sessionId": generated,
		"note":      "会话绑定未接线，并发记账可能出现原子性缺口",
	})
	return generated, nil
}

// generateSessionID 复刻 SessionManager.generateSessionId：sess_{base36 毫秒}_{12 位十六进制}。
func generateSessionID(now time.Time) (string, error) {
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("limit: 生成会话 id 失败: %w", err)
	}
	return "sess_" + strconv.FormatInt(now.UnixMilli(), 36) + "_" + hex.EncodeToString(random[:]), nil
}

// rpmLimit 复刻第 4 维：User 每分钟请求数。
func (s *Service) rpmLimit(ctx context.Context, userQuota UserQuota, now time.Time) *guard.RateLimitBlock {
	if userQuota.RPM <= 0 || !s.windows.Ready() {
		return nil
	}
	key := RPMWindowKey(userQuota.UserID)
	raw := s.windows.client.Raw()
	cutoff := now.Add(-time.Minute).UnixMilli()

	pipe := raw.Pipeline()
	pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(cutoff, 10))
	countCmd := pipe.ZCard(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil {
		s.log.Error("limit.check.rpm_failed", map[string]any{"error": err.Error(), "userId": userQuota.UserID})
		return nil
	}
	count := countCmd.Val()
	if count >= int64(userQuota.RPM) {
		resetAt := now.Add(time.Minute)
		s.log.Warn("limit.check.rpm_rejected", map[string]any{"userId": userQuota.UserID, "current": count, "limit": userQuota.RPM})
		return &guard.RateLimitBlock{
			Status:    429,
			ErrorType: "rate_limit_error",
			Message: s.render(MessageRPMExceeded, map[string]string{
				"current":   strconv.FormatInt(count, 10),
				"limit":     strconv.Itoa(userQuota.RPM),
				"resetTime": isoMillis(resetAt),
			}),
			RetryAfterSeconds: retryAfterSeconds(&resetAt, now),
			BlockedReason:     s.blockedReason("rpm", float64(count), float64(userQuota.RPM), &resetAt),
			LimitType:         "rpm",
			Current:           float64(count),
			Limit:             float64(userQuota.RPM),
			ResetTime:         isoMillis(resetAt),
		}
	}

	// 放行时记录本次请求；失败不影响放行（Node 侧同样吞掉）。
	record := raw.Pipeline()
	record.ZAdd(ctx, key, redis.Z{Score: float64(now.UnixMilli()), Member: strconv.FormatInt(now.UnixMilli(), 10) + ":" + strconv.FormatInt(count, 10)})
	record.Expire(ctx, key, 120*time.Second)
	if _, err := record.Exec(ctx); err != nil {
		s.log.Error("limit.check.rpm_record_failed", map[string]any{"error": err.Error(), "userId": userQuota.UserID})
	}
	return nil
}

// totalLimit 复刻第 1、2 维：Key/User 的永久总额度（带 5 分钟 Redis 缓存）。
func (s *Service) totalLimit(ctx context.Context, e Entity, id int64, keyHash string, limit *float64, resetAt *time.Time, now time.Time) *guard.RateLimitBlock {
	if limit == nil || *limit <= 0 {
		return nil
	}
	// Key 维度缺密钥字符串时无法在账本里定位：Node 侧在此处记 warn 并跳过执行。
	if e == EntityKey && keyHash == "" {
		s.log.Warn("limit.check.total_missing_key", map[string]any{
			"keyId": id,
			"note":  "缺少密钥字符串，跳过总额度执行",
		})
		return nil
	}

	current, err := s.totalCost(ctx, e, id, keyHash, resetAt, now)
	if err != nil {
		s.log.Error("limit.check.total_failed", map[string]any{"error": err.Error(), "entity": string(e), "id": id})
		return nil
	}
	if current < *limit {
		return nil
	}

	// Node 侧对总额度用一个「不会到来」的重置时刻，使 Retry-After 实际等同于永久封禁。
	noReset := time.Date(9999, 12, 31, 23, 59, 59, 999000000, time.UTC)
	s.log.Warn("limit.check.total_rejected", map[string]any{"entity": string(e), "id": id, "current": current, "limit": *limit})
	return &guard.RateLimitBlock{
		Status:    402,
		ErrorType: "rate_limit_error",
		Message: s.render(MessageTotalExceeded, map[string]string{
			"current": formatUSD(current),
			"limit":   formatUSD(*limit),
		}),
		RetryAfterSeconds: retryAfterSeconds(&noReset, now),
		BlockedReason:     s.blockedReason("usd_total", current, *limit, &noReset),
		LimitType:         "usd_total",
		Current:           current,
		Limit:             *limit,
		// Node 对总额度也传那个「不会到来」的重置时刻，故 reset_time 非空。
		ResetTime: isoMillis(noReset),
	}
}

// totalCost 读总额度用量：Redis 缓存 -> 账本 -> 回写缓存。
func (s *Service) totalCost(ctx context.Context, e Entity, id int64, keyHash string, resetAt *time.Time, now time.Time) (float64, error) {
	if !s.windows.Ready() {
		return s.totalCostFromLedger(ctx, e, id, keyHash, resetAt)
	}
	cacheKey := TotalCostCacheKey(e, id, keyHash, resetAt)
	raw := s.windows.client.Raw()
	cached, err := raw.Get(ctx, cacheKey).Result()
	if err == nil {
		return ParseCostText(cached), nil
	}
	if !isRedisMiss(err) {
		s.log.Warn("limit.check.total_cache_read_failed", map[string]any{"error": err.Error()})
		return s.totalCostFromLedger(ctx, e, id, keyHash, resetAt)
	}

	current, err := s.totalCostFromLedger(ctx, e, id, keyHash, resetAt)
	if err != nil {
		return 0, err
	}
	// 回写缓存不阻塞判定（Node 侧同样是不 await 的 fire-and-forget）。
	go func() {
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if setErr := raw.Set(writeCtx, cacheKey, FormatCostText(current), 300*time.Second).Err(); setErr != nil {
			s.log.Warn("limit.check.total_cache_write_failed", map[string]any{"error": setErr.Error()})
		}
	}()
	return current, nil
}

func (s *Service) totalCostFromLedger(ctx context.Context, e Entity, id int64, keyHash string, resetAt *time.Time) (float64, error) {
	if s.cfg.Ledger == nil {
		return 0, errors.New("limit: 账本回退未接线")
	}
	entityID := any(id)
	if e == EntityKey {
		// 账本按密钥字符串聚合（usage_ledger.key），不按 id。
		entityID = keyHash
	}
	total, err := s.cfg.Ledger.SumLedgerTotalCost(ctx, store.LedgerEntityType(e.ledgerEntity()), entityID, resetAt)
	if err != nil {
		return 0, err
	}
	return ParseCostText(total), nil
}

// costDimension 是单个周期限额的判定输入。
type costDimension struct {
	entity  Entity
	id      int64
	keyHash string
	period  Period
	amount  *float64
	// resetTime 仅 daily fixed 使用（"HH:mm"）。
	resetTime          string
	resetMode          ResetMode
	costResetAt        *time.Time
	limit5hCostResetAt *time.Time
}

// costLimit 判定单个周期限额，返回当前用量与是否触顶。
//
// 读路径与 Node 一致：Redis 快路径 -> 缓存未命中/Redis 故障 -> 账本回退 -> 回写固定窗口缓存。
func (s *Service) costLimit(ctx context.Context, dimension costDimension, now time.Time) (float64, bool, error) {
	current, err := s.currentCost(ctx, dimension, now)
	if err != nil {
		return 0, false, err
	}
	return current, current >= *dimension.amount, nil
}

func (s *Service) currentCost(ctx context.Context, dimension costDimension, now time.Time) (float64, error) {
	remoteOK := s.windows.Ready()

	// 5h 固定窗口：Node 侧同样只读固定窗口键，没有账本回退（Redis 不可用时计为 0）。
	if dimension.period == Period5h && dimension.resetMode == ResetFixed {
		if !remoteOK {
			return 0, nil
		}
		state, err := s.windows.Fixed5hWindowState(ctx, dimension.entity, dimension.id, now)
		if err != nil {
			s.log.Error("limit.check.fixed5h_failed", map[string]any{"error": err.Error()})
			return s.costFromLedger(ctx, dimension, now)
		}
		return state.Current, nil
	}

	if remoteOK {
		// 滚动窗口（5h rolling / daily rolling）与固定窗口在 Redis 里是两种表示，读法不同。
		if isRollingWindow(dimension) {
			current, exists, err := s.windows.RollingCost(ctx, dimension.entity, dimension.id, dimension.period, now)
			if err != nil {
				s.log.Error("limit.check.rolling_failed", map[string]any{"error": err.Error(), "period": string(dimension.period)})
				return s.costFromLedger(ctx, dimension, now)
			}
			// 值非 0，或键确实存在（说明窗口内真的是 0）时无需回退。
			if current != 0 || exists {
				return current, nil
			}
			s.log.Info("limit.check.cache_miss", map[string]any{
				"entity": string(dimension.entity),
				"period": string(dimension.period),
				"note":   "Redis 无窗口键，回退账本",
			})
			return s.costFromLedger(ctx, dimension, now)
		}
		return s.fixedCost(ctx, dimension, now)
	}

	return s.costFromLedger(ctx, dimension, now)
}

// fixedCost 读固定窗口键（daily fixed / 周 / 月）。
func (s *Service) fixedCost(ctx context.Context, dimension costDimension, now time.Time) (float64, error) {
	key := FixedWindowKey(dimension.entity, dimension.id, dimension.period, dimension.resetTime)
	current, exists, err := s.windows.FixedCost(ctx, key)
	if err != nil {
		s.log.Error("limit.check.fixed_read_failed", map[string]any{"error": err.Error(), "period": string(dimension.period)})
		return s.costFromLedger(ctx, dimension, now)
	}
	if !exists {
		s.log.Info("limit.check.cache_miss", map[string]any{
			"entity": string(dimension.entity),
			"period": string(dimension.period),
			"key":    key,
			"note":   "Redis 无窗口键，回退账本",
		})
		return s.costFromLedger(ctx, dimension, now)
	}
	return current, nil
}

// costFromLedger 复刻 checkCostLimitsFromDatabase 的窗口口径，并按需回写固定窗口缓存。
func (s *Service) costFromLedger(ctx context.Context, dimension costDimension, now time.Time) (float64, error) {
	if s.cfg.Ledger == nil {
		return 0, errors.New("limit: 账本回退未接线")
	}

	// 只有「User + 5h + 非固定」这一种组合会用 later(costResetAt, limit5hCostResetAt)；
	// 其它窗口继续沿用完整的重置边界，免得 5h 专用重置污染更长窗口。
	effectiveResetAt := dimension.costResetAt
	if dimension.entity == EntityUser && dimension.period == Period5h && dimension.resetMode != ResetFixed {
		effectiveResetAt = LaterReset(dimension.costResetAt, dimension.limit5hCostResetAt)
	}
	start := ClipStartByResetAt(
		WindowStart(dimension.period, now, dimension.resetTime, dimension.resetMode, s.loc),
		effectiveResetAt,
	)

	entityID := any(dimension.id)
	if dimension.entity == EntityKey {
		if dimension.keyHash == "" {
			return 0, errors.New("limit: Key 维度账本回退缺少密钥字符串")
		}
		entityID = dimension.keyHash
	}
	total, err := s.cfg.Ledger.SumLedgerCostInTimeRange(ctx, store.LedgerEntityType(dimension.entity.ledgerEntity()), entityID, start, now)
	if err != nil {
		return 0, err
	}
	current := ParseCostText(total)

	// 固定窗口的缓存回写：滚动窗口需要逐条消费明细才能忠实重建 ZSET，账本层目前只提供合计，
	// 故只回写固定窗口（daily fixed 与周/月；5h fixed 读的是自己的窗口键，不参与回写）。
	if s.windows.Ready() && !isRollingWindow(dimension) {
		key := FixedWindowKey(dimension.entity, dimension.id, dimension.period, dimension.resetTime)
		ttl := TTLForPeriodWithMode(dimension.period, now, dimension.resetTime, dimension.resetMode, s.loc)
		if err := s.windows.WarmFixed(ctx, key, current, ttl); err != nil {
			s.log.Error("limit.check.cache_warm_failed", map[string]any{"error": err.Error(), "key": key})
		}
	}
	return current, nil
}

// isRollingWindow 判定该维度是否为滚动窗口（滚动与固定窗口在 Redis 里是不同的数据结构）。
func isRollingWindow(dimension costDimension) bool {
	if dimension.period == Period5h {
		return dimension.resetMode != ResetFixed
	}
	if dimension.period == PeriodDaily {
		return dimension.resetMode == ResetRolling
	}
	return false
}

// FixedWindowKey 给出固定窗口的键名（daily fixed 带重置时刻后缀，周/月不带）。
func FixedWindowKey(e Entity, id int64, period Period, resetTime string) string {
	if period == PeriodDaily {
		return CostDailyFixedKey(e, id, resetTime)
	}
	return CostPeriodFixedKey(e, id, period)
}

// costBlock 把触顶的周期维度翻译成拦截块。
//
// 带 ctx 是因为其中一分支要反查 5h 固定窗口的 TTL（一次 Redis 往返）。
func (s *Service) costBlock(ctx context.Context, dimension costDimension, current float64, now time.Time) *guard.RateLimitBlock {
	var (
		code      string
		limitType string
		resetAt   *time.Time
	)
	switch dimension.period {
	case Period5h:
		limitType = "usd_5h"
		if dimension.resetMode == ResetFixed {
			code = Message5hExceeded
			resetAt = ResetAtFromTTL(now, s.fixed5hTTL(ctx, dimension, now))
		} else {
			code = Message5hRollingExceeded
		}
	case PeriodDaily:
		limitType = "daily_quota"
		if dimension.resetMode == ResetRolling {
			code = MessageDailyRollingExceeded
		} else {
			code = MessageDailyQuotaExceeded
			next := NextDailyReset(now, dimension.resetTime, s.loc)
			resetAt = &next
		}
	case PeriodWeekly:
		code, limitType = MessageWeeklyExceeded, "usd_weekly"
		next := NextWeekStart(now, s.loc)
		resetAt = &next
	case PeriodMonthly:
		code, limitType = MessageMonthlyExceeded, "usd_monthly"
		next := NextMonthStart(now, s.loc)
		resetAt = &next
	}

	// Node 侧重置时刻取不到时兜底为「现在」，滚动窗口则显式传 null（无 Retry-After）。
	messageReset := now
	if resetAt != nil {
		messageReset = *resetAt
	}
	s.log.Warn("limit.check.cost_rejected", map[string]any{
		"entity":  string(dimension.entity),
		"period":  string(dimension.period),
		"current": current,
		"limit":   *dimension.amount,
	})
	return &guard.RateLimitBlock{
		Status:    402,
		ErrorType: "rate_limit_error",
		Message: s.render(code, map[string]string{
			"current":   formatUSD(current),
			"limit":     formatUSD(*dimension.amount),
			"resetTime": isoMillis(messageReset),
		}),
		RetryAfterSeconds: retryAfterSeconds(resetAt, now),
		BlockedReason:     s.blockedReason(limitType, current, *dimension.amount, resetAt),
		LimitType:         limitType,
		Current:           current,
		Limit:             *dimension.amount,
		// 滚动窗口没有固定重置时刻（Node 传 null，响应里 reset_time 为 null 且不写 Retry-After）。
		ResetTime: isoMillisOrEmpty(resetAt),
	}
}

// fixed5hTTL 取 5h 固定窗口键剩余 TTL，用于给出重置时刻。
//
// 用调用方的 ctx 而不是 context.Background()：客户端已经断开时，这一读还会白占一次
// Redis 往返与连接池名额（调用点处于限流判定后的封顶响应路径）。
func (s *Service) fixed5hTTL(ctx context.Context, dimension costDimension, now time.Time) *int64 {
	if !s.windows.Ready() {
		return nil
	}
	state, err := s.windows.Fixed5hWindowState(ctx, dimension.entity, dimension.id, now)
	if err != nil || state.ResetAt == nil {
		return nil
	}
	seconds := int64(state.ResetAt.Sub(now) / time.Second)
	return &seconds
}

// render 渲染文案（语种按配置覆盖，默认 zh-CN）。
func (s *Service) render(code string, params map[string]string) string {
	locale := s.cfg.Locale
	if locale == "" {
		locale = DefaultLocale
	}
	return RenderMessage(locale, code, params)
}

// blockedReason 生成落库口径的拦截原因。
//
// Node 侧限流拦截不写 blocked_by（保持 NULL），而是把元数据拼进 error_message 的
// `rate_limit_metadata` 后缀；这里给出同一形制的 JSON，供接线层拼接。
func (s *Service) blockedReason(limitType string, current float64, limit float64, resetAt *time.Time) string {
	reset := "null"
	if resetAt != nil {
		reset = `"` + isoMillis(*resetAt) + `"`
	}
	return `{"type":"rate_limit_error","limit_type":"` + limitType + `","current_usage":` +
		FormatCostText(current) + `,"limit_value":` + FormatCostText(limit) + `,"reset_time":` + reset + `}`
}

// formatUSD 复刻 Node 侧 toFixed(4)。
func formatUSD(value float64) string {
	return strconv.FormatFloat(value, 'f', 4, 64)
}

// defaultMode 在模式缺省时给出默认值（Node 侧 ?? 的等价物）。
func defaultMode(mode ResetMode, fallback ResetMode) ResetMode {
	if mode == "" {
		return fallback
	}
	return mode
}

// isRedisMiss 判定是不是「键不存在」（go-redis 的 redis.Nil）。
func isRedisMiss(err error) bool {
	return errors.Is(err, redis.Nil)
}
