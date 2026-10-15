package convert

import "testing"

// TestUsageFromAnthropicParsesCacheCreationSplit 钉住 Anthropic 的 usage.cache_creation
// 明细进枢纽用量：5m/1h 拆分是缓存写计费分档的唯一来源（见 terminal 的
// deriveCacheCreationTokens），只读扁平 cache_creation_input_tokens 时两档无法区分。
func TestUsageFromAnthropicParsesCacheCreationSplit(t *testing.T) {
	value, err := ParseJSON([]byte(`{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":3,` +
		`"cache_creation_input_tokens":5,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":2}}`))
	if err != nil {
		t.Fatalf("解析正文失败: %v", err)
	}

	usage := usageFromAnthropic(value)
	assertToken(t, "扁平总量", numberPtr(5), usage.CacheWriteTokens)
	assertToken(t, "5m 分列", numberPtr(3), usage.CacheCreation5mTokens)
	assertToken(t, "1h 分列", numberPtr(2), usage.CacheCreation1hTokens)
}

// TestUsageFromAnthropicKeepsMissingSplitNil 上游未分列（或只分列了一档）时缺的那档保持 nil：
// nil 与 0 是两回事——0 会被当成「上游明说这一档是 0」。
func TestUsageFromAnthropicKeepsMissingSplitNil(t *testing.T) {
	flat, err := ParseJSON([]byte(`{"cache_creation_input_tokens":5}`))
	if err != nil {
		t.Fatalf("解析正文失败: %v", err)
	}
	usage := usageFromAnthropic(flat)
	assertToken(t, "未分列的 5m", nil, usage.CacheCreation5mTokens)
	assertToken(t, "未分列的 1h", nil, usage.CacheCreation1hTokens)

	partial, err := ParseJSON([]byte(`{"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":5}}`))
	if err != nil {
		t.Fatalf("解析正文失败: %v", err)
	}
	usage = usageFromAnthropic(partial)
	assertToken(t, "只分列 1h 时的 5m", nil, usage.CacheCreation5mTokens)
	assertToken(t, "只分列 1h 时的 1h", numberPtr(5), usage.CacheCreation1hTokens)
}
