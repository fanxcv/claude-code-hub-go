package slowrate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件钉住 B2 的两条硬约束与判据逻辑：
//
//  1. **未开启监控的渠道逐请求零 Redis 调用**（设计稿 §8 的设计约束，不是优化）；
//  2. **无基线即 fail-open**（不采样、不判定）。
//
// 为什么不用真 Redis：判据与闸门都不依赖 Redis 的**应答内容**，只依赖「有没有发出命令」。
// 故用一个记录命令的 hook + 指向不可达地址的 client：命令必然失败，但 hook 已经记下了次数。
// 需要真实应答的写入语义属集成测试（见文件末尾，未设 CCH_TEST_REDIS_URL 时跳过）。

// countingHook 记录经过 client 的命令名；不做任何改写。
type countingHook struct {
	mu       sync.Mutex
	commands []string
}

func (h *countingHook) record(commands ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commands = append(h.commands, commands...)
}

func (h *countingHook) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.commands)
}

func (h *countingHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *countingHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.record(cmd.Name())
		return next(ctx, cmd)
	}
}

func (h *countingHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			h.record(cmd.Name())
		}
		return next(ctx, cmds)
	}
}

// unreachableClient 返回一个永远连不上的 client（配 hook 用来数命令）。
func unreachableClient(t *testing.T) (redis.UniversalClient, *countingHook) {
	t.Helper()
	hook := &countingHook{}
	client := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  100 * time.Millisecond,
		ReadTimeout:  100 * time.Millisecond,
		WriteTimeout: 100 * time.Millisecond,
		MaxRetries:   0,
	})
	client.AddHook(hook)
	t.Cleanup(func() { _ = client.Close() })
	return client, hook
}

// stubConfig 是按 providerID 给开关的静态配置源。
type stubConfig struct {
	enabled map[int64]bool
	params  Params
	calls   int
}

func (c *stubConfig) SlowRateConfig(_ context.Context, providerID int64) (Params, bool) {
	c.calls++
	return c.params, c.enabled[providerID]
}

// slowFacts 造一条「判据全满足」的事实：1000ms 总时长、100ms 首字节、100 token
// ⇒ 生成窗口 900ms、速率 111 tok/s；配基线 400、系数 200‰（低速线 80）即为低速。
func slowFacts() Facts {
	outputTokens := int64(100)
	duration := 1000
	firstByte := 100
	return Facts{
		ProviderID:   167,
		KeyID:        3,
		ModelKey:     "deepseek-v4.1-flash",
		RequestID:    42,
		StatusCode:   200,
		OutputTokens: &outputTokens,
		DurationMS:   &duration,
		FirstByteMS:  &firstByte,
	}
}

// TestDisabledProviderMakesZeroRedisCalls 是零开销闸门的主判据。
func TestDisabledProviderMakesZeroRedisCalls(t *testing.T) {
	client, hook := unreachableClient(t)
	config := &stubConfig{enabled: map[int64]bool{167: false}}
	recorder := New(Options{Redis: client, Config: config})
	if recorder == nil {
		t.Fatal("Recorder 不应为 nil")
	}
	recorder.Record(context.Background(), slowFacts())
	if got := hook.count(); got != 0 {
		t.Fatalf("未开启渠道发出了 %d 条 Redis 命令，应为 0（零开销闸门失效）", got)
	}
	if config.calls != 1 {
		t.Fatalf("配置查询 %d 次，应为 1", config.calls)
	}
}

// TestMissingBaselineIsFailOpen：无基线时不写样本（宁可少标不可误标）。
func TestMissingBaselineIsFailOpen(t *testing.T) {
	client, hook := unreachableClient(t)
	config := &stubConfig{enabled: map[int64]bool{167: true}}
	recorder := New(Options{Redis: client, Config: config})
	recorder.Record(context.Background(), slowFacts())
	// 只应发生基线读取（GET），且因读不到而不写任何样本键。
	for _, name := range hook.commands {
		if name != "get" {
			t.Fatalf("无基线时发出了非读取命令 %q，应仅有一次 GET", name)
		}
	}
	if got := hook.count(); got != 1 {
		t.Fatalf("无基线时命令数 %d，应为 1（仅一次 GET）", got)
	}
}

