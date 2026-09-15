package limit

import (
	"encoding/json"
	"math"
	"strconv"
)

// 本文件是 Node `src/lib/rate-limit/lease.ts` 的等价实现：租约模型、Redis 键形制、
// 预算切片数学与 JSON 形制。
//
// 为什么 JSON 字段名逐字照抄 Node（camelCase 而非本仓的 snake_case）：租约正文直接存进
// Redis，是**跨语言共享的运行时数据**。若按 Go 习惯改名，回退到 Node 实现时读到的是一份
// 读不懂的租约（`remainingBudget` 缺失即被 deserialize 判为非法而丢弃），缓存键又会互相
// 覆盖。故这里的 tag 是契约，不是风格。

// LeaseWindow 是租约窗口类型，取值与 Node `LeaseWindow` 相同。
type LeaseWindow string

const (
	LeaseWindow5h      LeaseWindow = "5h"
	LeaseWindowDaily   LeaseWindow = "daily"
	LeaseWindowWeekly  LeaseWindow = "weekly"
	LeaseWindowMonthly LeaseWindow = "monthly"
)

// LeaseWindows 是判定顺序（与 Node `LeaseService.SETTLEMENT_WINDOWS` 一致）。
var LeaseWindows = []LeaseWindow{LeaseWindow5h, LeaseWindowDaily, LeaseWindowWeekly, LeaseWindowMonthly}

// LeaseEntity 是租约主体类型，取值与 Node `LeaseEntityType` 相同（即本包的 Entity）。
type LeaseEntity = Entity

// BudgetLease 是一份预算租约，字段与顺序对齐 Node `BudgetLease`。
type BudgetLease struct {
	EntityType      string  `json:"entityType"`
	EntityID        int64   `json:"entityId"`
	Window          string  `json:"window"`
	ResetMode       string  `json:"resetMode"`
	ResetTime       string  `json:"resetTime"`
	SnapshotAtMS    int64   `json:"snapshotAtMs"`
	CurrentUsage    float64 `json:"currentUsage"`
	LimitAmount     float64 `json:"limitAmount"`
	RemainingBudget float64 `json:"remainingBudget"`
	TTLSeconds      int64   `json:"ttlSeconds"`
	// CostResetAtMS / WindowResetAtMS 为 nil 表示「无该事实」，序列化为 null（Node 用可选字段）。
	CostResetAtMS   *int64 `json:"costResetAtMs"`
	WindowResetAtMS *int64 `json:"windowResetAtMs"`
}

// BuildLeaseKey 复刻 Node `buildLeaseKey`：`lease:{entityType}:{entityId}:{window}`，
// 5h 与 daily 追加 resetMode 后缀（这两种窗口有滚动/固定两种表示，混用会互相污染）。
//
// resetMode 为空时按 Node 的兜底：5h 视作 rolling，其余视作 fixed。
func BuildLeaseKey(entity LeaseEntity, id int64, window LeaseWindow, resetMode ResetMode) string {
	effective := resetMode
	if effective == "" {
		if window == LeaseWindow5h {
			effective = ResetRolling
		} else {
			effective = ResetFixed
		}
	}
	base := "lease:" + string(entity) + ":" + strconv.FormatInt(id, 10) + ":" + string(window)
	if window == LeaseWindow5h || window == LeaseWindowDaily {
		return base + ":" + string(effective)
	}
	return base
}

// CalculateLeaseSlice 复刻 Node `calculateLeaseSlice`：min(limit*percent, 剩余预算, capUSD)，
// 四舍五入到 4 位小数，且恒非负。
func CalculateLeaseSlice(limitAmount, currentUsage, percent float64, capUSD *float64) float64 {
	remaining := math.Max(0, limitAmount-currentUsage)
	if remaining == 0 {
		return 0
	}
	safePercent := math.Min(1, math.Max(0, percent))
	slice := limitAmount * safePercent
	slice = math.Min(slice, remaining)
	if capUSD != nil {
		slice = math.Min(slice, math.Max(0, *capUSD))
	}
	// Node 用 Math.round(slice * 10000) / 10000；Go 侧同法，避免二进制浮点尾差的比较漂移。
	return math.Max(0, math.Round(slice*10000)/10000)
}

// SerializeLease 与 Node `serializeLease` 同为 JSON 正文。
func SerializeLease(lease BudgetLease) (string, error) {
	raw, err := json.Marshal(lease)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// DeserializeLease 复刻 Node `deserializeLease`：JSON 非法或必填字段缺失即返回 false。
//
// 必填判据逐项对齐 Node（字段类型必须正确）：这是「宁可当缓存未命中重算，也不拿半份租约
// 做判定」的 fail-safe 面。
func DeserializeLease(raw string) (BudgetLease, bool) {
	var probe map[string]any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return BudgetLease{}, false
	}
	for _, field := range []string{
		"entityType", "entityId", "window", "resetMode", "resetTime",
		"snapshotAtMs", "currentUsage", "limitAmount", "remainingBudget", "ttlSeconds",
	} {
		if _, ok := probe[field]; !ok {
			return BudgetLease{}, false
		}
	}
	var lease BudgetLease
	if err := json.Unmarshal([]byte(raw), &lease); err != nil {
		return BudgetLease{}, false
	}
	if lease.EntityType == "" || lease.Window == "" || lease.ResetMode == "" {
		return BudgetLease{}, false
	}
	return lease, true
}

// IsLeaseExpired 复刻 Node `isLeaseExpired`：快照时刻 + TTL 秒已过即过期。
//
// 之所以不能只看 Redis TTL：租约正文里的 snapshotAtMs 与 TTL 是**判定依据**，Redis 的
// TTL 只是存储层兜底（重写租约时按剩余 TTL 续期），两者口径不同。
func IsLeaseExpired(lease BudgetLease, nowMS int64) bool {
	return nowMS >= lease.SnapshotAtMS+lease.TTLSeconds*1000
}
