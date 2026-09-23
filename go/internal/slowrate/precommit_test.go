package slowrate

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 本文件钉「提交前判慢」的闭环（见 recorder.go 的 RecordPrecommit）：
// 判废事实必须**就地**写进被判废那家的滑窗，不等任何终态采样。
//
// 每个用例用**互不相同的 providerID**（键形制含 providerID），且进门前各自清掉自己名下那几把
// 键（见 clearSlowKeys）——本仓踩过「多进程共用 CCH_TEST_REDIS_URL 的库、计数被翻倍」的坑。

// captureLogger 收 warn 事件：判废路径的失败面必须可见（本包测试此前没有日志替身）。
type captureLogger struct {
	mu     sync.Mutex
	events []string
}

func (l *captureLogger) Warn(event string, _ map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *captureLogger) has(event string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, got := range l.events {
		if got == event {
			return true
		}
	}
	return false
}

// precommitParams 取阈值 3 / 步长 10 / 封顶 30 / 窗 10 分钟：一档的边界好算。
func precommitParams() Params {
	return Params{
		WindowMinutes:    10,
		TriggerCount:     3,
		Ratio:            0.3,
		PenaltyStep:      10,
		PenaltyMax:       30,
		CooldownSeconds:  60,
		RecoveryRequests: 10,
	}
}

type precommitCase struct {
	client   redis.UniversalClient
	recorder *Recorder
	provider int64
	model    string
	now      time.Time
}

// newPrecommitCase 建一个真 Redis + 已开启监控的 Recorder；log 为 nil 时不记日志。
//
// providerID 是本用例的专属渠道号（每用例互不相同）：进门前先清掉它名下的残留键，跑完再清一遍。
// 不清的话，「同一请求只占一格」这类计数断言会被上一轮的残留顶掉（见 clearSlowKeys 的说明）。
func newPrecommitCase(t *testing.T, providerID int64, enabled bool, log Logger) *precommitCase {
	t.Helper()
	client := recoveryRedis(t)
	model := "precommit-probe-model"
	clearSlowKeys(t, client, providerID, model)
	t.Cleanup(func() { clearSlowKeys(t, client, providerID, model) })
	now := time.UnixMilli(1790050000000)
	recorder := New(Options{
		Redis:  client,
		Config: &stubConfig{enabled: map[int64]bool{providerID: enabled}, params: precommitParams()},
		Logger: log,
		Now:    func() time.Time { return now },
	})
	if recorder == nil {
		t.Fatal("Recorder 构造失败")
	}
	return &precommitCase{client: client, recorder: recorder, provider: providerID, model: model, now: now}
}

func (c *precommitCase) facts(requestID int64) PrecommitFacts {
	return PrecommitFacts{
		ProviderID: c.provider,
		SessionID:  "precommit-session",
		KeyID:      7,
		ModelKey:   c.model,
		RequestID:  requestID,
	}
}

// memberScore 读某请求在滑窗里的分数；键或成员不存在时 ok=false。
func (c *precommitCase) memberScore(t *testing.T, requestID int64) (float64, bool) {
	t.Helper()
	score, err := c.client.ZScore(context.Background(), samplesKey(c.provider, c.model),
		strconv.FormatInt(requestID, 10)).Result()
	if err == redis.Nil {
		return 0, false
	}
	if err != nil {
		t.Fatalf("读滑窗成员失败: %v", err)
	}
	return score, true
}

func (c *precommitCase) statePenalty(t *testing.T) (int, bool) {
	t.Helper()
	value, err := c.client.HGet(context.Background(), stateKey(c.provider, c.model), StateFieldPenalty).Int()
	if err == redis.Nil {
		return 0, false
	}
	if err != nil {
		t.Fatalf("读 state 失败: %v", err)
	}
	return value, true
}

// 钉子①：一次判废立即留下事实（滑窗成员），且**不需要基线**——判废是探测结论，
// 不建立在「这家平时多快」之上（事后采样那一支会因基线缺失而整段 fail-open）。
func TestRecordPrecommitWritesFactImmediately(t *testing.T) {
	c := newPrecommitCase(t, 991001, true, nil)
	ctx := context.Background()

	c.recorder.RecordPrecommit(ctx, c.facts(4242))

	score, ok := c.memberScore(t, 4242)
	if !ok {
		t.Fatal("判废后滑窗里应有该请求的成员（事实），实际没有")
	}
	if score != float64(c.now.UnixMilli()) {
		t.Fatalf("成员分数应写入时刻，实际 %v", score)
	}
	// 未达触发阈值（3）时不建 state 键：读侧的存在性闸门据此把「未慢过的组合」保持零成本，
	// 而 providers_health 会全库 SCAN `cch:slow:*:state`。
	if _, exists := c.statePenalty(t); exists {
		t.Fatal("计数未达触发阈值时不应创建 state 键")
	}
}

