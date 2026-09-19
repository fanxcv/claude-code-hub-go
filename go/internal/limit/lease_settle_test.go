package limit

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
)

// 本文件钉住「生产入口」这一侧的租约结算：`Service.SettleLeases` 把终态结算器交来的
// 成本扣到判定时用过的切片上（判定侧写计划见 limit.go 的 Check，接线见 cmd/cchd 与 dataplane）。
//
// 纯单测覆盖「计划 → 键位」的映射；Redis 行为（扣减、幂等、fail-open）单列集成用例，
// 未注入 CCH_TEST_REDIS_URL 时跳过。

// leaseSettlementTestPlan 是一份典型的**判定实际用过**的切片计划：key 5h(rolling)、
// key daily(fixed)、user 5h(rolling)。
//
// 为什么不再按「主体各四窗口」构造：计划记的是真正用过的切片（见 pctx.LeaseSettlementPlan），
// 结算也只扣这些——按主体展开会去扣没参与本次判定的窗口。
func leaseSettlementTestPlan() pctx.LeaseSettlementPlan {
	var plan pctx.LeaseSettlementPlan
	plan.Add(pctx.LeaseSettlementTarget{Entity: pctx.LeaseSettlementEntityKey, ID: 7, Window: pctx.LeaseSettlementWindow5h, ResetMode: string(ResetRolling)})
	plan.Add(pctx.LeaseSettlementTarget{Entity: pctx.LeaseSettlementEntityKey, ID: 7, Window: pctx.LeaseSettlementWindowDaily, ResetMode: string(ResetFixed)})
	plan.Add(pctx.LeaseSettlementTarget{Entity: pctx.LeaseSettlementEntityUser, ID: 9, Window: pctx.LeaseSettlementWindow5h, ResetMode: string(ResetRolling)})
	return plan
}

// 没有切片的主体不生成键位：带着 id=0 去查会多出四次恒为 missing 的噪声。
func TestBuildLeaseSettlementTargetsSkipsAbsentEntities(t *testing.T) {
	targets := buildLeaseSettlementTargets(SettleLeaseInput{
		Key:  LeaseSettlementEntity{ID: 7, Reset5h: ResetRolling, ResetDaily: ResetFixed},
		User: LeaseSettlementEntity{ID: 9, Reset5h: ResetRolling, ResetDaily: ResetFixed},
	})
	if len(targets) != 8 {
		t.Fatalf("两个主体各四个窗口，应生成 8 个键位，实际 %d", len(targets))
	}
	for _, target := range targets {
		if target.entity == EntityProvider {
			t.Fatalf("没有切片的 provider 维度不应生成键位: %+v", target)
		}
	}
	// 顺序即 KEYS 顺序：主体外层（key 先于 user）、窗口内层。
	if targets[0].entity != EntityKey || targets[0].window != LeaseWindow5h || targets[0].resetMode != ResetRolling {
		t.Errorf("首位应为 key/5h/rolling，实际 %+v", targets[0])
	}
	if targets[1].entity != EntityKey || targets[1].window != LeaseWindowDaily || targets[1].resetMode != ResetFixed {
		t.Errorf("次位应为 key/daily/fixed，实际 %+v", targets[1])
	}
	if targets[4].entity != EntityUser || targets[4].window != LeaseWindow5h {
		t.Errorf("第五位应转到 user/5h，实际 %+v", targets[4])
	}
	// 5h/daily 带模式后缀，周/月不带：键名不一致就等于扣到另一份键上。
	if got := BuildLeaseKey(targets[0].entity, targets[0].id, targets[0].window, targets[0].resetMode); got != "lease:key:7:5h:rolling" {
		t.Errorf("5h 键名不符: %s", got)
	}
	if got := BuildLeaseKey(targets[3].entity, targets[3].id, targets[3].window, targets[3].resetMode); got != "lease:key:7:monthly" {
		t.Errorf("月窗口键名不符: %s", got)
	}
}

