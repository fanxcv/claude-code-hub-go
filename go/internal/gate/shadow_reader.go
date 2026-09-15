package gate

import "io"

// ShadowReader 把读到的字节**旁路**喂给 ShadowObserver 后原样透传。
//
// 为什么需要它：Node 在 shadow 模式下不跑门控（`resolveStreamGateMode() === "enforce"`
// 之外不门控，见 forwarder.ts:2034 / 5334），但仍在响应处理阶段挂一个旁路观察者
// （response-handler.ts:3922、5372），用来量「首非空字节 vs 首有效内容」的判定分歧，
// 为 enforce 灰度提供误判率。本类型就是那条旁路的最小实现：
//
//   - 不改字节、不缓冲、不阻断：Read 的返回值与源完全一致；
//   - 观测异常只终止观测（ShadowObserver.Observe 内部已兜住，见 observer.go）；
//   - 非并发安全：每个上游流一个实例，与上游正文的读侧同 goroutine。
type ShadowReader struct {
	source   io.Reader
	observer *ShadowObserver
}

// NewShadowReader 构造旁路读取器。config 的家族必须已解析（调用方在家族未知时不要构造）。
func NewShadowReader(source io.Reader, config ShadowConfig) *ShadowReader {
	return &ShadowReader{source: source, observer: NewShadowObserver(config)}
}

// Read 读一段字节，先喂观测器再返回给调用方。
func (r *ShadowReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if n > 0 {
		r.observer.Observe(p[:n])
	}
	return n, err
}

// Close 把关闭动作透传给源（源不可关闭时为空操作）：包装层不得改变上游正文的归属，
// 否则调用方（pump 与竞速的输家清理）会漏关或双关。
func (r *ShadowReader) Close() error {
	if closer, ok := r.source.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
