package ws

import "bytes"

// sseSplitter 按事件边界切分 SSE 字节流。
//
// 分隔符与 Node 的扫描正则 `/\r?\n\r?\n/` 同口径：一个空行（LF、CRLF 或两者混写）结束一个事件，
// 分隔符本身不含在事件块内（与 Node 的 `buffer.slice(eventStart, delimiter.index)` 一致）。
// 跨 chunk 被截断的尾部保留在缓冲里，等下一段字节到达后再判定——这是「事件边界不得因
// HTTP 分块而漂移」的必要条件。
type sseSplitter struct {
	buf []byte
}

// push 追加一段字节，返回本次新切出的完整事件块（不含分隔符）。
func (s *sseSplitter) push(chunk []byte) [][]byte {
	s.buf = append(s.buf, chunk...)
	var events [][]byte
	start := 0
	for {
		offset, width := findSSEDelimiter(s.buf[start:])
		if offset < 0 {
			break
		}
		events = append(events, s.buf[start:start+offset])
		start += offset + width
	}
	if start > 0 {
		// 原地搬移剩余字节，避免每次切分都重新分配整个缓冲。
		s.buf = append(s.buf[:0], s.buf[start:]...)
	}
	return events
}

// pending 返回尚未成块的尾部。
func (s *sseSplitter) pending() []byte { return s.buf }

// takePending 取走尾部并清空缓冲（收尾时把残留当作最后一个事件处理）。
func (s *sseSplitter) takePending() []byte {
	pending := s.buf
	s.buf = nil
	return pending
}

// findSSEDelimiter 找最早的「空行」分隔符，返回偏移与宽度；未出现完整分隔符时返回 (-1, 0)。
//
// 四种组合都要认（Node 的正则等价）：`\n\n`、`\n\r\n`、`\r\n\n`、`\r\n\r\n`。
func findSSEDelimiter(buffer []byte) (int, int) {
	for index := 0; index+1 < len(buffer); index++ {
		switch {
		case buffer[index] == '\n' && buffer[index+1] == '\n':
			return index, 2
		case buffer[index] == '\n' && buffer[index+1] == '\r':
			if index+2 < len(buffer) && buffer[index+2] == '\n' {
				return index, 3
			}
		case buffer[index] == '\r' && buffer[index+1] == '\n':
			if index+3 < len(buffer) && buffer[index+2] == '\r' && buffer[index+3] == '\n' {
				return index, 4
			}
			if index+2 < len(buffer) && buffer[index+2] == '\n' {
				return index, 3
			}
		}
	}
	return -1, 0
}

// sseEventData 提取事件块里的 `data:` 字段值；无 data 行时返回 nil。
//
// SSE 规定冒号后至多削掉一个空格，其余空白属于数据本身（与 Node 的处理完全相同）。
// 多行 data 用 `\n` 连接。
func sseEventData(event []byte) []byte {
	var collected [][]byte
	found := false
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := line[len("data:"):]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		collected = append(collected, value)
		found = true
	}
	if !found {
		return nil
	}
	return bytes.Join(collected, []byte("\n"))
}
