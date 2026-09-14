package limit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/redis/go-redis/v9"
)

// fixedCostWindowLua 是 service.ts 里内联的 TRACK_FIXED_COST_WINDOW_LUA，逐字复制。
//
// 它没有进 lua-scripts.ts 的 18 段清单（Node 侧就是内联的），因此本包也只能内联一份：
// 语义是「键不存在则 SET 带 TTL，存在则 INCRBYFLOAT」，即只有首个记账才设置窗口 TTL。
const fixedCostWindowLua = `
    local existing = redis.call("GET", KEYS[1])
    if existing then
      return redis.call("INCRBYFLOAT", KEYS[1], ARGV[1])
    end

    redis.call("SET", KEYS[1], ARGV[1], "EX", ARGV[2])
    return tonumber(ARGV[1])
  `

// Fixed5hState 是 5h 固定窗口的读取结果。
//
// Exists 与 Current == 0 必须分开看：Node 侧用 exists 判断「键不存在」，
// 那是「窗口还没建立」而不是「窗口内确实没有消费」。
type Fixed5hState struct {
	Current float64
	Exists  bool
	// ResetAt 由键的 TTL 反推；键不存在或无 TTL 时为 nil。
	ResetAt *time.Time
}

// CostWindows 是成本窗口的读写层：每个方法对应 Node 侧的一处调用点。
type CostWindows struct {
	client *ratelimit.Client
	log    *logx.Logger
}

// NewCostWindows 组装成本窗口读写层；client 为 nil 时所有方法按 Redis 不可用处理。
func NewCostWindows(client *ratelimit.Client, logger *logx.Logger) *CostWindows {
	if logger == nil {
		logger = logx.New(nil)
	}
	return &CostWindows{client: client, log: logger}
}

// Ready 报告 Redis 是否可用（对应 Node 侧 redis.status === "ready"）。
func (w *CostWindows) Ready() bool {
	return w.client != nil && w.client.Raw() != nil
}

// RollingCost 读滚动窗口累计值（5h 或 daily），并报告键是否存在。
func (w *CostWindows) RollingCost(ctx context.Context, e Entity, id int64, period Period, now time.Time) (float64, bool, error) {
	// 参数先判：周期非法是调用方缺陷，不该被「Redis 不可用」掩盖成零值。
	var constName, key string
	var window int64
	switch period {
	case Period5h:
		constName, key, window = "GET_COST_5H_ROLLING_WINDOW", Cost5hKey(e, id, ResetRolling), window5hMillis
	case PeriodDaily:
		constName, key, window = "GET_COST_DAILY_ROLLING_WINDOW", CostDailyRollingKey(e, id), window24hMillis
	default:
		return 0, false, fmt.Errorf("limit: 不支持的滚动周期 %q", period)
	}
	if !w.Ready() {
		return 0, false, nil
	}

	value, err := w.client.EvalConst(ctx, constName, []string{key}, []any{now.UnixMilli(), window})
	if err != nil {
		return 0, false, err
	}
	current := ParseCostText(value)
	if current != 0 {
		return current, true, nil
	}
	// 值为 0 时再判键是否存在：脚本自身会清理过期成员，故 0 既可能是「真的没消费」，
	// 也可能是「键被清空」（此时必须回退到账本，否则 Redis 一被清空就能超支）。
	exists, err := w.client.Raw().Exists(ctx, key).Result()
	if err != nil {
		return 0, false, err
	}
	return current, exists > 0, nil
}

// FixedCost 读固定窗口累计值（daily fixed / weekly / monthly / 5h fixed 的键形制）。
func (w *CostWindows) FixedCost(ctx context.Context, key string) (float64, bool, error) {
	if !w.Ready() {
		return 0, false, nil
	}
	raw, err := w.client.Raw().Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return ParseCostText(raw), true, nil
}

// Fixed5hWindowState 读 5h 固定窗口：值 + 由 TTL 反推的重置时刻。
func (w *CostWindows) Fixed5hWindowState(ctx context.Context, e Entity, id int64, now time.Time) (Fixed5hState, error) {
	if !w.Ready() {
		return Fixed5hState{}, nil
	}
	key := Cost5hKey(e, id, ResetFixed)
	pipe := w.client.Raw().Pipeline()
	getCmd := pipe.Get(ctx, key)
	ttlCmd := pipe.TTL(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return Fixed5hState{}, err
	}
	raw, err := getCmd.Result()
	if errors.Is(err, redis.Nil) {
		return Fixed5hState{}, nil
	}
	if err != nil {
		return Fixed5hState{}, err
	}

	state := Fixed5hState{Current: ParseCostText(raw), Exists: true}
	ttl := ttlCmd.Val()
	if ttl > 0 {
		seconds := int64(ttl / time.Second)
		resetAt := now.Add(time.Duration(seconds) * time.Second)
		state.ResetAt = &resetAt
	}
	return state, nil
}

