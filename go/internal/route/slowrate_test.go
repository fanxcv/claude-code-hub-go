package route

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/redis/go-redis/v9"
)

// slowRateRedis 是最小 Redis 替身：只实现本包低速读侧用到的一个方法。
//
// 为什么不照 health_test.go 的 fakeRedis 用 HGet/Get 单命令：本读侧刻意走 Pipelined（一次往返），
// 替身只需实现它，命令分发在闭包里按 Name()/Args() 判定——这同时把「实现真的压成了一批」
// 变成可测事实（见 pipelineCalls）。
type slowRateRedis struct {
	redis.UniversalClient
	// values 存 String 型键（基线 JSON）与 Hash 型键（状态 JSON，见 slowRateStateValue）。
	values map[string]string
	// zsets 存 ZSET 型键（慢样本滑窗）。
	zsets map[string][]redis.Z
	// pipelineCalls 记录 Pipelined 被调用的次数，用于钉住「往返数不随候选数放大」。
	pipelineCalls int
	// zcountRanges 记录对滑窗发过的区间计数的 "下界|上界"，用于钉住「不再全量取回」。
	zcountRanges []string
	// readKeys 记录被读过的键，用于钉住「某个键根本没被读」（比「没调 Redis」更精确）。
	readKeys []string
}

func (f *slowRateRedis) Pipelined(
	ctx context.Context,
	fn func(redis.Pipeliner) error,
) ([]redis.Cmder, error) {
	f.pipelineCalls++
	pending := &slowRatePipeline{redis: f, ctx: ctx}
	if err := fn(pending); err != nil {
		return nil, err
	}
	// 返回值只用于「批内第一个失败命令」的错误语义；本替身把每条命令的结果写进各自 cmd，
	// 与真实库一致（调用方读的是 cmd 而不是返回的 cmder 列表）。
	return pending.cmds, pending.firstErr
}

// slowRatePipeline 把命令落到 map 上，语义对齐 Redis：键不存在即 redis.Nil。
type slowRatePipeline struct {
	redis.Pipeliner
	redis *slowRateRedis
	ctx   context.Context
	cmds  []redis.Cmder
	// firstErr 是批内第一个失败命令的错误（真实库 Pipelined 的返回语义）。
	firstErr error
}

func (p *slowRatePipeline) Get(_ context.Context, key string) *redis.StringCmd {
	p.redis.readKeys = append(p.redis.readKeys, key)
	cmd := redis.NewStringCmd(p.ctx)
	if value, ok := p.redis.values[key]; ok {
		cmd.SetVal(value)
	} else {
		cmd.SetErr(redis.Nil)
		if p.firstErr == nil {
			p.firstErr = redis.Nil
		}
	}
	p.cmds = append(p.cmds, cmd)
	return cmd
}

