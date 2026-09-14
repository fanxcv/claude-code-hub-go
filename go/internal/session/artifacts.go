package session

import (
	"context"
	"encoding/json"
	"strconv"
	"time"
)

// 本文件是会话**请求工件写侧**：复刻 SessionManager 里与请求侧调试工件有关的三条写入
// （storeSessionRequestBody / storeSessionClientRequestMeta / storeSessionMessages）。
//
// 三条工件都只在**会话详情调试**路径上用，因此都受同一组闸门约束（Node 同序）：
//
//  1. 高并发模式关闭工件：pctx.ShouldPersistDebugArtifacts（对应 TS 的
//     shouldPersistSessionDebugArtifacts = !highConcurrencyModeEnabled）。判定由调用方做，
//     本文件只承接「已经允许」的写入。
//  2. 单件大小上限 SESSION_REQUEST_ARTIFACT_MAX_BYTES（默认 5 MiB，低于下限 64 KiB 时取默认）。
//     超限**删键**而不是跳过——Node 同样 `del`：上一轮同名序号留下的旧工件必须失效，
//     否则读侧会读到这一次的请求配上一次的正文。
//  3. 消息正文开关 STORE_SESSION_MESSAGES：关闭时落盘的是脱敏副本（内容字段换 [REDACTED]）。
//
// 写入必须**有界**：工件是整份正文的序列化副本，是每流内存的主要放大器，故三条写入都在
// 调用方按上限预检后才走到这里（见 dataplane 的 requestArtifactAllowed）。

// redactedMarker 与 TS 侧的 REDACTED_MARKER 逐字一致。
const redactedMarker = "[REDACTED]"

// SessionArtifactOptions 是请求工件写入参数。
type SessionArtifactOptions struct {
	// StoreMessages 是 STORE_SESSION_MESSAGES：为真时原样落盘，为假时落脱敏副本。
	StoreMessages bool
	// MaxBytes 是单件上限 SESSION_REQUEST_ARTIFACT_MAX_BYTES；<=0 时取默认 5 MiB。
	MaxBytes int
	// StoreResponseBody 是 STORE_SESSION_RESPONSE_BODY：响应正文的**总开关**。
	//
	// 与请求侧三条工件的区别：请求侧的闸门是高并发模式（由调用方在组装事实时判掉）；
	// 响应正文另有一道独立开关，因为它是唯一一笔「把上游正文留在服务端」的写入——
	// 运维可能愿意存调试工件但不愿存模型回答原文。
	StoreResponseBody bool
}

// defaultSessionRequestArtifactMaxBytes 与 TS 侧的
// DEFAULT_SESSION_REQUEST_ARTIFACT_MAX_BYTES 一致（5 MiB）。
const defaultSessionRequestArtifactMaxBytes = 5 * 1024 * 1024

// ArtifactMaxBytes 给出本次装配生效的上限。
func (o SessionArtifactOptions) ArtifactMaxBytes() int {
	if o.MaxBytes > 0 {
		return o.MaxBytes
	}
	return defaultSessionRequestArtifactMaxBytes
}

