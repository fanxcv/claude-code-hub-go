// 供应商类型枚举

import type { CacheTtlPreference } from "./cache";

export type ProviderType =
  | "claude"
  | "claude-auth"
  | "codex"
  | "gemini"
  | "gemini-cli"
  | "openai-compatible";

// Codex（Responses API）请求参数覆写偏好
// - "inherit": 遵循客户端请求（默认）
// - 其他值: 强制覆写 reasoning.effort；具体模型可能只支持其中一部分档位
// - "ultra" 是 Codex 产品的子代理模式，不属于 Responses API reasoning.effort
export type CodexReasoningEffortPreference =
  | "inherit"
  | "none"
  | "minimal"
  | "low"
  | "medium"
  | "high"
  | "xhigh"
  | "max";

export type CodexReasoningSummaryPreference = "inherit" | "auto" | "detailed";

export type CodexTextVerbosityPreference = "inherit" | "low" | "medium" | "high";

// 由于 Select 的 value 需要是字符串，这里用 "true"/"false" 表示布尔值
export type CodexParallelToolCallsPreference = "inherit" | "true" | "false";

// OpenAI 官方将 image generation 暴露为内建工具而非顶层布尔字段。
// 这里的覆写语义为：
// - "true": 强制注入 type="image_generation" 的工具能力
// - "false": 强制移除 type="image_generation" 的工具能力
export type CodexImageGenerationPreference = "inherit" | "true" | "false";

export type CodexServiceTierPreference = "inherit" | "auto" | "default" | "flex" | "priority";

// Codex (Responses API) max_output_tokens override
// - "inherit": follow client request (default)
// - numeric string: force override to that value
// (thinking budget / adaptive thinking are Anthropic-only and intentionally not offered here)
export type CodexMaxTokensPreference = "inherit" | string;

// Anthropic (Messages API) parameter overrides
// - "inherit": follow client request (default)
// - numeric string: force override to that value
export type AnthropicMaxTokensPreference = "inherit" | string;
export type AnthropicThinkingBudgetPreference = "inherit" | string;

// Anthropic adaptive thinking configuration
export type AnthropicAdaptiveThinkingEffort = "low" | "medium" | "high" | "xhigh" | "max";
export type AnthropicAdaptiveThinkingModelMatchMode = "specific" | "all";
export interface AnthropicAdaptiveThinkingConfig {
  effort: AnthropicAdaptiveThinkingEffort;
  modelMatchMode: AnthropicAdaptiveThinkingModelMatchMode;
  models: string[];
}

// OpenAI-compatible (Chat Completions API) max_tokens override
// - "inherit": follow client request (default)
// - numeric string: force override to that value
export type OpenAIMaxTokensPreference = "inherit" | string;

export type ProviderModelRedirectMatchType = "exact" | "prefix" | "suffix" | "contains" | "regex";

export interface ProviderModelRedirectRule {
  matchType: ProviderModelRedirectMatchType;
  source: string;
  target: string;
}

export interface AllowedModelRule {
  matchType: ProviderModelRedirectMatchType;
  pattern: string;
}

export type AllowedModelRuleInput = string | AllowedModelRule;

export type ProviderPatchOperation<T> =
  | { mode: "no_change" }
  | { mode: "set"; value: T }
  | { mode: "clear" };

export type ProviderPatchDraftInput<T> =
  | { set: T; clear?: never; no_change?: never }
  | { clear: true; set?: never; no_change?: never }
  | { no_change: true; set?: never; clear?: never }
  | undefined;

export type ProviderBatchPatchField =
  // Basic / existing
  | "is_enabled"
  | "priority"
  | "weight"
  | "cost_multiplier"
  | "group_tag"
  | "model_redirects"
  | "allowed_models"
  | "allowed_clients"
  | "blocked_clients"
  | "anthropic_thinking_budget_preference"
  | "anthropic_adaptive_thinking"
  // Routing / Schedule
  | "active_time_start"
  | "active_time_end"
  | "preserve_client_ip"
  | "disable_session_reuse"
  | "group_priorities"
  | "cache_ttl_preference"
  | "swap_cache_ttl_billing"
  | "context_1m_preference"
  | "codex_reasoning_effort_preference"
  | "codex_reasoning_summary_preference"
  | "codex_text_verbosity_preference"
  | "codex_parallel_tool_calls_preference"
  | "codex_image_generation_preference"
  | "codex_service_tier_preference"
  | "codex_max_tokens_preference"
  | "anthropic_max_tokens_preference"
  | "openai_max_tokens_preference"
  | "gemini_google_search_preference"
  // Rate Limit
  | "limit_5h_usd"
  | "limit_5h_reset_mode"
  | "limit_daily_usd"
  | "daily_reset_mode"
  | "daily_reset_time"
  | "limit_weekly_usd"
  | "limit_monthly_usd"
  | "limit_total_usd"
  | "limit_concurrent_sessions"
  // Circuit Breaker
  | "circuit_breaker_failure_threshold"
  | "circuit_breaker_open_duration"
  | "circuit_breaker_half_open_success_threshold"
  | "circuit_breaker_release_increment"
  | "circuit_breaker_max_open_count"
  | "max_retry_attempts"
  // Network
  | "proxy_url"
  | "proxy_fallback_to_direct"
  | "first_byte_timeout_streaming_ms"
  | "streaming_idle_timeout_ms"
  | "request_timeout_non_streaming_ms"
  // MCP
  | "mcp_passthrough_type"
  | "mcp_passthrough_url";