// P1 回归：取值域外的目标不得构造出租约键，也不得静默丢弃——必须计入 dropped。
//
// 为什么单列这条：租约键把取值写进键名（主体/窗口/重置模式），一串取值域外的字符会拼出
// 一份「判定与结算自洽、但谁也不会再写」的键，账面只表现为 missing。这里要求它在成键之前
// 就被拦下，并计入 dropped（调用方据此留痕），而不是当成「没有切片要扣」。
func TestLeaseSettlementTargetsFromPlanDropsOutOfDomainValues(t *testing.T) {
	validTarget := pctx.LeaseSettlementTarget{
		Entity: pctx.LeaseSettlementEntityKey, ID: 7,
		Window: pctx.LeaseSettlementWindow5h, ResetMode: string(ResetRolling),
	}
	cases := []struct {
		name   string
		target pctx.LeaseSettlementTarget
	}{
		{"未知主体", pctx.LeaseSettlementTarget{Entity: "team", ID: 7, Window: pctx.LeaseSettlementWindow5h, ResetMode: string(ResetRolling)}},
		{"未知窗口", pctx.LeaseSettlementTarget{Entity: pctx.LeaseSettlementEntityKey, ID: 7, Window: "yearly", ResetMode: string(ResetRolling)}},
		{"取值域外的重置模式", pctx.LeaseSettlementTarget{Entity: pctx.LeaseSettlementEntityKey, ID: 7, Window: pctx.LeaseSettlementWindow5h, ResetMode: "bogus"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var plan pctx.LeaseSettlementPlan
			if !plan.Add(testCase.target) {
				t.Fatalf("结构完整的目标应入计划（取值域校验在结算侧）: %+v", testCase.target)
			}
			targets, dropped := leaseSettlementTargetsFromPlan(plan)
			if len(targets) != 0 {
				t.Fatalf("取值域外的目标不得成键: %+v", targets)
			}
			if dropped != 1 {
				t.Fatalf("取值域外的目标必须计入 dropped（调用方据此留痕），实际 %d", dropped)
			}
		})
	}

	// 合法目标不受影响：同计划里合法的一枚照旧成键，且键名里不会出现取值域外的字符串。
	var plan pctx.LeaseSettlementPlan
	plan.Add(validTarget)
	plan.Add(pctx.LeaseSettlementTarget{Entity: pctx.LeaseSettlementEntityKey, ID: 7, Window: pctx.LeaseSettlementWindow5h, ResetMode: "bogus"})
	targets, dropped := leaseSettlementTargetsFromPlan(plan)
	if len(targets) != 1 || dropped != 1 {
		t.Fatalf("合法目标应保留、非法目标应丢弃，实际 targets=%+v dropped=%d", targets, dropped)
	}
	key := BuildLeaseKey(targets[0].entity, targets[0].id, targets[0].window, targets[0].resetMode)
	if key != "lease:key:7:5h:rolling" {
		t.Fatalf("合法目标的键名不符: %s", key)
	}
	if strings.Contains(key, "bogus") {
		t.Fatalf("取值域外的模式不得出现在租约键里: %s", key)
	}
}

// 判定侧：取值域外的维度不得入计划，且必须留一条可检索的日志（不静默少扣）。
//
// 为什么日志也算验收面：这一维不入计划就永远不会被结算，租约余额会比实际更宽，
// 而它在账面上没有任何其他痕迹——日志是唯一的排查入口。
func TestRememberLeaseTargetDropsOutOfDomainDimensions(t *testing.T) {
	cases := []struct {
		name      string
		dimension costDimension
	}{
		{"未知主体", costDimension{entity: "team", id: 7, period: Period5h, resetMode: ResetRolling}},
		{"主体 id 非正", costDimension{entity: EntityKey, id: 0, period: Period5h, resetMode: ResetRolling}},
		{"未知窗口", costDimension{entity: EntityKey, id: 7, period: "yearly", resetMode: ResetRolling}},
		{"取值域外的重置模式", costDimension{entity: EntityKey, id: 7, period: Period5h, resetMode: "bogus"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var sink bytes.Buffer
			var plan pctx.LeaseSettlementPlan
			rememberLeaseTarget(logx.New(&sink), &plan, testCase.dimension)
			if !plan.Empty() {
				t.Fatalf("取值域外的维度不得入计划: %+v", plan.Targets)
			}
			if !strings.Contains(sink.String(), "limit.lease.plan_target_dropped") {
				t.Fatalf("丢弃必须留痕，实际日志: %q", sink.String())
			}
		})
	}

	var sink bytes.Buffer
	var plan pctx.LeaseSettlementPlan
	rememberLeaseTarget(logx.New(&sink), &plan, costDimension{entity: EntityKey, id: 7, period: Period5h, resetMode: ResetRolling})
	if len(plan.Targets) != 1 {
		t.Fatalf("合法维度应入计划，实际 %+v", plan.Targets)
	}
	if strings.Contains(sink.String(), "plan_target_dropped") {
		t.Fatalf("合法维度不得产生丢弃日志: %q", sink.String())
	}
}

