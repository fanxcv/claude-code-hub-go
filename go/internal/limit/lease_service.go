package limit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// LeaseService 是 Node `src/lib/rate-limit/lease-service.ts` 的等价实现：把「DB 权威用量」
// 切片成 Redis 里的**预算租约**，使每请求判定不再回表。
//
// 与既有窗口路径的关系（务必分清，两者不是同一机制）：
//   - 窗口路径（CostWindows + 账本回退）是「实时用量 vs 限额」；
//   - 租约路径是「切给这个实体的一小片预算 vs 本窗口内的实际消耗」，DB 用量按
//     quota_db_refresh_interval_seconds 刷新，窗口内靠结算扣减。
//
// Node 的 rate-limit-guard 与 provider-selector 用的都是**租约**路径（service.ts:1852
// checkCostLimitsWithLease，调用点见 rate-limit-guard.ts:236/282/328/402/479/514/551/588）。
//
// **Go 侧的实际范围**（与上述 Node 事实分清）：只有 Key 与 User 两个周期的限额判定已经切到
// 租约路径（limit.Service.check 的 dimensions 表）；**供应商限额仍走账本路径**
// （provider_cost.go 的 costLimit），故本进程不产生 provider 维度的切片，结算计划里也不会
// 出现 provider（见 pctx.LeaseSettlementPlan）。租约就绪时判定也不会静默回退：
// leaseCostLimit 的 decided=false 只在租约不可用时出现，此时由调用方回落账本路径并留痕。
type LeaseService struct {
	client   *ratelimit.Client
	settings QuotaLeaseSettingsReader
	ledger   LedgerReader
	windows  *CostWindows
	loc      *time.Location
	now      func() time.Time
	log      *logx.Logger

	decrementScript *redis.Script
	settleScript    *redis.Script
}

// leaseSettlementMarkerTTLSeconds 对应 Node `SETTLEMENT_MARKER_TTL_SECONDS`（5 分钟）。
// 标记只用来压住「同一次请求的重放」，故 TTL 取「客户端有界重连周期」量级即可。
const leaseSettlementMarkerTTLSeconds = 5 * 60

// leaseSettlementMarkerPrefix 对应 Node 的 `lease:settlement:{requestId}`。
const leaseSettlementMarkerPrefix = "lease:settlement:"

// leaseSettlementEntityOrder 对应 Node `SETTLEMENT_ENTITY_TYPES`，顺序即 KEYS 顺序，不得重排
// （结算结果的数组下标与之一一对应，前端/日志按位读）。
var leaseSettlementEntityOrder = []LeaseEntity{EntityKey, EntityUser, EntityProvider}

// NewLeaseService 组装租约服务。client 为 nil 或 settings 为 nil 时返回 nil：
// 没有 Redis 就没有租约，没有设置源就不知道该切多少——两者缺一都按「未接线」处理，
// 由调用方退回既有窗口/账本路径，而不是拿半份配置做判定。
func NewLeaseService(
	client *ratelimit.Client,
	settings QuotaLeaseSettingsReader,
	ledger LedgerReader,
	loc *time.Location,
	now func() time.Time,
	logger *logx.Logger,
) *LeaseService {
	if client == nil || client.Raw() == nil || settings == nil {
		return nil
	}
	if loc == nil {
		loc = time.UTC
	}
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = logx.New(nil)
	}
	return &LeaseService{
		client:          client,
		settings:        settings,
		ledger:          ledger,
		windows:         NewCostWindows(client, logger),
		loc:             loc,
		now:             now,
		log:             logger,
		decrementScript: redis.NewScript(decrementLeaseBudgetLua),
		settleScript:    redis.NewScript(settleLeaseBudgetsLua),
	}
}

// Ready 报告租约是否可用（Redis 与设置源都就绪）。
func (l *LeaseService) Ready() bool {
	return l != nil && l.client != nil && l.client.Raw() != nil
}

// GetCostLeaseParams 是一次租约取值的输入，字段对应 Node `GetCostLeaseParams`。
type GetCostLeaseParams struct {
	Entity   LeaseEntity
	EntityID int64
	// KeyHash 是密钥字符串：key 维度的账本聚合法与其它两维不同（按密钥串而非 id）。
	// Node 的 statistics 仓储按 id 聚合，故它没有这个字段；Go 的账本用密钥串，
	// 缺它时 key 维度的刷新会失败并 fail-open（留痕，不静默算成 0）。
	KeyHash     string
	Window      LeaseWindow
	LimitAmount float64
	ResetTime   string
	ResetMode   ResetMode
	CostResetAt *time.Time
}