export interface ProviderBatchPatchDraft {
  // Basic / existing
  is_enabled?: ProviderPatchDraftInput<boolean>;
  priority?: ProviderPatchDraftInput<number>;
  weight?: ProviderPatchDraftInput<number>;
  cost_multiplier?: ProviderPatchDraftInput<number>;
  group_tag?: ProviderPatchDraftInput<string>;
  model_redirects?: ProviderPatchDraftInput<ProviderModelRedirectRule[]>;
  allowed_models?: ProviderPatchDraftInput<AllowedModelRuleInput[]>;
  allowed_clients?: ProviderPatchDraftInput<string[]>;
  blocked_clients?: ProviderPatchDraftInput<string[]>;
  anthropic_thinking_budget_preference?: ProviderPatchDraftInput<AnthropicThinkingBudgetPreference>;
  anthropic_adaptive_thinking?: ProviderPatchDraftInput<AnthropicAdaptiveThinkingConfig>;
  // Routing / Schedule
  active_time_start?: ProviderPatchDraftInput<string>;
  active_time_end?: ProviderPatchDraftInput<string>;
  preserve_client_ip?: ProviderPatchDraftInput<boolean>;
  disable_session_reuse?: ProviderPatchDraftInput<boolean>;
  group_priorities?: ProviderPatchDraftInput<Record<string, number>>;
  cache_ttl_preference?: ProviderPatchDraftInput<CacheTtlPreference>;
  swap_cache_ttl_billing?: ProviderPatchDraftInput<boolean>;
  context_1m_preference?: ProviderPatchDraftInput<string>;
  codex_reasoning_effort_preference?: ProviderPatchDraftInput<CodexReasoningEffortPreference>;
  codex_reasoning_summary_preference?: ProviderPatchDraftInput<CodexReasoningSummaryPreference>;
  codex_text_verbosity_preference?: ProviderPatchDraftInput<CodexTextVerbosityPreference>;
  codex_parallel_tool_calls_preference?: ProviderPatchDraftInput<CodexParallelToolCallsPreference>;
  codex_image_generation_preference?: ProviderPatchDraftInput<CodexImageGenerationPreference>;
  codex_service_tier_preference?: ProviderPatchDraftInput<CodexServiceTierPreference>;
  codex_max_tokens_preference?: ProviderPatchDraftInput<CodexMaxTokensPreference>;
  anthropic_max_tokens_preference?: ProviderPatchDraftInput<AnthropicMaxTokensPreference>;
  openai_max_tokens_preference?: ProviderPatchDraftInput<OpenAIMaxTokensPreference>;
  gemini_google_search_preference?: ProviderPatchDraftInput<GeminiGoogleSearchPreference>;
  // Rate Limit
  limit_5h_usd?: ProviderPatchDraftInput<number>;
  limit_5h_reset_mode?: ProviderPatchDraftInput<"fixed" | "rolling">;
  limit_daily_usd?: ProviderPatchDraftInput<number>;
  daily_reset_mode?: ProviderPatchDraftInput<"fixed" | "rolling">;
  daily_reset_time?: ProviderPatchDraftInput<string>;
  limit_weekly_usd?: ProviderPatchDraftInput<number>;
  limit_monthly_usd?: ProviderPatchDraftInput<number>;
  limit_total_usd?: ProviderPatchDraftInput<number>;
  limit_concurrent_sessions?: ProviderPatchDraftInput<number>;
  // Circuit Breaker
  circuit_breaker_failure_threshold?: ProviderPatchDraftInput<number>;
  circuit_breaker_open_duration?: ProviderPatchDraftInput<number>;
  circuit_breaker_half_open_success_threshold?: ProviderPatchDraftInput<number>;
  // 等待阶梯：窗口 = 熔断时长 + 递增时长 × 级数 n（n 封顶最大次数）。
  circuit_breaker_release_increment?: ProviderPatchDraftInput<number>;
  circuit_breaker_max_open_count?: ProviderPatchDraftInput<number>;
  max_retry_attempts?: ProviderPatchDraftInput<number>;
  // Network
  proxy_url?: ProviderPatchDraftInput<string>;
  proxy_fallback_to_direct?: ProviderPatchDraftInput<boolean>;
  first_byte_timeout_streaming_ms?: ProviderPatchDraftInput<number>;
  streaming_idle_timeout_ms?: ProviderPatchDraftInput<number>;
  request_timeout_non_streaming_ms?: ProviderPatchDraftInput<number>;
  // MCP
  mcp_passthrough_type?: ProviderPatchDraftInput<McpPassthroughType>;
  mcp_passthrough_url?: ProviderPatchDraftInput<string>;
}

export interface ProviderBatchPatch {
  // Basic / existing
  is_enabled: ProviderPatchOperation<boolean>;
  priority: ProviderPatchOperation<number>;
  weight: ProviderPatchOperation<number>;
  cost_multiplier: ProviderPatchOperation<number>;
  group_tag: ProviderPatchOperation<string>;
  model_redirects: ProviderPatchOperation<ProviderModelRedirectRule[]>;
  allowed_models: ProviderPatchOperation<AllowedModelRuleInput[]>;
  allowed_clients: ProviderPatchOperation<string[]>;
  blocked_clients: ProviderPatchOperation<string[]>;
  anthropic_thinking_budget_preference: ProviderPatchOperation<AnthropicThinkingBudgetPreference>;
  anthropic_adaptive_thinking: ProviderPatchOperation<AnthropicAdaptiveThinkingConfig>;
  // Routing / Schedule
  active_time_start: ProviderPatchOperation<string>;
  active_time_end: ProviderPatchOperation<string>;
  preserve_client_ip: ProviderPatchOperation<boolean>;
  disable_session_reuse: ProviderPatchOperation<boolean>;
  group_priorities: ProviderPatchOperation<Record<string, number>>;
  cache_ttl_preference: ProviderPatchOperation<CacheTtlPreference>;
  swap_cache_ttl_billing: ProviderPatchOperation<boolean>;
  context_1m_preference: ProviderPatchOperation<string>;
  codex_reasoning_effort_preference: ProviderPatchOperation<CodexReasoningEffortPreference>;
  codex_reasoning_summary_preference: ProviderPatchOperation<CodexReasoningSummaryPreference>;
  codex_text_verbosity_preference: ProviderPatchOperation<CodexTextVerbosityPreference>;
  codex_parallel_tool_calls_preference: ProviderPatchOperation<CodexParallelToolCallsPreference>;
  codex_image_generation_preference: ProviderPatchOperation<CodexImageGenerationPreference>;
  codex_service_tier_preference: ProviderPatchOperation<CodexServiceTierPreference>;
  codex_max_tokens_preference: ProviderPatchOperation<CodexMaxTokensPreference>;
  anthropic_max_tokens_preference: ProviderPatchOperation<AnthropicMaxTokensPreference>;
  openai_max_tokens_preference: ProviderPatchOperation<OpenAIMaxTokensPreference>;
  gemini_google_search_preference: ProviderPatchOperation<GeminiGoogleSearchPreference>;
  // Rate Limit
  limit_5h_usd: ProviderPatchOperation<number>;
  limit_5h_reset_mode: ProviderPatchOperation<"fixed" | "rolling">;
  limit_daily_usd: ProviderPatchOperation<number>;
  daily_reset_mode: ProviderPatchOperation<"fixed" | "rolling">;
  daily_reset_time: ProviderPatchOperation<string>;
  limit_weekly_usd: ProviderPatchOperation<number>;
  limit_monthly_usd: ProviderPatchOperation<number>;
  limit_total_usd: ProviderPatchOperation<number>;
  limit_concurrent_sessions: ProviderPatchOperation<number>;
  // Circuit Breaker
  circuit_breaker_failure_threshold: ProviderPatchOperation<number>;
  circuit_breaker_open_duration: ProviderPatchOperation<number>;
  circuit_breaker_half_open_success_threshold: ProviderPatchOperation<number>;
  // 等待阶梯：递增时长（毫秒）与最大次数；两者都能 clear（= 不启用阶梯）。
  circuit_breaker_release_increment: ProviderPatchOperation<number>;
  circuit_breaker_max_open_count: ProviderPatchOperation<number>;
  max_retry_attempts: ProviderPatchOperation<number>;
  // Network
  proxy_url: ProviderPatchOperation<string>;
  proxy_fallback_to_direct: ProviderPatchOperation<boolean>;
  first_byte_timeout_streaming_ms: ProviderPatchOperation<number>;
  streaming_idle_timeout_ms: ProviderPatchOperation<number>;
  request_timeout_non_streaming_ms: ProviderPatchOperation<number>;
  // MCP
  mcp_passthrough_type: ProviderPatchOperation<McpPassthroughType>;
  mcp_passthrough_url: ProviderPatchOperation<string>;
}