// settleLeaseTestKeys 列出本用例会用到的全部键（含结算标记），供清理与断言共用。
func settleLeaseTestKeys(requestIDs ...string) []string {
	keys := make([]string, 0, 8+len(requestIDs))
	for _, entity := range []struct {
		entity Entity
		id     int64
	}{{EntityKey, 7}, {EntityUser, 9}} {
		for _, window := range LeaseWindows {
			for _, mode := range []ResetMode{ResetRolling, ResetFixed} {
				keys = append(keys, BuildLeaseKey(entity.entity, entity.id, window, mode))
			}
		}
	}
	for _, id := range requestIDs {
		keys = append(keys, leaseSettlementMarkerPrefix+id)
	}
	return keys
}

// writeLease 写一份租约（带 TTL：Lua 把 ttl<=0 视为没有租约）。
func writeLease(t *testing.T, service *LeaseService, key string, remaining float64) {
	t.Helper()
	body, err := SerializeLease(BudgetLease{
		EntityType: "key", EntityID: 7, Window: string(LeaseWindow5h), ResetMode: "rolling",
		ResetTime: "04:00", SnapshotAtMS: time.Now().UnixMilli(), CurrentUsage: 0,
		LimitAmount: 10, RemainingBudget: remaining, TTLSeconds: 300,
	})
	if err != nil {
		t.Fatalf("序列化租约失败: %v", err)
	}
	if err := service.client.Raw().SetEx(context.Background(), key, body, 5*time.Minute).Err(); err != nil {
		t.Fatalf("写入租约 %s 失败: %v", key, err)
	}
}

// readLeaseRemaining 读一份租约的余额；键不存在时返回 false。
func readLeaseRemaining(t *testing.T, service *LeaseService, key string) (float64, bool) {
	t.Helper()
	raw, err := service.client.Raw().Get(context.Background(), key).Result()
	if err != nil {
		return 0, false
	}
	lease, ok := DeserializeLease(raw)
	if !ok {
		t.Fatalf("租约正文不可解析: %s", raw)
	}
	return lease.RemainingBudget, true
}

// P1 回归（原为错误行为的钉子）：租约服务就绪但**每一维都回退了账本**（decided=false）时，
// 不得写下任何切片。
//
// 老实现按「租约服务可用」开局粗记一份完整计划，于是 Redis 里只要有旧租约，终态结算就会去
// 扣掉本次根本没参与判定的切片（误扣）。这里用不可达的 Redis 造出「就绪但读写失败」——
// 判定本身 fail-open 退回账本路径，计划必须是空的。
func TestCheckDoesNotRememberLeasesWhenJudgmentFellBackToLedger(t *testing.T) {
	plan := checkPlanFor(t, deadRedisClient(t), &fakeQuotaSource{
		key: KeyQuota{
			KeyID: 7, UserID: 9,
			Limit5hUSD: floatPtr(5), Limit5hResetMode: ResetFixed,
			LimitDailyUSD:  floatPtr(2),
			LimitWeeklyUSD: floatPtr(10),
		},
		user: UserQuota{UserID: 9, Limit5hUSD: floatPtr(5), LimitDailyUSD: floatPtr(2)},
	})
	if !plan.Empty() {
		t.Fatalf("判定全部回退账本时不得写切片计划: %+v", plan.Targets)
	}
}

