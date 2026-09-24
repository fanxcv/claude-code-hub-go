package route

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件钉住「低速隔离」的五条不变量（用户 2026-09-22 交办）：
//
//	① 无替代候选时不因隔离而失败（软信号 fail-open）
//	② 存量惩罚数据不被当作隔离（可区分来源的标记）
//	③ 探针租约唯一（同一组合最多 1 个在飞）
//	④ 阶梯只升不降，任何慢样本回到 0%（由写侧删连续干净计数实现）
//	⑤ 亲和不得绕过隔离（闸门放行的档也不得被亲和粘住）
//
// 每条都配了反证手法（见各用例注释），改动对应的判据即变红。

// quarantineRedis 造「某组合被新机制标慢」的替身：状态 + 可用基线 + 滑窗三件套。
//
// quarantined 为假时**不写**隔离标记，正是「存量状态」（旧代码写的、只有 penalty 的那种）。
func quarantineRedis(t *testing.T, providerID int64, modelKey string, quarantined bool, streak int) *failOpenRedis {
	t.Helper()
	client := newFailOpenRedis()
	if quarantined {
		client.values[SlowRateStateKey(providerID, modelKey)] = slowRateStateValueQuarantined(t, 20, 30, 3, 10, 30)
	} else {
		client.values[SlowRateStateKey(providerID, modelKey)] = slowRateStateValueWithParams(t, 20, 30, 3, 10, 30)
	}
	client.values[SlowRateBaselineKey(providerID, modelKey)] = slowRateBaselineValue(t, "primary")
	if client.zsets == nil {
		client.zsets = map[string][]redis.Z{}
	}
	client.zsets[SlowRateSamplesKey(providerID, modelKey)] = slowRateSamples(6, 60_000)
	if streak > 0 {
		client.values[SlowRateCleanStreakKey(providerID, modelKey)] = strconv.Itoa(streak)
	}
	return client
}

// holdProbeLease 让该组合的探针租约**已被占**（读侧因此不会尝试取用）。
func holdProbeLease(client *failOpenRedis, providerID int64, modelKey string) {
	client.values[SlowProbeLeaseKey(providerID, modelKey)] = "someone-else"
}

// TestQuarantineLadderPermille 钉住阶梯的三档取值（④ 的读侧面）。
//
// 反证：把 quarantinePermilleForStreak 改成恒返回 0 ⇒ 下面三档断言全红。
func TestQuarantineLadderPermille(t *testing.T) {
	for _, tc := range []struct {
		streak int
		want   int
	}{
		{0, 0},
		{QuarantineAdmissionTenFrom - 1, 0},
		{QuarantineAdmissionTenFrom, quarantineAdmissionTenPermille},
		{QuarantineAdmissionThirtyFrom - 1, quarantineAdmissionTenPermille},
		{QuarantineAdmissionThirtyFrom, quarantineAdmissionThirtyPermille},
		{QuarantineAdmissionThirtyFrom + 100, quarantineAdmissionThirtyPermille},
	} {
		if got := quarantinePermilleForStreak(tc.streak); got != tc.want {
			t.Errorf("streak=%d 的放行比例 = %d，期望 %d", tc.streak, got, tc.want)
		}
	}
}

// TestQuarantineAdmitsGate 钉住准入闸门的边界与确定性。
//
// 反证：把 quarantineAdmits 的 `admissionPermille <= 0` 早退去掉（让 0% 也走哈希）⇒ 第一条断言红；
// 把时间桶去掉（逐请求随机）⇒ 确定性那条红。
func TestQuarantineAdmitsGate(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if quarantineAdmits(0, 1, 7, "s1", now) {
		t.Fatal("0% 放行比例不得放行任何请求（隔离到底）")
	}
	if !quarantineAdmits(quarantineAdmissionFullPermille, 1, 7, "s1", now) {
		t.Fatal("100% 放行比例必须放行")
	}
	// 与其本身在同一时间桶内的重复判定必须同结论（否则同一会话会在十秒内反复进出隔离渠道）。
	first := quarantineAdmits(quarantineAdmissionTenPermille, 1, 7, "s1", now)
	for offset := 0; offset < 5; offset++ {
		again := quarantineAdmits(quarantineAdmissionTenPermille, 1, 7, "s1", now.Add(time.Duration(offset)*time.Second))
		if again != first {
			t.Fatalf("同一时间桶内结论不一致：offset=%ds 得 %v，首次得 %v", offset, again, first)
		}
	}
	// 10% 档必须真的只放行一小部分（千分比口径），而不是「要么全放要么全挡」。
	admitted := 0
	for keyID := int64(0); keyID < 500; keyID++ {
		if quarantineAdmits(quarantineAdmissionTenPermille, 1, keyID, "s1", now) {
			admitted++
		}
	}
	if admitted == 0 || admitted >= 500 {
		t.Fatalf("10%% 档放行 %d/500，期望少量但非零", admitted)
	}
}

