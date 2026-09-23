package route

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件钉住**软信号 fail-open**（2026-09-22 的生产 503 修复）：
// 过滤后候选为 0、且排除原因**全部**属软信号时，重新纳入这些候选，请求照常发出。
//
// 为何必须有：某模型只有一家供应商（生产实证 wb），该会话进入 60 秒低速冷却后唯一候选被剔掉
// ⇒ 无候选 ⇒ `POST /v1/chat/completions` 返回 503（30 分钟 33 次）。冷却的意图是让会话
// 「逃到别家」，只有一家时无处可逃，却把唯一候选剔掉——可用性反而比不冷却更差。

// failOpenNow 是夹具的固定时钟：熔断的 circuitOpenUntil 要落在这个点之后才算「窗口内 open」。
var failOpenNow = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// failOpenRedis 同时提供两条链路的替身能力：冷却读走 Pipelined（继承 slowRateRedis），
// 熔断读走 HGetAll（本类型新增）。
//
// 为何不能只用其中一个：本组钉子要能构造「一家冷却 + 一家熔断」的混合，两条判定必须同时可读。
type failOpenRedis struct {
	*slowRateRedis
	stateHashes map[string]map[string]string
}

// HGetAll 语义对齐 health_test.go 的 fakeRedis：键不存在给空 Hash（**不报错**），
// 于是「无状态即视为 closed」这条既有口径不受影响。
func (f *failOpenRedis) HGetAll(_ context.Context, key string) *redis.MapStringStringCmd {
	cmd := redis.NewMapStringStringCmd(context.Background())
	cmd.SetVal(f.stateHashes[key])
	return cmd
}

// newFailOpenRedis 造一个「候选都开监控」的替身：低速冷却只有 monitor=true 的渠道才生效。
func newFailOpenRedis() *failOpenRedis {
	return &failOpenRedis{
		slowRateRedis: &slowRateRedis{values: map[string]string{}},
		stateHashes:   map[string]map[string]string{},
	}
}

// openCircuit 把某家置为「熔断窗口内 open」（硬信号的判据）。
func (f *failOpenRedis) openCircuit(providerID int64) {
	f.stateHashes[providerStateKey(providerID)] = map[string]string{
		"circuitState":     "open",
		"circuitOpenUntil": strconv.FormatInt(failOpenNow.Add(time.Hour).UnixMilli(), 10),
	}
}

// coolDown 写一条低速冷却键（软信号的判据）。
func (f *failOpenRedis) coolDown(sessionID string, keyID, providerID int64) {
	f.values[SlowRateCooldownKey(sessionID, keyID, providerID)] = SlowRateCooldownMarker
}

func newFailOpenSelector(t *testing.T, client redis.UniversalClient, providers ...Provider) *Selector {
	t.Helper()
	source := &stubSource{providers: providers, byID: map[int64]Provider{}}
	for _, p := range providers {
		source.byID[p.ID] = p
	}
	return NewSelector(Options{
		Source: source,
		// 总闸开（Affinity 非 nil）：会话绑定是亲和的一层，总闸关时整层不参与（见 resolve）。
		// 本组夹具里只用它把「绑定短路」这条路径打开，不触发任何 Redis 读写（无 lookup）。
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		// 时钟与夹具的样本基准（slowRateTestNowMS）对齐：否则滑窗里的样本会落在窗外，
		// 计数恒为 0，隔离用例会静默退化成「什么都没发生」。
		SlowRate: NewSlowRateReader(SlowRateOptions{Redis: client, Now: func() time.Time { return time.UnixMilli(slowRateTestNowMS) }}),
		Health:   NewHealthReader(HealthOptions{Redis: client, Now: func() time.Time { return failOpenNow }}),
		Rand:     func() float64 { return 0 },
		Now:      func() time.Time { return time.UnixMilli(slowRateTestNowMS) },
	})
}