// GetCostLease 取一份租约：先读 Redis 缓存，命中且未过期则复用；否则回表刷新。
//
// 复用前要过三道「事实已变」检查（Node 同款）：5h 固定窗口已翻窗、限额被改、costResetAt 被改。
// 任一命中即强制刷新——否则租约会把旧窗口的余额带进新窗口，那是「明明重置了还拒绝请求」的来源。
//
// 返回 ok=false 表示本次拿不到租约（Redis 未就绪、缓存损坏、回表失败）：调用方按 Node 的
// fail-open 语义放行该维度并留痕。
func (l *LeaseService) GetCostLease(ctx context.Context, params GetCostLeaseParams) (*BudgetLease, bool) {
	if !l.Ready() {
		return nil, false
	}
	window := params.Window
	resetMode := params.ResetMode
	if resetMode == "" {
		if window == LeaseWindow5h {
			resetMode = ResetRolling
		} else {
			resetMode = ResetFixed
		}
	}
	params.ResetMode = resetMode

	leaseKey := BuildLeaseKey(params.Entity, params.EntityID, window, resetMode)
	raw := l.client.Raw()

	cached, err := raw.Get(ctx, leaseKey).Result()
	if err == nil {
		if lease, valid := DeserializeLease(cached); valid {
			if !IsLeaseExpired(lease, l.now().UnixMilli()) {
				if lease.Window == string(LeaseWindow5h) && lease.ResetMode == string(ResetFixed) &&
					lease.WindowResetAtMS != nil && *lease.WindowResetAtMS <= l.now().UnixMilli() {
					return l.refreshCostLease(ctx, params)
				}
				if lease.LimitAmount != params.LimitAmount {
					return l.refreshCostLease(ctx, params)
				}
				if l.costResetChanged(lease, params.CostResetAt) {
					return l.refreshCostLease(ctx, params)
				}
				return &lease, true
			}
		}
	} else if !errors.Is(err, redis.Nil) {
		l.log.Warn("limit.lease.cache_read_failed", map[string]any{"key": leaseKey, "error": err.Error()})
	}

	return l.refreshCostLease(ctx, params)
}

// costResetChanged 报告 costResetAt 是否与租约快照不同（nil 与 nil 视为相同）。
func (l *LeaseService) costResetChanged(lease BudgetLease, resetAt *time.Time) bool {
	var want *int64
	if resetAt != nil {
		value := resetAt.UnixMilli()
		want = &value
	}
	if want == nil && lease.CostResetAtMS == nil {
		return false
	}
	if want == nil || lease.CostResetAtMS == nil {
		return true
	}
	return *want != *lease.CostResetAtMS
}

// refreshCostLease 从 DB 权威用量重算切片并写回 Redis（对应 Node refreshCostLeaseFromDb）。
func (l *LeaseService) refreshCostLease(ctx context.Context, params GetCostLeaseParams) (*BudgetLease, bool) {
	settings, err := l.settings.QuotaLeaseSettings(ctx)
	if err != nil {
		l.log.Warn("limit.lease.settings_unreadable", map[string]any{"error": err.Error()})
		return nil, false
	}
	settings = settings.Normalize()
	ttlSeconds := settings.RefreshIntervalSeconds
	percent := settings.PercentFor(params.Window)

	now := l.now()
	resetMode := params.ResetMode
	currentUsage := 0.0
	var windowResetAtMS *int64

	if params.Window == LeaseWindow5h && resetMode == ResetFixed {
		// 5h 固定窗口的权威用量就在 Redis 的固定窗口键里（DB 无法按「本窗口」聚合，
		// 因为窗口起点由 Redis 的 TTL 决定）。Node 同款：readFixed5hWindowState。
		state, err := l.windows.Fixed5hWindowState(ctx, params.Entity, params.EntityID, now)
		if err != nil {
			l.log.Warn("limit.lease.fixed5h_state_failed", map[string]any{"error": err.Error()})
			return nil, false
		}
		currentUsage = state.Current
		if state.ResetAt != nil {
			value := state.ResetAt.UnixMilli()
			windowResetAtMS = &value
		}
	} else {
		usage, err := l.queryDBUsage(ctx, params, now)
		if err != nil {
			l.log.Warn("limit.lease.usage_query_failed", map[string]any{
				"entity": string(params.Entity), "window": string(params.Window), "error": err.Error(),
			})
			return nil, false
		}
		currentUsage = usage
	}

	remaining := CalculateLeaseSlice(params.LimitAmount, currentUsage, percent, settings.CapUSD)
	snapshotAtMS := now.UnixMilli()
	lease := BudgetLease{
		EntityType:      string(params.Entity),
		EntityID:        params.EntityID,
		Window:          string(params.Window),
		ResetMode:       string(resetMode),
		ResetTime:       params.ResetTime,
		SnapshotAtMS:    snapshotAtMS,
		CurrentUsage:    currentUsage,
		LimitAmount:     params.LimitAmount,
		RemainingBudget: remaining,
		TTLSeconds:      int64(ttlSeconds),
		WindowResetAtMS: windowResetAtMS,
	}
	if params.CostResetAt != nil {
		value := params.CostResetAt.UnixMilli()
		lease.CostResetAtMS = &value
	}

	payload, err := SerializeLease(lease)
	if err != nil {
		l.log.Warn("limit.lease.serialize_failed", map[string]any{"error": err.Error()})
		return nil, false
	}
	if err := l.client.Raw().SetEx(ctx, BuildLeaseKey(params.Entity, params.EntityID, params.Window, resetMode),
		payload, time.Duration(ttlSeconds)*time.Second).Err(); err != nil {
		l.log.Warn("limit.lease.cache_write_failed", map[string]any{"error": err.Error()})
		// 写失败不算失败：本次拿到的租约仍可用，只是下次还要回表。
	}
	return &lease, true
}

