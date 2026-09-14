package forward

import "testing"

// 本文件钉住「尝试级模型重定向快照」的构造口径——它最终会变成链项上的
// `modelRedirect`，前端据此渲染「原模型 → 转发模型」。
//
// 最容易写错的一处是 `billingModel`：Node 的语义是**计费依据 = 用户请求的模型**
// （`model-redirector.ts` 里字面写 `billingModel: originalModel`），不是转发目标。
// 若照字面直觉写成 target，账务侧读到的计费模型就会与 Node 分叉。

func TestAttemptModelRedirectCarriesRuleSource(t *testing.T) {
	plan := &Plan{Redirect: &ModelRedirect{
		Original: "claude-sonnet-4-5",
		Target:   "claude-sonnet-4-5-20250929",
		Rule:     "exact",
		Source:   "claude-sonnet-4-5",
	}}
	got := attemptModelRedirect(plan)
	if got == nil {
		t.Fatal("有重定向的 plan 应产出快照")
	}
	if got.OriginalModel != "claude-sonnet-4-5" || got.RedirectedModel != "claude-sonnet-4-5-20250929" {
		t.Fatalf("原/转发模型搬运错误：%+v", got)
	}
	// 计费依据是用户请求的模型。
	if got.BillingModel != got.OriginalModel {
		t.Fatalf("billingModel 应等于 originalModel（Node 口径），实际 %q", got.BillingModel)
	}
	// 规则三要素都要留到落链时刻：只有 target 无法回答「哪条规则命中的」。
	if got.MatchType != "exact" || got.Source != "claude-sonnet-4-5" || got.Target != "claude-sonnet-4-5-20250929" {
		t.Fatalf("matchedRule 三要素不全：%+v", got)
	}
}

func TestAttemptModelRedirectNilWhenNoRedirect(t *testing.T) {
	if got := attemptModelRedirect(nil); got != nil {
		t.Fatalf("nil plan 不应产出快照，实际 %+v", got)
	}
	if got := attemptModelRedirect(&Plan{}); got != nil {
		t.Fatalf("未施加重定向的 plan 不应产出快照，实际 %+v", got)
	}
}

// TestResolveModelRedirectFillsSource 钉住解析层：命中的规则必须把 source 一起带出来。
// 上层快照只是搬运，若解析时不填，链上的 matchedRule.source 恒空。
//
// 模式写法与 Node 一致：`prefix` 是纯 startsWith（`model-pattern-matcher.ts:12-13`），
// **不带通配符**，故模式是 `claude-sonnet-` 而不是 `claude-sonnet-*`。
func TestResolveModelRedirectFillsSource(t *testing.T) {
	raw := []byte(`[{"matchType":"prefix","source":"claude-sonnet-","target":"claude-3-7-sonnet"}]`)
	got, err := resolveModelRedirect("claude-sonnet-4-5", raw)
	if err != nil {
		t.Fatalf("解析规则失败: %v", err)
	}
	if got == nil {
		t.Fatal("应命中 prefix 规则")
	}
	if got.Source != "claude-sonnet-" || got.Rule != "prefix" || got.Target != "claude-3-7-sonnet" {
		t.Fatalf("规则三要素搬运错误：%+v", got)
	}
}
