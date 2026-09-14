package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件覆盖**等待阶梯**（circuit backoff ladder）在管理面投影上的三项加性字段：
//
//	consecutiveOpenCount / consecutiveOpenCountChangedAt / openWindowMinutes
//
// 分三层，各自都能独立跑：
//  1. 纯函数：窗口时长的取数规则（providerCircuitOpenWindowMinutes）与快照解析；
//  2. **真 Redis**：状态哈希的读写往返（含 Node 形态的旧哈希、以及复位是否把阶梯归零）；
//  3. 处理程序层：/providers/health 与 /providers/{id}/circuit-logs 两处都带上这三个字段
//     （这条要真 PG，见 requireLadderColumns 的前置跳过）。
//
// 为什么把「取数规则」测这么细：窗口时长是从哈希差值**读出**来的（不重算阶梯公式），
// 于是「级数为 0 时给什么」「改了 Redis 导致差值为负时给什么」全落在这一层，
// 而它们恰好是界面会直接显示的数。

// TestProviderCircuitOpenWindowMinutes 钉住窗口时长的取数规则：能算出就给分钟数，
// **算不出就给 nil**（不是 0、不是负数）。
func TestProviderCircuitOpenWindowMinutes(t *testing.T) {
	minute := int64(time.Minute / time.Millisecond)
	ptr := func(value int64) *int64 { return &value }

	cases := []struct {
		name        string
		openUntil   *int64
		changedAt   *int64
		level       int64
		wantMinutes *int64
	}{
		{
			name: "第 2 阶_窗口 25 分钟", openUntil: ptr(1000 * minute), changedAt: ptr(975 * minute),
			level: 2, wantMinutes: ptr(25),
		},
		{
			name: "第 3 阶_窗口 35 分钟（封顶后仍是该值）", openUntil: ptr(1000 * minute), changedAt: ptr(965 * minute),
			level: 3, wantMinutes: ptr(35),
		},
		{
			// 向上取整：差 90 秒记 2 分钟，而不是 1（与 recoveryMinutes 同口径）。
			name: "不足整分钟向上取整", openUntil: ptr(1000*minute + 90_000), changedAt: ptr(1000 * minute),
			level: 1, wantMinutes: ptr(2),
		},
		{
			name: "首次开闸（级数 0）不给窗口", openUntil: ptr(1000 * minute), changedAt: ptr(1000 * minute),
			level: 0, wantMinutes: nil,
		},
		{
			name: "老实例写的哈希（无 changedAt）不给窗口", openUntil: ptr(1000 * minute), changedAt: nil,
			level: 3, wantMinutes: nil,
		},
		{
			name: "changedAt 是空值（0）不给窗口", openUntil: ptr(1000 * minute), changedAt: ptr(0),
			level: 3, wantMinutes: nil,
		},
		{
			// 手工改过 Redis、或时钟回拨：报 0 会让界面说「本次窗口 0 分钟」。
			name: "差值为负（时钟回拨）不给窗口", openUntil: ptr(1000 * minute), changedAt: ptr(1002 * minute),
			level: 2, wantMinutes: nil,
		},
		{
			name: "openUntil 缺失不给窗口", openUntil: nil, changedAt: ptr(1000 * minute),
			level: 2, wantMinutes: nil,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := providerCircuitOpenWindowMinutes(testCase.openUntil, testCase.changedAt, testCase.level)
			switch {
			case testCase.wantMinutes == nil && got != nil:
				t.Fatalf("应给 nil，收到 %d", *got)
			case testCase.wantMinutes != nil && got == nil:
				t.Fatalf("应给 %d 分钟，收到 nil", *testCase.wantMinutes)
			case testCase.wantMinutes != nil && *got != *testCase.wantMinutes:
				t.Fatalf("应为 %d 分钟，收到 %d", *testCase.wantMinutes, *got)
			}
		})
	}
}

