package session

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// 本文件是会话**响应侧工件写侧**：复刻 SessionManager 里与响应有关的几条写入
// （storeSessionResponse / storeSessionRequestHeaders / storeSessionResponseHeaders /
// storeSessionUpstreamRequestMeta / storeSessionUpstreamResponseMeta /
// storeSessionRequestPhaseSnapshot / storeSessionResponsePhaseSnapshot）。
//
// 与请求侧（artifacts.go）的分工：请求侧落「客户端发来的东西」，本文件落「上游与回给客户端的
// 东西」。两者合成详情页的九类事实，缺任何一类都会让详情页静默变成半个页。
//
// 三条与 Node 同源的闸门（照抄，不自行发明）：
//
//  1. 高并发模式下全部不写（pctx.ShouldPersistDebugArtifacts），判定由调用方做。
//  2. STORE_MESSAGES 控制**脱敏**而不是「写不写」：为假时正文换 [REDACTED] 副本、结构保留。
//  3. STORE_SESSION_RESPONSE_BODY 是响应正**文**的总开关（其余工件不受它管）。
//
// 与 Node 的两处**登记差异**：
//
//  1. **响应正文的裁剪方式**。Node 的 storeSessionResponse 在正文超过
//     SESSION_RESPONSE_BODY_MAX_BYTES 时**删键**（详情页看不到正文）；本实现改为头尾窗口 +
//     内联截断标记（见 capture.go 文件头）。故本实现在任何尺寸下都落一份正文，
//     SESSION_RESPONSE_BODY_MAX_BYTES 不参与判定。
//  2. **相位快照的单件上限**。Node 对快照的每个字段用**请求工件上限**（默认 5 MiB）判定，
//     超限删掉该字段的键；本实现按协调者定档用 32 KiB 的单件上限。理由：相位快照是
//     「每个请求 4 份 × 4 字段」的乘数项，5 MiB 的上限会让一屏详情页背后的驻留量失去意义；
//     截断的行为差异同样落在「能看到大部分 + 明确标记」而非「字段消失」。

// SessionDetailPhaseSnapshot 是一份相位快照（kind 决定它是请求侧还是响应侧）。
//
// 字段与 Node 的 SessionDetailRequestSnapshot / SessionDetailResponseSnapshot 逐字对应；
// 请求侧多 messages，响应侧没有（Node 的响应快照只有 body/headers/meta）。
type SessionDetailPhaseSnapshot struct {
	// Body 是正文（请求侧是对象，响应侧是字符串）。
	Body any
	// Messages 只有请求侧有。
	Messages any
	// HasMessages 区分「没有 messages 字段」与「messages 为 null」。
	HasMessages bool
	// Headers 是脱敏后的头表。
	Headers map[string]string
	// Meta 是该侧的元信息（请求侧 url/method，响应侧 url/statusCode）。
	Meta SessionDetailPhaseMeta
}

// SessionDetailPhaseMeta 是相位快照的 meta 字段的并集。
//
// 请求侧与响应侧在 Node 里是两个类型（SessionDetailRequestMeta / SessionDetailResponseMeta），
// 但落盘的键名不重叠（clientUrl/upstreamUrl/method vs upstreamUrl/statusCode），故一个结构
// 用 omitempty 表达，读侧按 kind 取需要的字段。upstreamUrl 是共有字段。
type SessionDetailPhaseMeta struct {
	ClientURL   *string `json:"clientUrl,omitempty"`
	UpstreamURL *string `json:"upstreamUrl,omitempty"`
	Method      *string `json:"method,omitempty"`
	StatusCode  *int    `json:"statusCode,omitempty"`
}

// PhaseSnapshotOptions 是相位快照的写入参数。
type PhaseSnapshotOptions struct {
	// StoreMessages 为假时正文/消息换脱敏副本。
	StoreMessages bool
	// MaxBytes 是**单字段**上限；<=0 时取 defaultPhaseSnapshotFieldMaxBytes。
	MaxBytes int
}

// defaultPhaseSnapshotFieldMaxBytes 是相位快照单字段的默认上限（协调者定档 32 KiB）。
const defaultPhaseSnapshotFieldMaxBytes = 32 * 1024