// 正向：真正以租约判定的维度必须逐份记下来（主体 + 窗口 + 生效模式），结算靠它落键。
//
// 这里要求 Redis 可用：不可用时每一维都会回退账本，这条用例就失去意义。
// 夹具只给 5h + 固定窗口：固定 5h 的权威用量就在 Redis 的窗口键里，刷新不需要账本；
// rolling 与更长窗口的回刷要回表，那是另一个依赖面，不是本用例要证的事。
func TestCheckRemembersJudgedLeaseTargets(t *testing.T) {
	client := integrationClient(t)
	cleanupKeys(t, client, settleLeaseTestKeys()...)
	service, err := New(Config{
		Quotas: &fakeQuotaSource{
			key:  KeyQuota{KeyID: 7, UserID: 9, Limit5hUSD: floatPtr(5), Limit5hResetMode: ResetFixed},
			user: UserQuota{UserID: 9, Limit5hUSD: floatPtr(5), Limit5hResetMode: ResetFixed},
		},
		Redis: client,
		LeaseSettings: &leaseSettingsStub{settings: QuotaLeaseSettings{
			RefreshIntervalSeconds: 60,
			Percent5h:              0.05,
			PercentDaily:           0.05,
			PercentWeekly:          0.05,
			PercentMonthly:         0.05,
		}},
	})
	if err != nil {
		t.Fatalf("组装限流服务失败: %v", err)
	}
	req, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}
	req.SetAuth(pctx.AuthState{KeyID: 7, UserID: 9})
	if _, err := service.Check(context.Background(), req); err != nil {
		t.Fatalf("判定不应报错: %v", err)
	}
	plan, ok := req.LeaseSettlementPlan()
	if !ok || plan.Empty() {
		t.Fatalf("走租约判定的维度应当入计划: ok=%v targets=%+v", ok, plan.Targets)
	}

	// 两维均以 5h/fixed 判定：计划里就应当只有这两份切片，顺序即判定顺序（KEYS 顺序）。
	want := []pctx.LeaseSettlementTarget{
		{Entity: pctx.LeaseSettlementEntityKey, ID: 7, Window: pctx.LeaseSettlementWindow5h, ResetMode: string(ResetFixed)},
		{Entity: pctx.LeaseSettlementEntityUser, ID: 9, Window: pctx.LeaseSettlementWindow5h, ResetMode: string(ResetFixed)},
	}
	if len(plan.Targets) != len(want) {
		t.Fatalf("应恰好记下两维切片，实际 %+v", plan.Targets)
	}
	for index, expected := range want {
		if plan.Targets[index] != expected {
			t.Errorf("第 %d 份切片不符：应 %+v，实际 %+v", index, expected, plan.Targets[index])
		}
	}
}

// 未装配租约（无 Redis）时不得写计划：否则终态结算会拿着计划去查一圈空键。
func TestCheckDoesNotRememberLeasesWithoutLeaseService(t *testing.T) {
	plan := checkPlanFor(t, nil, &fakeQuotaSource{
		key:  KeyQuota{KeyID: 7, UserID: 9, Limit5hUSD: floatPtr(5), LimitDailyUSD: floatPtr(2)},
		user: UserQuota{UserID: 9, Limit5hUSD: floatPtr(5), LimitDailyUSD: floatPtr(2)},
	})
	if !plan.Empty() {
		t.Fatalf("无租约时不该写下切片计划: %+v", plan.Targets)
	}
}

// checkPlanFor 跑一次带鉴权的 Check，返回它写进上下文的切片计划。
func checkPlanFor(t *testing.T, client *ratelimit.Client, quotas QuotaSource) pctx.LeaseSettlementPlan {
	t.Helper()
	service, err := New(Config{
		Quotas: quotas,
		Redis:  client,
		LeaseSettings: &leaseSettingsStub{settings: QuotaLeaseSettings{
			RefreshIntervalSeconds: 60,
			Percent5h:              0.05,
			PercentDaily:           0.05,
			PercentWeekly:          0.05,
			PercentMonthly:         0.05,
		}},
	})
	if err != nil {
		t.Fatalf("组装限流服务失败: %v", err)
	}
	req, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}
	req.SetAuth(pctx.AuthState{KeyID: 7, UserID: 9})
	if _, err := service.Check(context.Background(), req); err != nil {
		t.Fatalf("判定不应因基础依赖不可达而报错: %v", err)
	}
	plan, _ := req.LeaseSettlementPlan()
	return plan
}

