package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住 `/providers/health` 的**低速降权投影**：界面要能看出「这家渠道当前是否被降级、
// 降了多少」。它接的是已有的熔断健康管道，不新增端点。
//
// 三层钉子：
//   - 读面聚合口径（`redisProviderSlowRates`）：per-渠道 vs 键的「渠道 × 模型」形制的落差；
//   - 投影三态（`providerSlowRateProjection`）：**读不到 ≠ 无降权 ≠ 0**；
//   - 端到端 handler 形状（缺 CCH_TEST_DSN 时跳过，与同包既有真库用例同纪律）。

// slowRateHealthFakeRedis 是最小 Redis 替身：只实现本读面用到的 Scan（枚举状态键族）
// 与 Pipelined（`route.SlowRateReader.Penalties` 一次往返里读 state + baseline）。
//
// 手法与同包 dashboard_simulator_slowrate_test.go 的替身一致（嵌入接口 + 只覆写被用到的方法）。
type slowRateHealthFakeRedis struct {
	redis.UniversalClient
	values map[string]string
	// scanErr 令 Scan 失败，用于造「读面在、但读不到」这一态。
	scanErr error
}

// Scan 按「前缀 + * + 后缀」匹配（本读面只用这一种形态，故不实现完整 glob）。
func (f *slowRateHealthFakeRedis) Scan(
	_ context.Context, _ uint64, match string, _ int64,
) *redis.ScanCmd {
	cmd := redis.NewScanCmd(context.Background(), nil)
	if f.scanErr != nil {
		cmd.SetErr(f.scanErr)
		return cmd
	}
	keys := make([]string, 0, len(f.values))
	for key := range f.values {
		if matched, ok := matchSlowRatePattern(match, key); ok && matched {
			keys = append(keys, key)
		}
	}
	cmd.SetVal(keys, 0)
	return cmd
}

func (f *slowRateHealthFakeRedis) Pipelined(
	_ context.Context,
	fn func(redis.Pipeliner) error,
) ([]redis.Cmder, error) {
	pending := &slowRateHealthFakePipeline{redis: f}
	if err := fn(pending); err != nil {
		return nil, err
	}
	return pending.cmds, nil
}

type slowRateHealthFakePipeline struct {
	redis.Pipeliner
	redis *slowRateHealthFakeRedis
	cmds  []redis.Cmder
}

func (p *slowRateHealthFakePipeline) Get(_ context.Context, key string) *redis.StringCmd {
	cmd := redis.NewStringCmd(context.Background())
	if value, ok := p.redis.values[key]; ok {
		cmd.SetVal(value)
	} else {
		cmd.SetErr(redis.Nil)
	}
	p.cmds = append(p.cmds, cmd)
	return cmd
}

// HMGet 按字段取状态 Hash。语义对齐 Redis：字段不存在给 nil 元素、键不存在给全 nil，均不报错。
func (p *slowRateHealthFakePipeline) HMGet(_ context.Context, key string, fields ...string) *redis.SliceCmd {
	cmd := redis.NewSliceCmd(context.Background())
	var decoded map[string]string
	if raw, ok := p.redis.values[key]; ok {
		_ = json.Unmarshal([]byte(raw), &decoded)
	}
	values := make([]any, 0, len(fields))
	for _, field := range fields {
		if value, found := decoded[field]; found {
			values = append(values, value)
			continue
		}
		values = append(values, nil)
	}
	cmd.SetVal(values)
	p.cmds = append(p.cmds, cmd)
	return cmd
}

// ZRangeWithScores 取滑窗成员。本文件的夹具不造滑窗（状态里也没有生效参数），
// 故一律空集——读侧因此走「参数缺失则回退快照」那条路，正是这些用例要钉的行为。
func (p *slowRateHealthFakePipeline) ZRangeWithScores(_ context.Context, _ string, _, _ int64) *redis.ZSliceCmd {
	cmd := redis.NewZSliceCmd(context.Background())
	cmd.SetVal([]redis.Z{})
	p.cmds = append(p.cmds, cmd)
	return cmd
}

// matchSlowRatePattern 支持「前缀*后缀」（前缀或后缀可空）。
func matchSlowRatePattern(pattern, key string) (bool, bool) {
	star := -1
	for index := 0; index < len(pattern); index++ {
		if pattern[index] == '*' {
			star = index
			break
		}
	}
	if star < 0 {
		return pattern == key, true
	}
	prefix, suffix := pattern[:star], pattern[star+1:]
	if len(key) < len(prefix)+len(suffix) {
		return false, true
	}
	return key[:len(prefix)] == prefix && key[len(key)-len(suffix):] == suffix, true
}

