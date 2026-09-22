package slowrate

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/slowlog"
)

// 本文件钉住恢复策略（用户 2026-09-22 需求）：连续 N 个**可判定样本**都不慢，即重置该组合的
// 降权。
//
// 为什么用真 Redis 而不是命令计数 hook：本功能的判据全部落在**命令的效果**上——计数有没有
// 攒够、达阈值后三个键有没有被删、慢样本有没有把计数打回零。用一个只数命令名的 hook 既看不出
// INCR 的返回值，也看不出 DEL 的目标，钉子会退化成「发了 INCR 就算过」——那正是「测试绿但功能
// 没接」的形态。故与 internal/route 的 integration_test.go 同型：未设 CCH_TEST_REDIS_URL 时整组
// 跳过（CI 即跳过）。
//
// 另有一条**不需要 Redis** 的判据（不产样本的请求不计入）单独用不可达 client + 命令 hook 钉，
// 见文件末尾——它的判据是「一条命令都不该发」，用真库反而看不出来。

const testRedisEnv = "CCH_TEST_REDIS_URL"

// recoveryRedis 读门控变量建 Redis 客户端；未设置时跳过。
// 库号固定落在 13：URL 自带库号时以其为准，否则显式切到 13，避免碰生产键空间。
func recoveryRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv(testRedisEnv)
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client
}

// recoveryHarness 是恢复策略用例的夹具：真 Redis + 固定基线 + 可拨钟 + 可指定 N。
//
// providerID 取远离生产区间的值（键形制含 providerID，避免与真实键撞名）。
type recoveryHarness struct {
	t        *testing.T
	client   redis.UniversalClient
	recorder *Recorder
	provider int64
	model    string
	now      time.Time
	params   Params
}

func newRecoveryHarness(t *testing.T, providerID int64, recoveryRequests int) *recoveryHarness {
	t.Helper()
	client := recoveryRedis(t)
	model := "recovery-probe-model"
	h := &recoveryHarness{
		t:        t,
		client:   client,
		provider: providerID,
		model:    model,
		now:      time.UnixMilli(1790046000000),
		params: Params{
			WindowMinutes:    10,
			TriggerCount:     3,
			Ratio:            0.3,
			PenaltyStep:      10,
			PenaltyMax:       50,
			CooldownSeconds:  60,
			RecoveryRequests: recoveryRequests,
		},
	}
	// 基线 400 ⇒ 低速线 = 400 × 0.3 = 120；slowFacts 的速率是 111（低于线）⇒ 判为慢。
	payload, err := json.Marshal(map[string]any{"median": 400.0, "samples": 1000})
	if err != nil {
		t.Fatalf("编码基线失败: %v", err)
	}
	h.client.Set(context.Background(), baselineKey(providerID, model), string(payload), time.Hour)
	h.recorder = New(Options{
		Redis:  client,
		Config: &stubConfig{enabled: map[int64]bool{providerID: true}, params: h.params},
		Now:    func() time.Time { return h.now },
	})
	if h.recorder == nil {
		t.Fatal("Recorder 不应为 nil")
	}
	t.Cleanup(func() {
		ctx := context.Background()
		h.client.Del(ctx, samplesKey(providerID, model), stateKey(providerID, model),
			cleanStreakKey(providerID, model), baselineKey(providerID, model))
	})
	return h
}

func (h *recoveryHarness) keys() (samples, state, streak string) {
	return samplesKey(h.provider, h.model), stateKey(h.provider, h.model), cleanStreakKey(h.provider, h.model)
}

// fastFacts 造一条**不慢**的可判定样本：100 token / 1000ms 总时长 / 100ms 首字节
// ⇒ 速率 111 tok/s，低于基线 400 × 0.3 = 120 的线——**这仍是慢**。
//
// 故要造干净样本必须抬高速率：见 cleanFacts。
func (h *recoveryHarness) cleanFacts(requestID int64) Facts {
	outputTokens := int64(4000)
	duration := 1000
	firstByte := 100
	return Facts{
		ProviderID:   h.provider,
		KeyID:        3,
		ModelKey:     h.model,
		RequestID:    requestID,
		StatusCode:   200,
		OutputTokens: &outputTokens,
		DurationMS:   &duration,
		FirstByteMS:  &firstByte,
	}
}

