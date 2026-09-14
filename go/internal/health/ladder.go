package health

import "github.com/fanxcv/claude-code-hub-go/go/internal/route"

// 本文件是**等待阶梯**（circuit backoff ladder）的纯计算部分。
//
// 语义（用户明示口径，逐字）：
//
//	第 n 次连续熔断的窗口时长 = base + increment × n
//	n = 连续「熔断 → 半开试探 → 未能恢复」的轮数（首次开闸 n = 0，即窗口就是 base）
//	n 封顶 maxCount：达到后一直用 base + increment × maxCount
//	一旦恢复（转入 closed），n 归零，下次重新从 base 起
//
// 例（base=5m, increment=10m, maxCount=3）：5 / 15 / 25 / 35 / 35 / 35…
//
// 为何这些函数在本包而非 route 包：route 是**只读状态**的一侧（选路只看 circuitOpenUntil，
// 不需要阶梯），阶梯只在写入侧的这次状态迁移里算一次。放这里可保证「阶梯判定只有一份」，
// 不会在读侧再长出第二份（两份迟早分叉）。
//
// 与 Node 的关系：Node 没有阶梯，本能力是 Go 侧的有意增强；出厂默认 increment=0 或
// maxCount=0（见 route.DefaultOpenDurationIncrementMS / DefaultMaxOpenCount）时，
// 下面的 windowMS 恒等于 base，行为与加阶梯之前**逐字段一致**。

// ladderLevel 把级数夹到 [0, maxCount]。
//
// maxCount 非正时恒为 0 —— 这正是「出厂默认不启用阶梯」：级数永远是 0，窗口永远是 base。
// 负数（配置写坏或历史残值）按 0 处理：负的级数会让窗口反而小于基础值，比不开阶梯更激进。
func ladderLevel(current, maxCount int64) int64 {
	if maxCount < 0 {
		maxCount = 0
	}
	if current < 0 {
		return 0
	}
	if current > maxCount {
		return maxCount
	}
	return current
}

// nextLadderLevel 求「本轮试探失败」之后的新级数：级数 +1，封顶 maxCount。
//
// 调用时机是本文件顶部的第二行语义——**一次试探失败只推进一级**。同一窗口期内的后续失败
// 走不到这里（它们落在 RecordProviderFailure 的「已开闸且在窗口内」早退分支），故不会重复计数。
func nextLadderLevel(current, maxCount int64) int64 {
	level := ladderLevel(current, maxCount)
	if level >= maxCount {
		return level
	}
	return level + 1
}

// ladderWindowMS 求第 level 级阶梯的开闸窗口时长：base + increment × min(level, maxCount)。
//
// 溢出与异常的保护都落到「不小于 base」：窗口比基础值还短（负数递增、乘法溢出）会让熔断器
// 比不开阶梯更激进，与这个特性的意图相反，故一律夹回 base。
func ladderWindowMS(config route.ProviderCircuitConfig, current int64) int64 {
	base := config.OpenDurationMS
	increment := config.OpenDurationIncrementMS
	if increment <= 0 {
		return base
	}
	level := ladderLevel(current, config.MaxOpenCount)
	window := base + increment*level
	if window < base {
		return base
	}
	return window
}