// TestSoftSignalFailOpenReinstatesSoleCandidate 钉住 ①：单候选 + 低速冷却 ⇒ **仍成功发出**。
//
// 反证：把 softSignalRejection 改成恒 false（软信号也当硬信号）⇒ 本用例红。
func TestSoftSignalFailOpenReinstatesSoleCandidate(t *testing.T) {
	redisClient := newFailOpenRedis()
	redisClient.coolDown("s1", 7, 1)
	selector := newFailOpenSelector(t, redisClient, slowRateProvider(1))

	result, err := selector.Select(context.Background(), Request{Model: "m1", SessionID: "s1", KeyID: 7})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	// 「仍成功发出」的判据是选出供应商：没有它，数据面走到 no_provider_available ⇒ 503。
	if result.Provider == nil || result.Provider.ID != 1 {
		t.Fatalf("选中 = %v，期望 1（唯一候选虽在本会话冷却中，但无替代可逃，须回退使用）", result.Provider)
	}
	// 回退必须在链上可见：否则事后只能看到「选了那家」，看不出这是回退而非正常选择。
	record, filtered := cooldownRecordOf(result.Context, 1)
	if !filtered {
		t.Fatalf("回退后仍无留痕记录：%+v", result.Context.FilteredProviders)
	}
	if record.Reason != ReasonNoAlternativeFailOpen {
		t.Errorf("理由 = %q，期望 %q", record.Reason, ReasonNoAlternativeFailOpen)
	}
	if record.Details != string(ReasonNoAlternativeFailOpen) {
		t.Errorf("详情 = %q，期望 %q（详情是 i18n 键，与理由同名）", record.Details, string(ReasonNoAlternativeFailOpen))
	}
	if result.Context.AfterHealthCheck != 1 {
		t.Errorf("afterHealthCheck = %d，期望 1（回退后的候选已回到参与池）", result.Context.AfterHealthCheck)
	}
	// 回退**只**改写 filteredProviders 里的理由，不得污染链项自身的 reason。
	//
	// 为何单钉：`usage_ledger` 触发器按**精确词**读 `provider_chain[-1].reason` 判终态与成功率
	// （drizzle/0104 的 fn_is_message_request_finalized / fn_compute_message_request_success_rate_outcome）。
	// 链项 reason 变成回退词，会让该请求被误判成未终态。而 filteredProviders 是被嵌入
	// decisionContext 的子数组，SQL 侧不读它（grep drizzle/ 与 lua/ 无消费点）——两者必须分开。
	if result.Reason != ReasonSelectedInitial {
		t.Errorf("链项 reason = %q，期望 %q（回退词只属 filteredProviders）", result.Reason, ReasonSelectedInitial)
	}
}

// TestSoftSignalFailOpenLeavesAlternativeSelectionIntact 钉住 ②：多候选 + 冷却 ⇒ 仍按冷却剔除、改选他家。
//
// 这是既有语义的对照面：回退只在「无替代候选」时启动，有替代就不该启动。
func TestSoftSignalFailOpenLeavesAlternativeSelectionIntact(t *testing.T) {
	redisClient := newFailOpenRedis()
	redisClient.coolDown("s1", 7, 2)
	selector := newFailOpenSelector(t, redisClient, slowRateProvider(1), slowRateProvider(2))

	result, err := selector.Select(context.Background(), Request{Model: "m1", SessionID: "s1", KeyID: 7})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 1 {
		t.Fatalf("选中 = %v，期望 1（2 在冷却中，应逃到 1）", result.Provider)
	}
	if got := reasonOf(t, result.Context, 2); got != ReasonSlowRateCooldown {
		t.Errorf("供应商 2 的理由 = %q，期望 %q（有替代候选时冷却语义不变）", got, ReasonSlowRateCooldown)
	}
	for _, record := range result.Context.FilteredProviders {
		if record.Reason == ReasonNoAlternativeFailOpen {
			t.Errorf("有替代候选时不应出现回退留痕：%+v", record)
		}
	}
}