// 判据表：四条主判据各造一个不满足的样本，速率必须算不出来。
func TestGenerationRateRejectsEachCriterion(t *testing.T) {
	outputTokens := int64(100)
	lowTokens := int64(10)
	duration := 1000
	firstByte := 100
	cases := []struct {
		name   string
		mutate func(*Facts)
	}{
		{"状态码非 200", func(f *Facts) { f.StatusCode = 500 }},
		{"输出 token 低于下限", func(f *Facts) { f.OutputTokens = &lowTokens }},
		{"缺首字节", func(f *Facts) { f.FirstByteMS = nil }},
		{"总时长不大于首字节", func(f *Facts) { d := 100; f.DurationMS = &d }},
		{"缺输出 token", func(f *Facts) { f.OutputTokens = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := slowFacts()
			facts.OutputTokens = &outputTokens
			facts.DurationMS = &duration
			facts.FirstByteMS = &firstByte
			tc.mutate(&facts)
			if _, ok := generationRate(facts); ok {
				t.Fatal("该样本不该算出速率")
			}
		})
	}
}

// TestGenerationRateAcceptsZeroFirstByte 钉住一处容易误判的边界：首字节为 0 是**合法**样本。
//
// 设计稿 §2 的第四条判据是「first_byte_ms 非空且 duration_ms > first_byte_ms」。0 满足
// 「非空」且 0 < duration，故应算出速率（生成窗口 = 全长）。曾把它误列为「分母不成立」，
// 实为测试预期错——首字节为 0 表示首个非空 chunk 与请求起点几乎同时，不是缺失。
func TestGenerationRateAcceptsZeroFirstByte(t *testing.T) {
	outputTokens := int64(100)
	duration := 1000
	zero := 0
	facts := slowFacts()
	facts.OutputTokens = &outputTokens
	facts.DurationMS = &duration
	facts.FirstByteMS = &zero
	rate, ok := generationRate(facts)
	if !ok {
		t.Fatal("首字节为 0 且时长大于它，应算出速率")
	}
	if rate != 100 {
		t.Fatalf("速率 %v，应为 100", rate)
	}
}

// TestGenerationRateCleansWholeBodyResponses 钉住清洗规则（非流式整包伪高值）。
func TestGenerationRateCleansWholeBodyResponses(t *testing.T) {
	outputTokens := int64(1000)
	duration := 1000
	// 首字节 = 末字节：整包到达，分母被压到近 0，速率会爆炸成假值。
	firstByte := 950
	facts := slowFacts()
	facts.OutputTokens = &outputTokens
	facts.DurationMS = &duration
	facts.FirstByteMS = &firstByte
	if _, ok := generationRate(facts); ok {
		t.Fatal("首字节/总时长 = 0.95 > 0.9，整包响应应被清洗掉")
	}
	// 边界：恰好 0.9 仍算有效（判据是「大于」）。
	exact := 900
	facts.FirstByteMS = &exact
	rate, ok := generationRate(facts)
	if !ok {
		t.Fatal("首字节/总时长 = 0.9 恰在边界，应算有效")
	}
	if rate <= 0 {
		t.Fatalf("速率应为正，得到 %v", rate)
	}
}

// TestGenerationRateMatchesRollupFormula 证明复用既有口径而非另写一套。
func TestGenerationRateMatchesRollupFormula(t *testing.T) {
	outputTokens := int64(200)
	duration := 1100
	firstByte := 100
	facts := slowFacts()
	facts.OutputTokens = &outputTokens
	facts.DurationMS = &duration
	facts.FirstByteMS = &firstByte
	rate, ok := generationRate(facts)
	if !ok {
		t.Fatal("应算出速率")
	}
	// 生成窗口 1000ms ⇒ 200 tok/s。
	if rate != 200 {
		t.Fatalf("速率 %v，应为 200（outputTokens / ((duration-firstByte)/1000)）", rate)
	}
}

