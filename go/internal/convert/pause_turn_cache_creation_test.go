package convert

import "testing"

// 本文件钉住本轮对齐 relaykit 的两条语义修正：
//  1. `pause_turn` 是「本轮流被暂停、未走完」，不得当正常收尾——否则客户端把半截结果当完整答复；
//  2. `usageToAnthropic` 必须把 cache_creation 的 5m/1h 分档写回，未分列时留白而不是写 0。

func TestPauseTurnIsNotNormalCompletion(t *testing.T) {
	if got := StopReasonFromAnthropic("pause_turn", true); got != StopMaxTokens {
		t.Fatalf("pause_turn 应归截断族（max_tokens），实际 %q", got)
	}
	// 三线渲染：chat 出 length、responses 出 incomplete，都不是正常收尾。
	if got := stopReasonToOpenAIChat(StopMaxTokens, false); got != "length" {
		t.Fatalf("chat finish_reason 应为 length，实际 %q", got)
	}
	if status := stopReasonToResponses(StopMaxTokens, false); status.Status != "incomplete" {
		t.Fatalf("responses status 应为 incomplete，实际 %q", status.Status)
	}
}

func TestUsageToAnthropicWritesCacheCreationDetail(t *testing.T) {
	five := float64(40)
	oneHour := float64(60)
	usage := &Usage{InputTokens: float64Ptr(5), OutputTokens: float64Ptr(7), CacheCreation5mTokens: &five, CacheCreation1hTokens: &oneHour}
	rendered := usageToAnthropic(usage)
	detail := rendered.ObjectField("cache_creation")
	if detail == nil {
		t.Fatal("分列非 nil 时必须写 cache_creation")
	}
	if got, ok := numberField(detail, "ephemeral_5m_input_tokens"); !ok || *got != five {
		t.Fatalf("5m 分档应为 %v，实际 %v", five, got)
	}
	if got, ok := numberField(detail, "ephemeral_1h_input_tokens"); !ok || *got != oneHour {
		t.Fatalf("1h 分档应为 %v，实际 %v", oneHour, got)
	}

	// 未分列（两个指针都 nil）时不得写该对象：写 0 等于把「不知道」当成「零缓存写入」。
	bare := usageToAnthropic(&Usage{InputTokens: float64Ptr(5)})
	if bare.ObjectField("cache_creation") != nil {
		t.Fatal("未分列时不得写 cache_creation")
	}
}

func float64Ptr(value float64) *float64 { return &value }
