package pubstatus

import (
	"context"
	"errors"
	"strconv"
	"testing"
)

// countedBucketReader 是逐桶读的替身：记录被读的键，可指定某些键返回错误。
type countedBucketReader struct {
	values   map[string]map[string]string
	failKeys map[string]bool
	readKeys []string
}

func (r *countedBucketReader) HGetAll(_ context.Context, key string) (map[string]string, error) {
	r.readKeys = append(r.readKeys, key)
	if r.failKeys[key] {
		return nil, errors.New("redis 抖动")
	}
	return r.values[key], nil
}

// pipelinedBucketReader 同时实现 RollupBucketPipeliner，记录批量调用次数。
type pipelinedBucketReader struct {
	*countedBucketReader
	batchCalls int
	// failAt 指向「批量结果里该下标落 nil」的桶（复刻单键失败的降级口径）。
	failAt map[int]bool
}

func (r *pipelinedBucketReader) HGetAllMany(_ context.Context, keys []string) []map[string]string {
	r.batchCalls++
	out := make([]map[string]string, len(keys))
	for i, key := range keys {
		if r.failAt[i] {
			continue
		}
		out[i] = r.values[key]
	}
	return out
}

// rollupBucketsWithValues 造 n 个桶起点，并给每个桶一个可辨识字段值（1..n）。
func rollupBucketsWithValues(t *testing.T, n int, prefix string) ([]string, map[string]map[string]string) {
	t.Helper()
	starts := make([]string, 0, n)
	values := map[string]map[string]string{}
	base := "2026-01-01T00:"
	for i := 0; i < n; i++ {
		start := base + twoDigits(i*5) + ":00.000Z"
		key, err := BuildRollupKey(start, PublicStatusRollupBucketMinutes, prefix)
		if err != nil {
			t.Fatalf("构造键失败：%v", err)
		}
		starts = append(starts, start)
		values[key] = map[string]string{"1|gpt-x|requests": strconv.Itoa(i + 1)}
	}
	return starts, values
}

func twoDigits(v int) string {
	if v < 10 {
		return "0" + strconv.Itoa(v)
	}
	return strconv.Itoa(v)
}

// TestReadRollupBucketsPipelinedSingleRoundTrip 断言实现批量面的读侧只发一次批量调用，
// 且不再逐桶读——这是本改动的全部收益（默认窗口 288 个桶压成 1 次往返）。
func TestReadRollupBucketsPipelinedSingleRoundTrip(t *testing.T) {
	starts, values := rollupBucketsWithValues(t, 3, "ps")
	reader := &pipelinedBucketReader{countedBucketReader: &countedBucketReader{values: values}}

	buckets := ReadRollupBuckets(context.Background(), reader, starts, "ps")

	if reader.batchCalls != 1 {
		t.Errorf("批量调用应恰好 1 次，得到 %d", reader.batchCalls)
	}
	if len(reader.readKeys) != 0 {
		t.Errorf("走批量路径时不应再逐桶读，实际读了 %v", reader.readKeys)
	}
	if len(buckets) != len(starts) {
		t.Fatalf("桶数应为 %d，得到 %d", len(starts), len(buckets))
	}
	if got := buckets[0].Values["1|gpt-x|requests"]; got != 1 {
		t.Errorf("首桶字段应解析为 1，得到 %v", got)
	}
	if got := buckets[2].Values["1|gpt-x|requests"]; got != 3 {
		t.Errorf("末桶字段应解析为 3，得到 %v", got)
	}
}

// TestReadRollupBucketsPipelinedDegradesPerBucket 断言批量路径保留「单桶失败就地降级为空桶、
// 整体不返错」的语义：把第 2 个桶标成失败，其余两桶必须照常返回内容，且不 panic。
func TestReadRollupBucketsPipelinedDegradesPerBucket(t *testing.T) {
	starts, values := rollupBucketsWithValues(t, 3, "ps")
	reader := &pipelinedBucketReader{
		countedBucketReader: &countedBucketReader{values: values},
		failAt:              map[int]bool{1: true},
	}

	buckets := ReadRollupBuckets(context.Background(), reader, starts, "ps")

	if len(buckets) != 3 {
		t.Fatalf("失败桶也要占位，桶数应为 3，得到 %d", len(buckets))
	}
	if got := buckets[0].Values["1|gpt-x|requests"]; got != 1 {
		t.Errorf("第 1 桶内容应保留（1），得到 %v", got)
	}
	if len(buckets[1].Values) != 0 {
		t.Errorf("失败桶应降级为空桶，得到 %v", buckets[1].Values)
	}
	if got := buckets[2].Values["1|gpt-x|requests"]; got != 3 {
		t.Errorf("失败桶之后的桶仍应返回（3），得到 %v", got)
	}
}

