package health

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

func TestLadderLevelClamping(t *testing.T) {
	cases := []struct {
		name     string
		current  int64
		maxCount int64
		want     int64
	}{
		{name: "负级数按 0", current: -3, maxCount: 3, want: 0},
		{name: "零次上限即恒 0", current: 5, maxCount: 0, want: 0},
		{name: "负上限按 0", current: 2, maxCount: -1, want: 0},
		{name: "封顶", current: 9, maxCount: 3, want: 3},
		{name: "区间内不动", current: 2, maxCount: 3, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ladderLevel(tc.current, tc.maxCount); got != tc.want {
				t.Fatalf("ladderLevel(%d, %d) = %d，期望 %d", tc.current, tc.maxCount, got, tc.want)
			}
		})
	}
}

func TestLadderWindowSequenceMatchesUserSpec(t *testing.T) {
	// 用户口径：base=5m, increment=10m, maxCount=3
	// → 5 / 15 / 25 / 35 / 35 / 35…（5m 那一档是 5+10×0，即首次开闸）
	config := route.ProviderCircuitConfig{
		FailureThreshold:         5,
		OpenDurationMS:           300000,
		HalfOpenSuccessThreshold: 2,
		OpenDurationIncrementMS:  600000,
		MaxOpenCount:             3,
	}
	want := []int64{300000, 900000, 1500000, 2100000, 2100000, 2100000}
	level := int64(0)
	for index, expected := range want {
		if got := ladderWindowMS(config, level); got != expected {
			t.Fatalf("第 %d 轮（级数 %d）窗口 = %d，期望 %d", index, level, got, expected)
		}
		// 下一轮：试探失败 → 级数 +1（封顶）
		level = nextLadderLevel(level, config.MaxOpenCount)
	}
}

func TestLadderDegeneratesToBaseWhenDisabled(t *testing.T) {
	// 出厂默认（increment=0 或 maxCount=0）必须恒为基础窗口：这是「默认不改变现行为」的硬要求。
	cases := []struct {
		name      string
		increment int64
		maxCount  int64
	}{
		{name: "递增为 0（出厂默认）", increment: 0, maxCount: 3},
		{name: "次数为 0（出厂默认）", increment: 600000, maxCount: 0},
		{name: "两者都为 0（出厂默认）", increment: 0, maxCount: 0},
		{name: "负递增", increment: -600000, maxCount: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := route.ProviderCircuitConfig{
				OpenDurationMS:          1800000,
				OpenDurationIncrementMS: tc.increment,
				MaxOpenCount:            tc.maxCount,
			}
			for level := int64(0); level <= 5; level++ {
				if got := ladderWindowMS(config, level); got != config.OpenDurationMS {
					t.Fatalf("级数 %d 时窗口 = %d，期望恒为基础值 %d", level, got, config.OpenDurationMS)
				}
			}
		})
	}
}

// ladderTestConfig 把阶梯配置写进 Redis 哈希（数据面就是从这里读的）。
func ladderTestConfig(t *testing.T, client *fakeRedis, providerID int64, incrementMS, maxCount int64) {
	t.Helper()
	key := route.ProviderConfigKeyPrefix + strconv.FormatInt(providerID, 10)
	if client.hashes[key] == nil {
		client.hashes[key] = map[string]string{}
	}
	client.hashes[key]["failureThreshold"] = "5"
	client.hashes[key]["openDuration"] = "300000"
	client.hashes[key]["halfOpenSuccessThreshold"] = "2"
	client.hashes[key]["releaseIncrement"] = strconv.FormatInt(incrementMS, 10)
	client.hashes[key]["maxOpenCount"] = strconv.FormatInt(maxCount, 10)
}

// failToOpen 连打失败直到开闸（阈值 5 次）。
func failToOpen(t *testing.T, writer *Writer, now *time.Time, providerID int64) {
	t.Helper()
	for attempt := 0; attempt < 5; attempt++ {
		if err := writer.RecordProviderFailure(context.Background(), providerID, nil); err != nil {
			t.Fatalf("记失败出错: %v", err)
		}
	}
}

