package guard

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件复刻 session 与 warmup 两个步骤。
//
// 两者的共同点是「都要先读系统设置」：会话身份是否允许跨供应商回退、预热请求是否由网关
// 抢答，都由 system_settings 决定，而这两个开关的默认值在迁移期可能还未接线，故所有失败
// 路径都按 Node 的 fail-open 语义处理并留痕。

// sessionStep 复刻 ProxySessionGuard.ensure 中与请求内状态有关的部分。
//
// 三件事：
//  1. 按系统设置与**端点策略**两因子定本次请求是否允许「原始端点跨供应商回退」，
//     并按高并发模式设置调试工件位。
//  2. 把会话绑定交给 SessionBinder（客户端 session id 提取、会话 id 分配与序号）。
//  3. 不做任何落库：请求日志上下文属 messageContext 步骤。
//
// 与 Node 的差异（有意）：Node 的 ensure 里还夹着 Codex session id 补全与 claude metadata
// 注入，两者都会改写正文；它们属于请求改写，随正文缝隙一起在接线波次落地，本步骤不重复实现。
func (d Deps) sessionStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		auth, hasAuth := authState(ctx)
		if !hasAuth || auth.KeyID == 0 {
			d.logger().Warn("guard.session.no_key", map[string]any{"note": "无密钥 id，跳过会话分配"})
			return nil, nil
		}

		allowRawSession := false
		// codexCompletionEnabled 的默认值取「关闭」：只有读到设置且该列为真才补全。
		// Node 的同处是 `?? true`（列默认 true），本仓读不到设置时一律按保守默认处理（见上方注释）。
		codexCompletionEnabled := false
		claudeMetadataEnabled := false
		warmupInterceptEnabled := false
		if d.Settings != nil {
			settings, err := d.Settings.FindSystemSettings(d.runContext(ctx))
			if err != nil {
				// 读不到设置就按关闭处理：这是 Node 侧「设置读取失败即用保守默认值」的等价语义。
				d.logger().Error("guard.session.settings_failed", map[string]any{"error": err.Error()})
			} else {
				// 两因子：设置开关 × 本端点是否属原始透传（Node session.ts:574-582）。
				// 只取设置会把闸门变成全局开关：生产该设置为 true 时 /v1/responses 永不补全。
				allowRawSession = settings.AllowNonConversationEndpointProviderFallback && d.EndpointRawPassthrough
				codexCompletionEnabled = settings.EnableCodexSessionIDCompletion
				claudeMetadataEnabled = settings.EnableClaudeMetadataUserIDInjection
				warmupInterceptEnabled = settings.InterceptAnthropicWarmupRequests
				// 高并发模式与调试工件位互为取反：调试工件是每流内存的主要放大器。
				ctx.SetPersistDebugArtifacts(!settings.EnableHighConcurrencyMode)
			}
		}

		// Codex 会话标识补全：Node 把它放在「提取 clientSessionId 之前」，好让随后的会话身份、
		// 亲和与限流都绑到补全后的稳定 id 上（session-guard.ts 的注释原话）。
		//
		// 与 Node 的差异（接线方式，不是语义）：Node 直接改 session.headers/request.message，
		// Go 侧改的是上下文（SetHeader + storeBody），两者最终都影响出站请求。
		// 另：Node 的 `!allowRawSession` 与「只对 Codex 请求（正文有 input 数组）生效」两个门与这里同判。
		//
		// allowRawSession 是**两因子**判定（Node session.ts:574-582）：
		// 系统设置 allowNonConversationEndpointProviderFallback × 端点策略。
		// 端点策略只对原始透传端点为真，故本字段为「非原始透传」（/v1/responses、/v1/messages 等）。
		body, _ := d.body(ctx)
		if codexCompletionEnabled && d.CodexCompletion != nil && !allowRawSession && hasCodexInputArray(body) {
			d.completeCodexSession(ctx, auth.KeyID, body)
		}

		if d.Sessions == nil {
			d.logger().Warn("guard.session.binder_missing", map[string]any{
				"note": "会话绑定缝隙未接线，本步骤跳过",
			})
			return nil, nil
		}

		result, err := d.Sessions.Ensure(d.runContext(ctx), SessionRequest{
			KeyID:           auth.KeyID,
			Body:            body,
			Headers:         rawHeaders(ctx.Headers()),
			UserAgent:       userAgent(ctx),
			AllowRawSession: allowRawSession,
		})
		if err != nil {
			return nil, err
		}

		d.logger().Debug("guard.session.bound", map[string]any{
			"sessionId": result.SessionID,
			"sequence":  result.Sequence,
		})
		// Claude metadata.user_id 注入：Node 在拿到会话 id **之后**注入（session-guard.ts:214-232），
		// 写进去的就是本次请求绑定的那个会话 id；它同时早于请求过滤器，故过滤器看到的是注入后的正文。
		warmupMaybeIntercepted := warmupInterceptEnabled && isWarmupRequest(ctx.Path(), body) &&
			auth.KeyID != 0 && auth.UserID != 0 && auth.APIKey != ""
		if claudeMetadataEnabled && !allowRawSession && !warmupMaybeIntercepted {
			d.injectClaudeMetadata(ctx, auth.KeyID, result.SessionID, body)
		}
		// 会话身份也交给本次请求的选路器：前缀亲和要按**本会话**判定是否闲置过久
		// （见 ProviderRouter.applyConversationIdle）。取的就是这里已解析出的身份，
		// 与落库列 session_id、交给 MessageWriter 的 SessionLookup 钩子同一值。
		if router, ok := d.Provider.(*ProviderRouter); ok {
			router.SetConversationSession(result.SessionID)
		}
		return nil, nil
	}
}