// writeSlowRateState 按**旧版写侧的形态**造夹具：状态键用 route.SlowRateStateKey 构造，
// 哈希只带快照字段（不含 windowSeconds 一族生效参数）；基线键用 route.SlowRateBaselineKey，
// 值是 jobs 冻结的 JSON 形状。
//
// 只带快照＝读侧判成「参数缺失」而回退读快照值——本文件几条用例钉的正是这条回退路径
// （界面与选路同口径、extended_stale 抑制）。带参数那条主路径在 internal/route 的用例里钉。
func writeSlowRateState(
	values map[string]string,
	providerID int64,
	modelKey string,
	penalty int,
	baselineSource string,
) {
	state, _ := json.Marshal(map[string]string{
		"penalty":   strconv.Itoa(penalty),
		"slowCount": "3",
		"enteredAt": "1700000000000",
	})
	values[route.SlowRateStateKey(providerID, modelKey)] = string(state)
	baseline, _ := json.Marshal(map[string]any{
		"median": 200.0, "samples": 120, "computedAt": 1700000000000,
		"source": baselineSource, "slowLine": 40.0,
	})
	values[route.SlowRateBaselineKey(providerID, modelKey)] = string(baseline)
}

// TestProviderSlowRatesAggregatesPerProvider 钉住聚合口径：渠道是 per-provider 展示，
// 而低速状态按「渠道 × 模型」存，一个渠道可有多个组合。
//
// 断言的四件事：
//   - 多组合取**最大** penalty（降权是排序位移量，多个组合在选路里各自竞争、不叠加）；
//   - ModelKey 与 Combinations 跟着那个最大值走（界面要如实说「还有 N 个模型」）；
//   - 不在可见集合里的渠道不出现（本端点只报可见供应商）；
//   - extended_stale 基线只做会话级降级、不做渠道级 penalty——它**不得**被报成降权
//     （判定复用选路那一份 `Penalties`，故这里钉的就是「界面与选路同口径」）。
func TestProviderSlowRatesAggregatesPerProvider(t *testing.T) {
	const (
		heavy   = int64(101) // 两个组合，其中一个降权更重
		light   = int64(102) // 一个组合
		stale   = int64(103) // 只有 extended_stale 基线
		staleOK = "extended_stale"
	)
	values := map[string]string{}
	writeSlowRateState(values, heavy, "model-a", 10, "primary")
	writeSlowRateState(values, heavy, "global:model-b", 30, "primary")
	writeSlowRateState(values, light, "model-a", 20, "primary")
	writeSlowRateState(values, stale, "model-a", 50, staleOK)
	// 一个不在可见集合里的渠道：它不该出现在结果里。
	writeSlowRateState(values, 999, "model-a", 99, "primary")

	client := &slowRateHealthFakeRedis{values: values}
	reader := NewRedisProviderSlowRates(client, route.NewSlowRateReader(route.SlowRateOptions{Redis: client, Logger: logx.New(nil)}), logx.New(nil))
	if reader == nil {
		t.Fatal("装配读面失败：应返回实现而不是 nil")
	}

	got, err := reader.ProviderSlowRates(context.Background(), []route.Provider{{ID: heavy}, {ID: light}, {ID: stale}})
	if err != nil {
		t.Fatalf("读低速降权表失败: %v", err)
	}

	heavyReading, exists := got[heavy]
	if !exists {
		t.Fatalf("降权最重的渠道应在结果里，实际：%v", got)
	}
	if heavyReading.Penalty != 30 {
		t.Errorf("多组合应取最大 penalty=30，收到 %d", heavyReading.Penalty)
	}
	if heavyReading.ModelKey != "global:model-b" {
		t.Errorf("ModelKey 应跟随最大 penalty 的组合（含冒号的模型键要能正确反解），收到 %q",
			heavyReading.ModelKey)
	}
	if heavyReading.Combinations != 2 {
		t.Errorf("生效组合数应为 2，收到 %d", heavyReading.Combinations)
	}

	if lightReading := got[light]; lightReading.Penalty != 20 || lightReading.Combinations != 1 {
		t.Errorf("单组合渠道应为 penalty=20 / combinations=1，收到 %+v", lightReading)
	}
	if _, exists := got[stale]; exists {
		t.Error("extended_stale 基线不得报成渠道级降权（它只做会话级降级）")
	}
	if _, exists := got[999]; exists {
		t.Error("不在可见集合里的渠道不该出现")
	}
}

