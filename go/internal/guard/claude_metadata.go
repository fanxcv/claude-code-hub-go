package guard

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件是 Claude metadata.user_id 注入在守卫链里的落地（Node
// `session-guard.ts:214-232` 那一段）。判定与写入都在这里，构建语义在 internal/session
// （与同概念的解析函数 ParseClaudeMetadataUserID 同包）。
//
// 与 Node 的两处意译：
//  1. `session.originalFormat === "claude"` → 本仓按入口协议族映射出的客户端格式判定
//     （clientFormatOf，与选路的格式兼容判定同源）。
//  2. Node 直接改 session.request.message；Go 侧改的是正文访问器持有的那份 map + 写回，
//     两者最终都影响出站请求与落盘的请求工件。
//
// 另外，Node 的转发层还有一处按**供应商类型**触发的同名注入（forwarder.ts:3715-3732，带
// `claude_metadata_user_id_injection` 审计）。那一处属转发包，不在本文件。

// injectClaudeMetadata 按开关与请求形态决定是否注入 user_id，命中则改写并写回正文。
//
// 调用前提（调用方已判）：开关打开、非原始端点跨供应商回退、非预热抢答。
func (d Deps) injectClaudeMetadata(ctx *pctx.Context, keyID int64, sessionID string, body map[string]any) {
	if d.ClaudeMetadata == nil || sessionID == "" || body == nil {
		return
	}
	// 只作用于 Claude 线的请求正文，且排除 Codex 形态（Node 的两个附加门）。
	if clientFormatOf(ctx.ProtocolFrom()) != convert.FormatClaude {
		return
	}
	if hasCodexInputArray(body) {
		return
	}
	if !d.ClaudeMetadata(body, keyID, sessionID, userAgent(ctx)) {
		return
	}
	if err := d.storeBody(ctx, body); err != nil {
		// 写不回就不当真：Node 侧写回不可能失败（内存对象），Go 的正文访问器可能已封存。
		d.logger().Warn("guard.session.claude_metadata_body_store_failed", map[string]any{
			"error": err.Error(),
		})
		return
	}
	d.logger().Debug("guard.session.claude_metadata_injected", map[string]any{
		"sessionId": sessionID,
	})
}
