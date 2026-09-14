package pubstatus

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是投影写侧的第二段：**从 rollup 桶构建公开 payload**。
//
// Node 出处：
//   - `buildBucketStarts`（rollup-store.ts:604-624）：窗口与桶起点的推导；
//   - `readPublicStatusRollupBuckets`（rollup-store.ts:489-543）：批量 HGETALL；
//   - `buildPublicStatusPayloadFromRollups`（rollup-store.ts:626-811）：聚合与状态判定。
//
// 它同时是「桶宽 5 分钟」与「展示粒度 intervalMinutes」之间的换算层：展示桶把
// `intervalMinutes / 5` 个基础桶相加，故 15/30/60 分钟档不需要额外的写入路径。

// RollupBucket 是一个基础桶的内容（rollup-store.ts:55-58）。
type RollupBucket struct {
	BucketStart string
	// Values 的键是 rollup 字段（`groupId|modelKey|metric`），非有限数值在读入时即丢弃。
	Values map[string]float64
}

// RollupAggregationResult 是聚合结果（rollup-store.ts:60-65）。
type RollupAggregationResult struct {
	GeneratedAt string
	CoveredFrom string
	CoveredTo   string
	Groups      []PublicStatusPayloadGroup
}

// MaxRollupGapBuckets 复刻 applyBoundedGapFill 的默认上限（rollup-store.ts:563）。
const MaxRollupGapBuckets = 3

// BuildRollupBucketStarts 复刻 buildBucketStarts（rollup-store.ts:604-624）。
//
// 两个粒度要分清：**基础桶宽恒为 5 分钟**（写入侧），而 coveredTo 对齐到
// `intervalMinutes`（展示粒度）。窗口长度是 `ceil(rangeHours*60/5)` 个基础桶，
// 故 coveredFrom 落在基础桶边界上。
func BuildRollupBucketStarts(now time.Time, rangeHours int, intervalMinutes int) (
	coveredFrom string,
	coveredTo string,
	bucketStarts []string,
	err error,
) {
	if err := assertSupportedRollupInterval(intervalMinutes); err != nil {
		return "", "", nil, err
	}
	baseBucketMs := int64(PublicStatusRollupBucketMinutes) * 60 * 1000
	bucketCount := int(math.Ceil(float64(rangeHours*60) / float64(PublicStatusRollupBucketMinutes)))
	coveredTo, err = AlignBucketStartUTC(now.UTC().Format(isoMilliLayout), intervalMinutes)
	if err != nil {
		return "", "", nil, err
	}
	coveredToMs, err := ParseISOMilli(coveredTo)
	if err != nil {
		return "", "", nil, err
	}
	coveredFromMs := coveredToMs.UnixMilli() - int64(bucketCount)*baseBucketMs
	starts := make([]string, 0, bucketCount)
	for index := 0; index < bucketCount; index++ {
		starts = append(starts, time.UnixMilli(coveredFromMs+int64(index)*baseBucketMs).UTC().Format(isoMilliLayout))
	}
	return time.UnixMilli(coveredFromMs).UTC().Format(isoMilliLayout), coveredTo, starts, nil
}

// assertSupportedRollupInterval 复刻 assertSupportedPublicStatusRollupInterval
// （rollup-store.ts:123-135）：只接受 5/15/30/60 四档。
func assertSupportedRollupInterval(intervalMinutes int) error {
	for _, option := range PublicStatusIntervalOptions {
		if option == intervalMinutes {
			return nil
		}
	}
	return fmt.Errorf(
		"pubstatus: unsupported public status rollup intervalMinutes: %d（支持 5/15/30/60）",
		intervalMinutes,
	)
}

// BuildRollupBucketStartsOnly 复刻 buildPublicStatusRollupBucketStarts（rollup-store.ts:813-819）：
// worker 只取桶起点列表去读桶。
func BuildRollupBucketStartsOnly(now time.Time, rangeHours int, intervalMinutes int) ([]string, error) {
	_, _, starts, err := BuildRollupBucketStarts(now, rangeHours, intervalMinutes)
	return starts, err
}