// slowFactsFor 造一条**慢**的可判定样本：速率 111 < 线 120。
func (h *recoveryHarness) slowFactsFor(requestID int64) Facts {
	facts := h.cleanFacts(requestID)
	tokens := int64(100)
	facts.OutputTokens = &tokens
	return facts
}

// countSamples 返回 samples 滑窗的活成员数（0 表示键不存在或被清空）。
func (h *recoveryHarness) countSamples() int64 {
	h.t.Helper()
	n, err := h.client.ZCard(context.Background(), samplesKey(h.provider, h.model)).Result()
	if err == redis.Nil {
		return 0
	}
	if err != nil {
		h.t.Fatalf("读 samples 失败: %v", err)
	}
	return n
}

func (h *recoveryHarness) exists(key string) bool {
	h.t.Helper()
	n, err := h.client.Exists(context.Background(), key).Result()
	if err != nil {
		h.t.Fatalf("读 %s 失败: %v", key, err)
	}
	return n > 0
}

// streakValue 返回连续干净计数；键不存在时为 0。
func (h *recoveryHarness) streakValue() int64 {
	h.t.Helper()
	value, err := h.client.Get(context.Background(), cleanStreakKey(h.provider, h.model)).Int64()
	if err == redis.Nil {
		return 0
	}
	if err != nil {
		h.t.Fatalf("读 streak 失败: %v", err)
	}
	return value
}

// seedPenalty 先把该组合推入降权状态（3 条慢样本 ⇒ 达 trigger 3 ⇒ state 键产生）。
func (h *recoveryHarness) seedPenalty() {
	h.t.Helper()
	for i := int64(1); i <= 3; i++ {
		h.recorder.Record(context.Background(), h.slowFactsFor(i))
	}
	samples, state, _ := h.keys()
	if !h.exists(state) {
		h.t.Fatalf("3 条慢样本后 state 键应存在（触发阈值 3）")
	}
	if n := h.countSamples(); n != 3 {
		h.t.Fatalf("3 条慢样本后滑窗应有 3 个成员，得到 %d", n)
	}
	_ = samples
}

// TestRecoveryResetsAfterConsecutiveCleanSamples 是恢复策略的主判据：
// 连续 N 个干净样本后，samples 与 state 键都必须消失（读侧据此当场得到零惩罚）。
func TestRecoveryResetsAfterConsecutiveCleanSamples(t *testing.T) {
	const n = 5
	h := newRecoveryHarness(t, 9101, n)
	h.seedPenalty()

	ctx := context.Background()
	_, state, streak := h.keys()

	// 前 N-1 个干净样本：键必须都还在（未达阈值不得重置）。
	for i := int64(100); i < 100+n-1; i++ {
		h.recorder.Record(ctx, h.cleanFacts(i))
	}
	if !h.exists(state) || h.countSamples() != 3 {
		t.Fatalf("第 %d 个干净样本就重置了：state=%v samples=%d，应仍未重置（阈值 %d）",
			n-1, h.exists(state), h.countSamples(), n)
	}
	if !h.exists(streak) {
		t.Fatal("干净样本应累计 streak 键")
	}

	// 第 N 个干净样本：三键全清。
	h.recorder.Record(ctx, h.cleanFacts(100+n-1))
	if h.exists(state) {
		t.Fatal("达恢复阈值后 state 键应被删除（否则读侧继续按其参数施惩罚）")
	}
	if h.countSamples() != 0 {
		t.Fatalf("达恢复阈值后 samples 滑窗应被删除，仍有 %d 个成员", h.countSamples())
	}
	if h.exists(streak) {
		t.Fatal("达恢复阈值后 streak 键应被删除（否则后续每个干净样本都变成一次重置写入）")
	}
}