// HMGet 按字段取状态 Hash。语义对齐 Redis：字段不存在给 nil 元素，键不存在给全 nil，**都不报错**。
func (p *slowRatePipeline) HMGet(_ context.Context, key string, fields ...string) *redis.SliceCmd {
	p.redis.readKeys = append(p.redis.readKeys, key)
	cmd := redis.NewSliceCmd(p.ctx)
	values := make([]any, 0, len(fields))
	var decoded map[string]string
	if raw, ok := p.redis.values[key]; ok {
		_ = json.Unmarshal([]byte(raw), &decoded)
	}
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

// ZCount 按分数区间数成员。
//
// 为什么不给个「恒等于全量」的简化替身：读侧改走区间计数后，替身假设区间语义，「窗外成员
// 不计入」那条钉子就退化成恒真（假绿）。故这里真做区间判定，只支持实现实际会发的两种形态
// （闭区间整数下界 + "+inf" 上界）；发别的形态就报错，不静默放过。
func (p *slowRatePipeline) ZCount(_ context.Context, key string, min, max string) *redis.IntCmd {
	p.redis.readKeys = append(p.redis.readKeys, key)
	p.redis.zcountRanges = append(p.redis.zcountRanges, min+"|"+max)
	cmd := redis.NewIntCmd(p.ctx)
	lower, lowerErr := strconv.ParseFloat(min, 64)
	upper, upperErr := strconv.ParseFloat(max, 64)
	if lowerErr != nil || upperErr != nil {
		cmd.SetErr(errors.New("slowRatePipeline: 只支持闭区间与 ±inf，实得 " + min + "|" + max))
		p.cmds = append(p.cmds, cmd)
		return cmd
	}
	count := 0
	for _, member := range p.redis.zsets[key] {
		if member.Score < lower || member.Score > upper {
			continue
		}
		count++
	}
	cmd.SetVal(int64(count))
	p.cmds = append(p.cmds, cmd)
	return cmd
}

// slowRateFailingRedis 让每条命令都报连接错，用于钉住 fail-open。
type slowRateFailingRedis struct {
	redis.UniversalClient
}

func (f *slowRateFailingRedis) Pipelined(
	_ context.Context,
	fn func(redis.Pipeliner) error,
) ([]redis.Cmder, error) {
	failing := &slowRateFailingPipeline{}
	if err := fn(failing); err != nil {
		return nil, err
	}
	return failing.cmds, errors.New("redis: connection refused")
}

type slowRateFailingPipeline struct {
	redis.Pipeliner
	cmds []redis.Cmder
}

func (p *slowRateFailingPipeline) Get(_ context.Context, _ string) *redis.StringCmd {
	cmd := redis.NewStringCmd(context.Background())
	cmd.SetErr(errors.New("redis: connection refused"))
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *slowRateFailingPipeline) HMGet(_ context.Context, _ string, _ ...string) *redis.SliceCmd {
	cmd := redis.NewSliceCmd(context.Background())
	cmd.SetErr(errors.New("redis: connection refused"))
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *slowRateFailingPipeline) ZCount(_ context.Context, _ string, _, _ string) *redis.IntCmd {
	cmd := redis.NewIntCmd(context.Background())
	cmd.SetErr(errors.New("redis: connection refused"))
	p.cmds = append(p.cmds, cmd)
	return cmd
}

// slowRateStateValue 构造状态键的值（仅快照 penalty，**无生效参数**）。
//
// 这是旧版本写侧留下的形态：读侧参数缺失时会回退到这个快照值，故它是「回退路径」的夹具。
func slowRateStateValue(t *testing.T, penalty int) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{SlowRateStateFieldPenalty: strconv.Itoa(penalty)})
	if err != nil {
		t.Fatalf("构造状态值失败: %v", err)
	}
	return string(raw)
}

// slowRateStateValueWithParams 构造带生效参数的状态键值（当前写侧的形态）。
func slowRateStateValueWithParams(t *testing.T, penalty, windowSeconds, triggerCount, step, max int) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		SlowRateStateFieldPenalty:       strconv.Itoa(penalty),
		SlowRateStateFieldWindowSeconds: strconv.Itoa(windowSeconds),
		SlowRateStateFieldTriggerCount:  strconv.Itoa(triggerCount),
		SlowRateStateFieldPenaltyStep:   strconv.Itoa(step),
		SlowRateStateFieldPenaltyMax:    strconv.Itoa(max),
	})
	if err != nil {
		t.Fatalf("构造状态值失败: %v", err)
	}
	return string(raw)
}

// slowRateBaselineValue 构造基线值（只关心 source 字段）。
func slowRateBaselineValue(t *testing.T, source string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"median": 241.1, "samples": 120, "source": source})
	if err != nil {
		t.Fatalf("构造基线值失败: %v", err)
	}
	return string(raw)
}

// slowRateProvider 是「已开启低速监控」的供应商。
func slowRateProvider(id int64) Provider {
	p := baseProvider(id, convert.ProviderClaude)
	p.SlowRateMonitorEnabled = true
	return p
}

func newSlowRateSelector(t *testing.T, client redis.UniversalClient, providers []Provider) *Selector {
	t.Helper()
	source := &stubSource{providers: providers, byID: map[int64]Provider{}}
	for _, p := range providers {
		source.byID[p.ID] = p
	}
	return NewSelector(Options{
		Source:   source,
		SlowRate: NewSlowRateReader(SlowRateOptions{Redis: client}),
		Rand:     func() float64 { return 0 },
	})
}

