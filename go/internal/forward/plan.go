package forward

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
)

// 计划构造阶段的可判别错误。
var (
	// ErrNoUpstreamURL 表示供应商与端点都没有可用基址。
	ErrNoUpstreamURL = errors.New("forward: 供应商与端点都没有上游基址")
	// ErrUnsupportedProviderType 表示该供应商类型不在三线矩阵内（当前为 gemini 系）。
	ErrUnsupportedProviderType = errors.New("forward: 供应商类型不支持转发")
	// ErrInvalidRequestPath 表示客户端路径非法（空或非绝对路径）。
	ErrInvalidRequestPath = errors.New("forward: 客户端路径非法")
	// ErrInvalidBody 表示请求正文不是合法 JSON 对象，无法做模型改写。
	ErrInvalidBody = errors.New("forward: 请求正文不是合法 JSON 对象")
	// ErrMissingMethod 表示请求方法为空。
	ErrMissingMethod = errors.New("forward: 请求方法为空")
	// ErrStatefulConversion 是「状态型字段跨协议线无承载位」的 sentinel。
	//
	// 为什么是 fail-closed 而不是记损继续：丢的不是可降级的约束，而是**上一次上下文的引用**
	// （previous_response_id / conversation）或**客户端显式要求的落库语义**（store:true）。
	// 静默继续时上游只能用一个缺了历史的对话作答，客户端拿到的是「形状正确但答非所问」的 200
	// ——比一个 400 难查得多（对齐 new-api 在 Responses→Chat 上的显式报错）。
	ErrStatefulConversion = errors.New("forward: 状态型字段在跨协议转换下无法承载")
)

// StatefulConversionError 携带具体的冲突字段名，供响应侧写出可自查的文案。
//
// 字段名必须到手：客户端要能据此自救（把历史放进 input，或改走同协议供应商），
// 而不是只看到一个「请求无效」。
type StatefulConversionError struct {
	Field string
}

func (e *StatefulConversionError) Error() string {
	return ErrStatefulConversion.Error() + ": " + e.Field
}

// Is 让 errors.Is(err, ErrStatefulConversion) 成立（否则调用方只能做类型断言）。
func (e *StatefulConversionError) Is(target error) bool { return target == ErrStatefulConversion }

// Provider 是转发视角的供应商投影。
//
// 字段集只覆盖转发所需：鉴权、基址、端点、客户端 IP 保留、自定义头、重试上限、非流式总超时、
// 模型重定向。参数覆写所需的各线私有配置不在其中——它由 OverrideApplier 实现自行取用。
type Provider struct {
	ID   int64
	Name string
	Type convert.ProviderType
	// Key 是上游密钥。绝不写入日志、错误文案或调试工件。
	Key string
	// URL 是供应商基址；为空时只能用端点 URL。
	URL string
	// Endpoints 是该供应商的端点候选，按尝试顺序排列；为空时只用 URL。
	Endpoints []Endpoint
	// PreserveClientIP 决定是否向该供应商透传客户端 IP 头。
	PreserveClientIP bool
	// CustomHeaders 是供应商级静态请求头。
	CustomHeaders map[string]string
	// MaxRetryAttempts 是当前供应商的重试上限；nil 表示用 Limits.DefaultMaxRetryAttempts。
	MaxRetryAttempts *int
	// RequestTimeoutNonStreamingMS 是本次尝试的总超时（含读体）；0 表示不限。
	RequestTimeoutNonStreamingMS int
	// FirstByteTimeoutStreamingMS 是流式请求的首字节阈值；超过后启动竞速候选。
	// 0 表示不启动（仅测试与非竞速场景）。
	FirstByteTimeoutStreamingMS int
	// ModelRedirects 是供应商级模型重定向规则（数组形态或旧的 map 形态）。
	ModelRedirects json.RawMessage

	// 以下列是**供应商级参数覆写偏好**（Node 的 provider.codex*/anthropic*/gemini* 族）。
	//
	// 为什么放在这里而不是让覆写实现自己查库：Node 施加覆写时用的就是它已经读出来的那行
	// 供应商对象（`provider.codexReasoningEffortPreference` 等），没有第二次查询；选路已经
	// 读过的同一行直接带过来，避免每次尝试多打一次库。
	//
	// 取值约定：空串与 `inherit`（DB 里两种形态都有）都表示「遵循客户端」。
	CacheTTLPreference                string
	CodexReasoningEffortPreference    string
	CodexReasoningSummaryPreference   string
	CodexTextVerbosityPreference      string
	CodexParallelToolCallsPreference  string
	CodexImageGenerationPreference    string
	CodexServiceTierPreference        string
	AnthropicMaxTokensPreference      string
	AnthropicThinkingBudgetPreference string
	// AnthropicAdaptiveThinking 是 jsonb 原文：写侧历史上无 schema 校验，故保留原文
	// 由覆写实现按 Node 的字段口径逐项读（解析失败视同未配置）。
	AnthropicAdaptiveThinking    json.RawMessage
	GeminiGoogleSearchPreference string
}

