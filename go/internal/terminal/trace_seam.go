package terminal

import (
	"time"
	"unicode/utf8"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件是**终态上报的接缝**：terminal 只负责把「一次终态的事实」交出去，不关心对方是
// Langfuse、内存收集器还是 nil（未装配）。三条约束与 rollup.go / notify.go 同构，理由也相同：
//
//  1. **失败不得影响结算**：上报接口**没有返回值**，实现必须自带降级；调用方在旁路前后
//     不改变控制流。
//  2. **时机：终态提交之后、且只在赢得终态时**。用 `Result.Committed` 当闸门同时解决重复上报：
//     重复结算会拿到 `Committed=false`。
//  3. **不做重试**。上报是观测事实，丢了只丢一条 trace，不值得为它引入重试队列与背压。
//
// 上报**不含正文**：TraceRecord 结构上就没有 prompt / completion 字段，这是 PII 面最小化的
// 结构性保证，而不是靠调用方自觉。

// maxTraceErrorBytes 是错误文本的上报长度上限。
//
// 上游回显可能很长（甚至整段 HTML 错误页），而错误文本是唯一可能带内容的字段，故截断。
// 取 512 字节：够看清是哪类错误，不足以装下整段回显。
const maxTraceErrorBytes = 512

// TraceRecord 是一次终态的可上报事实快照。
//
// 刻意是与 pctx 无关的纯数据：上报实现只依赖本包，不必懂请求上下文；也让单测能直接构造
// 一条事实断言序列化结果，不需要造整个上下文。
type TraceRecord struct {
	// ID 是请求日志行 id，也是上报事件 id 的来源（同一行的事实恒得同一个事件 id）。
	ID int64
	// UserID 是归属用户（无鉴权上下文的路径为 0）。
	UserID int64
	// Name 是展示名（方法 + 路径），用于在收集器上按接口分组。
	Name string
	// StartedAt / EndedAt 是本次请求的起止时刻。EndedAt 由 StartedAt + DurationMS 推出；
	// 缺 DurationMS 时与 StartedAt 相同。
	StartedAt time.Time
	EndedAt   time.Time
	// StatusCode 是终态状态码。
	StatusCode int
	// Model 是本次实际使用的模型（可能为空：被拦截的请求没有模型）。
	Model string
	// Usage 是 token 计数（指针：nil 表示该项没有值，不上报 0）。
	Usage Usage
	// HasCost 为假表示本次不计费（价格缺失、被拦截、replay 等），此时 CostUSD 为空。
	HasCost bool
	// CostUSD 是成本十进制串（账务口径原样，不预先转浮点）。
	CostUSD string
	// ErrorMessage / BlockedBy 是错误事实（已按 maxTraceErrorBytes 截断）。
	ErrorMessage string
	BlockedBy    string
	// ProviderChain / RoutingTrace 是选路留痕（jsonb 原文，未解析）。
	ProviderChain []byte
	RoutingTrace  []byte
}

// Tracer 是终态上报的接收面。
//
// 用窄接口而不是直接要求 `*tracing.Tracer`：terminal 只负责把事实交出去，不关心对方是
// 出站 HTTP、内存队列还是 nil（未装配）。
type Tracer interface {
	// RecordTerminal 接收一条终态事实。**不得阻塞、不得 panic、不得返回错误**（签名上就没有）：
	// 实现应把上报做成 fire-and-forget。
	RecordTerminal(record TraceRecord)
}

// traceTerminal 把一次已提交的终态折算成上报事实。
//
// 闸门与 affinityWriteback 一致：只在赢得终态之后发（未赢得终态说明这一行不由本请求负责，
// 重复结算也不该重复上报）。**无行标识时不发**——那只会产生一条没有主键的 trace，
// 在收集器上无法与任何一条请求对上（拦截类终态的建行发生在结算内部，pc 里还没有 id）。
func (s *Settler) traceTerminal(pc *pctx.Context, settlement Settlement, result Result) {
	if s == nil || s.tracer == nil || !result.Committed || pc == nil {
		return
	}
	id, ok := pc.MessageRequestID()
	if !ok || id <= 0 {
		return
	}

	record := TraceRecord{
		ID:            id,
		Name:          pc.Method() + " " + pc.Path(),
		StartedAt:     pc.StartedAt(),
		EndedAt:       endedAt(pc.StartedAt(), settlement.DurationMS),
		StatusCode:    settlement.StatusCode,
		Usage:         settlement.Usage,
		ProviderChain: settlement.ProviderChain,
		RoutingTrace:  settlement.RoutingTrace,
	}
	if auth, ok := pc.Auth(); ok {
		record.UserID = auth.UserID
	}
	if settlement.Model != nil {
		record.Model = *settlement.Model
	}
	if settlement.Cost != nil {
		record.HasCost = true
		record.CostUSD = settlement.Cost.Total
	}
	if settlement.ErrorMessage != nil {
		record.ErrorMessage = truncateText(*settlement.ErrorMessage, maxTraceErrorBytes)
	}
	if settlement.BlockedBy != nil {
		record.BlockedBy = *settlement.BlockedBy
	}
	s.tracer.RecordTerminal(record)
}

// endedAt 由起始时刻与耗时推出结束时刻；缺耗时（拦截类终态、未到达上游）时退回起始时刻，
// 不凭空造一段时长。
func endedAt(startedAt time.Time, durationMS *int) time.Time {
	if durationMS == nil {
		return startedAt
	}
	return startedAt.Add(time.Duration(*durationMS) * time.Millisecond)
}

// truncateText 按字节上限截断文本，并回退到最后一个完整 UTF-8 起始字节——
// 直接切字节会把多字节字符切成半个，落进 JSON 后是非法字符串。
func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "(truncated)"
}