export interface ProviderBatchApplyUpdates {
  // Basic / existing
  is_enabled?: boolean;
  priority?: number;
  weight?: number;
  cost_multiplier?: number;
  group_tag?: string | null;
  model_redirects?: ProviderModelRedirectRule[] | null;
  allowed_models?: AllowedModelRuleInput[] | null;
  allowed_clients?: string[];
  blocked_clients?: string[];
  anthropic_thinking_budget_preference?: AnthropicThinkingBudgetPreference | null;
  anthropic_adaptive_thinking?: AnthropicAdaptiveThinkingConfig | null;
  // Routing / Schedule
  active_time_start?: string | null;
  active_time_end?: string | null;
  preserve_client_ip?: boolean;
  disable_session_reuse?: boolean;
  group_priorities?: Record<string, number> | null;
  cache_ttl_preference?: CacheTtlPreference | null;
  swap_cache_ttl_billing?: boolean;
  context_1m_preference?: string | null;
  codex_reasoning_effort_preference?: CodexReasoningEffortPreference | null;
  codex_reasoning_summary_preference?: CodexReasoningSummaryPreference | null;
  codex_text_verbosity_preference?: CodexTextVerbosityPreference | null;
  codex_parallel_tool_calls_preference?: CodexParallelToolCallsPreference | null;
  codex_image_generation_preference?: CodexImageGenerationPreference | null;
  codex_service_tier_preference?: CodexServiceTierPreference | null;
  codex_max_tokens_preference?: CodexMaxTokensPreference | null;
  anthropic_max_tokens_preference?: AnthropicMaxTokensPreference | null;
  openai_max_tokens_preference?: OpenAIMaxTokensPreference | null;
  gemini_google_search_preference?: GeminiGoogleSearchPreference | null;
  // Rate Limit
  limit_5h_usd?: number | null;
  limit_5h_reset_mode?: "fixed" | "rolling";
  limit_daily_usd?: number | null;
  daily_reset_mode?: "fixed" | "rolling";
  daily_reset_time?: string;
  limit_weekly_usd?: number | null;
  limit_monthly_usd?: number | null;
  limit_total_usd?: number | null;
  limit_concurrent_sessions?: number;
  // Circuit Breaker
  circuit_breaker_failure_threshold?: number;
  circuit_breaker_open_duration?: number;
  circuit_breaker_half_open_success_threshold?: number;
  // 等待阶梯：窗口 = 熔断时长 + 递增时长 × 级数（级数封顶最大次数）；null = 不启用。
  circuit_breaker_release_increment?: number | null;
  circuit_breaker_max_open_count?: number | null;
  max_retry_attempts?: number | null;
  // Network
  proxy_url?: string | null;
  proxy_fallback_to_direct?: boolean;
  first_byte_timeout_streaming_ms?: number;
  streaming_idle_timeout_ms?: number;
  request_timeout_non_streaming_ms?: number;
  // MCP
  mcp_passthrough_type?: McpPassthroughType;
  mcp_passthrough_url?: string | null;
}

// Gemini (generateContent API) parameter overrides
// - "inherit": follow client request (default)
// - "enabled": force inject googleSearch tool
// - "disabled": force remove googleSearch tool from request
export type GeminiGoogleSearchPreference = "inherit" | "enabled" | "disabled";

// MCP 透传类型枚举
export type McpPassthroughType = "none" | "minimax" | "glm" | "custom";

// 静态自定义请求头：键值都为字符串；持久化为 jsonb
export type ProviderCustomHeaders = Record<string, string>;

export interface Provider {
  id: number;
  name: string;
  url: string;
  key: string;
  // 供应商聚合实体（按官网域名归一）
  providerVendorId: number | null;
  // 是否启用
  isEnabled: boolean;
  // 权重（0-100）
  weight: number;

  // 优先级和分组配置
  priority: number;
  groupPriorities: Record<string, number> | null;
  costMultiplier: number;
  groupTag: string | null;

  // 供应商类型：扩展支持 4 种类型
  providerType: ProviderType;
  // 是否透传客户端 IP
  preserveClientIp: boolean;
  // 是否跳过当前供应商的 sticky session 复用
  disableSessionReuse: boolean;
  // 低速降级（实验特性，默认关闭）：仅开启的供应商参与低速监控。
  slowRateMonitorEnabled: boolean;
  // 参数列可空，null = 取代码默认值（30 分钟 / 3 天 / 100 条 / 3 次 / 0.3 / 10 / 30 / 10）。
  // minSamples 是基线样本下限（默认 100）；triggerCount 是触发阈值（默认 3）。两列语义不同。
  //
  // **单位（用户 2026-09-22 改）**：windowSeconds 存的是**分钟**（默认 30）；
  // baselineWindowSeconds 存的是**天**（默认 3）；ratioPerMille 存的是 **0-1 小数**（默认 0.3）。
  // 字段名保留旧形（REST 契约不破坏），但**名字与单位已不符**——改名会断外部调用方，故只在
  // 注释与 UI 文案里标明真实单位。两者尺度差三个数量级（分钟 vs 天），曾是同一列，故拆开。
  slowRateWindowSeconds: number | null;
  slowRateBaselineWindowSeconds: number | null;
  slowRateMinSamples: number | null;
  slowRateTriggerCount: number | null;
  // 0-1 小数（字段名沿旧 per-mille 形制，值不再乘 1000）
  slowRateRatioPerMille: number | null;
  slowRatePenaltyStep: number | null;
  slowRatePenaltyMax: number | null;
  // 恢复策略阈值：连续这么多个可判定请求都不慢即重置降权（默认 10）。
  slowRateRecoveryRequests: number | null;
  // 首字后停滞探测阈值 T（秒）：null = 不探测（机制默认关闭）。
  slowRateProbeAfterFirstByteSeconds: number | null;
  modelRedirects: ProviderModelRedirectRule[] | null;

