package dataplane

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// codexSessionEntry 取本请求的 Codex 会话标识补全审计条目。
//
// 为什么读上下文而不是读计划：补全发生在守卫链的 session 步骤（认证之后、选路之前），
// 它的产物是**请求侧事实**（补了哪一侧、用哪个 id），与本次尝试的供应商无关——同一请求
// 可能经历多次尝试，把它挂在计划上会产生多条重复条目。Node 同样是「一次补全一条审计」。
func codexSessionEntry(state *RequestState) map[string]any {
	if state == nil || state.PC == nil {
		return nil
	}
	completion, ok := state.PC.CodexSessionCompletion()
	if !ok {
		return nil
	}
	return specialsettings.CodexSessionCompletionEntry(completion.Action, completion.Source, completion.SessionID)
}
