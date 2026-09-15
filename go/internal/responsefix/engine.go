package responsefix

import "time"

// FixerApplied 是审计条目里的一项修复器结果（Node 的 `fixersApplied[]`）。
type FixerApplied struct {
	Fixer   string
	Applied bool
	Details string
}

// Audit 是本次响应修复的审计事实（Node 的 `ResponseFixerSpecialSetting`）。
//
// 字段与 Node 逐字对齐：type=response_fixer、scope=response、hit、fixersApplied、
// totalBytesProcessed、processingTimeMs。Hit 为假时数据面不写任何审计（Node 同）。
type Audit struct {
	// Hit 表示至少有一个修复器真的改过字节。
	Hit bool
	// TotalBytesProcessed 是流经本层的字节数（非流式是正文长度；流式是所有 chunk 之和）。
	TotalBytesProcessed int64
	// ProcessingTimeMS 是修复耗时（毫秒，向上取整）。
	ProcessingTimeMS int
	// EncodingApplied/EncodingDetails 是编码修复器的结果。
	EncodingApplied bool
	EncodingDetails string
	// SSEApplied/SSEDetails 是 SSE 修复（含惰性帧过滤）的结果。
	SSEApplied bool
	SSEDetails string
	// JSONApplied/JSONDetails 是截断 JSON 补全的结果。
	JSONApplied bool
	JSONDetails string
	// IncludeSSE 决定审计里是否列出 sse 修复器：非流式路径不会出现（Node 的 includeSse=false）。
	IncludeSSE bool
}

// Entry 产出写进 special_settings 的审计条目。
//
// 与 Node 的差别只有一处：Node 的 details 是 `undefined` 时不会进 JSON，Go 侧用空串表示
// 同一件事，故空串键一律省略。fixersApplied 的顺序是 encoding → [sse] → json（Node 固定序）。
func (a Audit) Entry() map[string]any {
	fixers := make([]map[string]any, 0, 3)
	fixers = append(fixers, fixerEntry("encoding", a.EncodingApplied, a.EncodingDetails))
	if a.IncludeSSE {
		fixers = append(fixers, fixerEntry("sse", a.SSEApplied, a.SSEDetails))
	}
	fixers = append(fixers, fixerEntry("json", a.JSONApplied, a.JSONDetails))
	return map[string]any{
		"type":                "response_fixer",
		"scope":               "response",
		"hit":                 a.Hit,
		"fixersApplied":       fixers,
		"totalBytesProcessed": a.TotalBytesProcessed,
		"processingTimeMs":    a.ProcessingTimeMS,
	}
}

func fixerEntry(name string, applied bool, details string) map[string]any {
	entry := map[string]any{"fixer": name, "applied": applied}
	if details != "" {
		entry["details"] = details
	}
	return entry
}

// ApplyNonStream 修一段完整的非流式正文：encoding → 截断 JSON。
//
// 与 Node 的 processNonStream 一致：**不**做 SSE 修复、**不**做过帧过滤——
// 那两项只属于流式路径。返回的 Audit 供调用方在 hit 时落审计。
func ApplyNonStream(body []byte, config Config) ([]byte, Audit) {
	startedAt := time.Now()
	audit := Audit{TotalBytesProcessed: int64(len(body))}
	data := body

	if config.FixEncoding {
		result := EncodingFixer{}.Fix(data)
		if result.Applied {
			audit.EncodingApplied = true
			audit.EncodingDetails = result.Details
			data = result.Data
		}
	}

	if config.FixTruncatedJSON {
		result := JSONFixer{MaxDepth: config.MaxJSONDepth, MaxSize: config.MaxFixSize}.Fix(data)
		if result.Applied {
			audit.JSONApplied = true
			audit.JSONDetails = result.Details
			data = result.Data
		}
	}

	audit.Hit = audit.EncodingApplied || audit.JSONApplied
	audit.ProcessingTimeMS = elapsedMS(startedAt)
	return data, audit
}

// StreamFixer 是流式路径的修复器：逐块喂入，按「完整行」边界吐出修复后的字节。
//
// 为什么必须跨块缓冲：SSE 帧与行都可能被上游切在两个 chunk 之间，逐块修会把半个 JSON 当
// 完整载荷处理（补出来的括号是错的）。Node 的 ChunkBuffer 同此，且带两条守卫：
//
//   - 行尾的 CR 要等下一块确认是否 CRLF，故不切分（pendingCR）；
//   - 缓冲超过 maxFixSize 即整体降级为透传（不再修复也不再缓冲），防止上游长期不发换行
//     把内存拖成无界（Node 的 passthrough 分支）。
//
// 与 Node 的一处实现差异：Node 用「块数组 + 游标」避免搬字节，本实现用单块缓冲（上限就是
// maxFixSize）。ponytail: 上限固定时搬运总量有界，换更简单的代码；若将来 maxFixSize 提到
// 百 MB 级，再换回块队列。
type StreamFixer struct {
	config          Config
	clientResponses bool

	encoding *EncodingFixer
	sse      *SSEFixer
	json     *JSONFixer

	buffer         []byte
	processableEnd int
	pendingCR      bool
	passthrough    bool

	audit Audit
}

