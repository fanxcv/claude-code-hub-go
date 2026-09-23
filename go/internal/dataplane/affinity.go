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
//   - 墓碑（5420-5426）：供应商侧错误帧、未正常结束、静默超时等写墓碑。
//     TerminalIncomplete 是协议合法收尾、内容因输出额度等原因未完成，
//     并非供应商故障，故两边都不写。
//
// 本仓另加一类本类型未覆盖的收尾：
//   - 客户端主动中断：前缀墓碑照写、会话绑定侧不动（Node 对齐，见下）。
//
// 关于「流尾缺协议终止标记但正文已按分帧交付完毕」：**这一类不存在**，故本函数不再有
// 对应分支，见下方注记。
func affinityDirectiveForStream(outcome forward.StreamOutcome) terminal.AffinityDirective {
	success := outcome.StatusCode >= 200 && outcome.StatusCode < 300
	if outcome.Kind == forward.TerminalIncomplete {
		return terminal.AffinityDirective{}
	}
	if success && outcome.Kind == forward.TerminalCompleted {
		return terminal.AffinityDirective{WinnerProviderID: outcome.Provider.ID}
	}

	// 客户端主动中断不是供应商故障：前缀墓碑照写（Node 对齐），但会话绑定侧不得动作——
	// 否则用户按停就会给一家健康渠道写 60 秒冷却，下一请求无故换家。
	if outcome.Kind == forward.TerminalClientAborted {
		return terminal.AffinityDirective{
			TombstoneProviderID: outcome.Provider.ID,
			TombstoneKind:       terminal.AffinityTombstonePrefixOnly,
		}
	}
	// 上游在正文中途干净断流（无协议终止标记、内部状态码 2xx）：写墓碑 + 会话冷却，但冷却
	// 走**独立种类**，读侧据此把它与真实故障冷却分开（无替代候选时可 fail-open）。
	// 非 2xx 的截断（上游先给错误状态再断）不走本支，照旧按供应商故障冷却。
	if outcome.Kind == forward.TerminalUpstreamTruncated && success {
		return terminal.AffinityDirective{
			TombstoneProviderID: outcome.Provider.ID,
			TombstoneKind:       terminal.AffinityTombstoneUpstreamStreamCut,
		}
	}
	// 上游在正文中途发错误帧（内部状态码 2xx，且已向客户端交付过内容）：与「上游中途断流」
	// 同一个物理事件——上游读到非 EOF 错后先补一帧 error、再补终止标记（wb 池代理 2026-09
	// 上线的新收尾）。终态因此从 TerminalUpstreamTruncated 变成 TerminalUpstreamError
	// （error 帧文案命中 ErrorText，优先级高于 CompletionMarker），冷却必须与断流同档归软，
	// 否则同一事件只因多写一帧就从严转宽。
	//
	// 判据 `Observation.Bytes > 0` 即「已过门控提交」：观测器只在提交后随 Stream 构造
	// （forward/stream.go:277 调用 newStream，定义 663），且只喂提交后的字节——门控前缀
	// （forward/stream.go:719）与泵读到的正文（forward/stream.go:812），字节计数在
	// forward/observe.go:264。提交前的错误帧被门控拦成 FailGateError（gate/gate.go:508/579）
	// → Failure，根本不会产生本终态；故本终态带非零字节 ⟺ 客户端已拿到内容。
	// 零字节（未交付过任何内容）仍落默认分支（故障冷却，硬）。
	if outcome.Kind == forward.TerminalUpstreamError && success && outcome.Observation.Bytes > 0 {
		return terminal.AffinityDirective{
			TombstoneProviderID: outcome.Provider.ID,
			TombstoneKind:       terminal.AffinityTombstoneUpstreamStreamCut,
		}
	}
	return terminal.AffinityDirective{TombstoneProviderID: outcome.Provider.ID}
}

// 注记：曾经的 streamBodyDeliveredWithoutMarker 及其调用点（「流尾缺协议终止标记但正文已交付
// ⇒ 两边都不写墓碑」）已删除，因为它的前提是错的，且现在**结构上不可能成立**：
//
//   terminalKindFor 只在**未见到终止标记**时落 TerminalUpstreamTruncated（CompletionMarker
//   为真时走 TerminalCompleted；其 io.EOF 分支不可达且已补齐标记判断，见 forward/stream.go）。
//   故「本类型」与「正文已交付」互斥，该函数恒为假——留着它只会让读者以为存在这一类收尾。
//
// 为何原前提看着合理却错了：它断言「本类型 = 干净的 EOF，正文被中途切断会落 LocalError」。
// 前半句对（上游 FIN 确实是干净 EOF），后半句不对——**上游在正文中途干净 FIN 也落本类型**。
// 生产实证（2026-09-23，wb 池代理）：客户端拿到残流（尾部既无 finish_reason 也无 [DONE]），
// 终态正是本类型，而原实现把它当成功放过去了 ⇒ 客户端可见的失败既不计账也不冷却。
//
// 故本类型一律落到下方默认返回（墓碑 + 会话冷却），不再有例外分支。

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
