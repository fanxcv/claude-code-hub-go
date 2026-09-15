package responsefix

import "bytes"

// 逐字节处理 SSE 时需要的最小常量集。
const (
	byteLF    = '\n'
	byteCR    = '\r'
	byteSpace = ' '
)

// SSE 字段前缀（`data:` / `event:` / `id:` / `retry:`），用于逐行判定字段类型。
var (
	prefixData  = []byte("data:")
	prefixEvent = []byte("event:")
	prefixID    = []byte("id:")
	prefixRetry = []byte("retry:")
	prefixColon = []byte(":")
	bytesData   = []byte("data")
	doneMarker  = []byte("[DONE]")
)

// SSEFixer 移植 Node 的 `SseFixer`（sse-fixer.ts）：把畸形的 SSE 帧修成规范帧。
//
// 修的四种形态（都是上游手搓 SSE 时的常见笔误）：
//
//  1. 冒号后缺空格：`data:{"a":1}` → `data: {"a":1}`（规范要求字段名后紧跟一个空格）；
//  2. 漏掉 `data:` 前缀的裸 JSON 行 / `[DONE]` 行 → 补 `data: `；
//  3. `data :` 或 `Data:` / `DATA:` 这类大小写与空白错误 → 归一为小写 `data: `；
//  4. 换行不统一：CRLF/CR 一律归一为 LF；连续空行折叠为一个（SSE 用空行分隔事件，
//     多出来的空行会让某些客户端解析出空事件）；末尾缺换行则补一个。
//
// 没改动的输入必须**原样返回同一个切片**（不只是内容相等）：Node 的用例把这条钉住了，
// 它保证绝大多数正常帧不产生任何拷贝。
type SSEFixer struct{}

// CanFix 判定输入是否可能含 SSE 帧（Node 同名方法：字段前缀 / `data` 前缀 / 裸 JSON / 含 `data:`）。
func (SSEFixer) CanFix(data []byte) bool {
	if bytes.HasPrefix(data, prefixData) ||
		bytes.HasPrefix(data, prefixEvent) ||
		bytes.HasPrefix(data, prefixID) ||
		bytes.HasPrefix(data, prefixRetry) ||
		bytes.HasPrefix(data, prefixColon) {
		return true
	}
	if len(data) >= 4 {
		if lowerASCII(data[0]) == 'd' && lowerASCII(data[1]) == 'a' &&
			lowerASCII(data[2]) == 't' && lowerASCII(data[3]) == 'a' {
			return true
		}
	}
	if looksLikeJSONLine(data) {
		return true
	}
	return bytes.Contains(data, prefixData)
}

// Fix 返回修复后的字节；未发生变更时 Applied=false 且 Data 与输入同一切片。
func (f SSEFixer) Fix(input []byte) Result {
	if !f.CanFix(input) {
		return Result{Data: input}
	}

	var out []byte
	active := false
	cursor := 0
	changed := false
	lastWasEmpty := false

	// startSegment 在首次发生变更时把「变更点之前的原始字节」整段搬进输出。
	startSegment := func(start int) {
		if active {
			if cursor < start {
				out = append(out, input[cursor:start]...)
				cursor = start
			}
			return
		}
		active = true
		out = make([]byte, 0, len(input))
		if start > 0 {
			out = append(out, input[:start]...)
		}
		cursor = start
	}

	pos := 0
	for pos < len(input) {
		start := pos
		scan := start
		lineEnd := len(input)
		nextPos := len(input)
		newlineNormalized := false

		for scan < len(input) {
			b := input[scan]
			if b == byteLF {
				lineEnd = scan
				nextPos = scan + 1
				break
			}
			if b == byteCR {
				lineEnd = scan
				nextPos = scan + 1
				if nextPos < len(input) && input[nextPos] == byteLF {
					nextPos++
				}
				newlineNormalized = true
				break
			}
			scan++
		}

		// 末尾没有换行：Node 的历史行为是补一个 LF，这里显式标记为变更
		// （否则会出现 applied=false 但数据已经不同）。
		if nextPos == len(input) && lineEnd == len(input) {
			newlineNormalized = true
		}

		pos = nextPos
		line := input[start:lineEnd]

		if len(line) == 0 {
			switch {
			case lastWasEmpty:
				// 连续空行：丢弃当前这一行（不输出任何内容）。
				changed = true
				startSegment(start)
				cursor = pos
			case newlineNormalized:
				changed = true
				startSegment(start)
				out = append(out, byteLF)
				cursor = pos
			case active:
				out = append(out, input[cursor:pos]...)
				cursor = pos
			}
			lastWasEmpty = true
			continue
		}
		lastWasEmpty = false

		fixed, applied := fixSSELine(line)
		segmentChanged := applied || newlineNormalized
		if segmentChanged {
			changed = true
			startSegment(start)
			if applied {
				out = append(out, fixed...)
			} else {
				out = append(out, line...)
			}
			out = append(out, byteLF)
			cursor = pos
		} else if active {
			out = append(out, input[cursor:pos]...)
			cursor = pos
		}
	}

	if !active {
		return Result{Data: input}
	}
	if cursor < len(input) {
		out = append(out, input[cursor:]...)
	}
	return Result{Data: out, Applied: changed}
}