// TestQuarantineExcludesProviderWhenAlternativesExist 钉住「有替代即真排除」。
//
// 反证：把 filter.go 里 `in.quarantined[p.ID]` 那一段删掉 ⇒ provider 1 不再被排除，本用例红。
func TestQuarantineExcludesProviderWhenAlternativesExist(t *testing.T) {
	client := quarantineRedis(t, 1, "m1", true, 0)
	holdProbeLease(client, 1, "m1")
	selector := newFailOpenSelector(t, client, slowRateProvider(1), slowRateProvider(2))

	result, err := selector.Select(context.Background(), Request{Model: "m1", SessionID: "s1", KeyID: 7})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 2 {
		t.Fatalf("选中 = %v，期望 2（1 被隔离且有替代候选）", result.Provider)
	}
	if got := reasonOf(t, result.Context, 1); got != ReasonSlowRateQuarantine {
		t.Errorf("供应商 1 的理由 = %q，期望 %q", got, ReasonSlowRateQuarantine)
	}
}

// TestQuarantineKeepsHistoricalPenaltyData 钉住 ②：只有 penalty、没有隔离标记的状态**不**被真排除。
//
// 依据：真排除比降优先级激进，不能因为一次上线就把存量慢渠道全部改成不再接新流量。
//
// 反证：把读侧判据里的 `state.quarantined` 去掉（改成只判 penalty>0）⇒ 本用例红。
func TestQuarantineKeepsHistoricalPenaltyData(t *testing.T) {
	client := quarantineRedis(t, 1, "m1", false, 0)
	selector := newFailOpenSelector(t, client, slowRateProvider(1), slowRateProvider(2))

	result, err := selector.Select(context.Background(), Request{Model: "m1", SessionID: "s1", KeyID: 7})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	for _, record := range result.Context.FilteredProviders {
		if record.Reason == ReasonSlowRateQuarantine {
			t.Fatalf("存量状态不得被当作隔离：%+v", record)
		}
	}
	// 存量数据的既有语义是「降优先级」：它照旧参与选路（此处只断言未被真排除）。
	if result.Context.AfterHealthCheck != 2 {
		t.Errorf("afterHealthCheck = %d，期望 2（两家都仍参与竞争）", result.Context.AfterHealthCheck)
	}
}

// TestQuarantineFailsOpenForSoleCandidate 钉住 ①：唯一候选被隔离时**仍成功发出**。
//
// 用户裁决：无替代时不要因隔离不成事；拦到裁决点判慢再返回可重试 503 是速率闸门的事。
//
// 反证：把 ReasonSlowRateQuarantine 从 softSignalRejection 摘掉 ⇒ 本用例按期 503（Provider 为 nil）。
func TestQuarantineFailsOpenForSoleCandidate(t *testing.T) {
	client := quarantineRedis(t, 1, "m1", true, 0)
	selector := newFailOpenSelector(t, client, slowRateProvider(1))

	result, err := selector.Select(context.Background(), Request{Model: "m1", SessionID: "s1", KeyID: 7})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 1 {
		t.Fatalf("选中 = %v，期望 1（唯一候选被隔离时必须放行，否则可用性更差）", result.Provider)
	}
	if got := reasonOf(t, result.Context, 1); got != ReasonNoAlternativeFailOpen {
		t.Errorf("理由 = %q，期望 %q（回退必须在链上可见）", got, ReasonNoAlternativeFailOpen)
	}
}

