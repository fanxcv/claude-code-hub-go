package convert

// 本文件定义**协议转换失败**的事实类型（Go 侧新增，**超出 Node parity**）。
//
// 为什么需要它：Node 的语义是「转换不可用时静默回退为原生直通」（`forwarder.ts` 一带的
// try/catch → conversionPlan 置空），Go 侧逐字复刻了这个回退，但**把 err 丢掉了**——
// `forward/plan.go` 的转换分支只留一个布尔标记，于是用户在使用记录页上看不到任何
// 「转换失败」的痕迹，排障时无法区分「没转换」与「想转换但失败了」。
//
// 本类型只承载**事实**（协议对 + 阶段 + 原因 + 是否回退），不承载任何处置决定：
// 回退语义仍由 `forward` 决定，且必须保持与 Node 一致。

// ConversionFailurePhase 是失败发生的阶段。
//
// 两个阶段对应 `forward.BuildPlan` 里转换被置空的两处，二者语义不同，必须能区分：
//   - 路径解析失败：客户端路径在目标协议线上没有对应端点 → 整次放弃转换（Node 的 fail-closed）；
//   - 正文转换失败：协议对成立、路径也解析出来了，但正文解码/编码失败 → 回退原生直通。
type ConversionFailurePhase string

const (
	// PhasePathResolution 表示上游路径映射失败（目标协议线无对应端点）。
	PhasePathResolution ConversionFailurePhase = "path_resolution"
	// PhaseBodyConversion 表示正文解码或编码失败。
	PhaseBodyConversion ConversionFailurePhase = "body_conversion"
)

// ConversionFailure 是一次协议转换失败的可记录事实。
//
// 字段取值的唯一真源是**编译期的计划**（`forward.Plan`）：协议对来自 `ConversionPlan`，
// 阶段与原因来自实际的失败点，`Fallback` 表示本次是否按原生直通继续（不施加转换）。
// 记录侧不得二次推导，否则它证明不了「转换到底失败在哪」。
type ConversionFailure struct {
	// ClientProtocol 是客户端入站协议线。
	ClientProtocol WireProtocol
	// TargetProtocol 是本次原本要转到的上游协议线；无法判定时为空串。
	TargetProtocol WireProtocol
	// Phase 是失败阶段。
	Phase ConversionFailurePhase
	// Reason 是失败原因文本；**可能含未脱敏片段**，落库前必须经
	// `specialsettings.SanitizeReason` 处理（见 b 条要求：不得把上游 URL/凭据写进审计）。
	Reason string
	// Fallback 为真表示本次请求仍按原生直通继续（Node 语义），为假表示整次尝试未继续。
	Fallback bool
}
