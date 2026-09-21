package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 本文件钉住调度模拟器与真实选路在**低速降权**这一维的一致性。
//
// 缺陷背景（2026-09-21）：`route.SimulateOptions.SlowRatePenalties` 已定义但**无人填充**，
// 于是预览页显示基础档位、真实选路显示降权后档位（生产实测近 3 小时 1288 请求里 3 条链项
// 带 `slowPenalty`），而预览页正是排障时被信的那个。
//
// 三条用例分别钉：接线事实（候选与模型名真的传给了读取器）、输出真的被降权、未装配时逐字不变。

// stubSlowRatePenalties 是最小 SlowRatePenaltyReader 替身：回答一张固定表并记下入参。
type stubSlowRatePenalties struct {
	table    map[int64]int
	gotIDs   []int64
	gotModel string
	calls    int
}

func (s *stubSlowRatePenalties) Penalties(
	_ context.Context,
	candidates []route.Provider,
	requestModel string,
) map[int64]int {
	s.calls++
	s.gotIDs = s.gotIDs[:0]
	for _, candidate := range candidates {
		s.gotIDs = append(s.gotIDs, candidate.ID)
	}
	s.gotModel = requestModel
	return s.table
}

// slowRateSimulatorProvider 造一个真实形态的模拟器候选（含低速监控开关）。
func slowRateSimulatorProvider(id int64, priority int, monitorEnabled bool) route.SimulateProvider {
	groupTag := route.GroupDefault
	return route.SimulateProvider{Provider: route.Provider{
		ID:                     id,
		Name:                   fmt.Sprintf("p%d", id),
		ProviderType:           convert.ProviderClaude,
		IsEnabled:              true,
		Weight:                 1,
		Priority:               &priority,
		GroupTag:               &groupTag,
		SlowRateMonitorEnabled: monitorEnabled,
	}}
}

// effectivePriorityOf 从模拟结果的任一步快照里取某渠道的生效档位。
func effectivePriorityOf(result route.SimulateResult, providerID int64) (int, bool) {
	for _, step := range result.Steps {
		for _, snapshot := range append(append([]route.SimulateProviderSnapshot{}, step.Surviving...), step.FilteredOut...) {
			if snapshot.ID == providerID {
				return snapshot.EffectivePriority, true
			}
		}
	}
	return 0, false
}

func slowRateSimulatorOptions(providers []route.SimulateProvider) route.SimulateOptions {
	return route.SimulateOptions{
		Format:    convert.FormatClaude,
		ModelName: "m1",
		GroupTags: []string{route.GroupDefault},
		Now:       time.Now(),
		Providers: providers,
	}
}

// TestWireSimulatorDataSourcesAppliesSlowRatePenalties 钉住接线与效果：
// 读取器拿到的候选与模型名正确，且被降权的渠道在模拟结果里档位确实上移。
func TestWireSimulatorDataSourcesAppliesSlowRatePenalties(t *testing.T) {
	reader := &stubSlowRatePenalties{table: map[int64]int{2: 30}}
	api := &dashboardAPI{deps: Deps{SlowRatePenalties: reader}, logger: logx.New(nil)}

	options := slowRateSimulatorOptions([]route.SimulateProvider{
		slowRateSimulatorProvider(1, 10, true),
		slowRateSimulatorProvider(2, 10, true),
	})
	api.wireSimulatorDataSources(context.Background(), &options, nil)

	if reader.calls != 1 {
		t.Fatalf("读取器被调用 %d 次，期望 1 次", reader.calls)
	}
	if reader.gotModel != "m1" {
		t.Errorf("传给读取器的模型名 = %q，期望 m1", reader.gotModel)
	}
	if len(reader.gotIDs) != 2 || reader.gotIDs[0] != 1 || reader.gotIDs[1] != 2 {
		t.Errorf("传给读取器的候选 = %v，期望 [1 2]", reader.gotIDs)
	}
	if options.SlowRatePenalties[2] != 30 {
		t.Fatalf("降权表未接上：%v", options.SlowRatePenalties)
	}

	result, err := route.Simulate(context.Background(), options)
	if err != nil {
		t.Fatalf("模拟失败: %v", err)
	}
	demoted, ok := effectivePriorityOf(result, 2)
	if !ok {
		t.Fatal("结果里找不到渠道 2")
	}
	if demoted != 40 {
		t.Errorf("被降权渠道的生效档位 = %d，期望 40（基础 10 + 降权 30）", demoted)
	}
	kept, ok := effectivePriorityOf(result, 1)
	if !ok {
		t.Fatal("结果里找不到渠道 1")
	}
	if kept != 10 {
		t.Errorf("未降权渠道的生效档位 = %d，期望 10", kept)
	}
}

