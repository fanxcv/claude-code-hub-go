package route

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/redis/go-redis/v9"
)

// 本文件是低速降级在**选路侧**的两个只读面：渠道惩罚（penalty）与「本会话是否在冷却中」。
//
// 写侧在 B2（`internal/slowrate` 的 Recorder），基线产出在 B3（`internal/jobs`）；本文件只读，
// 不写任何键，故不改变那两处的地盘。
//
// 为什么键形制是**照抄**而不是 import：`slowrate` 依赖 `session`（写冷却键），`session` 依赖
// `guard`，`guard` 依赖 `route` —— 本包反向 import `slowrate`（或 `session`）立即成环。
// 于是同一段键形制必然在仓内写两遍，**唯一的防线是测试**：
// `slowrate_keys_mirror_test.go`（external test package）逐字节比对两侧的键，
// 任一侧改了而另一侧没跟上都会红。仓内 affinity 键亦有同类镜像，见 `affinity.go:22` 的说明。

// 低速键的形制：`cch:slow:{<providerID>:<modelKey>}:<suffix>`
// （镜像 `internal/slowrate/recorder.go:314-327` 的 scopeTag/samplesKey/stateKey/baselineKey）。
//
// 花括号是 Redis Cluster 的 hash tag：同一组合的多个键落同一槽，pipeline 才能压成一次往返。
const (
	slowRateKeyPrefix      = "cch:slow:"
	slowRateSamplesSuffix  = ":samples"
	slowRateStateSuffix    = ":state"
	slowRateBaselineSuffix = ":baseline"

	// 状态 Hash 的字段名：与写侧（internal/slowrate）逐字节一致。本包因 import 环不能引用
	// 那边的常量，两侧各写一遍字面量，由外部测试包的镜像钉子比对（slowrate_keys_mirror_test.go）。
	//
	// 后四项（窗长/阈值/步长/上限）是写侧随状态一起落的**生效参数**。读侧据「滑窗内慢样本数」
	// 当场派生惩罚需要它们，而 admin 的 providers_health 构造的是合成 Provider（只有 id 与开关，
	// 无参数列），取不到，只能从 Hash 带出。
	SlowRateStateFieldPenalty       = "penalty"
	SlowRateStateFieldWindowSeconds = "windowSeconds"
	SlowRateStateFieldTriggerCount  = "triggerCount"
	SlowRateStateFieldPenaltyStep   = "penaltyStep"
	SlowRateStateFieldPenaltyMax    = "penaltyMax"

	slowRateBaselineFieldName  = "source"
	slowRateCooldownKeyPattern = "session-binding:v1:{%s}:provider:%s:cooldown"
)

// 基线来源取值：与写侧 internal/jobs 的 BaselineSource 逐字一致（本包因 import 环不能引用
// 那边的常量，两侧各写字面量，由 mirror 钉子比对）。
//
// 只有前两者算「可用基线」：primary 是主窗 W1 达标（A0），extended 是回退扩展窗（A2）。
// extended_stale 是 A4（刚从故障/下线恢复，基线取自陈旧窗口）——设计稿明定它只做会话级降级、
// 不做渠道级惩罚，故不在可用之列。
const (
	slowRateSourcePrimary       = "primary"
	slowRateSourceExtended      = "extended"
	slowRateSourceExtendedStale = "extended_stale"
)

// SlowRateModelKey 归一请求模型为「渠道 x 模型」组合键里的模型分量。
//
// 逐字复用 `pubstatus.ResolveSuccessRateModelKey`（写侧 B2 的 dataplane 旁路与基线任务 B3
// 用的是同一个函数）：三处若各自拼一遍，同一模型的不同别名就会各算一套样本与基线，
// 而界面上看不出「基线为何是空的」。
func SlowRateModelKey(requestModel string) string {
	model := requestModel
	return pubstatus.ResolveSuccessRateModelKey(&model, nil)
}

