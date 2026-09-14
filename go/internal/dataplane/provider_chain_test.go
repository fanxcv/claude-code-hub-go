package dataplane

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 本文件钉住 provider_chain 的**形状**：链首是选择期条目、其后是尝试期条目。
//
// 依据是使用记录页弹窗读法（provider-chain-popover.tsx:522,526,533 都读 chain[0]）
// 与黄金样本（testdata/golden/message_request_row.json 的 chain[0] = initial_selection、
// chain[1] = request_success）。缺链首会让界面看不到「渠道复用 / 新会话」。

// chainTestSettler 造一个带请求上下文的结算器（P C 用于读选择期留痕）。
func chainTestSettler(t *testing.T) *storeSettler {
	t.Helper()
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}
	return &storeSettler{
		state:  &RequestState{StartedAt: time.Unix(1789203897, 0), PC: pc},
		logger: logx.New(nil),
	}
}

// encodeChainItem 把选路结果序列化成守卫链装入上下文的那种形态。
func encodeChainItem(t *testing.T, item route.ChainItem) []byte {
	t.Helper()
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("序列化链条目失败: %v", err)
	}
	return raw
}

func decodeChain(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("provider_chain 不是合法 JSON 数组: %v (%s)", err, payload)
	}
	return out
}

// TestProviderChainPrependsAffinityHitAsHead 亲和命中必须是链首，且带亲和详情与决策上下文。
//
// 生产形态（Node 时代实测）：affinity_hit -> retry_failed -> hedge_launched。
func TestProviderChainPrependsAffinityHitAsHead(t *testing.T) {
	settler := chainTestSettler(t)
	settler.state.PC.SetSelectionChainEntry(encodeChainItem(t, route.ChainItem{
		ID:              156,
		Name:            "HC_Chat",
		Reason:          string(route.ReasonSelectedAffinity),
		SelectionMethod: string(route.MethodPrefixAffinity),
		Affinity: &route.ChainAffinity{
			MatchedDepth:      iptr(153),
			MatchedPrefixByte: iptr(246580),
			MatchedFP:         "144fa77a62df0cb72441809f3198b776",
		},
	}))

	chain := decodeChain(t, settler.providerChain([]forward.AttemptOutcome{
		{ProviderID: 156, ProviderName: "HC_Chat", Attempt: 1, Reason: "retry_failed", StatusCode: 502, EndpointID: 714, EndpointURL: "http://hc:4321/", Message: "上游 502"},
		{ProviderID: 149, ProviderName: "ARK Codex", Attempt: 2, Reason: "hedge_launched"},
	}))

	if len(chain) != 3 {
		t.Fatalf("链应有 3 项（选择期 1 + 尝试期 2），实际 %d：%v", len(chain), chain)
	}
	if chain[0]["reason"] != "affinity_hit" {
		t.Fatalf("链首应为 affinity_hit，实际 %v", chain[0]["reason"])
	}
	if chain[0]["selectionMethod"] != "prefix_affinity" {
		t.Fatalf("链首 selectionMethod 应为 prefix_affinity，实际 %v", chain[0]["selectionMethod"])
	}
	affinity, ok := chain[0]["affinity"].(map[string]any)
	if !ok {
		t.Fatalf("链首缺少 affinity 详情：%v", chain[0])
	}
	if affinity["matchedFp"] != "144fa77a62df0cb72441809f3198b776" {
		t.Fatalf("affinity.matchedFp 不对：%v", affinity["matchedFp"])
	}
	if chain[1]["reason"] != "retry_failed" || chain[2]["reason"] != "hedge_launched" {
		t.Fatalf("尝试期条目的顺序/原因不对：%v", chain)
	}
}

// TestProviderChainDropsInitialSelectionWhenFirstAttemptFailed 首尝试失败时不补 initial_selection。
//
// Node 只在 `attemptCount === 1` 的成功支路写该条目（provider-selector.ts:476），
// 否则决策链会看起来「全部是加权随机初选」。
func TestProviderChainDropsInitialSelectionWhenFirstAttemptFailed(t *testing.T) {
	settler := chainTestSettler(t)
	settler.state.PC.SetSelectionChainEntry(encodeChainItem(t, route.ChainItem{
		ID:     156,
		Name:   "HC_Chat",
		Reason: string(route.ReasonSelectedInitial),
	}))

	chain := decodeChain(t, settler.providerChain([]forward.AttemptOutcome{
		{ProviderID: 156, ProviderName: "HC_Chat", Attempt: 1, Reason: "retry_failed"},
		{ProviderID: 149, ProviderName: "ARK Codex", Attempt: 2, Reason: "retry_success"},
	}))

	if len(chain) != 2 {
		t.Fatalf("首尝试失败时不应补 initial_selection，链应为 2 项，实际 %d：%v", len(chain), chain)
	}
	if chain[0]["reason"] != "retry_failed" {
		t.Fatalf("链首应为首个尝试，实际 %v", chain[0]["reason"])
	}
}

