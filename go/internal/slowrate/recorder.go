package slowrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/slowlog"
)

// Facts 是一次终态的速率事实，慢样本判定的全部输入。
//
// 为什么不是终态层的 Settlement：本包不得依赖 internal/terminal（terminal 的挂点通过
// terminal 包内定义的中性接口回调进来），故事实用本包自己的结构承载。
type Facts struct {
	// ProviderID 是实际作答的供应商（行级 provider_id 口径）。
	ProviderID int64
	// SessionID 与 KeyID 供会话冷却键使用；SessionID 为空表示本次请求没有会话身份，
	// 此时只做渠道级统计、不写会话冷却。
	SessionID string
	KeyID     int64
	// ModelKey 是跨供应商别名归一后的模型键（口径见 pubstatus.ResolveSuccessRateModelKey）。
	ModelKey string
	// RequestID 是 message_request 行 id，用作滑窗 ZSET 的成员（同一请求只入窗一次）。
	RequestID int64
	// StatusCode 是终态状态码。
	StatusCode int
	// OutputTokens / DurationMS / FirstByteMS 是速率分子与分母的原料；缺一即不判定。
	//
	// DurationMS / FirstByteMS 用 *int：与 terminal.Settlement 的同名列逐字同型，
	// 免去两侧转换（pubstatus.ComputeTokensPerSecond 要 *float64，在下面一处折算）。
	OutputTokens *int64
	DurationMS   *int
	FirstByteMS  *int
}

// Params 是一个渠道生效的低速监控参数（渠道覆写优先，缺省取 DefaultParams）。
type Params struct {
	// WindowMinutes 是**判定滑窗**的分钟数（列 slow_rate_window_minutes）。
	//
	// 为何是分钟：用户 2026-09-22 裁决该单位由秒改分钟——运维调参时说的都是「30 分钟」
	// 而不是「1800 秒」。旧列 slow_rate_window_seconds 已废弃（见 0134 迁移）。
	WindowMinutes int
	TriggerCount  int
	// Ratio 是低速系数（列 slow_rate_ratio，numeric(5,4)），**0-1 小数**。
	//
	// 为何是小数：旧列 slow_rate_ratio_per_mille 存千分比整数（300 = 0.3），
	// 用户 2026-09-22 裁决改为 0-1 小数（默认 0.3）——与设计稿 §3 的写法一致。
	Ratio           float64
	PenaltyStep     int
	PenaltyMax      int
	CooldownSeconds int
	// RecoveryRequests 是恢复阈值（列 slow_rate_recovery_requests，默认 10）：连续这么多个
	// **可判定样本**都不慢，即重置该组合的降权。用户的说法是「最近 N 个请求都没出现低速」，
	// 但计入的只有能算出速率的样本——见 recordClean 的判据说明。
	RecoveryRequests int
}

// DefaultParams 是六个渠道参数与冷却期的出厂值（与 providers 表 slow_rate_* 列的语义一致）。
//
// TriggerCount 对应列 slow_rate_trigger_count，语义是**触发阈值**（设计稿 §5 状态表：
// 「窗内低速次数 ≥ 阈值（默认 3）」与 §6 公式 `floor(slowCount / thresholdCount)`）。
// 它与 slow_rate_min_samples（**基线样本下限**，默认 100，只由 B3 基线定时任务读，
// 决定能否发布基线）是两件事，故拆作两列；本包只读前者。
//
// WindowMinutes 是**判定滑窗**（默认 30 分钟），与基线主窗（slow_rate_baseline_window_days，
// 默认 3 天）不同尺度。用户 2026-09-21 定的取值：判定窗 30 分钟、系数 0.3。
func DefaultParams() Params {
	return Params{
		WindowMinutes: 30,
		// 触发阈值：窗内低速达到 3 条即进一档（设计稿 §5 / §6）。
		TriggerCount: 3,
		// 系数 0.3：速率低于基线的 30% 即算低速（0-1 小数）。
		Ratio:       0.3,
		PenaltyStep: 10,
		PenaltyMax:  30,
		// 恢复阈值默认 10（用户 2026-09-22 指定）：连续 10 个可判定样本都不慢即重置降权。
		RecoveryRequests: 10,
		// 冷却期不在 providers 表里（表内没有它），故取常量。
		CooldownSeconds: 60,
	}
}

