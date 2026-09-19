package terminal

import (
	"fmt"
	"regexp"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// Settlement 是一次终态结算的全部输入：一次 Settle 只会产生**一条** UPDATE，
// 因此这里给出的每个字段都与 status_code 同语句落库（不变量 I3）。
type Settlement struct {
	// StatusCode 是终态状态码，必填（终态写总要有个状态码；0 不是合法的 HTTP 状态）。
	StatusCode int
	// DurationMS 是请求总耗时。到达过上游的请求必须提供——它进 outbox 事件载荷，
	// 且该事件只在 status_code 首次非 NULL 的那条语句产生，事后补不上。
	DurationMS  *int
	TTFTMS      *int
	FirstByteMS *int
	Usage       Usage
	// Cost 为 nil 表示本次不计费（价格缺失、被拦截、replay 等）。
	Cost                *Cost
	ProviderChain       []byte
	ErrorMessage        *string
	ErrorStack          *string
	ErrorCause          *string
	Model               *string
	ActualResponseModel *string
	ProviderID          *int64
	RoutingTrace        []byte
	// BlockedBy 是拦截类终态的标记（sensitive_word、warmup 等）。它与 StatusCode
	// 同语句落库：blocked_by 同时被账本与 outbox 两个触发器监视。
	BlockedBy           *string
	BlockedReason       *string
	Context1mApplied    *bool
	SwapCacheTTLApplied *bool
	// CostMultiplier / GroupCostMultiplier 是本次请求实际使用的成本倍率（numeric 列的文本形）。
	// Node 在建行时写这两列（message-service.ts:125-126）；Go 在终态写，理由见
	// nil 表示不写该列。
	CostMultiplier      *string
	GroupCostMultiplier *string
	// F3b 缓存模拟列：**仅流式终态**派生（Node 只在 response-handler.ts:5431 一处产出），
	// 且仅在缓存效果开关开启时写；非流式路径与关闭开关时保持 NULL。
	// 派生规则唯一真源是 Node 的 lib/cache-effectiveness/gate.ts，等价实现见 cachescore.go。
	CacheCompatibilityKey    *string
	CacheScoreEligible       *bool
	CacheScoreExcludedReason *string
	TheoreticalCacheTokens   *int64
	CacheTTLBucket           *string
	// SpecialSettingsAppend 是**终态追加**的审计条目（JSON 数组），以 jsonb 追加语义写库：
	// 建行时（守卫链）已写客户端侧审计，终态只补自己这条，两者不得互相抹除。
	// 目前的唯一生产者是「转换后思考强度探针」（见 internal/specialsettings）。
	SpecialSettingsAppend []byte

	// Affinity 是本次终态的亲和写回指令。它**不进 patch**：不写任何数据库列，而是
	// 「终态提交后发放的副作用」，对应 Node response-handler 的 postTerminalSideEffects
	// 与 affinity-recorder 的调用点。零值表示本次不写亲和。
	//
	// 只有 SettleContext 路径会发放它：亲和写回的事实（scope、指纹、generation）
	// 存在 pctx 里，没有 pctx 的 Settle/SettleBlocked 无从写。
	Affinity AffinityDirective

	// LeaseSettlement 是本次终态的**租约结算事实**：判定时用过的切片（主体 id + 生效重置模式）。
	// 与 Affinity 同规矩：不进 patch、不写任何列，只做「成本落库后发放的副作用」的入参
	// （见 lease_settle.go）。零值表示本次不走租约结算（未装配，或该路径没有 pctx）。
	LeaseSettlement pctx.LeaseSettlementPlan
}

// AffinityDirective 是一次终态的亲和写回指令。
//
// 两个字段互斥：同一次终态不可能既成功又失败，同时给时只有墓碑会写（保守侧）。
type AffinityDirective struct {
	// WinnerProviderID > 0 表示本次是成功终态：终态提交后把 tip 绑定写回给该供应商。
	WinnerProviderID int64
	// TombstoneProviderID > 0 表示本次终态系供应商侧失败：对提名边界写短 TTL 墓碑。
	TombstoneProviderID int64
}

// ErrIncompleteTerminalPatch 表示终态 patch 会写出残缺的终态：要么没有状态码，
// 要么在到达上游的请求上漏了 duration_ms（后者会让 outbox 事件永久缺 duration_ms）。
type ErrIncompleteTerminalPatch struct {
	Reason string
}

func (e *ErrIncompleteTerminalPatch) Error() string {
	return "terminal: 终态 patch 不完整: " + e.Reason
}

func incomplete(reason string, args ...any) error {
	return &ErrIncompleteTerminalPatch{Reason: fmt.Sprintf(reason, args...)}
}

// toPatch 把结算输入编译成一条终态 patch，并在编译期拒绝会写出残缺终态的输入。
func (s Settlement) toPatch() (store.DetailsPatch, error) {
	if s.StatusCode <= 0 {
		return store.DetailsPatch{}, incomplete("状态码为 %d，终态写必须带合法状态码", s.StatusCode)
	}
	// 不变量 I3：使 status_code 非 NULL 的语句必须同时带上该行的 duration_ms。
	// 一旦上游参与过本次请求（有 provider_chain），终态就一定是一次真实尝试的结果，
	// 其 duration_ms 必须在同语句内落库，否则 outbox 事件会永久缺少 duration_ms，
	// 因为 message_request_outbox_aiud 在 OLD.status_code IS NOT NULL 时直接返回。
	if len(s.ProviderChain) > 0 && s.DurationMS == nil {
		return store.DetailsPatch{}, incomplete(
			"该请求有 provider_chain 但缺 duration_ms；" +
				"outbox 事件只在 status_code 首次非空时产生，事后补写 duration_ms 不会被重新派发")
	}

	statusCode := s.StatusCode
	patch := store.DetailsPatch{
		StatusCode:                 &statusCode,
		DurationMS:                 s.DurationMS,
		TTFTMS:                     s.TTFTMS,
		FirstByteMS:                s.FirstByteMS,
		InputTokens:                s.Usage.InputTokens,
		OutputTokens:               s.Usage.OutputTokens,
		CacheCreationInputTokens:   s.Usage.CacheCreationInputTokens,
		CacheCreation5mInputTokens: s.Usage.CacheCreation5mInputTokens,
		CacheCreation1hInputTokens: s.Usage.CacheCreation1hInputTokens,
		CacheReadInputTokens:       s.Usage.CacheReadInputTokens,
		ProviderChain:              s.ProviderChain,
		RoutingTrace:               s.RoutingTrace,
		ErrorMessage:               s.ErrorMessage,
		ErrorStack:                 s.ErrorStack,
		ErrorCause:                 s.ErrorCause,
		Model:                      s.Model,
		ActualResponseModel:        s.ActualResponseModel,
		ProviderID:                 s.ProviderID,
		BlockedBy:                  s.BlockedBy,
		BlockedReason:              s.BlockedReason,
		Context1mApplied:           s.Context1mApplied,
		SwapCacheTTLApplied:        s.SwapCacheTTLApplied,
		CostMultiplier:             s.CostMultiplier,
		GroupCostMultiplier:        s.GroupCostMultiplier,
		CacheCompatibilityKey:      s.CacheCompatibilityKey,
		CacheScoreEligible:         s.CacheScoreEligible,
		CacheScoreExcludedReason:   s.CacheScoreExcludedReason,
		TheoreticalCacheTokens:     s.TheoreticalCacheTokens,
		CacheTTLBucket:             s.CacheTTLBucket,
		SpecialSettingsAppend:      s.SpecialSettingsAppend,
	}
	if s.Usage.CacheTTL != "" {
		cacheTTL := s.Usage.CacheTTL
		patch.CacheTTLApplied = &cacheTTL
	}
	return patch, nil
}

// setColumnsPattern 匹配 store 生成的 SET 赋值里的列名（`"列名" = $n`）。
var setColumnsPattern = regexp.MustCompile(`"([a-z0-9_]+)" = \$`)

// setColumnsOf 解析语句里被赋值的列名，按出现顺序返回。列集断言因此取自语句本身，
// 而不是另一份手抄清单——手抄清单不会随实现漂移，也就不可能发现漂移。
func setColumnsOf(query string) []string {
	matches := setColumnsPattern.FindAllStringSubmatch(query, -1)
	columns := make([]string, 0, len(matches))
	for _, match := range matches {
		if match[1] == "updated_at" {
			continue
		}
		columns = append(columns, match[1])
	}
	return columns
}

// SettleTimeColumns 返回终态 patch 能写入的列集。它由一条「所有字段都置位」的样本
// 经 store.BuildDetailsPatchQuery 生成后解析得到，用于列集覆盖断言。
func SettleTimeColumns() []string {
	patch, err := maxSettlement().toPatch()
	if err != nil {
		// 样本是包内常量，编译不出来说明样本本身写错了，属于编程错误。
		panic("terminal: 列集审计样本不合法: " + err.Error())
	}
	query, _ := store.BuildDetailsPatchQuery(1, patch)
	return setColumnsOf(query)
}

// MonitoredColumnsInLatePatch 返回迟到补写（终态提交之后的 patch）里出现的
// **触发器监视列**。不变量 I4 要求它恒为空：终态后重写监视列会重建账本行，
// 并破坏 outbox 的可见性语义（routing_trace 这类非监视列才可以补写）。
func MonitoredColumnsInLatePatch(patch store.DetailsPatch) []string {
	query, _ := store.BuildDetailsPatchQuery(1, patch)
	monitored := map[string]struct{}{}
	for _, column := range ledgerMonitoredColumns {
		monitored[column] = struct{}{}
	}
	for _, column := range outboxMonitoredColumns {
		monitored[column] = struct{}{}
	}
	var hits []string
	for _, column := range setColumnsOf(query) {
		if _, isMonitored := monitored[column]; isMonitored {
			hits = append(hits, column)
		}
	}
	return hits
}

// maxSettlement 是「所有字段都置位」的结算样本，只服务于列集审计。
func maxSettlement() Settlement {
	number := 1
	text := "x"
	flag := true
	providerID := int64(1)
	tokenCount := int64(1)
	return Settlement{
		StatusCode:  200,
		DurationMS:  &number,
		TTFTMS:      &number,
		FirstByteMS: &number,
		Usage: Usage{
			InputTokens:                &tokenCount,
			OutputTokens:               &tokenCount,
			CacheCreationInputTokens:   &tokenCount,
			CacheCreation5mInputTokens: &tokenCount,
			CacheCreation1hInputTokens: &tokenCount,
			CacheReadInputTokens:       &tokenCount,
			CacheTTL:                   "5m",
		},
		ProviderChain:       []byte(`[]`),
		RoutingTrace:        []byte(`{}`),
		ErrorMessage:        &text,
		ErrorStack:          &text,
		ErrorCause:          &text,
		Model:               &text,
		ActualResponseModel: &text,
		ProviderID:          &providerID,
		BlockedBy:           &text,
		BlockedReason:       &text,
		Context1mApplied:    &flag,
		SwapCacheTTLApplied: &flag,
	}
}
