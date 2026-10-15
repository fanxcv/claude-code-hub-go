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

// RollingRead 是滚动窗口读的入参：脚本常量名 + 窗口长度 + 取值时刻。
type RollingRead struct {
	ScriptConst  string
	WindowMillis int64
	Now          time.Time
}

// CacheRead 是批量读里的一项：Key 为缓存键；Rolling 非空按滚动窗口（EVAL）读，否则按字符串（GET）读；
// KeepTTL 为真时同批取回剩余 TTL（5h 固定窗口由 TTL 反推重置时刻）。
type CacheRead struct {
	Key     string
	Rolling *RollingRead
	KeepTTL bool
}

// WindowValue 是批量读出的单个窗口取值。Err 只影响该键，同批其它键照常可用。
type WindowValue struct {
	Value  float64
	Exists bool
	TTL    time.Duration
	Err    error
}

// ReadWindows 一次管道读取一批窗口键（一次网络往返），返回按键索引的取值。
//
// 语义与逐个读一致：缺键是 Exists=false 而不是错误，单键错误记在该键上由调用方按原有 fail-open
// 口径处理。Redis 不可用时返回空表，调用方各自的「Redis 不可用」分支照旧生效。
func (w *CostWindows) ReadWindows(ctx context.Context, reads []CacheRead) map[string]WindowValue {
	values := make(map[string]WindowValue, len(reads))
	if len(reads) == 0 || !w.Ready() {
		return values
	}

	pipe := w.client.Raw().Pipeline()
	gets := make([]*redis.StringCmd, len(reads))
	evals := make([]*redis.Cmd, len(reads))
	exists := make([]*redis.IntCmd, len(reads))
	ttls := make([]*redis.DurationCmd, len(reads))
	for index, read := range reads {
		if read.Rolling != nil {
			source, err := w.scriptSource(read.Rolling.ScriptConst)
			if err != nil {
				values[read.Key] = WindowValue{Err: err}
				continue
			}
			// 管道里发脚本正文：管道内命令只在 Exec 时统一执行，此时 NOSCRIPT 已无重试机会（与 queueRolling 同理）。
			evals[index] = pipe.Eval(ctx, source, []string{read.Key}, read.Rolling.Now.UnixMilli(), read.Rolling.WindowMillis)
			// 值为 0 时要判键是否存在（见 RollingCost 的注释）：同批取回，免得再要一次往返。
			exists[index] = pipe.Exists(ctx, read.Key)
		} else {
			gets[index] = pipe.Get(ctx, read.Key)
		}
		if read.KeepTTL {
			ttls[index] = pipe.TTL(ctx, read.Key)
		}
	}
	// Exec 只回第一个错误，而缺键（redis.Nil）本就常见；单键成败按各自的 Cmd 判定。
	_, _ = pipe.Exec(ctx)

	for index, read := range reads {
		if _, settled := values[read.Key]; settled {
			continue
		}
		value := WindowValue{}
		if read.Rolling == nil {
			raw, err := gets[index].Result()
			switch {
			case errors.Is(err, redis.Nil):
			case err != nil:
				value.Err = err
			default:
				value.Value, value.Exists = ParseCostText(raw), true
			}
		} else {
			raw, err := evals[index].Result()
			switch {
			case err != nil:
				value.Err = err
			default:
				value.Value = ParseCostText(raw)
				value.Exists = value.Value != 0
				if !value.Exists {
					count, existsErr := exists[index].Result()
					if existsErr != nil {
						value.Err = existsErr
					} else {
						value.Exists = count > 0
					}
				}
			}
		}
		if read.KeepTTL && value.Err == nil && ttls[index] != nil {
			if ttl, err := ttls[index].Result(); err == nil {
				value.TTL = ttl
			}
		}
		values[read.Key] = value
	}
	return values
}

// RollingCacheRead 给出滚动窗口的批量读入参（周期非法是调用方缺陷，不静默算成零值）。
func RollingCacheRead(e Entity, id int64, period Period, now time.Time) (CacheRead, error) {
	switch period {
	case Period5h:
		return CacheRead{
			Key:     Cost5hKey(e, id, ResetRolling),
			Rolling: &RollingRead{ScriptConst: "GET_COST_5H_ROLLING_WINDOW", WindowMillis: window5hMillis, Now: now},
		}, nil
	case PeriodDaily:
		return CacheRead{
			Key:     CostDailyRollingKey(e, id),
			Rolling: &RollingRead{ScriptConst: "GET_COST_DAILY_ROLLING_WINDOW", WindowMillis: window24hMillis, Now: now},
		}, nil
	default:
		return CacheRead{}, fmt.Errorf("limit: 不支持的滚动周期 %q", period)
	}
}

// RollingCost 读滚动窗口累计值（5h 或 daily），并报告键是否存在。
func (w *CostWindows) RollingCost(ctx context.Context, e Entity, id int64, period Period, now time.Time) (float64, bool, error) {
	// 参数先判：周期非法是调用方缺陷，不该被「Redis 不可用」掩盖成零值。
	read, err := RollingCacheRead(e, id, period, now)
	if err != nil {
		return 0, false, err
	}
	if !w.Ready() {
		return 0, false, nil
	}
	value := w.ReadWindows(ctx, []CacheRead{read})[read.Key]
	return value.Value, value.Exists, value.Err
}

// FixedCost 读固定窗口累计值（daily fixed / weekly / monthly / 5h fixed 的键形制）。
func (w *CostWindows) FixedCost(ctx context.Context, key string) (float64, bool, error) {
	if !w.Ready() {
		return 0, false, nil
	}
	value := w.ReadWindows(ctx, []CacheRead{{Key: key}})[key]
	return value.Value, value.Exists, value.Err
}

// Fixed5hWindowState 读 5h 固定窗口：值 + 由 TTL 反推的重置时刻。
func (w *CostWindows) Fixed5hWindowState(ctx context.Context, e Entity, id int64, now time.Time) (Fixed5hState, error) {
	if !w.Ready() {
		return Fixed5hState{}, nil
	}
	read := CacheRead{Key: Cost5hKey(e, id, ResetFixed), KeepTTL: true}
	value := w.ReadWindows(ctx, []CacheRead{read})[read.Key]
	if value.Err != nil {
		return Fixed5hState{}, value.Err
	}
	return Fixed5hState{Current: value.Value, Exists: value.Exists, ResetAt: resetAtFromTTL(now, value.TTL)}, nil
}

// resetAtFromTTL 由剩余 TTL 反推重置时刻；键不存在或没有 TTL 时为空。
func resetAtFromTTL(now time.Time, ttl time.Duration) *time.Time {
	if ttl <= 0 {
		return nil
	}
	resetAt := now.Add(ttl.Truncate(time.Second))
	return &resetAt
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
	source, err := w.scriptSource("TRACK_COST_ROLLING_WINDOW")
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

// scriptSource 取脚本正文（管道内只发正文：见 queueRolling）。
func (w *CostWindows) scriptSource(constName string) (string, error) {
	if w.client == nil || w.client.Scripts() == nil {
		return "", errors.New("limit: 脚本注册表缺失")
	}
	script, ok := w.client.Scripts().LookupConst(constName)
	if !ok {
		return "", fmt.Errorf("limit: 未登记的脚本 %s", constName)
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