// TestProviderCircuitSnapshotReadsLadderFields 用**真 Redis** 读三种哈希形态：
// 带阶梯两键的（Go 写侧形态）、缺这两键的（Node 形态 / 老实例）、以及键根本不存在。
func TestProviderCircuitSnapshotReadsLadderFields(t *testing.T) {
	client := circuitTestRedis(t)
	ctx := context.Background()
	states, ok := NewRedisCircuitStates(client, nil, nil).(ProviderCircuitStore)
	if !ok {
		t.Fatal("供应商级熔断存储未实现 ProviderCircuitStore")
	}
	const providerID int64 = 987654321

	key := providerCircuitKey(providerID)
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	changedAt := time.Now().Add(-5 * time.Minute).UnixMilli()
	openUntil := changedAt + 25*int64(time.Minute/time.Millisecond)

	// ① Go 写侧形态：两个阶梯键都在。
	if err := client.HSet(ctx, key, map[string]string{
		"failureCount":                  "9",
		"lastFailureTime":               strconv.FormatInt(time.Now().UnixMilli(), 10),
		"circuitState":                  "open",
		"circuitOpenUntil":              strconv.FormatInt(openUntil, 10),
		"halfOpenSuccessCount":          "0",
		"consecutiveOpenCount":          "2",
		"consecutiveOpenCountChangedAt": strconv.FormatInt(changedAt, 10),
	}).Err(); err != nil {
		t.Fatalf("写阶梯夹具失败: %v", err)
	}
	snapshot, err := states.ProviderCircuit(ctx, providerID)
	if err != nil {
		t.Fatalf("读熔断快照失败: %v", err)
	}
	if snapshot.ConsecutiveOpenCount != 2 {
		t.Errorf("consecutiveOpenCount 应为 2，收到 %d", snapshot.ConsecutiveOpenCount)
	}
	if snapshot.ConsecutiveOpenCountChangedAtMS == nil || *snapshot.ConsecutiveOpenCountChangedAtMS != changedAt {
		t.Errorf("consecutiveOpenCountChangedAt 应为 %d，收到 %v", changedAt, snapshot.ConsecutiveOpenCountChangedAtMS)
	}
	if window := providerCircuitOpenWindowMinutes(
		snapshot.CircuitOpenUntilMS, snapshot.ConsecutiveOpenCountChangedAtMS, snapshot.ConsecutiveOpenCount,
	); window == nil || *window != 25 {
		t.Errorf("由真哈希差值推出的窗口应为 25 分钟，收到 %v", window)
	}

	// ② Node 形态（缺阶梯两键）：读作 0/nil，不报错。
	if err := client.HDel(ctx, key, "consecutiveOpenCount", "consecutiveOpenCountChangedAt").Err(); err != nil {
		t.Fatalf("清阶梯两键失败: %v", err)
	}
	legacy, err := states.ProviderCircuit(ctx, providerID)
	if err != nil {
		t.Fatalf("Node 形态哈希下读快照不应报错: %v", err)
	}
	if legacy.ConsecutiveOpenCount != 0 || legacy.ConsecutiveOpenCountChangedAtMS != nil {
		t.Errorf("缺阶梯两键应读作 0/nil，收到 level=%d changedAt=%v",
			legacy.ConsecutiveOpenCount, legacy.ConsecutiveOpenCountChangedAtMS)
	}

	// ③ 键不存在（出厂闭态）。
	if err := client.Del(ctx, key).Err(); err != nil {
		t.Fatalf("删除键失败: %v", err)
	}
	missing, err := states.ProviderCircuit(ctx, providerID)
	if err != nil {
		t.Fatalf("键缺失下读快照不应报错: %v", err)
	}
	if missing.ConsecutiveOpenCount != 0 || missing.ConsecutiveOpenCountChangedAtMS != nil {
		t.Errorf("键缺失应读作出厂闭态（level=0/changedAt=nil），收到 %+v", missing)
	}
}

