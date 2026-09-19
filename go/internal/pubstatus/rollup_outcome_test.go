package pubstatus

import "testing"

// TestClassifyRequestOutcomeSignalResponsesWSReasonsAreNeutral 钉住上游 WS 两个词的信息性归属。
//
// 为什么必须钉：`ClassifyRequestOutcomeSignal` 的兜底是「带 reason 而无状态码 ⇒ failure」。
// 上游 WS 的降级/尝试条目正是这个形状（它没发生过 HTTP 交换，**没有**状态码），若不收进
// neutralReasons，则「WS 没走成、回落 HTTP 后成功」的请求会被计成失败——可用率被自己的
// 降级痕迹拖下水，与 http2_fallback 当年的地位完全一致。
func TestClassifyRequestOutcomeSignalResponsesWSReasonsAreNeutral(t *testing.T) {
	for _, word := range []string{"responses_ws_attempted", "responses_ws_fallback"} {
		reason := word
		if _, ok := ClassifyRequestOutcomeSignal(RequestOutcomeSignal{Reason: &reason}); ok {
			t.Fatalf("%q 是传输层信息性原因，不该被分类成结局", word)
		}
		item := ProviderChainItem{Reason: &reason}
		if taxonomy, ok := ClassifyProviderChainItemOutcome(item); ok {
			t.Fatalf("%q 的链项不该产生结局分类，得到 %q", word, taxonomy.Outcome)
		}
		if IsExcludedFromPublicStatusFailure(item) {
			t.Fatalf("%q 不该走「排除」分支（它本来就是中性，不是被排除的失败）", word)
		}
	}

	// 邻居对照：同样形状的未知词仍必须落进 failure，不能因为上面两条把中性面放大。
	unknown := "some_unknown_reason"
	taxonomy, ok := ClassifyRequestOutcomeSignal(RequestOutcomeSignal{Reason: &unknown})
	if !ok || taxonomy.Outcome != OutcomeFailure {
		t.Fatalf("未知 reason 仍应计失败，得到 ok=%v outcome=%q", ok, taxonomy.Outcome)
	}
}

// TestClassifyRequestOutcomeSignalUnsupportedIsExcluded 钉住新增档 `unsupported` 的公开状态归属。
//
// 这一档是「当前供应商明确声明该输入形态不受支持」（forward.CategoryProviderUnsupportedInput），
// 不计供应商健康度：若落进 failure 兜底，公开页的可用率会被「同一份输入换一家可能就成」的请求污染。
// 同时钉住它不误伤邻居——普通的 400 仍必须是 countable failure。
func TestClassifyRequestOutcomeSignalUnsupportedIsExcluded(t *testing.T) {
	unsupported := "unsupported"
	badRequest := 400

	taxonomy, ok := ClassifyRequestOutcomeSignal(RequestOutcomeSignal{
		Reason: &unsupported, StatusCode: &badRequest,
	})
	if !ok {
		t.Fatal("带 reason 与状态码的链项应可分类")
	}
	if taxonomy.Outcome != OutcomeExcluded {
		t.Fatalf("Outcome = %q，期望 excluded", taxonomy.Outcome)
	}
	if taxonomy.ExclusionFamily != ExclusionProviderUnsupportedInput {
		t.Fatalf("ExclusionFamily = %q，期望 %q", taxonomy.ExclusionFamily, ExclusionProviderUnsupportedInput)
	}
	if taxonomy.Countability != "excluded" || taxonomy.Result != "n/a" {
		t.Fatalf("Countability/Result = %q/%q，期望 excluded/n-a", taxonomy.Countability, taxonomy.Result)
	}

	item := ProviderChainItem{Reason: &unsupported, StatusCode: &badRequest}
	if !IsExcludedFromPublicStatusFailure(item) {
		t.Fatal("该档必须从公开状态的失败统计里排除")
	}

	// 邻居对照：同形状的普通 400 不得被顺带排除。
	other := "retry_failed"
	failure, ok := ClassifyRequestOutcomeSignal(RequestOutcomeSignal{Reason: &other, StatusCode: &badRequest})
	if !ok || failure.Outcome != OutcomeFailure {
		t.Fatalf("普通失败应计为 failure，得到 ok=%v outcome=%q", ok, failure.Outcome)
	}
	if IsExcludedFromPublicStatusFailure(ProviderChainItem{Reason: &other, StatusCode: &badRequest}) {
		t.Fatal("普通失败不得被排除")
	}
}