// RollupBucketReader 是批量读桶需要的 Redis 子集。
type RollupBucketReader interface {
	// HGetAll 返回一个桶的全部字段；键不存在时返回空表（与 go-redis 同义）。
	HGetAll(ctx context.Context, key string) (map[string]string, error)
}

// NewRedisRollupBucketReader 把 go-redis 客户端包成 RollupBucketReader。
func NewRedisRollupBucketReader(client redis.UniversalClient) RollupBucketReader {
	if client == nil {
		return nil
	}
	return redisRollupBucketReader{client: client}
}

type redisRollupBucketReader struct {
	client redis.UniversalClient
}

func (r redisRollupBucketReader) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return r.client.HGetAll(ctx, key).Result()
}

// ReadRollupBuckets 复刻 readPublicStatusRollupBuckets（rollup-store.ts:489-543）。
//
// 读失败与「桶不存在」在 Node 里同判：都给一个空桶（公开页宁少不脏）。故本函数**不返回错误**——
// 单个桶读不到就地降级为空桶，逐个返回错误会让上层在「Redis 抖一下」时丢掉整批桶。
func ReadRollupBuckets(
	ctx context.Context,
	reader RollupBucketReader,
	bucketStarts []string,
	prefix string,
) []RollupBucket {
	buckets := make([]RollupBucket, 0, len(bucketStarts))
	for _, bucketStart := range bucketStarts {
		values := map[string]float64{}
		if reader != nil {
			key, err := BuildRollupKey(bucketStart, PublicStatusRollupBucketMinutes, prefix)
			if err == nil {
				if raw, err := reader.HGetAll(ctx, key); err == nil {
					for field, rawValue := range raw {
						parsed, parseErr := strconv.ParseFloat(rawValue, 64)
						if parseErr != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
							continue
						}
						values[field] = parsed
					}
				}
			}
		}
		buckets = append(buckets, RollupBucket{BucketStart: bucketStart, Values: values})
	}
	return buckets
}

// ApplyBoundedGapFill 复刻 applyBoundedGapFill（rollup-store.ts:562-595）。
//
// 语义：相邻两个「已知状态」相同、且中间空档不超过 maxGapBuckets 时，把空档填成同一状态。
// 这是公开页「不因一次采集缺口就断开一条时间线」的做法；超过上限就留空（诚实显示缺口）。
func ApplyBoundedGapFill(timeline []*string, maxGapBuckets int) []*string {
	if maxGapBuckets <= 0 {
		maxGapBuckets = MaxRollupGapBuckets
	}
	result := make([]*string, len(timeline))
	copy(result, timeline)

	lastKnownIndex := -1
	for index, current := range timeline {
		if current == nil {
			continue
		}
		if lastKnownIndex >= 0 {
			previous := timeline[lastKnownIndex]
			gapBuckets := index - lastKnownIndex - 1
			if gapBuckets > 0 && gapBuckets <= maxGapBuckets && previous != nil && *previous == *current {
				for fillIndex := lastKnownIndex + 1; fillIndex < index; fillIndex++ {
					filled := *previous
					result[fillIndex] = &filled
				}
			}
		}
		lastKnownIndex = index
	}
	return result
}

// average 复刻 average（rollup-store.ts:597-602）：count<=0 或非有限值时无值，否则 4 位舍入。
func average(sum float64, count float64) *float64 {
	if math.IsNaN(sum) || math.IsInf(sum, 0) || math.IsNaN(count) || math.IsInf(count, 0) || count <= 0 {
		return nil
	}
	value := roundTo4(sum / count)
	return &value
}

func getRollupValue(bucket RollupBucket, groupID string, modelKey string, metric RollupMetric) float64 {
	return bucket.Values[BuildRollupField(groupID, modelKey, metric)]
}

