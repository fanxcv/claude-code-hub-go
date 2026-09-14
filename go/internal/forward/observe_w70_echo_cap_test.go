package forward

import (
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住「真实 Codex/Responses 客户端的流」在观测器里**必须走帧路径**，而不是退化成
// 窗口猜测。尺寸取自 2026-09-13 对生产上游 `https://ollama.com/v1/responses` 的真实抓取。
//
// 实测（真实抓取，请求体 213,713 B：instructions ~180 KB + 24 个带描述的 tools）：
//
//	响应 641,697 B / 仅 18 行；行长 max=213,635、次大=213,331、中位=147；
//	超过 64 KiB 的行 = 3
//	  帧 #1  response.created   213,327 B（回显完整请求体）
//	  帧 #3  response.in_progress 213,331 B（同样回显）
//	  帧 #17 response.completed 213,635 B（终态，**带真实 usage**）
//
// 观测器此前把分帧器上限设为 max(HeadBytes, 64 KiB) 且**没有**门控那套请求回显豁免
// （`gate.go` 的 BufferLimitExemption + IsRequestEchoFrame），于是真实客户端的第一帧就超限：
// `parserLost` 立刻置位、整条流的事实只能靠 4 KiB 头尾窗口去猜。
// 生产后果（实测）：`Ollama Codex` 供应商 Go 时代 3665 行里 1655 行没有用量（45.2%），
// 而 Node 时代同供应商同模型 2882 行只缺 54 行（1.9%）。

// realEchoFrameBytes 是真实抓取里请求回显帧的实测规模（213 KB）——常规上限（256 KiB）足以容纳它。
const realEchoFrameBytes = 213 << 10

// echoOnlyFrameBytes 落在**常规上限之上、豁免上限之下**：只有「回显帧豁免」能兜住。
// 取 512 KiB——真实客户端（超大系统提示 + 大工具表）确实能到这个量级，
// 而门控侧对回显帧的容忍度（2×4 MiB）远高于观测侧的常规上限。
const echoOnlyFrameBytes = 512 << 10

// responsesEchoFrame 造一个 openai-responses 家族的请求回显帧（response.created/in_progress）。
func responsesEchoFrame(t *testing.T, size int) string {
	t.Helper()
	return `event: response.created` + "\n" +
		`data: {"type":"response.created","response":{"model":"deepseek-v4.1-flash","instructions":"` +
		strings.Repeat("x", size) + `","usage":null}}` + "\n\n"
}

// responsesTerminalFrame 造终态帧（带真实形状的 usage）。
func responsesTerminalFrame(t *testing.T, inputTokens, outputTokens int) string {
	t.Helper()
	return `event: response.completed` + "\n" +
		`data: {"type":"response.completed","response":{"model":"deepseek-v4.1-flash",` +
		`"usage":{"input_tokens":` + itoa(inputTokens) + `,` +
		`"input_tokens_details":{"cached_tokens":0},` +
		`"output_tokens":` + itoa(outputTokens) + `,` +
		`"output_tokens_details":{"reasoning_tokens":0},` +
		`"total_tokens":` + itoa(inputTokens+outputTokens) + `}}}` + "\n\n"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

// feedObserver 按 8 KiB 分块喂入，模拟真实网络 chunk 边界。
func feedObserver(t *testing.T, observer *Observer, stream string) {
	t.Helper()
	raw := []byte(stream)
	for offset := 0; offset < len(raw); offset += 8 << 10 {
		end := offset + (8 << 10)
		if end > len(raw) {
			end = len(raw)
		}
		observer.Push(raw[offset:end])
	}
}

// TestObserverParsesRealSizedRequestEchoFrameWithoutOverflow 钉住：真实客户端规模的请求回显帧
// 不得被判成「流异常」，也不得让整条流退化成窗口回退。
//
// 这是本波次的主缺陷：观测器的分帧器缺门控那套请求回显豁免。
func TestObserverParsesRealSizedRequestEchoFrameWithoutOverflow(t *testing.T) {
	observer := newTestObserver(convert.FormatResponse, DefaultStreamHeadBytes, DefaultStreamTailBytes)
	stream := responsesEchoFrame(t, realEchoFrameBytes) +
		`event: response.output_text.delta` + "\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		responsesTerminalFrame(t, 56812, 4)

	feedObserver(t, observer, stream)
	snapshot := observer.Snapshot()

	if snapshot.BufferOverflow {
		t.Fatalf("真实规模的请求回显帧（%d B）不应被当成缓冲溢出：整条流会退化成窗口回退", realEchoFrameBytes)
	}
	if snapshot.Usage.InputTokens == nil || *snapshot.Usage.InputTokens != 56812 {
		t.Fatalf("应从帧路径取到 usage.input_tokens=56812，实际 %#v（overflow=%v）",
			snapshot.Usage.InputTokens, snapshot.BufferOverflow)
	}
	if snapshot.Usage.OutputTokens == nil || *snapshot.Usage.OutputTokens != 4 {
		t.Fatalf("应从帧路径取到 usage.output_tokens=4，实际 %#v", snapshot.Usage.OutputTokens)
	}
	if !snapshot.CompletionMarker {
		t.Fatal("应从帧路径认到终态标记（窗口猜测不算）")
	}
	// 帧路径工作时应真正数到全部帧（回显 1 + delta 1 + 终态 1）。
	if snapshot.Frames < 3 {
		t.Fatalf("帧数 = %d，期望 >= 3（说明回显帧之后的帧没有全部进入观测）", snapshot.Frames)
	}
}

// TestObserverKeepsFramesDeliveredBeforeParserError 钉住：分帧器报错时，**同一 chunk 里已经
// 解析出来的帧不得被丢弃**。
//
// 机理：`gate.Parser.Push` 把帧收进局部切片，出错时 `return nil, err` —— 已解析的帧随之消失；
// 观测器的 `consumeFrames` 又在出错时直接 return，于是这批帧的 model/usage 一并丢失。
// 生产上终态帧与超限帧落在同一个读块时，就是「模型有值、用量为空」的成因之一。
func TestObserverKeepsFramesDeliveredBeforeParserError(t *testing.T) {
	// 小窗口：确保 usage 落在窗口之外，只能由帧路径拿到，从而证明帧没有随错误被丢。
	observer := newTestObserver(convert.FormatResponse, 512, 512)

	usage := responsesTerminalFrame(t, 4321, 99)
	// 超限行（非回显帧，故不受豁免）：一条 300 KiB 的 output_text.delta。
	oversized := `event: response.output_text.delta` + "\n" +
		`data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("y", 300<<10) + `"}` + "\n\n"
	// 之后再来一条正常帧，保证流继续被观测。
	after := `event: response.created` + "\n" +
		`data: {"type":"response.created","response":{"model":"deepseek-v4.1-flash","usage":null}}` + "\n\n"

	stream := usage + oversized + after
	// **整段作为一个 chunk 喂入**：usage 帧、超限帧、后续帧同处一块，正是生产里的读块情形。
	observer.Push([]byte(stream))
	snapshot := observer.Snapshot()

	if snapshot.Usage.InputTokens == nil || *snapshot.Usage.InputTokens != 4321 {
		t.Fatalf("超限帧之前的终态帧事实被丢弃了：input_tokens = %#v，期望 4321", snapshot.Usage.InputTokens)
	}
	if snapshot.Usage.OutputTokens == nil || *snapshot.Usage.OutputTokens != 99 {
		t.Fatalf("超限帧之前的终态帧事实被丢弃了：output_tokens = %#v，期望 99", snapshot.Usage.OutputTokens)
	}
	if !snapshot.BufferOverflow {
		t.Fatal("超限行仍应如实标记缓冲溢出（诊断信号不能哑）")
	}
}

// TestObserverEchoFrameExemptionHasCeiling 钉住豁免有上限：远超豁免上限的回显帧仍按溢出处理
// （宁可退回窗口回退，也不无限驻留）。
func TestObserverEchoFrameExemptionHasCeiling(t *testing.T) {
	observer := newTestObserver(convert.FormatResponse, DefaultStreamHeadBytes, DefaultStreamTailBytes)
	// 构造一个超过豁免上限的回显帧。
	huge := responsesEchoFrame(t, DefaultObserverEchoFrameBytes+(64<<10)) + responsesTerminalFrame(t, 7, 8)
	feedObserver(t, observer, huge)
	snapshot := observer.Snapshot()

	if !snapshot.BufferOverflow {
		t.Fatal("超过豁免上限的回显帧应仍被当成溢出（豁免必须有天花板）")
	}
	// 即便溢出，终态帧的 usage 仍应能从窗口回退里拿回来（usage 在流尾）。
	if snapshot.Usage.InputTokens == nil || *snapshot.Usage.InputTokens != 7 {
		t.Fatalf("溢出后仍应通过窗口回退取到 usage，实际 %#v", snapshot.Usage.InputTokens)
	}
}

// TestObserverEchoFrameExemptionCoversBeyondOrdinaryCap 钉住豁免本身：落在常规上限之上、
// 豁免上限之下的回显帧必须被豁免，否则这类客户端（超大提示）的整条流仍会退化成窗口猜测。
func TestObserverEchoFrameExemptionCoversBeyondOrdinaryCap(t *testing.T) {
	if echoOnlyFrameBytes <= DefaultObserverParserBytes || echoOnlyFrameBytes > DefaultObserverEchoFrameBytes {
		t.Fatalf("夹具尺寸 %d 必须落在（常规上限 %d, 豁免上限 %d] 区间内，否则本用例失去意义",
			echoOnlyFrameBytes, DefaultObserverParserBytes, DefaultObserverEchoFrameBytes)
	}
	observer := newTestObserver(convert.FormatResponse, DefaultStreamHeadBytes, DefaultStreamTailBytes)
	feedObserver(t, observer, responsesEchoFrame(t, echoOnlyFrameBytes)+responsesTerminalFrame(t, 31415, 9))
	snapshot := observer.Snapshot()

	if snapshot.BufferOverflow {
		t.Fatalf("回显帧（%d B，在豁免上限内）不应被当成缓冲溢出", echoOnlyFrameBytes)
	}
	if snapshot.Usage.InputTokens == nil || *snapshot.Usage.InputTokens != 31415 {
		t.Fatalf("应从帧路径取到 usage.input_tokens=31415，实际 %#v", snapshot.Usage.InputTokens)
	}
}
