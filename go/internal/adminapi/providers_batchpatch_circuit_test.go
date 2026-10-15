package adminapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// batchCircuitRecorder 同时当熔断配置写入器与失效广播的替身，并把两类调用记进同一条序列——
// 「同步先于广播」这种次序断言只能靠共用序列看出来。全部不碰真库、真 Redis。
type batchCircuitRecorder struct {
	events   []string
	writes   []map[string]int64
	failIDs  map[int64]error
	domains  []cfgsync.Domain
	attempts []int64
}

func (r *batchCircuitRecorder) WriteProviderCircuitConfig(
	_ context.Context, providerID int64,
	failureThreshold, openDurationMS, halfOpenSuccessThreshold, releaseIncrementMS, maxOpenCount int64,
) error {
	return r.WriteProviderCircuitConfigFields(context.Background(), providerID, map[string]int64{
		providerCircuitFieldFailureThreshold:         failureThreshold,
		providerCircuitFieldOpenDuration:             openDurationMS,
		providerCircuitFieldHalfOpenSuccessThreshold: halfOpenSuccessThreshold,
		providerCircuitFieldReleaseIncrement:         releaseIncrementMS,
		providerCircuitFieldMaxOpenCount:             maxOpenCount,
	})
}

func (r *batchCircuitRecorder) WriteProviderCircuitConfigFields(
	_ context.Context, providerID int64, fields map[string]int64,
) error {
	r.attempts = append(r.attempts, providerID)
	if err, failing := r.failIDs[providerID]; failing {
		return err
	}
	r.events = append(r.events, "circuit:"+strconv.FormatInt(providerID, 10))
	r.writes = append(r.writes, fields)
	return nil
}

func (r *batchCircuitRecorder) InvalidateKeyAuth(context.Context, string) {}
func (r *batchCircuitRecorder) InvalidateUserAuth(context.Context, int64) {}
func (r *batchCircuitRecorder) InvalidateKeyCost(context.Context, int64)  {}
func (r *batchCircuitRecorder) InvalidateUserCost(context.Context, int64) {}
func (r *batchCircuitRecorder) PublishDomain(_ context.Context, domain cfgsync.Domain) {
	r.domains = append(r.domains, domain)
	r.events = append(r.events, "domain:"+string(domain))
}

// TestProviderBatchCircuitConfigFieldsOnlyChangedFields 钉住「只写改动字段」这条：
// 批量载荷只带改动的字段，未改字段在写侧没有真值——把它们写进哈希就是把库里/哈希里的值冲成默认。
func TestProviderBatchCircuitConfigFieldsOnlyChangedFields(t *testing.T) {
	got := providerBatchCircuitConfigFields(providerBatchApplyUpdates{
		"circuit_breaker_failure_threshold": int64(9),
		"circuit_breaker_open_duration":     int64(60000),
		"name":                              "改名不该进哈希",
	})
	want := map[string]int64{
		providerCircuitFieldFailureThreshold: 9,
		providerCircuitFieldOpenDuration:     60000,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("改动字段 = %v，期望 %v", got, want)
	}
	if left := providerBatchCircuitConfigFields(providerBatchApplyUpdates{"name": "只有改名"}); len(left) != 0 {
		t.Fatalf("非熔断字段不应进哈希，实得 %v", left)
	}
}