// StoreSessionRequestBody 复刻 storeSessionRequestBody（session-manager.ts:2560）。
//
// 键：session:{id}:req:{seq}:requestBody，TTL 取会话 TTL；seq 缺省按 1（Node 的
// normalizeRequestSequence ?? 1）。
func (b *Binder) StoreSessionRequestBody(
	ctx context.Context, sessionID string, body any, sequence int, options SessionArtifactOptions,
) error {
	if !b.Ready() || sessionID == "" {
		return nil
	}
	payload := body
	if !options.StoreMessages {
		payload = RedactSessionArtifact(body)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	key := RequestBodyKey(sessionID, sequence)
	if len(encoded) > options.ArtifactMaxBytes() {
		return b.client.rc.Raw().Del(ctx, key).Err()
	}
	return b.client.rc.Raw().Set(ctx, key, encoded, b.sessionTTL()).Err()
}

// StoreSessionMessages 复刻 storeSessionMessages（session-manager.ts:1938）。
//
// 键：session:{id}:req:{seq}:messages（无序号时退化为 session:{id}:messages）。
// 同样超限删键。
func (b *Binder) StoreSessionMessages(
	ctx context.Context, sessionID string, messages any, sequence int, options SessionArtifactOptions,
) error {
	if !b.Ready() || sessionID == "" {
		return nil
	}
	payload := messages
	if !options.StoreMessages {
		payload = RedactSessionArtifact(messages)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	key := MessagesSequenceKey(sessionID, sequence)
	if len(encoded) > options.ArtifactMaxBytes() {
		return b.client.rc.Raw().Del(ctx, key).Err()
	}
	return b.client.rc.Raw().Set(ctx, key, encoded, b.sessionTTL()).Err()
}

// RedactSessionArtifact 复刻 message-redaction.ts 的内容脱敏：**内容字段**换 [REDACTED]，
// 结构（键集、数组长度、角色与类型字段）原样保留。
//
// 为什么是「结构与 Node 两个入口（redactMessages / redactRequestBody）的并集」：Node 侧按
// 工件种类调不同入口，但两者覆盖的字段集合是同一批内容字段（messages[].content、system、
// prompt、contents[].parts、Gemini 的 inlineData.data、工具调用的 input/args）。这里用一个
// 递归遍历同时覆盖，避免两份几乎相同的实现各自漏字段——**登记**：单点实现是刻意的收敛，
// 若将来 Node 给某个入口单独新增字段，需要在 walkArtifactValue 里补上。
//
// 未覆盖的字段是刻意的：token 计数、模型名、metadata 之外的标量一律保留——脱敏的对象是
// 用户正文，不是调试元数据。
func RedactSessionArtifact(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return redactArtifactObject(typed)
	case []any:
		return redactArtifactArray(typed)
	default:
		return value
	}
}

// redactArtifactObject 复制并脱敏一个对象的内容字段。
func redactArtifactObject(object map[string]any) map[string]any {
	result := make(map[string]any, len(object))
	for key, child := range object {
		result[key] = child
	}
	if _, ok := result["content"]; ok {
		result["content"] = redactContentValue(result["content"])
	}
	// messages：请求正文里最常见的那一个入口。每一条消息本身也是一个对象，
	// 递归下去正好命中它们的 content 字段。
	if _, ok := result["messages"]; ok {
		result["messages"] = RedactSessionArtifact(result["messages"])
	}
	if _, ok := result["system"]; ok {
		result["system"] = redactSystemValue(result["system"])
	}
	// prompt 与 contents：Gemini 与旧版 OpenAI 的正文入口。
	if _, ok := result["prompt"]; ok {
		result["prompt"] = redactContentValue(result["prompt"])
	}
	if _, ok := result["contents"]; ok {
		result["contents"] = RedactSessionArtifact(result["contents"])
	}
	return result
}

// redactArtifactArray 逐个脱敏数组元素。
func redactArtifactArray(items []any) []any {
	result := make([]any, len(items))
	for index, item := range items {
		result[index] = RedactSessionArtifact(item)
	}
	return result
}

// redactContentValue 复刻 redactMessageContent 里对 content 字段的处理。
//
// content 可能是字符串、内容块数组，或（tool_result 里）再嵌一层的数组。
func redactContentValue(content any) any {
	switch typed := content.(type) {
	case string:
		return redactedMarker
	case []any:
		result := make([]any, len(typed))
		for index, block := range typed {
			result[index] = redactContentBlock(block)
		}
		return result
	default:
		return content
	}
}

// redactContentBlock 脱敏单个内容块（text / image source.data / tool_use.input /
// tool_result.content）。
func redactContentBlock(block any) any {
	object, ok := block.(map[string]any)
	if !ok {
		if _, isString := block.(string); isString {
			return redactedMarker
		}
		return block
	}
	result := make(map[string]any, len(object))
	for key, child := range object {
		result[key] = child
	}
	if _, hasText := result["text"]; hasText {
		if _, isString := result["text"].(string); isString {
			result["text"] = redactedMarker
		}
	}
	if source, ok := result["source"].(map[string]any); ok {
		if _, hasData := source["data"]; hasData {
			copied := make(map[string]any, len(source))
			for key, child := range source {
				copied[key] = child
			}
			copied["data"] = redactedMarker
			result["source"] = copied
		}
	}
	if _, hasInput := result["input"]; hasInput {
		result["input"] = redactedMarker
	}
	if _, hasContent := result["content"]; hasContent {
		result["content"] = redactContentValue(result["content"])
	}
	// Gemini：parts[].text / inlineData.data / functionCall.args。
	if _, hasParts := result["parts"]; hasParts {
		result["parts"] = RedactSessionArtifact(result["parts"])
	}
	if inline, ok := result["inlineData"].(map[string]any); ok {
		if _, hasData := inline["data"]; hasData {
			copied := make(map[string]any, len(inline))
			for key, child := range inline {
				copied[key] = child
			}
			copied["data"] = redactedMarker
			result["inlineData"] = copied
		}
	}
	if call, ok := result["functionCall"].(map[string]any); ok {
		if _, hasArgs := call["args"]; hasArgs {
			copied := make(map[string]any, len(call))
			for key, child := range call {
				copied[key] = child
			}
			copied["args"] = redactedMarker
			result["functionCall"] = copied
		}
	}
	return result
}

// redactSystemValue 复刻 redactSystemPrompt：字符串或块数组，两者都只换内容。
func redactSystemValue(system any) any {
	switch typed := system.(type) {
	case string:
		return redactedMarker
	case []any:
		result := make([]any, len(typed))
		for index, block := range typed {
			if object, ok := block.(map[string]any); ok {
				if _, hasText := object["text"]; hasText {
					copied := make(map[string]any, len(object))
					for key, child := range object {
						copied[key] = child
					}
					copied["text"] = redactedMarker
					result[index] = copied
					continue
				}
			}
			result[index] = block
		}
		return result
	default:
		return system
	}
}

// RequestBodyKey 是请求正文工件键：session:{id}:req:{seq}:requestBody。
func RequestBodyKey(sessionID string, sequence int) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(normalizeArtifactSequence(sequence)) +
		":requestBody"
}

