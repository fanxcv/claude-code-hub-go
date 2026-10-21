package forward

import (
	"bytes"
	"errors"
	"io"
)

// 本文件承载出站正文的**分段**表示：模型名改写只替换顶层 model 的值，其余字节引用入参
// 正文的子切片，故不必为一次改写复制整篇正文。
//
// 动机（生产活堆实测，hk 1.14.0）：改写产生的整篇副本占 16.1%。去掉它，一份正文在请求期
// 同时存在的份数由三（入站原文 / 改写副本 / 出站编码）降到二。
//
// 分段的落地时机逐条如下（判据写在各自调用点）：
//   - 拨号：Plan.Request 交出可 Seek 的分段 reader，不落地；
//   - 会**重新序列化**正文的步骤（供应商覆写、include_usage 补齐）：在它们之前落地，
//     否则 model 值的转义形态会随先后顺序变化；
//   - 只做字节区间替换的步骤（空参数归一）与改写互不重叠，可交换，不落地；
//   - 确实要连续字节的读取方（WS 帧正文、结算期的思考强度探针、缓存 TTL 解析）走
//     MaterializedBody。

// bodyParts 是分段正文：各段按序拼接即为发往上游的字节。
type bodyParts struct {
	segments [][]byte
	length   int
}

// append 追加一段；空段丢掉（拼接结果不变，读取方少一次边界判断）。
func (p *bodyParts) append(segment []byte) {
	if len(segment) == 0 {
		return
	}
	p.segments = append(p.segments, segment)
	p.length += len(segment)
}

// materialize 把各段拼成一份连续正文（本文件唯一会复制正文的地方）。
func (p *bodyParts) materialize() []byte {
	body := make([]byte, 0, p.length)
	for _, segment := range p.segments {
		body = append(body, segment...)
	}
	return body
}

// contains 报告拼接后的正文里是否出现 needle；跨段边界的命中也算。
//
// 只用于「需不需要为某一步落地」的判定。段内命中直接算；跨段命中只可能从某段末尾
// len(needle)-1 字节之内开始，故只拼那一小截邻域，不拼整篇。
func (p *bodyParts) contains(needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for index, segment := range p.segments {
		if bytes.Contains(segment, needle) {
			return true
		}
		overlap := len(needle) - 1
		if overlap > len(segment) {
			overlap = len(segment)
		}
		window := append([]byte(nil), segment[len(segment)-overlap:]...)
		// 尾巴只覆盖 needle 的前半，后续段再取满一个 needle 的长度就够（窗口至多 2 倍 needle）。
		appended := 0
		for _, next := range p.segments[index+1:] {
			take := len(next)
			if remaining := len(needle) - appended; take > remaining {
				take = remaining
			}
			window = append(window, next[:take]...)
			appended += take
			if appended >= len(needle) {
				break
			}
		}
		if bytes.Contains(window, needle) {
			return true
		}
	}
	return false
}

// Seek 的非法入参（与 bytes.Reader 同语义：只拒负偏移，允许越过末尾）。
var (
	errBodyPartsSeekWhence   = errors.New("forward: 分段正文 reader 不支持的 Seek 起点")
	errBodyPartsSeekNegative = errors.New("forward: 分段正文 reader 不能 Seek 到负偏移")
)

// bodyPartsReader 把分段正文读成一份流，并支持 Seek。
//
// 可 Seek 是拨号层的硬契约：HTTP/2 协议错误后要回退 HTTP/1.1 并**重放**正文（见 dial 的
// rewindBody），故这里不能只做 io.Reader。
type bodyPartsReader struct {
	parts  [][]byte
	length int64
	offset int64
}

func (r *bodyPartsReader) Read(p []byte) (int, error) {
	if r.offset >= r.length {
		return 0, io.EOF
	}
	written := 0
	skip := r.offset
	for _, part := range r.parts {
		if written == len(p) {
			break
		}
		if skip >= int64(len(part)) {
			skip -= int64(len(part))
			continue
		}
		n := copy(p[written:], part[skip:])
		written += n
		skip = 0
	}
	r.offset += int64(written)
	return written, nil
}

func (r *bodyPartsReader) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = r.offset + offset
	case io.SeekEnd:
		target = r.length + offset
	default:
		return 0, errBodyPartsSeekWhence
	}
	if target < 0 {
		return 0, errBodyPartsSeekNegative
	}
	r.offset = target
	return target, nil
}

// MaterializedBody 返回连续的出站正文。
//
// 分段时首调拼接一次并缓存到 Body（此后 Request 也走它），后续调用零分配；没有分段时
// 直接返回 Body。只有确实需要连续字节的读取方该调它——请求期调它等于把本 lane 省下的
// 那份副本又造回来。
func (p *Plan) MaterializedBody() []byte {
	if p == nil {
		return nil
	}
	if p.parts != nil {
		p.Body = p.parts.materialize()
		p.parts = nil
	}
	return p.Body
}
