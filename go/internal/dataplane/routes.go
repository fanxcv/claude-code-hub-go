// Package dataplane 把守卫链、选路、转发与终态结算接成可对外服务的 `/v1` 数据面。
//
// 定位：本包是**装配层**，不是业务层。守卫判定在 internal/guard，选路在 internal/route，
// 拨号与重试在 internal/forward，终态入账在 internal/terminal，池与只读视图在 internal/store。
// 本包只做四件事：把入站请求翻成 pctx、按路由选预设跑链、把转发结果写成 HTTP 响应、
// 把未实现的路由交回 Node（**绝不返回 501**——那会把「Go 还没实现」谎报成「协议不支持」）。
//
// 路由边界（见 routes.go 的 routeTable，README.md 有运维视角的说明）：
// Go 承三条主对话路由；其余（模型列表、count_tokens、compact、embeddings、/v1beta）
// 明确回退 Node。回退不是降级，是双跑过渡期的正常形态：前门按 CCH_EGRESS_ROUTES 判归属，
// 判给 Go 但本包没有路由的路径原样反代到 Node。
//
// WebSocket 不在本表：`/v1/responses` 的升级请求由 internal/ws 在进程入口按**同一路径的
// HTTP 归属**分流（归属给 Go 才接管，否则连升级请求一起下沉 Node），不经本包；尚未接管的
// WS 路径仍下沉 Node。
//
// 三条不变量（违反即缺陷）：
//
//  1. 一条请求只开一行、只结算一次。开行在守卫链的 messageContext 步骤（写 pctx 行标识），
//     终态在本包结算缝（读同一标识）。本包不得再建行。
//  2. 客户端零字节承诺交由 forward 的门控保证：提交前的失败在本包看不到正文，
//     因此本包只需在拿到结果后如实写回，不得自行缓冲上游正文。
//  3. 正文只被消费一次：入站正文经守卫链的 BodyAccessor（一次性 TakeBody）后在转发前取字节，
//     中间不复制第二份。流式路径的驻留量因此等于在途 chunk。
package dataplane

import (
	"net/http"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
)

// routeSpec 是一条由 Go 承载的路由。
//
// 字段刻意只含「入口判定所需」的事实：方法、归一化路径、守卫预设、客户端格式、协议族，
// 以及是否需要强制走门控（codex 兼容路径上游可能不声明 SSE）。端点策略的其余维度
// （重试、切换、熔断记账、过滤器旁路）由 guard.Policy 与转发候选取值表达，不在此重复。
type routeSpec struct {
	// Method 是归一为大写的 HTTP 方法。
	Method string
	// Path 是归一化后的精确路径（小写、无尾斜杠、无查询串）。
	Path string
	// Policy 是端点策略中与守卫预设有关的部分。
	Policy guard.Policy
	// Format 是客户端入站格式，决定转发时的转换方向与观测方言。
	Format convert.ClientFormat
	// Family 是客户端侧协议族，写回 pctx（归属判定与限流键的口径）。
	Family egress.Family
	// ForceGate 为真时即使上游未声明 SSE 也走门控。
	//
	// 当前三条路由都留 false：客户端要流时由转发计划的 ClientStream 判定（forward 的
	// shouldGate 已覆盖），而入口无法区分「客户端要非流」与「客户端要流但上游没声明 SSE」，
	// 一刀切开只会让非流式响应被 SSE 解析器判成门禁失败。
	ForceGate bool
	// RawPassthrough 为真表示本条属**原始透传**端点（Node 的 raw_passthrough 端点策略）。
	//
	// 它同时决定两件事，且两件都必须与 Node 同源：
	//   - 不重试、不切换供应商（Node：allowRetry/allowProviderSwitch 皆 false）；
	//   - 守卫链取 raw 预设（Node：guardPreset=raw_passthrough）。
	// 转换与响应改写不由本字段表达：crossLinePaths 对 count_tokens 与 compact **只允许同线**，
	// 因此跨线时 convert.ResolveUpstreamPath 必然失败并回落原生直通——正是 Node 的
	// 「不做转发前预处理」。
	RawPassthrough bool
	// ModelList 非 nil 表示该路径由**聚合式模型列表处理器**作答（不转发、不开行）。
	// nil 表示本路径走守卫链 + 转发。
	ModelList *modelListSpec
}