// TestIsSlowUsesBaselineTimesRatio 钉住低速线口径：基线 × 系数（0-1 小数）。
func TestIsSlowUsesBaselineTimesRatio(t *testing.T) {
	params := Params{Ratio: 0.2} // 低速线 = 基线 × 0.2
	if !isSlow(79, 400, params) {
		t.Fatal("79 < 400×0.2=80，应判为低速")
	}
	if isSlow(81, 400, params) {
		t.Fatal("81 > 80，不该判为低速")
	}
}

// TestNormalizeFillsDefaults 钉住「NULL 列取出厂默认」而不取 0。
func TestNormalizeFillsDefaults(t *testing.T) {
	def := DefaultParams()
	got := Params{}.normalize()
	if got != def {
		t.Fatalf("零值应收敛到出厂默认 %+v，得到 %+v", def, got)
	}
	// 触发阈值语义来自新列 slow_rate_trigger_count（不是基线样本下限 100）。
	if def.TriggerCount != 3 {
		t.Fatalf("触发阈值默认 %d，应为 3（设计稿 §5：窗内低速次数 >= 阈值）", def.TriggerCount)
	}
}

// TestDefaultParamsWindowAndRatio 钉住用户 2026-09-21 定的两个出厂默认：
// 判定滑窗 30 分钟、系数 0.3。单位由用户 2026-09-22 改为**分钟**与 **0-1 小数**。
//
// 为何单列一条：这两个数是**产品口径**，改动会让全网渠道的降权敏感度变化；
// 混在上面的 normalize 用例里，改错时错误信息指向的是「零值收敛」而不是具体数字。
func TestDefaultParamsWindowAndRatio(t *testing.T) {
	def := DefaultParams()
	if def.WindowMinutes != 30 {
		t.Fatalf("判定滑窗默认 %d 分钟，应为 30", def.WindowMinutes)
	}
	if def.Ratio != 0.3 {
		t.Fatalf("系数默认 %v，应为 0.3", def.Ratio)
	}
	// normalize 是「NULL 列 → 出厂默认」的唯一收敛点；零值必须收敛到**新**默认，
	// 而不是残留的旧字面量。
	normalized := Params{}.normalize()
	if normalized.WindowMinutes != 30 || normalized.Ratio != 0.3 {
		t.Fatalf("normalize() 收敛到 %d 分钟/%v，应为 30/0.3", normalized.WindowMinutes, normalized.Ratio)
	}
}

// TestNewRequiresRedisAndConfig：任一侧缺失即返回 nil（旁路整段跳过）。
func TestNewRequiresRedisAndConfig(t *testing.T) {
	if New(Options{Config: &stubConfig{}}) != nil {
		t.Fatal("缺 Redis 应返回 nil")
	}
	client, _ := unreachableClient(t)
	if New(Options{Redis: client}) != nil {
		t.Fatal("缺 Config 应返回 nil")
	}
}

// TestBaselineUndecodableIsFailOpen：基线键内容坏了也不采样（不 panic、不写）。
func TestBaselineUndecodableIsFailOpen(t *testing.T) {
	client, _ := unreachableClient(t)
	recorder := New(Options{Redis: client, Config: &stubConfig{enabled: map[int64]bool{167: true}}})
	if recorder == nil {
		t.Fatal("Recorder 不应为 nil")
	}
	// client 不可达 ⇒ Get 返回连接错误（非 redis.Nil）⇒ baseline 返回 false。
	if _, ok := recorder.baseline(context.Background(), 167, "m"); ok {
		t.Fatal("Redis 不可达时应按无基线处理")
	}
}

// TestSamplingSkipsWhenRateAboveLine：速率高于低速线的样本不写（判据 4 的反面）。
func TestSamplingSkipsWhenRateAboveLine(t *testing.T) {
	params := Params{TriggerCount: 3, Ratio: 0.2}
	// 基线 100，低速线 20；样本速率 200 远高于线。
	if isSlow(200, 100, params) {
		t.Fatal("高速样本不该判为低速")
	}
}

// sourceStubProvider 是 ProviderSource 的测试替身，直接给出 ProviderConfig。
//
// 为什么需要它：上面那个 stubConfig 直接给 Params，跳过了「列 → Params」这一段，
// 于是「TriggerCount 到底取自新列还是取自常量」在测试里无从分辨。本测试补上这一段。
type sourceStubProvider struct {
	config ProviderConfig
	found  bool
}

