package specialsettings

// TypeCodexSessionIDCompletion 是 Codex 会话标识补全的审计类型。
//
// 与 Node（`src/app/v1/_lib/proxy/session-guard.ts:128`）及前端
// `src/types/special-settings.ts` 的 `CodexSessionIdCompletionSpecialSetting` 逐字同形，
// 前端据此渲染「会话标识补全」一行，故字段名不得改写。
const TypeCodexSessionIDCompletion = "codex_session_id_completion"

// CodexSessionCompletionEntry 产出补全审计条目。
//
// action 为 none（两侧齐全、无需补）或 sessionId 为空时返回 nil：Node 只在
// `completion.applied && completion.action !== "none"` 时记录。
func CodexSessionCompletionEntry(action, source, sessionID string) map[string]any {
	if action == "" || action == "none" || sessionID == "" {
		return nil
	}
	return map[string]any{
		"type":      TypeCodexSessionIDCompletion,
		"scope":     "request",
		"hit":       true,
		"action":    action,
		"source":    source,
		"sessionId": sessionID,
	}
}