// routeTable 是 Go 当前承载的路由全集。
//
// 只列三条主对话路由，理由写在 README.md 的「未实现路由」一节：
//   - `/v1/messages/count_tokens` 与 `/v1/responses/compact` 属原始透传策略，它要求
//     「不做转发前预处理」，而 forward 当前只表达「不重试、不切换」（Candidate.RawPassthrough），
//     没有「不转换、不改写」的开关；在补上该开关前让它们回退 Node，好过在这里猜语义。
//   - `/v1/models` 三兄弟是聚合式模型列表（可用模型、价格、厂端点），本身不是转发路径。
//   - `/v1beta/*`（Gemini 原生）与 Responses WebSocket 尚无 codec / 适配器。
var routeTable = []routeSpec{
	{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Policy: guard.ChatPolicy(),
		Format: convert.FormatClaude,
		Family: egress.FamilyAnthropicMessages,
	},
	{
		Method: http.MethodPost,
		Path:   "/v1/chat/completions",
		Policy: guard.ChatPolicy(),
		Format: convert.FormatOpenAI,
		Family: egress.FamilyOpenAIChat,
	},
	{
		Method: http.MethodPost,
		Path:   "/v1/responses",
		Policy: guard.ChatPolicy(),
		Format: convert.FormatResponse,
		Family: egress.FamilyOpenAIResponses,
		// ForceGate 刻意不设：它会让**非流式**请求也走门控，而门控按 SSE 解析 JSON 正文，
		// 结果是把正常的非流式响应判成门禁失败（实测 502）。客户端是否要流由转发计划里的
		// ClientStream 表达（forward 的 shouldGate 已含该判定），入口不越权替它决定。
	},
}

// matchRoute 按方法与归一化路径查表。
//
// 查表顺序即契约：
//  1. 具名路由表（三条主对话路由）；
//  2. 族透传（`ResolveEndpointFamily`）：Node 侧对已知族的路径一律走通用透传，
//     上游 URL 由 dial.BuildUpstreamURL(base, 归一化客户端路径) 拼出；
//  3. 都不命中 → 调用方回退 Node（双跑期的正常形态）。
//
// 聚合式模型列表**不在这里**：它不是透传而是真处理器，且只在装配了 ModelCatalog 时
// 才能成事（没有它就只能把上游某家的清单原样端出去）。故它由 ServeHTTP 在
// 「目录已接线」的前提下单判，未接线时这五条路径回退 Node——宁可回退，不可答错。
//
// Path 用 egress.NormalizePath（折叠重复斜杠、去尾斜杠）而不是只做小写：归属判定用的就是
// 那把尺子，两处口径必须一致，否则会出现「前门判给 Go、数据面找不到路由」的静默回退。
//
// 具名表必须排在族透传之前：`/v1/messages` 与 `/v1/messages/count_tokens` 同属 claude 面，
// `/v1/responses/compact` 与 `/v1/responses` 同属 response 面，族是前缀语义、粒度更粗。
func matchRoute(method string, path string) (routeSpec, bool) {
	normalized := strings.ToLower(egress.NormalizePath(path))
	upperMethod := strings.ToUpper(method)
	for _, candidate := range routeTable {
		if candidate.Method == upperMethod && candidate.Path == normalized {
			return candidate, true
		}
	}
	// 聚合式模型列表的**精确路径**不得落入透传：目录未接线时它们必须回退 Node；
	// 若被透传接走，客户端会拿到上游某家的模型清单（错答），比回退更坏。
	// 只挡「方法也命中」的组合：`POST /v1beta/models` 不属聚合端点，仍走族透传（与 Node 同）。
	if _, ok := matchModelListRoute(upperMethod, normalized); ok {
		return routeSpec{}, false
	}
	return matchFamilyRoute(normalized)
}

