package dataplane

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件把 Node 的亲和写回时机判定逐条搬过来（成功写回 / 供应商侧失败写墓碑）。
// 判定依据全在 Node 源码里，注释给出行号；本包只做「时机判定」，键与 generation 由
// 选路包在提名时固化（见 route.AffinityWriteback）。

// affinityDirectiveForNonStream 给出非流式终态的亲和写回指令。
//
//   - 成功：forwarder.ts:2522 在成功分支立即写回，写回对象是产出该响应的供应商；
//     Go 侧的成功等价于 failure == nil（4xx/5xx 会被分类成失败，不走这里），
//     但仍要求 2xx——上游给了非 2xx 却被当成成功的路径不该把粘性写过去。
//   - 失败：forwarder.ts:2543-2552 只在 provider_error / resource_not_found 两类下写墓碑，
//     且排除 request-scoped 的闸门空完成（isRequestScopedGateFailure）：那类失败在任何
//     供应商上都会复现，写墓碑只会让后续请求绕开健康供应商。
//     客户端中断（CategoryClientAbort）不写墓碑——Node 同一处按类别排除了它。
func affinityDirectiveForNonStream(result *forward.Result, failure *forward.Failure) terminal.AffinityDirective {
	if failure == nil {
		if result == nil || result.StatusCode < 200 || result.StatusCode >= 300 {
			return terminal.AffinityDirective{}
		}
		return terminal.AffinityDirective{WinnerProviderID: result.Provider.ID}
	}
	return tombstoneDirective(failure)
}

// affinityDirectiveForStream 给出流式终态的亲和写回指令。
//
// Node 的判定在 response-handler.ts:2304（isIncompleteCompletion）与 2359-2363
// （isSuccessfulCompletion）：
//
//   - 成功写回（5414-5420）：协议终态正常抵达、未被判为错误、内部状态码 2xx；
//     客户端在协议终态之后断开也算成功（Node 的 clientAbortCompleteSuccess），
//     Go 的 TerminalCompleted 已涵盖这一情形（泵只在终态未定时才标 clientAborted）。
//   - 墓碑（5420-5426）：**非** incomplete 且存在错误文案的一切终态——上游错误帧、
//     非 2xx、未正常结束（502）、静默超时、以及客户端中断（Node 把中断归为
//     499/CLIENT_ABORTED，errorMessage 非空，故同样写墓碑）。incomplete
//     （response.incomplete：语义未完成但 2xx 且无错误）两边都不写。
func affinityDirectiveForStream(outcome forward.StreamOutcome) terminal.AffinityDirective {
	success := outcome.StatusCode >= 200 && outcome.StatusCode < 300
	incomplete := outcome.Observation.SawIncomplete && outcome.Observation.ErrorText == "" && success
	if incomplete {
		return terminal.AffinityDirective{}
	}
	if success && outcome.Kind == forward.TerminalCompleted {
		return terminal.AffinityDirective{WinnerProviderID: outcome.Provider.ID}
	}
	return terminal.AffinityDirective{TombstoneProviderID: outcome.Provider.ID}
}

// tombstoneDirective 按 Node 的类别判据给出墓碑指令。
//
// 两类都写墓碑（Node forwarder.ts:2543-2552 的判据），但**墓碑种类不同**：
//   - provider_error（真实 4xx/5xx、空响应、以 400 回传的容量故障）⇒ 会话绑定写冷却；
//   - resource_not_found（上游 404、本地模型缺口）⇒ 会话绑定只清绑定、不写冷却。
//
// 前缀墓碑（亲和侧）对两者一视同仁：那家的模型确实不可用，后续同前缀请求也该绕开它。
// 「该不该冷却这家」是会话绑定的语义，两者作用在不同的键上（见 terminal/settle.go）。
func tombstoneDirective(failure *forward.Failure) terminal.AffinityDirective {
	if failure.RequestScoped {
		return terminal.AffinityDirective{}
	}
	switch failure.Category {
	case forward.CategoryProviderError:
		return terminal.AffinityDirective{TombstoneProviderID: failure.ProviderID}
	case forward.CategoryResourceNotFound:
		return terminal.AffinityDirective{
			TombstoneProviderID: failure.ProviderID,
			TombstoneKind:       terminal.AffinityTombstoneResourceNotFound,
		}
	default:
		return terminal.AffinityDirective{}
	}
}