// normalize 把零值与越界值收敛到可用区间：渠道列可空，NULL 即取出厂默认。
func (p Params) normalize() Params {
	def := DefaultParams()
	if p.WindowMinutes <= 0 {
		p.WindowMinutes = def.WindowMinutes
	}
	if p.TriggerCount <= 0 {
		p.TriggerCount = def.TriggerCount
	}
	// 系数的闸门写成**正向合取**而不是 `<= 0`：小数域里 0 是合法下界、1 是合法上界，
	// 而 NaN 与任何值比较都为假——用 `<= 0` 会漏掉 NaN（它既非 >0 也非 <=0 的比较结果），
	// 让 NaN 一路乘进低速线，使判定静默恒假（永不标慢）。正向写法把 NaN 与越界一并收敛。
	if !(p.Ratio > 0 && p.Ratio <= 1) {
		p.Ratio = def.Ratio
	}
	if p.PenaltyStep <= 0 {
		p.PenaltyStep = def.PenaltyStep
	}
	if p.PenaltyMax <= 0 {
		p.PenaltyMax = def.PenaltyMax
	}
	if p.CooldownSeconds <= 0 {
		p.CooldownSeconds = def.CooldownSeconds
	}
	if p.RecoveryRequests <= 0 {
		p.RecoveryRequests = def.RecoveryRequests
	}
	return p
}

// ConfigSource 给出某渠道的低速监控生效参数。
//
// 第二个返回值为 false 表示该渠道未开启监控。**这是零开销的闸门**：未开启的渠道在
// 本包内一次 Redis 读写都不会发生（见 Recorder.Record 的首段）。
type ConfigSource interface {
	SlowRateConfig(ctx context.Context, providerID int64) (Params, bool)
}

// Recorder 把终态速率事实折算成慢样本，并维护滑窗、状态与会话冷却键。
//
// 无状态：全部可变状态都在 Redis 与注入的 ConfigSource 里，故可并发使用。
type Recorder struct {
	redis  redis.UniversalClient
	config ConfigSource
	logger Logger
	// now 可注入：滑窗按时刻清理，测试必须能拨钟。
	now func() time.Time
}

// Logger 是本包记 warn 的最小日志面（`logx.Logger` 满足）。
//
// 慢样本是纯旁路：任何失败都不得冒泡成结算错误，只能落日志。
type Logger interface {
	Warn(event string, fields map[string]any)
}

// Options 是 Recorder 的构造参数。
type Options struct {
	// Redis 必填；nil 时 New 返回 nil（旁路整段跳过，与「未开启监控」同判）。
	Redis redis.UniversalClient
	// Config 必填；nil 时 New 返回 nil。
	Config ConfigSource
	Logger Logger
	Now    func() time.Time
}