// MessagesSequenceKey 是按序号的 messages 键：session:{id}:req:{seq}:messages。
func MessagesSequenceKey(sessionID string, sequence int) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(normalizeArtifactSequence(sequence)) +
		":messages"
}

// ClientRequestMetaKey 是客户端请求元信息键：session:{id}:req:{seq}:clientReqMeta。
//
// **本波不写**（键名先落地，供详情面波次接手）：Node 的 storeSessionClientRequestMeta 先过
// sanitizeUrl（对 key/api_key/token 等查询参数换成 [REDACTED]），而 Go 侧还没有等价的
// sanitizeUrl——JS 的 encodeURIComponent 与 Go 的 url.QueryEscape 对空格、`+`、`!'()*`
// 的编码不同，矇一个近似实现只会把「泄露查询串里的密钥」变成一段静默的差异。
func ClientRequestMetaKey(sessionID string, sequence int) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(normalizeArtifactSequence(sequence)) +
		":clientRequestMeta"
}

// normalizeArtifactSequence 复刻 normalizeRequestSequence：非正数一律按 1。
func normalizeArtifactSequence(sequence int) int {
	if sequence <= 0 {
		return 1
	}
	return sequence
}

// ArtifactWriteTimeout 是工件写入的超时上限。
//
// 为什么单列：工件写入发生在热路径上（守卫链刚通过），Redis 卡住时绝不能把请求一起卡住。
// 超时后调用方只记 warn，请求照常转发。
const ArtifactWriteTimeout = 2 * time.Second