// SlowRateStateKey 是该组合的状态键（Hash，含 slowCount/penalty/enteredAt）。
func SlowRateStateKey(providerID int64, modelKey string) string {
	return slowRateKeyPrefix + slowRateScopeTag(providerID, modelKey) + slowRateStateSuffix
}

// SlowRateBaselineKey 是该组合的历史基线键（String JSON，B3 产出）。
func SlowRateBaselineKey(providerID int64, modelKey string) string {
	return slowRateKeyPrefix + slowRateScopeTag(providerID, modelKey) + slowRateBaselineSuffix
}

// SlowRateSamplesKey 是该组合的慢样本滑窗键（ZSET，成员为请求 id，分数为写入毫秒）。
//
// 它是**选路惩罚的唯一真源**：惩罚由窗内成员数当场派生，而不是读状态里的快照
// （快照只在写侧推进时刷新，渠道恢复后不再刷新，见 deriveSlowRatePenalty）。
func SlowRateSamplesKey(providerID int64, modelKey string) string {
	return slowRateKeyPrefix + slowRateScopeTag(providerID, modelKey) + slowRateSamplesSuffix
}

func slowRateScopeTag(providerID int64, modelKey string) string {
	return "{" + strconv.FormatInt(providerID, 10) + ":" + modelKey + "}"
}

// SlowRateCooldownKey 是「会话 x 供应商」冷却键，镜像 `session.ProviderCooldownKey`
// （`internal/session/keys.go:52`）。
//
// 必须与写侧**逐字节相同**：写侧在 `internal/slowrate` 里直接调 `session.ProviderCooldownKey`，
// 本侧拼错一个字符就静默失效（表现是「冷却写了但永远不生效」，没有任何报错）。测试钉住它。
func SlowRateCooldownKey(sessionID string, keyID, providerID int64) string {
	// sha256 的输入顺序与编码镜像 `session/keys.go:17-20` 的 bindingHashTag：
	// keyID 十进制 + "\x00" + sessionID，取十六进制全量（不是截断）。
	sum := sha256.Sum256([]byte(strconv.FormatInt(keyID, 10) + "\x00" + sessionID))
	tag := hex.EncodeToString(sum[:])
	return "session-binding:v1:{" + tag + "}:provider:" + strconv.FormatInt(providerID, 10) + ":cooldown"
}

// SlowRateReader 是低速降级的选路侧只读面。
//
// 与 `HealthReader` 同构（同为「Redis 读侧 + fail-open」），装配点见 Options.SlowRate。
type SlowRateReader struct {
	redis  redis.UniversalClient
	logger *logx.Logger
	// now 可注入时钟：活窗计数要拿「当前时刻」当区间上界，测试必须能拨钟。
	now func() time.Time
}

// SlowRateOptions 是 SlowRateReader 的构造参数（同 HealthOptions 的形制）。
type SlowRateOptions struct {
	// Redis 为 nil 时返回 nil 读取器，调用方据此整段跳过（等同未装配）。
	Redis redis.UniversalClient
	// Logger 可空：只影响读失败时的那条 warn。
	Logger *logx.Logger
	// Now 可注入时钟。
	Now func() time.Time
}