// New 构造 Recorder；Redis 或 Config 缺失时返回 nil（调用方据此整段跳过）。
func New(options Options) *Recorder {
	if options.Redis == nil || options.Config == nil {
		return nil
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Recorder{redis: options.Redis, config: options.Config, logger: options.Logger, now: now}
}

// minOutputTokens 是单样本的输出 token 下限。
//
// 它与 providers 表的 slow_rate_trigger_count（窗内低速样本数阈值）**不是同一件事**：
// 后者决定「窗口样本够不够判定」，本常量决定「这一条样本的速率有没有意义」。
// 短输出的速率由帧解析与网络往返主导，与生成能力无关（1-token 工具调用没有速率可言），
// 故照设计稿 §2 取常量 50，不做渠道覆写。
const minOutputTokens = 50

// maxFirstByteRatio 是样本清洗的硬前置：first_byte/duration 超过它即剔除。
//
// 依据（设计稿 §2 的实证）：`r >= 500` 的样本里 92%~97% 满足该条件，中位生成窗口仅
// 89~128ms——那是非流式整包一次性到达，首字节≈末字节，分母被压到近乎 0、速率爆炸成假值。
const maxFirstByteRatio = 0.9

// Record 处理一次终态事实：写慢样本滑窗、更新状态、必要时写会话冷却键。
//
// 失败一律只记日志：本包是旁路，不得影响结算结果，也没有可做的补救。
func (r *Recorder) Record(ctx context.Context, facts Facts) {
	if r == nil || facts.ProviderID <= 0 || facts.ModelKey == "" || facts.RequestID <= 0 {
		return
	}
	// 零开销闸门：未开启监控的渠道就此返回，不做任何 Redis 读写。
	params, enabled := r.config.SlowRateConfig(ctx, facts.ProviderID)
	if !enabled {
		return
	}
	params = params.normalize()

	// 基线由定时任务产出（B3）；读不到即 fail-open：不采样、不判定、不写冷却。
	baseline, ok := r.baseline(ctx, facts.ProviderID, facts.ModelKey)
	if !ok {
		return
	}
	rate, ok := generationRate(facts)
	if !ok {
		return
	}
	windowKey := samplesKey(facts.ProviderID, facts.ModelKey)
	stateKey := stateKey(facts.ProviderID, facts.ModelKey)
	streakKey := cleanStreakKey(facts.ProviderID, facts.ModelKey)
	at := r.now().UnixMilli()
	windowMS := int64(params.WindowMinutes) * 60 * 1000
	ttl := time.Duration(params.WindowMinutes*2) * time.Minute

	// 慢样本写入与状态推进分开：前者无条件（只要判据成立），后者需要看窗内计数。
	//
	// 能走到这里的样本一律是「可判定样本」（status 200、输出够长、首字比例达标、有基线），
	// 故非慢的这一支就是恢复策略要数的「干净样本」。
	if !isSlow(rate, baseline, params) {
		r.recordClean(ctx, facts, params, windowKey, stateKey, streakKey, ttl)
		return
	}

	pipe := r.redis.Pipeline()
	pipe.ZAdd(ctx, windowKey, redis.Z{Score: float64(at), Member: strconv.FormatInt(facts.RequestID, 10)})
	pipe.ZRemRangeByScore(ctx, windowKey, "0", strconv.FormatInt(at-windowMS, 10))
	pipe.Expire(ctx, windowKey, ttl)
	zcard := pipe.ZCard(ctx, windowKey)
	// 慢样本打断连续干净计数：删掉计数键本身（而不是删 state 里的某个字段）。
	//
	// 挂进同一次 pipeline 是为了零新增往返——它必须与慢样本同生共死，否则「慢样本写入」
	// 与「计数清零」之间会留下一个可被并发读看到的窗口。
	//
	// 为何是 DEL 而非 HDEL：连续计数的语义是「**连续**这么多个样本都不慢」。若只清一个
	// 字段而保留计数键，「2 干净 + 1 慢 + 2 干净」在 N=3 时会凑够 3 而误重置——计数必须归零。
	//
	// 计数刻意放在独立键而不是 state Hash：state 只在该组合**真的慢过**时才存在，
	// 而干净计数在每次可判定样本上都会长一截——写进 state 会让每个有流量的已开启组合
	// 凭空多出一个 state 键，而 adminapi 的 /providers/health 会 SCAN 全部 state 键。
	pipe.Del(ctx, streakKey)
	if _, err := pipe.Exec(ctx); err != nil {
		r.warn("slowrate.sample_write_failed", facts, err)
		return
	}

	count := int(zcard.Val())
	// 判定门槛（设计稿 §5 边界：窗内计数不足阈值即**不推进状态**，fail-open）。
	//
	// 注意这**不是**「惩罚不衰减」：衰减由读侧当场派生实现（它数的是滑窗里的活成员，
	// 见 route.deriveSlowRatePenalty），这里的提前返回只意味着「不把标记推得更重」。
	if count < params.TriggerCount {
		return
	}
	// 状态推进：窗内低速条数即 slowCount，惩罚按档位增长并封顶（设计稿 §6）。
	//
	// 同时写生效参数：读侧要据「滑窗内慢样本数」派生惩罚，而 admin 的合成 Provider
	// 没有参数列，只能从这份 Hash 里取（见 route.SlowRateStateFieldWindowSeconds 一族）。
	// 分档与封顶走 route.DeriveSlowRatePenalty——**与读侧同一个函数**。
	//
	// 为何共用：读侧在选路时按活窗计数当场重算（惩罚不能只涨不落），写侧算完落 state Hash
	//（供管理与快照回退读）。两边各写一道时，只要分档或封顶差一点就分叉，而且静默。
	// slowrate 可 import route（反向成环），故本包是能放下这个共用函数的唯一一侧。
	penalty := route.DeriveSlowRatePenalty(count, params.TriggerCount, params.PenaltyStep, params.PenaltyMax)
	statePipe := r.redis.Pipeline()
	// HGet 排在 HSet **之前**：pipeline 按序执行，故读到的是本轮的**旧**惩罚值，
	// 供低速日志去重（只在档位真的变了时记一条）。加在这一条既有 pipeline 里
	// 意味着日志不新增任何往返；见 internal/slowlog.RecordPenaltyChange 的去重理由。
	previousPenalty := statePipe.HGet(ctx, stateKey, StateFieldPenalty)
	statePipe.HSet(ctx, stateKey,
		StateFieldPenalty, penalty,
		StateFieldWindowMinutes, params.WindowMinutes,
		StateFieldTriggerCount, params.TriggerCount,
		StateFieldPenaltyStep, params.PenaltyStep,
		StateFieldPenaltyMax, params.PenaltyMax,
		"slowCount", count,
		"enteredAt", at,
	)
	statePipe.Expire(ctx, stateKey, ttl)
	if _, err := statePipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		r.warn("slowrate.state_write_failed", facts, err)
		return
	}
	// 惩罚档位变化记一条低速日志（旁路：失败只 warn，见 slowlog 包注释）。
	// 与 state 同 pipeline 取旧值，故本调用不新增 Redis 往返。
	slowlog.RecordPenaltyChange(
		ctx, r.redis, r.logger, facts.ProviderID, facts.ModelKey, previousPenalty.Val(), penalty,
	)
	r.writeCooldown(ctx, facts, params)
}