// TestSlowRatePenaltyAppliesToEffectivePriority 钉住：降权确实落在有效优先级上。
//
// 反证：把 rules.go 的 `penalties.penalized(baseEffectivePriority(...))` 改成只返回
// baseEffectivePriority，本用例与下面几条一起变红。
func TestSlowRatePenaltyAppliesToEffectivePriority(t *testing.T) {
	provider := baseProvider(7, convert.ProviderClaude)
	if got := resolveEffectivePriority(provider, "", penaltyTable{7: 25}); got != 25 {
		t.Errorf("有效优先级 = %d，期望 25（0 档 + 降权 25）", got)
	}
	if got := resolveEffectivePriority(provider, "", nil); got != 0 {
		t.Errorf("无降权表时有效优先级 = %d，期望 0", got)
	}
	// 降权是单向的：负值不得把渠道抬上去。
	if got := resolveEffectivePriority(provider, "", penaltyTable{7: -50}); got != 0 {
		t.Errorf("负降权后有效优先级 = %d，期望仍是 0（不得被抬高）", got)
	}
}

// TestSlowRatePenaltyAppliesAfterGroupOverride 钉住层级：降权必须加在**分组覆盖之后**。
//
// 这是本块最重要的一条：若把降权加在覆盖之前（或直接改 provider.Priority 列），
// 有覆盖时那一列根本不参与排序，降权会被覆盖吃掉——机制看起来接好了、实际不生效。
//
// 反证：把 rules.go 的 baseEffectivePriority 换成「penalized 后再取覆盖」
// （或直接在 EffectivePriority 上叠加），本用例变红。
func TestSlowRatePenaltyAppliesAfterGroupOverride(t *testing.T) {
	provider := baseProvider(9, convert.ProviderClaude)
	// 配置值 4，但 fan 组覆盖为 0 —— 覆盖是第一真源，配置列不参与排序。
	provider.Priority = intPtr(4)
	provider.GroupTag = strPtr("fan")
	provider.GroupPriorities = map[string]int{"fan": 0}

	if got := baseEffectivePriority(provider, "fan"); got != 0 {
		t.Fatalf("覆盖后的基础分层 = %d，期望 0", got)
	}
	if got := resolveEffectivePriority(provider, "fan", penaltyTable{9: 30}); got != 30 {
		t.Errorf("带降权的有效优先级 = %d，期望 30（覆盖 0 + 降权 30）", got)
	}
	// 关键对照：若降权错误地加在配置列（4）上并绕过覆盖，结果会是 4 而非 30。
	// 断言 30 就同时排除了「加在覆盖之前且被覆盖吃掉」的 0 与「加在配置列」的 34。
	if got := resolveEffectivePriority(provider, "other", penaltyTable{9: 30}); got != 34 {
		t.Errorf("非覆盖组下 = %d，期望 34（配置 4 + 降权 30）", got)
	}
}

// TestSlowRatePenaltyShiftsTierSelection 钉住降权真能改变被选中的档位（端到端过一遍 resolve）。
func TestSlowRatePenaltyShiftsTierSelection(t *testing.T) {
	fast := slowRateProvider(1) // 档位 0
	// 档位 10 的渠道被降权 30 → 40，仍不影响 0 档被选；反过来让档位 0 的被降权才有可观察变化。
	slow := slowRateProvider(2)
	slow.Priority = intPtr(0)

	redisClient := &slowRateRedis{values: map[string]string{
		SlowRateStateKey(1, "m1"): slowRateStateValue(t, 25),
	}}
	selector := newSlowRateSelector(t, redisClient, []Provider{fast, slow})

	result, err := selector.Select(context.Background(), Request{Model: "m1"})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	// 供应商 1 被降权到 25，供应商 2 仍在 0 档，故应选中 2。
	if result.Provider == nil || result.Provider.ID != 2 {
		t.Fatalf("选中 = %v，期望 2（1 被降权 25 后落低档）", result.Provider)
	}
	if result.Context.SelectedPriority != 0 {
		t.Errorf("selectedPriority = %d，期望 0（2 未被降权）", result.Context.SelectedPriority)
	}
}

