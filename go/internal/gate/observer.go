package gate

import (
	"time"
)

// ShadowObserverMaxBufferBytes 与 TS 的 STREAM_SHADOW_OBSERVER_MAX_BUFFER_CHARACTERS 一致：
// shadow 只做旁路诊断，不需要保留完整大帧；超限即停止观测，绝不因异常上游输入把进程堆撑大。
const ShadowObserverMaxBufferBytes = 1024 * 1024

// ShadowReport 是 shadow 观测的一次性报告，在首个决定性帧（content/error/malformed）
// 出现时产出。它只含判定与计数，不含正文原文。
type ShadowReport struct {
	Family          Family
	ProviderID      int
	ProviderName    string
	DecisiveVerdict Verdict
	// Divergent 表示现状「首非空字节即提交」与门控「首有效内容才提交」的判定分歧：
	// true 意味着门控会推迟提交（中性前缀）或触发 failover（error/malformed）。
	Divergent bool
	// FirstContentLag 是首字节到首个决定性帧的间隔；没有首字节时为 0。
	FirstContentLag time.Duration
	// VerdictCounts 是迄今各态计数。
	VerdictCounts map[Verdict]int
	// Incomplete 表示观测因缓冲超限或异常而提前终止（对应 TS 的 observation-incomplete）。
	Incomplete bool
}

// ShadowConfig 是 shadow 观测配置。
type ShadowConfig struct {
	Family       Family
	ProviderID   int
	ProviderName string
	// MaxBufferedBytes 为 0 时取 ShadowObserverMaxBufferBytes。
	MaxBufferedBytes int
	// OnReport 收到报告；nil 表示丢弃（调用方通常接到日志或指标上）。
	OnReport func(ShadowReport)
	// Now 便于测试注入时钟；nil 时用 time.Now。
	Now func() time.Time
}

// ShadowObserver 是 shadow 模式的旁路观察者：不缓冲不 failover，只统计
// 「首非空字节 vs 首有效内容帧」的延迟差与提交前的判定分布，用于灰度前评估。
//
// 非并发安全：每个上游流一个实例。
type ShadowObserver struct {
	config   ShadowConfig
	parser   *Parser
	counts   map[Verdict]int
	firstAt  time.Time
	reported bool
}

// NewShadowObserver 构造旁路观察者。
func NewShadowObserver(config ShadowConfig) *ShadowObserver {
	maxBufferedBytes := config.MaxBufferedBytes
	if maxBufferedBytes <= 0 {
		maxBufferedBytes = ShadowObserverMaxBufferBytes
	}
	return &ShadowObserver{
		config:  config,
		parser:  NewParser(ParserOptions{MaxBufferedBytes: maxBufferedBytes}),
		counts:  make(map[Verdict]int, 5),
		firstAt: time.Time{},
	}
}

// Observe 喂入一个上游 chunk。观测绝不影响热路径：任何分类或解析异常都只终止观测。
func (o *ShadowObserver) Observe(chunk []byte) {
	if o.reported {
		return
	}
	if o.firstAt.IsZero() && len(chunk) > 0 {
		o.firstAt = o.now()
	}
	if _, err := o.parser.Visit(chunk, func(frame Frame) bool {
		verdict := Classify(o.config.Family, frame.Event, frame.Data)
		o.counts[verdict]++
		switch verdict {
		case VerdictContent, VerdictError, VerdictMalformed:
			o.report(verdict, false)
			return false
		default:
			return true
		}
	}); err != nil {
		o.report("", true)
	}
}

func (o *ShadowObserver) report(verdict Verdict, incomplete bool) {
	if o.reported {
		return
	}
	o.reported = true
	if o.config.OnReport == nil {
		return
	}
	counts := make(map[Verdict]int, len(o.counts))
	for key, value := range o.counts {
		counts[key] = value
	}
	report := ShadowReport{
		Family:          o.config.Family,
		ProviderID:      o.config.ProviderID,
		ProviderName:    o.config.ProviderName,
		DecisiveVerdict: verdict,
		Divergent: verdict != VerdictContent ||
			counts[VerdictNeutral]+counts[VerdictTerminal] > 0,
		VerdictCounts: counts,
		Incomplete:    incomplete,
	}
	if !o.firstAt.IsZero() {
		report.FirstContentLag = o.now().Sub(o.firstAt)
	}
	o.config.OnReport(report)
}

func (o *ShadowObserver) now() time.Time {
	if o.config.Now != nil {
		return o.config.Now()
	}
	return time.Now()
}