// NewStreamFixer 构造流式修复器。clientResponses 表示客户端入站格式是 OpenAI Responses
// （Node 的 `session.originalFormat === "response"`）——只有这一条线才过滤惰性 chat 帧。
func NewStreamFixer(config Config, clientResponses bool) *StreamFixer {
	fixer := &StreamFixer{config: config, clientResponses: clientResponses}
	if config.FixEncoding {
		fixer.encoding = &EncodingFixer{}
	}
	if config.FixSSEFormat {
		fixer.sse = &SSEFixer{}
	}
	if config.FixTruncatedJSON {
		fixer.json = &JSONFixer{MaxDepth: config.MaxJSONDepth, MaxSize: config.MaxFixSize}
	}
	return fixer
}

// Write 喂入一块上游字节，返回客户端可见的字节（可能为空：本块尚未构成完整行）。
func (f *StreamFixer) Write(chunk []byte) []byte {
	f.audit.TotalBytesProcessed += int64(len(chunk))

	if f.passthrough {
		return chunk
	}

	// 降级：缓冲将超上限即整体透传，并把已缓冲的字节先吐出去（Node 同序）。
	if len(f.buffer)+len(chunk) > f.config.MaxFixSize {
		f.passthrough = true
		out := f.flushBuffer()
		return append(out, chunk...)
	}

	f.push(chunk)
	end := f.processableEndBytes()
	if end <= 0 {
		return nil
	}
	return f.applyFixers(f.take(end))
}

// Flush 结束流：吐出残留字节（Node 的 TransformStream.flush）。
func (f *StreamFixer) Flush() []byte {
	var out []byte
	if len(f.buffer) > 0 {
		out = f.applyFixers(f.drain())
	}
	f.audit.Hit = f.audit.EncodingApplied || f.audit.SSEApplied || f.audit.JSONApplied
	return out
}

// Audit 返回审计事实：IncludeSSE 恒真（流式路径才有 sse 项）。
func (f *StreamFixer) Audit() Audit {
	f.audit.IncludeSSE = true
	f.audit.Hit = f.audit.EncodingApplied || f.audit.SSEApplied || f.audit.JSONApplied
	return f.audit
}

// push 追加一块并增量维护「可处理末尾」索引（只扫新增块，避免每次全量回扫）。
func (f *StreamFixer) push(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	prevTotal := len(f.buffer)
	f.buffer = append(f.buffer, chunk...)

	// 跨块的 CRLF：上一块以 CR 结尾时，要看本块首字节是否为 LF。
	if f.pendingCR {
		if chunk[0] == byteLF {
			f.processableEnd = prevTotal + 1
		} else {
			f.processableEnd = prevTotal
		}
		f.pendingCR = false
	}

	for i, b := range chunk {
		if b == byteLF {
			f.processableEnd = prevTotal + i + 1
			continue
		}
		if b != byteCR {
			continue
		}
		if i+1 < len(chunk) {
			if chunk[i+1] != byteLF {
				f.processableEnd = prevTotal + i + 1
			}
			continue
		}
		// 块尾的 CR：等下一块确认是否为 CRLF。
		f.pendingCR = true
	}
}

// processableEndBytes 返回当前可安全处理的字节数（Node 的 findProcessableEnd）。
func (f *StreamFixer) processableEndBytes() int {
	if len(f.buffer) == 0 || f.pendingCR {
		return 0
	}
	return f.processableEnd
}

// take 取走前 size 字节并从缓冲里移除。
func (f *StreamFixer) take(size int) []byte {
	if size <= 0 {
		return nil
	}
	if size > len(f.buffer) {
		size = len(f.buffer)
	}
	out := make([]byte, size)
	copy(out, f.buffer[:size])
	f.buffer = append(f.buffer[:0], f.buffer[size:]...)
	f.processableEnd -= size
	if f.processableEnd < 0 {
		f.processableEnd = 0
	}
	return out
}

// drain 取走全部缓冲并清空。
func (f *StreamFixer) drain() []byte {
	out := f.take(len(f.buffer))
	f.flushBuffer()
	return out
}

