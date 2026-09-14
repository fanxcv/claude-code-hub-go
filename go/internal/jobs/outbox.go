package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"sync"
	"time"
)

// 语义出处（Node）：src/repository/routing-trace-outbox.ts。
//
// 为什么 Go 要做**消费侧**：outbox 是 Node 写入路径的产物（stageRoutingTraceOutbox 把
// routing_trace 先暂存到 Redis，再由回放器幂等地落库）。切换期 Node 可能写完 outbox 就被
// 摘掉或重启，条目会留在 Redis 里没人认领——Go 的回收器认领它，账本行才不会永久缺轨迹。
const (
	// OutboxHashKey / OutboxIndexKey 与 Node 的键名逐字一致（切换期两侧读写同一份）。
	OutboxHashKey  = "cch:routing-trace-outbox:v1"
	OutboxIndexKey = "cch:routing-trace-outbox:v1:index"

	outboxMaxEntries      = 10_000
	outboxRetention       = 7 * 24 * time.Hour
	outboxTrimBatch       = 1_000
	outboxDefaultReplay   = 100
	outboxBacklogWarn     = 1_000
	outboxBacklogErrorGap = 5 * time.Minute
	outboxEvery           = 30 * time.Second
)

// outboxDeleteIfUnchanged 与 Node DELETE_IF_UNCHANGED_LUA 逐字一致。
//
// 「不变才删」是回放幂等的关键：扫描到落库之间可能又有新的（更高修订的）载荷写进同一字段，
// 无条件 HDEL 会把那条更新的载荷一起删掉，轨迹就永久停在旧版本。
const outboxDeleteIfUnchanged = `
local current = redis.call('HGET', KEYS[1], ARGV[1])
if current == ARGV[2] then
  local deleted = redis.call('HDEL', KEYS[1], ARGV[1])
  redis.call('ZREM', KEYS[2], ARGV[1])
  return deleted
end
return 0`

// outboxIndexScannedAndTrim 与 Node INDEX_SCANNED_AND_TRIM_LUA 逐字一致。
//
// 两件事：把本轮扫到的字段登记进有界索引（历史版本的条目没有索引，靠这里惰性补齐），
// 以及按「过期 + 上限」裁剪索引与哈希——没有这一步，中断期间堆积的条目会无界增长。
const outboxIndexScannedAndTrim = `
for index = 5, #ARGV do
  local field = ARGV[index]
  if redis.call('HEXISTS', KEYS[1], field) == 1 then
    redis.call('ZADD', KEYS[2], 'NX', ARGV[1], field)
  end
end

local expired = redis.call(
  'ZRANGEBYSCORE',
  KEYS[2],
  '-inf',
  ARGV[2],
  'LIMIT',
  0,
  ARGV[4]
)
for _, field in ipairs(expired) do
  redis.call('ZREM', KEYS[2], field)
  redis.call('HDEL', KEYS[1], field)
end

local overflow = redis.call('ZCARD', KEYS[2]) - tonumber(ARGV[3])
local evicted = {}
if overflow > 0 then
  local trim_count = math.min(overflow, tonumber(ARGV[4]))
  evicted = redis.call('ZRANGE', KEYS[2], 0, trim_count - 1)
  for _, field in ipairs(evicted) do
    redis.call('ZREM', KEYS[2], field)
    redis.call('HDEL', KEYS[1], field)
  end
end

return {#expired, #evicted, redis.call('ZCARD', KEYS[2])}`

// OutboxEntry 是 outbox 载荷（Node RoutingTraceOutboxEntry）。
type OutboxEntry struct {
	Version        int             `json:"version"`
	RequestID      int64           `json:"requestId"`
	TraceUpdatedAt float64         `json:"traceUpdatedAt"`
	RoutingTrace   json.RawMessage `json:"routingTrace"`
}

