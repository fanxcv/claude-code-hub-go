package gate

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// 本文件是「提交前分级速率闸」的纯逻辑：语义 payload 计量 + 三档离散检查点状态机。
//
// 为什么要后移提交点：门控原本在首个有效内容帧到达时立即提交，客户端随即收到字节，
// 此后既不能改状态码也不能换家。而渠道「有内容但极慢」的病症恰恰发生在这里——既有中途
// 探测只判「T 秒内零内容」，对「每隔几秒吐一点」完全无感，于是请求一路磨到 100 秒以上。
// 故必须在提交前暂存一小段内容、量其产出速率，确认正常才提交。
//
// 判据由用户给定（2026-09-22），逐字落在此处：
//
//	t=1s  首秒窗口速率 > 2θ  -> 提交
//	t=3s  前 3s 平均    > θ   -> 提交
//	t=10s 前 10s 平均   > θ   -> 提交；否则判慢
//
// θ 是该渠道的速率阈值（语义字节/秒），由接线层按模型基线换算后经 Options.PrecommitRate 传入。
//
// 为什么必须是**离散检查点**：若改成「累计字节一旦越过阈值即提交」，则 3s 档（累计 > 3θ）
// 在数学上恒被 1s 档（累计 > 2θ）抢先命中而成为死代码——累计量单调递增，能到 3θ 必先越过
// 2θ。离散检查点让三档各有独立含义：1s 档抓「一开始就快」，3s 档抓「首秒慢但随后追上」，
// 10s 档才判死。

// 三档检查点与首档倍数。三个时长与「2 倍」都取自用户给定的判据，属**结构性常量**而非
// 可调阈值；阈值 θ 才是可配项（PrecommitRate）。改这几个常量等于改判据语义。
const (
	// PrecommitFirstCheckpoint 是首档检查点：自首个语义内容帧起 1 秒。
	PrecommitFirstCheckpoint = 1 * time.Second
	// PrecommitSecondCheckpoint 是次档检查点：自首个语义内容帧起 3 秒。
	PrecommitSecondCheckpoint = 3 * time.Second
	// PrecommitVerdictDeadline 是裁决期限：自首个语义内容帧起 10 秒仍未达标即判慢。
	PrecommitVerdictDeadline = 10 * time.Second
	// PrecommitFastMultiplier 是首档的倍数：首秒速率须超过 2θ 才放行。
	//
	// 首档取 2 倍而非 1 倍，是因为 1 秒的样本噪声大，需要一个余量才敢认定「明确不慢」；
	// 次档起样本变长，阈值回到 θ 本身。
	PrecommitFastMultiplier = 2
)

// ladderStage 是一档检查点：到 elapsed 时刻按 multiplier×θ 判定。
type ladderStage struct {
	elapsed    time.Duration
	multiplier int
}

// ladderStages 是检查点序列，顺序即语义（末档不过即判慢）。
var ladderStages = [3]ladderStage{
	{PrecommitFirstCheckpoint, PrecommitFastMultiplier},
	{PrecommitSecondCheckpoint, 1},
	{PrecommitVerdictDeadline, 1},
}

// LadderDecision 是一次检查点的裁决结果。
type LadderDecision string

const (
	// LadderContinue 表示本档未达标但尚未到期，继续观察到下一档。
	LadderContinue LadderDecision = "continue"
	// LadderCommit 表示速率达标，可以提交（放开）。
	LadderCommit LadderDecision = "commit"
	// LadderSlow 表示三档皆未达标，判定为低速请求。
	LadderSlow LadderDecision = "slow"
)

// Ladder 是提交前速率闸的状态机。零值不可用；用 NewLadder 构造（rate<=0 时返回 nil）。
//
// 非并发安全：每个上游流一个实例，与门控读侧同 goroutine。
type Ladder struct {
	rate      int
	startedAt time.Time
	started   bool
	payload   int
	stage     int
	// stages 覆盖出厂三档；nil 时用 ladderStages。存在的唯一理由是让包内单测把
	// 1s/3s/10s 缩到毫秒级——否则每个用例要真等 10 秒。
	stages []ladderStage
}

// NewLadder 构造速率闸；rateBytesPerSecond <= 0 表示不启用，返回 nil。
func NewLadder(rateBytesPerSecond int) *Ladder {
	if rateBytesPerSecond <= 0 {
		return nil
	}
	return &Ladder{rate: rateBytesPerSecond}
}

// newLadderWithStages 用指定档位构造速率闸（stages 为空时用出厂三档）。
func newLadderWithStages(rateBytesPerSecond int, stages []ladderStage) *Ladder {
	ladder := NewLadder(rateBytesPerSecond)
	if ladder == nil {
		return nil
	}
	if len(stages) > 0 {
		ladder.stages = stages
	}
	return ladder
}

// allStages 返回本次生效的档位序列。
func (l *Ladder) allStages() []ladderStage {
	if len(l.stages) > 0 {
		return l.stages
	}
	return ladderStages[:]
}

// Enabled 报告速率闸是否可用（nil 接收者安全）。
func (l *Ladder) Enabled() bool {
	return l != nil && l.rate > 0
}

// Observe 记入一帧的语义 payload 字节数，并在首个**非零**计量到达时启动时钟。
//
// 时钟起点是首个语义内容帧而非首个非空字节：中性头帧与心跳不启动时钟，
// 否则「上游先发 3 秒心跳再正常吐字」会被算成低速。零字节或负值一律不启动。
func (l *Ladder) Observe(payloadBytes int, now time.Time) {
	if !l.Enabled() || payloadBytes <= 0 {
		return
	}
	if !l.started {
		l.started = true
		l.startedAt = now
	}
	l.payload += payloadBytes
}