// Endpoint 是一个上游端点候选。
type Endpoint struct {
	ID    int64
	URL   string
	Label string
}

// ClientRequest 是构造计划所需的入站事实。
//
// Body 必须是最多被读一次的正文快照：调用方在入口读出后交给本包，本包不再回读上游或客户端。
type ClientRequest struct {
	Method string
	Path   string
	// Query 是客户端原始查询串（不含 "?"）。
	//
	// 为何单独一个字段：pctx 只持有 URL.Path，而 Node 拼上游 URL 时用的是整个 requestUrl
	// （`?alt=sse` 这类查询是上游选择 SSE 形状的开关，丢了就静默变成非流式响应）。
	Query   string
	Headers http.Header
	// Format 是客户端入站格式（claude/openai/response/gemini）。
	Format convert.ClientFormat
	// Model 是当前生效的模型名（用于匹配模型重定向规则）。
	Model string
	// Body 是客户端协议的 JSON 正文快照；HasBody 为假时为空。
	Body []byte
	// HasBody 为真表示存在正文。
	HasBody bool
	// MultipartBody 为真表示正文是 multipart 图片请求，不参与协议转换。
	MultipartBody bool
}

// PlanInput 是编译一次上游请求的全部输入。
type PlanInput struct {
	Client ClientRequest
	Target Target
	// ConversionEnabled 为真才允许跨协议转换（对应 provider.protocol_conversion_enabled）。
	ConversionEnabled bool
	// Overrides 为 nil 时不做供应商级参数覆写。
	Overrides OverrideApplier
	// CacheTTL1h 为真时补齐 anthropic-beta 的 1h 缓存标记。
	CacheTTL1h bool
	// ClientUserAgent / FilteredUserAgent / UserAgentModified 决定 codex 供应商的出站 User-Agent。
	ClientUserAgent   string
	FilteredUserAgent string
	UserAgentModified bool
}

// Target 是本次尝试的目标（供应商 + 端点）。
type Target struct {
	Provider Provider
	Endpoint Endpoint
}

// OverrideApplier 施加供应商级参数覆写。
//
// 实现按供应商类型分派，只改自己那条协议线的字段；protocol 是被传入正文的实际协议线，
// 调用方保证只在字段名对得上时调用。
type OverrideApplier interface {
	Apply(provider Provider, protocol convert.WireProtocol, body []byte) ([]byte, error)
}

// OverrideAuditSource 由 OverrideApplier 实现**额外**提供：把本次 Apply 产生的审计条目
// 交回计划（Node 的 `provider_parameter_override` 与 `gemini_google_search_override`）。
//
// 为什么是可选接口而不是改 OverrideApplier 的签名：覆写按供应商类型分派，审计只属于其中
// 若干分支；改签名会让「不产生审计」的实现也得造一个空返回值，并打断既有测试里那些
// 只关心正文改写的假实现。
type OverrideAuditSource interface {
	OverrideAuditEntries() []map[string]any
}

// CacheTTLResolver 由 OverrideApplier 实现**额外**提供：本次请求解析出的缓存 TTL。
//
// 为什么必须由覆写实现提供：TTL 的解析（密钥偏好 ?? 供应商偏好，forwarder.ts:774）与正文里的
// `cache_control.ttl` 是同一个决定的两半，另一半是出站 `anthropic-beta` 头（forwarder.ts:8829）。
// 两处各解析一次迟早分叉：正文写了 1h、头没补依赖标记，上游会按 5m 缓存。
type CacheTTLResolver interface {
	ResolvedCacheTTL(provider Provider) string
}

// ModelRedirect 是一次生效的模型重定向。
type ModelRedirect struct {
	Original string
	Target   string
	// Rule 是命中的匹配类型（exact/prefix/suffix/contains/regex）。
	Rule string
	// Source 是命中规则的源模式（如 `claude-*`）。链上要按 Node 的 matchedRule.source
	// 逐字落库，故必须留到落链时刻——只有 Target 无法还原「是哪条规则命中的」。
	Source string
}