// windowOf 读回 Redis 上的开闸窗口时长（circuitOpenUntil - 当前时刻）。
func windowOf(t *testing.T, client *fakeRedis, now *time.Time, providerID int64) int64 {
	t.Helper()
	raw := client.hashes[providerKey(providerID)]
	until, err := strconv.ParseInt(raw["circuitOpenUntil"], 10, 64)
	if err != nil {
		t.Fatalf("读 circuitOpenUntil 失败: %v（原值 %q）", err, raw["circuitOpenUntil"])
	}
	return until - now.UnixMilli()
}

func levelOf(t *testing.T, client *fakeRedis, providerID int64) int64 {
	t.Helper()
	raw := client.hashes[providerKey(providerID)]
	value, err := strconv.ParseInt(raw["consecutiveOpenCount"], 10, 64)
	if err != nil {
		t.Fatalf("读 consecutiveOpenCount 失败: %v（原值 %q）", err, raw["consecutiveOpenCount"])
	}
	return value
}

// TestLadderEscalatesOnFailedTrialsAndResetsOnRecovery 是阶梯的核心实证：
// 真状态迁移（fakeRedis 上的读改写）走出 5/15/25/35/35 的序列，恢复后归零并回到 base。
func TestLadderEscalatesOnFailedTrialsAndResetsOnRecovery(t *testing.T) {
	client := newFakeRedis()
	writer, now := testWriter(t, client, false, nil)
	const providerID int64 = 901
	ladderTestConfig(t, client, providerID, 600000, 3)

	// 第一轮：从 closed 触发开闸 → 第 0 级，窗口 = base(5m)
	failToOpen(t, writer, now, providerID)
	if got := windowOf(t, client, now, providerID); got != 300000 {
		t.Fatalf("首次开闸窗口 = %d，期望 base 300000", got)
	}
	if got := levelOf(t, client, providerID); got != 0 {
		t.Fatalf("首次开闸级数 = %d，期望 0", got)
	}

	// 连续 5 轮「窗口到期 → 试探失败」→ 期望 15m / 25m / 35m / 35m / 35m
	wantWindows := []int64{900000, 1500000, 2100000, 2100000, 2100000}
	wantLevels := []int64{1, 2, 3, 3, 3}
	for round, wantWindow := range wantWindows {
		*now = now.Add(time.Duration(windowOf(t, client, now, providerID)) * time.Millisecond).Add(time.Second)
		if err := writer.RecordProviderFailure(context.Background(), providerID, nil); err != nil {
			t.Fatalf("第 %d 轮试探失败记账出错: %v", round, err)
		}
		if got := windowOf(t, client, now, providerID); got != wantWindow {
			t.Fatalf("第 %d 轮新窗口 = %d，期望 %d（级数 %d）", round, got, wantWindow, levelOf(t, client, providerID))
		}
		if got := levelOf(t, client, providerID); got != wantLevels[round] {
			t.Fatalf("第 %d 轮级数 = %d，期望 %d", round, got, wantLevels[round])
		}
	}

	// 恢复：窗口到期后连续两次成功（halfOpenSuccessThreshold=2）→ closed 且级数归零
	*now = now.Add(time.Duration(windowOf(t, client, now, providerID)) * time.Millisecond).Add(time.Second)
	for attempt := 0; attempt < 2; attempt++ {
		if err := writer.RecordProviderSuccess(context.Background(), providerID); err != nil {
			t.Fatalf("记成功出错: %v", err)
		}
	}
	raw := client.hashes[providerKey(providerID)]
	if state := raw["circuitState"]; state != string(route.StateClosed) {
		t.Fatalf("恢复后状态 = %q，期望 closed", state)
	}
	if got := levelOf(t, client, providerID); got != 0 {
		t.Fatalf("恢复后级数 = %d，期望归零", got)
	}

	// 再触发一轮熔断 → 窗口回到 base（「如果恢复了，则又从 5 开始」）
	failToOpen(t, writer, now, providerID)
	if got := windowOf(t, client, now, providerID); got != 300000 {
		t.Fatalf("恢复后再熔断的窗口 = %d，期望回到 base 300000", got)
	}
}

