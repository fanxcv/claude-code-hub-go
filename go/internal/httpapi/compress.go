package httpapi

// 本文件是「动态响应的出站压缩」。
//
// 为什么需要它：静态资源之所以在传输时呈 br，是**构建期的副产品**——scripts/build-ui-embed.mjs
// 把文本产物逐文件 brotli 压缩后存进嵌入产物，运行时 internal/uiapp 直接把存储的压缩字节当响应体
// （见 internal/uiapp/serve_compressed.go）。而**运行期生成的响应此前完全不经压缩**：实测
// `/api/v1/usage-logs?limit=50` 单次下发 259~273 KB 且 `Content-Encoding` 为 identity
// 该页又是自动刷新的，等于反复支付这笔流量。
// 三条硬约束（都有实测依据，改动前先读）：
//
//  1. **流式响应绝不压缩**，两道互相独立的闸门：
//     ① `Content-Type` 是事件流（`text/event-stream`）一律放行；
//     ② **写出首个正文字节之前**收到 `Flush` 即放行——数据面的流式路径正是「WriteHeader 后
//     立刻 Flush 一次响应头」（internal/dataplane/stream.go），故它必然落在放行侧，
//     与它声明的 Content-Type 无关。
//     设两道是因为：任一条失效，另一条仍守住 SSE，而误压事件流的后果（逐事件可见性消失、
//     客户端把半截内容当完整回答）远大于漏压一个响应的代价。
//  2. **已有 `Content-Encoding` 的响应不得二次压缩**：uiapp 直出的 brotli 存储态、以及数据面
//     透传上游的压缩正文都属此类（后者连 `Content-Length` 都可能是压缩态的长度，
//     二次压缩必然损坏正文）。
//  3. **本包装器必须实现 `http.Flusher`**：internal/dataplane/stream.go 与
//     internal/adminapi/usage_logs_stream.go 都以 `writer.(http.Flusher)` 决定能否流式
//     （后者断言失败直接回 500「SSE 不受支持」——不是降级而是报错）。同时实现 `Unwrap`，
//     让 `http.NewResponseController` 仍能穿透本层取到 net/http 的写截止时间能力。

import (
	"compress/gzip"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
)

const (
	// compressMinBytes 是触发压缩的最小正文字节数。
	//
	// 低于它压缩没有收益：压缩流的块头与校验和本身就接近这个量级，小响应压完往往更大；
	// 而 net/http 对「未设 Content-Length 的小响应」会自动定长下发（缓冲 2 KiB 后补上长度），
	// 压了反而丢掉这个定长好处。
	compressMinBytes = 1024

	// compressBrotliQuality 取 5，与壳的即时压缩同档（internal/uiapp/serve_compressed.go）：
	// 那里实测同一正文 q9 需 16.6ms、q5 仅 4.8ms 而体积只差 4%，是「响应延迟 vs 流量」的取舍，
	// 取延迟。本层在**每一条**动态响应上，取舍同样偏向延迟。
	compressBrotliQuality = 5

	// compressGzipLevel 取 5：gzip 默认档（6）与它体积几乎相同，而这一档更快。
	compressGzipLevel = 5

	encodingBrotli = "br"
	encodingGzip   = "gzip"
)

// compressionEncodings 是服务端偏好的编码顺序：两者都被接受时优先 br（同档体积更小）。
var compressionEncodings = []string{encodingBrotli, encodingGzip}

// compressor 是「可按块写、可冲刷、可关闭」的压缩器。
//
// 按块写是刻意的：本层不缓冲整份正文（大响应 259 KB 起），压缩器流式消费写出的字节，
// 故每请求的额外内存驻留与正文大小无关。
type compressor interface {
	io.WriteCloser
	Flush() error
}

// 压缩器实例池：每条被压缩的响应都要一个压缩器，而 brotli 的窗口与哈希表不小（数十 KiB 起），
// 每次新建会让「省下来的流量」被 CPU 与分配吃掉。
var (
	brotliWriterPool = sync.Pool{New: func() any {
		return brotli.NewWriterLevel(io.Discard, compressBrotliQuality)
	}}
	gzipWriterPool = sync.Pool{New: func() any {
		writer, err := gzip.NewWriterLevel(io.Discard, compressGzipLevel)
		if err != nil {
			// 档位是常量，不可达；真到了这里也不能让一条响应路径 panic。
			return gzip.NewWriter(io.Discard)
		}
		return writer
	}}
)