// Plan 是编译好的上游请求。
type Plan struct {
	Method  string
	URL     string
	Headers http.Header
	// Body 是发往上游的正文，至多一份；nil 表示无正文。
	Body []byte
	// ContentLength 为 -1 表示长度未知。
	ContentLength int64

	// Protocol 是本次正文实际所属的协议线（转换生效时为目标线，否则为客户端线）。
	Protocol convert.WireProtocol
	// Conversion 非 nil 表示本次尝试施加了协议转换。
	Conversion *convert.ConversionPlan
	// ConversionLoss 是转换过程中的损失集合（仅转换生效时有值）。
	ConversionLoss *convert.LossReport
	// ConversionFallback 为真表示计划要求转换，但转换失败后按 Node 语义回退为原生直通。
	ConversionFallback bool
	// ConversionFailure 非 nil 表示本次**本来要转换但失败了**，承载失败事实（协议对 + 阶段 + 原因）。
	//
	// 它与 Conversion / ConversionFallback 的关系：
	//   - 转换成功：Conversion 非 nil、ConversionFailure 为 nil；
	//   - 转换失败：Conversion 为 nil、ConversionFailure 非 nil（ConversionFallback 视阶段而定）；
	//   - 压根不需要转换（原生同协议）：三者皆空，此时**不得**写失败条目。
	//
	// 为什么必须单独留这个事实：Node 与我们一样静默回退，但用户要能查出「转换失败在哪」——
	// 而失败原因在既有实现里被直接丢弃（见 plan.go 转换分支），故这里保留到落库时刻。
	ConversionFailure *convert.ConversionFailure
	// ToolNameRestore 是「上游可接受名 -> 客户端原名」的逆转表（转换生效且确有改写时非空）。
	//
	// 响应侧要拿它把上游回显的规范化工具名还原成客户端原名：客户端要用原名去自己的工具表里
	// 路由，收到改写过的名字就找不到工具（对应 Node 的 session.protocolToolNameRestore）。
	ToolNameRestore map[string]string
	// Redirect 非 nil 表示本次尝试改写了模型名。
	Redirect *ModelRedirect
	// ClientStream 表示客户端请求了流式输出（正文里的 stream 标记）。
	// 它是流式路径判定「该不该按流处理」的一个依据：有些上游不声明 SSE Content-Type
	// 却返回流，只看 MIME 会把它们误归为非流式。
	ClientStream bool
	// RequestTimeout 是本次尝试的总超时；0 表示不限。
	RequestTimeout time.Duration
	// OverrideSpecialSettings 是本次尝试中供应商级参数覆写产生的审计条目
	// （Node 的 provider_parameter_override / gemini_google_search_override）。
	//
	// 它与整流器条目走同一条终态追加通道（见 dataplane 的 specialSettingsAppendEntries）：
	// 产生在尝试循环里（每次尝试重算），但只有到终态才落库。
	OverrideSpecialSettings []map[string]any
}

// Request 把计划转换为拨号层请求。
//
// Body 以 reader 形式交出：内容仍是同一份切片，不复制第二份正文。
func (p *Plan) Request() dial.Request {
	req := dial.Request{
		Method:        p.Method,
		URL:           p.URL,
		Headers:       p.Headers,
		ContentLength: p.ContentLength,
	}
	if p.Body != nil {
		req.Body = bytes.NewReader(p.Body)
	}
	return req
}