  // Scheduled active time window (HH:mm format, null = always active)
  activeTimeStart: string | null;
  activeTimeEnd: string | null;

  // 模型列表：双重语义
  // - Anthropic 提供商：白名单（管理员限制可调度的模型，可选）
  // - 非 Anthropic 提供商：声明列表（提供商声称支持的模型，可选）
  // - null 或空数组：Anthropic 允许所有 claude 模型，非 Anthropic 允许任意模型
  allowedModels: AllowedModelRuleInput[] | null;
  allowedClients: string[]; // Allowed client patterns (empty = no restriction)
  blockedClients: string[]; // Blocked client patterns (blacklist, checked before allowedClients)

  // MCP 透传类型：控制是否启用 MCP 透传功能
  // 'none': 不启用（默认）
  // 'minimax': 透传到 minimax MCP 服务（图片识别、联网搜索）
  // 'glm': 透传到智谱 MCP 服务（图片分析、视频分析）
  // 'custom': 自定义 MCP 服务（预留）
  mcpPassthroughType: McpPassthroughType;

  // MCP 透传 URL：MCP 服务的基础 URL
  // 如果未配置，则自动从 provider.url 提取基础域名
  // 例如：https://api.minimaxi.com/anthropic -> https://api.minimaxi.com
  mcpPassthroughUrl: string | null;

  // 协议转换开关：是否允许接收协议不一致的客户端请求（由 provider-selector 判定）
  protocolConversionEnabled: boolean;

  // 金额限流配置
  limit5hUsd: number | null;
  limit5hResetMode: "fixed" | "rolling";
  limitDailyUsd: number | null;
  dailyResetMode: "fixed" | "rolling";
  dailyResetTime: string;
  limitWeeklyUsd: number | null;
  limitMonthlyUsd: number | null;
  // 总消费上限（手动重置后从 0 重新累计）
  limitTotalUsd: number | null;
  // 总消费重置时间：用于实现“达到总限额后手动重置用量”
  totalCostResetAt: Date | null;
  limitConcurrentSessions: number;

  // 熔断器配置（每个供应商独立配置）
  maxRetryAttempts: number | null;
  circuitBreakerFailureThreshold: number;
  circuitBreakerOpenDuration: number; // 毫秒
  circuitBreakerHalfOpenSuccessThreshold: number;
  // 等待阶梯（circuit backoff ladder）：窗口 = 熔断时长 + 递增时长 × 级数 n（n 封顶最大次数）。
  // 两者都为 null / 非正即不启用阶梯（窗口恒为熔断时长）。
  circuitBreakerReleaseIncrement: number | null;
  circuitBreakerMaxOpenCount: number | null;

  // 代理配置（支持 HTTP/HTTPS/SOCKS5）
  proxyUrl: string | null;
  proxyFallbackToDirect: boolean;

  // 静态自定义请求头：会被合并到出站请求，但不能覆盖鉴权头
  customHeaders: ProviderCustomHeaders | null;

  // 超时配置（毫秒）
  firstByteTimeoutStreamingMs: number;
  streamingIdleTimeoutMs: number;
  requestTimeoutNonStreamingMs: number;

  // 供应商官网地址（用于快速跳转管理）
  websiteUrl: string | null;
  faviconUrl: string | null;

  // Cache TTL override（inherit 表示不强制覆写）
  cacheTtlPreference: CacheTtlPreference | null;

  // Cache TTL billing swap: invert 1h<->5m for cost calculation
  swapCacheTtlBilling: boolean;

  // 1M Context Window 偏好配置（仅对 Anthropic 类型供应商有效）
  context1mPreference: string | null;

  // Codex（Responses API）参数覆写（仅对 Codex 类型供应商有效）
  codexReasoningEffortPreference: CodexReasoningEffortPreference | null;
  codexReasoningSummaryPreference: CodexReasoningSummaryPreference | null;
  codexTextVerbosityPreference: CodexTextVerbosityPreference | null;
  codexParallelToolCallsPreference: CodexParallelToolCallsPreference | null;
  codexImageGenerationPreference: CodexImageGenerationPreference | null;
  codexServiceTierPreference: CodexServiceTierPreference | null;
  codexMaxTokensPreference: CodexMaxTokensPreference | null;

  // Anthropic (Messages API) parameter overrides (only for claude/claude-auth providers)
  anthropicMaxTokensPreference: AnthropicMaxTokensPreference | null;
  anthropicThinkingBudgetPreference: AnthropicThinkingBudgetPreference | null;
  anthropicAdaptiveThinking: AnthropicAdaptiveThinkingConfig | null;

  // OpenAI-compatible (Chat Completions API) parameter overrides (only for openai-compatible providers)
  openaiMaxTokensPreference: OpenAIMaxTokensPreference | null;

  // Gemini (generateContent API) parameter overrides (only for gemini/gemini-cli providers)
  geminiGoogleSearchPreference: GeminiGoogleSearchPreference | null;

  // 废弃（保留向后兼容，但不再使用）
  // TPM (Tokens Per Minute): 每分钟可处理的文本总量
  tpm: number | null;
  // RPM (Requests Per Minute): 每分钟可发起的API调用次数
  rpm: number | null;
  // RPD (Requests Per Day): 每天可发起的API调用总次数
  rpd: number | null;
  // CC (Concurrent Connections/Requests): 同一时刻能同时处理的请求数量
  cc: number | null;

  createdAt: Date;
  updatedAt: Date;
  deletedAt?: Date;
}