// TestLadderAdvancesOncePerRound 钉住「一次窗口期内只 +1」。
func TestLadderAdvancesOncePerRound(t *testing.T) {
	client := newFakeRedis()
	writer, now := testWriter(t, client, false, nil)
	const providerID int64 = 902
	ladderTestConfig(t, client, providerID, 600000, 3)

	failToOpen(t, writer, now, providerID)
	*now = now.Add(301 * time.Second) // 窗口过期

	// 同一窗口期内连打 4 次失败：第一次重开闸（+1），其后都在「窗口内」早退。
	for attempt := 0; attempt < 4; attempt++ {
		if err := writer.RecordProviderFailure(context.Background(), providerID, nil); err != nil {
			t.Fatalf("记失败出错: %v", err)
		}
	}
	if got := levelOf(t, client, providerID); got != 1 {
		t.Fatalf("同一窗口期内 4 次失败后级数 = %d，期望只 +1 = 1", got)
	}
	if got := windowOf(t, client, now, providerID); got != 900000 {
		t.Fatalf("同一窗口期内 4 次失败后窗口 = %d，期望 900000（只推进一级）", got)
	}
}

// TestLadderIgnoresLegacyStateWithoutField 覆盖旧状态兼容：不含新字段的哈希按 0 处理，
// 且写入时不破坏既有字段。
func TestLadderIgnoresLegacyStateWithoutField(t *testing.T) {
	client := newFakeRedis()
	writer, now := testWriter(t, client, false, nil)
	const providerID int64 = 903
	ladderTestConfig(t, client, providerID, 600000, 3)

	// 手写一个「老实例」留下的哈希：没有 consecutiveOpenCount，且已处于窗口过期的 open。
	client.hashes[providerKey(providerID)] = map[string]string{
		"failureCount":         "7",
		"lastFailureTime":      strconv.FormatInt(now.Add(-time.Hour).UnixMilli(), 10),
		"circuitState":         string(route.StateOpen),
		"circuitOpenUntil":     strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10),
		"halfOpenSuccessCount": "0",
	}

	if err := writer.RecordProviderFailure(context.Background(), providerID, nil); err != nil {
		t.Fatalf("记失败出错: %v", err)
	}
	// 缺失字段按 0 处理 → 本次算第 1 级（一次试探失败），窗口 = base + increment
	if got := levelOf(t, client, providerID); got != 1 {
		t.Fatalf("旧状态升级后级数 = %d，期望 1（缺失按 0，再 +1）", got)
	}
	if got := windowOf(t, client, now, providerID); got != 900000 {
		t.Fatalf("旧状态升级后窗口 = %d，期望 900000", got)
	}
	// 既有字段不得被抹掉（failureCount 继续累加而不是被清零）
	raw := client.hashes[providerKey(providerID)]
	if raw["failureCount"] != "8" {
		t.Fatalf("既有 failureCount = %q，期望 8（未被抹掉）", raw["failureCount"])
	}
}

// TestLadderDisabledKeepsBaselineWindow 是「默认不改变现行为」的硬断言：
// 出厂默认（increment=0）下，连开 5 轮的窗口逐次等于 base。
func TestLadderDisabledKeepsBaselineWindow(t *testing.T) {
	client := newFakeRedis()
	writer, now := testWriter(t, client, false, nil)
	const providerID int64 = 904
	ladderTestConfig(t, client, providerID, 0, 0)

	failToOpen(t, writer, now, providerID)
	for round := 0; round < 5; round++ {
		if got := windowOf(t, client, now, providerID); got != 300000 {
			t.Fatalf("第 %d 轮窗口 = %d，期望恒为 base 300000", round, got)
		}
		if got := levelOf(t, client, providerID); got != 0 {
			t.Fatalf("第 %d 轮级数 = %d，期望恒为 0（不启用阶梯）", round, got)
		}
		*now = now.Add(301 * time.Second)
		if err := writer.RecordProviderFailure(context.Background(), providerID, nil); err != nil {
			t.Fatalf("记失败出错: %v", err)
		}
	}
}

// TestLadderFieldsAbsentOnEndpointState 钉住阶梯字段只写供应商级键。
func TestLadderFieldsAbsentOnEndpointState(t *testing.T) {
	client := newFakeRedis()
	writer, _ := testWriter(t, client, true, nil)
	if err := writer.RecordEndpointFailure(context.Background(), 77, nil); err != nil {
		t.Fatalf("记端点失败出错: %v", err)
	}
	raw := client.hashes[endpointKey(77)]
	if _, exists := raw["consecutiveOpenCount"]; exists {
		t.Fatal("端点级状态里不该出现阶梯字段")
	}
}
