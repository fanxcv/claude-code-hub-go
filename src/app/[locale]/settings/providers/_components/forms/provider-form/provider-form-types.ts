import type { Dispatch } from "react";
import type {
  AllowedModelRule,
  AnthropicAdaptiveThinkingConfig,
  AnthropicAdaptiveThinkingEffort,
  AnthropicAdaptiveThinkingModelMatchMode,
  AnthropicMaxTokensPreference,
  AnthropicThinkingBudgetPreference,
  CodexImageGenerationPreference,
  CodexMaxTokensPreference,
  CodexParallelToolCallsPreference,
  CodexReasoningEffortPreference,
  CodexReasoningSummaryPreference,
  CodexServiceTierPreference,
  CodexTextVerbosityPreference,
  GeminiGoogleSearchPreference,
  McpPassthroughType,
  OpenAIMaxTokensPreference,
  ProviderDisplay,
  ProviderModelRedirectRule,
  ProviderType,
} from "@/types/provider";
import type { BatchSettingsAnalysis } from "../../batch-edit/analyze-batch-settings";

// Form mode
export type FormMode = "create" | "edit" | "batch";

// Tab identifiers
export type TabId = "basic" | "routing" | "options" | "limits" | "network" | "testing";

// Sub-tab identifiers for sub-navigation within parent sections
export type SubTabId = "scheduling" | "activeTime" | "circuitBreaker" | "timeout";

// Combined navigation target (parent tab or sub-tab)
export type NavTargetId = TabId | SubTabId;

// Tab configuration
export interface TabConfig {
  id: TabId;
  labelKey: string;
  icon: string;
}

// Form state sections
export interface BasicInfoState {
  name: string;
  url: string;
  key: string;
  websiteUrl: string;
}

export interface SlowRateParams {
  // 低速降级（实验特性，默认关闭）：仅开启的供应商参与低速监控与降级。
  // 参数均 null 时取代码默认值（30 分钟 / 3 天 / 100 条 / 3 次 / 0.3 / 10 / 30 / 10）。
  slowRateMonitorEnabled: boolean;
  // **单位（用户 2026-09-22 改）**：slowRateWindowSeconds 存的是**分钟**（默认 30）；
  // slowRateBaselineWindowSeconds 存的是**天**（默认 3）；slowRateRatioPerMille 存的是
  // **0-1 小数**（默认 0.3）。字段名保留旧形（API 契约不破坏），但名字与单位已不符。
  slowRateWindowSeconds: number | null;
  slowRateBaselineWindowSeconds: number | null;
  slowRateMinSamples: number | null;
  slowRateTriggerCount: number | null;
  slowRateRatioPerMille: number | null;
  slowRatePenaltyStep: number | null;
  slowRatePenaltyMax: number | null;
  // 恢复策略阈值：连续这么多个可判定请求都不慢即重置降权（默认 10）。
  slowRateRecoveryRequests: number | null;
  // 首字后停滞探测阈值 T（秒）。
  //
  // null = **未覆盖**（不是「关闭」）：监控开关打开时取出厂值 30s；显式设 0 才是「不要探测」。
  slowRateProbeAfterFirstByteSeconds: number | null;
  /** 提交前速率闸（默认全关）：null = 未覆盖 ⇒ false。 */
  slowRatePrecommitEnabled: boolean | null;
  /** 提交前速率闸阈值（语义字节/秒）：null = 未覆盖 ⇒ 由基线推导。 */
  slowRatePrecommitMinBytesPerSecond: number | null;
}

export interface RoutingState extends SlowRateParams {
  providerType: ProviderType;
  groupTag: string[];
  preserveClientIp: boolean;
  disableSessionReuse: boolean;
  // 协议转换：开启后允许协议不一致的客户端请求路由到该供应商（网关两侧转换）
  protocolConversionEnabled: boolean;
  modelRedirects: ProviderModelRedirectRule[];
  allowedModels: AllowedModelRule[];
  allowedClients: string[];
  blockedClients: string[];
  priority: number;
  groupPriorities: Record<string, number>;
  weight: number;
  costMultiplier: number;
  cacheTtlPreference: "inherit" | "5m" | "1h";
  swapCacheTtlBilling: boolean;
  // Codex-specific
  codexReasoningEffortPreference: CodexReasoningEffortPreference;
  codexReasoningSummaryPreference: CodexReasoningSummaryPreference;
  codexTextVerbosityPreference: CodexTextVerbosityPreference;
  codexParallelToolCallsPreference: CodexParallelToolCallsPreference;
  codexImageGenerationPreference: CodexImageGenerationPreference;
  codexServiceTierPreference: CodexServiceTierPreference;
  codexMaxTokensPreference: CodexMaxTokensPreference;
  // Anthropic-specific
  anthropicMaxTokensPreference: AnthropicMaxTokensPreference;
  anthropicThinkingBudgetPreference: AnthropicThinkingBudgetPreference;
  anthropicAdaptiveThinking: AnthropicAdaptiveThinkingConfig | null;
  // OpenAI-compatible-specific
  openaiMaxTokensPreference: OpenAIMaxTokensPreference;
  // Gemini-specific
  geminiGoogleSearchPreference: GeminiGoogleSearchPreference;
  // Scheduled active time window (HH:mm format, null = always active)
  activeTimeStart: string | null;
  activeTimeEnd: string | null;
  // Static custom request headers as JSON text (parsed on submit, null/empty cleared on save)
  customHeadersText: string;
}