// newLeaseSettleService 组一个走租约判定的服务：本用例只考结算，故限额源用桩。
func newLeaseSettleService(t *testing.T, client *ratelimit.Client) *Service {
	t.Helper()
	service, err := New(Config{
		Quotas: &fakeQuotaSource{},
		Redis:  client,
		LeaseSettings: &leaseSettingsStub{settings: QuotaLeaseSettings{
			RefreshIntervalSeconds: 60,
			Percent5h:              0.05,
			PercentDaily:           0.05,
			PercentWeekly:          0.05,
			PercentMonthly:         0.05,
		}},
	})
	if err != nil {
		t.Fatalf("组装限流服务失败: %v", err)
	}
	if service.leases == nil {
		t.Fatal("租约服务未装配：本用例需要走租约路径")
	}
	return service
}

// 成功结算：成本按计划扣到对应的切片上；额度不足的那片被清零（不再重复超支）；
// 计划里没列的窗口不得被动。
func TestSettleLeasesDecrementsJudgedLeases(t *testing.T) {
	client := integrationClient(t)
	service := newLeaseSettleService(t, client)
	ctx := context.Background()
	cleanupKeys(t, client, settleLeaseTestKeys("req-settle-success")...)

	keyRolling := BuildLeaseKey(EntityKey, 7, LeaseWindow5h, ResetRolling)
	keyDaily := BuildLeaseKey(EntityKey, 7, LeaseWindowDaily, ResetFixed)
	userRolling := BuildLeaseKey(EntityUser, 9, LeaseWindow5h, ResetRolling)
	// key weekly 在计划里不存在：它必须原封不动（哪怕 Redis 里真有这份旧租约）。
	keyWeekly := BuildLeaseKey(EntityKey, 7, LeaseWindowWeekly, ResetFixed)
	writeLease(t, service.leases, keyRolling, 0.5)
	writeLease(t, service.leases, keyDaily, 0.01)
	writeLease(t, service.leases, userRolling, 0.4)
	writeLease(t, service.leases, keyWeekly, 0.3)

	service.SettleLeases(ctx, "req-settle-success", "0.25", leaseSettlementTestPlan())

	if remaining, ok := readLeaseRemaining(t, service.leases, keyRolling); !ok || remaining < 0.24 || remaining > 0.26 {
		t.Errorf("key 5h 应被扣到约 0.25，实际 %v ok=%v", remaining, ok)
	}
	// 不足：结算脚本把余额清零（否则刷新窗口内每个请求都会重复同一笔超支）。
	if remaining, ok := readLeaseRemaining(t, service.leases, keyDaily); !ok || remaining != 0 {
		t.Errorf("key daily 额度不足应清零，实际 %v ok=%v", remaining, ok)
	}
	if remaining, ok := readLeaseRemaining(t, service.leases, userRolling); !ok || remaining < 0.14 || remaining > 0.16 {
		t.Errorf("user 5h 应被扣到约 0.15，实际 %v ok=%v", remaining, ok)
	}
	// 计划里没列的窗口不得被动：这正是「只扣参与过判定的切片」这条约束的判别用例。
	if remaining, ok := readLeaseRemaining(t, service.leases, keyWeekly); !ok || remaining != 0.3 {
		t.Errorf("未参与判定的 key weekly 不得扣减，实际 %v ok=%v", remaining, ok)
	}
	// 没有切片的窗口不得被凭空创建。
	if _, ok := readLeaseRemaining(t, service.leases, BuildLeaseKey(EntityUser, 9, LeaseWindowMonthly, ResetFixed)); ok {
		t.Error("没写过的租约键不该在结算后出现")
	}
}