// NewSlowRateReader 构造读取器；Redis 为 nil 时返回 nil，调用方据此整段跳过（等同未装配）。
func NewSlowRateReader(opts SlowRateOptions) *SlowRateReader {
	if opts.Redis == nil {
		return nil
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &SlowRateReader{redis: opts.Redis, logger: opts.Logger, now: now}
}

// Penalties 批量读候选渠道的渠道级惩罚。
//
// 返回「providerID -> penalty」，只含**惩罚为正**且**基线可用**的渠道：基线键缺失、读失败、
// 坏 JSON、extended_stale（A4）一律不收录（见 slowRateBaselineUsable）；状态读不到、惩罚非正
// 同样不收录——调用方按「无惩罚」继续选路（fail-open）。
//
// 为什么 fail-open 而不是 fail-closed：低速是**软降权**，不是硬故障。Redis 抖动时把候选
// 整批降权（或反过来全部排除）会让一次故障变成选路行为突变，而低速机制本身并不承担
// 「保证不选慢渠道」的职责。
//
// 零开销：`SlowRateMonitorEnabled` 为假的渠道不进 Redis——默认全关时本方法在开头的
// 长度判断处直接返回，选路热路径上一个往返都不产生。
//
// 两段往返，且**与候选数无关**：第一段读状态与基线（每候选两条命令），第二段只对「状态里带
// 生效参数」的候选发区间计数（ZCount）。分两段是因为区间下界依赖窗长，而窗长来自第一段的
// 状态结果——构造命令时还读不到。不用 Lua 合并：多个候选分属不同 hash tag（不同槽），
// 跨槽多键 Lua 不适用，硬合并只会退化成「每候选一次往返」。
//
// 为何是区间计数而不是取回全量成员：本方法**每次选路**都走，而成员数的上界是「一个 TTL
// （2 倍窗长）内的慢样本数」，不是常数；ZCount 只回一个整数，载荷 O(1)。
func (r *SlowRateReader) Penalties(
	ctx context.Context,
	candidates []Provider,
	requestModel string,
) map[int64]int {
	out := make(map[int64]int, len(candidates))
	if r == nil || r.redis == nil || len(candidates) == 0 {
		return out
	}
	modelKey := SlowRateModelKey(requestModel)
	if modelKey == "" {
		return out
	}
	enabled := make([]Provider, 0, len(candidates))
	for _, provider := range candidates {
		if provider.SlowRateMonitorEnabled {
			enabled = append(enabled, provider)
		}
	}
	if len(enabled) == 0 {
		return out
	}

	states := make([]*redis.SliceCmd, len(enabled))
	baselines := make([]*redis.StringCmd, len(enabled))
	// Pipelined 而非 Pipeline+Exec：两者都是单次往返，但闭包形式让「哪些命令属于这一批」
	// 在类型上不可分割，也让测试替身只需实现一个方法。
	_, err := r.redis.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for index, provider := range enabled {
			states[index] = pipe.HMGet(ctx, SlowRateStateKey(provider.ID, modelKey),
				SlowRateStateFieldPenalty,
				SlowRateStateFieldWindowSeconds,
				SlowRateStateFieldTriggerCount,
				SlowRateStateFieldPenaltyStep,
				SlowRateStateFieldPenaltyMax,
			)
			baselines[index] = pipe.Get(ctx, SlowRateBaselineKey(provider.ID, modelKey))
		}
		return nil
	})
	// 返回的 err 是批内第一个失败命令的错误（含 redis.Nil）——Nil 是「键不存在」的正常情形，
	// 不该记为故障。真正的连接/命令错误在这里记一条，但**不中止**：下面逐命令判定，
	// 失败的各自跳过，整体仍 fail-open。
	if err != nil && !errors.Is(err, redis.Nil) {
		r.warn("route.slow_rate_penalty_read_failed", modelKey, err)
	}

	decoded := make([]slowRateState, len(enabled))
	needsCount := make([]int, 0, len(enabled))
	for index := range enabled {
		decoded[index] = decodeSlowRateState(states[index])
		if decoded[index].hasParams {
			needsCount = append(needsCount, index)
		}
	}

	// 第二段：只对参数齐备的候选发区间计数（参数缺失的走快照回退，不需要计数）。
	liveCounts := make([]int, len(enabled))
	if len(needsCount) > 0 {
		nowMS := r.now().UnixMilli()
		counts := make([]*redis.IntCmd, len(needsCount))
		_, err := r.redis.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for offset, index := range needsCount {
				// 下界取闭区间，与「窗内」的既有定义（score >= now-窗长）逐字一致。
				lower := nowMS - int64(decoded[index].params.windowSeconds)*1000
				counts[offset] = pipe.ZCount(
					ctx,
					SlowRateSamplesKey(enabled[index].ID, modelKey),
					strconv.FormatInt(lower, 10),
					"+inf",
				)
			}
			return nil
		})
		if err != nil && !errors.Is(err, redis.Nil) {
			r.warn("route.slow_rate_penalty_read_failed", modelKey, err)
		}
		for offset, index := range needsCount {
			// 读失败与「窗内无慢样本」同判（计 0 ⇒ 无惩罚，fail-open）。
			if value, err := counts[offset].Result(); err == nil {
				liveCounts[index] = int(value)
			}
		}
	}

	for index := range enabled {
		state := decoded[index]
		penalty := state.penalty
		if state.hasParams {
			penalty = deriveSlowRatePenalty(liveCounts[index], state.params)
		}
		if penalty <= 0 {
			continue
		}
		// 只有**有效**基线才支撑渠道级惩罚：设计稿 §3 对 A3（无基线）明定「不生成 penalty」、
		// 「选路侧该渠道无 penalty」，对 A4（extended_stale）明定不做渠道级惩罚。
		if !slowRateBaselineUsable(baselines[index]) {
			continue
		}
		out[enabled[index].ID] = penalty
	}
	return out
}

