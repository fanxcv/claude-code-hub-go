package guard

import (
	"fmt"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// modelStep 复刻 ProxyModelGuard。
//
// 语义（只在用户配置了模型白名单时才生效）：白名单为空则完全跳过；白名单非空时模型必填，
// 且必须与某条模式大小写不敏感地精确相等。
func (d Deps) modelStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		user, ok, err := d.currentUser(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			// 无用户上下文：认证本应已失败，这里跳过（与 Node 一致）。
			return nil, nil
		}

		allowed := user.AllowedModels
		if len(allowed) == 0 {
			return nil, nil
		}

		requested := d.requestedModel(ctx)
		if strings.TrimSpace(requested) == "" {
			return BuildError(
				400,
				"Model not allowed. Model specification is required when model restrictions are configured.",
				"invalid_request_error",
			), nil
		}

		lowered := strings.ToLower(requested)
		for _, pattern := range allowed {
			if strings.ToLower(pattern) == lowered {
				return nil, nil
			}
		}

		return BuildError(
			400,
			fmt.Sprintf("Model not allowed. The requested model '%s' is not in the allowed list.", requested),
			"invalid_request_error",
		), nil
	}
}

// requestedModel 读取请求中的模型名。
//
// 模型名位于正文（body）；取不到时返回空串，由调用方按「模型必填」处理。Gemini CLI 的
// 包装格式把模型放在 request.model 下，这里一并覆盖。
func (d Deps) requestedModel(ctx *pctx.Context) string {
	body, err := d.body(ctx)
	if err != nil {
		return ""
	}
	if model, ok := body["model"].(string); ok {
		return model
	}
	if requestData, ok := body["request"].(map[string]any); ok {
		if model, ok := requestData["model"].(string); ok {
			return model
		}
	}
	return ""
}
