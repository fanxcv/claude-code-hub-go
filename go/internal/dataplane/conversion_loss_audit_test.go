package dataplane

import (
	"net/http"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// 本文件守护「协议转换损失」这条审计的**产出**与**零噪声**。
//
// 为什么值得单独一组用例：`convert.LossReport` 此前是只写不读的死数据（`forward.BuildPlan`
// 把它挂在 `plan.ConversionLoss` 上，全仓无消费者），因此没有任何对拍基准能发现它回归到原状——
// 只有本文件的断言。两个方向都会让用户失去手段：
//   - 不产出：使用记录与库里看不到「这次跨线转换丢了 cache_control / 工具声明」；
//   - 零损失也产出：每个转换请求白写一条空 groups，按 type 过滤的查询淹在噪声里。

// TestAppendEntriesRecordsConversionLossBesideConversion 钉住「转了」与「转丢了什么」两条并存。
//
// 这两条不是互斥关系（对比 `protocol_conversion_failed` 与 `protocol_conversion` 的互斥）：
// 转换成功且确有损失时，用户要同时知道「发生了转换」和「丢了哪些能力」。
func TestAppendEntriesRecordsConversionLossBesideConversion(t *testing.T) {
	plan := &forward.Plan{
		Protocol: convert.ProtocolOpenAIChat,
		Body:     []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
		Conversion: &convert.ConversionPlan{
			ClientProtocol: convert.ProtocolAnthropicMessages,
			TargetProtocol: convert.ProtocolOpenAIChat,
		},
		ConversionLoss: &convert.LossReport{Entries: []convert.LossEntry{
			{Capability: convert.LossThinkingSignature, Direction: "request", Action: convert.LossDropped, Detail: "responses_has_no_signature"},
			{Capability: convert.LossMCPTool, Direction: "request", Action: convert.LossDropped, Detail: "mcp_call"},
			{Capability: convert.LossMCPTool, Direction: "request", Action: convert.LossDropped, Detail: "mcp_list_tools"},
		}},
	}
	entries := decodeEntries(t, specialSettingsAppendEntries(nil, plan))

	if findEntryOrNil(entries, "protocol_conversion") == nil {
		t.Fatalf("转换成功必须仍产出 protocol_conversion，实际条目：%v", entries)
	}
	lossEntry := findEntryOrNil(entries, "protocol_conversion_loss")
	if lossEntry == nil {
		t.Fatalf("有损失时必须产出 protocol_conversion_loss，实际条目：%v", entries)
	}
	// 两条相邻落库：同一件事的两半排在一起，「转换发生了但呈现为空」不该再有解释空间。
	if len(entries) != 2 || entries[0]["type"] != specialsettings.TypeProtocolConversion ||
		entries[1]["type"] != specialsettings.TypeProtocolConversionLoss {
		t.Fatalf("本条应恰好落「转换 + 损失」两条且损失紧随其后，实际顺序：%v", entryTypes(entries))
	}
	if lossEntry["clientProtocol"] != "anthropic-messages" || lossEntry["targetProtocol"] != "openai-chat" {
		t.Errorf("协议对应逐字落库且与转换条目同源，实际 %v / %v",
			lossEntry["clientProtocol"], lossEntry["targetProtocol"])
	}
	// 经 decodeEntries 走过一遍 JSON，数字读回来是 float64。
	if lossEntry["total"] != float64(3) {
		t.Errorf("total 应为未聚合的损失条数 3，实际 %v", lossEntry["total"])
	}
	groups, ok := lossEntry["groups"].([]any)
	if !ok {
		t.Fatalf("groups 应为聚合数组，实际 %T（%v）", lossEntry["groups"], lossEntry["groups"])
	}
	want := []struct {
		capability string
		action     string
		count      float64
	}{
		{capability: convert.LossMCPTool, action: "dropped", count: 2},
		{capability: convert.LossThinkingSignature, action: "dropped", count: 1},
	}
	if len(groups) != len(want) {
		t.Fatalf("应聚合成 %d 组（同类多条折成一条），实际 %d：%v", len(want), len(groups), groups)
	}
	for index, expected := range want {
		group, ok := groups[index].(map[string]any)
		if !ok {
			t.Fatalf("第 %d 组不是对象：%v", index, groups[index])
		}
		if group["capability"] != expected.capability || group["action"] != expected.action || group["count"] != expected.count {
			t.Errorf("第 %d 组应为 %s/%s×%v，实际 %v", index, expected.capability, expected.action, expected.count, group)
		}
	}
	// 转换失败条目不得因为「有损失」而出现：本次转换是成功的。
	if got := findEntryOrNil(entries, "protocol_conversion_failed"); got != nil {
		t.Errorf("转换成功时不得产出失败条目：%v", got)
	}
}

// TestAppendEntriesOmitsConversionLossWhenConversionClean 钉住「零损失不写条目」。
//
// `convertBody` 对每次成功的转换都会返回一个**非 nil** 的 LossReport（可能零条目），
// 所以「有损失集」不等于「有损失」——判据必须是条目数，否则每个干净转换都会白写一条空 groups。
func TestAppendEntriesOmitsConversionLossWhenConversionClean(t *testing.T) {
	plan := &forward.Plan{
		Protocol: convert.ProtocolOpenAIChat,
		Body:     []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
		Conversion: &convert.ConversionPlan{
			ClientProtocol: convert.ProtocolAnthropicMessages,
			TargetProtocol: convert.ProtocolOpenAIChat,
		},
		ConversionLoss: &convert.LossReport{},
	}
	entries := decodeEntries(t, specialSettingsAppendEntries(nil, plan))
	if findEntryOrNil(entries, "protocol_conversion") == nil {
		t.Fatalf("转换成功必须产出 protocol_conversion，实际条目：%v", entries)
	}
	if got := findEntryOrNil(entries, specialsettings.TypeProtocolConversionLoss); got != nil {
		t.Errorf("零损失不得产出损失条目，实际 %v", got)
	}
	if len(entries) != 1 {
		t.Errorf("本条应恰好落一条（只有转换），实际 %v", entryTypes(entries))
	}
}

// TestAppendEntriesRecordsNoLossForNativeRequest 钉住原生同协议请求不因损失集而留下痕迹。
//
// 原生直通没有转换、也就没有「转换损失」这一事实（`plan.ConversionLoss` 恒为 nil）；
// 反向断言防止未来把损失集从别的来源（如供应商覆写）挂上来时，静默给全部原生请求白写一条。
func TestAppendEntriesRecordsNoLossForNativeRequest(t *testing.T) {
	plan := &forward.Plan{
		Protocol: convert.ProtocolAnthropicMessages,
		Body:     []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`),
	}
	if entries := decodeEntries(t, specialSettingsAppendEntries(nil, plan)); len(entries) != 0 {
		t.Fatalf("原生同协议请求不得产出任何转换审计，实际 %v", entries)
	}
}

// TestAuditCarriesLossFromRealConversion 钉住「真实跨线转换 → 计划损失集 → 审计条目」这条链。
//
// 为何要端到端钉这一环：本文件其余用例都是**手工构造** forward.Plan（把一份损失集喂给审计），
// 而损失集的上一环——`forward.BuildPlan` 真的把 `convertBody` 的损失挂到 `plan.ConversionLoss` 上——
// 此前没有任何用例断言过（全仓除本文件外无提及）。两环各自绿而中间断掉，审计会静默退回为空。
func TestAuditCarriesLossFromRealConversion(t *testing.T) {
	plan, err := forward.BuildPlan(forward.PlanInput{
		Client: forward.ClientRequest{
			Method: http.MethodPost,
			Path:   "/v1/messages",
			Format: convert.FormatClaude,
			Model:  "claude-sonnet-4-5",
			// top_k 在 chat 线没有承载位，是 anthropic → chat 丢字段的一个稳定样本。
			Body: []byte(`{"model":"claude-sonnet-4-5","max_tokens":64,"top_k":40,` +
				`"messages":[{"role":"user","content":"hi"}]}`),
			HasBody: true,
		},
		Target: forward.Target{Provider: forward.Provider{
			ID: 1, Type: convert.ProviderOpenAICompatible, Key: "sk-test-placeholder", URL: "https://example.com",
		}},
		ConversionEnabled: true,
	})
	if err != nil {
		t.Fatalf("计划编译失败：%v", err)
	}
	if plan.Conversion == nil || plan.ConversionFallback {
		t.Fatalf("前置不成立：本用例需要 anthropic → openai-chat 的真实转换，实际 %+v", plan.Conversion)
	}
	// 第一环：损失集真的从转换器挂到了计划上。
	if plan.ConversionLoss == nil || len(plan.ConversionLoss.Entries) == 0 {
		t.Fatalf("真实转换必须产出非空损失集，实际 %+v", plan.ConversionLoss)
	}
	// 第二环：同一份计划进审计，损失按能力/动作聚合落库。
	entries := decodeEntries(t, specialSettingsAppendEntries(nil, plan))
	lossEntry := findEntryOrNil(entries, specialsettings.TypeProtocolConversionLoss)
	if lossEntry == nil {
		t.Fatalf("真实转换的损失必须落进审计，实际条目：%v", entryTypes(entries))
	}
	if lossEntry["clientProtocol"] != "anthropic-messages" || lossEntry["targetProtocol"] != "openai-chat" {
		t.Errorf("协议对应逐字等于本计划的协议对，实际 %v / %v",
			lossEntry["clientProtocol"], lossEntry["targetProtocol"])
	}
	groups, ok := lossEntry["groups"].([]any)
	if !ok || len(groups) == 0 {
		t.Fatalf("有损失却无分组会让条目失去信息量，实际 %v", lossEntry["groups"])
	}
	found := false
	for _, raw := range groups {
		group, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if group["capability"] == convert.LossTopK && group["action"] == string(convert.LossDropped) {
			found = true
		}
	}
	if !found {
		t.Errorf("损失分组里应有 %s/%s（转换器确实丢了 top_k），实际 %v",
			convert.LossTopK, convert.LossDropped, groups)
	}
}

// entryTypes 取条目类型序列，仅在断言失败时用于把「实际落了哪几条」打进日志。
func entryTypes(entries []map[string]any) []any {
	types := make([]any, 0, len(entries))
	for _, entry := range entries {
		types = append(types, entry["type"])
	}
	return types
}