// BuildPlan 把「客户端请求 + 供应商 + 端点」编译成上游请求计划。
//
// 覆盖顺序与 forwarder.ts 一致：路径映射 → 正文转换 → 模型重定向 → 参数覆写 → 出站 headers。
// 任一环节都不复制第二份正文：转换结果直接取代原切片，覆写结果直接取代转换结果。
func BuildPlan(in PlanInput) (*Plan, error) {
	if in.Client.Method == "" {
		return nil, ErrMissingMethod
	}
	if in.Client.Path == "" || !strings.HasPrefix(in.Client.Path, "/") {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRequestPath, in.Client.Path)
	}

	provider := in.Target.Provider
	isGemini := IsGeminiProviderType(provider.Type)
	baseURL := strings.TrimSpace(in.Target.Endpoint.URL)
	if baseURL == "" {
		baseURL = strings.TrimSpace(provider.URL)
	}
	if baseURL == "" && isGemini {
		// Node 的降级顺序末档：gemini 系供应商未配 url 时用官方端点
		// （forwarder.ts:3402-3407 的 `provider.url || GEMINI_PROTOCOL.*_ENDPOINT`）。
		baseURL = GeminiDefaultEndpoint(provider.Type)
	}
	if baseURL == "" {
		return nil, fmt.Errorf("%w: provider#%d", ErrNoUpstreamURL, provider.ID)
	}

	// Gemini 系供应商走原生透传，不进协议矩阵（见 buildGeminiPassthroughPlan）。
	if isGemini {
		return buildGeminiPassthroughPlan(in, baseURL)
	}

	// 客户端方言不在三线矩阵内（如 gemini 系）且供应商也不是 gemini：语料里这类组合恒
	// incompatible，选路层已把供应商排除。这里再狐一层——否则会把 gemini 方言的正文
	// 原样发到 anthropic/openai 端点上，上游只会给一个难查的 400（静默错误的请求）。
	if in.Client.Format != "" {
		if _, ok := convert.ProtocolOfClientFormat(in.Client.Format); !ok {
			return nil, fmt.Errorf(
				"%w: 客户端方言 %s 无对应协议线（供应商类型 %s）",
				ErrUnsupportedProviderType, in.Client.Format, provider.Type,
			)
		}
	}

	targetProtocol, ok := convert.ResolveTargetProtocol(in.Client.Format, provider.Type)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProviderType, provider.Type)
	}

	plan := &Plan{
		Method:       strings.ToUpper(in.Client.Method),
		Protocol:     targetProtocol,
		Headers:      http.Header{},
		ClientStream: ClientStreamRequestedFor(in.Client.Path, in.Client.Query, in.Client.Body),
	}
	if provider.RequestTimeoutNonStreamingMS > 0 {
		plan.RequestTimeout = time.Duration(provider.RequestTimeoutNonStreamingMS) * time.Millisecond
	}

	// 1) 转换计划：multipart 图片请求与未开启转换的供应商一律原生直通（对齐 Node 的拒绝语义）。
	if in.ConversionEnabled && !in.Client.MultipartBody {
		plan.Conversion = convert.PlanConversion(in.Client.Format, provider.Type, true)
	}

	// 2) 路径映射：转换生效时按目标线解析；解析不出即整次放弃（Node 的 fail-closed）。
	pathname := in.Client.Path
	if plan.Conversion != nil {
		resolved, ok := convert.ResolveUpstreamPath(plan.Conversion.TargetProtocol, in.Client.Path)
		if !ok {
			// 失败也要留事实（用户要能查「为何这次没转换」）：记下协议对、阶段与客户端路径。
			plan.ConversionFailure = &convert.ConversionFailure{
				ClientProtocol: plan.Conversion.ClientProtocol,
				TargetProtocol: plan.Conversion.TargetProtocol,
				Phase:          convert.PhasePathResolution,
				Reason: fmt.Sprintf(
					"目标协议线 %s 下无 %s 对应的上游端点（Node 的 fail-closed：放弃转换而非把未转换请求发到转换后的端点）",
					plan.Conversion.TargetProtocol, in.Client.Path,
				),
				// 与原文一致：路径解析失败不置 ConversionFallback（Node 的 fail-closed 语义），
				// 但请求仍以客户端原路径原生直通继续，故对「是否回退」如实记真。
				Fallback: true,
			}
			plan.Conversion = nil
		} else {
			pathname = resolved
		}
	}

	url, err := dial.BuildUpstreamURLWithQuery(baseURL, pathname, in.Client.Query)
	if err != nil {
		return nil, fmt.Errorf("forward: 构造上游 URL 失败: %w", err)
	}
	plan.URL = url

	// 3) 模型重定向：规则匹配置空表示不改写。
	redirect, err := resolveModelRedirect(in.Client.Model, provider.ModelRedirects)
	if err != nil {
		return nil, err
	}
	plan.Redirect = redirect

	// 4) 正文：先转换，再改写模型，最后施加供应商覆写。整个过程只保留一份正文。
	body := in.Client.Body
	switch {
	case !in.Client.HasBody:
		body = nil
	case plan.Conversion != nil:
		if err := rejectUnservableStateful(in.Client, plan.Conversion); err != nil {
			return nil, err
		}
		converted, loss, restore, err := convertBody(in.Client, plan.Conversion)
		if err != nil {
			// Node 语义：转换不可用时退回原生直通，而不是把未转换正文发到目标端点上。
			// 回退语义不变；此处只额外**保留失败事实**（原因文本在落库前脱敏），
			// 否则排障时无法区分「没转换」与「转换失败」。
			plan.ConversionFailure = &convert.ConversionFailure{
				ClientProtocol: plan.Conversion.ClientProtocol,
				TargetProtocol: plan.Conversion.TargetProtocol,
				Phase:          convert.PhaseBodyConversion,
				Reason:         err.Error(),
				Fallback:       true,
			}
			plan.Conversion = nil
			plan.ConversionFallback = true
		} else {
			body = converted
			plan.ConversionLoss = loss
			plan.ToolNameRestore = restore
		}
	}
	// 模型改写必须在**最终正文**上做（不得因「本次发生了转换」而跳过）：
	//
	// Node 的顺序是 `ModelRedirector.apply`（forwarder.ts:3284，**就地**改 `session.request.message.model`
	// 并重建 buffer）**先于**协议转换（forwarder.ts:3749 才快照/编码），故转换产出的目标方言正文里带的
	// 已经是重定向目标名。分方言看也成立：三个编码器都写**顶层** `model`
	// （codec_chat.go:806 / codec_responses.go:685 / codec_anthropic.go:629），而 `rewriteModelField`
	// 正是改写顶层 `model`，所以一次改写对三方言都命中。
	//
	// 历史缺陷：这里曾写 `plan.Conversion == nil && …`，于是**凡发生转换的请求都不改写**——上游收到的是
	// 客户端别名，凡「跨协议 + 配了 model_redirects」的供应商必 400/404（矩阵台 model 模式 3/9 的真因）。
	if redirect != nil && body != nil {
		rewritten, err := rewriteModelField(body, redirect.Target)
		if err != nil {
			return nil, err
		}
		body = rewritten
	}
	if in.Overrides != nil && body != nil {
		applied, err := in.Overrides.Apply(provider, plan.Protocol, body)
		if err != nil {
			return nil, fmt.Errorf("forward: 供应商参数覆写失败: %w", err)
		}
		body = applied
		// 审计与正文同源：条目在 Apply 里随改写一起决定，这里紧接着取回（下次尝试会覆盖）。
		if source, ok := in.Overrides.(OverrideAuditSource); ok {
			plan.OverrideSpecialSettings = source.OverrideAuditEntries()
		}
	}
	// 缓存 TTL 的一头一尾：正文里的 cache_control.ttl 由上一步写，出站 anthropic-beta 头在这里补。
	// 两者取同一个解析结果，故不依赖调用方是否单独设过 PlanFacts.CacheTTL1h。
	cacheTTL1h := in.CacheTTL1h
	if in.Overrides != nil {
		if resolver, ok := in.Overrides.(CacheTTLResolver); ok && resolver.ResolvedCacheTTL(provider) == "1h" {
			cacheTTL1h = true
		}
	}
	// include_usage 补齐：Node 把它放在供应商覆写与 final-phase 过滤器之后（forwarder.ts:3743），
	// 是出站正文的最后一步改写（见 openai_chat_usage_options.go）。
	if body != nil {
		completed, _, err := applyOpenAIChatStreamUsageOption(body, provider.Type, in.Client.Path)
		if err != nil {
			return nil, fmt.Errorf("forward: include_usage 补齐失败: %w", err)
		}
		body = completed
	}
	plan.Body = body
	plan.ContentLength = int64(len(body))
	if body == nil {
		plan.ContentLength = 0
	}

	// 5) 出站 headers 最后构造：它依赖最终基址。
	plan.Headers = BuildUpstreamHeaders(HeaderInput{
		ClientHeaders:     in.Client.Headers,
		Provider:          provider,
		BaseURL:           baseURL,
		CacheTTL1h:        cacheTTL1h,
		ClientUserAgent:   in.ClientUserAgent,
		FilteredUserAgent: in.FilteredUserAgent,
		UserAgentModified: in.UserAgentModified,
	})
	return plan, nil
}