// acquireCompressor 取一个已指向 dst 的压缩器，并返回归还函数（必须调用，否则退化成每次新建）。
func acquireCompressor(encoding string, dst io.Writer) (compressor, func()) {
	switch encoding {
	case encodingBrotli:
		writer := brotliWriterPool.Get().(*brotli.Writer)
		writer.Reset(dst)
		return writer, func() { brotliWriterPool.Put(writer) }
	case encodingGzip:
		writer := gzipWriterPool.Get().(*gzip.Writer)
		writer.Reset(dst)
		return writer, func() { gzipWriterPool.Put(writer) }
	}
	return nil, func() {}
}

// compressionState 是压缩包装器的三种状态。
type compressionState int

const (
	// compressionUndecided：响应头已收下但尚未下发给客户端，正文暂存在有界缓冲里。
	// 这是唯一「已收下正文却还没决定编码」的状态，也是流式闸门②的生效窗口。
	compressionUndecided compressionState = iota
	// compressionPassThrough：放行，逐字节直写，本层不再碰正文。
	compressionPassThrough
	// compressionCompressing：已下发 `Content-Encoding`，正文经压缩器写出。
	compressionCompressing
)

// compressResponseWriter 是压缩包装器。
type compressResponseWriter struct {
	writer  http.ResponseWriter
	request *http.Request

	state  compressionState
	status int
	// encoding 是协商出的编码；空串表示客户端不接受任何我们支持的编码。
	encoding string

	// decideBuffer 只在 compressionUndecided 期间持有正文字节，上限为 compressMinBytes
	// （达到即转 compressionCompressing），故它不随正文大小增长。
	decideBuffer []byte

	compressor compressor
	release    func()
	headerSent bool
}

// withCompression 包住整个 HTTP 处理器，按协商结果压缩动态响应。
//
// 客户端不接受任何我们支持的编码（含明确写 identity、或完全不发该头）时整条链上不做任何判断：
// 直接交给下层，连包装器都不建——这是绝大多数健康检查与老客户端的路径。
func (s *Server) withCompression(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		encoding := negotiateCompression(request.Header.Get("Accept-Encoding"))
		if encoding == "" {
			next.ServeHTTP(writer, request)
			return
		}
		wrapper := &compressResponseWriter{writer: writer, request: request, encoding: encoding}
		defer func() {
			if err := wrapper.Close(); err != nil {
				// 收尾失败只能在响应写完之后发生：状态码与响应头都已下发，改不了，只能留痕。
				s.logger.Debug("http_compress_close_failed", map[string]any{
					"path":  request.URL.Path,
					"error": err.Error(),
				})
			}
		}()
		next.ServeHTTP(wrapper, request)
	})
}

func (c *compressResponseWriter) Header() http.Header { return c.writer.Header() }

// Unwrap 让 http.NewResponseController 穿透本层。
//
// internal/adminapi/usage_logs_stream.go 用 ResponseController 给 SSE 连接设写截止时间；
// 没有 Unwrap 时它会停在包装器上、报「不支持」（那里容忍这种降级，但写超时兜底就静默没了）。
func (c *compressResponseWriter) Unwrap() http.ResponseWriter { return c.writer }

func (c *compressResponseWriter) WriteHeader(status int) {
	if c.status != 0 || c.headerSent {
		// 重复写头：net/http 忽略第二次，本层同样只认第一次（否则 status 会被后来的 500 覆盖，
		// 而压缩判断是在第一次写头时做的）。
		return
	}
	c.status = status
	if c.canCompress() {
		// 可压缩：响应头先扣在本层，下发时机取决于「首个正文字节」与「Flush」谁先到
		// （见 Write 与 Flush）——若先到的是 Flush，说明这是流式响应，须原样放行。
		return
	}
	c.state = compressionPassThrough
	c.sendHeader()
}