// queryDBUsage 取窗口内的 DB 权威用量（对应 Node queryDbUsage 的五个求和函数）。
func (l *LeaseService) queryDBUsage(ctx context.Context, params GetCostLeaseParams, now time.Time) (float64, error) {
	if l.ledger == nil {
		return 0, errors.New("limit: 租约回表缺少账本读取面")
	}
	entityID := any(params.EntityID)
	if params.Entity == EntityKey {
		if params.KeyHash == "" {
			return 0, errors.New("limit: Key 维度租约回表缺少密钥字符串")
		}
		entityID = params.KeyHash
	}
	start := WindowStart(Period(params.Window), now, params.ResetTime, params.ResetMode, l.loc)
	start = ClipStartByResetAt(start, params.CostResetAt)
	raw, err := l.ledger.SumLedgerCostInTimeRange(ctx, store.LedgerEntityType(params.Entity), entityID, start, now)
	if err != nil {
		return 0, err
	}
	return ParseCostText(raw), nil
}

// CheckCostLimitWithLease 判定单个窗口是否放行（对应 Node checkCostLimitsWithLease 的单窗口分支）。
//
// 返回 (allowed, currentUsage, ok)：ok=false 表示租约不可用，调用方按 Node 语义对**该窗口**
// 放行（fail-open）并留痕，而不是把整次请求判死。
func (l *LeaseService) CheckCostLimitWithLease(ctx context.Context, params GetCostLeaseParams) (bool, float64, bool) {
	lease, ok := l.GetCostLease(ctx, params)
	if !ok {
		return true, 0, false
	}
	if lease.RemainingBudget > 0 {
		return true, lease.CurrentUsage, true
	}
	return false, lease.CurrentUsage, true
}

// DecrementLeaseBudgetResult 对应 Node `DecrementLeaseBudgetResult`。
type DecrementLeaseBudgetResult struct {
	Success      bool
	NewRemaining float64
	FailOpen     bool
}

// DecrementLeaseBudget 原子扣减一份租约（对应 Node decrementLeaseBudget）。
//
// Redis 不可用或脚本报错一律 fail-open（视为扣减成功）：额度的权威仍在 DB 与结算路径，
// 不该因为一次扣减失败把请求判死。
func (l *LeaseService) DecrementLeaseBudget(ctx context.Context, params GetCostLeaseParams, cost float64) DecrementLeaseBudgetResult {
	if !l.Ready() {
		l.log.Warn("limit.lease.decrement_redis_missing", map[string]any{
			"entity": string(params.Entity), "window": string(params.Window),
		})
		return DecrementLeaseBudgetResult{Success: true, NewRemaining: -1, FailOpen: true}
	}
	leaseKey := BuildLeaseKey(params.Entity, params.EntityID, params.Window, params.ResetMode)
	res, err := l.decrementScript.Run(ctx, l.client.Raw(), []string{leaseKey},
		strconv.FormatFloat(cost, 'f', -1, 64)).Result()
	if err != nil {
		l.log.Warn("limit.lease.decrement_failed", map[string]any{"key": leaseKey, "error": err.Error()})
		return DecrementLeaseBudgetResult{Success: true, NewRemaining: -1, FailOpen: true}
	}
	values, ok := res.([]any)
	if !ok || len(values) < 2 {
		l.log.Warn("limit.lease.decrement_bad_reply", map[string]any{"key": leaseKey})
		return DecrementLeaseBudgetResult{Success: true, NewRemaining: -1, FailOpen: true}
	}
	newRemaining := luaNumber(values[0])
	flag := luaNumber(values[1])
	return DecrementLeaseBudgetResult{Success: flag == 1, NewRemaining: newRemaining}
}

