package route

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// errNoSource 表示构造选路器时未提供数据源。
var errNoSource = errors.New("route: 未提供 Source")

// ChainItem 是 provider_chain 中的一项，字段名与 Node 的 ProviderChainItem 对齐
// （只覆盖选路侧产出的字段；终态链负责追加状态码、耗时与错误详情）。
type ChainItem struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`

	// 供应商维度（便于日志审计，无需额外 join）。
	// VendorID 在 Node 侧是可选项（未关联厂时不写），故带 omitempty。
	VendorID     *int64               `json:"vendorId,omitempty"`
	ProviderType convert.ProviderType `json:"providerType"`

	// Reason 与 SelectionMethod 是选中/排除的原因（见 reason.go 的枚举）。
	Reason          string `json:"reason,omitempty"`
	SelectionMethod string `json:"selectionMethod,omitempty"`

	// 供应商配置（决策依据）。这四项与 CircuitState 在黄金样本里恒出现（未分组时
	// groupTag 显式为 null），故不加 omitempty——否则会与 Node 的落库形态差一个键。
	Priority       *int        `json:"priority"`
	Weight         *int        `json:"weight"`
	CostMultiplier json.Number `json:"costMultiplier"`
	GroupTag       *string     `json:"groupTag"`

	// 健康状态快照。
	CircuitState string `json:"circuitState"`

	// 亲和命中详情（仅 affinity_hit 时填充）。
	Affinity *ChainAffinity `json:"affinity,omitempty"`

	// DecisionContext 是完整的选择留痕。
	DecisionContext *DecisionContext `json:"decisionContext,omitempty"`

	// AttemptNumber 与 Timestamp 是尝试信息。
	AttemptNumber int   `json:"attemptNumber,omitempty"`
	Timestamp     int64 `json:"timestamp,omitempty"`

	// === 尝试结局的细节（Node 的 addProviderToChain 在同一条目上写这几个键） ===
	//
	// 为什么用指针/omitempty：Node 侧无值时是 `undefined`，JSON 序列化后**该键不存在**；
	// 写成 0/空串会让前端把「本轮没发生 HTTP 交换」渲染成「HTTP 0」。
	//
	// 以下六项只属**尝试期**条目（Node 的 chain[1..] 有、chain[0] 没有）：带 omitempty 是必要的，
	// 选择期条目不得写出这些键，否则链首键集会与黄金样本的 chain[0] 不一致
	// （chain_test.go 的 TestChainItemMatchesGoldenKeySet 钉着键集）。
	//
	// EndpointID / EndpointURL 是本次尝试实际使用的端点（一个供应商可有多个端点，
	// 只记供应商会丢掉「打的哪个端点」）。
	EndpointID  *int64 `json:"endpointId,omitempty"`
	EndpointURL string `json:"endpointUrl,omitempty"`
	// StatusCode 只在**真实发生过 HTTP 交换**时才有：成功/竞速胜者/上游报错/引流计费
	// ——而选路类条目（initial_selection / session_reuse / affinity_hit / hedge_launched）
	// 与本地拒绝类（concurrent_limit_failed / endpoint_pool_exhausted）**不写**。
	StatusCode *int `json:"statusCode,omitempty"`
	// ErrorMessage 记上游报错（失败路径），成功与选路条目不写。
	ErrorMessage string `json:"errorMessage,omitempty"`
	// ModelRedirect 是该次尝试实际生效的模型重定向快照（无重定向时不写这个键）。
	ModelRedirect *ChainModelRedirect `json:"modelRedirect,omitempty"`
}

// ChainModelRedirect 是链项上的模型重定向快照，逐字对应 Node 的
// `ProviderChainItem.modelRedirect`（由 `model-redirector.ts` 产出）。
//
// 键名必须与 Node 完全一致：前端 `LogicTraceTab` 直接读 `item.modelRedirect.originalModel`
// 与 `.redirectedModel`，改名即渲染空白。
//
// BillingModel 与原始模型同值是有意的：Node 写的就是 `billingModel: originalModel`
// （计费依据是**用户请求的模型**，不是转发目标），两侧必须逐字一致，否则对拍会差一个值。
type ChainModelRedirect struct {
	OriginalModel   string                  `json:"originalModel"`
	RedirectedModel string                  `json:"redirectedModel"`
	BillingModel    string                  `json:"billingModel"`
	MatchedRule     *ChainModelRedirectRule `json:"matchedRule,omitempty"`
}

// ChainModelRedirectRule 是命中的重定向规则详情（Node 的 modelRedirect.matchedRule）。
type ChainModelRedirectRule struct {
	MatchType string `json:"matchType"`
	Source    string `json:"source"`
	Target    string `json:"target"`
}

// ChainAffinity 是亲和命中的落链细节，对应 Node 的 ProviderChainItem.affinity。
type ChainAffinity struct {
	MatchedDepth      *int   `json:"matchedDepth"`
	MatchedPrefixByte *int   `json:"matchedPrefixBytes"`
	MatchedFP         string `json:"matchedFp"`
}

// ChainItem 把一次选路的 Result 投影为落链项。
//
// 快照口径：priority / weight / costMultiplier / groupTag 一律取「选中那一刻」的值，
// 与 Node 记录决策依据的语义一致——这些值随后可能被管理面改掉，落链必须留当时的读数。
func (r Result) ChainItem() ChainItem {
	if r.Provider == nil {
		return ChainItem{Reason: string(r.Reason), SelectionMethod: string(r.Method)}
	}
	p := *r.Provider
	priority := p.EffectivePriority()
	weight := p.Weight
	item := ChainItem{
		ID:              p.ID,
		Name:            p.Name,
		VendorID:        p.ProviderVendorID,
		ProviderType:    p.ProviderType,
		Reason:          string(r.Reason),
		SelectionMethod: string(r.Method),
		Priority:        &priority,
		Weight:          &weight,
		CostMultiplier:  p.CostMultiplier,
		GroupTag:        p.GroupTag,
		CircuitState:    string(r.CircuitState),
	}
	context := r.Context
	item.DecisionContext = &context
	item.Timestamp = r.Timestamp()
	if r.Affinity != nil {
		depth := r.Affinity.MatchedDepth
		prefixBytes := r.Affinity.MatchedPrefixByte
		item.Affinity = &ChainAffinity{
			MatchedDepth:      &depth,
			MatchedPrefixByte: &prefixBytes,
			MatchedFP:         r.Affinity.Hint.MatchedFP,
		}
	}
	return item
}

// Timestamp 返回结果产出的毫秒时间戳；未注入时钟时用当前时间。
// 它不改变选路语义，只影响落链的时间字段。
func (r Result) Timestamp() int64 {
	if r.timestamp == 0 {
		return time.Now().UnixMilli()
	}
	return r.timestamp
}