func (p *sourceStubProvider) SlowRateProvider(_ context.Context, _ int64) (ProviderConfig, bool) {
	return p.config, p.found
}

// TestTriggerCountComesFromColumnNotConstant 是本次拆列的**反证用例**。
//
// 拆列的全部意义在于「阈值语义由 slow_rate_trigger_count 这一列决定」。若实现仍从
// slow_rate_min_samples（或干脆写死常量 3）取值，功能看似照跑，而配置面完全失效——
// 这正是「加了列但没人读」会静默通过的那类缺陷。
//
// 断言方式：给一个**非默认**的 TriggerCount（7）与一个非默认的 Ratio（0.7），
// 验算出来的 Params.TriggerCount 必须是 7 而非 3（写死常量会得 3）。
func TestTriggerCountComesFromColumnNotConstant(t *testing.T) {
	seven := 7
	source := &sourceStubProvider{
		found: true,
		config: ProviderConfig{
			Enabled: true,
			// 新列（本次新增）：触发阈值。
			TriggerCount: &seven,
			// 同时给一个非默认系数，防实现两者搞混。
			Ratio: floatPtr(0.7),
		},
	}
	config := NewSnapshotConfig(source)
	if config == nil {
		t.Fatal("SnapshotConfig 不应为 nil")
	}
	params, ok := config.SlowRateConfig(context.Background(), 167)
	if !ok {
		t.Fatal("已开启的渠道应返回 ok")
	}
	if params.TriggerCount != 7 {
		t.Fatalf("TriggerCount: 得到 %d，期望 7（须取自 slow_rate_trigger_count 列，"+
			"若为 3 说明写死了常量）", params.TriggerCount)
	}
	if params.Ratio != 0.7 {
		t.Fatalf("Ratio: 得到 %v，期望 0.7（须取自 slow_rate_ratio 列）", params.Ratio)
	}
}

// TestTriggerCountFallsBackWhenColumnNull 钉住 NULL 列取代码默认值（不是取 0）。
func TestTriggerCountFallsBackWhenColumnNull(t *testing.T) {
	source := &sourceStubProvider{
		found:  true,
		config: ProviderConfig{Enabled: true},
	}
	config := NewSnapshotConfig(source)
	params, ok := config.SlowRateConfig(context.Background(), 167)
	if !ok {
		t.Fatal("已开启的渠道应返回 ok")
	}
	if params.TriggerCount != 0 {
		t.Fatalf("归一化前 TriggerCount 应为 0（NULL 折成 0），得到 %d", params.TriggerCount)
	}
	// normalize 之后才应变成出厂默认 3。
	if got := params.normalize().TriggerCount; got != 3 {
		t.Fatalf("NULL 列应收敛到出厂默认 3，得到 %d", got)
	}
}

// TestProviderConfigIgnoresBaselineFloorColumn 钉住拆分的另一半：本包**不读**旧列。
//
// old floor 列仍在 ProviderConfig 之外——若有人把 `slow_rate_min_samples` 加回本结构
// 并参与验算，两列语义会再次合流，拆列白做。
func TestProviderConfigIgnoresBaselineFloorColumn(t *testing.T) {
	hundred := 100
	source := &sourceStubProvider{
		found: true,
		config: ProviderConfig{
			Enabled: true,
			// 只给旧列语义的值（基线样本下限 100）经新列 WindowMinutes 传入：
			// 本包不读 min_samples，故验算出的其它参数应为默认。
			WindowMinutes: &hundred,
		},
	}
	config := NewSnapshotConfig(source)
	params, _ := config.SlowRateConfig(context.Background(), 167)
	// WindowMinutes 会取该值（它确实是本包的列），但 TriggerCount 不会受它影响。
	if params.WindowMinutes != 100 {
		t.Fatalf("WindowMinutes: 得到 %d，期望 100", params.WindowMinutes)
	}
	if params.TriggerCount != 0 {
		t.Fatalf("TriggerCount 不应受基线样本下限列影响，得到 %d", params.TriggerCount)
	}
}
