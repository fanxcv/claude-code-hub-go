package session

import (
	"context"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
)

// CodexSessionCompleterAdapter 把补全器接到守卫链的 CodexSessionCompleter 缝隙上。
//
// 为什么需要一层适配：守卫链的接口只能用 guard 包自有的类型（本包依赖 guard，反向不行），
// 而补全语义属于本包；这层只做类型搬运，不含判断。
type CodexSessionCompleterAdapter struct {
	completer *CodexCompleter
}

// NewCodexSessionCompleterAdapter 组装适配器。
func NewCodexSessionCompleterAdapter(opts CodexCompleterOptions) *CodexSessionCompleterAdapter {
	return &CodexSessionCompleterAdapter{completer: NewCodexCompleter(opts)}
}

// Complete 实现 guard.CodexSessionCompleter。
func (a *CodexSessionCompleterAdapter) Complete(
	ctx context.Context,
	request guard.CodexSessionCompletionRequest,
) (guard.CodexSessionCompletionResult, error) {
	completion := a.completer.Complete(ctx, CodexCompleteArgs{
		KeyID:     request.KeyID,
		Headers:   request.Headers,
		Body:      request.Body,
		UserAgent: request.UserAgent,
	})
	return guard.CodexSessionCompletionResult{
		Applied:               completion.Applied,
		Action:                completion.Action,
		Source:                completion.Source,
		SessionID:             completion.SessionID,
		SetBodyPromptCacheKey: completion.SetBodyPromptCacheKey,
		SetHeaderSessionID:    completion.SetHeaderSessionID,
		SetHeaderXSessionID:   completion.SetHeaderXSessionID,
	}, nil
}
