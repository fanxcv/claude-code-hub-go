package dataplane

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件钉住 `provider_chain` 链项上的**尝试结局细节**（Node 的 addProviderToChain 写的
// `statusCode` / `errorMessage` / `modelRedirect`）。
//
// 为什么单独立文件：这三个键此前**从不落库**，于是前端 `LogicTraceTab` 与
// `provider-chain-popover` 上的「HTTP 状态码」「错误」「模型重定向 原→新」三处恒空白
// （界面照常渲染、不报错，只是没内容）——形状对拍看不见这一类缺失。
//
// 断言打在**原始 JSON 键**上而不是结构体字段上：契约是「Node 有这几个键、没有时必须缺键」，
// 结构体字段为零值无法区分「缺键」与「键存在但值为空」。

// chainDetailsSettler 造一个只用于编码链的最小 settler。
func chainDetailsSettler() *storeSettler {
	return &storeSettler{state: &RequestState{StartedAt: time.Now()}, logger: logx.New(nil)}
}

// decodeChainItems 把链载荷解成原始 JSON 对象（保留键的缺失状态）。
func decodeChainItems(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal(payload, &items); err != nil {
		t.Fatalf("provider_chain 不是合法 JSON: %v（%s）", err, payload)
	}
	return items
}

func TestProviderChainAttemptDetailsEncoded(t *testing.T) {
	settler := chainDetailsSettler()
	attempts := []forward.AttemptOutcome{
		{
			// ① 成功且发生了模型重定向：三个键都应有。
			ProviderID: 101, ProviderName: "甲", Attempt: 1, Reason: "request_success",
			StatusCode: 200,
			ModelRedirect: &forward.AttemptModelRedirect{
				OriginalModel:   "claude-sonnet-4-5",
				RedirectedModel: "claude-sonnet-4-5-20250929",
				BillingModel:    "claude-sonnet-4-5",
				MatchType:       "exact",
				Source:          "claude-sonnet-4-5",
				Target:          "claude-sonnet-4-5-20250929",
			},
		},
		{
			// ② 上游报错：状态码与错误信息应有，重定向键应缺（本次无重定向）。
			ProviderID: 102, ProviderName: "乙", Attempt: 2, Reason: "retry_failed",
			StatusCode: 500,
			Message:    "上游返回 500",
		},
		{
			// ③ 选路类条目：本地拒绝，从未发生 HTTP 交换 —— 三个键**必须一个都不出现**。
			ProviderID: 103, ProviderName: "丙", Attempt: 3, Reason: "local_overload",
		},
		{
			// ④ 竞速输家：裁决时无状态码（Node 的 attempt.response?.status 同样多为 undefined），
			//    但该 attempt 自己的重定向快照应有。
			ProviderID: 104, ProviderName: "丁", Attempt: 4, Reason: "hedge_loser_billed",
			ModelRedirect: &forward.AttemptModelRedirect{
				OriginalModel:   "claude-sonnet-4-5",
				RedirectedModel: "claude-3-7-sonnet",
				BillingModel:    "claude-sonnet-4-5",
				MatchType:       "prefix",
				Source:          "claude-sonnet-*",
				Target:          "claude-3-7-sonnet",
			},
		},
	}

	items := decodeChainItems(t, settler.providerChain(attempts))
	if len(items) != len(attempts) {
		t.Fatalf("链项数 = %d，期望 %d", len(items), len(attempts))
	}

	// ① 成功条目的三个键。
	success := items[0]
	if got, ok := success["statusCode"].(float64); !ok || got != 200 {
		t.Fatalf("成功条目 statusCode = %v，期望 200", success["statusCode"])
	}
	redirect, ok := success["modelRedirect"].(map[string]any)
	if !ok {
		t.Fatalf("成功条目缺少 modelRedirect：%v", success)
	}
	// 键名必须与 Node 逐字一致——前端直接读 `item.modelRedirect.originalModel`。
	for key, want := range map[string]string{
		"originalModel":   "claude-sonnet-4-5",
		"redirectedModel": "claude-sonnet-4-5-20250929",
		"billingModel":    "claude-sonnet-4-5",
	} {
		if redirect[key] != want {
			t.Fatalf("modelRedirect.%s = %v，期望 %q", key, redirect[key], want)
		}
	}
	rule, ok := redirect["matchedRule"].(map[string]any)
	if !ok {
		t.Fatalf("modelRedirect 缺少 matchedRule：%v", redirect)
	}
	for key, want := range map[string]string{
		"matchType": "exact",
		"source":    "claude-sonnet-4-5",
		"target":    "claude-sonnet-4-5-20250929",
	} {
		if rule[key] != want {
			t.Fatalf("matchedRule.%s = %v，期望 %q", key, rule[key], want)
		}
	}
	if _, hasError := success["errorMessage"]; hasError {
		t.Fatalf("成功条目不应有 errorMessage：%v", success)
	}

	// ② 失败条目的状态码与错误信息；无重定向时该键必须缺失（而不是空对象）。
	failure := items[1]
	if got, ok := failure["statusCode"].(float64); !ok || got != 500 {
		t.Fatalf("失败条目 statusCode = %v，期望 500", failure["statusCode"])
	}
	if msg, _ := failure["errorMessage"].(string); msg == "" {
		t.Fatalf("失败条目应有 errorMessage：%v", failure)
	}
	if _, hasRedirect := failure["modelRedirect"]; hasRedirect {
		t.Fatalf("未发生重定向的条目不应有 modelRedirect：%v", failure)
	}

	// ③ 选路类条目：三个键都不得出现（写 0/空串会让界面渲染出「HTTP 0」）。
	selection := items[2]
	for _, key := range []string{"statusCode", "errorMessage", "modelRedirect"} {
		if _, present := selection[key]; present {
			t.Fatalf("选路类条目不应有 %s：%v", key, selection)
		}
	}

	// ④ 输家：无状态码但有重定向。
	loser := items[3]
	if _, present := loser["statusCode"]; present {
		t.Fatalf("无状态码的输家条目不应写 statusCode：%v", loser)
	}
	if _, present := loser["modelRedirect"]; !present {
		t.Fatalf("输家条目应带自己的重定向快照：%v", loser)
	}
}

// TestProviderChainDetailsDoNotDisturbSelectionFields 钉住「只增不改」：
// 选路留痕里的字段（供应商类型、决策上下文等）必须原样保留。
func TestProviderChainDetailsDoNotDisturbSelectionFields(t *testing.T) {
	settler := chainDetailsSettler()
	items := decodeChainItems(t, settler.providerChain([]forward.AttemptOutcome{{
		ProviderID: 201, ProviderName: "戊", Attempt: 1, Reason: "request_success", StatusCode: 200,
	}}))
	item := items[0]
	// 这些键来自 ChainItem 的非 omitempty 列，此前就恒在（黄金样本依赖），不能被新字段挤掉。
	for _, key := range []string{"id", "name", "attemptNumber", "reason", "statusCode"} {
		if _, present := item[key]; !present {
			t.Fatalf("链项缺少既有键 %s：%v", key, item)
		}
	}
	if item["reason"] != "request_success" {
		t.Fatalf("reason 被改动：%v", item["reason"])
	}
}