// ParseOutboxEntry 解析并校验条目（Node parseOutboxEntry 的判据，逐条对齐）。
//
// 判据里有一条容易忽略：`routingTrace.updatedAt` 必须等于条目顶层的 `traceUpdatedAt`。
// 两者不一致说明载荷被改过或版本不匹配，此时宁可丢弃也不能猜——写错轨迹比不写更难查。
func ParseOutboxEntry(payload string) (*OutboxEntry, bool) {
	var probe struct {
		Version        *int            `json:"version"`
		RequestID      *json.Number    `json:"requestId"`
		TraceUpdatedAt *json.Number    `json:"traceUpdatedAt"`
		RoutingTrace   json.RawMessage `json:"routingTrace"`
	}
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		return nil, false
	}
	if probe.Version == nil || *probe.Version != 1 {
		return nil, false
	}
	if probe.RequestID == nil || probe.TraceUpdatedAt == nil {
		return nil, false
	}
	requestID, err := probe.RequestID.Int64()
	if err != nil || requestID <= 0 {
		return nil, false
	}
	entryRevision, ok := finiteNumber(probe.TraceUpdatedAt)
	if !ok {
		return nil, false
	}
	traceRevision, ok := traceUpdatedAt(probe.RoutingTrace)
	if !ok || traceRevision != entryRevision {
		return nil, false
	}
	return &OutboxEntry{
		Version:        *probe.Version,
		RequestID:      requestID,
		TraceUpdatedAt: entryRevision,
		RoutingTrace:   probe.RoutingTrace,
	}, true
}

// traceUpdatedAt 取 routingTrace.updatedAt 的有限数字值。
func traceUpdatedAt(trace json.RawMessage) (float64, bool) {
	if len(trace) == 0 {
		return 0, false
	}
	var probe struct {
		UpdatedAt *json.Number `json:"updatedAt"`
	}
	if err := json.Unmarshal(trace, &probe); err != nil || probe.UpdatedAt == nil {
		return 0, false
	}
	return finiteNumber(probe.UpdatedAt)
}