// PhaseSnapshotFieldMaxBytes 给出本次装配生效的单字段上限。
func (o PhaseSnapshotOptions) PhaseSnapshotFieldMaxBytes() int {
	if o.MaxBytes > 0 {
		return o.MaxBytes
	}
	return defaultPhaseSnapshotFieldMaxBytes
}

// SessionResponseKey 是响应正文工件键：session:{id}:req:{seq}:response。
func SessionResponseKey(sessionID string, sequence int) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(normalizeArtifactSequence(sequence)) +
		":response"
}

// SessionRequestHeadersKey 是客户端请求头工件键：session:{id}:req:{seq}:reqHeaders。
func SessionRequestHeadersKey(sessionID string, sequence int) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(normalizeArtifactSequence(sequence)) +
		":reqHeaders"
}

// SessionResponseHeadersKey 是上游响应头工件键：session:{id}:req:{seq}:resHeaders。
func SessionResponseHeadersKey(sessionID string, sequence int) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(normalizeArtifactSequence(sequence)) +
		":resHeaders"
}

// SessionUpstreamRequestMetaKey 是上游请求元信息键：session:{id}:req:{seq}:upstreamReqMeta。
func SessionUpstreamRequestMetaKey(sessionID string, sequence int) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(normalizeArtifactSequence(sequence)) +
		":upstreamReqMeta"
}

// SessionUpstreamResponseMetaKey 是上游响应元信息键：session:{id}:req:{seq}:upstreamResMeta。
func SessionUpstreamResponseMetaKey(sessionID string, sequence int) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(normalizeArtifactSequence(sequence)) +
		":upstreamResMeta"
}

// SessionDetailSnapshotKey 是相位快照的字段键：
// session:{id}:req:{seq}:snapshot:{kind}:{phase}:{field}。
//
// kind 取值 "request" | "response"（Node 的 buildSessionDetailSnapshotKey 参数），phase 取值
// "before" | "after"，field 取值 "body" | "messages" | "headers" | "meta"。四者的组合是
// **独立键**（Node 的渐进迁移设计：只新增相位键，不替换旧的混合键）。
func SessionDetailSnapshotKey(sessionID string, sequence int, kind, phase, field string) string {
	return "session:" + sessionID + ":req:" + strconv.Itoa(normalizeArtifactSequence(sequence)) +
		":snapshot:" + kind + ":" + phase + ":" + field
}

// StoreSessionResponse 复刻 storeSessionResponse（session-manager.ts:2345-2410）。
//
// 入参是**已交付客户端**的正文（capture.go 的窗口结果）。三处照抄 Node：
//
//  1. STORE_SESSION_RESPONSE_BODY 为假时整体跳过（调用方判，这里再判一次以防漏网）。
//  2. 脱敏按 STORE_MESSAGES：为真原样；为假时若是 JSON 就脱敏后重新序列化，**非 JSON
//     （如 SSE 流）原样落盘**——Node 的 catch 分支。
//  3. 写正文成功后刷新所有者也在这里做（Node 在 storeSessionResponse 内 refresh）。
//
// 与 Node 的差异只有一处：超限行为（见文件头）。
func (b *Binder) StoreSessionResponse(
	ctx context.Context, sessionID string, sequence int, keyID int64, body []byte,
	options SessionArtifactOptions,
) error {
	if !b.Ready() || sessionID == "" || len(body) == 0 || !options.StoreResponseBody {
		return nil
	}
	payload := string(body)
	if !options.StoreMessages {
		if redacted, ok := redactJSONString(payload); ok {
			payload = redacted
		}
	}
	if err := b.StoreSessionRequestOwner(ctx, sessionID, sequence, keyID); err != nil {
		return err
	}
	return b.client.rc.Raw().Set(
		ctx, SessionResponseKey(sessionID, sequence), payload, b.sessionTTL(),
	).Err()
}

// SessionUpstreamRequestMeta 是上游请求元信息（Node 的 SessionRequestMeta）。
type SessionUpstreamRequestMeta struct {
	URL    string
	Method string
}

// SessionUpstreamResponseMeta 是上游响应元信息（Node 的 SessionResponseMeta）。
type SessionUpstreamResponseMeta struct {
	URL        string
	StatusCode int
}

