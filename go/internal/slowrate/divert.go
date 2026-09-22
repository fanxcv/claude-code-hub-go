package slowrate

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/redis/go-redis/v9"
)

// 本文件是「因低速被改道请求数」的**存储面**（用户 2026-09-22 需求）。
//
// 为什么按小时分桶而不是单个计数器：用户要的是**真窗口**（「最近 24 小时改道了多少」），
// 而「TTL 每次 INCR 都刷新」的 lifetime 计数答的是另一个问题（「距上次改道多久内的累计」）——
// 一个稳定每小时的流量会让后者的 TTL 永不过期，于是它单调增长、永不归零，读出来的数
// 越看越大却对不上任何窗口。分桶则天然可精确求和：读时只把目标窗口内的桶相加。
//
// 为什么落在 slowrate 而不是 slowlog：slowlog 是**事件流**（一条一条的降权/基线事件），
// 而这是**聚合计数**（每请求一次，量大且无单条价值）。两者寿命与读取方式都不同：
// 事件流按条读、本计数按窗口求和。混在一个键里会让事件流被逐请求条目淹没（slowlog 的
// MaxEntries=200 会被几分钟的流量冲垮，界面从此看不到降权事件）。

const (
	// divertBuckets 是保留的整点桶数。24 ⇒ 「最近 24 小时」这一档可精确求和。
	divertBuckets = 24
	// divertTTL 是键的寿命。取 25 小时（略长于 24 桶）：边界桶在满 24 小时后仍能被读到，
	// 否则「最近 24 小时」的求和会在每个整点刚过时少掉最老那一桶。
	divertTTL = 25 * time.Hour
	// divertKeyPrefix 是键前缀。**刻意不与 `cch:slow:` 同族**：管理面按 `cch:slow:*:state`
	// 全库 SCAN 那族键，同族会让扫描多匹配一批键、还得再过滤（本前缀不含那个冒号，故不匹配）。
	divertKeyPrefix = "cch:slowdivert:"
)

// divertBucketKey 是「一小时桶 × 一个成因」的键：`cch:slowdivert:{<pid>}:<hourStart>:<cause>`。
//
// 一桶一键（而不是一个渠道一个 Hash + 字段）是为**严格有界**：Hash 只能靠「写时剪掉滑出窗口
// 的那一桶」维护，而删除目标随当前整点**单向递增**，间断（尤其反复出现 24~25 小时的间断）
// 会让漏删的桶**永不再被触及**，字段数照样无界——实测三轮「活跃 24h + 间断 24h」后攒到 140 项。
// 一桶一键则每键自带 25 小时 TTL，键数自然上界 25h × 2 成因 = 50，且**不需要任何剪除逻辑**。
// 代价是键数 ×48（每渠道 ≤ 50），读面由窗口推导 48 个键名、一次 MGET 取回。
//
// 花括号是 Redis Cluster 的 hash tag：该渠道的所有桶全落同一槽（MGET 因而不会触发 CROSSSLOT）。
//
// 旧形制（单 Hash `cch:slowdivert:{pid}`，字段名 `<hourStart>:<cause>`）**直接放弃、不回读**：
// 本计数自 v1.9.21 上线起仅两小时、量极小，而回读旧形制要多养一套解析与兼容分支。故读到 0
// 不是故障，是换形制的预期结果。
func divertBucketKey(providerID int64, hourStart int64, cause route.DivertCause) string {
	return divertKeyPrefix + "{" + strconv.FormatInt(providerID, 10) + "}:" +
		strconv.FormatInt(hourStart, 10) + ":" + string(cause)
}

// DivertSnapshot 是一个渠道在窗口内的改道读数。
//
// 两个分项都要，且**不合并成一个总数就完事**：两者成因不同（会话冷却 vs 渠道降权），
// 运维的下一步动作也不同（冷却看会话与该家的历史，降权看该家的慢率与基线）。
type DivertSnapshot struct {
	// WindowHours 是求和所覆盖的整点桶数（口径必须随读数一起给，否则「12 次」无意义）。
	WindowHours int   `json:"windowHours"`
	Cooldown    int64 `json:"cooldown"`
	Penalty     int64 `json:"penalty"`
}

// Total 是两分项之和。
func (s DivertSnapshot) Total() int64 { return s.Cooldown + s.Penalty }

// DivertStore 是改道计数的读写面。读写同源一个类型：键形制与字段编码只应有一处实现。
//
// 为何不并进 Recorder：Recorder 的构造要求 ConfigSource（无配置源即返回 nil），而读面
// （管理面端点）不需要配置源——把读面挂在 Recorder 上会让「无配置源」的进程连读都拿不到。
type DivertStore struct {
	client redis.UniversalClient
	logger Logger
}