// TestWireSimulatorDataSourcesLeavesSlowRateUnwiredWhenNil 钉住未装配时的回归：
// 该维不降权，且输出与接线前逐字一致（回调留 nil，引擎自跳过）。
func TestWireSimulatorDataSourcesLeavesSlowRateUnwiredWhenNil(t *testing.T) {
	api := &dashboardAPI{deps: Deps{}, logger: logx.New(nil)}

	options := slowRateSimulatorOptions([]route.SimulateProvider{
		slowRateSimulatorProvider(1, 10, true),
		slowRateSimulatorProvider(2, 10, true),
	})
	api.wireSimulatorDataSources(context.Background(), &options, nil)

	if options.SlowRatePenalties != nil {
		t.Fatalf("未装配读取器时不该设置降权表：%v", options.SlowRatePenalties)
	}

	result, err := route.Simulate(context.Background(), options)
	if err != nil {
		t.Fatalf("模拟失败: %v", err)
	}
	for _, providerID := range []int64{1, 2} {
		got, ok := effectivePriorityOf(result, providerID)
		if !ok {
			t.Fatalf("结果里找不到渠道 %d", providerID)
		}
		if got != 10 {
			t.Errorf("渠道 %d 的生效档位 = %d，期望基础档位 10", providerID, got)
		}
	}
}

// TestWireSimulatorDataSourcesSlowRateEndToEnd 用**真实** route.SlowRateReader 走一遍
// 「模拟器 -> 读取器 -> Redis 键 -> 降权」，防止只测了接口形状而线上是哑的。
//
// 夹具穿戴真实形态：被降权渠道 `SlowRateMonitorEnabled=true`（读侧以此字段为门，
// 为假则整表为空、用例会绿而线上无声），基线 source=primary（extended 会被抑制）。
func TestWireSimulatorDataSourcesSlowRateEndToEnd(t *testing.T) {
	redisClient := &slowRateFakeRedis{values: map[string]string{
		route.SlowRateStateKey(2, route.SlowRateModelKey("m1")):    `{"penalty":"30"}`,
		route.SlowRateBaselineKey(2, route.SlowRateModelKey("m1")): `{"median":241.1,"samples":120,"source":"primary"}`,
	}}
	api := &dashboardAPI{
		deps:   Deps{SlowRatePenalties: route.NewSlowRateReader(redisClient, nil)},
		logger: logx.New(nil),
	}

	options := slowRateSimulatorOptions([]route.SimulateProvider{
		slowRateSimulatorProvider(1, 10, true),
		slowRateSimulatorProvider(2, 10, true),
	})
	api.wireSimulatorDataSources(context.Background(), &options, nil)

	if options.SlowRatePenalties[2] != 30 {
		t.Fatalf("端到端降权表 = %v，期望 {2:30}", options.SlowRatePenalties)
	}
	result, err := route.Simulate(context.Background(), options)
	if err != nil {
		t.Fatalf("模拟失败: %v", err)
	}
	demoted, ok := effectivePriorityOf(result, 2)
	if !ok {
		t.Fatal("结果里找不到渠道 2")
	}
	if demoted != 40 {
		t.Errorf("端到端被降权渠道的生效档位 = %d，期望 40", demoted)
	}

	// 未开启监控的渠道不得被读（读侧的门）：换一家关监控的同 id 渠道，表必空。
	disabled := &dashboardAPI{
		deps:   Deps{SlowRatePenalties: route.NewSlowRateReader(redisClient, nil)},
		logger: logx.New(nil),
	}
	disabledOptions := slowRateSimulatorOptions([]route.SimulateProvider{
		slowRateSimulatorProvider(2, 10, false),
	})
	disabled.wireSimulatorDataSources(context.Background(), &disabledOptions, nil)
	if len(disabledOptions.SlowRatePenalties) != 0 {
		t.Errorf("未开启监控的渠道不该拿到降权：%v", disabledOptions.SlowRatePenalties)
	}
}

// slowRateFakeRedis 是最小 Redis 替身：只实现本读侧用到的 Pipelined（一次往返）。
// 手法与 route/slowrate_test.go 的同名替身一致（嵌入接口 + 只覆写被用到的方法）。
type slowRateFakeRedis struct {
	redis.UniversalClient
	values map[string]string
}

func (f *slowRateFakeRedis) Pipelined(
	_ context.Context,
	fn func(redis.Pipeliner) error,
) ([]redis.Cmder, error) {
	pending := &slowRateFakePipeline{redis: f}
	if err := fn(pending); err != nil {
		return nil, err
	}
	return pending.cmds, nil
}

type slowRateFakePipeline struct {
	redis.Pipeliner
	redis *slowRateFakeRedis
	cmds  []redis.Cmder
}

func (p *slowRateFakePipeline) Get(_ context.Context, key string) *redis.StringCmd {
	cmd := redis.NewStringCmd(context.Background())
	if value, ok := p.redis.values[key]; ok {
		cmd.SetVal(value)
	} else {
		cmd.SetErr(redis.Nil)
	}
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *slowRateFakePipeline) HGet(_ context.Context, key, field string) *redis.StringCmd {
	cmd := redis.NewStringCmd(context.Background())
	if raw, ok := p.redis.values[key]; ok {
		var fields map[string]string
		if json.Unmarshal([]byte(raw), &fields) == nil {
			if value, found := fields[field]; found {
				cmd.SetVal(value)
				p.cmds = append(p.cmds, cmd)
				return cmd
			}
		}
	}
	cmd.SetErr(redis.Nil)
	p.cmds = append(p.cmds, cmd)
	return cmd
}
