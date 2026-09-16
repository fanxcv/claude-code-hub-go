package guard

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件承载 Codex 会话标识补全在守卫链里的落地（Node `src/app/v1/_lib/proxy/session-guard.ts`
// 第 109-131 行的那一段）。补全语义本身在 internal/session，本处只做三件事：
// 判定是否该补、把补全结果写回请求（正文 + 请求头）、把审计事实存进上下文。

// hasCodexInputArray 报告正文是否是 Codex（Responses）形态：顶层 `input` 为数组。
//
// Node 同判据是 `Array.isArray(requestMessage.input)`——它既是「Codex 请求」的判据，
// 也是补全只作用于 Codex 请求的原因：CLI 之外的客户端用别的方式表达对话身份。
func hasCodexInputArray(body map[string]any) bool {
	if body == nil {
		return false
	}
	_, ok := body["input"].([]any)
	return ok
}

// completeCodexSession 执行一次补全并写回。
//
// fail-open：补全失败（Redis 故障、正文不可写）只留日志，绝不阻断请求——这一点与 Node 一致
// （session-completer 内部把 Redis 异常吞成「降级生成 UUID v7」，而写回失败在原实现里不可能发生，
// 故这里把它按同一语义处理：写不回就不记审计，也不假装补过）。
func (d Deps) completeCodexSession(ctx *pctx.Context, keyID int64, body map[string]any) {
	completion, err := d.CodexCompletion.Complete(d.runContext(ctx), CodexSessionCompletionRequest{
		KeyID:     keyID,
		Body:      body,
		Headers:   rawHeaders(ctx.Headers()),
		UserAgent: userAgent(ctx),
	})
	if err != nil {
		d.logger().Warn("guard.session.codex_completion_failed", map[string]any{"error": err.Error()})
		return
	}
	// Node 只在 `applied && action !== "none"` 时记审计：两侧齐全的请求不产生条目。
	if !completion.Applied || completion.Action == "none" {
		return
	}
	if completion.SetBodyPromptCacheKey {
		// 快照必须在写入之前：注入事实的判据是「客户端原文里到底有没有这个键」，
		// 而不是「值能不能归一成会话标识」。两者在「给了值但值非法」时相反：
		// 客户端的 `prompt_cache_key:"short"` 会被归一规则作废、值被网关改写，但那个字段
		// 确实是客户端声明的约束——跨线丢弃时该记一条损失（信息档），不该被当成网关注入而略过。
		// 值为 null 视同未给：它本来就没声明任何约束（见 pctx.AddGatewayInjectedBodyField）。
		provided, hasKey := body["prompt_cache_key"]
		clientProvided := hasKey && provided != nil
		body["prompt_cache_key"] = completion.SessionID
		if err := d.storeBody(ctx, body); err != nil {
			d.logger().Warn("guard.session.codex_completion_body_store_failed", map[string]any{
				"error": err.Error(),
				"note":  "正文写不回，本次补全整体放弃（不写头、不记审计）",
			})
			return
		}
		// 登记「这个键是网关注入的」：跨线转换时它会被当作客户端声明的约束记进损失台账，
		// 而客户端原文里从未出现这个字段（判据见 pctx.AddGatewayInjectedBodyField）。
		if !clientProvided {
			ctx.AddGatewayInjectedBodyField("prompt_cache_key")
		}
	}
	if completion.SetHeaderSessionID {
		ctx.SetHeader("session_id", completion.SessionID)
	}
	if completion.SetHeaderXSessionID {
		ctx.SetHeader("x-session-id", completion.SessionID)
	}
	ctx.SetCodexSessionCompletion(pctx.CodexSessionCompletion{
		Action:    completion.Action,
		Source:    completion.Source,
		SessionID: completion.SessionID,
	})
	d.logger().Debug("guard.session.codex_completed", map[string]any{
		"action":    completion.Action,
		"source":    completion.Source,
		"sessionId": completion.SessionID,
	})
}