// 前端显示用的供应商类型（包含格式化后的数据）
export interface ProviderDisplay {
  id: number;
  name: string;
  url: string;
  maskedKey: string;
  isEnabled: boolean;
  weight: number;
  // 优先级和分组配置
  priority: number;
  groupPriorities: Record<string, number> | null;
  costMultiplier: number;
  groupTag: string | null;
  // 供应商类型
  providerType: ProviderType;
  // 供应商聚合实体（按官网域名归一）
  providerVendorId: number | null;
  // 是否透传客户端 IP
  preserveClientIp: boolean;
  // 是否跳过当前供应商的 sticky session 复用
  disableSessionReuse: boolean;
  // 低速降级（实验特性，默认关闭）
  slowRateMonitorEnabled: boolean;
  // 单位同 Provider 侧：windowSeconds 存分钟、baselineWindowSeconds 存天、ratioPerMille 存 0-1 小数。
  slowRateWindowSeconds: number | null;
  slowRateBaselineWindowSeconds: number | null;
  slowRateMinSamples: number | null;
  slowRateTriggerCount: number | null;
  slowRateRatioPerMille: number | null;
  slowRatePenaltyStep: number | null;
  slowRatePenaltyMax: number | null;
  // 恢复策略阈值（默认 10）。
  slowRateRecoveryRequests: number | null;
  // 首字后停滞探测阈值：null = 不探测。
  slowRateProbeAfterFirstByteSeconds: number | null;
  modelRedirects: ProviderModelRedirectRule[] | null;
  // Scheduled active time window
  activeTimeStart: string | null;
  activeTimeEnd: string | null;
  // 模型列表（双重语义）
  allowedModels: AllowedModelRuleInput[] | null;
  allowedClients: string[]; // Allowed client patterns (empty = no restriction)
  blockedClients: string[]; // Blocked client patterns (blacklist, checked before allowedClients)
  // MCP 透传类型
  mcpPassthroughType: McpPassthroughType;
  // MCP 透传 URL
  mcpPassthroughUrl: string | null;
  // 协议转换开关
  protocolConversionEnabled: boolean;
  // 金额限流配置
  limit5hUsd: number | null;
  limit5hResetMode: "fixed" | "rolling";
  limitDailyUsd: number | null;
  dailyResetMode: "fixed" | "rolling";
  dailyResetTime: string;
  limitWeeklyUsd: number | null;
  limitMonthlyUsd: number | null;
  limitTotalUsd: number | null;
  totalCostResetAt?: Date | null;
  limitConcurrentSessions: number;
  // 熔断器配置
  maxRetryAttempts: number | null;
  circuitBreakerFailureThreshold: number;
  circuitBreakerOpenDuration: number; // 毫秒
  circuitBreakerHalfOpenSuccessThreshold: number;
  // 等待阶梯（见上）：窗口 = 熔断时长 + 递增时长 × n（n 封顶最大次数）；null = 不启用。
  circuitBreakerReleaseIncrement: number | null;
  circuitBreakerMaxOpenCount: number | null;
  // 代理配置
  proxyUrl: string | null;
  proxyFallbackToDirect: boolean;
  // 静态自定义请求头
  customHeaders: ProviderCustomHeaders | null;
  // 超时配置（毫秒）
  firstByteTimeoutStreamingMs: number;
  streamingIdleTimeoutMs: number;
  requestTimeoutNonStreamingMs: number;
  // 供应商官网地址
  websiteUrl: string | null;
  faviconUrl: string | null;
  cacheTtlPreference: CacheTtlPreference | null;
  swapCacheTtlBilling: boolean;
  context1mPreference: string | null;
  codexReasoningEffortPreference: CodexReasoningEffortPreference | null;
  codexReasoningSummaryPreference: CodexReasoningSummaryPreference | null;
  codexTextVerbosityPreference: CodexTextVerbosityPreference | null;
  codexParallelToolCallsPreference: CodexParallelToolCallsPreference | null;
  codexImageGenerationPreference: CodexImageGenerationPreference | null;
  codexServiceTierPreference: CodexServiceTierPreference | null;
  codexMaxTokensPreference: CodexMaxTokensPreference | null;
  anthropicMaxTokensPreference: AnthropicMaxTokensPreference | null;
  anthropicThinkingBudgetPreference: AnthropicThinkingBudgetPreference | null;
  anthropicAdaptiveThinking: AnthropicAdaptiveThinkingConfig | null;
  openaiMaxTokensPreference: OpenAIMaxTokensPreference | null;
  geminiGoogleSearchPreference: GeminiGoogleSearchPreference | null;
  // 废弃字段（保留向后兼容）
  tpm: number | null;
  rpm: number | null;
  rpd: number | null;
  cc: number | null;
  createdAt: string; // 格式化后的日期字符串
  updatedAt: string; // 格式化后的日期字符串
  // 统计数据（可选）
  todayTotalCostUsd?: string;
  todayCallCount?: number;
  lastCallTime?: string | null;
  lastCallModel?: string | null;
}

/**
 * Provider statistics loaded asynchronously
 * Used by getProviderStatisticsAsync() return type
 */
export interface ProviderStatistics {
  todayCost: string;
  todayCalls: number;
  lastCallTime: string | null;
  lastCallModel: string | null;
}

/**
 * Map of provider ID to statistics
 */
export type ProviderStatisticsMap = Record<number, ProviderStatistics>;

/**
 * Provider-level (key/credential) circuit breaker snapshot.
 * Mirrors the shape returned by `/api/v1/providers/health`.
 */