// buildGeminiPassthroughPlan 编译 Gemini 线的**原生透传**计划。
//
// 与 Node 的 Gemini 分支（forwarder.ts:3299-3451）逐条对齐：
//   - **不进协议矩阵**：selection 语料里 gemini 的 targetProtocol 恒为 null（它不是一条可转换的线）；
//   - **路径与查询原样透传**：Node 用 `buildProxyUrl(baseUrl, session.requestUrl)` 拼原始
//     path + search（`?alt=sse` 等必须留住，它是上游选择 SSE 形状的开关）；
//   - **正文原样透传**：不像标准分支那样做模型重定向与供应商参数覆写（Node 的 gemini 分支没有这两步）；
//   - **鉴权与出站头**由 BuildUpstreamHeaders 的 gemini 分支负责（x-goog-api-key / Bearer）；
//   - **流式判定**额外认路径 `streamGenerateContent` 与查询 `alt=sse`。
func buildGeminiPassthroughPlan(in PlanInput, baseURL string) (*Plan, error) {
	url, err := dial.BuildUpstreamURLWithQuery(baseURL, in.Client.Path, in.Client.Query)
	if err != nil {
		return nil, fmt.Errorf("forward: 构造上游 URL 失败: %w", err)
	}

	plan := &Plan{
		Method:       strings.ToUpper(in.Client.Method),
		URL:          url,
		Protocol:     convert.ProtocolGemini,
		Headers:      http.Header{},
		ClientStream: ClientStreamRequestedFor(in.Client.Path, in.Client.Query, in.Client.Body),
	}
	if in.Target.Provider.RequestTimeoutNonStreamingMS > 0 {
		plan.RequestTimeout = time.Duration(in.Target.Provider.RequestTimeoutNonStreamingMS) * time.Millisecond
	}
	if in.Client.HasBody {
		plan.Body = in.Client.Body
	}
	// gemini 一族不走通用正文改写路径（本函数在 BuildPlan 早期就返回），所以覆写在这里单独施加：
	// Node 的 googleSearch 注入/移除也只存在于 gemini 原生透传分支（forwarder.ts:3274）。
	if in.Overrides != nil && len(plan.Body) > 0 {
		applied, err := in.Overrides.Apply(in.Target.Provider, convert.ProtocolGemini, plan.Body)
		if err != nil {
			return nil, fmt.Errorf("forward: 供应商参数覆写失败: %w", err)
		}
		plan.Body = applied
		if source, ok := in.Overrides.(OverrideAuditSource); ok {
			plan.OverrideSpecialSettings = source.OverrideAuditEntries()
		}
	}
	plan.ContentLength = int64(len(plan.Body))
	plan.Headers = BuildUpstreamHeaders(HeaderInput{
		ClientHeaders:     in.Client.Headers,
		Provider:          in.Target.Provider,
		BaseURL:           baseURL,
		CacheTTL1h:        in.CacheTTL1h,
		ClientUserAgent:   in.ClientUserAgent,
		FilteredUserAgent: in.FilteredUserAgent,
		UserAgentModified: in.UserAgentModified,
	})
	return plan, nil
}

