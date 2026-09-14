package route

import "github.com/fanxcv/claude-code-hub-go/go/internal/convert"

// sameProtocolPreference 是本次选路的「同协议偏好」判定。
//
// 需求（用户）：供应链决策时，优先级一致的情况下优先走同协议供应商
// （即不需要协议转换的那家），把跨协议转换留作退路。
//
// 为何单列一个类型而不是在 Select 里散写条件：判据只有一处定义，
// 「启用条件」与「同协议判据」才能被测试逐条钉住，也避免调用点各自解释 K 的边界。
type sameProtocolPreference struct {
	// enabled 表示本次是否做该判定。
	enabled bool
	// k 是同协议候选的权重倍率（enabled 时恒 > 1）。
	k int
}

// newSameProtocolPreference 判定本次是否启用同协议偏好，并带上倍率。
//
// 两条不启用的情形，都是「无法判定」而非「判定为否」：
//   - 客户端格式未知（客户端没给出可识别方言）：此时 guard 的格式兼容过滤同样不判定
//     （filter.go 只在 in.format != "" 时判），无从区分同协议与跨协议；
//   - k <= 1：倍率不构成偏好（1 是等权，0 或负数无意义）。零值即不启用，
//     保证「未装配该配置」与「显式关闭」行为一致。
func newSameProtocolPreference(format convert.ClientFormat, k int) sameProtocolPreference {
	if format == "" || k <= 1 {
		return sameProtocolPreference{}
	}
	return sameProtocolPreference{enabled: true, k: k}
}

// isSame 报告该候选是否同协议。
//
// 判据复用 convert.IsNativePair —— 与 guard 的格式兼容过滤走的是同一张配对表
// （filter.go 经 convert.ResolveProtocolCompat 调它）。这一点是硬要求：
// 若这里另写一套近似判断，偏好就可能指向一个「其实仍要转换」的供应商，与真实转发行为分叉。
//
// 未启用时恒返回 false：调用方只在 enabled 时消费它，但保持该函数自洽，
// 避免「忘了判 enabled 就拿到 true」这类误用。
func (p sameProtocolPreference) isSame(format convert.ClientFormat, provider Provider) bool {
	if !p.enabled {
		return false
	}
	return convert.IsNativePair(format, provider.ProviderType)
}