export interface ProviderCircuitHealth {
  circuitState: "closed" | "open" | "half-open";
  failureCount: number;
  lastFailureTime: number | null;
  circuitOpenUntil: number | null;
  recoveryMinutes: number | null;
  /**
   * 等待阶梯的级数 n：0 = 从未爬过阶梯（含阶梯未启用、已恢复）。
   *
   * 它是**原始**事实：窗口过期（有效态转 half-open）也不变。
   */
  consecutiveOpenCount: number;
  /** 级数最近一次变化的时刻；从未变化即 null。 */
  consecutiveOpenCountChangedAt: number | null;
  /**
   * 本次开闸窗口的时长（分钟，向上取整）。
   *
   * 级数为 0 时为 null：那时窗口就是基础时长（与未启用阶梯一致），
   * 服务端不报这个数字，界面因此也不必为它堆字。
   */
  openWindowMinutes: number | null;
  /**
   * 低速降权运行态（Go 侧增强字段，Node 无此键）。
   *
   * 三态是刻意的，界面必须区分：
   * - `null`/缺省：服务端未装配该读面（无 Redis），整段不显示；
   * - `available === false`：读面在但本次读不到，显示「读不到」而非「无降权」；
   * - `available === true` 且 `penalty === null`：确实无降权（不是 0，0 与 null 同义于此）。
   */
  slowRate?: ProviderSlowRateHealth | null;
  /**
   * 实时并发数（Go 侧增强字段，Node 无此键）。
   *
   * 四态是刻意的，界面必须区分：
   * - `null`/缺省：未装配读面，或全局统计开关关着（统计没在跑），整段不显示；
   * - `trackingEnabled === false`：开关关着，**不显示**（服务端此时不会给数）；
   * - `available === false`：开着但本次读不到，显示「读不到」而非「空闲」；
   * - `available === true`：`activeSessions === 0` 是**真实读数**（确实没有在飞请求）。
   */
  concurrency?: ProviderConcurrencyHealth | null;
}

/**
 * 渠道级低速降权读数（`/api/v1/providers/health` 的 `slowRate` 字段）。
 *
 * 聚合口径：一个渠道可有多个「渠道 × 模型」组合各带降权，本投影取**最大值**，
 * `modelKey` 是取到该最大值的那一个组合，`combinations` 是生效组合数。
 *
 * `available === false` 时 `penalty` 一律为 null：**不用 0 冒充**——
 * 0 与「无降权」同义，必须与「读不到」可区分（与 ProviderCircuitLogsState 同纪律）。
 */
export interface ProviderSlowRateHealth {
  available: boolean;
  /** 降权量（生效最大值）。无降权或读不到即 null。 */
  penalty: number | null;
  /** 降权最重的那一个组合的模型键。无降权即 null。 */
  modelKey: string | null;
  /** 有生效降权的组合数（含 modelKey 那一个）。 */
  combinations: number;
  /** 只在 available === false 时给出（例如 redis_unavailable）。 */
  unavailableReason?: string | null;
}

/**
 * 渠道级实时并发数（`/api/v1/providers/health` 的 `concurrency` 字段）。
 *
 * `activeSessions` 用 `null` 而不是 0 表示「读不到」：**不用 0 冒充**——
 * 0 是合法读数（确实没有在飞请求），与「没开统计 / 读不到」必须可区分
 * （与 `ProviderSlowRateHealth` 同纪律）。
 */
export interface ProviderConcurrencyHealth {
  /** 全局统计开关的本次读数。为假时服务端不会给 `activeSessions`。 */
  trackingEnabled: boolean;
  /** 为假表示开着统计但本次读不到；此时 `activeSessions` 为 null。 */
  available: boolean;
  /** 在飞请求数（每尝试计）；仅在 `available === true` 时给出。 */
  activeSessions: number | null;
  /** 只在 `available === false` 时给出（例如 redis_unavailable）。 */
  unavailableReason?: string | null;
}

/**
 * Map of provider ID to circuit-breaker health snapshot.
 */
export type ProviderHealthStatus = Record<number, ProviderCircuitHealth>;

/**
 * 「当前熔断状态」块。
 *
 * `available=false` 时数值字段一律为 `null`：**不用 0 冒充**——0 是合法读数（闭态且从未失败），
 * 与「读不到」必须可区分。这与 `/providers/health` 的既有纪律一致。
 */
export interface ProviderCircuitLogsState {
  available: boolean;
  circuitState: "closed" | "open" | "half-open" | null;
  failureCount: number | null;
  lastFailureTime: number | null;
  circuitOpenUntil: number | null;
  halfOpenSuccessCount: number | null;
  /** 距恢复还有几分钟（向上取整）；未开启或已到期时为 null。 */
  recoveryMinutes: number | null;
  /** 等待阶梯的级数 n：0 = 从未爬过阶梯（含阶梯未启用、已恢复）。 */
  consecutiveOpenCount: number | null;
  /** 级数最近一次变化的时刻；从未变化即 null。 */
  consecutiveOpenCountChangedAt: number | null;
  /** 本次开闸窗口的时长（分钟）；级数为 0 或求不出时为 null。 */
  openWindowMinutes: number | null;
  /** 仅在 available=false 时给出（例如 redis_unavailable）。 */
  unavailableReason: string | null;
}

/** 熔断阈值块（来源：供应商行 `circuit_breaker_*` 三列，写路径同步 Redis 哈希时的同源取数）。 */
export interface ProviderCircuitLogsThresholds {
  failureThreshold: number;
  openDuration: number;
  halfOpenSuccessThreshold: number;
}

/** 「最近错误」的时间范围——界面上必须显示，否则「24 小时内无错误」会被误读成「从无错误」。 */
export interface ProviderCircuitLogsWindow {
  limit: number;
  lookbackHours: number;
  since: string;
}

/** 熔断日志里的一条错误。 */
export interface ProviderCircuitLogsError {
  requestId: number;
  createdAt: string;
  model: string | null;
  statusCode: number | null;
  errorMessage: string | null;
  durationMs: number | null;
  endpoint: string | null;
  /** direct = 行级事实；chain = 该行链内的失败条目（该供应商在这次请求里失败过）。 */
  source: "direct" | "chain";
  /** 仅 source=chain 时可能非空：链上的结局词（如 retry_failed）。 */
  chainReason: string | null;
  /** errorMessage 是否被后端脱敏改写过——界面应据此提示「这不是上游原话」。 */
  redacted: boolean;
}

/** 供应商熔断日志（GET /api/v1/providers/{id}/circuit-logs）。 */
export interface ProviderCircuitLogs {
  providerId: number;
  circuit: ProviderCircuitLogsState;
  thresholds: ProviderCircuitLogsThresholds;
  window: ProviderCircuitLogsWindow;
  errors: ProviderCircuitLogsError[];
  /** 仅在「库读不到」时给出；此时 errors 为空数组（两块独立降级）。 */
  errorsUnavailableReason: string | null;
}

/** 低速日志里的一条事件（存储侧 internal/slowlog 的 Event 投影）。 */
export type ProviderSlowLogKind =
  | "penalty_up"
  | "penalty_down"
  | "baseline_published"
  | "baseline_revoked";