// TestResetProviderCircuitZeroesLadderFields 钉住复位把阶梯一并归零。
//
// 为什么需要它：复位是 **HSet 覆盖写**而非删键（照 Node 的 persistStateToRedis），
// 只写原有 5 个字段就会把上一轮的级数留在哈希里 → 复位后的供应商被读成「第 2 阶」而状态是 closed。
// 这条用真 Redis 直接调存储层，不需要 PG。
func TestResetProviderCircuitZeroesLadderFields(t *testing.T) {
	client := circuitTestRedis(t)
	ctx := context.Background()
	states, ok := NewRedisCircuitStates(client, nil, nil).(ProviderCircuitStore)
	if !ok {
		t.Fatal("供应商级熔断存储未实现 ProviderCircuitStore")
	}
	const providerID int64 = 987654322

	key := providerCircuitKey(providerID)
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	if err := client.HSet(ctx, key, map[string]string{
		"failureCount":                  "7",
		"lastFailureTime":               strconv.FormatInt(time.Now().UnixMilli(), 10),
		"circuitState":                  "open",
		"circuitOpenUntil":              strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10),
		"halfOpenSuccessCount":          "1",
		"consecutiveOpenCount":          "2",
		"consecutiveOpenCountChangedAt": strconv.FormatInt(time.Now().UnixMilli(), 10),
	}).Err(); err != nil {
		t.Fatalf("写熔断夹具失败: %v", err)
	}

	if err := states.ResetProviderCircuit(ctx, providerID); err != nil {
		t.Fatalf("复位失败: %v", err)
	}
	snapshot, err := states.ProviderCircuit(ctx, providerID)
	if err != nil {
		t.Fatalf("复位后读快照失败: %v", err)
	}
	if snapshot.CircuitState != defaultProviderCircuitState() {
		t.Errorf("复位后应为闭态，收到 %q", snapshot.CircuitState)
	}
	if snapshot.ConsecutiveOpenCount != 0 || snapshot.ConsecutiveOpenCountChangedAtMS != nil {
		t.Errorf("复位后阶梯应归零，收到 level=%d changedAt=%v",
			snapshot.ConsecutiveOpenCount, snapshot.ConsecutiveOpenCountChangedAtMS)
	}
	if window := providerCircuitOpenWindowMinutes(
		snapshot.CircuitOpenUntilMS, snapshot.ConsecutiveOpenCountChangedAtMS, snapshot.ConsecutiveOpenCount,
	); window != nil {
		t.Errorf("复位后不应报窗口时长，收到 %v", window)
	}
}

// TestProviderCircuitProjectionsCarryLadderFields 钉住两个端点的**响应键名**：
// 前端就是按这三个键名读的，改名即静默失效（界面只是不显示，不会报错）。
func TestProviderCircuitProjectionsCarryLadderFields(t *testing.T) {
	changedAt := int64(1_700_000_000_000)
	openUntil := changedAt + 25*int64(time.Minute/time.Millisecond)
	window := int64(25)
	state := "open"
	level := int64(2)

	// ① /providers/health 的投影。
	healthBody, err := json.Marshal(providerCircuitHealth{
		CircuitState:                  state,
		FailureCount:                  9,
		CircuitOpenUntil:              &openUntil,
		ConsecutiveOpenCount:          level,
		ConsecutiveOpenCountChangedAt: &changedAt,
		OpenWindowMinutes:             &window,
	})
	if err != nil {
		t.Fatalf("序列化 health 投影失败: %v", err)
	}
	var health map[string]any
	if err := json.Unmarshal(healthBody, &health); err != nil {
		t.Fatalf("解析 health 投影失败: %v", err)
	}
	for key, want := range map[string]float64{
		"consecutiveOpenCount": 2, "consecutiveOpenCountChangedAt": float64(changedAt), "openWindowMinutes": 25,
	} {
		got, ok := health[key]
		if !ok {
			t.Fatalf("health 投影缺键 %q（实际键：%v）", key, health)
		}
		if got != want {
			t.Errorf("health 的 %q 应为 %v，收到 %v", key, want, got)
		}
	}

	// ② /providers/{id}/circuit-logs 的投影（这一侧的纪律是「读不到一律 nil，不用 0 冒充」）。
	logsBody, err := json.Marshal(circuitLogsState{
		Available:                     true,
		CircuitState:                  &state,
		ConsecutiveOpenCount:          &level,
		ConsecutiveOpenCountChangedAt: &changedAt,
		OpenWindowMinutes:             &window,
	})
	if err != nil {
		t.Fatalf("序列化 circuit-logs 投影失败: %v", err)
	}
	var logs map[string]any
	if err := json.Unmarshal(logsBody, &logs); err != nil {
		t.Fatalf("解析 circuit-logs 投影失败: %v", err)
	}
	for key, want := range map[string]float64{
		"consecutiveOpenCount": 2, "consecutiveOpenCountChangedAt": float64(changedAt), "openWindowMinutes": 25,
	} {
		got, ok := logs[key]
		if !ok {
			t.Fatalf("circuit-logs 投影缺键 %q（实际键：%v）", key, logs)
		}
		if got != want {
			t.Errorf("circuit-logs 的 %q 应为 %v，收到 %v", key, want, got)
		}
	}

	// 读不到时（Available=false）三项必须是 null，而不是 0。
	unavailableBody, err := json.Marshal(circuitLogsState{Available: false})
	if err != nil {
		t.Fatalf("序列化不可读态失败: %v", err)
	}
	var unavailable map[string]any
	if err := json.Unmarshal(unavailableBody, &unavailable); err != nil {
		t.Fatalf("解析不可读态失败: %v", err)
	}
	for _, key := range []string{"consecutiveOpenCount", "consecutiveOpenCountChangedAt", "openWindowMinutes"} {
		if unavailable[key] != nil {
			t.Errorf("Available=false 时 %q 应为 null，收到 %v", key, unavailable[key])
		}
	}
}