// WarmFixed 把账本回退算出的固定窗口值写回 Redis（对应 Node 侧 Cache Warming）。
//
// 只用于固定窗口：滚动窗口的缓存重建需要逐条消费记录（时间戳 + 明细），
// 而账本读取层目前只提供合计值，无法忠实重建 ZSET 的衰减边界。
// ponytail: 滚动窗口暂不预热；若账本层补出「窗口内消费明细」查询，再按 Node 的 warmRollingCostZset 重建。
func (w *CostWindows) WarmFixed(ctx context.Context, key string, value float64, ttlSeconds int64) error {
	if !w.Ready() || ttlSeconds <= 0 {
		return nil
	}
	return w.client.Raw().Set(ctx, key, FormatCostText(value), time.Duration(ttlSeconds)*time.Second).Err()
}

// TrackCostInput 是记账入参，字段与 Node 侧 trackCost 的 options 对应。
//
// 每个维度自带重置模式与重置时刻：同一进程里不同供应商/密钥可以配不同窗口形制。
type TrackCostInput struct {
	KeyID      int64
	ProviderID int64
	UserID     int64
	// UserIDSet 区分「没有用户维度」与 UserID == 0。
	UserIDSet bool
	Cost      float64
	// CreatedAt 为零值时按当前时刻记账（回放/补记账可显式传入）。
	CreatedAt time.Time
	// RequestID 会写进滚动窗口成员名，便于按请求对账。
	RequestID string

	Key5hResetMode      ResetMode
	KeyResetTime        string
	KeyResetMode        ResetMode
	Provider5hResetMode ResetMode
	ProviderResetTime   string
	ProviderResetMode   ResetMode
	User5hResetMode     ResetMode
	UserResetTime       string
	UserResetMode       ResetMode
}

// TrackCost 复刻 trackCost：一次管道写入所有维度。
//
// 5h 与 daily 支持滚动（ZSET + Lua）与固定（STRING）两种表示；周/月固定窗口只记 Key 与
// Provider —— User 的长周期额度由账本（PostgreSQL）负责，与 Node 侧注释一致。
func (w *CostWindows) TrackCost(ctx context.Context, in TrackCostInput, loc *time.Location, fallbackNow time.Time) error {
	if !w.Ready() || in.Cost <= 0 {
		return nil
	}
	now := in.CreatedAt
	if now.IsZero() {
		now = fallbackNow
	}

	pipe := w.client.Raw().Pipeline()
	w.queue5h(ctx, pipe, EntityKey, in.KeyID, in.Key5hResetMode, in.Cost, now, in.RequestID)
	w.queue5h(ctx, pipe, EntityProvider, in.ProviderID, in.Provider5hResetMode, in.Cost, now, in.RequestID)
	if in.UserIDSet {
		w.queue5h(ctx, pipe, EntityUser, in.UserID, in.User5hResetMode, in.Cost, now, in.RequestID)
	}

	// daily：rolling 用 ZSET 记账，fixed 用 INCRBYFLOAT + EXPIRE（命令序列与 Node 一致）。
	w.queueDaily(ctx, pipe, EntityKey, in.KeyID, in.KeyResetMode, in.KeyResetTime, in.Cost, now, in.RequestID, loc)
	w.queueDaily(ctx, pipe, EntityProvider, in.ProviderID, in.ProviderResetMode, in.ProviderResetTime, in.Cost, now, in.RequestID, loc)
	if in.UserIDSet {
		w.queueDaily(ctx, pipe, EntityUser, in.UserID, in.UserResetMode, in.UserResetTime, in.Cost, now, in.RequestID, loc)
	}

	ttlWeekly := TTLForPeriod(PeriodWeekly, now, DefaultResetTime, loc)
	ttlMonthly := TTLForPeriod(PeriodMonthly, now, DefaultResetTime, loc)
	for _, target := range []struct {
		id  int64
		ttl int64
		key string
	}{
		{in.KeyID, ttlWeekly, CostPeriodFixedKey(EntityKey, in.KeyID, PeriodWeekly)},
		{in.KeyID, ttlMonthly, CostPeriodFixedKey(EntityKey, in.KeyID, PeriodMonthly)},
		{in.ProviderID, ttlWeekly, CostPeriodFixedKey(EntityProvider, in.ProviderID, PeriodWeekly)},
		{in.ProviderID, ttlMonthly, CostPeriodFixedKey(EntityProvider, in.ProviderID, PeriodMonthly)},
	} {
		pipe.IncrByFloat(ctx, target.key, in.Cost)
		pipe.Expire(ctx, target.key, time.Duration(target.ttl)*time.Second)
	}

	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	return nil
}