// TestRecoveryStreakBreaksOnSlowSample 钉住「连续」二字：中途一个慢样本必须把计数打回零。
//
// 构造「2 干净 + 1 慢 + 2 干净」，N=3。若慢样本只是 HDEL 了 state 里的一个字段而没删计数键，
// 计数会攒到 4 ≥ 3 而误重置——本用例就是那条错误的判据。
func TestRecoveryStreakBreaksOnSlowSample(t *testing.T) {
	const n = 3
	h := newRecoveryHarness(t, 9102, n)
	h.seedPenalty()

	ctx := context.Background()
	_, state, streak := h.keys()

	h.recorder.Record(ctx, h.cleanFacts(201))
	h.recorder.Record(ctx, h.cleanFacts(202))
	if !h.exists(streak) {
		t.Fatal("两个干净样本后 streak 键应存在")
	}

	// 一个慢样本：计数必须归零（键被删）。
	h.recorder.Record(ctx, h.slowFactsFor(203))
	if h.exists(streak) {
		t.Fatal("慢样本必须把连续干净计数打回零（键应被删除）")
	}

	// 再来 2 个干净样本：仍不足 N=3（若计数没归零，此时会是 4 ≥ 3 而误重置）。
	h.recorder.Record(ctx, h.cleanFacts(204))
	h.recorder.Record(ctx, h.cleanFacts(205))
	if !h.exists(state) {
		t.Fatal("计数未归零导致误重置：2 干净 + 1 慢 + 2 干净在 N=3 下不该重置")
	}
	if h.countSamples() != 4 {
		t.Fatalf("滑窗应有 4 个慢样本（3 播种 + 1），得到 %d", h.countSamples())
	}

	// 第 3 个连续干净样本：这次才该重置。
	h.recorder.Record(ctx, h.cleanFacts(206))
	if h.exists(state) || h.countSamples() != 0 {
		t.Fatal("打断后重新攒够 3 个连续干净样本应重置")
	}
}

// TestRecoveryIgnoresUnratableSamples 钉住计数对象：**只有可判定样本**才计入连续干净计数。
//
// 理由（任务书指定的实现约束）：短输出（<minOutputTokens）、非 200、整包到达（首字比例超限）
// 这些请求没有任何速率信息，若也计入「干净」，一个正在劣化但恰好只服务短请求的渠道会被误判
// 为已恢复、惩罚被清掉——与「宁可少标不可误标」的口径相反。
//
// 断言方式不是「键没建」而是**计数不变**：先攒 1 个干净样本，再送三类不可判定样本，
// 计数必须仍是 1。若恢复逻辑被放在 generationRate 之前，这里会涨到 4。
func TestRecoveryIgnoresUnratableSamples(t *testing.T) {
	h := newRecoveryHarness(t, 9105, 10)
	ctx := context.Background()

	h.recorder.Record(ctx, h.cleanFacts(501))
	if got := h.streakValue(); got != 1 {
		t.Fatalf("1 个干净样本后计数应为 1，得到 %d", got)
	}

	// ① 输出 token 低于下限。
	shortTokens := h.cleanFacts(502)
	tokens := int64(10)
	shortTokens.OutputTokens = &tokens
	// ② 状态码非 200。
	notOK := h.cleanFacts(503)
	notOK.StatusCode = 500
	// ③ 整包到达：首字比例 0.95 > 0.9。
	wholeBody := h.cleanFacts(504)
	exact := 950
	wholeBody.FirstByteMS = &exact

	for _, facts := range []Facts{shortTokens, notOK, wholeBody} {
		h.recorder.Record(ctx, facts)
	}
	if got := h.streakValue(); got != 1 {
		t.Fatalf("不可判定样本不应计入连续干净计数，期望仍为 1，得到 %d", got)
	}
}

// TestRecoveryRequestsDefaultIsTen 钉住 N 的出厂默认 10（参数 NULL 时）。
func TestRecoveryRequestsDefaultIsTen(t *testing.T) {
	if got := DefaultParams().RecoveryRequests; got != 10 {
		t.Fatalf("恢复阈值默认 %d，应为 10（用户 2026-09-22 指定）", got)
	}
	if got := (Params{}).normalize().RecoveryRequests; got != 10 {
		t.Fatalf("零值应收敛到出厂默认 10，得到 %d", got)
	}
}

