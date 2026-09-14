package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/usagefeed"
)

// 本文件实现使用记录页「推送模式」的服务端：`GET /usage-logs/stream` 的 **SSE**。
//
// 契约（冻结，前端按此对接，不得单方面改动）：
//
//	GET /usage-logs/stream
//	Content-Type: text/event-stream; charset=utf-8
//	Cache-Control: no-cache
//	Connection: keep-alive
//
//	event: ready
//	data: {}
//
//	event: new-rows
//	data: {"maxId": <int>, "minId": <int>, "count": <int>}
//
//	: keep-alive                     （每 15s 一行注释，防中间设备按空闲断连）
//
// 这是 **C2 信号式**推送：只广播「有新行落库」这一个事实，**绝不**把行内容（用户、密钥、
// 正文、金额）塞进事件。前端收到 `new-rows` 后再走增量接口取那几行
// （`GET /usage-logs?sinceId=<minId-1>&asc=true`，见 usage_logs_query.go 的 sinceId 分支）。
//
// `minId` 与 `maxId` 同出而分工不同：`maxId` 是常规高水位（`sinceId = maxId` 就够）；
// `minId` 是**同窗口下界**，专治「晚结算的低 id 行」——行 id 是开行顺序而非结算顺序，
// 一条流式请求开行 5 分钟后才结算时其 id 低于前端高水位，只靠 `maxId` 永远取不到它。
// 两者都是加性字段，老客户端忽略未知键。
//
// 为什么不做「推整行」：
//  1. 载荷量级不同（列表每页 284 KB），推流的成本与失败面都更大；
//  2. 行数据会**二次变化**（开行时无金额，终态结算后才带 token 与成本），推送快照会让前端
//     拿到中间态并把它误当终态；
//  3. 权限与脱敏在既有读端点上已经实现过一次，复用到第二个出口就是第二处要守的漏洞。
//
// 三条约束：
//
//  1. **每条连接一个订阅**，随连接建立、随连接结束注销（defer）。SSE 是长连接，整个连接期
//     占用一个处理 goroutine。
//  2. **未授权不进本文件**：路由走既有 AuthGuard（`AccessRead`），认证失败由守卫直接作答
//     401，不会建立长连接。前端据此**停止重连**。
//  3. **Redis 未装配也能建流**：此时只会收到 `ready` 与心跳。这是刻意的——前端应以「收到过
//     `new-rows`」判定推送是否真可用，不能靠连接成功冒充（见报告 §残余风险）。

// SSE 心跳与写超时。
const (
	// usageLogsStreamHeartbeat 是注释心跳间隔（契约里的 15s）。
	usageLogsStreamHeartbeat = 15 * time.Second
	// usageLogsStreamWriteTimeout 是单次写的时间上界。SSE 写会被客户端读速拖慢（TCP 背压），
	// 没有上界时一个「连得上但不读」的客户端能永久占住一个 goroutine 与一个订阅。
	usageLogsStreamWriteTimeout = 10 * time.Second
)

// usageLogsStreamHeartbeatInterval 是心跳的实际取值来源。
//
// 之所以用变量而不是直接用常量：心跳是「连接是否被中间设备活着」的唯一证据，必须有用例钉住，
// 而门禁纪律禁止测试等真实长超时（15s）。测试在自身作用域内把它改小再还原。
var usageLogsStreamHeartbeatInterval = usageLogsStreamHeartbeat

// RegisterUsageLogsStream 注册推送路由。
//
// 装配条件与其余「有额外依赖才注册」的路由一致：`Deps.NewRowsFeed` 为 nil（未装配推送面）
// 时**不注册**，请求原样回退 Node。与 fail-closed 同义——不能让 Go 用一个「连得上但永远
// 没有信号」的流冒充推送（那会让前端把推送当成可用，从而关掉轮询）。
func RegisterUsageLogsStream(router *Router, deps Deps) {
	if deps.NewRowsFeed == nil {
		logger := deps.Logger
		if logger != nil {
			logger.Warn("admin_usage_logs_stream_unwired", map[string]any{
				"module": "usage-logs",
				"route":  "/usage-logs/stream",
				"reason": "new_rows_feed_missing",
				"action": "route_not_registered",
			})
		}
		return
	}
	handler := &usageLogsStreamHandler{deps: deps}
	router.Add(Route{
		Method: http.MethodGet, Path: "/usage-logs/stream", Access: AccessRead,
		Module: "usage-logs", OperationID: "streamUsageLogs",
		Handler: http.HandlerFunc(handler.handle),
	})
}

type usageLogsStreamHandler struct {
	deps Deps
}