// TestSlowRateSkipsRedisWhenNotEnabled 钉住零开销：未开启监控的渠道不进 Redis。
//
// 这是设计约束而非优化——若未开启也读 Redis，等于给全站选路凭空加一次往返。
//
// 反证：把 Penalties 的 enabled 过滤去掉，pipelineCalls 变成 1，本用例变红。
func TestSlowRateSkipsRedisWhenNotEnabled(t *testing.T) {
	off := baseProvider(1, convert.ProviderClaude) // SlowRateMonitorEnabled 默认 false
	redisClient := &slowRateRedis{values: map[string]string{}}
	selector := newSlowRateSelector(t, redisClient, []Provider{off})

	if _, err := selector.Select(context.Background(), Request{Model: "m1"}); err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if redisClient.pipelineCalls != 0 {
		t.Errorf("未开启监控时 Redis 往返次数 = %d，期望 0（严格零开销）", redisClient.pipelineCalls)
	}
}

// TestSlowRateRoundTripsDoNotScaleWithCandidates 钉住「往返数不随候选数放大」。
//
// 为何是两段而不是一段：活窗计数的区间下界依赖窗长，而窗长来自第一段的状态结果——构造命令时
// 还读不到。常量两段是对的，N 段才是问题，故这里钉的是「与候选数无关」而不是「等于 1」。
func TestSlowRateRoundTripsDoNotScaleWithCandidates(t *testing.T) {
	for _, count := range []int{1, 3, 8} {
		providers := make([]Provider, 0, count)
		values := map[string]string{}
		for index := 1; index <= count; index++ {
			providers = append(providers, slowRateProvider(int64(index)))
			values[SlowRateStateKey(int64(index), "m1")] = slowRateStateValueWithParams(t, 0, 1800, 3, 10, 30)
			values[SlowRateBaselineKey(int64(index), "m1")] = slowRateBaselineValue(t, "primary")
		}
		redisClient := &slowRateRedis{values: values}
		selector := newSlowRateSelector(t, redisClient, providers)

		if _, err := selector.Select(context.Background(), Request{Model: "m1"}); err != nil {
			t.Fatalf("选路失败: %v", err)
		}
		if redisClient.pipelineCalls != 2 {
			t.Errorf("%d 个候选时 Redis 往返次数 = %d，期望 2（常量，不得随候选数放大）",
				count, redisClient.pipelineCalls)
		}
	}
}

// TestSlowRateCountsSamplesByRangeNotByFetch 钉住「不再取回滑窗全量成员」。
//
// 为何必须有它：本方法**每次选路**都走，而成员数上界是「一个 TTL 内的慢样本数」，不是常数；
// 取回全量会把载荷变成 O(成员数)。区间计数只回一个整数，故对滑窗的命令数与成员数无关。
//
// 反证：把实现的 ZCount 换成 ZCard（无区间），本钉子仍绿而「窗外不计入」那条变红——两条各钉
// 一面（载荷面 / 语义面），缺一不可。
func TestSlowRateCountsSamplesByRangeNotByFetch(t *testing.T) {
	for _, members := range []int{1, 500} {
		redisClient := &slowRateRedis{
			values: map[string]string{
				SlowRateStateKey(9, "m1"):    slowRateStateValueWithParams(t, 0, 1800, 3, 10, 30),
				SlowRateBaselineKey(9, "m1"): slowRateBaselineValue(t, "primary"),
			},
			zsets: map[string][]redis.Z{SlowRateSamplesKey(9, "m1"): slowRateSamples(members, 60_000)},
		}
		newSlowRateReaderAt(redisClient).Penalties(context.Background(), []Provider{slowRateProvider(9)}, "m1")

		if len(redisClient.zcountRanges) != 1 {
			t.Fatalf("%d 条成员时对滑窗的区间计数 = %d 次，期望 1（载荷不得随成员数增长）",
				members, len(redisClient.zcountRanges))
		}
		lower, upper, found := strings.Cut(redisClient.zcountRanges[0], "|")
		if !found || upper != "+inf" {
			t.Fatalf("区间 = %q，期望上界为 +inf", redisClient.zcountRanges[0])
		}
		if _, err := strconv.ParseInt(lower, 10, 64); err != nil {
			t.Errorf("区间下界 = %q，期望具体时刻（now-窗长）的整数而非无界", lower)
		}
	}
}

