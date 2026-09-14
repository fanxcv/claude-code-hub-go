package main

import (
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ws"
)

// 本文件把 `/v1/responses` 的 WebSocket 通道挂进进程入口。
//
// 历史：白名单时期升级请求会被判给 Node——升级请求的方法是 GET，而归属白名单里通常只写
// `POST /v1/responses`，于是这里要「以 POST 作为
// 等价事实再判一次归属」，否则 HTTP 与 WS 会分裂到两个后端。归属判定删除后，WS 与同路径 HTTP
// 必然同归本进程，那层判定连同它的白名单依赖一并消失。
//
// 现在这里只剩一件事：把边缘处理器包在**整个 HTTP 处理器**外层（`api.Handler()`），使边缘把
// 每一轮隧道打回这个 handler——与 Node 在私有 loopback 监听器上自打一次的形态同构。因此排空
// 闸门、在途计数与请求日志对 WS 轮次同样生效，不需要在 WS 上另建一套账。

// newWebSocketEdge 建 WebSocket 边缘处理器：非升级流量与其它路径由内部处理器原样处理
// （见 internal/ws 的 Edge.ServeHTTP），只有本包的 WS 路径上的升级请求才交给边缘。
func newWebSocketEdge(handler http.Handler, logger *logx.Logger) http.Handler {
	return ws.New(ws.Options{Inner: handler, Logger: logger})
}