// handle 是一条 SSE 连接的生命周期。
func (h *usageLogsStreamHandler) handle(writer http.ResponseWriter, request *http.Request) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		// 只在非标准中间件包裹下才会发生。返回 500 而不是静默降级成一次性响应：
		// 后者会让前端以为流建立了却永远收不到事件。
		h.writeProblem(writer, request, "SSE 不受支持：响应写入器无法 flush")
		return
	}
	// ResponseController 提供写截止时间（Go 1.20+ 的原生能力）。不支持时不报错，只是没有
	// 写超时兜底——降级仍可用，故只记一条 debug。
	controller := http.NewResponseController(writer)

	// 契约要求的响应头。Cache-Control 覆盖 Router.applyEnvelopeHeaders 设的
	// `no-store, no-cache, must-revalidate`：长连接的语义以 no-cache 为准。
	header := writer.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	// nginx 一类反代默认缓冲响应体，会把 SSE 积成整块。显式关掉。
	header.Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := request.Context()
	subscription, dispose := h.deps.NewRowsFeed.Subscribe()

	// 一条连接一个泵：把订阅信号搬进本地通道，好让主循环同时等「信号」与「心跳」。
	pumpDone := make(chan struct{})
	signals := make(chan usagefeed.Signal, 4)
	go func() {
		defer close(pumpDone)
		for {
			signal, err := subscription.Wait(ctx)
			if err != nil {
				// ctx 取消或订阅关闭：两种都表示这条连接该收尾了。
				return
			}
			if signal.MaxID <= 0 && signal.Count <= 0 {
				// 空信号没有意义（前端收到也只会白拉一次）。真实的订阅面不会产生它，
				// 这里跳过是为了让任何非常规实现也不会把连接拖进空转。
				continue
			}
			select {
			case signals <- signal:
			case <-ctx.Done():
				return
			}
		}
	}()
	// defer 顺序（LIFO）：先注销订阅（让 Wait 立刻返回），再等泵退出，避免 goroutine 泄漏。
	defer func() {
		dispose()
		<-pumpDone
	}()

	// ready 立即发：前端据此判定「服务端已开始推」。
	if !h.writeSSE(writer, controller, flusher, "ready", "{}") {
		return
	}
	h.debug("admin_usage_logs_stream_opened", nil)
	defer h.debug("admin_usage_logs_stream_closed", nil)

	heartbeat := time.NewTicker(usageLogsStreamHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case signal, open := <-signals:
			if !open {
				return
			}
			payload, err := json.Marshal(signal)
			if err != nil {
				// 载荷是自己构造的两个整数，序列化失败只可能是编程错误；不中断连接。
				h.warn("admin_usage_logs_stream_encode_failed", map[string]any{"error": err.Error()})
				continue
			}
			if !h.writeSSE(writer, controller, flusher, "new-rows", string(payload)) {
				return
			}
		case <-heartbeat.C:
			// 注释行不触发前端事件，只用来占住连接。
			if !h.writeRaw(writer, controller, flusher, ": keep-alive\n\n") {
				return
			}
		}
	}
}

// writeSSE 写一条 SSE 事件；返回 false 表示连接已不可写（调用方应结束循环）。
func (h *usageLogsStreamHandler) writeSSE(
	writer http.ResponseWriter,
	controller *http.ResponseController,
	flusher http.Flusher,
	event string,
	data string,
) bool {
	return h.writeRaw(writer, controller, flusher, fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
}

func (h *usageLogsStreamHandler) writeRaw(
	writer http.ResponseWriter,
	controller *http.ResponseController,
	flusher http.Flusher,
	raw string,
) bool {
	// 写截止时间：客户端不读时 Write 会因背压长时间阻塞，这里给它一个上界。
	// 不支持该能力（ErrNotSupported）时忽略——降级为「无上界」而不是拒服务。
	_ = controller.SetWriteDeadline(time.Now().Add(usageLogsStreamWriteTimeout))

	if _, err := writer.Write([]byte(raw)); err != nil {
		h.debug("admin_usage_logs_stream_write_failed", map[string]any{"error": err.Error()})
		return false
	}
	flusher.Flush()
	return true
}

// writeProblem 用问题信封作答（复用 Deps.Problems；未装配时退化为纯文本 500）。
func (h *usageLogsStreamHandler) writeProblem(
	writer http.ResponseWriter,
	request *http.Request,
	detail string,
) {
	if h.deps.Problems != nil {
		h.deps.Problems.WriteProblem(writer, request, http.StatusInternalServerError, "", detail)
		return
	}
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(http.StatusInternalServerError)
	_, _ = writer.Write([]byte(detail))
}

func (h *usageLogsStreamHandler) debug(event string, fields map[string]any) {
	if h.deps.Logger == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{"module": "usage-logs"}
	}
	h.deps.Logger.Debug(event, fields)
}

func (h *usageLogsStreamHandler) warn(event string, fields map[string]any) {
	if h.deps.Logger == nil {
		return
	}
	h.deps.Logger.Warn(event, fields)
}
