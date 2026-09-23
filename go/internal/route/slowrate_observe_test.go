package route

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是隔离运行态只读面（slowrate_observe.go）的用例。
//
// 三层钉子：
//   - **枚举口径**：四族键的并集（否则「有基线、有干净计数、但从未慢到触发隔离」那一态看不见，
//     而那正是生产 2026-09-22 的实测形态）；且只取本渠道的键（16 不得吃掉 167）。
//   - **准入阶梯**：streak -> 放行比例必须逐档对得上 quarantinePermilleForStreak，
//     且「未隔离」是 1000 而不是 0（0 会被读成「挡死」）。
//   - **活窗口径**：样本数只数窗内（不是 ZCARD 的全量），与选路施惩罚时看到的是同一个数。

// Scan 是替身对枚举组合键的支持：只实现本读面用到的「前缀 + *」形态。
func (f *slowRateRedis) Scan(
	_ context.Context, _ uint64, match string, _ int64,
) *redis.ScanCmd {
	cmd := redis.NewScanCmd(context.Background(), nil)
	prefix, _ := strings.CutSuffix(match, "*")
	keys := make([]string, 0, len(f.values)+len(f.zsets))
	for key := range f.values {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	// zsets 也要枚举：真实 Redis 里滑窗键与状态族（模式一致），少枚举它会把
	// 「滑窗有成员、状态键还没建」那一态从夹具里隐去。
	for key := range f.zsets {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	// 固定顺序：真实 Redis 的 SCAN 顺序不定，本读面自己排序，故替身给什么顺序都不该影响结果。
	sort.Strings(keys)
	cmd.SetVal(keys, 0)
	return cmd
}

// Scan 让「枚举阶段就失败」这条路可构造（Redis 不可达时 SCAN 是第一个报错的命令）。
func (f *slowRateFailingRedis) Scan(_ context.Context, _ uint64, _ string, _ int64) *redis.ScanCmd {
	cmd := redis.NewScanCmd(context.Background(), nil)
	cmd.SetErr(errors.New("redis: connection refused"))
	return cmd
}

// slowRateObserveReadFailRedis 让枚举成功、读命令全失败：覆盖「SCAN 通了但 pipeline 挂了」。
type slowRateObserveReadFailRedis struct {
	*slowRateRedis
}

func (f *slowRateObserveReadFailRedis) Pipelined(
	_ context.Context,
	fn func(redis.Pipeliner) error,
) ([]redis.Cmder, error) {
	pending := &slowRatePipeline{redis: f.slowRateRedis, ctx: context.Background()}
	if err := fn(pending); err != nil {
		return nil, err
	}
	return pending.cmds, errors.New("redis: connection refused")
}

// PTTL 报告探针租约的剩余寿命：键在 values 里即视为持有中，报一个租约期的常量。
//
// 键不存在时报 -2（与 go-redis 的 DurationCmd 同形：负数原样存成纳秒量级的 Duration），
// 故读侧必须按「负数即无租约」判定，这条用例顺带把它钉住。
func (p *slowRatePipeline) PTTL(_ context.Context, key string) *redis.DurationCmd {
	p.redis.readKeys = append(p.redis.readKeys, key)
	cmd := redis.NewDurationCmd(p.ctx, time.Millisecond)
	if _, ok := p.redis.values[key]; ok {
		cmd.SetVal(slowProbeLeaseTTL)
	} else {
		cmd.SetVal(-2 * time.Nanosecond)
	}
	p.cmds = append(p.cmds, cmd)
	return cmd
}

// slowRateObserveCombination 取指定模型键的观测行。
func slowRateObserveCombination(
	t *testing.T,
	observations []SlowRateStateObservation,
	modelKey string,
) SlowRateStateObservation {
	t.Helper()
	for _, observation := range observations {
		if observation.ModelKey == modelKey {
			return observation
		}
	}
	t.Fatalf("观测里没有模型 %q 的组合（实得 %d 行）", modelKey, len(observations))
	return SlowRateStateObservation{}
}

// TestObserveStatesReportsStateAndNoState 钉住「state 存在 / 不存在」两种情形下返回结构都正确。
//
// 右侧那条组合刻意造成**生产实测的形态**：状态键不存在、滑窗为空，只有基线与干净计数。
// 它必须出现在结果里——「监控在跑但从未慢到触发隔离」与「监控根本没开」是两回事，
// 而只扫状态键的实现会把前者整个漏掉（正是这次要修的观测盲区）。
func TestObserveStatesReportsStateAndNoState(t *testing.T) {
	reader := newSlowRateReaderAt(&slowRateRedis{
		values: map[string]string{
			SlowRateStateKey(9, "m-slow"):        slowRateStateValueQuarantined(t, 20, 30, 3, 10, 30),
			SlowRateBaselineKey(9, "m-slow"):     slowRateBaselineValue(t, "primary"),
			SlowRateCleanStreakKey(9, "m-slow"):  "4",
			SlowProbeLeaseKey(9, "m-slow"):       "holder",
			SlowRateBaselineKey(9, "m-clean"):    slowRateBaselineValue(t, "primary"),
			SlowRateCleanStreakKey(9, "m-clean"): "2",
		},
		zsets: map[string][]redis.Z{SlowRateSamplesKey(9, "m-slow"): slowRateSamples(3, 60_000)},
	})

	got, err := reader.ObserveStates(context.Background(), slowRateProvider(9))
	if err != nil {
		t.Fatalf("读隔离态失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("组合数 = %d，期望 2（状态键与基线键的并集）", len(got))
	}

	slow := slowRateObserveCombination(t, got, "m-slow")
	if !slow.StateExists {
		t.Errorf("m-slow 的 StateExists = 假，期望真（状态键在）")
	}
	if !slow.Quarantined {
		t.Errorf("m-slow 的 Quarantined = 假，期望真（标过隔离 + 活窗惩罚为正 + 基线可用）")
	}
	if slow.Penalty != 10 {
		t.Errorf("m-slow 的 Penalty = %d，期望 10（窗内 3 条 / 阈值 3 * 步长 10）", slow.Penalty)
	}
	if slow.CleanStreak != 4 {
		t.Errorf("m-slow 的 CleanStreak = %d，期望 4", slow.CleanStreak)
	}
	if slow.AdmissionPermille != 100 {
		t.Errorf("m-slow 的 AdmissionPermille = %d，期望 100（streak=4 落在 10%% 档）", slow.AdmissionPermille)
	}
	if slow.SampleLiveCount != 3 {
		t.Errorf("m-slow 的 SampleLiveCount = %d，期望 3", slow.SampleLiveCount)
	}
	if !slow.BaselineUsable {
		t.Errorf("m-slow 的 BaselineUsable = 假，期望真（source=primary）")
	}
	if !slow.ProbeLeaseHeld {
		t.Errorf("m-slow 的 ProbeLeaseHeld = 假，期望真（租约键在）")
	}
	if slow.ProbeLeaseTTL != slowProbeLeaseTTL {
		t.Errorf("m-slow 的 ProbeLeaseTTL = %v，期望 %v", slow.ProbeLeaseTTL, slowProbeLeaseTTL)
	}

	clean := slowRateObserveCombination(t, got, "m-clean")
	if clean.StateExists {
		t.Errorf("m-clean 的 StateExists = 真，期望假（从未慢到建状态键）")
	}
	if clean.Quarantined {
		t.Errorf("m-clean 的 Quarantined = 真，期望假")
	}
	if clean.Penalty != 0 || clean.SampleLiveCount != 0 {
		t.Errorf("m-clean 的 Penalty/SampleLiveCount = %d/%d，期望 0/0", clean.Penalty, clean.SampleLiveCount)
	}
	if clean.CleanStreak != 2 {
		t.Errorf("m-clean 的 CleanStreak = %d，期望 2（干净计数键在，证明监控在跑）", clean.CleanStreak)
	}
	if clean.AdmissionPermille != quarantineAdmissionFullPermille {
		t.Errorf("m-clean 的 AdmissionPermille = %d，期望 %d（未隔离即全放）",
			clean.AdmissionPermille, quarantineAdmissionFullPermille)
	}
	if !clean.BaselineUsable {
		t.Errorf("m-clean 的 BaselineUsable = 假，期望真")
	}
	if clean.ProbeLeaseHeld {
		t.Errorf("m-clean 的 ProbeLeaseHeld = 真，期望假（无租约键）")
	}
	if clean.ProbeLeaseTTL >= 0 {
		t.Errorf("m-clean 的 ProbeLeaseTTL = %v，期望为负（键不存在）", clean.ProbeLeaseTTL)
	}
}

// TestObserveStatesAdmissionPermilleFollowsCleanStreak 钉住准入比例与 streak 的对应关系。
//
// 阶梯的真源是 quarantinePermilleForStreak；本用例逐档列出「streak -> 放行千分比」，
// 并把两个边界造出来：未标隔离（存量数据）恒 1000，标了隔离但活窗已空也恒 1000。
func TestObserveStatesAdmissionPermilleFollowsCleanStreak(t *testing.T) {
	cases := []struct {
		streak int
		want   int
	}{
		{0, 0},
		{QuarantineAdmissionTenFrom - 1, 0},
		{QuarantineAdmissionTenFrom, 100},
		{QuarantineAdmissionThirtyFrom - 1, 100},
		{QuarantineAdmissionThirtyFrom, 300},
		{QuarantineAdmissionThirtyFrom + 3, 300},
	}
	for _, item := range cases {
		reader := newSlowRateReaderAt(&slowRateRedis{
			values: map[string]string{
				SlowRateStateKey(9, "m1"):       slowRateStateValueQuarantined(t, 20, 30, 3, 10, 30),
				SlowRateBaselineKey(9, "m1"):    slowRateBaselineValue(t, "primary"),
				SlowRateCleanStreakKey(9, "m1"): strconv.Itoa(item.streak),
			},
			zsets: map[string][]redis.Z{SlowRateSamplesKey(9, "m1"): slowRateSamples(3, 60_000)},
		})
		got, err := reader.ObserveStates(context.Background(), slowRateProvider(9))
		if err != nil {
			t.Fatalf("streak=%d 时读隔离态失败: %v", item.streak, err)
		}
		observation := slowRateObserveCombination(t, got, "m1")
		if !observation.Quarantined {
			t.Errorf("streak=%d：Quarantined = 假，期望真", item.streak)
		}
		if observation.AdmissionPermille != item.want {
			t.Errorf("streak=%d：AdmissionPermille = %d，期望 %d",
				item.streak, observation.AdmissionPermille, item.want)
		}
	}

	t.Run("存量状态无隔离标记", func(t *testing.T) {
		// 旧版本写下的状态（无 quarantine 字段）：即使惩罚为正、streak 很高，也不得报隔离。
		reader := newSlowRateReaderAt(&slowRateRedis{
			values: map[string]string{
				SlowRateStateKey(9, "m1"):       slowRateStateValueWithParams(t, 20, 30, 3, 10, 30),
				SlowRateBaselineKey(9, "m1"):    slowRateBaselineValue(t, "primary"),
				SlowRateCleanStreakKey(9, "m1"): strconv.Itoa(QuarantineAdmissionThirtyFrom + 1),
			},
			zsets: map[string][]redis.Z{SlowRateSamplesKey(9, "m1"): slowRateSamples(3, 60_000)},
		})
		got, err := reader.ObserveStates(context.Background(), slowRateProvider(9))
		if err != nil {
			t.Fatalf("读隔离态失败: %v", err)
		}
		observation := slowRateObserveCombination(t, got, "m1")
		if observation.Quarantined {
			t.Errorf("存量状态被当成隔离了（无 quarantine 字段）")
		}
		if observation.AdmissionPermille != quarantineAdmissionFullPermille {
			t.Errorf("AdmissionPermille = %d，期望 %d", observation.AdmissionPermille, quarantineAdmissionFullPermille)
		}
		if observation.Penalty != 10 {
			t.Errorf("Penalty = %d，期望 10（惩罚仍应报出来，只是不入隔离）", observation.Penalty)
		}
	})

	t.Run("活窗已空", func(t *testing.T) {
		// 标过隔离但窗里已经没有慢样本：不再挡流量，放行比例回到 1000（否则界面会把已恢复的家说成还在挡）。
		reader := newSlowRateReaderAt(&slowRateRedis{
			values: map[string]string{
				SlowRateStateKey(9, "m1"):       slowRateStateValueQuarantined(t, 20, 30, 3, 10, 30),
				SlowRateBaselineKey(9, "m1"):    slowRateBaselineValue(t, "primary"),
				SlowRateCleanStreakKey(9, "m1"): "1",
			},
		})
		got, err := reader.ObserveStates(context.Background(), slowRateProvider(9))
		if err != nil {
			t.Fatalf("读隔离态失败: %v", err)
		}
		observation := slowRateObserveCombination(t, got, "m1")
		if observation.Quarantined {
			t.Errorf("活窗为空却报隔离")
		}
		if observation.Penalty != 0 {
			t.Errorf("Penalty = %d，期望 0", observation.Penalty)
		}
		if observation.AdmissionPermille != quarantineAdmissionFullPermille {
			t.Errorf("AdmissionPermille = %d，期望 %d", observation.AdmissionPermille, quarantineAdmissionFullPermille)
		}
	})
}

// TestObserveStatesCountsOnlyLiveWindow 钉住样本数只数**窗内**成员，而不是 ZCARD 的全量。
//
// 为何这条重要：滑窗只在**下一次慢写**时才裁掉窗外成员，而键的 TTL 是 2 倍窗长，
// 故 ZCARD 会把窗外成员算进来，读数比选路看到的惩罚偏大——排障时正是这个偏差最误导人。
func TestObserveStatesCountsOnlyLiveWindow(t *testing.T) {
	reader := newSlowRateReaderAt(&slowRateRedis{
		values: map[string]string{
			SlowRateStateKey(9, "m1"):       slowRateStateValueQuarantined(t, 10, 30, 3, 10, 30),
			SlowRateBaselineKey(9, "m1"):    slowRateBaselineValue(t, "primary"),
			SlowRateCleanStreakKey(9, "m1"): "0",
		},
		// 3 条在 1 分钟内（窗内），另 3 条在 40 分钟前（30 分钟窗之外）。
		zsets: map[string][]redis.Z{
			SlowRateSamplesKey(9, "m1"): append(slowRateSamples(3, 60_000), slowRateSamples(3, 40*60_000)...),
		},
	})
	got, err := reader.ObserveStates(context.Background(), slowRateProvider(9))
	if err != nil {
		t.Fatalf("读隔离态失败: %v", err)
	}
	observation := slowRateObserveCombination(t, got, "m1")
	if observation.SampleLiveCount != 3 {
		t.Errorf("SampleLiveCount = %d，期望 3（窗外那 3 条不该计入；若得 6 说明用了 ZCARD）",
			observation.SampleLiveCount)
	}
}

// TestObserveStatesScopesToRequestedProvider 钉住枚举口径只取本渠道的键。
//
// 前缀以 `{<pid>:` 结尾，故 pid=16 不会吃掉 pid=167 的键——这是「按渠道排障时看见别家读数」
// 这类最难察觉的错。同时钉住：模型键里的冒号（如 `global:m1`）不被当成分隔符。
func TestObserveStatesScopesToRequestedProvider(t *testing.T) {
	reader := newSlowRateReaderAt(&slowRateRedis{
		values: map[string]string{
			SlowRateStateKey(16, "global:m1"):  slowRateStateValueQuarantined(t, 20, 30, 3, 10, 30),
			SlowRateStateKey(167, "m1"):        slowRateStateValueQuarantined(t, 20, 30, 3, 10, 30),
			SlowRateStateKey(160, "m1"):        slowRateStateValueQuarantined(t, 20, 30, 3, 10, 30),
			SlowProbeLeaseKey(16, "global:m1"): "holder",
		},
	})
	got, err := reader.ObserveStates(context.Background(), slowRateProvider(16))
	if err != nil {
		t.Fatalf("读隔离态失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("组合数 = %d，期望 1（只有 16 的键）", len(got))
	}
	if got[0].ModelKey != "global:m1" {
		t.Errorf("ModelKey = %q，期望 global:m1（含冒号的模型键不得被切开）", got[0].ModelKey)
	}
	if !got[0].ProbeLeaseHeld {
		t.Errorf("租约键不在枚举口径里，但必须被读到（ProbeLeaseHeld = 假）")
	}
}

// TestObserveStatesCountsSamplesBeforeStateExists 钉住「滑窗有成员、状态键还不存在」这一态看得见。
//
// 为什么这条必须单独钉：慢样本不足触发阈值时写侧**只 ZADD 滑窗、不建状态键**
// （见 slowrate.advanceSlowState 的门槛），于是「慢过但从未触发隔离」只体现在滑窗上。
// 若计数只在状态键存在时才发，这一态会读成 sampleLiveCount=0，与「真的没慢过」不可分。
// 同时这条路走的是「渠道行与状态都没有参数」的回退分支（夹具的行不带参数、状态键不存在），
// 故它顺带钉住回退窗长可用。
func TestObserveStatesCountsSamplesBeforeStateExists(t *testing.T) {
	reader := newSlowRateReaderAt(&slowRateRedis{
		zsets: map[string][]redis.Z{SlowRateSamplesKey(9, "m1"): slowRateSamples(2, 60_000)},
	})
	got, err := reader.ObserveStates(context.Background(), slowRateProvider(9))
	if err != nil {
		t.Fatalf("读隔离态失败: %v", err)
	}
	observation := slowRateObserveCombination(t, got, "m1")
	if observation.StateExists {
		t.Errorf("StateExists = 真，期望假（未达触发阈值）")
	}
	if observation.SampleLiveCount != 2 {
		t.Errorf("SampleLiveCount = %d，期望 2（滑窗成员先于状态键存在）", observation.SampleLiveCount)
	}
	if observation.Quarantined || observation.AdmissionPermille != quarantineAdmissionFullPermille {
		t.Errorf("未触发隔离却报了隔离（quarantined=%v permille=%d）",
			observation.Quarantined, observation.AdmissionPermille)
	}
}

// TestObserveStatesFailsLoudly 钉住读失败**返回错误**而不是回空表。
//
// 空表在管理面的含义是「没有组合」，与「读不到」混同正是这次要修的观测盲区本身。
func TestObserveStatesFailsLoudly(t *testing.T) {
	t.Run("枚举阶段失败", func(t *testing.T) {
		reader := newSlowRateReaderAt(&slowRateFailingRedis{})
		if _, err := reader.ObserveStates(context.Background(), slowRateProvider(9)); err == nil {
			t.Errorf("SCAN 失败时 ObserveStates 返回了 nil 错误（空表会被读成「没有组合」）")
		}
	})

	t.Run("读命令失败", func(t *testing.T) {
		reader := newSlowRateReaderAt(&slowRateObserveReadFailRedis{slowRateRedis: &slowRateRedis{
			values: map[string]string{SlowRateStateKey(9, "m1"): slowRateStateValueQuarantined(t, 20, 30, 3, 10, 30)},
		}})
		if _, err := reader.ObserveStates(context.Background(), slowRateProvider(9)); err == nil {
			t.Errorf("读命令失败时 ObserveStates 返回了 nil 错误")
		}
	})
}