// TestSlowRatePenaltyEquivalenceWithMixedWindow 钉住迁移等价性：窗内成员与窗外成员混在一起时，
// 派生结果与「只数窗内」的口径逐条相同。
//
// 反证：把区间下界去掉（改成 ZCard 或无区间计数），每条用例的计数都会被窗外成员抬高，本用例变红。
func TestSlowRatePenaltyEquivalenceWithMixedWindow(t *testing.T) {
	for _, tc := range []struct{ live, expired, want int }{
		{0, 4, 0},
		{2, 4, 0},
		{3, 4, 10},
		{6, 4, 20},
		{9, 4, 30},
		{12, 4, 30},
	} {
		t.Run(strconv.Itoa(tc.live)+"_expired_"+strconv.Itoa(tc.expired), func(t *testing.T) {
			members := append(slowRateSamples(tc.live, 60_000), slowRateSamples(tc.expired, 1_900_000)...)
			redisClient := &slowRateRedis{
				values: map[string]string{
					SlowRateStateKey(9, "m1"):    slowRateStateValueWithParams(t, 999, 1800, 3, 10, 30),
					SlowRateBaselineKey(9, "m1"): slowRateBaselineValue(t, "primary"),
				},
				zsets: map[string][]redis.Z{SlowRateSamplesKey(9, "m1"): members},
			}
			got := newSlowRateReaderAt(redisClient).Penalties(
				context.Background(), []Provider{slowRateProvider(9)}, "m1")
			if got[9] != tc.want {
				t.Errorf("窗内 %d 条 + 窗外 %d 条 ⇒ 降权 %d，期望 %d", tc.live, tc.expired, got[9], tc.want)
			}
		})
	}
}

// TestSlowRateExtendedStaleSuppressesChannelPenalty 钉住设计稿 §3 的 A4：
// extended_stale 组合只做会话级降级，**不做渠道级降权**。
//
// 反证：删掉 Penalties 里的 source 判定，本用例变红。
func TestSlowRateExtendedStaleSuppressesChannelPenalty(t *testing.T) {
	provider := slowRateProvider(5)
	redisClient := &slowRateRedis{values: map[string]string{
		SlowRateStateKey(5, "m1"):    slowRateStateValue(t, 30),
		SlowRateBaselineKey(5, "m1"): slowRateBaselineValue(t, "extended_stale"),
	}}
	reader := NewSlowRateReader(SlowRateOptions{Redis: redisClient})

	penalties := reader.Penalties(context.Background(), []Provider{provider}, "m1")
	if _, found := penalties[5]; found {
		t.Errorf("extended_stale 组合仍拿到渠道级降权 %v，期望被抑制", penalties)
	}

	// 对照：primary 基线时降权照常生效（证明上面不是「读取本身坏了」）。
	redisClient.values[SlowRateBaselineKey(5, "m1")] = slowRateBaselineValue(t, "primary")
	penalties = reader.Penalties(context.Background(), []Provider{provider}, "m1")
	if penalties[5] != 30 {
		t.Errorf("primary 组合降权 = %d，期望 30", penalties[5])
	}
}

// slowRateTestNowMS 是新增用例的统一「当前时刻」：拨钟后滑窗区间才是确定的。
const slowRateTestNowMS = int64(1_000_000_000_000)

// newSlowRateReaderAt 造一个时钟固定在本用例基准时刻的读取器。
func newSlowRateReaderAt(client redis.UniversalClient) *SlowRateReader {
	return NewSlowRateReader(SlowRateOptions{
		Redis: client,
		Now:   func() time.Time { return time.UnixMilli(slowRateTestNowMS) },
	})
}

// slowRateSamples 造 count 条距基准时刻 offsetMS 毫秒的滑窗成员。
func slowRateSamples(count int, offsetMS int64) []redis.Z {
	members := make([]redis.Z, 0, count)
	for index := 0; index < count; index++ {
		members = append(members, redis.Z{
			Score:  float64(slowRateTestNowMS - offsetMS),
			Member: strconv.Itoa(index + 1),
		})
	}
	return members
}

