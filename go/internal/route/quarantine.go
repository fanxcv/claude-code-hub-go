package route

import (
	"hash/fnv"
	"strconv"
	"time"
)

// 本文件是「低速隔离」的**纯判定**部分：准入阶梯与按请求的准入闸门。
//
// 为什么必须是纯函数、且只放这里：隔离判定要被两处消费——过滤阶段（applyFilters 的排除）
// 与亲和提名阶段（validateAffinityCandidate 的硬校验，亲和不得绕过隔离）。两处各算一遍
// 必然分叉，而分叉的表现是「被隔离的渠道仍被粘住的会话选中」——静默且难以归因。
//
// 为什么隔离态本身不落新键：真源是既有的慢状态 Hash（`cch:slow:{...}:state`）里的
// `quarantine` 字段 + 既有的连续干净计数键（`cch:slow:{...}:streak`）。前者只由新代码的慢
// 路径写下，**存量 state 里没有这个字段**，故历史惩罚数据天然不被当作隔离（这正是设计要求的
// 「可区分来源」）；后者本来就在每次可判定样本上增减，隔离阶梯只是换个方式读它，写侧零改动。

// 准入阶梯的阈值：**未标定的初值**。
//
// 用户明示「阈值不许拍脑袋」，而标定唯一的数据来源是影子埋点（生产尚未产出），故这里先取
// 整值常量，并在报告里记为待标定项。语义是「进入隔离后，连续这么多个可判定样本都不慢，
// 就把放行比例抬一档」：
//
//	streak < QuarantineAdmissionTenFrom                        -> 0%（只放行探针租约持有者）
//	streak >= QuarantineAdmissionTenFrom                       -> 10%
//	streak >= QuarantineAdmissionThirtyFrom                    -> 30%
//	streak >= RecoveryRequests（既有参数，默认 10）              -> 100%
//
// 最后一档**不由本文件实现**：连续干净样本达到 RecoveryRequests 时，写侧 recordClean 的既有
// 重置路径会删掉 samples/state/streak，读侧当场看不到隔离（与惩罚衰减同一机制）。故这里只需
// 两档中间阈值，避免把 RecoveryRequests 复制成第二个真源。
const (
	// QuarantineAdmissionTenFrom 是抬到 10% 放行所需的连续干净样本数。
	QuarantineAdmissionTenFrom = 3
	// QuarantineAdmissionThirtyFrom 是抬到 30% 放行所需的连续干净样本数。
	QuarantineAdmissionThirtyFrom = 6
	// quarantineAdmissionFullPermille 是「不隔离」的放行比例（千分比）。
	quarantineAdmissionFullPermille = 1000
	// quarantineAdmissionTenPermille / quarantineAdmissionThirtyPermille 是中间两档的放行比例。
	quarantineAdmissionTenPermille    = 100
	quarantineAdmissionThirtyPermille = 300
)

// SlowRateQuarantine 是一个「渠道 x 模型」组合的隔离开关及其放行比例。
//
// AdmissionPermille 为 1000 表示**不隔离**（调用方据此不产出任何隔离行为，与无隔离态同效）。
type SlowRateQuarantine struct {
	// AdmissionPermille 是本次允许该组合参与正常选路的比例（千分比，0..1000）。
	AdmissionPermille int
	// CleanStreak 是进入隔离以来的连续干净样本数（仅排障用，不参与判定）。
	CleanStreak int
	// ProbeLeaseFree 是本次读到的探针租约状态（空闲为真）。
	//
	// 它由读侧**同一趟 pipeline** 顺带取回，目的是让选路只在租约空闲时才尝试 SET NX：
	// 否则每个请求都会向 Redis 发一次写命令，而隔离期间这些请求正是大头。
	ProbeLeaseFree bool
}

// quarantinePermilleForStreak 由连续干净样本数派生放行比例。
//
// 单独成函数而不是内联进读侧：阶梯是这条特性里最容易被「顺手调一下」的地方，写成具名纯函数
// 才有单点可测可改。
func quarantinePermilleForStreak(cleanStreak int) int {
	switch {
	case cleanStreak >= QuarantineAdmissionThirtyFrom:
		return quarantineAdmissionThirtyPermille
	case cleanStreak >= QuarantineAdmissionTenFrom:
		return quarantineAdmissionTenPermille
	default:
		return 0
	}
}

// quarantineExclusions 把隔离态折成「本次排除集合」：准入闸门通过的组合不入集合（照常参与选路）。
//
// 判定放在这里而不是 filter 里：准入闸门要时钟与请求身份（会话/key），而 applyFilters 是纯过滤。
func quarantineExclusions(
	states map[int64]SlowRateQuarantine,
	keyID int64,
	sessionID string,
	now time.Time,
) map[int64]bool {
	if len(states) == 0 {
		return nil
	}
	excluded := make(map[int64]bool, len(states))
	for providerID, state := range states {
		if quarantineAdmits(state.AdmissionPermille, providerID, keyID, sessionID, now) {
			continue
		}
		excluded[providerID] = true
	}
	return excluded
}

// quarantineAdmissionBucket 是准入闸门的时间桶宽度。
//
// 10 秒：短到「同一个会话不会被永久挡在门外」，长到同一会话在十秒内拿到**稳定**的准入结论
// （逐请求随机会让同一会话在十秒内反复进出隔离渠道，观测上表现为「隔离没生效」）。
const quarantineAdmissionBucket = 10 * time.Second

// quarantineAdmits 判定本次请求是否获准进入「被隔离的」该组合。
//
// 判据是请求身份的稳定哈希而非随机数：同一 (会话, key, 渠道, 模型, 时间桶) 必然得到同一结论，
// 故测试与事后归因都是可复现的（仓内 Options.Rand 只用于加权选择，不对抽查式准入负责）。
//
// 放行比例为 0 时**不做哈希**直接返回假：让「隔离到底」的语义与哈希实现解耦，避免将来换哈希
// 时把 0% 变成「偶尔放一个」。
func quarantineAdmits(admissionPermille int, providerID, keyID int64, sessionID string, now time.Time) bool {
	if admissionPermille <= 0 {
		return false
	}
	if admissionPermille >= quarantineAdmissionFullPermille {
		return true
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(strconv.FormatInt(providerID, 10)))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(strconv.FormatInt(keyID, 10)))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(sessionID))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(strconv.FormatInt(now.UnixNano()/int64(quarantineAdmissionBucket), 10)))
	return int(hasher.Sum32()%quarantineAdmissionFullPermille) < admissionPermille
}