export interface RateLimitState {
  limit5hUsd: number | null;
  limitDailyUsd: number | null;
  dailyResetMode: "fixed" | "rolling";
  dailyResetTime: string;
  limitWeeklyUsd: number | null;
  limitMonthlyUsd: number | null;
  limitTotalUsd: number | null;
  limitConcurrentSessions: number | null;
}

export interface CircuitBreakerState {
  failureThreshold: number | undefined;
  openDurationMinutes: number | undefined;
  halfOpenSuccessThreshold: number | undefined;
  maxRetryAttempts: number | null;
  /**
   * 等待阶梯的递增时长（分钟）；表单里用分钟，提交时转成毫秒。
   *
   * null = 不启用阶梯（窗口恒为熔断时长）——这是出厂默认，也是用户明确要的默认：
   * 不启用时行为与加该特性之前逐字段一致。
   */
  releaseIncrementMinutes: number | null;
  /** 等待阶梯的最大次数（级数 n 的封顶值）；null = 不启用阶梯。 */
  maxOpenCount: number | null;
}

export interface NetworkState {
  proxyUrl: string;
  proxyFallbackToDirect: boolean;
  firstByteTimeoutStreamingSeconds: number | undefined;
  streamingIdleTimeoutSeconds: number | undefined;
  requestTimeoutNonStreamingSeconds: number | undefined;
}

export interface McpState {
  mcpPassthroughType: McpPassthroughType;
  mcpPassthroughUrl: string;
}

export interface BatchState {
  isEnabled: "no_change" | "true" | "false";
}

export interface UIState {
  activeTab: TabId;
  activeSubTab: SubTabId | null;
  isPending: boolean;
  showFailureThresholdConfirm: boolean;
}

// Complete form state
export interface ProviderFormState {
  basic: BasicInfoState;
  routing: RoutingState;
  rateLimit: RateLimitState;
  circuitBreaker: CircuitBreakerState;
  network: NetworkState;
  mcp: McpState;
  batch: BatchState;
  ui: UIState;
}