// TestProviderBatchSyncCircuitConfigsCoversEveryEffectiveProvider 钉住批量面的「改动面」：
// effectiveProviderIDs 里每个供应商都要被写一次（漏一个等于那个渠道的阈值不生效）。
func TestProviderBatchSyncCircuitConfigsCoversEveryEffectiveProvider(t *testing.T) {
	recorder := &batchCircuitRecorder{}
	fields := map[string]int64{providerCircuitFieldFailureThreshold: 9}
	providerBatchSyncCircuitConfigs(Deps{ProviderCircuitConfig: recorder}, []int64{41, 42}, fields)
	if !reflect.DeepEqual(recorder.attempts, []int64{41, 42}) {
		t.Fatalf("写入的供应商 = %v，期望 [41 42]", recorder.attempts)
	}
	for _, written := range recorder.writes {
		if !reflect.DeepEqual(written, fields) {
			t.Fatalf("写入字段 = %v，期望 %v", written, fields)
		}
	}

	// 未装配（无 Redis）：不做事、不 panic，也不给每个供应商各记一条 warn（调用方负责可观测）。
	providerBatchSyncCircuitConfigs(Deps{}, []int64{41, 42}, fields)

	// 一条失败不阻断其余供应商：失败只汇成一条 warn，已落库的请求照常作答。
	failing := &batchCircuitRecorder{failIDs: map[int64]error{41: errors.New("redis 不可达")}}
	providerBatchSyncCircuitConfigs(Deps{ProviderCircuitConfig: failing}, []int64{41, 42}, fields)
	if !reflect.DeepEqual(failing.attempts, []int64{41, 42}) {
		t.Fatalf("失败后应继续尝试其余供应商，实得 %v", failing.attempts)
	}
}

// TestProviderBatchRunPostCommitEffectsSyncsCircuitBeforeBroadcast 同时钉两件：
//  1. 熔断阈值改动必须落到哈希（此前只重发广播，数据面永远看不到新阈值）；
//  2. 同步先于广播（广播让读方以 Redis 为新真值，反序会把旧阈值再遮回去）；
//  3. 两个同域广播分支已合并成一次（发布幂等，合并不改语义，但重复发是噪音）。
func TestProviderBatchRunPostCommitEffectsSyncsCircuitBeforeBroadcast(t *testing.T) {
	recorder := &batchCircuitRecorder{}
	deps := Deps{ProviderCircuitConfig: recorder, Invalidator: recorder}
	request := httptest.NewRequest("POST", "/api/v1/providers:batchPatch:apply", nil)

	providerBatchRunPostCommitEffects(deps, request, &store.ProviderBatchApplyResult{
		EffectiveProviderIDs: []int64{11},
		PostCommitEffects:    store.ProviderBatchPostCommitEffects{CircuitBreakerChanged: true},
	}, map[string]int64{providerCircuitFieldFailureThreshold: 9})

	want := []string{"circuit:11", "domain:" + string(cfgsync.DomainProviders)}
	if !reflect.DeepEqual(recorder.events, want) {
		t.Fatalf("调用次序 = %v，期望 %v", recorder.events, want)
	}
	if len(recorder.domains) != 1 {
		t.Fatalf("同域广播应只有一次，实得 %d 次：%v", len(recorder.domains), recorder.domains)
	}

	// 非熔断字段的批量改动：不写哈希，但广播照发一次。
	other := &batchCircuitRecorder{}
	providerBatchRunPostCommitEffects(
		Deps{ProviderCircuitConfig: other, Invalidator: other}, request,
		&store.ProviderBatchApplyResult{EffectiveProviderIDs: []int64{11}},
		map[string]int64{})
	if len(other.writes) != 0 {
		t.Fatalf("非熔断改动不应写哈希，实得 %v", other.writes)
	}
	if len(other.domains) != 1 {
		t.Fatalf("广播应恰好一次，实得 %v", other.domains)
	}
}

// TestProviderBatchPostCommitEffectsFlagsChangedCircuitFields 钉住触发条件覆盖批量面里
// 所有会进哈希的字段：批量契约表与熔断哈希的交集就是这三个阈值（release_increment /
// max_open_count 只存在于单条写路径的字段表），漏一个就是「改了但不生效」。
func TestProviderBatchPostCommitEffectsFlagsChangedCircuitFields(t *testing.T) {
	for _, field := range []string{
		"circuit_breaker_failure_threshold",
		"circuit_breaker_open_duration",
		"circuit_breaker_half_open_success_threshold",
	} {
		effect := providerBatchPostCommitEffects(providerBatchApplyUpdates{field: int64(1)}, []string{field}, nil)
		if !effect.CircuitBreakerChanged {
			t.Fatalf("字段 %s 的改动未置 CircuitBreakerChanged", field)
		}
	}
	if providerBatchPostCommitEffects(providerBatchApplyUpdates{"name": "x"}, []string{"name"}, nil).CircuitBreakerChanged {
		t.Fatal("非熔断字段不应置 CircuitBreakerChanged")
	}
}