// warmupResponse 是 Anthropic warmup 的抢答体。
//
// 字段顺序与 Node 侧对象字面量一致（model、id、type、role、content、stop_reason、
// stop_sequence、usage），因为真实客户端会解析它并缓存响应形状。
type warmupResponse struct {
	Model        string          `json:"model"`
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Content      []warmupContent `json:"content"`
	StopReason   string          `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        warmupUsage     `json:"usage"`
}

type warmupContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type warmupUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// warmupStep 复刻 ProxyWarmupGuard.ensure。
//
// 位置是契约：它在 rateLimit 与 provider 之前早退，因此抢答不触发限流、不选路、不计费。
// 判定刻意严格（单条 user 消息、单个 text 块、内容为 warmup、cache_control 为 ephemeral），
// 避免把正常请求误当预热抢答。
func (d Deps) warmupStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		body, err := d.body(ctx)
		if err != nil {
			return nil, nil
		}
		if !isWarmupRequest(ctx.Path(), body) {
			return nil, nil
		}

		if d.Settings == nil {
			return nil, nil
		}
		settings, err := d.Settings.FindSystemSettings(d.runContext(ctx))
		if err != nil {
			d.logger().Error("guard.warmup.settings_failed", map[string]any{"error": err.Error()})
			return nil, nil
		}
		if !settings.InterceptAnthropicWarmupRequests {
			return nil, nil
		}

		auth, ok := authState(ctx)
		if !ok || auth.KeyID == 0 || auth.UserID == 0 || auth.APIKey == "" {
			return nil, nil
		}

		model := d.requestedModel(ctx)
		responseText, err := json.Marshal(buildWarmupPayload(model))
		if err != nil {
			return nil, err
		}

		headers := http.Header{}
		headers.Set("content-type", "application/json; charset=utf-8")

		if d.WarmupLog != nil {
			record := WarmupRecord{
				KeyID:         auth.KeyID,
				UserID:        auth.UserID,
				APIKey:        auth.APIKey,
				Model:         model,
				UserAgent:     userAgent(ctx),
				Endpoint:      ctx.Path(),
				MessagesCount: messagesCount(body),
				DurationMS:    durationMillis(ctx),
			}
			if err := d.WarmupLog.RecordWarmup(d.runContext(ctx), ctx, record); err != nil {
				// Node 侧写日志失败不影响响应。
				d.logger().Error("guard.warmup.log_failed", map[string]any{"error": err.Error()})
			}
		}

		d.logger().Debug("guard.warmup.intercepted", map[string]any{
			"userId":   auth.UserID,
			"endpoint": ctx.Path(),
		})

		d.storeWarmupArtifacts(ctx, auth.KeyID, string(responseText))

		return NewResponse(200, headers, responseText), nil
	}
}

// buildWarmupPayload 复刻 buildWarmupResponseBody：最小合法响应 + 随机消息 id。
//
// 模型为空时写 "unknown"，与 Node 一致。随机 id 让每个抢答响应互不相同，避免客户端把
// 重复 id 当缓存命中。
func buildWarmupPayload(model string) warmupResponse {
	if model == "" {
		model = "unknown"
	}
	var random [8]byte
	// crypto/rand.Read 在 Linux 上不会失败；万一失败就用全零，不阻断抢答。
	_, _ = rand.Read(random[:])
	return warmupResponse{
		Model:      model,
		ID:         "msg_cch_" + hex.EncodeToString(random[:]),
		Type:       "message",
		Role:       "assistant",
		Content:    []warmupContent{{Type: "text", Text: "I'm ready to help you."}},
		StopReason: "end_turn",
		Usage:      warmupUsage{},
	}
}

// isWarmupRequest 复刻 ProxySession.isWarmupRequest。
func isWarmupRequest(path string, body map[string]any) bool {
	if normalizeEndpointPath(path) != "/v1/messages" {
		return false
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 1 {
		return false
	}
	first, ok := messages[0].(map[string]any)
	if !ok {
		return false
	}
	if role, _ := first["role"].(string); role != "user" {
		return false
	}
	content, ok := first["content"].([]any)
	if !ok || len(content) != 1 {
		return false
	}
	block, ok := content[0].(map[string]any)
	if !ok {
		return false
	}
	if blockType, _ := block["type"].(string); blockType != "text" {
		return false
	}
	text, ok := block["text"].(string)
	if !ok || len(text) == 0 {
		return false
	}
	if strings.ToLower(strings.TrimSpace(text)) != "warmup" {
		return false
	}
	cacheControl, ok := block["cache_control"].(map[string]any)
	if !ok {
		return false
	}
	return cacheControl["type"] == "ephemeral"
}

// durationMillis 是请求已耗时（毫秒），供 warmup 日志的时长字段使用。
func durationMillis(ctx *pctx.Context) int64 {
	return time.Since(ctx.StartedAt()).Milliseconds()
}
