package pubstatus

import "testing"

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
