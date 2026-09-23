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
//
// 本仓另加两类本类型未覆盖的收尾：
//   - 客户端主动中断：前缀墓碑照写、会话绑定侧不动（Node 对齐，见下）；
//   - 流尾缺协议终止标记但正文已按分帧交付完毕：两边都不写
//     （见 streamBodyDeliveredWithoutMarker）。
func affinityDirectiveForStream(outcome forward.StreamOutcome) terminal.AffinityDirective {
	success := outcome.StatusCode >= 200 && outcome.StatusCode < 300
	incomplete := outcome.Observation.SawIncomplete && outcome.Observation.ErrorText == "" && success
	if incomplete {
		return terminal.AffinityDirective{}
	}
	if success && outcome.Kind == forward.TerminalCompleted {
		return terminal.AffinityDirective{WinnerProviderID: outcome.Provider.ID}
	}
	if streamBodyDeliveredWithoutMarker(outcome, success) {
		return terminal.AffinityDirective{}
	}

	// 客户端主动中断不是供应商故障：前缀墓碑照写（Node 对齐），但会话绑定侧不得动作——
	// 否则用户按停就会给一家健康渠道写 60 秒冷却，下一请求无故换家。
	if outcome.Kind == forward.TerminalClientAborted {
		return terminal.AffinityDirective{
			TombstoneProviderID: outcome.Provider.ID,
			TombstoneKind:       terminal.AffinityTombstonePrefixOnly,
		}
	}
	return terminal.AffinityDirective{TombstoneProviderID: outcome.Provider.ID}
}

// streamBodyDeliveredWithoutMarker 报告「上游把响应正文正常收尾、只漏了协议终止标记」。
//
// 判据的核心是 CompletionMarker：它记录观测器是否真见到了与协议族匹配的终止标记
// （[DONE] / message_stop / response.completed）。TerminalUpstreamTruncated 有**两条来路**
// （见 forward/terminalKindFor）：泵报 io.EOF 的提前返回（不看标记），以及「无错误、无错误帧、
// 无标记」的兜底。**两条来路都不保证正文已交付**——生产实证（2026-09-23，wb 池代理）里，
// 上游在 tool_calls 参数中间干净地 FIN，正文被切断，终态同样是本类型，而它的尾部既无
// finish_reason 也无 [DONE]。故原判据「本类型即正文已交付」是错的，必须显式要求标记已见：
// 标记已见 ⇒ 正文按分帧交付完毕，缺的只是分类没归到 TerminalCompleted（io.EOF 提前返回所致）；
// 标记未见 ⇒ 正文可能被中途切断，属供应商侧真故障，必须走墓碑与冷却。
//
// 为何两边都不写（既不写 winner 也不写故障墓碑）：
//   - 落库行与可用性投影对这类收尾按「成功」记账（终态层刻意不给它写 error_message，
//     见 dataplane 的 Stream 结算；DB 侧成功率判据按 2xx 判 success），选路侧不该反过来
//     记成「这家刚失败过」——生产事故正是这条误判把 60 秒会话冷却写给了一家健康渠道；
//   - 奖励也不行：把跨会话的前缀亲和 tip 写回给一个漏发终止标记的渠道，是另一件未被求的事；
//   - 与 Node 的判据同源：它写墓碑的前提是「存在错误文案」，本例没有；与同文件的 incomplete
//     分支同形——证据不足的收尾不做判定。
//
// 四个显式条件缺一不可：非 2xx 是上游明说失败；ErrorText 非空是流内错误帧（Err==io.EOF
// 的收尾路径不看 ErrorText，故必须在这里判）；Bytes 为 0 是「200 后立刻干净 EOF」即正文
// 从未送达；CompletionMarker 为假是正文可能被中途切断——这四种都不放过，照旧写墓碑与冷却。
func streamBodyDeliveredWithoutMarker(outcome forward.StreamOutcome, success bool) bool {
	return success &&
		outcome.Kind == forward.TerminalUpstreamTruncated &&
		outcome.Observation.CompletionMarker &&
		outcome.Observation.ErrorText == "" &&
		outcome.Observation.Bytes > 0
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
