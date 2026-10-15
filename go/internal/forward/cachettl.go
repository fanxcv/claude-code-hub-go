package forward

import (
	"bytes"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// cacheTTLMixed 表示本次请求的缓存写入 5m/1h 两种归档都有。
const cacheTTLMixed = "mixed"

// AppliedCacheTTL 返回本计划出站正文里**实际应用**的缓存 TTL 归属：
// "5m" / "1h" / "mixed"；正文里没有任何 ephemeral 缓存块时为 ""。
//
// 为什么读正文而不是再解析一遍偏好与客户端标称：正文就是上游实际收到的那一份——供应商
// 覆写（见 applyCacheTTL）与客户端自带的 cache_control.ttl 谁生效，读它一次即知；另立一份
// 判定迟早与写入处分叉。取值口径与写入处同一条：只有恰好 "1h" 是 1h，其余（含 ttl 缺失
// 这个 Anthropic 默认 5m 的形态，以及脏值）一律 5m。
//
// 消费方是计费：上游未分列 5m/1h 明细时，cache_creation_input_tokens 的差额按本值归档
// （见 terminal 的 deriveCacheCreationTokens）。
func (p *Plan) AppliedCacheTTL() string {
	if p == nil {
		return ""
	}
	return appliedCacheTTL(p.Body)
}

// appliedCacheTTL 扫描的块面与写入处一致（system 数组与 messages[].content 数组）。
func appliedCacheTTL(body []byte) string {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"cache_control"`)) {
		return ""
	}
	tree, err := convert.ParseJSON(body)
	if err != nil || !tree.IsObject() {
		return ""
	}

	hasShort, hasLong := false, false
	collect := func(blocks *convert.Value) {
		for _, block := range blocks.Items() {
			if block == nil || !block.IsObject() {
				continue
			}
			control := memberValue(block, "cache_control")
			if control == nil || !control.IsObject() {
				continue
			}
			if kind, _ := control.StringField("type"); kind != "ephemeral" {
				continue
			}
			if ttl, ok := control.StringField("ttl"); ok && ttl == cacheTTLLong {
				hasLong = true
				continue
			}
			hasShort = true
		}
	}

	if system := memberValue(tree, "system"); system != nil && system.IsArray() {
		collect(system)
	}
	if messages := memberValue(tree, "messages"); messages != nil && messages.IsArray() {
		for _, message := range messages.Items() {
			if content := memberValue(message, "content"); content != nil && content.IsArray() {
				collect(content)
			}
		}
	}

	switch {
	case hasShort && hasLong:
		return cacheTTLMixed
	case hasLong:
		return cacheTTLLong
	case hasShort:
		return cacheTTLShort
	}
	return ""
}
