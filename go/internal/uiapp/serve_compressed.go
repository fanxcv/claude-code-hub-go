package uiapp

import (
	"bytes"
	"net/http"

	"github.com/andybalholm/brotli"
)

// 本文件是「压缩态产物的应答面」：Accept-Encoding 协商、明文还原、以及壳的即时压缩。
//
// **ETag 口径**（本文件最容易被改错的一处）：ETag 取**存储态**字节的摘要——未压缩产物下
// 即明文，压缩产物下即 brotli 流；壳另按其**注入后**的明文算（既有行为，见 uiapp.go）。
//
// 为什么不按「明文」统一口径：那需要在装配时把 607 个压缩产物全解压一遍只为算摘要
// （一次性 0.3s 量级 + 一份瞬时内存尖峰），而收益为零——同一 URL 的两种表示（br / identity）
// 本就由 `Vary: Accept-Encoding` 分开缓存，这是 Vary 的标准用途；而两种表示的 ETag 是否相同，
// 对客户端缓存与 304 语义都没有影响（客户端只需在自己那份表示上协商）。
//
// 304 的正确性也不受影响：存储态字节不变 ⇒ ETag 不变 ⇒ 该 ETag 对应的明文（或注入后明文）
// 也逐字节不变（brotli 解压是确定性的），故命中 304 时客户端手上那份一定就是当前内容。

const (
	// shellRecompressQuality 是壳在「注入后即时压缩」时用的质量。
	//
	// 取 5 而不是装配期的 9：壳是每请求可变的（要注入会话与元数据），压缩在热路径上，
	// 实测 306 KiB 的壳 q9 需 16.6ms、q5 仅 4.8ms，而体积只差 4%（72 -> 75 KiB）。
	// 这里是「响应延迟 vs 流量」的取舍，取延迟。
	shellRecompressQuality = 5
)

// plainBody 返回产物的明文（存储态为 br 时先解压）。
//
// 它是「注入/压缩前必须先还原」这条顺序约束的唯一入口：壳的注入与即时压缩、
// 以及非 br 客户端的回退，都走这里。
func (h *Handler) plainBody(file asset) ([]byte, error) {
	if file.encoding != encodingBrotli {
		return file.body, nil
	}
	return decodeBrotli(file.body, file.rawSize)
}

// needsVary 判断本次响应是否需要 `Vary: Accept-Encoding`。
//
// 只有「同一 URL 可能按协商返回不同字节」的资源才需要：压缩存储的产物，以及壳
// （壳即使存储态未压，服务时也会按协商即时压缩）。
// 明文存储的静态资源**不设** Vary：它们的响应不随 Accept-Encoding 变化，
// 加了只会白白降低中间缓存的命中率。
func needsVary(file asset) bool {
	return file.encoding == encodingBrotli || file.shell
}

// compressForClient 按协商把明文压成 brotli；客户端不接受时原样返回。
//
// 存在的理由只有一个：壳是每请求可变的（要注入会话快照），而它是页面入口、体积最大，
// 最值得压缩。压缩失败一律退回明文——响应仍然正确，只是大一些，绝不因此让页面失败。
func compressForClient(request *http.Request, body []byte) (out []byte, contentEncoding string) {
	if !acceptsBrotli(request.Header.Get("Accept-Encoding")) {
		return body, ""
	}
	var buffer bytes.Buffer
	writer := brotli.NewWriterLevel(&buffer, shellRecompressQuality)
	if _, err := writer.Write(body); err != nil {
		return body, ""
	}
	if err := writer.Close(); err != nil {
		return body, ""
	}
	return buffer.Bytes(), "br"
}