// TestSlowRatePenaltyDecaysWithWindow 钉住「惩罚随样本过期连续衰减」——本次 bug 的回归钉子。
//
// 症状（生产实测）：渠道的「低速降权 +10」出现后一直不消失。根因是惩罚读的是写侧留下的状态
// 快照，而渠道恢复后不再产生慢样本、写侧也就不再触达 Redis，快照便停在最后一次推进的值上，
// 直到状态键 TTL 到期为止。设计稿「恢复」一节要求的正是「惩罚值随样本过期连续衰减」，
// 滑窗里的活成员数才是那个真值。
//
// 反证：把派生换回「只读状态快照」（即改动前的实现），本用例变红。
func TestSlowRatePenaltyDecaysWithWindow(t *testing.T) {
	provider := slowRateProvider(9)
	reader := newSlowRateReaderAt(&slowRateRedis{
		values: map[string]string{
			// 状态里留着陈旧的惩罚 30，且带齐生效参数。
			SlowRateStateKey(9, "m1"):    slowRateStateValueWithParams(t, 30, 1800, 3, 10, 30),
			SlowRateBaselineKey(9, "m1"): slowRateBaselineValue(t, "primary"),
		},
		zsets: map[string][]redis.Z{},
	})

	if got := reader.Penalties(context.Background(), []Provider{provider}, "m1"); len(got) != 0 {
		t.Fatalf("活窗内无慢样本却仍报降权 %v，期望无降权（陈旧快照必须被滑窗覆盖）", got)
	}
}

// TestSlowRatePenaltyDerivesFromLiveWindow 钉住派生口径：floor(窗内计数/阈值)*步长，封顶上限。
//
// 状态里的快照刻意写成 999：它必须被派生值完全覆盖，而不是参与任何取舍。
func TestSlowRatePenaltyDerivesFromLiveWindow(t *testing.T) {
	for _, tc := range []struct {
		count int
		want  int
	}{
		{0, 0},
		{2, 0},
		{3, 10},
		{6, 20},
		{9, 30},
		{12, 30},
	} {
		t.Run(strconv.Itoa(tc.count), func(t *testing.T) {
			reader := newSlowRateReaderAt(&slowRateRedis{
				values: map[string]string{
					SlowRateStateKey(9, "m1"):    slowRateStateValueWithParams(t, 999, 1800, 3, 10, 30),
					SlowRateBaselineKey(9, "m1"): slowRateBaselineValue(t, "primary"),
				},
				// 全部成员都在 1800s 窗内（距基准时刻 1 分钟）。
				zsets: map[string][]redis.Z{SlowRateSamplesKey(9, "m1"): slowRateSamples(tc.count, 60_000)},
			})

			got := reader.Penalties(context.Background(), []Provider{slowRateProvider(9)}, "m1")
			if got[9] != tc.want {
				t.Errorf("窗内 %d 条 ⇒ 降权 %d，期望 %d", tc.count, got[9], tc.want)
			}
		})
	}
}

// TestSlowRatePenaltyIgnoresExpiredSamples 钉住：窗外成员不计入。
//
// 为什么必须自己过滤：窗外成员只在**写侧**被 `ZREMRANGEBYSCORE` 清掉，渠道一恢复就不再有写入，
// 旧成员会一直留在 ZSET 里（TTL 是 2 倍窗长），直到整键过期。故读侧不能假设成员都是活的。
//
// 反证：把内层过滤改成「不过滤」（相当于无区间地数成员），本用例变红。
func TestSlowRatePenaltyIgnoresExpiredSamples(t *testing.T) {
	// 3 条在 1800s 窗内，3 条已过期（最早的在 1900s 之前）。
	members := append(slowRateSamples(3, 60_000), slowRateSamples(3, 1_900_000)...)
	reader := newSlowRateReaderAt(&slowRateRedis{
		values: map[string]string{
			SlowRateStateKey(9, "m1"):    slowRateStateValueWithParams(t, 30, 1800, 3, 10, 30),
			SlowRateBaselineKey(9, "m1"): slowRateBaselineValue(t, "primary"),
		},
		zsets: map[string][]redis.Z{SlowRateSamplesKey(9, "m1"): members},
	})

	got := reader.Penalties(context.Background(), []Provider{slowRateProvider(9)}, "m1")
	if got[9] != 10 {
		t.Errorf("窗内 3 条（另有 3 条已过期）⇒ 降权 %d，期望 10", got[9])
	}
}

