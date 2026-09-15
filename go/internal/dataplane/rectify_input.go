package dataplane

import (
	"sync"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/rectify"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// responsesInputBody 是 responses 路由的正文访问器包装：**首次取正文时**按下发开关归一 `input`
// （字符串/单对象 → 数组）。
//
// 为什么放在这里而不是转发层：Node 在 guard pipeline **之前**归一（proxy-handler.ts:113），
// 于是守卫链的过滤器、建行时的审计、以及后续的转换器看到的是**同一份已归一**的正文。
// 若放到转发层，过滤器与建行审计就已经按未归一的形状读过了（两处口径分叉）。
//
// 归一在 JSON() 里做而不是在装配时做：正文是延迟读取的（解压必须发生在鉴权之后），
// 装配期拿不到正文，而包装访问器天然等到「第一次有人要正文」那一刻。
type responsesInputBody struct {
	inner guard.BodyAccess
	once  sync.Once
	// enabled 是开关判定；nil 表示未接线（按 Node 默认开启处理）。
	enabled func() bool
	// onAudit 接收审计条目；nil 表示不记（此时仍会归一，只是没有审计留痕）。
	onAudit func(map[string]any)
	logger  *logx.Logger
}

// JSON 取正文并在首次调用时就地归一；归一失败只记日志，不阻断请求（与守卫链 fail-open 同）
func (b *responsesInputBody) JSON() (map[string]any, error) {
	body, err := b.inner.JSON()
	if err != nil {
		return nil, err
	}
	b.once.Do(func() { b.normalize(body) })
	return body, nil
}

// Store 把修改后的正文写回，语义与内层一致。
func (b *responsesInputBody) Store(body map[string]any) error {
	return b.inner.Store(body)
}

// normalize 执行一次归一：写回正文并按需产出审计条目。
func (b *responsesInputBody) normalize(body map[string]any) {
	if b.enabled != nil && !b.enabled() {
		return
	}
	fields, applied := rectify.NormalizeResponseInput(body)
	if !applied {
		return
	}
	if storeErr := b.inner.Store(body); storeErr != nil {
		b.logger.Error("dataplane.response_input_rectifier_store_failed", map[string]any{"error": storeErr.Error()})
		return
	}
	if b.onAudit != nil {
		b.onAudit(specialsettings.ProactiveRectifierEntry(specialsettings.TypeResponseInputRectifier, fields))
	}
	b.logger.Info("dataplane.response_input_rectifier_applied", map[string]any{
		"action":       fields["action"],
		"originalType": fields["originalType"],
	})
}

// wrapResponsesInputBodyFactory 按需把正文工厂包一层 responses input 归一。
//
// 只对 /v1/responses 生效：Node 的调用点判定就是 `session.originalFormat === "response"`。
func wrapResponsesInputBodyFactory(
	factory guard.BodyFactory,
	state *RequestState,
	switches func() rectify.Switches,
	logger *logx.Logger,
) guard.BodyFactory {
	if factory == nil || state == nil {
		return factory
	}
	return func(ctx *pctx.Context) (guard.BodyAccess, error) {
		inner, err := factory(ctx)
		if err != nil || inner == nil {
			return inner, err
		}
		return &responsesInputBody{
			inner:   inner,
			enabled: func() bool { return switches().ResponseInput },
			onAudit: state.appendRectifierAudit,
			logger:  logger,
		}, nil
	}
}