// LeaseSettlementEntity 是一个主体的结算入参（id + 两个短窗口的重置模式）。
type LeaseSettlementEntity struct {
	ID         int64
	Reset5h    ResetMode
	ResetDaily ResetMode
}

// SettleLeaseInput 对应 Node `SettleLeaseBudgetsParams`。
type SettleLeaseInput struct {
	RequestID string
	Cost      float64
	Key       LeaseSettlementEntity
	User      LeaseSettlementEntity
	Provider  LeaseSettlementEntity
}

// LeaseBudgetSettlementStatus 对应 Node `LeaseBudgetSettlementStatus`。
type LeaseBudgetSettlementStatus string

const (
	LeaseSettlementDecremented  LeaseBudgetSettlementStatus = "decremented"
	LeaseSettlementMissing      LeaseBudgetSettlementStatus = "missing"
	LeaseSettlementInsufficient LeaseBudgetSettlementStatus = "insufficient"
)

// LeaseBudgetSettlement 是一个窗口的结算结果。
type LeaseBudgetSettlement struct {
	EntityType   string
	EntityID     int64
	Window       LeaseWindow
	Status       LeaseBudgetSettlementStatus
	NewRemaining float64
}

// SettleLeaseBudgetsResult 对应 Node `SettleLeaseBudgetsResult`。
type SettleLeaseBudgetsResult struct {
	RequestID   string
	Status      string
	Settlements []LeaseBudgetSettlement
	FailOpen    bool
}

// SettleLeaseBudgets 把一次请求的实际成本结算到 12 份租约上（4 窗口 × 3 主体）。
//
// 幂等：请求 id 作标记键，重放时 Lua 直接返回上一次的结果（status=duplicate），
// 因此结算可以安全重试——这一点很重要，结算路径的调用方通常是「终态写入后的旁路」。
//
// 契约对照用途：本方法从「主体集合」展开出四窗口的全部切片（Node buildSettlementTargets 口
// 径），保留给 Node 语义对拍与既有调用方。**生产路径走 settleLeaseTargets**：判定侧只记了
// 真正用过的切片（见 pctx.LeaseSettlementPlan），按主体展开会去扣没参与本次判定的窗口。
func (l *LeaseService) SettleLeaseBudgets(ctx context.Context, in SettleLeaseInput) SettleLeaseBudgetsResult {
	return l.settleLeaseTargets(ctx, in.RequestID, in.Cost, buildLeaseSettlementTargets(in))
}

// settleLeaseTargets 把成本结算到**显式给定**的切片上（主体、窗口、重置模式均不由本层推定）。
//
// markerID 是幂等标记：同一条请求可以结算多次（胜者一笔、每条竞速输家各一笔），每次用不同标记、
// 带各自的成本增量，故合计扣减等于本次请求的总成本。
func (l *LeaseService) settleLeaseTargets(
	ctx context.Context,
	markerID string,
	cost float64,
	targets []leaseSettlementTarget,
) SettleLeaseBudgetsResult {
	requestID := trimSpace(markerID)
	failOpen := func(reason string, fields map[string]any) SettleLeaseBudgetsResult {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["requestId"] = requestID
		fields["reason"] = reason
		l.log.Warn("limit.lease.settle_fail_open", fields)
		return SettleLeaseBudgetsResult{RequestID: requestID, Status: "fail_open", FailOpen: true}
	}
	if !l.Ready() {
		return failOpen("redis_missing", nil)
	}
	if requestID == "" || !(cost > 0) {
		return failOpen("invalid_input", map[string]any{"cost": cost})
	}
	if len(targets) == 0 {
		// 空切片集合不该走到这里（调用方已按空计划返回）：走到就是调用方漏了判定。
		return failOpen("no_targets", nil)
	}

	keys := make([]string, 0, len(targets)+1)
	keys = append(keys, leaseSettlementMarkerPrefix+requestID)
	for _, target := range targets {
		keys = append(keys, BuildLeaseKey(target.entity, target.id, target.window, target.resetMode))
	}

	res, err := l.settleScript.Run(ctx, l.client.Raw(), keys,
		strconv.FormatFloat(cost, 'f', -1, 64),
		strconv.Itoa(leaseSettlementMarkerTTLSeconds)).Result()
	if err != nil {
		return failOpen("script_failed", map[string]any{"error": err.Error()})
	}
	values, ok := res.([]any)
	if !ok || len(values) < 2 {
		return failOpen("bad_reply", nil)
	}
	duplicate := luaNumber(values[0])
	payload, ok := values[1].(string)
	if !ok {
		return failOpen("bad_payload", nil)
	}
	settlements, err := parseLeaseSettlements(payload, targets)
	if err != nil {
		return failOpen("payload_decode_failed", map[string]any{"error": err.Error()})
	}
	status := "settled"
	if duplicate == 1 {
		status = "duplicate"
	}
	return SettleLeaseBudgetsResult{RequestID: requestID, Status: status, Settlements: settlements}
}

