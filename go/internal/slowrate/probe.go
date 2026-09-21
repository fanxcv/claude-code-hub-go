package slowrate

import (
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
)

// 本文件是「首字后低速探测换家」的**判定参数与判据**：探测阈值、最低 token 数，
// 以及那个纯判定函数。本文件**不做任何 IO、不接线**——L2/L3 的实施在 forward/gate 侧，
// 它们只调用这里的常量和 IsSlowProbe。
//
// 与终态判定（Recorder.Record）的关系：那条路在请求**结束后**看整段速率；本路在请求
// **进行中**看「自首字起已生成多少 token」，目的是在用户还没等到完整回答之前就判出
// 「这家在慢慢吐字」，从而中止它并换家。两者的速率口径必须同源（同为
// pubstatus.ComputeTokensPerSecond），否则同一家渠道会在两条路上得到两个结论。

// DefaultProbeParams 是探测机制的出厂值（与 providers 表的两个探测列对应）。
//
// AfterFirstByteSeconds 的零值语义是**不探测**：列 NULL 即取这里的值，而这里的默认值
// 是「启用」。两者看似矛盾，实则分工明确——
//   - 列 NULL ⇒ 渠道**未配置**，机制对该渠道**关闭**（产品承诺：默认不改既有行为）；
//   - 列有值且 <= 0 ⇒ 同样关闭（见 IsSlowProbe 的 T <= 0 分支）。
//
// 所以「默认关闭」由**列的 NULL**承载，而不是由本常量的取值承载：本常量只在渠道显式
// 留空「最低 token 数」时提供默认，以及给 L2/L3 一个可引用的出厂阈值。
const (
	// DefaultProbeAfterFirstByteSeconds 是首字后探测阈值 T 的出厂值（秒）。
	//
	// 30 秒的依据：正常渠道的生成阶段（首字到末字）中位在数秒量级（生产实测 wb 的
	// fb/dur 中位生成窗为秒级），30 秒足够长到不会误杀「正常但输出很长」的请求，
	// 又足够短到用户不必干等一分钟才发现这家在磨。
	DefaultProbeAfterFirstByteSeconds = 30

	// DefaultProbeMinTokens 是探测时的最低已生成 token 数。
	//
	// 复用 minOutputTokens 而**不是**新写一个 50：两者回答的是同一个问题——
	// 「这么少的输出，速率还有没有意义」。终态判定用它剔除短输出样本，中途探测用它
	// 剔除「刚开始吐字」的时刻。若各写一个数字，改了一处漏另一处就会出现
	// 「终态判不出慢、中途却判出慢」的分裂。
	DefaultProbeMinTokens = minOutputTokens
)

// ProbeParams 是一个渠道生效的探测参数（渠道覆写优先，缺省取出厂值）。
type ProbeParams struct {
	// AfterFirstByteSeconds 是自**首字到达**起算的探测阈值 T（秒）。
	// <= 0 表示不探测（机制关闭）。
	AfterFirstByteSeconds int
	// MinTokens 是探测时的最低已生成 token 数（低于此数不判定）。
	// <= 0 时取 DefaultProbeMinTokens。
	MinTokens int
	// RatioPerMille 是低速线的千分比系数（300 = 0.3），与终态判定同一口径。
	RatioPerMille int
}

// normalize 把零值与越界值收敛到可用区间（与 Params.normalize 同一手法）。
//
// 注意 **AfterFirstByteSeconds 不在收敛之列**：它的零值/负值是「关闭」这个明确语义，
// 不能被静默改成 30——那会让「管理员把它配成 0 想关掉」变成「它又被打开了」。
func (p ProbeParams) normalize() ProbeParams {
	if p.MinTokens <= 0 {
		p.MinTokens = DefaultProbeMinTokens
	}
	if p.RatioPerMille <= 0 {
		p.RatioPerMille = DefaultParams().RatioPerMille
	}
	return p
}

// ProbeInput 是一次中途探测的输入事实。
type ProbeInput struct {
	// TokensSoFar 是首字之后已生成的 token 数（估算：调用方按已解析的增量文本折算）。
	TokensSoFar int
	// ElapsedSinceFirstByte 是自首字到达起已过去的时长。
	//
	// 为何起点是**首字**而不是请求开始：首字之前的时间是上游排队与预填充，那段时间的
	// 长度反映的是「这家忙不忙」，不是「这家生成得快不快」。用户感知的「慢」也正是在
	// 首字之后——他已经看到东西在动，却动得很慢。
	ElapsedSinceFirstByte time.Duration
	// Baseline 是该「渠道 × 模型」组合的历史基线（tok/s，由基线定时任务产出）。
	// <= 0 表示无基线。
	Baseline float64
}

// IsSlowProbe 判定一次中途探测是否「已到点且速率不达标」。
//
// 三道 gate 缺一不可，各自的理由：
//
//	① ElapsedSinceFirstByte >= T  —— 时间闸。未到点就判，等于把「正常但稍慢」的开头
//	   当成故障；T <= 0 时恒 false，即机制关闭。
//	② TokensSoFar >= MinTokens    —— 样本量闸。这是**防空判**的闸：模型在思考阶段
//	   （reasoning / 长预填充）可以先吐很少的 token 而速率极低，若只看「token/时间」，
//	   一段正在正常思考的响应会被判成慢渠道并被中止换家——那是把「模型在想」误判成
//	   「渠道不行」，代价是白烧一次上游调用且用户多等一轮。要求已生成足量 token，
//	   才说明「它确实在生成，只是生成得慢」。
//	③ 速率 < Baseline × RatioPerMille/1000 —— 速率闸。与终态判定同一口径，
//	   经 pubstatus.ComputeTokensPerSecond 计算（本函数不另立公式）。
//
// Baseline <= 0 时恒 false（fail-open）：无基线就无从判「慢」，与 Recorder.Record 在基线
// 缺失时不判定同向。宁可少标不可误标——误标的后果是中止一个本来正常的请求。
func IsSlowProbe(input ProbeInput, params ProbeParams) bool {
	params = params.normalize()

	// ① 时间闸（含「机制关闭」）。
	threshold := time.Duration(params.AfterFirstByteSeconds) * time.Second
	if threshold <= 0 || input.ElapsedSinceFirstByte < threshold {
		return false
	}

	// ② 样本量闸。
	if input.TokensSoFar < params.MinTokens {
		return false
	}

	// ③ 速率闸：复用与终态判定同一个计算口径。
	if input.Baseline <= 0 {
		return false
	}
	tokens := int64(input.TokensSoFar)
	// 首字之后即为生成阶段，故「首字偏移」为 0、总时长等于已过去时长：
	// 这样 ComputeTokensPerSecond 算出的正是「首字后的生成速率」，与终态判定的
	// generationMs = duration - firstByte 同义。
	zeroFirstByte := 0.0
	elapsedMs := float64(input.ElapsedSinceFirstByte) / float64(time.Millisecond)
	rate := pubstatus.ComputeTokensPerSecond(&tokens, &elapsedMs, &zeroFirstByte)
	if rate == nil {
		return false
	}
	return *rate < input.Baseline*float64(params.RatioPerMille)/1000
}