// recordClean 累计「连续干净样本」，达到恢复阈值即重置该组合的降权。
//
// 恢复策略（用户 2026-09-22 需求：「最近 N 个请求都没有出现低速请求了，也就直接重置低速加的
// 优先级」）。粒度是**渠道 × 模型**：惩罚本就按 scope 存（键含 modelKey），而写侧一次终态
// 只知道一个 modelKey，故这是唯一可实现的粒度（整个渠道一起重置需要 SCAN 该渠道所有 scope，
// 在终态异步写路径上不可接受）。
//
// 只把**可判定样本**计入（本函数只从 isSlow 那一行之后进入）：短输出（<minOutputTokens）、
// 非 200、整包到达（首字比例超限）这些请求没有任何速率信息，若也计入「干净」，一个正在劣化
// 但恰好只服务短请求的渠道会被误判为已恢复、惩罚被清掉——与「宁可少标不可误标」的口径相反。
// 基线缺失时调用方已提前返回（fail-open），而读侧本来就不对无基线的组合施惩罚，故那批请求
// 计不计入都无行为差异。
//
// 读侧零改动：惩罚是纯读时派生（读侧数的是 samples 滑窗里的活成员，state 的 penalty 字段
// 只在「生效参数缺失」时作旧数据回退），故删掉 samples 与 state 后，下一次选路当场得到
// 零惩罚，/providers/health 的投影也随之消失。
func (r *Recorder) recordClean(
	ctx context.Context,
	facts Facts,
	params Params,
	windowKey string,
	stateKey string,
	streakKey string,
	ttl time.Duration,
) {
	pipe := r.redis.Pipeline()
	streak := pipe.Incr(ctx, streakKey)
	// TTL 与滑窗同寿命：干净计数是「最近一段时间的连续干净」，不是一个永久累计值。
	// 零流量时计数不增长、惩罚因此不被清除，但惩罚会随滑窗自然归零（样本滑出窗后读侧的
	// 活窗计数低于阈值），最迟一个窗长——这是既有行为，不额外处理。
	pipe.Expire(ctx, streakKey, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		r.warn("slowrate.streak_write_failed", facts, err)
		return
	}
	if int(streak.Val()) < params.RecoveryRequests {
		return
	}
	// 达阈值：三个键原子地一起删（见下），并记一条「降权解除」事件。
	//
	// 为何必须原子：先前实现是「INCR 一条 pipeline，DEL 另一条 pipeline」，两条之间有竞态
	// 窗口——若并发请求恰好在此期间写入一条慢样本，那条样本（连同它推进的惩罚）会被删除，
	// 渠道被**错误恢复**，直到下一条慢样本重建状态。
	//
	// 修法：WATCH 三个键 + 单次 MULTI。慢样本写会同时改这三个键（ZADD window / DEL streak /
	// HSet state），故只要期间有慢样本落盘，EXEC 必失败（TxFailedErr）——此时**放弃删除**
	// 就是正确行为：让下一个干净样本重新累计。
	//
	// 为何不引入 Lua：本修法零新文件、零新部署面（Lua 要双副本 + MANIFEST + 黄金样本对拍），
	// 而 WATCH/MULTI 是客户端标准能力，且这段只在「连续 N 个干净样本」后走一次（非热路径）。
	if err := r.resetAfterRecovery(ctx, facts, windowKey, stateKey, streakKey); err != nil {
		r.warn("slowrate.recovery_reset_failed", facts, err)
	}
}