// TestSlowProbeLeaseIsExclusiveAndDirected 钉住 ③：租约唯一、命中者被**定向**到被隔离的渠道。
//
// 单飞的完整语义（续租、终态释放、compare-and-delete）另见 probe_lease_test.go；本用例只钉
// 「选路层第二次拿不到」这一面。
//
// 反证：把 AcquireSlowProbe 的 `SetNX` 改成 `Set` ⇒ 第二次选路也会拿到租约，第二条断言红。
func TestSlowProbeLeaseIsExclusiveAndDirected(t *testing.T) {
	client := quarantineRedis(t, 1, "m1", true, 0)
	selector := newFailOpenSelector(t, client, slowRateProvider(1), slowRateProvider(2))
	// 请求级 ctx：探针租约的续租 goroutine 随它退出（见 keepSlowProbeAlive）；用
	// context.Background() 会让该 goroutine 随测试进程一直存活。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first, err := selector.Select(ctx, Request{Model: "m1", SessionID: "s1", KeyID: 7})
	if err != nil {
		t.Fatalf("首次选路失败: %v", err)
	}
	if first.Provider == nil || first.Provider.ID != 1 {
		t.Fatalf("首次选中 = %v，期望 1（命中探针租约应被定向到被隔离的那家）", first.Provider)
	}
	if first.SlowProbe == nil {
		t.Fatal("首次选路未标记探针：缓存的探针请求无法被数据面识别（竞速与释放都依赖它）")
	}
	if first.SlowProbe.ProviderID != 1 || first.SlowProbe.ModelKey != "m1" {
		t.Errorf("租约作用域 = (%d,%q)，期望 (1,m1)", first.SlowProbe.ProviderID, first.SlowProbe.ModelKey)
	}
	// 探针命中仍要留痕，以便排查隔离渠道为何获选。
	if first.SessionBindingBypass != SessionBindingBypassTransient {
		t.Errorf("探针选的 bypass = %v，期望 transient", first.SessionBindingBypass)
	}

	second, err := selector.Select(ctx, Request{Model: "m1", SessionID: "s1", KeyID: 7})
	if err != nil {
		t.Fatalf("第二次选路失败: %v", err)
	}
	if second.SlowProbe != nil {
		t.Fatal("租约期内第二次选路仍拿到探针：同一组合最多 1 个在飞的上界失效")
	}
	if second.Provider == nil || second.Provider.ID != 2 {
		t.Fatalf("第二次选中 = %v，期望 2（租约被占，隔离生效，改选替代候选）", second.Provider)
	}
}

// TestSlowProbeLeaseFailOpenOnRedisError 钉住「Redis 故障 ⇒ 不探但不失败」。
//
// 反向面（可观测 warn）由 slowRateReaderWithLogger 的既有用例族覆盖；本用例只钉行为侧。
func TestSlowProbeLeaseFailOpenOnRedisError(t *testing.T) {
	reader := NewSlowRateReader(SlowRateOptions{Redis: &slowRateFailingRedis{}})
	if grant := reader.AcquireSlowProbe(context.Background(), 1, "m1", "holder"); grant != nil {
		t.Fatalf("Redis 故障时不得发放租约，实得 %+v", grant)
	}
}

// TestQuarantineBlocksSessionBinding 钉住 ⑤：会话绑定不得绕过隔离。
//
// 反证：把 select.go 里 `&& !quarantineExcluded[bound.ID]` 去掉 ⇒ 被粘住的会话选回被隔离的 1，本用例红。
func TestQuarantineBlocksSessionBinding(t *testing.T) {
	client := quarantineRedis(t, 1, "m1", true, 0)
	holdProbeLease(client, 1, "m1")
	selector := newFailOpenSelector(t, client, slowRateProvider(1), slowRateProvider(2))

	result, err := selector.Select(context.Background(), Request{
		Model:          "m1",
		SessionID:      "s1",
		KeyID:          7,
		SessionBinding: &SessionBindingSnapshot{ProviderID: 1, Generation: "3"},
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 2 {
		t.Fatalf("选中 = %v，期望 2（会话绑定指向被隔离的 1，不得绕过隔离）", result.Provider)
	}
	// 隔离属临时原因，须留痕；备用成功仍可改绑。
	if result.SessionBindingBypass != SessionBindingBypassTransient {
		t.Errorf("绑定 bypass = %v，期望 transient", result.SessionBindingBypass)
	}
}