// StoreSessionRequestHeaders 复刻 storeSessionRequestHeaders（:2831-2849）。
//
// **不刷新所有者**：Node 这条写入里没有 refreshSessionRequestOwner（只有响应侧的几条有）。
// 照抄这个不对称——刷新时刻会改变围栏的存活窗口，而围栏是安全边界。
func (b *Binder) StoreSessionRequestHeaders(
	ctx context.Context, sessionID string, sequence int, headers map[string]string,
) error {
	if !b.Ready() || sessionID == "" || headers == nil {
		return nil
	}
	encoded, err := json.Marshal(headers)
	if err != nil {
		return err
	}
	return b.client.rc.Raw().Set(
		ctx, SessionRequestHeadersKey(sessionID, sequence), encoded, b.sessionTTL(),
	).Err()
}

// StoreSessionResponseHeaders 复刻 storeSessionResponseHeaders（:2854-2872）：先刷所有者。
func (b *Binder) StoreSessionResponseHeaders(
	ctx context.Context, sessionID string, sequence int, keyID int64, headers map[string]string,
) error {
	if !b.Ready() || sessionID == "" || headers == nil {
		return nil
	}
	encoded, err := json.Marshal(headers)
	if err != nil {
		return err
	}
	if err := b.StoreSessionRequestOwner(ctx, sessionID, sequence, keyID); err != nil {
		return err
	}
	return b.client.rc.Raw().Set(
		ctx, SessionResponseHeadersKey(sessionID, sequence), encoded, b.sessionTTL(),
	).Err()
}

// StoreSessionUpstreamRequestMeta 复刻 storeSessionUpstreamRequestMeta（:2735-2757）：
// url 过 sanitizeUrl，**不刷所有者**（与请求头同理）。
func (b *Binder) StoreSessionUpstreamRequestMeta(
	ctx context.Context, sessionID string, sequence int, meta SessionUpstreamRequestMeta,
) error {
	if !b.Ready() || sessionID == "" {
		return nil
	}
	payload := struct {
		URL    string `json:"url"`
		Method string `json:"method"`
	}{URL: SanitizeURL(meta.URL), Method: meta.Method}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return b.client.rc.Raw().Set(
		ctx, SessionUpstreamRequestMetaKey(sessionID, sequence), encoded, b.sessionTTL(),
	).Err()
}

// StoreSessionUpstreamResponseMeta 复刻 storeSessionUpstreamResponseMeta（:2794-2818）：
// url 过 sanitizeUrl，**刷新所有者**。
func (b *Binder) StoreSessionUpstreamResponseMeta(
	ctx context.Context, sessionID string, sequence int, keyID int64,
	meta SessionUpstreamResponseMeta,
) error {
	if !b.Ready() || sessionID == "" {
		return nil
	}
	payload := struct {
		URL        string `json:"url"`
		StatusCode int    `json:"statusCode"`
	}{URL: SanitizeURL(meta.URL), StatusCode: meta.StatusCode}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := b.StoreSessionRequestOwner(ctx, sessionID, sequence, keyID); err != nil {
		return err
	}
	return b.client.rc.Raw().Set(
		ctx, SessionUpstreamResponseMetaKey(sessionID, sequence), encoded, b.sessionTTL(),
	).Err()
}