func (c *compressResponseWriter) Write(payload []byte) (int, error) {
	if c.status == 0 {
		// 处理程序没显式写头：与 net/http 的语义一致，等效为 200。
		c.WriteHeader(http.StatusOK)
	}
	switch c.state {
	case compressionPassThrough:
		return c.writer.Write(payload)
	case compressionCompressing:
		return c.compressor.Write(payload)
	}
	// 未决定：先攒够 compressMinBytes 再判。攒够即压缩——此后正文不可能再是流式，
	// 因为真正的流式路径在写出首个正文字节之前就已经 Flush 过（闸门②）。
	if len(c.decideBuffer) == 0 && len(payload) >= compressMinBytes {
		// 首个正文块就超过阈值（大响应的常见形状）：直接压缩，不必先把它抄进缓冲。
		if err := c.startCompressing(); err != nil {
			return 0, err
		}
		return c.compressor.Write(payload)
	}
	c.decideBuffer = append(c.decideBuffer, payload...)
	if len(c.decideBuffer) >= compressMinBytes {
		if err := c.startCompressing(); err != nil {
			return 0, err
		}
	}
	return len(payload), nil
}

// Flush 实现 http.Flusher（本层必须实现，见文件头约束③）。
func (c *compressResponseWriter) Flush() {
	if c.state == compressionUndecided {
		// 闸门②：首个正文字节都还没写就 Flush ⇒ 这是流式响应（数据面的流与两个 SSE 端点
		// 都是这个形状）。立刻放弃压缩、把已暂存的字节原样放行；此后本层只做透传。
		c.state = compressionPassThrough
		c.sendHeader()
		c.dumpDecideBuffer()
	}
	if c.state == compressionCompressing {
		// 压缩器内部有块缓冲：不冲刷它，客户端就要等到块满才见到数据。
		_ = c.compressor.Flush()
	}
	if flusher, ok := c.writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Close 收尾：处理程序返回后由 withCompression 调用。
//
// 未决定状态下它才做最终判断——正文已完整，此时「够大就压、不够就原样」。
func (c *compressResponseWriter) Close() error {
	switch c.state {
	case compressionUndecided:
		if len(c.decideBuffer) >= compressMinBytes {
			if err := c.startCompressing(); err != nil {
				return err
			}
			break
		}
		c.state = compressionPassThrough
		c.sendHeader()
		c.dumpDecideBuffer()
		return nil
	case compressionPassThrough:
		// 处理程序一个字都没写（如 204、HEAD）：头仍须下发。
		c.sendHeader()
		return nil
	}
	if c.compressor == nil {
		return nil
	}
	err := c.compressor.Close()
	if c.release != nil {
		c.release()
	}
	c.compressor, c.release = nil, nil
	return err
}

// startCompressing 下发带 Content-Encoding 的响应头并把已暂存的正文交给压缩器。
func (c *compressResponseWriter) startCompressing() error {
	header := c.writer.Header()
	header.Set("Content-Encoding", c.encoding)
	appendVary(header, "Accept-Encoding")
	// 压缩后长度不可预知：预置的 Content-Length 一律作废。留着它会让客户端按压缩前的长度
	// 截断（或等更多字节而挂住），这类故障表现为「响应不完整」而非报错，很难回溯。
	header.Del("Content-Length")
	c.sendHeader()
	c.state = compressionCompressing
	engine, release := acquireCompressor(c.encoding, c.writer)
	c.compressor, c.release = engine, release
	if len(c.decideBuffer) > 0 {
		buffered := c.decideBuffer
		c.decideBuffer = nil
		if _, err := c.compressor.Write(buffered); err != nil {
			return err
		}
	}
	return nil
}

// sendHeader 把响应头下发给下层（幂等）。
func (c *compressResponseWriter) sendHeader() {
	if c.headerSent {
		return
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	c.writer.WriteHeader(status)
	c.headerSent = true
}

// dumpDecideBuffer 把暂存的正文原样写出（放行路径专用）。
func (c *compressResponseWriter) dumpDecideBuffer() {
	if len(c.decideBuffer) == 0 {
		return
	}
	buffered := c.decideBuffer
	c.decideBuffer = nil
	_, _ = c.writer.Write(buffered)
}

// canCompress 是所有「不压」判据的集中处，在首次写头时评估一次。
func (c *compressResponseWriter) canCompress() bool {
	if c.encoding == "" {
		return false
	}
	// 无正文或语义上不允许改动的状态。
	if c.status < 200 || c.status == http.StatusNoContent || c.status == http.StatusNotModified {
		return false
	}
	if c.request != nil && c.request.Method == http.MethodHead {
		// HEAD 无正文可压；而它的 Content-Length 必须与 GET 一致，压了反而失真。
		return false
	}
	header := c.writer.Header()
	// 约束②：已有编码（uiapp 的 brotli 存储态、数据面透传的上游压缩正文）不得二次压缩。
	if header.Get("Content-Encoding") != "" {
		return false
	}
	// 分段响应：每段独立编码，压了会让客户端拼不出完整对象。
	if header.Get("Content-Range") != "" {
		return false
	}
	if hasNoTransform(header.Get("Cache-Control")) {
		return false
	}
	// 约束①：事件流不压（与 Flush 闸门互为备份）。
	return compressibleContentType(header.Get("Content-Type"))
}

// negotiateCompression 按 Accept-Encoding 选出要用的编码；返回空表示不压缩。
//
// 语义取 RFC 9110 §12.5.3 够用的子集：每个 token 的 q 缺省为 1，q<=0 视为拒绝，
// 显式列出的编码优先于通配 `*`。服务端偏好 br 优于 gzip——只要两者都被接受就选 br，
// 不追求「按 q 严格排序」：严格排序只会让 `gzip;q=1, br;q=0.5` 由 gzip 胜出，
// 而两者都合法，br 的体积始终更小。
func negotiateCompression(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	explicit := map[string]float64{}
	wildcard := -1.0
	for _, part := range strings.Split(header, ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		name, params, _ := strings.Cut(token, ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		quality := 1.0
		for _, param := range strings.Split(params, ";") {
			key, value, found := strings.Cut(param, "=")
			if !found || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				quality = parsed
			}
		}
		if name == "*" {
			wildcard = quality
			continue
		}
		explicit[name] = quality
	}
	for _, encoding := range compressionEncodings {
		if quality, found := explicit[encoding]; found {
			if quality > 0 {
				return encoding
			}
			continue
		}
		if wildcard > 0 {
			return encoding
		}
	}
	return ""
}

// compressibleContentType 用白名单判断正文类型可否压缩。
//
// 用白名单而不是黑名单：黑名单总会漏掉某种「已经压过的」类型，漏掉即把压缩态再压一遍
// （白费 CPU 且几乎不省流量）；白名单漏掉的最坏结果只是少压一个响应。
// 故图片、音视频、字体、zip/gzip、Office 文档这类「装入容器时就已压缩」的类型自然不在表内。
func compressibleContentType(raw string) bool {
	if raw == "" {
		// 无 Content-Type：net/http 会在首次写出时嗅探，此处无法预判它是不是二进制或事件流。
		// 保守放行。实测被压的大响应（管理面 JSON）都显式设了类型。
		return false
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(strings.Split(raw, ";")[0]))
	}
	switch {
	case mediaType == "text/event-stream", mediaType == "text/x-event-stream":
		// 事件流：即便上层漏了 Flush 闸门，这里也要挡住。
		return false
	case strings.HasPrefix(mediaType, "text/"):
		return true
	case mediaType == "application/json", strings.HasSuffix(mediaType, "+json"):
		return true
	case mediaType == "application/javascript", mediaType == "application/x-javascript",
		mediaType == "application/ecmascript":
		return true
	case mediaType == "application/xml", strings.HasSuffix(mediaType, "+xml"):
		return true
	case mediaType == "application/x-ndjson", mediaType == "application/ndjson":
		return true
	case mediaType == "application/x-www-form-urlencoded":
		return true
	}
	return false
}

// appendVary 把 token 追加进 Vary，已存在则不重复。
//
// 必须设 Vary：同一 URL 的正文随 Accept-Encoding 变，缺了它中间缓存（CDN/反代）会把压缩态
// 发给不接受该编码的客户端，表现为「页面全是乱码」。
func appendVary(header http.Header, token string) {
	for _, existing := range header.Values("Vary") {
		for _, item := range strings.Split(existing, ",") {
			if strings.EqualFold(strings.TrimSpace(item), token) {
				return
			}
		}
	}
	header.Add("Vary", token)
}

// hasNoTransform 判 Cache-Control 是否含 no-transform（RFC 9111：要求中间层不得改动正文编码）。
func hasNoTransform(cacheControl string) bool {
	for _, directive := range strings.Split(cacheControl, ",") {
		if strings.EqualFold(strings.TrimSpace(directive), "no-transform") {
			return true
		}
	}
	return false
}