// matchFamilyRoute 按端点族给出通用透传路由。
//
// 方法**不做限制**：Node 的族目录是按路径建表的 catch-all（`app.all("*")`），
// `DELETE /v1/files/{id}`、`GET /v1/batches/{id}` 这类都要代理；在这里按方法挑挑拣拣，
// 只会把本该转发的请求推回 Node。
//
// 模型列表族（`/v1/models`、`/v1beta/models`）**不在本函数里排除**：Node 只对精确路径
// `GET /v1/models` 挂了聚合处理器，而 `/v1/models/{model}`、`POST /v1beta/models`
// 落到 catch-all 走代理（族是前缀/正则语义，粒度比处理器粗）。两者的先后由
// matchModelListRoute 在前一步用**精确路径**决定。
func matchFamilyRoute(normalizedPath string) (routeSpec, bool) {
	family, ok := ResolveEndpointFamily(normalizedPath)
	if !ok {
		return routeSpec{}, false
	}
	return specForFamily(family), true
}

// specForFamily 把族投影成路由规格。
//
// 两个字段的取值都是**逐条对齐 Node** 得来，不是就近猜的：
//   - Format/Family 由族的 surface 决定（Node 的 surface 就是「客户端协议面」）；
//   - Policy 取 chat 预设（Node 的 DEFAULT_ENDPOINT_POLICY.guardPreset === "chat"），
//     唯有原始透传端点取 raw 预设（RAW_PASSTHROUGH_ENDPOINT_POLICY.guardPreset）。
func specForFamily(family EndpointFamily) routeSpec {
	spec := routeSpec{
		Policy: guard.ChatPolicy(),
		Format: clientFormatForSurface(family.Surface),
		Family: egressFamilyForSurface(family.Surface),
	}
	if family.RawPassthrough {
		// RAW_PASSTHROUGH_ENDPOINT_POLICY 的 allowRawCrossProviderFallback 为 true，
		// 且 Node 在 proxy-handler 里把它与系统设置 allowNonConversationEndpointProviderFallback
		// （缺省 true）取与，故缺省落到 RAW_SAFE_SESSION_PIPELINE。
		//
		// ponytail: 这里写死 true 而非读设置——设置源不在这条纯函数路由链上。若将来把该设置
		// 接进装配层，应改为由装配层传入（Node 在 proxy-handler.ts:55-80 读取）。
		spec.Policy = guard.Policy{
			Preset:                   guard.PresetRawPassthrough,
			RawCrossProviderFallback: true,
		}
		spec.RawPassthrough = true
	}
	return spec
}

// clientFormatForSurface 把族的 surface 映射成客户端格式。
func clientFormatForSurface(surface EndpointFamilySurface) convert.ClientFormat {
	switch surface {
	case SurfaceClaude:
		return convert.FormatClaude
	case SurfaceOpenAI:
		return convert.FormatOpenAI
	case SurfaceResponse:
		return convert.FormatResponse
	case SurfaceGemini:
		return convert.FormatGemini
	case SurfaceGeminiCLI:
		return convert.FormatGeminiCLI
	default:
		return ""
	}
}

// egressFamilyForSurface 把族的 surface 映射成 pctx 里的协议族。
//
// pctx.ProtocolFrom 同时是「选路时客户端格式的兜底来源」（guard 的 clientFormatOf）与请求
// 日志的 protocolFrom 字段，故它必须与 spec.Format 同源，不能留空。
//
// gemini-cli 没有独立的 egress 族（egress.Family 只有四个具名族），落 gemini：
// 两者共用 gemini 适配器与转换线，日志里区分靠请求路径。
func egressFamilyForSurface(surface EndpointFamilySurface) egress.Family {
	switch surface {
	case SurfaceClaude:
		return egress.FamilyAnthropicMessages
	case SurfaceOpenAI:
		return egress.FamilyOpenAIChat
	case SurfaceResponse:
		return egress.FamilyOpenAIResponses
	case SurfaceGemini, SurfaceGeminiCLI:
		return egress.FamilyGemini
	default:
		return ""
	}
}

// nowOr 是可注入时钟的兜底。
func nowOr(clock func() time.Time) time.Time {
	if clock == nil {
		return time.Now()
	}
	return clock()
}