// slowRatePenaltyParams 是派生惩罚所需的四个生效参数（写侧随状态一起落 Hash）。
type slowRatePenaltyParams struct {
	windowSeconds int
	triggerCount  int
	penaltyStep   int
	penaltyMax    int
}

// slowRateState 是读侧对一条状态 Hash 的解析结果。
type slowRateState struct {
	// penalty 是写侧留下的快照值，**仅在参数缺失**（旧版本写下的状态）时回退使用。
	penalty int
	// params 齐备时 hasParams 为真，惩罚改由滑窗计数当场派生。
	params    slowRatePenaltyParams
	hasParams bool
}

// decodeSlowRateState 解析 HMGet 的五个字段；任一环节不符预期一律返回零值。
//
// 返回值零值的含义是「既无快照也无参数」⇒ 调用方得到惩罚 0（fail-open，与读不到键同判）。
func decodeSlowRateState(cmd *redis.SliceCmd) slowRateState {
	raw, err := cmd.Result()
	if err != nil || len(raw) < 5 {
		return slowRateState{}
	}
	params := slowRatePenaltyParams{
		windowSeconds: slowRateInt(raw[1]),
		triggerCount:  slowRateInt(raw[2]),
		penaltyStep:   slowRateInt(raw[3]),
		penaltyMax:    slowRateInt(raw[4]),
	}
	return slowRateState{
		penalty: slowRateInt(raw[0]),
		params:  params,
		// 四项都要为正才派生：写侧 normalize 已保证生效参数非零，缺任一即为旧数据。
		hasParams: params.windowSeconds > 0 && params.triggerCount > 0 &&
			params.penaltyStep > 0 && params.penaltyMax > 0,
	}
}

// slowRateInt 把 HMGet 的元素折成 int；nil（字段不存在）与非数字一律 0。
func slowRateInt(value any) int {
	text, ok := value.(string)
	if !ok {
		return 0
	}
	number, err := strconv.Atoi(text)
	if err != nil {
		return 0
	}
	return number
}

// deriveSlowRatePenalty 由滑窗内的慢样本数派生惩罚：floor(count/阈值)*步长，封顶上限。
//
// 为什么必须当场派生而不能读状态里的快照：快照只在写侧推进时刷新，而渠道恢复后不再产生慢
// 样本、写侧也就不再触达 Redis——快照会一直停在最后一次推进的值，直到状态键 TTL 到期
// （2 倍窗长）。实测表现是「低速降权 +10 出现后一直不消失」。设计稿「恢复」一节要求的正是
// 「惩罚值随样本过期连续衰减」，滑窗计数才是那个真值。
func deriveSlowRatePenalty(liveCount int, params slowRatePenaltyParams) int {
	if liveCount < params.triggerCount {
		return 0
	}
	penalty := (liveCount / params.triggerCount) * params.penaltyStep
	if penalty > params.penaltyMax {
		return params.penaltyMax
	}
	return penalty
}