// StoreSessionPhaseSnapshot 复刻 storeSessionRequestPhaseSnapshot / storeSessionResponsePhaseSnapshot。
//
// 落四组键（body/messages/headers/meta），**逐字段**判定大小：超限的字段**删键**（与 Node 的
// `redis.del(bodyKey)` 同判——保留上一版会让读侧把上一次的正文配上这一次的元信息）。
// 四个字段都没内容时不写任何键（Node 同样只在「字段在入参里」时写）。
//
// 返回写的字段名（按 body/messages/headers/meta 顺序），供调用方记日志与测试断言。
func (b *Binder) StoreSessionPhaseSnapshot(
	ctx context.Context, sessionID string, sequence int, keyID int64,
	kind, phase string, snapshot SessionDetailPhaseSnapshot, options PhaseSnapshotOptions,
) []string {
	if !b.Ready() || sessionID == "" {
		return nil
	}
	maxBytes := options.PhaseSnapshotFieldMaxBytes()
	raw := b.client.rc.Raw()
	written := make([]string, 0, 4)

	// body：请求侧与响应侧都可能有。响应侧的 body 是字符串（原文），请求侧是对象。
	if snapshot.Body != nil {
		payload := snapshot.Body
		if !options.StoreMessages {
			payload = redactPhaseBody(kind, payload)
		}
		written = append(written, b.writePhaseField(
			ctx, raw, SessionDetailSnapshotKey(sessionID, sequence, kind, phase, "body"),
			payload, maxBytes,
		))
	}
	if snapshot.HasMessages {
		payload := snapshot.Messages
		if !options.StoreMessages {
			payload = RedactSessionArtifact(payload)
		}
		written = append(written, b.writePhaseField(
			ctx, raw, SessionDetailSnapshotKey(sessionID, sequence, kind, phase, "messages"),
			payload, maxBytes,
		))
	}
	if len(snapshot.Headers) > 0 {
		written = append(written, b.writePhaseField(
			ctx, raw, SessionDetailSnapshotKey(sessionID, sequence, kind, phase, "headers"),
			snapshot.Headers, maxBytes,
		))
	}
	if !zeroSessionDetailMeta(snapshot.Meta) {
		written = append(written, b.writePhaseField(
			ctx, raw, SessionDetailSnapshotKey(sessionID, sequence, kind, phase, "meta"),
			snapshot.Meta, maxBytes,
		))
	}
	// 所有者在写完第一份快照后刷新一次：Node 的相位写入器与响应体/头/元信息同族，都刷。
	if len(written) > 0 {
		_ = b.StoreSessionRequestOwner(ctx, sessionID, sequence, keyID)
	}
	return writePhaseFieldNames(written)
}

// writePhaseField 写一个快照字段：序列化超过上限时**删键**，否则 setex。
//
// 返回**完整键**（调用方靠写PhaseFieldNames 一次缩写；在两层里各缩一次会把
// 字段名再当键缩一遍，结果恒空——而调用方正是靠返回值判「有没有写进去」）。
func (b *Binder) writePhaseField(
	ctx context.Context, raw redis.UniversalClient, key string, payload any, maxBytes int,
) string {
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > maxBytes {
		_ = raw.Del(ctx, key).Err()
		return ""
	}
	if err := raw.Set(ctx, key, encoded, b.sessionTTL()).Err(); err != nil {
		return ""
	}
	return key
}

// zeroSessionDetailMeta 判定一个 meta 是否「什么都没说」（四个指针全空）。
func zeroSessionDetailMeta(meta SessionDetailPhaseMeta) bool {
	return meta.ClientURL == nil && meta.UpstreamURL == nil &&
		meta.Method == nil && meta.StatusCode == nil
}

// writePhaseFieldNames 把写成功的键压成字段名（测试与日志可读）。
func writePhaseFieldNames(keys []string) []string {
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if index := strings.LastIndex(key, ":"); index != -1 {
			names = append(names, key[index+1:])
		}
	}
	return names
}

// redactPhaseBody 按 kind 决定快照正文的脱敏形态。
//
// 请求侧正文是对象（走 RedactSessionArtifact，与 requestBody 同族）；响应侧正文是**字符串**
// （原文，可能是 JSON 也可能是 SSE 文本），按 Node 的 redactResponseBody 语义：
// 能解成 JSON 就脱敏后重新序列化，解不动就原样保留。
func redactPhaseBody(kind string, body any) any {
	if kind == "response" {
		if text, ok := body.(string); ok {
			if redacted, parsed := redactJSONString(text); parsed {
				return redacted
			}
			return text
		}
		return RedactSessionArtifact(body)
	}
	return RedactSessionArtifact(body)
}

// redactJSONString 复刻 redactResponseBody 的字符串分支：能解析成 JSON 就脱敏并重新序列化。
//
// 返回 (结果, 是否为 JSON)：第二个值区分「脱敏了」与「原样返还」（Node 的 catch 分支）。
func redactJSONString(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value, false
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return value, false
	}
	encoded, err := json.Marshal(RedactSessionArtifact(decoded))
	if err != nil {
		return value, false
	}
	return string(encoded), true
}