// TestHardSignalRejectionUnchangedForSoleCandidate 钉住 ③：单候选 + **熔断** ⇒ 行为不变（仍无候选）。
//
// 反证：把 softSignalRejection 改成恒 true（硬信号也 fail-open）⇒ 本用例红。
func TestHardSignalRejectionUnchangedForSoleCandidate(t *testing.T) {
	redisClient := newFailOpenRedis()
	redisClient.openCircuit(1)
	selector := newFailOpenSelector(t, redisClient, slowRateProvider(1))

	result, err := selector.Select(context.Background(), Request{Model: "m1", SessionID: "s1", KeyID: 7})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider != nil {
		t.Fatalf("选中 = %v，期望 nil（唯一候选熔断时不得回退，行为须与改前逐字一致）", result.Provider)
	}
	if result.Context.AfterHealthCheck != 0 {
		t.Errorf("afterHealthCheck = %d，期望 0（硬信号剔除不进回退）", result.Context.AfterHealthCheck)
	}
	if got := reasonOf(t, result.Context, 1); got != ReasonCircuitOpen {
		t.Errorf("理由 = %q，期望 %q（不得被改写成回退理由）", got, ReasonCircuitOpen)
	}
}

// TestMixedSoftAndHardSignalsDoNotFailOpen 钉住 ④：一家冷却 + 一家熔断 ⇒ **不回退**。
//
// 为何如此定：只要场上有任一硬信号，就说明「无候选」不只是本会话的回避造成的。把软信号那家
// 放行会让请求打向一家本可逃开的慢渠道，而硬信号那家仍不可用——回退并不能恢复可用性，
// 只会把「明确的失败」换成「可能更慢的失败」。
func TestMixedSoftAndHardSignalsDoNotFailOpen(t *testing.T) {
	redisClient := newFailOpenRedis()
	redisClient.coolDown("s1", 7, 1)
	redisClient.openCircuit(2)
	selector := newFailOpenSelector(t, redisClient, slowRateProvider(1), slowRateProvider(2))

	result, err := selector.Select(context.Background(), Request{Model: "m1", SessionID: "s1", KeyID: 7})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider != nil {
		t.Fatalf("选中 = %v，期望 nil（软硬混合时不回退）", result.Provider)
	}
	for _, record := range result.Context.FilteredProviders {
		if record.Reason == ReasonNoAlternativeFailOpen {
			t.Errorf("软硬混合时不应出现回退留痕：%+v", record)
		}
	}
	if got := reasonOf(t, result.Context, 1); got != ReasonSlowRateCooldown {
		t.Errorf("软信号那家的理由 = %q，期望保持 %q（未回退即不改写）", got, ReasonSlowRateCooldown)
	}
}

// TestSoftSignalFailOpenNeedsRejectedCandidate 钉住空真边界：压根没有候选时不得触发回退。
//
// 「全部被排除的原因都属软信号」在空集上恒真；不加门就会把「无事发生」误报成「已回退」。
func TestSoftSignalFailOpenNeedsRejectedCandidate(t *testing.T) {
	selector := newFailOpenSelector(t, newFailOpenRedis())

	result, err := selector.Select(context.Background(), Request{Model: "m1", SessionID: "s1", KeyID: 7})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider != nil {
		t.Fatalf("选中 = %v，期望 nil（无候选可选）", result.Provider)
	}
	for _, record := range result.Context.FilteredProviders {
		if record.Reason == ReasonNoAlternativeFailOpen {
			t.Errorf("无被排除候选时不应出现回退留痕：%+v", record)
		}
	}
}

// TestSoftSignalRejectionClassifiesOnlySelfInflictedAvoidance 钉住软信号分界表本身。
//
// 为何逐条列全：这张表是「无替代候选时能不能放行」的唯一判据来源。多放一条（例如把熔断也
// 算软信号）= 上游已知故障仍被打过去；少放一条（低速冷却不算）= 本缺陷复发（唯一候选被剔 ⇒ 503）。
func TestSoftSignalRejectionClassifiesOnlySelfInflictedAvoidance(t *testing.T) {
	soft := []Reason{ReasonSlowRateCooldown}
	hard := []Reason{
		ReasonCircuitOpen,
		ReasonProviderErrorCooldown,
		ReasonRateLimited,
		ReasonDisabled,
		ReasonModelNotAllowed,
		ReasonScheduleInactive,
		ReasonExcluded,
	}
	for _, reason := range soft {
		if !softSignalRejection(reason) {
			t.Errorf("%q 应判为软信号（本网关自己加的回避，无替代候选时可放行）", reason)
		}
	}
	for _, reason := range hard {
		if softSignalRejection(reason) {
			t.Errorf("%q 应判为硬信号（上游已知故障或需改配置，不得因无替代候选而放行）", reason)
		}
	}
}