// leaseSettlementTarget 是一个待结算的（主体, 窗口）位。
type leaseSettlementTarget struct {
	entity    LeaseEntity
	id        int64
	window    LeaseWindow
	resetMode ResetMode
}

// buildLeaseSettlementTargets 复刻 Node `buildSettlementTargets`：主体外层、窗口内层，
// 顺序即 KEYS 顺序（Lua 按下标回填结果）。
//
// 与 Node 有意的差异：**主体 id <= 0 的维度不生成键**。id<=0 意味着本次没有这一维的切片
// （供应商维度的租约判定尚未接线；User/Key 在未鉴权时也走不到这里），带着 id=0 去查只会多
// 四次 Redis 往返，并在结果里留下四条恒为 missing 的噪声——而 missing 在生产上是要用来
// 发现「判定与结算的键不合一」的线索，不该被这类占位项模糊。
func buildLeaseSettlementTargets(in SettleLeaseInput) []leaseSettlementTarget {
	byEntity := map[LeaseEntity]LeaseSettlementEntity{
		EntityKey:      in.Key,
		EntityUser:     in.User,
		EntityProvider: in.Provider,
	}
	targets := make([]leaseSettlementTarget, 0, len(leaseSettlementEntityOrder)*len(LeaseWindows))
	for _, entity := range leaseSettlementEntityOrder {
		detail := byEntity[entity]
		if detail.ID <= 0 {
			continue
		}
		for _, window := range LeaseWindows {
			var resetMode ResetMode
			switch window {
			case LeaseWindow5h:
				resetMode = detail.Reset5h
			case LeaseWindowDaily:
				resetMode = detail.ResetDaily
			}
			targets = append(targets, leaseSettlementTarget{entity: entity, id: detail.ID, window: window, resetMode: resetMode})
		}
	}
	return targets
}

// parseLeaseSettlements 解析 Lua 回填的结果数组（与 targets 同位同长）。
func parseLeaseSettlements(payload string, targets []leaseSettlementTarget) ([]LeaseBudgetSettlement, error) {
	var items [][]float64
	if err := json.Unmarshal([]byte(payload), &items); err != nil {
		return nil, err
	}
	if len(items) != len(targets) {
		return nil, fmt.Errorf("limit: 租约结算结果条数不符：%d != %d", len(items), len(targets))
	}
	out := make([]LeaseBudgetSettlement, 0, len(targets))
	for index, item := range items {
		if len(item) != 2 {
			return nil, errors.New("limit: 租约结算结果项形制非法")
		}
		status := LeaseSettlementMissing
		switch {
		case item[0] == 1:
			status = LeaseSettlementDecremented
		case item[0] == -1:
			status = LeaseSettlementInsufficient
		}
		out = append(out, LeaseBudgetSettlement{
			EntityType:   string(targets[index].entity),
			EntityID:     targets[index].id,
			Window:       targets[index].window,
			Status:       status,
			NewRemaining: item[1],
		})
	}
	return out, nil
}

// luaNumber 把 Lua 返回的数值（go-redis 解成 int64 或 string）转成 float64。
func luaNumber(value any) float64 {
	switch typed := value.(type) {
	case int64:
		return float64(typed)
	case string:
		parsed, err := strconv.ParseFloat(typed, 64)
		if err != nil {
			return 0
		}
		return parsed
	case float64:
		return typed
	default:
		return 0
	}
}