// TestSlowRatePenaltyFallsBackWhenParamsMissing 钉住兼容：旧版本写下的状态没有生效参数，
// 此时回退读快照值——升级不该把正在生效的降权清零，等写侧下次推进自然带上参数。
func TestSlowRatePenaltyFallsBackWhenParamsMissing(t *testing.T) {
	reader := newSlowRateReaderAt(&slowRateRedis{
		values: map[string]string{
			SlowRateStateKey(9, "m1"):    slowRateStateValue(t, 30),
			SlowRateBaselineKey(9, "m1"): slowRateBaselineValue(t, "primary"),
		},
		// 滑窗里其实有 9 条，但参数缺失时一律不看窗（无从知道窗长）。
		zsets: map[string][]redis.Z{SlowRateSamplesKey(9, "m1"): slowRateSamples(9, 60_000)},
	})

	got := reader.Penalties(context.Background(), []Provider{slowRateProvider(9)}, "m1")
	if got[9] != 30 {
		t.Errorf("参数缺失时降权 = %d，期望回退到快照值 30", got[9])
	}
}

// TestSlowRateFailsOpenOnRedisError 钉住 fail-open：Redis 读失败不得阻断选路。
func TestSlowRateFailsOpenOnRedisError(t *testing.T) {
	providers := []Provider{slowRateProvider(1), slowRateProvider(2)}
	source := &stubSource{providers: providers, byID: map[int64]Provider{1: providers[0], 2: providers[1]}}
	selector := NewSelector(Options{
		Source:   source,
		SlowRate: NewSlowRateReader(SlowRateOptions{Redis: &slowRateFailingRedis{}}),
		Rand:     func() float64 { return 0 },
	})

	result, err := selector.Select(context.Background(), Request{Model: "m1"})
	if err != nil {
		t.Fatalf("Redis 故障时选路返回错误: %v（应 fail-open）", err)
	}
	if result.Provider == nil {
		t.Fatalf("Redis 故障时选出 nil 供应商（应 fail-open 继续按原优先级选）")
	}
	if result.Context.SelectedPriority != 0 {
		t.Errorf("selectedPriority = %d，期望 0（读不到即无降权）", result.Context.SelectedPriority)
	}
}

// TestSlowRateCooldownExcludesCandidate 钉住会话级冷却：命中冷却键的候选被排除。
//
// 反证：去掉 filter.go 的 slowRateRejection 调用，本用例变红。
func TestSlowRateCooldownExcludesCandidate(t *testing.T) {
	first := slowRateProvider(1)
	second := slowRateProvider(2)
	redisClient := &slowRateRedis{values: map[string]string{
		// 会话 s1 + key 7 对供应商 2 正在冷却。
		SlowRateCooldownKey("s1", 7, 2): "slow",
	}}
	selector := newSlowRateSelector(t, redisClient, []Provider{first, second})

	result, err := selector.Select(context.Background(), Request{Model: "m1", SessionID: "s1", KeyID: 7})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got := reasonOf(t, result.Context, 2); got != ReasonSlowRateCooldown {
		t.Errorf("供应商 2 的过滤理由 = %q，期望 %q", got, ReasonSlowRateCooldown)
	}
	if result.Provider == nil || result.Provider.ID != 1 {
		t.Fatalf("选中 = %v，期望 1（2 在本会话冷却中）", result.Provider)
	}
}

// TestSlowRateCooldownNeedsSessionIdentity 钉住边界：无会话身份时不判定冷却。
//
// 冷却键的形制含会话与 key，缺任一项都构不出键——此时应回落到只有渠道级降权，
// 且**不产生**任何冷却读往返。
func TestSlowRateCooldownNeedsSessionIdentity(t *testing.T) {
	provider := slowRateProvider(1)
	redisClient := &slowRateRedis{values: map[string]string{
		SlowRateCooldownKey("s1", 7, 1): "slow",
	}}
	selector := newSlowRateSelector(t, redisClient, []Provider{provider})

	result, err := selector.Select(context.Background(), Request{Model: "m1"})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 1 {
		t.Fatalf("选中 = %v，期望 1（无会话身份即不判冷却）", result.Provider)
	}
	for _, record := range result.Context.FilteredProviders {
		if record.Reason == ReasonSlowRateCooldown {
			t.Errorf("无会话身份时出现了冷却排除记录：%+v", record)
		}
	}
	// 精确断言：冷却键一次都没被读（渠道级降权那条读是允许且必要的）。
	cooldownKey := SlowRateCooldownKey("s1", 7, 1)
	for _, key := range redisClient.readKeys {
		if key == cooldownKey {
			t.Errorf("无会话身份时仍读了冷却键 %s", cooldownKey)
		}
	}
}

