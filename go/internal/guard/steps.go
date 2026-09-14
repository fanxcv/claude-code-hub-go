package guard

import (
	"context"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// defaultLogger 是未注入日志器时的落点。写 stderr，与 logx.New 的默认行为一致。
var defaultLogger = logx.New(nil)

// logger 返回守卫链自身的日志器。
func (d Deps) logger() *logx.Logger {
	if d.Logger == nil {
		return defaultLogger
	}
	return d.Logger
}

// runContext 把 pctx 映射回标准库 context，供缝隙里的数据库与 Redis 调用使用。
func (d Deps) runContext(ctx *pctx.Context) context.Context {
	if d.RequestContext == nil {
		return context.Background()
	}
	if derived := d.RequestContext(ctx); derived != nil {
		return derived
	}
	return context.Background()
}

// Steps 把依赖装配成步骤键到实现的映射，对应 Node 侧的 Steps 常量表。
//
// 每次调用都新建闭包，链上不带跨请求的可变状态：Deps 里的实现本身必须是并发安全的。
func (d Deps) Steps() StepIndex {
	return StepIndex{
		StepAuth:                  d.authStep(),
		StepClient:                d.clientStep(),
		StepModel:                 d.modelStep(),
		StepVersion:               d.versionStep(),
		StepProbe:                 d.probeStep(),
		StepSession:               d.sessionStep(),
		StepWarmup:                d.warmupStep(),
		StepRequestFilter:         d.requestFilterStep(),
		StepSensitive:             d.sensitiveStep(),
		StepReplayAttach:          d.replayStep(),
		StepRateLimit:             d.rateLimitStep(),
		StepProvider:              d.providerStep(),
		StepProviderRequestFilter: d.providerRequestFilterStep(),
		StepMessageContext:        d.messageContextStep(),
	}
}

// body 取出请求正文，供依赖正文的步骤使用。
//
// 返回错误即视为「本步骤无法判定」，各步骤按 Node 侧同名守卫的语义决定放行还是拒绝：
// 敏感词与版本检查 fail-open，过滤器 fail-open，会话与选路则按各自语义处理。
func (d Deps) body(ctx *pctx.Context) (map[string]any, error) {
	if d.Body == nil {
		return nil, ErrBodyUnavailable
	}
	access, err := d.Body(ctx)
	if err != nil {
		return nil, err
	}
	if access == nil {
		return nil, ErrBodyUnavailable
	}
	return access.JSON()
}

// storeBody 把改动后的正文写回，供后续步骤与上游转发使用。
func (d Deps) storeBody(ctx *pctx.Context, body map[string]any) error {
	if d.Body == nil {
		return ErrBodyUnavailable
	}
	access, err := d.Body(ctx)
	if err != nil {
		return err
	}
	if access == nil {
		return ErrBodyUnavailable
	}
	return access.Store(body)
}

// messagesFromBody 复刻 ProxySession.getMessages：按 Claude、Codex、Gemini、Gemini CLI
// 的优先级取消息数组。
//
// 与 Node 的差异（有意）：Node 用 !== undefined 判定「键存在」，Go 侧 map 判定键是否存在，
// 语义等价。
func messagesFromBody(body map[string]any) any {
	if body == nil {
		return nil
	}
	if value, ok := body["messages"]; ok {
		return value
	}
	if value, ok := body["input"]; ok {
		return value
	}
	if value, ok := body["contents"]; ok {
		return value
	}
	if requestData, ok := body["request"].(map[string]any); ok {
		if value, ok := requestData["contents"]; ok {
			return value
		}
	}
	return nil
}

// normalizeEndpointPath 复刻 endpoint-paths.ts 的 normalizeEndpointPath：去查询串、
// 去尾斜杠（根路径除外）、小写化。
func normalizeEndpointPath(path string) string {
	trimmed := path
	if index := strings.IndexByte(trimmed, '?'); index >= 0 {
		trimmed = trimmed[:index]
	}
	if len(trimmed) > 1 && strings.HasSuffix(trimmed, "/") {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return strings.ToLower(trimmed)
}

// messagesCount 是消息条数；非数组时为 0。
func messagesCount(body map[string]any) int {
	messages, ok := messagesFromBody(body).([]any)
	if !ok {
		return 0
	}
	return len(messages)
}
