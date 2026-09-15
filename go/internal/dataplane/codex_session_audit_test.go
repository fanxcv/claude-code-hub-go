package dataplane

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// 本文件守护 Codex 会话标识补全的审计**产出**：守卫链把事实写进上下文（认证后、选路前），
// 终态结算时随其它探针条目一次追加（零额外每请求写入）。
//
// 为什么值得单独一组用例：这条审计是「补全是否发生、用的是哪个 id」的唯一用户可见证据；
// 不产出时，同一个请求在链上看不出补过——排障只能靠猜（而补全正是缓存键的来源）。

// TestAppendEntriesRecordsCodexSessionCompletion 钉住补全条目与其它条目同批产出。
func TestAppendEntriesRecordsCodexSessionCompletion(t *testing.T) {
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/responses"})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	pc.SetCodexSessionCompletion(pctx.CodexSessionCompletion{
		Action:    "completed_missing_fields",
		Source:    "header_session_id",
		SessionID: "01a0a2a1-c7ff-7747-81cc-4e27411e8938",
	})
	state := &RequestState{PC: pc}

	entries := decodeEntries(t, specialSettingsAppendEntries(state, &forward.Plan{Protocol: convert.ProtocolOpenAIResponses}))
	entry := findEntryOrNil(entries, specialsettings.TypeCodexSessionIDCompletion)
	if entry == nil {
		t.Fatalf("补全事实存在时必须产出条目，实际条目：%v", entries)
	}
	if entry["action"] != "completed_missing_fields" || entry["source"] != "header_session_id" {
		t.Errorf("动作/来源应逐字落库，实际 %v / %v", entry["action"], entry["source"])
	}
	if entry["sessionId"] != "01a0a2a1-c7ff-7747-81cc-4e27411e8938" {
		t.Errorf("会话标识应落库，实际 %v", entry["sessionId"])
	}
	if entry["scope"] != "request" || entry["hit"] != true {
		t.Errorf("scope/hit 不符：%v", entry)
	}
}

// TestCodexSessionEntryIsNilWithoutCompletion 钉住「没补就不记」。
func TestCodexSessionEntryIsNilWithoutCompletion(t *testing.T) {
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/responses"})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}

	if got := codexSessionEntry(&RequestState{PC: pc}); got != nil {
		t.Errorf("未补全时应返回 nil，实际 %v", got)
	}
	if got := codexSessionEntry(&RequestState{}); got != nil {
		t.Errorf("无上下文时应返回 nil，实际 %v", got)
	}
	if got := codexSessionEntry(nil); got != nil {
		t.Errorf("无状态时应返回 nil，实际 %v", got)
	}
}

// TestCodexSessionEntryIsPerRequestNotPerAttempt 钉住「一次补全一条审计」。
//
// 补全发生在守卫链（与供应商无关），故同一个请求无论经历几次尝试都只应有一条条目；
// 挂在计划上按 attempt 记会给出重复行。
func TestCodexSessionEntryIsPerRequestNotPerAttempt(t *testing.T) {
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/responses"})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	pc.SetCodexSessionCompletion(pctx.CodexSessionCompletion{
		Action: "reused_fingerprint_cache", Source: "fingerprint_cache", SessionID: "01a0a2a1-c7ff-7747-81cc-4e27411e8938",
	})
	state := &RequestState{PC: pc}

	first := codexSessionEntry(state)
	second := codexSessionEntry(state)
	if first == nil || second == nil {
		t.Fatalf("两次取值都应产出条目")
	}
	if first["sessionId"] != second["sessionId"] || first["action"] != second["action"] {
		t.Errorf("同请求的两次取值应一致：%v vs %v", first, second)
	}
}
