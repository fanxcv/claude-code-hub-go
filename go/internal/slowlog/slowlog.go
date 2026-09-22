// Package slowlog 记录低速降权的**事件**（不是样本）：惩罚升/降档、基线发布/撤销。
//
// 为什么是独立包：写侧在 internal/slowrate 与 internal/jobs 两处（那两处正由并行 lane 改动），
// 读侧在 internal/adminapi（管理面端点）。把键形制、序列化与读写在同一个包里定死，两侧只各留
// 一行调用，既不重复键字面量，也不让并行改动互相踩。
//
// 为什么只落 Redis 不建表：熔断日志本身就没有事件表（状态在 Redis、错误行是 message_request
// 的实时投影），故「与熔断日志同寿命」唯一自洽的落法就是也走 Redis。事件量级极小（生产实测
// 仅 1 家渠道开启低速监控），落表要多一条迁移 + 一个 store 读面 + 一个清理作业，收益是零。
// 代价必须如实登记：**它不持久**，键随 TTL 消失；界面必须写出时间范围，不能让「窗内无记录」
// 被读成「从未发生」。
package slowlog

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Kind 是事件种类。取值是**存储契约**：已落下的条目按它渲染，改名会让界面认不出旧条目。
type Kind string

const (
	// KindPenaltyUp 惩罚升档：该组合的降权量变大（用户可见的「这家被压了」）。
	KindPenaltyUp Kind = "penalty_up"
	// KindPenaltyDown 惩罚降档：降权量变小（滑窗内慢样本减少，或恢复策略直接归零）。
	//
	// 归零也走本档（而非单列一种「reset」）：恢复策略删掉样本与状态后，下一次慢样本到来时
	// 读到的旧值就是 0，于是自然产生一条 `0 -> N` 的升档、以及此前删键时的 `N -> 0` 降档。
	// 单列「reset」需要恢复策略那处多写一行，而那一行不在本包的文件里（见报告「未接线」）。
	KindPenaltyDown Kind = "penalty_down"
	// KindBaselinePublished 基线发布（含中位数与样本数）。
	KindBaselinePublished Kind = "baseline_published"
	// KindBaselineRevoked 基线撤销（陈旧基线不再支配读侧）。
	KindBaselineRevoked Kind = "baseline_revoked"
)

const (
	// keyPrefix 是事件流的键前缀。**刻意不与 `cch:slow:` 同族**：那条前缀下已有
	// `cch:slow:{id:model}:state` 一族，而管理面按前缀扫描那族键（providers_health 的
	// SCAN 模式是 `cch:slow:*:state`）。同族会让扫描多匹配一批键、还得再过滤一次。
	keyPrefix = "cch:slowlog:"

	// MaxEntries 是单渠道保留的事件条数上限。超出即从最旧裁掉（XADD 的 MAXLEN）。
	//
	// 200 的依据：单组合每 30 分钟窗最多 3 条升档（惩罚封顶 3 档），加基线的每小时 1~2 条，
	// 一天的条数在几十量级；200 足以覆盖多组合渠道的观察窗，又不至于把键撑大。
	MaxEntries = 200

	// EventTTL 是事件流的寿命，与熔断状态同寿命（health 的 providerStateTTLSeconds = 86400），
	// 故两者在同一弹窗里不会出现「一边有、一边已过期」的错觉。
	EventTTL = 24 * time.Hour

	// DefaultLimit / MaxLimit 是查询面的默认与硬上限，与熔断日志端点同档。
	DefaultLimit = 20
	MaxLimit     = 100
)

// Logger 是本包记 warn 的最小日志面（`logx.Logger` 与 `slowrate.Logger` 都满足）。
//
// 本包是纯旁路：任何失败只 warn，绝不冒泡——写日志失败绝不能变成结算失败。
type Logger interface {
	Warn(event string, fields map[string]any)
}

