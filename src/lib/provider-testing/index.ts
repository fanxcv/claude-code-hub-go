/**
 * Provider Testing Service
 * Unified provider testing with three-tier validation
 *
 * Based on relay-pulse implementation patterns:
 * https://github.com/prehisle/relay-pulse
 */

// Node 退役后，本模块只剩**展示契约**：原先汇出的解析器（parsers）、测试服务（test-service）、
// 工具（utils）与校验器（validators）都是 Node 侧探针引擎的实现，它们依赖已删的
// `@/app/v1/_lib/headers` 等模块；探针引擎已由 Go 侧承接（`internal/providertest`），
// UI 只消费结果类型。
export type {
  ClaudeTestBody,
  CodexTestBody,
  GeminiTestBody,
  OpenAITestBody,
  ParsedResponse,
  ProviderTestConfig,
  ProviderTestResult,
  StatusValue,
  TestStatus,
  TestSubStatus,
  TokenUsage,
  ValidationDetails,
} from "./types";
export { STATUS_VALUES, TEST_DEFAULTS } from "./types";