// flushBuffer 清空缓冲状态。
func (f *StreamFixer) flushBuffer() []byte {
	out := make([]byte, len(f.buffer))
	copy(out, f.buffer)
	f.buffer = f.buffer[:0]
	f.processableEnd = 0
	f.pendingCR = false
	return out
}

// applyFixers 是修复序列：encoding → sse → `data:` 行内 JSON → 惰性帧过滤。
//
// 惰性帧过滤是序列的固定末步，且审计上计入 sse 修复（Node 同）。
func (f *StreamFixer) applyFixers(input []byte) []byte {
	data := input

	if f.encoding != nil {
		result := f.encoding.Fix(data)
		if result.Applied {
			f.audit.EncodingApplied = true
			if f.audit.EncodingDetails == "" {
				f.audit.EncodingDetails = result.Details
			}
			data = result.Data
		}
	}

	if f.sse != nil {
		result := f.sse.Fix(data)
		if result.Applied {
			f.audit.SSEApplied = true
			if f.audit.SSEDetails == "" {
				f.audit.SSEDetails = result.Details
			}
			data = result.Data
		}
	}

	if f.json != nil {
		result := fixSSEJSONLines(data, *f.json)
		if result.Applied {
			f.audit.JSONApplied = true
			if f.audit.JSONDetails == "" {
				f.audit.JSONDetails = result.Details
			}
			data = result.Data
		}
	}

	if f.clientResponses {
		result := filterInertChatCompletionChunks(data)
		if result.Applied {
			f.audit.SSEApplied = true
			if f.audit.SSEDetails == "" {
				f.audit.SSEDetails = result.Details
			}
			data = result.Data
		}
	}

	return data
}

// fixSSEJSONLines 逐行修 `data:` 载荷里的截断 JSON（Node 的 fixSseJsonLines）。
//
// 只处理 LF 分隔的行：SseFixer 的输出已统一为 LF，故这里不必再认 CR。
func fixSSEJSONLines(data []byte, jsonFixer JSONFixer) Result {
	var chunks []byte
	active := false
	cursor := 0
	applied := false

	lineStart := 0
	for i := 0; i < len(data); i++ {
		if data[i] != byteLF {
			continue
		}
		line := data[lineStart:i]
		fixed, fixApplied := fixMaybeDataJSONLine(line, jsonFixer)

		if !fixApplied {
			if active {
				chunks = append(chunks, data[cursor:i+1]...)
				cursor = i + 1
			}
			lineStart = i + 1
			continue
		}

		applied = true
		if !active {
			active = true
			chunks = make([]byte, 0, len(data))
		}
		if cursor < lineStart {
			chunks = append(chunks, data[cursor:lineStart]...)
		}
		chunks = append(chunks, fixed...)
		chunks = append(chunks, byteLF)
		cursor = i + 1
		lineStart = i + 1
	}

	// 末尾无换行的残留（flush 时可能出现）。
	if lineStart < len(data) {
		line := data[lineStart:]
		fixed, fixApplied := fixMaybeDataJSONLine(line, jsonFixer)
		if !fixApplied {
			if active {
				chunks = append(chunks, data[cursor:]...)
			}
		} else {
			applied = true
			if !active {
				active = true
				chunks = make([]byte, 0, len(data))
			}
			if cursor < lineStart {
				chunks = append(chunks, data[cursor:lineStart]...)
			}
			chunks = append(chunks, fixed...)
		}
	}

	if !active {
		return Result{Data: data}
	}
	return Result{Data: chunks, Applied: applied}
}

// fixMaybeDataJSONLine 修一行 `data: <json>`：只修载荷，前缀按规范重建为 `data: `。
func fixMaybeDataJSONLine(line []byte, jsonFixer JSONFixer) ([]byte, bool) {
	if len(line) < len(prefixData) {
		return line, false
	}
	for i := 0; i < len(prefixData); i++ {
		if line[i] != prefixData[i] {
			return line, false
		}
	}

	payloadStart := len(prefixData)
	if payloadStart < len(line) && line[payloadStart] == byteSpace {
		payloadStart++
	}

	payload := line[payloadStart:]
	result := jsonFixer.Fix(payload)
	if !result.Applied {
		return line, false
	}

	out := make([]byte, 0, len(prefixData)+1+len(result.Data))
	out = append(out, prefixData...)
	out = append(out, byteSpace)
	out = append(out, result.Data...)
	return out, true
}

// elapsedMS 取毫秒耗时（与 Node 的 `Math.max(0, Math.round(now - startedAt))` 同向）。
func elapsedMS(startedAt time.Time) int {
	elapsed := int(time.Since(startedAt).Milliseconds())
	if elapsed < 0 {
		return 0
	}
	return elapsed
}