// Event 是一条低速事件。
//
// 字段用指针表达「本事件不含此维」：惩罚事件只有 PenaltyFrom/PenaltyTo，基线事件只有
// Median/Samples。零值与「不含」必须可区分，否则界面会把「没这一维」渲染成 0
// （与熔断日志 `circuitLogsState` 的既有纪律一致）。
type Event struct {
	Kind       Kind   `json:"kind"`
	At         int64  `json:"at"` // 毫秒时间戳
	ProviderID int64  `json:"providerId"`
	ModelKey   string `json:"modelKey,omitempty"`
	// PenaltyFrom / PenaltyTo 是降权量的前后值（惩罚类事件）。
	PenaltyFrom *int `json:"penaltyFrom,omitempty"`
	PenaltyTo   *int `json:"penaltyTo,omitempty"`
	// Median / Samples 是基线读数（基线发布事件）。
	Median  *float64 `json:"median,omitempty"`
	Samples *int64   `json:"samples,omitempty"`
	// Reason 是补充说明：基线来源（primary/extended/extended_stale）或撤销原因。
	Reason string `json:"reason,omitempty"`
}

// Key 是一个渠道的事件流键。含渠道 id，故可直接按渠道查（与熔断日志的查询面一致）。
//
// 花括号是 Redis Cluster 的 hash tag：该渠道的事件全落同一槽。
func Key(providerID int64) string {
	return keyPrefix + "{" + strconv.FormatInt(providerID, 10) + "}"
}

// Record 写一条事件。**永不返回错误**：写日志是旁路，失败只 warn。
func Record(ctx context.Context, client redis.UniversalClient, logger Logger, event Event) {
	if client == nil || event.ProviderID <= 0 {
		return
	}
	if event.At == 0 {
		event.At = time.Now().UnixMilli()
	}
	payload, err := json.Marshal(event)
	if err != nil {
		warn(logger, "slowlog.encode_failed", event, err)
		return
	}
	key := Key(event.ProviderID)
	// Pipelined 而非 Pipeline+Exec：两者都是单次往返，但闭包形式让「哪些命令属于这一批」
	// 与 route/slowrate.go 的既有写法一致，也少一个中间变量。
	_, err = client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		// MaxLen 用**精确**裁剪（Approx=false ⇒ `MAXLEN =N`）而不是 `~N`：近似裁剪只保证
		// 「至少留 N 条」，实际可能留下更多，容量就成了不定的。事件频率极低，精确裁剪的开销无意义。
		pipe.XAdd(ctx, &redis.XAddArgs{
			Stream: key,
			MaxLen: MaxEntries,
			Values: map[string]any{"event": string(payload)},
		})
		pipe.Expire(ctx, key, EventTTL)
		return nil
	})
	if err != nil {
		warn(logger, "slowlog.write_failed", event, err)
	}
}

// RecordPenaltyChange 按「旧值 -> 新值」的对比记录一次惩罚变化；**相等即不记**。
//
// 为什么必须去重：写侧在窗内计数达到阈值后**每个慢样本**都会重写状态，若照写不误，日志会
// 退化成逐请求的样本日志（设计稿明说这会造成写放大），而用户要看的是「什么时候被压了」。
//
// previousRaw 直接收 HGet 的**原值字符串**：调用点已在同一次 pipeline 里把这个读数取回来了
// （在 HSet 之前），本函数不再多花一次往返——这正是去重不引入热路径成本的关键。
// 空串（字段不存在，即首次或刚被恢复策略删键）与非数字一律按 0 处理。
func RecordPenaltyChange(
	ctx context.Context,
	client redis.UniversalClient,
	logger Logger,
	providerID int64,
	modelKey string,
	previousRaw string,
	current int,
) {
	if client == nil || providerID <= 0 || current <= 0 {
		return
	}
	old := 0
	if parsed, err := strconv.Atoi(previousRaw); err == nil {
		old = parsed
	}
	if old == current {
		return
	}
	kind := KindPenaltyUp
	if current < old {
		kind = KindPenaltyDown
	}
	from, to := old, current
	Record(ctx, client, logger, Event{
		Kind:        kind,
		ProviderID:  providerID,
		ModelKey:    modelKey,
		PenaltyFrom: &from,
		PenaltyTo:   &to,
	})
}