export interface ProviderSlowLogEvent {
  kind: ProviderSlowLogKind;
  /** 毫秒时间戳。 */
  at: number;
  modelKey: string | null;
  /** 降权量的前后值（惩罚类事件）；基线事件为 null——用 null 表达「不含此维」而不是 0。 */
  penaltyFrom: number | null;
  penaltyTo: number | null;
  /** 基线读数（基线发布事件）。 */
  median: number | null;
  samples: number | null;
  /** 基线来源（primary/extended/extended_stale）或撤销原因。 */
  reason: string | null;
}

/** 低速事件的时间范围——与熔断日志同理，界面必须显示，否则「24h 内无降权」会被误读成「从未降权」。 */
export interface ProviderSlowLogsWindow {
  limit: number;
  retentionHours: number;
  since: string;
}

/** 供应商低速日志（GET /api/v1/providers/{id}/slow-logs）。 */
export interface ProviderSlowLogs {
  providerId: number;
  window: ProviderSlowLogsWindow;
  events: ProviderSlowLogEvent[];
  /** 仅在「读不到」时给出；此时 events 为空数组。 */
  unavailableReason: string | null;
}

export interface CreateProviderData {
  name: string;
  url: string;
  key: string;
  // 是否启用（默认 true）- 数据库字段名
  is_enabled?: boolean;
  // 权重（默认 1）
  weight?: number;

  // 优先级和分组配置
  priority?: number;
  group_priorities?: Record<string, number> | null;
  cost_multiplier?: number;
  group_tag?: string | null;

  // 供应商类型和模型配置
  provider_type?: ProviderType;
  preserve_client_ip?: boolean;
  disable_session_reuse?: boolean;
  model_redirects?: ProviderModelRedirectRule[] | null;
  active_time_start?: string | null;
  active_time_end?: string | null;
  allowed_models?: AllowedModelRuleInput[] | null;
  allowed_clients?: string[] | null;
  blocked_clients?: string[] | null;
  mcp_passthrough_type?: McpPassthroughType;
  mcp_passthrough_url?: string | null;
  // 协议转换开关（默认 false）
  protocol_conversion_enabled?: boolean;

  // 金额限流配置
  limit_5h_usd?: number | null;
  limit_5h_reset_mode?: "fixed" | "rolling";
  limit_daily_usd?: number | null;
  daily_reset_mode?: "fixed" | "rolling";
  daily_reset_time?: string;
  limit_weekly_usd?: number | null;
  limit_monthly_usd?: number | null;
  limit_total_usd?: number | null;
  limit_concurrent_sessions?: number;

  // 熔断器配置
  max_retry_attempts?: number | null;
  circuit_breaker_failure_threshold?: number;
  circuit_breaker_open_duration?: number; // 毫秒
  circuit_breaker_half_open_success_threshold?: number;
  // 等待阶梯（见 CreateProviderData 的同名说明）：null = 不启用阶梯。
  circuit_breaker_release_increment?: number | null;
  circuit_breaker_max_open_count?: number | null;

  // 代理配置（支持 HTTP/HTTPS/SOCKS5）
  proxy_url?: string | null;
  proxy_fallback_to_direct?: boolean;

  // 静态自定义请求头
  custom_headers?: ProviderCustomHeaders | null;

  // 超时配置（毫秒）
  first_byte_timeout_streaming_ms?: number;
  streaming_idle_timeout_ms?: number;
  request_timeout_non_streaming_ms?: number;

  // 供应商官网地址
  website_url?: string | null;
  favicon_url?: string | null;
  cache_ttl_preference?: CacheTtlPreference | null;
  swap_cache_ttl_billing?: boolean;
  context_1m_preference?: string | null;
  codex_reasoning_effort_preference?: CodexReasoningEffortPreference | null;
  codex_reasoning_summary_preference?: CodexReasoningSummaryPreference | null;
  codex_text_verbosity_preference?: CodexTextVerbosityPreference | null;
  codex_parallel_tool_calls_preference?: CodexParallelToolCallsPreference | null;
  codex_image_generation_preference?: CodexImageGenerationPreference | null;
  codex_service_tier_preference?: CodexServiceTierPreference | null;
  codex_max_tokens_preference?: CodexMaxTokensPreference | null;
  anthropic_max_tokens_preference?: AnthropicMaxTokensPreference | null;
  anthropic_thinking_budget_preference?: AnthropicThinkingBudgetPreference | null;
  anthropic_adaptive_thinking?: AnthropicAdaptiveThinkingConfig | null;
  openai_max_tokens_preference?: OpenAIMaxTokensPreference | null;
  gemini_google_search_preference?: GeminiGoogleSearchPreference | null;

  // 低速降级（实验特性，默认关闭，逐渠道开关）
  slow_rate_monitor_enabled?: boolean;
  slow_rate_window_seconds?: number | null;
  slow_rate_min_samples?: number | null;
  slow_rate_trigger_count?: number | null;
  // 千分比：200 = 0.2
  slow_rate_ratio_per_mille?: number | null;
  slow_rate_penalty_step?: number | null;
  slow_rate_penalty_max?: number | null;
  // 首字后停滞探测阈值：null = 不探测。
  slow_rate_probe_after_first_byte_seconds?: number | null;

  // 废弃字段（保留向后兼容）
  // TPM (Tokens Per Minute): 每分钟可处理的文本总量
  tpm: number | null;
  // RPM (Requests Per Minute): 每分钟可发起的API调用次数
  rpm: number | null;
  // RPD (Requests Per Day): 每天可发起的API调用总次数
  rpd: number | null;
  // CC (Concurrent Connections/Requests): 同一时刻能同时处理的请求数量
  cc: number | null;
}

export interface UpdateProviderData {
  name?: string;
  url?: string;
  key?: string;
  // 是否启用 - 数据库字段名
  is_enabled?: boolean;
  // 权重（0-100）
  weight?: number;

  // 优先级和分组配置
  priority?: number;
  group_priorities?: Record<string, number> | null;
  cost_multiplier?: number;
  group_tag?: string | null;

  // 供应商类型和模型配置
  provider_type?: ProviderType;
  preserve_client_ip?: boolean;
  disable_session_reuse?: boolean;
  model_redirects?: ProviderModelRedirectRule[] | null;
  active_time_start?: string | null;
  active_time_end?: string | null;
  allowed_models?: AllowedModelRuleInput[] | null;
  allowed_clients?: string[] | null;
  blocked_clients?: string[] | null;
  mcp_passthrough_type?: McpPassthroughType;
  mcp_passthrough_url?: string | null;
  // 协议转换开关（默认 false）
  protocol_conversion_enabled?: boolean;