// TestRecoveryRequestsComesFromColumn 钉住 N 由列决定而非写死常量。
//
// 用非默认值（2）验算：若实现从 DefaultParams 取（10），本用例会挂在「第 2 个就该重置」。
func TestRecoveryRequestsComesFromColumn(t *testing.T) {
	const n = 2
	h := newRecoveryHarness(t, 9103, n)
	h.seedPenalty()

	ctx := context.Background()
	_, state, _ := h.keys()
	h.recorder.Record(ctx, h.cleanFacts(301))
	if !h.exists(state) {
		t.Fatalf("N=%d 时第 1 个干净样本就重置了", n)
	}
	h.recorder.Record(ctx, h.cleanFacts(302))
	if h.exists(state) {
		t.Fatalf("N=%d 时第 2 个干净样本应触发重置（若为默认 10 则不会）", n)
	}
}

// TestRecoveryLeavesSessionCooldown 钉住一处**已知残留**：重置不清会话冷却键。
//
// 键含会话身份、无法枚举，只能等各自 60 秒 TTL 自然过期。把它钉成用例是为了让「重置后最多
// 60 秒内旧会话仍可能避开该渠道」这条边界可见——它不是缺陷，是明确的取舍。
func TestRecoveryLeavesSessionCooldown(t *testing.T) {
	const n = 2
	h := newRecoveryHarness(t, 9104, n)
	ctx := context.Background()
	// 带会话身份的慢样本会写冷却键。
	slow := h.slowFactsFor(401)
	slow.SessionID = "sess_recovery_lane_b"
	slow.KeyID = 7
	for i := int64(1); i <= 3; i++ {
		facts := slow
		facts.RequestID = 400 + i
		h.recorder.Record(ctx, facts)
	}
	cooldown := session.ProviderCooldownKey(slow.SessionID, slow.KeyID, h.provider)
	if !h.exists(cooldown) {
		t.Fatal("慢样本应写会话冷却键")
	}
	h.recorder.Record(ctx, h.cleanFacts(410))
	h.recorder.Record(ctx, h.cleanFacts(411))
	if h.exists(stateKey(h.provider, h.model)) {
		t.Fatal("应已重置")
	}
	if !h.exists(cooldown) {
		t.Fatal("重置不应清会话冷却键（已知残留：键含会话身份无法枚举，等 60 秒 TTL）")
	}
}

// TestFirstThresholdCrossingRecordsPenaltyUpLog 钉住「首次跨阈」那一档的升档日志确实落盘。
//
// 为什么单独立一条：慢分支的 state pipeline 里 HGet（取旧惩罚值）在**首次**跨阈时必然未命中，
// go-redis 的 pipeline Exec 会因此返回 redis.Nil；若把它当致命错误提前返回，首次跨阈会静默丢掉
// 三件事——会话冷却键、升档日志、并被误报一条 state_write_failed。冷却键已由
// TestRecoveryLeavesSessionCooldown 钉住，本用例钉日志这一面（它是用户在弹窗里最该看到的一条）。
func TestFirstThresholdCrossingRecordsPenaltyUpLog(t *testing.T) {
	// N 取大值，避免恢复策略在三次样本内就把状态清掉。
	h := newRecoveryHarness(t, 9105, 10)
	ctx := context.Background()
	slow := h.slowFactsFor(501)
	for i := int64(1); i <= 3; i++ {
		facts := slow
		facts.RequestID = 500 + i
		h.recorder.Record(ctx, facts)
	}
	events, err := slowlog.NewReader(h.client, nil).Recent(ctx, h.provider, 10)
	if err != nil {
		t.Fatalf("读低速日志失败: %v", err)
	}
	for _, event := range events {
		if event.Kind != slowlog.KindPenaltyUp {
			continue
		}
		if event.PenaltyFrom == nil || event.PenaltyTo == nil {
			t.Fatalf("升档事件应带前后值: %+v", event)
		}
		if *event.PenaltyFrom != 0 || *event.PenaltyTo != 10 {
			t.Fatalf("首次跨阈应为 0 -> 10，得到 %d -> %d", *event.PenaltyFrom, *event.PenaltyTo)
		}
		return
	}
	t.Fatalf("首次跨阈应记一条 penalty_up 日志，实际 %d 条事件: %+v", len(events), events)
}