// TestProviderSlowRateProjectionKeepsNoDemotionApart 钉住投影的三态区分：
// 「未装配」「读不到」「无降权」必须三件事，且**读不到不得用 0 冒充**。
//
// 为什么这条最要紧：0 是「无降权」的合法取值，若读不到也回 0，界面就无法把
// 「Redis 挂了」与「一切正常」分开——同族端点 circuitLogsState 正是为这条立了 Available。
func TestProviderSlowRateProjectionKeepsNoDemotionApart(t *testing.T) {
	if got := providerSlowRateProjection(false, false, providerSlowRateSnapshot{}); got != nil {
		t.Errorf("未装配时应为 nil（键缺失，前端整段不显示），收到 %+v", got)
	}

	unreadable := providerSlowRateProjection(true, true, providerSlowRateSnapshot{})
	if unreadable == nil || unreadable.Available {
		t.Fatalf("读不到时 available 应为 false，收到 %+v", unreadable)
	}
	if unreadable.Penalty != nil {
		t.Errorf("读不到时 penalty 必须为 null，不得用 0 冒充，收到 %d", *unreadable.Penalty)
	}
	if unreadable.UnavailableReason == nil || *unreadable.UnavailableReason == "" {
		t.Error("读不到时应给出 unavailableReason")
	}

	healthy := providerSlowRateProjection(true, false, providerSlowRateSnapshot{})
	if healthy == nil || !healthy.Available {
		t.Fatalf("读到了但无降权时 available 应为 true，收到 %+v", healthy)
	}
	if healthy.Penalty != nil || healthy.ModelKey != nil {
		t.Errorf("无降权时 penalty/modelKey 应为 null，收到 %+v", healthy)
	}

	demoted := providerSlowRateProjection(true, false,
		providerSlowRateSnapshot{Penalty: 30, ModelKey: "model-a", Combinations: 2})
	if demoted.Penalty == nil || *demoted.Penalty != 30 {
		t.Fatalf("有降权时应带读数，收到 %+v", demoted)
	}
	if demoted.ModelKey == nil || *demoted.ModelKey != "model-a" || demoted.Combinations != 2 {
		t.Errorf("有降权时应带模型键与组合数，收到 %+v", demoted)
	}
}

// TestProviderSlowRatesReportsReadFailure 钉住读面把「扫不动」如实报成错误，
// 而不是回一张空表——空表在调用方那里与「全渠道无降权」同义。
func TestProviderSlowRatesReportsReadFailure(t *testing.T) {
	client := &slowRateHealthFakeRedis{
		values:  map[string]string{},
		scanErr: errors.New("redis down"),
	}
	reader := NewRedisProviderSlowRates(client, route.NewSlowRateReader(route.SlowRateOptions{Redis: client}), nil)
	if _, err := reader.ProviderSlowRates(context.Background(), []route.Provider{{ID: 1}}); err == nil {
		t.Fatal("扫描失败时应返回错误，不得静默回空表")
	}
}