// Started 报告时钟是否已启动（尚无语义内容帧时为假）。
func (l *Ladder) Started() bool {
	return l.Enabled() && l.started
}

// PayloadBytes 返回自时钟起点累计的语义 payload 字节数。
func (l *Ladder) PayloadBytes() int {
	if !l.Enabled() {
		return 0
	}
	return l.payload
}

// StartedAt 返回时钟起点（未启动时为零值）。
func (l *Ladder) StartedAt() time.Time {
	if !l.Enabled() {
		return time.Time{}
	}
	return l.startedAt
}

// Deadline 返回下一档检查点的绝对时刻；未启动或已无档位时返回 false。
func (l *Ladder) Deadline() (time.Time, bool) {
	if !l.Enabled() || !l.started {
		return time.Time{}, false
	}
	stages := l.allStages()
	if l.stage >= len(stages) {
		return time.Time{}, false
	}
	return l.startedAt.Add(stages[l.stage].elapsed), true
}

// StageCount 返回本次生效的档位数。
func (l *Ladder) StageCount() int {
	if !l.Enabled() {
		return 0
	}
	return len(l.allStages())
}

// Stage 返回尚未裁决的档位序号（0 起）。用于可观测性与测试。
func (l *Ladder) Stage() int {
	if !l.Enabled() {
		return -1
	}
	return l.stage
}

// Evaluate 在当前时刻裁决下一档。
//
// 速率用**实际流逝**而非档位名义时长做分母：若某档因故被晚判（读侧被更大的超时压住），
// 用实际流逝会让算出的速率偏低，从而偏保守地不提交——误杀一个只是「没赶上检查点」的
// 正常请求，代价远低于把一个慢请求放行。名义时长只在检查点准时命中时等于实际流逝。
func (l *Ladder) Evaluate(now time.Time) LadderDecision {
	if !l.Enabled() || !l.started {
		return LadderContinue
	}
	stages := l.allStages()
	if l.stage >= len(stages) {
		return LadderSlow
	}
	stage := stages[l.stage]
	if now.Before(l.startedAt.Add(stage.elapsed)) {
		return LadderContinue
	}
	if need := l.requiredBytes(now, stage); int64(l.payload) > need {
		return LadderCommit
	}
	l.stage++
	if l.stage >= len(stages) {
		return LadderSlow
	}
	return LadderContinue
}

// requiredBytes 是本档达标所需的累计字节数：θ × 倍数 × 实际流逝秒数。
//
// 整数运算（毫秒精度）避免浮点边界抖动：θ=50 时首档恰需 100 字节。
func (l *Ladder) requiredBytes(now time.Time, stage ladderStage) int64 {
	elapsedMillis := now.Sub(l.startedAt).Milliseconds()
	return int64(l.rate) * int64(stage.multiplier) * elapsedMillis / 1000
}

// SemanticPayloadBytes 计量一帧的「语义 payload」字节数。
//
// 口径（用户所定）：text / thinking / tool arguments 的 delta 计入；头帧、心跳、usage、
// 终止包装、JSON 外壳、SSE 的 `event:`/`data:` 前缀一律不计入。之所以不计外壳：速率是用来
// 判「模型吐字快不快」的，把 JSON 键名与引号算进去会让一条只发心跳的流看起来有产出。
//
// 复用 classify.go 的帧规则：命中的**首条**内容规则决定计量口径（与 Verdict 取首条命中
// 同源），故不会因多条规则重叠而重复计数。
func SemanticPayloadBytes(family Family, event string, data string) int {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return 0
	}
	signal, ok := streamSignals[family]
	if !ok {
		return 0
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return 0
	}
	if !isObjectLike(parsed) {
		return 0
	}
	effective := effectiveEventName(event, parsed)
	for _, rule := range signal.contentRules {
		if !frameRuleMatches(rule, effective, parsed) {
			continue
		}
		if bytes := rulePayloadBytes(rule, parsed); bytes > 0 {
			return bytes
		}
		// 规则命中但载荷是容器（如 web_search_tool_result 的 content 是对象集合，
		// semanticValueBytes 只认标量）⇒ 整帧即 opaque payload。
		// 不这么兑底会让这类流「是内容却量不到字节」，时钟永不启动、速率闸永不裁决。
		return len(trimmed)
	}
	// 无内容规则命中却仍被判为内容的帧（openai-responses 的 remote compaction）：
	// 整帧就是 opaque payload，按整帧字节计。漏掉它会让「只发 compaction 的流」看起来零产出。
	if family == FamilyOpenAIResponses && isResponsesCompactionContent(effective, parsed) {
		return len(trimmed)
	}
	return 0
}

// rulePayloadBytes 汇总一条内容规则各 anyPaths 解析出的语义字节。
func rulePayloadBytes(rule frameRule, parsed any) int {
	total := 0
	for _, path := range rule.anyPaths {
		total += semanticValueBytes(ResolvePath(parsed, path))
	}
	return total
}

// semanticValueBytes 只对**标量文本**计量：字符串按 UTF-8 字节数，数字按其字面量。
// 容器不递归取值（路径已定位到标量字段，递归会把结构开销算成产出）。
func semanticValueBytes(value any) int {
	switch typed := value.(type) {
	case string:
		return len(typed)
	case float64:
		return len(strconv.FormatFloat(typed, 'g', -1, 64))
	case []any:
		total := 0
		for _, item := range typed {
			total += semanticValueBytes(item)
		}
		return total
	default:
		return 0
	}
}