// Action types for reducer
export type ProviderFormAction =
  // Basic info actions
  | { type: "SET_NAME"; payload: string }
  | { type: "SET_URL"; payload: string }
  | { type: "SET_KEY"; payload: string }
  | { type: "SET_WEBSITE_URL"; payload: string }
  // Routing actions
  | { type: "SET_PROVIDER_TYPE"; payload: ProviderType }
  | { type: "SET_GROUP_TAG"; payload: string[] }
  | { type: "SET_PRESERVE_CLIENT_IP"; payload: boolean }
  | { type: "SET_DISABLE_SESSION_REUSE"; payload: boolean }
  | { type: "SET_PROTOCOL_CONVERSION_ENABLED"; payload: boolean }
  // 低速降级六个字段一起更新：它们是一个整体（开关 + 五参数），拆成六个 action 只增加
  // 与 routing 状态的重复映射，没有额外表达力。
  | { type: "SET_SLOW_RATE_PARAMS"; payload: Partial<SlowRateParams> }
  | { type: "SET_MODEL_REDIRECTS"; payload: ProviderModelRedirectRule[] }
  | { type: "SET_ALLOWED_MODELS"; payload: AllowedModelRule[] }
  | { type: "SET_ALLOWED_CLIENTS"; payload: string[] }
  | { type: "SET_BLOCKED_CLIENTS"; payload: string[] }
  | { type: "SET_PRIORITY"; payload: number }
  | { type: "SET_GROUP_PRIORITIES"; payload: Record<string, number> }
  | { type: "SET_WEIGHT"; payload: number }
  | { type: "SET_COST_MULTIPLIER"; payload: number }
  | { type: "SET_CACHE_TTL_PREFERENCE"; payload: "inherit" | "5m" | "1h" }
  | { type: "SET_SWAP_CACHE_TTL_BILLING"; payload: boolean }
  | { type: "SET_CODEX_REASONING_EFFORT"; payload: CodexReasoningEffortPreference }
  | { type: "SET_CODEX_REASONING_SUMMARY"; payload: CodexReasoningSummaryPreference }
  | { type: "SET_CODEX_TEXT_VERBOSITY"; payload: CodexTextVerbosityPreference }
  | { type: "SET_CODEX_PARALLEL_TOOL_CALLS"; payload: CodexParallelToolCallsPreference }
  | { type: "SET_CODEX_IMAGE_GENERATION"; payload: CodexImageGenerationPreference }
  | { type: "SET_CODEX_SERVICE_TIER"; payload: CodexServiceTierPreference }
  | { type: "SET_CODEX_MAX_TOKENS"; payload: CodexMaxTokensPreference }
  | { type: "SET_ANTHROPIC_MAX_TOKENS"; payload: AnthropicMaxTokensPreference }
  | { type: "SET_OPENAI_MAX_TOKENS"; payload: OpenAIMaxTokensPreference }
  | { type: "SET_ANTHROPIC_THINKING_BUDGET"; payload: AnthropicThinkingBudgetPreference }
  | { type: "SET_ADAPTIVE_THINKING_EFFORT"; payload: AnthropicAdaptiveThinkingEffort }
  | {
      type: "SET_ADAPTIVE_THINKING_MODEL_MATCH_MODE";
      payload: AnthropicAdaptiveThinkingModelMatchMode;
    }
  | { type: "SET_ADAPTIVE_THINKING_MODELS"; payload: string[] }
  | { type: "SET_ADAPTIVE_THINKING_ENABLED"; payload: boolean }
  | { type: "SET_GEMINI_GOOGLE_SEARCH"; payload: GeminiGoogleSearchPreference }
  | { type: "SET_ACTIVE_TIME_START"; payload: string | null }
  | { type: "SET_ACTIVE_TIME_END"; payload: string | null }
  | { type: "SET_CUSTOM_HEADERS_TEXT"; payload: string }
  // Rate limit actions
  | { type: "SET_LIMIT_5H_USD"; payload: number | null }
  | { type: "SET_LIMIT_DAILY_USD"; payload: number | null }
  | { type: "SET_DAILY_RESET_MODE"; payload: "fixed" | "rolling" }
  | { type: "SET_DAILY_RESET_TIME"; payload: string }
  | { type: "SET_LIMIT_WEEKLY_USD"; payload: number | null }
  | { type: "SET_LIMIT_MONTHLY_USD"; payload: number | null }
  | { type: "SET_LIMIT_TOTAL_USD"; payload: number | null }
  | { type: "SET_LIMIT_CONCURRENT_SESSIONS"; payload: number | null }
  // Circuit breaker actions
  | { type: "SET_FAILURE_THRESHOLD"; payload: number | undefined }
  | { type: "SET_OPEN_DURATION_MINUTES"; payload: number | undefined }
  | { type: "SET_HALF_OPEN_SUCCESS_THRESHOLD"; payload: number | undefined }
  | { type: "SET_RELEASE_INCREMENT_MINUTES"; payload: number | null }
  | { type: "SET_MAX_OPEN_COUNT"; payload: number | null }
  | { type: "SET_MAX_RETRY_ATTEMPTS"; payload: number | null }
  // Network actions
  | { type: "SET_PROXY_URL"; payload: string }
  | { type: "SET_PROXY_FALLBACK_TO_DIRECT"; payload: boolean }
  | { type: "SET_FIRST_BYTE_TIMEOUT_STREAMING"; payload: number | undefined }
  | { type: "SET_STREAMING_IDLE_TIMEOUT"; payload: number | undefined }
  | { type: "SET_REQUEST_TIMEOUT_NON_STREAMING"; payload: number | undefined }
  // MCP actions
  | { type: "SET_MCP_PASSTHROUGH_TYPE"; payload: McpPassthroughType }
  | { type: "SET_MCP_PASSTHROUGH_URL"; payload: string }
  // UI actions
  | { type: "SET_ACTIVE_TAB"; payload: TabId }
  | { type: "SET_ACTIVE_NAV"; payload: { tab: TabId; subTab: SubTabId | null } }
  | { type: "SET_IS_PENDING"; payload: boolean }
  | { type: "SET_SHOW_FAILURE_THRESHOLD_CONFIRM"; payload: boolean }
  // Bulk actions
  | { type: "RESET_FORM" }
  | { type: "LOAD_PROVIDER"; payload: ProviderDisplay }
  // Batch actions
  | { type: "SET_BATCH_IS_ENABLED"; payload: "no_change" | "true" | "false" };

// Form props
export interface ProviderFormProps {
  mode: FormMode;
  onSuccess?: () => void;
  provider?: ProviderDisplay;
  cloneProvider?: ProviderDisplay;
  enableMultiProviderTypes: boolean;
  hideUrl?: boolean;
  hideWebsiteUrl?: boolean;
  preset?: {
    name?: string;
    url?: string;
    websiteUrl?: string;
    providerType?: ProviderType;
  };
  urlResolver?: (providerType: ProviderType) => Promise<string | null>;
  allowedProviderTypes?: ProviderType[];
}

// Context value
export interface ProviderFormContextValue {
  state: ProviderFormState;
  dispatch: Dispatch<ProviderFormAction>;
  mode: FormMode;
  provider?: ProviderDisplay;
  enableMultiProviderTypes: boolean;
  hideUrl: boolean;
  hideWebsiteUrl: boolean;
  groupSuggestions: string[];
  batchProviders?: ProviderDisplay[];
  dirtyFields: Set<string>;
  batchAnalysis?: BatchSettingsAnalysis;
}