// TestSlowRateCooldownAlsoBlocksAffinityNomination 钉住亲和短路也要被冷却拦住。
//
// 为何单列一条：亲和命中会**短路整场选路**（resolve 直接返回提名者），过滤阶段只能把它
// 从候选集里去掉，而提名者不需要留在集里就能被选中（validateAffinityCandidate 自己重做硬校验）。
// 若只在过滤阶段判，被粘住的会话会一次次绕过冷却撞回同一家慢渠道。
//
// 反证：去掉 affinity.go 的 validateAffinityCandidate 里那段冷却判定，本用例变红。
func TestSlowRateCooldownAlsoBlocksAffinityNomination(t *testing.T) {
	provider := slowRateProvider(1)
	req := Request{
		Model:     "m1",
		Format:    convert.FormatClaude,
		KeyID:     7,
		SessionID: "s1",
	}
	selector := newSlowRateSelector(t, &slowRateRedis{values: map[string]string{}}, []Provider{provider})

	// 无冷却时提名被接受。
	if !selector.validateAffinityCandidate(context.Background(), provider, req, map[int64]bool{}) {
		t.Fatalf("无冷却时提名应被接受")
	}

	// 写冷却键后提名必须被拒。
	redisClient := &slowRateRedis{values: map[string]string{
		SlowRateCooldownKey("s1", 7, 1): "slow",
	}}
	selector = newSlowRateSelector(t, redisClient, []Provider{provider})
	if selector.validateAffinityCandidate(context.Background(), provider, req, map[int64]bool{}) {
		t.Fatalf("冷却期内提名仍被接受（会话会绕过冷却撞回同一家慢渠道）")
	}
}

// TestSlowRateDecisionChainRecordsPenalty 钉住决策链留痕：降权必须能在链上看到。
func TestSlowRateDecisionChainRecordsPenalty(t *testing.T) {
	first := slowRateProvider(1)
	second := slowRateProvider(2)
	redisClient := &slowRateRedis{values: map[string]string{
		SlowRateStateKey(1, "m1"): slowRateStateValue(t, 25),
	}}
	selector := newSlowRateSelector(t, redisClient, []Provider{first, second})

	result, err := selector.Select(context.Background(), Request{Model: "m1"})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	var recorded int
	var found bool
	for _, candidate := range result.Context.ConsideredCandidates {
		if candidate.ID == 1 {
			recorded, found = candidate.SlowPenalty, true
		}
	}
	if !found {
		t.Fatalf("候选留痕里没有供应商 1：%+v", result.Context.ConsideredCandidates)
	}
	if recorded != 25 {
		t.Errorf("链上 slowPenalty = %d，期望 25", recorded)
	}
}

// TestSlowRatePenaltyOmittedFromJSONWhenZero 钉住与黄金样本的兼容：未降权时该键**不出现**。
//
// 这是本块最容易踩的坑：decisionContext 是落链契约，chain_test.go 对键集有精确相等断言
// （黄金样本 13 键）。若不带 omitempty，既有对拍会红，而正确的出路是让 0 值不序列化。
func TestSlowRatePenaltyOmittedFromJSONWhenZero(t *testing.T) {
	encoded, err := json.Marshal(ConsideredCandidate{ID: 1, Name: "p1"})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(encoded), "slowPenalty") {
		t.Errorf("降权为 0 时 slowPenalty 不应出现，实际: %s", encoded)
	}

	encoded, err = json.Marshal(ConsideredCandidate{ID: 1, Name: "p1", SlowPenalty: 30})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(encoded), `"slowPenalty":30`) {
		t.Errorf("降权非 0 时应出现 slowPenalty，实际: %s", encoded)
	}
}

// TestSlowRateModelKeyMatchesWriteSide 钉住归一函数与写侧同源。
func TestSlowRateModelKeyMatchesWriteSide(t *testing.T) {
	if got := SlowRateModelKey(" deepseek-v4.1-flash "); got != "deepseek-v4.1-flash" {
		t.Errorf("模型键 = %q，期望去空白后的值", got)
	}
	if got := SlowRateModelKey(""); got != "" {
		t.Errorf("空模型键 = %q，期望空串（调用方据此跳过读）", got)
	}
}