// NewDivertStore 构造改道读写面；client 为 nil 时返回 nil（调用方据此不装配）。
func NewDivertStore(client redis.UniversalClient, logger Logger) *DivertStore {
	if client == nil {
		return nil
	}
	return &DivertStore{client: client, logger: logger}
}

// Record 把一次改道计入当前整点桶。
//
// 不受「渠道是否开启低速监控」闸门约束：该渠道即便刚被关闭监控，它被冷却/降权挤掉的
// 历史仍是真实发生过的，而闸门的意义是「不监控即不算慢」，不是「不算改道」。
//
// 写成本：INCR + EXPIRE 同一次 pipeline = 一次往返（与旧形制相同）。失败只 warn（本计数是旁路，
// 与慢样本同样的纪律：绝不影响结算）。
func (s *DivertStore) Record(ctx context.Context, providerID int64, cause route.DivertCause, at time.Time) {
	if s == nil || s.client == nil || providerID <= 0 || cause == "" {
		return
	}
	key := divertBucketKey(providerID, hourStartUnix(at), cause)
	pipe := s.client.Pipeline()
	pipe.Incr(ctx, key)
	// 刷新本键 TTL：桶只在自己那一小时里收写，故「最后一次写 + 25h」就是该桶需要活到的最晚
	// 时刻（读完窗内最老那桶还需 1 小时余量），不会无限续命。
	pipe.Expire(ctx, key, divertTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		s.warn("slowrate.divert_write_failed", map[string]any{
			"providerId": providerID,
			"cause":      string(cause),
		}, err)
	}
}

// warn 是本类型的日志出口（与 slowlog.warn 同纪律：旁路失败只 warn）。
func (s *DivertStore) warn(event string, fields map[string]any, err error) {
	if s == nil || s.logger == nil {
		return
	}
	fields["error"] = err.Error()
	s.logger.Warn(event, fields)
}

// hourStartUnix 把时刻折成它所在整点的秒级时间戳（UTC 无关：整点对齐是绝对的）。
func hourStartUnix(at time.Time) int64 {
	return at.Truncate(time.Hour).Unix()
}

// ReadDivert 读一个渠道的窗口内改道读数（`/providers/{id}/slow-logs` 的 diverts 字段）。
//
// 键名由窗口完全推导（24 桶 × 2 成因 = 48 个），故一次 pipeline（两条 MGET）取回、本地求和，
// 缺失键（那小时没被改道过）按 0 计。为什么不在 Redis 侧求和：求和需要「哪些桶在窗口内」
// 这一判断，而那由窗口直接决定，客户端推导比一次脚本部署与黄金对拍便宜。
func (s *DivertStore) ReadDivert(ctx context.Context, providerID int64, now time.Time) (DivertSnapshot, error) {
	snapshot := DivertSnapshot{WindowHours: divertBuckets}
	if s == nil || s.client == nil || providerID <= 0 {
		return snapshot, nil
	}
	current := hourStartUnix(now)
	cooldownKeys := make([]string, 0, divertBuckets)
	penaltyKeys := make([]string, 0, divertBuckets)
	for offset := divertBuckets - 1; offset >= 0; offset-- {
		hourStart := current - int64(offset)*3600
		cooldownKeys = append(cooldownKeys, divertBucketKey(providerID, hourStart, route.DivertCauseCooldown))
		penaltyKeys = append(penaltyKeys, divertBucketKey(providerID, hourStart, route.DivertCausePenalty))
	}
	pipe := s.client.Pipeline()
	cooldowns := pipe.MGet(ctx, cooldownKeys...)
	penalties := pipe.MGet(ctx, penaltyKeys...)
	if _, err := pipe.Exec(ctx); err != nil {
		return snapshot, fmt.Errorf("读改道计数失败: %w", err)
	}
	snapshot.Cooldown = sumDivertBuckets(cooldowns.Val())
	snapshot.Penalty = sumDivertBuckets(penalties.Val())
	return snapshot, nil
}

// sumDivertBuckets 求一次 MGET 结果的和。缺失键（nil）与脏值都按 0 计——脏数据不该让整个读数失败。
func sumDivertBuckets(values []any) int64 {
	var total int64
	for _, raw := range values {
		text, ok := raw.(string)
		if !ok {
			continue
		}
		count, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			continue
		}
		total += count
	}
	return total
}
