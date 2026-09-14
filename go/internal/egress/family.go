package egress

// Family 是协议方言族名，取值与 src/app/v1/_lib/protocol-convert 的 codec 命名一致。
//
// 词汇的消费者在本包之外（`internal/dataplane/routes.go` 的族映射、`internal/pctx` 的
// 管线上下文），故类型与四个具名族保留；原先本包内的族/规则解析器（`ParseFamily`、
// `familyAliases`、`allowedMethods`、`Rule` 等）随归属白名单一并删除——它们没有本包之外的
// 调用点，见。
type Family string

const (
	// FamilyAny 匹配任意方言。
	FamilyAny Family = "any"
	// FamilyAnthropicMessages 是 Anthropic Messages 方言。
	FamilyAnthropicMessages Family = "anthropic-messages"
	// FamilyOpenAIChat 是 OpenAI Chat Completions 方言。
	FamilyOpenAIChat Family = "openai-chat"
	// FamilyOpenAIResponses 是 OpenAI Responses 方言。
	FamilyOpenAIResponses Family = "openai-responses"
	// FamilyGemini 是 Gemini 方言。
	FamilyGemini Family = "gemini"
)
