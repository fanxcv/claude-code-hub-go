package dataplane

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// 本文件钉住上游 WS 事实在 `provider_chain` 上的形态（Node 的 ProviderChainItem 同名字段）。
//
// 断言打在**原始 JSON 键**上：契约是「有事实才有键」，结构体零值区分不出「缺键」与
// 「键在但为空」——而界面正是按键存在与否渲染「这次走没走上游 WS」。

func TestProviderChainEmitsWSFallbackEntryWithoutOverwritingOutcome(t *testing.T) {
	settler := chainDetailsSettler()
	attempts := []forward.AttemptOutcome{
		{
			ProviderID: 55, ProviderName: "codex-甲", Attempt: 1,
			Reason: "request_success", StatusCode: 200,
			WS: &forward.AttemptWSFacts{
				ClientTransport:  "websocket",
				Attempted:        true,
				DowngradedToHTTP: true,
				DowngradeReason:  "ws_upgrade_rejected",
			},
		},
	}
	items := decodeChainItems(t, settler.providerChain(attempts))
	if len(items) != 2 {
		t.Fatalf("WS 事实应额外产生一条信息性条目，期望 2 条，得到 %d 条: %v", len(items), items)
	}

	info := items[0]
	if got := info["reason"]; got != forward.ReasonResponsesWSFallback {
		t.Fatalf("信息性条目的 reason 应为 %q，得到 %v", forward.ReasonResponsesWSFallback, got)
	}
	if got := info["clientTransport"]; got != "websocket" {
		t.Fatalf("clientTransport 缺失或不对: %v", got)
	}
	if got := info["upstreamWsAttempted"]; got != true {
		t.Fatalf("upstreamWsAttempted 应为 true，得到 %v", got)
	}
	if got := info["downgradedToHttp"]; got != true {
		t.Fatalf("downgradedToHttp 应为 true，得到 %v", got)
	}
	if got := info["downgradeReason"]; got != "ws_upgrade_rejected" {
		t.Fatalf("downgradeReason 不对: %v", got)
	}
	if _, exists := info["upstreamWsConnected"]; exists {
		t.Fatal("没连上时不得写 upstreamWsConnected（写 false 会被渲染成「连了但失败」）")
	}
	if _, exists := info["statusCode"]; exists {
		t.Fatal("信息性条目没有发生过 HTTP 交换，不得写 statusCode")
	}

	attempt := items[1]
	if got := attempt["reason"]; got != "request_success" {
		t.Fatalf("尝试条目的真实结局被覆盖了：%v", got)
	}
	if _, exists := attempt["clientTransport"]; exists {
		t.Fatal("WS 事实只该写在那条信息性条目上，不该重复写进尝试条目")
	}
	if _, exists := attempt["statusCode"]; !exists {
		t.Fatal("尝试条目仍应带 statusCode（200）")
	}
}

func TestProviderChainEmitsWSAttemptedEntryWhenConnected(t *testing.T) {
	settler := chainDetailsSettler()
	attempts := []forward.AttemptOutcome{
		{
			ProviderID: 56, ProviderName: "codex-乙", Attempt: 1,
			Reason: "request_success", StatusCode: 200,
			WS: &forward.AttemptWSFacts{
				ClientTransport: "websocket",
				Attempted:       true,
				Connected:       true,
			},
		},
	}
	items := decodeChainItems(t, settler.providerChain(attempts))
	if len(items) != 2 {
		t.Fatalf("期望 2 条条目，得到 %d", len(items))
	}
	if got := items[0]["reason"]; got != forward.ReasonResponsesWSAttempted {
		t.Fatalf("走成时信息性条目的 reason 应为 %q，得到 %v", forward.ReasonResponsesWSAttempted, got)
	}
	if got := items[0]["upstreamWsConnected"]; got != true {
		t.Fatalf("upstreamWsConnected 应为 true，得到 %v", got)
	}
	if _, exists := items[0]["downgradedToHttp"]; exists {
		t.Fatal("没降级时不得写 downgradedToHttp")
	}
}

func TestProviderChainOmitsWSKeysWithoutFacts(t *testing.T) {
	settler := chainDetailsSettler()
	attempts := []forward.AttemptOutcome{
		{ProviderID: 57, ProviderName: "普通上游", Attempt: 1, Reason: "request_success", StatusCode: 200},
	}
	items := decodeChainItems(t, settler.providerChain(attempts))
	if len(items) != 1 {
		t.Fatalf("没有 WS 事实时不该多出条目，得到 %d 条", len(items))
	}
	for _, key := range []string{"clientTransport", "upstreamWsAttempted", "upstreamWsConnected", "downgradedToHttp", "downgradeReason"} {
		if _, exists := items[0][key]; exists {
			t.Fatalf("没有 WS 事实时不得写键 %q（与接入前的链形态逐字一致）", key)
		}
	}
}