func (w *CostWindows) queue5h(ctx context.Context, pipe redis.Pipeliner, e Entity, id int64, mode ResetMode, cost float64, now time.Time, requestID string) {
	if mode == ResetFixed {
		if err := pipe.Eval(ctx, fixedCostWindowLua, []string{Cost5hKey(e, id, ResetFixed)}, FormatCostText(cost), int64(5*3600)).Err(); err != nil {
			w.log.Error("limit.track.queue_failed", map[string]any{"error": err.Error()})
		}
		return
	}
	w.queueRolling(ctx, pipe, Cost5hKey(e, id, ResetRolling), cost, now, window5hMillis, requestID, ttl5hRolling)
}

func (w *CostWindows) queueDaily(ctx context.Context, pipe redis.Pipeliner, e Entity, id int64, mode ResetMode, resetTime string, cost float64, now time.Time, requestID string, loc *time.Location) {
	if mode == ResetRolling {
		w.queueRolling(ctx, pipe, CostDailyRollingKey(e, id), cost, now, window24hMillis, requestID, ttlDailyRolling)
		return
	}
	key := CostDailyFixedKey(e, id, resetTime)
	ttl := TTLForPeriodWithMode(PeriodDaily, now, resetTime, ResetFixed, loc)
	pipe.IncrByFloat(ctx, key, cost)
	pipe.Expire(ctx, key, time.Duration(ttl)*time.Second)
}

// queueRolling 把一次消费写进滚动窗口 ZSET。
//
// 管道里用 EVAL 而不是 EVALSHA：管道内命令只在 Exec 时统一执行，此时 NOSCRIPT 已经没有
// 重试机会（客户端不会把管道拆开重发）。Node 侧 ioredis 的 pipeline.eval 同样直接发脚本正文，
// 这里与之一致；正文取自内嵌清单，不另写一份。
//
// 脚本取不到时只记日志：单条记账失败不应让整次请求失败（Node 侧同样静默吞掉）。
func (w *CostWindows) queueRolling(ctx context.Context, pipe redis.Pipeliner, key string, cost float64, now time.Time, windowMillis int64, requestID string, ttlSeconds int64) {
	source, err := w.rollingScriptSource()
	if err != nil {
		w.log.Error("limit.track.script_missing", map[string]any{"error": err.Error(), "key": key})
		return
	}
	if err := pipe.Eval(
		ctx,
		source,
		[]string{key},
		FormatCostText(cost),
		now.UnixMilli(),
		windowMillis,
		requestID,
		ttlSeconds,
	).Err(); err != nil {
		w.log.Error("limit.track.queue_failed", map[string]any{"error": err.Error(), "key": key})
	}
}

func (w *CostWindows) rollingScriptSource() (string, error) {
	if w.client == nil || w.client.Scripts() == nil {
		return "", errors.New("limit: 脚本注册表缺失")
	}
	script, ok := w.client.Scripts().LookupConst("TRACK_COST_ROLLING_WINDOW")
	if !ok {
		return "", errors.New("limit: 未登记的脚本 TRACK_COST_ROLLING_WINDOW")
	}
	return script.Source, nil
}

// TrackFixedCostWindowScript 暴露内联脚本正文，供测试与 Node 侧同名内联脚本对账。
func TrackFixedCostWindowScript() string { return fixedCostWindowLua }

// ParseCostText 把 Redis 返回的成本值解析为浮点数；非法值按 0 处理（Node 侧 parseFloat 语义）。
func ParseCostText(value any) float64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case float64:
		return typed
	case int64:
		return float64(typed)
	case string:
		return parseFloatOrZero(typed)
	case []byte:
		return parseFloatOrZero(string(typed))
	default:
		return parseFloatOrZero(fmt.Sprint(typed))
	}
}

func parseFloatOrZero(raw string) float64 {
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// FormatCostText 是记账数值的文本形制（与 Node 侧 cost.toString() 的最小表示一致）。
func FormatCostText(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