// TestReadRollupBucketsPipelinedAllBucketsFailedStaysEmpty 断言批量结果里全是失败条目时
// 每个桶都降级为空桶且仍然占位（不因整批读失败而丢掉时间线）。
func TestReadRollupBucketsPipelinedShortResultStaysEmpty(t *testing.T) {
	starts, values := rollupBucketsWithValues(t, 3, "ps")
	reader := &pipelinedBucketReader{
		countedBucketReader: &countedBucketReader{values: values},
		failAt:              map[int]bool{0: true, 1: true, 2: true},
	}

	buckets := ReadRollupBuckets(context.Background(), reader, starts, "ps")

	if len(buckets) != 3 {
		t.Fatalf("桶数应为 3，得到 %d", len(buckets))
	}
	for i, bucket := range buckets {
		if len(bucket.Values) != 0 {
			t.Errorf("第 %d 桶应降级为空桶，得到 %v", i, bucket.Values)
		}
	}
}

// TestReadRollupBucketsPipelinedShortResultFallsBack 断言批量面返回的长度与 keys 不符时
// 落回逐桶读。
//
// 必须盖住这条：投影适配器（projectionBucketReader）在类型上总是满足批量面，但它只是把
// 调用透传给底层；底层不支持时返回 nil，若不比对长度就会把**整批桶读成空**——公开页会
// 静默丢失全部数据（本改动第一版正是在这里被 write-golden 用例抓住）。
func TestReadRollupBucketsPipelinedShortResultFallsBack(t *testing.T) {
	starts, values := rollupBucketsWithValues(t, 2, "ps")
	reader := &nilPipelineBucketReader{countedBucketReader: &countedBucketReader{values: values}}

	buckets := ReadRollupBuckets(context.Background(), reader, starts, "ps")

	if len(reader.readKeys) != 2 {
		t.Errorf("批量面不可用时应逐桶读 2 次，得到 %d", len(reader.readKeys))
	}
	if got := buckets[0].Values["1|gpt-x|requests"]; got != 1 {
		t.Errorf("回退后第 1 桶内容应保留（1），得到 %v", got)
	}
	if got := buckets[1].Values["1|gpt-x|requests"]; got != 2 {
		t.Errorf("回退后第 2 桶内容应保留（2），得到 %v", got)
	}
}

// nilPipelineBucketReader 复刻 projectionBucketReader 的形态：类型上满足批量面，
// 但底层不支持时返回 nil。
type nilPipelineBucketReader struct {
	*countedBucketReader
}

func (r *nilPipelineBucketReader) HGetAllMany(context.Context, []string) []map[string]string {
	return nil
}

// TestReadRollupBucketsFallsBackToPerBucketRead 断言不实现批量面的读侧仍走逐桶路径，
// 且单桶失败依旧降级为空桶（改前的语义不得回归）。
func TestReadRollupBucketsFallsBackToPerBucketRead(t *testing.T) {
	starts, values := rollupBucketsWithValues(t, 2, "ps")
	failingKey, err := BuildRollupKey(starts[1], PublicStatusRollupBucketMinutes, "ps")
	if err != nil {
		t.Fatalf("构造键失败：%v", err)
	}
	reader := &countedBucketReader{
		values:   values,
		failKeys: map[string]bool{failingKey: true},
	}

	buckets := ReadRollupBuckets(context.Background(), reader, starts, "ps")

	if len(reader.readKeys) != 2 {
		t.Errorf("无批量面时应逐桶读 2 次，得到 %d", len(reader.readKeys))
	}
	if len(buckets) != 2 {
		t.Fatalf("桶数应为 2，得到 %d", len(buckets))
	}
	if got := buckets[0].Values["1|gpt-x|requests"]; got != 1 {
		t.Errorf("第 1 桶内容应保留（1），得到 %v", got)
	}
	if len(buckets[1].Values) != 0 {
		t.Errorf("单桶失败应降级为空桶，得到 %v", buckets[1].Values)
	}
}