// requireLadderColumns 在测试库还没应用 0124 迁移时**跳过**而不是判失败。
//
// 为什么要显式前置：providers 的读路径会 SELECT 这两列
// （circuit_breaker_release_increment / circuit_breaker_max_open_count），
// 列不存在时整条 SQL 报错，端点回 400 `provider.action_failed`——
// 那与「投影写错了」是两件事，混在一起会让人误以为改动有问题。
//
// 修法（需要库主属主权限，集成测试用的账号不是 providers 的属主）：
//
//	DSN=postgres://<owner>:<pw>@127.0.0.1:5432/cch_loadtest bun run db:migrate
func requireLadderColumns(t *testing.T, pools *store.Pools) {
	t.Helper()
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'providers'
		   AND column_name IN ('circuit_breaker_release_increment', 'circuit_breaker_max_open_count')`,
	).Scan(&count); err != nil {
		t.Fatalf("查列失败: %v", err)
	}
	if count < 2 {
		t.Skip("测试库缺 0124 迁移（providers 的等待阶梯两列），" +
			"请先用属主 DSN 跑 bun run db:migrate；跳过投影端到端断言")
	}
}

// TestProvidersHealthProjectsLadderLevel 端到端钉住 /providers/health 的三个阶梯字段：
// 级数与变化时间是**原始**事实（窗口过期也不变），窗口时长由哈希差值给出，级数 0 时为 null。
//
// 需要真 PG（要按可见性取供应商 id）+ 真 Redis；测试库缺 0124 迁移时跳过（见 requireLadderColumns）。
func TestProvidersHealthProjectsLadderLevel(t *testing.T) {
	pools := testPools(t)
	requireLadderColumns(t, pools)
	fixture := seedProviders(t, pools)
	client := circuitTestRedis(t)
	ctx := context.Background()
	now := time.Now()
	key := providerCircuitKey(fixture.enabledID)

	// 第 2 阶、本次窗口 25 分钟、窗口还在走（openUntil 在未来）=> 有效态仍是 open。
	changedAt := now.Add(-10 * time.Minute).UnixMilli()
	openUntil := now.Add(15 * time.Minute).UnixMilli()
	if err := client.HSet(ctx, key, map[string]string{
		"failureCount":                  "9",
		"lastFailureTime":               strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10),
		"circuitState":                  "open",
		"circuitOpenUntil":              strconv.FormatInt(openUntil, 10),
		"halfOpenSuccessCount":          "0",
		"consecutiveOpenCount":          "2",
		"consecutiveOpenCountChangedAt": strconv.FormatInt(changedAt, 10),
	}).Err(); err != nil {
		t.Fatalf("写阶梯夹具失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	router := providersHealthRouter(t, pools, NewRedisCircuitStates(client, nil, nil))
	status, body := circuitCall(t, router, http.MethodGet, "/providers/health", "", true)
	if status != http.StatusOK {
		t.Fatalf("health 应为 200，收到 %d：%.300s", status, body)
	}
	ladder := decodeProvidersHealth(t, body)[strconv.FormatInt(fixture.enabledID, 10)]
	if ladder.CircuitState != "open" {
		t.Errorf("窗口内的 open 应报 open，收到 %q", ladder.CircuitState)
	}
	if ladder.ConsecutiveOpenCount != 2 {
		t.Errorf("consecutiveOpenCount 应为 2，收到 %d", ladder.ConsecutiveOpenCount)
	}
	if ladder.ConsecutiveOpenCountChangedAt == nil || *ladder.ConsecutiveOpenCountChangedAt != changedAt {
		t.Errorf("consecutiveOpenCountChangedAt 应为 %d，收到 %v", changedAt, ladder.ConsecutiveOpenCountChangedAt)
	}
	if ladder.OpenWindowMinutes == nil || *ladder.OpenWindowMinutes != 25 {
		t.Errorf("openWindowMinutes 应为 25，收到 %v", ladder.OpenWindowMinutes)
	}

	// 窗口已过期：有效态转 half-open，但级数与窗口时长是**原始事实**，不应被改写。
	expiredChangedAt := now.Add(-35 * time.Minute).UnixMilli()
	if err := client.HSet(ctx, key, map[string]string{
		"circuitState":                  "open",
		"circuitOpenUntil":              strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10),
		"consecutiveOpenCount":          "3",
		"consecutiveOpenCountChangedAt": strconv.FormatInt(expiredChangedAt, 10),
	}).Err(); err != nil {
		t.Fatalf("写过期窗口夹具失败: %v", err)
	}
	status, body = circuitCall(t, router, http.MethodGet, "/providers/health", "", true)
	if status != http.StatusOK {
		t.Fatalf("health 应为 200，收到 %d：%.300s", status, body)
	}
	expired := decodeProvidersHealth(t, body)[strconv.FormatInt(fixture.enabledID, 10)]
	if expired.CircuitState != "half-open" {
		t.Errorf("窗口过期的 open 应报 half-open，收到 %q", expired.CircuitState)
	}
	if expired.ConsecutiveOpenCount != 3 {
		t.Errorf("半开下仍应报第 3 阶，收到 %d", expired.ConsecutiveOpenCount)
	}
	if expired.OpenWindowMinutes == nil || *expired.OpenWindowMinutes != 34 {
		t.Errorf("openWindowMinutes 应为 34（窗口结束比推进晚 34 分），收到 %v", expired.OpenWindowMinutes)
	}

	// 首次开闸（级数 0）：窗口就是基础时长，报 null 而不是一个噪声数字。
	if err := client.HSet(ctx, key, map[string]string{
		"circuitState":                  "open",
		"circuitOpenUntil":              strconv.FormatInt(now.Add(30*time.Minute).UnixMilli(), 10),
		"consecutiveOpenCount":          "0",
		"consecutiveOpenCountChangedAt": "",
	}).Err(); err != nil {
		t.Fatalf("写首次开闸夹具失败: %v", err)
	}
	status, body = circuitCall(t, router, http.MethodGet, "/providers/health", "", true)
	if status != http.StatusOK {
		t.Fatalf("health 应为 200，收到 %d：%.300s", status, body)
	}
	first := decodeProvidersHealth(t, body)[strconv.FormatInt(fixture.enabledID, 10)]
	if first.ConsecutiveOpenCount != 0 {
		t.Errorf("首次开闸级数应为 0，收到 %d", first.ConsecutiveOpenCount)
	}
	if first.OpenWindowMinutes != nil || first.ConsecutiveOpenCountChangedAt != nil {
		t.Errorf("级数 0 时窗口与变化时间都应为 null，收到 %v / %v",
			first.OpenWindowMinutes, first.ConsecutiveOpenCountChangedAt)
	}

	// Node 形态的哈希（无阶梯两键）=> 0/nil，不报错。
	if err := client.HSet(ctx, key, map[string]string{
		"circuitState":         "open",
		"circuitOpenUntil":     strconv.FormatInt(now.Add(time.Minute).UnixMilli(), 10),
		"failureCount":         "4",
		"halfOpenSuccessCount": "0",
	}).Err(); err != nil {
		t.Fatalf("写 Node 形态夹具失败: %v", err)
	}
	if err := client.HDel(ctx, key, "consecutiveOpenCount", "consecutiveOpenCountChangedAt").Err(); err != nil {
		t.Fatalf("清阶梯两键失败: %v", err)
	}
	status, body = circuitCall(t, router, http.MethodGet, "/providers/health", "", true)
	if status != http.StatusOK {
		t.Fatalf("Node 形态哈希下 health 应为 200，收到 %d：%.300s", status, body)
	}
	legacy := decodeProvidersHealth(t, body)[strconv.FormatInt(fixture.enabledID, 10)]
	if legacy.ConsecutiveOpenCount != 0 || legacy.ConsecutiveOpenCountChangedAt != nil ||
		legacy.OpenWindowMinutes != nil {
		t.Errorf("无阶梯键的旧哈希应报 0/nil/nil，收到 %+v", legacy)
	}
}

// TestCircuitLogsProjectsLadderLevel 端到端钉住 /providers/{id}/circuit-logs 的同一组字段
// （弹窗读的就是这一处；两处必须同源同口径）。
func TestCircuitLogsProjectsLadderLevel(t *testing.T) {
	pools := testPools(t)
	requireLadderColumns(t, pools)
	fixture := seedProviders(t, pools)
	client := circuitTestRedis(t)
	ctx := context.Background()

	changedAt := time.Now().Add(-5 * time.Minute).UnixMilli()
	openUntil := time.Now().Add(30 * time.Minute).UnixMilli()
	key := providerCircuitKey(fixture.enabledID)
	if err := client.HSet(ctx, key, map[string]string{
		"failureCount":                  "3",
		"lastFailureTime":               strconv.FormatInt(time.Now().Add(-time.Minute).UnixMilli(), 10),
		"circuitState":                  "open",
		"circuitOpenUntil":              strconv.FormatInt(openUntil, 10),
		"halfOpenSuccessCount":          "0",
		"consecutiveOpenCount":          "3",
		"consecutiveOpenCountChangedAt": strconv.FormatInt(changedAt, 10),
	}).Err(); err != nil {
		t.Fatalf("写阶梯夹具失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	router := circuitLogsRouter(t, pools, NewRedisCircuitStates(client, nil, nil))
	status, _, payload := circuitLogsGet(t, router, fixture.enabledID, "")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}
	if payload.Circuit.ConsecutiveOpenCount == nil || *payload.Circuit.ConsecutiveOpenCount != 3 {
		t.Errorf("consecutiveOpenCount 应为 3，收到 %v", payload.Circuit.ConsecutiveOpenCount)
	}
	if payload.Circuit.ConsecutiveOpenCountChangedAt == nil ||
		*payload.Circuit.ConsecutiveOpenCountChangedAt != changedAt {
		t.Errorf("consecutiveOpenCountChangedAt 应为 %d，收到 %v",
			changedAt, payload.Circuit.ConsecutiveOpenCountChangedAt)
	}
	if payload.Circuit.OpenWindowMinutes == nil || *payload.Circuit.OpenWindowMinutes != 35 {
		t.Errorf("openWindowMinutes 应为 35，收到 %v", payload.Circuit.OpenWindowMinutes)
	}
}