// rejectUnservableStateful 在转换真的会把状态型字段丢掉时返回 fail-closed 错误。
//
// 只对 OpenAI 两族客户端判定：这几个键本就是 Responses 线的参数，其它方言里出现同名字段只是
// 杂项键；同时客户端收到的错误体是 OpenAI 形状（`guard.BuildError`），只有这两族读得懂——
// 不能因为一个杂项键给 claude 客户端发一个形状不对的 400。
//
// 本错误是**候选级**的，不是请求级：forward 的两条路径（串行 attempt.go、竞速 hedge.go）都把
// 它当成「该候选不可服务」，把供应商记入排除集后换下一个候选；只有当所有候选都无法承载时，
// 它才作为本次请求的结论交给客户端（400 + 字段名）。
func rejectUnservableStateful(client ClientRequest, plan *convert.ConversionPlan) error {
	if client.Format != convert.FormatResponse && client.Format != convert.FormatOpenAI {
		return nil
	}
	if plan == nil || !client.HasBody || len(client.Body) == 0 {
		return nil
	}
	body, err := convert.ParseJSON(client.Body)
	if err != nil {
		return nil
	}
	if field := convert.StatefulConversionConflict(plan.ClientProtocol, body); field != "" {
		return &StatefulConversionError{Field: field}
	}
	return nil
}

// convertBody 用枢纽编解码把客户端协议正文转换为目标协议正文。
//
// 与 Node 的 buildUpstreamRequest 一致：编码用的模型名取自**快照正文**的 model 字段，
// 而不是重定向后的模型——重定向若要对转换路径生效，调用方必须在生成快照时就写入目标模型
// （Node 的 primeOriginalBody 在模型重定向之后取快照，故首个供应商的重定向会进入快照）。
func convertBody(
	client ClientRequest,
	plan *convert.ConversionPlan,
) ([]byte, *convert.LossReport, map[string]string, error) {
	value, err := convert.ParseJSON(client.Body)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %v", ErrInvalidBody, err)
	}
	ctx := convert.ConvertCtx{
		ClientFormat:   clientFormatOfProtocol(plan.ClientProtocol),
		TargetProto:    plan.TargetProtocol,
		Model:          client.Model,
		Stream:         ClientStreamRequestedFromValue(client.Path, client.Query, value),
		ProviderID:     -1,
		ToWireToolName: convert.NormalizeToolName,
	}
	decoded, ok := convert.DecodeRequest(plan.ClientProtocol, value, ctx)
	if !ok {
		return nil, nil, nil, fmt.Errorf("%w: 客户端协议 %s 无解码器", ErrUnsupportedProviderType, plan.ClientProtocol)
	}
	encoded, ok := convert.EncodeRequest(plan.TargetProtocol, decoded.Value, ctx)
	if !ok || encoded.Body == nil {
		return nil, nil, nil, fmt.Errorf("%w: 目标协议 %s 无编码器", ErrUnsupportedProviderType, plan.TargetProtocol)
	}
	loss := decoded.Loss
	loss.Entries = append(loss.Entries, encoded.Loss.Entries...)
	// 逆转表取自被解码的客户端请求：编码器写进上游的是规范化名，响应侧要还原回原名。
	restore := convert.BuildToolNameRestoreMap(toolNamesOf(decoded.Value))
	if len(restore) == 0 {
		restore = nil
	}
	return []byte(encoded.Body.MarshalCompact()), &loss, restore, nil
}