// fixSSELine 修一行（不含换行符）。返回修复后的行与是否改过。
func fixSSELine(line []byte) ([]byte, bool) {
	switch {
	case bytes.HasPrefix(line, prefixData):
		return fixDataLine(line)
	case bytes.HasPrefix(line, prefixEvent):
		return fixFieldLine(line, prefixEvent)
	case bytes.HasPrefix(line, prefixID):
		return fixFieldLine(line, prefixID)
	case bytes.HasPrefix(line, prefixRetry):
		return fixFieldLine(line, prefixRetry)
	case bytes.HasPrefix(line, prefixColon):
		// 注释行：SSE 规范允许，保持原样。
		return line, false
	}

	if looksLikeJSONLine(line) {
		out := make([]byte, 0, 6+len(line))
		out = append(out, prefixData...)
		out = append(out, byteSpace)
		out = append(out, line...)
		return out, true
	}

	if fixed, applied := tryFixMalformedSSELine(line); applied {
		return fixed, true
	}
	return line, false
}

// fixDataLine 给 `data:` 后补一个空格（已有空格则不动）。
func fixDataLine(line []byte) ([]byte, bool) {
	return fixFieldLine(line, prefixData)
}

// fixFieldLine 给字段前缀后补一个空格（已有空格则不动）。
func fixFieldLine(line []byte, prefix []byte) ([]byte, bool) {
	if !bytes.HasPrefix(line, prefix) {
		return line, false
	}
	after := line[len(prefix):]
	if len(after) > 0 && after[0] == byteSpace {
		return line, false
	}
	out := make([]byte, 0, len(prefix)+1+len(after))
	out = append(out, prefix...)
	out = append(out, byteSpace)
	out = append(out, after...)
	return out, true
}

// tryFixMalformedSSELine 修两种「连前缀都不规范」的写法：
//
//	模式 1：`data :xxx`（data 与冒号之间只有空白）→ `data: xxx`；
//	模式 2：`Data:` / `DATA:` 等大小写错误 → 先归一前缀，再按模式 1 的规则补空格。
func tryFixMalformedSSELine(line []byte) ([]byte, bool) {
	if bytes.HasPrefix(line, bytesData) {
		rest := line[len(bytesData):]
		colonPos := bytes.IndexByte(rest, ':')
		if colonPos >= 0 {
			onlyWhitespace := true
			for _, b := range rest[:colonPos] {
				if !isWhitespaceByte(b) {
					onlyWhitespace = false
					break
				}
			}
			if onlyWhitespace {
				trimmed := bytes.TrimLeft(rest[colonPos+1:], " ")
				out := make([]byte, 0, 6+len(trimmed))
				out = append(out, prefixData...)
				out = append(out, byteSpace)
				out = append(out, trimmed...)
				return out, true
			}
		}
	}

	if len(line) >= 5 &&
		lowerASCII(line[0]) == 'd' && lowerASCII(line[1]) == 'a' &&
		lowerASCII(line[2]) == 't' && lowerASCII(line[3]) == 'a' && line[4] == ':' {
		normalized := make([]byte, len(line))
		copy(normalized, prefixData)
		copy(normalized[5:], line[5:])
		if fixed, applied := fixDataLine(normalized); applied {
			return fixed, true
		}
		return normalized, true
	}

	return nil, false
}

// looksLikeJSONLine 判定「裸 JSON 行」：跳过前导空白后是 `{` / `[`，或 `[DONE]` 终止标记。
func looksLikeJSONLine(line []byte) bool {
	i := 0
	for i < len(line) && isWhitespaceByte(line[i]) {
		i++
	}
	if i >= len(line) {
		return false
	}
	if line[i] == '{' || line[i] == '[' {
		return true
	}
	if len(line)-i >= len(doneMarker) {
		return bytes.Equal(line[i:i+len(doneMarker)], doneMarker)
	}
	return false
}

// lowerASCII 只在 A-Z 上做小写化（Node 的 toLowerAscii）。
func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 0x20
	}
	return b
}