  // 金额限流配置
  limit_5h_usd?: number | null;
  limit_5h_reset_mode?: "fixed" | "rolling";
  limit_daily_usd?: number | null;
  daily_reset_mode?: "fixed" | "rolling";
  daily_reset_time?: string;
  limit_weekly_usd?: number | null;
  limit_monthly_usd?: number | null;
  limit_total_usd?: number | null;
  limit_concurrent_sessions?: number;

  // 熔断器配置
  max_retry_attempts?: number | null;
  circuit_breaker_failure_threshold?: number;
  circuit_breaker_open_duration?: number; // 毫秒
  circuit_breaker_half_open_success_threshold?: number;
  // 等待阶梯（见 CreateProviderData 的同名说明）：null = 不启用阶梯。
  circuit_breaker_release_increment?: number | null;
  circuit_breaker_max_open_count?: number | null;

  // 代理配置（支持 HTTP/HTTPS/SOCKS5）
  proxy_url?: string | null;
  proxy_fallback_to_direct?: boolean;

  // 静态自定义请求头
  custom_headers?: ProviderCustomHeaders | null;

  // 超时配置（毫秒）
  first_byte_timeout_streaming_ms?: number;
  streaming_idle_timeout_ms?: number;
  request_timeout_non_streaming_ms?: number;

  // 供应商官网地址
  website_url?: string | null;
  favicon_url?: string | null;
  cache_ttl_preference?: CacheTtlPreference | null;
  swap_cache_ttl_billing?: boolean;
  context_1m_preference?: string | null;
  codex_reasoning_effort_preference?: CodexReasoningEffortPreference | null;
  codex_reasoning_summary_preference?: CodexReasoningSummaryPreference | null;
  codex_text_verbosity_preference?: CodexTextVerbosityPreference | null;
  codex_parallel_tool_calls_preference?: CodexParallelToolCallsPreference | null;
  codex_image_generation_preference?: CodexImageGenerationPreference | null;
  codex_service_tier_preference?: CodexServiceTierPreference | null;
  codex_max_tokens_preference?: CodexMaxTokensPreference | null;
  anthropic_max_tokens_preference?: AnthropicMaxTokensPreference | null;
  anthropic_thinking_budget_preference?: AnthropicThinkingBudgetPreference | null;
  anthropic_adaptive_thinking?: AnthropicAdaptiveThinkingConfig | null;
  openai_max_tokens_preference?: OpenAIMaxTokensPreference | null;
  gemini_google_search_preference?: GeminiGoogleSearchPreference | null;

  // 低速降级（实验特性，默认关闭，逐渠道开关）
  slow_rate_monitor_enabled?: boolean;
  slow_rate_window_seconds?: number | null;
  slow_rate_min_samples?: number | null;
  slow_rate_trigger_count?: number | null;
  // 千分比：200 = 0.2
  slow_rate_ratio_per_mille?: number | null;
  slow_rate_penalty_step?: number | null;
  slow_rate_penalty_max?: number | null;
  // 首字后停滞探测阈值：null = 不探测。
  slow_rate_probe_after_first_byte_seconds?: number | null;

  // 废弃字段（保留向后兼容）
  // TPM (Tokens Per Minute): 每分钟可处理的文本总量
  tpm?: number | null;
  // RPM (Requests Per Minute): 每分钟可发起的API调用次数
  rpm?: number | null;
  // RPD (Requests Per Day): 每天可发起的API调用总次数
  rpd?: number | null;
  // CC (Concurrent Connections/Requests): 同一时刻能同时处理的请求数量
  cc?: number | null;
}

export interface ProviderVendor {
  id: number;
  websiteDomain: string;
  displayName: string | null;
  websiteUrl: string | null;
  faviconUrl: string | null;
  createdAt: Date;
  updatedAt: Date;
}

export type ProviderEndpointProbeSource = "scheduled" | "manual" | "runtime";

export interface ProviderEndpoint {
  id: number;
  vendorId: number;
  providerType: ProviderType;
  url: string;
  label: string | null;
  sortOrder: number;
  isEnabled: boolean;
  lastProbedAt: Date | null;
  lastProbeOk: boolean | null;
  lastProbeStatusCode: number | null;
  lastProbeLatencyMs: number | null;
  lastProbeErrorType: string | null;
  lastProbeErrorMessage: string | null;
  createdAt: Date;
  updatedAt: Date;
  deletedAt: Date | null;
}

export interface ProviderEndpointProbeLog {
  id: number;
  endpointId: number;
  source: ProviderEndpointProbeSource;
  ok: boolean;
  statusCode: number | null;
  latencyMs: number | null;
  errorType: string | null;
  errorMessage: string | null;
  createdAt: Date;
}

// ---- 2026-09 node 退役迁移（原 actions/provider-endpoints、actions/providers、actions/provider-slots） ----
export type DashboardProviderVendor = ProviderVendor & { providerTypes: ProviderType[] };

export interface PreviewProviderBatchPatchResult {
  previewToken: string;
  previewRevision: string;
  previewExpiresAt: string;
  providerIds: number[];
  changedFields: ProviderBatchPatchField[];
  rows: ProviderBatchPreviewRow[];
  summary: {
    providerCount: number;
    fieldCount: number;
    skipCount: number;
  };
}

export type ProviderApiTestSuccessDetails = {
  responseTime?: number;
  model?: string;
  usage?: Record<string, unknown>;
  content?: string;
  rawResponse?: string;
  streamInfo?: {
    chunksReceived: number;
    format: "sse" | "ndjson";
  };
};

export interface ProviderBatchPreviewRow {
  providerId: number;
  providerName: string;
  field: ProviderBatchPatchField;
  status: "changed" | "skipped";
  before: unknown;
  after: unknown;
  skipReason?: string;
}

export interface EditProviderResult {
  undoToken: string;
  operationId: string;
}

export interface RemoveProviderResult {
  undoToken: string;
  operationId: string;
}

/**
 * 供应商并发插槽信息
 */
export interface ProviderSlotInfo {
  /** 供应商 ID */
  providerId: number;
  /** 供应商名称 */
  name: string;
  /** 当前已使用插槽数（活跃 Session 数） */
  usedSlots: number;
  /** 总插槽数（并发限制） */
  totalSlots: number;
  /** 总 Token 流量（从排行榜获取） */
  totalVolume: number;
}