// toolNamesOf 取枢纽请求里声明的工具名（响应侧逆转表的来源）。
func toolNamesOf(request *convert.Request) []string {
	if request == nil {
		return nil
	}
	names := make([]string, 0, len(request.Tools))
	for _, tool := range request.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// ClientStreamRequested 读取客户端正文里的 stream 标记；不存在即 false。
//
// 导出是因为竞速判定（数据面）必须与计划编译用同一把尺子：Node 的竞速条件读的正是
// 原始 message.stream === true（forwarder.ts:4918），两边各写一份迟早会分叉。
//
// 走 convert.TopLevelBoolTrue 而不是 convert.ParseJSON：语义严格等价
// （对拍见 convert 包的 TestTopLevelBoolTrueMatchesParseJSON），但不必为读一个布尔
// 把整份正文建成树——生产剖析里这一项占全服务累计内存分配的 15.2%，
// 而且同一请求会问它三次（见 ClientStreamRequestedFromValue 那条复用路径）。
func ClientStreamRequested(body []byte) bool {
	return convert.TopLevelBoolTrue(body, "stream")
}

// ClientStreamRequestedFromValue 是 ClientStreamRequestedFor 的「已持有解析结果」变体：
// 调用方若已经把同一份正文 ParseJSON 过，就直接问那棵树，省掉一次全量重解析。
//
// 用途：convertBody 先 ParseJSON 再解码，那里正是「已持有」的场景；
// 传进来的 value 必定非 nil——非法正文在 ParseJSON 那一步就已经返回错误了。
func ClientStreamRequestedFromValue(pathname string, rawQuery string, value *convert.Value) bool {
	if field, ok := value.Get("stream"); ok {
		if requested, ok := field.Bool(); ok && requested {
			return true
		}
	}
	// 路径与查询串的信号与正文无关，口径与 ClientStreamRequestedFor 保持一致。
	if strings.Contains(strings.ToLower(pathname), "streamgeneratecontent") {
		return true
	}
	return hasSSEAltParam(rawQuery)
}

// ClientStreamRequestedFor 是 ClientStreamRequested 的完整口径：除正文的 stream 标记外，
// 还认 Gemini 的两种路径/查询形态。
//
// 为何必需：Gemini 的流式**不写在正文里**——“：streamGenerateContent”写在路径上，
// “?alt=sse”写在查询上（Node forwarder.ts:3345-3350 正是这三条或在一起）。
// 只看正文会让这类请求被当成非流式：上游返回的 SSE 虽凭 Content-Type 仍能识别，
// 但首字节阈值/空闲超时/竞速闸门这些按「客户端要流」分支的旋钮会走错档。
//
// 这两个信号本身是**路径形状事实**（Gemini 语系的写法），故不按客户端方言门控：
// Node 的该分支是按**供应商类型**进入的，拿方言再筛一道反而会漏掉它。
func ClientStreamRequestedFor(pathname string, rawQuery string, body []byte) bool {
	if ClientStreamRequested(body) {
		return true
	}
	if strings.Contains(strings.ToLower(pathname), "streamgeneratecontent") {
		return true
	}
	return hasSSEAltParam(rawQuery)
}

// hasSSEAltParam 判定查询串里是否有 alt=sse（大小写不敏感，允许重复出现与额外参数）。
func hasSSEAltParam(rawQuery string) bool {
	for _, pair := range strings.Split(rawQuery, "&") {
		name, value, found := strings.Cut(pair, "=")
		if !found || !strings.EqualFold(name, "alt") {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(value), "sse") {
			return true
		}
	}
	return false
}

// clientFormatOfProtocol 把协议线映射回客户端格式（编解码器上下文需要它）。
func clientFormatOfProtocol(protocol convert.WireProtocol) convert.ClientFormat {
	switch protocol {
	case convert.ProtocolAnthropicMessages:
		return convert.FormatClaude
	case convert.ProtocolOpenAIChat:
		return convert.FormatOpenAI
	case convert.ProtocolOpenAIResponses:
		return convert.FormatResponse
	default:
		return ""
	}
}

// rewriteModelField 就地改写 JSON 正文的 model 字段，保留其余键序。
func rewriteModelField(body []byte, model string) ([]byte, error) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidBody, err)
	}
	if _, ok := decoded["model"]; !ok {
		return body, nil
	}
	decoded["model"] = model
	rewritten, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("forward: 模型改写序列化失败: %w", err)
	}
	return rewritten, nil
}

