package slowrate

import "time"

// 本文件是「首字后停滞探测换家」的**判定参数与判据**：探测阈值，以及那个纯判定函数。
// 本文件**不做任何 IO、不接线**——接线在 forward/gate 侧（见 gate.Options.ProbeAfterFirstByte
// 与 forward.StreamOptions.ProbeAfterFirstByteFor）。
//
// 这条机制判的是**停滞（stall）**，不是「慢（slow）」。判据只有一条：
// **自首字节到达起，超过阈值 T 仍未提交任何内容帧**。
//
// 为什么不能判「慢」（即按 token 速率判）：门控在首次分类出内容帧时**即提交**，故探测窗口
// 按定义**不含任何内容帧**——窗口内已生成 token 恒为 0。速率判据在它唯一的适用窗口里恒不
// 成立，且窗口内拿不到权威 token 数（usage 只在收尾帧给）。2026-09-21 据此删去了原先的
// token 闸与速率闸，连同 providers 表的 slow_rate_probe_min_tokens 列（迁移 0133）。
//
// 2026-09-22 追加（用户裁定「进行中止损」）：那条推理只否定「在原探测窗口里判速率」，
// 而用户要的恰恰是**把窗口延长到首个内容帧之后**——先暂存一小段内容、看它的产出速率，
// 快则立即放行、慢则在客户端零字节时换家。那是**另一条机制**（提交前速率闸），
// 参数在 `Precommit*` 一节，与本文的停滞探测并列，两者互不替代：
//   - 停滞探测抓「首字后完全没有内容」（速率恒 0 的退化形态）；
//   - 提交前速率闸抓「有内容但极慢」。
//
// 与既有 IdleTimeout 的唯一区别是**分母**：
//   - IdleTimeout 看**读间隔**：上游一直发中性帧（心跳/头帧）就永不触发；
//   - 本判据看**自首字节起的绝对时长**：上游一直发中性帧而不产内容也会触发。
//
// 这正是它相对 IdleTimeout 的增量价值所在（该形态在生产是否存在及其占比，尚未取证）。

// DefaultProbeAfterFirstByteSeconds 是首字后停滞阈值 T 的出厂值（秒）。
//
// 30 秒的依据：正常渠道的生成阶段（首字到末字）中位在数秒量级（生产实测 wb 的
// fb/dur 中位生成窗为秒级），30 秒足够长到不会误杀「正常但输出很长」的请求，
// 又足够短到用户不必干等一分钟才发现这家在卡。
//
// 生效条件（2026-09-22 修正）：**低速监控开关打开**（providers.slow_rate_monitor_enabled）
// 且该渠道未显式覆写该列时，取本出厂值。开关关闭 ⇒ 整条低速机制关闭，不探测（零开销）。
//
// 先前注释写「『默认关闭』由列的 NULL 承载」——那句已与实现不符：列的 NULL 只是「未覆盖」，
// 不是「关闭」；真正的关闭由**监控开关**承载。列显式设值仍覆盖出厂值（兼容存量行为）。
const DefaultProbeAfterFirstByteSeconds = 30

// PrecommitBytesPerToken 是「语义字节 ⇒ token」的近似换算比。
//
// 为何需要换算：提交前拿不到权威 token 数（usage 只在收尾帧给），只能数字节；
// 而基线是 tok/s。4 是英文文本与 JSON 的经验中值。**这是近似**：中文与 tool JSON 会明显
// 偏离（中文约 1.5 B/token，JSON 可到 10+）。故它是**待标定项**，将由影子日志的实测
// 分布替换（影子期只记录不裁决，见 forward.StreamOptions.PrecommitShadow）。
const PrecommitBytesPerToken = 4

// DerivePrecommitMinBytesPerSecond 由基线中位速率与该渠道的低速系数推导提交前速率闸
// 阈值（语义字节/秒）。
//
// 语义与事后判定一致（`isSlow` 的 `rate < baseline × Ratio`），只是把单位从 tok/s 折成
// 字节/秒。任一输入非正即返回 0（不启用）——没有基线就不能判慢，宁可 fail-open。
func DerivePrecommitMinBytesPerSecond(baselineTokPerSecond, ratio float64) int {
	if baselineTokPerSecond <= 0 || ratio <= 0 {
		return 0
	}
	return int(baselineTokPerSecond*ratio*PrecommitBytesPerToken + 0.5)
}

// ProbeParams 是一个渠道生效的探测参数（渠道覆写优先，缺省取出厂值）。
type ProbeParams struct {
	// AfterFirstByteSeconds 是自**首字到达**起算的停滞阈值 T（秒）。
	// <= 0 表示不探测（机制关闭）。
	AfterFirstByteSeconds int
}

// ProbeInput 是一次中途探测的输入事实。
type ProbeInput struct {
	// ElapsedSinceFirstByte 是自首字到达起已过去的时长。
	//
	// 为何起点是**首字**而不是请求开始：首字之前的时间是上游排队与预填充，那段时间的
	// 长度反映的是「这家忙不忙」，不是「这家生成得快不快」。用户感知的「卡」也正是在
	// 首字之后——他已经看到东西在动，却迟迟不动。
	ElapsedSinceFirstByte time.Duration
}

// IsSlowProbe 判定一次中途探测是否「已到点仍未产出内容」。
//
// T <= 0 时恒 false，即机制关闭。这是**唯一**的判据——它不涉及速率，理由见文件头。
//
// 注意：判定只在门控等待期（首字节已到、内容尚未提交）内有意义；内容一到就会提交，
// 此后不再探测。
func IsSlowProbe(input ProbeInput, params ProbeParams) bool {
	if params.AfterFirstByteSeconds <= 0 {
		return false
	}
	return input.ElapsedSinceFirstByte >= time.Duration(params.AfterFirstByteSeconds)*time.Second
}
