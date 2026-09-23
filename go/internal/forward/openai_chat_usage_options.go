package forward

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件承载 `stream_options.include_usage` 的补齐，对应 Node
// `src/app/v1/_lib/proxy/openai-chat-usage-options.ts` 的
// `ensureOpenAIChatStreamUsageOption`（调用点 `forwarder.ts:3743`，在供应商覆写与
// final-phase 过滤器**之后**、序列化之前）。
//
// 为什么必须补：OpenAI 兼容上游只在流式响应的最后一帧附 usage，**且只在请求显式声明
// `stream_options.include_usage` 时**。不补这一句，网关拿不到上游口径的 token 用量——
// 缓存命中（cache_read）也就无从观测，用量只能退化成估算。

// includeUsageChatPath 是这条规则的适用路径。
//
// 判据用**客户端**请求路径（Node 的 `session.requestUrl.pathname`），不是解析后的上游路径：
// 客户端没走 chat 线时，其正文里也没有 chat 形态的 `stream`/`stream_options` 可言。
const includeUsageChatPath = "/v1/chat/completions"

// applyOpenAIChatStreamUsageOption 按 Node 的同名函数补齐 `include_usage`，
// 返回改写后的正文与是否改写（沿用本包「只保留一份正文」的写法）。
//
// 与 Node 逐分支对齐：
//   - 供应商类型必须是 openai-compatible（claude / codex / gemini 系不走这条）；
//   - 请求路径必须是 `/v1/chat/completions`；
//   - 顶层 `stream` 必须严格为布尔真（缺省与非 true 一律不动）；
//   - `stream_options` 缺省或为 null 时新建成 `{include_usage:true}`；
//   - 已是对象时**保留其它键**、强制 `include_usage` 为 true；已是 true 则原样返回；
//   - `stream_options` 是数组或标量时不动（Node 的 `typeof !== "object" || Array.isArray` 同判）。
//
// 解析失败时原样返回：Node 这条规则作用于**已解析**的对象、永不抛错；在 Go 侧新增一个失败模式
// 会把「客户端正文非法」升级成转发期错误，而它在守卫链的正文解析处已被拦下。
//
// 正文模型用 convert.Value 而非 Go map 往返：后者经 encoding/json 序列化会把键序变字典序、
// 把 `<` 转成 `\u003c`、把数字经 float64 重排（`1e21` → `1e+21`、超 2^53 的整数丢精度）。
// Value 保序、保数字字面量，输出为紧凑形态（空白与转义形态会归一，语义不变）。
func applyOpenAIChatStreamUsageOption(
	body []byte,
	providerType convert.ProviderType,
	requestPath string,
) ([]byte, bool, error) {
	if len(body) == 0 || providerType != convert.ProviderOpenAICompatible || requestPath != includeUsageChatPath {
		return body, false, nil
	}
	decoded, err := convert.ParseJSON(body)
	if err != nil || !decoded.IsObject() {
		return body, false, nil
	}
	// `stream` 严格为 true：Node 判的是 `body.stream !== true`，故 1 / "true" 都不算。
	stream, ok := decoded.Get("stream")
	if !ok {
		return body, false, nil
	}
	if streaming, isBool := stream.Bool(); !isBool || !streaming {
		return body, false, nil
	}

	options, present := decoded.Get("stream_options")
	switch {
	case !present || options.IsNull():
		decoded.Set("stream_options", convert.NewObject().Set("include_usage", convert.NewBool(true)))
	case includeUsageIsTrue(options):
		return body, false, nil
	default:
		if !options.IsObject() {
			// 数组或标量：Node 不动它（这种形态上游本来也不认）。
			return body, false, nil
		}
		options.Set("include_usage", convert.NewBool(true))
	}

	return []byte(decoded.MarshalCompact()), true, nil
}

// includeUsageIsTrue 报告 `stream_options` 已经是「对象且 include_usage 为 true」。
func includeUsageIsTrue(options *convert.Value) bool {
	if !options.IsObject() {
		return false
	}
	include, ok := options.Get("include_usage")
	if !ok {
		return false
	}
	value, isBool := include.Bool()
	return isBool && value
}
