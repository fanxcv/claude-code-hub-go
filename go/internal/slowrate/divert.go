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
	// 全库 SCAN 那族键，同族会让扫描多匹配一批键、还得再过滤。
	divertKeyPrefix = "cch:slowdivert:"
)

// DivertKey 是一个渠道的改道计数键（按渠道，跨模型合计——弹窗本来就是按渠道开的）。
//
// 花括号是 Redis Cluster 的 hash tag：该渠道的计数全落同一槽。
func DivertKey(providerID int64) string {
	return divertKeyPrefix + "{" + strconv.FormatInt(providerID, 10) + "}"
}

// DivertBucketField 是某小时桶里某成因的字段名。形制是存储契约的一部分：读侧要按同一形制
// 反解桶起点，改名会让旧字段读不出来。
func DivertBucketField(hourStart int64, cause route.DivertCause) string {
	return strconv.FormatInt(hourStart, 10) + ":" + string(cause)
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
// 写成本：HINCRBY + EXPIRE 同一次 pipeline = 一次往返。失败只 warn（本计数是旁路，
// 与慢样本同样的纪律：绝不影响结算）。
func (s *DivertStore) Record(ctx context.Context, providerID int64, cause route.DivertCause, at time.Time) {
	if s == nil || s.client == nil || providerID <= 0 || cause == "" {
		return
	}
	key := DivertKey(providerID)
	field := DivertBucketField(hourStartUnix(at), cause)
	pipe := s.client.Pipeline()
	pipe.HIncrBy(ctx, key, field, 1)
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
// 用 HGETALL 一次取回整个 Hash（最多 24 桶 × 2 成因 = 48 个字段），在本地按桶起点求和。
// 为什么不在 Redis 侧求和：字段名承载桶起点，求和需要「哪些字段在窗口内」这一判断，
// 而那必须按数值比较——用 Lua 或 HGETALL 都行，但 HGETALL 少一次脚本部署与对拍成本，
// 且该 Hash 尺寸有界（48 字段）。
func (s *DivertStore) ReadDivert(ctx context.Context, providerID int64, now time.Time) (DivertSnapshot, error) {
	snapshot := DivertSnapshot{WindowHours: divertBuckets}
	if s == nil || s.client == nil || providerID <= 0 {
		return snapshot, nil
	}
	fields, err := s.client.HGetAll(ctx, DivertKey(providerID)).Result()
	if err != nil {
		return snapshot, fmt.Errorf("读改道计数失败: %w", err)
	}
	oldest := hourStartUnix(now) - int64(divertBuckets-1)*3600
	for field, raw := range fields {
		hourStart, cause, ok := parseDivertBucket(field)
		if !ok || hourStart < oldest {
			continue
		}
		count, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}
		switch cause {
		case route.DivertCauseCooldown:
			snapshot.Cooldown += count
		case route.DivertCausePenalty:
			snapshot.Penalty += count
		}
	}
	return snapshot, nil
}

// parseDivertBucket 反解桶字段名。形制不对即跳过（旧字段或脏数据不该让整个读数失败）。
func parseDivertBucket(field string) (int64, route.DivertCause, bool) {
	for index := len(field) - 1; index >= 0; index-- {
		if field[index] != ':' {
			continue
		}
		hourStart, err := strconv.ParseInt(field[:index], 10, 64)
		if err != nil {
			return 0, "", false
		}
		return hourStart, route.DivertCause(field[index+1:]), true
	}
	return 0, "", false
}