// TestProviderChainKeepsInitialSelectionOnFirstAttemptSuccess 首尝试成功时链首是 initial_selection，
// 与黄金样本的形状一致（chain[0] = initial_selection、chain[1] = request_success）。
func TestProviderChainKeepsInitialSelectionOnFirstAttemptSuccess(t *testing.T) {
	settler := chainTestSettler(t)
	settler.state.PC.SetSelectionChainEntry(encodeChainItem(t, route.ChainItem{
		ID:              1,
		Name:            "mock-upstream",
		Reason:          string(route.ReasonSelectedInitial),
		SelectionMethod: string(route.MethodWeightedRandom),
	}))

	chain := decodeChain(t, settler.providerChain([]forward.AttemptOutcome{
		{ProviderID: 1, ProviderName: "mock-upstream", Attempt: 1, Reason: forward.ReasonRequestSuccess, StatusCode: 200},
	}))

	if len(chain) != 2 {
		t.Fatalf("链应有 2 项（选择期 + 尝试期），实际 %d：%v", len(chain), chain)
	}
	if chain[0]["reason"] != "initial_selection" || chain[1]["reason"] != "request_success" {
		t.Fatalf("链应为 initial_selection -> request_success，实际 %v -> %v", chain[0]["reason"], chain[1]["reason"])
	}
}

// TestProviderChainAttemptEntryCarriesAttemptFactsOnly 尝试期条目只带「这一刻的事实」：
// 端点、状态码、失败说明要有；选路期的 selectionMethod / affinity / decisionContext 不得出现
// （它们只属于链首，Node 的 chain[1..] 也没有它们）。
func TestProviderChainAttemptEntryCarriesAttemptFactsOnly(t *testing.T) {
	settler := chainTestSettler(t)
	settler.state.PC.SetSelectionChainEntry(encodeChainItem(t, route.ChainItem{
		ID:     7,
		Name:   "甲",
		Reason: string(route.ReasonSelectedAffinity),
	}))
	// 选路留痕仍带决策上下文，用于验证尝试条目会把它清掉。
	settler.state.recordSelection(7, route.Result{
		Provider: &route.Provider{ID: 7, Name: "甲"},
		Context:  route.DecisionContext{TotalProviders: 3},
		Method:   route.MethodPrefixAffinity,
		Reason:   route.ReasonSelectedAffinity,
	})

	chain := decodeChain(t, settler.providerChain([]forward.AttemptOutcome{
		{
			ProviderID: 7, ProviderName: "甲", Attempt: 1, Reason: "retry_failed",
			EndpointID: 714, EndpointURL: "http://hc:4321/", StatusCode: 502, Message: "上游 502",
		},
	}))
	attempt := chain[len(chain)-1]

	for _, want := range []string{"id", "name", "reason", "attemptNumber", "endpointId", "endpointUrl", "statusCode", "errorMessage"} {
		if _, ok := attempt[want]; !ok {
			t.Fatalf("尝试期条目缺少 %q：%v", want, attempt)
		}
	}
	for _, forbidden := range []string{"selectionMethod", "affinity", "decisionContext"} {
		if _, ok := attempt[forbidden]; ok {
			t.Fatalf("尝试期条目不应有 %q（只属于链首）：%v", forbidden, attempt)
		}
	}
	if attempt["statusCode"] != float64(502) || attempt["endpointId"] != float64(714) {
		t.Fatalf("尝试期条目的结局事实不对：%v", attempt)
	}
}

// TestProviderChainWithoutSelectionEntryKeepsAttemptsOnly 没有选择期留痕时不编造链首
// （守卫链未接线或选路失败时，链就是尝试本身）。
func TestProviderChainWithoutSelectionEntryKeepsAttemptsOnly(t *testing.T) {
	settler := chainTestSettler(t)
	chain := decodeChain(t, settler.providerChain([]forward.AttemptOutcome{
		{ProviderID: 9, ProviderName: "丙", Attempt: 3, Reason: "hedge_loser_billed"},
	}))
	if len(chain) != 1 || chain[0]["reason"] != "hedge_loser_billed" {
		t.Fatalf("无留痕时链应为尝试本身，实际 %v", chain)
	}
}

func iptr(value int) *int { return &value }

// TestIntegrationChainHeadIsSelectionEntry 端到端钉住「守卫链 → pctx → 数据面」的接线：
// 真库真上游打一条请求，落库的 provider_chain[0] 必须是**选择期**条目（initial_selection
// 或 affinity_hit），且决策上下文非零（totalProviders 有真值）。
//
// 为什么需要它：链首是界面弹窗判定「渠道复用 / 新会话」的唯一输入
// （provider-chain-popover.tsx:526）。单元测试只能证明「给了留痕就能排出链首」，
// 证明不了生产接线真的把留痕装进了上下文——这条用例覆盖后半段。
func TestIntegrationChainHeadIsSelectionEntry(t *testing.T) {
	pools := integrationStore(t)
	upstream := newIntegrationStreamUpstream(t, http.StatusOK)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))
	integrationStreamRequest(t, pools, provisioned, "/v1/messages")
	row := waitForFinalizedRow(t, provisioned)

	chain := providerChainOf(t, row)
	if len(chain) == 0 {
		t.Fatalf("provider_chain 不应为空：%v", row["provider_chain"])
	}
	head := chain[0]
	reason, _ := head["reason"].(string)
	if reason != "initial_selection" && reason != "affinity_hit" {
		t.Fatalf("链首应是选择期条目（initial_selection/affinity_hit），实际 %q：%v", reason, head)
	}
	context, ok := head["decisionContext"].(map[string]any)
	if !ok {
		t.Fatalf("链首缺少 decisionContext：%v", head)
	}
	if total, _ := context["totalProviders"].(float64); total < 1 {
		t.Fatalf("decisionContext.totalProviders 应为真实候选数（>=1），实际 %v：%v", context["totalProviders"], context)
	}
}
