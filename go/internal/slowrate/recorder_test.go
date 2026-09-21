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

// TestIsSlowUsesBaselineTimesRatio 钉住低速线口径：基线 × 系数（千分比）。
func TestIsSlowUsesBaselineTimesRatio(t *testing.T) {
	params := Params{RatioPerMille: 200} // 低速线 = 基线 × 0.2
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
	// 触发阈值语义（不是样本下限 100）。
	if def.MinSamples != 3 {
		t.Fatalf("触发阈值默认 %d，应为 3（设计稿 §5：窗内低速次数 >= 阈值）", def.MinSamples)
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
	params := Params{MinSamples: 3, RatioPerMille: 200}
	// 基线 100，低速线 20；样本速率 200 远高于线。
	if isSlow(200, 100, params) {
		t.Fatal("高速样本不该判为低速")
	}
}
