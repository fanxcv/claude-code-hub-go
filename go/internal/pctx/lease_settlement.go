package pctx

// LeaseSettlementEntity 是租约结算主体的取值域（与 limit 包的 Entity 一一对应）。
//
// 为什么用本包自己的字符串类型而不是导 limit 的类型：pctx 是判定侧与结算侧之间的唯一通道，
// 而 limit 已经导入 pctx，反向导入成环。两套取值域的映射各写一处、显式 switch（见 limit 包）。
type LeaseSettlementEntity string

const (
	LeaseSettlementEntityKey      LeaseSettlementEntity = "key"
	LeaseSettlementEntityUser     LeaseSettlementEntity = "user"
	LeaseSettlementEntityProvider LeaseSettlementEntity = "provider"
)

// LeaseSettlementWindow 是租约窗口的取值域（与 limit 包的 LeaseWindow 一一对应）。
type LeaseSettlementWindow string

const (
	LeaseSettlementWindow5h      LeaseSettlementWindow = "5h"
	LeaseSettlementWindowDaily   LeaseSettlementWindow = "daily"
	LeaseSettlementWindowWeekly  LeaseSettlementWindow = "weekly"
	LeaseSettlementWindowMonthly LeaseSettlementWindow = "monthly"
)

// LeaseSettlementTarget 是一次判定**实际用过**的一份切片：主体、窗口、以及该窗口生效的重置模式。
//
// 为什么连重置模式一起带：租约键形制是 `lease:{entity}:{id}:{window}:{resetMode}`
// （5h 与 daily 带模式后缀，见 limit.BuildLeaseKey）。判定时按哪种模式切的片，
// 结算就必须扣同一份键——模式不一致会扣到另一份键上，表现为「结算静默不生效」。
// 这里存的是**判定时实际生效的模式**（缺省已补齐），不是设置里的原始值。
type LeaseSettlementTarget struct {
	Entity    LeaseSettlementEntity
	ID        int64
	Window    LeaseSettlementWindow
	ResetMode string
}

// Empty 报告这枚目标是否为空（主体 id 非正即没有切片可言）。
func (t LeaseSettlementTarget) Empty() bool {
	return t.Entity == "" || t.ID <= 0 || t.Window == ""
}

// LeaseSettlementPlan 是本次请求判定时用过的切片清单：守卫链的限流步写、终态结算读。
//
// 为什么要有这个槽位：判定在守卫链（limit 包），扣减在终态结算（terminal 包），
// 两者之间只有请求上下文这一条通道，而结算器只看得到 Settlement 一份输入。
//
// 为什么逐维记而不是「本进程走了租约路径」记一份：同一条请求里不同维度可以一半走租约、
// 一半回退账本（limit.leaseCostLimit 返回 decided=false），若按主体粗记一份，结算会去扣
// **没有参与本次判定**的切片。这里的语义是「哪几份切片真的被判定用过」，空清单即不结算。
//
// 为什么不带 provider 维度：本进程的供应商维度尚未走租约判定（限额判定仍走账本路径，
// 没有切片可扣），见 limit/lease_service.go 的文件头与 limit/provider_cost.go。
type LeaseSettlementPlan struct {
	Targets []LeaseSettlementTarget
}

// Empty 报告计划里没有任何切片（本次请求没有走租约判定）。
func (p LeaseSettlementPlan) Empty() bool {
	return len(p.Targets) == 0
}

// Add 追加一枚结算目标，返回是否真的入列；主体 id 非正或结构写不全的目标不入列。
//
// 为什么要在这里丢：一枚写不全的目标会让结算侧去查一个拼错的键，而结果只会表现成
// 「missing」——那是用来发现「判定与结算键不合一」的线索，不能被这种占位项模糊。
//
// 为什么返回 bool：这里只能做**结构性**判据（主体/窗口非空、id 为正）。取值域（rolling/fixed
// 这类取值）属于限流包，本包不认识——反向导入会成环。故取值域校验在判定侧与结算侧共用
// limit.leaseSettlementTargetValid，而这里返回 false 只表示这枚目标连结构都不完整。
// 调用方**必须**留痕（见 limit.rememberLeaseTarget）：静默丢弃的后果是这一维永不结算，
// 租约余额比实际更宽，窗口内判定会放行本该拒的请求。
func (p *LeaseSettlementPlan) Add(target LeaseSettlementTarget) bool {
	if p == nil || target.Empty() {
		return false
	}
	p.Targets = append(p.Targets, target)
	return true
}

// SetLeaseSettlementPlan 装入租约结算计划。
//
// 为什么整份写而不是逐维追加：限流步是先判完所有维度、再一次性落计划，判定过程中
// 本地的切片集合与请求上下文里的计划必须一致（逐维追加会让「判完了但没落」的中间态
// 可见于并发读者）。限流步对同一条请求只跑一次。
func (c *Context) SetLeaseSettlementPlan(plan LeaseSettlementPlan) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.leaseSettlementPlan = plan
	c.leaseSettlementPlanSet = true
}

// LeaseSettlementPlan 取租约结算计划；第二个返回值为 false 表示本次未走租约判定。
func (c *Context) LeaseSettlementPlan() (LeaseSettlementPlan, bool) {
	if c == nil {
		return LeaseSettlementPlan{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.leaseSettlementPlanSet {
		return LeaseSettlementPlan{}, false
	}
	return c.leaseSettlementPlan, true
}