func finiteNumber(value *json.Number) (float64, bool) {
	parsed, err := value.Float64()
	if err != nil {
		return 0, false
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

// OutboxReplay 回放 routing trace outbox。
type OutboxReplay struct {
	deps OpsDeps
	// Limit 是单轮 HSCAN 的 COUNT 提示（Node DEFAULT_REPLAY_LIMIT）。
	Limit int

	mu        sync.Mutex
	cursor    uint64
	lastLogAt time.Time
}

// NewOutboxReplay 构造回放任务。
func NewOutboxReplay(deps OpsDeps) *OutboxReplay {
	return &OutboxReplay{deps: deps, Limit: outboxDefaultReplay}
}

// OutboxReplayResult 是单轮结果（字段名与 Node 的日志键一致）。
type OutboxReplayResult struct {
	Available bool
	Scanned   int
	Replayed  int
	Discarded int
	Retained  int
	Backlog   int
}

// Run 执行一轮回放。
func (o *OutboxReplay) Run(ctx context.Context) (OpsOutcome, error) {
	result, err := o.replayOnce(ctx)
	if err != nil {
		return OpsOutcome{}, err
	}
	if !result.Available {
		return OpsOutcome{Fields: map[string]any{"skipped": "no_redis"}}, nil
	}
	outcome := OpsOutcome{
		Processed: result.Replayed,
		Fields: map[string]any{
			"scanned":   result.Scanned,
			"replayed":  result.Replayed,
			"discarded": result.Discarded,
			"retained":  result.Retained,
			"backlog":   result.Backlog,
		},
	}
	if result.Scanned > 0 || result.Retained > 0 {
		o.deps.logger().Info("routing_trace_outbox_replay_completed", outcome.Fields)
	}
	return outcome, nil
}

// replayOnce 是单轮主体（Node replayRoutingTraceOutbox）。
func (o *OutboxReplay) replayOnce(ctx context.Context) (OutboxReplayResult, error) {
	result := OutboxReplayResult{}
	if o.deps.Pools == nil {
		return result, errors.New("jobs: outbox 回放需要数据库连接池")
	}
	if o.deps.Redis == nil {
		return result, nil
	}
	result.Available = true

	limit := o.Limit
	if limit < 1 {
		limit = outboxDefaultReplay
	}

	o.mu.Lock()
	cursor := o.cursor
	o.mu.Unlock()

	fields, nextCursor, err := o.deps.Redis.HScan(ctx, OutboxHashKey, cursor, "", int64(limit)).Result()
	if err != nil {
		return result, err
	}
	o.mu.Lock()
	o.cursor = nextCursor
	o.mu.Unlock()

	// Redis 的 COUNT 只是提示，一页可能大于 limit：整页处理完再推进游标，否则会漏掉尾部。
	scannedFields := make([]string, 0, len(fields)/2)
	for index := 0; index+1 < len(fields); index += 2 {
		field := fields[index]
		payload := fields[index+1]
		scannedFields = append(scannedFields, field)
		result.Scanned++

		entry, valid := ParseOutboxEntry(payload)
		if !valid || field != strconv.FormatInt(entry.RequestID, 10) {
			deleted, err := o.deleteIfUnchanged(ctx, field, payload)
			if err != nil {
				result.Retained++
				continue
			}
			if deleted {
				result.Discarded++
			} else {
				result.Retained++
			}
			continue
		}

		exists, err := o.deps.Pools.PersistRoutingTraceMonotonic(
			ctx, entry.RequestID, entry.RoutingTrace, entry.TraceUpdatedAt,
		)
		if err != nil {
			// 落库失败：条目**保留**在 Redis 里等下一轮，绝不删除（删了就永久丢轨迹）。
			result.Retained++
			o.deps.logger().Warn("routing_trace_outbox_replay_failed", map[string]any{
				"requestId": entry.RequestID,
				"error":     err.Error(),
			})
			continue
		}
		if exists {
			result.Replayed++
		} else {
			// 目标行不存在（请求行尚未开或已被删除）：条目失去意义，丢弃。
			result.Discarded++
		}
		if deleted, err := o.deleteIfUnchanged(ctx, field, payload); err != nil || !deleted {
			result.Retained++
		}
	}

	o.indexScannedAndTrim(ctx, scannedFields)

	backlog, err := o.deps.Redis.HLen(ctx, OutboxHashKey).Result()
	if err == nil {
		result.Backlog = int(backlog)
		o.logBacklogPressure(result.Backlog)
	}
	return result, nil
}

// deleteIfUnchanged 确认条目未被更新过再删除。
func (o *OutboxReplay) deleteIfUnchanged(ctx context.Context, field, payload string) (bool, error) {
	deleted, err := o.deps.Redis.Eval(
		ctx, outboxDeleteIfUnchanged, []string{OutboxHashKey, OutboxIndexKey}, field, payload,
	).Int()
	if err != nil {
		o.deps.logger().Warn("routing_trace_outbox_ack_failed", map[string]any{
			"requestId": field,
			"error":     err.Error(),
		})
		return false, err
	}
	return deleted > 0, nil
}

// indexScannedAndTrim 把扫到的字段补进有界索引并按上限裁剪。
func (o *OutboxReplay) indexScannedAndTrim(ctx context.Context, fields []string) {
	if len(fields) == 0 {
		return
	}
	now := o.deps.now()
	keys := []string{OutboxHashKey, OutboxIndexKey}
	args := []any{
		now.UnixMilli(),
		now.Add(-outboxRetention).UnixMilli(),
		outboxMaxEntries,
		outboxTrimBatch,
	}
	for _, field := range fields {
		args = append(args, field)
	}
	if err := o.deps.Redis.Eval(ctx, outboxIndexScannedAndTrim, keys, args...).Err(); err != nil {
		o.deps.logger().Warn("routing_trace_outbox_bounds_failed", map[string]any{
			"error": err.Error(),
		})
	}
}

// logBacklogPressure 按 Node 的阈值与节流记积压告警。
//
// 节流的原因：回放 30 秒一跳，积压时每轮都告警会把日志淹掉，真信号反而被埋。
func (o *OutboxReplay) logBacklogPressure(backlog int) {
	if backlog < outboxBacklogWarn {
		return
	}
	now := o.deps.now()
	o.mu.Lock()
	shouldLog := o.lastLogAt.IsZero() || now.Sub(o.lastLogAt) >= outboxBacklogErrorGap
	if shouldLog {
		o.lastLogAt = now
	}
	o.mu.Unlock()
	if !shouldLog {
		return
	}
	fields := map[string]any{"backlog": backlog, "maxEntries": outboxMaxEntries}
	if backlog >= outboxMaxEntries {
		o.deps.logger().Error("routing_trace_outbox_backlog_full", fields)
		return
	}
	o.deps.logger().Warn("routing_trace_outbox_backlog_high", fields)
}
