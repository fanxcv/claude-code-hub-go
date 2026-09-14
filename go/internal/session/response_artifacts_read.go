package session

import (
	"context"
	"encoding/json"
	"strings"
)

// 本文件是响应侧工件的**只读面**（详情页读它）。与 artifacts_read.go 的分工：那个读请求侧，
// 本文件读响应侧。
//
// 唯一真源：src/lib/session-manager.ts 的 getSessionRequestHeaders（:2883-2905）、
// getSessionResponseHeaders（:2907-2929）、getSessionUpstreamRequestMeta（:2761-2784）、
// getSessionUpstreamResponseMeta（:2800-2829）、getSessionResponse（:2920-2960）、
// getSessionRequestPhaseSnapshot（:3067-3110）与 getSessionResponsePhaseSnapshot（:3195-3240）。
//
// 读语义三处照抄 Node：
//
//  1. **序号非法即无工件**：normalizeRequestSequence 把非正数判成 null，Node 的每条读都直接
//     返回 null（不回退 legacy 键）。
//  2. **结构不合法即无工件**：Node 的每条读都做形状校验（meta 必须有 url/method 或
//     url/statusCode 且类型正确），不合格返回 null。这不是洁癖——详情页拿到半个 meta 会渲染出
//     undefined，宁缺勿错。
//  3. **故障与不存在同判**：Redis 故障、坏 JSON、形状不合格都折成「无此工件」。

// SessionResponseBody 读响应正文工件：session:{id}:req:{seq}:response。
//
// 返回 (正文, 是否存在)。正文是**裸字符串**（可能是 JSON 也可能是 SSE 文本、或带截断标记的
// 头尾窗口），故不解码成结构——与 Node 的 getSessionResponse 同判。
func (b *Binder) SessionResponseBody(
	ctx context.Context, sessionID string, sequence int,
) (string, bool) {
	if !b.Ready() || sessionID == "" || sequence <= 0 {
		return "", false
	}
	raw, err := b.client.rc.Raw().Get(ctx, SessionResponseKey(sessionID, sequence)).Result()
	if err != nil || raw == "" {
		return "", false
	}
	return raw, true
}

// SessionRequestBody 读请求正文工件（详情页的 legacy 回退入口）。
//
// 返回**已解出的 JSON 值**（对象/数组/标量）：详情页要把它原样塞进响应体的 requestBody 字段，
// 保持一层不透明的 JSON 比先解成固定结构再序列化更安全（不会丢掉未知字段）。
// 解不动时返回原文（Node 的 parseJsonStringOrNull 在调用侧处理这个分支）。
func (b *Binder) SessionRequestBody(
	ctx context.Context, sessionID string, sequence int,
) (any, bool) {
	if !b.Ready() || sessionID == "" || sequence <= 0 {
		return nil, false
	}
	raw, err := b.client.rc.Raw().Get(ctx, RequestBodyKey(sessionID, sequence)).Result()
	if err != nil || raw == "" {
		return nil, false
	}
	decoded, decodeErr := decodeJSONValue(raw)
	if decodeErr != nil {
		return raw, true
	}
	return decoded, true
}

// SessionRequestHeaders 读客户端请求头工件。
func (b *Binder) SessionRequestHeaders(
	ctx context.Context, sessionID string, sequence int,
) map[string]string {
	return b.readHeaderRecord(ctx, SessionRequestHeadersKey(sessionID, sequence), sequence, sessionID)
}

// SessionResponseHeaders 读上游响应头工件。
func (b *Binder) SessionResponseHeaders(
	ctx context.Context, sessionID string, sequence int,
) map[string]string {
	return b.readHeaderRecord(ctx, SessionResponseHeadersKey(sessionID, sequence), sequence, sessionID)
}

