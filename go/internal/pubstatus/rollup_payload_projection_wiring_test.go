package pubstatus

import (
	"context"
	"testing"
)

// batchingProjectionRedis 是「底层投影支持批量读」的替身：内嵌既有 fakeProjectionRedis
// 拿到 ProjectionRedis 的全部方法，只额外加上批量面。
type batchingProjectionRedis struct {
	*fakeProjectionRedis
	batchCalls int
}

func (r *batchingProjectionRedis) HGetAllMany(_ context.Context, keys []string) []map[string]string {
	r.batchCalls++
	out := make([]map[string]string, len(keys))
	for i, key := range keys {
		fields := r.hash[key]
		copied := make(map[string]string, len(fields))
		for field, value := range fields {
			copied[field] = value
		}
		out[i] = copied
	}
	return out
}

// TestReadRollupBucketsWithProjectionRedisPipelines 断言生产装配链
// （ReadRollupBucketsWithProjectionRedis → projectionBucketReader）在底层投影支持批量读时
// 真的走批量：这是本改动在线上唯一会走的路径，只测窄接口会漏掉适配器那一层——
// 「适配器在类型上满足批量面但把调用吞成 nil」正是第一版被 write-golden 抓住的形态。
func TestReadRollupBucketsWithProjectionRedisPipelines(t *testing.T) {
	starts, values := rollupBucketsWithValues(t, 2, "ps")
	redis := &batchingProjectionRedis{fakeProjectionRedis: newFakeProjectionRedis()}
	for key, fields := range values {
		for field, value := range fields {
			redis.seedHash(key+"\u0000"+field, value)
		}
	}

	buckets := ReadRollupBucketsWithProjectionRedis(context.Background(), redis, starts, "ps")

	if redis.batchCalls != 1 {
		t.Errorf("底层支持批量读时应恰好批量一次，得到 %d", redis.batchCalls)
	}
	if len(buckets) != 2 {
		t.Fatalf("桶数应为 2，得到 %d", len(buckets))
	}
	if got := buckets[0].Values["1|gpt-x|requests"]; got != 1 {
		t.Errorf("第 1 桶应读到 1，得到 %v", got)
	}
	if got := buckets[1].Values["1|gpt-x|requests"]; got != 2 {
		t.Errorf("第 2 桶应读到 2，得到 %v", got)
	}
}

// TestReadRollupBucketsWithProjectionRedisFallsBackWhenUnsupported 断言底层投影不支持
// 批量读时（既有 fakeProjectionRedis 就没有批量面），适配器如实落回逐桶读并仍返回全部内容。
func TestReadRollupBucketsWithProjectionRedisFallsBackWhenUnsupported(t *testing.T) {
	starts, values := rollupBucketsWithValues(t, 2, "ps")
	redis := newFakeProjectionRedis()
	for key, fields := range values {
		for field, value := range fields {
			redis.seedHash(key+"\u0000"+field, value)
		}
	}

	buckets := ReadRollupBucketsWithProjectionRedis(context.Background(), redis, starts, "ps")

	if len(buckets) != 2 {
		t.Fatalf("桶数应为 2，得到 %d", len(buckets))
	}
	if got := buckets[0].Values["1|gpt-x|requests"]; got != 1 {
		t.Errorf("回退路径第 1 桶应读到 1，得到 %v", got)
	}
	if got := buckets[1].Values["1|gpt-x|requests"]; got != 2 {
		t.Errorf("回退路径第 2 桶应读到 2，得到 %v", got)
	}
}