// resetAfterRecovery 在事务保护下删除三个键，并把解除的降权量记入低速日志。
//
// 返回 redis.TxFailedErr 表示期间有并发写入（慢样本）——调用方按 warn 记，但那是**预期**
// 的放弃而不是故障；两种情形都不影响结算。
func (r *Recorder) resetAfterRecovery(
	ctx context.Context,
	facts Facts,
	windowKey string,
	stateKey string,
	streakKey string,
) error {
	err := r.redis.Watch(ctx, func(tx *redis.Tx) error {
		// 旧惩罚值在事务**之外**读、WATCH **之内**取：WATCH 保证读到之后再无并发写，
		// 故这个值就是本次删除真正解除掉的那个。
		previousRaw := tx.HGet(ctx, stateKey, StateFieldPenalty).Val()
		if _, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, windowKey, stateKey, streakKey)
			return nil
		}); err != nil {
			return err
		}
		// 只在事务真提交之后记日志：与慢样本的 RecordPenaltyChange 同一纪律
		//（先落库、后记事件，记失败不回滚）。
		slowlog.RecordRecoveryReset(ctx, r.redis, r.logger, facts.ProviderID, facts.ModelKey, previousRaw)
		return nil
	}, windowKey, stateKey, streakKey)
	if errors.Is(err, redis.TxFailedErr) {
		// 期间有慢样本落盘 ⇒ 放弃本次重置。不计故障：下一个干净样本会重新累计。
		return nil
	}
	return err
}

// writeCooldown 写会话×供应商冷却键：冷却期内该会话的选路会跳过这家渠道（读侧在 B4）。
//
// 键形制复用 session.ProviderCooldownKey，不另立——两处拼同一个键，拼错即静默失效。
// 写侧不走 Binder.Clear：那是「带 CAS 清理绑定 + 顺带写冷却」的组合动作，而本处不打算清绑定
// （清绑定会让会话重新走初选，正是要避免的）；直接 SETEX 只写冷却键，语义最小。
func (r *Recorder) writeCooldown(ctx context.Context, facts Facts, params Params) {
	if facts.SessionID == "" {
		return
	}
	key := session.ProviderCooldownKey(facts.SessionID, facts.KeyID, facts.ProviderID)
	if err := r.redis.Set(ctx, key, "slow", time.Duration(params.CooldownSeconds)*time.Second).Err(); err != nil {
		r.warn("slowrate.cooldown_write_failed", facts, err)
	}
}