// InCooldown 批量判定候选里哪些「对本会话正在冷却期内」。
//
// 语义边界（设计稿 §8 的会话级强制降级）：
//   - 只在本次请求带会话身份时生效（无 sessionID / keyID 即无冷却概念，返回空集）；
//   - 只判定**已开启监控**的渠道：冷却键由写侧在开启后才产生，未开启渠道读它只会白花往返，
//     且「关掉监控」应当立即停止对该渠道的降级；
//   - 读失败 fail-open：判不出冷却即不排除任何人。
//
// 一次往返：全部候选压进同一个 pipeline。
func (r *SlowRateReader) InCooldown(
	ctx context.Context,
	sessionID string,
	keyID int64,
	candidates []Provider,
) map[int64]bool {
	out := make(map[int64]bool, len(candidates))
	if r == nil || r.redis == nil || sessionID == "" || keyID == 0 || len(candidates) == 0 {
		return out
	}
	enabled := make([]Provider, 0, len(candidates))
	for _, provider := range candidates {
		if provider.SlowRateMonitorEnabled {
			enabled = append(enabled, provider)
		}
	}
	if len(enabled) == 0 {
		return out
	}

	cmds := make([]*redis.StringCmd, len(enabled))
	_, err := r.redis.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for index, provider := range enabled {
			cmds[index] = pipe.Get(ctx, SlowRateCooldownKey(sessionID, keyID, provider.ID))
		}
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		r.warn("route.slow_rate_cooldown_read_failed", "", err)
	}
	for index := range enabled {
		// 键存在即命中；redis.Nil 表示未命中（不是故障），其余错误同样不排除（fail-open）。
		if err := cmds[index].Err(); err == nil {
			out[enabled[index].ID] = true
		}
	}
	return out
}

// slowRateBaselineSource 取基线 JSON 的 source 字段；键缺失、读失败、非 JSON 一律返回空串。
//
// 空串不是任何一种有效来源（见 slowRateBaselineUsable），调用方据此跳过惩罚：三种情形都意味着
// 「这条基线读不出来」，而读不出来的基线不支撑任何判定。
func slowRateBaselineSource(cmd *redis.StringCmd) string {
	raw, err := cmd.Result()
	if err != nil {
		return ""
	}
	var payload struct {
		Source string `json:"source"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return ""
	}
	return payload.Source
}

// slowRateBaselineUsable 报告该基线是否足以支撑一次渠道级惩罚。
//
// 只有 primary（A0：主窗 W1 达标）与 extended（A2：回退扩展窗）算有效。其余一律不支撑：
//   - extended_stale（A4）：基线取自陈旧窗口，设计稿明定不做渠道级惩罚；
//   - 空串：键缺失、读失败、坏 JSON——slowRateBaselineSource 对这三种都返回空串。
//
// 为何**键缺失**也必须跳过（这正是本次修的缺陷）：状态 Hash 与慢样本滑窗里的「慢」，是写入时
// 用当时那条基线判出来的。基线已被判定无效（A3：样本不足，基线任务已撤键），再据它派生的状态
// 施惩罚，等于让陈旧判定继续生效——正是 A3 撤键要消除的东西。
//
// 这与 fail-open 不矛盾：fail-open 指的是「读不到就不降权、该渠道按原有优先级参与选路」，
// 不是「读不到就照旧按陈旧状态降权」。
func slowRateBaselineUsable(cmd *redis.StringCmd) bool {
	switch slowRateBaselineSource(cmd) {
	case slowRateSourcePrimary, slowRateSourceExtended:
		return true
	default:
		return false
	}
}

// warn 记一条选路侧的低速读失败。低速读失败不影响选路结果，故只记日志不返回错误。
func (r *SlowRateReader) warn(event, modelKey string, err error) {
	if r == nil || r.logger == nil {
		return
	}
	fields := map[string]any{"error": err.Error()}
	if modelKey != "" {
		fields["modelKey"] = modelKey
	}
	r.logger.Warn(event, fields)
}