// 重放（同一请求标记）不得重复扣减：结算路径会被重试调用，幂等必须成立。
func TestSettleLeasesReplayDoesNotDecrementTwice(t *testing.T) {
	client := integrationClient(t)
	service := newLeaseSettleService(t, client)
	ctx := context.Background()
	cleanupKeys(t, client, settleLeaseTestKeys("req-settle-replay")...)

	keyRolling := BuildLeaseKey(EntityKey, 7, LeaseWindow5h, ResetRolling)
	writeLease(t, service.leases, keyRolling, 0.5)

	service.SettleLeases(ctx, "req-settle-replay", "0.2", leaseSettlementTestPlan())
	first, ok := readLeaseRemaining(t, service.leases, keyRolling)
	if !ok || first < 0.29 || first > 0.31 {
		t.Fatalf("首轮应扣到约 0.3，实际 %v ok=%v", first, ok)
	}

	service.SettleLeases(ctx, "req-settle-replay", "0.2", leaseSettlementTestPlan())
	second, ok := readLeaseRemaining(t, service.leases, keyRolling)
	if !ok || second != first {
		t.Fatalf("重放不得再扣：首轮 %v，重放后 %v ok=%v", first, second, ok)
	}
}

// 零成本（以及不可解析的成本文本）不结算：判定侧从未扣减，标记键也不该产生。
func TestSettleLeasesSkipsNonPositiveCost(t *testing.T) {
	client := integrationClient(t)
	service := newLeaseSettleService(t, client)
	ctx := context.Background()
	cleanupKeys(t, client, settleLeaseTestKeys("req-zero-cost", "req-garbage-cost")...)

	keyRolling := BuildLeaseKey(EntityKey, 7, LeaseWindow5h, ResetRolling)
	writeLease(t, service.leases, keyRolling, 0.5)

	service.SettleLeases(ctx, "req-zero-cost", "0", leaseSettlementTestPlan())
	service.SettleLeases(ctx, "req-garbage-cost", "not-a-number", leaseSettlementTestPlan())

	if remaining, ok := readLeaseRemaining(t, service.leases, keyRolling); !ok || remaining != 0.5 {
		t.Errorf("零成本不得扣减，实际 %v ok=%v", remaining, ok)
	}
	for _, requestID := range []string{"req-zero-cost", "req-garbage-cost"} {
		if exists, err := client.Raw().Exists(ctx, leaseSettlementMarkerPrefix+requestID).Result(); err != nil || exists != 0 {
			t.Errorf("请求 %s 不该写下结算标记（exists=%d err=%v）", requestID, exists, err)
		}
	}
}

// 失败路径：Redis 不可用时结算 fail-open，且**不消耗请求标记**——否则真正的重试会被当成
// 「已结算」跳过，那一笔预算就再也不会被扣（判定侧拿着旧余额放行，正是要修的缺口）。
func TestSettleLeasesFailureDoesNotConsumeMarker(t *testing.T) {
	healthy := integrationClient(t)
	service := newLeaseSettleService(t, healthy)
	ctx := context.Background()
	cleanupKeys(t, healthy, settleLeaseTestKeys("req-settle-retry")...)

	keyRolling := BuildLeaseKey(EntityKey, 7, LeaseWindow5h, ResetRolling)
	writeLease(t, service.leases, keyRolling, 0.5)

	// 第一次结算撞上不可达的 Redis：接口没有错误可返，实现内部 fail-open 并留痕。
	dead := newLeaseSettleService(t, deadRedisClient(t))
	dead.SettleLeases(ctx, "req-settle-retry", "0.2", leaseSettlementTestPlan())

	// 重试（同一个请求标记）必须仍然生效：这说明上一次失败没有留下标记。
	service.SettleLeases(ctx, "req-settle-retry", "0.2", leaseSettlementTestPlan())
	if remaining, ok := readLeaseRemaining(t, service.leases, keyRolling); !ok || remaining < 0.29 || remaining > 0.31 {
		t.Errorf("失败后的重试应完成扣减，实际 %v ok=%v", remaining, ok)
	}
}

// 空计划（本次请求未走租约判定）直接返回，不产生任何 Redis 写入。
func TestSettleLeasesSkipsEmptyPlan(t *testing.T) {
	client := integrationClient(t)
	service := newLeaseSettleService(t, client)
	ctx := context.Background()
	cleanupKeys(t, client, settleLeaseTestKeys("req-empty-plan")...)

	service.SettleLeases(ctx, "req-empty-plan", "0.2", pctx.LeaseSettlementPlan{})
	if exists, err := client.Raw().Exists(ctx, leaseSettlementMarkerPrefix+"req-empty-plan").Result(); err != nil || exists != 0 {
		t.Errorf("空计划不该写下结算标记（exists=%d err=%v）", exists, err)
	}
}