// baseline 读该组合的历史基线（由 B3 的定时任务产出）。
//
// 读不到（键不存在 / 反序列化失败 / Redis 报错）一律返回 false：调用方据此 fail-open，
// 不写样本也不判定——宁可少标不可误标。
func (r *Recorder) baseline(ctx context.Context, providerID int64, modelKey string) (float64, bool) {
	raw, err := r.redis.Get(ctx, baselineKey(providerID, modelKey)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			r.warn("slowrate.baseline_read_failed", Facts{ProviderID: providerID, ModelKey: modelKey}, err)
		}
		return 0, false
	}
	var payload struct {
		Median float64 `json:"median"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		r.warn("slowrate.baseline_undecodable", Facts{ProviderID: providerID, ModelKey: modelKey}, err)
		return 0, false
	}
	if payload.Median <= 0 {
		return 0, false
	}
	return payload.Median, true
}

// generationRate 算生成阶段速率（tok/s），逐字复用 public-status 的既有口径。
func generationRate(facts Facts) (float64, bool) {
	if facts.StatusCode != 200 {
		return 0, false
	}
	if facts.OutputTokens == nil || *facts.OutputTokens < minOutputTokens {
		return 0, false
	}
	if facts.DurationMS == nil || *facts.DurationMS <= 0 || facts.FirstByteMS == nil {
		return 0, false
	}
	if *facts.DurationMS <= *facts.FirstByteMS {
		return 0, false
	}
	// 清洗：整包到达的伪高值（首字节≈末字节）。
	ratio := float64(*facts.FirstByteMS) / float64(*facts.DurationMS)
	if ratio > maxFirstByteRatio {
		return 0, false
	}
	duration := float64(*facts.DurationMS)
	firstByte := float64(*facts.FirstByteMS)
	rate := pubstatus.ComputeTokensPerSecond(facts.OutputTokens, &duration, &firstByte)
	if rate == nil {
		return 0, false
	}
	return *rate, true
}

// isSlow 判定速率是否低于低速线（基线 × 系数）。
//
// 系数是 0-1 小数（见 Params.Ratio），故直接相乘、不除 1000。**它与 jobs 包算低速线的那处
// 各写一遍，改单位时必须同时改**（两边都是 `基线 × 系数`，只改一处会让低速线差 1000 倍且静默）。
func isSlow(rate, baseline float64, params Params) bool {
	return rate < baseline*params.Ratio
}

func (r *Recorder) warnFields(event string, fields map[string]any, err error) {
	if r.logger == nil {
		return
	}
	fields["error"] = err.Error()
	r.logger.Warn(event, fields)
}

func (r *Recorder) warn(event string, facts Facts, err error) {
	if r.logger == nil {
		return
	}
	r.logger.Warn(event, map[string]any{
		"providerId": facts.ProviderID,
		"modelKey":   facts.ModelKey,
		"requestId":  facts.RequestID,
		"error":      err.Error(),
	})
}

// 键形制：cch:slow:{<providerID>:<modelKey>}:<suffix>。
//
// 花括号是 Redis Cluster 的 hash tag：同一组合的多个键落同一槽，pipeline 才能压成一次往返。
func scopeTag(providerID int64, modelKey string) string {
	return fmt.Sprintf("{%d:%s}", providerID, modelKey)
}

// 状态 Hash 的字段名。读侧（internal/route）因 import 环不能引用本包，只能各写一遍字面量，
// 由 route_test 的镜像钉子（slowrate_keys_mirror_test.go）逐字比对——改这里必改那里。
//
// 后四项是**生效参数**（已应用渠道覆写），读侧据它们把滑窗计数折成惩罚。
//
// 窗长字段是**分钟**（与 Params.WindowMinutes 同单位）：它是随状态一起落的瞬时缓存，
// 状态键 TTL 只有 2 倍窗长，故改名不需要迁移；旧字段窗口期内的状态键会被读侧判成
// 「参数缺失」而回退读快照（fail-open），上限一个 TTL。
const (
	StateFieldPenalty       = "penalty"
	StateFieldWindowMinutes = "windowMinutes"
	StateFieldTriggerCount  = "triggerCount"
	StateFieldPenaltyStep   = "penaltyStep"
	StateFieldPenaltyMax    = "penaltyMax"
)

func samplesKey(providerID int64, modelKey string) string {
	return "cch:slow:" + scopeTag(providerID, modelKey) + ":samples"
}

// SamplesKey 是该组合的慢样本滑窗键（ZSET，成员为请求 id，分数为写入毫秒，TTL 为 2 倍窗长）。
//
// 它是读侧派生惩罚的唯一真源，故必须跨包同形。导出是为了让镜像钉子有**真实对照物**
// （同 jobs.BaselineKey 的用法）：读侧因 import 环只能自拼，错一个字符就静默失效。
func SamplesKey(providerID int64, modelKey string) string {
	return samplesKey(providerID, modelKey)
}

func stateKey(providerID int64, modelKey string) string {
	return "cch:slow:" + scopeTag(providerID, modelKey) + ":state"
}

// cleanStreakKey 是该组合的「连续干净样本」计数键（STRING，INCR 累加，TTL 同滑窗）。
func cleanStreakKey(providerID int64, modelKey string) string {
	return "cch:slow:" + scopeTag(providerID, modelKey) + ":streak"
}

func baselineKey(providerID int64, modelKey string) string {
	return "cch:slow:" + scopeTag(providerID, modelKey) + ":baseline"
}