// BuildPayloadFromRollups 复刻 buildPublicStatusPayloadFromRollups（rollup-store.ts:626-811）。
//
// 这里的每个 `toFixed` 都是逐字节对拍项：可用率两位、均值四位。故一律走 toFixed4 家族，
// 不用简单的乘除取整。
func BuildPayloadFromRollups(
	now time.Time,
	rangeHours int,
	intervalMinutes int,
	groups []ConfiguredGroup,
	rollupBuckets []RollupBucket,
) (RollupAggregationResult, error) {
	coveredFrom, coveredTo, bucketStarts, err := BuildRollupBucketStarts(now, rangeHours, intervalMinutes)
	if err != nil {
		return RollupAggregationResult{}, err
	}

	bucketByStart := make(map[string]RollupBucket, len(rollupBuckets))
	for _, bucket := range rollupBuckets {
		bucketByStart[bucket.BucketStart] = bucket
	}
	intervalFactor := int(math.Max(1, math.Round(float64(intervalMinutes)/float64(PublicStatusRollupBucketMinutes))))

	type displayBucket struct {
		bucketStart string
		index       int
	}
	displayBuckets := make([]displayBucket, 0, len(bucketStarts))
	for index, bucketStart := range bucketStarts {
		if index%intervalFactor == 0 {
			displayBuckets = append(displayBuckets, displayBucket{bucketStart: bucketStart, index: index})
		}
	}

	type modelAggregate struct {
		bucketStart  string
		successCount float64
		failureCount float64
		ttfbSum      float64
		ttfbCount    float64
		tpsSum       float64
		tpsCount     float64
	}

	payloadGroups := make([]PublicStatusPayloadGroup, 0, len(groups))
	for _, group := range groups {
		groupIDs := []string{ConfiguredGroupID(group)}
		models := make([]PublicStatusPayloadModel, 0, len(group.Models))
		for _, model := range group.Models {
			aggregateBuckets := make([]modelAggregate, 0, len(displayBuckets))
			for _, display := range displayBuckets {
				aggregate := modelAggregate{bucketStart: display.bucketStart}
				end := display.index + intervalFactor
				if end > len(bucketStarts) {
					end = len(bucketStarts)
				}
				for _, bucketStart := range bucketStarts[display.index:end] {
					bucket, ok := bucketByStart[bucketStart]
					if !ok {
						continue
					}
					for _, groupID := range groupIDs {
						aggregate.successCount += getRollupValue(bucket, groupID, model.PublicModelKey, RollupMetricSuccess)
						aggregate.failureCount += getRollupValue(bucket, groupID, model.PublicModelKey, RollupMetricFailure)
						aggregate.ttfbSum += getRollupValue(bucket, groupID, model.PublicModelKey, RollupMetricTTFbSum)
						aggregate.ttfbCount += getRollupValue(bucket, groupID, model.PublicModelKey, RollupMetricTTFbCount)
						aggregate.tpsSum += getRollupValue(bucket, groupID, model.PublicModelKey, RollupMetricTPSSum)
						aggregate.tpsCount += getRollupValue(bucket, groupID, model.PublicModelKey, RollupMetricTPSCount)
					}
				}
				aggregateBuckets = append(aggregateBuckets, aggregate)
			}

			rawTimeline := make([]*string, 0, len(aggregateBuckets))
			for _, bucket := range aggregateBuckets {
				total := bucket.successCount + bucket.failureCount
				if total <= 0 {
					rawTimeline = append(rawTimeline, nil)
					continue
				}
				state := string(TimelineStateFailed)
				if bucket.successCount > 0 {
					state = string(TimelineStateOperational)
				}
				rawTimeline = append(rawTimeline, &state)
			}
			filledTimeline := ApplyBoundedGapFill(rawTimeline, MaxRollupGapBuckets)

			var latestTTFTMs *float64
			var latestTPS *float64
			timeline := make([]PublicStatusTimelineBucket, 0, len(aggregateBuckets))
			for index, bucket := range aggregateBuckets {
				bucketStartMs, parseErr := ParseISOMilli(bucket.bucketStart)
				if parseErr != nil {
					return RollupAggregationResult{}, parseErr
				}
				total := bucket.successCount + bucket.failureCount
				var availabilityPct *float64
				switch {
				case total > 0:
					value := toFixed2(bucket.successCount / total * 100)
					availabilityPct = &value
				case filledTimeline[index] != nil && *filledTimeline[index] == string(TimelineStateOperational):
					value := float64(100)
					availabilityPct = &value
				case filledTimeline[index] != nil && *filledTimeline[index] == string(TimelineStateFailed):
					value := float64(0)
					availabilityPct = &value
				}
				ttftMs := average(bucket.ttfbSum, bucket.ttfbCount)
				tps := average(bucket.tpsSum, bucket.tpsCount)
				if ttftMs != nil {
					latestTTFTMs = ttftMs
				}
				if tps != nil {
					latestTPS = tps
				}
				state := TimelineStateNoData
				if filledTimeline[index] != nil {
					switch *filledTimeline[index] {
					case string(TimelineStateOperational):
						state = TimelineStateOperational
					case string(TimelineStateFailed):
						state = TimelineStateFailed
					}
				}
				timeline = append(timeline, PublicStatusTimelineBucket{
					BucketStart:     bucket.bucketStart,
					BucketEnd:       time.UnixMilli(bucketStartMs.UnixMilli() + int64(intervalMinutes)*60*1000).UTC().Format(isoMilliLayout),
					State:           state,
					AvailabilityPct: availabilityPct,
					TTFTMs:          ttftMs,
					TPS:             tps,
					SampleCount:     total,
				})
			}

			var totalSuccess, totalFailure float64
			for _, bucket := range aggregateBuckets {
				totalSuccess += bucket.successCount
				totalFailure += bucket.failureCount
			}
			totalCount := totalSuccess + totalFailure
			var availabilityPct *float64
			if totalCount > 0 {
				value := toFixed2(totalSuccess / totalCount * 100)
				availabilityPct = &value
			}

			var latestKnown *modelAggregate
			for index := len(aggregateBuckets) - 1; index >= 0; index-- {
				bucket := aggregateBuckets[index]
				if bucket.successCount+bucket.failureCount > 0 {
					latestKnown = &aggregateBuckets[index]
					break
				}
			}
			var latestBucketAvailabilityPct *float64
			if latestKnown != nil {
				value := latestKnown.successCount / (latestKnown.successCount + latestKnown.failureCount) * 100
				latestBucketAvailabilityPct = &value
			}

			var latestStateRaw *string
			if latestKnown != nil && latestKnown.successCount <= 0 {
				failed := string(TimelineStateFailed)
				latestStateRaw = &failed
			} else if latestBucketAvailabilityPct != nil && *latestBucketAvailabilityPct < 50 {
				degraded := "degraded"
				latestStateRaw = &degraded
			} else {
				for index := len(filledTimeline) - 1; index >= 0; index-- {
					if filledTimeline[index] != nil {
						latestStateRaw = filledTimeline[index]
						break
					}
				}
			}

			latestState := TimelineStateNoData
			if latestStateRaw != nil {
				switch *latestStateRaw {
				case string(TimelineStateOperational):
					latestState = TimelineStateOperational
				case "degraded":
					latestState = TimelineStateDegraded
				case string(TimelineStateFailed):
					latestState = TimelineStateFailed
				}
			}

			models = append(models, PublicStatusPayloadModel{
				PublicModelKey:   model.PublicModelKey,
				Label:            model.Label,
				VendorIconKey:    model.VendorIconKey,
				RequestTypeBadge: model.RequestTypeBadge,
				LatestState:      latestState,
				AvailabilityPct:  availabilityPct,
				LatestTTFTMs:     latestTTFTMs,
				LatestTPS:        latestTPS,
				Timeline:         timeline,
			})
		}

		payloadGroups = append(payloadGroups, PublicStatusPayloadGroup{
			PublicGroupSlug: group.PublicGroupSlug,
			DisplayName:     group.DisplayName,
			ExplanatoryCopy: group.ExplanatoryCopy,
			Models:          models,
		})
	}

	return RollupAggregationResult{
		GeneratedAt: coveredTo,
		CoveredFrom: coveredFrom,
		CoveredTo:   coveredTo,
		Groups:      payloadGroups,
	}, nil
}

// toFixed2 复刻 `Number(x.toFixed(2))`（可用率用两位）。
func toFixed2(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return value
	}
	parsed, err := strconv.ParseFloat(strconv.FormatFloat(value, 'f', 2, 64), 64)
	if err != nil {
		return value
	}
	return parsed
}
