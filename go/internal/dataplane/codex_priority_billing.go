package dataplane

import (
	"context"
	"encoding/json"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件实现「codex priority（Fast Mode）按哪个 service_tier 计费」的判定。
//
// Node 出处：
//   - 请求侧：getRequestedCodexServiceTier（response-handler.ts:1227-1237）——仅 codex 供应商，
//     取请求正文的 `service_tier`；
//   - 响应侧：parseServiceTierFromResponseText（response-handler.ts:1239+）——取响应里的
//     `service_tier`（顶层或 `response.service_tier`）；
//   - 选择：resolveCodexPriorityBillingDecision（response-handler.ts:1288-1342）——按
//     system_settings.codex_priority_billing_source 取 requested 或 actual；选定的档位等于
//     `priority` 时，本次按 priority 单价计费（terminal 的 PriorityServiceTierApplied）。
//
// 生产取值是 `requested`，即用请求里的档位。**在此之前 Go 从不置这个标志位**，priority 档
// 单价永远取不到——这是「少收钱」的一类缺口，故本判定不只是读一个设置。
//
// 与 Node 的一处刻意差异（登记在报告里）：流式响应侧的 actual 档不采。原因是它需要
// forward 观测层保留整段 service_tier（Node 扫 SSE 文本取「最后一次」），而 Go 的流式观测
// 只留字节数与末次用量；该文件属另一 lane。缺失时的行为与 Node 的「响应未返回值」同支：
// 回退 requested。

// codexPriorityTier 是触发 priority 单价计费的档位字面量（OpenAI 的 service_tier 取值）。
const codexPriorityTier = "priority"

// codexPriorityBillingSourceRequested / ...Actual 是设置的两个取值。
const (
	codexPriorityBillingSourceRequested = "requested"
	codexPriorityBillingSourceActual    = "actual"
)

// captureRequestedServiceTier 从客户端请求正文里取 codex 的 service_tier。
//
// 与 Node 同判：非字符串一律视为「未声明」。取不到时留空串，由选择逻辑回退。
func captureRequestedServiceTier(state *RequestState, body *bodyAccess) {
	if state == nil || body == nil {
		return
	}
	access, err := body.factory(state.PC)
	if err != nil || access == nil {
		return
	}
	parsed, err := access.JSON()
	if err != nil || parsed == nil {
		return
	}
	if tier, ok := parsed["service_tier"].(string); ok {
		state.requestedServiceTier = tier
	}
}

// parseServiceTierFromResponseText 从**非流式**响应正文里取 service_tier。
//
// 只读顶层与 `response.service_tier` 两处，与 Node 的非流式分支一致；流式的
// 「扫 SSE 取最后一次」不在此实现（见文件头注释）。
func parseServiceTierFromResponseText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil || parsed == nil {
		return ""
	}
	if tier, ok := parsed["service_tier"].(string); ok && tier != "" {
		return tier
	}
	if nested, ok := parsed["response"].(map[string]any); ok {
		if tier, ok := nested["service_tier"].(string); ok && tier != "" {
			return tier
		}
	}
	return ""
}

// codexPriorityGate 是「按哪个 service_tier 计费」的设置面。
//
// 形态与 cacheScoreGate 一致：持有设置源与一个 TTL 缓存（同源两张表会两次查库，且热路径
// 不该每请求读库）。nil 接收者或用未接线时按「不计 priority 档」处理。
type codexPriorityGate struct {
	settings guard.SettingsSource
	logger   *logx.Logger
	cached   *cfgsync.TTLMap[int, string]
}

// newCodexPriorityGate 建设置面。settings 为 nil 时只用出厂值（requested）。
func newCodexPriorityGate(settings guard.SettingsSource, logger *logx.Logger) *codexPriorityGate {
	if logger == nil {
		logger = logx.New(nil)
	}
	return &codexPriorityGate{
		settings: settings,
		logger:   logger,
		cached:   cfgsync.NewTTLMap[int, string](cfgsync.Spec(cfgsync.DomainSystemSettings).TTL, 1),
	}
}

// source 读 `system_settings.codex_priority_billing_source`（TTL 缓存）。
//
// 取值非法（空串或未知）时按 Node 的兜底走 requested（session.ts:1521-1535）。
// 读失败也写缓存：一次库抖动不该把热路径变成每请求一次设置查询。
func (g *codexPriorityGate) source(ctx context.Context) string {
	if g == nil {
		return codexPriorityBillingSourceRequested
	}
	if value, ok := g.cached.Get(billingSourceKey); ok {
		return value
	}
	value := codexPriorityBillingSourceRequested
	if g.settings != nil {
		settings, err := g.settings.FindSystemSettings(ctx)
		if err != nil || settings == nil {
			g.logger.Warn("dataplane.codex_priority_billing_source_lookup_failed",
				map[string]any{"error": errorText(err)})
		} else if settings.CodexPriorityBillingSource == codexPriorityBillingSourceActual {
			value = codexPriorityBillingSourceActual
		}
	}
	g.cached.Set(billingSourceKey, value)
	return value
}

// applied 判定本次是否按 priority 档单价计费。
//
// providerType 必须是 codex（Node 的 resolveCodexPriorityBillingDecision 首判如此）；
// requested / actual 分别是请求侧与响应侧的档位，空串表示未声明。
func (g *codexPriorityGate) applied(ctx context.Context, providerType string, requested string, actual string) bool {
	if g == nil || providerType != string(convert.ProviderCodex) {
		return false
	}
	tier := requested
	if g.source(ctx) == codexPriorityBillingSourceActual && actual != "" {
		tier = actual
	}
	return tier == codexPriorityTier
}