// readHeaderRecord 是两条头工件读的共用实现：形状不合法（非对象、值为非字符串）返回 nil。
func (b *Binder) readHeaderRecord(
	ctx context.Context, key string, sequence int, sessionID string,
) map[string]string {
	if !b.Ready() || sessionID == "" || sequence <= 0 {
		return nil
	}
	raw, err := b.client.rc.Raw().Get(ctx, key).Result()
	if err != nil {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil
	}
	// Node 的 parseHeaderRecord：值是字符串才收；**全部项都不是字符串时返回 null**
	// （`Object.keys(result).length > 0 ? result : null`）。
	headers := make(map[string]string, len(decoded))
	for name, value := range decoded {
		if text, ok := value.(string); ok {
			headers[name] = text
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

// SessionUpstreamRequestMetaRead 是读出的上游请求元信息。
type SessionUpstreamRequestMetaRead struct {
	URL    string
	Method string
}

// SessionUpstreamResponseMetaRead 是读出的上游响应元信息。
type SessionUpstreamResponseMetaRead struct {
	URL        string
	StatusCode int
}

// SessionUpstreamRequestMetaRead 读上游请求元信息工件；形状不合法返回 nil。
func (b *Binder) ReadSessionUpstreamRequestMeta(
	ctx context.Context, sessionID string, sequence int,
) *SessionUpstreamRequestMetaRead {
	if !b.Ready() || sessionID == "" || sequence <= 0 {
		return nil
	}
	raw, err := b.client.rc.Raw().Get(ctx, SessionUpstreamRequestMetaKey(sessionID, sequence)).Result()
	if err != nil {
		return nil
	}
	var decoded struct {
		URL    *string `json:"url"`
		Method *string `json:"method"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil
	}
	// Node 的形状校验：url 与 method 都必须是字符串，缺一即 null。
	if decoded.URL == nil || decoded.Method == nil {
		return nil
	}
	return &SessionUpstreamRequestMetaRead{URL: *decoded.URL, Method: *decoded.Method}
}

// ReadSessionUpstreamResponseMeta 读上游响应元信息工件；形状不合法返回 nil。
func (b *Binder) ReadSessionUpstreamResponseMeta(
	ctx context.Context, sessionID string, sequence int,
) *SessionUpstreamResponseMetaRead {
	if !b.Ready() || sessionID == "" || sequence <= 0 {
		return nil
	}
	raw, err := b.client.rc.Raw().Get(ctx, SessionUpstreamResponseMetaKey(sessionID, sequence)).Result()
	if err != nil {
		return nil
	}
	var decoded struct {
		URL        *string `json:"url"`
		StatusCode *int    `json:"statusCode"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil
	}
	if decoded.URL == nil || decoded.StatusCode == nil {
		return nil
	}
	return &SessionUpstreamResponseMetaRead{URL: *decoded.URL, StatusCode: *decoded.StatusCode}
}

// SessionDetailSnapshotRead 是读出的一份相位快照。
//
// 四个字段都带 present 语义：Node 在没有**任何**字段时返回 null（整份快照不存在），有字段时
// 缺的字段给 null。故 HasSnapshot 决定「快照存在与否」，字段决定「这一项有没有值」。
type SessionDetailSnapshotRead struct {
	Body        any
	HasBody     bool
	Messages    any
	HasMessages bool
	Headers     map[string]string
	Meta        SessionDetailPhaseMeta
}

// ReadSessionPhaseSnapshot 读一份相位快照（四个字段键合成一份）。
//
// kind 取值 "request" | "response"，phase 取值 "before" | "after"。
// 四键全空时返回 nil（Node 的 `if (bodyValue === null && ... ) return null`）。
func (b *Binder) ReadSessionPhaseSnapshot(
	ctx context.Context, sessionID string, sequence int, kind, phase string,
) *SessionDetailSnapshotRead {
	if !b.Ready() || sessionID == "" || sequence <= 0 {
		return nil
	}
	raw := b.client.rc.Raw()
	bodyValue, bodyErr := raw.Get(ctx, SessionDetailSnapshotKey(sessionID, sequence, kind, phase, "body")).Result()
	if bodyErr != nil {
		bodyValue = ""
	}
	messagesValue, messagesErr := raw.Get(ctx, SessionDetailSnapshotKey(sessionID, sequence, kind, phase, "messages")).Result()
	if messagesErr != nil {
		messagesValue = ""
	}
	headersValue, headersErr := raw.Get(ctx, SessionDetailSnapshotKey(sessionID, sequence, kind, phase, "headers")).Result()
	if headersErr != nil {
		headersValue = ""
	}
	metaValue, metaErr := raw.Get(ctx, SessionDetailSnapshotKey(sessionID, sequence, kind, phase, "meta")).Result()
	if metaErr != nil {
		metaValue = ""
	}
	if bodyValue == "" && messagesValue == "" && headersValue == "" && metaValue == "" {
		return nil
	}

	snapshot := &SessionDetailSnapshotRead{}
	if bodyValue != "" {
		if decoded, err := decodeJSONValue(bodyValue); err == nil {
			snapshot.Body = decoded
			snapshot.HasBody = true
		}
	}
	if messagesValue != "" {
		if decoded, err := decodeJSONValue(messagesValue); err == nil {
			snapshot.Messages = decoded
			snapshot.HasMessages = true
		}
	}
	if headersValue != "" {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(headersValue), &decoded); err == nil {
			headers := make(map[string]string, len(decoded))
			for name, value := range decoded {
				if text, ok := value.(string); ok {
					headers[name] = text
				}
			}
			if len(headers) > 0 {
				snapshot.Headers = headers
			}
		}
	}
	if metaValue != "" {
		var decoded struct {
			ClientURL   *string `json:"clientUrl"`
			UpstreamURL *string `json:"upstreamUrl"`
			Method      *string `json:"method"`
			StatusCode  *int    `json:"statusCode"`
		}
		if err := json.Unmarshal([]byte(metaValue), &decoded); err == nil {
			snapshot.Meta = SessionDetailPhaseMeta{
				ClientURL:   decoded.ClientURL,
				UpstreamURL: decoded.UpstreamURL,
				Method:      decoded.Method,
				StatusCode:  decoded.StatusCode,
			}
		}
	}
	return snapshot
}

// decodeJSONValue 解一个 JSON 值（保持数字为 json.Number，避免大整数在 float64 里失真）。
func decodeJSONValue(raw string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}