// P1 回归（端到端）：Redis 里**仍有**旧租约、但本次判定全部回退账本（decided=false）时，结算不得扣减。
//
// 这是「开局按租约就绪粗记一份计划」会造成的误扣：那些切片没参与本次判定，却会被结算扣掉。
// 构造：Redis 活着且旧租约未过期，但限额被改成与租约不同（强制刷新）且设置源报错 → 刷新失败 →
// 每一维都 decided=false，于是计划为空、余额不动、标记也不该产生。
func TestSettleLeasesDoesNotDecrementWhenJudgmentFellBackToLedger(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	cleanupKeys(t, client, settleLeaseTestKeys("req-fallback")...)

	service, err := New(Config{
		Quotas: &fakeQuotaSource{
			key:  KeyQuota{KeyID: 7, UserID: 9, Limit5hUSD: floatPtr(5)},
			user: UserQuota{UserID: 9, Limit5hUSD: floatPtr(5)},
		},
		Redis:         client,
		LeaseSettings: &leaseSettingsStub{err: errors.New("设置源不可用")},
	})
	if err != nil {
		t.Fatalf("组装限流服务失败: %v", err)
	}
	if service.leases == nil {
		t.Fatal("租约服务未装配：本用例需要「Redis 在、判定回退」这一态")
	}

	// writeLease 写的是 LimitAmount=10 的旧租约；上面的限额是 5，故缓存必被刷新，而刷新会失败。
	keyRolling := BuildLeaseKey(EntityKey, 7, LeaseWindow5h, ResetRolling)
	writeLease(t, service.leases, keyRolling, 0.5)

	req, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}
	req.SetAuth(pctx.AuthState{KeyID: 7, UserID: 9})
	if _, err := service.Check(ctx, req); err != nil {
		t.Fatalf("判定不应因依赖不可达而报错: %v", err)
	}
	plan, _ := req.LeaseSettlementPlan()
	if !plan.Empty() {
		t.Fatalf("判定全部回退账本时不得写切片计划: %+v", plan.Targets)
	}

	service.SettleLeases(ctx, "req-fallback", "0.2", plan)

	if remaining, ok := readLeaseRemaining(t, service.leases, keyRolling); !ok || remaining != 0.5 {
		t.Errorf("未参与判定的旧租约不得被扣减，实际 %v ok=%v", remaining, ok)
	}
	if exists, err := client.Raw().Exists(ctx, leaseSettlementMarkerPrefix+"req-fallback").Result(); err != nil || exists != 0 {
		t.Errorf("空计划不该写下结算标记（exists=%d err=%v）", exists, err)
	}
}

// 并发结算同一标记：余额只减一次（幂等标记 + Lua 单脚本原子性的合口）。
//
// 为什么单独钉：结算与刷新的竞态里，「同一请求被判两次」在并发下才真正暴露——两个调用
// 各自读到相同的 remainingBudget 再各写一次，余额就会被扣两遍。
func TestSettleLeasesConcurrentSameMarkerDecrementsOnce(t *testing.T) {
	client := integrationClient(t)
	service := newLeaseSettleService(t, client)
	ctx := context.Background()
	cleanupKeys(t, client, settleLeaseTestKeys("req-concurrent")...)

	keyRolling := BuildLeaseKey(EntityKey, 7, LeaseWindow5h, ResetRolling)
	writeLease(t, service.leases, keyRolling, 0.5)

	const callers = 4
	var wg sync.WaitGroup
	for index := 0; index < callers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			service.SettleLeases(ctx, "req-concurrent", "0.2", leaseSettlementTestPlan())
		}()
	}
	wg.Wait()

	if remaining, ok := readLeaseRemaining(t, service.leases, keyRolling); !ok || remaining < 0.29 || remaining > 0.31 {
		t.Fatalf("并发结算同一标记只应扣一次（应约 0.3），实际 %v ok=%v", remaining, ok)
	}
}