// 钉子①延伸：计数达触发阈值即落到惩罚上（读侧按活窗计数派生，见 route.DeriveSlowRatePenalty）。
func TestRecordPrecommitReachesPenaltyAtTrigger(t *testing.T) {
	c := newPrecommitCase(t, 991002, true, nil)
	ctx := context.Background()

	for _, id := range []int64{11, 12, 13} {
		c.recorder.RecordPrecommit(ctx, c.facts(id))
	}

	penalty, exists := c.statePenalty(t)
	if !exists {
		t.Fatal("计数达阈值后应创建 state 键（读侧施惩罚的存在性闸门）")
	}
	want := route.DeriveSlowRatePenalty(3, 3, 10, 30)
	if penalty != want {
		t.Fatalf("惩罚应为 %d，实际 %d", want, penalty)
	}
}

// 钉子②幂等：同一请求的多次判废（同家重试、多 attempt）只占滑窗一格；
// 与终态样本共用同一成员，故「同请求既被判废又被采样」也不会重复计。
func TestRecordPrecommitIsIdempotentPerRequest(t *testing.T) {
	c := newPrecommitCase(t, 991003, true, nil)
	ctx := context.Background()

	c.recorder.RecordPrecommit(ctx, c.facts(777))
	c.recorder.RecordPrecommit(ctx, c.facts(777))
	c.recorder.RecordPrecommit(ctx, c.facts(777))

	count, err := c.client.ZCard(ctx, samplesKey(c.provider, c.model)).Result()
	if err != nil {
		t.Fatalf("读滑窗基数失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("同一请求重复判废应只占一格，实际 %d 格", count)
	}
}

// 钉子③：判废是一条慢事实，必须打断「连续干净样本」计数（否则恢复策略会把它当没发生）。
func TestRecordPrecommitBreaksCleanStreak(t *testing.T) {
	c := newPrecommitCase(t, 991004, true, nil)
	ctx := context.Background()

	streakKey := cleanStreakKey(c.provider, c.model)
	if err := c.client.Set(ctx, streakKey, "9", time.Hour).Err(); err != nil {
		t.Fatalf("预置连续干净计数失败: %v", err)
	}

	c.recorder.RecordPrecommit(ctx, c.facts(555))

	exists, err := c.client.Exists(ctx, streakKey).Result()
	if err != nil {
		t.Fatalf("查连续干净计数失败: %v", err)
	}
	if exists != 0 {
		t.Fatal("判废后连续干净计数应被清零（键应被删除）")
	}
}

// 钉子④：未开启监控的渠道一次 Redis 读写都不发（零开销闸门）。
func TestRecordPrecommitSkipsDisabledProvider(t *testing.T) {
	c := newPrecommitCase(t, 991005, false, nil)
	ctx := context.Background()

	c.recorder.RecordPrecommit(ctx, c.facts(888))

	exists, err := c.client.Exists(ctx, samplesKey(c.provider, c.model)).Result()
	if err != nil {
		t.Fatalf("查滑窗键失败: %v", err)
	}
	if exists != 0 {
		t.Fatal("未开启监控的渠道不应写任何键")
	}
}

// 钉子⑤：Redis 写失败只 warn，不 panic、不冒泡（接口本就没有返回值）。
func TestRecordPrecommitWriteFailureOnlyWarns(t *testing.T) {
	log := &captureLogger{}
	bad := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 50 * time.Millisecond,
		ReadTimeout: 50 * time.Millisecond,
		// -1 关掉 go-redis 的内部重试：本用例要的是「一次就失败」，不是重试三次。
		MaxRetries: -1,
	})
	t.Cleanup(func() { _ = bad.Close() })
	recorder := New(Options{
		Redis:  bad,
		Config: &stubConfig{enabled: map[int64]bool{991006: true}, params: precommitParams()},
		Logger: log,
		Now:    func() time.Time { return time.UnixMilli(1790050000000) },
	})
	if recorder == nil {
		t.Fatal("Recorder 构造失败")
	}

	recorder.RecordPrecommit(context.Background(), PrecommitFacts{
		ProviderID: 991006,
		ModelKey:   "precommit-probe-model",
		RequestID:  999,
	})

	if !log.has("slowrate.precommit_write_failed") {
		t.Fatalf("写失败应记一条 slowrate.precommit_write_failed，实际事件：%v", log.events)
	}
}

// 钉子⑥：入参不全（无请求 id / 无模型键 / 无渠道）时整段跳过——请求 id 是幂等键，
// 没有它就无从保证「同一请求只计一次」。
func TestRecordPrecommitSkipsIncompleteFacts(t *testing.T) {
	c := newPrecommitCase(t, 991007, true, nil)
	ctx := context.Background()

	c.recorder.RecordPrecommit(ctx, PrecommitFacts{ProviderID: c.provider, ModelKey: c.model})
	c.recorder.RecordPrecommit(ctx, PrecommitFacts{ProviderID: c.provider, RequestID: 1})
	c.recorder.RecordPrecommit(ctx, PrecommitFacts{ModelKey: c.model, RequestID: 1})

	exists, err := c.client.Exists(ctx, samplesKey(c.provider, c.model)).Result()
	if err != nil {
		t.Fatalf("查滑窗键失败: %v", err)
	}
	if exists != 0 {
		t.Fatal("入参不全时不应写任何键")
	}
}
