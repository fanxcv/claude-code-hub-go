package guard

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// providerStep 复刻 guard-pipeline.ts 的 provider 步骤（选路）。
//
// 选路实体逻辑属选路包；本步骤只做两件事：把上下文交给选路缝隙，把结果写回上下文槽位。
// 缝隙缺失时留一条 warn 并继续——过渡期 Go 侧尚无选路实现时，链条仍可用于比对前面各步。
func (d Deps) providerStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		if d.Provider == nil {
			d.logger().Warn("guard.provider.missing", map[string]any{
				"note": "选路缝隙未接线，本步骤跳过",
			})
			return nil, nil
		}
		selection, err := d.Provider.Select(d.runContext(ctx), ctx)
		if err != nil {
			return nil, err
		}
		ctx.SetProvider(selection)
		return nil, nil
	}
}

// messageContextStep 复刻 guard-pipeline.ts 的 messageContext 步骤。
//
// 只创建请求日志上下文（message_request 行），终态写入属终态结算包。
func (d Deps) messageContextStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		if d.MessageContext == nil {
			d.logger().Warn("guard.message_context.missing", map[string]any{
				"note": "请求日志上下文缝隙未接线，本步骤跳过",
			})
			return nil, nil
		}
		if err := d.MessageContext.EnsureContext(d.runContext(ctx), ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
}

// replayStep 复刻 guard-pipeline.ts 的 replayAttach 步骤。
//
// 位置是契约的一部分：它在 rateLimit 之前，重放命中完全免费；在 auth 与 sensitive 之后，
// 命中重放不能绕过鉴权与敏感词。缝隙为 nil 表示本波未接线，直接跳过。
func (d Deps) replayStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		if d.Replay == nil {
			return nil, nil
		}
		response, err := d.Replay.Attach(d.runContext(ctx), ctx)
		if err != nil {
			return nil, err
		}
		if response != nil {
			d.logger().Debug("guard.replay.hit", nil)
		}
		return response, nil
	}
}
