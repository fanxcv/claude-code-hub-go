package dataplane

import (
	"context"
	"net/http"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/responsefix"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// 本文件把响应修复器接进数据面的两条交付路径。Node 的位置是「上游响应到手、输出归一化
// 之前」（`response-fixer/index.ts` 的 `process()`），因此本层在**协议转换之前**动字节：
// 修复器看到的必须是上游线方言的字节，否则惰性 `chat.completion.chunk` 帧的过滤无从判定。

// ResponseFixWiring 是响应修复器的接线面。
type ResponseFixWiring struct {
	// AppendSpecialSettings 把响应侧审计条目追加到已建行的请求日志（终态之后的补写）。
	//
	// nil 表示未接线：正文照常修复，只是审计不落库——审计缺失比响应失败轻，故不阻断。
	AppendSpecialSettings func(ctx context.Context, requestID int64, entries []byte) error
}

// responseFixConfig 读一次设置快照。返回 ok=false 表示本次完全不动字节。
//
// 两条与 Node 同向的缺省：设置源未接线或读取失败时不动（宁可不修，也不拿猜测的配置改正文）；
// `enable_response_fixer=false` 即整体关闭。
func (h *Handler) responseFixConfig(ctx context.Context) (responsefix.Config, bool) {
	if h.options.Base.Settings == nil {
		return responsefix.Config{}, false
	}
	settings, err := h.options.Base.Settings.FindSystemSettings(ctx)
	if err != nil || settings == nil {
		return responsefix.Config{}, false
	}
	if !settings.EnableResponseFixer {
		return responsefix.Config{}, false
	}
	// `response_fixer_config` 是 jsonb：缺键取出厂值，显式 0/false 覆盖（Node 的展开语义）。
	return responsefix.ParseConfig(settings.ResponseFixerConfig), true
}

// fixNonStreamBody 修一段完整的非流式正文（对应 Node 的 processNonStream）。
//
// 未接线、开关关闭、正文仍带内容编码（压缩态）时原样返回：修压过的字节只会把它弄坏，
// Node 侧不存在这种状态是因为它的 fetch 自动解压，Go 保留上游编码透传，故必须在此显式跳过。
func (h *Handler) fixNonStreamBody(ctx context.Context, state *RequestState, headers http.Header, body []byte) []byte {
	if len(body) == 0 || hasOpaqueContentEncoding(headers) {
		return body
	}
	config, ok := h.responseFixConfig(ctx)
	if !ok {
		return body
	}
	fixed, audit := responsefix.ApplyNonStream(body, config)
	h.recordResponseFixAudit(ctx, state, audit)
	return fixed
}

// newResponseFixStream 构造流式修复器；nil 表示本次不修（未接线、开关关闭、非 SSE 或带内容编码）。
//
// 只对 `text/event-stream` 生效：修复器按 SSE 行边界工作，把别的流式正文（例如 NDJSON）
// 喂进来会既切错边界又白做修复。带内容编码（压缩态）时也跳过：修压过的字节只会把它弄坏，
// 与转换器的跳过条件同源。
func (h *Handler) newResponseFixStream(ctx context.Context, state *RequestState, headers http.Header) *responsefix.StreamFixer {
	if !hasEventStreamContentType(headers) || hasOpaqueContentEncoding(headers) {
		return nil
	}
	config, ok := h.responseFixConfig(ctx)
	if !ok {
		return nil
	}
	// 惰性帧过滤只对 OpenAI Responses 客户端生效（Node 的 `session.originalFormat === "response"`）。
	clientResponses := state != nil && state.Format == convert.FormatResponse
	return responsefix.NewStreamFixer(config, clientResponses)
}

// recordResponseFixAudit 写响应修复的审计条目（只在真的改过字节时）。
//
// 落点是 `message_request.special_settings` 的追加：终态已经写过了，这一步是补写，
// 与 Node 的 `updateMessageRequestDetails` 同一时机、同一目标列。
func (h *Handler) recordResponseFixAudit(ctx context.Context, state *RequestState, audit responsefix.Audit) {
	if !audit.Hit {
		return
	}
	if h.options.ResponseFix.AppendSpecialSettings == nil || state == nil || state.PC == nil {
		return
	}
	requestID, ok := state.PC.MessageRequestID()
	if !ok {
		return
	}
	entries := specialsettings.AppendEntries(audit.Entry())
	if len(entries) == 0 {
		return
	}
	if err := h.options.ResponseFix.AppendSpecialSettings(ctx, requestID, entries); err != nil {
		h.logger.Warn("dataplane.response_fix_audit_failed", map[string]any{
			"request_id": requestID,
			"error":      err.Error(),
		})
	}
}

// hasEventStreamContentType 判定 SSE 内容类型（Node 用 `includes("text/event-stream")`，
// 大小写不敏感：上游会把 Content-Type 写成 `Text/Event-Stream`）。
func hasEventStreamContentType(headers http.Header) bool {
	if headers == nil {
		return false
	}
	return strings.Contains(strings.ToLower(headers.Get("Content-Type")), "text/event-stream")
}