// RecordRecoveryReset 记一次「恢复策略解除降权」（降权量归零）。
//
// 为何现在才有：KindPenaltyDown 早就定义好了，但**生产上几乎从不产生**——降档发生在选路
// 读侧（读时派生），写侧无从知晓；恢复达阈值又只 DEL 键、不记日志。于是界面上看不到
// 「什么时候恢复的」，而这正是运维最需要的一侧（「它到底好转了没有」）。
//
// 为何复用 KindPenaltyDown 而不新增 Kind：语义就是「降权量变小」（这里是从 N 到 0），
// 且 Kind 是**存储契约**——已落下的条目按它渲染，新增一种会让旧条目与新条目在界面上
// 分成两行，而它们其实是同一件事。reason=recovery 用来把「恢复解除」与「滑窗衰减」区分开。
//
// previousRaw 直接收 HGet 的**原值字符串**（调用方在事务内已取回）：空串（字段不存在）
// 与非数字一律按 0 处理，此时不记——「本来就是 0」没有解除可说。
func RecordRecoveryReset(
	ctx context.Context,
	client redis.UniversalClient,
	logger Logger,
	providerID int64,
	modelKey string,
	previousRaw string,
) {
	if client == nil || providerID <= 0 {
		return
	}
	previous, err := strconv.Atoi(previousRaw)
	if err != nil || previous <= 0 {
		return
	}
	from, to := previous, 0
	Record(ctx, client, logger, Event{
		Kind:        KindPenaltyDown,
		ProviderID:  providerID,
		ModelKey:    modelKey,
		PenaltyFrom: &from,
		PenaltyTo:   &to,
		Reason:      "recovery",
	})
}

// RecordBaselinePublished 记一次基线发布（含中位数与样本数）。
func RecordBaselinePublished(
	ctx context.Context,
	client redis.UniversalClient,
	logger Logger,
	providerID int64,
	modelKey string,
	median float64,
	samples int64,
	source string,
	at time.Time,
) {
	if client == nil || providerID <= 0 {
		return
	}
	Record(ctx, client, logger, Event{
		Kind:       KindBaselinePublished,
		At:         at.UnixMilli(),
		ProviderID: providerID,
		ModelKey:   modelKey,
		Median:     &median,
		Samples:    &samples,
		Reason:     source,
	})
}

// RecordBaselineRevoked 记一次基线撤销。
func RecordBaselineRevoked(
	ctx context.Context,
	client redis.UniversalClient,
	logger Logger,
	providerID int64,
	modelKey string,
) {
	if client == nil || providerID <= 0 {
		return
	}
	Record(ctx, client, logger, Event{
		Kind:       KindBaselineRevoked,
		ProviderID: providerID,
		ModelKey:   modelKey,
		Reason:     "no_baseline",
	})
}

// Reader 按渠道读事件（管理面端点用）。
type Reader struct {
	client redis.UniversalClient
	logger Logger
}

// NewReader 构造读面；client 为 nil 时返回 nil（调用方据此不注册路由）。
func NewReader(client redis.UniversalClient, logger Logger) *Reader {
	if client == nil {
		return nil
	}
	return &Reader{client: client, logger: logger}
}

// Recent 读某渠道最近的事件，**新的在前**。
//
// 无数据时返回**空切片**而不是 nil：界面要能区分「查了，没有记录」与「查不了」，
// 前者是正常结果。
func (r *Reader) Recent(ctx context.Context, providerID int64, limit int) ([]Event, error) {
	if r == nil || r.client == nil || providerID <= 0 {
		return []Event{}, nil
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	items, err := r.client.XRevRangeN(ctx, Key(providerID), "+", "-", int64(limit)).Result()
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(items))
	for _, item := range items {
		raw, ok := item.Values["event"].(string)
		if !ok {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			// 单条坏条目不该让整页失败：跳过并留痕（写侧编码失败才会产生这种条目）。
			warn(r.logger, "slowlog.decode_failed", Event{ProviderID: providerID}, err)
			continue
		}
		events = append(events, event)
	}
	return events, nil
}

func warn(logger Logger, event string, payload Event, err error) {
	if logger == nil {
		return
	}
	logger.Warn(event, map[string]any{
		"providerId": payload.ProviderID,
		"kind":       string(payload.Kind),
		"error":      err.Error(),
	})
}
