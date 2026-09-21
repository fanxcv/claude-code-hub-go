package route

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

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
	slowRateKeyPrefix          = "cch:slow:"
	slowRateStateSuffix        = ":state"
	slowRateBaselineSuffix     = ":baseline"
	slowRateStateFieldPenalty  = "penalty"
	slowRateBaselineFieldName  = "source"
	slowRateSourceExtended     = "extended_stale"
	slowRateCooldownKeyPattern = "session-binding:v1:{%s}:provider:%s:cooldown"
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
}

// NewSlowRateReader 构造读取器；Redis 为 nil 时返回 nil，调用方据此整段跳过（等同未装配）。
func NewSlowRateReader(redisClient redis.UniversalClient, logger *logx.Logger) *SlowRateReader {
	if redisClient == nil {
		return nil
	}
	return &SlowRateReader{redis: redisClient, logger: logger}
}

// Penalties 批量读候选渠道的渠道级惩罚。
//
// 返回「providerID -> penalty」，只含**惩罚为正**且**未被 extended_stale 抑制**的渠道；
// 读不到、读失败、惩罚非正一律不收录——调用方按「无惩罚」继续选路（fail-open）。
//
// 为什么 fail-open 而不是 fail-closed：低速是**软降权**，不是硬故障。Redis 抖动时把候选
// 整批降权（或反过来全部排除）会让一次故障变成选路行为突变，而低速机制本身并不承担
// 「保证不选慢渠道」的职责。
//
// 零开销：`SlowRateMonitorEnabled` 为假的渠道不进 Redis——默认全关时本方法在开头的
// 长度判断处直接返回，选路热路径上一个往返都不产生。
//
// 一次往返：状态键与基线键各一条命令，全部压进同一个 pipeline（候选数通常个位数）。
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

	states := make([]*redis.StringCmd, len(enabled))
	baselines := make([]*redis.StringCmd, len(enabled))
	// Pipelined 而非 Pipeline+Exec：两者都是单次往返，但闭包形式让「哪些命令属于这一批」
	// 在类型上不可分割，也让测试替身只需实现一个方法。
	_, err := r.redis.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for index, provider := range enabled {
			states[index] = pipe.HGet(ctx, SlowRateStateKey(provider.ID, modelKey), slowRateStateFieldPenalty)
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
	for index := range enabled {
		penalty, err := states[index].Int()
		if err != nil || penalty <= 0 {
			continue
		}
		// extended_stale（设计稿 §3 的 A4）只做会话级降级、不做渠道级惩罚：
		// 该组合刚从故障/下线恢复，基线取自陈旧窗口，不足以支撑一次全局范围的分层改写。
		if slowRateBaselineSource(baselines[index]) == slowRateSourceExtended {
			continue
		}
		out[enabled[index].ID] = penalty
	}
	return out
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

// slowRateBaselineSource 取基线 JSON 的 source 字段；缺失、非 JSON、读失败一律返回空串
// （空串不等于 extended_stale，故惩罚照常生效——这是 fail-open 的一侧）。
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