// resolveModelRedirect 在供应商规则里找出匹配当前模型的规则。
//
// 规则形态与 Node 的 provider-model-redirects 一致：数组形态 [{matchType,source,target}] 或
// 旧的 map 形态 {source: target}（等价于 exact 规则）。匹配类型：exact/prefix/suffix/contains/regex。
func resolveModelRedirect(model string, raw json.RawMessage) (*ModelRedirect, error) {
	if model == "" || len(raw) == 0 {
		return nil, nil
	}
	rules, err := parseModelRedirectRules(raw)
	if err != nil {
		return nil, err
	}
	for _, rule := range rules {
		if matchesModelPattern(model, rule.MatchType, rule.Source) {
			return &ModelRedirect{
				Original: model,
				Target:   rule.Target,
				Rule:     rule.MatchType,
				Source:   rule.Source,
			}, nil
		}
	}
	return nil, nil
}

// modelRedirectRule 是一条归一化后的规则。
type modelRedirectRule struct {
	MatchType string
	Source    string
	Target    string
}

// allowedModelRedirectMatchTypes 是规则允许的匹配类型集合。
var allowedModelRedirectMatchTypes = map[string]bool{
	"exact":    true,
	"prefix":   true,
	"suffix":   true,
	"contains": true,
	"regex":    true,
}

func parseModelRedirectRules(raw json.RawMessage) ([]modelRedirectRule, error) {
	var asArray []map[string]any
	if err := json.Unmarshal(raw, &asArray); err == nil {
		rules := make([]modelRedirectRule, 0, len(asArray))
		for _, entry := range asArray {
			matchType, _ := entry["matchType"].(string)
			source, _ := entry["source"].(string)
			target, _ := entry["target"].(string)
			matchType = strings.TrimSpace(matchType)
			source = strings.TrimSpace(source)
			target = strings.TrimSpace(target)
			if !allowedModelRedirectMatchTypes[matchType] || source == "" || target == "" {
				continue
			}
			rules = append(rules, modelRedirectRule{MatchType: matchType, Source: source, Target: target})
		}
		return rules, nil
	}

	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		return nil, fmt.Errorf("forward: 模型重定向规则非法: %w", err)
	}
	rules := make([]modelRedirectRule, 0, len(asMap))
	for source, rawTarget := range asMap {
		target, ok := rawTarget.(string)
		source = strings.TrimSpace(source)
		target = strings.TrimSpace(target)
		if !ok || source == "" || target == "" {
			continue
		}
		rules = append(rules, modelRedirectRule{MatchType: "exact", Source: source, Target: target})
	}
	return rules, nil
}

// matchesModelPattern 复刻 model-pattern-matcher.ts 的 matchesPattern。
//
// regex 分支不隐式补锚点；正则不合法时按 glob 处理（对齐 Node 的兼容行为）。
// 该实现与 route 包的私有 matcher 重复：待 matcher 上提到共享包后，本函数应被删除。
func matchesModelPattern(model, matchType, pattern string) bool {
	switch matchType {
	case "exact":
		return model == pattern
	case "prefix":
		return strings.HasPrefix(model, pattern)
	case "suffix":
		return strings.HasSuffix(model, pattern)
	case "contains":
		return strings.Contains(model, pattern)
	case "regex":
		re, ok := compileModelPattern(pattern)
		return ok && re.MatchString(model)
	default:
		return false
	}
}

// observeFormatFor 决定观测器解析上游流所用的方言。
//
// 转换生效时上游说的是**目标线**方言；若仍按客户端方言解析，用量字段与终止标记都会读错，
// 一条正常结束的流会被误判为上游截断（终态与计费随之写错）。无转换时两者同一，行为不变。
func observeFormatFor(plan *Plan, fallback convert.ClientFormat) convert.ClientFormat {
	if plan == nil || plan.Conversion == nil {
		return fallback
	}
	return clientFormatOfProtocol(plan.Protocol)
}
