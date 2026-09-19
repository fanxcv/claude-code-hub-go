package dataplane

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// TestNewWSNoticeStopsAtKeyLimit 钉住「跳过上游 WS」去重表的**上界语义**：表到顶即停止上报，
// 且实际容量与注释里的数字一致。
//
// 为什么按告警条数断言：去重状态在闭包里，唯一可观测的产物就是日志行——数行数即数键数。
// 修前判据在写入**之后**（`len(seen) > 512`），于是实际会放进 513 个键，与注释所称的 512 差一。
func TestNewWSNoticeStopsAtKeyLimit(t *testing.T) {
	var out bytes.Buffer
	notice := newWSNotice(logx.New(&out))

	// 键的维度是「原因|供应商类型|供应商 id」，故逐个不同的 id 造不同的键。
	for id := 0; id < wsNoticeKeyLimit+50; id++ {
		notice(forward.WSSkip{
			Cause:        forward.WSSkipCauseNotEligible,
			ProviderType: "codex",
			ProviderID:   int64(id),
		})
	}
	if got := strings.Count(out.String(), "forward.ws_skip"); got != wsNoticeKeyLimit {
		t.Fatalf("去重表上界应为 %d 条告警，实际 %d 条", wsNoticeKeyLimit, got)
	}

	// 到顶之后连**已有键**也不再重复上报（表已停用，只留已记的那批）。
	before := out.Len()
	notice(forward.WSSkip{Cause: forward.WSSkipCauseNotEligible, ProviderType: "codex", ProviderID: 0})
	if out.Len() != before {
		t.Fatal("到顶之后不得再产出告警")
	}
}