// TestProvidersHealthCarriesSlowRate 是端到端 handler 用例：真路由表 + 真 PG（可见性取数）
// + 假 Redis（低速键）。缺 CCH_TEST_DSN 时跳过，与同包既有真库用例同纪律。
//
// 断言三件事：有降权的渠道带读数、无键的渠道是「已读但无降权」、未装配读面时该字段为 null。
func TestProvidersHealthCarriesSlowRate(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	ctx := context.Background()

	demotedID, healthyID := fixture.enabledID, fixture.otherID
	// 夹具按写侧形态造：只有 demoted 渠有状态键与基线键。
	values := map[string]string{}
	writeSlowRateState(values, demotedID, "model-a", 40, "primary")
	client := &slowRateHealthFakeRedis{values: values}

	deps := Deps{
		Logger:   logx.New(nil),
		Guard:    principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Store:    pools,
		Problems: NewProblems(nil),
		// 熔断面与低速面同一次装配里给（本文件的关注点是后者）。
		CircuitStates:     NewRedisCircuitStates(client, nil, nil),
		ProviderSlowRates: NewRedisProviderSlowRates(client, route.NewSlowRateReader(route.SlowRateOptions{Redis: client}), nil),
	}
	router := New(Options{Deps: deps})
	RegisterProviders(router, deps)

	status, body := circuitCall(t, router, http.MethodGet, "/providers/health", "", true)
	if status != http.StatusOK {
		t.Fatalf("health 应为 200，收到 %d：%.300s", status, body)
	}
	var snapshot map[string]struct {
		SlowRate *providerSlowRateHealth `json:"slowRate"`
	}
	if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
		t.Fatalf("解析 health 响应失败: %v（原文 %.300s）", err, body)
	}

	demoted := snapshot[strconv.FormatInt(demotedID, 10)].SlowRate
	if demoted == nil || !demoted.Available || demoted.Penalty == nil || *demoted.Penalty != 40 {
		t.Fatalf("有降权的渠道应带读数，收到 %+v", demoted)
	}
	if demoted.ModelKey == nil || *demoted.ModelKey != "model-a" {
		t.Errorf("应带降权最重的模型键，收到 %+v", demoted)
	}

	healthy := snapshot[strconv.FormatInt(healthyID, 10)].SlowRate
	if healthy == nil || !healthy.Available {
		t.Fatalf("无键渠道应是「已读但无降权」，收到 %+v", healthy)
	}
	if healthy.Penalty != nil {
		t.Errorf("无降权时 penalty 应为 null，收到 %d", *healthy.Penalty)
	}

	// 未装配读面：整个 slowRate 键为 null（不是零值对象的「available=true / 无降权」）。
	unwired := Deps{
		Logger:        logx.New(nil),
		Guard:         principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Store:         pools,
		Problems:      NewProblems(nil),
		CircuitStates: NewRedisCircuitStates(client, nil, nil),
	}
	unwiredRouter := New(Options{Deps: unwired})
	RegisterProviders(unwiredRouter, unwired)
	status, body = circuitCall(t, unwiredRouter, http.MethodGet, "/providers/health", "", true)
	if status != http.StatusOK {
		t.Fatalf("未装配时 health 仍应 200，收到 %d：%.300s", status, body)
	}
	var unwiredSnapshot map[string]struct {
		SlowRate *providerSlowRateHealth `json:"slowRate"`
	}
	if err := json.Unmarshal([]byte(body), &unwiredSnapshot); err != nil {
		t.Fatalf("解析未装配响应失败: %v", err)
	}
	if entry := unwiredSnapshot[strconv.FormatInt(demotedID, 10)].SlowRate; entry != nil {
		t.Errorf("未装配读面时 slowRate 应为 null，收到 %+v", entry)
	}
	_ = ctx
}

// TestSlowRateCandidatesCarryRowParams 钉住管理面候选带上渠道行的**实时四参数**。
//
// 为什么这条必须钉：读侧按候选行上的参数算滑窗下界与档位。候选若只带 id，读侧会回退到状态里
// 记录的旧参数，管理面因此滞后到下一次慢样本；而数据面传的是完整行、当场生效——同一时刻两处
// 会显示不同的降权。这是一条**接线钉子**（本仓既有教训：定义了不等于接上了）。
func TestSlowRateCandidatesCarryRowParams(t *testing.T) {
	window, trigger, step, max := 5, 3, 10, 30
	candidates := slowRateCandidates([]store.AdminProvider{{
		ID:                     7,
		SlowRateMonitorEnabled: false,
		SlowRateWindowMinutes:  &window,
		SlowRateTriggerCount:   &trigger,
		SlowRatePenaltyStep:    &step,
		SlowRatePenaltyMax:     &max,
	}})
	if len(candidates) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(candidates))
	}
	got := candidates[0]
	if got.ID != 7 {
		t.Errorf("ID = %d，期望 7", got.ID)
	}
	if !got.SlowRateMonitorEnabled {
		t.Error("开关应按已开启报：状态键只由已开启监控的渠道写出，按原值过滤会滤掉真实存在降权的渠道")
	}
	for name, pair := range map[string]struct {
		got  *int
		want int
	}{
		"WindowMinutes": {got.SlowRateWindowMinutes, window},
		"TriggerCount":  {got.SlowRateTriggerCount, trigger},
		"PenaltyStep":   {got.SlowRatePenaltyStep, step},
		"PenaltyMax":    {got.SlowRatePenaltyMax, max},
	} {
		if pair.got == nil {
			t.Errorf("%s 丢了（应为 %d）：读侧据它算窗长与档位，丢了就退回状态里的旧参数", name, pair.want)
			continue
		}
		if *pair.got != pair.want {
			t.Errorf("%s = %d，期望 %d", name, *pair.got, pair.want)
		}
	}
}
